// Package notify delivers a one-shot terminal-state notification (Completed /
// Failed) for a summary task back to the task creator through the octo-server IM
// bot. It is invoked by the worker AFTER a task's terminal status is durably
// committed to the summary DB.
//
// Design (OCT-4, approved plan):
//   - Dedup / idempotency via a summary_notification row keyed
//     UNIQUE(task_id, notify_kind). The first writer INSERTs status='pending'
//     and wins the right to deliver; a duplicate-key error means another run
//     already owns this (task, kind), so we skip. Rows are NEVER deleted.
//     completed and failed are independent kinds (Leader 拍板选 A：各发一次).
//   - On HTTP success: UPDATE status='sent', sent_at=now.
//   - On HTTP failure: UPDATE status='failed', last_error, attempt_count+1, with
//     bounded same-row retries (MaxNotifyAttempts). The SECRET token is never
//     written to last_error.
//   - Cancelled tasks are not notified (the caller never calls OnTaskTerminal
//     for Cancelled).
//   - Optional quiet window suppresses SCHEDULED-task notifications only;
//     on-demand (manual) tasks are never suppressed, and suppression drops the
//     notification for this run (no 顺延).
package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

// Config is the subset of application config the notifier needs. It is passed
// explicitly (rather than importing internal/config) so the package stays
// decoupled and trivially testable.
type Config struct {
	Enabled     bool
	WebBaseURL  string
	MaxAttempts int
	QuietStart  string // "HH:MM" or ""
	QuietEnd    string // "HH:MM" or ""
}

// Notifier wires the dedup state machine (summary DB) to the bot Deliverer.
type Notifier struct {
	db        *gorm.DB
	deliverer Deliverer
	cfg       Config
	now       func() time.Time // injectable clock for tests
}

