package summaryspec

import (
	"testing"
)

func strptr(s string) *string { return &s }

func TestValidateFillsDefaults(t *testing.T) {
	d := Draft{Objective: strptr("总结本周风险")}
	spec, src, err := Validate(d, Options{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if spec.Language != DefaultLanguage || spec.DetailLevel != DefaultDetailLevel || spec.CitationPolicy != DefaultCitation {
		t.Fatalf("defaults not applied: %+v", spec)
	}
	// Objective was provided (default ProvidedSource = inferred); enums defaulted.
	if src["objective"] != SourceInferred {
		t.Errorf("objective source = %q, want inferred", src["objective"])
	}
	for _, f := range []string{"language", "detail_level", "citation_policy", "channels", "time_range"} {
		if src[f] != SourceDefault {
			t.Errorf("%s source = %q, want default", f, src[f])
		}
	}
}

func TestValidateRecordsProvidedSources(t *testing.T) {
	d := Draft{
		Objective: strptr("风险"),
		Language:  strptr("en"),
		Channels:  []Channel{{ChannelID: "ch1", Name: "dev"}},
	}
	spec, src, err := Validate(d, Options{ProvidedSource: SourceUser, ChannelSource: SourceUI})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if spec.Language != "en" {
		t.Errorf("language = %q, want en", spec.Language)
	}
	if src["objective"] != SourceUser {
		t.Errorf("objective source = %q, want user", src["objective"])
	}
	if src["language"] != SourceUser {
		t.Errorf("language source = %q, want user", src["language"])
	}
	if src["channels"] != SourceUI {
		t.Errorf("channels source = %q, want ui", src["channels"])
	}
}

func TestValidateCoercesUnknownEnum(t *testing.T) {
	d := Draft{Objective: strptr("x"), Language: strptr("klingon"), DetailLevel: strptr("epic")}
	spec, src, err := Validate(d, Options{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if spec.Language != DefaultLanguage {
		t.Errorf("unknown language not coerced: %q", spec.Language)
	}
	if spec.DetailLevel != DefaultDetailLevel {
		t.Errorf("unknown detail_level not coerced: %q", spec.DetailLevel)
	}
	// Coerced fields must be downgraded to default provenance so a bad guess
	// can't masquerade as an explicit requirement.
	if src["language"] != SourceDefault || src["detail_level"] != SourceDefault {
		t.Errorf("coerced enum source not downgraded: %v", src)
	}
}

func TestValidateRejectsEmptySpec(t *testing.T) {
	if _, _, err := Validate(Draft{}, Options{}); err == nil {
		t.Fatal("empty draft (no objective/topic/channels) must be rejected")
	}
	// A channel-only spec is valid scope even without objective/topic.
	if _, _, err := Validate(Draft{Channels: []Channel{{ChannelID: "ch1"}}}, Options{}); err != nil {
		t.Fatalf("channel-only spec should be valid: %v", err)
	}
}

func TestHashOrderInsensitiveAndContentSensitive(t *testing.T) {
	a, _, _ := Validate(Draft{
		Objective:      strptr("risk"),
		OutputSections: []string{"风险", "待办"},
		Channels:       []Channel{{ChannelID: "b"}, {ChannelID: "a"}},
	}, Options{})
	b, _, _ := Validate(Draft{
		Objective:      strptr("risk"),
		OutputSections: []string{"待办", "风险"},                          // reordered
		Channels:       []Channel{{ChannelID: "a"}, {ChannelID: "b"}}, // reordered
	}, Options{})

	ha, err := a.Hash()
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := b.Hash()
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if ha != hb {
		t.Fatalf("hash not order-insensitive: %s != %s", ha, hb)
	}

	c, _, _ := Validate(Draft{Objective: strptr("different")}, Options{})
	hc, _ := c.Hash()
	if hc == ha {
		t.Fatal("different content must hash differently")
	}
}

func TestParseDraftDistinguishesAbsent(t *testing.T) {
	d, err := ParseDraft([]byte(`{"objective":"x"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Objective == nil || *d.Objective != "x" {
		t.Errorf("objective not parsed: %+v", d.Objective)
	}
	if d.Language != nil {
		t.Errorf("absent language should be nil, got %v", d.Language)
	}
}
