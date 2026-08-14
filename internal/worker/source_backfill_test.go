//go:build cgo

package worker

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// backfillSourcesFromMessages must give every generated summary a
// summary_source footprint, even when the task was created without explicit
// sources (the pipeline then auto-selects channels and — before this fix —
// the actually-used channels were never persisted). Reference → refine → save
// (tier-4 origin derivation in agent_summary.go) reads summary_source, so a
// zero-row task can never be iterated on. These tests pin the write-back
// contract:
//
//   - adds rows for channels actually used, mapped via
//     pipeline.StorageChannelTypeToSourceType
//   - never overwrites or deletes user-specified rows (append-only merge)
//   - idempotent across retries (executePipeline re-runs on retry)
//   - skips messages with unknown storage channel_type or empty channel_id
//   - caps total rows so a pathological channel explosion can't blow up the table

func TestBackfillSourcesFromMessages_AddsMissingChannels(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-ADD", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}

	msgs := []pipeline.Message{
		{ChannelID: "grp-a", ChannelType: model.ChannelTypeGroup, SourceName: "群A"},
		{ChannelID: "grp-a", ChannelType: model.ChannelTypeGroup, SourceName: "群A"}, // dup, one row
		{ChannelID: "grp-a____thr1", ChannelType: model.ChannelTypeThread, SourceName: "子区1"},
		// R9 P1: DM messages are present but must NEVER be backfilled —
		// see TestBackfillSourcesFromMessages_SkipsDMChannels.
		{ChannelID: "u2@u1", ChannelType: model.ChannelTypeDM, SourceName: "私聊-u2"},
	}

	p := &Processor{db: db}
	if err := p.backfillSourcesFromMessages(task.ID, msgs); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var rows []model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 deduped rows (DM skipped per R9 P1), got %d: %+v", len(rows), rows)
	}
	type want struct {
		sourceType int
		sourceID   string
		name       string
	}
	wants := []want{
		{model.SourceGroup, "grp-a", "群A"},
		{model.SourceThread, "grp-a____thr1", "子区1"},
	}
	for i, w := range wants {
		if rows[i].SourceType != w.sourceType || rows[i].SourceID != w.sourceID {
			t.Errorf("row %d = (type %d, id %q), want (type %d, id %q)",
				i, rows[i].SourceType, rows[i].SourceID, w.sourceType, w.sourceID)
		}
		if rows[i].SourceName != w.name {
			t.Errorf("row %d name = %q, want %q", i, rows[i].SourceName, w.name)
		}
	}
}

func TestBackfillSourcesFromMessages_PreservesExistingAndAppendsMissing(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-MERGE", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	existing := model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp-a", SourceName: "用户选的群A"}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatalf("seed existing source: %v", err)
	}

	msgs := []pipeline.Message{
		{ChannelID: "grp-a", ChannelType: model.ChannelTypeGroup, SourceName: "群A-抓取名"},
		{ChannelID: "grp-b", ChannelType: model.ChannelTypeGroup, SourceName: "群B"},
	}

	p := &Processor{db: db}
	if err := p.backfillSourcesFromMessages(task.ID, msgs); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var rows []model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (1 kept + 1 appended), got %d: %+v", len(rows), rows)
	}
	// Original row must be untouched — same ID, same user-provided name.
	if rows[0].ID != existing.ID || rows[0].SourceName != "用户选的群A" {
		t.Errorf("existing row modified: id=%d name=%q, want id=%d name=%q",
			rows[0].ID, rows[0].SourceName, existing.ID, "用户选的群A")
	}
	if rows[1].SourceID != "grp-b" || rows[1].SourceType != model.SourceGroup {
		t.Errorf("appended row = (type %d, id %q), want (type %d, id %q)",
			rows[1].SourceType, rows[1].SourceID, model.SourceGroup, "grp-b")
	}
}

func TestBackfillSourcesFromMessages_IdempotentAcrossRetries(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-IDEM", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	msgs := []pipeline.Message{
		{ChannelID: "grp-a", ChannelType: model.ChannelTypeGroup, SourceName: "群A"},
		{ChannelID: "grp-b", ChannelType: model.ChannelTypeGroup, SourceName: "群B"},
	}

	p := &Processor{db: db}
	for i := 0; i < 3; i++ { // simulate retries re-running executePipeline
		if err := p.backfillSourcesFromMessages(task.ID, msgs); err != nil {
			t.Fatalf("backfill run %d: %v", i+1, err)
		}
	}

	var count int64
	if err := db.Model(&model.SummarySource{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows after 3 runs (idempotent), got %d", count)
	}
}

