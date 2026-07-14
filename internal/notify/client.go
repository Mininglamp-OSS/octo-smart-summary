package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Channel type values on the IM wire protocol (SendMessageRequest.ChannelType).
// 1=DM, 2=Group, 5=Thread. The state machine only ever builds DM targets, but
// the enum is kept for target resolution parity with the origin channel.
const (
	WireChannelDM     = 1
	WireChannelGroup  = 2
	WireChannelThread = 5
)

// Deliverer is the transport the state machine drives per recipient. It is an
// interface so the claim/deliver/mark logic is unit-testable without a live
// octo-server.
type Deliverer interface {
	// EnsureFriend is a delivery precondition hook. internal-notify needs none,
	// so its implementation is a no-op returning nil.
	EnsureFriend(ctx context.Context, spaceID, targetUID string) error
	// SendMessage posts one message for one recipient. The state machine calls
	// it once per recipient (targets is a single uid on the wire), so
	// per-recipient dedup/retry semantics are preserved. A delivery that did not
	// actually reach the user (network error, non-2xx, or 2xx with the recipient
	// filtered out server-side) MUST return an error so the state machine records
	// attempt+last_error and the sweep retries.
	SendMessage(ctx context.Context, spaceID string, msg SendMessageRequest) error
}

// SendMessageRequest is the state-machine-internal message shape. deliver()
// fills ChannelID with the recipient uid, ChannelType=WireChannelDM, and Card
// with the octo-server notification card fields.
type SendMessageRequest struct {
	ChannelID   string         `json:"channel_id"`
	ChannelType int            `json:"channel_type"`
	Payload     map[string]any `json:"payload,omitempty"`
	Card        *notifyCard    `json:"card,omitempty"`
}

type notifyCard struct {
	TaskNo      string `json:"task_no"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	TimeRange   string `json:"time_range,omitempty"`
	Members     int    `json:"members,omitempty"`
	MsgCount    int    `json:"msg_count,omitempty"`
	GeneratedAt string `json:"generated_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// oboReservedKeys are OBO markers that must never appear in a payload.
var oboReservedKeys = map[string]struct{}{
	"actual_sender_uid": {},
}

func payloadHasOBOReserved(payload map[string]any) bool {
	for k := range payload {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "__obo_") || strings.HasPrefix(lk, "obo_") {
			return true
		}
		if _, ok := oboReservedKeys[lk]; ok {
			return true
		}
	}
	return false
}

// notifyReq mirrors octo-server modules/notify.NotifyReq (POST /v1/internal/notify).
// One request carries exactly one recipient in Targets so the state machine's
// per-recipient dedup/retry granularity is preserved.
type notifyReq struct {
	SpaceID  string         `json:"space_id"`
	Service  string         `json:"service"`
	Event    string         `json:"event"`
	Targets  []string       `json:"targets"`
	ActorUID string         `json:"actor_uid"`
	Payload  map[string]any `json:"payload,omitempty"`
	Card     *notifyCard    `json:"card,omitempty"`
}

const (
	// notifyService is the service label carried in every NotifyReq.
	notifyService = "summary-service"
	// notifyEndpoint is the octo-server internal notify path.
	notifyEndpoint = "/v1/internal/notify"
	// internalTokenHeader authenticates the request (constant-time compared server-side).
	internalTokenHeader = "X-Internal-Token"
)

// InternalNotifyDeliverer posts to octo-server's internal-notify API. Auth is the
// constant X-Internal-Token; no per-user relationship is needed, so EnsureFriend
// is a no-op.
type InternalNotifyDeliverer struct {
	baseURL string
	token   string
	// Kept for constructor compatibility; card deep links are built server-side.
	webBaseURL string
	client     *http.Client
}

// NewInternalNotifyDeliverer builds the internal-notify transport. token is the
// SUMMARY_NOTIFY_TOKEN secret (memory-only, only ever set on the request header).
func NewInternalNotifyDeliverer(baseURL, token, webBaseURL string) *InternalNotifyDeliverer {
	return &InternalNotifyDeliverer{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		webBaseURL: strings.TrimRight(webBaseURL, "/"),
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// EnsureFriend is a no-op: internal-notify needs no per-user relationship.
func (d *InternalNotifyDeliverer) EnsureFriend(ctx context.Context, spaceID, targetUID string) error {
	return nil
}

// notifyResp mirrors octo-server modules/notify.NotifyResp. The server returns
// 200 even when every target is filtered out (non-member), so Delivered is the
// only trustworthy signal that the message actually reached someone.
type notifyResp struct {
	Delivered []string          `json:"delivered"`
	Filtered  map[string]string `json:"filtered"`
}

// SendMessage posts one recipient's message to /v1/internal/notify.
func (d *InternalNotifyDeliverer) SendMessage(ctx context.Context, spaceID string, msg SendMessageRequest) error {
	if payloadHasOBOReserved(msg.Payload) {
		return fmt.Errorf("notify payload contains forbidden OBO reserved field")
	}
	var payload map[string]any
	if msg.Card == nil && len(msg.Payload) > 0 {
		payload = make(map[string]any, len(msg.Payload))
		for k, v := range msg.Payload {
			payload[k] = v
		}
	}

	// NotifyReq.space_id is required (binding); fall back to payload.space_id.
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		if ps, _ := payload["space_id"].(string); ps != "" {
			spaceID = strings.TrimSpace(ps)
		}
	}

	req := notifyReq{
		SpaceID:  spaceID,
		Service:  notifyService,
		Event:    "",
		Targets:  []string{msg.ChannelID}, // single recipient — keep per-uid granularity
		ActorUID: "",
		Payload:  payload,
		Card:     msg.Card,
	}
	return d.post(ctx, notifyEndpoint, req, msg.ChannelID)
}

// post sends the request and treats "not actually delivered" as an error.
// recipientUID is the single target; a 2xx whose Delivered list omits it means
// the server filtered the recipient (e.g. non-member) — surface that as an
// explicit failure so the state machine retries instead of marking sent.
func (d *InternalNotifyDeliverer) post(ctx context.Context, path string, body any, recipientUID string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", path, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(internalTokenHeader, d.token)

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s request failed: %w", path, err)
	}
	defer resp.Body.Close()

	// Bounded read: the token lives only in the request header and is never
	// echoed back, so the body is safe to inspect/surface.
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(rawResp)))
	}

	// 2xx does not guarantee delivery: the server filters non-member targets and
	// still returns 200 {delivered:[], filtered:{...}}. Require our recipient in
	// Delivered, else fail so this is retried and left as last_error.
	var body200 notifyResp
	if err := json.Unmarshal(rawResp, &body200); err != nil {
		return fmt.Errorf("%s: parse response: %w", path, err)
	}
	for _, uid := range body200.Delivered {
		if uid == recipientUID {
			return nil
		}
	}
	if reason := body200.Filtered[recipientUID]; reason != "" {
		return fmt.Errorf("%s: recipient filtered by server: %s", path, reason)
	}
	return fmt.Errorf("%s: recipient not in delivered set (filtered/dropped server-side)", path)
}
