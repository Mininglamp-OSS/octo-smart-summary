package agent

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 提示词与工具的可配置机制：
//   - 默认提示词以 .md 文件 embed 进二进制（prompts/<name>.md），改词无需改代码。
//   - 运行时可用 AGENT_PROMPT_DIR 指向外部目录覆盖同名 .md，实现热改、不重编译。
//   - 每个 Profile = 一段系统提示词 + 一组允许的工具名单，按场景组合。

//go:embed prompts/*.md
var embeddedPrompts embed.FS

// ToolFactory 返回一个工具的 schema + handler。所有工具在 toolFactories 集中登记。
type ToolFactory func() (Tool, Handler)

// toolFactories 是全部可用工具的中央登记表：key = 工具名，供 Profile 按名挑选。
// 新增工具只需在此加一行，再在对应 Profile 的 Tools 里引用其名字。
var toolFactories = map[string]ToolFactory{
	"get_current_time":   GetCurrentTimeTool,
	"extract_time_range": ExtractTimeRangeTool,
	// Summary tools (Stage 2)
	"list_channels":            ListChannelsTool,
	"narrow_channels_by_topic": NarrowChannelsByTopicTool,
	"find_shared_channels":     FindSharedChannelsTool,
	"peek_channel":             PeekChannelTool,
	"fetch_channel":            FetchChannelTool,
	"search_messages":          SearchMessagesTool,
	"filter_relevant":          FilterRelevantTool,
	"summarize_chunk":          SummarizeChunkTool,
	"merge_summaries":          MergeSummariesTool,
}

// Profile 描述一个 agent 场景：提示词文件名（不含 .md）+ 允许的工具名单 + 策略。
type Profile struct {
	// PromptFile 是 prompts/ 下的文件名（不含扩展名），如 "chat"、"summary"。
	PromptFile string
	// Tools 是该场景启用的工具名单，须存在于 toolFactories。
	Tools []string
	// Policy 控制回环步数/预算/超时。
	Policy Policy
}

// profiles 是内置场景登记表。改这里即可调整每个场景用哪段词、挂哪些工具。
var profiles = map[string]Profile{
	"chat": {
		PromptFile: "chat",
		Tools:      []string{"get_current_time", "extract_time_range"},
		Policy:     Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 240 * time.Second},
	},
	"summary": {
		PromptFile: "summary",
		Tools:      []string{"get_current_time", "extract_time_range", "list_channels", "narrow_channels_by_topic", "find_shared_channels", "peek_channel", "fetch_channel", "search_messages", "filter_relevant", "summarize_chunk", "merge_summaries"},
		Policy:     Policy{MaxSteps: 20, MaxTokens: 60000, StepTimeout: 240 * time.Second},
	},
	"summary_refine": {
		PromptFile: "summary_refine",
		Tools:      []string{"list_channels", "narrow_channels_by_topic", "find_shared_channels", "peek_channel", "fetch_channel", "search_messages", "filter_relevant", "summarize_chunk", "merge_summaries", "get_current_time", "extract_time_range"},
		// MaxTokens bumped from 40000 to 120000: reference-based flows inject
		// the old summary's content + snapshot + citations JSON into every
		// turn's system message (~21K tokens/turn observed), so a 2-3 step
		// refine (get_time + fetch + answer) blew through 40K by step 2 and
		// triggered "已达 token 预算" mid-flow. 120K gives room for 4-5 tool
		// steps at ~25K each. See CHAT-REFERENCE-BASED-DESIGN-v1 diagnostic.
		//
		// StepTimeout 240s (was 60s): a single LLM planning call on
		// sonnet/kimi with 20-40K tokens of context routinely takes 60-100s;
		// the old 60s tripped stepCtx before the LLM finished streaming and
		// surfaced as `[agent] chat runner error: context deadline exceeded`.
		// AGENT_STEP_TIMEOUT env still overrides at GetProfile time (see below).
		Policy: Policy{MaxSteps: 15, MaxTokens: 120000, StepTimeout: 240 * time.Second},
	},
}

var (
	promptCacheMu sync.RWMutex
	promptCache   = map[string]string{}
)

// LoadPrompt 读取指定名字的系统提示词。
// 优先级：AGENT_PROMPT_DIR/<name>.md（外部可热改） > embed 内置默认。
// 结果按 name 缓存；外部目录存在时跳过缓存以便热改（改完即生效）。
func LoadPrompt(name string) (string, error) {
	if dir := strings.TrimSpace(os.Getenv("AGENT_PROMPT_DIR")); dir != "" {
		p := filepath.Join(dir, name+".md")
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
		// 外部目录设了但没这个文件：回退到 embed，不报错。
	}

	promptCacheMu.RLock()
	if v, ok := promptCache[name]; ok {
		promptCacheMu.RUnlock()
		return v, nil
	}
	promptCacheMu.RUnlock()

	b, err := embeddedPrompts.ReadFile("prompts/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("prompt %q not found (embed): %w", name, err)
	}
	v := strings.TrimSpace(string(b))

	promptCacheMu.Lock()
	promptCache[name] = v
	promptCacheMu.Unlock()
	return v, nil
}

// BuildRegistry 按名单构造一个只含指定工具的 Registry。未知工具名报错。
func BuildRegistry(toolNames []string) (*Registry, error) {
	reg := NewRegistry()
	for _, name := range toolNames {
		factory, ok := toolFactories[name]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q (not in toolFactories)", name)
		}
		schema, handler := factory()
		reg.Register(schema, handler)
	}
	return reg, nil
}

// GetProfile 取一个场景定义。未知场景报错。
//
// StepTimeout override: the static profile values above are code defaults
// (60s) that historically fit the pipeline flow. Interactive agent chat —
// especially refine flows injecting ~21K tokens of referenced summary per
// turn — can push a single LLM planning call well past 60s on slower
// models (kimi / sonnet with large context). AGENT_STEP_TIMEOUT env
// overrides the static 240s default. Setting
// to 0 disables the override and keeps the code default.
func GetProfile(name string) (Profile, error) {
	p, ok := profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown agent profile %q", name)
	}
	if override := agentStepTimeoutOverride(); override > 0 {
		p.Policy.StepTimeout = override
	}
	return p, nil
}

// GetToolFactory returns a tool factory by name. Used for per-request registry construction.
func GetToolFactory(name string) (ToolFactory, bool) {
	f, ok := toolFactories[name]
	return f, ok
}

// agentStepTimeoutOverride returns the AGENT_STEP_TIMEOUT env value as a
// duration if it parses to a positive integer, else 0 (meaning: keep the
// profile's static StepTimeout). Read directly from os.Getenv rather than
// via config.Config so this remains the single source of truth and profile
// lookup does not require SetSummaryDeps. This matters for unit tests that
// build a runner without initializing the whole deps container.
func agentStepTimeoutOverride() time.Duration {
	v := strings.TrimSpace(os.Getenv("AGENT_STEP_TIMEOUT"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
