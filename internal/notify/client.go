package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrTokenNotYetProvisioned marks the narrow startup-race case where the bot
// token row is not yet written by octo-server. Callers use errors.Is on the
// wrapped chain to divert delivery into a no-budget-consumed retry path
// (see Notifier.markDeferred) instead of counting it as a normal HTTP failure.
// Any other tokenFn error (real DB failure, etc.) is deliberately NOT this
// sentinel and follows the standard failure/attempt_count accounting.
var ErrTokenNotYetProvisioned = errors.New("bot token not yet provisioned")

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
	//
	// spaceID (SummaryTask.SpaceID) is required so octo-server can inject the
	// authoritative payload.space_id for a system-bot DM. octo-server
	// fail-closes on system-bot DMs: it STRIPS any client-supplied
	// payload.space_id and instead trusts only the value it resolves itself.
	// The single worker-controllable input on that resolution path is the
	// HTTP request header `X-Space-ID`: for a system-bot DM, when
	// CheckMembership(spaceID, recipientUID)=true (the recipient is an active
	// member of the space — the case here), octo-server adopts the header and
	// uses it to inject the authoritative payload.space_id. Without the header
	// the message is filtered out / unopenable. The header is only attached
	// when spaceID is non-empty (empty → no header, preserving compatibility).
	SendMessage(ctx context.Context, spaceID string, msg SendMessageRequest) error
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
//
// 启动顺序竞态修复（OCT-5 / 方案 D）：token 不再在构造时固化。容器编排不保证
// summary-worker 与 octo-server 的启动先后；若 worker 先起、server 尚未
// ensureSummaryBotToken 写库，启动时查库会拿到空 token。旧实现会就此把 notifier
// 永久 disable，即便 server 之后写了 token 也不恢复，必须人工重启 worker。
//
// 现在 HTTPDeliverer 持有一个 tokenFn（token provider），在每次投递前 lazy 解析
// token，并把「首次拿到的非空 token」缓存住（见 cachedToken）。空值不缓存，下次
// 再查，因此 server 晚起后写了 token，worker 能在后续投递中自动恢复，无需重启。
type HTTPDeliverer struct {
	baseURL string
	tokenFn func() (string, error)
	client  *http.Client

	// token 缓存：只缓存「非空成功值」。一旦缓存命中即不再调用 tokenFn。
	cacheMu     sync.RWMutex
	cachedToken string
}

// NewHTTPDeliverer builds a Deliverer from a fixed token. token is the
// SUMMARY_BOT_TOKEN secret — it is stored only in memory and only ever placed in
// the Authorization header.
//
// 向后兼容：保留旧签名。内部包成一个返回固定 token 的 tokenFn。固定 token 为空时
// 仍会在投递时报错（best-effort 失败处理吞掉），不再 panic、不会永久禁用。
func NewHTTPDeliverer(baseURL, token string) *HTTPDeliverer {
	return NewHTTPDelivererWithTokenFunc(baseURL, func() (string, error) {
		return token, nil
	})
}

// NewHTTPDelivererWithTokenFunc builds a Deliverer that lazily resolves the bot
// token via tokenFn on each post (with non-empty-value caching). This is the
// path that survives the startup-order race: assemble the notifier even when the
// token is not yet available, and let it resolve once octo-server has written it
// into the shared IM DB.
func NewHTTPDelivererWithTokenFunc(baseURL string, tokenFn func() (string, error)) *HTTPDeliverer {
	return &HTTPDeliverer{
		baseURL: strings.TrimRight(baseURL, "/"),
		tokenFn: tokenFn,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// resolveToken returns the bot token, preferring the cached non-empty value and
// otherwise invoking tokenFn. A non-empty success is cached; empty values and
// errors are NOT cached so a late-starting server can still be picked up later.
func (d *HTTPDeliverer) resolveToken() (string, error) {
	d.cacheMu.RLock()
	cached := d.cachedToken
	d.cacheMu.RUnlock()
	if cached != "" {
		return cached, nil
	}

	if d.tokenFn == nil {
		return "", fmt.Errorf("no bot token provider configured")
	}
	token, err := d.tokenFn()
	if err != nil {
		return "", err
	}
	if token == "" {
		// Sentinel-wrapped so upstream can errors.Is this specific startup-race
		// case and retry without consuming attempt budget (markDeferred).
		return "", fmt.Errorf("bot token unavailable (empty); not yet provisioned by octo-server: %w", ErrTokenNotYetProvisioned)
	}

	d.cacheMu.Lock()
	d.cachedToken = token
	d.cacheMu.Unlock()
	return token, nil
}

func (d *HTTPDeliverer) EnsureFriend(ctx context.Context, spaceID, targetUID string) error {
	body := map[string]string{
		"target_uid": targetUID,
		"space_id":   spaceID,
	}
	// octo-server resolves the authoritative space from the X-Space-ID header
	// for a system bot; attach it (only when known) so ensureFriend builds the
	// space-scoped whitelist against the same authoritative space as the send.
	return d.post(ctx, "/v1/bot/ensureFriend", body, spaceHeader(spaceID))
}

func (d *HTTPDeliverer) SendMessage(ctx context.Context, spaceID string, msg SendMessageRequest) error {
	if payloadHasOBOReserved(msg.Payload) {
		// Defensive: never let an OBO reserved field leak into a bot message.
		return fmt.Errorf("sendMessage payload contains forbidden OBO reserved field")
	}
	// octo-server STRIPS client payload.space_id for a system-bot DM and trusts
	// only the value it resolves from the X-Space-ID header (adopted when the
	// recipient is an active member of that space). Attach the header — without
	// it the message is filtered out. Only when spaceID is non-empty.
	return d.post(ctx, "/v1/bot/sendMessage", msg, spaceHeader(spaceID))
}

// spaceHeader returns the X-Space-ID header map for a non-empty (trimmed)
// spaceID, or nil when empty so post attaches no header (compat with the
// empty-SpaceID path). octo-server TrimSpace's the value, but we also avoid
// introducing any surrounding whitespace ourselves.
func spaceHeader(spaceID string) map[string]string {
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return nil
	}
	return map[string]string{"X-Space-ID": spaceID}
}

func (d *HTTPDeliverer) post(ctx context.Context, path string, body any, headers map[string]string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	token, err := d.resolveToken()
	if err != nil {
		// Lazy resolve failed (token not yet provisioned by octo-server). Fail
		// this delivery so the existing best-effort failure handling kicks in;
		// do NOT panic and do NOT permanently disable the notifier — a later
		// post will retry resolution and recover once the server writes it.
		return fmt.Errorf("%s: resolve bot token: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

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
