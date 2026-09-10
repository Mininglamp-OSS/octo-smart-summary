//go:build cgo

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func contentTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}, &model.SummarySchedule{},
		&model.SummaryResult{}, &model.PersonalResult{}, &model.PersonalResultVersion{},
		&contentTestGenerationRun{}, &contentTestAudit{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// sqlite3 recognizes DATETIME but not MySQL's DATETIME(6) as a timestamp.
// Production migrations and the MySQL concurrency suite retain microseconds.
type contentTestGenerationRun struct {
	model.SummaryGenerationRun
	EffectiveAt  time.Time  `gorm:"column:effective_at;type:datetime;not null"`
	ScheduledFor *time.Time `gorm:"column:scheduled_for;type:datetime;uniqueIndex:uk_generation_schedule_slot"`
	LeaseUntil   *time.Time `gorm:"column:lease_until;type:datetime;index:idx_generation_recovery"`
	CreatedAt    time.Time  `gorm:"column:created_at;type:datetime;not null"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;type:datetime;not null"`
}

func (contentTestGenerationRun) TableName() string { return "summary_generation_run" }

type contentTestAudit struct {
	model.SummaryContentAudit
	CreatedAt time.Time `gorm:"column:created_at;type:datetime;not null"`
}

func (contentTestAudit) TableName() string { return "summary_content_audit" }

func seedContentTask(t *testing.T, db *gorm.DB, id int64, users ...string) model.SummaryTask {
	t.Helper()
	task := model.SummaryTask{ID: id, SpaceID: "s", TaskNo: fmt.Sprintf("ST%d", id),
		CreatorID: "owner", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted}
	mustContentCreate(t, db, &task)
	for _, user := range users {
		mustContentCreate(t, db, &model.SummaryParticipant{TaskID: id, UserID: user, Status: model.ParticipantSubmitted})
	}
	return task
}

func mustContentCreate(t *testing.T, db *gorm.DB, value any) {
	t.Helper()
	if err := db.Create(value).Error; err != nil {
		t.Fatal(err)
	}
}

func TestContentCatalogProvisionalReadNeverWrites(t *testing.T) {
	db := contentTestDB(t)
	seedContentTask(t, db, 1, "owner")
	pr := model.PersonalResult{TaskID: 1, UserID: "owner", Content: "report [1]", CitationsJSON: `[{"index":1,"content":"evidence"}]`}
	mustContentCreate(t, db, &pr)
	service := NewContentService(db)
	var token string
	for i := 0; i < 2; i++ {
		catalog, err := service.Catalog(context.Background(), "s", 1, "owner")
		if err != nil {
			t.Fatal(err)
		}
		if len(catalog.Contents) != 1 || catalog.CreatedVia != "unknown" {
			t.Fatalf("catalog: %+v", catalog)
		}
		content := catalog.Contents[0]
		if content.Kind != ContentPersonal || content.CurrentVersion == nil || !content.CurrentVersion.Provisional ||
			content.ContentRevision != 1 || content.Capabilities.CanEdit || content.Capabilities.CanRefine {
			t.Fatalf("wrong provisional/capability contract: %+v", content)
		}
		if i == 1 && token != content.CurrentVersion.VersionID {
			t.Fatal("GET changed provisional token")
		}
		token = content.CurrentVersion.VersionID
		page, err := service.Versions(context.Background(), "s", 1, "owner", content.ContentID, "", 20)
		if err != nil || len(page.Items) != 1 || page.Items[0].VersionID != token {
			t.Fatalf("provisional version page: %+v %v", page, err)
		}
	}
	var count int64
	db.Model(&model.PersonalResultVersion{}).Count(&count)
	if count != 0 {
		t.Fatal("GET materialized a baseline")
	}
	var after model.PersonalResult
	db.First(&after, pr.ID)
	if after.CurrentVersionID != nil || after.ContentRevision != 0 || !after.UpdatedAt.Equal(pr.UpdatedAt) {
		t.Fatalf("GET mutated canonical row: %+v", after)
	}
}

func TestContentVersionsBeyondFiveAndNamespaceIsolation(t *testing.T) {
	db := contentTestDB(t)
	task := seedContentTask(t, db, 1, "owner", "member")
	for i := 1; i <= 7; i++ {
		r := model.SummaryResult{ID: int64(i), TaskID: 1, Content: fmt.Sprint(i), Version: i, OperationType: "generate"}
		mustContentCreate(t, db, &r)
	}
	if err := db.Model(&task).Updates(map[string]any{"current_result_id": 7, "content_revision": 7}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewContentService(db)
	target := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentResult}
	var got []int
	cursor := ""
	for {
		page, err := service.Versions(context.Background(), "s", 1, "owner", target.ID(), cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Items {
			got = append(got, v.Version)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if fmt.Sprint(got) != "[7 6 5 4 3 2 1]" {
		t.Fatalf("history was truncated: %v", got)
	}
	wrong := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentPersonal, UserID: "owner"}
	if _, err := service.Version(context.Background(), "s", 1, "owner", wrong.ID(),
		(ContentVersionIdentity{Target: target, RowID: 1}).ID()); err == nil {
		t.Fatal("result ID accepted in personal namespace")
	}
	for _, actor := range []string{"outsider", ""} {
		if _, err := service.Catalog(context.Background(), "s", 1, actor); err == nil {
			t.Fatalf("unauthorized catalog access: %q", actor)
		}
	}
	if _, err := service.Catalog(context.Background(), "other-space", 1, "owner"); err == nil {
		t.Fatal("cross-space catalog allowed")
	}
}

func TestTeamSingleSubmissionRemainsTeamAndDoesNotLeakCitations(t *testing.T) {
	db := contentTestDB(t)
	task := seedContentTask(t, db, 1, "member")
	current := model.SummaryResult{TaskID: 1, Content: "team [P1] [1]", Version: 2,
		CitationsJSON:     `[{"index":1,"content":"private secret"}]`,
		TeamCitationsJSON: `[{"index":1,"user_id":"member","user_name":"Historical Name","personal_result_id":5,"task_id":1}]`}
	mustContentCreate(t, db, &current)
	old := current
	old.ID, old.Version = 0, 1
	mustContentCreate(t, db, &old)
	db.Model(&task).Update("current_result_id", current.ID)
	mustContentCreate(t, db, &model.PersonalResult{TaskID: 1, UserID: "member", Content: "member secret"})
	service := NewContentService(db)
	catalog, err := service.Catalog(context.Background(), "s", 1, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Contents) != 1 || catalog.Contents[0].Kind != ContentResult {
		t.Fatalf("single submitted member changed team identity: %+v", catalog)
	}
	v := catalog.Contents[0].CurrentVersion
	if v.CitationVisibility != "permission_hidden" || len(v.Citations) != 0 || len(v.TeamCitations) != 1 {
		t.Fatalf("citation boundary: %+v", v)
	}
	memberTarget := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentPersonal, UserID: "member"}
	if _, err := service.Versions(context.Background(), "s", 1, "owner", memberTarget.ID(), "", 20); err == nil {
		t.Fatal("creator read member's private versions")
	}
	target := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentResult}
	oldDTO, err := service.Version(context.Background(), "s", 1, "owner", target.ID(),
		(ContentVersionIdentity{Target: target, RowID: old.ID}).ID())
	if err != nil || oldDTO.TeamCitationVisibility != "historical_identity_only" ||
		oldDTO.TeamCitations[0].PersonalResultID != 0 || oldDTO.TeamCitations[0].UserName != "Historical Name" {
		t.Fatalf("historical identity: %+v %v", oldDTO, err)
	}
	data, _ := json.Marshal(oldDTO)
	if strings.Contains(string(data), "private secret") {
		t.Fatal("private evidence escaped DTO")
	}
}

func TestPersonalIdentitySurvivesRunRowRecreation(t *testing.T) {
	db := contentTestDB(t)
	seedContentTask(t, db, 1, "owner")
	old := model.PersonalResultVersion{TaskID: 1, UserID: "owner", ParticipantRefID: 11, Content: "kept", Version: 1}
	mustContentCreate(t, db, &old)
	pr := model.PersonalResult{ID: 91, TaskID: 1, UserID: "owner", ParticipantRefID: 22,
		Content: "kept", CurrentVersionID: &old.ID}
	mustContentCreate(t, db, &pr)
	target := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentPersonal, UserID: "owner"}
	service := NewContentService(db)
	page, err := service.Versions(context.Background(), "s", 1, "owner", target.ID(), "", 20)
	if err != nil || len(page.Items) != 1 || page.Items[0].Content != "kept" {
		t.Fatalf("run row ID incorrectly used for historical lookup: %+v %v", page, err)
	}
}

