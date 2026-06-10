package types

import "testing"

// TestSubscriptionStatusConstants pins the canonical SubscriptionStatus set
// to exactly {active, paused, cancelled, expired}. The audit (P3 #3) found
// the stale comment listed "suspended", which is NOT a valid subscription
// status. If a constant's value drifts from the canonical contract, this
// fails for the right reason.
func TestSubscriptionStatusConstants(t *testing.T) {
	cases := map[SubscriptionStatus]string{
		SubscriptionStatusActive:    "active",
		SubscriptionStatusPaused:    "paused",
		SubscriptionStatusCancelled: "cancelled",
		SubscriptionStatusExpired:   "expired",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("SubscriptionStatus const = %q, want %q", got, want)
		}
	}

	// Negative control: "suspended" and "inactive" must NOT be among the
	// canonical subscription statuses — they were the stale comment values.
	canonical := map[string]bool{
		SubscriptionStatusActive:    true,
		SubscriptionStatusPaused:    true,
		SubscriptionStatusCancelled: true,
		SubscriptionStatusExpired:   true,
	}
	for _, forbidden := range []string{"suspended", "inactive"} {
		if canonical[forbidden] {
			t.Errorf("%q must not be a canonical subscription status", forbidden)
		}
	}
}

// TestDatasetStatusConstants pins DatasetStatus to {active, inactive,
// archived}.
func TestDatasetStatusConstants(t *testing.T) {
	cases := map[DatasetStatus]string{
		DatasetStatusActive:   "active",
		DatasetStatusInactive: "inactive",
		DatasetStatusArchived: "archived",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("DatasetStatus const = %q, want %q", got, want)
		}
	}
}

// TestSubscriptionRequestStatusConstants pins the canonical request status
// set to {pending, approved, rejected}.
func TestSubscriptionRequestStatusConstants(t *testing.T) {
	cases := map[SubscriptionRequestStatus]string{
		SubscriptionRequestStatusPending:  "pending",
		SubscriptionRequestStatusApproved: "approved",
		SubscriptionRequestStatusRejected: "rejected",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("SubscriptionRequestStatus const = %q, want %q", got, want)
		}
	}
}

// TestCompanyStatusConstants pins the canonical 8-value CompanyStatus set,
// matching the Go API source of truth (audit P3 #5).
func TestCompanyStatusConstants(t *testing.T) {
	cases := map[CompanyStatus]string{
		CompanyStatusProvisioning:       "provisioning",
		CompanyStatusActive:             "active",
		CompanyStatusInactive:           "inactive",
		CompanyStatusSuspended:          "suspended",
		CompanyStatusProvisioningFailed: "provisioning_failed",
		CompanyStatusOnboardingFailed:   "onboarding_failed",
		CompanyStatusDeprovisioning:     "deprovisioning",
		CompanyStatusDecommissionFailed: "decommission_failed",
	}
	if len(cases) != 8 {
		t.Fatalf("expected exactly 8 canonical company statuses, got %d", len(cases))
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("CompanyStatus const = %q, want %q", got, want)
		}
	}
}

// TestTierConstants pins the read-tolerant tier vocabulary and the single
// canonical write value (free). Paid tiers are accepted on read for
// backward compatibility.
func TestTierConstants(t *testing.T) {
	cases := map[SubscriptionTier]string{
		TierFree:         "free",
		TierStarter:      "starter",
		TierBasic:        "basic",
		TierPremium:      "premium",
		TierProfessional: "professional",
		TierEnterprise:   "enterprise",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("tier const = %q, want %q", got, want)
		}
	}

	// The canonical write value is "free".
	if TierFree != "free" {
		t.Errorf("canonical write tier must be \"free\", got %q", TierFree)
	}
}
