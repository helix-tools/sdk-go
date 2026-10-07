// Package producer provides functionality for uploading datasets to the Helix Connect Platform.
//
// It handles the entire lifecycle of dataset production, including authentication,
// encrypting, compressing, uploading datasets, and notifying subscribers of new uploads.
//
// TODO: adopt structured logging with configurable levels (e.g. debug).
package producer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	stscreds "github.com/helix-tools/sdk-go/v2/credentials"
	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
	"github.com/helix-tools/sdk-go/v2/internal/transferclient"
	"github.com/helix-tools/sdk-go/v2/internal/useragent"
	"github.com/helix-tools/sdk-go/v2/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Producer handles uploading and managing datasets on Helix Connect platform.
type Producer struct {
	APIEndpoint string

	// Deprecated: the platform now owns the upload destination, so
	// NewProducer leaves BucketName empty and dataset creation no longer
	// sends it. The field stays only so existing code keeps compiling.
	BucketName string

	CustomerID string

	// This producer's own encryption key id, supplied by the Helix API
	// when the Producer is created. Empty means it could not be
	// resolved, and every UploadDataset call fails until it is. A failed
	// lookup is retried on the next upload (see ensureEncryptionKeyID)
	// unless the API gave a definitive "no key configured" answer.
	KMSKeyID string

	Region string

	awsConfig  aws.Config
	httpClient *http.Client
	kmsClient  *kms.Client
	s3Client   *s3.Client

	// storageClient is used only for the presigned-URL upload — never for a
	// call to the Helix API — so a slow but healthy transfer is never cut by
	// a total-duration cap. A Producer built directly (bypassing
	// NewProducer, as tests do but production code never does) leaves this
	// nil; uploadToPresignedURL falls back to httpClient in that case so
	// existing tests that only ever set httpClient keep working unchanged.
	storageClient *http.Client

	// keyLookupAttempted is set once NewProducer has made its
	// construction-time key lookup, gating the retry in
	// ensureEncryptionKeyID: a Producer built directly (bypassing
	// NewProducer, as tests do but production code never does) never sets
	// it, so an empty KMSKeyID keeps failing the same local, network-free
	// check it always has rather than reaching out to an API endpoint the
	// caller never configured.
	keyLookupAttempted bool

	keyLookupMu sync.Mutex
	// keyLookupDefinitiveNoKey caches a definitive "no key configured"
	// answer (a 404): that is never retried, same as before this field
	// existed. A clean 200 with a missing, empty, or blank key is NOT
	// definitive — the account's key may simply not be provisioned yet —
	// so it is never cached here; the next upload retries it.
	keyLookupDefinitiveNoKey bool
	// keyLookupInFlight is the currently-running lookup, if any, so
	// concurrent callers share its one result instead of each firing their
	// own request at the API.
	keyLookupInFlight *keyLookupCall
}

// keyLookupCall is one in-flight (or just-finished) encryption-key lookup,
// shared by every caller that asked while it was running.
type keyLookupCall struct {
	done            chan struct{}
	keyID           string
	definitiveNoKey bool
	err             error
}

// APIError represents an error returned by the Helix API with status code.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Body)
}

// IsConflict returns true if the error is a 409 Conflict (duplicate resource).
func (e *APIError) IsConflict() bool {
	return e.StatusCode == http.StatusConflict
}

