# Helix Go SDK - Component Memory

## Component Identity
- **Agent**: helix-sdk-go
- **Workspace**: `dme/sdk/go`
- **Module**: `github.com/helix-tools/sdk-go`
- **Go Version**: 1.25.1

---

## Directory Structure

```
sdk/go/
├── api/                    # Integration test package
│   ├── client.go           # HTTP client with AWS SigV4 auth
│   ├── config.go           # Test configuration loader
│   ├── cleanup.go          # Test resource cleanup utilities
│   ├── fixtures.go         # Test data generators
│   ├── companies_test.go   # Company CRUD tests
│   ├── datasets_test.go    # Dataset CRUD tests
│   ├── subscription_requests_test.go
│   └── subscriptions_test.go
├── consumer/
│   ├── consumer.go         # Consumer client implementation
│   └── consumer_test.go    # Unit tests
├── producer/
│   ├── producer.go         # Producer client implementation
│   ├── analyze.go          # Data analysis/schema inference
│   ├── dataset_payload.go  # Payload builder utilities
│   ├── analyze_test.go
│   ├── dataset_payload_test.go
│   └── upload_validation_test.go
├── types/
│   ├── common.go           # Config, Dataset, DataFreshness
│   ├── company.go          # Company, Address, User types
│   ├── subscription.go     # Subscription types
│   └── subscription_request.go  # Request types
├── test/
│   └── e2e_main.go         # E2E test entry point
├── go.mod
├── go.sum
├── Makefile
├── README.md
├── CHANGELOG.md
└── AGENTS.md
```

---

## Exported Types

### types/common.go
- `Config` - Client configuration (APIEndpoint, AWSAccessKeyID, AWSSecretAccessKey, CustomerID, Region)
- `Dataset` - Full dataset metadata (ID, Name, Description, ProducerID, Category, etc.)
- `DataFreshness` - Enum for update cadence (hourly, daily, weekly, monthly, quarterly, yearly, once, on-demand, 2x-per-day, 4x-per-day)
- `EmptyPayloadHash` - SHA256 hash constant for SigV4

### types/company.go
- `Company` - Company/customer entity
- `Address` - Physical address
- `CompanySettings` - Notification/rate limits
- `OnboardingInfo` - Onboarding status
- `InfrastructureInfo` - Provisioned AWS resources
- `CompanyUser` - User within company
- `UserPermissions` - User capability flags
- `CreateCompanyRequest`, `UpdateCompanyRequest`
- `CompaniesResponse`, `CreateCompanyResponse`
- `InviteUserRequest`, `CompanyUsersResponse`

### types/subscription.go
- `Subscription` - Active subscription with queue info
- `SubscriptionsResponse`
- `CreateSubscriptionRequest`
- `RevokeSubscriptionResponse`

### types/subscription_request.go
- `SubscriptionRequest` - Access request from consumer
- `CreateSubscriptionRequestPayload`
- `ApproveRejectPayload`
- `SubscriptionRequestsResponse`
- `ApproveRequestResponse`

---

## Consumer Client

### Constructor
```go
func NewConsumer(cfg types.Config) (*Consumer, error)
```

### Exported Methods
| Method | Description |
|--------|-------------|
| `GetDataset(ctx, datasetID) (*types.Dataset, error)` | Retrieve dataset metadata |
| `GetDownloadURL(ctx, datasetID) (*DownloadURLInfo, error)` | Get presigned download URL |
| `DownloadDataset(ctx, datasetID, outputPath) error` | Download, decrypt, decompress |
| `ListDatasets(ctx) ([]Dataset, error)` | List all available datasets |
| `ListSubscriptions(ctx) ([]Subscription, error)` | List consumer's subscriptions |
| `PollNotifications(ctx, opts) ([]Notification, error)` | Poll SQS for upload notifications |
| `DeleteNotification(ctx, receiptHandle) error` | Acknowledge/delete a notification |
| `ClearQueue(ctx) error` | Purge all messages from queue |

### Internal Types (consumer package)
- `DownloadURLInfo` - Presigned URL response
- `Dataset` - Simplified dataset for listings
- `Notification` - SQS notification message
- `Subscription` - Subscription with queue details
- `PollNotificationsOptions` - Polling configuration

