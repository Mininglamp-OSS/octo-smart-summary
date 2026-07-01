package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	sendSpaceID []string // spaceID passed to each SendMessage (parallel to sendCalls)
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

func (f *fakeDeliverer) SendMessage(ctx context.Context, spaceID string, msg SendMessageRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls = append(f.sendCalls, msg)
	f.sendSpaceID = append(f.sendSpaceID, spaceID)
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
	n := New(db, nil, d, cfg)
	// Fixed clock at 10:00 Asia/Shanghai (outside the default quiet window).
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }
	// Default test sanitizer is identity so existing tests can assert on the
	// exact errMsg they pass in (e.g. "LLM timeout"). Production wires
	// worker.SanitizeErrorForUser via WithErrorSanitizer; the dedicated R3
	// regression tests below opt into that mapping explicitly.
	n.errorSanitizer = func(s string) string { return s }
	return n
}

func TestOnTaskTerminal_CompletedDelivers(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

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
	text, _ := msg.Payload["content"].(string)
	if !strings.Contains(text, "今日群聊") {
		t.Fatalf("completed text missing title: %q", text)
	}
	// The unreachable WKApp-internal result link was removed (see notify.go
	// buildText); success notifications must NOT carry a browser URL anymore.
	if strings.Contains(text, "http://") || strings.Contains(text, "https://") || strings.Contains(text, "查看结果") {
		t.Fatalf("completed text must not carry a result link anymore: %q", text)
	}
	// octo-server recognizes a plain-text bot message by ContentType type=1 (Text)
	// carrying "content"; a bare {"text":...} renders empty. Assert the wire shape.
	if tp, _ := msg.Payload["type"].(int); tp != 1 {
		t.Fatalf("expected payload type=1 (Text), got %v", msg.Payload["type"])
	}
	if _, ok := msg.Payload["text"]; ok {
		t.Fatalf("payload must not carry legacy 'text' key, got %v", msg.Payload)
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
	text, _ := d.sendCalls[0].Payload["content"].(string)
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
	n := New(db, nil, d, Config{Enabled: true, QuietStart: "22:00", QuietEnd: "07:00"})
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
	n := New(db, nil, d, Config{Enabled: false})
	n.Sweep(context.Background()) // must not panic / not query
	if len(d.sendCalls) != 0 {
		t.Fatalf("disabled notifier sweep must be no-op")
	}
}

// ---------------------------------------------------------------------------
// HTTPDeliverer lazy-token tests (启动顺序竞态修复 / OCT-5)
//
// 这些测试验证：bot token 在投递时 lazy 解析（而非启动时固化），空值不缓存使得
// server 晚起后能最终恢复，非空值缓存命中后不再调用 tokenFn，且 lazy 解析到的
// token 确实出现在 Authorization 头里。
// ---------------------------------------------------------------------------

// TestHTTPDeliverer_LazyToken_EmptyThenNonEmpty_NotCachedRecovers 模拟 worker 先起、
// server 晚写 token：tokenFn 首次返回空（投递失败），第二次返回非空（投递成功）。
// 验证 deliverer 不缓存空值，第二次能拿到 token —— 即启动顺序竞态被修复，无需重启。
func TestHTTPDeliverer_LazyToken_EmptyThenNonEmpty_NotCachedRecovers(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var calls int32
	tokenFn := func() (string, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return "", nil // server 尚未写库
		}
		return "late-token", nil // server 已写库
	}

	d := NewHTTPDelivererWithTokenFunc(srv.URL, tokenFn)

	// 首次投递：token 为空 → 应失败（best-effort 失败处理会吞掉），不 panic。
	err := d.SendMessage(context.Background(), "", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM})
	if err == nil {
		t.Fatalf("expected first delivery to fail because token is empty")
	}

	// 第二次投递：token 已可用 → 应成功，且不缓存上一次的空值。
	if err := d.SendMessage(context.Background(), "", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
		t.Fatalf("expected second delivery to succeed after token became available, got: %v", err)
	}
	if gotAuth != "Bearer late-token" {
		t.Fatalf("expected Authorization to carry lazily-resolved token, got %q", gotAuth)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected tokenFn called twice (empty not cached), got %d", got)
	}
}

