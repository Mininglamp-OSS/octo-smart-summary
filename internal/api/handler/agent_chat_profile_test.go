package handler

import (
	"os"
	"testing"
)

// TestResolveChatProfile covers 缺点十三 (Profile 静默降级): the effective profile
// must match the request payload under v2, and stay byte-identical under flag-off.
func TestResolveChatProfile(t *testing.T) {
	cases := []struct {
		name         string
		mode         string // AGENT_SUMMARY_V2_MODE value ("" = unset → off)
		reqProfile   string
		hasChannels  bool
		hasRefs      bool
		wantProfile  string
		wantConflict bool
	}{
		// ---- flag OFF: legacy behavior, byte-identical, never rejects ----
		{"off/empty→chat", "off", "", false, false, "chat", false},
		{"off/empty+channels→chat (legacy downgrade preserved)", "off", "", true, false, "chat", false},
		{"off/empty+refs→chat", "off", "", false, true, "chat", false},
		{"off/explicit chat+channels→chat, no conflict", "off", "chat", true, false, "chat", false},
		{"off/explicit summary kept", "off", "summary", false, false, "summary", false},
		{"unset behaves as off", "", "", true, false, "chat", false},

		// ---- flag ON: auto-upgrade to match payload ----
		{"on/empty+channels→summary", "on", "", true, false, "summary", false},
		{"on/empty+refs-only→summary_refine", "on", "", false, true, "summary_refine", false},
		{"on/empty+channels+refs→summary (channels win)", "on", "", true, true, "summary", false},
		{"on/empty+nothing→chat", "on", "", false, false, "chat", false},

		// ---- flag ON: reject explicit data-less chat that contradicts payload ----
		{"on/explicit chat+channels→conflict", "on", "chat", true, false, "chat", true},
		{"on/explicit chat+refs→conflict", "on", "chat", false, true, "chat", true},
		{"on/explicit chat+nothing→chat, no conflict", "on", "chat", false, false, "chat", false},

		// ---- flag ON: explicit non-chat profiles pass through untouched ----
		{"on/explicit summary kept", "on", "summary", true, false, "summary", false},
		{"on/explicit summary_refine kept", "on", "summary_refine", false, true, "summary_refine", false},

		// shadow mode is also "enabled"
		{"shadow/empty+channels→summary", "shadow", "", true, false, "summary", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode == "" {
				os.Unsetenv("AGENT_SUMMARY_V2_MODE")
			} else {
				os.Setenv("AGENT_SUMMARY_V2_MODE", tc.mode)
			}
			t.Cleanup(func() { os.Unsetenv("AGENT_SUMMARY_V2_MODE") })

			gotProfile, gotConflict := resolveChatProfile(tc.reqProfile, tc.hasChannels, tc.hasRefs)
			if gotProfile != tc.wantProfile || gotConflict != tc.wantConflict {
				t.Errorf("resolveChatProfile(%q, ch=%v, ref=%v) mode=%s = (%q, %v), want (%q, %v)",
					tc.reqProfile, tc.hasChannels, tc.hasRefs, tc.mode,
					gotProfile, gotConflict, tc.wantProfile, tc.wantConflict)
			}
		})
	}
}
