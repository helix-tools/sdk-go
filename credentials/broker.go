// Package credentials implements an aws.CredentialsProvider that mints
// short-lived AWS STS session credentials from the Helix Connect credential
// broker (POST /v1/credentials/session), per credential_session.schema.json
// (sdk-schemas #17, contract frozen in sdk-schemas PR #17) and STS-PLAN.md
// §P3 (B1).
//
// Two ways to consume it:
//
//   - Production / opt-in customers: SelectProvider(apiEndpoint, cfg) picks
//     the right aws.CredentialsProvider for a types.Config — static (default,
//     byte-identical to the SDK's pre-STS behavior) or sts (this package's
//     auto-refreshing broker-backed provider, wrapped in aws.CredentialsCache).
//     consumer.NewConsumer and producer.NewProducer call this internally.
//
//   - Tests that need to disable auto-refresh or force a refresh (e2e
//     requirement E.8.2's "auto_refresh=False" / "force_refresh()" hooks,
//     translated to Go idiom): use Provider directly (each Retrieve call
//     mints fresh, no caching — freeze the one result you want with
//     awssdk-go-v2's credentials.NewStaticCredentialsProvider to simulate
//     "auto_refresh=False"), or hold onto the *aws.CredentialsCache returned
//     by NewCredentialsCache and call its exported Invalidate() method to
//     force the next Retrieve to re-mint ("force_refresh()").
//
// Bootstrap authentication: mint requests are SigV4-signed with the caller's
// existing static AWS key (AWSAccessKeyID/AWSSecretAccessKey) — the ratified
// B0/B1 bootstrap mechanism (STS-PLAN.md §9 decision #1), verified directly
// against the real broker implementation (helix-tools/api PR #129), whose
// route chain accepts only AuthMethodSigV4 (RequireMachineAuth) — there is no
// bearer/API-key auth path yet. See STS_C0_INVENTORY.md at the repo root for
// the full bind-site inventory this package's wiring is derived from.
package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/helix-tools/sdk-go/v2/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	// MintPath is the credential-broker endpoint path.
	MintPath = "/v1/credentials/session"

	// sessionTTLSeconds mirrors credential_session.schema.json's
	// ttl_seconds ("const": 900) and helix-tools/api PR #129's
	// SessionResponse — the STS DurationSeconds floor and the ratified
	// 15-minute TTL (STS-PLAN.md §9 decision #2) coincide, so every
	// successful mint is exactly this many seconds. Used only to derive
	// proactiveExpiryWindow below; actual credential expiry is always
	// computed from the server's returned ttl_seconds/expiration, never
	// hardcoded (see Provider.Retrieve).
	sessionTTLSeconds = 900

	// proactiveExpiryWindow is passed to aws.CredentialsCache as
	// ExpiryWindow: 1/3 of the 900s TTL (300s = 5min), matching the
	// "refresh at ~2/3 of the remaining lifetime" note in
	// credential_session.schema.json's expiration field and the "proactive
	// refresh when remaining <= 1/3 TTL (5 min at TTL 15m)" policy in
	// STS-PLAN.md/C-sdk.md C.1. aws.CredentialsCache (see
	// credential_cache.go in aws-sdk-go-v2) has a SINGLE refresh
	// threshold — unlike botocore's two-tier advisory/mandatory model,
	// every Retrieve() call once inside this window synchronously
	// (single-flight-protected) re-mints. That collapses the "<=5min
	// proactive" and the stricter "<=2min blocking" policy bullets into
	// one mechanism: nothing within 5 minutes of expiry is ever served
	// un-refreshed, which is a strictly safer behavior than a two-tier
	// scheme, not a gap.
	proactiveExpiryWindow = sessionTTLSeconds / 3 * time.Second

	// expiryWindowJitterFrac spreads concurrent SDK instances' refresh
	// attempts across a ~2.5-5 minute pre-expiry band instead of all
	// refreshing at exactly T-5:00 (STS-PLAN.md/C-sdk.md C.1 bullet 1:
	// "Go: ... ExpiryWindowJitterFrac = 0.5").
	expiryWindowJitterFrac = 0.5

	// mintMaxAttempts caps mint attempts at 1 initial + 2 retries,
	// matching C-sdk.md C.1 bullet 3 ("mint retry 2x with exponential
	// backoff + jitter, then raise ... AuthenticationError").
	mintMaxAttempts = 3

	// mintRetryBaseDelay is the base for exponential backoff between mint
	// retries (base * 2^(attempt-1), +/-25% jitter). Kept short because
	// the broker's own rate limit is generous relative to SDK-side
	// refresh cadence (helix-tools/api PR #129:
	// CustomerBasedRateLimit(0.33 req/s, burst 5) per customer) and mint
	// retries are already gated by mintMaxAttempts.
	mintRetryBaseDelay = 200 * time.Millisecond

	// defaultMintTimeout bounds a single mint HTTP round-trip.
	defaultMintTimeout = 10 * time.Second

	// mintService is the SigV4 service name used to sign mint requests —
	// the same "execute-api" service every other authenticated call in
	// this SDK already signs against (consumer.go, producer.go,
	// api/client.go).
	mintService = "execute-api"
)

