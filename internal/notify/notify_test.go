//go:build cgo

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	// Fixed clock at 10:00 Asia/Shanghai.
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }
	// Default test sanitizer is identity so existing tests can assert on the
	// exact errMsg they pass in (e.g. "LLM timeout"). Production wires
	// worker.SanitizeErrorForUser via WithErrorSanitizer; the dedicated R3
	// regression tests below opt into that mapping explicitly.
	n.errorSanitizer = func(s string) string { return s }
	return n
}

func sentCard(t *testing.T, msg SendMessageRequest) notifyCard {
	t.Helper()
	if msg.Card == nil {
		t.Fatalf("expected card send, got nil card (payload=%v)", msg.Payload)
	}
	if msg.Payload != nil {
		t.Fatalf("card send must omit payload, got %v", msg.Payload)
	}
	return *msg.Card
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
	card := sentCard(t, msg)
	if card.Kind != model.NotifyKindCompleted || card.TaskNo != "TST-1" || card.Title != "今日群聊" {
		t.Fatalf("unexpected completed card: %+v", card)
	}
	rawCard, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal completed card: %v", err)
	}
	if strings.Contains(string(rawCard), "\"reason\"") {
		t.Fatalf("completed card must omit reason on the wire, got %s", rawCard)
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
	card := sentCard(t, d.sendCalls[0])
	if card.Kind != model.NotifyKindFailed || card.Reason != "LLM timeout" {
		t.Fatalf("failed card missing reason: %+v", card)
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

// TestScheduledAndManual_BothDeliverImmediately confirms the quiet window was
// removed (一期不做): scheduled and manual terminal notifications are both
// delivered immediately, regardless of time of day.
func TestScheduledAndManual_BothDeliverImmediately(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	n.OnTaskTerminal(baseTask(9, model.TriggerScheduled), model.StatusCompleted, "")
	n.OnTaskTerminal(baseTask(10, model.TriggerManual), model.StatusCompleted, "")
	if len(d.sendCalls) != 2 {
		t.Fatalf("scheduled + manual must both deliver immediately, got %d sends", len(d.sendCalls))
	}
}

func TestResolveTarget_DMFallbackForAllOrigins(t *testing.T) {
	n := newTestNotifier(setupNotifyTestDB(t), &fakeDeliverer{}, Config{Enabled: true})
	origins := []int{model.OriginChannelGlobal, model.OriginChannelDM, model.OriginChannelGroup, model.OriginChannelThread}
	for _, o := range origins {
		task := baseTask(1, model.TriggerManual)
		task.OriginChannelType = o
		task.OriginChannelID = "origin-chan"
		tgt, ok := n.creatorTarget(task)
		if !ok {
			t.Fatalf("origin %d: expected resolvable target", o)
		}
		if tgt.ChannelType != WireChannelDM || tgt.ChannelID != "user-1" || tgt.TargetUID != "user-1" {
			t.Fatalf("origin %d: expected creator DM fallback, got %+v", o, tgt)
		}
	}
}

func TestResolveTarget_EmptyCreatorUnresolvable(t *testing.T) {
	n := newTestNotifier(setupNotifyTestDB(t), &fakeDeliverer{}, Config{Enabled: true})
	task := baseTask(1, model.TriggerManual)
	task.CreatorID = ""
	if _, ok := n.creatorTarget(task); ok {
		t.Fatalf("empty creator must be unresolvable")
	}
	// resolveTargets for a non-by-person task with empty creator yields no target.
	if tgts, err := n.resolveTargets(task); err != nil || len(tgts) != 0 {
		t.Fatalf("empty creator non-by-person must yield no targets/no err, got %d err=%v", len(tgts), err)
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
		TaskID:       203,
		NotifyKind:   model.NotifyKindCompleted,
		RecipientUID: "user-1",
		Status:       model.NotifyStatusPending,
		CreatedAt:    stale,
		UpdatedAt:    stale,
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
		TaskID:       204,
		NotifyKind:   model.NotifyKindCompleted,
		RecipientUID: "user-1",
		Status:       model.NotifyStatusPending,
		CreatedAt:    fresh,
		UpdatedAt:    fresh,
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
// InternalNotifyDeliverer tests (发送层：octo-server /v1/internal/notify)
//
// 验证：POST 到 /v1/internal/notify、带 X-Internal-Token 头、body 字段
// (space_id/service/targets 单 uid/actor_uid 空/card)、非 2xx 返 error、
// EnsureFriend no-op。
// ---------------------------------------------------------------------------

// TestInternalNotifyDeliverer_PostsToInternalNotify asserts the request path,
// X-Internal-Token header, and NotifyReq body shape for a single recipient.
// Card sends must omit payload because octo-server owns card layout/deep-link.
func TestInternalNotifyDeliverer_PostsToInternalNotify(t *testing.T) {
	var gotPath, gotToken, gotCT string
	var gotBody notifyReq
	var gotRaw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Internal-Token")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotRaw)
		raw, _ := json.Marshal(gotRaw)
		_ = json.Unmarshal(raw, &gotBody)
		_ = json.NewEncoder(w).Encode(notifyResp{Delivered: []string{"u1"}})
	}))
	defer srv.Close()

	d := NewInternalNotifyDeliverer(srv.URL, "secret-int-token", "https://web.example.com")
	msg := SendMessageRequest{
		ChannelID:   "u1",
		ChannelType: WireChannelDM,
		Card: &notifyCard{
			TaskNo:      "TST-1",
			Kind:        model.NotifyKindCompleted,
			Title:       "空间「研发一组」·今日群聊",
			TimeRange:   "2026-06-25 00:00 ~ 2026-06-25 23:59",
			Members:     3,
			MsgCount:    128,
			GeneratedAt: "2026-06-26 09:30",
		},
	}
	if err := d.SendMessage(context.Background(), "space-9", msg); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if gotPath != "/v1/internal/notify" {
		t.Fatalf("expected path /v1/internal/notify, got %q", gotPath)
	}
	if gotToken != "secret-int-token" {
		t.Fatalf("expected X-Internal-Token header, got %q", gotToken)
	}
	if gotCT != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", gotCT)
	}
	if gotBody.SpaceID != "space-9" {
		t.Fatalf("expected body.space_id=space-9, got %q", gotBody.SpaceID)
	}
	if gotBody.Service != "summary-service" {
		t.Fatalf("expected body.service=summary-service, got %q", gotBody.Service)
	}
	// State machine drives one recipient per call: targets must be a single uid.
	if len(gotBody.Targets) != 1 || gotBody.Targets[0] != "u1" {
		t.Fatalf("expected targets=[u1] (single-recipient granularity), got %v", gotBody.Targets)
	}
	if gotBody.ActorUID != "" {
		t.Fatalf("expected empty actor_uid, got %q", gotBody.ActorUID)
	}
	if _, ok := gotRaw["payload"]; ok {
		t.Fatalf("card request must omit payload, got raw body %s", string(gotRaw["payload"]))
	}
	if gotBody.Card == nil {
		t.Fatalf("expected card in request")
	}
	if gotBody.Card.TaskNo != "TST-1" || gotBody.Card.Kind != model.NotifyKindCompleted || gotBody.Card.Title != "空间「研发一组」·今日群聊" {
		t.Fatalf("unexpected card: %+v", gotBody.Card)
	}
	if gotBody.Card.TimeRange != "2026-06-25 00:00 ~ 2026-06-25 23:59" ||
		gotBody.Card.Members != 3 ||
		gotBody.Card.MsgCount != 128 ||
		gotBody.Card.GeneratedAt != "2026-06-26 09:30" {
		t.Fatalf("card meta mismatch: %+v", gotBody.Card)
	}
	if strings.Contains(string(gotRaw["card"]), "\"reason\"") {
		t.Fatalf("completed card request must omit reason, got %s", gotRaw["card"])
	}
}

