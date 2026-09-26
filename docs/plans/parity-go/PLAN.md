# Parity fix wave — sdk-go (branch fix/parity-2026-09-go)

Findings: B-01, A-01, A-15/B-06, B-08, A-06, A-08, A-09, A-12, A-16, A-21, A-05
(D3), A-17 (disposition), B-02, B-09, D-08 (D7), B-05, B-17. Decisions D1, D3, D7
are binding; the codex plan critique (A-01 must stay source-compatible) is applied.

## Approach

Each finding: failing test first (red for the right reason), fix, negative
control (revert the fix, watch the test fail), bypass test where an evasion is
conceivable. Blind mocks that planted a shape the API never sends are fixed to
the real wire shape (id-only datasets, the approve `{request, subscription}`
envelope).

`internal/compat` is the guard for "Ringboost's producer must not break": a
generated consumer program that uses every exported identifier of the previous
public API (functions and methods with exact signatures, constants, variables,
every exported struct field with its exact type) must still build against this
checkout. Against `db12167` the only removal is package `api` (A-12); everything
else is additive.

## Exported symbols changed

Added (nothing previously exported was removed or re-typed, except package `api`):

| Package | Symbol |
| --- | --- |
| types | `Dataset.UnmarshalJSON`, `Dataset.IsPublic`, `Dataset.PricePerAccess`, `Subscription.ProducerInfo`, `ProducerInfo`, `ApproveRequestResponse.UnmarshalJSON`, `CompanyStatusPendingApproval`, `CompanyStatusRejected`, `CompanyStatusPendingOffboard`, `CompanyStatusOffboarded` |
| producer | `Producer.ApproveSubscriptionRequestWithSubscription` |
| consumer | `APIError` (+ `IsUnauthorized`/`IsForbidden`/`IsNotFound`/`IsConflict`/`IsRateLimited`), `Dataset.Record`, `Dataset.UnmarshalJSON`, `PollNotificationsOptions.VisibilityTimeout`, `PollNotificationsOptions.ShortPoll` |
| agent | `AgentMe`, `Client.GetMe`, `ServiceUnavailableError`, `IsServiceUnavailable`, `DefaultAPIBaseURL` |

Same signature, changed behaviour: `Producer.ApproveSubscriptionRequest` (returns the
envelope's request; `Status`/`ID` now populated; errors on a body with no request),
`Producer.UpdateDataset` / `DeleteDataset` and `Consumer.GetDataset` (empty id is a
client-side error), `Consumer.ListDatasets` (rows carry the full record in `Record`;
`consumer.Dataset` keeps `ID`, `Name`, `Metadata.CompressionEnabled/EncryptionEnabled`
as declared fields — keyed literals and comparability still work), `Consumer.PollNotifications` /
`ClearQueue` (role fallback, `VisibilityTimeout`, `ShortPoll`), `agent.NewClient`
(empty base URL falls back to env then the production endpoint), agent 503
classification, `producer.NewProducer` parameter lookup, and struct tags:
`types.OnboardingInfo` (`credential_url`, `credential_url_expires_at`),
`types.SubscriptionRequest` (three required keys lose `omitempty`).

Deprecated (kept, compile-compatible): `ApproveSubscriptionRequestOptions.DatasetID`
(no longer sent; one-time warning), `agent.Client.Me`, the six admin-only company
request/response types in `types`, `DatasetMarketplace.StripeProductID/StripePriceID`.

Removed from the public surface: package `api` -> `internal/api` (A-12).

go.mod: `github.com/aws/smithy-go v1.24.2` moves from indirect to direct (same
version, already in the graph): `ShortPoll` needs one serialize middleware to put an
explicit `WaitTimeSeconds: 0` on the wire, because the generated serializer omits
zero and SQS then applies the queue default (a 20 s long poll).

## Dispositions

- A-17: documented (README "Differences from the other SDKs"): Go `DownloadDataset`
  always decrypts and decompresses, has no progress callback; direct subscription and
  `update_dataset_data` are Python-only; STS refresh is automatic.
- A-16: 403/404/409/400 mapping left as `*agent.APIError` with the status (Go's
  existing typed shape); 503 marker classification and the default base URL adopted.
- B-17: Go gains `is_public` / `price_per_access`; `parent_dataset_id`, TS/Python items
  are other lanes.
- D-08: Go follows the wire for the expiry key too (`credential_url_expires_at`, what the
  API model emits). The schemas still name it `credentials_portal_expires_at`; the
  schema lane should rename it alongside `credential_url`.

## Wave 2 — rules R1-R4 (Thales, 2026-09-25)

| Rule | Go SDK state | Change |
| --- | --- | --- |
| R1 uploads always encrypted + compressed | `UploadOptions.Encrypt/Compress` kept; false already errored, but `Metadata` / `DatasetOverrides` could still set the record's flags to false or drop them, and `NewProducer` printed "encryption will be disabled" | `validateUploadOptions` (single point, in `processFile`) refuses false flags, a missing key, and disabled flags in `Metadata` / `DatasetOverrides` (top-level or nested `"metadata"`) before the file is read or any network call; the record's `metadata.encryption_enabled` / `compression_enabled` are pinned to true after the override merge; the key-lookup warning says uploads will fail |
| R2 downloads always decrypt + decompress | decided from the record's flags: a record saying "false" returned raw bytes; header length read from the object was allocated unchecked | `decryptAndDecompress` is unconditional for both download paths; the object's header is validated before anything is allocated (`errNotEncrypted`); undecrypted or non-compressed content is refused (`errNotCompressed`); no file written on refusal |
| R3 snake_case + RFC 3339 string dates | already true on every wire tag; agent package uses `time.Time` (RFC 3339 on the wire) | `TestWireStructs` (AST walk of every wire struct: every exported field tagged, snake_case names, string dates, no `time.Time` on stored records); `agent` exempt from the date rule with the reason recorded in the test |
| R4 ids are `<prefix>-<uuid>`, server-assigned | the SDK generates no entity id and no create/invite request type carries one | same walk: request types (found by name pattern, not a hand list) carry no `id` / `_id`; upload POST body asserted to carry no id |

Ringboost's exporter options (`Encrypt: true, Compress: true, CompressionLevel: 6`
plus its `Metadata` and `DatasetOverrides`) are pinned by
`TestUploadDataset_RingboostCallPathIsUnchanged`, which passes identically
against the pre-change producer.

