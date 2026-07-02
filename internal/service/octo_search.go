package service

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// OctoSearchBatchClient is a stateless HTTP client for the octo-search Batch
// Messages API (api-spec v0.1). It exposes single atomic operations
// (Submit / Poll / Delete / DownloadAndParse); the polling loop, 413 split
// retry and ndjson row -> Message mapping are orchestrated by the caller
// (pipeline.fetchViaBatch). The client never imports the pipeline package.
type OctoSearchBatchClient struct {
	baseURL string // root address WITHOUT /v1 suffix (see plan §5.4)
	token   string // s2s bearer token, never logged
	http    *http.Client
}

// NewOctoSearchBatchClient creates a client.
//
// baseURL should be the root address without the /v1 suffix
// (e.g. "http://octo-search-batch:8080"); the client appends
// "/v1/messages/batch" itself. A trailing slash is trimmed defensively.
func NewOctoSearchBatchClient(baseURL, token string) *OctoSearchBatchClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &OctoSearchBatchClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// No client-wide Timeout: per-request deadlines come from ctx, while the
		// transport timeout prevents a peer from accepting a connection and never
		// sending response headers.
		http: &http.Client{Transport: transport},
	}
}

// ── Typed errors (contract point 1) ────────────────────────────────────────
// 4xx are surfaced as identifiable sentinels so fetchViaBatch can branch:
//   - ErrSingleTaskTooLarge (413) -> caller bisects the batch / shrinks window
//   - ErrUnauthorized (401) / ErrBadRequest (400) -> caller fails fast
//     & alert (config/contract problem, NOT isolated as a per-batch skip)
//   - ErrTaskNotFound (404) / ErrTimeRangeTooLong (422) -> caller fail-fast
//
// 5xx (500/503) are NOT given sentinels: the client retries them internally
// with backoff (<=3 times, 503 spaced >=10s). Only after retries are
// exhausted does it return a plain (wrapped) error, which the caller treats
// as an isolatable per-batch failure (continue).
var (
	ErrSingleTaskTooLarge = errors.New("octo-search: single task too large (413)")
	ErrBadRequest         = errors.New("octo-search: invalid request (400)")
	ErrUnauthorized       = errors.New("octo-search: unauthorized (401)")
	ErrTaskNotFound       = errors.New("octo-search: task not found (404)")
	ErrTimeRangeTooLong   = errors.New("octo-search: time range too long (422)")
	// ErrFatalClient wraps any *other* 4xx (403/409/429/...) that has no
	// dedicated sentinel. The caller fails fast rather than
	// isolating the batch, because a misconfig/permission/rate-limit
	// problem causes silent partial data). See classifyHTTPError default case.
	ErrFatalClient = errors.New("octo-search: fatal client error (4xx)")
)

// ── DTOs ───────────────────────────────────────────────────────────────────

// BatchMessageRow is one raw ndjson row (api-spec §5.1). Field names mirror the
// message export schema. Payload is already parsed plain text produced by
// octo-search-batch, not the original IM JSON payload. The client only produces
// raw rows; from_uid->SenderUID, SourceName/ChannelType backfill and SendTime
// formatting are the caller's job (it holds the reverse-lookup map).
type BatchMessageRow struct {
	MessageSeq int64  `json:"message_seq"`
	FromUID    string `json:"from_uid"`
	ChannelID  string `json:"channel_id"`
	Timestamp  int64  `json:"timestamp"`
	Payload    string `json:"payload"`
}

// BatchStatus is the GET /v1/messages/batch/{task_id} response (api-spec §3.2).
type BatchStatus struct {
	Status      string    `json:"status"` // queued/running/completed/partial/failed/cancelled
	ActualCount int64     `json:"actual_count"`
	Parts       []Part    `json:"parts"`
	Warnings    []Warning `json:"warnings"`
	ErrorCode   string    `json:"error_code"`    // failed only
	ErrorMsg    string    `json:"error_message"` // failed only
}

// Part is one NDJSON shard descriptor (api-spec §3.2 / §5.2).
type Part struct {
	URL          string   `json:"url"`
	SizeBytes    int64    `json:"size_bytes"`
	MessageCount int      `json:"message_count"`
	SHA256       string   `json:"sha256"`
	ExpiresAt    int64    `json:"expires_at"`
	ChannelIDs   []string `json:"channel_ids"`
}

// Warning is a truncation / partial warning (api-spec §3.2).
type Warning struct {
	ChannelID  string `json:"channel_id"`
	Code       string `json:"code"` // per_channel_truncated / total_truncated
	Limit      int    `json:"limit"`
	ActualSeen int    `json:"actual_seen"`
}

