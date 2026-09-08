//go:build cgo

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func TestContentRolloutAllowlistIsSeparateExactAndFailClosed(t *testing.T) {
	t.Setenv("SUMMARY_CONTENT_READ_SPACES", "s,other")
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
	if ContentProtocolRequired(model.SummaryTask{SpaceID: "s"}) {
		t.Fatal("read allowlist enabled writes")
	}
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "s, s, *, OTHER")
	for _, space := range []string{"other", "S", "unknown", "*", ""} {
		if ContentProtocolRequired(model.SummaryTask{SpaceID: space}) {
			t.Fatalf("unexpected enrollment: %q", space)
		}
	}
	for _, space := range []string{"s", "OTHER"} {
		if !ContentProtocolRequired(model.SummaryTask{SpaceID: space}) {
			t.Fatalf("missing enrollment: %q", space)
		}
	}
}

func TestContentEnrollmentRollsBackWithFailedWrite(t *testing.T) {
	db := contentTestDB(t)
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	requireWriteOK(t, db.Callback().Create().Before("gorm:create").Register("fail_content_audit", func(tx *gorm.DB) {
		if tx.Statement.Table == "summary_content_audit" {
			tx.AddError(errors.New("synthetic audit failure"))
		}
	}))
	t.Cleanup(func() { db.Callback().Create().Remove("fail_content_audit") })
	_, err := NewContentService(db).Edit(context.Background(), "s", 1, "owner", target.ID(),
		EditContentRequest{ContentBaseline: baselineOf(current), Content: "must roll back"})
	if err == nil {
		t.Fatal("expected audit failure")
	}
	var task model.SummaryTask
	requireWriteOK(t, db.First(&task, 1).Error)
	if task.ContentProtocolVersion != 0 || contentRowCount(t, db, target) != 0 {
		t.Fatal("failed write enrolled task or left a lazy V1")
	}
}

func TestContentRetentionSurvivesFlagWithdrawalForBothStores(t *testing.T) {
	for _, kind := range []string{ContentResult, ContentPersonal} {
		t.Run(kind, func(t *testing.T) {
			db := contentTestDB(t)
			target, current := seedWritableContent(t, db, 1, kind)
			s := NewContentService(db)
			current, err := s.Edit(context.Background(), "s", 1, "owner", target.ID(),
				EditContentRequest{ContentBaseline: baselineOf(current), Content: "managed"})
			requireWriteOK(t, err)
			for i := 2; i <= 7; i++ {
				if kind == ContentPersonal {
					requireWriteOK(t, db.Create(&model.PersonalResultVersion{TaskID: 1, UserID: "owner", Version: i, Content: "history"}).Error)
				} else {
					requireWriteOK(t, db.Create(&model.SummaryResult{TaskID: 1, Version: i, Content: "history"}).Error)
				}
			}
			t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
			requireWriteOK(t, PruneSummaryResultVersions(db, 1, 5))
			requireWriteOK(t, PrunePersonalResultVersions(db, 1, "owner", 5))
			if contentRowCount(t, db, target) != 7 {
				t.Fatal("flag withdrawal re-enabled retention limit")
			}
			called := false
			err = WithLegacyContentWrite(db, 1, func(tx *gorm.DB) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrContentProtocolRequired) || called {
				t.Fatalf("legacy mutation not fenced: %v", err)
			}
			catalog, err := s.Catalog(context.Background(), "s", 1, "owner")
			requireWriteOK(t, err)
			if catalog.Contents[0].CurrentVersion.VersionID != current.VersionID ||
				catalog.Contents[0].ContentRevision != current.ContentRevision {
				t.Fatal("cleaner changed current version or revision")
			}
		})
	}
}

func TestContentRetentionLegacyAndReadOnlySpacesStillPrune(t *testing.T) {
	db := contentTestDB(t)
	t.Setenv("SUMMARY_CONTENT_READ_SPACES", "s")
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
	target, _ := seedWritableContent(t, db, 1, ContentResult)
	for i := 2; i <= 7; i++ {
		requireWriteOK(t, db.Create(&model.SummaryResult{TaskID: 1, Version: i, Content: "legacy"}).Error)
	}
	// Remove the current pointer solely in this synthetic fixture to isolate
	// the keep-five policy; legacy current-pointer protection is unchanged.
	requireWriteOK(t, db.Model(&model.SummaryTask{}).Where("id = ?", 1).Update("current_result_id", nil).Error)
	requireWriteOK(t, PruneSummaryResultVersions(db, 1, 5))
	if contentRowCount(t, db, target) != 5 {
		t.Fatal("read-only rollout changed legacy retention")
	}
	requireWriteOK(t, WithLegacyContentWrite(db, 1, func(*gorm.DB) error { return nil }))
}