### Queue URL Resolution Logic
1. On first `PollNotifications()` call, if `queueURL` is nil:
2. Call `ListSubscriptions()` to get all subscriptions
3. Find first subscription with non-nil `SQSQueueURL`
4. Cache the queue URL in `c.queueURL` for subsequent calls
5. All subscriptions for same consumer share the same queue

---

## Producer Client

### Constructor
```go
func NewProducer(cfg types.Config) (*Producer, error)
```

Initializes by:
1. Loading AWS config with static credentials
2. Validating credentials via STS GetCallerIdentity
3. Fetching S3 bucket from SSM Parameter Store
4. Fetching KMS key ID from SSM (optional, warns if missing)

### Exported Methods
| Method | Description |
|--------|-------------|
| `UploadDataset(ctx, filePath, opts) (*types.Dataset, error)` | Upload with compression/encryption |
| `ListMyDatasets(ctx) ([]types.Dataset, error)` | List producer's datasets |
| `GetDatasetSubscribers(ctx, datasetID) ([]types.Subscription, error)` | List dataset subscribers |
| `RevokeSubscription(ctx, subscriptionID) error` | Revoke a subscription |

### Helper Functions
- `NewUploadOptions(datasetName) UploadOptions` - Create options with sane defaults

### UploadOptions
```go
type UploadOptions struct {
    Category         string
    Compress         bool              // Required: true
    CompressionLevel int               // Default: 6
    DataFreshness    types.DataFreshness
    DatasetName      string
    Description      string
    Encrypt          bool              // Required: true
    Metadata         map[string]any
    DatasetOverrides map[string]any
}
```

### SSM Parameter Resolution
Candidates checked in order:
1. `$HELIX_SSM_CUSTOMER_PREFIX/{customerID}/{param}`
2. `/helix-tools/{env}/customers/{customerID}/{param}`
3. `/helix/{env}/customers/{customerID}/{param}`
4. `/helix/customers/{customerID}/{param}`

Where `{env}` comes from `HELIX_ENVIRONMENT` or `ENVIRONMENT` env vars (default: "production")

### Dataset ID Generation
Format: `{producer_id}-{slugified_name}` (no timestamp, enables upsert)

### Upload Flow
1. Validate encryption/compression required
2. Analyze data (schema inference, field emptiness)
3. Read file
4. Compress with gzip
5. Encrypt with KMS envelope encryption (AES-256-GCM)
6. Upload to S3 with cost-tracking tags
7. POST to API to register (auto-updates on 409 conflict)

---

## api/ Package (Integration Tests)

### Client
```go
func NewClient(ctx, baseURL, creds, region) (*Client, error)
func NewTestClient(t, cfg, creds) *Client
```

Methods: `Get`, `Post`, `Patch`, `Put`, `Delete`, `Request`

### Error Helpers
- `IsNotFoundError(err) bool`
- `IsForbiddenError(err) bool`
- `IsConflictError(err) bool`
- `IsBadRequestError(err) bool`

### Configuration
- `LoadTestConfig(t) TestConfig` - Loads from env vars
- `LoadCredentialsFromSSM(ctx, customerID) (Credentials, error)`

### Fixtures
- `NewTestCompany(testID, customerType)`
- `NewTestProducerCompany(testID)`
- `NewTestConsumerCompany(testID)`
- `NewTestDatasetPayload(testID, producerID)`
- `NewTestSubscriptionRequest(producerID, datasetID)`
- `NewTestUserInvite(testID)`

---

## Dependencies (go.mod)

**Direct:**
- `github.com/aws/aws-sdk-go-v2` v1.39.6
- `github.com/aws/aws-sdk-go-v2/config` v1.31.17
- `github.com/aws/aws-sdk-go-v2/credentials` v1.18.21
- `github.com/aws/aws-sdk-go-v2/service/kms` v1.48.0
- `github.com/aws/aws-sdk-go-v2/service/s3` v1.90.0
- `github.com/aws/aws-sdk-go-v2/service/sqs` v1.42.13
- `github.com/aws/aws-sdk-go-v2/service/ssm` v1.67.0
- `github.com/aws/aws-sdk-go-v2/service/sts` v1.39.1

---

## SDK Parity Analysis

