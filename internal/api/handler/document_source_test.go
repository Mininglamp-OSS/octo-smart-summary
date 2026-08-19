package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeDocumentFenceText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "普通文本 hello", "普通文本 hello"},
		{"closing fence neutralized", "</文档数据>", docFencePlaceholder},
		{"opening fence neutralized", "<文档数据>", docFencePlaceholder},
		{"full-width angle/slash folded", "＜/文档数据＞", docFencePlaceholder},
		{"cjk angle brackets folded", "〈/文档数据〉", docFencePlaceholder},
		{"small-form brackets folded", "﹤/文档数据﹥", docFencePlaceholder},
		{"repeated fullwidth solidus folded", "</／文档数据>", docFencePlaceholder},
		{"spaced fence still matched", "<  文档数据 >", docFencePlaceholder},
		{"zero-width inside fence stripped", "<文​档数据>", docFencePlaceholder},
		// Attribute-bearing closers: a whitespace-only tail rejected these, so they were
		// emitted verbatim into the prompt — strictly easier to author than the
		// cross-chunk split, since the attacker only types one extra character.
		{"closing fence with attribute", "</文档数据 attr=x>", docFencePlaceholder},
		{"self-closing fence", "</文档数据/>", docFencePlaceholder},
		{"tab before attribute", "</文档数据\t attr>", docFencePlaceholder},
		{"opening fence with attribute", "<文档数据 attr=x>", docFencePlaceholder},
		// The attribute tail is bounded so a stray "<文档数据" cannot swallow the document
		// up to some distant unrelated ">".
		{
			"overlong attribute tail is not swallowed",
			"<文档数据 " + strings.Repeat("a", 100) + "> 尾巴",
			"<文档数据 " + strings.Repeat("a", 100) + "> 尾巴",
		},
		// Unclosed form: documented as out of reach for this pattern. Pinned so the gap
		// is visible rather than assumed closed.
		{"unclosed fence survives (known gap)", "a </文档数据 INJECT", "a </文档数据 INJECT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeDocumentFenceText(tc.in); got != tc.want {
				t.Fatalf("sanitizeDocumentFenceText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeFetchedDocumentSource_ContentAndChunkCaps(t *testing.T) {
	doc := &documentSummarySource{
		Content: strings.Repeat("字", maxDocumentPromptRunes+500),
	}
	// 250 chunks (> maxDocumentChunks), each 1 rune so the total-rune cap does not bite first.
	for i := 0; i < maxDocumentChunks+50; i++ {
		doc.Chunks = append(doc.Chunks, documentSourceChunk{Text: "x"})
	}
	// One oversized chunk to exercise per-chunk truncation (placed within the cap window).
	doc.Chunks[0] = documentSourceChunk{Text: strings.Repeat("超", maxDocumentChunkRunes+100)}

	normalizeFetchedDocumentSource(doc, documentRefReq{DocumentID: "d1", Version: "v2"})

	if n := utf8.RuneCountInString(doc.Content); n > maxDocumentPromptRunes {
		t.Errorf("content not capped: got %d runes, want <= %d", n, maxDocumentPromptRunes)
	}
	if len(doc.Chunks) > maxDocumentChunks {
		t.Errorf("chunk count not capped: got %d, want <= %d", len(doc.Chunks), maxDocumentChunks)
	}
	if n := utf8.RuneCountInString(doc.Chunks[0].Text); n > maxDocumentChunkRunes {
		t.Errorf("oversized chunk not truncated: got %d runes, want <= %d", n, maxDocumentChunkRunes)
	}
	if doc.DocumentID != "d1" {
		t.Errorf("DocumentID not taken from ref: got %q", doc.DocumentID)
	}
	if doc.Version != "v2" {
		t.Errorf("Version fallback failed: got %q", doc.Version)
	}
	if !doc.Truncated {
		t.Error("Truncated flag should be set when caps drop content")
	}
	// Regression guard (round-3 P0): a capped doc must still carry its retained
	// content into the prompt, not just the marker.
	prompt := buildDocumentPreviewPrompt(doc)
	if !strings.Contains(prompt, "[文档内容已按长度上限截断]") {
		t.Error("capped doc should carry the truncation marker in the prompt")
	}
	if utf8.RuneCountInString(prompt) < maxDocumentChunkRunes {
		t.Errorf("retained ~%d-rune chunk missing from prompt: only %d runes", maxDocumentChunkRunes, utf8.RuneCountInString(prompt))
	}
}

func TestNormalizeFetchedDocumentSource_BlankChunksDoNotConsumeCap(t *testing.T) {
	doc := &documentSummarySource{}
	for i := 0; i < maxDocumentChunks; i++ {
		doc.Chunks = append(doc.Chunks, documentSourceChunk{Text: "   "}) // blank
	}
	doc.Chunks = append(doc.Chunks, documentSourceChunk{Text: "真正有内容的一段"})

	normalizeFetchedDocumentSource(doc, documentRefReq{DocumentID: "d1"})

	if len(doc.Chunks) != 1 {
		t.Fatalf("expected the one real chunk to survive, got %d chunks", len(doc.Chunks))
	}
	if doc.Chunks[0].Text != "真正有内容的一段" {
		t.Errorf("real chunk dropped; got %q", doc.Chunks[0].Text)
	}
}

func TestFetchSummarySource_HTTPBoundary(t *testing.T) {
	var gotToken, gotAuth string
	// Redirect target that WOULD succeed if the client followed the 302. Pointing the
	// Location at an unresolvable host instead (e.g. evil.example) makes the case pass
	// whether or not CheckRedirect exists — following the redirect just fails DNS and
	// still maps to 502 — so deleting the production callback left the suite green.
	var redirectFollowed bool
	followTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectFollowed = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"document_id":"d1","title":"leaked","version":"v","content":"c"}`))
	}))
	defer followTarget.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Token")
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Query().Get("version") {
		case "redirect":
			w.Header().Set("Location", followTarget.URL)
			w.WriteHeader(http.StatusFound) // 302
		case "forbidden":
			w.WriteHeader(http.StatusForbidden) // 403
		case "notfound":
			w.WriteHeader(http.StatusNotFound) // 404
		case "big":
			w.WriteHeader(http.StatusOK)
			// Syntactically VALID JSON larger than the cap. The previous case emitted raw
			// "x"s, which fail to decode at the first byte whether or not the cap exists —
			// removing the LimitReader left the suite green. A well-formed body can only be
			// rejected by the cap actually biting.
			_, _ = w.Write([]byte(`{"document_id":"d1","title":"t","version":"v","content":"`))
			chunk := strings.Repeat("x", 1<<16)
			for i := 0; i < 80; i++ {
				_, _ = w.Write([]byte(chunk))
			}
			_, _ = w.Write([]byte(`"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"document_id":"d1","title":"t","version":"v","content":"c"}`))
		}
	}))
	defer srv.Close()

	// Build the client through the PRODUCTION constructor. Hand-rolling an
	// http.Client here would test this test's own CheckRedirect callback rather than
	// the one newDefaultDocumentSourceClient installs — deleting the production
	// callback then leaves the suite green.
	t.Setenv("DOCUMENT_SUMMARY_SOURCE_API_URL", srv.URL)
	t.Setenv("DOCUMENT_SOURCE_API_URL", "")
	client := newDefaultDocumentSourceClient()
	if client == nil {
		t.Fatal("newDefaultDocumentSourceClient returned nil with the env var set")
	}
	c, ok := client.(*httpDocumentSourceClient)
	if !ok {
		t.Fatalf("unexpected client type %T", client)
	}
	hdr := http.Header{}
	hdr.Set("Token", "tok-123")
	hdr.Set("Authorization", "bf_should_not_forward")

	statusOf := func(version string) int {
		_, err := c.FetchSummarySource(context.Background(), "sp", "u", "d1", version, hdr)
		var se *documentSourceError
		if err != nil && errors.As(err, &se) {
			return se.status
		}
		if err != nil {
			t.Fatalf("unexpected non-documentSourceError: %v", err)
		}
		return 200
	}

	if s := statusOf("redirect"); s != http.StatusBadGateway {
		t.Errorf("302 should map to 502, got %d", s)
	}
	if redirectFollowed {
		t.Error("client followed the 302: CheckRedirect must refuse redirects so the user's Token is never replayed to another host")
	}
	if s := statusOf("forbidden"); s != http.StatusBadRequest {
		t.Errorf("403 should map to 400, got %d", s)
	}
	if s := statusOf("notfound"); s != http.StatusBadRequest {
		t.Errorf("404 should map to 400, got %d", s)
	}
	if s := statusOf("big"); s != http.StatusBadGateway {
		t.Errorf("oversized payload should map to 502, got %d", s)
	}
	if _, err := c.FetchSummarySource(context.Background(), "sp", "u", "d1", "", hdr); err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
	if gotToken != "tok-123" {
		t.Errorf("Token not forwarded: got %q", gotToken)
	}
	if gotAuth != "" {
		t.Errorf("Authorization must not be forwarded, upstream saw %q", gotAuth)
	}
}