// Closed error-code taxonomy from credential_session.schema.json's
// error_code definition (sdk-schemas #17), mirrored exactly in
// helix-tools/api PR #129's session_types.go. SDKs branch on these.
const (
	ErrCodeSubscriptionExpired        = "subscription_expired"
	ErrCodeSubscriptionWindowTooShort = "subscription_window_too_short"
	ErrCodeRoleNotProvisioned         = "role_not_provisioned"
	ErrCodeCustomerSuspended          = "customer_suspended"
	ErrCodeInsufficientScope          = "insufficient_scope"
)

// MintError is returned when the broker rejects, or fails to answer, a
// credential-session mint request. Code is one of the ErrCode* constants
// when the broker returned a typed error envelope; it is empty for
// untyped/transport-level failures (Message still carries useful detail).
type MintError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

// Error implements the error interface.
func (e *MintError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("helix credential broker: mint refused (%s): %s [status %d, request_id %s]",
			e.Code, e.Message, e.StatusCode, e.RequestID)
	}
	return fmt.Sprintf("helix credential broker: mint failed: %s [status %d]", e.Message, e.StatusCode)
}

// IsSubscriptionExpired reports whether err is a *MintError carrying the
// subscription_expired code (finding #24 enforced at mint time).
func IsSubscriptionExpired(err error) bool { return hasCode(err, ErrCodeSubscriptionExpired) }

// IsCustomerSuspended reports whether err is a *MintError carrying the
// customer_suspended code.
func IsCustomerSuspended(err error) bool { return hasCode(err, ErrCodeCustomerSuspended) }

func hasCode(err error, code string) bool {
	me, ok := err.(*MintError)
	return ok && me.Code == code
}

// mintSuccessResponse mirrors credential_session.schema.json's "success"
// definition / helix-tools/api PR #129's SessionResponse exactly. All six
// fields are required by the frozen contract.
type mintSuccessResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	Expiration      string `json:"expiration"` // RFC3339
	TTLSeconds      int64  `json:"ttl_seconds"`
	Region          string `json:"region"`
}

// missingFields returns the names of any required-by-contract fields left
// at their zero value, for a clear "malformed mint response" error.
func (s mintSuccessResponse) missingFields() []string {
	var missing []string
	if s.AccessKeyID == "" {
		missing = append(missing, "access_key_id")
	}
	if s.SecretAccessKey == "" {
		missing = append(missing, "secret_access_key")
	}
	if s.SessionToken == "" {
		missing = append(missing, "session_token")
	}
	if s.Expiration == "" {
		missing = append(missing, "expiration")
	}
	if s.TTLSeconds == 0 {
		missing = append(missing, "ttl_seconds")
	}
	if s.Region == "" {
		missing = append(missing, "region")
	}
	return missing
}