### ✅ Methods Present in All SDKs

| Go | Python | TypeScript |
|----|--------|------------|
| Consumer.GetDataset | get_dataset | getDataset |
| Consumer.GetDownloadURL | get_download_url | getDownloadUrl |
| Consumer.DownloadDataset | download_dataset | downloadDataset |
| Consumer.ListDatasets | list_datasets | listDatasets |
| Consumer.ListSubscriptions | list_subscriptions | listSubscriptions |
| Consumer.PollNotifications | poll_notifications | pollNotifications |
| Consumer.DeleteNotification | delete_notification | *(auto-ack)* |
| Consumer.ClearQueue | clear_queue | ❌ Missing |
| Producer.UploadDataset | upload_dataset | uploadDataset |
| Producer.ListMyDatasets | list_my_datasets | listMyDatasets |
| Producer.GetDatasetSubscribers | get_dataset_subscribers | getDatasetSubscribers |
| Producer.RevokeSubscription | revoke_subscription | revokeSubscription |

### ⚠️ Methods Missing from Go SDK

| Python | TypeScript | Description |
|--------|------------|-------------|
| subscribe_to_dataset | ❌ | Direct subscription |
| create_subscription_request | createSubscriptionRequest | Request access |
| list_subscription_requests | listSubscriptionRequests | List requests |
| get_subscription_request | ❌ | Get single request |
| update_dataset | *(via upsert)* | Update existing dataset |
| delete_dataset | ❌ | Delete a dataset |
| approve_subscription_request | approveSubscriptionRequest | **Producer only** |
| reject_subscription_request | rejectSubscriptionRequest | **Producer only** |
| list_subscribers | ❌ | List all subscribers |

### ⚠️ TypeScript Missing from Go

| TypeScript | Description |
|------------|-------------|
| clearQueue | Clear notification queue |

---

## Known Issues & TODOs

### From Code Comments

1. **Logger**: `TODO: Use thalesfsp/sypl logger, and set log levels to 'debug'` (consumer.go, producer.go)
2. **Context**: `TODO: Allow to pass context for better control` (NewConsumer, NewProducer)
3. **SSM Patterns**: `TODO: Get pattern from AWS SSM` for WaitTimeSeconds (consumer.go)
4. **API Endpoint**: `TODO: Get this from AWS SSM` for default endpoint
5. **Region**: `TODO: Get this from AWS SSM` for default region
6. **S3 Key Format**: `TODO: Get this from AWS SSM` for S3 key pattern

### Parity Concerns

1. **Missing Consumer Methods**:
   - `CreateSubscriptionRequest` - Consumers can't request access via Go SDK
   - `ListSubscriptionRequests` - Consumers can't view their pending requests
   - `SubscribeToDataset` - Legacy direct subscription

2. **Missing Producer Methods**:
   - `ApproveSubscriptionRequest` - Producers can't approve via Go SDK
   - `RejectSubscriptionRequest` - Producers can't reject via Go SDK
   - `DeleteDataset` - Producers can't delete datasets via Go SDK
   - `ListSubscribers` - Producers can't list all subscribers

3. **TypeScript Missing**: `ClearQueue` method not in TypeScript SDK

4. **Admin Functions**: Python SDK has `admin.py` - correctly NOT implemented in Go (per AGENTS.md)

---

## API Endpoint

Default: `https://api-go.helix.tools` (or `HELIX_API_ENDPOINT` env var)

---

## Recent Changes (from CHANGELOG.md)

- **2026-01-07**: Added `ClearQueue()` to Consumer
- **2026-01-06**: Fixed `updateDataset` response parsing
- **2026-01-05**: Dataset upsert behavior (auto-update on 409), removed timestamp from dataset ID
- **2026-01-03**: SDK parity fixes for Company, OnboardingInfo, InfrastructureInfo
- **2025-12-31**: SSM path fallbacks, dataset registration payload fixes
- **2025-12-29**: Integration test package added
- **2025-12-18**: Empty file validation
- **2025-12-14**: Fixed notification parsing for raw SQS messages

---

## File Checksums (for change detection)

Last analyzed: 2025-02-09

