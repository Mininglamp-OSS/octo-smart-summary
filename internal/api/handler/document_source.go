package handler

// Shared document-source infrastructure.
//
// The client that fetches a document's summarize-ready content from the document
// service, plus the request/response shapes, fence sanitization, and chunk
// normalization that any document-summarizing handler builds on. Today the sole
// consumer is the ephemeral AI 速览 preview (document_preview.go); it lives in its
// own file so a future persisted document-summary path can reuse it rather than
// redefine these symbols.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxDocumentSourceResponseBytes = 4 << 20
	maxDocumentIDLen               = 64
	maxDocumentVersionLen          = 128
	maxDocumentTitleRunes          = 200
	maxDocumentChunkRunes          = 12000
	maxDocumentChunks              = 200
	maxDocumentPromptRunes         = 80000
	// maxRetryAfterSeconds caps an echoed upstream Retry-After at 24h. A larger value
	// is either a bug upstream or an attempt to park a client indefinitely; dropping
	// the header is better than forwarding either.
	maxRetryAfterSeconds = 86400
)

var errDocumentSourceNotConfigured = errors.New("document summary source API is not configured")

type documentSourceError struct {
	status  int
	message string
	// retryAfter carries the upstream Retry-After header verbatim on the 429 path.
	// Empty on every other path. Telling a client to back off without telling it for
	// how long makes the status advisory rather than actionable.
	retryAfter string
}

func (e *documentSourceError) Error() string { return e.message }

type documentRefReq struct {
	DocumentID string `json:"document_id"`
	Version    string `json:"version,omitempty"`
}

type documentSummarySource struct {
	DocumentID string                `json:"document_id"`
	Title      string                `json:"title"`
	Version    string                `json:"version"`
	Content    string                `json:"content"`
	Chunks     []documentSourceChunk `json:"chunks,omitempty"`
	// Truncated is set by normalizeFetchedDocumentSource when any size cap
	// (chunk count, per-chunk runes, or total runes) dropped document content,
	// so the prompt builder can surface the truncation marker. Not wire data.
	Truncated bool `json:"-"`
}

type documentSourceChunk struct {
	ChunkID string `json:"chunk_id,omitempty"`
	Page    int    `json:"page,omitempty"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text"`
}

type documentSourceClient interface {
	FetchSummarySource(ctx context.Context, spaceID, userID, documentID, version string, header http.Header) (*documentSummarySource, error)
}

type httpDocumentSourceClient struct {
	baseURL string
	client  *http.Client
}

