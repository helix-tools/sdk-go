package types

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

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

// TestCompanyStatusConstants pins the canonical 12-value CompanyStatus set:
// the 8 provisioning-lifecycle values plus the 4 self-service onboarding
// values (pending_approval, rejected, pending_offboard, offboarded) the API
// allowlist accepts and live companies carry (parity audit B-09, decision D1:
// company.schema.json absorbs customer.schema.json's enum).
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
		CompanyStatusPendingApproval:    "pending_approval",
		CompanyStatusRejected:           "rejected",
		CompanyStatusPendingOffboard:    "pending_offboard",
		CompanyStatusOffboarded:         "offboarded",
	}
	if len(cases) != 12 {
		t.Fatalf("expected exactly 12 canonical company statuses, got %d", len(cases))
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("CompanyStatus const = %q, want %q", got, want)
		}
	}

	// "cancelled" is forbidden by the API allowlist and must not be a constant.
	for got := range cases {
		if got == "cancelled" {
			t.Error("cancelled must not be a canonical company status")
		}
	}
}

// TestSubscriptionTierConstants pins SubscriptionTier to {free}, the only
// value the subscription schema allows.
func TestSubscriptionTierConstants(t *testing.T) {
	cases := map[SubscriptionTier]string{
		TierFree: "free",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("SubscriptionTier const = %q, want %q", got, want)
		}
	}

	if TierFree != "free" {
		t.Errorf("canonical write tier must be \"free\", got %q", TierFree)
	}
}

// TestCompanyTierConstants pins CompanyTier to the six company-schema values.
// TierStarter through TierEnterprise are CompanyTier values; the compile check
// below keeps them usable where a SubscriptionTier is expected.
func TestCompanyTierConstants(t *testing.T) {
	cases := map[CompanyTier]string{
		CompanyTierFree:  "free",
		TierStarter:      "starter",
		TierBasic:        "basic",
		TierPremium:      "premium",
		TierProfessional: "professional",
		TierEnterprise:   "enterprise",
	}
	if len(cases) != 6 {
		t.Fatalf("expected exactly 6 company tiers, got %d", len(cases))
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("CompanyTier const = %q, want %q", got, want)
		}
	}

	var legacy SubscriptionTier = TierStarter
	if legacy != "starter" {
		t.Errorf("TierStarter assigned to SubscriptionTier = %q, want \"starter\"", legacy)
	}
}

// canonicalStringAliases maps each named string type that mirrors a schema
// enum to the exact values its constants must declare.
var canonicalStringAliases = map[string][]string{
	"DatasetStatus": {"active", "inactive", "archived"},
	"DatasetCategory": {
		"phone-numbers", "contact-data", "business-listings", "demographic-data",
		"geographic-data", "financial-data", "telecommunications", "marketing",
		"sales", "analytics", "general", "test",
	},
	"DatasetVisibility":         {"public", "private", "restricted"},
	"AccessTier":                {"free", "premium", "enterprise"},
	"SubscriptionStatus":        {"active", "paused", "cancelled", "expired"},
	"SubscriptionTier":          {"free"},
	"SubscriptionRequestStatus": {"pending", "approved", "rejected", "approved_pending_payment"},
	"CompanyStatus": {
		"provisioning", "active", "inactive", "suspended", "provisioning_failed",
		"onboarding_failed", "deprovisioning", "decommission_failed",
		"pending_approval", "rejected", "pending_offboard", "offboarded",
	},
	"CompanyTier":            {"free", "starter", "basic", "premium", "professional", "enterprise"},
	"CustomerType":           {"producer", "consumer", "both"},
	"StripeStatus":           {"active", "past_due", "canceled", "unpaid", "trialing", "incomplete"},
	"ConsumerRelationStatus": {"provisioning", "active", "inactive"},
}

// TestCanonicalStringAliasSets pins the exact constants declared with each
// enum alias. It reads the declared type of every constant, not only its
// value: an alias is the same type as string, so a value-only check cannot
// see a constant moved onto the wrong enum.
func TestCanonicalStringAliasSets(t *testing.T) {
	declared := declaredStringConsts(t, packageSources(t))
	for alias, want := range canonicalStringAliases {
		if diff := setDiff(declared[alias], want); diff != "" {
			t.Errorf("%s: %s", alias, diff)
		}
	}
}

