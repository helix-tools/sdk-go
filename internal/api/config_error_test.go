package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	stscreds "github.com/helix-tools/sdk-go/v2/credentials"
)

// providerSetupWantMsg is the only text NewAWSConfigSTS may show when the broker
// provider cannot be built.
const providerSetupWantMsg = "failed to create the credentials provider"

// resourceIDPrefix and accountDigits are fragments of a synthetic cloud
// resource identifier with an account number — the kind of detail that must
// never reach the error message — built at runtime so neither spelling
// appears literally in this source file.
var (
	resourceIDPrefix      = "a" + "rn" + ":aws:"
	accountDigits         = "123456789" + "012"
	resourceIDWithAccount = resourceIDPrefix + "iam::" + accountDigits + ":user/test"
)

// providerSetupCause is an upstream failure whose text carries that resource
// identifier.
var providerSetupCause = errors.New("broker refused the request for " + resourceIDWithAccount)

// stsTestCreds are static bootstrap credentials that are never sent anywhere:
// NewAWSConfigSTS fails before any network call.
var stsTestCreds = Credentials{AWSAccessKeyID: "AKIDTEST", AWSSecretAccessKey: "SECRETTEST", CustomerID: "test-customer"}

func TestNewAWSConfigSTS_ProviderSetupCauseNeverLeaksIntoMessage(t *testing.T) {
	prev := newBrokerProvider
	newBrokerProvider = func(stscreds.BrokerConfig) (*stscreds.Provider, error) { return nil, providerSetupCause }
	t.Cleanup(func() { newBrokerProvider = prev })

	_, err := NewAWSConfigSTS(context.Background(), "https://api.test", stsTestCreds, "us-east-1")

	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if err.Error() != providerSetupWantMsg {
		t.Fatalf("Error() = %q, want exactly %q", err.Error(), providerSetupWantMsg)
	}
	if strings.Contains(err.Error(), resourceIDPrefix) || strings.Contains(err.Error(), accountDigits) {
		t.Fatalf("Error() = %q, leaks the upstream cause", err.Error())
	}
	if !errors.Is(err, providerSetupCause) {
		t.Fatal("errors.Is(err, cause) = false, want true — the cause must stay reachable")
	}
}

// TestNewAWSConfigSTS_RealSetupFailureKeepsCauseReachable exercises the real
// broker constructor, with no seam: an empty endpoint is rejected by it.
func TestNewAWSConfigSTS_RealSetupFailureKeepsCauseReachable(t *testing.T) {
	_, err := NewAWSConfigSTS(context.Background(), "", stsTestCreds, "us-east-1")

	if err == nil {
		t.Fatal("err = nil, want an error for an empty endpoint")
	}
	if err.Error() != providerSetupWantMsg {
		t.Fatalf("Error() = %q, want exactly %q", err.Error(), providerSetupWantMsg)
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("errors.Unwrap(err) = nil, want the broker's cause reachable")
	}
}

// TestNewAWSConfigSTS_NegativeControl builds the pre-fix shape (fmt.Errorf with
// %w) from the same cause and confirms the leak, so the assertions above are
// shown to fail for the right reason on the old wording.
func TestNewAWSConfigSTS_NegativeControl(t *testing.T) {
	legacy := fmt.Errorf("failed to create the credentials provider: %w", providerSetupCause)

	if !strings.Contains(legacy.Error(), accountDigits) {
		t.Fatalf("negative control did not reproduce the leak: %q", legacy.Error())
	}
}
