package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmclient"
)

// The 16 MiB size guard (#256 P2-5) is load-bearing on HOW the request body is
// serialized: Go's default HTML escaping inflates <>& ~6×, which can push a
// completed Map/Reduce body past MaxRequestBodyBytes. Production send sites go
// through llmclient.MarshalRequest — that is where SetEscapeHTML(false) and the
// trailing-newline trim actually live — so the assertions pin THAT function
// (round-5 P2, Jerry-Xin 5478795037: the previous test guarded a
// production-dead service.MarshalRequestBody and a revert of llmclient's
// encoder to json.Marshal left it green).
//
// Escape sequences are built from explicit bytes/runes rather than written as
// string literals so the assertions cannot be confused by source-level escaping.
func TestMarshalRequest_NoHTMLEscapeInflation(t *testing.T) {
	req := llmclient.Request{
		Messages: []llmclient.Message{
			{Role: "user", Content: "a<b>c&d"},
		},
	}

	got, err := llmclient.MarshalRequest("test-model", req, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotStr := string(got)

	// 1. The three HTML metacharacters must survive verbatim — this is the point.
	for _, unescaped := range []string{"<", ">", "&"} {
		if !strings.Contains(gotStr, unescaped) {
			t.Errorf("MarshalRequest dropped/escaped %q; HTML escaping must be off: %s", unescaped, gotStr)
		}
	}
	// ...and with HTML escaping off, "a<b>c&d" needs NO escaping at all, so the
	// serialized body must contain zero backslashes. A single backslash would mean
	// something got escaped (i.e. the \u00XX inflation is back).
	backslash := string([]byte{0x5c})
	if strings.Contains(gotStr, backslash) {
		t.Errorf("MarshalRequest escaped a metacharacter (found a backslash): %s", gotStr)
	}

	// 2. No trailing newline — Encoder.Encode appends one; the guard measures these
	//    exact bytes, so the body and the measured length must match byte-for-byte.
	if strings.HasSuffix(gotStr, "\n") {
		t.Errorf("MarshalRequest left a trailing newline: %q", gotStr)
	}

	// 3. Still valid JSON that semantically round-trips the content.
	var roundTrip struct {
		Model    string              `json:"model"`
		Messages []llmclient.Message `json:"messages"`
	}
	if err := json.Unmarshal(got, &roundTrip); err != nil {
		t.Fatalf("MarshalRequest produced invalid JSON: %v; body=%s", err, gotStr)
	}
	if roundTrip.Model != "test-model" || len(roundTrip.Messages) != 1 || roundTrip.Messages[0].Content != "a<b>c&d" {
		t.Errorf("round-trip mismatch: %+v", roundTrip)
	}

	// 4. Residual inflation is real: U+2028 / U+2029 are escaped
	//    UNCONDITIONALLY, independent of SetEscapeHTML. They expand from a
	//    3-byte UTF-8 rune to a 6-byte escape (~2×). This test pins that common
	//    residual class only; other rare JSON-required escape classes such as
	//    C0 controls can still expand more. The size guard is intentionally a
	//    coarse backstop, not a proof that every possible byte pattern fits.
	lineSep := string(rune(0x2028))
	reqSep := llmclient.Request{
		Messages: []llmclient.Message{
			{Role: "user", Content: "x" + lineSep + "y"},
		},
	}
	sep, err := llmclient.MarshalRequest("test-model", reqSep, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(sep), lineSep) {
		t.Errorf("expected U+2028 to be escaped (SetEscapeHTML does not affect it), but the raw rune survived: %s", sep)
	}
	if !strings.Contains(string(sep), "u2028") {
		t.Errorf("expected the U+2028 escape form in the output: %s", sep)
	}
}

// The size guard's constant is exported through the service facade; pin the
// alias so a silent change to the ceiling surfaces here.
func TestMaxRequestBodyBytesAlias(t *testing.T) {
	if MaxRequestBodyBytes != llmclient.MaxRequestBodyBytes {
		t.Fatalf("MaxRequestBodyBytes alias drifted: service=%d llmclient=%d", MaxRequestBodyBytes, llmclient.MaxRequestBodyBytes)
	}
}
