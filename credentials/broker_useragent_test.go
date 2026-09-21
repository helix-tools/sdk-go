// Tests pinning that the STS credential-broker mint POST (buildRequest)
// carries the SDK-identifying User-Agent header (see internal/useragent),
// and that it is excluded from the SigV4 SignedHeaders set — mirrors
// consumer/consumer_useragent_test.go's TestMakeAPIRequest_* pair for the
// mint call, the one Helix API request this package builds outside
// makeAPIRequest.
package credentials

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/helix-tools/sdk-go/v2/internal/useragent"
)

// wireFormatRe is the exact contract the api lane parses: the FIRST
// token of User-Agent must be "helix-sdk-go/<semver-without-v>",
// optionally followed by " (go/<runtime.Version()>)".
var wireFormatRe = regexp.MustCompile(`^helix-sdk-go/\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?( \(go/go[0-9.]+(rc\d+)?\))?$`)

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

// TestProvider_BuildRequest_SetsUserAgent is the core positive case: the
// mint POST built by buildRequest (invoked via Retrieve) must carry the
// exact SDK User-Agent value.
func TestProvider_BuildRequest_SetsUserAgent(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	})

	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	req := broker.lastRequest()
	if req == nil {
		t.Fatal("broker never received a request")
	}
	got := req.Header.Get("User-Agent")
	if !wireFormatRe.MatchString(got) {
		t.Errorf("User-Agent = %q, does not match wire contract %s", got, wireFormatRe.String())
	}
	if want := useragent.String(); got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

// TestProvider_BuildRequest_UserAgentNotInSignedHeaders proves the new
// header never joins the SigV4 SignedHeaders set on the mint request —
// same rationale as consumer's analogous test: aws-sdk-go-v2's signer only
// ignores "User-Agent" by name during signing, so this pins the mint
// request's signature never depends on the (non-deterministic across Go
// versions) User-Agent string.
func TestProvider_BuildRequest_UserAgentNotInSignedHeaders(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	})

	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	req := broker.lastRequest()
	if req == nil {
		t.Fatal("broker never received a request")
	}
	if ua := req.Header.Get("User-Agent"); !wireFormatRe.MatchString(ua) {
		t.Fatalf("User-Agent = %q, does not match wire contract %s", ua, wireFormatRe.String())
	}
	signed := signedHeadersOf(t, req.Header.Get("Authorization"))
	if containsHeader(signed, "user-agent") {
		t.Errorf("SignedHeaders = %v, must NOT include user-agent", signed)
	}
}