// IsUnauthorized reports a 401 (bad or expired credentials).
func (e *APIError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports a 403 (authenticated but not permitted).
func (e *APIError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// IsNotFound reports a 404 (the resource does not exist or is not visible).
func (e *APIError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsRateLimited reports a 429: back off and retry.
func (e *APIError) IsRateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// UploadOptions contains options for uploading datasets.
//
// NOTE: Use NewUploadOptions() to get sane defaults.
// NOTE: Encryption and compression are required: every upload is compressed
// and then encrypted, and no option turns either off. UploadDataset returns an
// error — before any network call — when the encryption key is missing, or when
// Metadata / DatasetOverrides try to switch the record's encryption_enabled /
// compression_enabled flags off.
// NOTE: DatasetName and a Description of at least 10 characters are required.
// A zero-value UploadOptions is not a valid upload. UploadDataset returns an
// error, before any network call, when Description is shorter than 10 characters
// after surrounding spaces are trimmed, unless DatasetOverrides sets "description".
type UploadOptions struct {
	Category string
	// Deprecated: ignored. Every upload is compressed regardless of this field,
	// so false is fine. It stays so existing code keeps compiling. Zero-value
	// options are still not enough: DatasetName and a Description of at least 10
	// characters are required.
	Compress         bool
	CompressionLevel int // Default: 6 (compression level 1-9)
	DataFreshness    types.DataFreshness
	DatasetName      string
	Description      string
	// Deprecated: ignored. Every upload is encrypted regardless of this field,
	// so false is fine. It stays so existing code keeps compiling. Zero-value
	// options are still not enough: DatasetName and a Description of at least 10
	// characters are required.
	Encrypt          bool
	Metadata         map[string]any
	DatasetOverrides map[string]any
}

// NewUploadOptions creates UploadOptions with sane defaults.
//
// NOTE: This is the recommended way to create upload options.
func NewUploadOptions(datasetName string) UploadOptions {
	return UploadOptions{
		DatasetName:      datasetName,
		Category:         "general",
		DataFreshness:    types.DataFreshnessDaily,
		Encrypt:          true,
		Compress:         true,
		CompressionLevel: 6,
	}
}

// NewProducer creates a new Producer instance.
//
// TODO: Allow to pass context for better control.
func NewProducer(cfg types.Config) (*Producer, error) {
	// Basic validation.
	if cfg.APIEndpoint == "" {
		envEndpoint := strings.TrimSpace(os.Getenv("HELIX_API_ENDPOINT"))
		if envEndpoint != "" {
			cfg.APIEndpoint = envEndpoint
		} else {
			cfg.APIEndpoint = "https://api-go.helix.tools"
		}
	}

	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	// Select the AWS credentials provider: "static" (default, byte-identical
	// to the pre-existing behavior) or "sts" (auto-refreshing broker-issued
	// session credentials, opt-in via cfg.CredentialMode). See
	// credentials.SelectProvider for the full mode-inference matrix.
	credProvider, err := stscreds.SelectProvider(cfg.APIEndpoint, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to select AWS credentials provider: %w", err)
	}
	isStaticMode := isStaticCredentialsProvider(credProvider)

	// Bounds the start-up identity check so a connection that is accepted but
	// never answered reports the same temporary-failure error as NewConsumer
	// (consumer/consumer.go's awsHTTPClient) instead of blocking indefinitely.
	awsHTTPClient := &http.Client{
		Timeout: 25 * time.Second,
	}

	// Load AWS config.
	awsCfg, err := loadAWSConfig(context.Background(), cfg.Region, credProvider, awsHTTPClient)
	if err != nil {
		return nil, err
	}

	// Validate credentials.
	stsClient := sts.NewFromConfig(awsCfg)
	if err := validateCredentials(context.Background(), stsClient, strings.TrimSpace(cfg.APIKey) != "", isStaticMode); err != nil {
		return nil, err
	}

	p := &Producer{
		APIEndpoint: cfg.APIEndpoint,
		CustomerID:  cfg.CustomerID,
		Region:      cfg.Region,

		awsConfig:     awsCfg,
		httpClient:    &http.Client{},
		kmsClient:     kms.NewFromConfig(awsCfg),
		s3Client:      s3.NewFromConfig(awsCfg),
		storageClient: transferclient.New(),
	}

	// Get the producer's encryption key id from the API. Without one the
	// Producer is still built (non-upload calls work) but every UploadDataset
	// call fails: encryption is never skipped. A failure here that got no
	// definitive answer (a transient outage, say) is not permanent: the
	// next upload retries it instead of failing closed until a new Producer
	// is constructed (see ensureEncryptionKeyID). keyLookupAttempted is set
	// here, before p is shared with anything else, so ensureEncryptionKeyID
	// knows this call must actually ask the API rather than treating it as
	// a hand-built Producer that never attempted a lookup at all.
	p.keyLookupAttempted = true
	if _, _, err := p.ensureEncryptionKeyID(context.Background()); err != nil {
		fmt.Printf("Warning: %v\n", err)
	}

	return p, nil
}

// producerConfigPath is the API route that returns the calling producer's own
// upload configuration.
const producerConfigPath = "/v1/self/producer-config"

// producerConfigTimeout bounds the producer-config call NewProducer makes, so
// an unreachable API delays construction instead of hanging it.
var producerConfigTimeout = 30 * time.Second

// newGCMWithNonceSize builds the AEAD for encryptData. It is a variable only so
// a test can drive the cipher-setup failure branch, which cannot fail for the
// fixed 16-byte nonce.
var newGCMWithNonceSize = cipher.NewGCMWithNonceSize

// maxProducerConfigBytes caps how much of the producer-config answer is read.
const maxProducerConfigBytes = 64 << 10

// errEncryptionKeyUnresolved is what a producer without an encryption key is
// told: uploads fail, they are never sent unencrypted.
const errEncryptionKeyUnresolved = "encryption key configuration could not be resolved, uploads will fail until it is"

// errKeyLookupNoResponse marks a resolveEncryptionKeyID failure that got no
// definitive answer from the API: a network/transport failure, a timeout, a
// non-200 status other than 404, a malformed or oversized body, a redirect,
// or a clean 200 whose encryption_key_id is missing, empty, or blank — the
// account's key may simply not be provisioned yet, so this is never treated
// as permanent. A lookup marked with it is never cached — ensureEncryptionKeyID
// retries it on the next upload instead of treating a transient or
// not-yet-provisioned state as permanent. The only failure NOT marked with
// it is a 404: that is the API's definitive answer that this account
// genuinely has no key configured, and it is cached like any other resolved
// answer.
var errKeyLookupNoResponse = errors.New("encryption key lookup got no definitive answer")

// producerConfig is the body of GET /v1/self/producer-config.
type producerConfig struct {
	EncryptionKeyID string `json:"encryption_key_id"`
}

// resolveEncryptionKeyID asks the API for this producer's encryption key id.
// Any failure (an answer other than a direct 200, a body that is not exactly
// one JSON object, an empty value) returns
// an empty key and an error that says plainly what it means: uploads fail. The
// underlying cause stays reachable via errors.Unwrap/errors.As.
func (p *Producer) resolveEncryptionKeyID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, producerConfigTimeout)
	defer cancel()

	resp, err := p.sendSignedRequest(ctx, http.MethodGet, producerConfigPath, nil)
	if err != nil {
		return "", sdkerr.WrapMarked(errEncryptionKeyUnresolved, errKeyLookupNoResponse, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap so an oversized answer is refused rather
	// than silently truncated into something that parses.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProducerConfigBytes+1))
	if err != nil {
		return "", sdkerr.WrapMarked(errEncryptionKeyUnresolved, errKeyLookupNoResponse, err)
	}
	if len(raw) > maxProducerConfigBytes {
		return "", sdkerr.WrapMarked(errEncryptionKeyUnresolved, errKeyLookupNoResponse, errors.New("producer configuration answer is too large"))
	}

	// A 404 is the API's definitive answer that this account has no
	// encryption key configured. Cached below like any other resolved
	// answer and never retried; the underlying APIError (never surfaced
	// in Error()) stays reachable via errors.As for debugging.
	if resp.StatusCode == http.StatusNotFound {
		return "", sdkerr.Wrap(errEncryptionKeyUnresolved, &APIError{StatusCode: resp.StatusCode, Body: string(raw)})
	}

	// Only the route's own 200 answer counts: not another 2xx, and not an
	// answer reached by following a redirect.
	if resp.StatusCode != http.StatusOK {
		return "", sdkerr.WrapMarked(errEncryptionKeyUnresolved, errKeyLookupNoResponse, &APIError{StatusCode: resp.StatusCode, Body: string(raw)})
	}
	if resp.Request != nil && resp.Request.Response != nil {
		return "", sdkerr.WrapMarked(errEncryptionKeyUnresolved, errKeyLookupNoResponse, errors.New("producer configuration request was redirected"))
	}

	// json.Unmarshal, unlike a streaming decode, refuses trailing data after
	// the object.
	var cfg producerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", sdkerr.WrapMarked(errEncryptionKeyUnresolved, errKeyLookupNoResponse, err)
	}

	// A clean 200 with a missing, empty, or blank key is NOT the same as a
	// 404: the API answered, but the account's key may simply not be
	// provisioned yet. Marked with errKeyLookupNoResponse like any other
	// non-definitive failure, so ensureEncryptionKeyID retries it on the
	// next upload instead of caching it as permanent.
	if strings.TrimSpace(cfg.EncryptionKeyID) == "" {
		return "", sdkerr.WrapMarked(errEncryptionKeyUnresolved, errKeyLookupNoResponse, errors.New("producer configuration answer carries no encryption key"))
	}

	return cfg.EncryptionKeyID, nil
}

// ensureEncryptionKeyID returns the producer's encryption key id, resolving
// it via resolveEncryptionKeyID when needed. A successful lookup, or a
// definitive "no key configured" answer (a 404), is cached on p and never
// looked up again. Any other failure (network, timeout, non-200 other than
// 404, malformed/oversized body, redirect, or a clean 200 with a missing,
// empty, or blank key) is NOT cached: the next call retries it, so a
// transient outage — or an account whose key simply is not provisioned yet
// — does not fail every upload forever.
//
// Concurrent callers while a lookup is running share its one result instead
// of each firing their own request at the API. The lookup itself runs in its
// own goroutine (started by whichever caller finds none already in flight),
// on a context derived from that caller's own ctx via context.WithoutCancel
// — preserving any request-scoped values it carries — further bounded by
// resolveEncryptionKeyID's own producerConfigTimeout. No caller's
// cancellation can ever cancel or poison the shared lookup for the others
// waiting on it. Each caller waits on its own ctx instead: if ctx is done
// first, ensureEncryptionKeyID returns ctx.Err() promptly without waiting
// for the lookup to finish, leaving the lookup to keep running for whoever
// else is still waiting (or to be found cached by the next caller).
//
// A Producer built directly, bypassing NewProducer (as production code never
// does but tests do), never has keyLookupAttempted set: it is reported as a
// definitive non-key exactly like before this method existed, with no
// network call, so a hand-built Producer with no APIEndpoint/httpClient
// never panics or reaches out to an API it was never configured to call.
func (p *Producer) ensureEncryptionKeyID(ctx context.Context) (keyID string, definitiveNoKey bool, err error) {
	p.keyLookupMu.Lock()

	if p.KMSKeyID != "" {
		keyID := p.KMSKeyID
		p.keyLookupMu.Unlock()
		return keyID, false, nil
	}
	if p.keyLookupDefinitiveNoKey || !p.keyLookupAttempted {
		p.keyLookupMu.Unlock()
		return "", true, errors.New(errEncryptionKeyUnresolved)
	}

	call := p.keyLookupInFlight
	if call == nil {
		call = &keyLookupCall{done: make(chan struct{})}
		p.keyLookupInFlight = call
		// context.WithoutCancel(ctx) carries this caller's request-scoped
		// values into the shared lookup without its cancellation (or any
		// other waiter's) ever canceling or poisoning it — that is the bug
		// this function exists to fix, see its doc comment. It cannot leak
		// or accumulate: at most one runs at a time (single-flight, guarded
		// by keyLookupMu) and resolveEncryptionKeyID already bounds it with
		// producerConfigTimeout.
		go p.runKeyLookup(call, context.WithoutCancel(ctx))
	}
	p.keyLookupMu.Unlock()

	select {
	case <-call.done:
		return call.keyID, call.definitiveNoKey, call.err
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

// runKeyLookup performs one shared encryption-key lookup for call and
// publishes its result by closing call.done. lookupCtx is the triggering
// caller's own ctx with context.WithoutCancel already applied (see
// ensureEncryptionKeyID), so request-scoped values travel with it while it
// runs to completion — bounded only by resolveEncryptionKeyID's own
// producerConfigTimeout — regardless of whether any (or all) of the callers
// waiting on call have since had their own ctx canceled.
func (p *Producer) runKeyLookup(call *keyLookupCall, lookupCtx context.Context) {
	resolvedKeyID, lookupErr := p.resolveEncryptionKeyID(lookupCtx)
	definitive := lookupErr != nil && !errors.Is(lookupErr, errKeyLookupNoResponse)

	p.keyLookupMu.Lock()
	p.keyLookupInFlight = nil
	switch {
	case lookupErr == nil:
		p.KMSKeyID = resolvedKeyID
	case definitive:
		p.keyLookupDefinitiveNoKey = true
	}
	p.keyLookupMu.Unlock()

	call.keyID, call.definitiveNoKey, call.err = resolvedKeyID, definitive, lookupErr
	close(call.done)
}

// loadAWSConfig wraps config.LoadDefaultConfig, an AWS SDK call: on failure
// the customer sees a clean, capability-language message while the raw AWS
// error (which can carry local shared-config-file paths or profile detail)
// stays reachable via errors.Unwrap/errors.As for debugging.
func loadAWSConfig(ctx context.Context, region string, credProvider aws.CredentialsProvider, httpClient aws.HTTPClient) (aws.Config, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credProvider),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		return aws.Config{}, sdkerr.Wrap("failed to load AWS config", err)
	}
	return awsCfg, nil
}

// validateCredentials makes one identity-check call to AWS, to fail fast on
// bad credentials. On failure the customer sees a clean message while the raw
// underlying error (which can carry internal account details) stays reachable
// via errors.Unwrap/errors.As for debugging.
//
// In sts mode the call first gets session credentials from the Helix
// credential service, so a failure there is reported as what it is — the
// service could not be reached, or it returned one of its customer-facing
// messages (e.g. a revoked API key) — never as "invalid AWS credentials",
// which would mislead an API-key caller who configured no AWS keys at all.
// Any other error the service answered with (a 5xx, or a 4xx with no mapped
// message) is reported as a credential service error carrying the service's
// scrubbed code and message. The one exception is a 401/403 to a static-key
// caller (apiKeyConfigured false): the service rejected their AWS keys, so
// "invalid AWS credentials" is the right advice for them. Any other failure
// for an API-key caller (e.g. an unusable answer from the service, or its
// credentials being rejected) still names the credential service.
//
// isStaticMode is true only for the legacy static-credentials bootstrap —
// the aws.CredentialsProvider SelectProvider returned was a plain
// credentials.StaticCredentialsProvider, never a broker. Only then does a
// failure that means the identity check itself could not complete — a
// canceled/expired context, or a response the cloud SDK reports as a
// retryable HTTP status (via *smithyhttp.ResponseError: 408, 429, or
// 500-599) — get classified as a temporary failure instead of "invalid AWS
// credentials": a provider outage is not the same thing as a credential
// rejection. That classification never applies in broker/sts mode: there,
// any failure other than a *stscreds.MintError or
// ErrCredentialServiceUnreachable — whether it came from the broker mint
// call or from the GetCallerIdentity call itself, including a canceled
// context or a retryable status — falls straight through to today's
// unchanged fallback below, byte-for-byte as on origin/main.
//
// The temporary-failure message itself carries no raw upstream detail:
// nothing from it is reachable via errors.Unwrap onto the real cause, or
// fmt's "%+v". But two caller-checkable relationships that existed before
// this classification was added are preserved regardless:
// errors.Is(err, context.Canceled) / errors.Is(err, context.DeadlineExceeded)
// still report true when that is why the check failed, and
// errors.As(err, &respErr) for a *smithyhttp.ResponseError still matches —
// populated with only the HTTP status code, never the real response's body,
// headers, or wrapped error. A definitive answer (401/403, or anything else
// the identity check rejected the request with) keeps today's behavior,
// cause included, unchanged.
func validateCredentials(ctx context.Context, stsClient *sts.Client, apiKeyConfigured, isStaticMode bool) error {
	if _, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
			return sdkerr.WrapSentinel(sdkerr.ErrCredentialServiceUnreachable, err)
		}
		var mintErr *stscreds.MintError
		if errors.As(err, &mintErr) {
			if mintErr.Friendly {
				return sdkerr.Wrap(mintErr.Message, err)
			}
			rejectedAWSKeys := !apiKeyConfigured &&
				(mintErr.StatusCode == http.StatusUnauthorized || mintErr.StatusCode == http.StatusForbidden)
			if !rejectedAWSKeys {
				return sdkerr.Wrap(sdkerr.CredentialServiceMessage(mintErr.StatusCode, mintErr.Code, mintErr.Message), err)
			}
		} else if isStaticMode {
			if errors.Is(err, context.Canceled) {
				// No HTTP response was ever received, so there is no
				// *smithyhttp.ResponseError to find — but context.Canceled
				// itself is attached as a marker, so errors.Is keeps
				// matching it without the raw upstream error ever being
				// reachable.
				return sdkerr.WrapMarkers(sdkerr.IdentityCheckTemporaryFailureMessage(0), context.Canceled)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return sdkerr.WrapMarkers(sdkerr.IdentityCheckTemporaryFailureMessage(0), context.DeadlineExceeded)
			}
			var respErr *smithyhttp.ResponseError
			if errors.As(err, &respErr) && sdkerr.IsRetryableIdentityStatus(respErr.HTTPStatusCode()) {
				return sdkerr.WrapMarkers(
					sdkerr.IdentityCheckTemporaryFailureMessage(respErr.HTTPStatusCode()),
					sanitizedResponseError(respErr.HTTPStatusCode()),
				)
			}
		}
		if apiKeyConfigured {
			return sdkerr.Wrap(sdkerr.KeyCallerServiceFailure, err)
		}
		return sdkerr.Wrap("invalid AWS credentials", err)
	}
	return nil
}

