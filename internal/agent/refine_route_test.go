package agent

import (
	"strings"
	"testing"
)

// TestClassifyRefine covers SS-08's 3-way refine routing (缺点十二).
func TestClassifyRefine(t *testing.T) {
	cases := []struct {
		name           string
		instruction    string
		wantIntent     RefineIntent
		wantFetch      bool
		wantReuseRange bool
		wantReuseCite  bool
	}{
		// rewrite: pure text work / Q&A → no fetch, reuse citations
		{"translate", "帮我把这份总结翻译成英文", RefineRewrite, false, false, true},
		{"condense", "精简一下，太长了", RefineRewrite, false, false, true},
		{"polish", "润色排版一下", RefineRewrite, false, false, true},
		{"qa", "这份总结里到底说了什么结论", RefineRewrite, false, false, true},
		{"empty→safe rewrite", "", RefineRewrite, false, false, true},
		{"unknown→safe rewrite", "随便改改", RefineRewrite, false, false, true},

		// augment: same scope, fill gaps → fetch, reuse old range
		{"fill gaps", "补全遗漏的要点", RefineAugment, true, true, false},
		{"more detail", "把第二部分展开更详细一些", RefineAugment, true, true, false},
		{"more complete", "写得更全面些", RefineAugment, true, true, false},

		// extend: fresh/incremental → fetch, NEW window
		{"latest", "补充最新进展", RefineExtend, true, false, false}, // 补充(augment)+最新(extend) → extend wins
		{"today", "今天有什么新消息", RefineExtend, true, false, false},
		{"incremental", "做个增量更新", RefineExtend, true, false, false},
		{"recent week", "最近一周的情况", RefineExtend, true, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRefine(tc.instruction)
			if got.Intent != tc.wantIntent || got.Fetch != tc.wantFetch ||
				got.ReuseTimeRange != tc.wantReuseRange || got.ReuseCitations != tc.wantReuseCite {
				t.Errorf("ClassifyRefine(%q) = %+v, want intent=%s fetch=%t reuseRange=%t reuseCite=%t",
					tc.instruction, got, tc.wantIntent, tc.wantFetch, tc.wantReuseRange, tc.wantReuseCite)
			}
		})
	}
}

// TestExtendBeatsAugment locks the priority: a request carrying BOTH an augment
// word and a fresh-data word must route to extend (it needs a new fetch window).
func TestExtendBeatsAugment(t *testing.T) {
	got := ClassifyRefine("补充一下最新的进展") // 补充 = augment, 最新 = extend
	if got.Intent != RefineExtend {
		t.Fatalf("expected extend to win over augment, got %s", got.Intent)
	}
}

// TestHardNoFetch verifies SS-08b: only a CONFIDENT rewrite (explicit keyword)
// is safe to strip fetch tools; the ambiguous fallback and the fetch paths are
// never HardNoFetch.
func TestHardNoFetch(t *testing.T) {
	cases := []struct {
		instruction string
		wantIntent  RefineIntent
		wantHard    bool
	}{
		{"翻译成英文", RefineRewrite, true},   // explicit rewrite keyword
		{"精简一下", RefineRewrite, true},    // explicit
		{"总结里说了什么", RefineRewrite, true}, // explicit Q&A
		{"把下午客户会议的讨论加进总结里", RefineAugment, false},
		{"整理中文档里提到的风险", RefineRewrite, false},
		{"讨论精英文化的那部分", RefineRewrite, false},
		{"帮我翻译成英文", RefineRewrite, true},
		{"随便改改", RefineRewrite, false}, // ambiguous fallback → keep tools
		{"", RefineRewrite, false},     // empty fallback → keep tools
		{"补全遗漏", RefineAugment, false}, // augment must keep fetch
		{"最新进展", RefineExtend, false},  // extend must keep fetch
	}
	for _, tc := range cases {
		got := ClassifyRefine(tc.instruction)
		if got.Intent != tc.wantIntent || got.HardNoFetch != tc.wantHard {
			t.Errorf("ClassifyRefine(%q) intent=%s hardNoFetch=%t, want intent=%s hardNoFetch=%t",
				tc.instruction, got.Intent, got.HardNoFetch, tc.wantIntent, tc.wantHard)
		}
		// Safety invariant: HardNoFetch implies no fetch.
		if got.HardNoFetch && got.Fetch {
			t.Errorf("ClassifyRefine(%q): HardNoFetch must imply !Fetch", tc.instruction)
		}
	}
}