func TestValidateDocumentRefs(t *testing.T) {
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: "ok", Version: "v1"}}); err != nil {
		t.Errorf("valid ref rejected: %v", err)
	}
	longID := strings.Repeat("a", maxDocumentIDLen+1)
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: longID}}); err == nil {
		t.Error("expected error for over-long document_id, got nil")
	}
	// url.PathEscape leaves dot-segments intact, so ".." would reach the document
	// service as /api/documents/../summary-source.
	for _, id := range []string{".", ".."} {
		if err := validateDocumentRefs([]documentRefReq{{DocumentID: id}}); err == nil {
			t.Errorf("expected error for dot-segment document_id %q, got nil", id)
		}
	}
	multi := []documentRefReq{{DocumentID: "A", Version: "1"}, {DocumentID: "A", Version: "2"}}
	if err := validateDocumentRefs(multi); err == nil {
		t.Error("expected error for multiple versions of one document, got nil")
	}
}

func TestNormalizeDocumentRefs_DedupTrimSort(t *testing.T) {
	in := []documentRefReq{
		{DocumentID: " B ", Version: "1"},
		{DocumentID: "A", Version: "1"},
		{DocumentID: "A", Version: "1"},  // dup
		{DocumentID: "  ", Version: "x"}, // empty after trim -> dropped
	}
	out := normalizeDocumentRefs(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 refs after dedup/drop, got %d: %+v", len(out), out)
	}
	if out[0].DocumentID != "A" || out[1].DocumentID != "B" {
		t.Errorf("expected sorted [A, B], got [%s, %s]", out[0].DocumentID, out[1].DocumentID)
	}
}