`ApproveSubscriptionRequestWithSubscription` is the envelope-returning approve
method name (TS `approveSubscriptionRequestWithSubscription`, Python
`approve_subscription_request_with_subscription`); nothing to rename.

New files: `consumer/envelope_helpers_test.go`,
`consumer/download_always_decrypt_test.go`,
`producer/upload_encrypted_compressed_test.go`, `types/wire_names_test.go`.
Reworked blind tests: the consumer download fixtures served plaintext with the
flags "false" (now a real compressed + encrypted object behind a fake key service); the
zero-byte "legal" download (now an empty dataset, compressed and encrypted); the producer
"compress-only" size test (now pins size_bytes = bytes PUT).

## Files

- CHANGELOG.md
- Makefile
- README.md
- agent/client.go
- agent/client_test.go
- agent/errors.go
- agent/parity_test.go
- agent/types.go
- consumer/consumer.go
- consumer/consumer_sts_test.go
- consumer/download_always_decrypt_test.go
- consumer/download_outcome_callback_test.go
- consumer/envelope_helpers_test.go
- consumer/security_test.go
- consumer/example_test.go
- consumer/list_datasets_full_test.go
- consumer/poll_parity_test.go
- go.mod
- docs/plans/parity-go/PLAN.md
- internal/api/cleanup.go (moved from api/)
- internal/api/client.go (moved from api/)
- internal/api/companies_test.go (moved from api/)
- internal/api/company_wire.go
- internal/api/config.go (moved from api/)
- internal/api/datasets_test.go (moved from api/)
- internal/api/fixtures.go (moved from api/)
- internal/api/subscription_requests_test.go (moved from api/)
- internal/api/subscriptions_test.go (moved from api/)
- internal/compat/compat_test.go
- internal/compat/gen/main.go
- internal/compat/testdata/surface_v2.go.txt
- memory.md
- producer/approve_envelope_test.go
- producer/dataset_id_wire_test.go
- producer/example_test.go
- producer/marketplace_test.go
- producer/producer.go
- producer/ssm_lookup_test.go
- producer/subscription_request_pricing_test.go
- producer/update_dataset_parity_test.go
- producer/upload_encrypted_compressed_test.go
- producer/upload_sizes_test.go
- types/common.go
- types/company.go
- types/company_test.go
- types/dataset_decode_test.go
- types/enums_test.go
- types/marketplace.go
- types/subscription.go
- types/subscription_request.go
- types/subscription_request_test.go
- types/subscription_test.go
- types/wire_names_test.go
