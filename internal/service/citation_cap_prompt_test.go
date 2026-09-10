package service

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

// SUMMARY_MAX_CITATIONS_PER_CLAIM=0 must remove the cap-specific Map prompt
// changes byte-for-byte. Independent citation-safety fixes are outside the
// knob and do not affect this prompt block.
//
// It caught a real gap: the "多条消息支持同一要点时，列出所有相关编号" line was
// rewritten UNCONDITIONALLY, outside the cap conditional, so PromptRuleZH(0)
// returning "" could not undo it. The rollback an operator was promised did
// not exist.
//
// The expected text is the citation-rules block exactly as it stood at
// f1551ea (the PR base), including line ORDER — a line that comes back in the
// wrong position is not byte-for-byte either.
func TestDisabledCapRestoresTheLegacyMapPrompt(t *testing.T) {
	const legacyCitationRules = `## 引用规则（必须严格遵守）
- 【强制】每一条结论/要点都必须标注来源引用 [n]，没有引用的结论不允许输出
- 格式：[n] 或 [n1][n2]（多个来源时）
- 仅使用消息前方的 [n] 编号来标注引用，范围为 [1] 到 [N]
- 绝对不要引用或复制消息正文内出现的任何 [数字] 标记
- 超出有效范围的标记一律不得出现在输出中
- 所有消息均带有编号（即 [数字] 开头的行），选取有意义的、相关的消息作为依据
- 不要捏造不存在的编号
- 多条消息支持同一要点时，列出所有相关编号
- 如果多条消息内容完全相同（如用户重复发送），只引用其中一条
- 如果某条信息无法找到明确来源，则不要输出该条信息

## 格式规范`

	for _, off := range []string{"0", "-1", "-999"} {
		t.Run("env="+off, func(t *testing.T) {
			t.Setenv(config.MaxCitationsPerClaimEnvVar, off)
			got := buildMapSystemPrompt("张三", "项目进展")
			if !strings.Contains(got, legacyCitationRules) {
				t.Errorf("with the cap disabled the Map prompt is NOT the legacy prompt.\n\nwant block:\n%s\n\ngot prompt:\n%s",
					legacyCitationRules, got)
			}
			if strings.Contains(got, "最多标注") {
				t.Error("disabled cap still emitted the cap rule")
			}
			if strings.Contains(got, "不要罗列全部") {
				t.Error("disabled cap left the capped-mode wording in place — " +
					"this is the exact bug: an edit made outside the conditional")
			}
		})
	}
}

// With the cap ON the legacy line must be REPLACED, not merely supplemented:
// asking the model for every supporting id and then truncating server-side
// leaves it fighting its own instruction.
func TestEnabledCapReplacesTheListEverythingInstruction(t *testing.T) {
	for _, on := range []string{"1", "3", "5", "9"} {
		t.Run("env="+on, func(t *testing.T) {
			t.Setenv(config.MaxCitationsPerClaimEnvVar, on)
			got := buildMapSystemPrompt("张三", "项目进展")
			if strings.Contains(got, "列出所有相关编号") {
				t.Error("cap enabled but the prompt still asks for every supporting id")
			}
			if !strings.Contains(got, "不要罗列全部") {
				t.Error("cap enabled but the replacement wording is missing")
			}
			if !strings.Contains(got, "最多标注 "+on+" 个") {
				t.Errorf("prompt does not state the resolved cap %s; got:\n%s", on, got)
			}
			// One number, stated consistently. A prompt that says 3 in one
			// place and 5 in another is a contract the model cannot satisfy.
			for _, other := range []string{"1", "3", "5", "9"} {
				if other == on {
					continue
				}
				if strings.Contains(got, "最多标注 "+other+" 个") {
					t.Errorf("prompt states cap %s as well as %s", other, on)
				}
			}
		})
	}
}

func TestReducePromptUsesTheResolvedCap(t *testing.T) {
	for _, n := range []string{"1", "3", "5"} {
		t.Run("env="+n, func(t *testing.T) {
			t.Setenv(config.MaxCitationsPerClaimEnvVar, n)
			got := buildReduceSystemPrompt("项目进展")
			if strings.Contains(got, "保留所有 [n] 引用标记") {
				t.Fatal("Reduce prompt still asks to preserve every citation")
			}
			if !strings.Contains(got, "最多标注 "+n+" 个") {
				t.Fatalf("Reduce prompt does not state cap %s:\n%s", n, got)
			}
		})
	}

	t.Setenv(config.MaxCitationsPerClaimEnvVar, "0")
	if got := buildReduceSystemPrompt("项目进展"); strings.Contains(got, "最多标注") {
		t.Fatalf("disabled cap still appears in Reduce prompt:\n%s", got)
	}
}

