package summaryspec

import (
	"strings"
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

func TestBuildPromptGuidanceReflectsSpec(t *testing.T) {
	spec, _, err := Validate(Draft{
		Objective:      strptr("总结项目风险"),
		Audience:       strptr("研发负责人"),
		Language:       strptr("en"),
		DetailLevel:    strptr("detailed"),
		OutputSections: []string{"风险", "待办"},
		Exclusions:     []string{"闲聊"},
	}, Options{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	g := spec.BuildPromptGuidance()
	for _, want := range []string{"总结项目风险", "研发负责人", "en", "不要压缩", "风险、待办", "闲聊", "优先级"} {
		if !strings.Contains(g, want) {
			t.Errorf("guidance missing %q\n---\n%s", want, g)
		}
	}
}

func TestBuildPromptGuidanceEmptyForZeroSpec(t *testing.T) {
	// A fully-zero spec has nothing to instruct with → empty guidance, so the
	// caller keeps the exact legacy prompt.
	if g := (Spec{}).BuildPromptGuidance(); g != "" {
		t.Fatalf("zero spec should produce empty guidance, got %q", g)
	}
}

func TestBuildPromptGuidanceDetailLevels(t *testing.T) {
	brief, _, _ := Validate(Draft{Objective: strptr("x"), DetailLevel: strptr("brief")}, Options{})
	if !strings.Contains(brief.BuildPromptGuidance(), "精炼") {
		t.Error("brief guidance should mention 精炼")
	}
	// standard is the implicit baseline → no detail instruction line.
	std, _, _ := Validate(Draft{Objective: strptr("x")}, Options{})
	if strings.Contains(std.BuildPromptGuidance(), "详细程度") {
		t.Error("standard should not emit a detail-level line")
	}
}