// TestInternalNotifyDeliverer_FilteredRecipientReturnsError covers M1: the
// server returns 200 even when the recipient is filtered out (non-member), with
// an empty Delivered list. That is a silent drop — SendMessage must turn it into
// an error so the state machine retries instead of marking sent.
func TestInternalNotifyDeliverer_FilteredRecipientReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notifyResp{
			Delivered: []string{},
			Filtered:  map[string]string{"u1": "not_a_member"},
		})
	}))
	defer srv.Close()

	d := NewInternalNotifyDeliverer(srv.URL, "tok", "")
	err := d.SendMessage(context.Background(), "space-9",
		SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM, Payload: map[string]any{"type": 1, "content": "x"}})
	if err == nil {
		t.Fatalf("filtered recipient (200 + empty delivered) must return error, not silent success")
	}
	if !strings.Contains(err.Error(), "not_a_member") {
		t.Fatalf("expected filtered reason in error, got %v", err)
	}
}

// TestInternalNotifyDeliverer_EmptyDeliveredReturnsError: 200 with empty
// delivered and no filtered entry for the uid is still a drop → error.
func TestInternalNotifyDeliverer_EmptyDeliveredReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notifyResp{Delivered: []string{}})
	}))
	defer srv.Close()

	d := NewInternalNotifyDeliverer(srv.URL, "tok", "")
	err := d.SendMessage(context.Background(), "space-9",
		SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM, Payload: map[string]any{"type": 1, "content": "x"}})
	if err == nil {
		t.Fatalf("empty delivered set must return error")
	}
}