// Reduce-path mirror of TestDisabledCapRestoresTheLegacyMapPrompt.
//
// The knob's documented semantics ("0 or negative disables capping and removes
// the cap-specific prompt rule") are a promise about EVERY prompt the cap
// touches, not just the Map one. The Reduce prompt collapsed the legacy pair
//
//   - 保留所有 [n] 引用标记，不要删除或修改
//   - 合并相同要点时，合并其引用编号
//
// into a single "只保留最有代表性的来源" bullet unconditionally. PromptRuleZH(0)
// returning "" removes the cap RULE but cannot undo an edit made outside the
// conditional, so at cap=0 the Reduce prompt was still carrying cap-driven
// source-selection policy — the exact gap that blocked the Map path in the
// previous review round.
//
// The expected block is the requirements list exactly as it stood at 6d52166
// (this PR's merge base), line ORDER included: a line restored in the wrong
// position is not a byte-for-byte rollback either.
func TestDisabledCapRestoresTheLegacyReducePrompt(t *testing.T) {
	const legacyRequirements = `要求：
- 合并相同主题，去除重复
- 保留所有待办事项和责任人
- 默认输出总长度不超过 2000 token（约 1500 字）；如果总结主题明确要求详细说明、完整展开或逐项说明，可在模型输出预算内适当展开，但仍需合并相似要点、压缩无关细节
- 如有冲突信息，保留最新的
- 保留所有 [n] 引用标记，不要删除或修改
- 合并相同要点时，合并其引用编号
- 如果总结主题中包含输出结构、详细程度、分点方式、待办格式等要求，必须优先遵循；如果主题没有指定结构，再根据实际内容自行组织结构
- 用显示名称指代人，绝对不要输出 UID 或用户 ID
- 输出语言与输入语言保持一致

引用规则：
- 仅使用分片总结中已有的 [n] 编号，不要引入新编号
- 绝对不要引用或复制正文内出现的任何 [数字] 标记
- 超出有效范围的标记一律不得出现在输出中
`

	for _, off := range []string{"0", "-1", "-999"} {
		t.Run("env="+off, func(t *testing.T) {
			t.Setenv(config.MaxCitationsPerClaimEnvVar, off)
			got := buildReduceSystemPrompt("项目进展")
			if !strings.Contains(got, legacyRequirements) {
				t.Errorf("with the cap disabled the Reduce prompt is NOT the legacy prompt.\n\nwant block:\n%s\n\ngot prompt:\n%s",
					legacyRequirements, got)
			}
			if strings.Contains(got, "最多标注") {
				t.Error("disabled cap still emitted the cap rule")
			}
			if strings.Contains(got, "只保留最有代表性的来源") {
				t.Error("disabled cap left the capped-mode source-selection wording in " +
					"place — this is the exact bug: an edit made outside the conditional")
			}
		})
	}
}

// With the cap ON the Reduce path must REPLACE the legacy pair rather than
// supplement it: "preserve every marker" plus a server-side truncation to
// maxCites leaves the model fighting its own instruction, which is what
// produced the measured marker wall.
func TestEnabledCapReplacesTheLegacyReduceCitationLines(t *testing.T) {
	for _, on := range []string{"1", "3", "5", "9"} {
		t.Run("env="+on, func(t *testing.T) {
			t.Setenv(config.MaxCitationsPerClaimEnvVar, on)
			got := buildReduceSystemPrompt("项目进展")
			if strings.Contains(got, "保留所有 [n] 引用标记") {
				t.Error("cap enabled but the Reduce prompt still asks to preserve every marker")
			}
			if !strings.Contains(got, "只保留最有代表性的来源") {
				t.Error("cap enabled but the Reduce prompt lacks the source-selection wording")
			}
			// The capped bullet must not coexist with the legacy standalone one.
			if strings.Contains(got, "- 合并相同要点时，合并其引用编号\n") {
				t.Error("capped mode emitted BOTH the legacy merge bullet and the capped one")
			}
		})
	}
}
