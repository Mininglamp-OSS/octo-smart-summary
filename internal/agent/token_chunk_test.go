package agent

import "testing"

func mm(content string) map[string]interface{} {
	return map[string]interface{}{"content": content, "citation_index": 1, "sender_name": "u"}
}

func TestEstimateTokens(t *testing.T) {
	// CJK is dense (1 char/token by default), ASCII sparse (~4 chars/token).
	if got := estimateTokens("你好世界", 1, 4); got != 4 {
		t.Errorf("4 CJK @1/tok = %d, want 4", got)
	}
	if got := estimateTokens("hello world!", 1, 4); got != 3 {
		t.Errorf("12 ascii @4/tok = %d, want 3", got)
	}
	if got := estimateTokens("", 1, 4); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
	if got := estimateTokens("x", 1, 4); got != 1 {
		t.Errorf("non-empty must be >=1, got %d", got)
	}
	// Zero ratios must not divide-by-zero.
	if got := estimateTokens("abc", 0, 0); got < 1 {
		t.Errorf("zero ratios guarded, got %d", got)
	}
}

func TestSplitByTokenBudget(t *testing.T) {
	// 10 messages, ~10 tokens each (40 CJK chars @1/tok... use 10 CJK chars).
	msgs := make([]map[string]interface{}, 10)
	for i := range msgs {
		msgs[i] = mm("一二三四五六七八九十") // 10 CJK ≈ 10 tokens
	}
	// budget 25 tokens → ~2 msgs/chunk → 5 chunks.
	chunks := splitMsgMapsByTokenBudget(msgs, 25, 0, 1, 4)
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != 10 {
		t.Fatalf("no message may be dropped: total=%d want 10", total)
	}
	if len(chunks) < 4 || len(chunks) > 6 {
		t.Fatalf("budget 25 over ~10-tok msgs → ~5 chunks, got %d", len(chunks))
	}

	// maxMsgs cap overrides a generous budget.
	capped := splitMsgMapsByTokenBudget(msgs, 100000, 3, 1, 4)
	if len(capped) != 4 { // 3,3,3,1
		t.Fatalf("maxMsgs=3 over 10 → 4 chunks, got %d", len(capped))
	}
}

func TestSplitOversizedMessageOwnChunk(t *testing.T) {
	big := ""
	for i := 0; i < 500; i++ {
		big += "字"
	}
	msgs := []map[string]interface{}{mm("短"), mm(big), mm("短")}
	// budget 50 → the 500-token message can't merge; it stands alone.
	chunks := splitMsgMapsByTokenBudget(msgs, 50, 0, 1, 4)
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != 3 {
		t.Fatalf("oversized message dropped: total=%d want 3", total)
	}
}

// TestComputeCoverageNoDrop verifies the SS-06b guarantee: any input, zero drop.
func TestComputeCoverageNoDrop(t *testing.T) {
	for _, n := range []int{201, 500, 2000, 12345} {
		p, d, _ := ComputeCoverage(n, 0)
		if p != n || d != 0 {
			t.Fatalf("ComputeCoverage(%d) = processed %d dropped %d, want %d/0", n, p, d, n)
		}
	}
}
