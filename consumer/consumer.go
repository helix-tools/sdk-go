// Package consumer provides functionality for consuming datasets from the Helix Connect Platform.
//
// It handles the entire lifecycle of dataset consumption, including authentication,
// downloading, decrypting, and decompressing datasets. It also provides mechanisms
// to poll and acknowledge dataset upload notifications via SQS.
//
// TODO: adopt structured logging with configurable levels (e.g. debug).
package consumer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
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
	"time"

	stscreds "github.com/helix-tools/sdk-go/v2/credentials"
	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
	"github.com/helix-tools/sdk-go/v2/internal/useragent"
	"github.com/helix-tools/sdk-go/v2/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// emptyPayloadHash is the SHA256 hash of an empty payload.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// SDKVersion is the FALLBACK Go SDK version surfaced in download outcome
// callbacks, used only when the real build version can't be resolved at
// runtime (see effectiveSDKVersion / resolveSDKVersion in
// sdk_version.go) — an unavailable build info, running this SDK's own
// test suite, or a consumer using a `replace` directive to a local
// checkout. Kept in lockstep with the latest module version tag as a
// best-effort default for those dev-build cases; the wire-sent
// sdk_version value for a normally-built consumer binary instead
// reflects the actual resolved module version, which cannot drift.
const SDKVersion = "2.22.2"

// SDKLanguage identifies this SDK's language in download outcome callbacks
// (matches the dataset_download_event JSON Schema's sdk_language field).
const SDKLanguage = "go"

// defaultHTTPClientTimeout bounds every Consumer HTTP call (getDataset,
// getDownloadUrl, the object download, outcome callback) so a stuck connection
// cannot hang the caller indefinitely. It is intentionally longer than
// the per-call ctx budget (5s for the outcome callback) so that the
// per-call deadline still wins where one is supplied; this is purely
// defense-in-depth against future code paths that omit it.
const defaultHTTPClientTimeout = 10 * time.Second

// ErrorCategory mirrors the dataset_download_event JSON Schema enum. Each
// value tags one phase of the DownloadDataset pipeline so a failed download
// surfaces a meaningful category in the producer dashboard. Keep in sync
// with sdk-schemas/dataset_download_event.schema.json (properties.
// error_category.enum).
type ErrorCategory string

// Valid ErrorCategory values — must match the JSON Schema enum exactly.
const (
	ErrorCategorySignedURLFetch ErrorCategory = "signed_url_fetch"
	ErrorCategoryMetadataFetch  ErrorCategory = "metadata_fetch"
	ErrorCategoryNetworkFetch   ErrorCategory = "network_fetch"
	ErrorCategoryKMSDecrypt     ErrorCategory = "kms_decrypt"
	ErrorCategoryDecompress     ErrorCategory = "decompress"
	ErrorCategoryDiskWrite      ErrorCategory = "disk_write"
	ErrorCategoryUnknown        ErrorCategory = "unknown"
)

// errNotEncrypted and errNotCompressed mark a downloaded object that does not
// have the shape every upload produces (compressed, then the encryption envelope).
// A download never passes such an object through: it is an error.
var (
	errNotEncrypted  = errors.New("object is not in the encrypted format every upload produces; refusing to return it unencrypted")
	errNotCompressed = errors.New("object is not compressed; refusing to return it uncompressed")
)

// maxWrappedKeyLen bounds the wrapped-data-key length an object's header may
// declare. It is the size limit on a wrapped key; a larger value cannot
// be a real envelope.
const maxWrappedKeyLen = 6144

// errorMessageMaxChars caps error_message before sending so a stack trace
// can't blow the server-side 500-char limit.
const errorMessageMaxChars = 500

// outcomeCallbackTimeout is the tight HTTP timeout on the outcome POST so
// a slow observability API can't hold up an otherwise successful download.
const outcomeCallbackTimeout = 5 * time.Second

// userPathPattern strips /Users/<name>/ paths from error_message before
// sending so the producer dashboard never sees a consumer's home dir.
var userPathPattern = regexp.MustCompile(`/Users/[^/\s]+`)

// homePathPattern is the Linux equivalent of userPathPattern.
var homePathPattern = regexp.MustCompile(`/home/[^/\s]+`)

// RecordOutcomeRequest is the body shape for POST /v1/datasets/:id/
// download-events. Status-dependent fields (error_*) use omitempty so
// a success callback doesn't carry stale error fields and vice versa.
// EventID, Status, DurationMs, and BytesDownloaded are emitted
// unconditionally — they're populated on every callback (success or
// error). BytesDownloaded specifically: a legitimate 0-byte success
// (empty dataset, empty NDJSON file, decompressed-to-empty payload)
// must distinguish from "no telemetry sent" on the dashboard, so we
// always serialize the int even when it's zero. Schema-side, the
// server's persistence layer drops zero values per
// dataset_download_event.schema.json's "persisted only when non-zero"
// guidance — that's a server concern, not a wire-format one.
type RecordOutcomeRequest struct {
	EventID         string        `json:"event_id"`
	Status          string        `json:"status"` // "success" | "error"
	ErrorCategory   ErrorCategory `json:"error_category,omitempty"`
	ErrorMessage    string        `json:"error_message,omitempty"`
	DurationMs      int64         `json:"duration_ms"`
	BytesDownloaded int64         `json:"bytes_downloaded"`
	SDKVersion      string        `json:"sdk_version,omitempty"`
	SDKLanguage     string        `json:"sdk_language,omitempty"`
}

// Consumer handles downloading and managing datasets from Helix Connect platform.
type Consumer struct {
	APIEndpoint string
	CustomerID  string
	Region      string

	awsConfig  aws.Config
	httpClient *http.Client
	kmsClient  *kms.Client
	queueURL   *string // Cache for per-consumer queue URL.
	sqsClient  *sqs.Client
}