func TestBackfillSourcesFromMessages_SkipsUnknownTypeAndEmptyID(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-SKIP", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}

	msgs := []pipeline.Message{
		{ChannelID: "grp-a", ChannelType: 3, SourceName: "保留类型3"}, // WuKongIM reserved, unmapped
		{ChannelID: "grp-b", ChannelType: 0, SourceName: "零类型"},   // unset
		{ChannelID: "", ChannelType: model.ChannelTypeGroup, SourceName: "空ID"},
		{ChannelID: "grp-ok", ChannelType: model.ChannelTypeGroup, SourceName: "有效"},
	}

	p := &Processor{db: db}
	if err := p.backfillSourcesFromMessages(task.ID, msgs); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var rows []model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Find(&rows).Error; err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) != 1 || rows[0].SourceID != "grp-ok" {
		t.Fatalf("expected only the valid row, got %+v", rows)
	}
}

func TestBackfillSourcesFromMessages_NoMessagesNoRowsNoError(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-EMPTY", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}

	p := &Processor{db: db}
	if err := p.backfillSourcesFromMessages(task.ID, nil); err != nil {
		t.Fatalf("backfill on empty messages must not error: %v", err)
	}
	var count int64
	if err := db.Model(&model.SummarySource{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows, got %d", count)
	}
}

// A pathological auto-select (many channels) must not exceed the same cap
// the create endpoint enforces on user-supplied sources (maxSourceCount=30,
// api/handler/task.go). Existing rows count toward the cap.
func TestBackfillSourcesFromMessages_CapsTotalRows(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-CAP", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// 29 existing rows.
	for i := 0; i < 29; i++ {
		row := model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: fmt.Sprintf("grp-existing-%02d", i)}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	msgs := []pipeline.Message{
		{ChannelID: "new-1", ChannelType: model.ChannelTypeGroup, SourceName: "n1"},
		{ChannelID: "new-2", ChannelType: model.ChannelTypeGroup, SourceName: "n2"},
		{ChannelID: "new-3", ChannelType: model.ChannelTypeGroup, SourceName: "n3"},
	}

	p := &Processor{db: db}
	if err := p.backfillSourcesFromMessages(task.ID, msgs); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var count int64
	if err := db.Model(&model.SummarySource{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 30 {
		t.Fatalf("expected cap at 30 total rows, got %d", count)
	}
}

// R9 P1 (yujiawei, PR #190 review 4926742282): the backfill turned the
// creator's private-chat partners into a field every task reader can see —
// DM rows name the peer UID and survive in a user-visible table whose roster
// is mutable after generation (AddMembers on Completed tasks). A DM is also
// never a useful origin_channel_id for a refined summary, and a normalized
// DM channel id (uidA@uidB) reaches 65 chars with 32-char UIDs, overflowing
// summary_source.source_id varchar(64). DM channels must never be
// backfilled.
func TestBackfillSourcesFromMessages_SkipsDMChannels(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-NODM", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}

	msgs := []pipeline.Message{
		{ChannelID: "grp-a", ChannelType: model.ChannelTypeGroup, SourceName: "群A"},
		{ChannelID: "grp-a____thr1", ChannelType: model.ChannelTypeThread, SourceName: "子区1"},
		{ChannelID: "u2@u1", ChannelType: model.ChannelTypeDM, SourceName: "私聊-u2"},
	}

	p := &Processor{db: db}
	if err := p.backfillSourcesFromMessages(task.ID, msgs); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var rows []model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (group+thread, DM skipped), got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.SourceType == model.SourceDirect {
			t.Errorf("DM row persisted: %+v", r)
		}
	}
}

// R9 P1: backfilled rows must be flagged derived=1 so every user-visible
// projection (list/detail sources, share snapshot, reference-context
// bullets) can exclude them while tier-4 origin derivation keeps reading
// them. User-specified rows are never re-flagged.
func TestBackfillSourcesFromMessages_MarksBackfilledRowsDerived(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-FLAG", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	explicit := model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp-explicit", SourceName: "用户选的群"}
	if err := db.Create(&explicit).Error; err != nil {
		t.Fatalf("seed explicit source: %v", err)
	}

	msgs := []pipeline.Message{
		{ChannelID: "grp-explicit", ChannelType: model.ChannelTypeGroup, SourceName: "用户选的群"},
		{ChannelID: "grp-auto", ChannelType: model.ChannelTypeGroup, SourceName: "自动抓取的群"},
	}

	p := &Processor{db: db}
	if err := p.backfillSourcesFromMessages(task.ID, msgs); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var rows []model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].ID != explicit.ID || rows[0].Derived {
		t.Errorf("explicit row re-flagged or replaced: id=%d derived=%v, want id=%d derived=false", rows[0].ID, rows[0].Derived, explicit.ID)
	}
	if rows[1].SourceID != "grp-auto" || !rows[1].Derived {
		t.Errorf("backfilled row = (id %q, derived %v), want (grp-auto, true)", rows[1].SourceID, rows[1].Derived)
	}
}