// TestHTTPDeliverer_LazyToken_AlwaysEmpty_FailsNoPanic 验证 tokenFn 持续返回空时，
// post 返回错误而非 panic；上层 best-effort 失败处理负责吞掉。
func TestHTTPDeliverer_LazyToken_AlwaysEmpty_FailsNoPanic(t *testing.T) {
	tokenFn := func() (string, error) { return "", nil }
	d := NewHTTPDelivererWithTokenFunc("http://127.0.0.1:0", tokenFn)

	err := d.SendMessage(context.Background(), "", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM})
	if err == nil {
		t.Fatalf("expected error when token stays empty")
	}
	// 再调一次仍应失败（空值不缓存、不永久禁用）。
	if err := d.EnsureFriend(context.Background(), "s1", "u1"); err == nil {
		t.Fatalf("expected error on EnsureFriend when token stays empty")
	}
}

// TestHTTPDeliverer_LazyToken_TokenFnError_Propagates 验证 tokenFn 返回 error 时
// 投递失败、错误传播，不缓存、不 panic（模拟 imDB 查询出错）。
func TestHTTPDeliverer_LazyToken_TokenFnError_Propagates(t *testing.T) {
	tokenFn := func() (string, error) { return "", errors.New("db down") }
	d := NewHTTPDelivererWithTokenFunc("http://127.0.0.1:0", tokenFn)

	err := d.SendMessage(context.Background(), "", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM})
	if err == nil {
		t.Fatalf("expected error when tokenFn errors")
	}
}

// TestHTTPDeliverer_LazyToken_NonEmptyCached_TokenFnNotReinvoked 验证缓存命中：
// tokenFn 返回非空后被缓存，后续投递不再调用 tokenFn（用计数器断言）。
func TestHTTPDeliverer_LazyToken_NonEmptyCached_TokenFnNotReinvoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var calls int32
	tokenFn := func() (string, error) {
		atomic.AddInt32(&calls, 1)
		return "cached-token", nil
	}
	d := NewHTTPDelivererWithTokenFunc(srv.URL, tokenFn)

	for i := 0; i < 5; i++ {
		if err := d.SendMessage(context.Background(), "", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
			t.Fatalf("delivery %d failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected tokenFn invoked once (cached afterwards), got %d", got)
	}
}

// TestHTTPDeliverer_BackwardCompat_FixedToken 验证旧构造 NewHTTPDeliverer 仍可用，
// 固定 token 出现在 Authorization 头里（向后兼容）。
func TestHTTPDeliverer_BackwardCompat_FixedToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewHTTPDeliverer(srv.URL, "fixed-secret")
	if err := d.SendMessage(context.Background(), "", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
		t.Fatalf("backward-compat delivery failed: %v", err)
	}
	if gotAuth != "Bearer fixed-secret" {
		t.Fatalf("expected fixed token in Authorization, got %q", gotAuth)
	}
}

