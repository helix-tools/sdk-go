// Package credentials implements an aws.CredentialsProvider that mints
// short-lived AWS STS session credentials from the Helix Connect credential
// broker (POST /v1/credentials/session), per credential_session.schema.json
// (sdk-schemas #17).
//
// Two ways to consume it:
//
//   - Production / opt-in customers: SelectProvider(apiEndpoint, cfg) picks
//     the right aws.CredentialsProvider for a types.Config — static (default,
//     byte-identical to the SDK's pre-STS behavior) or sts (this package's
//     auto-refreshing broker-backed provider, wrapped in aws.CredentialsCache).
//     consumer.NewConsumer and producer.NewProducer call this internally.
//
//   - Tests that need to disable auto-refresh or force a refresh: use
//     Provider directly (each Retrieve call mints fresh, no caching — freeze
//     the one result you want with awssdk-go-v2's
//     credentials.NewStaticCredentialsProvider to simulate
//     "auto_refresh=False"), or hold onto the *aws.CredentialsCache returned
//     by NewCredentialsCache and call its exported Invalidate() method to
//     force the next Retrieve to re-mint ("force_refresh()").
//
// Bootstrap authentication: a mint request is either SigV4-signed with the
// caller's existing static AWS key (AWSAccessKeyID/AWSSecretAccessKey) or
// sent with a Helix API key ("Authorization: HLX-API-Key <key>", no SigV4
// signature on that request) — see BrokerConfig.APIKey and SelectProvider's
// mode-resolution rule for which one wins when both are configured.
package credentials

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
	"github.com/helix-tools/sdk-go/v2/internal/useragent"
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
// error_code definition (sdk-schemas #17). SDKs branch on these.
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

	// Friendly marks Message as one of the exact customer-facing strings
	// from design §4.11's error table (friendlyMintErrorMessage) — Error()
	// returns it verbatim, with no status/code/request_id decoration, so
	// every Helix SDK (TS, Python, Go) surfaces identical text for the same
	// server response.
	Friendly bool

	// latchable caches isLatchableMintCode's verdict, computed in mint()
	// from the RAW error.code/error.message pair before Message is
	// potentially overwritten with friendly text — see
	// isLatchableMintError's doc comment for why the decision cannot be
	// safely re-derived later from this struct's own exported fields.
	latchable bool
}

// Error implements the error interface.
func (e *MintError) Error() string {
	if e.Friendly {
		return e.Message
	}
	if e.Code != "" {
		return fmt.Sprintf("helix credential broker: mint refused (%s): %s [status %d, request_id %s]",
			e.Code, e.Message, e.StatusCode, e.RequestID)
	}
	return fmt.Sprintf("helix credential broker: mint failed: %s [status %d]", e.Message, e.StatusCode)
}

// friendlyMintErrorMessage returns the exact customer-facing message from
// design §4.11's error table for a mint failure, or "" when this (status,
// code, message) combination has no mapped message — the caller then keeps
// MintError's generic formatting. usedAPIKey distinguishes the two possible
// causes of a 401: today's existing "bad static key" failure (unchanged,
// not mapped here) vs. a rejected API key (newly mapped) — a 401 is only
// ever mapped to the API-key message when the mint was actually
// API-key-bootstrapped, so no existing static/SigV4 caller's error text
// changes. These strings are shared verbatim across the TypeScript, Python
// and Go SDKs; never edit one without the other two.
//
// code and message must be the RAW envelope values (helix-tools/api's
// error.code / error.message), not any already-substituted friendly text —
// see the mint error contract (scratchpad/briefs/mint-error-contract.md):
// the API's revoked-key response carries code "forbidden" (today) or
// "api_key_expired"/"static_credentials_retired" (api PR #449's canonical
// codes) OR "api_key_revoked" (the post-fix canonical form), with the
// human-readable detail living in message, not code — "feature not enabled:
// sts_broker" and "api key revoked" are both exact MESSAGE strings, never
// codes, so they are matched against message, not code.
func friendlyMintErrorMessage(statusCode int, code, message string, usedAPIKey bool) string {
	if usedAPIKey && statusCode == http.StatusUnauthorized {
		return "Helix API key was rejected. Create a new key in the Helix portal under API Keys."
	}
	if statusCode != http.StatusForbidden {
		return ""
	}
	switch code {
	case "api_key_revoked":
		return "This Helix API key has been revoked. Create a new key in the Helix portal under API Keys."
	case "api_key_expired":
		return "This Helix API key has expired. Create a new key in the Helix portal under API Keys."
	case "static_credentials_retired":
		return "AWS access keys have been retired for this account. Configure apiKey (Helix API key) instead."
	}
	switch message {
	case "api key revoked":
		return "This Helix API key has been revoked. Create a new key in the Helix portal under API Keys."
	case "feature not enabled: sts_broker":
		return "API keys are not enabled for this account yet. Keep using your AWS access keys, or contact Helix support."
	default:
		return ""
	}
}

