// Package notify delivers a terminal-state notification (Completed / Failed)
// for a summary task to its recipients (by-person: participants ∪ creator;
// otherwise the creator) via octo-server internal-notify. It is invoked by the
// worker AFTER a task's terminal status is durably committed to the summary DB.
//
// Design:
//   - Dedup / idempotency via a summary_notification row keyed
//     UNIQUE(task_id, notify_kind, recipient_uid). The first writer INSERTs
//     status='pending' and wins the right to deliver; a duplicate-key error
//     means that recipient is already owned, so we skip. Rows are NEVER deleted.
//     completed and failed are independent kinds, deduped per recipient.
//   - On delivery success: UPDATE status='sent', sent_at=now.
//   - On delivery failure: UPDATE status='failed', last_error, attempt_count+1,
//     with bounded same-row retries (MaxNotifyAttempts). The SECRET token is
//     never written to last_error.
//   - Cancelled tasks are not notified (the caller never calls OnTaskTerminal
//     for Cancelled).
package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

// Config is the subset of application config the notifier needs. It is passed
// explicitly (rather than importing internal/config) so the package stays
// decoupled and trivially testable.
type Config struct {
	Enabled     bool
	MaxAttempts int
}

// Notifier wires the dedup state machine (summary DB) to the bot Deliverer.
type Notifier struct {
	db             *gorm.DB
	imDB           *gorm.DB // shared IM DB, read-only; used to resolve space names. May be nil.
	deliverer      Deliverer
	cfg            Config
	now            func() time.Time    // injectable clock for tests
	errorSanitizer func(string) string // single render-point sanitizer for failure reasons (see WithErrorSanitizer)
}

// New builds a Notifier. A nil deliverer or Enabled=false makes OnTaskTerminal a
// no-op, so callers can wire it unconditionally. imDB is the shared IM DB used
// (read-only) to resolve the space name for notification text; it may be nil, in
// which case the text gracefully degrades to the space-less wording.
func New(db *gorm.DB, imDB *gorm.DB, deliverer Deliverer, cfg Config) *Notifier {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 3
	}
	return &Notifier{
		db:        db,
		imDB:      imDB,
		deliverer: deliverer,
		cfg:       cfg,
		now:       timezone.Now,
	}
}

// WithErrorSanitizer wires a single-render-point sanitizer for failure-reason
// strings. It MUST be set in production: buildText runs every failure reason
// (whether arriving via the synchronous OnTaskTerminal path OR the
// sweep/redeliver path that reloads task.ErrorMessage raw from DB) through this
// sanitizer before composing the user DM. The canonical implementation lives in
// internal/worker.SanitizeErrorForUser (whitelist-maps LLM/timeout/chunk errors
// to safe Chinese strings, everything else → "AI 处理失败，请稍后重试").
//
// If left nil, buildText falls back to a hardcoded safe string for any
// non-empty failure reason — never leaks raw err.Error() to the user DM.
// See PR#113 Jerry-Xin/OctoBoooot R3 blocker: sanitizing only at the worker
// call sites left the sweep redeliver path renderring raw DSN/IP/stack.
func (n *Notifier) WithErrorSanitizer(fn func(string) string) *Notifier {
	if n != nil {
		n.errorSanitizer = fn
	}
	return n
}

// OnTaskTerminal is the single entry point the worker calls right AFTER a task
// reaches a terminal status (Completed / Failed) durably in the DB. errMsg is
// the task's ErrorMessage for a Failed task (ignored for Completed). It is
// best-effort: a delivery failure is logged and persisted, never propagated to
// the worker's completion path.
func (n *Notifier) OnTaskTerminal(task model.SummaryTask, status int, errMsg string) {
	if n == nil || !n.cfg.Enabled || n.deliverer == nil || n.db == nil {
		return
	}

	kind, ok := kindForStatus(status)
	if !ok {
		// Cancelled / non-terminal: never notify.
		return
	}

	// Quiet window removed (一期不做): manual and scheduled notifications are
	// delivered immediately with no time-of-day suppression.

	// completed fans out to every recipient (participants ∪ creator for a
	// by-person task, else just the creator). failed goes ONLY to the creator to
	// avoid broadcasting a failure to participants who never asked. Each recipient
	// runs the claim/deliver/mark state machine on its own (task, kind, uid) row,
	// so one recipient's failure never blocks another's.
	var targets []deliveryTarget
	if kind == model.NotifyKindFailed {
		if t, ok := n.creatorTarget(task); ok {
			targets = []deliveryTarget{t}
		}
	} else {
		// A participant-lookup error must NOT degrade to a creator-only "success":
		// that would fake-deliver to the creator, never build participant rows,
		// and hide the miss from the sweep. Skip this run and let the next
		// OnTaskTerminal trigger / sweep retry a full resolve.
		resolved, err := n.resolveTargets(task)
		if err != nil {
			log.Printf("[notify] task=%d kind=%s: resolveTargets failed, skipping (will retry on next trigger/sweep): %v", task.ID, kind, err)
			return
		}
		targets = resolved
	}
	if len(targets) == 0 {
		log.Printf("[notify] task=%d kind=%s: no deliverable target (empty creator), skipping", task.ID, kind)
		return
	}

	for _, target := range targets {
		n.deliverToRecipient(task, kind, errMsg, target)
	}
}

