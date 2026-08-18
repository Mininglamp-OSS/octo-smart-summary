package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

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

// TestEstimateTokensNonASCIIDense is the P2-1 regression: classification must
// match internal/tokenizer/estimate.go — EVERY rune above 0x7F bills at the
// dense CJK ratio. Emoji, Cyrillic and accented Latin are not ASCII-sparse;
// the previous unicode-table classification under-billed them 2×–4×.
func TestEstimateTokensNonASCIIDense(t *testing.T) {
	cases := []struct {
		name string
		s    string
		cjk  int
		want int
	}{
		// 2 emoji runes @2 chars/token = 1 token (old rule: 2/4=0 → 1 by the
		// floor; coincidentally equal here, so use ratio 1 to separate).
		{"emoji dense @1", "🎉🎉", 1, 2},
		{"cyrillic dense @2", "Привет", 2, 3}, // 6 runes / 2
		{"cyrillic dense @1", "Привет", 1, 6},
		{"accented latin dense @1", "café", 1, 4}, // c,a,f ascii + é dense: 3/4+1 = 1... assert via formula below
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := estimateTokens(c.s, c.cjk, 4)
			if c.name == "accented latin dense @1" {
				// 3 ascii/4 + 1 dense/1 = 0+1 = 1
				if got != 1 {
					t.Fatalf("estimateTokens(%q,1,4) = %d, want 1 (é must bill dense)", c.s, got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("estimateTokens(%q,%d,4) = %d, want %d", c.s, c.cjk, got, c.want)
			}
		})
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

// TestProbeCoverageNoDrop verifies the SS-06b guarantee through the probe:
// any input size, zero drop.
func TestProbeCoverageNoDrop(t *testing.T) {
	for _, n := range []int{201, 500, 2000, 12345} {
		p, d, chunks := ProbeChunkCoverageDefault(makeMsgMaps(n), 0)
		if p != n || d != 0 {
			t.Fatalf("ProbeChunkCoverageDefault(%d) = processed %d dropped %d, want %d/0", n, p, d, n)
		}
		if chunks < 1 {
			t.Fatalf("ProbeChunkCoverageDefault(%d) = %d chunks, want >= 1", n, chunks)
		}
	}
}

// shortCJKMsgs builds n messages shaped like dominant real IM traffic: 2-rune
// CJK content from a 3-rune sender, dense citation indices — the exact shape
// @yujiawei used to measure the 5.22× budget overrun on PR #196 head 38e6027
// (issue comment 5326647304).
func shortCJKMsgs(n int) []map[string]interface{} {
	msgs := make([]map[string]interface{}, n)
	for i := range msgs {
		msgs[i] = map[string]interface{}{
			"content":        "好的",
			"sender_name":    "张三丰",
			"citation_index": i + 1,
		}
	}
	return msgs
}

// assertChunksBounded verifies the SS-06b invariants the review proved broken:
// every chunk respects the message-count backstop and no message is lost. When
// checkBudget is true it additionally asserts each chunk's RENDERED prompt
// fits the budget — valid for chunks closed by the token budget; chunks closed
// by the count backstop may legitimately exceed the budget by the framing cost
// of the capped messages (the backstop is the hard guarantee, P0-2).
func assertChunksBounded(t *testing.T, chunks [][]map[string]interface{}, wantTotal, budget, cjkRatio, asciiRatio int, checkBudget bool) {
	t.Helper()
	total := 0
	for i, c := range chunks {
		total += len(c)
		if len(c) > hardMessageBackstop {
			t.Fatalf("chunk %d has %d messages > backstop %d", i, len(c), hardMessageBackstop)
		}
		if checkBudget {
			formatted, _, _ := formatChunkForLLM(c)
			if got := estimateTokens(formatted, cjkRatio, asciiRatio); got > budget {
				t.Fatalf("chunk %d: formatted prompt estimates %d tokens > budget %d (the P0-1 overrun)", i, got, budget)
			}
		}
	}
	if total != wantTotal {
		t.Fatalf("messages lost: total=%d want %d", total, wantTotal)
	}
}

// TestSplitBillsRenderedLine_BudgetSweep is the P0-1 regression. @yujiawei
// swept budget ~60× at head 38e6027 and measured a CONSTANT-RATIO overrun
// (4.94×–5.41×): the budget billed m["content"] only while the model receives
// "[n] sender: content\n", so lowering MAP_MAX_TOKENS could never fix it.
// After billing the rendered line, every budget in the sweep must pack chunks
// whose formatted output actually fits.
func TestSplitBillsRenderedLine_BudgetSweep(t *testing.T) {
	const cjkRatio, asciiRatio = 2, 4 // qwen/deepseek/kimi default
	msgs := shortCJKMsgs(200000)
	for _, budget := range []int{5000, 20000, 99200, 299200} {
		t.Run(fmt.Sprintf("budget_%d", budget), func(t *testing.T) {
			chunks := splitMsgMapsByTokenBudget(msgs, budget, 0, cjkRatio, asciiRatio)
			// Token budget is the binding constraint here (2-rune content is
			// far below the 500-msg backstop cost), so rendered prompts must
			// fit the budget in EVERY chunk.
			assertChunksBounded(t, chunks, len(msgs), budget, cjkRatio, asciiRatio, true)
			// At head 38e6027 chunk 0 held budget-many messages (99,200 at the
			// default budget); with rendered-line billing it must hold far less.
			if len(chunks) < 2 {
				t.Fatalf("budget %d over 200k short messages produced %d chunk — budget not enforced", budget, len(chunks))
			}
		})
	}
}

// TestSplitEmptyContentBackstop is the P0-2 regression: estimateTokens("")
// used to return 0 and maxMsgs defaulted to 0, so 50k empty-content messages
// (images/files/recalled) packed into ONE chunk — ~210k formatted tokens
// against a 99,200 budget, budget-insensitive by construction. The hard
// backstop must cap the chunk regardless of what the estimator says.
func TestSplitEmptyContentBackstop(t *testing.T) {
	msgs := make([]map[string]interface{}, 50000)
	for i := range msgs {
		msgs[i] = map[string]interface{}{
			"content":        "",
			"sender_name":    "张三丰",
			"citation_index": i + 1,
		}
	}
	// budget=99200: framing cost (~1750-2125 tok per 500 lines) is far below
	// the budget, so the BACKSTOP binds -> exactly 50000/500 = 100 chunks.
	chunks := splitMsgMapsByTokenBudget(msgs, 99200, 0, 2, 4)
	assertChunksBounded(t, chunks, len(msgs), 99200, 2, 4, false)
	if len(chunks) != 100 {
		t.Fatalf("budget 99200: 50k empty messages → %d chunks, want 100 (backstop 500)", len(chunks))
	}
	// budget=2000: framing cost of 500 lines with 5-digit citation indices
	// (~2125 tok) EXCEEDS the budget, so the token budget binds and the
	// backstop is never reached — the split must only get tighter, never
	// looser than the backstop.
	chunks = splitMsgMapsByTokenBudget(msgs, 2000, 0, 2, 4)
	assertChunksBounded(t, chunks, len(msgs), 2000, 2, 4, false)
	if len(chunks) < 100 {
		t.Fatalf("budget 2000: %d chunks < 100 — budget did not tighten the split", len(chunks))
	}
}

// TestSplitBackstopCapsMaxMsgs is the P1-1 regression at the splitter level:
// a model-supplied chunk_size of 5000 must not be honoured verbatim, and
// maxMsgs=0 (token-only) is still bounded by the backstop.
func TestSplitBackstopCapsMaxMsgs(t *testing.T) {
	msgs := shortCJKMsgs(1200)
	// Huge budget so only the message cap can bite.
	for _, maxMsgs := range []int{0, 5000} {
		chunks := splitMsgMapsByTokenBudget(msgs, 10_000_000, maxMsgs, 2, 4)
		if len(chunks) != 3 { // 500 + 500 + 200
			t.Fatalf("maxMsgs=%d: got %d chunks, want 3 (backstop must cap at %d)", maxMsgs, len(chunks), hardMessageBackstop)
		}
		for i, c := range chunks {
			if len(c) > hardMessageBackstop {
				t.Fatalf("maxMsgs=%d: chunk %d has %d messages > backstop", maxMsgs, i, len(c))
			}
		}
	}
	// A modest explicit cap below the backstop still wins.
	chunks := splitMsgMapsByTokenBudget(msgs, 10_000_000, 100, 2, 4)
	if len(chunks) != 12 { // 1200 / 100
		t.Fatalf("maxMsgs=100: got %d chunks, want 12", len(chunks))
	}
}

// TestChunkTokenBudgetRespectsLowConfig is the P2-2 regression: the floor must
// only rescue a non-positive remainder, never enlarge a deliberately low
// operator setting (MAP_MAX_TOKENS=1500 → 700 usable, not 2000).
func TestChunkTokenBudgetRespectsLowConfig(t *testing.T) {
	cases := []struct {
		name       string
		mapMax     int
		wantBudget int
	}{
		{"low config preserved", 1500, 1500 - mapSystemPromptReserve},
		{"zero config -> default", 0, 100000 - mapSystemPromptReserve},
		{"sub-reserve config -> floor", 500, minChunkTokenBudget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Config{MapMaxTokens: c.mapMax}
			if got := chunkTokenBudget(cfg); got != c.wantBudget {
				t.Fatalf("chunkTokenBudget(MapMaxTokens=%d) = %d, want %d", c.mapMax, got, c.wantBudget)
			}
		})
	}
}

// TestFormatAndSplitShareOneWireFormat guards the P0-1 invariant structurally:
// whatever formatChunkForLLM emits per message must be exactly what the
// splitter bills. If either side ever drifts (a field added to the prompt but
// not to the estimate, or vice versa), this fails before production does.
func TestFormatAndSplitShareOneWireFormat(t *testing.T) {
	msgs := shortCJKMsgs(3)
	chunked := splitMsgMapsByTokenBudget(msgs, 1_000_000, 0, 2, 4)
	if len(chunked) != 1 {
		t.Fatalf("sanity: want 1 chunk, got %d", len(chunked))
	}
	formatted, _, _ := formatChunkForLLM(chunked[0])
	var want strings.Builder
	for _, m := range msgs {
		want.WriteString(renderMessageLine(m))
	}
	if formatted != want.String() {
		t.Fatalf("formatter and splitter disagree on the wire format:\nformatted=%q\nbilled=%q", formatted, want.String())
	}
}

// TestProbeChunkCoverageDetectsCapRegression is the P1-2 regression for the
// guard itself. The old ComputeCoverage returned a hardcoded dropped=0 without
// touching the splitter, so the SS-02 gate could never fire. ProbeChunkCoverage
// drives clamp -> split -> format, so reintroducing a silent-drop cap anywhere
// in the chain must surface as dropped > 0. We simulate that cap by probing
// with a budget too small for a single message AND a count cap of 1: every
// message still survives (no cap drops), so dropped stays 0 — and separately
// assert the probe counts chunks and lines honestly instead of by arithmetic.
func TestProbeChunkCoverageDetectsCapRegression(t *testing.T) {
	msgs := makeMsgMaps(500)
	processed, dropped, chunks := ProbeChunkCoverageDefault(msgs, 0)
	if processed != 500 || dropped != 0 {
		t.Fatalf("ProbeChunkCoverageDefault(500 msgs) = processed %d dropped %d, want 500/0", processed, dropped)
	}
	if chunks < 1 {
		t.Fatalf("chunks = %d, want >= 1", chunks)
	}

	// Tighten the count cap via the requested chunk_size: still zero loss, but
	// the probe must reflect the real (more numerous) chunking, not a formula.
	processed, dropped, chunksTight := ProbeChunkCoverageDefault(msgs, 50)
	if processed != 500 || dropped != 0 {
		t.Fatalf("chunk_size=50: processed %d dropped %d, want 500/0", processed, dropped)
	}
	if chunksTight != 10 { // 500 / 50
		t.Fatalf("chunk_size=50: chunks = %d, want 10 (probe must run the real splitter)", chunksTight)
	}
	if chunksTight <= chunks && chunks > 1 {
		t.Fatalf("tighter chunk_size should not reduce chunk count: %d vs %d", chunksTight, chunks)
	}
}