func TestPersonalMismatchAndBrokenPointerRequireRepairWithoutWriting(t *testing.T) {
	db := contentTestDB(t)
	seedContentTask(t, db, 1, "owner")
	old := model.PersonalResultVersion{TaskID: 1, UserID: "owner", Content: "original", Version: 1}
	mustContentCreate(t, db, &old)
	now := time.Now()
	pr := model.PersonalResult{TaskID: 1, UserID: "owner", Content: "edited", CurrentVersionID: &old.ID, EditedAt: &now}
	mustContentCreate(t, db, &pr)
	service := NewContentService(db)
	catalog, err := service.Catalog(context.Background(), "s", 1, "owner")
	if err != nil || catalog.Contents[0].Integrity != "normalization_required" ||
		catalog.Contents[0].CurrentVersion.Content != "edited" {
		t.Fatalf("canonical edit was hidden: %+v %v", catalog, err)
	}
	var row model.PersonalResultVersion
	db.First(&row, old.ID)
	if row.Content != "original" {
		t.Fatal("compatibility GET normalized history")
	}
	db.Model(&pr).Update("current_version_id", 999)
	catalog, err = service.Catalog(context.Background(), "s", 1, "owner")
	if err != nil || catalog.Contents[0].Integrity != "repair_required" || catalog.Contents[0].CurrentVersion != nil {
		t.Fatalf("broken pointer silently replaced by MAX: %+v %v", catalog, err)
	}
}