// deliverToRecipient runs the per-recipient claim/deliver/mark state machine for
// a single (task, kind, recipient_uid). Extracted from OnTaskTerminal so the
// by-person fan-out drives it once per recipient; each call is fully independent
// (its own dedup row, retry budget, and failure handling).
func (n *Notifier) deliverToRecipient(task model.SummaryTask, kind, errMsg string, target deliveryTarget) {
	uid := target.TargetUID

	// 1) Preemptive unique insert — claim the right to deliver this (task, kind, uid).
	claimed, row, err := n.claim(task.ID, kind, uid)
	if err != nil {
		log.Printf("[notify] task=%d kind=%s uid=%s: claim failed: %v", task.ID, kind, uid, err)
		return
	}
	if !claimed {
		// First-delivery is serialized by the unique INSERT, but the retry
		// decision must ALSO be atomic: two runners firing in the same tick
		// (markPersonalFailed notify + scanStuckTasks reload-and-notify on the
		// same task) would otherwise both read the same failed row, both see
		// budget remaining, and both deliver, sending duplicate failure DMs.
		// claimRetry does an atomic CAS UPDATE; only the runner that flips the
		// row to pending (RowsAffected==1) proceeds, the loser skips.
		if row != nil && row.Status == model.NotifyStatusFailed && row.AttemptCount < n.cfg.MaxAttempts {
			won, e := n.claimRetry(task.ID, kind, uid)
			if e != nil {
				log.Printf("[notify] task=%d kind=%s uid=%s: claimRetry failed: %v", task.ID, kind, uid, e)
				return
			}
			if !won {
				return
			}
			log.Printf("[notify] task=%d kind=%s uid=%s: retrying failed row (won retry slot)", task.ID, kind, uid)
		} else {
			return
		}
	}

	// 2) Build + deliver.
	text := n.buildText(task, kind, errMsg)
	deliverErr := n.deliver(task.ID, target, text)

	// 3) Persist outcome.
	if deliverErr == nil {
		n.markSent(task.ID, kind, uid)
		log.Printf("[notify] task=%d kind=%s uid=%s delivered to channel_type=%d", task.ID, kind, uid, target.ChannelType)
		return
	}
	n.markFailed(task.ID, kind, uid, deliverErr)
	log.Printf("[notify] task=%d kind=%s uid=%s delivery failed: %v", task.ID, kind, uid, sanitize(deliverErr.Error()))
}

func kindForStatus(status int) (string, bool) {
	switch status {
	case model.StatusCompleted:
		return model.NotifyKindCompleted, true
	case model.StatusFailed:
		return model.NotifyKindFailed, true
	default:
		return "", false
	}
}