// TestInternalNotifyDeliverer_Non2xxReturnsError asserts a non-2xx response is
// surfaced as an error so the state machine records attempt+last_error and the
// sweep retries (NOT fire-and-forget).
func TestInternalNotifyDeliverer_Non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	d := NewInternalNotifyDeliverer(srv.URL, "tok", "")
	err := d.SendMessage(context.Background(), "space-9",
		SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM, Payload: map[string]any{"content": "x"}})
	if err == nil {
		t.Fatalf("expected error on non-2xx response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to mention HTTP status, got %v", err)
	}
}

// TestInternalNotifyDeliverer_NetworkErrorReturnsError asserts a transport error
// (unreachable server) is returned, driving the retry state machine.
func TestInternalNotifyDeliverer_NetworkErrorReturnsError(t *testing.T) {
	d := NewInternalNotifyDeliverer("http://127.0.0.1:0", "tok", "")
	err := d.SendMessage(context.Background(), "space-9",
		SendMessageRequest{ChannelID: "u1", ChannelType: WireChannelDM, Payload: map[string]any{"content": "x"}})
	if err == nil {
		t.Fatalf("expected error when server is unreachable")
	}
}

// TestInternalNotifyDeliverer_EnsureFriendIsNoop asserts EnsureFriend performs no
// HTTP call and returns nil (internal-notify needs no friend relationship).
func TestInternalNotifyDeliverer_EnsureFriendIsNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewInternalNotifyDeliverer(srv.URL, "tok", "")
	if err := d.EnsureFriend(context.Background(), "space-9", "u1"); err != nil {
		t.Fatalf("EnsureFriend must be a no-op returning nil, got %v", err)
	}
	if called {
		t.Fatalf("EnsureFriend must not make any HTTP call")
	}
}

// TestInternalNotifyDeliverer_SpaceIDFallsBackToPayload asserts that when the
// spaceID arg is empty the deliverer falls back to payload.space_id for the
// required top-level body field.
func TestInternalNotifyDeliverer_SpaceIDFallsBackToPayload(t *testing.T) {
	var gotBody notifyReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(notifyResp{Delivered: []string{"u1"}})
	}))
	defer srv.Close()

	d := NewInternalNotifyDeliverer(srv.URL, "tok", "")
	msg := SendMessageRequest{
		ChannelID:   "u1",
		ChannelType: WireChannelDM,
		Payload:     map[string]any{"content": "x", "space_id": "space-fallback"},
	}
	if err := d.SendMessage(context.Background(), "", msg); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if gotBody.SpaceID != "space-fallback" {
		t.Fatalf("expected body.space_id to fall back to payload.space_id, got %q", gotBody.SpaceID)
	}
}

// ---------------------------------------------------------------------------
// PR#113 notify — card wire shape + 「空间名 + 总结名称」标题
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

// TestBuildCard_CompletedFoldsSpaceAndTitle 注入可控 imDB 让 resolveSpaceName
// 返回已知空间名，断言 card.title 同时携带空间名与总结标题。
func TestBuildCard_CompletedFoldsSpaceAndTitle(t *testing.T) {
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
	card := sentCard(t, d.sendCalls[0])
	if card.Title != "空间「研发一组」·今日群聊" {
		t.Fatalf("completed card title mismatch: %+v", card)
	}
}