func newDefaultDocumentSourceClient() documentSourceClient {
	base := strings.TrimSpace(os.Getenv("DOCUMENT_SUMMARY_SOURCE_API_URL"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("DOCUMENT_SOURCE_API_URL"))
	}
	if base == "" {
		return nil
	}
	return &httpDocumentSourceClient{
		baseURL: strings.TrimRight(base, "/"),
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *httpDocumentSourceClient) FetchSummarySource(ctx context.Context, spaceID, userID, documentID, version string, header http.Header) (*documentSummarySource, error) {
	if c == nil || c.baseURL == "" {
		return nil, errDocumentSourceNotConfigured
	}
	escapedID := url.PathEscape(documentID)
	u := c.baseURL + "/api/documents/" + escapedID + "/summary-source"
	if version != "" {
		u += "?version=" + url.QueryEscape(version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Space-Id", spaceID)
	req.Header.Set("X-User-Id", userID)
	// Forward only the Token header (authenticated by StrictAuthMiddleware).
	// Authorization belongs to the bot realm (bf_* bearer) and is never
	// validated on this route group, so forwarding it makes the effective
	// principal ambiguous to the document service.
	if v := header.Get("Token"); v != "" {
		req.Header.Set("Token", v)
	}
	if v := header.Get("Accept-Language"); v != "" {
		req.Header.Set("Accept-Language", v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &documentSourceError{status: http.StatusGatewayTimeout, message: "document source API timeout"}
		}
		return nil, &documentSourceError{status: http.StatusBadGateway, message: err.Error()}
	}
	defer resp.Body.Close()
	// 401 joins 403/404 in the RESPONSE class: from the caller's side all three mean
	// "this document is not readable with your credentials", which is actionable, and
	// reporting it as "文档服务暂不可用" told the user to wait for something that will
	// never resolve itself. In the LOG they stay separable: StrictAuthMiddleware has
	// already resolved this same Token to a user id moments earlier, so an upstream
	// 401 more often means the two services disagree about token audience/format — an
	// operator-actionable, all-users condition — while 403/404 is per-document and
	// per-user. Collapsing them in the dashboard would hide a service-wide
	// misconfiguration inside routine permission noise.
	if resp.StatusCode == http.StatusUnauthorized {
		log.Printf("[handler] document source rejected the forwarded Token (upstream 401) doc=%q user=%q space=%q — check token audience/format agreement between services", documentID, userID, spaceID)
		return nil, &documentSourceError{status: http.StatusBadRequest, message: fmt.Sprintf("document %s is not accessible (upstream %d)", documentID, resp.StatusCode)}
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return nil, &documentSourceError{status: http.StatusBadRequest, message: fmt.Sprintf("document %s is not accessible (upstream %d)", documentID, resp.StatusCode)}
	}
	// Upstream throttling is passed through as throttling. Folding it into the 502
	// outage class inflates the document-service failure signal with load events —
	// the same signal the client-disconnect branch in document_preview.go exists to
	// keep honest — and hides the retry semantics from the caller. Retry-After rides
	// along so the backoff is actionable rather than advisory.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &documentSourceError{
			status:     http.StatusTooManyRequests,
			message:    "document source API is rate limiting",
			retryAfter: sanitizeRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &documentSourceError{status: http.StatusBadGateway, message: fmt.Sprintf("document source API redirected with status %d", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Remaining 4xx (400/409/422/…) mean this service and the document service
		// disagree about the contract. That is our bug, not the user's document and not
		// a credential problem, so it stays in the 502 class where an operator will see
		// it; the real upstream status is carried in the message for the log.
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusGatewayTimeout {
			status = http.StatusGatewayTimeout
		}
		return nil, &documentSourceError{status: status, message: fmt.Sprintf("document source API status %d", resp.StatusCode)}
	}
	var out documentSummarySource
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxDocumentSourceResponseBytes))
	if err := decoder.Decode(&out); err != nil {
		return nil, &documentSourceError{status: http.StatusBadGateway, message: "document source API returned invalid or oversized payload"}
	}
	if out.DocumentID == "" {
		out.DocumentID = documentID
	}
	if out.Version == "" {
		out.Version = version
	}
	return &out, nil
}

// sanitizeRetryAfter validates an upstream Retry-After before we echo it into our
// own response header. The value is attacker-influenced in the same sense as any
// upstream header, so it is re-emitted only in the two forms RFC 9110 defines —
// delta-seconds or an HTTP-date — and dropped otherwise. Nothing downstream has to
// reason about header splitting or about a client parsing garbage.
//
// The ceiling applies to BOTH forms. Checking it only on delta-seconds would make
// the cap a formatting detail: the same "come back in a century" instruction just
// has to be written as a date to sail past it, and a past date is the mirror case
// of the negative delta-seconds already rejected.
func sanitizeRetryAfter(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return ""
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 || secs > maxRetryAfterSeconds {
			return ""
		}
		return strconv.Itoa(secs)
	}
	if t, err := http.ParseTime(v); err == nil {
		delta := time.Until(t)
		if delta < 0 || delta > maxRetryAfterSeconds*time.Second {
			return ""
		}
		return t.UTC().Format(http.TimeFormat)
	}
	return ""
}

// documentSourceClient returns the handler's configured document-source client
// (nil when DOCUMENT_SOURCE_API_URL is unset).
func (h *AgentSummaryHandler) documentSourceClient() documentSourceClient {
	return h.documentClient
}

func normalizeDocumentRefs(refs []documentRefReq) []documentRefReq {
	seen := map[string]struct{}{}
	out := make([]documentRefReq, 0, len(refs))
	for _, ref := range refs {
		ref.DocumentID = strings.TrimSpace(ref.DocumentID)
		ref.Version = strings.TrimSpace(ref.Version)
		if ref.DocumentID == "" {
			continue
		}
		key := ref.DocumentID + "\x00" + ref.Version
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocumentID == out[j].DocumentID {
			return out[i].Version < out[j].Version
		}
		return out[i].DocumentID < out[j].DocumentID
	})
	return out
}

func validateDocumentRefs(refs []documentRefReq) error {
	versionsByDoc := map[string]string{}
	for _, ref := range refs {
		if utf8.RuneCountInString(ref.DocumentID) > maxDocumentIDLen {
			return fmt.Errorf("document_id too long: %s", ref.DocumentID)
		}
		// url.PathEscape leaves "." and ".." untouched (both are unreserved), so a bare
		// dot-segment reaches the document service as GET /api/documents/../summary-source
		// and any normalizing intermediary collapses it to /api/summary-source — a
		// single-segment climb carrying the caller's own token. Every other dangerous
		// character is already percent-encoded, so rejecting the two dot-segments closes it.
		if ref.DocumentID == "." || ref.DocumentID == ".." {
			return fmt.Errorf("document_id must not be a path segment: %s", ref.DocumentID)
		}
		if utf8.RuneCountInString(ref.Version) > maxDocumentVersionLen {
			return fmt.Errorf("document version too long: %s", ref.Version)
		}
		if existing, ok := versionsByDoc[ref.DocumentID]; ok && existing != ref.Version {
			return fmt.Errorf("multiple versions of one document are not supported: %s", ref.DocumentID)
		}
		versionsByDoc[ref.DocumentID] = ref.Version
	}
	return nil
}