// IsTerminal reports whether the status is a terminal state (state machine
// §3.3). The caller's polling loop stops on terminal states.
func (s *BatchStatus) IsTerminal() bool {
	switch s.Status {
	case "completed", "partial", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// ── Request bodies ───────────────────────────────────────────────────────────

type batchSubmitChannel struct {
	ChannelID string `json:"channel_id"`
}

type batchSubmitScope struct {
	Channels []batchSubmitChannel `json:"channels"`
}

type batchSubmitTimeRange struct {
	StartTS int64 `json:"start_ts"`
	EndTS   int64 `json:"end_ts"`
}

// IsDeleted is sent explicitly to match the legacy MySQL content query's
// is_deleted = 0 predicate.
type batchSubmitRequest struct {
	Scope     batchSubmitScope     `json:"scope"`
	TimeRange batchSubmitTimeRange `json:"time_range"`
	IsDeleted int                  `json:"is_deleted"`
}

type batchSubmitResponse struct {
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
}

// ── Retry / backoff config ───────────────────────────────────────────────────

const (
	maxServerRetries      = 3                // 5xx retries (api-spec §7)
	base5xxBackoff        = 1 * time.Second  // 500 backoff base (exp + jitter)
	min503Backoff         = 10 * time.Second // 503 must space >=10s (api-spec §7)
	responseHeaderTimeout = 60 * time.Second
	partDownloadTries     = 2 // per-part integrity retries (§5.3)
	maxPartBytes          = int64(256 * 1024 * 1024)

	pathBatch = "/v1/messages/batch"
)

// ── Submit ───────────────────────────────────────────────────────────────────

// Submit posts a batch task. Expects HTTP 202. 4xx are mapped to the typed
// sentinels above; 5xx are retried internally then returned as a plain error.
func (c *OctoSearchBatchClient) Submit(ctx context.Context, channelIDs []string, startTS, endTS int64) (string, error) {
	chs := make([]batchSubmitChannel, 0, len(channelIDs))
	for _, id := range channelIDs {
		chs = append(chs, batchSubmitChannel{ChannelID: id})
	}
	reqBody := batchSubmitRequest{
		Scope:     batchSubmitScope{Channels: chs},
		TimeRange: batchSubmitTimeRange{StartTS: startTS, EndTS: endTS},
		IsDeleted: 0,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("octo-search submit: marshal request: %w", err)
	}

	var taskID string
	doErr := c.doWithRetry(ctx, func() (retry bool, status int, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+pathBatch, bytes.NewReader(raw))
		if err != nil {
			return false, 0, fmt.Errorf("octo-search submit: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		c.setAuth(req)

		resp, err := c.http.Do(req)
		if err != nil {
			// transport error: retryable (acts like a 5xx)
			return true, 0, fmt.Errorf("octo-search submit: do request: %w", err)
		}
		defer resp.Body.Close()
		c.logRequestID("submit", resp)

		if resp.StatusCode == http.StatusAccepted { // 202
			var sr batchSubmitResponse
			if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
				return false, resp.StatusCode, fmt.Errorf("octo-search submit: decode 202 body: %w", err)
			}
			if sr.TaskID == "" {
				return false, resp.StatusCode, errors.New("octo-search submit: 202 with empty task_id")
			}
			taskID = sr.TaskID
			return false, resp.StatusCode, nil
		}
		return c.classifyHTTPError("submit", resp)
	})
	if doErr != nil {
		return "", doErr
	}
	return taskID, nil
}

// ── Poll ─────────────────────────────────────────────────────────────────────

// Poll performs a SINGLE status query (api-spec §3). It only retries 5xx
// internally; it does NOT loop waiting for a terminal state — that loop
// (5s backoff / 20min total / ctx-cancel -> Delete) is the caller's job.
func (c *OctoSearchBatchClient) Poll(ctx context.Context, taskID string) (*BatchStatus, error) {
	url := c.baseURL + pathBatch + "/" + taskID

	var status *BatchStatus
	doErr := c.doWithRetry(ctx, func() (retry bool, statusCode int, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, 0, fmt.Errorf("octo-search poll: build request: %w", err)
		}
		c.setAuth(req)

		resp, err := c.http.Do(req)
		if err != nil {
			return true, 0, fmt.Errorf("octo-search poll: do request: %w", err)
		}
		defer resp.Body.Close()
		c.logRequestID("poll", resp)

		if resp.StatusCode == http.StatusOK { // 200
			var bs BatchStatus
			if err := json.NewDecoder(resp.Body).Decode(&bs); err != nil {
				return false, resp.StatusCode, fmt.Errorf("octo-search poll: decode body: %w", err)
			}
			status = &bs
			return false, resp.StatusCode, nil
		}
		return c.classifyHTTPError("poll", resp)
	})
	if doErr != nil {
		return nil, doErr
	}
	return status, nil
}

// ── Delete ───────────────────────────────────────────────────────────────────

// Delete cancels a task (api-spec §4). Idempotent: terminal tasks also return
// 200. This is a best-effort cleanup — callers invoke it on ctx-cancel /
// poll-timeout with an independent short timeout and treat failure as a
// warning, not a hard error.
func (c *OctoSearchBatchClient) Delete(ctx context.Context, taskID string) error {
	url := c.baseURL + pathBatch + "/" + taskID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("octo-search delete: build request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("octo-search delete: do request: %w", err)
	}
	defer resp.Body.Close()
	c.logRequestID("delete", resp)

	// 200 = accepted (incl. already-terminal). 404 = unknown/not-ours; for a
	// best-effort cleanup that is effectively "nothing to cancel".
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		return nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("octo-search delete: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// ── DownloadAndParse ─────────────────────────────────────────────────────────

// DownloadAndParse downloads every part, verifies gzip sha256, and parses the
// decompressed ndjson into raw BatchMessageRow values.
//
// Per-part failure isolation (api-spec §5.3): a part whose sha256 never
// matches after partDownloadTries retries is SKIPPED with a warning log; it
// does not fail the whole call. Returns (good rows, nil) when some/all parts
// succeed. Only when EVERY part fails (zero rows parsed AND at least one part
// was bad) does it return an error, so the caller does not mistake a
// total download failure for an empty result.
func (c *OctoSearchBatchClient) DownloadAndParse(ctx context.Context, parts []Part) ([]BatchMessageRow, error) {
	if len(parts) == 0 {
		return nil, nil
	}

	rows := make([]BatchMessageRow, 0, 1024)
	var badParts int
	for i := range parts {
		partRows, err := c.downloadPart(ctx, parts[i])
		if err != nil {
			badParts++
			log.Printf("octo-search download: part %d/%d skipped after retries: %v", i+1, len(parts), err)
			continue
		}
		rows = append(rows, partRows...)
	}

	// All parts bad and nothing recovered -> surface as error so the caller's
	// per-batch isolation kicks in (don't masquerade as legit empty result).
	if badParts == len(parts) && len(rows) == 0 {
		return nil, fmt.Errorf("octo-search download: all %d part(s) failed", len(parts))
	}
	return rows, nil
}

// downloadPart downloads one part URL, retrying on transport error or sha256
// mismatch up to partDownloadTries times, then parses gzip ndjson.
func (c *OctoSearchBatchClient) downloadPart(ctx context.Context, part Part) ([]BatchMessageRow, error) {
	var lastErr error
	for attempt := 1; attempt <= partDownloadTries; attempt++ {
		raw, err := c.fetchPartBytes(ctx, part.URL)
		if err != nil {
			lastErr = err
			continue
		}
		if part.SHA256 == "" {
			lastErr = errors.New("missing sha256")
			continue
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, part.SHA256) {
			lastErr = fmt.Errorf("sha256 mismatch: want %s got %s", part.SHA256, got)
			continue
		}
		rows, err := parseGzipNDJSON(raw)
		if err != nil {
			lastErr = err
			continue
		}
		// Treat a manifest/content row-count disagreement as part corruption
		// (same tier as sha256): the part file does not match what the manifest
		// declares, so retry; if it still mismatches it is reported as a bad part.
		// This is part-level integrity, not server-side completeness — we do not
		// validate aggregate ActualCount here.
		if part.MessageCount > 0 && len(rows) != part.MessageCount {
			lastErr = fmt.Errorf("message_count mismatch: manifest %d got %d", part.MessageCount, len(rows))
			continue
		}
		return rows, nil
	}
	return nil, lastErr
}

// fetchPartBytes GETs the presigned URL and returns the raw (still gzipped)
// body bytes for sha256 verification.
func (c *OctoSearchBatchClient) fetchPartBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build part request: %w", err)
	}
	// Disable Go's transparent gzip handling. With no explicit Accept-Encoding,
	// the default Transport adds "Accept-Encoding: gzip" and auto-decompresses
	// responses carrying "Content-Encoding: gzip" (stripping the header too).
	// Our part objects are gzip *payloads* and sha256 is computed over the raw
	// gzip bytes; if the object store also returns a Content-Encoding: gzip
	// header, ReadAll would yield decompressed ndjson -> sha256 would never
	// match and parseGzipNDJSON would fail. "identity" forces the raw bytes.
	req.Header.Set("Accept-Encoding", "identity")
	// Presigned S3 URL: no Authorization header.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get part: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("get part: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPartBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxPartBytes {
		return nil, fmt.Errorf("get part: body exceeds %d bytes", maxPartBytes)
	}
	return raw, nil
}

