package agent

import (
	"sort"
	"strings"
	"unicode"
)

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
	// The distinguishing shape is an ADD CONSTRUCTION, matched per clause (see
	// containsAddition): an add verb means "augment" only when, within its own
	// clause, it either (a) names a destination noun AFTER it (加进总结里 / 写进摘要
	// / 加到开头), or (b) is preceded by a 把/将 object marker (把客户反馈写进来 /
	// 把数据带上) — the "把 X <verb>" frame whose destination (the summary) is
	// implicit in the directional complement. 带上下文 carries neither, so it stays
	// a rewrite; 加入成员说了什么 has no 把 and no destination noun after 加入, so it
	// stays a rewrite too.
	refineAddVerbs = []string{"加上", "加进", "加入", "加到", "添加", "带上", "写进", "写入", "放进", "放入", "附上", "添上", "包含进", "纳入",
		// Round 6: the families reported as still hard-stripped. Note these tend to
		// take their destination BEFORE the verb (在摘要中增加…), which is why
		// containsAddition scans both sides of the verb rather than only after it.
		//
		// 覆盖 and 同步 were in this list for one round and are deliberately NOT:
		// 覆盖 means OVERWRITE (把重复内容覆盖掉) and 同步 means reconcile
		// (内容同步一下格式) at least as often as either means "add", so both routed
		// pure formatting edits to a fetch.
		"增加", "扩展", "扩充", "算上", "收录", "补进"}
	// refineSourceScopedAddVerbs are add verbs whose OTHER reading is a pure-text
	// operation, so they only count as an addition when their clause also names a
	// data source. 覆盖 means both "cover (include)" and "overwrite"; 同步 means both
	// "bring in" and "reconcile". Unconditionally they routed 摘要覆盖掉重复内容 and
	// 内容同步一下格式 to a fetch; conditionally, 这份摘要要覆盖运维群的内容 still
	// routes to augment. The object decides, not the verb.
	refineSourceScopedAddVerbs = []string{"覆盖", "同步"}
	// refineSourceNouns name places MATERIAL COMES FROM. Used for routing only —
	// never for the tool strip, which is residue-based (see rewriteResidueEmpty). A
	// routing mistake costs one extra fetch; that is the cheap direction.
	refineSourceNouns = []string{"群", "频道", "子区", "会话", "聊天记录", "对话记录"}
	// refineAddTargets are the things an add verb can name as its destination.
	refineAddTargets = []string{"总结", "摘要", "报告", "纪要", "段落", "开头", "结尾", "末尾", "正文", "内容", "里面", "里头", "文中", "小标题", "标题", "列表", "章节"}
	// refineAddObjectMarkers introduce the object of a "把 X <add-verb>" frame. A
	// marker only counts when it sits before the add verb IN THE SAME CLAUSE, so a
	// 把 from an unrelated earlier clause ("把这份总结精简一下，只保留最近的结论") does
	// not manufacture an addition.
	refineAddObjectMarkers = []string{"把", "将", "连"}
	// refineQuestionMarkers mark an INTERROGATIVE — a question ABOUT the artifact
	// rather than an instruction to change it. They gate only the
	// destination-before-verb rule (c) below, where a destination noun and an add
	// verb can co-occur inside a noun phrase without any addition being asked for:
	// "总结里新加入成员说了什么" is asking what the newly-JOINED members said, so
	// 加入 modifies 成员 and is not an imperative at all.
	refineQuestionMarkers = []string{"说了什么", "说了啥", "说了些什么", "什么意思", "讲了什么", "讲了啥"}
	// refineLocativeParticles turn a destination noun that PRECEDES the add verb
	// into an actual destination. "在摘要中增加X" / "报告里添上Y" name where the
	// material goes; "标题增加一个编号" names the thing being EDITED. Without this
	// requirement the before-verb rule swallowed ordinary formatting instructions
	// (给每个小标题加上序号, 正文排版扩展一下行距) and sent them to fetch.
	refineLocativeParticles = []string{"里", "中", "内", "里面", "里头", "当中", "之中"}
	// refineRewriteKeywords mark a CONFIDENT pure-text request (translate,
	// condense, polish, re-layout, or Q&A over the old summary). Matching one of
	// these — as opposed to falling through to the default — is what lets SS-08b
	// safely strip the fetch tools (HardNoFetch).
	refineRewriteKeywords = []string{
		"翻译", "精简", "简化", "缩短", "压缩", "删减",
		"润色", "排版", "格式", "改语气", "语气", "重新组织", "重组",
		"提炼", "换成", "改写成", "改写", "重写", "说了什么", "说了啥", "什么意思",
		"讲了什么", "只保留", "去掉", "删掉",
		// Anchored language phrasings. Round 1 removed bare 英文/中文 because they
		// collided with 精英文化 / 整理中文档, which lost the two commonest rewrite
		// requests (用英文重写, 改成中文) to the ambiguous fallback — so SS-08b's
		// zero-fetch enforcement stopped firing for them. Anchoring recovers both
		// without reopening the collision.
		"翻译成英文", "输出英文", "用英文", "改成英文", "英文版", "英文输出",
		"翻译成中文", "输出中文", "用中文", "改成中文", "中文版", "中文输出",
	}
	// refineResidueFillers are the semantically-empty words removed, along with the
	// matched rewrite keywords, before the HardNoFetch residue test (see
	// rewriteResidueEmpty). Every entry is a word that a request for NEW DATA never
	// consists of: politeness/quantity particles, grammatical glue, the artifact
	// nouns, the names of PARTS of an existing document, locatives, and Chinese
	// numerals/ordinals. Adding one can only ever turn HardNoFetch ON for a string
	// whose only remaining content was structural — it can never erase an addition
	// or a retrieval verb, because those are not in this list. It is consulted ONLY
	// inside the confident-rewrite branch (a rewrite keyword already matched), so it
	// cannot affect a bare artifact reference like "这份摘要", which never reaches it.
	refineResidueFillers = []string{
		// politeness / quantity
		"帮我", "麻烦", "请", "一下", "一点", "一些", "这份", "这段", "这个", "那段", "那个", "些", "点",
		// grammatical glue and coordinating conjunctions (an addition survives them)
		"的", "了", "吧", "呀", "啊", "把", "将", "也", "并且", "并", "再", "而且", "一并",
		"顺便", "顺带", "顺路", "另外", "另", "同时", "然后", "以及", "还有", "还要", "接着",
		"外加", "外带", "此外", "加之", "捎带", "兼", "随后", "跟着", "以后", "之后", "要",
		// "re-" modifiers
		"重新", "重",
		// artifact nouns (naming the thing being edited is not new data)
		"摘要", "总结", "报告", "纪要", "正文", "全文", "文档", "文章", "这篇", "本文",
		// parts of an existing document (subtractive edits name these, never new data)
		"段落", "段", "句子", "句", "行", "字词", "字", "词", "小标题", "标题", "开头", "结尾",
		"末尾", "部分", "结论", "章节", "列表", "序号", "编号", "第", "篇幅",
		// origin containers — WHERE material lives, structural like the artifact nouns.
		// An addition that names a container also names its content (a channel name, a
		// data kind), and that content noun is NOT a filler, so it survives the residue.
		"群消息", "群", "频道", "子区", "会话", "对话记录", "聊天记录", "对话", "消息",
		// pure-adjustment verbs that are not themselves rewrite keywords
		"调整", "调", "整理", "组织",
		// locatives
		"里面", "里头", "里", "中", "内", "文中", "当中", "之中",
		// Chinese numerals / ordinals
		"一", "二", "两", "三", "四", "五", "六", "七", "八", "九", "十", "零", "百", "千",
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
	case containsAny(s, refineExtendKeywords) && !(hasRewrite && !hasAddition && !containsAny(s, refineAugmentKeywords)):
		// extend outranks augment ("补充最新进展" needs a fresh window), but NOT a
		// pure rewrite that merely mentions time: "把这份总结精简一下，只保留最近的结论",
		// "更新一下格式" and "去掉最近的部分" all asked for zero new data and were
		// each told to fetch. A time word plus a rewrite verb and no addition is a
		// rewrite whose SUBJECT is the existing text; the time word is a filter over
		// what is already there, not a request for more.
		//
		// The augment-keyword term is what keeps that exception from swallowing the
		// rule it was carved out of: "格式调整，补充最新进展" carries a rewrite verb
		// AND an augment verb AND a time word, and without it the rewrite verb alone
		// demoted it to augment/ReuseTimeRange — i.e. answered an explicit request for
		// today's material out of yesterday's window.
		return RefineRoute{Intent: RefineExtend, Fetch: true, ReuseTimeRange: false, ReuseCitations: false}
	case containsAny(s, refineAugmentKeywords) || hasAddition:
		return RefineRoute{Intent: RefineAugment, Fetch: true, ReuseTimeRange: true, ReuseCitations: false}
	case hasRewrite:
		// Confident rewrite: an explicit pure-text keyword matched, so it MAY be safe
		// to strip the fetch tools (HardNoFetch) — 纯格式零 fetch.
		//
		// The strip fires only when NOTHING content-bearing is left after the rewrite
		// keywords are removed (rewriteResidueEmpty). A time word additionally vetoes
		// it, because then the request is only *probably* a rewrite.
		hardNoFetch := rewriteResidueEmpty(s) && !containsAny(s, refineExtendKeywords)
		return RefineRoute{Intent: RefineRewrite, Fetch: false, ReuseTimeRange: false, ReuseCitations: true, HardNoFetch: hardNoFetch}
	default:
		// Ambiguous fallback: still the rewrite path (cheapest, no fetch by
		// default) but NOT HardNoFetch — keep the tools available so a mis-read
		// can still recover by fetching.
		return RefineRoute{Intent: RefineRewrite, Fetch: false, ReuseTimeRange: false, ReuseCitations: true, HardNoFetch: false}
	}
}

