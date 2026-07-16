package agent

// ToolLabels maps tool names to their abstract, user-facing phase.
// SSE progress events expose ONLY the phase (a safe enum) — never the raw tool
// name — so the frontend can render abstract Chinese status without leaking which
// concrete tools power the summarization. The Label is kept for server-side logs
// only and is NOT sent over SSE.
//
// Phase enum (6-tier, stable contract with the frontend):
//   understand | retrieve | filter | distill | compose | reply
var ToolLabels = map[string]struct {
	Phase string
	Label string
}{
	"list_channels":            {Phase: "understand", Label: "探索频道"},
	"narrow_channels_by_topic": {Phase: "understand", Label: "定位相关频道"},
	"find_shared_channels":     {Phase: "understand", Label: "查找共同频道"},
	"peek_channel":             {Phase: "understand", Label: "预览频道"},
	"get_current_time":         {Phase: "understand", Label: "获取当前时间"},
	"extract_time_range":       {Phase: "understand", Label: "解析时间范围"},
	"fetch_channel":            {Phase: "retrieve", Label: "抓取消息"},
	"search_messages":          {Phase: "retrieve", Label: "搜索消息"},
	"filter_relevant":          {Phase: "filter", Label: "筛选相关消息"},
	"summarize_chunk":          {Phase: "distill", Label: "分块总结"},
	"merge_summaries":          {Phase: "compose", Label: "合并结果"},
}

// GetToolLabel returns the abstract phase (and internal-only label) for a tool.
// SECURITY: if the tool is unknown it falls back to a generic phase with an EMPTY
// label — it never echoes the raw tool name, so new/unmapped tools cannot leak
// their identity through the SSE stream.
func GetToolLabel(toolName string) (phase, label string) {
	if entry, ok := ToolLabels[toolName]; ok {
		return entry.Phase, entry.Label
	}
	return "understand", ""
}
