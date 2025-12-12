// Package types defines common types used across the SDK.
package types

// EmptyPayloadHash is the SHA256 hash of an empty payload.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Config contains configuration for the Consumer.
type Config struct {
	APIEndpoint        string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	CustomerID         string
	Region             string
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

// Dataset represents a dataset in the catalog
type Dataset struct {
	ID              string         `json:"_id"`
	IDAlias         string         `json:"id,omitempty"`
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	ProducerID      string         `json:"producer_id"`
	Category        string         `json:"category"`
	DataFreshness   DataFreshness  `json:"data_freshness"`
	Visibility      string         `json:"visibility"`
	Status          string         `json:"status"`
	S3Key           string         `json:"s3_key"`
	S3Bucket        string         `json:"s3_bucket"`
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
}