// parseGzipNDJSON decompresses gzip bytes and parses one JSON row per line.
func parseGzipNDJSON(gz []byte) ([]BatchMessageRow, error) {
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()

	rows := make([]BatchMessageRow, 0, 256)
	sc := bufio.NewScanner(gr)
	// Single ndjson line may exceed the default 64KB; enlarge the buffer.
	const maxLine = 8 * 1024 * 1024
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var row BatchMessageRow
		if err := json.Unmarshal(b, &row); err != nil {
			return nil, fmt.Errorf("ndjson line %d: %w", line, err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ndjson scan: %w", err)
	}
	return rows, nil
}

// ── shared helpers ───────────────────────────────────────────────────────────

func (c *OctoSearchBatchClient) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}

// logRequestID logs the server-echoed X-Request-Id for tracing. The token is
// never logged.
func (c *OctoSearchBatchClient) logRequestID(op string, resp *http.Response) {
	if rid := resp.Header.Get("X-Request-Id"); rid != "" {
		log.Printf("octo-search %s: status=%d x-request-id=%s", op, resp.StatusCode, rid)
	}
}

// classifyHTTPError maps a non-success status to a typed sentinel (4xx) or a
// retry signal (5xx). It returns (retry, status, err): retry=true asks
// doWithRetry to back off and try again; retry=false is terminal. status is the
// real HTTP status code so backoff can branch reliably (e.g. 503 spacing)
// without fragile string matching on the error text.
func (c *OctoSearchBatchClient) classifyHTTPError(op string, resp *http.Response) (bool, int, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	snippet := strings.TrimSpace(string(body))
	st := resp.StatusCode
	switch st {
	case http.StatusBadRequest: // 400
		return false, st, fmt.Errorf("%w (%s: %s)", ErrBadRequest, op, snippet)
	case http.StatusUnauthorized: // 401
		return false, st, fmt.Errorf("%w (%s)", ErrUnauthorized, op)
	case http.StatusNotFound: // 404
		return false, st, fmt.Errorf("%w (%s: %s)", ErrTaskNotFound, op, snippet)
	case http.StatusRequestEntityTooLarge: // 413
		return false, st, fmt.Errorf("%w (%s: %s)", ErrSingleTaskTooLarge, op, snippet)
	case 422: // Unprocessable Entity — time_range_too_long
		return false, st, fmt.Errorf("%w (%s: %s)", ErrTimeRangeTooLong, op, snippet)
	case http.StatusInternalServerError, http.StatusServiceUnavailable: // 500/503
		return true, st, fmt.Errorf("octo-search %s: server error %d: %s", op, st, snippet)
	default:
		// Unknown 4xx -> fail-fast (wrap ErrFatalClient so the caller's
		// isFatalBatchErr recognises it); unknown 5xx -> retry.
		if st >= 500 {
			return true, st, fmt.Errorf("octo-search %s: server error %d: %s", op, st, snippet)
		}
		return false, st, fmt.Errorf("%w (%s: unexpected status %d: %s)", ErrFatalClient, op, st, snippet)
	}
}

