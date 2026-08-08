package types

import "testing"

// TestFreeDatasetGrants_ConvertsIDsAtFreeTier pins the core behavior: each
// input id becomes one grant with Tier "free", in the same order, so a
// caller can always populate InviteConsumerInput.DatasetTiers instead of
// choosing between it and the legacy Datasets field.
func TestFreeDatasetGrants_ConvertsIDsAtFreeTier(t *testing.T) {
	got := FreeDatasetGrants("ds-1", "ds-2", "ds-3")

	want := []InviteConsumerDatasetGrant{
		{DatasetID: "ds-1", Tier: "free"},
		{DatasetID: "ds-2", Tier: "free"},
		{DatasetID: "ds-3", Tier: "free"},
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d grants, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("grant[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestFreeDatasetGrants_EmptyInput pins that no ids produces an empty
// (nil-or-zero-length) slice, not a slice of one zero-value grant — a
// caller who forgot to collect any dataset ids should get a value that
// itself fails InviteConsumer's "must contain at least 1 dataset"
// validation, not a silently-invalid single blank-id grant.
func TestFreeDatasetGrants_EmptyInput(t *testing.T) {
	got := FreeDatasetGrants()
	if len(got) != 0 {
		t.Fatalf("expected 0 grants for no ids, got %d: %+v", len(got), got)
	}
}

// TestFreeDatasetGrants_DuplicatesPassThrough pins that the helper does NOT
// silently deduplicate — it maps ids 1:1 to grants and leaves duplicate
// detection to validateInviteConsumerInput (producer/partner_invite.go),
// exactly as it would for a hand-built []InviteConsumerDatasetGrant. Two
// different behaviors here (silent dedup vs pass-through) would make the
// helper's output diverge from what a caller building the slice by hand
// would get for the same duplicate input.
func TestFreeDatasetGrants_DuplicatesPassThrough(t *testing.T) {
	got := FreeDatasetGrants("ds-1", "ds-1")

	want := []InviteConsumerDatasetGrant{
		{DatasetID: "ds-1", Tier: "free"},
		{DatasetID: "ds-1", Tier: "free"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d grants (no dedup), got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("grant[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
