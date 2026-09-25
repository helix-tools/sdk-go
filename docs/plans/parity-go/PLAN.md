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
| consumer | `APIError` (+ `IsUnauthorized`/`IsForbidden`/`IsNotFound`/`IsConflict`/`IsRateLimited`), `Dataset.UnmarshalJSON`, `PollNotificationsOptions.VisibilityTimeout`, `PollNotificationsOptions.ShortPoll` |
| agent | `AgentMe`, `Client.GetMe`, `ServiceUnavailableError`, `IsServiceUnavailable`, `DefaultAPIBaseURL` |

Same signature, changed behaviour: `Producer.ApproveSubscriptionRequest` (returns the
envelope's request; `Status`/`ID` now populated; errors on a body with no request),
`Producer.UpdateDataset` / `DeleteDataset` and `Consumer.GetDataset` (empty id is a
client-side error), `Consumer.ListDatasets` (rows carry the full record;
`consumer.Dataset` embeds `types.Dataset` and keeps `ID`, `Name`,
`Metadata.CompressionEnabled/EncryptionEnabled`), `Consumer.PollNotifications` /
`ClearQueue` (role fallback, `VisibilityTimeout`, `ShortPoll`), `agent.NewClient`
(empty base URL falls back to env then the production endpoint), agent 503
classification, `producer.NewProducer` parameter lookup, and struct tags:
`types.OnboardingInfo` (`credential_url`, `credential_url_expires_at`),
`types.SubscriptionRequest` (three required keys lose `omitempty`).

Deprecated (kept, compile-compatible): `ApproveSubscriptionRequestOptions.DatasetID`
(no longer sent; one-time warning), `agent.Client.Me`, the six admin-only company
request/response types in `types`, `DatasetMarketplace.StripeProductID/StripePriceID`.

Removed from the public surface: package `api` -> `internal/api` (A-12).

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
- consumer/example_test.go
- consumer/list_datasets_full_test.go
- consumer/poll_parity_test.go
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
