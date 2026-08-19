package handler

// Shared document-source infrastructure.
//
// The client that fetches a document's summarize-ready content from the document
// service, plus the request/response shapes, fence sanitization, and chunk
// normalization that any document-summarizing handler builds on. This is the
// common substrate under both the ephemeral AI 速览 preview (document_preview.go)
// and the persisted agent document summary — kept in its own file so neither
// depends on the other.

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
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return nil, &documentSourceError{status: http.StatusBadRequest, message: fmt.Sprintf("document %s is not accessible", documentID)}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &documentSourceError{status: http.StatusBadGateway, message: fmt.Sprintf("document source API redirected with status %d", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
// the <文档数据> fence early: full-width angle/slash folding, invisible character
// stripping, then structural regex matching with a non-empty placeholder.
var (
	docFenceInvisiblePattern = regexp.MustCompile(`[\p{Cf}\x{00ad}]`)
	docFenceTagPattern       = regexp.MustCompile(`<[\s\p{Zs}]*/?[\s\p{Zs}]*文档数据[\s\p{Zs}]*>`)
)

const docFencePlaceholder = "[文档数据]"

func sanitizeDocumentFenceText(s string) string {
	s = strings.NewReplacer(
		"＜", "<",
		"＞", ">",
		"／", "/",
		"\r", " ",
		"\t", " ",
		"\x00", " ",
		"\v", " ",
		"\f", " ",
		"", " ",
		" ", " ",
		" ", " ",
	).Replace(s)
	s = docFenceInvisiblePattern.ReplaceAllString(s, "")
	s = docFenceTagPattern.ReplaceAllString(s, docFencePlaceholder)
	return strings.TrimSpace(s)
}

func normalizeFetchedDocumentSource(doc *documentSummarySource, ref documentRefReq) {
	doc.DocumentID = ref.DocumentID
	doc.Version = truncateRunes(strings.TrimSpace(doc.Version), maxDocumentVersionLen)
	if doc.Version == "" {
		doc.Version = ref.Version
	}
	doc.Title = truncateRunes(strings.TrimSpace(doc.Title), maxDocumentTitleRunes)
	doc.Content = truncateRunes(strings.TrimSpace(doc.Content), maxDocumentPromptRunes)
	normalized := make([]documentSourceChunk, 0, len(doc.Chunks))
	total := 0
	for i, chunk := range doc.Chunks {
		if i >= maxDocumentChunks {
			break
		}
		chunk.ChunkID = truncateRunes(strings.TrimSpace(chunk.ChunkID), maxDocumentVersionLen)
		chunk.Title = truncateRunes(strings.TrimSpace(chunk.Title), maxDocumentTitleRunes)
		chunk.Text = truncateRunes(strings.TrimSpace(chunk.Text), maxDocumentChunkRunes)
		if chunk.Text == "" {
			continue
		}
		total += utf8.RuneCountInString(chunk.Text)
		if total > maxDocumentPromptRunes {
			remaining := maxDocumentPromptRunes - (total - utf8.RuneCountInString(chunk.Text))
			if remaining <= 0 {
				break
			}
			chunk.Text = truncateRunes(chunk.Text, remaining)
			normalized = append(normalized, chunk)
			break
		}
		normalized = append(normalized, chunk)
	}
	doc.Chunks = normalized
}
