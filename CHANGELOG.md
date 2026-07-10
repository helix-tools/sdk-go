# Changelog

## Unreleased

### Added
- **STS session credentials (opt-in)** — STS-PLAN.md §P3 (B1). New
  `credentials` package (`github.com/helix-tools/sdk-go/v2/credentials`)
  implements an `aws.CredentialsProvider` that mints short-lived AWS STS
  session credentials from the Helix Connect credential broker (`POST
  /v1/credentials/session`, contract frozen in sdk-schemas #17), wrapped in
  `aws.CredentialsCache` for auto-refresh (proactive refresh at ~2/3 of the
  900s TTL, i.e. remaining <=5min, with jitter; single-flight coalescing and
  the cache's own fail-closed behavior on refresh failure all courtesy of
  `aws.CredentialsCache` — no reimplementation). `types.Config` gains two
  additive fields: `APIKey` (reserved for a later phase's platform-scoped
  bootstrap — not yet wired, see below) and `CredentialMode` (`"static"` |
  `"sts"`, **default `"static"`**). `NewConsumer`/`NewProducer` select the
  provider via `credentials.SelectProvider`; every other AWS client (KMS,
  SQS, SSM, S3) and the hand-rolled API SigV4 signer inherit whichever
  provider was selected automatically — **zero signing-path changes** (the
  session token is emitted in `X-Amz-Security-Token` and included in
  SigV4 `SignedHeaders` automatically by the existing signer whenever the
  retrieved credential carries one). `api/config.go` gains a sibling
  `NewAWSConfigSTS` test-utility variant (`NewAWSConfig`/
  `LoadCredentialsFromSSM` untouched). Zero new runtime dependencies —
  built entirely on `aws-sdk-go-v2`'s already-vendored `aws.Credentials{}`/
  `aws.CredentialsProvider`/`aws.CredentialsCache` primitives.
  - **Static remains the default and is byte-identical.** `CredentialMode`
    is *never* inferred as `"sts"` — only an explicit opt-in enables it.
    Every existing caller (Ringboost's live static-key path included) is
    unaffected: same construction call, same signed-request shape, no
    `X-Amz-Security-Token` header, pinned by
    `TestSelectProvider_StaticPath_ByteIdenticalToDirectConstruction` and
    `TestMakeAPIRequest_StaticMode_NoSecurityTokenHeader` (consumer +
    producer).
  - **Mint-request bootstrap is the existing static AWS key (SigV4), not a
    bearer API key** — STS-PLAN.md §9 decision #1, verified directly
    against the real broker implementation (`helix-tools/api` PR #129:
    `RequireMachineAuth(AuthMethodSigV4)` accepts only SigV4). `APIKey` is
    additive/reserved for a later phase; setting it without static keys
    returns a clear "not yet supported" construction error rather than
    silently sending an unauthenticated request.
  - Publish gate unchanged from every other SDK release: this PR **merges
    inert** (default static). Tags are pushed only after the broker is
    live-dark, session-token-aware `UnifiedAuth` is verified, and local-first
    live validation passes across a real 15-minute expiry boundary — **not
    part of this PR**. Target release: **v2.6.0**.
  - See `STS_C0_INVENTORY.md` at the repo root for the full grep-based
    bind-site inventory this change was derived from (codex gate finding
    #6).

### Tests
- `credentials/broker_test.go` (new): mint client happy/bad/edge paths
  (401/403/429/5xx/malformed-JSON/missing-fields, each with the correct
  retry-vs-fail-fast classification and call-count assertions); clock-skew
  hardening in both directions; the real `aws.CredentialsCache` refresh
  engine exercised end-to-end via compressed real-time windows (fresh /
  within-window / refresh-storm / single-flight under `-race` / optFn
  override / fail-closed-on-broker-down); `SelectProvider`'s full
  mode-inference matrix; the static-path byte-identical regression pin.
  Every load-bearing assertion above was watched to fail for the right
  reason with a temporary reverted fix, then restored (default-static
  inference, session-token propagation into the signed request, fail-closed
  behavior, 403 non-retryability).
- `consumer/consumer_sts_test.go`, `producer/producer_sts_test.go` (new):
  wire-level pins that static-mode signing never emits
  `X-Amz-Security-Token` and sts-mode signing emits it **and** includes it
  in SigV4 `SignedHeaders` (an attached-but-unsigned token is rejected
  server-side per `credential_session.schema.json`).

### Fixed
- **`RateLimitConfig` buckets are now `*RateLimitBucket` pointers**
  (DRIFT-GOSDK-RATELIMIT-1). The `Read`/`Write`/`Delete` fields were value
  `RateLimitBucket` structs, but the authoritative Go API
  (`internal/pkg/agents` `RateLimitConfig`) uses `*RateLimitBucket` where the
  pointer carries the contract: nil bucket = unlimited (key omitted),
  present `{"rpm":0}` = blocked (HTTP 429 `class_blocked_by_registry`),
  present `{"rpm":>0}` = limited. With value structs `omitempty` is a no-op,
  so a zero-value bucket always serialized as `{"rpm":0}` and the SDK could
  not represent — nor round-trip — the unlimited-vs-blocked distinction; it
  silently collapsed both into "blocked". Wire keys are unchanged
  (`read`/`write`/`delete`, `rpm`/`burst`), so this is source-compatible at
  the JSON level; Go callers constructing a `RateLimitConfig` must now take
  the address of their buckets (`&RateLimitBucket{...}`) and may leave a
  class `nil` to mean unlimited.

### Tests
- Added `TestRateLimitConfig_PointerBucketStatesRoundTrip` and
  `TestRateLimitConfig_AbsentFieldsDecodeToNil` (`agent/types_test.go`) —
  pin all three states across a full marshal/unmarshal cycle: a nil bucket
  must be absent on the wire and decode back to nil; a present `{"rpm":0}`
  must round-trip as a non-nil bucket with RPM 0 (distinct from nil); a
  present `{"rpm":>0}` must round-trip its rate and burst. The nil-vs-zero
  assertions are structurally impossible to satisfy under value semantics.
## 2026-06-09 (v2.3.0 — SDK parity + enum alignment, intended tag, not yet released)

### Added
- **Typed status/tier constants in `types/`** matching the canonical contract
  exactly (security-audit R3.2), replacing comment-documented-only values:
  - `SubscriptionStatus` = `active, paused, cancelled, expired` (the stale
    comment previously claimed `suspended` — there is no suspended/inactive
    subscription status).
  - `DatasetStatus` = `active, inactive, archived`.
  - `SubscriptionRequestStatus` = `pending, approved, rejected`.
  - `CompanyStatus` = the 8 canonical values (`provisioning, active, inactive,
    suspended, provisioning_failed, onboarding_failed, deprovisioning,
    decommission_failed`); the field comment previously listed only 4.
  - `SubscriptionTier` read-tolerant set (`free, starter, basic, premium,
    professional, enterprise`); the only canonical write value is `free`.
  All are `= string` type aliases, so existing `string` fields and callers
  compile unchanged.
- **Consumer `ListSubscriptionRequests(ctx, status)`** is now the canonical
  public parity method (matches `SDK-PARITY-METHODS-SPEC.md`). The old
  `ListMySubscriptionRequests` is retained as a thin deprecated alias that
  delegates to it — no break for existing callers.

### Changed
- Stale `CreateSubscriptionRequestInput.Tier` doc comment ("defaults to
  basic") corrected to "free" to match the runtime default and canonical
  write value.

### Removed
- Dead private `Producer.updateDataset` (no callers; the public
  `UpdateDataset` is the canonical surface) and its sole helper
  `getStringField`.
- Dead unused `payload` variable in `Producer.ApproveSubscriptionRequest`
  (the request was already sent via `payloadMap`).

### Security
- `govulncheck` (rebuilt with go1.26.3) reports 2 reachable Go standard-library
  vulnerabilities — `GO-2026-5039` (`net/textproto`) and `GO-2026-5037`
  (`crypto/x509`) — both fixed in go1.26.4. No third-party-dependency
  vulnerabilities. Remediate by building/releasing with go1.26.4+.

### Tests
- `types/enums_test.go` — pins every status/tier constant to its canonical
  value, with a negative control asserting `suspended`/`inactive` are NOT
  canonical subscription statuses.
- `consumer/subscription_requests_parity_test.go` — end-to-end httptest
  coverage of `ListSubscriptionRequests` (no-filter + status filter), the
  `ListMySubscriptionRequests` backward-compat alias, and
  `GetSubscriptionRequest`.
- `producer/update_dataset_parity_test.go` — end-to-end httptest coverage of
  the public `UpdateDataset` (PATCH path, omitempty body, happy + 403 paths).

## 2026-05-05 (v2.2.0)

### Fixed
- **Module path declares `/v2` suffix**: go.mod's module path is now
  `github.com/helix-tools/sdk-go/v2` to match the major version. Pre-existing
  bug since v2.0.0 — every v2.x tag was unfetchable through proxy.golang.org
  with `invalid version: go.mod has non-.../v2 module path`. No public API
  surface changes; this is a module-declaration repair. Consumers using
  `replace` directives or vendored copies must update their imports from
  `github.com/helix-tools/sdk-go/...` to `github.com/helix-tools/sdk-go/v2/...`.
- **`SDKVersion` constant**: synced from 2.1.4 → 2.2.0.

## 2026-05-05 (v2.1.4)

### Fixed
- **`RecordOutcomeRequest.BytesDownloaded` always serialized**: Dropped
  `,omitempty` from the `bytes_downloaded` JSON tag. With omitempty in
  place, a legitimate 0-byte successful download (empty dataset, empty
  NDJSON file, decompressed-to-empty payload) had `bytes_downloaded`
  stripped from the wire, leaving the producer dashboard unable to tell
  "0 bytes downloaded" apart from "no SDK telemetry received yet". This
  mirrors the parallel-session `DurationMs` flake fix (commit `45b765d`)
  for the bytes field. No prod behavior change for non-zero downloads;
  receivers see `bytes_downloaded=0` instead of absent on legal 0-byte
  successes and on early-failure error paths. The schema's "persisted
  only when non-zero" rule continues to apply server-side at the Mongo
  upsert layer (see `dataset_download_event.schema.json`), so zero-value
  records do not pollute the dashboard's persisted history.
- **`SDKVersion` constant synced to module tag**: The constant had
  drifted at "2.1.1" through commits v2.1.2 and v2.1.3 (neither bumped
  it). Re-synced to "2.1.4" to honor the doc comment's "in lockstep with
  the module version tag" guarantee — this matters because the producer
  dashboard's incident-triage view groups events by `sdk_version`, and
  a stuck constant masks single-version regressions across the consumer
  fleet.

### Tests
- Added `TestDownloadOutcome_SuccessZeroBytes` — pins the legal 0-byte
  success case: `bytes_downloaded` must appear in the payload as the
  literal number 0, not be absent. Uses two-value type assertion to
  distinguish `(ok=false)` "missing key" from `(ok=true, got=0)` "key
  set to zero".
- Updated `TestDownloadOutcome_NetworkFetchError` — flipped from
  asserting `bytes_downloaded` is absent on network failure to asserting
  it's present and equal to 0, matching the new wire-format invariant.
  Locks in that early-failure paths cannot regress to omitting the
  field.

## 2026-05-04 (v2.1.3 — backfill for silently-rolled-in commit 45b765d)

### Fixed
- **`DurationMs` always serialized in outcome callback** (commit 45b765d,
  untagged on its own — rolled into v2.1.3's tag). Dropped `,omitempty` from
  the `duration_ms` JSON tag so fast-failing paths (network_fetch errors
  completing in <1ms wall time where `Milliseconds()` returns 0) don't have
  the field stripped from the wire. Eliminated 10-20% flake on
  `TestDownloadOutcome_NetworkFetchError`. Mirrors the rationale of the
  v2.1.4 BytesDownloaded fix.

## 2026-05-04 (v2.1.2 — backfill for the silent ship)

### Changed
- **Default subscription tier**: `"basic"` → `"free"` (commit 5912509).
  Aligns with helix-api's tier enum collapse to `{"free"}` per the
  cross-stack tier-drift cleanup pass.

## 2026-05-04 (v2.1.3)

### Fixed
- **Consumer `makeAPIRequest` 2xx success range**: Widened the success check
  from `resp.StatusCode != 200` to `resp.StatusCode < 200 || resp.StatusCode >= 300`,
  matching the HTTP RFC and the pattern already used by `producer.go`,
  `api/client.go`, and `agent/client.go`. The helix-api returns **201 Created**
  on a successful `POST /v1/subscription-requests`, so before this fix every
  real `Consumer.CreateSubscriptionRequest` call surfaced a fake
  `"API request failed: 201"` error to the caller even though the request
  WAS persisted in Mongo. The previous test suite mocked 200 from `httptest`,
  masking the bug. Live-verified against `https://api-go.helix.tools` with
  Phone.com (consumer) credentials — request created with status `pending`.

### Tests
- Added `TestCreateSubscriptionRequest_Accepts201Created` — pins the 201
  success-path regression.
- Added `TestMakeAPIRequest_StatusCodeContract` — table-driven coverage of
  the full 2xx contract: 200 OK, 201 Created, 204 No Content (via
  `CancelSubscription` / DELETE), 400 Bad Request, 401 Unauthorized,
  500 Internal Server Error, 199 (below 2xx), 300 Multiple Choices.
- Added `TestMakeAPIRequest_MalformedBodyOn201` — pins that a successful
  status code with unparseable body surfaces a decode error (not a fake
  "API request failed"), keeping the status-code contract orthogonal to
  the body-parse contract.

## 2026-02-09

### Added
- **Consumer Subscription Request**: Added `CreateSubscriptionRequest(ctx, input CreateSubscriptionRequestInput) (*types.SubscriptionRequest, error)` method to Consumer. Allows consumers to request access to a producer's datasets. The producer must approve before access is granted.
- **Producer Subscription Management**: Added three new methods to Producer for managing subscription requests:
  - `ListSubscriptionRequests(ctx, status string) ([]types.SubscriptionRequest, error)` - List incoming subscription requests filtered by status (pending/approved/rejected)
  - `ApproveSubscriptionRequest(ctx, requestID string, opts *ApproveSubscriptionRequestOptions) (*types.SubscriptionRequest, error)` - Approve a consumer's subscription request
  - `RejectSubscriptionRequest(ctx, requestID string, reason string) (*types.SubscriptionRequest, error)` - Reject a consumer's subscription request
- **New Types**: Added `CreateSubscriptionRequestInput` and `ApproveSubscriptionRequestOptions` to types package
- **SDK Parity**: These methods achieve full parity with TypeScript SDK subscription request functionality

### Fixed
- **Consumer API Signing**: Fixed `makeAPIRequest` in consumer to properly compute SHA256 hash of request body for POST requests. Previously used empty payload hash for all requests, which would fail AWS SigV4 validation on POST/PUT requests with body.

### Tests
- Added unit tests for subscription request payload marshaling/unmarshaling in `consumer/consumer_test.go`
- Added unit tests for producer subscription request methods in `producer/subscription_request_test.go`

## 2026-01-07

### Added
- **Consumer Queue Management**: Added `ClearQueue()` method to Consumer. Allows consumers to purge all messages from their notification queue. Uses AWS SQS PurgeQueue API with 60-second rate limit handling.

## 2026-01-06

### Fixed
- **Dataset Upsert Response**: Fixed `updateDataset` to construct the return value from the payload instead of parsing the API response. The API returns different field names (`id` vs `_id`, `total_size_bytes` vs `size_bytes`), causing empty dataset fields. Now the SDK returns a properly populated Dataset object after update.

## 2026-01-05

### Added
- **Dataset Upsert Behavior**: `UploadDataset` now automatically updates existing datasets instead of failing with 409 conflict. When uploading a dataset with the same name, the SDK detects the conflict and calls `PATCH /v1/datasets/:id` to update the metadata. This enables seamless repeated uploads for data freshness updates.
- **APIError Type**: Added `APIError` type with `IsConflict()` method for detecting 409 status codes.

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
