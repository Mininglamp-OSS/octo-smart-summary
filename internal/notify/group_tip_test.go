//go:build cgo

package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupGroupTipTestDB builds an in-memory sqlite fixture with the tables
// OnGroupTip touches (notifications for the dedup rows, sources for the fan-out
// query). Kept separate from setupNotifyTestDB so a schema drift on either
// side surfaces here rather than silently in existing tests.
func setupGroupTipTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.SummaryNotification{}, &model.SummarySource{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func groupTipTask(id int64, mode int, status int) model.SummaryTask {
	return model.SummaryTask{
		ID:          id,
		TaskNo:      "GT-1",
		Title:       "群聊总结",
		SpaceID:     "space-9",
		CreatorID:   "user-creator",
		SummaryMode: mode,
		Status:      status,
	}
}

// mustAddSources inserts the given (source_type, source_id) pairs against a
// task so OnGroupTip's fan-out has something to iterate over.
func mustAddSources(t *testing.T, db *gorm.DB, taskID int64, srcs []model.SummarySource) {
	t.Helper()
	for i := range srcs {
		srcs[i].TaskID = taskID
		if err := db.Create(&srcs[i]).Error; err != nil {
			t.Fatalf("create source: %v", err)
		}
	}
}

func TestOnGroupTip_FansOutOneTipPerGroupSource(t *testing.T) {
	db := setupGroupTipTestDB(t)
	fake := &fakeDeliverer{}
	n := newTestNotifier(db, fake, Config{Enabled: true, MaxAttempts: 3})

	task := groupTipTask(1, model.ModeByPerson-1 /* non-by-person */, model.StatusCompleted)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-a"},
		{SourceType: model.SourceGroup, SourceID: "group-b"},
		{SourceType: model.SourceThread, SourceID: "thread-x"}, // must be skipped
		{SourceType: model.SourceDirect, SourceID: "dm-y"},     // must be skipped
	})

	n.OnGroupTip(task)

	if len(fake.sendCalls) != 2 {
		t.Fatalf("expected 2 group sends, got %d", len(fake.sendCalls))
	}
	channels := map[string]bool{}
	for _, msg := range fake.sendCalls {
		if msg.ChannelType != WireChannelGroup {
			t.Errorf("expected WireChannelGroup=%d, got %d", WireChannelGroup, msg.ChannelType)
		}
		if ct, _ := msg.Payload["content_type"].(int); ct != 21 {
			t.Errorf("expected payload content_type=21 (summaryNotify), got %v", msg.Payload["content_type"])
		}
		if fu, _ := msg.Payload["from_uid"].(string); fu != task.CreatorID {
			t.Errorf("expected payload from_uid=%s, got %v", task.CreatorID, msg.Payload["from_uid"])
		}
		channels[msg.ChannelID] = true
	}
	if !channels["group-a"] || !channels["group-b"] {
		t.Errorf("expected both group-a and group-b to receive tip, got %v", channels)
	}
	if channels["thread-x"] || channels["dm-y"] {
		t.Errorf("thread/dm sources must not receive group tip; got %v", channels)
	}

	// Each group source should have a Sent dedup row.
	var count int64
	db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND status = ?",
			task.ID, model.NotifyKindGroupTip, model.NotifyStatusSent).
		Count(&count)
	if count != 2 {
		t.Errorf("expected 2 sent notification rows, got %d", count)
	}
}

func TestOnGroupTip_IsIdempotentAcrossRetriggers(t *testing.T) {
	db := setupGroupTipTestDB(t)
	fake := &fakeDeliverer{}
	n := newTestNotifier(db, fake, Config{Enabled: true, MaxAttempts: 3})

	task := groupTipTask(1, 1 /* non-by-person */, model.StatusCompleted)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-a"},
	})

	// Two triggers of the same completion (status event + fallback poll
	// pattern) must land at most one delivery per (task, group).
	n.OnGroupTip(task)
	n.OnGroupTip(task)

	if len(fake.sendCalls) != 1 {
		t.Fatalf("idempotency violated: expected exactly 1 send, got %d", len(fake.sendCalls))
	}
}

func TestOnGroupTip_SkipsFailedTask(t *testing.T) {
	db := setupGroupTipTestDB(t)
	fake := &fakeDeliverer{}
	n := newTestNotifier(db, fake, Config{Enabled: true, MaxAttempts: 3})

	// Failed → group tip must NOT fire (would broadcast failure to the group).
	task := groupTipTask(1, 1 /* non-by-person */, model.StatusFailed)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-a"},
	})

	n.OnGroupTip(task)
	if len(fake.sendCalls) != 0 {
		t.Errorf("failed task must not emit group tip, got %d sends", len(fake.sendCalls))
	}
}

func TestOnGroupTip_SkipsByPersonMode(t *testing.T) {
	db := setupGroupTipTestDB(t)
	fake := &fakeDeliverer{}
	n := newTestNotifier(db, fake, Config{Enabled: true, MaxAttempts: 3})

	// by-person mode's fan-out already goes to participants via DMs; layering
	// group tips on top would double-broadcast.
	task := groupTipTask(1, model.ModeByPerson, model.StatusCompleted)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-a"},
	})

	n.OnGroupTip(task)
	if len(fake.sendCalls) != 0 {
		t.Errorf("by-person task must not emit group tip, got %d sends", len(fake.sendCalls))
	}
}

