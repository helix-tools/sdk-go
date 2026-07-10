// Tests for the STS credential-mode surface (STS-PLAN.md §P3/B1). Mirrors
// consumer/consumer_sts_test.go — see that file's doc comment for the
// rationale behind what's tested here vs. in credentials/broker_test.go.
package producer

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

// newTestProducerWithCredentials mirrors newTestProducer
// (update_dataset_parity_test.go) but accepts an explicit
// aws.CredentialsProvider so tests can inject either a static (no token) or
// sts-shaped (with SessionToken) fixture, bypassing NewProducer's AWS STS +
// SSM calls exactly like the existing helper does.
func newTestProducerWithCredentials(endpoint string, provider aws.CredentialsProvider) *Producer {
	return &Producer{
		APIEndpoint: endpoint,
		BucketName:  "dme-producer-test",
		CustomerID:  "test-producer",
		Region:      "us-east-1",
		awsConfig:   aws.Config{Region: "us-east-1", Credentials: provider},
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

func staticCredsProviderForTests() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     "AKIASTATICTEST12345",
			SecretAccessKey: "staticSecretAccessKeyForSigningTests1234",
			SessionToken:    "",
			CanExpire:       false,
		}, nil
	})
}

func stsCredsProviderForTests() aws.CredentialsProvider {
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

func signedHeadersOfProducer(t *testing.T, authHeader string) []string {
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

func containsHeaderProducer(headers []string, want string) bool {
	for _, h := range headers {
		if strings.EqualFold(h, want) {
			return true
		}
	}
	return false
}

// TestMakeAPIRequest_StaticMode_NoSecurityTokenHeader is the Ringboost
// regression pin at the wire level for the producer's signing path.
func TestMakeAPIRequest_StaticMode_NoSecurityTokenHeader(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	p := newTestProducerWithCredentials(server.URL, staticCredsProviderForTests())
	if _, err := p.ListMyDatasets(context.Background()); err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}

	if captured == nil {
		t.Fatal("server never received a request")
	}
	if tok := captured.Header.Get("X-Amz-Security-Token"); tok != "" {
		t.Errorf("X-Amz-Security-Token = %q, want absent for static-mode credentials", tok)
	}
	signed := signedHeadersOfProducer(t, captured.Header.Get("Authorization"))
	if containsHeaderProducer(signed, "x-amz-security-token") {
		t.Errorf("SignedHeaders = %v, must NOT include x-amz-security-token for static-mode credentials", signed)
	}
}

// TestMakeAPIRequest_STSMode_SecurityTokenHeaderPresentAndSigned is the
// "session token in the signed request" requirement for the producer's
// signing path: the token must be present AND part of SignedHeaders.
func TestMakeAPIRequest_STSMode_SecurityTokenHeaderPresentAndSigned(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	p := newTestProducerWithCredentials(server.URL, stsCredsProviderForTests())
	if _, err := p.ListMyDatasets(context.Background()); err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}

	if captured == nil {
		t.Fatal("server never received a request")
	}
	wantToken := "sts-session-token-opaque-value"
	if tok := captured.Header.Get("X-Amz-Security-Token"); tok != wantToken {
		t.Errorf("X-Amz-Security-Token = %q, want %q", tok, wantToken)
	}
	signed := signedHeadersOfProducer(t, captured.Header.Get("Authorization"))
	if !containsHeaderProducer(signed, "x-amz-security-token") {
		t.Errorf("SignedHeaders = %v, want it to include x-amz-security-token (attached-but-unsigned tokens are rejected server-side)", signed)
	}
}

// TestSelectProvider_WiresIntoProducerConfig is a thin check that the exact
// call NewProducer makes (stscreds.SelectProvider(cfg.APIEndpoint, cfg))
// returns a provider suitable for config.WithCredentialsProvider. Deep
// mode-inference/validation coverage lives in credentials/broker_test.go.
func TestSelectProvider_WiresIntoProducerConfig(t *testing.T) {
	cfg := types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     "AKIAWIRINGTEST12345",
		AWSSecretAccessKey: "wiringTestSecretAccessKey123456789",
		CustomerID:         "producer-wiring-test",
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
