// Tests for the STS credential-mode surface (STS-PLAN.md §P3/B1). This file
// covers what is specific to Consumer's own request-signing path:
//
//  1. the Ringboost regression pin, at the wire level — a Consumer signed
//     with static (AKIA) credentials must never emit X-Amz-Security-Token;
//  2. the "session token in the signed request" requirement — a Consumer
//     signed with sts-mode (ASIA + session token) credentials MUST emit
//     X-Amz-Security-Token, AND that header must be included in the SigV4
//     SignedHeaders list (a token merely attached but unsigned is rejected
//     server-side per credential_session.schema.json's session_token
//     description);
//  3. a thin end-to-end wiring check that NewConsumer's actual call
//     (stscreds.SelectProvider(cfg.APIEndpoint, cfg)) plugs into
//     config.WithCredentialsProvider the way credentials_test.go's own
//     SelectProvider tests assume.
//
// Deep refresh-timing / single-flight / 403-surfacing / fail-closed-expiry
// coverage lives in credentials/broker_test.go, which tests Provider and
// aws.CredentialsCache directly — see STS_C0_INVENTORY.md for why the split
// is drawn there (NewConsumer/NewProducer cannot be unit-tested end-to-end
// without hitting real AWS STS — see the existing
// TestDefaultHTTPClientTimeoutIsBounded comment in consumer_test.go).
package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stscreds "github.com/helix-tools/sdk-go/v2/credentials"
	"github.com/helix-tools/sdk-go/v2/types"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// newTestConsumerWithCredentials mirrors newTestConsumer
// (download_outcome_callback_test.go) but accepts an explicit
// aws.CredentialsProvider so tests can inject either a static (no token) or
// sts-shaped (with SessionToken) fixture, bypassing NewConsumer's AWS STS
// validation call exactly like the existing helper does.
func newTestConsumerWithCredentials(endpoint string, provider aws.CredentialsProvider) *Consumer {
	return &Consumer{
		APIEndpoint: endpoint,
		CustomerID:  "test-customer",
		Region:      "us-east-1",
		awsConfig:   aws.Config{Region: "us-east-1", Credentials: provider},
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// staticCreds/stsCreds are fixed aws.CredentialsProvider values so both
// tests below sign against known, comparable inputs.
func staticCredsProvider() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     "AKIASTATICTEST12345",
			SecretAccessKey: "staticSecretAccessKeyForSigningTests1234",
			SessionToken:    "", // static path: no token, ever
			CanExpire:       false,
		}, nil
	})
}

func stsCredsProvider() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     "ASIASTSTEST1234567X",
			SecretAccessKey: "stsSecretAccessKeyForSigningTests123456",
			SessionToken:    "sts-session-token-opaque-value",
			CanExpire:       true,
			Expires:         time.Now().Add(15 * time.Minute),
		}, nil
	})
}

// signedHeadersOf extracts the SignedHeaders list from a SigV4 Authorization
// header (format: "AWS4-HMAC-SHA256 Credential=.../..., SignedHeaders=h1;h2;h3, Signature=...").
func signedHeadersOf(t *testing.T, authHeader string) []string {
	t.Helper()
	const marker = "SignedHeaders="
	i := strings.Index(authHeader, marker)
	if i < 0 {
		t.Fatalf("Authorization header %q has no SignedHeaders component", authHeader)
	}
	rest := authHeader[i+len(marker):]
	if j := strings.Index(rest, ","); j >= 0 {
		rest = rest[:j]
	}
	return strings.Split(rest, ";")
}

func containsHeader(headers []string, want string) bool {
	for _, h := range headers {
		if strings.EqualFold(h, want) {
			return true
		}
	}
	return false
}

