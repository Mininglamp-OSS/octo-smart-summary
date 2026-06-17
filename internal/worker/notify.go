package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// contentTypeSummaryNotify is the IM message content type the octo-web client
// (#291) registers to render the summary-notify tip. OCT-43 decision #2
// (Frontend) owns the authoritative payload key contract for this type.
const contentTypeSummaryNotify = 21

// channelTypePerson is octo-server's DM (person) channel type
// (octo-lib common.ChannelTypePerson). The tip is delivered as a directed DM
// to each authorized user, never broadcast to the origin channel.
const channelTypePerson = 1

// Notifier emits the summary-completed tip for a finished task. Implementations
// must be idempotent: NotifyCompleted may be invoked more than once for the
// same task (concurrent CAS winners, restarts) and must emit at most once.
type Notifier interface {
	NotifyCompleted(taskID int64)
}

// noopNotifier is installed when summary-notify is unconfigured. It does
// nothing — crucially it does NOT touch notified_at, so enabling the feature
// later can still notify previously-completed tasks.
type noopNotifier struct{}

func (noopNotifier) NotifyCompleted(int64) {}

// emitSummaryNotify fires the summary-completed tip for a task that this
// goroutine just transitioned to Completed. Nil-safe so Processors built via
// struct literal (tests) don't panic.
func (p *Processor) emitSummaryNotify(taskID int64) {
	if p.notifier == nil {
		return
	}
	p.notifier.NotifyCompleted(taskID)
}

// messageSender is the outbound transport to octo-server's bot API. Extracted
// so tests can assert target set + payload without a live HTTP server.
type messageSender interface {
	Send(channelID string, channelType int, payload map[string]interface{}) error
}

// botNotifier emits a directed contentType=21 tip to the access-gated target
// set (creator + explicit participants) of a completed task.
type botNotifier struct {
	db       *gorm.DB
	sender   messageSender
	fromUID  string
	fromName string
}

// NewNotifier builds the summary-notify emitter. Returns a no-op when the
// feature is not fully configured (see config.NotifyConfigured).
func NewNotifier(db *gorm.DB, cfg *config.Config) Notifier {
	if !cfg.NotifyConfigured() {
		log.Printf("[notify] summary-notify disabled (not configured); emit is a no-op")
		return noopNotifier{}
	}
	return &botNotifier{
		db: db,
		sender: &botAPISender{
			url:    cfg.NotifyBotAPIURL,
			token:  cfg.NotifyBotToken,
			client: &http.Client{Timeout: 5 * time.Second},
		},
		fromUID:  cfg.NotifyBotUID,
		fromName: cfg.NotifyBotName,
	}
}

// NotifyCompleted emits the tip exactly once per task. The notified_at CAS
// (notified_at IS NULL → now) is the idempotency anchor: only the single
// goroutine whose CAS affects one row proceeds to send, so concurrent callers
// and post-restart retries are deduplicated even across processes.
func (n *botNotifier) NotifyCompleted(taskID int64) {
	now := time.Now().UTC()
	// deleted_at IS NULL mirrors authorizeTaskAccess: SummaryTask.DeletedAt is a
	// plain *time.Time (not gorm.DeletedAt), so GORM applies no automatic
	// soft-delete scoping — the predicate must be explicit. A soft-deleted task
	// must neither win the CAS nor burn the notified_at anchor, so an undelete
	// can still notify later.
	cas := n.db.Model(&model.SummaryTask{}).
		Where("id = ? AND notified_at IS NULL AND deleted_at IS NULL", taskID).
		Update("notified_at", now)
	if cas.Error != nil {
		log.Printf("[notify] task %d notified_at CAS error: %v", taskID, cas.Error)
		return
	}
	if cas.RowsAffected != 1 {
		// Already notified (lost the CAS race or replayed after restart).
		return
	}

	var task model.SummaryTask
	if err := n.db.Where("id = ?", taskID).First(&task).Error; err != nil {
		log.Printf("[notify] task %d load failed after CAS: %v", taskID, err)
		return
	}
	// Defense in depth: the CAS already excludes soft-deleted rows, but bail if
	// the row was deleted between CAS and load so a deleted task never fans out.
	if task.DeletedAt != nil {
		return
	}

	targets := notifyTargetUserIDs(n.db, &task)
	payload := n.buildPayload(&task)
	sent := 0
	for _, uid := range targets {
		if err := n.sender.Send(uid, channelTypePerson, payload); err != nil {
			// Best-effort per recipient: one failed DM must not block the rest.
			log.Printf("[notify] task %d send to %s failed: %v", taskID, uid, err)
			continue
		}
		sent++
	}
	log.Printf("[notify] task %d emitted summary-notify: %d/%d targets", taskID, sent, len(targets))
}

// notifyTargetUserIDs returns the access-gated notify target set for a task:
// the creator plus every explicit participant, deduplicated and creator-first.
// This mirrors handler.canAccessTask — origin-channel membership alone does NOT
// receive a tip, which is what prevents the channel-wide leak (octo-web #291).
func notifyTargetUserIDs(db *gorm.DB, task *model.SummaryTask) []string {
	seen := make(map[string]struct{})
	var targets []string
	add := func(uid string) {
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		targets = append(targets, uid)
	}

	add(task.CreatorID)

	var participants []model.SummaryParticipant
	db.Where("task_id = ?", task.ID).Find(&participants)
	for _, p := range participants {
		add(p.UserID)
	}
	return targets
}

// buildPayload constructs the contentType=21 tip payload.
//
// OCT-43 decision #2 (Frontend): these keys are the proposed contract; the
// authoritative shape is whatever octo-web #291 registered client-side. Confirm
// the exact required keys (link / task ref form) before merge. from_uid /
// from_name are surfaced here for the renderer even though octo-server stamps
// the authoritative from_uid from the bot token.
func (n *botNotifier) buildPayload(task *model.SummaryTask) map[string]interface{} {
	return map[string]interface{}{
		"type":      contentTypeSummaryNotify,
		"from_uid":  n.fromUID,
		"from_name": n.fromName,
		"task_id":   task.ID,
		"task_no":   task.TaskNo,
		"title":     task.Title,
	}
}

// botAPISender posts to octo-server's POST /v1/bot/sendMessage as a system
// App-Bot. octo-server stamps from_uid = the authenticated bot.
type botAPISender struct {
	url    string
	token  string
	client *http.Client
}

func (s *botAPISender) Send(channelID string, channelType int, payload map[string]interface{}) error {
	body, err := json.Marshal(map[string]interface{}{
		"channel_id":   channelID,
		"channel_type": channelType,
		"payload":      payload,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("bot api status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
