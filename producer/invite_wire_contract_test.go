package producer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// This file pins the EXACT JSON body this SDK POSTs to
// /v1/self/invite-consumer for four canonical scenarios. The backlog item
// this closes ("per-dataset invite tiers are expressed differently in kind
// across the three SDKs") exists because per-SDK tests each validate their
// own copy of the wire contract and cannot see divergence between them --
// a change here that isn't mirrored in the Python (helix-connect) and
// TypeScript (@helix-tools/sdk-typescript) SDKs' equivalent tests
// reintroduces exactly that gap.
//
// THE PYTHON AND TYPESCRIPT SDKs PIN THESE SAME FOUR BODIES VERBATIM --
// same company_name, business_email, contact_name, tier, and dataset ids,
// not just the same keys/structure. A reviewer must be able to open all
// three SDKs' golden wire-contract test files side by side and see
// byte-identical JSON for each of the four scenarios below. ANY CHANGE TO
// wantLegacyStringForm, wantObjectFormMixedTiers, wantObjectFormTierOmitted,
// or wantLegacyFormWithContactName MUST be mirrored in both sibling SDKs'
// golden wire-contract tests IN THE SAME CHANGE-SET, or this cross-SDK
// contract silently drifts again -- exactly the blind spot this item was
// filed about.
//
// All three SDKs ALWAYS send the top-level "tier" key as "free" when the
// caller leaves it unset -- Python defaults the `tier` parameter to
// `"free"`, TypeScript defaults it with `tier ?? 'free'` (whose own comment
// states the intent verbatim: "Always send tier for cross-SDK wire
// parity"), and Go now defaults it the same way in InviteConsumer before
// marshalling. This is the top-level invite-wide tier, distinct from the
// PER-DATASET "tier" key nested inside each object-form grant in scenarios
// (b)/(c) below, which remains genuinely optional on the wire (the server
// defaults an omitted per-dataset tier to "free" itself).
//
// Scenario naming mirrors the three forms invite-consumer-request.schema.json
// allows for the "datasets" key, plus a fourth pinning an optional field:
//   (a) legacy string form   -- a flat array of dataset id strings
//   (b) object form          -- an array of {dataset_id, tier} objects
//   (c) object form, tier omitted on the FIRST entry only -- proves the
//       PER-DATASET "tier" key is genuinely optional on the wire (server
//       defaults it to "free"), not just optional in the Go struct
//   (d) legacy string form with contact_name supplied -- pins the optional
//       ContactName field's presence on the wire, which (a)-(c) don't cover

// wantLegacyStringForm is scenario (a): 2 dataset ids, no tiers.
const wantLegacyStringForm = `{
	"company_name": "Acme Analytics",
	"business_email": "data@acme.example",
	"tier": "free",
	"datasets": ["dataset-123", "dataset-456"]
}`

// wantObjectFormMixedTiers is scenario (b): 2 datasets, one free, one paid.
const wantObjectFormMixedTiers = `{
	"company_name": "Acme Analytics",
	"business_email": "data@acme.example",
	"tier": "free",
	"datasets": [
		{"dataset_id": "dataset-123", "tier": "free"},
		{"dataset_id": "dataset-456", "tier": "paid"}
	]
}`

// wantObjectFormTierOmitted is scenario (c): object form where the FIRST
// entry's PER-DATASET tier is left unset client-side and must be ABSENT
// from that wire object entirely (not sent as "" or null) -- the server
// defaults an omitted per-dataset tier to "free". The top-level "tier" is
// still always present, defaulted to "free" like every other scenario.
const wantObjectFormTierOmitted = `{
	"company_name": "Acme Analytics",
	"business_email": "data@acme.example",
	"tier": "free",
	"datasets": [
		{"dataset_id": "dataset-123"},
		{"dataset_id": "dataset-456", "tier": "paid"}
	]
}`

// wantLegacyFormWithContactName is scenario (d): the legacy string form
// with an optional ContactName supplied, pinning that contact_name appears
// on the wire alongside the always-present tier.
const wantLegacyFormWithContactName = `{
	"company_name": "Acme Analytics",
	"business_email": "data@acme.example",
	"contact_name": "Dana Ruiz",
	"tier": "free",
	"datasets": ["dataset-123"]
}`