// R10 (Jerry-Xin, review 4928758044): "Derived rows now feed back into
// generation scope. Both processor.go and personal_processor.go load all
// summary_source rows into specifiedSources ... later processing treats
// auto-backfilled internal rows as explicit source constraints. If that is
// not intended, filter derived = 0 in worker fetch input and reserve derived
// rows for deriveOriginFromSummarySources." — not intended: derived rows are
// a tier-4 origin-derivation footprint, not user-specified constraints.
func TestExplicitSpecifiedSources_ExcludesDerivedRows(t *testing.T) {
	sources := []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "grp-explicit", SourceName: "显式群", Derived: false},
		{SourceType: model.SourceGroup, SourceID: "grp-derived", SourceName: "auto-backfilled", Derived: true},
		{SourceType: model.SourceThread, SourceID: "grp-explicit____thr", SourceName: "显式子区", Derived: false},
	}
	got := explicitSpecifiedSources(sources)
	if len(got) != 2 {
		t.Fatalf("specifiedSources = %d rows, want 2 (derived row must not feed back into fetch scope)", len(got))
	}
	for _, m := range got {
		if m["source_id"] == "grp-derived" {
			t.Fatalf("derived row leaked into specifiedSources: %v", m)
		}
	}
}

// R10 (yujiawei, issue comment 5280351017): "A unique index on (task_id,
// source_type, source_id) plus ON DUPLICATE KEY UPDATE / INSERT IGNORE would
// make idempotency hold structurally rather than by timing." Concrete
// trigger: scanStuckTasks flips Processing → Pending when the lease expires
// WITHOUT signalling the original worker; two workers then race the
// read-then-insert backfill and both persist the same rows.
func TestSummarySource_UniqueConstraintRejectsDuplicate(t *testing.T) {
	db := newFileBackedTestDB(t)
	task := model.SummaryTask{TaskNo: "BF-UNIQ", SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	row := model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp-x", SourceName: "群X"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp-x", SourceName: "群X-dup"}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("duplicate (task_id, source_type, source_id) insert succeeded; want unique-constraint rejection")
	}
}

func TestBackfillSourcesFromMessages_ConcurrentRaceNoDuplicates(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		db := newFileBackedTestDB(t)
		task := model.SummaryTask{TaskNo: fmt.Sprintf("BF-RACE-%d", iter), SpaceID: "sp", CreatorID: "u1", Title: "t", SummaryMode: model.ModeByPerson}
		if err := db.Create(&task).Error; err != nil {
			t.Fatalf("seed task: %v", err)
		}
		msgs := []pipeline.Message{
			{ChannelID: "grp-race", ChannelType: model.ChannelTypeGroup, SourceName: "竞速群"},
			{ChannelID: fmt.Sprintf("grp-race-extra-%d", iter), ChannelType: model.ChannelTypeGroup, SourceName: "竞速群2"},
		}
		// Two workers claim the same task after a lease expiry and both
		// reach backfillSourcesFromMessages with an empty read.
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				p := &Processor{db: db}
				errs[i] = p.backfillSourcesFromMessages(task.ID, msgs)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("iter %d worker %d: backfill must be conflict-tolerant, got: %v", iter, i, err)
			}
		}
		var count int64
		db.Model(&model.SummarySource{}).Where("task_id = ? AND source_id = ?", task.ID, "grp-race").Count(&count)
		if count != 1 {
			t.Fatalf("iter %d: %d rows for the same (task_id, source_type, source_id); want exactly 1 (race lost the structural guarantee)", iter, count)
		}
	}
}