// doWithRetry runs fn, retrying when fn returns retry=true, up to
// maxServerRetries, with exponential backoff + jitter (503 spaced >=10s).
// fn returns (retry, status, err): status carries the HTTP status code (0 for
// transport errors) so backoff can branch on 503 reliably.
func (c *OctoSearchBatchClient) doWithRetry(ctx context.Context, fn func() (retry bool, status int, err error)) error {
	var lastErr error
	for attempt := 0; attempt <= maxServerRetries; attempt++ {
		retry, status, err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || attempt == maxServerRetries {
			return lastErr
		}
		// backoff before next attempt
		delay := backoffDelay(attempt, status)
		select {
		case <-ctx.Done():
			return fmt.Errorf("octo-search: context cancelled during retry: %w (last: %v)", ctx.Err(), lastErr)
		case <-time.After(delay):
		}
	}
	return lastErr
}

// backoffDelay computes exponential backoff with jitter. 503 (service_degraded)
// is spaced at least min503Backoff per api-spec §7. status is the real HTTP
// status code (0 for transport errors) — branching on it is reliable, unlike
// matching "503" in the error string.
func backoffDelay(attempt, status int) time.Duration {
	d := base5xxBackoff << attempt // 1s, 2s, 4s...
	// 503 must be spaced >=10s.
	if status == http.StatusServiceUnavailable && d < min503Backoff {
		d = min503Backoff
	}
	// +/-20% jitter
	jitter := time.Duration(rand.Int63n(int64(d) / 5))
	return d + jitter
}