| File | Lines |
|------|-------|
| consumer/consumer.go | ~600 |
| producer/producer.go | ~450 |
| producer/analyze.go | ~280 |
| producer/dataset_payload.go | ~130 |
| types/common.go | ~60 |
| types/company.go | ~130 |
| types/subscription.go | ~50 |
| types/subscription_request.go | ~55 |
| api/client.go | ~180 |
| api/config.go | ~150 |

---

## Related Components

- **helix-sdk-python** (`../python/`) - Python SDK, has admin functions
- **helix-sdk-ts** (`../typescript/`) - TypeScript SDK
- **helix-sdk-schemas** (`../schemas/`) - Canonical schema definitions
- **helix-api-go** - Go API backend

---

## Action Items for Parity

### High Priority
1. Add `CreateSubscriptionRequest()` to Consumer
2. Add `ListSubscriptionRequests()` to Consumer
3. Add `ApproveSubscriptionRequest()` to Producer
4. Add `RejectSubscriptionRequest()` to Producer

### Medium Priority
5. Add `DeleteDataset()` to Producer
6. Add `ListSubscribers()` to Producer
7. Add `ClearQueue()` to TypeScript SDK

### Low Priority
8. Add structured logging with sypl
9. Support context passing in constructors
10. SSM-based configuration for defaults

---

## Coding Style Guide

This section documents the coding patterns and conventions used in the Go SDK codebase for consistency in future development.

### Naming Conventions

#### Package Names
- **Lowercase, single word**: `producer`, `consumer`, `types`, `api`
- **No underscores or hyphens**
- **Descriptive and domain-specific**

#### File Names
- **Lowercase with underscores**: `consumer.go`, `dataset_payload.go`, `subscription_request.go`
- **Test files use `_test.go` suffix**: `consumer_test.go`, `analyze_test.go`
- **One primary type per file** when logical (e.g., `subscription.go`, `company.go`)

#### Struct Names
- **PascalCase**: `Producer`, `Consumer`, `Dataset`, `UploadOptions`
- **Clear, domain-specific names** without stuttering (e.g., `types.Dataset` not `types.DatasetType`)
- **Response types suffixed with `Response`**: `SubscriptionsResponse`, `CompaniesResponse`
- **Request types suffixed with `Request`**: `CreateCompanyRequest`, `InviteUserRequest`
- **Payload types suffixed with `Payload`**: `CreateSubscriptionRequestPayload`

#### Method Names
- **PascalCase for exported**: `UploadDataset`, `GetDownloadURL`, `ListSubscriptions`
- **camelCase for unexported**: `makeAPIRequest`, `compressData`, `encryptData`
- **Verb-first pattern**: `Get*`, `List*`, `Create*`, `Update*`, `Delete*`, `Revoke*`
- **Boolean getters use `Is*`**: `IsConflict()`, `IsNotFoundError()`

#### Variable Names
- **camelCase**: `datasetID`, `awsConfig`, `httpClient`
- **Short names for loop variables**: `i`, `k`, `v`, `sub`, `err`
- **Descriptive names for structs/complex types**: `producerClient`, `subscriptionID`
- **Context always named `ctx`**

#### Constants
- **PascalCase for exported**: `DefaultAPIEndpoint`, `DefaultRegion`, `EmptyPayloadHash`
- **camelCase for unexported**: `emptyPayloadHash`, `largeFileThreshold`
- **Grouped with `const ()` blocks**

#### Type Aliases (Enums)
- **Use `type X string` pattern**: `type DataFreshness string`
- **Constants prefixed with type name**: `DataFreshnessDaily`, `DataFreshnessWeekly`

### Documentation Style

#### Package Documentation
```go
// Package producer provides functionality for uploading datasets to the Helix Connect Platform.
//
// It handles the entire lifecycle of dataset production, including authentication,
// encrypting, compressing, uploading datasets, and notifying subscribers via SNS.
package producer
```

#### Struct Documentation
```go
// Producer handles uploading and managing datasets on Helix Connect platform.
type Producer struct {
```

#### Method Documentation
```go
// UploadDataset uploads a dataset with optional encryption and compression.
//
// NOTE: Use NewUploadOptions() to get sane defaults.
func (p *Producer) UploadDataset(ctx context.Context, filePath string, opts UploadOptions) (*types.Dataset, error) {
```