// TestBuildCard_CompletedIncludesRichMeta 验证成功通知的完整元信息：
// 时间范围 + 参与成员数 + 消息数量 + 生成时间。
func TestBuildCard_CompletedIncludesRichMeta(t *testing.T) {
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
	card := sentCard(t, d.sendCalls[0])
	if card.Kind != model.NotifyKindCompleted ||
		card.TimeRange != "2026-06-25 00:00 ~ 2026-06-25 23:59" ||
		card.Members != 3 ||
		card.MsgCount != 128 ||
		card.GeneratedAt != "2026-06-26 09:30" {
		t.Fatalf("completed card meta mismatch: %+v", card)
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
	card := sentCard(t, d.sendCalls[0])
	if card.Members != 0 {
		t.Fatalf("single-person task must omit member count; got %+v", card)
	}
}

func TestBuildCard_CompletedOmitsNonPositiveCountsOnWire(t *testing.T) {
	db := setupNotifyTestDB(t)
	if err := db.Exec("CREATE TABLE summary_result (id INTEGER PRIMARY KEY, task_id INTEGER, total_msg_count INTEGER, version INTEGER, generated_at DATETIME)").Error; err != nil {
		t.Fatalf("create summary_result: %v", err)
	}
	if err := db.Exec("CREATE TABLE summary_participant (id INTEGER PRIMARY KEY, task_id INTEGER)").Error; err != nil {
		t.Fatalf("create summary_participant: %v", err)
	}
	db.Exec("INSERT INTO summary_result (task_id, total_msg_count, version, generated_at) VALUES (?,?,?,?)",
		603, -7, 1, time.Date(2026, 6, 26, 9, 30, 0, 0, timezone.Location()))

	d := &fakeDeliverer{}
	n := New(db, nil, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(603, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	card := sentCard(t, d.sendCalls[0])
	if card.Members != 0 || card.MsgCount != 0 {
		t.Fatalf("non-positive counts must stay zero for omitempty, got %+v", card)
	}
	rawCard, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal completed card: %v", err)
	}
	body := string(rawCard)
	if strings.Contains(body, "\"msg_count\"") || strings.Contains(body, "\"members\"") {
		t.Fatalf("completed card must omit non-positive counts on the wire, got %s", body)
	}
}

func TestBuildCard_FailedIncludesSpaceAndReason(t *testing.T) {
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

	card := sentCard(t, d.sendCalls[0])
	if card.Kind != model.NotifyKindFailed || card.Title != "空间「研发一组」·今日群聊" || card.Reason != "LLM timeout" {
		t.Fatalf("failed card mismatch: %+v", card)
	}
}

// TestBuildCard_DegradesWhenIMDBNil 降级路径：imDB 为 nil 时 title 只保留总结名。
func TestBuildCard_DegradesWhenIMDBNil(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := New(db, nil, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(303, model.TriggerManual), model.StatusCompleted, "")

	card := sentCard(t, d.sendCalls[0])
	if card.Title != "今日群聊" {
		t.Fatalf("degraded card title mismatch: %+v", card)
	}
}

// TestBuildCard_DegradesWhenSpaceNotFound 降级路径：imDB 在但查不到该 space_id，
// 同样只保留总结名，且绝不阻断投递。
func TestBuildCard_DegradesWhenSpaceNotFound(t *testing.T) {
	db := setupNotifyTestDB(t)
	imDB := setupIMTestDB(t) // table exists but no matching row
	d := &fakeDeliverer{}
	n := New(db, imDB, d, Config{Enabled: true})
	n.now = func() time.Time { return time.Date(2026, 6, 26, 10, 0, 0, 0, timezone.Location()) }

	n.OnTaskTerminal(baseTask(304, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("delivery must not be blocked by missing space, got %d sends", len(d.sendCalls))
	}
	card := sentCard(t, d.sendCalls[0])
	if card.Title != "今日群聊" {
		t.Fatalf("degraded card title mismatch: %+v", card)
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

// TestDeliver_CardOmitsPayload_WhenSpaceKnown asserts terminal card sends do
// not include the legacy payload object.
func TestDeliver_CardOmitsPayload_WhenSpaceKnown(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	// baseTask carries SpaceID="space-9".
	n.OnTaskTerminal(baseTask(1, model.TriggerManual), model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	_ = sentCard(t, d.sendCalls[0])
}

// TestDeliver_CardOmitsPayload_WhenSpaceEmpty asserts the empty-SpaceID path
// also sends card without payload.
func TestDeliver_CardOmitsPayload_WhenSpaceEmpty(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := baseTask(1, model.TriggerManual)
	task.SpaceID = ""
	n.OnTaskTerminal(task, model.StatusCompleted, "")

	if len(d.sendCalls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(d.sendCalls))
	}
	_ = sentCard(t, d.sendCalls[0])
}

// --- spaceID propagation to the send layer ---
//
// deliver() must pass target.SpaceID to Deliverer.SendMessage; the
// InternalNotifyDeliverer then places it in the required top-level
// NotifyReq.space_id. These tests lock the propagation through both the
// synchronous and sweep-redeliver paths.

// TestDeliver_PassesSpaceIDToSendMessage 断言 deliver()（OnTaskTerminal 路径）把
// target.SpaceID 透传给 Deliverer.SendMessage 的 spaceID 参数。
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
		card := sentCard(t, call)
		for _, m := range rawMarkers {
			if strings.Contains(card.Reason, m) {
				t.Fatalf("send #%d leaked raw marker %q into card reason: %+v", i, m, card)
			}
		}
		if card.Reason != "AI 处理失败，请稍后重试" {
			t.Fatalf("send #%d missing safe failure reason; got %+v", i, card)
		}
	}

	// Final state: sweep succeeded → sent.
	db.Where("task_id = ?", 901).First(&row)
	if row.Status != model.NotifyStatusSent {
		t.Fatalf("after sweep retry expected sent, got %s (attempts=%d)", row.Status, row.AttemptCount)
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

// TestBuildCard_NilSanitizerFallsBackToSafeString asserts the defensive
// fallback if Notifier is constructed without WithErrorSanitizer.
func TestBuildCard_NilSanitizerFallsBackToSafeString(t *testing.T) {
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
	card := sentCard(t, d.sendCalls[0])
	for _, m := range []string{"dial tcp", "10.2.3.4", "postgres://", "secretpw"} {
		if strings.Contains(card.Reason, m) {
			t.Fatalf("nil sanitizer must not leak raw marker %q to card reason: %+v", m, card)
		}
	}
	if card.Reason != "AI 处理失败，请稍后重试" {
		t.Fatalf("nil sanitizer fallback must render the safe default; got %+v", card)
	}
}

// ---------------------------------------------------------------------------
// by-person 多目标逐人 DM (feat/notify-delivery)
//
// completed：给「所有 participant ∪ 发起者 CreatorID」并集去重后逐人各发一条 DM。
// failed：只发发起者一人（不群发，避免噪音）。
// 非 by-person：成功/失败都只回退发起者单 DM（保持现状语义）。
// 幂等：每个收件人独立去重（三元键 task_id+notify_kind+recipient_uid），重试只
// 重发失败的那个人。
// ---------------------------------------------------------------------------

// setupByPersonDB 在通知库上追加 summary_participant 表，供 resolveTargets 查参与人。
func setupByPersonDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupNotifyTestDB(t)
	if err := db.AutoMigrate(&model.SummaryParticipant{}); err != nil {
		t.Fatalf("automigrate participant: %v", err)
	}
	return db
}

func seedParticipants(t *testing.T, db *gorm.DB, taskID int64, uids ...string) {
	t.Helper()
	for _, uid := range uids {
		// Seed as an active (submitted) status so these fan-out/dedup/idempotency
		// tests express "real participants who should receive the DM"; the default
		// zero value is ParticipantPending, which resolveTargets now excludes.
		if err := db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: uid, Status: model.ParticipantSubmitted}).Error; err != nil {
			t.Fatalf("seed participant %s: %v", uid, err)
		}
	}
}

// seedParticipantStatus seeds one participant row with an explicit status,
// so tests can exercise resolveTargets' pending/declined exclusion filter.
func seedParticipantStatus(t *testing.T, db *gorm.DB, taskID int64, uid string, status int) {
	t.Helper()
	if err := db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: uid, Status: status}).Error; err != nil {
		t.Fatalf("seed participant %s (status=%d): %v", uid, status, err)
	}
}