// claim attempts the preemptive INSERT. Returns claimed=true when this run won
// the (task, kind) slot. When the row already exists, claimed=false and the
// existing row is returned so the caller can decide whether retry budget remains.
func (n *Notifier) claim(taskID int64, kind, uid string) (bool, *model.SummaryNotification, error) {
	now := n.now()
	row := &model.SummaryNotification{
		TaskID:       taskID,
		NotifyKind:   kind,
		RecipientUID: uid,
		Status:       model.NotifyStatusPending,
		AttemptCount: 0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// Plain Create: the UNIQUE(task_id, notify_kind, recipient_uid) constraint
	// rejects a duplicate with an error we treat as "this recipient is owned".
	err := n.db.Create(row).Error
	if err == nil {
		return true, row, nil
	}
	if isDuplicateKey(err) {
		var existing model.SummaryNotification
		if e := n.db.Where("task_id = ? AND notify_kind = ? AND recipient_uid = ?", taskID, kind, uid).First(&existing).Error; e != nil {
			return false, nil, e
		}
		return false, &existing, nil
	}
	return false, nil, err
}

// claimRetry atomically reclaims a failed (task, kind) row for one more
// delivery attempt. It flips status failed->pending in a single conditional
// UPDATE guarded on status=failed AND attempt_count<MaxAttempts, so concurrent
// runners race on the row and exactly one gets RowsAffected==1. Returns
// won=true only for that single winner; losers skip to avoid duplicate sends.
func (n *Notifier) claimRetry(taskID int64, kind, uid string) (bool, error) {
	now := n.now()
	res := n.db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND recipient_uid = ? AND status = ? AND attempt_count < ?",
			taskID, kind, uid, model.NotifyStatusFailed, n.cfg.MaxAttempts).
		Updates(map[string]any{
			"status":     model.NotifyStatusPending,
			"updated_at": now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (n *Notifier) deliver(taskID int64, target deliveryTarget, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Light guard against the silent-success trap: for a system-bot DM, if the
	// recipient is NOT an active member of this Space, octo-server strips space_id
	// but still returns 200 — the message is then dropped and the user silently
	// gets nothing while we would mark it "sent". Detect provable non-membership
	// here and fail the delivery explicitly instead. (nil imDB / query error =>
	// allowed; server still enforces.)
	if target.TargetUID != "" && target.SpaceID != "" &&
		!n.recipientIsActiveMember(uint(taskID), target.SpaceID, target.TargetUID) {
		return fmt.Errorf("recipient %s is not an active member of space %s; "+
			"server would strip space_id and drop the notification", target.TargetUID, target.SpaceID)
	}

	// DM 可达前置：先 ensureFriend（幂等，带 space_id 供 server 拼 s{spaceID}_{uid}
	// 白名单 channel），再 sendMessage.
	if target.TargetUID != "" {
		if err := n.deliverer.EnsureFriend(ctx, target.SpaceID, target.TargetUID); err != nil {
			return fmt.Errorf("ensureFriend: %w", err)
		}
	}
	// octo-server identifies a plain-text bot message by its ContentType enum
	// (type=1=Text) carrying the body in "content"; a bare {"text":...} payload is
	// not recognized and renders as an empty/unopenable message. Send the
	// server-recognized shape instead.
	//
	// summary is registered as a system bot, so its messages must carry space_id:
	// octo-server's filterPersonMessagesBySpace drops a system-bot message whose
	// payload has an empty space_id (it would otherwise be unopenable in the UI).
	// Include space_id only when known, to hit the "exact match keep" branch and
	// avoid regressing the empty-SpaceID case.
	payload := map[string]any{"type": 1, "content": text}
	if target.SpaceID != "" {
		payload["space_id"] = target.SpaceID
	}
	msg := SendMessageRequest{
		ChannelID:   target.ChannelID,
		ChannelType: target.ChannelType,
		Payload:     payload,
	}
	if err := n.deliverer.SendMessage(ctx, target.SpaceID, msg); err != nil {
		return fmt.Errorf("sendMessage: %w", err)
	}
	return nil
}

// recipientIsActiveMember reports whether uid is an active member (status=1) of
// an active Space (status=1), mirroring octo-server's space.CheckMembership.
//
// Why this matters: for a system-bot DM, octo-server only adopts the X-Space-ID
// header (and thus injects the authoritative payload.space_id) when the
// RECIPIENT is an active member of that Space. If they are not, the server
// STRIPS space_id yet still returns 200 — the message is then dropped by
// filterPersonMessagesBySpace and the user silently never sees it, while the
// worker would otherwise record it as "sent". Pre-checking here turns that
// silent loss into an explicit delivery failure (retried/visible), instead of a
// false success.
//
// Conservative failure mode: on a nil imDB or a query error we return true
// (do NOT block delivery) — the server-side CheckMembership still guards
// correctness; this pre-check only exists to avoid the silent-success trap when
// we can cheaply prove non-membership.
func (n *Notifier) recipientIsActiveMember(taskID uint, spaceID, uid string) bool {
	if n == nil || n.imDB == nil {
		return true // cannot check → don't block; server still enforces.
	}
	spaceID = strings.TrimSpace(spaceID)
	uid = strings.TrimSpace(uid)
	if spaceID == "" || uid == "" {
		return true // nothing to check against; leave existing behavior.
	}
	var count int
	err := n.imDB.Raw(
		"SELECT COUNT(*) FROM space_member sm "+
			"INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.uid = ? AND sm.space_id = ? AND sm.status = 1",
		uid, spaceID,
	).Scan(&count).Error
	if err != nil {
		log.Printf("[notify] task=%d: recipientIsActiveMember(space=%s,uid=%s) query failed: %v; allowing delivery", taskID, spaceID, uid, err)
		return true // fail-open: don't turn a transient DB error into a drop.
	}
	return count > 0
}

func (n *Notifier) markSent(taskID int64, kind, uid string) {
	now := n.now()
	if err := n.db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND recipient_uid = ?", taskID, kind, uid).
		Updates(map[string]any{
			"status":        model.NotifyStatusSent,
			"sent_at":       now,
			"updated_at":    now,
			"attempt_count": gorm.Expr("attempt_count + 1"),
			"last_error":    nil,
		}).Error; err != nil {
		log.Printf("[notify] task=%d kind=%s uid=%s: markSent failed: %v", taskID, kind, uid, err)
	}
}

func (n *Notifier) markFailed(taskID int64, kind, uid string, cause error) {
	now := n.now()
	le := truncateForLastError(sanitize(cause.Error()))
	if err := n.db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND recipient_uid = ?", taskID, kind, uid).
		Updates(map[string]any{
			"status":        model.NotifyStatusFailed,
			"updated_at":    now,
			"attempt_count": gorm.Expr("attempt_count + 1"),
			"last_error":    le,
		}).Error; err != nil {
		log.Printf("[notify] task=%d kind=%s uid=%s: markFailed failed: %v", taskID, kind, uid, err)
	}
}

// markDeferred removed with the bot-token startup-race path: internal-notify
// authenticates with a static X-Internal-Token, so there is no
// "token not yet provisioned" state. All delivery errors are real failures and
// go through markFailed (attempt+last_error, bounded by MaxAttempts + sweep).

// buildText composes the user-facing notification body. Success carries the
// result link (when a base URL is configured); failure carries the sanitized
// error message.
func (n *Notifier) buildText(task model.SummaryTask, kind, errMsg string) string {
	title := strings.TrimSpace(task.Title)
	space := n.resolveSpaceName(task)
	switch kind {
	case model.NotifyKindCompleted:
		var b strings.Builder
		switch {
		case space != "" && title != "":
			fmt.Fprintf(&b, "你在空间「%s」的总结「%s」已生成完成。", space, title)
		case space != "":
			fmt.Fprintf(&b, "你在空间「%s」的总结已生成完成。", space)
		case title != "":
			fmt.Fprintf(&b, "你的总结「%s」已生成完成。", title)
		default:
			b.WriteString("你的总结已生成完成。")
		}
		// 追加有用的元信息（全部 best-effort，查不到/为零的行直接跳过，绝不阻断通知）：
		// - 时间范围：本次总结覆盖的聊天区间；
		// - 参与成员：多人任务的成员数（单人任务无意义，省略）；
		// - 消息数量：本次总结处理的消息条数；
		// - 生成时间：结果产出的本地时间。
		// 之前这里追加的是「查看结果：<link>」外链，但该链接与前端真实入口
		// （WKApp 内部路由 /summary/detail?taskId=）三重错位、根本打不开，已移除。
		if tr := formatTimeRange(task); tr != "" {
			fmt.Fprintf(&b, "\n时间范围：%s", tr)
		}
		meta := n.resultMeta(task)
		if cnt := n.participantCount(task); cnt > 0 {
			fmt.Fprintf(&b, "\n参与成员：%d 人", cnt)
		}
		if meta.msgCount > 0 {
			fmt.Fprintf(&b, "\n消息数量：%d 条", meta.msgCount)
		}
		if !meta.generatedAt.IsZero() {
			fmt.Fprintf(&b, "\n生成时间：%s", timezone.In(meta.generatedAt).Format("2006-01-02 15:04"))
		}
		return b.String()
	case model.NotifyKindFailed:
		var b strings.Builder
		switch {
		case space != "" && title != "":
			fmt.Fprintf(&b, "你在空间「%s」的总结「%s」生成失败。", space, title)
		case space != "":
			fmt.Fprintf(&b, "你在空间「%s」的总结生成失败。", space)
		case title != "":
			fmt.Fprintf(&b, "你的总结「%s」生成失败。", title)
		default:
			b.WriteString("你的总结生成失败。")
		}
		// Single render-point sanitization: every failure reason — whether arriving
		// via the synchronous worker path (OnTaskTerminal) or the sweep/redeliver
		// path that reloads task.ErrorMessage raw from DB — is scrubbed here before
		// it can reach the user DM. See WithErrorSanitizer / PR#113 R3.
		if reason := strings.TrimSpace(errMsg); reason != "" {
			safe := reason
			if n.errorSanitizer != nil {
				safe = n.errorSanitizer(reason)
			} else {
				// Defensive: never leak raw internals if no sanitizer wired.
				safe = "AI 处理失败，请稍后重试"
			}
			fmt.Fprintf(&b, "\n失败原因：%s", safe)
		}
		return b.String()
	default:
		return ""
	}
}

// resolveSpaceName looks up the human-readable space name for the task's
// SpaceID from the shared IM DB (read-only). It is best-effort and degrades
// gracefully: a nil imDB, empty SpaceID, query error, or no row all yield ""
// so buildText falls back to the space-less wording. It never panics and never
// blocks delivery — any error is swallowed (logged only).
func (n *Notifier) resolveSpaceName(task model.SummaryTask) string {
	if n == nil || n.imDB == nil {
		return ""
	}
	spaceID := strings.TrimSpace(task.SpaceID)
	if spaceID == "" {
		return ""
	}
	var name string
	if err := n.imDB.Raw("SELECT name FROM space WHERE space_id = ? LIMIT 1", spaceID).Scan(&name).Error; err != nil {
		log.Printf("[notify] task=%d: resolveSpaceName(space_id=%s) failed: %v", task.ID, spaceID, err)
		return ""
	}
	return strings.TrimSpace(name)
}

// resultLink builds the result URL from the configured base. Empty base → no link.
// notifyResultMeta carries the best-effort metadata pulled from summary_result
// for a completed task's notification text. Zero values mean "unknown / omit".
type notifyResultMeta struct {
	generatedAt time.Time
	msgCount    int
}

// resultMeta best-effort loads the completed task's summary_result row for the
// generation time + processed message count shown in the success notification.
// It degrades gracefully: nil db, query error, or no row all yield the zero
// value so buildText simply omits those lines. Never panics, never blocks.
func (n *Notifier) resultMeta(task model.SummaryTask) notifyResultMeta {
	if n == nil || n.db == nil {
		return notifyResultMeta{}
	}
	var row struct {
		GeneratedAt   time.Time
		TotalMsgCount int
	}
	// Latest version wins if a result was regenerated. Read-only single row.
	if err := n.db.Raw(
		"SELECT generated_at, total_msg_count FROM summary_result WHERE task_id = ? ORDER BY version DESC, id DESC LIMIT 1",
		task.ID,
	).Scan(&row).Error; err != nil {
		log.Printf("[notify] task=%d: resultMeta query failed: %v", task.ID, err)
		return notifyResultMeta{}
	}
	return notifyResultMeta{generatedAt: row.GeneratedAt, msgCount: row.TotalMsgCount}
}

// participantCount best-effort counts the participants of a by-person task for
// the "参与成员：N 人" line. Returns 0 (line omitted) for single-person tasks,
// nil db, or a query error. Read-only; never panics, never blocks.
func (n *Notifier) participantCount(task model.SummaryTask) int {
	if n == nil || n.db == nil {
		return 0
	}
	var cnt int64
	if err := n.db.Raw(
		"SELECT COUNT(*) FROM summary_participant WHERE task_id = ?",
		task.ID,
	).Scan(&cnt).Error; err != nil {
		log.Printf("[notify] task=%d: participantCount query failed: %v", task.ID, err)
		return 0
	}
	return int(cnt)
}

// formatTimeRange renders the chat window a summary covers as
// "YYYY-MM-DD HH:MM ~ YYYY-MM-DD HH:MM" in the local timezone. Returns "" when
// either bound is zero so buildText omits the line.
func formatTimeRange(task model.SummaryTask) string {
	if task.TimeRangeStart.IsZero() || task.TimeRangeEnd.IsZero() {
		return ""
	}
	start := timezone.In(task.TimeRangeStart).Format("2006-01-02 15:04")
	end := timezone.In(task.TimeRangeEnd).Format("2006-01-02 15:04")
	return start + " ~ " + end
}

// SweepInterval is the recommended cron cadence for Notifier.Sweep. Exposed
// so callers (cmd/summary-worker) don't have to pick a magic interval.
const SweepInterval = 60 * time.Second

// PendingLease is how long a 'pending' row may sit without progress before
// Sweep reclaims it. The worker's synchronous delivery path uses a 15s HTTP
// timeout (see deliver), so anything over ~1 min must mean the worker crashed
// between claim and markSent/markFailed, OR markFailed itself failed (e.g.
// the byte-truncation utf8 wedge that truncateForLastError now prevents — kept
// as belt-and-suspenders for any other write error).
const PendingLease = 5 * time.Minute

// sweepBatchSize caps how many candidate rows Sweep handles per tick so a
// degraded octo-server cannot pile up a single sweep beyond the cron cadence.
const sweepBatchSize = 50

// Sweep is the background recovery pass invoked by the scheduler cron. It
// looks for two classes of rows that the synchronous worker path cannot
// recover on its own:
//
//   - status='failed' AND attempt_count<MaxAttempts: a transient delivery
//     blip dropped the message; retry budget remains but no fresh
//     OnTaskTerminal hook will fire on its own (terminal callbacks are
//     one-shot per task transition). Without this sweep, MaxAttempts is
//     effectively 1 for the common path.
//   - status='pending' AND updated_at<now-PendingLease: a worker crashed (or
//     a write failed) between claim and the markSent/markFailed write.
//     Without this sweep the row would sit at 'pending' forever and the
//     dedup claim would keep skipping the (task, kind) pair.
//
// Both branches use the existing atomic CAS (claimRetry / claimPending) so a
// concurrent OnTaskTerminal cannot double-send: only the runner whose UPDATE
// flips the row to pending (RowsAffected==1) proceeds.
//
// Sweep is best-effort: any per-row error is logged and the loop continues.
// It is safe to call when the notifier is disabled (returns immediately).
func (n *Notifier) Sweep(ctx context.Context) {
	if n == nil || !n.cfg.Enabled || n.deliverer == nil || n.db == nil {
		return
	}
	n.sweepRetryFailed(ctx)
	n.sweepStalePending(ctx)
}

// sweepRetryFailed re-attempts delivery for rows stuck at status='failed'
// with retry budget remaining.
func (n *Notifier) sweepRetryFailed(ctx context.Context) {
	var rows []model.SummaryNotification
	if err := n.db.
		Where("status = ? AND attempt_count < ?", model.NotifyStatusFailed, n.cfg.MaxAttempts).
		Order("updated_at ASC").
		Limit(sweepBatchSize).
		Find(&rows).Error; err != nil {
		log.Printf("[notify] sweep: query failed rows: %v", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		won, err := n.claimRetry(row.TaskID, row.NotifyKind, row.RecipientUID)
		if err != nil {
			log.Printf("[notify] sweep: task=%d kind=%s uid=%s claimRetry: %v", row.TaskID, row.NotifyKind, row.RecipientUID, err)
			continue
		}
		if !won {
			continue // another runner won, or budget exhausted concurrently
		}
		n.redeliver(row.TaskID, row.NotifyKind, row.RecipientUID)
	}
}

// sweepStalePending reclaims rows stuck at status='pending' past the lease
// (worker died or a write step failed between claim and markSent/markFailed).
func (n *Notifier) sweepStalePending(ctx context.Context) {
	cutoff := n.now().Add(-PendingLease)
	var rows []model.SummaryNotification
	if err := n.db.
		Where("status = ? AND updated_at < ? AND attempt_count < ?",
			model.NotifyStatusPending, cutoff, n.cfg.MaxAttempts).
		Order("updated_at ASC").
		Limit(sweepBatchSize).
		Find(&rows).Error; err != nil {
		log.Printf("[notify] sweep: query pending rows: %v", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		won, err := n.claimStalePending(row.TaskID, row.NotifyKind, row.RecipientUID, cutoff)
		if err != nil {
			log.Printf("[notify] sweep: task=%d kind=%s uid=%s claimStalePending: %v", row.TaskID, row.NotifyKind, row.RecipientUID, err)
			continue
		}
		if !won {
			continue
		}
		log.Printf("[notify] sweep: reclaimed stale pending row task=%d kind=%s uid=%s", row.TaskID, row.NotifyKind, row.RecipientUID)
		n.redeliver(row.TaskID, row.NotifyKind, row.RecipientUID)
	}
}

// claimStalePending atomically refreshes a stale 'pending' row's updated_at
// so exactly one sweeper wins the right to redeliver it. Guarded on
// status='pending' AND updated_at<cutoff so concurrent sweepers race and only
// the row flipped by RowsAffected==1 proceeds.
func (n *Notifier) claimStalePending(taskID int64, kind, uid string, cutoff time.Time) (bool, error) {
	now := n.now()
	res := n.db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND recipient_uid = ? AND status = ? AND updated_at < ?",
			taskID, kind, uid, model.NotifyStatusPending, cutoff).
		Update("updated_at", now)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// redeliver performs one delivery attempt for a row whose retry slot we just
// won (claimRetry or claimStalePending). It reloads the SummaryTask so the
// message reflects the durable terminal-state row, then rebuilds the target for
// THIS recipient_uid (not blindly the creator) and mirrors the
// build+deliver+persist sequence from OnTaskTerminal. Any failure persists
// via markFailed and is left for the next sweep tick or until budget runs out.
func (n *Notifier) redeliver(taskID int64, kind, uid string) {
	var task model.SummaryTask
	if err := n.db.First(&task, taskID).Error; err != nil {
		// Task row gone (extremely unusual: rows are never deleted in the
		// happy path). Treat as a permanent delivery failure so attempt_count
		// advances and the row eventually leaves the retry set.
		n.markFailed(taskID, kind, uid, fmt.Errorf("reload task %d: %w", taskID, err))
		return
	}
	target, ok := targetForUID(task, uid)
	if !ok {
		n.markFailed(taskID, kind, uid, errors.New("no deliverable target on reload"))
		return
	}
	errMsg := ""
	if task.ErrorMessage != nil {
		errMsg = *task.ErrorMessage
	}
	text := n.buildText(task, kind, errMsg)
	if deliverErr := n.deliver(taskID, target, text); deliverErr != nil {
		n.markFailed(taskID, kind, uid, deliverErr)
		log.Printf("[notify] sweep: task=%d kind=%s uid=%s delivery failed: %v", taskID, kind, uid, sanitize(deliverErr.Error()))
		return
	}
	n.markSent(taskID, kind, uid)
	log.Printf("[notify] sweep: task=%d kind=%s uid=%s delivered via retry", taskID, kind, uid)
}

// truncateForLastError caps an error string for the VARCHAR(500) last_error
// column. It enforces both a byte cap (≤480 bytes, leaving utf8mb4 headroom)
// AND a rune boundary so a multi-byte codepoint is never sliced through the
// middle — MySQL strict mode rejects invalid UTF-8 with Error 1366, which
// would leave the notification row stuck in 'pending' (no sweep / no retry).
func truncateForLastError(s string) string {
	const maxBytes = 480
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	// Back off until the tail is a valid UTF-8 sequence. utf8.ValidString runs
	// over the whole string, but the only way a prefix becomes invalid is a
	// truncated trailing rune (at most 3 bytes for utf8mb4-safe runes), so this
	// loop terminates in ≤3 iterations.
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// sanitize strips any accidental Bearer token / Authorization material from an
// error string before it is persisted to last_error or logged. Defense in
// depth: the deliverer already keeps the token out of errors. It scans forward
// and advances past each rewritten marker so the inserted "Bearer [REDACTED]"
// is never re-matched (which would loop forever).
func sanitize(s string) string {
	const marker = "bearer "
	const redacted = "Bearer [REDACTED]"
	var b strings.Builder
	lower := strings.ToLower(s)
	for {
		idx := strings.Index(lower, marker)
		if idx < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:idx])
		b.WriteString(redacted)
		rest := s[idx+len(marker):]
		end := strings.IndexAny(rest, " \n\t")
		if end < 0 {
			// Token runs to end of string; drop it entirely.
			break
		}
		// Keep the delimiter and everything after; continue scanning the tail.
		s = rest[end:]
		lower = strings.ToLower(s)
	}
	return b.String()
}
