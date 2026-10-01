// Package types defines common types used across the SDK.
package types

import (
	"encoding/json"
	"fmt"
)

// EmptyPayloadHash is the SHA256 hash of an empty payload.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// CredentialMode selects how Consumer/Producer obtain the AWS credentials
// used to authenticate SDK operations.
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
	// sdk-schemas #17). The mint request is bootstrap-authenticated either
	// with a Helix API key (APIKey, header "HLX-API-Key") or with
	// AWSAccessKeyID/AWSSecretAccessKey (SigV4) — see APIKey and
	// credentials.SelectProvider's mode-resolution rule for which one wins
	// when both are set.
	CredentialModeSTS CredentialMode = "sts"
)

// Config contains configuration for the Consumer.
type Config struct {
	APIEndpoint        string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	CustomerID         string
	Region             string

	// APIKey is a Helix API key (hlx_-prefixed, created in the Helix portal
	// under API Keys), used to bootstrap CredentialModeSTS: the SDK sends it
	// as "Authorization: HLX-API-Key <key>" when minting a broker session,
	// with no SigV4 signature on that request. It is only ever sent to an
	// https:// endpoint, or to localhost/127.0.0.1 for local development —
	// see credentials.SelectProvider. When both APIKey and
	// AWSAccessKeyID/AWSSecretAccessKey are set, APIKey wins (see
	// credentials.SelectProvider's mode-resolution rule); the unused
	// credential is ignored with a one-time warning, never silently mixed.
	// Additive field: the zero value is a complete no-op for every existing
	// caller. Excluded from json.Marshal entirely (see Config.MarshalJSON)
	// so an incidental marshal (logging, a debug dump) never serializes the
	// raw key — String/GoString redaction alone does not cover
	// encoding/json, which never consults fmt.Stringer/GoStringer.
	APIKey string

	// CredentialMode selects "static" (default; existing AKIA behavior,
	// byte-identical) or "sts" (auto-refreshing broker-issued session
	// credentials, opt-in). Left empty, the mode is inferred: APIKey present
	// -> "sts" via the key; else static keys present -> "static" (preserves
	// today's behavior exactly); nothing present -> construction error.
	// "sts" bootstrapped by static keys is never inferred — it must be
	// requested explicitly, so no existing caller can silently start
	// minting STS sessions. See credentials.SelectProvider for the full
	// matrix.
	CredentialMode CredentialMode
}

// String implements fmt.Stringer. AWSSecretAccessKey and APIKey are
// redacted so a Config is safe to appear in an incidental %v/%+v — a debug
// print, or an error wrapped with %w — without leaking either credential.
func (c Config) String() string {
	secret := ""
	if c.AWSSecretAccessKey != "" {
		secret = "<redacted>"
	}
	key := ""
	if c.APIKey != "" {
		key = "<redacted>"
	}
	return fmt.Sprintf(
		"Config{APIEndpoint:%q, AWSAccessKeyID:%q, AWSSecretAccessKey:%q, CustomerID:%q, Region:%q, APIKey:%q, CredentialMode:%q}",
		c.APIEndpoint, c.AWSAccessKeyID, secret, c.CustomerID, c.Region, key, c.CredentialMode)
}

// GoString implements fmt.GoStringer. Go's fmt package only consults
// Stringer for %v/%+v — %#v bypasses it entirely and reflects every field
// verbatim via reflection. Without this method, "%#v" of a Config would
// print the raw AWSSecretAccessKey and APIKey.
func (c Config) GoString() string {
	return c.String()
}

// MarshalJSON implements json.Marshaler, omitting APIKey from the encoded
// output entirely (never present, not merely an empty string) — see the
// field's own doc comment for why. The shadow type below is an ANONYMOUS
// struct literal, not a named type declaration, so it is never mistaken for
// one of the API's wire payloads by this package's own TestWireStructs
// (wire_names_test.go), which treats every named struct declared in this
// package as a wire type needing a snake_case json tag on each field unless
// explicitly exempted — Config is SDK-side configuration, not a wire
// payload, and a struct tag on APIKey alone would otherwise flip that
// classification for the whole type.
func (c Config) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		APIEndpoint        string
		AWSAccessKeyID     string
		AWSSecretAccessKey string
		CustomerID         string
		Region             string
		CredentialMode     CredentialMode
	}{
		APIEndpoint:        c.APIEndpoint,
		AWSAccessKeyID:     c.AWSAccessKeyID,
		AWSSecretAccessKey: c.AWSSecretAccessKey,
		CustomerID:         c.CustomerID,
		Region:             c.Region,
		CredentialMode:     c.CredentialMode,
	})
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

// Dataset represents a dataset in the catalog.
//
// The customer-facing dataset API (GET, PATCH and list responses) identifies a
// dataset with "id" and never sends "_id" (which is the stored-document key).
// Decoding therefore fills ID from "_id" when present and from "id"
// otherwise, so ID is populated on every real response and can be passed
// straight to Producer.UpdateDataset / Consumer.GetDataset. IDAlias always
// holds the raw "id" value.
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
	// LastUpdatedData is when the dataset's underlying DATA last changed
	// (distinct from LastUpdated/UpdatedAt, which track metadata). Nullable:
	// datasets that have never had a file upload/delete carry null.
	LastUpdatedData *string `json:"last_updated_data,omitempty"`
	CreatedAt       string  `json:"created_at"`
	CreatedBy       string  `json:"created_by"`
	UpdatedAt       string  `json:"updated_at"`
	UpdatedBy       string  `json:"updated_by"`
	DeletedAt       *string `json:"deleted_at,omitempty"`
	DeletedBy       *string `json:"deleted_by,omitempty"`
	// FileFormats, TotalFiles, TotalSizeBytes, OriginalSize, CompressedSize,
	// and AccessCount are storage-usage stats populated by the API's dataset
	// stats pipeline; absent on datasets predating that pipeline.
	FileFormats    []string `json:"file_formats,omitempty"`
	TotalFiles     int64    `json:"total_files,omitempty"`
	TotalSizeBytes int64    `json:"total_size_bytes,omitempty"`
	OriginalSize   int64    `json:"original_size,omitempty"`
	CompressedSize int64    `json:"compressed_size,omitempty"`
	AccessCount    int64    `json:"access_count,omitempty"`
	// Marketplace pricing (schema PR #18). Optional and server-managed: nil
	// while the marketplace_payments feature flag is off — tolerate absence.
	Marketplace *DatasetMarketplace `json:"marketplace,omitempty"`
	// IsPublic is Deprecated: superseded by Visibility; still sent by the API.
	IsPublic bool `json:"is_public,omitempty"`
	// PricePerAccess is Deprecated: the legacy per-access price in USD cents,
	// superseded by Marketplace.PriceMonthlyCents; still sent when set.
	PricePerAccess int64 `json:"price_per_access,omitempty"`
}

// UnmarshalJSON decodes a dataset and normalises the identifier: ID falls back
// to the API's "id" key when "_id" is absent, and SizeBytes falls back to
// total_size_bytes (the only size the customer-facing API sends) when
// size_bytes is absent or zero. An explicit "_id" / non-zero "size_bytes"
// always wins, so bodies that carry the stored-document shape decode as before.
func (d *Dataset) UnmarshalJSON(data []byte) error {
	type plain Dataset // drops this method, avoiding infinite recursion

	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}

	if p.ID == "" {
		p.ID = p.IDAlias
	}

	if p.SizeBytes == 0 {
		p.SizeBytes = p.TotalSizeBytes
	}

	*d = Dataset(p)

	return nil
}
