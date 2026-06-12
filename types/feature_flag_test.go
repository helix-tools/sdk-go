package types

import (
	"encoding/json"
	"testing"
)

// TestFeatureFlagJSONTags pins the wire field names of FeatureFlag to
// feature_flag.schema.json (sdk-schemas v1.0.0). If a json tag drifts from the
// snake_case schema key, the round-trip below fails for the right reason.
func TestFeatureFlagJSONTags(t *testing.T) {
	raw := `{
		"enabled": true,
		"since": "2026-03-24T05:18:00Z",
		"enabled_by": "thalesfsp",
		"disabled_by": "admin@helix.tools",
		"disabled_at": "2026-04-01T00:00:00Z",
		"reason": "beta access",
		"expires_at": "2026-12-31T23:59:59Z"
	}`

	var ff FeatureFlag
	if err := json.Unmarshal([]byte(raw), &ff); err != nil {
		t.Fatalf("unmarshal FeatureFlag: %v", err)
	}

	if !ff.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if ff.Since != "2026-03-24T05:18:00Z" {
		t.Errorf("Since = %q, want the schema since value", ff.Since)
	}
	if ff.EnabledBy != "thalesfsp" {
		t.Errorf("EnabledBy = %q, want %q", ff.EnabledBy, "thalesfsp")
	}
	if ff.DisabledBy != "admin@helix.tools" {
		t.Errorf("DisabledBy = %q, want %q", ff.DisabledBy, "admin@helix.tools")
	}
	if ff.DisabledAt != "2026-04-01T00:00:00Z" {
		t.Errorf("DisabledAt = %q, want the schema disabled_at value", ff.DisabledAt)
	}
	if ff.Reason != "beta access" {
		t.Errorf("Reason = %q, want %q", ff.Reason, "beta access")
	}
	if ff.ExpiresAt != "2026-12-31T23:59:59Z" {
		t.Errorf("ExpiresAt = %q, want the schema expires_at value", ff.ExpiresAt)
	}
}

// TestCompanyFeatureFlagsRoundTrip verifies the feature_flags map attaches to
// Company under the snake_case key and survives a marshal/unmarshal round trip
// keyed by feature name (e.g. "partner_invite").
func TestCompanyFeatureFlagsRoundTrip(t *testing.T) {
	in := Company{
		ID:           "company-1760724651304-ringboost",
		CompanyName:  "Ringboost",
		CustomerType: "both",
		Status:       CompanyStatusActive,
		FeatureFlags: FeatureFlags{
			"partner_invite": {Enabled: true, Since: "2026-03-24T05:18:00Z", EnabledBy: "thalesfsp"},
		},
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal Company: %v", err)
	}

	// The map must serialize under the schema key "feature_flags".
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal to generic map: %v", err)
	}
	if _, ok := generic["feature_flags"]; !ok {
		t.Fatalf("serialized Company is missing the \"feature_flags\" key: %s", b)
	}

	var out Company
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal Company: %v", err)
	}
	pi, ok := out.FeatureFlags["partner_invite"]
	if !ok {
		t.Fatalf("partner_invite flag missing after round trip")
	}
	if !pi.Enabled || pi.EnabledBy != "thalesfsp" {
		t.Errorf("partner_invite round trip = %+v, want enabled by thalesfsp", pi)
	}
}

// TestCompanyFeatureFlagsOmittedWhenNil confirms a nil feature_flags map is
// omitted from the wire form (omitempty), matching "missing field = default
// deny" in the schema.
func TestCompanyFeatureFlagsOmittedWhenNil(t *testing.T) {
	b, err := json.Marshal(Company{ID: "company-1-x", CompanyName: "X", CustomerType: "consumer", Status: CompanyStatusActive})
	if err != nil {
		t.Fatalf("marshal Company: %v", err)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal to generic map: %v", err)
	}
	if _, ok := generic["feature_flags"]; ok {
		t.Errorf("nil FeatureFlags should be omitted, but key is present: %s", b)
	}
}