// rewriteResidueEmpty reports whether, after deleting every matched rewrite
// keyword plus the closed set of structural fillers (refineResidueFillers) and
// all digits / punctuation / whitespace, NOTHING content-bearing remains.
//
// This replaces the ∀-over-clauses test for the HardNoFetch decision. HardNoFetch
// is the only irreversible decision in this PR — buildRunnerForProfile removes
// list/narrow/find/peek/fetch/search/filter outright, so a request for new data
// becomes silently impossible to satisfy — and rounds 4-9 proved that guessing it
// from vocabulary or clause boundaries does not converge: clause segmentation in
// unpunctuated Chinese is not a finite-list problem, so each round a new
// no-connective phrasing ("翻译成英文补一些运营数据") landed on the wrong side.
//
// The residue is a property of what the instruction asks for AFTER the recognised
// rewrite verbs are removed, not of any conjunction/boundary vocabulary. If
// anything content-bearing survives, that leftover is an obligation the rewrite
// verbs cannot satisfy (an addition, a retrieval, a channel/topic to go and get),
// so the tools MUST stay. An UNRECOGNISED addition can no longer hide next to a
// recognised rewrite, because it survives into the residue rather than being
// ignored by a vocabulary test. Every filler is structural (particles, artifact
// nouns, document parts, containers, numerals); no content noun (运营数据 / 客户反馈
// / 张三的原话 / a channel name) is a filler, so an addition's content always
// survives. The asymmetry is deliberate: a false negative here costs a few unused
// tool schemas in the prompt; a false positive costs an unsatisfiable request with
// no recovery, so leftover-means-keep is the safe default.
//
//	"翻译成英文"             -> residue "" -> strip
//	"翻译成英文补一些运营数据"  -> residue "补运营数据" -> keep tools
//
// Credit: proposed and hand-verified against the protection set by yujiawei in the
// round-9 review.
func rewriteResidueEmpty(s string) bool {
	residue := s
	// Rewrite keywords longest-first, so a compound like 翻译成英文 is removed whole
	// before its substring 翻译 could leave 成英文 behind.
	for _, kw := range refineRewriteKeywordsByLen {
		residue = strings.ReplaceAll(residue, kw, "")
	}
	for _, f := range refineResidueFillersByLen {
		residue = strings.ReplaceAll(residue, f, "")
	}
	// Content-bearing iff any rune survives that is not a digit, space, punctuation
	// or symbol.
	for _, r := range residue {
		if unicode.IsDigit(r) || unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		return false
	}
	return true
}