// sanitizedResponseError returns a *smithyhttp.ResponseError carrying only
// statusCode, so a caller's pre-existing errors.As(err, &respErr) for the
// cloud SDK's response-error type keeps matching after a temporary-failure
// classification, without the real response's body, headers, request, or
// wrapped error ever becoming reachable through it. Body and Header are
// non-nil and empty (never the real upstream values) so a caller that reads
// or closes them after errors.As does not hit a nil pointer. Request is a
// non-nil stand-in pointed at a neutral, reserved (RFC 2606) host, so a
// caller that reads Response.Request.URL or .Method — as the real SDK
// response always has both populated — does not hit a nil pointer either.
func sanitizedResponseError(statusCode int) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{
			StatusCode: statusCode,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("")),
			Request: &http.Request{
				Method: http.MethodPost,
				URL:    &url.URL{Scheme: "https", Host: "identity.invalid", Path: "/"},
			},
		}},
	}
}

// isStaticCredentialsProvider reports whether p is the plain
// credentials.StaticCredentialsProvider that SelectProvider returns for the
// legacy static-keys bootstrap — never true for a broker/sts-mode provider.
func isStaticCredentialsProvider(p aws.CredentialsProvider) bool {
	_, ok := p.(credentials.StaticCredentialsProvider)
	return ok
}

// compressData compresses data.
func (p *Producer) compressData(data []byte, level int) ([]byte, error) {
	var buf bytes.Buffer
	gzWriter, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, sdkerr.Wrap("failed to create compression writer", err)
	}

	if _, err := gzWriter.Write(data); err != nil {
		return nil, sdkerr.Wrap("failed to write compressed data", err)
	}

	if err := gzWriter.Close(); err != nil {
		return nil, sdkerr.Wrap("failed to close compression writer", err)
	}

	return buf.Bytes(), nil
}