// docFenceGuard neutralizes untrusted document text that could close the
// <文档数据> fence early. The whole implementation — homoglyph folding, invisible
// handling, and the two structural passes — lives in fence_guard.go and is shared
// with the <引用数据> guard; see that file for why neither the delimiter set nor
// the invisible set is enumerated by hand any more.
var docFenceGuard = newFenceGuard("文档数据")

// Names kept so the existing test suite keeps addressing the guard's parts directly
// after centralization — those tests are the regression barrier for rounds 4–10 and
// are deliberately not rewritten as part of this change.
var (
	docFencePlaceholder     = docFenceGuard.placeholder
	docFenceHeadPlaceholder = docFenceGuard.headPlaceholder
	docFenceTagPattern      = docFenceGuard.tagPattern
	docFenceHeadPattern     = docFenceGuard.headPattern
)

// sanitizeDocumentFenceText neutralizes forged <文档数据> tags in untrusted document
// text, replacing them with a visible placeholder. Only ever shortens its input,
// which buildDocumentPreviewPrompt's post-assembly pass relies on.
func sanitizeDocumentFenceText(s string) string {
	return docFenceGuard.neutralize(s)
}

func normalizeFetchedDocumentSource(doc *documentSummarySource, ref documentRefReq) {
	doc.DocumentID = ref.DocumentID
	doc.Version = truncateRunes(strings.TrimSpace(doc.Version), maxDocumentVersionLen)
	if doc.Version == "" {
		doc.Version = ref.Version
	}
	doc.Title = truncateRunes(strings.TrimSpace(doc.Title), maxDocumentTitleRunes)

	rawContent := strings.TrimSpace(doc.Content)
	doc.Content = truncateRunes(rawContent, maxDocumentPromptRunes)
	// The content field is only rendered when no chunks survive (see
	// buildDocumentPreviewPrompt). Defer flagging its truncation until we know that,
	// so the marker never claims a cut the model never saw.
	contentTruncated := utf8.RuneCountInString(rawContent) > maxDocumentPromptRunes

	normalized := make([]documentSourceChunk, 0, len(doc.Chunks))
	total := 0
	kept := 0
	for _, chunk := range doc.Chunks {
		// ChunkID/Page are carried for the wire contract only — nothing downstream reads
		// them, so they are deliberately not normalized. Title IS rendered into the
		// prompt (see buildDocumentPreviewPrompt), so it is clamped like any other
		// untrusted, model-visible string.
		chunk.Title = truncateRunes(strings.TrimSpace(chunk.Title), maxDocumentTitleRunes)
		trimmed := strings.TrimSpace(chunk.Text)
		if trimmed == "" {
			// Blank chunks are dropped and must NOT consume the count cap.
			continue
		}
		if kept >= maxDocumentChunks {
			doc.Truncated = true
			break
		}
		if utf8.RuneCountInString(trimmed) > maxDocumentChunkRunes {
			doc.Truncated = true
		}
		chunk.Text = truncateRunes(trimmed, maxDocumentChunkRunes)
		total += utf8.RuneCountInString(chunk.Text)
		if total > maxDocumentPromptRunes {
			remaining := maxDocumentPromptRunes - (total - utf8.RuneCountInString(chunk.Text))
			doc.Truncated = true
			if remaining <= 0 {
				break
			}
			chunk.Text = truncateRunes(chunk.Text, remaining)
			normalized = append(normalized, chunk)
			break
		}
		normalized = append(normalized, chunk)
		kept++
	}
	doc.Chunks = normalized
	if len(normalized) == 0 && contentTruncated {
		doc.Truncated = true
	}
}

// documentPreviewHasNoContent reports whether a normalized document has anything
// left to summarize once fence tags are discounted. It cannot reuse
// sanitizeDocumentFenceText, whose placeholder is deliberately non-empty (that is
// what keeps an injected tag visible to the model as neutralized text); measuring
// emptiness needs the tags removed outright. Without this, a body consisting solely
// of "<文档数据>" clears the gate and bills a completion for an empty document,
// while a caller sending "" gets a clean 40004.
//
// The chunks-else-content precedence mirrors buildDocumentPreviewPrompt exactly:
// the builder renders doc.Content ONLY when there are no chunks. Judging the union
// instead would let a doc with fence-only chunks plus real content clear this gate
// and then bill a completion on a body the model never receives — the mirror of
// the case this gate was added to prevent.
func documentPreviewHasNoContent(doc *documentSummarySource) bool {
	if len(doc.Chunks) > 0 {
		for _, chunk := range doc.Chunks {
			if documentTextWithoutFenceTags(chunk.Text) != "" {
				return false
			}
		}
		return true
	}
	return documentTextWithoutFenceTags(doc.Content) == ""
}

func documentTextWithoutFenceTags(s string) string {
	return docFenceGuard.strip(s)
}
