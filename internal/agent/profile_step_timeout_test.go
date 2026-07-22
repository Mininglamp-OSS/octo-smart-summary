package agent

import (
	"testing"
	"time"
)

// TestGetProfile_StepTimeoutOverride 覆盖 AGENT_STEP_TIMEOUT env 覆盖 profile 静态
// StepTimeout 的路径。修 refine 场景 60s 硬编码 stepCtx 挂 (agent chat runner
// error: context deadline exceeded @ 83s) 的核心机制;要保证:
//   - 未设 env → 保持 profile 静态 default(现在 240s · 之前 60s)
//   - 设 env > 0 → override 生效,3 个 profile 都受影响
//   - 设 env=0 或非法 → 保持 default(不 override)
func TestGetProfile_StepTimeoutOverride(t *testing.T) {
	// 2 个 profile 都要跑一次 · summary / summary_refine(仓库里只有这两个)
	profileNames := []string{"summary", "summary_refine"}

	// case 1: 未设 env · 保持 240s 静态 default
	t.Run("no_env_keeps_static_default", func(t *testing.T) {
		t.Setenv("AGENT_STEP_TIMEOUT", "")
		for _, name := range profileNames {
			p, err := GetProfile(name)
			if err != nil {
				t.Fatalf("GetProfile(%q): %v", name, err)
			}
			if p.Policy.StepTimeout != 240*time.Second {
				t.Errorf("profile %q: unset env should keep static default 240s, got %v",
					name, p.Policy.StepTimeout)
			}
		}
	})

	// case 2: env=120 · override 生效(选一个不同于 240 的值来区分 default vs override)
	t.Run("env_120_overrides_all_profiles", func(t *testing.T) {
		t.Setenv("AGENT_STEP_TIMEOUT", "120")
		for _, name := range profileNames {
			p, err := GetProfile(name)
			if err != nil {
				t.Fatalf("GetProfile(%q): %v", name, err)
			}
			if p.Policy.StepTimeout != 120*time.Second {
				t.Errorf("profile %q: AGENT_STEP_TIMEOUT=120 should override to 120s, got %v",
					name, p.Policy.StepTimeout)
			}
		}
	})

	// case 3: env=0 · 保持 default(0 视为"关闭 override")
	t.Run("env_zero_keeps_static_default", func(t *testing.T) {
		t.Setenv("AGENT_STEP_TIMEOUT", "0")
		p, err := GetProfile("summary_refine")
		if err != nil {
			t.Fatalf("GetProfile: %v", err)
		}
		if p.Policy.StepTimeout != 240*time.Second {
			t.Errorf("env=0 should keep default 240s, got %v", p.Policy.StepTimeout)
		}
	})

	// case 4: env 非法(负值 / 非数字) · 保持 default
	t.Run("env_invalid_keeps_static_default", func(t *testing.T) {
		invalidVals := []string{"-1", "abc", "60s", "1.5"}
		for _, v := range invalidVals {
			t.Setenv("AGENT_STEP_TIMEOUT", v)
			p, err := GetProfile("summary_refine")
			if err != nil {
				t.Fatalf("GetProfile: %v", err)
			}
			if p.Policy.StepTimeout != 240*time.Second {
				t.Errorf("env=%q should keep default 240s, got %v", v, p.Policy.StepTimeout)
			}
		}
	})
}