func sentUIDs(d *fakeDeliverer) map[string]int {
	got := make(map[string]int)
	for _, c := range d.sendCalls {
		got[c.ChannelID]++
	}
	return got
}

func byPersonTask(id int64) model.SummaryTask {
	task := baseTask(id, model.TriggerManual)
	task.SummaryMode = model.ModeByPerson
	return task
}

// (a) by-person + 发起者不在 participant 里 → completed 每人各一条、发起者也收到、去重无重复。
func TestOnTaskTerminal_ByPerson_CompletedFansOutToAllPlusCreator(t *testing.T) {
	db := setupByPersonDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := byPersonTask(1001) // creator=user-1
	seedParticipants(t, db, 1001, "p-a", "p-b")

	n.OnTaskTerminal(task, model.StatusCompleted, "")

	got := sentUIDs(d)
	want := map[string]int{"p-a": 1, "p-b": 1, "user-1": 1}
	if len(d.sendCalls) != 3 {
		t.Fatalf("expected 3 sends (2 participants + creator), got %d (%v)", len(d.sendCalls), got)
	}
	for uid, c := range want {
		if got[uid] != c {
			t.Fatalf("expected uid %s to receive %d DM, got %d (all=%v)", uid, c, got[uid], got)
		}
	}
	// One row per (task, completed, uid) — three distinct recipient_uids.
	var cnt int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ? AND notify_kind = ?", 1001, model.NotifyKindCompleted).Count(&cnt)
	if cnt != 3 {
		t.Fatalf("expected 3 notification rows (one per recipient), got %d", cnt)
	}
}