// encryptData encrypts data before upload.
// Process:
// 1. Generate random data key (32 bytes)
// 2. Encrypt data with the data key
// 3. Protect the data key with the account's encryption key
// 4. Return: [key_length][encrypted_key][iv][tag][encrypted_data]
func (p *Producer) encryptData(ctx context.Context, data []byte) ([]byte, error) {
	if p.KMSKeyID == "" {
		return nil, fmt.Errorf("encryption key not configured for this account, cannot encrypt data")
	}

	if p.kmsClient == nil {
		return nil, errors.New("encryption is not configured; cannot encrypt data")
	}

	// Generate random data key and IV.
	dataKey := make([]byte, 32) // 256-bit key.
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("failed to generate data key: %w", err)
	}

	iv := make([]byte, 16) // 16-byte IV (matches the Python SDK).
	if _, err := rand.Read(iv); err != nil {
		return nil, fmt.Errorf("failed to generate IV: %w", err)
	}

	// Encrypt data with the data key.
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Use 16-byte nonce to match Python's os.urandom(16).
	aesGCM, err := newGCMWithNonceSize(block, 16)
	if err != nil {
		return nil, sdkerr.Wrap("failed to prepare encryption", err)
	}

	// Encrypt the data (auth tag is automatically appended).
	encryptedData := aesGCM.Seal(nil, iv, data, nil)

	// Split encrypted data and auth tag
	authTagSize := aesGCM.Overhead()
	actualEncryptedData := encryptedData[:len(encryptedData)-authTagSize]
	authTag := encryptedData[len(encryptedData)-authTagSize:]

	// Protect the data key with the account's encryption key.
	encryptOutput, err := p.kmsClient.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(p.KMSKeyID),
		Plaintext: dataKey,
	})
	if err != nil {
		return nil, sdkerr.Wrap("encryption failed", sdkerr.SanitizeCause(err))
	}

	// Package: [4 bytes: key length][encrypted key][16 bytes: IV][16 bytes: tag][encrypted data].
	var result bytes.Buffer

	// Write encrypted key length (4 bytes, big-endian).
	keyLength, err := checkedEncryptedKeyLength(uint64(len(encryptOutput.CiphertextBlob)))
	if err != nil {
		return nil, err
	}
	if err := binary.Write(&result, binary.BigEndian, keyLength); err != nil {
		return nil, fmt.Errorf("failed to write key length: %w", err)
	}

	// Write encrypted key.
	result.Write(encryptOutput.CiphertextBlob)

	// Write IV (16 bytes).
	result.Write(iv)

	// Write auth tag (16 bytes).
	result.Write(authTag)

	// Write encrypted data.
	result.Write(actualEncryptedData)

	return result.Bytes(), nil
}

func checkedEncryptedKeyLength(length uint64) (uint32, error) {
	if length > uint64(math.MaxUint32) {
		return 0, fmt.Errorf("encrypted key length %d exceeds uint32 capacity", length)
	}
	return uint32(length), nil
}

// CreateDatasetResponse represents the API response when creating a dataset record.
type CreateDatasetResponse struct {
	ID        string `json:"id"`
	UploadURL string `json:"upload_url"`
	S3Key     string `json:"s3_key"`
}

// ProcessedFileData contains the processed (encrypted/compressed) file data and metadata.
type ProcessedFileData struct {
	Data         []byte
	OriginalSize int64
	Sizes        map[string]any
	Analysis     *AnalysisResult
}

// createDatasetRecord creates a dataset record in the catalog and retrieves a
// presigned upload URL. This is step 2 of the upload flow — it runs AFTER
// processFile (step 1) so the POST body carries the real sizes, version and
// record_count, matching what v1.3.11 sent (producer.go buildDatasetPayload,
// pre-v2). v2.15.0 called this before processFile, so it always POSTed
// zero/absent sizes and an empty version; the caller (UploadDataset) still
// POSTs before any bytes reach storage, so the catalog-record-before-upload race
// protection the original POST-first refactor introduced is unchanged.
func (p *Producer) createDatasetRecord(ctx context.Context, filePath string, opts UploadOptions, processed *ProcessedFileData) (*CreateDatasetResponse, error) {
	// Analyze data (memory-efficient streaming). Independent of processFile's
	// in-memory compress/encrypt pass over the same file, so it runs here
	// regardless of step order.
	var analysis *AnalysisResult
	analysisResult, err := p.analyzeData(filePath, DefaultAnalysisOptions())
	if err != nil {
		fmt.Printf("⚠️  Warning: Data analysis failed, continuing without analysis: %v\n", err)
	} else {
		analysis = analysisResult
	}

	// Build metadata, starting from the caller's own keys.
	metadata := make(map[string]any)
	maps.Copy(metadata, opts.Metadata)

	// Sizes (original/compressed/encrypted_size_bytes) plus
	// encryption_enabled/compression_enabled, taken from the now-completed
	// processFile pass. Matches v1.3.11's "sizes" map verbatim (same key
	// names) instead of the encryption_enabled/compression_enabled-only pair
	// v2.15.0 could set before processing had run.
	//
	// Also record encryption/compression so the CONSUMER download knows to
	// reverse them: Consumer.DownloadDataset reads
	// dataset.Metadata["encryption_enabled"] and ["compression_enabled"] to
	// decide whether to decrypt/decompress. Without these, download returns
	// the raw encrypted+compressed bytes and the round-trip sha256 mismatches.
	// (Found 2026-07-06 by the SDK-only E2E suite — go round-trip corruption
	// once notifications started arriving.) Upload mandates both.
	maps.Copy(metadata, processed.Sizes)

	// file_format / encoding defaults, matching v1.3.11's buildDatasetPayload
	// (only fill if the caller didn't already set them via Metadata).
	if _, exists := metadata["file_format"]; !exists {
		metadata["file_format"] = "json"
	}
	if _, exists := metadata["encoding"]; !exists {
		metadata["encoding"] = "utf-8"
	}

	// record_count defaults to 0, matching v1.3.11's buildDatasetPayload,
	// which always set it (top-level AND in metadata) instead of omitting it
	// when analysis failed.
	recordCount := 0
	if analysis != nil {
		recordCount = analysis.RecordCount
		metadata["schema"] = analysis.Schema
		metadata["field_emptiness"] = analysis.FieldEmptiness
		metadata["record_count"] = analysis.RecordCount
		if analysis.AnalysisErrors > 0 {
			metadata["analysis_errors"] = analysis.AnalysisErrors
		}
	} else {
		metadata["record_count"] = 0
	}

	// s3_key MUST be sent, dataset-NAME-keyed, matching Python/TS
	// (`datasets/{name}/data.ndjson[.gz]`). The API honors a client s3_key and
	// otherwise defaults to `datasets/{producer_id}/{dataset_id}/data`
	// (service.go CreateDataset). That default breaks the whole notify pipeline:
	// the ingestion processor derives dataset_name from the key's FIRST segment,
	// so a producer-id-keyed object yields dataset_name=<producer_id>, the
	// findOneAndUpdate never matches, and subscribers are notified with the
	// wrong name (or not at all). Go always compresses, so the file is
	// `data.ndjson.gz`. (Found 2026-07-06 by the SDK-only E2E suite: go uploads
	// landed under datasets/<customer_id>/ while py/ts used datasets/<name>/.)
	s3Key := fmt.Sprintf("datasets/%s/data.ndjson.gz", opts.DatasetName)

	// version defaults to today's UTC date, matching v1.3.11's
	// buildDatasetPayload (`now.Format("2006-01-02")`), computed
	// unconditionally so a caller who never touches DatasetOverrides still
	// gets a non-empty version instead of the "" v2.15.0 sent.
	version := time.Now().UTC().Format("2006-01-02")

	// Build dataset payload, now WITH size, version and record_count.
	// access_tier is REQUIRED by the create validator (an access_tier not in
	// {free,premium,enterprise} is rejected); the Python/TS SDKs send it too.
	// It defaults to "free" (the only live tier) and stays overridable via
	// DatasetOverrides below.
	//
	// s3_bucket_name is deliberately NOT sent: the platform owns the upload
	// destination and resolves it server-side, matching Python/TS. A caller
	// who passes it explicitly in DatasetOverrides still has it sent
	// untouched, and the API validates it.
	//
	// visibility is sent explicitly as "private" to match Python/TS (both send
	// it on create) even though the server defaults an empty/absent visibility
	// to "private" itself (datasets/service.go CreateDataset) — Go silently
	// relying on that default, while the other two SDKs send it, was a latent
	// cross-SDK payload divergence. Stays overridable via DatasetOverrides.
	// size_bytes MUST be sent top-level: v1.3.11 sent it
	// (dataset_payload.go:168, "size_bytes": finalSize) and the API maps
	// request size_bytes to the catalog's total_size_bytes
	// (datasets/service.go CreateDataset, ~line 699). v2.16.0 dropped it,
	// which zeroed production total_size_bytes. len(processed.Data) is the
	// exact byte length of the object about to be uploaded to storage (compressed,
	// then encrypted — both mandatory), so it always matches
	// metadata.encrypted_size_bytes.
	// encryption MUST be sent top-level, matching Python (producer.py
	// _create_dataset_record, "encryption": encryption_enabled) and TypeScript
	// (datasetSchema.ts buildDatasetPayload, encryption:
	// metadataPayload.encryption_enabled || false). The create endpoint's
	// extractBool (api service.go) drops metadata.encryption_enabled from the
	// stored record whenever the request has no top-level "encryption" field,
	// so a request that only sets the metadata key ends up with a record that
	// is silently missing it — found 2026-09-26 comparing Go's stored
	// record against Python/TS's for an identical upload. Always true: Encrypt
	// cannot be false (validateUploadOptions), so this mirrors the same
	// invariant as pinnedMetadata's encryption_enabled below, not a new one.
	payload := map[string]any{
		"name":           opts.DatasetName,
		"description":    opts.Description,
		"category":       opts.Category,
		"data_freshness": string(opts.DataFreshness),
		"producer_id":    p.CustomerID,
		"s3_key":         s3Key,
		"access_tier":    "free",
		"visibility":     "private",
		"version":        version,
		"record_count":   recordCount,
		"size_bytes":     int64(len(processed.Data)),
		"encryption":     true,
		"metadata":       metadata,
	}

	// Merge dataset overrides — a caller's explicit value (including an
	// explicit version, even "") always wins over the computed default
	// above, matching v1.3.11's deepMergeMaps(payload, overrideCopy).
	if opts.DatasetOverrides != nil {
		maps.Copy(payload, opts.DatasetOverrides)
	}

	// A "metadata" override REPLACES the computed metadata wholesale, which
	// would drop the two flags that tell every consumer this dataset is
	// encrypted and compressed. They are pinned after the merge, so the record
	// carries them whatever the overrides did (UploadDataset has already
	// refused overrides that try to set them to anything but true).
	pinnedMetadata, err := metadataObject(payload["metadata"])
	if err != nil {
		return nil, err
	}
	pinnedMetadata["encryption_enabled"] = true
	pinnedMetadata["compression_enabled"] = true
	payload["metadata"] = pinnedMetadata

	// POST to /v1/datasets to create record and get presigned URL
	var response CreateDatasetResponse
	err = p.makeAPIRequest(ctx, "POST", "/v1/datasets", payload, &response)
	if err != nil {
		return nil, fmt.Errorf("failed to create dataset record: %w", err)
	}

	return &response, nil
}