// apiKeyPattern matches a raw Helix API key (design §4.2: "hlx_" + 43
// base64url characters), mirroring the redaction pattern the design
// prescribes for server-side logs. Applied defensively to every mint error
// message, static or API-key bootstrapped: if a server bug ever echoed a
// submitted key back in an error body, it must never reach a caller's error
// text or logs.
//
// Matches "hlx_" plus a run of one or more base64url characters, rather than
// a fixed {43} bounded by \b: \b asserts a transition between a word and a
// non-word character, and '-' is NOT a word character, so a trailing \b
// fails to match — and therefore fails to redact — any real key that
// happens to end in '-'. Matching the whole contiguous base64url run
// instead needs no boundary assertion: it stops at the first character
// outside the key alphabet, whatever that character is (end of string,
// whitespace, a URL delimiter like '&' or '?', etc.), so it can never
// under-redact a trailing character the way the \b version did.
var apiKeyPattern = regexp.MustCompile(`hlx_[A-Za-z0-9_-]{43,}`)

func redactAPIKeys(s string) string {
	return apiKeyPattern.ReplaceAllString(s, "hlx_<redacted>")
}

// IsSubscriptionExpired reports whether err is a *MintError carrying the
// subscription_expired code, enforced at mint time.
func IsSubscriptionExpired(err error) bool { return hasCode(err, ErrCodeSubscriptionExpired) }

// IsCustomerSuspended reports whether err is a *MintError carrying the
// customer_suspended code.
func IsCustomerSuspended(err error) bool { return hasCode(err, ErrCodeCustomerSuspended) }

func hasCode(err error, code string) bool {
	me, ok := err.(*MintError)
	return ok && me.Code == code
}

// isLatchableMintError reports whether err is a *MintError representing a
// permanent problem with THIS Provider's own bootstrap credential — a
// rejected signature/key (401), or one of the specific 403 causes design
// §4.11 maps to a friendly message (a revoked/expired/not-enabled/retired
// key; see friendlyMintErrorMessage). These are properties of the
// credential value baked into BrokerConfig at construction time, which
// never changes for the lifetime of a Provider, so retrying — or simply
// waiting — can never produce a different outcome; see Provider.latchedErr.
//
// Every OTHER 4xx (subscription_expired, subscription_window_too_short,
// role_not_provisioned, customer_suspended, insufficient_scope, and any
// other 403/400) is deliberately NOT latched: those reflect ACCOUNT-level
// state that can change independently of the credential — a renewed
// subscription, a lifted suspension, a provisioned role — while the caller
// keeps using the very same Provider. Latching those would wrongly keep
// surfacing a stale failure forever, even after the account-side condition
// that caused it is resolved.
//
// The decision itself is made once, in mint(), from the RAW error.code/
// error.message pair (see isLatchableMintCode) and cached on
// MintError.latchable — never recomputed here from me.Code/me.Message,
// because mint() overwrites Message with the friendly customer-facing text
// before returning, so by the time an error reaches here the exact server
// message a latch rule may key on (e.g. "api key revoked") can already be
// gone.
func isLatchableMintError(err error) bool {
	var me *MintError
	if !errors.As(err, &me) {
		return false
	}
	return me.latchable
}