func TestOnGroupTip_SkipsWhenNotEnabled(t *testing.T) {
	db := setupGroupTipTestDB(t)
	fake := &fakeDeliverer{}
	n := newTestNotifier(db, fake, Config{Enabled: false, MaxAttempts: 3})

	task := groupTipTask(1, 1, model.StatusCompleted)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-a"},
	})

	n.OnGroupTip(task)
	if len(fake.sendCalls) != 0 {
		t.Errorf("disabled notifier must not emit; got %d sends", len(fake.sendCalls))
	}
}

func TestOnGroupTip_OneGroupFailureDoesNotBlockOthers(t *testing.T) {
	db := setupGroupTipTestDB(t)
	// Fail the very first delivery attempt (any group; we assert isolation by
	// counting total deliveries across both groups).
	fake := &fakeDeliverer{sendErrOnce: errors.New("transient network")}
	n := newTestNotifier(db, fake, Config{Enabled: true, MaxAttempts: 3})

	task := groupTipTask(1, 1, model.StatusCompleted)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-a"},
		{SourceType: model.SourceGroup, SourceID: "group-b"},
	})

	n.OnGroupTip(task)

	// Both groups must have been attempted (one failed, one succeeded) — one
	// group's failure never blocks another.
	if len(fake.sendCalls) != 2 {
		t.Fatalf("expected 2 delivery attempts across groups, got %d", len(fake.sendCalls))
	}

	// One row Sent, one row Failed → attempt_count=1, retry-eligible on sweep.
	var sent, failed int64
	db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND status = ?",
			task.ID, model.NotifyKindGroupTip, model.NotifyStatusSent).Count(&sent)
	db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND status = ?",
			task.ID, model.NotifyKindGroupTip, model.NotifyStatusFailed).Count(&failed)
	if sent != 1 || failed != 1 {
		t.Errorf("expected sent=1 failed=1, got sent=%d failed=%d", sent, failed)
	}
}

func TestOnGroupTip_SkipsWhenNoGroupSources(t *testing.T) {
	db := setupGroupTipTestDB(t)
	fake := &fakeDeliverer{}
	n := newTestNotifier(db, fake, Config{Enabled: true, MaxAttempts: 3})

	// Only DM / thread sources — nothing to fan out to.
	task := groupTipTask(1, 1, model.StatusCompleted)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceDirect, SourceID: "dm-y"},
		{SourceType: model.SourceThread, SourceID: "thread-x"},
	})

	n.OnGroupTip(task)
	if len(fake.sendCalls) != 0 {
		t.Errorf("no group sources → no sends; got %d", len(fake.sendCalls))
	}
}

func TestOnGroupTip_UsesGroupChannelWithoutEnsureFriend(t *testing.T) {
	db := setupGroupTipTestDB(t)
	fake := &fakeDeliverer{}
	n := newTestNotifier(db, fake, Config{Enabled: true, MaxAttempts: 3})

	task := groupTipTask(1, 1, model.StatusCompleted)
	mustAddSources(t, db, task.ID, []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-a"},
	})

	n.OnGroupTip(task)

	// Group delivery must NOT call EnsureFriend (there is no per-user
	// relationship for a group destination and octo-server does not require
	// one). The DM path in OnTaskTerminal is the only caller of EnsureFriend.
	if len(fake.ensureCalls) != 0 {
		t.Errorf("group tip must not EnsureFriend, got %d calls", len(fake.ensureCalls))
	}
	if len(fake.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(fake.sendCalls))
	}
	msg := fake.sendCalls[0]
	if msg.ChannelType != WireChannelGroup {
		t.Errorf("expected group channel type %d, got %d", WireChannelGroup, msg.ChannelType)
	}
	if msg.ChannelID != "group-a" {
		t.Errorf("expected channel_id=group-a, got %s", msg.ChannelID)
	}
	if _, hasCard := any(msg.Card).(*notifyCard); hasCard && msg.Card != nil {
		t.Errorf("group tip should use Payload not Card; got Card=%+v", msg.Card)
	}
	// Sanity: SpaceID is threaded through the deliverer so octo-server can
	// enforce space membership on the group.
	if len(fake.sendSpaceID) != 1 || fake.sendSpaceID[0] != task.SpaceID {
		t.Errorf("expected SpaceID=%s threaded to SendMessage, got %v",
			task.SpaceID, fake.sendSpaceID)
	}
}

// Compile-time assertion: OnGroupTip's implementation must not accidentally
// take a *context.Context signature — the deliverer receives it internally.
// This is a smoke test to catch a signature drift during refactors.
func TestOnGroupTip_MethodSignature(t *testing.T) {
	var _ func(task model.SummaryTask) = (&Notifier{}).OnGroupTip
	_ = context.Background // keep the import used
}
