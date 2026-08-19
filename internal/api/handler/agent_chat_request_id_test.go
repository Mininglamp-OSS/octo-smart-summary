package handler

import (
	"strings"
	"testing"
)

// TestRequestIDPattern covers the VARCHAR(128) backstop for the optional V2
// idempotency key: request_id flows into agent_summary_run uk_run_request's
// VARCHAR(128) column, and INSERT IGNORE would silently truncate an overlong
// value — two distinct request_ids sharing a 128-char prefix would then
// collide on the unique key and the later one would silently adopt the
// earlier run. The handler gate rejects non-empty values that fail the pattern.
func TestRequestIDPattern(t *testing.T) {
	valid := []string{
		"req-1",
		"A_b-9",
		strings.Repeat("a", 128), // exactly the column width
	}
	for _, v := range valid {
		if !requestIDPattern.MatchString(v) {
			t.Errorf("requestIDPattern should accept %q", v[:min(20, len(v))])
		}
	}

	invalid := []string{
		strings.Repeat("a", 129), // would be truncated by VARCHAR(128)
		"req 1",                  // space
		"req;drop",               // quote/injection-ish chars
		"",                       // empty never reaches the gate (optional field)
	}
	for _, v := range invalid {
		if requestIDPattern.MatchString(v) {
			t.Errorf("requestIDPattern should reject %q", v[:min(20, len(v))])
		}
	}
}