// (a-dedup) 发起者同时也是 participant → 并集去重，只发一条给发起者。
func TestOnTaskTerminal_ByPerson_CreatorAlsoParticipant_Deduped(t *testing.T) {
	db := setupByPersonDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := byPersonTask(1002) // creator=user-1
	seedParticipants(t, db, 1002, "user-1", "p-x")

	n.OnTaskTerminal(task, model.StatusCompleted, "")

	got := sentUIDs(d)
	if len(d.sendCalls) != 2 {
		t.Fatalf("expected 2 sends (deduped creator+participant), got %d (%v)", len(d.sendCalls), got)
	}
	if got["user-1"] != 1 || got["p-x"] != 1 {
		t.Fatalf("expected exactly one DM each to user-1 and p-x, got %v", got)
	}
}

// (b) failed 时只有发起者收到（participant 不收）。
func TestOnTaskTerminal_ByPerson_FailedOnlyCreator(t *testing.T) {
	db := setupByPersonDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := byPersonTask(1003) // creator=user-1
	seedParticipants(t, db, 1003, "p-a", "p-b")

	n.OnTaskTerminal(task, model.StatusFailed, "boom")

	got := sentUIDs(d)
	if len(d.sendCalls) != 1 || got["user-1"] != 1 {
		t.Fatalf("failed must go ONLY to creator, got %d sends (%v)", len(d.sendCalls), got)
	}
	// No participant row should exist for the failed kind.
	var cnt int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ? AND notify_kind = ? AND recipient_uid <> ?", 1003, model.NotifyKindFailed, "user-1").Count(&cnt)
	if cnt != 0 {
		t.Fatalf("failed must not create participant rows, got %d", cnt)
	}
}

// (c) 非 by-person → completed 和 failed 都只发发起者。
func TestOnTaskTerminal_NonByPerson_OnlyCreatorBothKinds(t *testing.T) {
	db := setupByPersonDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	// Even if participant rows exist, a non-by-person task must ignore them.
	seedParticipants(t, db, 1004, "p-a", "p-b")
	task := baseTask(1004, model.TriggerManual) // SummaryMode != ModeByPerson

	n.OnTaskTerminal(task, model.StatusCompleted, "")
	n.OnTaskTerminal(task, model.StatusFailed, "boom")

	got := sentUIDs(d)
	if len(d.sendCalls) != 2 || got["user-1"] != 2 {
		t.Fatalf("non-by-person must send only to creator (completed+failed), got %d sends (%v)", len(d.sendCalls), got)
	}
}

// (d) 三元去重键幂等：同 (task,kind,uid) 重复触发只发一次；不同 uid 各自独立。
func TestOnTaskTerminal_ByPerson_PerRecipientIdempotent(t *testing.T) {
	db := setupByPersonDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true})

	task := byPersonTask(1005) // creator=user-1
	seedParticipants(t, db, 1005, "p-a", "p-b")

	// Fire three times: dedup must collapse to exactly one DM per recipient.
	n.OnTaskTerminal(task, model.StatusCompleted, "")
	n.OnTaskTerminal(task, model.StatusCompleted, "")
	n.OnTaskTerminal(task, model.StatusCompleted, "")

	got := sentUIDs(d)
	if len(d.sendCalls) != 3 {
		t.Fatalf("dedup: expected 3 sends total across 3 recipients, got %d (%v)", len(d.sendCalls), got)
	}
	for _, uid := range []string{"p-a", "p-b", "user-1"} {
		if got[uid] != 1 {
			t.Fatalf("dedup: uid %s must receive exactly 1 DM, got %d (%v)", uid, got[uid], got)
		}
	}
	var cnt int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ?", 1005).Count(&cnt)
	if cnt != 3 {
		t.Fatalf("dedup: expected 3 rows (one per uid), got %d", cnt)
	}
}

