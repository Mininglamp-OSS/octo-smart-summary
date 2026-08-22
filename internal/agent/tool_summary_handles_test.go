package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalSummarizeChunkResultReturnsOnlyHandleAndCoverage(t *testing.T) {
	ctx := withSummaryHandleStore(context.Background())
	largeSummary := strings.Repeat("局部总结正文", 6000)
	out, err := marshalSummarizeChunkResult(ctx, largeSummary, 7, chunkCoverage{
		InputCount:            1200,
		ProcessedCount:        1200,
		OversizedMessageCount: 2,
		ChunkSize:             200,
	})
	if err != nil {
		t.Fatalf("marshalSummarizeChunkResult: %v", err)
	}
	if len(out) >= 1024 {
		t.Fatalf("planner-visible Map result is %d bytes, want < 1024", len(out))
	}
	if strings.Contains(out, largeSummary[:100]) || strings.Contains(out, `"summary":`) {
		t.Fatal("planner-visible Map result leaked the summary body")
	}

	var result summarizeChunkToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.SummaryHandle == "" || result.ChunkCount != 7 || result.ProcessedCount != 1200 {
		t.Fatalf("unexpected result: %+v", result)
	}
	store, _ := summaryHandleStoreFromContext(ctx)
	resolved, err := store.ResolveAll([]string{result.SummaryHandle})
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if resolved.Entries[0].Text != largeSummary {
		t.Fatal("stored Map text did not round-trip")
	}
}

func TestMergeSummariesToolResolvesHandlesInsideBackend(t *testing.T) {
	ctx := withSummaryHandleStore(context.Background())
	store, _ := summaryHandleStoreFromContext(ctx)
	h1, _ := store.Put("first body", 2)
	h2, _ := store.Put("second body", 5)

	original := mergeSummariesLLM
	defer func() { mergeSummariesLLM = original }()
	var gotCombined string
	mergeSummariesLLM = func(_ context.Context, combined, _ string) (string, bool, error) {
		gotCombined = combined
		return "# merged", false, nil
	}

	_, handler := MergeSummariesTool()
	args, _ := json.Marshal(map[string]interface{}{"summary_handles": []string{h2, h1}})
	out, err := handler(ctx, args)
	if err != nil {
		t.Fatalf("merge_summaries: %v", err)
	}
	if !strings.HasPrefix(gotCombined, "second body") || !strings.Contains(gotCombined, "first body") {
		t.Fatalf("resolved bodies in wrong order: %q", gotCombined)
	}
	var result struct {
		MergedSummary string `json:"merged_summary"`
		ChunkCount    int    `json:"chunk_count"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.MergedSummary != "# merged" || result.ChunkCount != 7 {
		t.Fatalf("unexpected merge result: %+v", result)
	}
	if store.NeedsReduce() {
		t.Fatal("successful Reduce over all handles should satisfy the runner gate")
	}
}

func TestMergeSummariesToolRejectsBodyOrPartialHandleArguments(t *testing.T) {
	ctx := withSummaryHandleStore(context.Background())
	store, _ := summaryHandleStoreFromContext(ctx)
	h1, _ := store.Put("one", 1)
	_, _ = store.Put("two", 1)
	_, handler := MergeSummariesTool()

	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{name: "legacy body argument", args: `{"summaries":["one","two"]}`, want: "at least one"},
		{name: "partial handles", args: `{"summary_handles":["` + h1 + `"]}`, want: "all Map results"},
		{name: "malformed JSON", args: `{"summary_handles":["` + h1 + `"`, want: "parse args"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := handler(ctx, json.RawMessage(tc.args)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