// TestHTTPDeliverer_SendMessage_AttachesXSpaceIDHeader asserts the X-Space-ID
// header carries the spaceID on the wire. octo-server STRIPS client
// payload.space_id for a system-bot DM and only trusts the value resolved from
// the X-Space-ID header (adopted when the recipient is an active member of that
// space). Without this header the notification is filtered out as a system-bot
// message with no space_id. Empty spaceID must NOT send the header.
func TestHTTPDeliverer_SendMessage_AttachesXSpaceIDHeader(t *testing.T) {
	var gotHeader string
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Space-ID")
		_, present = r.Header["X-Space-Id"] // canonical MIME key for X-Space-ID
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := NewHTTPDeliverer(srv.URL, "tok")

	// non-empty spaceID → header present with that value.
	if err := d.SendMessage(context.Background(), "space-9", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if !present || gotHeader != "space-9" {
		t.Fatalf("expected X-Space-ID=space-9, present=%v got=%q", present, gotHeader)
	}

	// empty spaceID → header absent (avoid sending a meaningless/forgeable hint).
	gotHeader, present = "", false
	if err := d.SendMessage(context.Background(), "", SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if present {
		t.Fatalf("expected no X-Space-ID header when spaceID empty, got %q", gotHeader)
	}
}

// ---------------------------------------------------------------------------
// PR#113 notify — payload wire shape + 「空间名 + 总结名称」文本
// ---------------------------------------------------------------------------

// setupIMTestDB builds an in-memory IM DB with just the `space` table that
// resolveSpaceName reads (read-only raw SELECT). Schema is NOT owned by this
// service — created here only to back the unit test.
func setupIMTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	if err := db.Exec("CREATE TABLE space (space_id TEXT PRIMARY KEY, name TEXT)").Error; err != nil {
		t.Fatalf("create space table: %v", err)
	}
	return db
}

// TestBuildText_CompletedIncludesSpaceAndTitle 注入可控 imDB 让 resolveSpaceName
// 返回已知空间名，断言成功文本同时含「空间「<名>」」与「总结「<Title>」」。
func TestBuildText_CompletedIncludesSpaceAndTitle(t *testing.T) {
	db := setupNotifyTestDB(t)
	imDB := setupIMTestDB(t)
	if err := imDB.Exec("INSERT INTO space (space_id, name) VALUES (?, ?)", "space-9", "研发一组").Error; err != nil {
		t.Fatalf("seed space: %v", err)
	}
	d := &fakeDeliverer{}
	n := New(db, imDB, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(301, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	text, _ := d.sendCalls[0].Payload["content"].(string)
	if !strings.Contains(text, "空间「研发一组」") {
		t.Fatalf("completed text missing space name: %q", text)
	}
	if !strings.Contains(text, "总结「今日群聊」") {
		t.Fatalf("completed text missing title: %q", text)
	}
	if strings.Contains(text, "http://") || strings.Contains(text, "https://") || strings.Contains(text, "查看结果") {
		t.Fatalf("completed text must not carry a result link anymore: %q", text)
	}
}

// TestBuildText_CompletedIncludesRichMeta 验证成功通知追加的完整元信息：
// 时间范围 + 参与成员数 + 消息数量 + 生成时间（替代已删除的不可达链接）。
// 都是 best-effort，此处 seed 齐数据确认都渲染出来。
func TestBuildText_CompletedIncludesRichMeta(t *testing.T) {
	db := setupNotifyTestDB(t)
	// Seed the read-only source tables buildText best-effort queries.
	if err := db.Exec("CREATE TABLE summary_result (id INTEGER PRIMARY KEY, task_id INTEGER, total_msg_count INTEGER, version INTEGER, generated_at DATETIME)").Error; err != nil {
		t.Fatalf("create summary_result: %v", err)
	}
	if err := db.Exec("CREATE TABLE summary_participant (id INTEGER PRIMARY KEY, task_id INTEGER)").Error; err != nil {
		t.Fatalf("create summary_participant: %v", err)
	}
	gen := time.Date(2026, 6, 26, 9, 30, 0, 0, timezone.Location())
	db.Exec("INSERT INTO summary_result (task_id, total_msg_count, version, generated_at) VALUES (?,?,?,?)", 601, 128, 1, gen)
	db.Exec("INSERT INTO summary_participant (task_id) VALUES (?),(?),(?)", 601, 601, 601)

	d := &fakeDeliverer{}
	n := New(db, nil, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	task := baseTask(601, model.TriggerManual)
	task.TimeRangeStart = time.Date(2026, 6, 25, 0, 0, 0, 0, timezone.Location())
	task.TimeRangeEnd = time.Date(2026, 6, 25, 23, 59, 0, 0, timezone.Location())

	n.OnTaskTerminal(task, model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	text, _ := d.sendCalls[0].Payload["content"].(string)
	for _, want := range []string{
		"已生成完成",
		"时间范围：2026-06-25 00:00 ~ 2026-06-25 23:59",
		"参与成员：3 人",
		"消息数量：128 条",
		"生成时间：2026-06-26 09:30",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("completed text missing %q; full text:\n%s", want, text)
		}
	}
	if strings.Contains(text, "http://") || strings.Contains(text, "https://") {
		t.Fatalf("completed text must not carry a link: %q", text)
	}
}

// TestParticipantCount_SinglePersonOmitted 单人任务（无 participant 行）不渲染
// 「参与成员」行；meta 表不存在时也优雅降级不崩。
func TestParticipantCount_SinglePersonOmitted(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := New(db, nil, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(602, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send (best-effort must not block), got %d", len(d.sendCalls))
	}
	text, _ := d.sendCalls[0].Payload["content"].(string)
	if strings.Contains(text, "参与成员") {
		t.Fatalf("single-person task must omit member line; got %q", text)
	}
}
func TestBuildText_FailedIncludesSpaceAndReason(t *testing.T) {
	db := setupNotifyTestDB(t)
	imDB := setupIMTestDB(t)
	if err := imDB.Exec("INSERT INTO space (space_id, name) VALUES (?, ?)", "space-9", "研发一组").Error; err != nil {
		t.Fatalf("seed space: %v", err)
	}
	d := &fakeDeliverer{}
	n := New(db, imDB, d, Config{Enabled: true})
	// Identity sanitizer: this test asserts space name + verbatim reason are
	// composed together; it is not exercising the R3 scrub (covered separately).
	n.errorSanitizer = func(s string) string { return s }
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(302, model.TriggerManual), model.StatusFailed, "LLM timeout")

	text, _ := d.sendCalls[0].Payload["content"].(string)
	if !strings.Contains(text, "空间「研发一组」") {
		t.Fatalf("failed text missing space name: %q", text)
	}
	if !strings.Contains(text, "总结「今日群聊」") || !strings.Contains(text, "失败") {
		t.Fatalf("failed text missing title/失败: %q", text)
	}
	if !strings.Contains(text, "LLM timeout") {
		t.Fatalf("failed text missing reason: %q", text)
	}
}

// TestBuildText_DegradesWhenIMDBNil 降级路径：imDB 为 nil 时退回不带空间名的旧文案。
func TestBuildText_DegradesWhenIMDBNil(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := New(db, nil, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(303, model.TriggerManual), model.StatusCompleted, "")

	text, _ := d.sendCalls[0].Payload["content"].(string)
	if strings.Contains(text, "空间「") {
		t.Fatalf("nil imDB must degrade to space-less wording, got %q", text)
	}
	if text != "你的总结「今日群聊」已生成完成。" {
		t.Fatalf("degraded text mismatch: %q", text)
	}
}

// TestBuildText_DegradesWhenSpaceNotFound 降级路径：imDB 在但查不到该 space_id，
// 同样退回旧文案，且绝不阻断投递。
func TestBuildText_DegradesWhenSpaceNotFound(t *testing.T) {
	db := setupNotifyTestDB(t)
	imDB := setupIMTestDB(t) // table exists but no matching row
	d := &fakeDeliverer{}
	n := New(db, imDB, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(304, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("delivery must not be blocked by missing space, got %d sends", len(d.sendCalls))
	}
	text, _ := d.sendCalls[0].Payload["content"].(string)
	if strings.Contains(text, "空间「") {
		t.Fatalf("missing space row must degrade to space-less wording, got %q", text)
	}
	if !strings.Contains(text, "你的总结「今日群聊」已生成完成。") {
		t.Fatalf("degraded text mismatch: %q", text)
	}
}

// TestResolveSpaceName_NilReceiverAndNilDB 直接覆盖 resolveSpaceName 的 nil 防护。
func TestResolveSpaceName_NilReceiverAndNilDB(t *testing.T) {
	var nilN *Notifier
	if got := nilN.resolveSpaceName(baseTask(1, model.TriggerManual)); got != "" {
		t.Fatalf("nil receiver must return empty, got %q", got)
	}
	n := New(setupNotifyTestDB(t), nil, &fakeDeliverer{}, Config{Enabled: true})
	if got := n.resolveSpaceName(baseTask(1, model.TriggerManual)); got != "" {
		t.Fatalf("nil imDB must return empty, got %q", got)
	}
	// Empty SpaceID short-circuits even with a live imDB.
	n2 := New(setupNotifyTestDB(t), setupIMTestDB(t), &fakeDeliverer{}, Config{Enabled: true})
	task := baseTask(1, model.TriggerManual)
	task.SpaceID = ""
	if got := n2.resolveSpaceName(task); got != "" {
		t.Fatalf("empty space_id must return empty, got %q", got)
	}
}

// --- space_id in payload (system-bot space filter fix) ---

// TestDeliver_PayloadCarriesSpaceID_WhenKnown asserts the delivered payload
// includes space_id when the target SpaceID is non-empty. Summary runs as a
// system bot; octo-server's filterPersonMessagesBySpace drops a system-bot
// message whose payload has an empty space_id, so it must be carried through.
// fail-before: deliver previously built the payload without space_id, so this
// key assertion would have failed.
func TestDeliver_PayloadCarriesSpaceID_WhenKnown(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	// baseTask carries SpaceID="space-9".
	n.OnTaskTerminal(baseTask(1, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	got, ok := d.sendCalls[0].Payload["space_id"]
	if !ok {
		t.Fatalf("payload must carry space_id when target.SpaceID is set, got %v", d.sendCalls[0].Payload)
	}
	if got != "space-9" {
		t.Fatalf("expected space_id=space-9, got %v", got)
	}
}

// TestDeliver_PayloadOmitsSpaceID_WhenEmpty asserts the payload does NOT
// include a space_id key when the target SpaceID is empty, avoiding a
// regression on the empty-SpaceID path.
func TestDeliver_PayloadOmitsSpaceID_WhenEmpty(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := baseTask(1, model.TriggerManual)
	task.SpaceID = ""
	n.OnTaskTerminal(task, model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	if _, ok := d.sendCalls[0].Payload["space_id"]; ok {
		t.Fatalf("payload must not carry space_id when target.SpaceID is empty, got %v", d.sendCalls[0].Payload)
	}
}

// --- X-Space-ID header (system-bot DM fail-closed space resolution fix) ---
//
// 真根因：summary 是系统 bot，octo-server 对系统 bot 的 DM fail-closed：client 在
// payload 里传的 space_id 一律被 strip，server 只认自己解析的权威 space_id。解析
// 路径上 worker 唯一可控的输入是 HTTP 请求头 X-Space-ID —— 当接收人是该 space 活跃
// 成员（CheckMembership=true）时 server 采纳该头并注入权威 payload.space_id。因此
// worker 发 sendMessage / ensureFriend 必须带 X-Space-ID 头，光改 payload 无效。

// TestDeliver_PassesSpaceIDToSendMessage 断言 deliver()（OnTaskTerminal 路径）把
// target.SpaceID 透传给 Deliverer.SendMessage 的 spaceID 参数 —— 这是 X-Space-ID
// 头值的来源。fail-before：旧 SendMessage 签名没有 spaceID 参数，无从透传。
func TestDeliver_PassesSpaceIDToSendMessage(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	// baseTask carries SpaceID="space-9".
	n.OnTaskTerminal(baseTask(1, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendSpaceID) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendSpaceID))
	}
	if d.sendSpaceID[0] != "space-9" {
		t.Fatalf("expected spaceID=space-9 passed to SendMessage, got %q", d.sendSpaceID[0])
	}
}

// TestRedeliver_PassesSpaceIDToSendMessage 确认 redeliver（sweep 重投递）路径同样
// 经过 deliver→SendMessage，因此也带上权威 spaceID。
func TestRedeliver_PassesSpaceIDToSendMessage(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{sendErrOnce: errors.New("transient blip")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := baseTask(401, model.TriggerManual)
	n.OnTaskTerminal(task, model.StatusFailed, "x") // first attempt fails, row -> failed

	if err := db.AutoMigrate(&model.SummaryTask{}); err != nil {
		t.Fatalf("automigrate task: %v", err)
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("save task: %v", err)
	}

	n.Sweep(context.Background()) // redeliver path

	if len(d.sendSpaceID) != 2 {
		t.Fatalf("expected 2 sends (initial + sweep redeliver), got %d", len(d.sendSpaceID))
	}
	for i, sid := range d.sendSpaceID {
		if sid != "space-9" {
			t.Fatalf("send #%d: expected spaceID=space-9 (redeliver must carry it too), got %q", i, sid)
		}
	}
}

// --- R3 (PR#113 Jerry-Xin/OctoBoooot) — sweep redeliver MUST sanitize ---
//
// Regression test for the blocker reported on head fede1ab5: the sweep path
// reloaded task.ErrorMessage raw from DB and rendered it into the user DM,
// bypassing the worker-side SanitizeErrorForUser scrub applied only at
// OnTaskTerminal call sites. We now sanitize at a single render point in
// buildText, and the production wiring (cmd/summary-worker/main.go) injects
// worker.SanitizeErrorForUser via WithErrorSanitizer. This test asserts that
// the sanitizer is actually invoked on the sweep redeliver path — i.e. raw
// internal substrings never reach the deliverer.
func TestSweep_RedeliverSanitizesRawError(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{sendErrOnce: errors.New("transient blip")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	// Wire a strict sanitizer mimicking worker.SanitizeErrorForUser: anything
	// containing raw markers maps to a fixed safe string. We can't import the
	// worker package here (import cycle), so we cover the contract: the render
	// point invokes the injected sanitizer, raw markers never reach the DM.
	rawMarkers := []string{"dial tcp", "10.2.3.4", "postgres://", "goroutine", "secretpw"}
	sanitizerCalls := 0
	n.errorSanitizer = func(s string) string {
		sanitizerCalls++
		for _, m := range rawMarkers {
			if strings.Contains(s, m) {
				return "AI 处理失败，请稍后重试"
			}
		}
		return s
	}

	// Raw err in the shape the reviewer flagged: DSN + IP + credential + stack head.
	rawErr := "dial tcp 10.2.3.4:5432: connect: connection refused (dsn=postgres://user:secretpw@10.2.3.4:5432/db) goroutine 1 [running]"
	task := baseTask(901, model.TriggerManual)
	task.ErrorMessage = &rawErr
	if err := db.AutoMigrate(&model.SummaryTask{}); err != nil {
		t.Fatalf("automigrate task: %v", err)
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("save task: %v", err)
	}

	// First call: synchronous path. Even on the immediate hop the new
	// single-render-point sanitize runs, so the synchronous send (which fails
	// once via sendErrOnce) must not leak the raw err either.
	n.OnTaskTerminal(task, model.StatusFailed, rawErr)

	var row model.SummaryNotification
	if err := db.Where("task_id = ?", 901).First(&row).Error; err != nil {
		t.Fatalf("row not found: %v", err)
	}
	if row.Status != model.NotifyStatusFailed {
		t.Fatalf("precondition: expected failed after first send, got %s", row.Status)
	}

	// Sweep — this is the path that historically read task.ErrorMessage raw
	// and rendered it unsanitized into the DM.
	n.Sweep(context.Background())

	if len(d.sendCalls) != 2 {
		t.Fatalf("expected 2 send calls (1 initial fail + 1 sweep retry), got %d", len(d.sendCalls))
	}
	if sanitizerCalls < 2 {
		t.Fatalf("sanitizer must be invoked on BOTH the synchronous AND the redeliver render; got calls=%d", sanitizerCalls)
	}
	for i, call := range d.sendCalls {
		text, _ := call.Payload["content"].(string)
		for _, m := range rawMarkers {
			if strings.Contains(text, m) {
				t.Fatalf("send #%d leaked raw marker %q into DM text:\n%s", i, m, text)
			}
		}
		if !strings.Contains(text, "AI 处理失败，请稍后重试") {
			t.Fatalf("send #%d missing safe failure reason; got:\n%s", i, text)
		}
	}

	// Final state: sweep succeeded → sent.
	db.Where("task_id = ?", 901).First(&row)
	if row.Status != model.NotifyStatusSent {
		t.Fatalf("after sweep retry expected sent, got %s (attempts=%d)", row.Status, row.AttemptCount)
	}
}

// TestHTTPDeliverer_SendMessage_SetsXSpaceIDHeader 用 httptest 起 server 捕获请求头，
// 断言 SpaceID 非空时 sendMessage 请求带 X-Space-ID 头且值正确。这是真根因修复的
// 核心断言：server 会 strip payload.space_id，只认这个头。
func TestHTTPDeliverer_SendMessage_SetsXSpaceIDHeader(t *testing.T) {
	var gotSpace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSpace = r.Header.Get("X-Space-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewHTTPDeliverer(srv.URL, "tok")
	if err := d.SendMessage(context.Background(), "space-9",
		SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if gotSpace != "space-9" {
		t.Fatalf("expected X-Space-ID=space-9, got %q", gotSpace)
	}
}

// TestHTTPDeliverer_SendMessage_OmitsXSpaceIDHeader_WhenEmpty 断言 SpaceID 为空时
// 不带 X-Space-ID 头（保持向后兼容，不引入空头）。
func TestHTTPDeliverer_SendMessage_OmitsXSpaceIDHeader_WhenEmpty(t *testing.T) {
	var present bool
	var gotSpace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Space-Id"] // canonical MIME key for X-Space-ID
		gotSpace = r.Header.Get("X-Space-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewHTTPDeliverer(srv.URL, "tok")
	if err := d.SendMessage(context.Background(), "",
		SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if present || gotSpace != "" {
		t.Fatalf("expected NO X-Space-ID header when spaceID empty, got present=%v value=%q", present, gotSpace)
	}
}

// TestHTTPDeliverer_SendMessage_TrimsXSpaceIDHeader 断言头值不带前后空格（即便调用
// 方传入带空格的 spaceID，spaceHeader 也会 TrimSpace，避免引入脏值）。
func TestHTTPDeliverer_SendMessage_TrimsXSpaceIDHeader(t *testing.T) {
	var gotSpace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSpace = r.Header.Get("X-Space-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewHTTPDeliverer(srv.URL, "tok")
	if err := d.SendMessage(context.Background(), "  space-9  ",
		SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if gotSpace != "space-9" {
		t.Fatalf("expected trimmed X-Space-ID=space-9, got %q", gotSpace)
	}
}

// TestHTTPDeliverer_EnsureFriend_SetsXSpaceIDHeader 断言 ensureFriend 也带 X-Space-ID
// 头（同一权威 space 解析路径），SpaceID 为空时不带。
func TestHTTPDeliverer_EnsureFriend_SetsXSpaceIDHeader(t *testing.T) {
	var gotSpace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSpace = r.Header.Get("X-Space-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewHTTPDeliverer(srv.URL, "tok")
	if err := d.EnsureFriend(context.Background(), "space-9", "u1"); err != nil {
		t.Fatalf("ensureFriend failed: %v", err)
	}
	if gotSpace != "space-9" {
		t.Fatalf("expected X-Space-ID=space-9 on ensureFriend, got %q", gotSpace)
	}

	gotSpace = ""
	if err := d.EnsureFriend(context.Background(), "", "u1"); err != nil {
		t.Fatalf("ensureFriend (empty space) failed: %v", err)
	}
	if gotSpace != "" {
		t.Fatalf("expected NO X-Space-ID header when spaceID empty, got %q", gotSpace)
	}
}

// ---------------------------------------------------------------------------
// 轻量防护：接收人非该 space 活跃成员时，deliver 应显式失败，而非静默「已发送」
// （octo-server 对系统 bot DM 在接收人非成员时会 strip space_id 但仍返 200，
// 导致消息被丢弃、用户看不到；worker 侧预检把静默丢失转成显式失败。）
// ---------------------------------------------------------------------------

// setupIMTestDBWithMembers 建带 space + space_member 两表的内存 IM 库，用于成员校验。
func setupIMTestDBWithMembers(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	if err := db.Exec("CREATE TABLE space (space_id TEXT PRIMARY KEY, name TEXT, status INTEGER)").Error; err != nil {
		t.Fatalf("create space table: %v", err)
	}
	if err := db.Exec("CREATE TABLE space_member (space_id TEXT, uid TEXT, status INTEGER)").Error; err != nil {
		t.Fatalf("create space_member table: %v", err)
	}
	return db
}

// TestDeliver_ActiveMember_Delivers 接收人是活跃成员 → 正常投递。
func TestDeliver_ActiveMember_Delivers(t *testing.T) {
	db := setupNotifyTestDB(t)
	imDB := setupIMTestDBWithMembers(t)
	imDB.Exec("INSERT INTO space (space_id, name, status) VALUES (?,?,1)", "space-9", "研发一组")
	imDB.Exec("INSERT INTO space_member (space_id, uid, status) VALUES (?,?,1)", "space-9", "user-1")
	d := &fakeDeliverer{}
	n := New(db, imDB, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(501, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("active member: expected 1 send, got %d", len(d.sendCalls))
	}
	var row model.SummaryNotification
	if err := db.Where("task_id=?", 501).First(&row).Error; err != nil {
		t.Fatalf("load notification: %v", err)
	}
	if row.Status != "sent" {
		t.Fatalf("active member: expected status=sent, got %q", row.Status)
	}
}

// TestDeliver_NonMember_FailsNotSilentSent 接收人非活跃成员 → 不发送，记录 failed（非 sent）。
func TestDeliver_NonMember_FailsNotSilentSent(t *testing.T) {
	db := setupNotifyTestDB(t)
	imDB := setupIMTestDBWithMembers(t)
	imDB.Exec("INSERT INTO space (space_id, name, status) VALUES (?,?,1)", "space-9", "研发一组")
	// user-1 有一行但 status=2（被降权/退出）→ 非活跃成员。
	imDB.Exec("INSERT INTO space_member (space_id, uid, status) VALUES (?,?,2)", "space-9", "user-1")
	d := &fakeDeliverer{}
	n := New(db, imDB, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(502, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 0 {
		t.Fatalf("non-member: expected 0 send (blocked), got %d", len(d.sendCalls))
	}
	var row model.SummaryNotification
	if err := db.Where("task_id=?", 502).First(&row).Error; err != nil {
		t.Fatalf("load notification: %v", err)
	}
	if row.Status == "sent" {
		t.Fatalf("non-member: notification wrongly marked sent (silent-success trap not closed)")
	}
}

// TestDeliver_NilIMDB_AllowsDelivery imDB 为 nil → 无法校验 → 放行（不阻断，server 兜底）。
func TestDeliver_NilIMDB_AllowsDelivery(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := New(db, nil, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(503, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("nil imDB: expected 1 send (fail-open), got %d", len(d.sendCalls))
	}
}

// TestBuildText_NilSanitizerFallsBackToSafeString asserts the defensive
// fallback: if Notifier is constructed without WithErrorSanitizer (production
// misconfig), buildText still refuses to render raw err.Error() to the user
// DM. This guards against future call sites accidentally forgetting the wiring.
func TestBuildText_NilSanitizerFallsBackToSafeString(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	// Bypass newTestNotifier so we get a Notifier with nil errorSanitizer.
	n := New(db, nil, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	rawErr := "dial tcp 10.2.3.4: connect refused dsn=postgres://u:secretpw@h/db"
	task := baseTask(902, model.TriggerManual)

	n.OnTaskTerminal(task, model.StatusFailed, rawErr)

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	text, _ := d.sendCalls[0].Payload["content"].(string)
	for _, m := range []string{"dial tcp", "10.2.3.4", "postgres://", "secretpw"} {
		if strings.Contains(text, m) {
			t.Fatalf("nil sanitizer must not leak raw marker %q to DM; text=%q", m, text)
		}
	}
	if !strings.Contains(text, "AI 处理失败，请稍后重试") {
		t.Fatalf("nil sanitizer fallback must render the safe default; text=%q", text)
	}
}
