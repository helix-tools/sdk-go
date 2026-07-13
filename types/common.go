// Package types defines common types used across the SDK.
package types

// EmptyPayloadHash is the SHA256 hash of an empty payload.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// CredentialMode selects how Consumer/Producer obtain the AWS credentials
// used to sign API requests (SigV4) and construct AWS service clients (KMS,
// SQS, SSM, S3).
type CredentialMode string

const (
	// CredentialModeStatic uses the long-lived AWSAccessKeyID/
	// AWSSecretAccessKey pair directly, byte-identical to the SDK's
	// pre-STS behavior (github.com/aws/aws-sdk-go-v2/credentials.
	// NewStaticCredentialsProvider). This is the default: an empty
	// CredentialMode with static keys set behaves exactly as before this
	// field existed — no existing caller's behavior changes.
	CredentialModeStatic CredentialMode = "static"

	// CredentialModeSTS auto-refreshes short-lived AWS STS session
	// credentials minted from the Helix Connect credential broker (POST
	// /v1/credentials/session — see credential_session.schema.json,
	// sdk-schemas #17). The mint request itself is bootstrap-authenticated
	// with AWSAccessKeyID/AWSSecretAccessKey (STS-PLAN.md §9 decision #1:
	// the broker's B0/B1 bootstrap is the existing SigV4 static key, not a
	// new API key — verified against the real broker implementation,
	// helix-tools/api PR #129, which accepts only SigV4). APIKey-based
	// bootstrap is reserved for a later phase (P5) and is not yet wired.
	CredentialModeSTS CredentialMode = "sts"
)

// Config contains configuration for the Consumer.
type Config struct {
	APIEndpoint        string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	CustomerID         string
	Region             string

	// APIKey is reserved for the platform-scoped Helix API key bootstrap
	// (hlx_-prefixed, STS-PLAN.md P5). It is currently NOT wired to any
	// authentication path — the credential broker accepts only SigV4 today
	// (see CredentialModeSTS) — so setting APIKey without
	// AWSAccessKeyID/AWSSecretAccessKey produces a clear construction-time
	// error rather than silently sending an unauthenticated request.
	// Additive field: the zero value is a complete no-op for every existing
	// caller.
	APIKey string

	// CredentialMode selects "static" (default; existing AKIA behavior,
	// byte-identical) or "sts" (auto-refreshing broker-issued session
	// credentials, opt-in). Left empty, the mode is inferred: static keys
	// present -> "static" (preserves today's behavior exactly); nothing
	// present -> construction error. "sts" is never inferred — it must be
	// requested explicitly, so no existing caller can silently start
	// minting STS sessions.
	CredentialMode CredentialMode
}

// DataFreshness enumerates allowed dataset update cadences.
type DataFreshness string

const (
	DataFreshnessTwoTimesPerDay  DataFreshness = "2x-per-day"
	DataFreshnessFourTimesPerDay DataFreshness = "4x-per-day"
	DataFreshnessHourly          DataFreshness = "hourly"
	DataFreshnessDaily           DataFreshness = "daily"
	DataFreshnessWeekly          DataFreshness = "weekly"
	DataFreshnessMonthly         DataFreshness = "monthly"
	DataFreshnessQuarterly       DataFreshness = "quarterly"
	DataFreshnessYearly          DataFreshness = "yearly"
	DataFreshnessOnce            DataFreshness = "once"
	DataFreshnessOnDemand        DataFreshness = "on-demand"
)

// DatasetStatus is the canonical lifecycle state of a dataset.
// Canonical contract values: active, inactive, archived.
type DatasetStatus = string

// Canonical DatasetStatus values.
const (
	DatasetStatusActive   DatasetStatus = "active"
	DatasetStatusInactive DatasetStatus = "inactive"
	DatasetStatusArchived DatasetStatus = "archived"
)

// DatasetUpdateInput contains fields for updating a dataset via PATCH.
// All fields are optional (pointer types) - nil means "no change".
type DatasetUpdateInput struct {
	Name          *string        `json:"name,omitempty"`
	Description   *string        `json:"description,omitempty"`
	Category      *string        `json:"category,omitempty"`
	DataFreshness *string        `json:"data_freshness,omitempty"`
	Visibility    *string        `json:"visibility,omitempty"`
	Status        *string        `json:"status,omitempty"`
	AccessTier    *string        `json:"access_tier,omitempty"`
	Version       *string        `json:"version,omitempty"`
	VersionNotes  *string        `json:"version_notes,omitempty"`
	Tags          []string       `json:"tags,omitempty"`
	Schema        map[string]any `json:"schema,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// Dataset represents a dataset in the catalog
type Dataset struct {
	ID            string        `json:"_id"`
	IDAlias       string        `json:"id,omitempty"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	ProducerID    string        `json:"producer_id"`
	Category      string        `json:"category"`
	DataFreshness DataFreshness `json:"data_freshness"`
	Visibility    string        `json:"visibility"`
	Status        string        `json:"status"`
	AccessTier    string        `json:"access_tier,omitempty"`
	S3Key         string        `json:"s3_key"`
	S3BucketName  string        `json:"s3_bucket_name,omitempty"`
	S3Bucket      string        `json:"s3_bucket"`
	// Encryption is the API's top-level encryption flag. The create endpoint
	// PROMOTES metadata.encryption_enabled to this field and drops it from
	// metadata, so download must fall back here (mirrors the Python SDK).
	Encryption      bool           `json:"encryption,omitempty"`
	SizeBytes       int64          `json:"size_bytes"`
	RecordCount     int            `json:"record_count"`
	Version         string         `json:"version"`
	VersionNotes    string         `json:"version_notes"`
	ParentDatasetID *string        `json:"parent_dataset_id,omitempty"`
	IsLatestVersion bool           `json:"is_latest_version"`
	Metadata        map[string]any `json:"metadata"`
	Schema          map[string]any `json:"schema"`
	Validation      map[string]any `json:"validation"`
	Tags            []string       `json:"tags"`
	Pricing         map[string]any `json:"pricing"`
	Stats           map[string]any `json:"stats"`
	LastUpdated     string         `json:"last_updated"`
	CreatedAt       string         `json:"created_at"`
	CreatedBy       string         `json:"created_by"`
	UpdatedAt       string         `json:"updated_at"`
	UpdatedBy       string         `json:"updated_by"`
	DeletedAt       *string        `json:"deleted_at,omitempty"`
	DeletedBy       *string        `json:"deleted_by,omitempty"`
	// Marketplace pricing (schema PR #18). Optional and server-managed: nil
	// while the marketplace_payments feature flag is off — tolerate absence.
	Marketplace *DatasetMarketplace `json:"marketplace,omitempty"`
}
