package service

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestNormalizeGeneratedCitations(t *testing.T) {
	citations := []model.Citation{{Index: 9}, {Index: 73}, {Index: 88}}
	got, err := NormalizeGeneratedCitations("Budget [9,73]. ROI [88].", citations)
	if err != nil || got != "Budget [9][73]. ROI [88]." {
		t.Fatalf("%q %v", got, err)
	}
	if len(CleanUnreferencedCitations(got, citations)) != 3 {
		t.Fatal("lost group sources")
	}
	for _, text := range []string{"Missing [9,74] valid [88]", "Gap [9–73] valid [88]"} {
		if _, err := NormalizeGeneratedCitations(text, citations); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestCleanUnreferencedCitations_KeepsReferenced(t *testing.T) {
	content := "This is a summary with [1] and [3] references."
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
		{Index: 3, Sender: "Carol", Content: "foo"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 2 {
		t.Fatalf("expected 2 citations, got %d", len(result))
	}
	if result[0].Index != 1 {
		t.Errorf("expected first citation index=1, got %d", result[0].Index)
	}
	if result[1].Index != 3 {
		t.Errorf("expected second citation index=3, got %d", result[1].Index)
	}
}

func TestCleanUnreferencedCitations_RemovesAll(t *testing.T) {
	content := "No citations here."
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 0 {
		t.Errorf("expected 0 citations, got %d", len(result))
	}
}

func TestCleanUnreferencedCitations_EmptyCitations(t *testing.T) {
	content := "Some content with [1]."
	var citations []model.Citation

	result := CleanUnreferencedCitations(content, citations)

	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestCleanUnreferencedCitations_SkipsMarkdownLinks(t *testing.T) {
	content := "See [1](https://example.com) and real [2] citation."
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(result))
	}
	if result[0].Index != 2 {
		t.Errorf("expected citation index=2, got %d", result[0].Index)
	}
}

func TestCleanUnreferencedCitations_SkipsFencedCode(t *testing.T) {
	content := "Before\n```\ncode block [1] reference\n```\nAfter [2] reference."
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(result))
	}
	if result[0].Index != 2 {
		t.Errorf("expected citation index=2, got %d", result[0].Index)
	}
}

func TestCleanUnreferencedCitations_SkipsInlineCode(t *testing.T) {
	content := "Use `array[1]` for access, and see [2] for details."
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(result))
	}
	if result[0].Index != 2 {
		t.Errorf("expected citation index=2, got %d", result[0].Index)
	}
}

func TestCleanUnreferencedCitations_EndOfContent(t *testing.T) {
	content := "Summary ends with reference [1]"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(result))
	}
	if result[0].Index != 1 {
		t.Errorf("expected citation index=1, got %d", result[0].Index)
	}
}

func TestCleanUnreferencedCitations_MultipleSameIndex(t *testing.T) {
	content := "First [1] and again [1] reference."
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(result))
	}
	if result[0].Index != 1 {
		t.Errorf("expected citation index=1, got %d", result[0].Index)
	}
}

func TestCleanUnreferencedCitations_LargeIndex(t *testing.T) {
	content := "Reference [12345] here."
	citations := []model.Citation{
		{Index: 12345, Sender: "Alice", Content: "hello"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(result))
	}
}

func TestCleanUnreferencedCitations_UnclosedFencedCode(t *testing.T) {
	content := "Before\n```\ncode block [1] reference\nno closing fence [2]"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 0 {
		t.Errorf("expected 0 citations (all in unclosed fence), got %d", len(result))
	}
}

func TestCleanUnreferencedCitations_FencedCodeWithLanguage(t *testing.T) {
	content := "Before\n```go\narr[1] = value\n```\nAfter [2] reference."
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "hello"},
		{Index: 2, Sender: "Bob", Content: "world"},
	}

	result := CleanUnreferencedCitations(content, citations)

	if len(result) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(result))
	}
	if result[0].Index != 2 {
		t.Errorf("expected citation index=2, got %d", result[0].Index)
	}
}