// processFile reads, compresses, and encrypts the file data.
// This is step 1 of the upload flow — it runs BEFORE createDatasetRecord so
// the real sizes are known when the POST body is built. It has no network
// side effect other than one local encryption step; it never uploads anything.
//
// It is the single enforcement point for the upload invariant (see
// validateUploadOptions): a call that would switch encryption or compression
// off, or that has no encryption key, fails here, before the file is read or
// anything reaches the network.
func (p *Producer) processFile(ctx context.Context, filePath string, opts UploadOptions) (*ProcessedFileData, error) {
	if err := p.validateUploadOptions(ctx, opts); err != nil {
		return nil, err
	}

	safeFilePath, err := cleanContainedPath(filePath)
	if err != nil {
		return nil, fmt.Errorf("invalid upload file path: %w", err)
	}

	// Read original file
	data, err := os.ReadFile(filepath.Clean(safeFilePath))
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	originalSize := int64(len(data))

	// Validate file is not empty
	if originalSize == 0 {
		return nil, fmt.Errorf("file is empty: %s (no data to upload)", filePath)
	}

	// Step 1: Compress FIRST
	fmt.Printf("📦 Compressing %d bytes (level %d)...\n", len(data), opts.CompressionLevel)

	compressed, err := p.compressData(data, opts.CompressionLevel)
	if err != nil {
		return nil, fmt.Errorf("compression failed: %w", err)
	}

	compressedSize := int64(len(compressed))
	compressionRatio := (1 - float64(compressedSize)/float64(originalSize)) * 100
	fmt.Printf("Compressed: %d bytes (%.1f%% reduction)\n", compressedSize, compressionRatio)

	// Step 2: Encrypt SECOND
	fmt.Printf("🔒 Encrypting %d bytes...\n", compressedSize)

	encrypted, err := p.encryptData(ctx, compressed)
	if err != nil {
		return nil, fmt.Errorf("encryption failed: %w", err)
	}

	fmt.Printf("Encrypted: %d bytes\n", len(encrypted))

	return &ProcessedFileData{
		Data:         encrypted,
		OriginalSize: originalSize,
		Sizes: map[string]any{
			"original_size_bytes":   originalSize,
			"compressed_size_bytes": compressedSize,
			"encrypted_size_bytes":  int64(len(encrypted)),
			"encryption_enabled":    true,
			"compression_enabled":   true,
		},
	}, nil
}

// flagKeysThatMustStayOn are the record fields that say a dataset is
// encrypted / compressed. Nothing a caller passes may set them to anything
// but true.
var flagKeysThatMustStayOn = []string{"encryption", "encryption_enabled", "compression", "compression_enabled"}

// minDescriptionLength is the API's minimum dataset description length, counted
// after surrounding spaces are trimmed (the create-dataset rule).
const minDescriptionLength = 10

// validateUploadOptions enforces the upload invariant: every upload is
// compressed and then encrypted, and no option turns either off. It runs
// before the file is read, and it refuses:
//   - a Producer without an encryption key: if a prior lookup (at
//     construction or a previous upload) got no definitive answer, this
//     retries it — a transient outage does not fail every upload forever —
//     but a Producer built directly with no lookup ever attempted, or one
//     the API has definitively said has no key configured, fails this
//     check locally, with no network call,
//   - Metadata or DatasetOverrides — top-level or under "metadata" — that set
//     one of the record's encryption/compression flags to anything but true,
//     or that give "metadata" as something other than an object,
//   - a Description shorter than minDescriptionLength after trimming, unless
//     DatasetOverrides sets "description" (that value is what gets sent, and the
//     API checks it).
//
// UploadOptions.Encrypt and UploadOptions.Compress are deprecated and ignored,
// so leaving them false is accepted.
func (p *Producer) validateUploadOptions(ctx context.Context, opts UploadOptions) error {
	// Always routed through ensureEncryptionKeyID — never a direct,
	// unsynchronized read of p.KMSKeyID — because a concurrent upload can be
	// caching a just-resolved key under its lock at the same moment.
	if _, definitiveNoKey, err := p.ensureEncryptionKeyID(ctx); err != nil {
		if definitiveNoKey {
			return errors.New("encryption requested but no encryption key configured for this account")
		}
		return err
	}

	if err := rejectDisabledFlags("UploadOptions.Metadata", opts.Metadata); err != nil {
		return err
	}

	if err := rejectDisabledFlags("UploadOptions.DatasetOverrides", opts.DatasetOverrides); err != nil {
		return err
	}

	_, overridden := opts.DatasetOverrides["description"]
	if !overridden && len(strings.TrimSpace(opts.Description)) < minDescriptionLength {
		return &ValidationError{Field: "description", Message: fmt.Sprintf("must be at least %d characters", minDescriptionLength)}
	}

	if raw, present := opts.DatasetOverrides["metadata"]; present {
		metadata, err := metadataObject(raw)
		if err != nil {
			return err
		}

		return rejectDisabledFlags(`UploadOptions.DatasetOverrides["metadata"]`, metadata)
	}

	return nil
}