// DownloadURLInfo contains information about a dataset download URL.
// Note: Go API returns file_name, file_size instead of nested dataset object.
type DownloadURLInfo struct {
	DownloadURL string `json:"download_url"`
	ExpiresAt   string `json:"expires_at"`
	FileName    string `json:"file_name,omitempty"`
	FileSize    int64  `json:"file_size,omitempty"`
	// EventID is the server-side observability hook. The API records one
	// `dataset_download_events` row at URL-issue time with status=success
	// and returns its identifier here. The SDK uses it to call back with
	// the actual download outcome via POST /v1/datasets/:id/download-events.
	// Absent in environments where observability isn't wired (older API
	// versions, dev) — the callback then no-ops.
	EventID string `json:"event_id,omitempty"`
	// Legacy nested dataset object (for backward compatibility).
	Dataset *struct {
		ID        string `json:"_id"`
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"dataset,omitempty"`
}

// APIError is returned for any non-2xx response from the Helix API. Callers
// can branch on the status without matching message text:
//
//	var apiErr *consumer.APIError
//	if errors.As(err, &apiErr) && apiErr.IsRateLimited() {
//		// back off and retry
//	}
//
// Error() keeps the historical "API request failed: <status> - <body>" text.
type APIError struct {
	StatusCode int
	Body       string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("API request failed: %d - %s", e.StatusCode, e.Body)
}

// IsUnauthorized reports a 401 (bad or expired credentials).
func (e *APIError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports a 403 (authenticated but not permitted).
func (e *APIError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// IsNotFound reports a 404 (the resource does not exist or is not visible).
func (e *APIError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsConflict reports a 409 (duplicate or state conflict).
func (e *APIError) IsConflict() bool { return e.StatusCode == http.StatusConflict }

// IsRateLimited reports a 429: back off and retry.
func (e *APIError) IsRateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// ValidationError is returned for a request that fails a client-side check
// before anything is sent to the API. errors.As tells it apart from the
// *APIError the server returns.
type ValidationError struct {
	Field   string
	Message string
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s: %s", e.Field, e.Message)
}

// Dataset is one row of Consumer.ListDatasets. ID, Name and the two Metadata
// flags are the fields this package has always exposed, unchanged in name, type
// and meaning; Record carries the full catalog record — the same *types.Dataset
// shape Consumer.GetDataset returns (ProducerID, Category, Description, Status,
// sizes, Marketplace, the raw metadata map, ...) — so a listing no longer needs
// one GetDataset call per row.
//
// Record is set on every row ListDatasets returns; it is nil only on a Dataset
// built by hand.
type Dataset struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Metadata holds the two flags download handling reads, resolved exactly as
	// DownloadDataset resolves them (resolveEncryptCompress): an explicit
	// metadata.encryption_enabled wins, otherwise the record's top-level
	// Encryption flag applies. The create endpoint writes both places.
	Metadata struct {
		CompressionEnabled bool `json:"compression_enabled"`
		EncryptionEnabled  bool `json:"encryption_enabled"`
	} `json:"metadata"`

	// Record is the full catalog record for this row.
	Record *types.Dataset `json:"-"`
}

// UnmarshalJSON decodes the full catalog record (so ID is filled from "id" or
// "_id", like every types.Dataset) and derives the legacy fields from it.
func (d *Dataset) UnmarshalJSON(data []byte) error {
	var record types.Dataset
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}

	*d = Dataset{ID: record.ID, Name: record.Name, Record: &record}
	d.Metadata.EncryptionEnabled, d.Metadata.CompressionEnabled = resolveEncryptCompress(&record)

	return nil
}

// Notification represents a dataset upload notification received from SQS.
type Notification struct {
	DatasetID      string `json:"dataset_id"`
	DatasetName    string `json:"dataset_name,omitempty"`
	EventType      string `json:"event_type"`
	MessageID      string `json:"message_id"`
	ProducerID     string `json:"producer_id"`
	RawMessage     string `json:"raw_message"`
	ReceiptHandle  string `json:"receipt_handle"`
	S3Bucket       string `json:"s3_bucket"`
	S3Key          string `json:"s3_key"`
	SizeBytes      int64  `json:"size_bytes"`
	SubscriberID   string `json:"subscriber_id"`
	SubscriptionID string `json:"subscription_id"`
	Timestamp      string `json:"timestamp"`
}

// Subscription is an alias for types.Subscription for backward compatibility.
// Use types.Subscription directly for new code.
type Subscription = types.Subscription

// ListSubscriptionsOptions contains options for listing subscriptions.
type ListSubscriptionsOptions struct {
	// Role filters subscriptions by role for "both" customers.
	// Valid values: "consumer", "producer"
	// If omitted, API defaults to consumer for "both" customers.
	Role string
}

// PollNotificationsOptions contains options for polling notifications from SQS.
type PollNotificationsOptions struct {
	AutoAcknowledge *bool    // Automatically acknowledge (delete) messages after receiving (default: true)
	MaxMessages     int32    // Maximum number of messages to retrieve (1-10, default: 10)
	SubscriptionIDs []string // Optional list of subscription IDs to filter notifications

	// Long polling wait time (1-20 seconds). 0 means "not set" and selects
	// the default of 20; use ShortPoll to ask for an immediate return.
	WaitTimeSeconds int32

	// ShortPoll returns immediately with whatever is queued instead of
	// waiting for messages (SQS WaitTimeSeconds = 0). Go's zero value cannot
	// distinguish "unset" from an explicit 0, so this flag is how a caller
	// asks for it; it takes precedence over WaitTimeSeconds.
	ShortPoll bool

	// VisibilityTimeout is how many seconds a received message stays hidden
	// from other pollers before it becomes visible again if it is not
	// acknowledged (1-43200). 0 (or a negative value) means "not set" and
	// selects the default of 300.
	VisibilityTimeout int32
}

// NewConsumer creates a new Consumer instance.
//
// TODO: Allow to pass context for better control.
func NewConsumer(cfg types.Config) (*Consumer, error) {
	// Basic validation.
	//
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

	awsHTTPClient := &http.Client{
		Timeout: 25 * time.Second,
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

	return &Consumer{
		APIEndpoint: cfg.APIEndpoint,
		CustomerID:  cfg.CustomerID,
		Region:      cfg.Region,

		awsConfig:  awsCfg,
		httpClient: &http.Client{Timeout: defaultHTTPClientTimeout},
		kmsClient:  kms.NewFromConfig(awsCfg),
		sqsClient:  sqs.NewFromConfig(awsCfg),
	}, nil
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

// GetDataset retrieves metadata for a specific dataset.
func (c *Consumer) GetDataset(ctx context.Context, datasetID string) (*types.Dataset, error) {
	// An empty id would GET /v1/datasets/ — the collection — and decode its
	// list body into an empty Dataset without any error.
	if strings.TrimSpace(datasetID) == "" {
		return nil, errors.New("dataset id is required")
	}

	path := fmt.Sprintf("/v1/datasets/%s", url.PathEscape(datasetID))

	var dataset types.Dataset
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &dataset); err != nil {
		return nil, err
	}

	return &dataset, nil
}

// GetDownloadURL retrieves a presigned download URL for a dataset.
func (c *Consumer) GetDownloadURL(ctx context.Context, datasetID string) (*DownloadURLInfo, error) {
	path := fmt.Sprintf("/v1/datasets/%s/download", url.PathEscape(datasetID))

	var raw json.RawMessage
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}

	var urlInfo DownloadURLInfo
	if err := json.Unmarshal(raw, &urlInfo); err != nil {
		return nil, err
	}

	// The legacy nested dataset object is tagged "_id", but the API identifies a
	// dataset with "id" (see types.Dataset): fall back to it.
	if urlInfo.Dataset != nil && urlInfo.Dataset.ID == "" {
		var alt struct {
			Dataset struct {
				ID string `json:"id"`
			} `json:"dataset"`
		}
		if json.Unmarshal(raw, &alt) == nil {
			urlInfo.Dataset.ID = alt.Dataset.ID
		}
	}

	return &urlInfo, nil
}

// resolveEncryptCompress decides whether a downloaded object must be decrypted
// and/or decompressed, from the dataset record.
//
// isEncrypted defaults to the top-level dataset.Encryption field and is only
// overridden when metadata.encryption_enabled is explicitly present. The create
// endpoint PROMOTES metadata.encryption_enabled to the top-level `encryption`
// field and drops it from metadata, so a metadata-only read wrongly sees false
// and skips decrypt (then decompresses still-encrypted bytes and fails on the
// header). Mirrors the Python SDK's
// metadata.get("encryption_enabled", dataset.get("encryption", False)).
//
// isCompressed is metadata-only (default false): the API keeps
// compression_enabled in metadata and has no promoted top-level equivalent.
func resolveEncryptCompress(dataset *types.Dataset) (isEncrypted, isCompressed bool) {
	if dataset == nil {
		return false, false
	}
	isEncrypted = dataset.Encryption
	if dataset.Metadata != nil {
		if enc, ok := dataset.Metadata["encryption_enabled"].(bool); ok {
			isEncrypted = enc
		}
		if comp, ok := dataset.Metadata["compression_enabled"].(bool); ok {
			isCompressed = comp
		}
	}
	return isEncrypted, isCompressed
}

// DownloadDataset downloads and processes a dataset to a local file.
//
// Observability: after the URL is issued by the API a server-side
// `dataset_download_events` row is created with status=success. This method
// then tracks the actual download outcome and fires a fire-and-forget
// callback to POST /v1/datasets/:id/download-events so the producer
// dashboard reflects what really happened (status=error + category +
// message on failure, or status=success + bytes_downloaded + duration_ms
// on success). The callback is best-effort — its failure NEVER affects
// the caller's experience.
func (c *Consumer) DownloadDataset(ctx context.Context, datasetID, outputPath string) (retErr error) {
	fmt.Printf("Downloading dataset %s...\n", datasetID)

	start := time.Now()
	// `phase` tracks where we are in the pipeline so an exception at any
	// failure point becomes a meaningful error_category in the outcome
	// callback. Categories must match the schema's enum: signed_url_fetch
	// / metadata_fetch / network_fetch / kms_decrypt / decompress /
	// disk_write / unknown.
	phase := ErrorCategoryUnknown
	var (
		eventID         string
		bytesDownloaded int64
		errorMessage    string
	)

	// defer fires the outcome callback after the pipeline returns or
	// panics. We capture variables by value into the deferred goroutine
	// so a later mutation can't poison the payload, and use a fresh
	// context.Background() rather than the caller's ctx — the caller's
	// ctx may already be cancelled by the time defer runs.
	defer func() {
		if eventID == "" {
			// Older API didn't surface event_id; nothing to update.
			return
		}
		durationMs := time.Since(start).Milliseconds()
		req := RecordOutcomeRequest{
			EventID:     eventID,
			DurationMs:  durationMs,
			SDKVersion:  effectiveSDKVersion(),
			SDKLanguage: SDKLanguage,
		}
		if retErr == nil {
			req.Status = "success"
			req.BytesDownloaded = bytesDownloaded
		} else {
			req.Status = "error"
			req.ErrorCategory = phase
			req.ErrorMessage = sanitizeErrorMessage(errorMessage)
		}
		// Capture by value into the goroutine. Best-effort: errors are
		// swallowed so the producer dashboard's URL-issued row remains
		// the source of truth even if the callback API is down.
		go func(payload RecordOutcomeRequest, dsID string) {
			cbCtx, cancel := context.WithTimeout(context.Background(), outcomeCallbackTimeout)
			defer cancel()
			_ = c.recordOutcome(cbCtx, dsID, payload)
		}(req, datasetID)
	}()

	// 1. Metadata fetch (BEFORE signed-url fetch — matches TS order so a
	// metadata failure has no event_id captured yet and the callback
	// becomes a no-op).
	//
	// Every upload is compressed and encrypted, so every download is decrypted
	// and decompressed: the record's flags are NOT consulted (a record that
	// claims otherwise is never a licence to return raw bytes). The fetch stays
	// so an unknown or forbidden dataset fails here, as metadata_fetch, before
	// a signed URL is issued.
	phase = ErrorCategoryMetadataFetch
	if _, err := c.GetDataset(ctx, datasetID); err != nil {
		errorMessage = err.Error()
		return fmt.Errorf("failed to get dataset metadata: %w", err)
	}

	// 2. Signed-URL fetch.
	phase = ErrorCategorySignedURLFetch
	urlInfo, err := c.GetDownloadURL(ctx, datasetID)
	if err != nil {
		errorMessage = err.Error()
		return fmt.Errorf("failed to get download URL: %w", err)
	}
	// Capture event_id for the outcome callback. Absent against older
	// API versions — the callback path becomes a no-op.
	eventID = urlInfo.EventID

	phase = ErrorCategoryDiskWrite
	safeOutputPath, err := cleanContainedPath(outputPath)
	if err != nil {
		errorMessage = err.Error()
		return fmt.Errorf("invalid output path: %w", err)
	}

	// 3. Network fetch.
	phase = ErrorCategoryNetworkFetch
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlInfo.DownloadURL, nil)
	if err != nil {
		// urlInfo.DownloadURL is a server-issued presigned URL carrying a
		// SigV4 signature/credential scope in its query string; a malformed
		// version of it must not reach the caller (or the outcome
		// callback) via the raw *url.Error http.NewRequestWithContext
		// returns, so SanitizeCause is applied explicitly (case (b) of
		// its doc comment).
		wrapped := sdkerr.Wrap("failed to build download request", sdkerr.SanitizeCause(err))
		errorMessage = wrapped.Error()
		return wrapped
	}
	resp, err := c.httpClient.Do(req)
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
			wrapped := sdkerr.Wrap("failed to download", err)
			errorMessage = wrapped.Error()
			return wrapped
		}
		// No response arrived at all: a genuine no-response transport
		// failure, case (a) of SanitizeCause's doc comment.
		wrapped := sdkerr.Wrap("failed to download", sdkerr.SanitizeCause(err))
		errorMessage = wrapped.Error()
		return wrapped
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errorMessage = fmt.Sprintf("download failed with status %d", resp.StatusCode)
		return fmt.Errorf("%s", errorMessage)
	}

	contentLength := resp.ContentLength
	largeFileThreshold := int64(100 * 1024 * 1024) // 100MB

	if contentLength > largeFileThreshold {
		// Large-file path: stream to temp, process, write final.
		tempFile, terr := os.CreateTemp("", "helix-dataset-*")
		if terr != nil {
			errorMessage = terr.Error()
			return fmt.Errorf("failed to create temp file: %w", terr)
		}
		defer os.Remove(tempFile.Name())
		defer func() {
			// Best-effort fallback for early returns; the normal-path close is checked below.
			_ = tempFile.Close()
		}()

		sizeGB := float64(contentLength) / (1024 * 1024 * 1024)
		fmt.Printf("Streaming %.2f GB to temporary file...\n", sizeGB)

		written, cerr := io.Copy(tempFile, resp.Body)
		if cerr != nil {
			// io.Copy reads from the HTTP response body, so a network-level
			// failure mid-stream surfaces here exactly as it would from
			// resp.Body.Read directly.
			wrapped := sdkerr.Wrap("failed to stream to temp file", cerr)
			errorMessage = wrapped.Error()
			return wrapped
		}
		if cerr := tempFile.Close(); cerr != nil {
			errorMessage = cerr.Error()
			return fmt.Errorf("failed to close temp file: %w", cerr)
		}
		fmt.Printf("Downloaded %d bytes to temp file\n", written)
		bytesDownloaded = written

		data, rerr := os.ReadFile(tempFile.Name())
		if rerr != nil {
			errorMessage = rerr.Error()
			return fmt.Errorf("failed to read temp file: %w", rerr)
		}

		data, phase, err = c.decryptAndDecompress(ctx, data)
		if err != nil {
			errorMessage = err.Error()
			return err
		}
		bytesDownloaded = int64(len(data))

		phase = ErrorCategoryDiskWrite
		if werr := writeFileWithinRoot(safeOutputPath, data); werr != nil {
			errorMessage = werr.Error()
			return fmt.Errorf("failed to write file: %w", werr)
		}
		fmt.Printf("Saved to %s\n", safeOutputPath)
		return nil
	}

	// Small-file path: process in memory.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		wrapped := sdkerr.Wrap("failed to read response", err)
		errorMessage = wrapped.Error()
		return wrapped
	}

	fmt.Printf("Downloaded %d bytes\n", len(data))
	bytesDownloaded = int64(len(data))

	data, phase, err = c.decryptAndDecompress(ctx, data)
	if err != nil {
		errorMessage = err.Error()
		return err
	}
	bytesDownloaded = int64(len(data))

	phase = ErrorCategoryDiskWrite
	if err := writeFileWithinRoot(safeOutputPath, data); err != nil {
		errorMessage = err.Error()
		return fmt.Errorf("failed to write file: %w", err)
	}
	fmt.Printf("Saved to %s\n", safeOutputPath)

	return nil
}

// decryptAndDecompress reverses what every upload does — compress, then encrypt —
// and is the only way DownloadDataset turns a stored object into dataset
// bytes: there is no option to skip either step. An object that is not
// encrypted, or not compressed, is an error, never a pass-through. On failure
// it reports the pipeline phase that failed, for the outcome callback.
func (c *Consumer) decryptAndDecompress(ctx context.Context, data []byte) ([]byte, ErrorCategory, error) {
	fmt.Printf("Decrypting %d bytes...\n", len(data))
	decrypted, err := c.decryptData(ctx, data)
	if err != nil {
		// decryptData's own errors are already clean (an authored message
		// with the cause attached, or a bare sentinel) — re-wrapping here
		// would only add a redundant prefix, not any new information.
		return nil, ErrorCategoryKMSDecrypt, err
	}
	fmt.Printf("Decrypted to %d bytes\n", len(decrypted))

	fmt.Printf("Decompressing %d bytes...\n", len(decrypted))
	decompressed, err := c.decompressData(decrypted)
	if err != nil {
		return nil, ErrorCategoryDecompress, err
	}
	decompressedGB := float64(len(decompressed)) / (1024 * 1024 * 1024)
	if decompressedGB > 1 {
		fmt.Printf("Decompressed to %.2f GB\n", decompressedGB)
	} else {
		fmt.Printf("Decompressed to %d bytes\n", len(decompressed))
	}

	return decompressed, ErrorCategoryUnknown, nil
}

// recordOutcome posts the outcome of an actual download to the API so the
// producer dashboard reflects what really happened. Best-effort — caller
// should swallow errors. Uses outcomeCallbackTimeout (5s) on the request
// context so a slow API can't hold up an otherwise successful download.
//
// dataset_id is encoded with encodeURIComponent semantics (matches the
// TS SDK), which means slashes in IDs become %2F so the path matches
// what /v1/datasets/:id routing expects on the server side.
func (c *Consumer) recordOutcome(ctx context.Context, datasetID string, payload RecordOutcomeRequest) error {
	path := fmt.Sprintf("/v1/datasets/%s/download-events", encodeURIComponent(datasetID))
	return c.makeAPIRequest(ctx, http.MethodPost, path, payload, nil)
}

// encodeURIComponent mirrors JavaScript's encodeURIComponent: every char
// that isn't an unreserved letter, digit, or one of "-_.!~*'()" is
// percent-encoded — including "/", which Go's url.PathEscape preserves.
// Used on the outcome-callback path so a dataset_id with a slash routes
// to /v1/datasets/:id correctly server-side.
func encodeURIComponent(s string) string {
	// url.QueryEscape encodes spaces as "+"; convert back to "%20" so
	// the result matches encodeURIComponent's output exactly.
	escaped := url.QueryEscape(s)
	return strings.ReplaceAll(escaped, "+", "%20")
}

// sanitizeErrorMessage strips filesystem paths from an error string and
// caps the length before sending to the producer dashboard. The TS SDK
// uses the same regex pair (/Users/<name>/ and /home/<name>/) — keep in
// sync.
func sanitizeErrorMessage(msg string) string {
	if msg == "" {
		return ""
	}
	scrubbed := userPathPattern.ReplaceAllString(msg, "/Users/<redacted>")
	scrubbed = homePathPattern.ReplaceAllString(scrubbed, "/home/<redacted>")
	if len(scrubbed) > errorMessageMaxChars {
		scrubbed = scrubbed[:errorMessageMaxChars]
	}
	return scrubbed
}

// decryptData opens the encryption envelope every upload produces:
//
//	[4 bytes big-endian wrapped-key length][wrapped key][16 IV][16 tag][ciphertext]
//
// The header comes from an untrusted object, so it is validated against the
// bytes actually present before anything is allocated or sent for unwrapping: a
// plaintext object (its first four bytes read as a length) is refused with
// errNotEncrypted instead of being passed through.
func (c *Consumer) decryptData(ctx context.Context, data []byte) ([]byte, error) {
	if c.kmsClient == nil {
		return nil, errors.New("encryption is not configured; cannot decrypt")
	}

	const ivLen, tagLen = 16, 16

	if len(data) < 4 {
		return nil, errNotEncrypted
	}
	keyLen := binary.BigEndian.Uint32(data[:4])
	rest := data[4:]
	if keyLen == 0 || keyLen > maxWrappedKeyLen || uint64(len(rest)) < uint64(keyLen)+ivLen+tagLen {
		return nil, errNotEncrypted
	}

	encryptedKey := rest[:keyLen]
	iv := rest[keyLen : keyLen+ivLen]
	authTag := rest[keyLen+ivLen : keyLen+ivLen+tagLen]
	encryptedData := rest[keyLen+ivLen+tagLen:]

	// Unwrap the data key.
	decryptOut, err := c.kmsClient.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: encryptedKey,
	})
	if err != nil {
		return nil, sdkerr.Wrap("decryption failed", err)
	}

	// Decrypt the payload. Every failure branch below uses the
	// same clean message as the unwrap branch above, so decryptData's contract
	// is uniform: Error() is always exactly "decryption failed" and the
	// underlying cause (however unlikely — the unwrap handed back a malformed key
	// size, tampered ciphertext failing the authentication check, ...) is always
	// reachable via errors.Unwrap, never bare on the wire to the caller.
	block, err := aes.NewCipher(decryptOut.Plaintext)
	if err != nil {
		return nil, sdkerr.Wrap("decryption failed", err)
	}

	// Use 16-byte nonce (Python uses os.urandom(16) for IV).
	aesGCM, err := cipher.NewGCMWithNonceSize(block, ivLen)
	if err != nil {
		return nil, sdkerr.Wrap("decryption failed", err)
	}

	// The authentication tag is expected appended to the ciphertext.
	ciphertext := make([]byte, 0, len(encryptedData)+tagLen)
	ciphertext = append(ciphertext, encryptedData...)
	ciphertext = append(ciphertext, authTag...)

	plaintext, err := aesGCM.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return nil, sdkerr.Wrap("decryption failed", err)
	}

	return plaintext, nil
}

// decompressData decompresses data. Bytes that are not a compressed stream are refused
// with errNotCompressed: a download never returns an uncompressed object.
func (c *Consumer) decompressData(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, sdkerr.WrapSentinel(errNotCompressed, err)
	}

	defer gr.Close()

	out, err := io.ReadAll(gr)
	if err != nil {
		return nil, sdkerr.Wrap("decompression failed", err)
	}
	return out, nil
}

// ListDatasets lists all available datasets.
//
// Parameters:
//   - producerID: Optional. Filter datasets by producer ID. If not provided, returns all accessible datasets.
//
// Example:
//
//	// List all datasets
//	datasets, err := consumer.ListDatasets(ctx)
//
//	// List datasets from a specific producer
//	datasets, err := consumer.ListDatasets(ctx, "company-123456")
func (c *Consumer) ListDatasets(ctx context.Context, producerID ...string) ([]Dataset, error) {
	return paginateAll(func(page int) ([]Dataset, *int, int, error) {
		path := fmt.Sprintf("/v1/datasets?page=%d&limit=100", page)
		if len(producerID) > 0 && producerID[0] != "" {
			path = fmt.Sprintf("/v1/datasets?producer_id=%s&page=%d&limit=100", url.QueryEscape(producerID[0]), page)
		}

		var response struct {
			Datasets   []Dataset `json:"datasets"`
			Page       *int      `json:"page"`
			TotalPages int       `json:"total_pages"`
		}

		if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, nil, 0, err
		}

		return response.Datasets, response.Page, response.TotalPages, nil
	})
}

// ListSubscriptions lists all active subscriptions for this consumer.
// For "both" customers (who are both producer and consumer), use opts.Role to filter by role.
func (c *Consumer) ListSubscriptions(ctx context.Context, opts *ListSubscriptionsOptions) ([]Subscription, error) {
	return paginateAll(func(page int) ([]Subscription, *int, int, error) {
		path := fmt.Sprintf("/v1/subscriptions?page=%d&limit=100", page)
		if opts != nil && opts.Role != "" {
			path += "&role=" + url.QueryEscape(opts.Role)
		}

		var response struct {
			Subscriptions []Subscription `json:"subscriptions"`
			Page          *int           `json:"page"`
			TotalPages    int            `json:"total_pages"`
		}

		if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, nil, 0, err
		}

		return response.Subscriptions, response.Page, response.TotalPages, nil
	})
}

// CreateSubscriptionRequest creates a subscription request to access a producer's datasets.
// The producer must approve the request before the consumer gains access.
//
// Parameters:
//   - input.ProducerID: Required. The ID of the producer to request access from.
//   - input.DatasetID: Optional. Specific dataset ID (nil for all-datasets access).
//   - input.Tier: Optional. Subscription tier (defaults to "free"). "free" is the
//     only tier accepted, in any letter case and with surrounding spaces ignored;
//     any other value returns a *ValidationError before any request is sent.
//   - input.Message: Optional. Message to the producer explaining the request.
//
// Returns the created subscription request with status "pending".
func (c *Consumer) CreateSubscriptionRequest(ctx context.Context, input types.CreateSubscriptionRequestInput) (*types.SubscriptionRequest, error) {
	// Mirror the API's own tier rule (trimmed, case-insensitive), so a value the
	// API accepts today is still accepted here.
	if tier := strings.ToLower(strings.TrimSpace(input.Tier)); tier != "" && tier != "free" {
		return nil, &ValidationError{Field: "tier", Message: fmt.Sprintf("%q is not supported: the only tier is \"free\"", input.Tier)}
	}

	// Build request payload
	payload := types.CreateSubscriptionRequestPayload{
		ProducerID: input.ProducerID,
		DatasetID:  input.DatasetID,
		Tier:       input.Tier,
		Message:    input.Message,
	}

	// Default tier to "free" — the only canonical tier accepted by the API
	// (paid tiers were collapsed upstream).
	if payload.Tier == "" {
		payload.Tier = "free"
	}

	var result types.SubscriptionRequest
	if err := c.makeAPIRequest(ctx, http.MethodPost, "/v1/subscription-requests", payload, &result); err != nil {
		return nil, err
	}

	return &result, nil
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

// makeAPIRequest makes an authenticated API request.
func (c *Consumer) makeAPIRequest(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	reqURL := c.APIEndpoint + path

	var (
		reqBody  io.Reader
		jsonData []byte
	)

	if body != nil {
		var err error
		jsonData, err = json.Marshal(body)
		if err != nil {
			return err
		}

		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	// SigV4 ignores User-Agent when building its signed-headers set (see
	// aws-sdk-go-v2's signer/internal/v4.IgnoredHeaders), so setting it
	// before signing is safe — TestMakeAPIRequest_UserAgentNotInSignedHeaders
	// pins that it never leaks into SignedHeaders regardless.
	req.Header.Set("User-Agent", useragent.String())

	// Sign request with AWS SigV4
	creds, err := c.awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return sdkerr.Wrap("failed to retrieve credentials", err)
	}

	// Calculate payload hash for SigV4
	payloadHash := emptyPayloadHash
	if body != nil {
		// Hash the actual JSON body
		h := sha256.New()
		h.Write(jsonData)
		payloadHash = fmt.Sprintf("%x", h.Sum(nil))
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, "execute-api", c.Region, time.Now()); err != nil {
		return sdkerr.Wrap("failed to sign request", err)
	}

	resp, err := c.httpClient.Do(req)
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
			return sdkerr.Wrap("request failed", err)
		}
		// No response arrived at all: a genuine no-response transport
		// failure, case (a) of SanitizeCause's doc comment.
		return sdkerr.Wrap("request failed", sdkerr.SanitizeCause(err))
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)

		return &APIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			// json.Decoder does not wrap a Read error from resp.Body — a
			// connection reset mid-response surfaces here exactly as it
			// would from resp.Body.Read directly.
			return sdkerr.Wrap("failed to decode response", err)
		}
	}

	return nil
}

// PollNotifications polls the per-consumer SQS queue for dataset upload notifications.
//
// IMPORTANT: This uses a DEDICATED queue for this consumer, so only
// notifications relevant to this consumer are delivered here. You can
// optionally filter by subscription IDs for advanced use cases.
//
// Messages are automatically acknowledged (deleted) by default after retrieval.
// This prevents duplicate processing and simplifies the developer experience.
// Set opts.AutoAcknowledge to false if you need manual control over message deletion.
func (c *Consumer) PollNotifications(ctx context.Context, opts PollNotificationsOptions) ([]Notification, error) {
	// Apply defaults
	if opts.MaxMessages <= 0 {
		opts.MaxMessages = 10
	}

	if opts.MaxMessages > 10 {
		opts.MaxMessages = 10 // AWS limit.
	}

	switch {
	case opts.ShortPoll:
		opts.WaitTimeSeconds = 0
	case opts.WaitTimeSeconds <= 0:
		opts.WaitTimeSeconds = 20
	case opts.WaitTimeSeconds > 20:
		opts.WaitTimeSeconds = 20 // AWS limit.
	}

	if opts.VisibilityTimeout <= 0 {
		opts.VisibilityTimeout = 300
	}

	if opts.VisibilityTimeout > 43200 {
		opts.VisibilityTimeout = 43200 // AWS limit (12 hours).
	}

	// Default AutoAcknowledge to true.
	autoAcknowledge := true

	if opts.AutoAcknowledge != nil {
		autoAcknowledge = *opts.AutoAcknowledge
	}

	// Get per-consumer queue URL from active subscriptions where this customer is the consumer.
	if err := c.resolveQueueURL(ctx, "no active subscriptions found. Create a subscription first using CreateSubscriptionRequest()"); err != nil {
		return nil, err
	}

	queueURL := aws.ToString(c.queueURL)

	// The SQS serializer omits a zero WaitTimeSeconds, and an omitted value means
	// "the queue's own default" (20 seconds for consumer queues) — i.e. a long
	// poll. A short poll therefore has to put an explicit 0 on the wire.
	var optFns []func(*sqs.Options)
	if opts.ShortPoll {
		optFns = append(optFns, forceZeroWaitTime)
	}

	// Poll SQS for messages.
	receiveOutput, err := c.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		MaxNumberOfMessages:   opts.MaxMessages,
		MessageAttributeNames: []string{"All"},
		QueueUrl:              aws.String(queueURL),
		VisibilityTimeout:     opts.VisibilityTimeout,
		WaitTimeSeconds:       opts.WaitTimeSeconds,
	}, optFns...)
	if err != nil {
		return nil, sdkerr.Wrap("failed to poll SQS queue", err)
	}

	var notifications []Notification

	for _, message := range receiveOutput.Messages {
		// Parse message body - handle both wrapped and raw message formats.
		// Wrapped messages have a "Message" field containing the stringified notification.
		// Raw messages contain the notification fields directly (event_type, producer_id, etc.).

		messageBody := aws.ToString(message.Body)

		// First, try to parse as a generic JSON to determine format.
		var parsedBody map[string]any
		if err := json.Unmarshal([]byte(messageBody), &parsedBody); err != nil {
			fmt.Printf("Warning: Failed to parse message body: %v\n", err)
			continue
		}

		// Notification data structure.
		var notificationData struct {
			DatasetID      string `json:"dataset_id"`
			DatasetName    string `json:"dataset_name"`
			EventType      string `json:"event_type"`
			ProducerID     string `json:"producer_id"`
			S3Bucket       string `json:"s3_bucket"`
			S3Key          string `json:"s3_key"`
			SizeBytes      int64  `json:"size_bytes"`
			SubscriberID   string `json:"subscriber_id"`
			SubscriptionID string `json:"subscription_id"`
			Timestamp      string `json:"timestamp"`
		}

		// Determine message format and parse notification data.
		if wrappedMessage, hasWrapper := parsedBody["Message"].(string); hasWrapper {
			// Wrapped format: { "Type": "Notification", "Message": "{...}", ... }
			if err := json.Unmarshal([]byte(wrappedMessage), &notificationData); err != nil {
				fmt.Printf("Warning: Failed to parse notification payload: %v\n", err)
				continue
			}
		} else if _, hasEventType := parsedBody["event_type"]; hasEventType {
			// Raw notification payload format, with no wrapper.
			if err := json.Unmarshal([]byte(messageBody), &notificationData); err != nil {
				fmt.Printf("Warning: Failed to parse raw notification payload: %v\n", err)
				continue
			}
		} else {
			fmt.Printf("Warning: Unknown message format for %s, skipping\n", aws.ToString(message.MessageId))
			continue
		}

		// The service routes each notification to its consumer's queue, so
		// messages here are already this consumer's; no subscriber_id filter is needed.

		// Optional filter by subscription IDs if provided (advanced use case).
		if len(opts.SubscriptionIDs) > 0 {
			found := false

			for _, subID := range opts.SubscriptionIDs {
				if subID == notificationData.SubscriptionID {
					found = true

					break
				}
			}
			if !found {
				continue // Skip this notification - doesn't match our subscriptions.
			}
		}

		notification := Notification{
			DatasetID:      notificationData.DatasetID,
			DatasetName:    notificationData.DatasetName,
			EventType:      notificationData.EventType,
			MessageID:      aws.ToString(message.MessageId),
			ProducerID:     notificationData.ProducerID,
			RawMessage:     aws.ToString(message.Body),
			ReceiptHandle:  aws.ToString(message.ReceiptHandle),
			S3Bucket:       notificationData.S3Bucket,
			S3Key:          notificationData.S3Key,
			SizeBytes:      notificationData.SizeBytes,
			SubscriberID:   notificationData.SubscriberID,
			SubscriptionID: notificationData.SubscriptionID,
			Timestamp:      notificationData.Timestamp,
		}

		notifications = append(notifications, notification)

		// Auto-acknowledge (delete) message by default.
		if autoAcknowledge {
			if err := c.DeleteNotification(ctx, notification.ReceiptHandle); err != nil {
				fmt.Printf("Warning: Failed to auto-acknowledge notification %s: %v\n", notification.MessageID, err)
			}
		}
	}

	return notifications, nil
}

// forceZeroWaitTime adds an explicit "WaitTimeSeconds": 0 to the serialized
// ReceiveMessage request, which the generated serializer drops for a zero
// value. It runs after serialization and before signing, so the signature
// covers the final body.
func forceZeroWaitTime(o *sqs.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Serialize.Add(middleware.SerializeMiddlewareFunc("HelixShortPoll", zeroWaitSerialize), middleware.After)
	})
}

// zeroWaitSerialize is the serialize-step middleware behind forceZeroWaitTime.
func zeroWaitSerialize(ctx context.Context, in middleware.SerializeInput, next middleware.SerializeHandler) (middleware.SerializeOutput, middleware.Metadata, error) {
	req, ok := in.Request.(*smithyhttp.Request)
	if !ok || req.GetStream() == nil {
		return next.HandleSerialize(ctx, in)
	}

	patched, err := withZeroWaitTime(req.GetStream())
	if err != nil {
		return middleware.SerializeOutput{}, middleware.Metadata{}, err
	}

	// A bytes.Reader is seekable, so SetStream's only failure mode (a Seek
	// error) cannot occur; the error is still propagated rather than dropped.
	if req, err = req.SetStream(bytes.NewReader(patched)); err != nil {
		return middleware.SerializeOutput{}, middleware.Metadata{}, err
	}

	in.Request = req

	return next.HandleSerialize(ctx, in)
}

// withZeroWaitTime reads a serialized JSON request body and returns it with
// "WaitTimeSeconds" set to 0; every other key is preserved.
func withZeroWaitTime(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}

	fields["WaitTimeSeconds"] = json.RawMessage("0")

	return json.Marshal(fields)
}

// resolveQueueURL caches the per-consumer queue URL, taking it from a
// subscription where THIS customer is the consumer. noSubscriptions is the
// error text for a customer with no subscriptions at all (it differs per
// caller).
//
// The list is requested with role=consumer to disambiguate "both" customers.
// The API answers 400 when that role does not match the credential's customer
// type (or the type is unknown), so on a 400 — and only a 400 — the list is
// requested again without a role. The rows are filtered to
// consumer_id == this customer either way: the API sends sqs_queue_url on
// producer-side rows too, and those queues are not ours to poll or purge.
func (c *Consumer) resolveQueueURL(ctx context.Context, noSubscriptions string) error {
	if c.queueURL != nil {
		return nil
	}

	subscriptions, err := c.ListSubscriptions(ctx, &ListSubscriptionsOptions{Role: "consumer"})
	if err != nil {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("failed to get subscriptions: %w", err)
		}

		if subscriptions, err = c.ListSubscriptions(ctx, nil); err != nil {
			return fmt.Errorf("failed to get subscriptions: %w", err)
		}
	}

	if len(subscriptions) == 0 {
		return errors.New(noSubscriptions)
	}

	var queueURL *string

	haveConsumerSub := false

	for _, sub := range subscriptions {
		if sub.ConsumerID != c.CustomerID {
			continue
		}

		haveConsumerSub = true

		if sub.SQSQueueURL != nil {
			queueURL = sub.SQSQueueURL

			break
		}
	}

	if !haveConsumerSub {
		return errors.New("no subscriptions found where you are the consumer")
	}

	if queueURL == nil {
		return errors.New("per-consumer queue not provisioned. This may be a legacy subscription. " +
			"Please contact support or create a new subscription to get a dedicated queue.")
	}

	c.queueURL = queueURL

	return nil
}

// DeleteNotification deletes a notification message from the SQS queue after processing.
func (c *Consumer) DeleteNotification(ctx context.Context, receiptHandle string) error {
	if c.queueURL == nil {
		return fmt.Errorf("queue URL not available. Call PollNotifications() first to initialize the queue URL")
	}

	queueURL := aws.ToString(c.queueURL)

	// Delete message.
	if _, err := c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	}); err != nil {
		return sdkerr.Wrap("failed to delete notification", err)
	}

	return nil
}

// ListSubscriptionRequests lists the consumer's own subscription requests.
// Allows consumers to track the status of their pending, approved, or rejected requests.
//
// This is the canonical parity method (see SDK-PARITY-METHODS-SPEC.md). It
// maps to GET /v1/subscription-requests[?status={status}], following every
// page of its paginated response until total_pages is exhausted.
//
// Parameters:
//   - status: Filter by request status. Canonical values: "pending",
//     "approved", "rejected". If empty, returns all requests regardless of
//     status.
//
// Returns a slice of subscription requests matching the filter.
func (c *Consumer) ListSubscriptionRequests(ctx context.Context, status string) ([]types.SubscriptionRequest, error) {
	return paginateAll(func(page int) ([]types.SubscriptionRequest, *int, int, error) {
		path := fmt.Sprintf("/v1/subscription-requests?page=%d&limit=100", page)
		if status != "" {
			path += "&status=" + url.QueryEscape(status)
		}

		var response struct {
			Requests   []types.SubscriptionRequest `json:"requests"`
			Page       *int                        `json:"page"`
			TotalPages int                         `json:"total_pages"`
		}

		if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, nil, 0, err
		}

		return response.Requests, response.Page, response.TotalPages, nil
	})
}

// ListMySubscriptionRequests is a backward-compatibility alias for
// ListSubscriptionRequests. Prefer ListSubscriptionRequests (the canonical
// parity name) in new code.
//
// Deprecated: use ListSubscriptionRequests.
func (c *Consumer) ListMySubscriptionRequests(ctx context.Context, status string) ([]types.SubscriptionRequest, error) {
	return c.ListSubscriptionRequests(ctx, status)
}

// GetSubscriptionRequest retrieves details of a specific subscription request by ID.
//
// Parameters:
//   - requestID: The subscription request ID (e.g., "req-abc123-def456").
//
// Returns the subscription request with all details.
func (c *Consumer) GetSubscriptionRequest(ctx context.Context, requestID string) (*types.SubscriptionRequest, error) {
	path := fmt.Sprintf("/v1/subscription-requests/%s", url.PathEscape(requestID))

	var result types.SubscriptionRequest
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// CancelSubscription cancels an active subscription.
//
// Parameters:
//   - subscriptionID: The subscription ID to cancel.
//
// Returns an error if the cancellation fails.
func (c *Consumer) CancelSubscription(ctx context.Context, subscriptionID string) error {
	return c.makeAPIRequest(ctx, "DELETE", fmt.Sprintf("/v1/subscriptions/%s", url.PathEscape(subscriptionID)), nil, nil)
}

// GetSubscription retrieves a specific subscription by ID.
//
// Parameters:
//   - subscriptionID: The subscription ID to retrieve.
//
// Returns the subscription details or an error if not found.
func (c *Consumer) GetSubscription(ctx context.Context, subscriptionID string) (*types.Subscription, error) {
	var sub types.Subscription
	if err := c.makeAPIRequest(ctx, "GET", fmt.Sprintf("/v1/subscriptions/%s", url.PathEscape(subscriptionID)), nil, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// ClearQueue clears all messages from the consumer's notification queue.
//
// This permanently deletes all messages in the queue. Use with caution.
//
// IMPORTANT: AWS limits PurgeQueue to once every 60 seconds per queue.
// Calling this method more frequently will result in an error.
func (c *Consumer) ClearQueue(ctx context.Context) error {
	// Initialize queue URL if not already set.
	if err := c.resolveQueueURL(ctx, "no active subscriptions found. Cannot determine queue URL"); err != nil {
		return err
	}

	queueURL := aws.ToString(c.queueURL)

	// Purge queue.
	if _, err := c.sqsClient.PurgeQueue(ctx, &sqs.PurgeQueueInput{
		QueueUrl: aws.String(queueURL),
	}); err != nil {
		// Categorize by the typed SQS exception, not by inspecting the raw
		// upstream error message text.
		var inProgress *sqstypes.PurgeQueueInProgress
		if errors.As(err, &inProgress) {
			return sdkerr.Wrap("queue purge already in progress. AWS limits PurgeQueue to once every 60 seconds per queue", err)
		}

		return sdkerr.Wrap("failed to clear queue", err)
	}

	return nil
}
