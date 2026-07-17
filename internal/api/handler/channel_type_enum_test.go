package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// TestChannelTypeEnumBridge locks in the two-way mapping between application-
// layer OriginChannel* and WuKongIM storage-layer channel_type. Regression
// guard for SUM-158 blocker 4, where CreateAgentSummary used to persist the
// raw storage value as origin_channel_type.
func TestChannelTypeEnumBridge(t *testing.T) {
	// Forward: OriginChannel (app) → channel_type (storage).
	forward := []struct {
		app, storage int
		label        string
	}{
		{model.OriginChannelGroup, model.ChannelTypeGroup, "Group"},
		{model.OriginChannelThread, model.ChannelTypeThread, "Thread"},
		{model.OriginChannelDM, model.ChannelTypeDM, "DM"},
	}
	for _, tc := range forward {
		got := appOriginToStorageChannelType(tc.app)
		if got != tc.storage {
			t.Errorf("appOriginToStorageChannelType(%s app=%d) = %d, want storage=%d",
				tc.label, tc.app, got, tc.storage)
		}
	}
	// Global / unknown → 0 (no single channel).
	if got := appOriginToStorageChannelType(model.OriginChannelGlobal); got != 0 {
		t.Errorf("appOriginToStorageChannelType(Global) = %d, want 0", got)
	}
	if got := appOriginToStorageChannelType(999); got != 0 {
		t.Errorf("appOriginToStorageChannelType(unknown 999) = %d, want 0", got)
	}

	// Reverse: channel_type (storage) → OriginChannel (app).
	reverse := []struct {
		storage, app int
		label        string
	}{
		{model.ChannelTypeDM, model.OriginChannelDM, "DM"},
		{model.ChannelTypeGroup, model.OriginChannelGroup, "Group"},
		{model.ChannelTypeThread, model.OriginChannelThread, "Thread"},
	}
	for _, tc := range reverse {
		got, ok := storageChannelTypeToAppOrigin(tc.storage)
		if !ok || got != tc.app {
			t.Errorf("storageChannelTypeToAppOrigin(%s storage=%d) = (%d, %v), want (app=%d, true)",
				tc.label, tc.storage, got, ok, tc.app)
		}
	}
	// Unrecognized storage values (e.g. 0/3/4/99) → (0, false) so callers can
	// distinguish "not recognized" from a real OriginChannelGlobal=0.
	unrecognized := []int{0, 3, 4, 6, 99}
	for _, s := range unrecognized {
		got, ok := storageChannelTypeToAppOrigin(s)
		if ok {
			t.Errorf("storageChannelTypeToAppOrigin(unknown storage=%d) = (%d, true), want (0, false)",
				s, got)
		}
		if got != 0 {
			t.Errorf("storageChannelTypeToAppOrigin(unknown storage=%d) returned val %d, want 0", s, got)
		}
	}

	// Round-trip: app → storage → app must be identity for the three
	// recognized origin values (defends against future enum drift).
	for _, tc := range forward {
		storage := appOriginToStorageChannelType(tc.app)
		backApp, ok := storageChannelTypeToAppOrigin(storage)
		if !ok || backApp != tc.app {
			t.Errorf("round-trip %s: app=%d → storage=%d → (%d, %v), want (%d, true)",
				tc.label, tc.app, storage, backApp, ok, tc.app)
		}
	}
}
