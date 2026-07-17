package agent

import (
	"testing"
)

// checkRequiredFields extracts and validates the "required" array from a tool's parameters.
// If no expected fields are provided, it only checks that parameters is valid.
func checkRequiredFields(t *testing.T, params map[string]interface{}, expected ...string) {
	t.Helper()

	var requiredSet map[string]bool
	if reqRaw, ok := params["required"]; ok {
		if reqArr, ok := reqRaw.([]interface{}); ok {
			requiredSet = make(map[string]bool)
			for _, r := range reqArr {
				if s, ok := r.(string); ok {
					requiredSet[s] = true
				}
			}
		} else if reqStr, ok := reqRaw.([]string); ok {
			requiredSet = make(map[string]bool)
			for _, s := range reqStr {
				requiredSet[s] = true
			}
		} else {
			t.Fatalf("parameters.required has unexpected type: %T", reqRaw)
		}
	}

	for _, exp := range expected {
		if !requiredSet[exp] {
			t.Errorf("expected %q to be required", exp)
		}
	}
}

// TestListChannelsToolSchema verifies the JSON schema is well-formed.
func TestListChannelsToolSchema(t *testing.T) {
	schema, _ := ListChannelsTool()

	if schema.Function.Name != "list_channels" {
		t.Errorf("expected name 'list_channels', got %q", schema.Function.Name)
	}
	if schema.Type != "function" {
		t.Errorf("expected type 'function', got %q", schema.Type)
	}
	if schema.Function.Description == "" {
		t.Error("description should not be empty")
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params)
}

func TestNarrowChannelsByTopicToolSchema(t *testing.T) {
	schema, _ := NarrowChannelsByTopicTool()

	if schema.Function.Name != "narrow_channels_by_topic" {
		t.Errorf("expected name 'narrow_channels_by_topic', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "topic", "channel_ids")
}

func TestFindSharedChannelsToolSchema(t *testing.T) {
	schema, _ := FindSharedChannelsTool()

	if schema.Function.Name != "find_shared_channels" {
		t.Errorf("expected name 'find_shared_channels', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "participant_uids")
}

func TestPeekChannelToolSchema(t *testing.T) {
	schema, _ := PeekChannelTool()

	if schema.Function.Name != "peek_channel" {
		t.Errorf("expected name 'peek_channel', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "channel_id")
}

func TestFetchChannelToolSchema(t *testing.T) {
	schema, _ := FetchChannelTool()

	if schema.Function.Name != "fetch_channel" {
		t.Errorf("expected name 'fetch_channel', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "channel_id", "time_start", "time_end")
}

func TestSearchMessagesToolSchema(t *testing.T) {
	schema, _ := SearchMessagesTool()

	if schema.Function.Name != "search_messages" {
		t.Errorf("expected name 'search_messages', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "messages_handle", "keywords")
}

func TestFilterRelevantToolSchema(t *testing.T) {
	schema, _ := FilterRelevantTool()

	if schema.Function.Name != "filter_relevant" {
		t.Errorf("expected name 'filter_relevant', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "messages_handle")
}

func TestSummarizeChunkToolSchema(t *testing.T) {
	schema, _ := SummarizeChunkTool()

	if schema.Function.Name != "summarize_chunk" {
		t.Errorf("expected name 'summarize_chunk', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "messages_handle")
}

func TestMergeSummariesToolSchema(t *testing.T) {
	schema, _ := MergeSummariesTool()

	if schema.Function.Name != "merge_summaries" {
		t.Errorf("expected name 'merge_summaries', got %q", schema.Function.Name)
	}

	params, ok := schema.Function.Parameters.(map[string]interface{})
	if !ok {
		t.Fatal("parameters should be map[string]interface{}")
	}

	checkRequiredFields(t, params, "summaries")
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"hello world", 20, "hello world"},
		{"hello world", 5, "hello..."},
		{"你好世界", 2, "你好..."},
		{"", 5, ""},
		{"abc", 0, "..."},
	}

	for _, tt := range tests {
		result := truncateStr(tt.input, tt.maxLen)
		if result != tt.expected {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
		}
	}
}
