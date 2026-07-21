# Changelog

## Unreleased

### Documentation
- docs: unify README to the canonical cross-SDK template -- restructured README.md into the 12 section names/order shared with the TypeScript and Go SDK READMEs (Overview, Installation, Authentication & Credentials incl. an STS subsection, Quickstart -- Producer, Quickstart -- Consumer, Marketplace, Partner Invites, Payouts (Stripe Connect), Versioning & Changelog, Support, License). Split the previous combined Marketplace section's payout-onboarding snippet into a dedicated Payouts (Stripe Connect) section; added an `UpdateDataset` snippet to the Producer quickstart. Moved the `/v2` module-path caveat out of Installation and into Versioning & Changelog. `producer/example_test.go` updated in lockstep (added `Example_payouts`, split from `Example_marketplace`; added the `UpdateDataset` call to `Example_quickstart`) so `go vet`/`go test` continue to compile every README snippet against the real API. No behavior change; corrected the Support section's documentation link to https://dev.helix.tools (was the wrong https://docs.helix.tools domain).

## 2026-07-20 (v2.8.1)

### Documentation
- docs: content-policy scrub -- removed AWS-internals language (S3 bucket / KMS key / SSM resolution, KMS decrypt) from the README's Producer quickstart note and Credentials section, replacing it with capability-level language (authentication and destination configuration, decryption, handled automatically). No behavior change.

## 2026-07-20 (v2.8.0)

### Added
- **Producer self-serve AI-agent surface (Wave-2 Model-2)**. Three
  methods on `*producer.Producer` mirroring `/v1/self/ai-agent`
  (producer JWT v1 / SigV4): `ProvisionAIAgent(ctx)` (`POST`, idempotent,
  no request body) and `DeprovisionAIAgent(ctx)` (`DELETE`, idempotent
  no-op when absent), both returning `*types.AIAgentProvisionResult`
  (`{status, message}`); and `GetAIAgentStatus(ctx)` (`GET`) returning
  `*types.AIAgentStatus` — the masked status carrying ONLY the lifecycle
  `Status`, an optional USD `Cost` (read via `CostUSD()`), `CreatedAt`,
  and a curated `Message` on error (never any infra internals). Status
  values are the `types.AIAgentState*` constants
  (`not_provisioned`/`provisioning`/`active`/`error`/`deprovisioning`).
  Types verified field-for-field against the Go API's
  `internal/resources/aiagent/types.go`. NOTE: server-side this surface
  is dark behind `HELIX_MODEL2_ENABLED`; while off, all three endpoints
  respond 404 (surfaced as `*producer.APIError` with `StatusCode` 404),
  so live e2e is deferred until the flag is flipped.

