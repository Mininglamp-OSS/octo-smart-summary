package pipeline

import (
	"context"
	"errors"
	"testing"
)

// TestNarrowByTopicReportDistinguishesPassThrough pins the signal the result
// alone cannot carry.
//
// NarrowByTopic is the identity on four paths, and a caller comparing only the
// returned slice cannot tell a deliberate "all candidates are relevant" from a
// transient LLM blip. tool_narrow_channels records the run's committed scope from
// this call, and because the stored union is monotonic, recording a pass-through
// is unrecoverable for the rest of the run: every later clean fetch still reports
// the untouched candidates as never-fetched gaps.
func TestNarrowByTopicReportDistinguishesPassThrough(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "c1", ChannelName: "研发"},
		{ChannelID: "c2", ChannelName: "销售"},
		{ChannelID: "c3", ChannelName: "运维"},
	}
	ok := func(ctx context.Context, prompt string) (string, error) { return `["c1"]`, nil }

	t.Run("a real narrowing reports true", func(t *testing.T) {
		got, narrowed := NarrowByTopicReport(context.Background(), "研发进展", candidates, ok)
		if !narrowed {
			t.Fatal("narrowed = false, want true — the filter ran and selected a subset")
		}
		if len(got) != 1 || got[0].ChannelID != "c1" {
			t.Fatalf("result = %+v, want just c1", got)
		}
	})

	t.Run("an explicit all-channel selection is still an authoritative decision", func(t *testing.T) {
		all := func(ctx context.Context, prompt string) (string, error) {
			return `["c1","c2","c3"]`, nil
		}
		got, narrowed := NarrowByTopicReport(context.Background(), "所有群聊", candidates, all)
		if !narrowed {
			t.Fatal("narrowed = false, want true for a successful explicit all-channel selection")
		}
		if len(got) != len(candidates) {
			t.Fatalf("result = %+v, want every authorised candidate", got)
		}
	})

	for _, tc := range []struct {
		name  string
		topic string
		llm   LLMCallFn
	}{
		{"empty topic", "", ok},
		{"nil llmFn", "研发进展", nil},
		{"LLM error", "研发进展", func(ctx context.Context, p string) (string, error) {
			return "", errors.New("upstream 503")
		}},
		{"unparseable output", "研发进展", func(ctx context.Context, p string) (string, error) {
			return "抱歉，我无法处理这个请求", nil
		}},
		{"zero matches", "研发进展", func(ctx context.Context, p string) (string, error) {
			return `["nope"]`, nil
		}},
	} {
		t.Run(tc.name+" reports false", func(t *testing.T) {
			got, narrowed := NarrowByTopicReport(context.Background(), tc.topic, candidates, tc.llm)
			if narrowed {
				t.Fatal("narrowed = true, want false — this is a pass-through, not a scope decision")
			}
			if len(got) != len(candidates) {
				t.Fatalf("a pass-through must return the input unchanged, got %+v", got)
			}
		})
	}

	t.Run("no candidates", func(t *testing.T) {
		if _, narrowed := NarrowByTopicReport(context.Background(), "研发进展", nil, ok); narrowed {
			t.Fatal("narrowed = true on an empty candidate list")
		}
	})

	// The deprecated wrapper must keep its exact old behaviour for every path.
	t.Run("NarrowByTopic wrapper is unchanged", func(t *testing.T) {
		if got := NarrowByTopic(context.Background(), "研发进展", candidates, ok); len(got) != 1 {
			t.Fatalf("wrapper result = %+v, want the narrowed set", got)
		}
		if got := NarrowByTopic(context.Background(), "", candidates, ok); len(got) != len(candidates) {
			t.Fatalf("wrapper must pass through on an empty topic, got %+v", got)
		}
	})
}

// TestIntersectParticipantChannelsIsIdentityOnEmptyParticipants pins the fact
// behind the find_shared_channels guard: with no participants the "intersection"
// is the caller's own full channel list, i.e. exactly what list_channels returns
// and was deliberately stopped from being recorded as scope.
func TestIntersectParticipantChannelsIsIdentityOnEmptyParticipants(t *testing.T) {
	creator := []ChannelInfo{{ChannelID: "c1"}, {ChannelID: "c2"}, {ChannelID: "c3"}}

	got, err := IntersectParticipantChannels(context.Background(), creator, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(creator) {
		t.Fatalf("result = %+v, want the creator list unchanged — this is why the caller must not record it", got)
	}

	got, err = IntersectParticipantChannels(context.Background(), creator, []string{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(creator) {
		t.Fatalf("an empty (non-nil) participant slice is the same identity path, got %+v", got)
	}
}
