package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupNotifyTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.SummaryNotification{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// ensureCall records one EnsureFriend invocation.
type ensureCall struct {
	SpaceID string
	UID     string
}

// fakeDeliverer records calls and can be told to fail.
type fakeDeliverer struct {
	mu          sync.Mutex
	ensureCalls []ensureCall
	sendCalls   []SendMessageRequest
	failEnsure  error
	failSend    error
	sendErrOnce error // returned once then cleared (for retry tests)
}

func (f *fakeDeliverer) EnsureFriend(ctx context.Context, spaceID, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls = append(f.ensureCalls, ensureCall{SpaceID: spaceID, UID: uid})
	return f.failEnsure
}

func (f *fakeDeliverer) SendMessage(ctx context.Context, msg SendMessageRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls = append(f.sendCalls, msg)
	if f.sendErrOnce != nil {
		e := f.sendErrOnce
		f.sendErrOnce = nil
		return e
	}
	return f.failSend
}

func baseTask(id int64, trigger int) model.SummaryTask {
	return model.SummaryTask{
		ID:                id,
		TaskNo:            "TST-1",
		Title:             "今日群聊",
		SpaceID:           "space-9",
		CreatorID:         "user-1",
		TriggerType:       trigger,
		OriginChannelType: model.OriginChannelGlobal,
	}
}

func newTestNotifier(db *gorm.DB, d Deliverer, cfg Config) *Notifier {
	n := New(db, d, cfg)
	// Fixed clock at 10:00 Asia/Shanghai (outside the default quiet window).
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }
	return n
}

func TestOnTaskTerminal_CompletedDelivers(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true, WebBaseURL: "https://app.example.com"})

	n.OnTaskTerminal(baseTask(1, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	if len(d.ensureCalls) != 1 || d.ensureCalls[0].UID != "user-1" || d.ensureCalls[0].SpaceID != "space-9" {
		t.Fatalf("expected ensureFriend(space-9, user-1), got %v", d.ensureCalls)
	}
	msg := d.sendCalls[0]
	if msg.ChannelType != WireChannelDM || msg.ChannelID != "user-1" {
		t.Fatalf("expected DM to user-1, got type=%d id=%s", msg.ChannelType, msg.ChannelID)
	}
	text, _ := msg.Payload["text"].(string)
	if !strings.Contains(text, "今日群聊") || !strings.Contains(text, "https://app.example.com/summary/TST-1") {
		t.Fatalf("completed text missing title/link: %q", text)
	}
	// OBO reserved fields must not be present.
	if payloadHasOBOReserved(msg.Payload) {
		t.Fatalf("payload leaked OBO reserved field: %v", msg.Payload)
	}

	var row model.SummaryNotification
	db.Where("task_id = ? AND notify_kind = ?", 1, model.NotifyKindCompleted).First(&row)
	if row.Status != model.NotifyStatusSent || row.SentAt == nil {
		t.Fatalf("expected status=sent with sent_at, got status=%s sent_at=%v", row.Status, row.SentAt)
	}
}

func TestOnTaskTerminal_FailedCarriesReason(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	n.OnTaskTerminal(baseTask(2, model.TriggerManual), model.StatusFailed, "LLM timeout")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	text, _ := d.sendCalls[0].Payload["text"].(string)
	if !strings.Contains(text, "失败") || !strings.Contains(text, "LLM timeout") {
		t.Fatalf("failed text missing reason: %q", text)
	}
}

func TestOnTaskTerminal_DedupSameKindSendsOnce(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := baseTask(3, model.TriggerManual)
	n.OnTaskTerminal(task, model.StatusCompleted, "")
	n.OnTaskTerminal(task, model.StatusCompleted, "") // duplicate
	n.OnTaskTerminal(task, model.StatusCompleted, "") // duplicate

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected exactly 1 send after 3 calls (dedup), got %d", len(d.sendCalls))
	}
	var cnt int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ?", 3).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("expected exactly 1 notification row, got %d", cnt)
	}
}

