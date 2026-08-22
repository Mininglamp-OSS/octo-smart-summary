package service

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

// CONFIGURATION.md tells operators that SUMMARY_MAX_CITATIONS_PER_CLAIM=0
// "restores the previous behavior byte-for-byte, prompt included". This test
// is that guarantee.
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
				t.Errorf("with the cap disabled the Map prompt is NOT the legacy prompt.\n"+
					"CONFIGURATION.md promises byte-for-byte rollback.\n\nwant block:\n%s\n\ngot prompt:\n%s",
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