// TestBuildRefineGuidance checks each route yields a non-empty block that names
// its route and respects the reuse-time-range rule.
func TestBuildRefineGuidance(t *testing.T) {
	rewrite := BuildRefineGuidance(ClassifyRefine("翻译成英文"))
	if !strings.Contains(rewrite, "rewrite") || !strings.Contains(rewrite, "沿用老 citations") {
		t.Errorf("rewrite guidance missing markers: %s", rewrite)
	}
	augment := BuildRefineGuidance(ClassifyRefine("补全遗漏"))
	if !strings.Contains(augment, "augment") || !strings.Contains(augment, "可以复用老 time_range") {
		t.Errorf("augment guidance should relax the time-range rule: %s", augment)
	}
	extend := BuildRefineGuidance(ClassifyRefine("最新进展"))
	if !strings.Contains(extend, "extend") || !strings.Contains(extend, "绝不复用老 time_range") {
		t.Errorf("extend guidance should forbid reusing the old range: %s", extend)
	}
}

// TestRefineAddVerbsRequireADestination pins the round-2 collision. Adding the
// bare add-verbs 加上/加进/加入/添加/带上 to the augment list fixed the reported
// repros and immediately reintroduced the CJK substring class the round-1 fix had
// removed: 带上 ⊂ 带上下文, so a pure typography request routed to augment and was
// told to re-fetch and re-run Map/Reduce.
//
// An add verb now has to name a DESTINATION, which is the actual distinguishing
// shape ("write X INTO the summary" vs "带上下文"). That rule also covers the
// verbs flagged as an uncovered tail (写进/放进/附上) without another guessing
// round, because what is matched is the construction rather than the verb.
func TestRefineAddVerbsRequireADestination(t *testing.T) {
	cases := []struct {
		instruction string
		wantIntent  RefineIntent
		why         string
	}{
		// Real additions: a destination is named, so new content must be gathered.
		{"把下午客户会议的讨论加进总结里", RefineAugment, "round-1 repro"},
		{"把客户反馈原文也写进总结", RefineAugment, "写进 + 总结"},
		{"把摘要加入到开头", RefineAugment, "加入 + 开头"},
		{"精简后把客户反馈原文也写进总结", RefineAugment, "rewrite verb present, but it IS an addition"},
		{"给每段加上小标题", RefineAugment, "加上 + 小标题"},

		// Not additions: the verb is part of another word, or nothing is being
		// added to anything.
		{"翻译成英文，带上下文", RefineRewrite, "带上 ⊂ 带上下文 — the round-2 collision"},
		{"总结里新加入成员说了什么", RefineRewrite, "加入成员 is the subject, not a destination"},
	}
	for _, tc := range cases {
		got := ClassifyRefine(tc.instruction)
		if got.Intent != tc.wantIntent {
			t.Errorf("ClassifyRefine(%q) intent=%s, want %s (%s)", tc.instruction, got.Intent, tc.wantIntent, tc.why)
		}
	}
}

// TestRefineImplicitDestinationAddIsAugment pins the round-4 P1-2 (yujiawei):
// seven of nine "把 X 写进来/放进去/加上/带上/附上" phrasings routed to a
// hardNoFetch rewrite because the positional rule required an explicit
// destination noun AFTER the verb, but these express the destination implicitly
// with a directional complement while marking the object with 把. The classifier
// must route them so the fetch tools are NOT stripped (Fetch=true for augment, or
// a fetching extend when a time word is present), because each explicitly asks to
// pull in content not in the current summary.
func TestRefineImplicitDestinationAddIsAugment(t *testing.T) {
	augment := []string{
		"精简一下，把客户反馈原文也写进来",
		"总结精简些，把测试群的结论放进去",
		"压缩一下，把销售群的数据写进去",
		"格式改一下，把运维群的结论添上",
		"润色一下，把架构群的决议也加上",
		"去掉废话，把客户群的反馈加进来",
		"改成中文，把销售群的数据带上",
	}
	for _, ins := range augment {
		got := ClassifyRefine(ins)
		if got.Intent != RefineAugment {
			t.Errorf("ClassifyRefine(%q) intent=%s, want augment", ins, got.Intent)
		}
		if !got.Fetch || got.HardNoFetch {
			t.Errorf("ClassifyRefine(%q) fetch=%t hardNoFetch=%t, want a fetching route with tools available",
				ins, got.Fetch, got.HardNoFetch)
		}
	}

	// "重新排版，把昨天的会议纪要附上" carries a time word (昨天): it needs yesterday's
	// data, so a FETCHING extend is correct — the point is only that it must not be
	// a zero-fetch rewrite.
	if got := ClassifyRefine("重新排版，把昨天的会议纪要附上"); !got.Fetch || got.HardNoFetch {
		t.Errorf("time-worded add = %+v, want a fetching route (not a stripped rewrite)", got)
	}

	// The construction that the positional rule was written to protect must NOT
	// regress: no 把, no destination noun after 带上 ⇒ still a confident rewrite.
	if got := ClassifyRefine("翻译成英文，带上下文"); got.Intent != RefineRewrite || !got.HardNoFetch {
		t.Errorf("ClassifyRefine(翻译成英文，带上下文) = %+v, want a hardNoFetch rewrite", got)
	}
}