#### Field Documentation (inline)
```go
type UploadOptions struct {
    Category         string
    Compress         bool              // Required: true
    CompressionLevel int               // Default: 6 (gzip compression level 1-9)
    DataFreshness    types.DataFreshness
}
```

#### TODO Comments
```go
// TODO: Use thalesfsp/sypl logger, and set log levels to `debug`.
// TODO: Allow to pass context for better control.
// TODO: Get this from AWS SSM.
```

#### IMPORTANT/WARNING Comments
```go
// IMPORTANT: This uses a DEDICATED queue for this consumer.
// IMPORTANT: AWS limits PurgeQueue to once every 60 seconds per queue.
```

### Code Organization

#### Import Grouping
Three groups separated by blank lines:
1. Standard library
2. Internal packages (this module)
3. External packages

```go
import (
    "bytes"
    "context"
    "fmt"

    "github.com/helix-tools/sdk-go/types"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/config"
)
```

#### Package Structure
- `types/` - Shared types used across packages
- `producer/` - Producer client and related functionality
- `consumer/` - Consumer client and related functionality
- `api/` - Integration test utilities
- `test/` - E2E test entry points

#### Export Patterns
- **Exported**: Types/functions intended for SDK users (`Producer`, `Consumer`, `UploadDataset`)
- **Unexported**: Internal helpers (`makeAPIRequest`, `compressData`, `ssmParamCandidates`)
- **Struct fields**: Export API-visible fields, unexport internal state (`awsConfig`, `httpClient`)

### Error Handling

#### Custom Error Types
```go
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
```

#### Error Wrapping
Use `fmt.Errorf` with `%w` verb for wrapping:
```go
return nil, fmt.Errorf("failed to load AWS config: %w", err)
return nil, fmt.Errorf("KMS encryption failed: %w", err)
return nil, fmt.Errorf("failed to open file: %w", err)
```

#### Error Checking Pattern
```go
if err != nil {
    return nil, fmt.Errorf("context: %w", err)
}
```

#### Error Type Assertions
Use `errors.As` for type checking:
```go
var apiErr *APIError
if errors.As(err, &apiErr) && apiErr.IsConflict() {
    // Handle conflict
}
```

#### Error Helper Functions
```go
func IsNotFoundError(err error) bool {
    if apiErr, ok := err.(*APIError); ok {
        return apiErr.StatusCode == http.StatusNotFound
    }
    return false
}
```

### Struct Patterns

#### Struct Definitions
- Public fields first, then private
- Group related fields
- Use JSON tags with `snake_case`
- Use `omitempty` for optional fields
- Use pointer types for nullable fields

```go
type Dataset struct {
    ID              string         `json:"_id"`
    IDAlias         string         `json:"id,omitempty"`
    Name            string         `json:"name"`
    Description     string         `json:"description"`
    ProducerID      string         `json:"producer_id"`
    ParentDatasetID *string        `json:"parent_dataset_id,omitempty"`
    Metadata        map[string]any `json:"metadata"`
}
```

#### Constructor Pattern
Named constructors returning `(*Type, error)`:
```go
func NewProducer(cfg types.Config) (*Producer, error) {
    // Validation
    if cfg.APIEndpoint == "" {
        cfg.APIEndpoint = DefaultAPIEndpoint
    }
    
    // Setup
    awsCfg, err := config.LoadDefaultConfig(...)
    if err != nil {
        return nil, fmt.Errorf("failed to load AWS config: %w", err)
    }
    
    return &Producer{
        APIEndpoint: cfg.APIEndpoint,
        // ...
        awsConfig:  awsCfg,
        httpClient: &http.Client{},
    }, nil
}
```

#### Options Pattern
Use dedicated options structs with `New*Options` constructor:
```go
type UploadOptions struct {
    DatasetName      string
    Category         string
    Compress         bool
    CompressionLevel int
}

func NewUploadOptions(datasetName string) UploadOptions {
    return UploadOptions{
        DatasetName:      datasetName,
        Category:         "general",
        Compress:         true,
        CompressionLevel: 6,
    }
}
```

#### Method Receivers
- **Pointer receivers** for methods that modify state or for consistency
- **Value receivers** rarely used (only for small immutable types)

