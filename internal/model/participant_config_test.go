package model

import (
	"testing"
	"time"
)

// V5 §3.1 方案A: ParseScheduleParticipantConfig must normalize every historical
// participant_config shape into the V5 object form.

func TestParseScheduleParticipantConfig_LegacyObjectArray(t *testing.T) {
	raw := JSON(`[{"user_id":"u_a","user_name":"A"},{"user_id":"u_b"}]`)
	cfg := ParseScheduleParticipantConfig(raw)
	if len(cfg.Participants) != 2 {
		t.Fatalf("want 2 participants, got %d", len(cfg.Participants))
	}
	for _, p := range cfg.Participants {
		if p.Confirmed {
			t.Errorf("legacy array entry %s must normalize to confirmed=false", p.UserID)
		}
	}
	if cfg.ConfirmGatePassed {
		t.Errorf("legacy config gate must be false")
	}
}

func TestParseScheduleParticipantConfig_LegacyStringArray(t *testing.T) {
	raw := JSON(`["u_a","u_b"]`)
	cfg := ParseScheduleParticipantConfig(raw)
	if len(cfg.Participants) != 2 {
		t.Fatalf("want 2 participants from string array, got %d", len(cfg.Participants))
	}
	if cfg.Participants[0].UserID != "u_a" || cfg.Participants[0].Confirmed {
		t.Errorf("string-array normalization wrong: %+v", cfg.Participants[0])
	}
}

func TestParseScheduleParticipantConfig_V5ObjectForm(t *testing.T) {
	now := time.Now().UTC()
	src := ScheduleParticipantConfig{
		Participants: []ScheduleParticipantEntry{
			{UserID: "u_a", Confirmed: true, ConfirmedAt: &now},
			{UserID: "u_b", Confirmed: false},
		},
		ConfirmGatePassed: false,
	}
	raw, err := src.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cfg := ParseScheduleParticipantConfig(raw)
	if len(cfg.Participants) != 2 {
		t.Fatalf("want 2, got %d", len(cfg.Participants))
	}
	if !cfg.IsConfirmed("u_a") || cfg.IsConfirmed("u_b") {
		t.Errorf("V5 confirm state not round-tripped: %+v", cfg.Participants)
	}
}

func TestParseScheduleParticipantConfig_Empty(t *testing.T) {
	cfg := ParseScheduleParticipantConfig(nil)
	if len(cfg.Participants) != 0 || cfg.ConfirmGatePassed {
		t.Errorf("empty config must be empty, got %+v", cfg)
	}
}

// RecomputeGate passes iff every roster member (creator included, Q2) is confirmed.
func TestRecomputeGate_CreatorIncluded(t *testing.T) {
	cfg := ScheduleParticipantConfig{Participants: []ScheduleParticipantEntry{
		{UserID: "u1", Confirmed: true}, // creator
		{UserID: "u2", Confirmed: true},
	}}
	cfg.RecomputeGate("u1")
	if !cfg.ConfirmGatePassed {
		t.Errorf("all confirmed (creator included) => gate must pass")
	}

	// Un-confirm the creator => gate must drop (Q2: creator also must confirm).
	cfg.Participants[0].Confirmed = false
	cfg.RecomputeGate("u1")
	if cfg.ConfirmGatePassed {
		t.Errorf("unconfirmed creator must keep gate false (Q2)")
	}
}

func TestEnsureCreatorEntry_AddsUnconfirmedCreator(t *testing.T) {
	cfg := ScheduleParticipantConfig{Participants: []ScheduleParticipantEntry{
		{UserID: "u2", Confirmed: true},
	}}
	added := cfg.EnsureCreatorEntry("u1")
	if !added {
		t.Fatalf("creator entry should have been added")
	}
	e := cfg.FindParticipant("u1")
	if e == nil || e.Confirmed {
		t.Errorf("creator must be added as confirmed=false, got %+v", e)
	}
	// Idempotent.
	if cfg.EnsureCreatorEntry("u1") {
		t.Errorf("EnsureCreatorEntry must be idempotent")
	}
}

func TestEffectiveUserIDs_CreatorFirstDedup(t *testing.T) {
	cfg := ScheduleParticipantConfig{Participants: []ScheduleParticipantEntry{
		{UserID: "u2"}, {UserID: "u1"}, {UserID: "u2"},
	}}
	got := cfg.EffectiveUserIDs("u1")
	want := []string{"u1", "u2"}
	if len(got) != len(want) || got[0] != "u1" || got[1] != "u2" {
		t.Errorf("EffectiveUserIDs wrong: got %v want %v", got, want)
	}
}