// rejectDisabledFlags fails when m carries one of flagKeysThatMustStayOn with
// any value other than the boolean true (false, "false", 0, null, ...). A key
// that only differs from a flag's name by case or surrounding whitespace
// ("Encryption_Enabled", "compression_enabled ") is refused whatever its value:
// a JSON decoder that folds case would treat it as the flag itself, so it is
// never a way to say something else about it.
func rejectDisabledFlags(where string, m map[string]any) error {
	for key, value := range m {
		for _, flag := range flagKeysThatMustStayOn {
			if !strings.EqualFold(strings.TrimSpace(key), flag) {
				continue
			}

			if key != flag {
				return fmt.Errorf("%s: key %q is a variant spelling of %q; use exactly %q — every upload is encrypted and compressed", where, key, flag, flag)
			}

			if value != true {
				return fmt.Errorf("%s: %q cannot be disabled — every upload is encrypted and compressed (got %v)", where, key, value)
			}
		}
	}

	return nil
}

// metadataObject returns a private copy of v as a JSON object. nil is an empty
// object; anything that is not an object (a string, a number, a list) is an
// error. A typed map or a struct is read through its JSON form, so the flags
// cannot hide inside a type the caller chose.
func metadataObject(v any) (map[string]any, error) {
	if m, ok := v.(map[string]any); ok {
		return maps.Clone(m), nil
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf(`"metadata" must be an object: %w`, err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf(`"metadata" must be an object: %w`, err)
	}

	if m == nil {
		m = map[string]any{}
	}

	return m, nil
}

// storageHTTPClient returns the client used for a direct transfer to
// storage: no total-duration cap, only a connect timeout and an inactivity
// timeout (see internal/transferclient). Falls back to httpClient when
// storageClient is nil — a Producer built directly, bypassing NewProducer —
// so a hand-built Producer behaves as it always has.
func (p *Producer) storageHTTPClient() *http.Client {
	if p.storageClient != nil {
		return p.storageClient
	}
	return p.httpClient
}

// uploadToPresignedURL uploads the processed data to the presigned URL.
// This is step 3 of the new POST-first upload flow.
func (p *Producer) uploadToPresignedURL(ctx context.Context, uploadURL string, data []byte) error {
	fmt.Printf("📤 Uploading %d bytes to presigned URL...\n", len(data))

	// Wrapping the body lets the storage client tell the moment the whole
	// body has been handed off to net/http, and wait longer for storage's
	// response from that point on than it waits for in-flight inactivity —
	// see transferclient.WrapUploadBody's doc comment for why that distinct,
	// longer wait exists.
	uploadCtx, body := transferclient.WrapUploadBody(ctx, bytes.NewReader(data))
	req, err := http.NewRequestWithContext(uploadCtx, "PUT", uploadURL, body)
	if err != nil {
		// uploadURL is a server-issued presigned URL carrying a SigV4
		// signature/credential scope in its query string; a malformed
		// version of it must not reach the caller via the raw *url.Error
		// http.NewRequestWithContext returns, so SanitizeCause is applied
		// explicitly (case (b) of its doc comment).
		return sdkerr.Wrap("failed to create upload request", sdkerr.SanitizeCause(err))
	}

	// Set content type for binary data
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))

	resp, err := p.storageHTTPClient().Do(req)
	if resp != nil {
		// Do/RoundTrip returning a non-nil Response means storage's status
		// line and headers are fully read — exactly the event the storage
		// client's fixed post-handoff response window is waiting on. Tell
		// it so now, before any read of resp.Body below, so that read (if
		// any) is bound by the normal inactivity timeout again instead of
		// the fixed window that only ever governed this preceding wait —
		// see transferclient.MarkResponseHeadersReceived's doc comment.
		transferclient.MarkResponseHeadersReceived(uploadCtx)
	}
	if err != nil {
		if resp != nil {
			// net/http pairs a non-nil Response with a non-nil error only
			// when a CheckRedirect callback refuses to continue: the
			// service DID answer, so this is not a genuine no-response
			// transport failure and err's real type/fields must stay
			// reachable exactly as they would have before SanitizeCause
			// existed. net/http has already closed resp.Body itself
			// before returning in this case (see Client.Do's doc), so
			// closing it again here would double-Close it.
			return sdkerr.Wrap("failed to upload to presigned URL", err)
		}
		// No response arrived at all (DNS, connection, TLS, timeout): a
		// genuine no-response transport failure, case (a) of
		// SanitizeCause's doc comment.
		return sdkerr.Wrap("failed to upload to presigned URL", sdkerr.SanitizeCause(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	fmt.Printf("✅ Upload successful\n")
	return nil
}

// UploadDataset uploads a dataset, always compressed and then encrypted.
// FLOW (process-before-POST, still catalog-record-before-storage-upload):
//  1. Process file (compress + encrypt) — no upload yet, so the real sizes
//     are known.
//  2. POST to /v1/datasets with those sizes plus version/record_count/
//     metadata to create the record and get a presigned URL.
//  3. PUT the processed bytes to the presigned URL.
//  4. GET the dataset record and return it.
//
// Step 2 still happens before any bytes reach storage, so the race the original
// POST-first refactor closed (a storage event firing before the catalog record
// exists) stays closed. A refused POST still means zero PUTs — step 1 has no
// side effect beyond one local compress and one encryption call.
//
// NOTE: Use NewUploadOptions() to get sane defaults.
func (p *Producer) UploadDataset(ctx context.Context, filePath string, opts UploadOptions) (*types.Dataset, error) {
	// Set defaults for fields not specified
	if opts.Category == "" {
		opts.Category = "general"
	}

	if opts.DataFreshness == "" {
		opts.DataFreshness = types.DataFreshnessDaily
	}

	if opts.CompressionLevel == 0 {
		opts.CompressionLevel = 6
	}

	// Step 1: Process file (compress + encrypt) so the real sizes are known
	// before the POST. processFile refuses — before reading the file or making
	// any network call — every option combination that would skip either step.
	processedData, err := p.processFile(ctx, filePath, opts)
	if err != nil {
		return nil, err
	}

	// Step 2: Create dataset record — now WITH the real sizes/version/
	// record_count/metadata — and get a presigned URL.
	createResp, err := p.createDatasetRecord(ctx, filePath, opts, processedData)
	if err != nil {
		return nil, err
	}

	fmt.Printf("✅ Dataset record created: %s\n", createResp.ID)

	// Step 3: Upload to presigned URL
	if err := p.uploadToPresignedURL(ctx, createResp.UploadURL, processedData.Data); err != nil {
		return nil, fmt.Errorf("dataset record created but upload failed: %w", err)
	}

	// Step 4: Return dataset (fetch updated record from API)
	dataset := &types.Dataset{}
	err = p.makeAPIRequest(ctx, "GET", fmt.Sprintf("/v1/datasets/%s", url.PathEscape(createResp.ID)), nil, dataset)
	if err != nil {
		// If GET fails, construct a basic dataset response
		fmt.Printf("⚠️  Warning: Failed to fetch dataset details: %v\n", err)
		return &types.Dataset{
			ID:           createResp.ID,
			IDAlias:      createResp.ID,
			Name:         opts.DatasetName,
			Description:  opts.Description,
			ProducerID:   p.CustomerID,
			Category:     opts.Category,
			S3Key:        createResp.S3Key,
			S3BucketName: p.BucketName,
			S3Bucket:     p.BucketName,
		}, nil
	}

	return dataset, nil
}

// makeAPIRequest makes an authenticated API request.
func (p *Producer) makeAPIRequest(ctx context.Context, method, path string, body, response any) error {
	resp, err := p.sendSignedRequest(ctx, method, path, body)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)

		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	if response != nil {
		if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
			// json.Decoder does not wrap a Read error from resp.Body — a
			// connection reset mid-response surfaces here exactly as it
			// would from resp.Body.Read directly.
			return sdkerr.Wrap("failed to decode response", err)
		}
	}

	return nil
}

