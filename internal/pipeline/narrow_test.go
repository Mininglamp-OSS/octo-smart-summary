package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestPreRetrievalNarrow_TopicTruncation(t *testing.T) {
	start := time.Now().Add(-24 * time.Hour)
	end := time.Now()

	// stub that captures the topic passed to the LLM
	var captured string
	stubFn := func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		for _, m := range msgs {
			if m.Role == "user" {
				captured = m.Content
				break
			}
		}
		return `{"has_time_expr": false}`, nil
	}

	t.Run("topic<=1000 is not truncated", func(t *testing.T) {
		captured = ""
		topic := strings.Repeat("あ", 1000)
		PreRetrievalNarrow(context.Background(), topic, start, end, stubFn)
		if !strings.Contains(captured, topic) {
			t.Errorf("expected full topic (1000 runes) to be preserved in prompt")
		}
	})

	t.Run("topic>1000 is truncated to 1000", func(t *testing.T) {
		captured = ""
		topic := strings.Repeat("あ", 1500)
		PreRetrievalNarrow(context.Background(), topic, start, end, stubFn)
		// The truncated topic should have exactly 1000 runes of 'あ'
		truncated := strings.Repeat("あ", 1000)
		if !strings.Contains(captured, truncated) {
			t.Errorf("expected truncated topic (1000 runes) in prompt")
		}
		// Should NOT contain the full 1500-rune topic
		if strings.Contains(captured, topic) {
			t.Errorf("expected topic to be truncated, but full topic found in prompt")
		}
		// Verify rune count of the topic extracted from the prompt
		// The topic appears between quotes in the prompt: 主题："<topic>"
		idx := strings.Index(captured, "主题：\"")
		if idx < 0 {
			t.Fatalf("could not find topic marker in prompt")
		}
		after := captured[idx+len("主题：\""):]
		endIdx := strings.Index(after, "\"")
		if endIdx < 0 {
			t.Fatalf("could not find closing quote for topic")
		}
		extracted := after[:endIdx]
		if got := utf8.RuneCountInString(extracted); got != 1000 {
			t.Errorf("expected extracted topic to have 1000 runes, got %d", got)
		}
	})
}
