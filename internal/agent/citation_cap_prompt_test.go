package agent

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

// The planner prompt used to HARDCODE the cap number twice. That is the exact
// drift citation.PromptRuleZH exists to prevent, reintroduced in a .md file
// where no compiler could see it.
func TestPlannerPromptStatesTheResolvedCap(t *testing.T) {
	for _, n := range []string{"1", "3", "5", "12"} {
		t.Run("env="+n, func(t *testing.T) {
			t.Setenv(config.MaxCitationsPerClaimEnvVar, n)
			got, err := LoadPrompt("summary")
			if err != nil {
				t.Fatalf("LoadPrompt: %v", err)
			}
			if strings.Contains(got, CitationCapPlaceholder) {
				t.Fatal("placeholder was not substituted")
			}
			want := "最多标注 " + n + " 个"
			if !strings.Contains(got, want) {
				t.Errorf("planner prompt does not state cap %s; citation-rules section:\n%s",
					n, citationSection(got))
			}
			// It must not state any OTHER number: a planner told 3 while the
			// code enforces 5 is a contract the model cannot satisfy.
			for _, other := range []string{"1", "3", "5", "12"} {
				if other != n && strings.Contains(got, "最多标注 "+other+" 个") {
					t.Errorf("planner prompt states cap %s as well as %s", other, n)
				}
			}
		})
	}
}

// The documented kill switch. At 0 the planner must not be told to cap at all,
// and must not be told markers are truncated server-side — because with the
// cap disabled they are not.
func TestPlannerPromptDropsTheCapRuleWhenDisabled(t *testing.T) {
	for _, off := range []string{"0", "-1"} {
		t.Run("env="+off, func(t *testing.T) {
			t.Setenv(config.MaxCitationsPerClaimEnvVar, off)
			got, err := LoadPrompt("summary")
			if err != nil {
				t.Fatalf("LoadPrompt: %v", err)
			}
			if strings.Contains(got, CitationCapPlaceholder) {
				t.Fatal("placeholder left in the prompt")
			}
			for _, forbidden := range []string{"最多标注", "服务端被截断", "长串引用"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("cap disabled but the planner prompt still says %q:\n%s",
						forbidden, citationSection(got))
				}
			}
			// The rest of the citation rules must survive.
			if !strings.Contains(got, "## 引用规则") {
				t.Error("the citation-rules section itself disappeared")
			}
			if !strings.Contains(got, "分开写成 `[19][20][21]`") {
				t.Error("an unrelated citation rule was dropped along with the cap rule")
			}
			if strings.Contains(got, "\n\n- 编号来源") {
				t.Error("removing the cap rule left a blank line inside the bullet list")
			}
		})
	}
}

// Mutation evidence: the pre-fix prompt is served verbatim, so it states 3 no
// matter what the operator configured.
func TestMutationHardcodedPlannerCapIgnoresTheKnob(t *testing.T) {
	const preFix = "- 每一条结论/要点最多标注 3 个 `[n]`；支持来源更多时,只选最有代表性、最新的 3 条"

	t.Setenv(config.MaxCitationsPerClaimEnvVar, "5")
	got, err := LoadPrompt("summary")
	if err != nil {
		t.Fatalf("LoadPrompt: %v", err)
	}
	if strings.Contains(got, preFix) {
		t.Fatalf("MUTATION CHECK FAILED: with the knob at 5 the prompt still contains the "+
			"hardcoded 3-marker rule:\n%s", citationSection(got))
	}
	if !strings.Contains(got, "最多标注 5 个") {
		t.Fatalf("prompt does not state the configured cap 5:\n%s", citationSection(got))
	}
	t.Logf("MUTATION EVIDENCE: knob=5 -> planner prompt says 5. The pre-fix file "+
		"hardcoded %q regardless of configuration.", preFix)
}

// The final answer of the PLANNER is capped. Previously nothing did this:
// summary.md claimed "超出上限的标记会在服务端被截断" while no call site
// touched the planner's body, and cap.go's package doc listed an
// internal/api/handler enforcement site that did not exist.
func TestCapFinalAnswerEnforcesTheCap(t *testing.T) {
	t.Setenv(config.MaxCitationsPerClaimEnvVar, "3")

	reply := "结论一：范围已确认[1][2][3][4][5][6][7][8]\n结论二：负责人已定[9][10][11][12]"
	msgs := []Message{
		{Role: "user", Content: "总结一下"},
		{Role: "assistant", Content: reply},
	}

	got, gotMsgs := CapFinalAnswer("s1", reply, msgs)

	for i, line := range strings.Split(got, "\n") {
		if n := len(citation.Numbers(line)); n > 3 {
			t.Errorf("line %d kept %d markers, cap is 3: %q", i, n, line)
		}
		if n := len(citation.Numbers(line)); n == 0 {
			t.Errorf("line %d lost every citation: %q", i, line)
		}
	}
	// The persisted assistant turn must match what the user was shown.
	if gotMsgs[1].Content != got {
		t.Errorf("persisted assistant message %q != returned reply %q", gotMsgs[1].Content, got)
	}
	if gotMsgs[0].Content != "总结一下" {
		t.Error("the user message was modified")
	}
}

func TestCapFinalAnswerIsAByteIdenticalNoOpWhenDisabled(t *testing.T) {
	reply := "结论一：范围已确认[1][2][3][4][5][6][7][8]"
	for _, off := range []string{"0", "-1"} {
		t.Setenv(config.MaxCitationsPerClaimEnvVar, off)
		msgs := []Message{{Role: "assistant", Content: reply}}
		got, gotMsgs := CapFinalAnswer("s1", reply, msgs)
		if got != reply {
			t.Errorf("env=%s: cap disabled but the reply changed:\n got: %q\nwant: %q", off, got, reply)
		}
		if gotMsgs[0].Content != reply {
			t.Errorf("env=%s: cap disabled but the persisted message changed", off)
		}
	}
}

// citationSection extracts the "## 引用规则" block for readable failures.
func citationSection(prompt string) string {
	i := strings.Index(prompt, "## 引用规则")
	if i < 0 {
		return prompt
	}
	rest := prompt[i:]
	if j := strings.Index(rest[len("## 引用规则"):], "\n## "); j >= 0 {
		return rest[:len("## 引用规则")+j]
	}
	return rest
}
