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
	// HardNoFetch reports that this is a CONFIDENT rewrite (an explicit rewrite
	// keyword matched, not the ambiguous fallback), so the caller may safely
	// build the runner without any data-fetching tools — enforcing "纯格式零
	// fetch" in code rather than only advising it in the prompt (SS-08b). The
	// ambiguous fallback keeps Fetch=false but HardNoFetch=false, so its tools
	// stay available and a mis-read is recoverable.
	HardNoFetch bool
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
	// refineAddVerbs are the "put X into the summary" verbs. They are matched
	// STRUCTURALLY rather than as bare substrings: round 2 added 带上/加入/添加 to
	// the list above and immediately reintroduced the CJK collision the round-1 fix
	// had removed — 带上 ⊂ 带上下文, so "翻译成英文，带上下文" (a pure typography
	// request) routed to augment and was told to re-fetch and re-run Map/Reduce.
	//
	// The distinguishing shape is a DESTINATION: an add verb only means "augment"
	// when something is being added TO something. 带上下文 has no destination;
	// 加进总结里 / 写进摘要 / 加到开头 do. This also covers the verbs the review
	// listed as an uncovered tail (写进/放进/附上) without another round of guessing,
	// because the rule is the construction, not the verb.
	refineAddVerbs = []string{"加上", "加进", "加入", "加到", "添加", "带上", "写进", "写入", "放进", "放入", "附上", "添上", "包含进", "纳入"}
	// refineAddTargets are the things an add verb can name as its destination.
	refineAddTargets = []string{"总结", "摘要", "报告", "纪要", "段落", "开头", "结尾", "末尾", "正文", "内容", "里面", "里头", "文中", "小标题", "标题", "列表", "章节"}
	// refineRewriteKeywords mark a CONFIDENT pure-text request (translate,
	// condense, polish, re-layout, or Q&A over the old summary). Matching one of
	// these — as opposed to falling through to the default — is what lets SS-08b
	// safely strip the fetch tools (HardNoFetch).
	refineRewriteKeywords = []string{
		"翻译", "精简", "简化", "缩短", "压缩", "删减",
		"润色", "排版", "格式", "改语气", "语气", "重新组织", "重组",
		"摘要", "提炼", "换成", "改写成", "说了什么", "说了啥", "什么意思",
		"讲了什么", "只保留", "去掉", "删掉",
		// Anchored language phrasings. Round 1 removed bare 英文/中文 because they
		// collided with 精英文化 / 整理中文档, which lost the two commonest rewrite
		// requests (用英文重写, 改成中文) to the ambiguous fallback — so SS-08b's
		// zero-fetch enforcement stopped firing for them. Anchoring recovers both
		// without reopening the collision.
		"翻译成英文", "输出英文", "用英文", "改成英文", "英文版", "英文输出",
		"翻译成中文", "输出中文", "用中文", "改成中文", "中文版", "中文输出",
	}
)

// ClassifyRefine classifies a refine instruction into an explicit route.
// Empty / unrecognized instructions fall back to the safe, cheapest path
// (rewrite: no fetch), because fetching on a mis-read is more costly and
// surprising than not fetching.
func ClassifyRefine(instruction string) RefineRoute {
	s := strings.ToLower(strings.TrimSpace(instruction))
	hasAddition := containsAddition(s)
	hasRewrite := containsAny(s, refineRewriteKeywords)
	switch {
	case containsAny(s, refineExtendKeywords) && !(hasRewrite && !hasAddition):
		// extend outranks augment ("补充最新进展" needs a fresh window), but NOT a
		// pure rewrite that merely mentions time: "把这份总结精简一下，只保留最近的结论",
		// "更新一下格式" and "去掉最近的部分" all asked for zero new data and were
		// each told to fetch. A time word plus a rewrite verb and no addition is a
		// rewrite whose SUBJECT is the existing text; the time word is a filter over
		// what is already there, not a request for more.
		return RefineRoute{Intent: RefineExtend, Fetch: true, ReuseTimeRange: false, ReuseCitations: false}
	case containsAny(s, refineAugmentKeywords) || hasAddition:
		return RefineRoute{Intent: RefineAugment, Fetch: true, ReuseTimeRange: true, ReuseCitations: false}
	case hasRewrite:
		// Confident rewrite: an explicit pure-text keyword matched, so it is safe
		// to strip the fetch tools (HardNoFetch) — 纯格式零 fetch.
		//
		// One exception: when a time word IS present the request is only *probably*
		// a rewrite, so the tools stay available. The route is still the no-fetch
		// path, but a mis-read stays recoverable instead of being enforced in code.
		hardNoFetch := !containsAny(s, refineExtendKeywords)
		return RefineRoute{Intent: RefineRewrite, Fetch: false, ReuseTimeRange: false, ReuseCitations: true, HardNoFetch: hardNoFetch}
	default:
		// Ambiguous fallback: still the rewrite path (cheapest, no fetch by
		// default) but NOT HardNoFetch — keep the tools available so a mis-read
		// can still recover by fetching.
		return RefineRoute{Intent: RefineRewrite, Fetch: false, ReuseTimeRange: false, ReuseCitations: true, HardNoFetch: false}
	}
}

// containsAddition reports whether s asks for content to be added INTO the
// summary, i.e. an add verb that names a destination.
//
// Requiring the destination is what separates "把客户反馈写进总结" (augment: new
// content must be gathered) from "翻译成英文，带上下文" (a rewrite whose 带上 is
// just the head of 带上下文). A bare verb list cannot make that distinction, which
// is why adding one reintroduced the collision class it was meant to close.
func containsAddition(s string) bool {
	for _, verb := range refineAddVerbs {
		idx := strings.Index(s, verb)
		if idx < 0 {
			continue
		}
		rest := s[idx+len(verb):]
		for _, target := range refineAddTargets {
			if strings.Contains(rest, target) {
				return true
			}
		}
	}
	return false
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