// (d-independent) 一个收件人 fail、其余成功：重试只重发失败的那个人，不重发已成功的人。
func TestOnTaskTerminal_ByPerson_OneFailsOthersUnaffected(t *testing.T) {
	db := setupByPersonDB(t)
	// First send fails (sendErrOnce), subsequent succeed. The failed recipient
	// gets status=failed with budget; a second OnTaskTerminal must retry ONLY
	// that recipient and leave the already-sent ones alone.
	d := &fakeDeliverer{sendErrOnce: errors.New("transient blip on first recipient")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := byPersonTask(1006) // creator=user-1
	seedParticipants(t, db, 1006, "p-a", "p-b")

	n.OnTaskTerminal(task, model.StatusCompleted, "") // one of the three fails once

	var failedRows int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ? AND status = ?", 1006, model.NotifyStatusFailed).Count(&failedRows)
	if failedRows != 1 {
		t.Fatalf("expected exactly 1 recipient failed on first pass, got %d", failedRows)
	}
	var sentRows int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ? AND status = ?", 1006, model.NotifyStatusSent).Count(&sentRows)
	if sentRows != 2 {
		t.Fatalf("expected 2 recipients sent on first pass, got %d", sentRows)
	}

	sendsAfterFirst := len(d.sendCalls) // 3 (2 ok + 1 failed attempt)

	// Second pass: only the failed recipient should be retried (one more send);
	// the two already-sent recipients must be skipped (dedup).
	n.OnTaskTerminal(task, model.StatusCompleted, "")

	if len(d.sendCalls) != sendsAfterFirst+1 {
		t.Fatalf("retry must resend ONLY the failed recipient (+1 send); before=%d after=%d", sendsAfterFirst, len(d.sendCalls))
	}
	var allSent int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ? AND status = ?", 1006, model.NotifyStatusSent).Count(&allSent)
	if allSent != 3 {
		t.Fatalf("after retry all 3 recipients must be sent, got %d", allSent)
	}
}

// (e) 单人（非 by-person）fail-before/pass-after 不回归：首投失败→标 failed，
// 预算内二次触发重试成功。等价于历史单目标语义在新签名下不变。
func TestOnTaskTerminal_SinglePerson_FailBeforePassAfter(t *testing.T) {
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{sendErrOnce: errors.New("transient blip")}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := baseTask(1007, model.TriggerManual) // single-person
	n.OnTaskTerminal(task, model.StatusCompleted, "")

	var row model.SummaryNotification
	db.Where("task_id = ? AND recipient_uid = ?", 1007, "user-1").First(&row)
	if row.Status != model.NotifyStatusFailed || row.AttemptCount != 1 {
		t.Fatalf("fail-before: expected failed attempt=1, got status=%s attempt=%d", row.Status, row.AttemptCount)
	}

	n.OnTaskTerminal(task, model.StatusCompleted, "") // retry succeeds
	db.Where("task_id = ? AND recipient_uid = ?", 1007, "user-1").First(&row)
	if row.Status != model.NotifyStatusSent {
		t.Fatalf("pass-after: expected sent, got %s", row.Status)
	}
	if len(d.sendCalls) != 2 {
		t.Fatalf("expected 2 sends (fail + retry), got %d", len(d.sendCalls))
	}
}

// (f) by-person 参与人查询出错时：绝不降级成「只发发起者的假成功」。
// 之前 err 被静默吞掉 → 只 add(CreatorID) → 发起者被标 sent、participant 一行不建、
// sweep 也发现不了，participant 永久漏发。修复后 resolveTargets 返回 err，
// OnTaskTerminal 跳过整轮：不投递、不建任何行（尤其没有 creator 的 sent 行）。
// 用一个缺 summary_participant 表的通知库触发真实查询错误（no such table）。
func TestOnTaskTerminal_ByPerson_ParticipantQueryError_NoPartialSuccess(t *testing.T) {
	// setupNotifyTestDB 只建 summary_notification，故意不建 summary_participant，
	// 让 resolveTargets 的 Find(&parts) 真正报错（而非空结果）。
	db := setupNotifyTestDB(t)
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := byPersonTask(1008) // creator=user-1, ModeByPerson

	n.OnTaskTerminal(task, model.StatusCompleted, "")

	// fail-then-no-partial-success: 一条都不能发，尤其不能发给发起者。
	if len(d.sendCalls) != 0 {
		t.Fatalf("participant query error must skip delivery entirely, got %d sends (%v)", len(d.sendCalls), sentUIDs(d))
	}
	// 一行都不能建：既不能有 creator 的 sent 行，也不能有任何 pending/failed 行。
	var cnt int64
	db.Model(&model.SummaryNotification{}).Where("task_id = ?", 1008).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("participant query error must create NO notification rows (no fake creator success), got %d", cnt)
	}
	var creatorSent int64
	db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND recipient_uid = ? AND status = ?", 1008, "user-1", model.NotifyStatusSent).
		Count(&creatorSent)
	if creatorSent != 0 {
		t.Fatalf("must not produce a creator sent row on participant query error, got %d", creatorSent)
	}
}

