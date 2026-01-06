# Changelog

## 2026-01-05

### Fixed
- **SDK Parity - Dataset ID**: Removed timestamp from dataset ID generation. IDs are now `{producer_id}-{slugified_name}` instead of `{producer_id}-{slugified_name}-{timestamp}`. This matches Portal behavior and prevents duplicate datasets when uploading the same dataset multiple times.
- **SDK Parity - S3 Bucket Field**: Removed redundant `s3_bucket` field from dataset payload. Now only sends `s3_bucket_name` which is what the Go API expects. This fixes MongoDB field naming mismatch issues.

## 2026-01-03

### Fixed
- **SDK Parity**: Added `Tier` field to `Company` struct to match customer.schema.json
- **SDK Parity**: Updated `OnboardingInfo` struct with `WelcomeEmailSent` and `WelcomeEmailSentAt` fields
- **SDK Parity**: Updated `InfrastructureInfo` struct - renamed `IAMUser` to `IAMUserARN`, added `CredentialsSSMPath`
- **SDK Parity**: Fixed `DownloadURLInfo` in consumer.go to handle both Go API format (flat `file_name`, `file_size`) and legacy nested `dataset` object
- **SDK Parity**: Fixed `InviteUserRequest` role comment to include `superadmin`

## 2026-01-01

### Fixed
- **SQS Polling Timeouts**: Configure AWS HTTP client timeouts to avoid long-poll hangs during notification polling.

## 2025-12-31

### Fixed
- **SSM Path Fallbacks**: Producer now resolves SSM parameters from `/helix-tools/{env}/customers` with legacy fallbacks and optional `HELIX_SSM_CUSTOMER_PREFIX` override.
- **Dataset Registration**: Producer payload now includes `s3_bucket_name` and a default `access_tier` (Go API requirement) while keeping `s3_bucket` for compatibility.
- **API Endpoint Default**: Producer/consumer now default to `HELIX_API_ENDPOINT` or `https://api-go.helix.tools` when no endpoint is provided.

## 2025-12-29

### Added
- **Integration Test Package**: New `/api` package with comprehensive integration tests for the Go API.
  - `client.go`: HTTP client with AWS SigV4 authentication for API testing
  - `config.go`: Environment-based test configuration loader
  - `cleanup.go`: LIFO cleanup registry for test resource management
  - `fixtures.go`: Test data generators with TEST_ prefix for isolation
  - `companies_test.go`: Company CRUD integration tests
  - `datasets_test.go`: Dataset CRUD integration tests
  - `subscription_requests_test.go`: Subscription request flow tests
  - `subscriptions_test.go`: Subscription management tests

- **Type Definitions**: Added new types in `/types` package:
  - `company.go`: Company, Address, CreateCompanyRequest, UpdateCompanyRequest
  - `subscription.go`: Subscription, SubscriptionsResponse, RevokeSubscriptionResponse
  - `subscription_request.go`: SubscriptionRequest, ApproveRejectPayload

- **Makefile**: Added Makefile with test targets:
  - `make test-integration`: Run integration tests
  - `make test-integration-local`: Run against localhost
  - `make test-integration-prod`: Run against production

### Documentation
- Created ADR-0022: Integration and E2E Test Architecture

## 2025-12-18

### Added
- **Empty File Validation**: Added validation in `UploadDataset()` to reject empty files with a clear error message: `"file is empty: {path} (no data to upload)"`. This prevents uploading zero-byte files which would fail downstream processing.
- **Unit Tests**: Added `producer/upload_validation_test.go` with tests for empty file validation.

## 2025-12-14

### Fixed
- **Notification Parsing Bug**: Fixed notification parsing error when receiving raw SQS messages. The consumer now handles both SNS-wrapped messages (default) and raw notification payloads (when `raw_message_delivery = true` or direct SQS). Previously, when a raw message was received, the code attempted to parse an empty `snsMessage.Message` string, causing the notification parsing to fail.

### Added
- **Unit Tests**: Added notification parsing tests to `consumer/consumer_test.go` covering both SNS-wrapped and raw message formats.