// TestCanonicalStringAliasSets_NegativeControl shows the pin reports each way
// a constant can land on the wrong alias. The exact-set fixture must not be
// reported, so the comparison is not simply always-failing.
func TestCanonicalStringAliasSets_NegativeControl(t *testing.T) {
	const pkg = "package types\n"
	cases := []struct {
		name    string
		sources map[string]string
		alias   string
		drifted bool
	}{
		{
			name:    "exact set is not drift",
			sources: map[string]string{"a.go": pkg + "const (\n\tTierFree SubscriptionTier = \"free\"\n)\n"},
			alias:   "SubscriptionTier",
		},
		{
			name:    "paid tier typed as SubscriptionTier",
			sources: map[string]string{"a.go": pkg + "const (\n\tTierFree SubscriptionTier = \"free\"\n\tTierStarter SubscriptionTier = \"starter\"\n)\n"},
			alias:   "SubscriptionTier",
			drifted: true,
		},
		{
			name:    "one-line const typed as alias",
			sources: map[string]string{"a.go": pkg + "const TierFree SubscriptionTier = \"free\"\nconst TierStarter SubscriptionTier = \"starter\"\n"},
			alias:   "SubscriptionTier",
			drifted: true,
		},
		{
			name: "alias constant in a sibling file",
			sources: map[string]string{
				"a.go": pkg + "const TierFree SubscriptionTier = \"free\"\n",
				"b.go": pkg + "const TierStarter SubscriptionTier = \"starter\"\n",
			},
			alias:   "SubscriptionTier",
			drifted: true,
		},
		{
			name:    "value that is not a string literal",
			sources: map[string]string{"a.go": pkg + "const starter = \"starter\"\n\nconst (\n\tTierFree SubscriptionTier = starter\n)\n"},
			alias:   "SubscriptionTier",
			drifted: true,
		},
		{
			name:    "spec that inherits the alias is counted",
			sources: map[string]string{"a.go": pkg + "const (\n\tTierFree SubscriptionTier = \"free\"\n\tTierBasic\n)\n"},
			alias:   "SubscriptionTier",
			drifted: true,
		},
		{
			name:    "value missing from CompanyTier",
			sources: map[string]string{"a.go": pkg + "const (\n\tCompanyTierFree CompanyTier = \"free\"\n\tTierStarter CompanyTier = \"starter\"\n)\n"},
			alias:   "CompanyTier",
			drifted: true,
		},
	}
	for _, tc := range cases {
		diff := setDiff(declaredStringConsts(t, tc.sources)[tc.alias], canonicalStringAliases[tc.alias])
		if (diff != "") != tc.drifted {
			t.Errorf("%s: drift reported = %v (%q), want %v", tc.name, diff != "", diff, tc.drifted)
		}
	}
}

// declaredStringConsts maps each type name to the string values of the
// constants declared with that type across the given Go sources. A spec with
// neither a type nor a value repeats the previous spec's, as Go does.
func declaredStringConsts(t *testing.T, sources map[string]string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	declared := map[string][]string{}
	for name, src := range sources {
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			var typ ast.Expr
			var vals []ast.Expr
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if len(vs.Values) > 0 {
					typ, vals = vs.Type, vs.Values
				}
				ident, ok := typ.(*ast.Ident)
				if !ok {
					continue
				}
				for i := range vs.Names {
					if i >= len(vals) {
						continue
					}
					// A value that is not a string literal is recorded under a
					// placeholder, so it can never match a canonical value.
					value := "<non-literal>"
					if lit, ok := vals[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", lit.Value, err)
						}
						value = v
					}
					declared[ident.Name] = append(declared[ident.Name], value)
				}
			}
		}
	}
	return declared
}

