package agent

import "strings"

// SS-08: deterministic 3-way refine routing (缺点十二 note + 原方案 §7 "Refine
// 三分流"). Today's summary_refine is entirely prompt-driven: the model guesses
// iterate/regenerate/Q&A on its own, and the prompt rigidly forbids reusing the
// old time range even when the user only wants to fill gaps in the SAME window.
//
// ClassifyRefine turns the user's refine instruction into an explicit route so
// the prompt receives a determined first-pass decision (injected as guidance,
// not a hard gate — the tools stay available, so a misclassification is
// recoverable). It is a pure function over the instruction text and is fully
// unit-tested; no LLM call.

// RefineIntent is the classified shape of a refine request.
type RefineIntent string

const (
	// RefineRewrite: pure formatting / translation / condensation / Q&A over the
	// existing summary. No new data needed — reuse the old citations verbatim.
	RefineRewrite RefineIntent = "rewrite"
	// RefineAugment: fill gaps / add detail / re-weight within the SAME scope and
	// time window. May reuse the old time range as the fetch window and re-run
	// Map/Reduce with the extra emphasis.
	RefineAugment RefineIntent = "augment"
	// RefineExtend: the user wants fresh / incremental data beyond the old window
	// ("最新 / 今天 / 增量 / 新进展"). Compute a NEW time window and re-fetch.
	RefineExtend RefineIntent = "extend"
)

// RefineRoute is the routing decision derived from the intent. The booleans are
// what the guidance block communicates to the model; SS-08b will additionally
// act on them in code (skip fetch, load a compatible artifact).
type RefineRoute struct {
	Intent RefineIntent
	// Fetch reports whether new messages should be fetched at all.
	Fetch bool
	// ReuseTimeRange reports whether the old summary's time window may be reused
	// as the fetch window (augment) versus computing a fresh one (extend).
	ReuseTimeRange bool
	// ReuseCitations reports whether the old summary's citations carry over
	// unchanged (rewrite path, where no new data is pulled).
	ReuseCitations bool
}

// Keyword sets are checked in priority order extend > augment > rewrite: a
// request that says "补充最新进展" carries both an augment word (补充) and a
// fresh-data word (最新) and must route to extend (it needs a new fetch window),
// so the fresh-data check runs first.
var (
	refineExtendKeywords = []string{
		"最新", "今天", "最近", "近期", "这几天", "这两天", "近几天",
		"增量", "新进展", "新消息", "有什么新", "有没有新", "更新一下", "更新下",
		"刷新", "昨天", "本周", "这周", "实时", "至今", "到现在", "近来", "如今",
	}
	refineAugmentKeywords = []string{
		"补全", "补充", "补上", "遗漏", "漏了", "漏掉", "没提到", "缺了",
		"更详细", "详细些", "详细点", "展开", "细化", "更全面", "全面些",
		"重新分析", "换个角度", "深入", "再挖", "更完整", "完整些",
	}
)

// ClassifyRefine classifies a refine instruction into an explicit route.
// Empty / unrecognized instructions fall back to the safe, cheapest path
// (rewrite: no fetch), because fetching on a mis-read is more costly and
// surprising than not fetching.
func ClassifyRefine(instruction string) RefineRoute {
	s := strings.ToLower(strings.TrimSpace(instruction))
	switch {
	case containsAny(s, refineExtendKeywords):
		return RefineRoute{Intent: RefineExtend, Fetch: true, ReuseTimeRange: false, ReuseCitations: false}
	case containsAny(s, refineAugmentKeywords):
		return RefineRoute{Intent: RefineAugment, Fetch: true, ReuseTimeRange: true, ReuseCitations: false}
	default:
		return RefineRoute{Intent: RefineRewrite, Fetch: false, ReuseTimeRange: false, ReuseCitations: true}
	}
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// BuildRefineGuidance renders a Chinese guidance block for the classified route,
// appended to the summary_refine system prompt (v2 only). It states the decided
// route explicitly so the model does not re-derive it from scratch, and — for
// the augment path — relaxes the prompt's blanket "绝不复用老 time_range" rule,
// which 缺点十二 flagged as wrong for same-scope gap-filling.
func BuildRefineGuidance(route RefineRoute) string {
	var b strings.Builder
	b.WriteString("\n\n---\n\n## 🧭 本次改写的既定路线（系统已按你的指令判定，优先遵循）\n\n")
	switch route.Intent {
	case RefineExtend:
		b.WriteString("**路线：增量 / 新窗口（extend）** —— 用户要更新的数据。\n")
		b.WriteString("- 用 `get_current_time` 拿当下，自行计算一个**新的时间窗**（绝不复用老 time_range 作为抓取窗）。\n")
		b.WriteString("- **必须**调 `fetch_channel` / `search_messages` 拉新消息，基于新数据重写。\n")
		b.WriteString("- citations 用新消息；如产物仍引用老结论可保留少量老 citations。\n")
	case RefineAugment:
		b.WriteString("**路线：同范围补全（augment）** —— 用户要在**同一时间窗内**补细节 / 补遗漏 / 更全面。\n")
		b.WriteString("- **可以复用老 time_range 作为抓取窗**（这是同范围补全，不是过期窗）——若需重抓同窗数据以补全，直接用老窗口。\n")
		b.WriteString("- 以老 content 为底，针对用户强调的方面加权重跑 Map/Reduce，补进遗漏要点。\n")
		b.WriteString("- 新增内容配新 citations；保留仍成立的老 citations。\n")
	default: // RefineRewrite
		b.WriteString("**路线：纯改写 / 问答（rewrite）** —— 用户要文字加工（精简 / 翻译 / 润色 / 排版）或直接问老总结说了什么。\n")
		b.WriteString("- **不需要**调抓取类工具；以老 content 为底做加工，或直接从老 content 提取答复。\n")
		b.WriteString("- **沿用老 citations**，不凭空新造；不改动引用编号。\n")
	}
	b.WriteString("\n（此为系统判定的默认路线；若用户指令与判定明显冲突，以用户明确要求为准。）")
	return b.String()
}
