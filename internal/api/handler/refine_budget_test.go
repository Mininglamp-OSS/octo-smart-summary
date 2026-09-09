package handler

import (
	"os"
	"testing"
	"time"
)

// TestRefineTimeout_ParseTable pins the behaviour of every input class.
//
// This is the test the original PR was missing: refineTimeout() decides the
// parent deadline for all four refine handlers, so a value that parses but is
// nonsense reshapes every refine request in production. The two rows that
// matter most are the wrong-unit and overflow cases — both parse cleanly and
// both are invisible without a clamp.
func TestRefineTimeout_ParseTable(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		env  string
		want time.Duration
	}{
		{name: "unset keeps the historical default", set: false, want: defaultRefineTimeout},
		{name: "empty is treated as unset", set: true, env: "", want: defaultRefineTimeout},
		{name: "whitespace only is treated as unset", set: true, env: "   ", want: defaultRefineTimeout},
		{name: "plain value", set: true, env: "120", want: 120 * time.Second},
		{name: "surrounding whitespace is tolerated", set: true, env: "  120  ", want: 120 * time.Second},
		{name: "one second is the smallest accepted value", set: true, env: "1", want: time.Second},
		{name: "at the maximum", set: true, env: "1800", want: maxRefineTimeout},

		// Malformed input must degrade to the default, never to "no deadline".
		{name: "non-numeric falls back", set: true, env: "abc", want: defaultRefineTimeout},
		{name: "float is rejected by Atoi", set: true, env: "90.5", want: defaultRefineTimeout},
		{name: "duration string is not accepted", set: true, env: "90s", want: defaultRefineTimeout},
		{name: "zero falls back", set: true, env: "0", want: defaultRefineTimeout},
		{name: "negative falls back", set: true, env: "-30", want: defaultRefineTimeout},

		// Wrong unit: someone assumes milliseconds. Parses fine, and without a
		// clamp yields a 25-hour deadline that holds the connection open.
		{name: "milliseconds mistake is clamped", set: true, env: "90000", want: maxRefineTimeout},

		// Overflow: seconds * time.Second wraps to a NEGATIVE duration, which
		// makes every refine handler build an already-expired context, failing
		// all four endpoints instantly and silently.
		{name: "int64 max does not wrap negative", set: true, env: "9223372036854775807", want: maxRefineTimeout},
		{name: "beyond int64 is unparsable", set: true, env: "99999999999999999999", want: defaultRefineTimeout},

		// The budget at which a fallback model gets one complete attempt under
		// the runner's real semantics: (MaxAttempts+1)*LLM_TIMEOUT + backoffs
		// = 4*180 + 3. Documented in CONFIGURATION.md and refine_budget.go, and
		// it must be expressible through this knob rather than hitting the
		// ceiling.
		{name: "the documented fallback-reachable budget is expressible", set: true, env: "723", want: 723 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv first even in the unset case: it registers the cleanup
			// AND lets Unsetenv pin the row against an ambient REFINE_TIMEOUT in
			// the developer's shell, which would otherwise make this row lie.
			t.Setenv(refineTimeoutEnvVar, tc.env)
			if !tc.set {
				os.Unsetenv(refineTimeoutEnvVar)
			}
			got := refineTimeout()
			if got != tc.want {
				t.Errorf("refineTimeout() = %v, want %v", got, tc.want)
			}
			// The invariant that actually protects production: whatever the
			// input, the result is always a usable positive deadline.
			if got <= 0 {
				t.Errorf("refineTimeout() = %v; a non-positive budget builds an already-expired context", got)
			}
			if got > maxRefineTimeout {
				t.Errorf("refineTimeout() = %v, above the ceiling %v", got, maxRefineTimeout)
			}
		})
	}
}

// TestRefineTimeout_DefaultIsUnchanged guards the deliberate decision not to
// raise the default in this change. Raising it alters user-visible latency on a
// path that errors at 90s today, and the correct value depends on measured
// refine percentiles (#220). If someone changes it, that should be a conscious
// edit to this test, not a silent side effect.
func TestRefineTimeout_DefaultIsUnchanged(t *testing.T) {
	if defaultRefineTimeout != 90*time.Second {
		t.Errorf("defaultRefineTimeout = %v, want 90s; raising it changes user-visible refine latency", defaultRefineTimeout)
	}
}