// canonicalFieldTypes names the declared type of each struct field that the
// schemas constrain to an enum. Changing one back to plain string fails the
// build of this test, not only the schema check.
var canonicalFieldTypes = map[string]string{
	"Dataset.Category":                "DatasetCategory",
	"Dataset.Visibility":              "DatasetVisibility",
	"Dataset.Status":                  "DatasetStatus",
	"Dataset.AccessTier":              "AccessTier",
	"Subscription.Tier":               "SubscriptionTier",
	"Subscription.Status":             "SubscriptionStatus",
	"SubscriptionRequest.Tier":        "SubscriptionTier",
	"SubscriptionRequest.Status":      "SubscriptionRequestStatus",
	"Company.CustomerType":            "CustomerType",
	"Company.Status":                  "CompanyStatus",
	"Company.Tier":                    "CompanyTier",
	"Company.StripeStatus":            "StripeStatus",
	"ProducerConsumerRelation.Status": "ConsumerRelationStatus",
	"ProducerConsumerRelation.Tier":   "CompanyTier",
}

// TestCanonicalFieldTypes pins the declared type of each enum-constrained
// struct field, so a field cannot quietly return to string.
func TestCanonicalFieldTypes(t *testing.T) {
	for _, diff := range fieldTypeDiffs(declaredFieldTypes(t, packageSources(t)), canonicalFieldTypes) {
		t.Error(diff)
	}
}

// TestCanonicalFieldTypes_NegativeControl shows the field pin reports a field
// reverted to string and a field whose type is no longer the expected alias.
func TestCanonicalFieldTypes_NegativeControl(t *testing.T) {
	const pkg = "package types\n"
	want := map[string]string{"Dataset.Visibility": "DatasetVisibility"}
	cases := []struct {
		name    string
		src     string
		drifted bool
	}{
		{"exact alias is not drift", pkg + "type Dataset struct {\n\tVisibility DatasetVisibility\n}\n", false},
		{"field reverted to string", pkg + "type Dataset struct {\n\tVisibility string\n}\n", true},
		{"field lost its pointer", pkg + "type Dataset struct {\n\tVisibility *DatasetVisibility\n}\n", true},
	}
	for _, tc := range cases {
		diffs := fieldTypeDiffs(declaredFieldTypes(t, map[string]string{"a.go": tc.src}), want)
		if (len(diffs) > 0) != tc.drifted {
			t.Errorf("%s: drift reported = %v (%q), want %v", tc.name, len(diffs) > 0, diffs, tc.drifted)
		}
	}
}

// declaredFieldTypes maps "Struct.Field" to the declared type of each field of
// each top-level struct type in the given sources, as written: an identifier,
// or "*" plus one for a pointer. Any other form is "", which matches nothing.
func declaredFieldTypes(t *testing.T, sources map[string]string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	declared := map[string]string{}
	for name, src := range sources {
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, f := range st.Fields.List {
					for _, field := range f.Names {
						declared[ts.Name.Name+"."+field.Name] = typeExprName(f.Type)
					}
				}
			}
		}
	}
	return declared
}

// typeExprName returns a field type as written: an identifier, or "*" plus an
// identifier for a pointer. Any other form returns "".
func typeExprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return "*" + id.Name
		}
	}
	return ""
}

// fieldTypeDiffs describes each field whose declared type is not the wanted one.
func fieldTypeDiffs(declared, want map[string]string) []string {
	var diffs []string
	for field, wantType := range want {
		if got := declared[field]; got != wantType {
			diffs = append(diffs, fmt.Sprintf("%s declared %q, want %q", field, got, wantType))
		}
	}
	slices.Sort(diffs)
	return diffs
}

// packageSources returns the non-test Go files of this package, by name.
func packageSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(b)
	}
	return sources
}

// setDiff describes how the declared values differ from the canonical ones,
// or returns "" when they are the same set, duplicates counted.
func setDiff(declared, canonical []string) string {
	d := slices.Clone(declared)
	c := slices.Clone(canonical)
	slices.Sort(d)
	slices.Sort(c)
	if slices.Equal(d, c) {
		return ""
	}
	return fmt.Sprintf("declared %q, want %q", d, c)
}