// isLatchableMintCode implements the mint error contract's latch rule
// (scratchpad/briefs/mint-error-contract.md): 401 always latches; a 403
// latches when error.code is one of the closed revoked/expired/retired
// codes, OR — because helix-tools/api's real revoked-key and
// not-enabled-feature responses carry their distinguishing text in
// error.message, not error.code (code is just "forbidden") — when
// error.message, compared exactly, is one of the two message-keyed cases.
// code and message must be the RAW envelope values, captured before any
// friendly-text substitution.
func isLatchableMintCode(statusCode int, code, message string) bool {
	if statusCode == http.StatusUnauthorized {
		return true
	}
	if statusCode != http.StatusForbidden {
		return false
	}
	switch code {
	case "api_key_revoked", "api_key_expired", "static_credentials_retired":
		return true
	}
	switch message {
	case "api key revoked", "feature not enabled: sts_broker":
		return true
	default:
		return false
	}
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

	// AWSAccessKeyID / AWSSecretAccessKey SigV4-sign the mint request,
	// using the caller's existing static AWS key. Required unless APIKey is
	// set.
	AWSAccessKeyID     string
	AWSSecretAccessKey string

	// APIKey, when set, bootstraps the mint request with
	// "Authorization: HLX-API-Key <key>" instead of a SigV4 signature —
	// AWSAccessKeyID/AWSSecretAccessKey are then not required and are
	// ignored by this Provider. Leading/trailing whitespace (e.g. a
	// trailing newline from an env file) is trimmed. NewProvider refuses to
	// construct a Provider that would send this over an insecure transport
	// — see allowsAPIKeyTransport. Excluded from json.Marshal entirely (see
	// BrokerConfig.MarshalJSON) so an incidental marshal (logging, a debug
	// dump) never serializes the raw key — String/GoString redaction alone
	// does not cover encoding/json, which does not consult
	// fmt.Stringer/GoStringer at all.
	APIKey string

	// HTTPClient overrides the client used for mint HTTP calls (tests).
	// Defaults to a client with defaultMintTimeout when nil. Its Transport,
	// Timeout and Jar are honored, but its CheckRedirect is always replaced
	// with one that refuses every redirect on the mint call — see
	// refuseMintRedirect for why.
	HTTPClient *http.Client

	// now overrides the clock (tests only, unexported). Defaults to
	// time.Now when nil.
	now func() time.Time
}

// String implements fmt.Stringer. AWSSecretAccessKey and APIKey are
// redacted so a BrokerConfig (and, by extension, a *Provider holding one —
// see Provider.String) is safe to appear in an incidental %v/%+v.
func (c BrokerConfig) String() string {
	secret := ""
	if c.AWSSecretAccessKey != "" {
		secret = "<redacted>"
	}
	key := ""
	if c.APIKey != "" {
		key = "<redacted>"
	}
	return fmt.Sprintf(
		"BrokerConfig{APIEndpoint:%q, CustomerID:%q, Region:%q, AWSAccessKeyID:%q, AWSSecretAccessKey:%q, APIKey:%q}",
		c.APIEndpoint, c.CustomerID, c.Region, c.AWSAccessKeyID, secret, key)
}

// GoString implements fmt.GoStringer. Go's fmt package only consults
// Stringer for %v/%+v — %#v bypasses it entirely and reflects every field,
// including unexported ones, verbatim. Without this method, "%#v" of a
// BrokerConfig (or a *Provider holding one, see Provider.GoString) would
// print the raw AWSSecretAccessKey and APIKey.
func (c BrokerConfig) GoString() string {
	return c.String()
}

// MarshalJSON implements json.Marshaler, omitting APIKey from the encoded
// output entirely (never present, not merely an empty string) — see the
// field's own doc comment, and Config.MarshalJSON in package types for why
// the shadow type below is an anonymous struct literal rather than a named
// type: this package's own TestWireStructs (types/wire_names_test.go) walks
// every named struct declared in a wire-facing package and would otherwise
// treat a tagged BrokerConfig as a wire payload requiring every field to
// carry a snake_case json tag, which BrokerConfig (SDK-side configuration,
// never itself serialized to the API) is not.
func (c BrokerConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		APIEndpoint        string
		CustomerID         string
		Region             string
		AWSAccessKeyID     string
		AWSSecretAccessKey string
		HTTPClient         *http.Client
	}{
		APIEndpoint:        c.APIEndpoint,
		CustomerID:         c.CustomerID,
		Region:             c.Region,
		AWSAccessKeyID:     c.AWSAccessKeyID,
		AWSSecretAccessKey: c.AWSSecretAccessKey,
		HTTPClient:         c.HTTPClient,
	})
}

