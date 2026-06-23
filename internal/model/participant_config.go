package model

import (
	"encoding/json"
	"time"
)

// ScheduleParticipantEntry is one collaborator inside a schedule's
// participant_config, V5-upgraded with an embedded one-time confirmation state.
//
// V5 (one-time / schedule-level confirm) replaces V4's per-round, per-task
// confirm window. A member confirms a schedule ONCE; every later scheduled run
// reads this list and skips re-confirmation. The confirm state therefore lives
// here, inside the schedule's participant_config JSON (Q1 = 方案A, zero
// migration), not on the per-round task.
type ScheduleParticipantEntry struct {
	UserID      string     `json:"user_id"`
	UserName    string     `json:"user_name,omitempty"`
	Confirmed   bool       `json:"confirmed"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

// ScheduleParticipantConfig is the V5 object form of participant_config:
// a list of collaborators each carrying its own confirm state, plus a
// schedule-level gate flag that turns true once EVERY listed member (creator included, Q2) has confirmed.
type ScheduleParticipantConfig struct {
	Participants      []ScheduleParticipantEntry `json:"participants"`
	ConfirmGatePassed bool                       `json:"confirm_gate_passed"`
}

// legacyParticipantArrayEntry matches the pre-V5 / handler write shape, where
// participant_config is a bare JSON array of {user_id,user_name} objects
// (no confirm state). Older configs may even be ["u_a","u_b"] bare strings.
type legacyParticipantArrayEntry struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

// ParseScheduleParticipantConfig normalizes ANY historical participant_config
// shape into the V5 ScheduleParticipantConfig:
//
//   - V5 object form {"participants":[{user_id,confirmed,...}],"confirm_gate_passed":bool}
//     is used as-is.
//   - legacy bare array of objects [{"user_id":...,"user_name":...}] is read with
//     confirmed=false (Q1 兼容: old configs never confirmed under V5 semantics).
//   - legacy bare array of strings ["u_a","u_b"] is read with confirmed=false.
//
// Empty/nil raw yields an empty config (no participants, gate not passed).
// This is the single source of truth so worker/service/api stay consistent.
func ParseScheduleParticipantConfig(raw JSON) ScheduleParticipantConfig {
	cfg := ScheduleParticipantConfig{Participants: []ScheduleParticipantEntry{}}
	if len(raw) == 0 {
		return cfg
	}

	// Try the V5 object form first.
	var obj ScheduleParticipantConfig
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Participants != nil {
		if obj.Participants == nil {
			obj.Participants = []ScheduleParticipantEntry{}
		}
		return obj
	}

	// Fall back to a legacy bare array of objects.
	var arrObj []legacyParticipantArrayEntry
	if err := json.Unmarshal(raw, &arrObj); err == nil {
		for _, e := range arrObj {
			if e.UserID == "" {
				continue
			}
			cfg.Participants = append(cfg.Participants, ScheduleParticipantEntry{
				UserID:    e.UserID,
				UserName:  e.UserName,
				Confirmed: false,
			})
		}
		return cfg
	}

	// Last resort: legacy bare array of strings.
	var arrStr []string
	if err := json.Unmarshal(raw, &arrStr); err == nil {
		for _, uid := range arrStr {
			if uid == "" {
				continue
			}
			cfg.Participants = append(cfg.Participants, ScheduleParticipantEntry{
				UserID:    uid,
				Confirmed: false,
			})
		}
	}
	return cfg
}

// Marshal serializes the V5 config back into participant_config JSON.
func (c ScheduleParticipantConfig) Marshal() (JSON, error) {
	if c.Participants == nil {
		c.Participants = []ScheduleParticipantEntry{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return JSON(b), nil
}

// FindParticipant returns a pointer to the entry for userID, or nil.
func (c *ScheduleParticipantConfig) FindParticipant(userID string) *ScheduleParticipantEntry {
	for i := range c.Participants {
		if c.Participants[i].UserID == userID {
			return &c.Participants[i]
		}
	}
	return nil
}

// EffectiveUserIDs returns the full confirm roster for a schedule: the creator
// (always part of the roster under V5/Q2, even when absent from participants)
// followed by every distinct configured participant, in stable order.
func (c *ScheduleParticipantConfig) EffectiveUserIDs(creatorID string) []string {
	seen := make(map[string]struct{}, len(c.Participants)+1)
	out := make([]string, 0, len(c.Participants)+1)
	add := func(uid string) {
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	add(creatorID)
	for _, p := range c.Participants {
		add(p.UserID)
	}
	return out
}

// IsConfirmed reports whether userID has confirmed this schedule. The creator is
// NOT auto-confirmed under V5/Q2: an entry must exist with Confirmed=true. The
// only exception is a creator who is not represented in the roster at all (legacy
// single-person AUTO configs handled elsewhere) — callers that need creator
// presence should ensure a creator entry exists first.
func (c *ScheduleParticipantConfig) IsConfirmed(userID string) bool {
	if e := c.FindParticipant(userID); e != nil {
		return e.Confirmed
	}
	return false
}

// RecomputeGate sets ConfirmGatePassed=true iff every member of the effective
// roster (creator + configured participants) is confirmed. An empty roster leaves the gate false.
func (c *ScheduleParticipantConfig) RecomputeGate(creatorID string) {
	roster := c.EffectiveUserIDs(creatorID)
	if len(roster) == 0 {
		c.ConfirmGatePassed = false
		return
	}
	for _, uid := range roster {
		if !c.IsConfirmed(uid) {
			c.ConfirmGatePassed = false
			return
		}
	}
	c.ConfirmGatePassed = true
}

// EnsureCreatorEntry guarantees the creator is present as a roster entry so the
// creator also gets a confirm toggle (Q2: creator must confirm too, no longer
// auto-accepted). It does NOT mark the creator confirmed. Returns true if an entry was added.
func (c *ScheduleParticipantConfig) EnsureCreatorEntry(creatorID string) bool {
	if creatorID == "" {
		return false
	}
	if c.FindParticipant(creatorID) != nil {
		return false
	}
	c.Participants = append(c.Participants, ScheduleParticipantEntry{
		UserID:    creatorID,
		Confirmed: false,
	})
	return true
}