// TestRefineTimeWordDoesNotForceAFetch pins the priority carve-out. extend
// outranking rewrite meant any rewrite mentioning time was told to fetch new
// messages for a request needing zero new data.
func TestRefineTimeWordDoesNotForceAFetch(t *testing.T) {
	for _, instruction := range []string{
		"把这份总结精简一下，只保留最近的结论",
		"更新一下格式",
		"去掉最近的部分",
	} {
		got := ClassifyRefine(instruction)
		if got.Intent != RefineRewrite || got.Fetch {
			t.Errorf("ClassifyRefine(%q) intent=%s fetch=%t, want a no-fetch rewrite", instruction, got.Intent, got.Fetch)
		}
		// Tools stay available: a time word makes this only PROBABLY a rewrite, so
		// a mis-read must stay recoverable rather than be enforced in code.
		if got.HardNoFetch {
			t.Errorf("ClassifyRefine(%q): a time word present ⇒ keep the tools available", instruction)
		}
	}

	// The genuine extend case must still fetch.
	if got := ClassifyRefine("补充最新进展"); got.Intent != RefineExtend || !got.Fetch {
		t.Errorf("ClassifyRefine(补充最新进展) = %+v, want a fetching extend", got)
	}
}

// TestRefineAnchoredLanguagePhrasings recovers the two commonest rewrite requests
// that round 1 lost. Dropping the bare 英文/中文 keywords fixed 精英文化 / 中文档 but
// sent 用英文重写 and 改成中文 to the ambiguous fallback, so SS-08b's zero-fetch
// enforcement stopped firing for them. Anchored phrasings restore both without
// reopening the collision — the two are asserted together so a future edit cannot
// trade one for the other again.
func TestRefineAnchoredLanguagePhrasings(t *testing.T) {
	for _, instruction := range []string{"用英文重写", "改成中文", "翻译成英文", "输出中文"} {
		if got := ClassifyRefine(instruction); !got.HardNoFetch {
			t.Errorf("ClassifyRefine(%q) should be a confident rewrite, got %+v", instruction, got)
		}
	}
	for _, instruction := range []string{"讨论精英文化的那部分", "整理中文档里提到的风险"} {
		if got := ClassifyRefine(instruction); got.HardNoFetch {
			t.Errorf("ClassifyRefine(%q) is prose containing the keyword, must not strip tools", instruction)
		}
	}
}

// TestRefineNeverStripsToolsWhenDataIsAsked pins the round-4/5/6 blocker class.
//
// HardNoFetch is the one decision in this PR a runtime mistake cannot undo:
// buildRunnerForProfile removes list/narrow/find/peek/fetch/search/filter
// outright, so a request for new data becomes silently impossible to satisfy.
// Three consecutive rounds found a NEW family of phrasings being hard-stripped
// while the previously pinned families stayed green — the signature of a keyword
// list at its limit, not of a list one entry short.
//
// So the strip is now gated on a high-precision veto (refineDataSignals) rather
// than on the routing decision alone. The assertion here is deliberately weak on
// INTENT and strict on ENFORCEMENT: whichever way these route, the model must
// keep the ability to go and get the data.
func TestRefineNeverStripsToolsWhenDataIsAsked(t *testing.T) {
	for _, instruction := range []string{
		// Round 5/6 blocker set (Jerry-Xin), verbatim.
		"在摘要中增加客户反馈",
		"摘要里再增加一下产品群的讨论",
		"这份摘要要覆盖运维群的内容",
		"把摘要扩展到包括销售群",
		"换成按项目分组，并把测试群也算上",
		"只保留 P0 问题，另外看看研发群还有没有别的",
		// Round 6 additions (yujiawei), verbatim.
		"精简一下，并同步销售群的新动态",
		"把总结翻译成英文，顺便看看研发群后续进展",
		"用中文重写，并查一下客服群刚才说了什么",
		"翻译一下 并 把销售数据 加进 报告",
		// Round 6 family stress-test (Jerry-Xin), not from any earlier CR.
		"把运维群也纳入总结里",
		"加上客户群的讨论",
		"补上运维数据",
		"收录销售反馈",
		"带上测试群的消息",
		"总结里再加一段客户评价",
		"报告里添上运维群的问题",
		"内容再扩充一下销售群的反馈",
		"帮我汇总一下测试群的意见放进摘要",
		"摘要再补充一些客户反馈",
	} {
		if got := ClassifyRefine(instruction); got.HardNoFetch {
			t.Errorf("ClassifyRefine(%q) strips the fetch tools, so the request is impossible to satisfy: %+v", instruction, got)
		}
	}
}

