package handler

import (
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
		{"spaced fence still matched", "<  文档数据 >", docFencePlaceholder},
		{"zero-width inside fence stripped", "<文​档数据>", docFencePlaceholder},
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
}

func TestValidateDocumentRefs(t *testing.T) {
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: "ok", Version: "v1"}}); err != nil {
		t.Errorf("valid ref rejected: %v", err)
	}
	longID := strings.Repeat("a", maxDocumentIDLen+1)
	if err := validateDocumentRefs([]documentRefReq{{DocumentID: longID}}); err == nil {
		t.Error("expected error for over-long document_id, got nil")
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
