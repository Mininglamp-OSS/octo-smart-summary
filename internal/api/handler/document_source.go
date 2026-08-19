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
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
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
)

var errDocumentSourceNotConfigured = errors.New("document summary source API is not configured")

type documentSourceError struct {
	status  int
	message string
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
	// 401 joins 403/404: the document service rejected the forwarded Token, which is
	// a credential condition on this document, not an outage. Reporting it as "文档服务
	// 暂不可用" told the user to wait for something that will never resolve itself.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return nil, &documentSourceError{status: http.StatusBadRequest, message: fmt.Sprintf("document %s is not accessible (upstream %d)", documentID, resp.StatusCode)}
	}
	// Upstream throttling is passed through as throttling. Folding it into the 502
	// outage class inflates the document-service failure signal with load events —
	// the same signal the client-disconnect branch in document_preview.go exists to
	// keep honest — and hides the retry semantics from the caller.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &documentSourceError{status: http.StatusTooManyRequests, message: "document source API is rate limiting"}
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

// sanitizeDocumentFenceText neutralizes untrusted document text that could close
// the <文档数据> fence early: full-width / small-form / CJK angle-and-slash folding,
// invisible-character stripping, then two structural passes.
//
// Two passes, in this order:
//
//  1. docFenceTagPattern — the full, well-formed tag. Folds to the readable
//     [文档数据] placeholder, and is what lets documentTextWithoutFenceTags strip a
//     fence-only body to the empty string.
//  2. docFenceHeadPattern — the tag *head* alone, with no closing `>` required.
//
// Pass 2 is what makes this convergent. Rounds 4–7 each widened pass 1's tail to
// cover one more well-formed shape (attributes, self-closing, repeated solidus)
// and each left a longer variant reachable: any bound on the tail is a bound the
// attacker picks, and no bound at all lets a stray `<文档数据` swallow the document
// up to a distant unrelated `>`. Neutralizing the head sidesteps the tail
// entirely — once the leading `<` is gone the remainder is inert text, whatever
// its shape — so the overlong, unclosed, and not-yet-imagined forms all collapse
// together rather than one per round.
//
// Budget invariant (relied on by buildDocumentPreviewPrompt's post-assembly pass):
// both passes can only ever shorten the text. Pass 1's shortest match is
// `<文档数据>` (6 runes) against a 6-rune placeholder; pass 2's is `<文档数据`
// (5 runes) against a 4-rune placeholder.
//
// NOTE: this intentionally duplicates the <引用数据> guard in
// agent_reference_context.go, and the two have now *diverged* — that one accepts
// neither an attribute tail nor repeated solidus. Centralizing both onto one
// tag-parameterized helper, with a fuzz target over the tag grammar, is the agreed
// next change; it is a cross-feature refactor and stays out of this PR.
var (
	docFenceInvisiblePattern = regexp.MustCompile(`[\p{Cf}\x{00ad}]`)
	// Pass 1: well-formed tag. The optional tail must *start* with whitespace or a
	// solidus, i.e. real attribute/self-closing syntax — so `<文档数据格式说明>` and
	// `x < 文档数据量 > 1000` (prose that merely contains the tag name) are left alone
	// instead of being collapsed into the placeholder. Anything this pass declines is
	// still caught by pass 2, so declining costs no containment.
	docFenceTagPattern = regexp.MustCompile(`<[\s\p{Zs}]*/*[\s\p{Zs}]*文档数据(?:[\s\p{Zs}/][^>]{0,64})?>`)
	// Pass 2: tag head, no `>` required, but the tag name must be followed by a real
	// tag boundary (whitespace, solidus, `>`, or end of text). That boundary is what
	// keeps prose intact: `<文档数据格式说明>` is a different word, not a fence, and
	// `</文档数据X` is not a well-formed closer for this tag either — the threat is a
	// tag the model reads as closing the fence, which requires the boundary. Runs
	// after pass 1 so ordinary tags still render as the nicer [文档数据]; substituting
	// this for pass 1 would break the fence-only emptiness gate (`<文档数据>` would
	// strip to `>`, not to "").
	docFenceHeadPattern = regexp.MustCompile(`<[\s\p{Zs}]*/*[\s\p{Zs}]*文档数据([\s\p{Zs}/>]|$)`)
	// Fold full-width (＜＞／), small-form (﹤﹥), and CJK (〈〉) angle brackets/solidus to
	// ASCII, and collapse control + line-separator chars, before structural matching.
	docFenceReplacer = strings.NewReplacer(
		"＜", "<", "＞", ">", "／", "/",
		"〈", "<", "〉", ">",
		"﹤", "<", "﹥", ">",
		"\r", " ", "\t", " ", "\x00", " ", "\v", " ", "\f", " ",
		"\u0085", " ", "\u2028", " ", "\u2029", " ",
	)
)

const docFencePlaceholder = "[文档数据]"

// docFenceHeadPlaceholder is deliberately 4 runes — shorter than the 5-rune
// shortest head match (`<文档数据` at end of text) — so pass 2 keeps the "sanitizing
// only ever shortens" property structurally true, with no budget-reservation
// arithmetic anywhere. The boundary character is preserved via ${1}, so the
// replacement is always exactly one rune shorter than what it replaced.
const docFenceHeadPlaceholder = "文档数据"

func sanitizeDocumentFenceText(s string) string {
	s = docFenceReplacer.Replace(s)
	s = docFenceInvisiblePattern.ReplaceAllString(s, "")
	s = docFenceTagPattern.ReplaceAllString(s, docFencePlaceholder)
	// Head pass runs second, over whatever pass 1 declined: overlong tails, unclosed
	// forms, and anything else that is not a well-formed tag.
	s = docFenceHeadPattern.ReplaceAllString(s, docFenceHeadPlaceholder+"${1}")
	return strings.TrimSpace(s)
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
	s = docFenceReplacer.Replace(s)
	s = docFenceInvisiblePattern.ReplaceAllString(s, "")
	s = docFenceTagPattern.ReplaceAllString(s, "")
	return strings.TrimSpace(docFenceHeadPattern.ReplaceAllString(s, "${1}"))
}