// allowsAPIKeyTransport reports whether apiEndpoint is safe to carry a
// Helix API key (design §4.11's transport guard): either an https:// URL
// (the key travels encrypted regardless of the destination host), or a URL
// whose host is EXACTLY "localhost" or "127.0.0.1" (the escape hatch for a
// non-TLS local dev server). The hostname check is an exact match on
// url.URL.Hostname(), never a substring/suffix/prefix match, so a lookalike
// host such as "localhost.evil.example" cannot smuggle an insecure (http://)
// request past the guard — that host is only allowed over https://, same as
// any other host.
func allowsAPIKeyTransport(apiEndpoint string) bool {
	u, err := url.Parse(apiEndpoint)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1":
		return true
	default:
		return false
	}
}

// errMintRedirectRefused is returned by refuseMintRedirect to fail closed on
// ANY redirect from a mint response. The mint endpoint is a fixed,
// fully-qualified URL (BrokerConfig.APIEndpoint + MintPath) that never
// legitimately redirects; Go's default http.Client would otherwise replay
// this request's Authorization header (a SigV4 signature or a raw Helix API
// key) at whatever scheme/host a Location header names — including an
// https-> http downgrade, which would then carry the credential in
// cleartext. Go's own redirect handling only strips sensitive headers when
// the redirect target's HOST differs (see net/http's
// shouldCopyHeaderOnRedirect); it does not consider the scheme at all, so a
// same-host https->http redirect would otherwise sail through with
// Authorization intact. Refusing every redirect (not just scheme/host
// changes) also covers a same-host, same-scheme redirect — see
// refuseMintRedirect's doc comment for why that case is refused too.
var errMintRedirectRefused = errors.New("credentials: refusing to follow a redirect on a credential mint request")

// refuseMintRedirect is installed as every mint HTTP client's CheckRedirect,
// unconditionally. The mint endpoint (BrokerConfig.APIEndpoint + MintPath)
// is a fixed URL with no legitimate reason to redirect at all, so refusing
// every redirect — not just a scheme downgrade or a host change — is the
// correct, simplest fail-closed policy: a legitimate API move should update
// APIEndpoint/MintPath in configuration, never rely on silently following a
// redirect from a credential-minting endpoint.
func refuseMintRedirect(*http.Request, []*http.Request) error {
	return errMintRedirectRefused
}

// Provider implements aws.CredentialsProvider by minting a fresh AWS STS
// session credential from the Helix credential broker on every Retrieve
// call. Provider itself never caches — wrap it with NewCredentialsCache for
// production use (adds the proactive-refresh window, jitter, and
// single-flight coalescing courtesy of aws-sdk-go-v2's own
// aws.CredentialsCache), or call Retrieve directly in tests that need to
// disable auto-refresh.
//
// Provider also implements aws.HandleFailRefreshCredentialsCacheStrategy and
// aws.AdjustExpiresByCredentialsCacheStrategy (see HandleFailToRefresh and
// AdjustExpiresBy below) so that, when wrapped in NewCredentialsCache, a
// broker blip during a due refresh rides through on the last-known-good
// session until its TRUE hard expiry rather than failing the caller
// immediately — a broker blip stays invisible to the caller.
type Provider struct {
	cfg        BrokerConfig
	httpClient *http.Client
	now        func() time.Time

	mu             sync.Mutex
	lastHardExpiry time.Time
	haveHardExpiry bool

	// latchedErr negative-caches a mint failure that reflects a permanent
	// problem with THIS Provider's own bootstrap credential — see
	// isLatchableMintError — rather than account-level state that could
	// resolve on its own. BrokerConfig's credential fields are immutable
	// after NewProvider, so once this Provider's own credential is
	// confirmed bad, retrying can never produce a different outcome: every
	// later Retrieve returns the same error without a network call. A fresh
	// Provider (a new credential) always starts with this unset.
	latchedErr error
}