func TestOnTaskTerminal_CompletedAndFailedAreIndependent(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := baseTask(4, model.TriggerManual)
	n.OnTaskTerminal(task, model.StatusCompleted, "")
	n.OnTaskTerminal(task, model.StatusFailed, "boom")

	if len(d.sendCalls) != 2 {
		t.Fatalf("expected 2 sends (completed + failed), got %d", len(d.sendCalls))
	}
	var cnt int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ?", 4).Count(&cnt)
	if cnt != 2 {
		t.Fatalf("expected 2 notification rows, got %d", cnt)
	}
}

func TestOnTaskTerminal_CancelledNeverNotifies(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	n.OnTaskTerminal(baseTask(5, model.TriggerManual), model.StatusCancelled, "")

	if len(d.sendCalls) != 0 {
		t.Fatalf("cancelled must not notify, got %d sends", len(d.sendCalls))
	}
}

func TestOnTaskTerminal_DisabledIsNoop(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: false})

	n.OnTaskTerminal(baseTask(6, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 0 {
		t.Fatalf("disabled must be no-op, got %d sends", len(d.sendCalls))
	}
}

func TestOnTaskTerminal_FailureMarksFailedWithRetryBudget(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{sendErrOnce: errors.New("network down: Bearer secret-token-123 leaked")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := baseTask(7, model.TriggerManual)
	n.OnTaskTerminal(task, model.StatusFailed, "x")

	var row model.SummaryNotification
	db.Where("task_id = ?", 7).First(&row)
	if row.Status != model.NotifyStatusFailed {
		t.Fatalf("expected status=failed, got %s", row.Status)
	}
	if row.AttemptCount != 1 {
		t.Fatalf("expected attempt_count=1, got %d", row.AttemptCount)
	}
	if row.LastError == nil {
		t.Fatalf("expected last_error set")
	}
	// SECRET must never be persisted to last_error.
	if strings.Contains(*row.LastError, "secret-token-123") {
		t.Fatalf("last_error leaked the bearer token: %q", *row.LastError)
	}
	if !strings.Contains(*row.LastError, "[REDACTED]") {
		t.Fatalf("expected token redaction marker, got %q", *row.LastError)
	}

	// Second call has retry budget (1 < 3) and now succeeds.
	n.OnTaskTerminal(task, model.StatusFailed, "x")
	db.Where("task_id = ?", 7).First(&row)
	if row.Status != model.NotifyStatusSent {
		t.Fatalf("expected retry to send, status=%s", row.Status)
	}
}

func TestOnTaskTerminal_RetryBudgetExhausted(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{failSend: errors.New("always down")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 2})

	task := baseTask(8, model.TriggerManual)
	n.OnTaskTerminal(task, model.StatusFailed, "x") // attempt 1
	n.OnTaskTerminal(task, model.StatusFailed, "x") // attempt 2 -> budget exhausted
	sendsBefore := len(d.sendCalls)
	n.OnTaskTerminal(task, model.StatusFailed, "x") // no budget -> must NOT send again

	if len(d.sendCalls) != sendsBefore {
		t.Fatalf("expected no further sends after budget exhausted; before=%d after=%d", sendsBefore, len(d.sendCalls))
	}
	var row model.SummaryNotification
	db.Where("task_id = ?", 8).First(&row)
	if row.AttemptCount != 2 {
		t.Fatalf("expected attempt_count capped at 2, got %d", row.AttemptCount)
	}
}

func TestQuietWindow_SuppressesScheduledNotManual(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := New(db, d, Config{Enabled: true, QuietStart: "22:00", QuietEnd: "07:00"})
	// Clock at 23:30 Asia/Shanghai (inside the overnight quiet window).
	n.now = func() time.Time { return time.Date(2026, 6, 26, 23, 30, 0, 0, timezone.Location()) }

	// Scheduled task: suppressed.
	n.OnTaskTerminal(baseTask(9, model.TriggerScheduled), model.StatusCompleted, "")
	if len(d.sendCalls) != 0 {
		t.Fatalf("scheduled in quiet window must be suppressed, got %d sends", len(d.sendCalls))
	}
	// No row should be created when suppressed (we never claim).
	var cnt int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ?", 9).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("suppressed notification should not create a row, got %d", cnt)
	}

	// Manual task: never suppressed.
	n.OnTaskTerminal(baseTask(10, model.TriggerManual), model.StatusCompleted, "")
	if len(d.sendCalls) != 1 {
		t.Fatalf("manual task must not be suppressed, got %d sends", len(d.sendCalls))
	}
}