// TestRefineStillStripsToolsForPureTextRequests is the counterpart. The veto must
// stay high-precision in the other direction too, or SS-08b's zero-fetch
// enforcement stops firing for the requests it exists for.
func TestRefineStillStripsToolsForPureTextRequests(t *testing.T) {
	for _, instruction := range []string{
		"精简一下",
		"翻译成英文",
		"用英文重写",
		"改成中文",
		"润色一下",
		"重新排版",
		"总结里说了什么",
		"翻译成英文，带上下文", // 带上 ⊂ 带上下文: no addition, no data signal
	} {
		if got := ClassifyRefine(instruction); !got.HardNoFetch {
			t.Errorf("ClassifyRefine(%q) is a pure-text request and should enforce zero fetch: %+v", instruction, got)
		}
	}
}

// TestRefineWhitespaceIsNotAClauseBoundary pins the regression the clause-scoping
// change introduced: Chinese does not delimit clauses with spaces, so treating
// ' ' as a separator shattered the 把…加进 construction into fragments and
// hard-stripped the tools. Spaces are normalised away before matching instead.
func TestRefineWhitespaceIsNotAClauseBoundary(t *testing.T) {
	spaced := ClassifyRefine("翻译一下 并 把销售数据 加进 报告")
	tight := ClassifyRefine("翻译一下并把销售数据加进报告")
	if spaced.Intent != tight.Intent || spaced.HardNoFetch != tight.HardNoFetch {
		t.Errorf("spacing changed the classification: spaced=%+v tight=%+v", spaced, tight)
	}
	if spaced.Intent != RefineAugment {
		t.Errorf("把X加进Y is an addition regardless of spacing, got %+v", spaced)
	}

	// And a real clause boundary must still scope the match: the 把 here belongs
	// to a different clause than any add verb.
	if got := ClassifyRefine("把这份总结精简一下，只保留最近的结论"); got.Intent != RefineRewrite {
		t.Errorf("a 把 in an unrelated clause must not manufacture an addition: %+v", got)
	}
}

// TestRefineDestinationBeforeVerbIsAnAddition covers the other half of the
// positional fix. Chinese puts the destination before the verb as often as after
// it (在摘要中增加X / 这份摘要要覆盖Y), and the after-only rule missed every one.
func TestRefineDestinationBeforeVerbIsAnAddition(t *testing.T) {
	for _, instruction := range []string{
		"在摘要中增加客户反馈",
		"这份摘要要覆盖运维群的内容",
		"报告里添上运维群的问题",
	} {
		if got := ClassifyRefine(instruction); got.Intent != RefineAugment {
			t.Errorf("ClassifyRefine(%q) intent=%s, want augment", instruction, got.Intent)
		}
	}

	// But not in a QUESTION: there the destination noun and the add verb can
	// co-occur inside a noun phrase with nothing being added — 加入 modifies 成员.
	if got := ClassifyRefine("总结里新加入成员说了什么"); got.Intent != RefineRewrite {
		t.Errorf("an interrogative is not an addition instruction: %+v", got)
	}
}

// TestRefineArtifactNounDoesNotAuthorizeTheStrip pins the removal of 摘要 from
// refineRewriteKeywords.
//
// 摘要 is the artifact NOUN, not an operation verb — it is simultaneously listed
// in refineAddTargets, which is its correct role. While it sat in the rewrite set,
// ANY instruction that merely named the thing being edited landed in the
// confident-rewrite branch and had its fetch tools removed, which is why every
// "在摘要中增加…" phrasing was unsatisfiable. Naming the artifact says nothing
// about the operation, so it must fall to the recoverable path.
//
// Pinned separately because the other round-6 fixes (the data-signal veto and the
// destination-before-verb rule) independently rescue the reported phrasings: put
// 摘要 back and the reported set still passes, so without this test the removal
// would be silently revertible.
func TestRefineArtifactNounDoesNotAuthorizeTheStrip(t *testing.T) {
	for _, instruction := range []string{
		"这份摘要",
		"摘要重新整理一下",
		"摘要里的第三点",
	} {
		if got := ClassifyRefine(instruction); got.HardNoFetch {
			t.Errorf("ClassifyRefine(%q): naming the artifact is not an operation and must not strip the tools: %+v", instruction, got)
		}
	}

	// A real operation verb in the same sentence still authorizes the strip.
	if got := ClassifyRefine("把摘要精简一下"); !got.HardNoFetch {
		t.Errorf("摘要 + a genuine rewrite verb should still enforce zero fetch: %+v", got)
	}
}
