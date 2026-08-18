package agent

import (
	"unicode"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

// SS-06b: token-aware chunking replaces the fixed message-count split so Map
// chunks are balanced by content (defect #9: uneven-length messages caused wild
// token imbalance) and the 200-message format cap can be removed without risking
// context overflow — each chunk is now bounded by a token budget instead.
const (
	// mapSystemPromptReserve is subtracted from the Map token budget to leave
	// room for the fixed summarize system prompt.
	mapSystemPromptReserve = 800
	// minChunkTokenBudget floors the per-chunk budget so a mis-set config can't
	// produce single-message chunks.
	minChunkTokenBudget = 2000
)

// estimateTokens is a pure, cgo-free token estimate matching the tokenizer's
// fallback mode: CJK runes are token-dense (~cjkRatio chars/token) while other
// text is sparser (~asciiRatio chars/token). It only needs to BOUND a chunk, not
// count exactly, so the approximation is deliberate — and it keeps this package
// free of the cgo libtokenizers dependency.
func estimateTokens(s string, cjkRatio, asciiRatio int) int {
	if cjkRatio < 1 {
		cjkRatio = 1
	}
	if asciiRatio < 1 {
		asciiRatio = 1
	}
	var cjk, other int
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) ||
			(r >= 0x3000 && r <= 0x303f) {
			cjk++
		} else {
			other++
		}
	}
	t := cjk/cjkRatio + other/asciiRatio
	if t < 1 && len(s) > 0 {
		t = 1
	}
	return t
}

// chunkTokenBudget resolves the per-chunk token budget from config (the Map
// budget minus a system-prompt reserve), floored so it is always usable.
func chunkTokenBudget(cfg config.Config) int {
	budget := cfg.ResolveMapMaxTokens() - mapSystemPromptReserve
	if budget < minChunkTokenBudget {
		budget = minChunkTokenBudget
	}
	return budget
}

// splitMsgMapsByTokenBudget groups msgMaps into chunks bounded by a token budget
// (SS-06b). maxMsgs (>0) additionally caps the message count per chunk (the
// optional chunk_size hint); 0 means token-only. A single message larger than
// the whole budget becomes its own chunk rather than being dropped or splitting
// mid-message. Never drops a message: every input ends up in exactly one chunk.
func splitMsgMapsByTokenBudget(msgMaps []map[string]interface{}, budget, maxMsgs, cjkRatio, asciiRatio int) [][]map[string]interface{} {
	if len(msgMaps) == 0 {
		return nil
	}
	if budget < 1 {
		budget = minChunkTokenBudget
	}
	var chunks [][]map[string]interface{}
	var cur []map[string]interface{}
	curTok := 0
	for _, m := range msgMaps {
		content, _ := m["content"].(string)
		t := estimateTokens(content, cjkRatio, asciiRatio)
		atCap := maxMsgs > 0 && len(cur) >= maxMsgs
		if len(cur) > 0 && (curTok+t > budget || atCap) {
			chunks = append(chunks, cur)
			cur = nil
			curTok = 0
		}
		cur = append(cur, m)
		curTok += t
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}