// refineRewriteKeywordsByLen is refineRewriteKeywords ordered by descending rune
// length so rewriteResidueEmpty removes the longest match first.
var refineRewriteKeywordsByLen = sortByRuneLenDesc(refineRewriteKeywords)

// refineResidueFillersByLen is refineResidueFillers longest-first, so a compound
// filler (摘要) is removed before a single-rune filler (要) can split it.
var refineResidueFillersByLen = sortByRuneLenDesc(refineResidueFillers)

func sortByRuneLenDesc(in []string) []string {
	out := append([]string(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return len([]rune(out[i])) > len([]rune(out[j]))
	})
	return out
}

// containsAddition reports whether s asks for content to be added INTO the
// summary. It matches the add CONSTRUCTION clause by clause, so an add verb in
// one clause is judged against only its own clause's object marker / destination.
//
// Two shapes count as an addition:
//   - an add verb followed, in the same clause, by a destination noun
//     ("把客户反馈写进总结" / "加进总结里" / "加到开头"); and
//   - an add verb preceded, in the same clause, by a 把/将 object marker
//     ("把客户反馈原文也写进来" / "把销售群的数据带上") — the destination is
//     implicit in the directional complement (进来/进去/上).
//
// Clause scoping is what separates "把这份总结精简一下，只保留最近的结论" (the 把 and
// the only add-verb-free clauses never meet) and "翻译成英文，带上下文" (带上 ⊂
// 带上下文, no 把, no destination) from a real addition. A bare verb list cannot
// make that distinction, which is why adding one reintroduced the collision class
// it was meant to close.
func containsAddition(s string) bool {
	for _, clause := range splitRefineClauses(s) {
		verbs := refineAddVerbs
		if containsAny(clause, refineSourceNouns) {
			// The clause names a data source, so the ambiguous verbs read as
			// additions here — see refineSourceScopedAddVerbs.
			verbs = append(append([]string{}, refineAddVerbs...), refineSourceScopedAddVerbs...)
		}
		for _, verb := range verbs {
			idx := strings.Index(clause, verb)
			if idx < 0 {
				continue
			}
			// (a) explicit destination noun after the verb.
			if containsAny(clause[idx+len(verb):], refineAddTargets) {
				return true
			}
			// (b) 把/将 object marker before the verb, same clause.
			if containsAny(clause[:idx], refineAddObjectMarkers) {
				return true
			}
			// (c) destination noun BEFORE the verb, same clause, followed by a
			// LOCATIVE particle. Chinese puts the destination first as often as last
			// (在摘要中增加X, 报告里添上Y) and the after-only rule missed every one.
			//
			// The particle is what separates a destination from a SUBJECT: "标题增加
			// 一个编号" and "正文排版扩展一下行距" name the thing being edited, not a
			// place material goes, and without this requirement they were routed to a
			// fetch — which then persisted fetch_expected=1 and produced a false
			// PARTIAL when the model correctly did not fetch.
			//
			// Not for interrogatives: in a question the two can co-occur inside a noun
			// phrase with nothing being added ("总结里新加入成员说了什么" — 加入
			// modifies 成员). Rules (a) and (b) carry an explicit imperative frame, so
			// they do not need the guard.
			if !containsAny(clause, refineQuestionMarkers) && hasLocativeDestination(clause[:idx]) {
				return true
			}
		}
	}
	return false
}