// TestMakeAPIRequest_StaticMode_NoSecurityTokenHeader is the Ringboost
// regression pin at the wire level: a Consumer signed with static (AKIA-
// shaped) credentials must never send X-Amz-Security-Token, and the header
// must not even appear in SignedHeaders.
func TestMakeAPIRequest_StaticMode_NoSecurityTokenHeader(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"_id":"ds-1","name":"d"}`))
	}))
	defer server.Close()

	c := newTestConsumerWithCredentials(server.URL, staticCredsProvider())
	if _, err := c.GetDataset(context.Background(), "ds-1"); err != nil {
		t.Fatalf("GetDataset: %v", err)
	}

	if captured == nil {
		t.Fatal("server never received a request")
	}
	if tok := captured.Header.Get("X-Amz-Security-Token"); tok != "" {
		t.Errorf("X-Amz-Security-Token = %q, want absent for static-mode credentials", tok)
	}
	signed := signedHeadersOf(t, captured.Header.Get("Authorization"))
	if containsHeader(signed, "x-amz-security-token") {
		t.Errorf("SignedHeaders = %v, must NOT include x-amz-security-token for static-mode credentials", signed)
	}
}

// TestMakeAPIRequest_STSMode_SecurityTokenHeaderPresentAndSigned is the
// "session token in the signed request" requirement: sts-mode (ASIA +
// session token) credentials must produce a request carrying
// X-Amz-Security-Token, AND that header must be part of SignedHeaders (a
// token merely attached but unsigned is rejected server-side —
// credential_session.schema.json's session_token field description).
func TestMakeAPIRequest_STSMode_SecurityTokenHeaderPresentAndSigned(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"_id":"ds-1","name":"d"}`))
	}))
	defer server.Close()

	c := newTestConsumerWithCredentials(server.URL, stsCredsProvider())
	if _, err := c.GetDataset(context.Background(), "ds-1"); err != nil {
		t.Fatalf("GetDataset: %v", err)
	}

	if captured == nil {
		t.Fatal("server never received a request")
	}
	wantToken := "sts-session-token-opaque-value"
	if tok := captured.Header.Get("X-Amz-Security-Token"); tok != wantToken {
		t.Errorf("X-Amz-Security-Token = %q, want %q", tok, wantToken)
	}
	signed := signedHeadersOf(t, captured.Header.Get("Authorization"))
	if !containsHeader(signed, "x-amz-security-token") {
		t.Errorf("SignedHeaders = %v, want it to include x-amz-security-token (attached-but-unsigned tokens are rejected server-side)", signed)
	}
}

// TestSelectProvider_WiresIntoConsumerConfig is a thin check that the exact
// call NewConsumer makes (stscreds.SelectProvider(cfg.APIEndpoint, cfg))
// returns a provider suitable for config.WithCredentialsProvider — i.e. the
// wiring at the NewConsumer call site is dimensionally correct. Deep
// mode-inference/validation coverage lives in credentials/broker_test.go;
// this only pins that consumer.go is calling it the way that package's
// tests assume.
func TestSelectProvider_WiresIntoConsumerConfig(t *testing.T) {
	cfg := types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     "AKIAWIRINGTEST12345",
		AWSSecretAccessKey: "wiringTestSecretAccessKey123456789",
		CustomerID:         "customer-wiring-test",
		Region:             "us-east-1",
		CredentialMode:     types.CredentialModeSTS,
	}

	provider, err := stscreds.SelectProvider(cfg.APIEndpoint, cfg)
	if err != nil {
		t.Fatalf("SelectProvider: %v", err)
	}
	if _, ok := provider.(*aws.CredentialsCache); !ok {
		t.Errorf("provider type = %T, want *aws.CredentialsCache for sts mode", provider)
	}

	// A types.Config carrying only static keys (today's exact existing
	// shape, mode omitted) must still infer "static" — this is the
	// regression guard for every current caller of NewConsumer.
	staticCfg := types.Config{
		APIEndpoint:        cfg.APIEndpoint,
		AWSAccessKeyID:     cfg.AWSAccessKeyID,
		AWSSecretAccessKey: cfg.AWSSecretAccessKey,
		CustomerID:         cfg.CustomerID,
		Region:             cfg.Region,
	}
	staticProvider, err := stscreds.SelectProvider(staticCfg.APIEndpoint, staticCfg)
	if err != nil {
		t.Fatalf("SelectProvider (static): %v", err)
	}
	if _, ok := staticProvider.(*aws.CredentialsCache); ok {
		t.Error("provider for a mode-omitted, static-keys-only Config must NOT be an *aws.CredentialsCache")
	}
}