// mintErrorResponse mirrors credential_session.schema.json's "error"
// definition — the Go API's standard nested error envelope (top-level
// message + nested error{code,message,request_id}).
type mintErrorResponse struct {
	Message string `json:"message"`
	Error   struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// BrokerConfig configures a Provider.
type BrokerConfig struct {
	// APIEndpoint is the Helix Connect API base URL, e.g.
	// https://api-go.helix.tools. Required.
	APIEndpoint string

	// CustomerID is carried for diagnostics only — the broker resolves the
	// caller's identity from the SigV4-signed request, not from this
	// field (mirrors every other authenticated call in this SDK).
	CustomerID string

	// Region is the AWS region used both to sign the mint request and as
	// the default if the broker's response ever needs cross-checking.
	// Required.
	Region string

	// AWSAccessKeyID / AWSSecretAccessKey SigV4-sign the mint request —
	// the caller's existing static AWS key, which is the ratified B0/B1
	// broker bootstrap mechanism (STS-PLAN.md §9 decision #1). Both
	// required.
	AWSAccessKeyID     string
	AWSSecretAccessKey string

	// HTTPClient overrides the client used for mint HTTP calls (tests).
	// Defaults to a client with defaultMintTimeout when nil.
	HTTPClient *http.Client

	// now overrides the clock (tests only, unexported). Defaults to
	// time.Now when nil.
	now func() time.Time
}

// Provider implements aws.CredentialsProvider by minting a fresh AWS STS
// session credential from the Helix credential broker on every Retrieve
// call. Provider itself never caches — wrap it with NewCredentialsCache for
// production use (adds the proactive-refresh window, jitter, and
// single-flight coalescing courtesy of aws-sdk-go-v2's own
// aws.CredentialsCache), or call Retrieve directly in tests that need to
// disable auto-refresh.
type Provider struct {
	cfg        BrokerConfig
	httpClient *http.Client
	now        func() time.Time
}

// NewProvider validates cfg and returns a Provider. It performs no network
// I/O: validation is local and synchronous, so a bad BrokerConfig (missing
// endpoint/region/bootstrap credentials) surfaces immediately at
// construction, before any broker round-trip is attempted.
func NewProvider(cfg BrokerConfig) (*Provider, error) {
	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		return nil, fmt.Errorf("credentials: BrokerConfig.APIEndpoint is required")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, fmt.Errorf("credentials: BrokerConfig.Region is required")
	}
	if cfg.AWSAccessKeyID == "" || cfg.AWSSecretAccessKey == "" {
		return nil, fmt.Errorf("credentials: BrokerConfig requires AWSAccessKeyID and AWSSecretAccessKey to bootstrap-authenticate mint requests (SigV4) — see STS-PLAN.md §9 decision #1")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultMintTimeout}
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	return &Provider{cfg: cfg, httpClient: httpClient, now: now}, nil
}

// Retrieve implements aws.CredentialsProvider. It mints a fresh session
// credential from the broker (retrying transient failures with backoff+
// jitter), then computes Expires as local_now + ttl_seconds capped by the
// parsed server expiration — the clock-skew-hardened formula from
// STS-PLAN.md/C-sdk.md C.1 bullet 4: using the SMALLER of the two bounds
// means client/server clock drift can only ever shorten, never extend, a
// credential's effective client-side lifetime beyond what the server
// actually granted.
func (p *Provider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	resp, err := p.mintWithRetry(ctx)
	if err != nil {
		return aws.Credentials{}, err
	}

	serverExpiry, perr := time.Parse(time.RFC3339, resp.Expiration)
	if perr != nil {
		return aws.Credentials{}, fmt.Errorf("credentials: broker returned unparseable expiration %q: %w", resp.Expiration, perr)
	}
	if resp.TTLSeconds <= 0 {
		return aws.Credentials{}, fmt.Errorf("credentials: broker returned non-positive ttl_seconds %d", resp.TTLSeconds)
	}

	expires := p.now().Add(time.Duration(resp.TTLSeconds) * time.Second)
	if serverExpiry.Before(expires) {
		expires = serverExpiry
	}

	return aws.Credentials{
		AccessKeyID:     resp.AccessKeyID,
		SecretAccessKey: resp.SecretAccessKey,
		SessionToken:    resp.SessionToken,
		CanExpire:       true,
		Expires:         expires,
		Source:          "HelixCredentialBroker",
	}, nil
}