// hasLocativeDestination reports whether prefix ends a destination phrase: a
// target noun immediately followed by a locative particle (摘要中 / 报告里).
// A bare target noun is not enough — see rule (c) in containsAddition.
func hasLocativeDestination(prefix string) bool {
	for _, target := range refineAddTargets {
		for off := 0; ; {
			i := strings.Index(prefix[off:], target)
			if i < 0 {
				break
			}
			tail := prefix[off+i+len(target):]
			for _, p := range refineLocativeParticles {
				if strings.HasPrefix(tail, p) {
					return true
				}
			}
			off += i + len(target)
		}
	}
	return false
}

// splitRefineClauses splits an instruction on sentence/clause punctuation so the
// add-construction match stays local to one clause.
//
// Whitespace is NOT a boundary. Chinese does not delimit clauses with spaces, so
// treating ' ' and '\t' as separators shattered "翻译一下 并 把销售数据 加进 报告"
// into fragments, which lost the 把…加进 construction and hard-stripped the tools —
// a regression the clause-scoping change introduced. Spaces are normalised away
// before matching instead, so they can neither split a construction nor glue two
// real clauses together.
func splitRefineClauses(s string) []string {
	s = strings.NewReplacer(" ", "", "\t", "", "\u3000", "").Replace(s)
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case '，', ',', '。', '.', '；', ';', '、', '！', '!', '？', '?', '\n', '\r', '：', ':':
			return true
		}
		return false
	})
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
