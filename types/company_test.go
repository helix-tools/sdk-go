package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// D7 / D-08: the onboarding credential link is credential_url (the API model
// and live data), not credentials_portal_url. Its expiry is
// credential_url_expires_at.
func TestOnboardingInfo_CredentialURLWireKeys(t *testing.T) {
	var c Company
	body := `{"_id":"customer-1","onboarding":{"credential_url":"https://portal.example/c/abc","credential_url_expires_at":"2026-09-30T00:00:00Z","onboarding_source":"api"}}`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Onboarding == nil {
		t.Fatal("Onboarding = nil")
	}
	if c.Onboarding.CredentialsPortalURL != "https://portal.example/c/abc" {
		t.Errorf("CredentialsPortalURL = %q, want the credential_url value", c.Onboarding.CredentialsPortalURL)
	}
	if c.Onboarding.CredentialsPortalExpires == nil || *c.Onboarding.CredentialsPortalExpires != "2026-09-30T00:00:00Z" {
		t.Errorf("CredentialsPortalExpires = %v, want the credential_url_expires_at value", c.Onboarding.CredentialsPortalExpires)
	}

	out, err := json.Marshal(c.Onboarding)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"credential_url":`) || strings.Contains(string(out), "credentials_portal") {
		t.Errorf("re-encoded onboarding = %s, want credential_url and no credentials_portal_* keys", out)
	}
}
