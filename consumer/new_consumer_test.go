package consumer

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestNewConsumer_OnlyChecksCredentials drives the real NewConsumer with the
// identity service pointed at a local fake: construction makes exactly one
// call, the credential check, and looks nothing else up.
func TestNewConsumer_OnlyChecksCredentials(t *testing.T) {
	var identityCalls atomic.Int64
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identityCalls.Add(1)
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
			`<Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDTEST</UserId><Account>123456789012</Account>` +
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>r-1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	}))
	defer identity.Close()

	empty := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "")
	// Every other service is pointed at a closed port: any call beyond the
	// credential check would fail construction.
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	t.Setenv("AWS_ENDPOINT_URL_STS", identity.URL)
	t.Setenv("HELIX_API_ENDPOINT", "")

	c, err := NewConsumer(types.Config{
		AWSAccessKeyID:     "AKIDTESTCONSUMER",
		AWSSecretAccessKey: "fake-secret",
		CustomerID:         "cons-1",
	})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	if c.APIEndpoint != "https://api-go.helix.tools" || c.Region != "us-east-1" {
		t.Errorf("APIEndpoint=%q Region=%q, want the defaults", c.APIEndpoint, c.Region)
	}
	if got := identityCalls.Load(); got != 1 {
		t.Errorf("identity service called %d time(s), want exactly 1", got)
	}
}