func TestQuietWindow_OutsideWindowDelivers(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true, QuietStart: "22:00", QuietEnd: "07:00"}) // clock 10:00 CST -> outside

	n.OnTaskTerminal(baseTask(11, model.TriggerScheduled), model.StatusCompleted, "")
	if len(d.sendCalls) != 1 {
		t.Fatalf("scheduled outside quiet window must deliver, got %d", len(d.sendCalls))
	}
}

func TestResolveTarget_DMFallbackForAllOrigins(t *testing.T) {
	origins := []int{model.OriginChannelGlobal, model.OriginChannelDM, model.OriginChannelGroup, model.OriginChannelThread}
	for _, o := range origins {
		task := baseTask(1, model.TriggerManual)
		task.OriginChannelType = o
		task.OriginChannelID = "origin-chan"
		tgt, ok := resolveTarget(task)
		if !ok {
			t.Fatalf("origin %d: expected resolvable target", o)
		}
		if tgt.ChannelType != WireChannelDM || tgt.ChannelID != "user-1" || tgt.TargetUID != "user-1" {
			t.Fatalf("origin %d: expected creator DM fallback, got %+v", o, tgt)
		}
	}
}

func TestResolveTarget_EmptyCreatorUnresolvable(t *testing.T) {
	task := baseTask(1, model.TriggerManual)
	task.CreatorID = ""
	if _, ok := resolveTarget(task); ok {
		t.Fatalf("empty creator must be unresolvable")
	}
}