// mintWithRetry performs up to mintMaxAttempts mint round-trips, applying
// exponential backoff+jitter between attempts, and stops immediately on a
// non-retryable failure (a definitive auth/authz decision or a permanently
// malformed response — retrying either wastes the retry budget on an
// outcome that cannot change).
func (p *Provider) mintWithRetry(ctx context.Context) (*mintSuccessResponse, error) {
	var lastErr error
	for attempt := 0; attempt < mintMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoffDelay(attempt)):
			}
		}

		resp, retryable, err := p.mint(ctx)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("credentials: mint failed after %d attempts: %w", mintMaxAttempts, lastErr)
}

// backoffDelay returns exponential backoff (base * 2^(attempt-1)) with up
// to +/-25% jitter, for the attempt'th retry (attempt >= 1).
func backoffDelay(attempt int) time.Duration {
	base := mintRetryBaseDelay * time.Duration(int64(1)<<uint(attempt-1))
	jitter := time.Duration((rand.Float64()*0.5 - 0.25) * float64(base)) //nolint:gosec // timing jitter, not security-sensitive
	return base + jitter
}

// mint performs ONE mint HTTP round-trip. The second return value reports
// whether the caller should retry: network/transport errors, 429, and 5xx
// are retryable; everything else (2xx-but-malformed, 4xx auth/authz
// decisions such as subscription_expired) is not.
func (p *Provider) mint(ctx context.Context) (*mintSuccessResponse, bool, error) {
	req, err := p.buildRequest(ctx)
	if err != nil {
		return nil, false, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("credentials: mint request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("credentials: failed to read mint response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var success mintSuccessResponse
		if jerr := json.Unmarshal(body, &success); jerr != nil {
			return nil, false, fmt.Errorf("credentials: malformed mint response JSON: %w", jerr)
		}
		if missing := success.missingFields(); len(missing) > 0 {
			return nil, false, fmt.Errorf("credentials: mint response missing required field(s): %s", strings.Join(missing, ", "))
		}
		return &success, false, nil
	}

	mErr := &MintError{StatusCode: resp.StatusCode}
	var typedErr mintErrorResponse
	if jerr := json.Unmarshal(body, &typedErr); jerr == nil && (typedErr.Error.Code != "" || typedErr.Message != "") {
		mErr.Code = typedErr.Error.Code
		mErr.RequestID = typedErr.Error.RequestID
		mErr.Message = typedErr.Error.Message
		if mErr.Message == "" {
			mErr.Message = typedErr.Message
		}
	} else {
		mErr.Message = strings.TrimSpace(string(body))
	}

	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return nil, retryable, mErr
}

// buildRequest builds the SigV4-signed mint POST. The broker's request body
// is entirely optional (helix-tools/api PR #129's session_controller.go
// only binds when Content-Length != 0; an absent body defaults to "all
// owned planes, standard TTL") and this SDK version has no per-request
// scoping config surface, so the request carries no body — the same
// zero-body pattern already used by every other signed GET/DELETE call in
// this SDK (types.EmptyPayloadHash).
func (p *Provider) buildRequest(ctx context.Context) (*http.Request, error) {
	reqURL := strings.TrimRight(p.cfg.APIEndpoint, "/") + MintPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: failed to build mint request: %w", err)
	}

	bootstrap := awscreds.NewStaticCredentialsProvider(p.cfg.AWSAccessKeyID, p.cfg.AWSSecretAccessKey, "")
	bootstrapCreds, err := bootstrap.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("credentials: failed to resolve bootstrap credentials: %w", err)
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, bootstrapCreds, req, types.EmptyPayloadHash, mintService, p.cfg.Region, p.now()); err != nil {
		return nil, fmt.Errorf("credentials: failed to sign mint request: %w", err)
	}

	return req, nil
}

