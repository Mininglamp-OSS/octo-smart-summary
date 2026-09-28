package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// MarshalRequestBody is load-bearing for the 16 MiB size guard: it disables Go's
// HTML escaping so <>& do not inflate ~6× and push a completed Map/Reduce body
// past MaxRequestBodyBytes (#256 P2-5). Nothing else pinned that behaviour, so a
// refactor back to json.Marshal at any send site would silently restore the
// inflation with no failing test. This pins the load-bearing properties.
//
// Escape sequences are built from explicit bytes/runes rather than written as
// string literals so the assertions cannot be confused by source-level escaping.
func TestMarshalRequestBody(t *testing.T) {
	type payload struct {
		Msg string `json:"msg"`
	}
	in := payload{Msg: "a<b>c&d"}

	got, err := MarshalRequestBody(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotStr := string(got)

	// 1. The three HTML metacharacters must survive verbatim — this is the point.
	for _, unescaped := range []string{"<", ">", "&"} {
		if !strings.Contains(gotStr, unescaped) {
			t.Errorf("MarshalRequestBody dropped/escaped %q; HTML escaping must be off: %s", unescaped, gotStr)
		}
	}
	// ...and with HTML escaping off, "a<b>c&d" needs NO escaping at all, so the
	// serialized body must contain zero backslashes. A single backslash would mean
	// something got escaped (i.e. the \u00XX inflation is back).
	backslash := string([]byte{0x5c})
	if strings.Contains(gotStr, backslash) {
		t.Errorf("MarshalRequestBody escaped a metacharacter (found a backslash): %s", gotStr)
	}

	// 2. No trailing newline — Encoder.Encode appends one; the guard measures these
	//    exact bytes, so the body and the measured length must match byte-for-byte.
	if strings.HasSuffix(gotStr, "\n") {
		t.Errorf("MarshalRequestBody left a trailing newline: %q", gotStr)
	}

	// 3. Still valid JSON that round-trips.
	var back payload
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, gotStr)
	}
	if back != in {
		t.Fatalf("round-trip changed the value: got %+v, want %+v", back, in)
	}

	// 4. Residual inflation is real (matches the MaxRequestBodyBytes comment):
	//    U+2028 / U+2029 are escaped UNCONDITIONALLY, independent of SetEscapeHTML,
	//    so the worst case is ~2×, not 1×. The RAW rune must NOT appear in the
	//    output (it was expanded to a 6-byte escape). Pin it so the comment cannot
	//    drift from behaviour.
	lineSep := string(rune(0x2028))
	sep, err := MarshalRequestBody(payload{Msg: "x" + lineSep + "y"})
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