// sendSignedRequest builds, SigV4-signs and sends an API request. The caller
// owns the returned response and must close its body.
func (p *Producer) sendSignedRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	apiURL, err := url.Parse(p.APIEndpoint + path)
	if err != nil {
		return nil, fmt.Errorf("invalid API URL: %w", err)
	}

	var (
		reqBody  io.Reader
		jsonData []byte
	)

	if body != nil {
		var err error
		jsonData, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}

		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, apiURL.String(), reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// SigV4 ignores User-Agent when building its signed-headers set (see
	// aws-sdk-go-v2's signer/internal/v4.IgnoredHeaders), so setting it
	// before signing is safe — TestMakeAPIRequest_UserAgentNotInSignedHeaders
	// pins that it never leaks into SignedHeaders regardless.
	req.Header.Set("User-Agent", useragent.String())

	// Sign request with AWS SigV4.
	creds, err := p.awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, sdkerr.Wrap("failed to retrieve credentials", err)
	}

	// Calculate payload hash for SigV4.
	var payloadHash string

	if body != nil {
		// Hash the actual JSON body.
		h := crypto.SHA256.New()

		h.Write(jsonData)

		payloadHash = fmt.Sprintf("%x", h.Sum(nil))
	} else {
		// Empty payload hash for GET requests.
		payloadHash = types.EmptyPayloadHash
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, "execute-api", p.Region, time.Now()); err != nil {
		return nil, sdkerr.Wrap("failed to sign request", err)
	}

	// Execute request.
	resp, err := p.httpClient.Do(req)
	if err != nil {
		if resp != nil {
			// net/http pairs a non-nil Response with a non-nil error only
			// when a CheckRedirect callback refuses to continue: the
			// service DID answer, so this is not a genuine no-response
			// transport failure and err's real type/fields must stay
			// reachable exactly as they would have before SanitizeCause
			// existed. net/http has already closed resp.Body itself
			// before returning in this case (see Client.Do's doc), so
			// closing it again here would double-Close it.
			return nil, sdkerr.Wrap("request failed", err)
		}
		// No response arrived at all: a genuine no-response transport
		// failure, case (a) of SanitizeCause's doc comment.
		return nil, sdkerr.Wrap("request failed", sdkerr.SanitizeCause(err))
	}

	return resp, nil
}

// maxListPages caps how many pages paginateAll will follow for a single
// list call, so a server whose total_pages disagrees with reality (or lies)
// can't make a list method loop forever. A list that still has more pages
// after maxListPages non-empty pages is an error, never a silently
// truncated result.
const maxListPages = 1000

// paginateAll drives fetchPage across pages 1..N, appending each page's
// items in order. It stops at the server-reported totalPages or at the
// first page that comes back with zero items, whichever happens first —
// the empty-page check is what keeps a server that misreports totalPages
// (e.g. always claims more pages than it actually has) from ever being
// trusted past its real data.
//
// fetchPage also returns the page number the server actually served
// (respPage), nil when the response omits that field. A server that
// ignores the requested ?page and keeps re-serving the same page (instead
// of honestly reporting it) would otherwise make this loop append the same
// items over and over until totalPages is reached — paginateAll rejects
// that mismatch instead of silently duplicating data.
//
// A response that omits page entirely is only trusted on a single-page
// result (totalPages <= 1, where there is nothing to be ambiguous about).
// Once more than one page is being followed, an omitted page is treated
// the same as a mismatch: the real API always echoes the page it served,
// so a multi-page response missing that field means a server that can't be
// trusted to be advancing either — accepting it silently would let a
// page-ignoring server return the first page N times over with no error.
//
// The first response also locks the pagination shape for the whole call:
// its totalPages value, and whether it carried the page field. Every later
// response must match both, or paginateAll errors instead of appending —
// a server that reports total_pages=3 on page 1 and then total_pages=1
// while omitting page on page 2 would otherwise dodge the checks above
// (each response is self-consistent on its own) and silently re-serve the
// same page under a shape that looks like a valid single page.
//
// If the server still reports more pages after maxListPages non-empty
// pages, paginateAll returns an error instead of the rows collected so far.
func paginateAll[T any](fetchPage func(page int) (items []T, respPage *int, totalPages int, err error)) ([]T, error) {
	all := []T{}

	var (
		lockedTotalPages  int
		lockedPagePresent bool
	)

	for page := 1; page <= maxListPages; page++ {
		items, respPage, totalPages, err := fetchPage(page)
		if err != nil {
			return nil, err
		}

		if page == 1 {
			lockedTotalPages = totalPages
			lockedPagePresent = respPage != nil
		} else if totalPages != lockedTotalPages {
			return nil, fmt.Errorf("list pagination: server reported total_pages=%d on page 1 but total_pages=%d on page %d", lockedTotalPages, totalPages, page)
		} else if (respPage != nil) != lockedPagePresent {
			return nil, fmt.Errorf("list pagination: server's page field presence on page %d (present=%v) no longer matches page 1 (present=%v)", page, respPage != nil, lockedPagePresent)
		}

		switch {
		case respPage != nil && *respPage != page:
			return nil, fmt.Errorf("list pagination: requested page %d but server returned page %d", page, *respPage)
		case respPage == nil && totalPages > 1:
			return nil, fmt.Errorf("list pagination: requested page %d of %d but server response omitted the page field", page, totalPages)
		}

		all = append(all, items...)

		if len(items) == 0 || page >= totalPages {
			return all, nil
		}
	}

	return nil, fmt.Errorf("list pagination: server reports more than %d pages; refusing to return a truncated list", maxListPages)
}

// ListMyDatasets lists all datasets uploaded by this producer, following
// every page of GET /v1/datasets' paginated response until total_pages is
// exhausted.
func (p *Producer) ListMyDatasets(ctx context.Context) ([]types.Dataset, error) {
	return paginateAll(func(page int) ([]types.Dataset, *int, int, error) {
		path := fmt.Sprintf("/v1/datasets?producer_id=%s&page=%d&limit=100", url.QueryEscape(p.CustomerID), page)

		var response struct {
			Datasets   []types.Dataset `json:"datasets"`
			Page       *int            `json:"page"`
			TotalPages int             `json:"total_pages"`
		}

		if err := p.makeAPIRequest(ctx, "GET", path, nil, &response); err != nil {
			return nil, nil, 0, err
		}

		return response.Datasets, response.Page, response.TotalPages, nil
	})
}

// GetDatasetSubscribers lists all subscribers for a specific dataset,
// following every page of GET /v1/subscriptions' paginated response.
func (p *Producer) GetDatasetSubscribers(ctx context.Context, datasetID string) ([]types.Subscription, error) {
	return paginateAll(func(page int) ([]types.Subscription, *int, int, error) {
		path := fmt.Sprintf("/v1/subscriptions?dataset_id=%s&page=%d&limit=100", url.QueryEscape(datasetID), page)

		var response struct {
			Subscriptions []types.Subscription `json:"subscriptions"`
			Page          *int                 `json:"page"`
			TotalPages    int                  `json:"total_pages"`
		}

		if err := p.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, nil, 0, err
		}

		return response.Subscriptions, response.Page, response.TotalPages, nil
	})
}

// RevokeSubscription revokes a subscription.
func (p *Producer) RevokeSubscription(ctx context.Context, subscriptionID string) error {
	path := fmt.Sprintf("/v1/subscriptions/%s/revoke", url.PathEscape(subscriptionID))

	// PUT request with empty body
	if err := p.makeAPIRequest(ctx, http.MethodPut, path, map[string]string{}, nil); err != nil {
		return err
	}

	return nil
}

