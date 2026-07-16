package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
)

// buildRunner constructs a runner for the given profile name with LLM configuration.
// If uid is non-empty and profile is "summary" or "summary_refine", it will be injected into tool handlers.
// This is a shared helper used by both AgentChatHandler and AgentSummaryHandler.
func buildRunner(profileName, uid, llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) (*agent.Runner, string, error) {
	profile, err := agent.GetProfile(profileName)
	if err != nil {
		return nil, "", fmt.Errorf("load profile %q: %w", profileName, err)
	}
	system, err := agent.LoadPrompt(profile.PromptFile)
	if err != nil {
		return nil, "", fmt.Errorf("load prompt %q: %w", profile.PromptFile, err)
	}

	var reg *agent.Registry
	if (profileName == "summary" || profileName == "summary_refine") && uid != "" {
		reg, err = buildSummaryRegistryWithUID(uid)
	} else {
		reg, err = agent.BuildRegistry(profile.Tools)
	}
	if err != nil {
		return nil, "", fmt.Errorf("build registry: %w", err)
	}

	client := agent.NewClient(llmApiURL, llmApiKey, llmModel, llmTimeout, llmMaxTokens)
	pool := agent.NewPool(4)
	runner := agent.NewRunner(client, reg, pool, profile.Policy)
	return runner, system, nil
}

// buildSummaryRegistryWithUID builds a summary registry with uid injected into tool handlers.
func buildSummaryRegistryWithUID(uid string) (*agent.Registry, error) {
	reg := agent.NewRegistry()

	// Non-summary tools (no uid injection needed)
	for _, name := range []string{"get_current_time", "extract_time_range"} {
		factory, ok := agent.GetToolFactory(name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		schema, handler := factory()
		reg.Register(schema, handler)
	}

	// Summary tools: wrap handlers to inject uid via context
	summaryTools := []string{
		"list_channels", "narrow_channels_by_topic", "find_shared_channels",
		"peek_channel", "fetch_channel", "search_messages",
		"filter_relevant", "summarize_chunk", "merge_summaries",
	}
	for _, name := range summaryTools {
		factory, ok := agent.GetToolFactory(name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		schema, origHandler := factory()

		// Wrap handler to inject uid into context
		wrappedHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
			ctx = context.WithValue(ctx, agent.ContextKeyUID, uid)
			return origHandler(ctx, args)
		}
		reg.Register(schema, wrappedHandler)
	}

	return reg, nil
}
