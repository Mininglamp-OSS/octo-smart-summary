package service

import (
	"context"
	"strings"
	"testing"
)

func TestGenerateAbstract_BestEffortGuards(t *testing.T) {
	ctx := context.Background()
	// nil client → empty, no panic/call.
	if got := GenerateAbstract(ctx, nil, "some content"); got != "" {
		t.Fatalf("nil llm should return empty, got %q", got)
	}
	// empty / whitespace content → empty, no call attempted (llm non-nil but
	// unused; passing a zero-value client is fine because it is never invoked).
	if got := GenerateAbstract(ctx, &LLMClient{}, "   \n  "); got != "" {
		t.Fatalf("blank content should return empty, got %q", got)
	}
}

func TestNormalizeAbstract(t *testing.T) {
	if got := normalizeAbstract("  \n hi there \n "); got != "hi there" {
		t.Fatalf("trim failed: %q", got)
	}
	if got := normalizeAbstract("   "); got != "" {
		t.Fatalf("blank should normalize to empty, got %q", got)
	}
	long := strings.Repeat("摘", maxAbstractRunes+50)
	got := normalizeAbstract(long)
	if n := len([]rune(got)); n > maxAbstractRunes {
		t.Fatalf("expected cap at %d runes, got %d", maxAbstractRunes, n)
	}
}