// captureInviteBody starts a test server that captures the raw POST body of
// exactly one /v1/self/invite-consumer request and returns a *Producer
// wired to it plus a pointer to the captured bytes.
func captureInviteBody(t *testing.T) (*Producer, *[]byte) {
	t.Helper()
	var gotRaw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		gotRaw = raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"consumer_id":"c-1","status":"provisioning","invited_by":"p","company_name":"Acme Analytics","message":"ok","email_sent":true}`))
	}))
	t.Cleanup(server.Close)
	return newTestProducer(server.URL), &gotRaw
}

// assertWireBodyEqual parses both the actual raw request body and the
// expected golden JSON as generic maps and compares them with
// reflect.DeepEqual, so the assertion is on exact keys and values, never on
// key order or byte-for-byte formatting.
func assertWireBodyEqual(t *testing.T, wantJSON string, gotRaw []byte) {
	t.Helper()

	var want map[string]any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("golden JSON itself failed to parse (bad test fixture): %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatalf("actual wire body failed to parse as JSON: %v\nraw: %s", err, gotRaw)
	}

	if !reflect.DeepEqual(got, want) {
		wantPretty, _ := json.MarshalIndent(want, "", "  ")
		gotPretty, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("wire body mismatch.\n--- want ---\n%s\n--- got ---\n%s", wantPretty, gotPretty)
	}
}

// TestInviteWireContract_LegacyStringForm pins scenario (a): the legacy
// []string Datasets field must marshal to a flat array of id strings under
// "datasets", with no other dataset-shaped key on the wire.
func TestInviteWireContract_LegacyStringForm(t *testing.T) {
	p, gotRaw := captureInviteBody(t)

	_, err := p.InviteConsumer(context.Background(), types.InviteConsumerInput{
		CompanyName:   "Acme Analytics",
		BusinessEmail: "data@acme.example",
		Datasets:      []string{"dataset-123", "dataset-456"},
	})
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	assertWireBodyEqual(t, wantLegacyStringForm, *gotRaw)
}

// TestInviteWireContract_ObjectFormMixedTiers pins scenario (b): the
// DatasetTiers field must marshal to an array of {dataset_id, tier}
// objects under the SAME "datasets" key (never a separate "dataset_tiers"
// key), with one free and one paid grant.
func TestInviteWireContract_ObjectFormMixedTiers(t *testing.T) {
	p, gotRaw := captureInviteBody(t)

	_, err := p.InviteConsumer(context.Background(), types.InviteConsumerInput{
		CompanyName:   "Acme Analytics",
		BusinessEmail: "data@acme.example",
		DatasetTiers: []types.InviteConsumerDatasetGrant{
			{DatasetID: "dataset-123", Tier: "free"},
			{DatasetID: "dataset-456", Tier: "paid"},
		},
	})
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	assertWireBodyEqual(t, wantObjectFormMixedTiers, *gotRaw)
}

// TestInviteWireContract_ObjectFormTierOmitted pins scenario (c): the FIRST
// grant's Tier is left as the Go zero value ("") and must produce an
// object entry with NO "tier" key at all -- not an empty string, not null.
// The second grant carries an explicit "paid" tier.
func TestInviteWireContract_ObjectFormTierOmitted(t *testing.T) {
	p, gotRaw := captureInviteBody(t)

	_, err := p.InviteConsumer(context.Background(), types.InviteConsumerInput{
		CompanyName:   "Acme Analytics",
		BusinessEmail: "data@acme.example",
		DatasetTiers: []types.InviteConsumerDatasetGrant{
			{DatasetID: "dataset-123"}, // Tier deliberately unset
			{DatasetID: "dataset-456", Tier: "paid"},
		},
	})
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	assertWireBodyEqual(t, wantObjectFormTierOmitted, *gotRaw)
}

// TestInviteWireContract_LegacyFormWithContactName pins scenario (d): the
// legacy string form with an optional ContactName supplied. No prior
// scenario covers ContactName's presence on the wire.
func TestInviteWireContract_LegacyFormWithContactName(t *testing.T) {
	p, gotRaw := captureInviteBody(t)

	_, err := p.InviteConsumer(context.Background(), types.InviteConsumerInput{
		CompanyName:   "Acme Analytics",
		BusinessEmail: "data@acme.example",
		ContactName:   "Dana Ruiz",
		Datasets:      []string{"dataset-123"},
	})
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	assertWireBodyEqual(t, wantLegacyFormWithContactName, *gotRaw)
}

// TestInviteWireContract_FreeDatasetGrantsMatchesObjectForm pins that
// types.FreeDatasetGrants produces the IDENTICAL wire body to a hand-built
// DatasetTiers slice of all-free grants -- the helper is purely a
// convenience constructor, never a different code path on the wire. This
// is additional Go-only coverage beyond the four cross-SDK canonical
// scenarios above (FreeDatasetGrants has no TypeScript/Python equivalent
// to diff against, since Go is the only SDK needing a union-avoidance
// helper).
func TestInviteWireContract_FreeDatasetGrantsMatchesObjectForm(t *testing.T) {
	p, gotRaw := captureInviteBody(t)

	_, err := p.InviteConsumer(context.Background(), types.InviteConsumerInput{
		CompanyName:   "Acme Analytics",
		BusinessEmail: "data@acme.example",
		DatasetTiers:  types.FreeDatasetGrants("ds-1", "ds-2"),
	})
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	want := `{
		"company_name": "Acme Analytics",
		"business_email": "data@acme.example",
		"tier": "free",
		"datasets": [
			{"dataset_id": "ds-1", "tier": "free"},
			{"dataset_id": "ds-2", "tier": "free"}
		]
	}`
	assertWireBodyEqual(t, want, *gotRaw)
}
