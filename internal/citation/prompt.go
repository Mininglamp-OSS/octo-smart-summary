package citation

import "fmt"

// PromptRuleZH renders the per-claim citation-cap rule appended to the Map
// prompts (agent internal/agent/tool_summarize_chunk.go, worker
// internal/service/llm.go), or "" when capping is disabled.
//
// It lives here, next to the enforcement, because the prompt and the
// post-processor must state the same number. A prompt that asks for at most 3
// while the code truncates at 5 (or vice versa) is a contract the model
// cannot satisfy and a reviewer cannot verify; keeping the sentence and the
// cap in one package makes the drift impossible.
//
// Returning "" for Disabled is what keeps the kill switch honest: with
// SUMMARY_MAX_CITATIONS_PER_CLAIM=0 the prompt is byte-identical to the
// pre-change prompt AND no truncation runs, so the whole feature reverts
// without a rebuild.
//
// The leading newline is part of the value so callers can concatenate onto a
// bullet list without deciding whether they need a separator.
func PromptRuleZH(max int) string {
	if max < 1 {
		return ""
	}
	return fmt.Sprintf(
		"\n- 每一条结论/要点最多标注 %d 个 [n]；支持来源更多时，只选最有代表性、最新的 %d 条"+
			"\n- 不要输出 [3][7][12][15]… 这样的长串引用，也不要在同一条结论里重复同一个编号",
		max, max)
}

// PlannerPromptRuleZH renders the per-claim cap rule for the AGENT PLANNER
// prompt (internal/agent/prompts/summary.md, via the {{CITATION_CAP_RULE}}
// placeholder), or "" when capping is disabled.
//
// Separate from PromptRuleZH because the planner's rule is a markdown bullet
// list inside an existing "## 引用规则" section and uses that file's
// punctuation style, whereas PromptRuleZH is appended to the Map system
// prompts. Same resolved number, same source of truth — the number is a
// parameter in both, never a literal in a file.
//
// The truncation claim is stated only here, and it is now TRUE for this path:
// the agent's final answer is capped in internal/agent/summary_answer.go. It
// was previously hardcoded into summary.md while no call site capped the
// planner's body at all.
//
// Returns "" (including the trailing newline) for Disabled, so the placeholder
// line disappears entirely and the kill switch leaves the planner prompt with
// no cap instruction — matching the enforcement, which also does nothing.
func PlannerPromptRuleZH(max int) string {
	if max < 1 {
		return ""
	}
	return fmt.Sprintf(
		"- 每一条结论/要点最多标注 %d 个 `[n]`；支持来源更多时,只选最有代表性、最新的 %d 条\n"+
			"- 不要输出 `[3][7][12][15]…` 这样的长串引用,也不要在同一条结论里重复同一个编号(超出上限的标记会在服务端被截断)\n",
		max, max)
}