```go
func (p *Producer) UploadDataset(ctx context.Context, ...) (*types.Dataset, error)
func (c *Consumer) GetDataset(ctx context.Context, ...) (*types.Dataset, error)
func (e *APIError) Error() string
func (e *APIError) IsConflict() bool
```

### Interface Patterns

#### Interface Definitions
- Not heavily used in this codebase (concrete types preferred)
- When used, follow `-er` suffix convention
- Define interfaces where needed, not preemptively

#### Implicit Interface Satisfaction
Standard library interfaces like `error` and `io.Reader` are satisfied:
```go
func (e *APIError) Error() string  // Satisfies error interface
```

### Test Patterns

#### Test Function Naming
```go
func TestSubscriptions(t *testing.T)                    // Main test
func TestSubscriptionWithDataset(t *testing.T)          // Variation
func TestNotificationParsingSNSWrapped(t *testing.T)    // Specific case
func TestNotificationParsingPreviouslyFailingCase(t *testing.T)  // Bug regression
```

#### Subtests
Use `t.Run` for logical groupings:
```go
t.Run("List_Consumer_Subscriptions", func(t *testing.T) {
    // ...
})

t.Run("Create_Subscription_ViaApproval", func(t *testing.T) {
    // ...
})
```

#### Test Helpers
- Use `t.Helper()` at start of helper functions
- Use `t.Skip()` for conditional skipping
- Use `t.Logf()` for informational output

```go
func (c TestConfig) RequireProducerCredentials(t *testing.T) {
    t.Helper()
    if c.ProducerCredentials.CustomerID == "" {
        t.Skip("HELIX_TEST_PRODUCER_ID not set")
    }
}
```

#### Test Assertions
Use direct comparisons with `t.Fatalf` or `t.Errorf`:
```go
if sub.ID != "sub-123" {
    t.Fatalf("unexpected id: %s", sub.ID)
}

if result.EventType != "dataset_updated" {
    t.Errorf("expected event_type 'dataset_updated', got '%s'", result.EventType)
}
```

### Formatting Conventions

#### gofmt Compliance
- All code must pass `gofmt`
- Use `goimports` for import organization
- No manual formatting overrides

#### Line Length
- No strict limit, but aim for readability
- Break long function signatures across lines
- Break long struct literals across lines

#### Blank Lines
- One blank line between functions
- One blank line between logical sections within functions
- Blank line after `if err != nil { return }` blocks

#### Defer Placement
Place `defer` immediately after resource acquisition:
```go
file, err := os.Open(filePath)
if err != nil {
    return nil, fmt.Errorf("failed to open file: %w", err)
}
defer file.Close()
```

#### Console Output (SDK-specific)
Use emoji prefixes for user-facing output:
```go
fmt.Printf("📦 Compressing %d bytes with gzip (level %d)...\n", len(data), opts.CompressionLevel)
fmt.Printf("🔒 Encrypting %d bytes with KMS key...\n", len(data))
fmt.Printf("📤 Uploading %d bytes to S3...\n", len(data))
fmt.Printf("✅ Uploaded to s3://%s/%s\n", p.BucketName, s3Key)
fmt.Printf("⚠️  Warning: Data analysis failed\n")
```

### JSON Conventions

#### Field Names
- Use `snake_case` in JSON tags
- Match API contract exactly
- Use `_id` for MongoDB-style IDs, with `id` alias

```go
ID      string `json:"_id"`
IDAlias string `json:"id,omitempty"`
```

#### Optional Fields
- Use `omitempty` for optional fields
- Use pointer types for nullable fields that need explicit null

```go
Phone    *string `json:"phone,omitempty"`
DeletedAt *string `json:"deleted_at,omitempty"`
```

#### Any Type
Use `map[string]any` for dynamic/unknown structures:
```go
Metadata map[string]any `json:"metadata"`
Schema   map[string]any `json:"schema"`
```

### Context Usage

- First parameter in all API methods
- Named `ctx`
- Passed through to all AWS SDK calls
- Created with `context.Background()` in tests and constructors

```go
func (p *Producer) UploadDataset(ctx context.Context, filePath string, opts UploadOptions) (*types.Dataset, error)
func (c *Consumer) GetDataset(ctx context.Context, datasetID string) (*types.Dataset, error)
```