// String implements fmt.Stringer, delegating to cfg's own redaction. Go's
// fmt package already checks nested struct fields for Stringer during
// %v/%+v recursion, so BrokerConfig.String alone would cover this — this
// method exists as an explicit, version-independent guarantee rather than
// relying on that implementation detail.
func (p *Provider) String() string {
	return fmt.Sprintf("Provider{%s}", p.cfg.String())
}

// GoString implements fmt.GoStringer, delegating to cfg's own redaction —
// see BrokerConfig.GoString for why %#v needs its own, separate coverage
// from String/Stringer.
func (p *Provider) GoString() string {
	return p.String()
}

// NewProvider validates cfg and returns a Provider. It performs no network
// I/O: validation is local and synchronous, so a bad BrokerConfig (missing
// endpoint/region/bootstrap credentials, an API key over an insecure
// transport) surfaces immediately at construction, before any broker
// round-trip is attempted.
func NewProvider(cfg BrokerConfig) (*Provider, error) {
	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		return nil, fmt.Errorf("credentials: BrokerConfig.APIEndpoint is required")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, fmt.Errorf("credentials: BrokerConfig.Region is required")
	}

	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	hasAPIKey := cfg.APIKey != ""
	hasStatic := cfg.AWSAccessKeyID != "" && cfg.AWSSecretAccessKey != ""

	if !hasAPIKey && !hasStatic {
		return nil, fmt.Errorf("credentials: BrokerConfig requires AWSAccessKeyID and AWSSecretAccessKey (or APIKey) to bootstrap mint requests")
	}
	if hasAPIKey && !allowsAPIKeyTransport(cfg.APIEndpoint) {
		return nil, fmt.Errorf("credentials: refusing to send a Helix API key to endpoint %q — APIEndpoint must be https://, or host localhost/127.0.0.1", cfg.APIEndpoint)
	}

	baseClient := cfg.HTTPClient
	if baseClient == nil {
		baseClient = &http.Client{Timeout: defaultMintTimeout}
	}
	// A fresh *http.Client, never the caller-supplied one mutated in place:
	// mutating cfg.HTTPClient's own CheckRedirect would silently change the
	// behavior of any OTHER code sharing that same *http.Client pointer.
	// Transport/Timeout/Jar are carried over so a caller-supplied client
	// (tests, a custom Transport) still behaves as configured; CheckRedirect
	// is always refuseMintRedirect regardless of what the caller set, since
	// this is a Provider-owned security invariant, not something to leave to
	// chance in a client documented as a test override.
	httpClient := &http.Client{
		Transport:     baseClient.Transport,
		Jar:           baseClient.Jar,
		Timeout:       baseClient.Timeout,
		CheckRedirect: refuseMintRedirect,
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
// parsed server expiration — using the SMALLER of the two bounds means
// client/server clock drift can only ever shorten, never extend, a
// credential's effective client-side lifetime beyond what the server
// actually granted.
func (p *Provider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	if err := p.getLatchedError(); err != nil {
		return aws.Credentials{}, err
	}

	resp, err := p.mintWithRetry(ctx)
	if err != nil {
		p.latchIfPermanent(err)
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

	// Record the TRUE (unadjusted) hard expiry of this mint, independent of
	// whatever aws.CredentialsCache's own ExpiryWindow adjustment later
	// does to the Expires it stores. HandleFailToRefresh/AdjustExpiresBy
	// below use this to bound a ride-through precisely at real expiry.
	p.mu.Lock()
	p.lastHardExpiry = expires
	p.haveHardExpiry = true
	p.mu.Unlock()

	return aws.Credentials{
		AccessKeyID:     resp.AccessKeyID,
		SecretAccessKey: resp.SecretAccessKey,
		SessionToken:    resp.SessionToken,
		CanExpire:       true,
		Expires:         expires,
		Source:          "HelixCredentialBroker",
	}, nil
}

// getLatchedError returns the negative-cached mint error, if any — see
// Provider.latchedErr.
func (p *Provider) getLatchedError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latchedErr
}

// latchIfPermanent negative-caches err in p.latchedErr if isLatchableMintError
// reports it reflects a permanent problem with this Provider's own bootstrap
// credential. A no-op for every other error (retryable transport/5xx
// failures that exhausted mintWithRetry's bounded retries, and account-level
// 4xx refusals that could resolve independently of the credential).
func (p *Provider) latchIfPermanent(err error) {
	if !isLatchableMintError(err) {
		return
	}
	p.mu.Lock()
	p.latchedErr = err
	p.mu.Unlock()
}

// HandleFailToRefresh implements aws.HandleFailRefreshCredentialsCacheStrategy.
// aws.CredentialsCache calls this when a due refresh's Retrieve fails; old is
// the previously cached (window-adjusted) credential. Rather than failing
// the caller immediately, ride through on old's key material as long as the
// TRUE hard expiry from the last successful mint (tracked in
// p.lastHardExpiry, independent of old.Expires' own adjustment) has not
// actually passed yet — a broker blip during the ~5-minute pre-expiry
// refresh window becomes invisible to callers, which keep serving the
// last-known-good credential until it truly expires.
//
// The returned Expires is deliberately set to lastHardExpiry+
// proactiveExpiryWindow, NOT lastHardExpiry itself: aws.CredentialsCache
// unconditionally re-applies its own -(ExpiryWindow-jitter) adjustment
// (via AdjustExpiresBy below) to whatever this method returns before
// storing it, so this pre-compensates for that upcoming subtraction.
// AdjustExpiresBy then clamps the final result to never exceed
// lastHardExpiry regardless of the per-call random jitter draw — the two
// methods work together; neither is safe/precise alone. Net effect: after
// one ride-through, the cache serves the SAME stale-but-still-valid
// credential (no further mint attempts, no request storm) until real-world
// time reaches lastHardExpiry, at which point Expired() correctly flips
// true and the next Retrieve tries the broker again (recovered or not).
//
// If there is no prior successful mint to ride through on (haveHardExpiry
// false), old carries no usable key material, or the true hard expiry has
// already passed, the original refresh error is returned unchanged — fail
// closed, never fabricate or indefinitely reuse a truly expired credential.
func (p *Provider) HandleFailToRefresh(_ context.Context, old aws.Credentials, err error) (aws.Credentials, error) {
	p.mu.Lock()
	hardExpiry, have := p.lastHardExpiry, p.haveHardExpiry
	p.mu.Unlock()

	if !have || !old.HasKeys() || !p.now().Before(hardExpiry) {
		return aws.Credentials{}, err
	}

	old.CanExpire = true
	old.Expires = hardExpiry.Add(proactiveExpiryWindow)
	return old, nil
}

// AdjustExpiresBy implements aws.AdjustExpiresByCredentialsCacheStrategy,
// overriding aws.CredentialsCache's default window-subtraction with the
// same computation PLUS a clamp: the result never exceeds the true hard
// expiry from the last successful mint (see HandleFailToRefresh's doc
// comment for why this pairing is required for a precise, non-overshooting
// ride-through). For a normal fresh mint this clamp is always a no-op — a
// freshly minted credential's window-adjusted Expires is always safely
// below its own hard expiry — so this only changes behavior during a
// ride-through.
func (p *Provider) AdjustExpiresBy(creds aws.Credentials, dur time.Duration) (aws.Credentials, error) {
	if !creds.CanExpire {
		return creds, nil
	}
	creds.Expires = creds.Expires.Add(dur)

	p.mu.Lock()
	hardExpiry, have := p.lastHardExpiry, p.haveHardExpiry
	p.mu.Unlock()
	if have && creds.Expires.After(hardExpiry) {
		creds.Expires = hardExpiry
	}
	return creds, nil
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
			delay, err := backoffDelay(attempt)
			if err != nil {
				return nil, fmt.Errorf("credentials: failed to generate retry jitter: %w", err)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
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
func backoffDelay(attempt int) (time.Duration, error) {
	return backoffDelayFrom(rand.Reader, attempt)
}

func backoffDelayFrom(random io.Reader, attempt int) (time.Duration, error) {
	var randomBytes [8]byte
	if _, err := io.ReadFull(random, randomBytes[:]); err != nil {
		return 0, err
	}

	// Use the upper 53 random bits so every possible value is represented
	// exactly as a float64 in [0, 1).
	randomFraction := float64(binary.BigEndian.Uint64(randomBytes[:])>>11) / (1 << 53)
	base := mintRetryBaseDelay * time.Duration(int64(1)<<uint(attempt-1))
	jitter := time.Duration((randomFraction*0.5 - 0.25) * float64(base))
	return base + jitter, nil
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
		return nil, true, sdkerr.Wrap("credentials: mint request failed", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, sdkerr.Wrap("credentials: failed to read mint response", err)
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
	mErr.Message = redactAPIKeys(mErr.Message)

	// Decided from the RAW code/message pair, before the friendly-text
	// substitution below can overwrite mErr.Message — see
	// isLatchableMintError's doc comment.
	mErr.latchable = isLatchableMintCode(mErr.StatusCode, mErr.Code, mErr.Message)

	usedAPIKey := p.cfg.APIKey != ""
	if friendly := friendlyMintErrorMessage(resp.StatusCode, mErr.Code, mErr.Message, usedAPIKey); friendly != "" {
		mErr.Message = friendly
		mErr.Friendly = true
	}

	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return nil, retryable, mErr
}

// buildRequest builds the mint POST, bootstrap-authenticated either with
// the Helix API key (Authorization: HLX-API-Key <key>, no SigV4 signature)
// or, when no APIKey is configured, with a SigV4 signature from the static
// AWS key — see BrokerConfig.APIKey. The broker's request body is entirely
// optional (helix-tools/api PR #129's session_controller.go only binds when
// Content-Length != 0; an absent body defaults to "all owned planes,
// standard TTL") and this SDK version has no per-request scoping config
// surface, so the request carries no body in either case — the same
// zero-body pattern already used by every other signed GET/DELETE call in
// this SDK (types.EmptyPayloadHash).
func (p *Provider) buildRequest(ctx context.Context) (*http.Request, error) {
	reqURL := strings.TrimRight(p.cfg.APIEndpoint, "/") + MintPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: failed to build mint request: %w", err)
	}

	// SigV4 ignores User-Agent when building its signed-headers set (see
	// aws-sdk-go-v2's signer/internal/v4.IgnoredHeaders), so setting it
	// before signing is safe — mirrors consumer.makeAPIRequest. Harmless
	// (and unsigned) on the API-key path too.
	req.Header.Set("User-Agent", useragent.String())

	if p.cfg.APIKey != "" {
		// NewProvider already trimmed APIKey and verified the transport
		// guard (allowsAPIKeyTransport) at construction time, so no
		// per-request re-check is needed here — p.cfg.APIEndpoint never
		// changes after construction. http.Header.Set would itself reject
		// a value containing a raw CR/LF (net/http's header-write path
		// returns "invalid header field value"), but NewProvider's
		// TrimSpace already strips the common case (a trailing newline from
		// an env file) before it ever reaches here.
		req.Header.Set("Authorization", "HLX-API-Key "+p.cfg.APIKey)
		return req, nil
	}

	bootstrap := awscreds.NewStaticCredentialsProvider(p.cfg.AWSAccessKeyID, p.cfg.AWSSecretAccessKey, "")
	bootstrapCreds, err := bootstrap.Retrieve(ctx)
	if err != nil {
		return nil, sdkerr.Wrap("credentials: failed to resolve bootstrap credentials", err)
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, bootstrapCreds, req, types.EmptyPayloadHash, mintService, p.cfg.Region, p.now()); err != nil {
		return nil, sdkerr.Wrap("credentials: failed to sign mint request", err)
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

// warningWriter receives mode-resolution warnings emitted by SelectProvider
// (design §4.11's "exactly one warning" rule). It is a package-level
// variable, mirroring producer.deprecationWriter, so tests can capture the
// output instead of scraping os.Stderr; defaults to os.Stderr for real
// callers.
var warningWriter io.Writer = os.Stderr

// The exact warning message bodies from design §4.11's mode-resolution
// rule. These are shared verbatim across the TypeScript, Python and Go
// SDKs — never edit one without the other two. warn prefixes them with the
// package's usual "helix sdk-go: " convention (see
// producer.deprecationWriter's callers).
const (
	warnMsgKeyFieldIgnoredInStaticMode   = "apiKey is ignored because credentialMode is 'static'"
	warnMsgStaticFieldsIgnoredWhenKeySet = "AWS access keys are ignored because apiKey is set"
)

func warn(message string) {
	fmt.Fprintln(warningWriter, "helix sdk-go: "+message)
}

// SelectProvider infers/validates cfg's credential mode and returns the
// aws.CredentialsProvider NewConsumer/NewProducer should use. It performs no
// network I/O — safe to unit test exhaustively without a broker or AWS STS.
//
// Mode-resolution rule (design §4.11), identical across the TS/Python/Go
// SDKs:
//
//  1. Explicit CredentialMode:
//     - "static" requires static keys; a set APIKey is ignored, with the
//     warnMsgKeyFieldIgnoredInStaticMode warning.
//     - "sts" bootstraps with APIKey if set (ignoring static keys, if also
//     set, with the warnMsgStaticFieldsIgnoredWhenKeySet warning), else with
//     static keys (today's existing behavior, unchanged).
//  2. No mode set:
//     - APIKey set -> sts via the key; static keys, if also set, are
//     ignored with the warnMsgStaticFieldsIgnoredWhenKeySet warning.
//     - else static keys set -> "static" (preserves every existing
//     caller's behavior exactly — bootstrap-by-static-key "sts" is NEVER
//     inferred, only explicit opt-in).
//     - else: construction error.
//
// There is no silent fallback from a failed API-key mint to static keys in
// either branch: the sts-mode providers constructed here simply return the
// broker's error from Retrieve — nothing catches that error and substitutes
// a different provider.
func SelectProvider(apiEndpoint string, cfg types.Config) (aws.CredentialsProvider, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	hasAPIKey := apiKey != ""
	hasStaticKeys := cfg.AWSAccessKeyID != "" && cfg.AWSSecretAccessKey != ""

	mode := cfg.CredentialMode
	if mode == "" {
		if hasAPIKey {
			if hasStaticKeys {
				warn(warnMsgStaticFieldsIgnoredWhenKeySet)
			}
			return newAPIKeyProvider(apiEndpoint, cfg.CustomerID, cfg.Region, apiKey)
		}
		if hasStaticKeys {
			mode = types.CredentialModeStatic
		} else {
			return nil, fmt.Errorf("credentials: no credentials configured — set AWSAccessKeyID and AWSSecretAccessKey, or APIKey")
		}
	}

	switch mode {
	case types.CredentialModeStatic:
		if !hasStaticKeys {
			return nil, fmt.Errorf("credentials: CredentialMode %q requires AWSAccessKeyID and AWSSecretAccessKey", types.CredentialModeStatic)
		}
		if hasAPIKey {
			warn(warnMsgKeyFieldIgnoredInStaticMode)
		}
		return awscreds.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, ""), nil

	case types.CredentialModeSTS:
		if hasAPIKey {
			if hasStaticKeys {
				warn(warnMsgStaticFieldsIgnoredWhenKeySet)
			}
			return newAPIKeyProvider(apiEndpoint, cfg.CustomerID, cfg.Region, apiKey)
		}
		if !hasStaticKeys {
			return nil, fmt.Errorf("credentials: CredentialMode %q requires AWSAccessKeyID and AWSSecretAccessKey to bootstrap the broker mint request, or APIKey", types.CredentialModeSTS)
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

// newAPIKeyProvider builds the sts-mode provider for the API-key bootstrap
// path, shared by both SelectProvider branches that resolve to it.
func newAPIKeyProvider(apiEndpoint, customerID, region, apiKey string) (aws.CredentialsProvider, error) {
	provider, err := NewProvider(BrokerConfig{
		APIEndpoint: apiEndpoint,
		CustomerID:  customerID,
		Region:      region,
		APIKey:      apiKey,
	})
	if err != nil {
		return nil, err
	}
	return NewCredentialsCache(provider), nil
}
