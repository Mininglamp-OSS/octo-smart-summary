package worker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupNotifyTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Pin to a single connection so the concurrent idempotency test shares one
	// in-memory database (separate sqlite :memory: connections are separate DBs).
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

type capturedSend struct {
	channelID   string
	channelType int
	payload     map[string]interface{}
}

// recordingSender captures every Send call. Thread-safe so it can be used under
// the concurrent idempotency test.
type recordingSender struct {
	mu    sync.Mutex
	sends []capturedSend
	calls int32
}

func (r *recordingSender) Send(channelID string, channelType int, payload map[string]interface{}) error {
	atomic.AddInt32(&r.calls, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, capturedSend{channelID: channelID, channelType: channelType, payload: payload})
	return nil
}

func (r *recordingSender) recipients() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.sends))
	for _, s := range r.sends {
		out = append(out, s.channelID)
	}
	return out
}

func seedNotifyTask(t *testing.T, db *gorm.DB, creator string, participants []string) int64 {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-NOTIFY",
		SpaceID:        "space1",
		CreatorID:      creator,
		Title:          "周会纪要",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerManual,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	for _, uid := range participants {
		if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: uid}).Error; err != nil {
			t.Fatalf("create participant: %v", err)
		}
	}
	return task.ID
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestNotifyPolicyGate verifies the target set is exactly creator + explicit
// participants (deduped), and that a non-participant — e.g. a member of the
// origin channel who never joined the task — is NEVER targeted. This is the
// policy gate that prevents the channel-wide leak (octo-web #291).
func TestNotifyPolicyGate(t *testing.T) {
	tests := []struct {
		name         string
		creator      string
		participants []string
		wantTargets  []string
		neverTarget  string
	}{
		{
			name:         "private creator-only",
			creator:      "creator1",
			participants: nil,
			wantTargets:  []string{"creator1"},
			neverTarget:  "outsider",
		},
		{
			name:         "creator plus participants",
			creator:      "creator1",
			participants: []string{"alice", "bob"},
			wantTargets:  []string{"creator1", "alice", "bob"},
			neverTarget:  "outsider",
		},
		{
			name:         "creator also a participant is deduped",
			creator:      "creator1",
			participants: []string{"creator1", "alice"},
			wantTargets:  []string{"creator1", "alice"},
			neverTarget:  "outsider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupNotifyTestDB(t)
			taskID := seedNotifyTask(t, db, tt.creator, tt.participants)

			sender := &recordingSender{}
			n := &botNotifier{db: db, sender: sender, fromUID: "bot-uid", fromName: "智能总结"}
			n.NotifyCompleted(taskID)

			got := sender.recipients()
			if len(got) != len(tt.wantTargets) {
				t.Fatalf("target count = %d (%v), want %d (%v)", len(got), got, len(tt.wantTargets), tt.wantTargets)
			}
			for _, want := range tt.wantTargets {
				if !contains(got, want) {
					t.Errorf("missing expected target %q in %v", want, got)
				}
			}
			if contains(got, tt.neverTarget) {
				t.Errorf("non-participant %q must never be targeted, got %v", tt.neverTarget, got)
			}
			// Every emit is a directed DM, never a channel broadcast.
			for _, s := range sender.sends {
				if s.channelType != channelTypePerson {
					t.Errorf("channelType = %d, want person DM (%d)", s.channelType, channelTypePerson)
				}
				if s.payload["type"] != contentTypeSummaryNotify {
					t.Errorf("payload type = %v, want %d", s.payload["type"], contentTypeSummaryNotify)
				}
			}
		})
	}
}

// TestNotifyIdempotencyConcurrent fires NotifyCompleted from many goroutines
// against one task and asserts exactly one fan-out occurs — the notified_at CAS
// must let a single winner through. This is the per-task exactly-once guarantee
// regardless of how many clients/workers observe completion.
func TestNotifyIdempotencyConcurrent(t *testing.T) {
	db := setupNotifyTestDB(t)
	participants := []string{"alice", "bob"}
	taskID := seedNotifyTask(t, db, "creator1", participants)
	wantTargetsPerEmit := 1 + len(participants) // creator + participants

	sender := &recordingSender{}
	n := &botNotifier{db: db, sender: sender, fromUID: "bot-uid", fromName: "智能总结"}

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			n.NotifyCompleted(taskID)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&sender.calls); got != int32(wantTargetsPerEmit) {
		t.Fatalf("Send called %d times, want exactly %d (single fan-out)", got, wantTargetsPerEmit)
	}

	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.NotifiedAt == nil {
		t.Error("notified_at must be set after emit")
	}
}

// TestNotifyAlreadyNotifiedNoop verifies a task whose notified_at is already set
// (e.g. emitted before a worker restart) is not re-notified.
func TestNotifyAlreadyNotifiedNoop(t *testing.T) {
	db := setupNotifyTestDB(t)
	taskID := seedNotifyTask(t, db, "creator1", []string{"alice"})
	already := time.Now().UTC().Add(-time.Minute)
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("notified_at", already).Error; err != nil {
		t.Fatalf("preset notified_at: %v", err)
	}

	sender := &recordingSender{}
	n := &botNotifier{db: db, sender: sender, fromUID: "bot-uid", fromName: "智能总结"}
	n.NotifyCompleted(taskID)

	if got := atomic.LoadInt32(&sender.calls); got != 0 {
		t.Fatalf("Send called %d times, want 0 for an already-notified task", got)
	}
}
