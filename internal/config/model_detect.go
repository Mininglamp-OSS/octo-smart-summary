package config

import "strings"

// IsKimiModel returns true for Kimi K2 series models.
// Matches: kimi-k2.6, kimi-k2.5, mlamp/kimi-k2.6, kimi_k2.6, etc.
func IsKimiModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "kimi-k2") || strings.Contains(m, "kimi_k2")
}

// IsQwenOrDeepSeekModel returns true for Qwen 3.6 / DeepSeek V4 models.
func IsQwenOrDeepSeekModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "qwen3.6") || strings.Contains(m, "deepseek-v4")
}
