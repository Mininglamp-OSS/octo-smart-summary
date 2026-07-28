package service

import (
	"context"
	"fmt"
	"strings"
)

// maxAbstractRunes caps the generated abstract length. Defensive: the prompt
// already asks for a short abstract, but the column is VARCHAR(300) and the UI
// callout expects 2-4 sentences, so we hard-cap regardless of what the model
// returns.
const maxAbstractRunes = 120

// maxAbstractInputRunes bounds how much of the summary body we send to the LLM.
// Bodies can be large, and this call is made lazily on the first detail read —
// capping the input keeps that read fast and cheap. The leading portion carries
// the gist for a short abstract.
const maxAbstractInputRunes = 8000

const abstractPromptTemplate = `你是文本摘要助手。把下面的「总结正文」压缩成 2-4 句、不超过 120 字的中文摘要。直接输出摘要本身:不要加"摘要:"之类的前缀,不要标题,不要 Markdown 语法(不要 #、*、列表、表格、代码块)。

总结正文:
%s`

// GenerateAbstract produces a short plain-text abstract for a finalized summary
// body via a single lightweight LLM call (Option B: works uniformly for
// worker-generated and agent-captured summaries, and for backfilling old ones).
//
// It is strictly best-effort: on a nil client, empty content, an LLM error or
// an empty result it returns "" so a caller never blocks (or fails) the summary
// on the abstract — an empty abstract simply hides the callout.
func GenerateAbstract(ctx context.Context, llm *LLMClient, content string) string {
	if llm == nil {
		return ""
	}
	body := strings.TrimSpace(content)
	if body == "" {
		return ""
	}
	if runes := []rune(body); len(runes) > maxAbstractInputRunes {
		body = string(runes[:maxAbstractInputRunes])
	}
	out, _, err := llm.Call(ctx, []ChatMessage{{Role: "user", Content: fmt.Sprintf(abstractPromptTemplate, body)}}, 0.3)
	if err != nil {
		return ""
	}
	return normalizeAbstract(out)
}

// normalizeAbstract trims the raw model output and hard-caps it to
// maxAbstractRunes. Split out for testing without an LLM round-trip.
func normalizeAbstract(raw string) string {
	abstract := strings.TrimSpace(raw)
	if abstract == "" {
		return ""
	}
	if runes := []rune(abstract); len(runes) > maxAbstractRunes {
		abstract = strings.TrimSpace(string(runes[:maxAbstractRunes]))
	}
	return abstract
}
