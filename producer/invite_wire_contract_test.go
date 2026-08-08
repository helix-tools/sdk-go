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
// /v1/self/invite-consumer for three canonical scenarios. The backlog item
// this closes ("per-dataset invite tiers are expressed differently in kind
// across the three SDKs") exists because per-SDK tests each validate their
// own copy of the wire contract and cannot see divergence between them --
// a change here that isn't mirrored in the Python (helix-connect) and
// TypeScript (@helix-tools/sdk-typescript) SDKs' equivalent tests
// reintroduces exactly that gap.
//
// THE PYTHON AND TYPESCRIPT SDKs CARRY THE IDENTICAL EXPECTED BODIES BELOW
// (same keys, same values, same structure) FOR THE SAME THREE SCENARIOS.
// Any change to wantLegacyStringForm, wantObjectFormMixedTiers, or
// wantObjectFormTierOmitted MUST be mirrored in both sibling SDKs' golden
// wire-contract tests, or this cross-SDK contract silently drifts again.
//
// Scenario naming mirrors the three forms invite-consumer-request.schema.json
// allows for the "datasets" key:
//   (a) legacy string form   -- a flat array of dataset id strings
//   (b) object form          -- an array of {dataset_id, tier} objects
//   (c) object form, tier omitted on one entry -- proves the per-dataset
//       "tier" key is genuinely optional on the wire (server defaults it to
//       "free"), not just optional in the Go struct.

// wantLegacyStringForm is scenario (a): 2 dataset ids, no tiers.
const wantLegacyStringForm = `{
	"company_name": "Acme Analytics",
	"business_email": "data@acme.example",
	"datasets": ["ds-1", "ds-2"]
}`

// wantObjectFormMixedTiers is scenario (b): 2 datasets, one free, one paid.
const wantObjectFormMixedTiers = `{
	"company_name": "Acme Analytics",
	"business_email": "data@acme.example",
	"datasets": [
		{"dataset_id": "ds-free", "tier": "free"},
		{"dataset_id": "ds-paid", "tier": "paid"}
	]
}`

// wantObjectFormTierOmitted is scenario (c): object form where the second
// entry's tier is left unset client-side and must be ABSENT from the wire
// object entirely (not sent as "" or null) -- the server defaults an
// omitted per-dataset tier to "free".
const wantObjectFormTierOmitted = `{
	"company_name": "Acme Analytics",
	"business_email": "data@acme.example",
	"datasets": [
		{"dataset_id": "ds-1", "tier": "free"},
		{"dataset_id": "ds-2"}
	]
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
		Datasets:      []string{"ds-1", "ds-2"},
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
			{DatasetID: "ds-free", Tier: "free"},
			{DatasetID: "ds-paid", Tier: "paid"},
		},
	})
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	assertWireBodyEqual(t, wantObjectFormMixedTiers, *gotRaw)
}

// TestInviteWireContract_ObjectFormTierOmitted pins scenario (c): a grant
// whose Tier is left as the Go zero value ("") must produce an object
// entry with NO "tier" key at all -- not an empty string, not null.
func TestInviteWireContract_ObjectFormTierOmitted(t *testing.T) {
	p, gotRaw := captureInviteBody(t)

	_, err := p.InviteConsumer(context.Background(), types.InviteConsumerInput{
		CompanyName:   "Acme Analytics",
		BusinessEmail: "data@acme.example",
		DatasetTiers: []types.InviteConsumerDatasetGrant{
			{DatasetID: "ds-1", Tier: "free"},
			{DatasetID: "ds-2"}, // Tier deliberately unset
		},
	})
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	assertWireBodyEqual(t, wantObjectFormTierOmitted, *gotRaw)
}

// TestInviteWireContract_FreeDatasetGrantsMatchesObjectForm pins that
// types.FreeDatasetGrants produces the IDENTICAL wire body to a hand-built
// DatasetTiers slice of all-free grants -- the helper is purely a
// convenience constructor, never a different code path on the wire.
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
		"datasets": [
			{"dataset_id": "ds-1", "tier": "free"},
			{"dataset_id": "ds-2", "tier": "free"}
		]
	}`
	assertWireBodyEqual(t, want, *gotRaw)
}