func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"22:00", 22*60 + 0, true},
		{"07:30", 7*60 + 30, true},
		{"00:00", 0, true},
		{"23:59", 23*60 + 59, true},
		{"24:00", 0, false},
		{"22", 0, false},
		{"", 0, false},
		{"aa:bb", 0, false},
		{"12:60", 0, false},
	}
	for _, c := range cases {
		got, ok := parseHHMM(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseHHMM(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestPayloadHasOBOReserved(t *testing.T) {
	if !payloadHasOBOReserved(map[string]any{"__obo_uid": "x"}) {
		t.Errorf("expected __obo_ prefix detected")
	}
	if !payloadHasOBOReserved(map[string]any{"obo_sender": "x"}) {
		t.Errorf("expected obo_ prefix detected")
	}
	if !payloadHasOBOReserved(map[string]any{"actual_sender_uid": "x"}) {
		t.Errorf("expected actual_sender_uid detected")
	}
	if payloadHasOBOReserved(map[string]any{"text": "hello"}) {
		t.Errorf("clean payload flagged as OBO")
	}
}

// --- B2 (PR#113 review P1-2) ---

func TestMarkFailed_TruncatesAtRuneBoundary_NoUTF8Wedge(t *testing.T) {
	// Reviewer P1-2: byte-wise truncation of a CJK error string could sever a
	// multi-byte rune; under MySQL strict mode the resulting invalid UTF-8
	// rejects the UPDATE and the row stays at 'pending' forever.
	// truncateForLastError must produce a valid UTF-8 string and the row must
	// reach 'failed'.
	db := setupNotifyTestDB(t)
	// Long CJK error message: 300 三-byte runes = 900 bytes, well past the
	// 480-byte cap so the byte cut would almost certainly land mid-rune.
	longCJK := strings.Repeat("中", 300)
	d := &fakeDeliverer{failSend: errors.New("octo-server boom: " + longCJK)}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	n.OnTaskTerminal(baseTask(101, model.TriggerManual), model.StatusFailed, "x")

	var row model.SummaryNotification
	if err := db.Where("task_id = ?", 101).First(&row).Error; err != nil {
		t.Fatalf("row not found: %v", err)
	}
	if row.Status != model.NotifyStatusFailed {
		t.Fatalf("expected status=failed (markFailed must not wedge at pending), got %s", row.Status)
	}
	if row.LastError == nil {
		t.Fatalf("expected last_error to be set")
	}
	le := *row.LastError
	if !utf8.ValidString(le) {
		t.Fatalf("last_error must be valid UTF-8 after truncation, got bytes=%q", []byte(le))
	}
	if len(le) > 480 {
		t.Fatalf("last_error must be ≤480 bytes for VARCHAR(500) utf8mb4 headroom, got %d", len(le))
	}
	// Must still carry the original prefix so operators can diagnose.
	// The deliver layer wraps with "sendMessage: "; the prefix must still
	// contain the original error head so operators can diagnose.
	if !strings.Contains(le, "octo-server boom:") {
		t.Fatalf("expected truncated message to keep diagnostic prefix, got %q", le)
	}
}

func TestTruncateForLastError_ShortInputUnchanged(t *testing.T) {
	in := "short ascii error"
	if got := truncateForLastError(in); got != in {
		t.Fatalf("short input must pass through, got %q", got)
	}
	cjk := "失败：LLM 超时"
	if got := truncateForLastError(cjk); got != cjk {
		t.Fatalf("short CJK input must pass through, got %q", got)
	}
}

func TestTruncateForLastError_NeverSplitsRune(t *testing.T) {
	// Build a string whose byte length crosses the 480 cap exactly inside a
	// multi-byte rune so a naive [:480] slice would produce invalid UTF-8.
	// 中 is 3 bytes; placing 160 of them gives 480 bytes (boundary-aligned),
	// then a 4-byte rune (𠮷, U+20BB7) starting at byte 480 ensures the cut
	// would land mid-rune for any cap inside that rune. We assert validity for
	// a sweep of caps around the boundary by repeatedly building inputs that
	// straddle.
	inputs := []string{
		strings.Repeat("中", 160) + "𠮷" + strings.Repeat("a", 100),
		strings.Repeat("a", 479) + "𠮷xx",
		strings.Repeat("a", 478) + "中" + strings.Repeat("b", 100),
		strings.Repeat("a", 477) + "中" + strings.Repeat("b", 100),
	}
	for i, in := range inputs {
		out := truncateForLastError(in)
		if !utf8.ValidString(out) {
			t.Errorf("case %d: output not valid UTF-8: %q", i, out)
		}
		if len(out) > 480 {
			t.Errorf("case %d: output too long: %d bytes", i, len(out))
		}
	}
}

// --- B1 (PR#113 review P1-1) — background sweep ---

func TestSweep_RetriesFailedRowWithBudget(t *testing.T) {
	// Simulates the common case: first OnTaskTerminal sees a transient HTTP
	// failure and leaves the row at status='failed', attempt_count=1. No
	// further OnTaskTerminal will fire (terminal callbacks are one-shot per
	// task transition). Sweep must redeliver and reach status='sent'.
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{sendErrOnce: errors.New("transient blip")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := baseTask(201, model.TriggerManual)
	n.OnTaskTerminal(task, model.StatusFailed, "x")
	var row model.SummaryNotification
	if err := db.Where("task_id = ?", 201).First(&row).Error; err != nil {
		t.Fatalf("row not found: %v", err)
	}
	if row.Status != model.NotifyStatusFailed {
		t.Fatalf("precondition: expected failed after first attempt, got %s", row.Status)
	}

	// Persist the original task so redeliver can reload it.
	if err := db.AutoMigrate(&model.SummaryTask{}); err != nil {
		t.Fatalf("automigrate task: %v", err)
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("save task: %v", err)
	}

	n.Sweep(context.Background())

	db.Where("task_id = ?", 201).First(&row)
	if row.Status != model.NotifyStatusSent {
		t.Fatalf("sweep must redeliver and mark sent, got status=%s attempts=%d", row.Status, row.AttemptCount)
	}
	if len(d.sendCalls) != 2 {
		t.Fatalf("expected 2 send calls (1 initial fail + 1 sweep retry), got %d", len(d.sendCalls))
	}
}

func TestSweep_DoesNotRetryWhenBudgetExhausted(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{failSend: errors.New("always down")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 2})

	task := baseTask(202, model.TriggerManual)
	if err := db.AutoMigrate(&model.SummaryTask{}); err != nil {
		t.Fatalf("automigrate task: %v", err)
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("save task: %v", err)
	}

	n.OnTaskTerminal(task, model.StatusFailed, "x") // attempt 1
	n.Sweep(context.Background())                   // attempt 2 -> exhausted
	sendsBefore := len(d.sendCalls)
	n.Sweep(context.Background()) // must not retry

	if len(d.sendCalls) != sendsBefore {
		t.Fatalf("expected no further sends; before=%d after=%d", sendsBefore, len(d.sendCalls))
	}
	var row model.SummaryNotification
	db.Where("task_id = ?", 202).First(&row)
	if row.AttemptCount != 2 {
		t.Fatalf("attempt_count must cap at MaxAttempts=2, got %d", row.AttemptCount)
	}
}

func TestSweep_ReclaimsStalePendingRow(t *testing.T) {
	// Simulates a worker crash between claim() and markSent/markFailed: the
	// row is left at status='pending' and would normally be skipped by every
	// future OnTaskTerminal (dedup) forever. Sweep must reclaim it past the
	// lease and redeliver.
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := baseTask(203, model.TriggerManual)
	if err := db.AutoMigrate(&model.SummaryTask{}); err != nil {
		t.Fatalf("automigrate task: %v", err)
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("save task: %v", err)
	}

	// Inject a stale pending row directly: updated_at well past the lease.
	stale := n.now().Add(-2 * PendingLease)
	if err := db.Create(&model.SummaryNotification{
		TaskID:     203,
		NotifyKind: model.NotifyKindCompleted,
		Status:     model.NotifyStatusPending,
		CreatedAt:  stale,
		UpdatedAt:  stale,
	}).Error; err != nil {
		t.Fatalf("seed stale pending: %v", err)
	}

	n.Sweep(context.Background())

	var row model.SummaryNotification
	db.Where("task_id = ?", 203).First(&row)
	if row.Status != model.NotifyStatusSent {
		t.Fatalf("sweep must redeliver stale pending row, got status=%s", row.Status)
	}
	if len(d.sendCalls) != 1 {
		t.Fatalf("expected exactly 1 send on reclaim, got %d", len(d.sendCalls))
	}
}

func TestSweep_DoesNotReclaimFreshPendingRow(t *testing.T) {
	// A fresh pending row (worker still trying) must not be stolen by Sweep.
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := baseTask(204, model.TriggerManual)
	if err := db.AutoMigrate(&model.SummaryTask{}); err != nil {
		t.Fatalf("automigrate task: %v", err)
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("save task: %v", err)
	}

	fresh := n.now() // just claimed
	if err := db.Create(&model.SummaryNotification{
		TaskID:     204,
		NotifyKind: model.NotifyKindCompleted,
		Status:     model.NotifyStatusPending,
		CreatedAt:  fresh,
		UpdatedAt:  fresh,
	}).Error; err != nil {
		t.Fatalf("seed fresh pending: %v", err)
	}

	n.Sweep(context.Background())

	if len(d.sendCalls) != 0 {
		t.Fatalf("fresh pending row must not be reclaimed, got %d sends", len(d.sendCalls))
	}
	var row model.SummaryNotification
	db.Where("task_id = ?", 204).First(&row)
	if row.Status != model.NotifyStatusPending {
		t.Fatalf("expected status=pending preserved, got %s", row.Status)
	}
}

func TestSweep_DisabledIsNoop(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := New(db, d, Config{Enabled: false})
	n.Sweep(context.Background()) // must not panic / not query
	if len(d.sendCalls) != 0 {
		t.Fatalf("disabled notifier sweep must be no-op")
	}
}
