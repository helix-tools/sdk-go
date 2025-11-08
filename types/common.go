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
	Category      string         `json:"category"`
	DataFreshness DataFreshness  `json:"data_freshness"`
	Description   string         `json:"description"`
	ID            string         `json:"_id,omitempty"`
	Metadata      map[string]any `json:"metadata"`
	Name          string         `json:"name"`
	ProducerID    string         `json:"producer_id"`
	S3Key         string         `json:"s3_key"`
	SizeBytes     int64          `json:"size_bytes"`
}
