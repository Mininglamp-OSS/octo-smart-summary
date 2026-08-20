package agent

import (
	"fmt"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

// SS-06b: token-aware chunking replaces the fixed message-count split so Map
// chunks are balanced by content (defect #9: uneven-length messages caused wild
// token imbalance) and the 200-message format cap can be removed without risking
// context overflow — each chunk is bounded by a token budget that bills the
// RENDERED prompt lines (fix-forward for PR #196 review P0-1), plus a hard
// message-count backstop (P0-2) so no estimator bug can produce an unbounded
// chunk.
const (
	// mapSystemPromptReserve is subtracted from the Map token budget to leave
	// room for the fixed summarize system prompt.
	mapSystemPromptReserve = 800
	// minChunkTokenBudget is the fallback budget used ONLY when the configured
	// Map budget minus the system-prompt reserve leaves nothing usable
	// (config <= reserve). A deliberately low MAP_MAX_TOKENS is never silently
	// enlarged (PR #196 review P2-2).
	minChunkTokenBudget = 2000
	// hardMessageBackstop caps messages per chunk regardless of the token
	// budget and the chunk_size hint, so degenerate estimates (e.g. empty
	// content, which renders to a cheap framing-only line) can never pack an
	// unbounded number of messages into one chunk (PR #196 review P0-2).
	//
	// 200 restores EXACTLY the bound main relied on: the pre-SS-06b formatter
	// hard-capped every chunk at 200 messages unconditionally. Round-3 review
	// (yujiawei, review 4960791240) measured with an exact tiktoken tokenizer
	// that estimateTokens under-bills structural ASCII ~1.9× (real BPE ≈ 2
	// chars/token vs the 4 chars/token estimate), so a budget-bound chunk at
	// backstop 500 could carry ~1.9× its nominal budget in REAL tokens —
	// reachable via the model-settable chunk_size ∈ [201, 500], a route main
	// did not have. Capping at 200 closes that route; the default path
	// (chunk_size unset → 200) is unchanged. The fix is NOT a denser ASCII
	// ratio: that would re-diverge from internal/tokenizer/estimate.go
	// (undoing the P2-1 parity fix) and ~4× Map-call counts for ASCII traffic.
	hardMessageBackstop = 200
	// minSaneMapMaxTokens mirrors the worker-path guard
	// (internal/worker/personal_processor.go: resolved MapMaxTokens < 10000 →
	// default, loudly). A resolved Map budget below this is treated as a
	// degenerate operator setting: MAP_MAX_TOKENS just above the system-prompt
	// reserve (e.g. 801) leaves a usable budget of ~1 token and splits one
	// message per chunk — a 100k-message invocation becomes 100k sequential
	// LLM calls (round-3 review, MAP_MAX_TOKENS 800→801 cliff; yujiawei P2-4).
	minSaneMapMaxTokens = 10000
	// fallbackMapMaxTokens is the loud fallback for a degenerate setting.
	// Mirrors config.defaultMapMaxTokens (unexported there).
	fallbackMapMaxTokens = 100000
)

// estimateTokens is a pure, cgo-free token estimate matching the tokenizer's
// fallback mode. Classification follows internal/tokenizer/estimate.go exactly
// — every rune above 0x7F is token-dense (~cjkRatio chars/token), everything
// else is sparser (~asciiRatio chars/token) — so emoji, Cyrillic, Arabic and
// accented Latin are billed at the dense ratio in both estimators (PR #196
// review P2-1). It only needs to BOUND a chunk, not count exactly, so the
// approximation is deliberate — and it keeps this package free of the cgo
// libtokenizers dependency.
func estimateTokens(s string, cjkRatio, asciiRatio int) int {
	if cjkRatio < 1 {
		cjkRatio = 1
	}
	if asciiRatio < 1 {
		asciiRatio = 1
	}
	cjk, other := classifyRunes(s)
	t := cjk/cjkRatio + other/asciiRatio
	if t < 1 && len(s) > 0 {
		t = 1
	}
	return t
}

// classifyRunes counts token-dense runes (r > 0x7F — the exact rule of
// internal/tokenizer/estimate.go, P2-1) versus sparse runes in s.
func classifyRunes(s string) (cjk, other int) {
	for _, r := range s {
		if r > 0x7F {
			cjk++
		} else {
			other++
		}
	}
	return cjk, other
}

// renderMessageLine renders one message exactly as formatChunkForLLM emits it.
// It is the single source of truth for the wire format: the formatter writes
// these lines to the prompt, and the token splitter bills these same lines,
// so the budget always charges what the model actually receives (PR #196
// review P0-1 — previously only m["content"] was billed, leaving the
// "[n] sender: " framing and newline free, a measured 3.85×–5.22× overrun on
// short-message traffic).
func renderMessageLine(m map[string]interface{}) string {
	sender, _ := m["sender_name"].(string)
	content, _ := m["content"].(string)
	citationIndex, _ := m["citation_index"].(int)
	return fmt.Sprintf("[%d] %s: %s\n", citationIndex, sender, content)
}

// chunkTokenBudget resolves the per-chunk token budget from config (the Map
// budget minus a system-prompt reserve).
//
// Cliff guard (round-3 review, yujiawei P2-4, review 4960791240): a resolved
// Map budget just above the reserve — e.g. MAP_MAX_TOKENS=801 → usable budget
// 1 token — splits one message per chunk, turning a 100k-message invocation
// into 100k sequential LLM calls. The worker path already guards exactly this
// (internal/worker/personal_processor.go: `if maxTokens < 10000 { ... using
// default 100000 }`); yujiawei: "That precedent makes the case for adding
// one." This guard mirrors it: below minSaneMapMaxTokens, fall back LOUDLY to
// the global default. NOTE: this supersedes the round-1 P2-2 expectation that
// a low-but-positive setting like MAP_MAX_TOKENS=1500 must be preserved (700
// usable) — round-3's cliff finding takes precedence, and the fallback is
// logged, not silent.
func chunkTokenBudget(cfg config.Config) int {
	mapMax := cfg.ResolveMapMaxTokens()
	if mapMax < minSaneMapMaxTokens {
		log.Printf("[config] resolved MapMaxTokens=%d below sane floor %d, using default %d (agent Map path)", mapMax, minSaneMapMaxTokens, fallbackMapMaxTokens)
		mapMax = fallbackMapMaxTokens
	}
	budget := mapMax - mapSystemPromptReserve
	if budget < 1 {
		budget = minChunkTokenBudget
	}
	return budget
}

// splitMsgMapsByTokenBudget groups msgMaps into chunks bounded by a token
// budget (SS-06b). Each message is billed as its RENDERED line
// (renderMessageLine), not its raw content, so the framing the model actually
// receives is what gets budgeted (PR #196 review P0-1). maxMsgs (>0)
// additionally caps the message count per chunk (the optional chunk_size
// hint); in every case the per-chunk message count is also capped by
// hardMessageBackstop, so zero-cost messages can never form an unbounded
// chunk (P0-2). A single message larger than the whole budget becomes its own
// chunk rather than being dropped or split mid-message. Never drops a message:
// every input ends up in exactly one chunk.
func splitMsgMapsByTokenBudget(msgMaps []map[string]interface{}, budget, maxMsgs, cjkRatio, asciiRatio int) [][]map[string]interface{} {
	if len(msgMaps) == 0 {
		return nil
	}
	if budget < 1 {
		budget = minChunkTokenBudget
	}
	if cjkRatio < 1 {
		cjkRatio = 1
	}
	if asciiRatio < 1 {
		asciiRatio = 1
	}
	msgCap := maxMsgs
	if msgCap <= 0 || msgCap > hardMessageBackstop {
		msgCap = hardMessageBackstop
	}
	var chunks [][]map[string]interface{}
	var cur []map[string]interface{}
	// Running rune totals over the whole chunk's RENDERED lines. Billed once
	// per chunk (not per message), so integer-division truncation happens at
	// most once per chunk instead of compounding across ~100k messages
	// (PR #196 review P2-4). This makes the budget check exact: the chunk's
	// estimate equals estimateTokens over the concatenated formatted output.
	curCJK, curOther := 0, 0
	for _, m := range msgMaps {
		line := renderMessageLine(m)
		lCJK, lOther := classifyRunes(line)
		atCap := len(cur) >= msgCap
		overBudget := len(cur) > 0 &&
			(curCJK+lCJK)/cjkRatio+(curOther+lOther)/asciiRatio > budget
		if len(cur) > 0 && (overBudget || atCap) {
			chunks = append(chunks, cur)
			cur = nil
			curCJK, curOther = 0, 0
		}
		cur = append(cur, m)
		curCJK += lCJK
		curOther += lOther
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}