### Fixed
- **`consumer.SDKVersion` no longer drifts from the real released
  version.** The `sdk_version` field sent in the download-outcome
  telemetry callback (`consumer/consumer.go`'s `DownloadDataset`) now
  resolves the SDK's actual build version via
  `runtime/debug.ReadBuildInfo()` — the real module version a consumer's
  `go.mod`/`go.sum` pins, which the Go toolchain itself stamps into
  every binary — falling back to the `SDKVersion` constant only when
  build info is unavailable or unresolved (a `"(devel)"` build: running
  this SDK's own test suite, or a consumer depending on this module via
  a `replace` directive to a local checkout). Previously `SDKVersion` was
  a hand-bumped constant that had silently drifted to `"2.5.0"` across
  two subsequent releases (v2.6.0, v2.7.0) despite its own doc comment's promise
  to stay "in lockstep with the module version tag" — nothing enforced
  that promise, undermining the producer dashboard's per-version
  incident-triage grouping (see the v2.1.4 and v2.2.0 entries below,
  where this exact class of drift was "fixed" by hand twice before and
  drifted again anyway). The constant remains exported
  (source-compatible with any existing caller reading it directly) and
  is bumped to `"2.8.0"` as the fallback value.

### Tests
- `consumer/sdk_version.go` + `consumer/sdk_version_test.go` (new):
  table-driven coverage of `resolveSDKVersion` against synthetic
  `debug.BuildInfo` values — build info unavailable, this module as the
  `"(devel)"` main module (running this SDK's own tests), this module as
  a real-tagged or pseudo-versioned dependency (`v` prefix stripped),
  this module missing from `Deps` entirely, and a `"(devel)"`-pinned
  dependency (local `replace` directive) — all correctly falling back to
  the constant where expected. `TestSDKVersionConst_MajorMatchesModulePath`
  guards the fallback constant's major version against `go.mod`'s
  `/v2` module-path suffix so the two can never silently disagree.
  `TestDownloadDataset_CallsEffectiveSDKVersion` is a static (source-shape)
  regression guard on the `DownloadDataset` call site itself: dynamic/
  black-box testing structurally cannot catch a regression back to the
  bare `SDKVersion` constant here, because inside this SDK's own test
  binary `effectiveSDKVersion()` always resolves to the very same
  fallback value — negative-control verified (reverted the call site,
  watched this test fail for the right reason while every dynamic test
  in `download_outcome_callback_test.go` stayed green, then restored).
  `TestDownloadOutcome_SuccessCallback`'s `sdk_version` assertion was
  also strengthened from "non-empty" (which the stale `"2.5.0"` constant
  would have satisfied just as well) to an exact match against
  `effectiveSDKVersion()`.

## 2026-07-14 (v2.7.0)

### Added
- **Marketplace — consumer + producer methods.** Consumer:
  `BrowseMarketplace(ctx, params)` (`GET /v1/datasets/marketplace`, a
  public endpoint — optional filters: search/category/sort/page),
  `GetDatasetDetails(ctx, datasetID)` (`GET /v1/datasets/:id/details`, a
  public composite of dataset + reviews + related datasets + the
  caller's subscription info), and `CreateSubscriptionCheckout(ctx,
  input)` (`POST /v1/subscriptions/checkout` with exactly one of
  `input.DatasetID`/`input.RequestID`; returns the hosted Stripe
  Checkout URL only — the SDK never opens or redirects to it). Producer:
  `GetEarnings(ctx, period)` (`GET /v1/self/marketplace/earnings`, a
  tolerant pass-through map — the contract isn't frozen server-side yet)
  and `SetDatasetMarketplace(ctx, datasetID, input)` (`PATCH
  /v1/datasets/:id/marketplace`, PATCH semantics — only non-nil
  `PriceMonthlyCents`/`Listed` fields are sent). New
  `types/marketplace.go` mirrors sdk-schemas PR #18
  (`DatasetMarketplace`, `SubscriptionBilling`, pagination/response
  types); `types.Dataset` gains an optional `Marketplace` field and
  `types.Subscription` gains `Billing` — both pointer-typed and
  absent-tolerant, since every marketplace field is omitted server-side
  while the `marketplace_payments` feature flag is off (never
  defaulted). Browse/details shapes anchored directly to the helix-api
  handlers. Closes ClickUp 86e01nb3t (#8).
- **Producer Stripe Connect payout surface — Python SDK parity (PR #11
  equivalent)**. `producer.ConnectOnboard(ctx)` replaces the earlier
  `GetConnectOnboardingLink` (never in a tagged release, so no
  compatibility concern): `POST /v1/self/connect/onboard`, no request
  body, now returns BOTH `URL` and `AccountID` instead of silently
  dropping the account id. New `producer.CreateConnectLoginLink(ctx)`:
  `POST /v1/self/connect/login-link`, no request body, returns the hosted
  Stripe Express dashboard URL; a 403 (no Connect account yet, or
  onboarding incomplete) surfaces as `*producer.APIError` with
  `StatusCode` 403. Both new types (`types.ConnectOnboardResponse`,
  `types.ConnectLoginLinkResponse`) and the existing
  `producer.GetConnectStatus(ctx)` verified field-for-field against the
  Go API's `internal/resources/connect/types.go` (`OnboardResponse`,
  `StatusResponse`, `LoginLinkResponse`) and cross-checked against the
  Python SDK's `helix_connect/producer.py` (PR #11) (#9).

### Fixed
- **`types.ConnectStatus` was stale/wrong-shaped and drifted from the Go
  API contract.** The struct mirrored the `company.schema.json`
  `connect_*`-prefixed persisted fields instead of the actual
  `GET /v1/self/connect/status` response, so field names (and thus JSON
  decoding) never matched what the server sends:
  `connect_charges_enabled`/`connect_payouts_enabled`/
  `connect_details_submitted`/`connect_onboarded_at` are renamed to the
  unprefixed `charges_enabled`/`payouts_enabled`/`details_submitted`
  (`connect_onboarded_at` dropped — the API never returns it), and three
  fields the API returns but the SDK never exposed are added:
  `onboarding_complete`, `can_price_datasets`, `requirements_due` (a
  **bool** flag — Stripe has outstanding requirements for the account —
  never a list of requirement keys), and `disabled_reason`.
  `ConnectAccountID` changes from `*string` to `string` to match the API's
  own (non-pointer) `StatusResponse.ConnectAccountID`.

### Tests
- `consumer/marketplace_test.go`, `producer/marketplace_test.go`: drive
  the real `BrowseMarketplace`/`GetDatasetDetails`/
  `CreateSubscriptionCheckout`/`GetEarnings`/`SetDatasetMarketplace`
  methods end-to-end against an `httptest` server — real SigV4 signing
  and error mapping; happy + error(404) + edge (absent fields, 0/false
  sent, the exactly-one `DatasetID`/`RequestID` guard) paths.
- `producer/marketplace_test.go` also rewrote the Connect suite to drive
  the real `ConnectOnboard`/`GetConnectStatus`/`CreateConnectLoginLink`
  methods — request shape (path/method/no body), full response decode,
  the 403/500 bad paths, the never-onboarded edge case (fields absent ->
  zero value), and a dedicated negative control
  (`TestGetConnectStatus_RequirementsDueIsBool`) proving
  `requirements_due` really is typed `bool` end-to-end. Every assertion
  was watched to fail for the right reason with the corresponding fix
  reverted (wrong path, wrong field type/tag, missing method), then
  restored.

## 2026-07-11 (v2.6.0)

### Added
- **STS session credentials (opt-in)** — STS-PLAN.md §P3 (B1). New
  `credentials` package (`github.com/helix-tools/sdk-go/v2/credentials`)
  implements an `aws.CredentialsProvider` that mints short-lived AWS STS
  session credentials from the Helix Connect credential broker (`POST
  /v1/credentials/session`, contract frozen in sdk-schemas #17), wrapped in
  `aws.CredentialsCache` for auto-refresh (proactive refresh at ~2/3 of the
  900s TTL, i.e. remaining <=5min, with jitter; single-flight coalescing
  courtesy of `aws.CredentialsCache` itself — no reimplementation). A broker
  blip while a refresh is due rides through on the last-known-good session
  until its TRUE hard expiry ("serve-last-good creds until hard expiry" —
  `Provider` implements `aws.HandleFailRefreshCredentialsCacheStrategy` +
  `aws.AdjustExpiresByCredentialsCacheStrategy` to do this precisely,
  without ever letting the cache believe a truly-expired credential is
  still fresh); once genuinely past hard expiry with no successful refresh,
  it fails closed with a clear error — never fabricates or indefinitely
  reuses an expired credential. `types.Config` gains two
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
  - Merged inert (default static), same publish gate as every other SDK
    release: tagged as v2.6.0 only after the broker went live-dark,
    session-token-aware `UnifiedAuth` was verified, and local-first live
    validation passed across a real 15-minute expiry boundary.
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
  override); a broker outage riding through on the last-known-good
  credential until true hard expiry without re-attempting a mint on every
  call, and failing closed only once genuinely past that hard expiry (two
  tests, each pinning one side of that boundary); `SelectProvider`'s full
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

## 2026-07-06 (v2.5.0)

### Added
- **Producer partner-invite methods**: `InviteConsumer(ctx, input)`
  (`POST /v1/self/invite-consumer` — invites a consumer partner company,
  auto-granting access to the given datasets at invite time; validates
  client-side first: company name 2-200 chars, valid business email
  (max 254 chars), 1-50 unique non-blank dataset ids), `ListConsumers(ctx)`
  (`GET /v1/self/consumers` — includes deactivated relations), and
  `DeactivateConsumer(ctx, consumerID)` (`PATCH
  /v1/self/consumers/:id/deactivate`). All three are gated behind the
  `partner_invite` feature flag; a producer without it enabled gets a
  403 `*producer.APIError`. The server provisions the consumer account
  asynchronously and sends a welcome email —
  `InviteConsumerResponse.EmailSent`/`EmailError` surface whether that
  dispatch succeeded; provisioning is not rolled back on email failure.
  New types `types.InviteConsumerInput`/`InviteConsumerResponse`/
  `ProducerConsumerRelation`/`ListConsumersResponse`/
  `DeactivateConsumerResponse` match the corresponding sdk-schemas
  contracts and the Go API's `self` package source of truth.

### Fixed
- **`UploadDataset` create-payload gaps that broke prod end-to-end.**
  `createDatasetRecord` (the path `UploadDataset` uses) omitted fields
  the deployed API and consumer download need — no mocked unit test
  caught it, only the SDK-only prod E2E suite:
  - `s3_bucket_name` + `access_tier`: required by
    `ValidateCreateDatasetRequest`; previously a 400.
  - `s3_key`: an absent key defaulted to
    `datasets/{producer_id}/{dataset_id}`, but the s3-event-processor
    derives `dataset_name` from the key's first segment — now
    dataset-NAME-keyed (`datasets/{name}/data.ndjson.gz`), matching the
    Python/TS SDKs.
  - `metadata.encryption_enabled`/`compression_enabled`: absent meant
    `Consumer.DownloadDataset` skipped decrypt/decompress, corrupting the
    round trip (sha256 mismatch).
- **`Consumer.DownloadDataset` fell back to the top-level `encryption`
  flag.** The create endpoint promotes `metadata.encryption_enabled` to a
  top-level `encryption` field and drops it from metadata, so a
  metadata-only read saw `isEncrypted=false`, skipped decrypt, and
  gunzipped still-encrypted bytes (`gzip: invalid header`). `isEncrypted`
  now falls back to `types.Dataset.Encryption` when the metadata key is
  absent, mirroring the Python SDK.
- **`InviteConsumer`/`DeactivateConsumer` sent untrimmed values on the
  wire** (codex REFUTE catch): `InviteConsumer` trimmed dataset ids for
  validation but POSTed the raw slice, and `DeactivateConsumer`
  path-escaped the raw arg — whitespace-padded input diverged from the
  canonical server-side value. Both now send/escape the trimmed value.

### Tests
- Regression test drives the real `createDatasetRecord`, asserting every
  field the deployed API requires is present on the wire
  (`producer/upload_post_first_test.go`).
- `consumer/resolve_encrypt_compress_test.go`: pins the encryption-flag
  fallback (top-level `Encryption` used only when
  `metadata.encryption_enabled` is absent).
- Wire-payload tests for `InviteConsumer`/`DeactivateConsumer`
  (`producer/partner_invite_test.go`) pin the trimmed values,
  negative-controlled.

## 2026-06-11 (v2.4.0)

### Added
- **`types.FeatureFlag` / `types.FeatureFlags`** mirroring
  `feature_flag.schema.json` (sdk-schemas v1.0.0): the `FeatureFlag`
  struct plus a `FeatureFlags` map type, and a new `feature_flags` field
  on `Company` (`omitempty` — an absent flag means default-deny, matching
  the schema) (#4).

### Tests
- `types/feature_flag_test.go`: contract tests pin the snake_case JSON
  tags and the `Company` round-trip; negative-control verified (breaking
  the `feature_flags` tag fails the test for the right reason).

## 2026-06-10 (v2.3.0 — SDK parity + enum alignment)

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

## 2026-06-08 (v2.2.1)

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