// ListSubscriptionRequests lists incoming subscription requests for this
// producer, following every page of GET /v1/producers/subscription-requests'
// paginated response until total_pages is exhausted.
// Returns requests that match the specified status filter.
//
// Parameters:
//   - status: Filter by request status. Valid values: "pending", "approved", "rejected".
//     If empty, defaults to "pending".
//
// Returns a slice of subscription requests matching the filter.
func (p *Producer) ListSubscriptionRequests(ctx context.Context, status string) ([]types.SubscriptionRequest, error) {
	// Default to pending if not specified
	if status == "" {
		status = "pending"
	}

	return paginateAll(func(page int) ([]types.SubscriptionRequest, *int, int, error) {
		path := fmt.Sprintf("/v1/producers/subscription-requests?status=%s&page=%d&limit=100", url.QueryEscape(status), page)

		var response struct {
			Requests   []types.SubscriptionRequest `json:"requests"`
			Page       *int                        `json:"page"`
			TotalPages int                         `json:"total_pages"`
		}

		if err := p.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, nil, 0, err
		}

		return response.Requests, response.Page, response.TotalPages, nil
	})
}

// deprecationWriter receives one-time deprecation warnings. It is a variable
// so tests can capture the output.
var deprecationWriter io.Writer = os.Stderr

// warnedApproveDatasetID makes the DatasetID deprecation warning fire once
// per process instead of once per approval.
var warnedApproveDatasetID atomic.Bool

// ApproveSubscriptionRequest approves a subscription request from a consumer.
// This provisions the resources the consumer needs to receive upload
// notifications and access the producer's datasets.
//
// Parameters:
//   - requestID: The subscription request ID to approve.
//   - opts: Optional parameters for approval:
//   - Notes: Optional internal notes about the approval.
//   - DatasetID: Deprecated. The API has no such field and always grants
//     the scope of the original request, so it is no longer sent; passing
//     it prints a one-time deprecation warning and a later release will
//     reject it.
//   - PriceMonthlyCents: Optional per-consumer monthly USD-cents price for
//     THIS approval (see types.ApproveSubscriptionRequestOptions for the
//     full nil/0/positive semantics). A negative value is rejected
//     client-side with a *ValidationError before any request is sent.
//
// Returns the updated subscription request with status "approved" (or, when
// PriceMonthlyCents is a positive value, "approved_pending_payment" until
// the consumer completes checkout). The API answers an approval with a
// {request, subscription} envelope; this method returns the request half and
// keeps its original signature. Use ApproveSubscriptionRequestWithSubscription
// to also receive the provisioned subscription.
func (p *Producer) ApproveSubscriptionRequest(ctx context.Context, requestID string, opts *types.ApproveSubscriptionRequestOptions) (*types.SubscriptionRequest, error) {
	resp, err := p.ApproveSubscriptionRequestWithSubscription(ctx, requestID, opts)
	if err != nil {
		return nil, err
	}

	return &resp.Request, nil
}

// ApproveSubscriptionRequestWithSubscription approves a subscription request
// exactly like ApproveSubscriptionRequest but returns the API's full
// {request, subscription} envelope: Request is the updated subscription
// request and Subscription is the subscription that approval provisioned. It
// is nil while the request is approved_pending_payment (nothing is
// provisioned until the consumer completes checkout).
//
// A response without a request object is reported as an error rather than a
// zero-valued success.
func (p *Producer) ApproveSubscriptionRequestWithSubscription(ctx context.Context, requestID string, opts *types.ApproveSubscriptionRequestOptions) (*types.ApproveRequestResponse, error) {
	path := fmt.Sprintf("/v1/subscription-requests/%s", url.PathEscape(requestID))

	// Use a map to include the optional price_monthly_cents field, which is
	// not part of ApproveRejectPayload.
	payloadMap := map[string]any{
		"action": "approve",
	}
	if opts != nil {
		if opts.Notes != nil {
			payloadMap["notes"] = *opts.Notes
		}
		if opts.DatasetID != nil && warnedApproveDatasetID.CompareAndSwap(false, true) {
			fmt.Fprintln(deprecationWriter, "helix sdk-go: ApproveSubscriptionRequestOptions.DatasetID is deprecated and ignored — "+
				"the API always grants the scope of the original request; the option will be removed in a future release")
		}
		if opts.PriceMonthlyCents != nil {
			// CAREFUL: this branch is keyed on "!= nil", NOT on the pointed-to
			// value — a pointer to 0 (free grant) MUST still reach the wire as
			// price_monthly_cents:0, distinct from an absent key (dataset's own
			// price applies). Conflating "nil" with "points to zero" was the bug
			// this whole feature exists to avoid.
			if *opts.PriceMonthlyCents < 0 {
				return nil, &ValidationError{Field: "price_monthly_cents", Message: "must be >= 0"}
			}
			payloadMap["price_monthly_cents"] = *opts.PriceMonthlyCents
		}
	}

	var result types.ApproveRequestResponse
	if err := p.makeAPIRequest(ctx, http.MethodPost, path, payloadMap, &result); err != nil {
		return nil, err
	}

	if result.Request.ID == "" && result.Request.Status == "" {
		return nil, errors.New("unexpected approve response: no request object in the body")
	}

	return &result, nil
}

// requireDatasetID refuses an empty dataset id before a request is built: an
// empty id turns PATCH/DELETE /v1/datasets/{id} into a call on the collection.
func requireDatasetID(datasetID string) error {
	if strings.TrimSpace(datasetID) == "" {
		return &ValidationError{Field: "dataset_id", Message: "is required"}
	}

	return nil
}

// UpdateDataset updates an existing dataset's metadata.
// This is a public API for producers to update dataset information without re-uploading data.
//
// Parameters:
//   - datasetID: The dataset ID to update.
//   - input: DatasetUpdateInput containing fields to update (nil fields are ignored).
//
// input.Metadata is deprecated and cannot be updated here, so UpdateDataset
// returns a *ValidationError, before any network call, when it is set.
//
// Returns the updated dataset.
func (p *Producer) UpdateDataset(ctx context.Context, datasetID string, input types.DatasetUpdateInput) (*types.Dataset, error) {
	if err := requireDatasetID(datasetID); err != nil {
		return nil, err
	}

	//nolint:staticcheck // SA1019: reads the deprecated field on purpose, to refuse it before any request.
	if input.Metadata != nil {
		return nil, &ValidationError{Field: "metadata", Message: "is not updatable by UpdateDataset; remove it from the input"}
	}

	path := fmt.Sprintf("/v1/datasets/%s", url.PathEscape(datasetID))

	var result types.Dataset
	if err := p.makeAPIRequest(ctx, http.MethodPatch, path, input, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ListSubscribers lists all active subscribers across all of the producer's datasets.
// Provides an aggregated view of who has access to the producer's data.
//
// Returns a SubscribersResponse containing all subscribers with their subscription details.
func (p *Producer) ListSubscribers(ctx context.Context) (*types.SubscribersResponse, error) {
	var response types.SubscribersResponse
	if err := p.makeAPIRequest(ctx, http.MethodGet, "/v1/producers/subscribers", nil, &response); err != nil {
		return nil, err
	}

	return &response, nil
}

// RejectSubscriptionRequest rejects a subscription request from a consumer.
//
// Parameters:
//   - requestID: The subscription request ID to reject.
//   - reason: Optional reason for rejection (will be visible to the consumer).
//
// Returns the updated subscription request with status "rejected".
func (p *Producer) RejectSubscriptionRequest(ctx context.Context, requestID string, reason string) (*types.SubscriptionRequest, error) {
	path := fmt.Sprintf("/v1/subscription-requests/%s", url.PathEscape(requestID))

	payloadMap := map[string]any{
		"action": "reject",
	}
	if reason != "" {
		payloadMap["reason"] = reason
	}

	var result types.SubscriptionRequest
	if err := p.makeAPIRequest(ctx, http.MethodPost, path, payloadMap, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// DeleteDataset permanently removes a dataset.
//
// Parameters:
//   - datasetID: The dataset ID to delete.
//
// Returns an error if the deletion fails.
func (p *Producer) DeleteDataset(ctx context.Context, datasetID string) error {
	if err := requireDatasetID(datasetID); err != nil {
		return err
	}

	return p.makeAPIRequest(ctx, "DELETE", fmt.Sprintf("/v1/datasets/%s", url.PathEscape(datasetID)), nil, nil)
}