func TestProvisionalTokenDetectsLegacyEditWithoutRevisionIncrement(t *testing.T) {
	db := contentTestDB(t)
	seedContentTask(t, db, 1, "owner")
	pr := model.PersonalResult{TaskID: 1, UserID: "owner", Content: "before"}
	mustContentCreate(t, db, &pr)
	service := NewContentService(db)
	catalog, _ := service.Catalog(context.Background(), "s", 1, "owner")
	c := catalog.Contents[0]
	db.Model(&pr).Update("content", "after")
	if _, err := service.Version(context.Background(), "s", 1, "owner", c.ContentID, c.CurrentVersion.VersionID); err == nil {
		t.Fatal("stale provisional version was accepted")
	}
}

func TestNormalizedSingleCannotLeakToUnexpectedParticipant(t *testing.T) {
	db := contentTestDB(t)
	task := seedContentTask(t, db, 1, "owner", "member")
	db.Model(&task).Update("generation_spec_json", `{"collaboration":"single","participants":["owner"]}`)
	mustContentCreate(t, db, &model.PersonalResult{TaskID: 1, UserID: "owner", Content: "private"})
	if _, err := NewContentService(db).Catalog(context.Background(), "s", 1, "member"); err == nil {
		t.Fatal("unexpected participant read single owner content")
	}
}

func TestContentDuplicateVersionsFailClosedAndDoNotDeleteHistory(t *testing.T) {
	db := contentTestDB(t)
	seedContentTask(t, db, 1, "owner", "member")
	for _, content := range []string{"retained A", "retained B"} {
		mustContentCreate(t, db, &model.SummaryResult{TaskID: 1, Content: content, Version: 1})
	}
	target := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentResult}
	_, err := NewContentService(db).Versions(context.Background(), "s", 1, "owner", target.ID(), "", 20)
	if err == nil || err.Error() != "version_repair_required" {
		t.Fatalf("ambiguous duplicate page was accepted: %v", err)
	}
	var count int64
	db.Model(&model.SummaryResult{}).Where("task_id = 1").Count(&count)
	if count != 2 {
		t.Fatal("duplicate repair silently deleted history")
	}
}

func TestContentLegacyScheduleRosterKeepsTeamIdentity(t *testing.T) {
	db := contentTestDB(t)
	task := seedContentTask(t, db, 1, "owner")
	schedule := model.SummarySchedule{SpaceID: "s", CreatorID: "owner", SummaryMode: model.ModeByPerson,
		ParticipantConfig: model.JSON(`{"participants":[{"user_id":"owner"},{"user_id":"member"}]}`)}
	mustContentCreate(t, db, &schedule)
	db.Model(&task).Update("schedule_id", schedule.ID)
	catalog, err := NewContentService(db).Catalog(context.Background(), "s", 1, "owner")
	if err != nil || catalog.Contents[0].Kind != ContentResult {
		t.Fatalf("one confirmed member reclassified scheduled team: %+v %v", catalog, err)
	}
}