// New builds a Notifier. A nil deliverer or Enabled=false makes OnTaskTerminal a
// no-op, so callers can wire it unconditionally.
func New(db *gorm.DB, deliverer Deliverer, cfg Config) *Notifier {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 3
	}
	return &Notifier{
		db:        db,
		deliverer: deliverer,
		cfg:       cfg,
		now:       timezone.Now,
	}
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

	// Quiet window suppresses SCHEDULED notifications only (按需任务永不抑制).
	if task.TriggerType == model.TriggerScheduled && n.inQuietWindow(n.now()) {
		log.Printf("[notify] task=%d kind=%s suppressed by quiet window (scheduled)", task.ID, kind)
		return
	}

	target, ok := resolveTarget(task)
	if !ok {
		log.Printf("[notify] task=%d kind=%s: no deliverable target (empty creator), skipping", task.ID, kind)
		return
	}

	// 1) Preemptive unique insert — claim the right to deliver this (task, kind).
	claimed, row, err := n.claim(task.ID, kind)
	if err != nil {
		log.Printf("[notify] task=%d kind=%s: claim failed: %v", task.ID, kind, err)
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
			won, e := n.claimRetry(task.ID, kind)
			if e != nil {
				log.Printf("[notify] task=%d kind=%s: claimRetry failed: %v", task.ID, kind, e)
				return
			}
			if !won {
				return
			}
			log.Printf("[notify] task=%d kind=%s: retrying failed row (won retry slot)", task.ID, kind)
		} else {
			return
		}
	}

	// 2) Build + deliver.
	text := n.buildText(task, kind, errMsg)
	deliverErr := n.deliver(target, text)

	// 3) Persist outcome.
	if deliverErr == nil {
		n.markSent(task.ID, kind)
		log.Printf("[notify] task=%d kind=%s delivered to channel_type=%d", task.ID, kind, target.ChannelType)
		return
	}
	n.markFailed(task.ID, kind, deliverErr)
	log.Printf("[notify] task=%d kind=%s delivery failed: %v", task.ID, kind, sanitize(deliverErr.Error()))
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
func (n *Notifier) claim(taskID int64, kind string) (bool, *model.SummaryNotification, error) {
	now := n.now()
	row := &model.SummaryNotification{
		TaskID:       taskID,
		NotifyKind:   kind,
		Status:       model.NotifyStatusPending,
		AttemptCount: 0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// Plain Create: the UNIQUE(task_id, notify_kind) constraint rejects a
	// duplicate with an error we treat as "someone else owns it".
	err := n.db.Create(row).Error
	if err == nil {
		return true, row, nil
	}
	if isDuplicateKey(err) {
		var existing model.SummaryNotification
		if e := n.db.Where("task_id = ? AND notify_kind = ?", taskID, kind).First(&existing).Error; e != nil {
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
func (n *Notifier) claimRetry(taskID int64, kind string) (bool, error) {
	now := n.now()
	res := n.db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ? AND status = ? AND attempt_count < ?",
			taskID, kind, model.NotifyStatusFailed, n.cfg.MaxAttempts).
		Updates(map[string]any{
			"status":     model.NotifyStatusPending,
			"updated_at": now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (n *Notifier) deliver(target deliveryTarget, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// DM 可达前置：先 ensureFriend（幂等，带 space_id 供 server 拼 s{spaceID}_{uid}
	// 白名单 channel），再 sendMessage.
	if target.TargetUID != "" {
		if err := n.deliverer.EnsureFriend(ctx, target.SpaceID, target.TargetUID); err != nil {
			return fmt.Errorf("ensureFriend: %w", err)
		}
	}
	msg := SendMessageRequest{
		ChannelID:   target.ChannelID,
		ChannelType: target.ChannelType,
		Payload:     map[string]any{"text": text},
	}
	if err := n.deliverer.SendMessage(ctx, msg); err != nil {
		return fmt.Errorf("sendMessage: %w", err)
	}
	return nil
}

func (n *Notifier) markSent(taskID int64, kind string) {
	now := n.now()
	if err := n.db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ?", taskID, kind).
		Updates(map[string]any{
			"status":        model.NotifyStatusSent,
			"sent_at":       now,
			"updated_at":    now,
			"attempt_count": gorm.Expr("attempt_count + 1"),
			"last_error":    nil,
		}).Error; err != nil {
		log.Printf("[notify] task=%d kind=%s: markSent failed: %v", taskID, kind, err)
	}
}

func (n *Notifier) markFailed(taskID int64, kind string, cause error) {
	now := n.now()
	le := sanitize(cause.Error())
	if len(le) > 480 {
		le = le[:480]
	}
	if err := n.db.Model(&model.SummaryNotification{}).
		Where("task_id = ? AND notify_kind = ?", taskID, kind).
		Updates(map[string]any{
			"status":        model.NotifyStatusFailed,
			"updated_at":    now,
			"attempt_count": gorm.Expr("attempt_count + 1"),
			"last_error":    le,
		}).Error; err != nil {
		log.Printf("[notify] task=%d kind=%s: markFailed failed: %v", taskID, kind, err)
	}
}

// buildText composes the user-facing notification body. Success carries the
// result link (when a base URL is configured); failure carries the sanitized
// error message.
func (n *Notifier) buildText(task model.SummaryTask, kind, errMsg string) string {
	title := strings.TrimSpace(task.Title)
	switch kind {
	case model.NotifyKindCompleted:
		var b strings.Builder
		if title != "" {
			fmt.Fprintf(&b, "你的总结「%s」已生成完成。", title)
		} else {
			b.WriteString("你的总结已生成完成。")
		}
		if link := n.resultLink(task); link != "" {
			fmt.Fprintf(&b, "\n查看结果：%s", link)
		}
		return b.String()
	case model.NotifyKindFailed:
		var b strings.Builder
		if title != "" {
			fmt.Fprintf(&b, "你的总结「%s」生成失败。", title)
		} else {
			b.WriteString("你的总结生成失败。")
		}
		if reason := strings.TrimSpace(errMsg); reason != "" {
			fmt.Fprintf(&b, "\n失败原因：%s", reason)
		}
		return b.String()
	default:
		return ""
	}
}

// resultLink builds the result URL from the configured base. Empty base → no link.
func (n *Notifier) resultLink(task model.SummaryTask) string {
	base := strings.TrimRight(strings.TrimSpace(n.cfg.WebBaseURL), "/")
	if base == "" || task.TaskNo == "" {
		return ""
	}
	return base + "/summary/" + task.TaskNo
}

// inQuietWindow reports whether t (Asia/Shanghai) falls inside the configured
// quiet window. Disabled (returns false) unless both bounds parse as "HH:MM".
// Supports wrap-around windows (e.g. 22:00-07:00).
func (n *Notifier) inQuietWindow(t time.Time) bool {
	start, ok1 := parseHHMM(n.cfg.QuietStart)
	end, ok2 := parseHHMM(n.cfg.QuietEnd)
	if !ok1 || !ok2 || start == end {
		return false
	}
	cur := t.Hour()*60 + t.Minute()
	if start < end {
		return cur >= start && cur < end
	}
	// Wrap-around (overnight) window.
	return cur >= start || cur < end
}

// parseHHMM parses "HH:MM" into minutes-of-day. Returns ok=false on any malformed input.
func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := atoiBounded(parts[0], 0, 23)
	m, err2 := atoiBounded(parts[1], 0, 59)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return h*60 + m, true
}

func atoiBounded(s string, lo, hi int) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("non-digit")
		}
		n = n*10 + int(c-'0')
	}
	if n < lo || n > hi {
		return 0, errors.New("out of range")
	}
	return n, nil
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