// (f-control) 参与人表存在且正常：新 (,, error) 签名不得回归多目标扇出。
// 与 fail 用例对照，确认改动只在"查询出错"分支生效，正常路径依旧全绿。
func TestOnTaskTerminal_ByPerson_ParticipantQueryOK_StillFansOut(t *testing.T) {
	db := setupByPersonDB(t) // 建了 summary_participant 表
	d := &fakeDeliverer{}
	n := newTestNotifier(db, d, Config{Enabled: true, MaxAttempts: 3})

	task := byPersonTask(1009) // creator=user-1
	seedParticipants(t, db, 1009, "p-a", "p-b")

	n.OnTaskTerminal(task, model.StatusCompleted, "")

	got := sentUIDs(d)
	if len(d.sendCalls) != 3 {
		t.Fatalf("normal path must still fan out to 3 recipients, got %d (%v)", len(d.sendCalls), got)
	}
	for _, uid := range []string{"p-a", "p-b", "user-1"} {
		if got[uid] != 1 {
			t.Fatalf("uid %s must receive exactly 1 DM, got %d (%v)", uid, got[uid], got)
		}
	}
}

// (g) by-person 状态过滤：resolveTargets 只 fan-out 真正参与的人。
// pending(0)/declined(2) 成员绝不进扇出（不该收「总结完成」通知），
// accepted(1)/submitted(5) 成员 + 发起者 CreatorID 必须在 targets 里。
// 与生成阶段 (personal_processor.go) 排除 pending/declined 的口径对齐。
// fail-before: 未加 status 过滤时全部 participant 都会被塞进 targets，
// pending/declined 的断言会红；加过滤后变绿。
func TestResolveTargets_ByPerson_ExcludesPendingAndDeclined(t *testing.T) {
	db := setupByPersonDB(t)
	n := newTestNotifier(db, &fakeDeliverer{}, Config{Enabled: true})

	task := byPersonTask(1010)                                                    // creator=user-1, ModeByPerson
	seedParticipantStatus(t, db, 1010, "p-pending", model.ParticipantPending)     // 0 -> excluded
	seedParticipantStatus(t, db, 1010, "p-declined", model.ParticipantDeclined)   // 2 -> excluded
	seedParticipantStatus(t, db, 1010, "p-accepted", model.ParticipantAccepted)   // 1 -> included
	seedParticipantStatus(t, db, 1010, "p-submitted", model.ParticipantSubmitted) // 5 -> included

	targets, err := n.resolveTargets(task)
	if err != nil {
		t.Fatalf("resolveTargets returned err: %v", err)
	}

	got := make(map[string]int)
	for _, tgt := range targets {
		got[tgt.TargetUID]++
	}

	// declined/pending must NOT be fanned out.
	for _, uid := range []string{"p-pending", "p-declined"} {
		if got[uid] != 0 {
			t.Fatalf("uid %s (pending/declined) must be excluded from targets, got %d (all=%v)", uid, got[uid], got)
		}
	}
	// accepted/submitted + creator must be present exactly once.
	for _, uid := range []string{"p-accepted", "p-submitted", "user-1"} {
		if got[uid] != 1 {
			t.Fatalf("uid %s must appear exactly once in targets, got %d (all=%v)", uid, got[uid], got)
		}
	}
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets (accepted + submitted + creator), got %d (%v)", len(targets), got)
	}
}