// NewCredentialsCache wraps p in an aws.CredentialsCache configured with
// this surface's refresh policy (proactiveExpiryWindow ± jitter — see those
// constants for derivation from the 900s TTL). optFns are applied AFTER the
// defaults, so callers/tests can override any option (e.g. a compressed
// ExpiryWindow for fast integration tests). The returned *aws.CredentialsCache
// exposes Invalidate() — call it to force the next Retrieve to re-mint
// ("force_refresh()" in e2e requirement E.8.2's terms).
func NewCredentialsCache(p *Provider, optFns ...func(*aws.CredentialsCacheOptions)) *aws.CredentialsCache {
	opts := append([]func(*aws.CredentialsCacheOptions){
		func(o *aws.CredentialsCacheOptions) {
			o.ExpiryWindow = proactiveExpiryWindow
			o.ExpiryWindowJitterFrac = expiryWindowJitterFrac
		},
	}, optFns...)
	return aws.NewCredentialsCache(p, opts...)
}

// SelectProvider infers/validates cfg's credential mode and returns the
// aws.CredentialsProvider NewConsumer/NewProducer should use. It performs no
// network I/O — safe to unit test exhaustively without a broker or AWS STS.
//
// Mode-inference matrix (empty CredentialMode): static keys present ->
// static (preserves every existing caller's behavior exactly — "sts" is
// NEVER inferred, only explicit opt-in); nothing present -> construction
// error. An explicit CredentialMode always wins, but is still validated
// against whatever credentials are actually present.
func SelectProvider(apiEndpoint string, cfg types.Config) (aws.CredentialsProvider, error) {
	hasStaticKeys := cfg.AWSAccessKeyID != "" && cfg.AWSSecretAccessKey != ""

	mode := cfg.CredentialMode
	if mode == "" {
		if hasStaticKeys {
			mode = types.CredentialModeStatic
		} else {
			return nil, fmt.Errorf("credentials: no credentials configured — set AWSAccessKeyID and AWSSecretAccessKey")
		}
	}

	switch mode {
	case types.CredentialModeStatic:
		if !hasStaticKeys {
			return nil, fmt.Errorf("credentials: CredentialMode %q requires AWSAccessKeyID and AWSSecretAccessKey", types.CredentialModeStatic)
		}
		return awscreds.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, ""), nil

	case types.CredentialModeSTS:
		if !hasStaticKeys {
			if cfg.APIKey != "" {
				return nil, fmt.Errorf("credentials: CredentialMode %q with APIKey is not yet supported by this SDK version — the credential broker currently accepts only the SigV4 bootstrap path (STS-PLAN.md §9 decision #1); set AWSAccessKeyID/AWSSecretAccessKey instead", types.CredentialModeSTS)
			}
			return nil, fmt.Errorf("credentials: CredentialMode %q requires AWSAccessKeyID and AWSSecretAccessKey to bootstrap the broker mint request", types.CredentialModeSTS)
		}
		provider, err := NewProvider(BrokerConfig{
			APIEndpoint:        apiEndpoint,
			CustomerID:         cfg.CustomerID,
			Region:             cfg.Region,
			AWSAccessKeyID:     cfg.AWSAccessKeyID,
			AWSSecretAccessKey: cfg.AWSSecretAccessKey,
		})
		if err != nil {
			return nil, err
		}
		return NewCredentialsCache(provider), nil

	default:
		return nil, fmt.Errorf("credentials: invalid CredentialMode %q: must be %q, %q, or empty", mode, types.CredentialModeStatic, types.CredentialModeSTS)
	}
}
