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

// Channel type values on the IM-bot wire protocol (sendMessage.channel_type).
// These are the octo-server bot API values, NOT model.OriginChannel* — see
// resolveTarget for the mapping. 1=DM, 2=Group, 5=Thread.
const (
	WireChannelDM     = 1
	WireChannelGroup  = 2
	WireChannelThread = 5
)

// Deliverer is the bot-side transport: establish the bot<->user relationship and
// post a message. It is an interface so the trigger/state-machine logic can be
// unit-tested without a live octo-server (联调阶段对齐真实契约).
type Deliverer interface {
	// EnsureFriend establishes the bot<->user relationship so a DM is deliverable.
	// Idempotent; must be a no-op error when already friends. Called BEFORE send.
	//
	// spaceID (SummaryTask.SpaceID) is required so octo-server can build the
	// space-prefixed IM whitelist channel (s{spaceID}_{uid}); under multi-Space
	// deployments the whitelist is attached to the space-prefixed channel,
	// otherwise the DM is silently dropped. The space prefix is used ONLY on the
	// ensureFriend whitelist side — sendMessage.channel_id stays a bare uid.
	EnsureFriend(ctx context.Context, spaceID, targetUID string) error
	// SendMessage posts a bot message to the resolved channel.
	SendMessage(ctx context.Context, msg SendMessageRequest) error
}

// SendMessageRequest is the body of POST {OCTO_API_URL}/v1/bot/sendMessage.
// The payload carries ONLY text (+ result link / failure reason already folded
// into text by the caller). It MUST NOT carry any OBO reserved fields
// (__obo_*/obo_*/actual_sender_uid) — this is a first-class bot message.
type SendMessageRequest struct {
	ChannelID   string         `json:"channel_id"`
	ChannelType int            `json:"channel_type"`
	Payload     map[string]any `json:"payload"`
}

// oboReservedKeys are the OBO markers that must never appear in a bot payload.
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

// HTTPDeliverer is the production Deliverer talking to octo-server's bot API.
type HTTPDeliverer struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPDeliverer builds a Deliverer. token is the SUMMARY_BOT_TOKEN secret —
// it is stored only in memory and only ever placed in the Authorization header.
func NewHTTPDeliverer(baseURL, token string) *HTTPDeliverer {
	return &HTTPDeliverer{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *HTTPDeliverer) EnsureFriend(ctx context.Context, spaceID, targetUID string) error {
	body := map[string]string{
		"target_uid": targetUID,
		"space_id":   spaceID,
	}
	return d.post(ctx, "/v1/bot/ensureFriend", body)
}

func (d *HTTPDeliverer) SendMessage(ctx context.Context, msg SendMessageRequest) error {
	if payloadHasOBOReserved(msg.Payload) {
		// Defensive: never let an OBO reserved field leak into a bot message.
		return fmt.Errorf("sendMessage payload contains forbidden OBO reserved field")
	}
	return d.post(ctx, "/v1/bot/sendMessage", msg)
}

func (d *HTTPDeliverer) post(ctx context.Context, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.token)

	resp, err := d.client.Do(req)
	if err != nil {
		// Never include req body / token in the error.
		return fmt.Errorf("%s request failed: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Read a bounded slice of the response for diagnostics; the token lives only
	// in the request header and is never echoed back, so this snippet is safe.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(snippet)))
}
