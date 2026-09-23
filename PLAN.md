# PLAN — UploadDataset sends sizes, version and metadata again (ClickUp 86e3d27v3)

Lane: fix/upload-sends-sizes · sdk-go. Base `origin/main` @ `586b7b9` (v2.15.0).

## Problem
v2.15.0's `UploadDataset` POSTs the catalog record BEFORE processing the file (the POST-first
refactor that closed a race between the S3 upload and the catalog record existing). That means
the POST body carries zero sizes, an empty `version`, and is missing `record_count`,
`metadata.file_format` and `metadata.encoding` — every field v1.3.11 sent. The API stores
exactly what it receives and overwrites the previous record, so Ringboost's daily re-upload
zeroes/blanks those fields in the catalog on every run.

## Contract
Match v1.3.11's field semantics exactly (read from `producer/producer.go` and
`producer/dataset_payload.go` at tag `v1.3.11`):
- `metadata.original_size_bytes` / `compressed_size_bytes` / `encrypted_size_bytes` — the real
  sizes from the compress+encrypt pass, plus `encryption_enabled`/`compression_enabled`.
- `metadata.file_format` = `"json"`, `metadata.encoding` = `"utf-8"` — only filled if the caller
  didn't already set them via `UploadOptions.Metadata`.
- top-level `version` = `time.Now().UTC().Format("2006-01-02")`, computed unconditionally.
- top-level `record_count` and `metadata.record_count` — `analysis.RecordCount` when analysis
  succeeds, `0` otherwise (v1.3.11 always set these; v2.15.0 omitted them entirely when analysis
  failed and never sent the top-level field at all).
- `DatasetOverrides` always wins over every computed value above, including an explicit
  `version: ""` — matching v1.3.11's unconditional `deepMergeMaps(payload, overrideCopy)`.

## Change
- `producer/producer.go`:
  - `UploadDataset` now calls `processFile` (step 1, compress + encrypt, no upload) BEFORE
    `createDatasetRecord` (step 2, the POST). The POST still runs before `uploadToPresignedURL`
    (step 3) and the GET (step 4), so the catalog-record-before-S3-upload race protection the
    original POST-first refactor introduced is unchanged — a refused POST still means zero PUTs.
  - `createDatasetRecord` gained a fourth parameter, `processed *ProcessedFileData`, and now
    builds `version`, `record_count`, and merges `processed.Sizes`/`file_format`/`encoding` into
    the payload before `DatasetOverrides` is applied.
  - `compressData` and `encryptData` are untouched — only call order and payload construction
    changed, so the on-wire compress+encrypt byte format is unchanged.
- `producer/create_dataset_bucket_test.go`: the three existing tests that drive
  `createDatasetRecord` directly now pass a hand-built `fakeProcessedFileData()` (real KMS
  encryption isn't needed to test the payload-construction logic).
- `producer/upload_sizes_test.go` (new): the v1-field-table test, record_count default-zero
  test, DatasetOverrides-wins tests (including the explicit `version: ""` self-attack), a
  zero-PUTs-on-refused-POST end-to-end test (426/403/500), a process-before-POST end-to-end
  happy-path test with a mocked KMS server, and a compressData gzip round-trip golden test.
- `CHANGELOG.md`, `UPLOAD_FLOW_CHANGES.md`: documented the flow change and the field-level fix.
- `PLAN.md`: this file.

Deliberately NOT touched (outside this lane's blast radius): `consumer/consumer.go`'s
`SDKVersion` and `internal/useragent`'s `fallbackVersion` fallback-version constants — bumping
those to 2.16.0 is the orchestrator's concern at tag time, not this PR's.

## Self-attack
1. **5 GB file — does processing-before-POST now hold the whole file in memory where v2.15.0
   streamed?** No: `processFile`'s `os.ReadFile` full-file read is UNCHANGED by this PR (verified
   via `git diff origin/main -- producer/producer.go`, which shows only `processFile`'s doc
   comment changed, not its body). v2.15.0 already loaded the whole file into memory in
   `processFile` — this PR only changed WHEN that call happens relative to the POST, not what it
   does. Memory footprint for a 5 GB file is identical before and after this PR (and was already
   a pre-existing characteristic of v2.15.0, not introduced here).
2. **Processing succeeds, POST refused — are temp files cleaned up?** N/A: grepped the whole
   `producer` package for `CreateTemp`/`TempFile` — the SDK never creates a temp file in either
   version. `processFile` only reads the caller-supplied `filePath` into memory; the caller (the
   Ringboost exporter) owns any temp file's lifecycle, unaffected by this reorder.
3. **`version=""` explicit vs. omitted — which wins, and does it match v1?** An explicit
   `DatasetOverrides["version"] = ""` wins and blanks the version (v1.3.11's
   `deepMergeMaps` does `base[key] = value` unconditionally for any key present in the override
   map, including `""`). Omitting the key entirely leaves the computed UTC date. Both are pinned
   by `TestCreateDatasetRecord_ExplicitEmptyVersionOverridesComputedDate`.

---

# PLAN — list methods decode the API's paginated response (ClickUp 86e3d27v3, same PR #28)

Lane: fix/upload-sends-sizes · sdk-go. Base: HEAD of this branch (`50dbfbe`), on top of the
sizes fix above. Ships in the same v2.16.0 release.

## Problem
`GET /v1/datasets` and `GET /v1/subscriptions` both return a paginated OBJECT —
`{datasets|subscriptions, total_count, page, limit, total_pages}` (helix-tools/api
`internal/resources/datasets/types.go` `ListDatasetsResponse`,
`internal/resources/subscriptions/types.go` `ListSubscriptionsResponse`) — with a default page
size of 20 when the caller sends no `limit`. `Producer.ListMyDatasets` decoded straight into
`[]types.Dataset`, so every call failed with "cannot unmarshal object into Go value of type
[]types.Dataset" — observed against production on 2026-09-23 with both v1.3.11 and v2.15.0.
`Producer.GetDatasetSubscribers`, `Consumer.ListDatasets` and `Consumer.ListSubscriptions`
already decoded the envelope correctly (a local `{items, count}` struct) but never read
`total_pages`, so they silently returned only the first 20 items with no error and no way for
the caller to know more existed.

## Blast-radius audit (every list method in producer/ and consumer/)
Checked against the API handlers at
`/private/tmp/claude-501/-Users-molonelaveh-Dev-thalesfsp-dme/6edd9898-3e6b-4909-99df-628f38ffbe63/scratchpad/local-stack/wt/api`:

| Method | Endpoint | API paginates? | Status |
|---|---|---|---|
| `Producer.ListMyDatasets` | `GET /v1/datasets` | yes, default limit 20 | **fixed** — was unmarshal error |
| `Producer.GetDatasetSubscribers` | `GET /v1/subscriptions?dataset_id=` | yes, default limit 20 | **fixed** — was silent truncation |
| `Consumer.ListDatasets` | `GET /v1/datasets` | yes, default limit 20 | **fixed** — was silent truncation |
| `Consumer.ListSubscriptions` | `GET /v1/subscriptions` | yes, default limit 20 | **fixed** — was silent truncation |
| `Producer.ListSubscriptionRequests` | `GET /v1/producers/subscription-requests` | no — `ListProducerRequests` returns every matching row, `ListSubscriptionRequestsResponse` has no `total_pages` | already correct |
| `Producer.ListSubscribers` | `GET /v1/producers/subscribers` | no — `ListProducerSubscribers` is unbounded (`FindActiveByProducerID`) | already correct |
| `Producer.ListConsumers` | `GET /v1/self/consumers` | no — `self.Service.ListConsumers` is unbounded | already correct |
| `Consumer.ListSubscriptionRequests` / `ListMySubscriptionRequests` | `GET /v1/subscription-requests` | no — `ListConsumerRequests` returns every matching row | already correct |
| `Consumer.BrowseMarketplace` | `GET /v1/datasets/marketplace` | yes, but the SDK returns the whole `MarketplaceBrowseResponse` (including the `Pagination` block) to the caller by design — caller drives paging via `MarketplaceBrowseParams.Page` | already correct, different contract (exposes pagination, doesn't hide it) |
| `Consumer.PollNotifications` | SQS long-poll | N/A, not an HTTP list endpoint | not applicable |

## Change
- `producer/producer.go`: added an unexported generic `paginateAll[T any]` helper (loops
  `fetchPage(page)` 1..N, appending items; stops at `total_pages` or the first empty page,
  hard-capped at `maxListPages = 1000`). `ListMyDatasets` and `GetDatasetSubscribers` now build
  their request via `paginateAll`, requesting `limit=100` per page. Public signatures unchanged.
- `consumer/consumer.go`: same `paginateAll[T any]` helper (package-private duplicate — the two
  packages don't share an internal package for this, and the helper is ~15 lines, so duplicating
  it was simpler than introducing a new shared package for two call sites). `ListDatasets` and
  `ListSubscriptions` now use it the same way.
- `producer/producer_useragent_test.go`, `producer/producer_sts_test.go`: the four mocks that
  fed `ListMyDatasets` a bare `[]` body (valid under the old buggy decode, invalid under the
  fixed one) now return the real paginated-object shape.
- `producer/producer_list_pagination_test.go` (new), `consumer/consumer_list_pagination_test.go`
  (new): exact-API-shape decode test, three-pages-in-order-exactly-three-requests test,
  bad-`total_pages`-stops-on-empty-page test, and a negative control per package, for both
  `ListMyDatasets`/`GetDatasetSubscribers` and `ListDatasets`/`ListSubscriptions`.
- `CHANGELOG.md`: documented under `## Unreleased` → `### Fixed`.
- `PLAN.md`: this section.

## Self-attack
1. **A server that paginates correctly but the SDK's hard cap (1000 pages) is hit first — does a
   legitimate large list get silently truncated the same way the original bug did?** At
   `limit=100` per page, 1000 pages is 100,000 items — no real producer/consumer catalog is
   remotely close to that. If it ever were, the cap fails safe (returns what it has instead of
   hanging), which is qualitatively different from the original bug (that failed on EVERY call,
   including a 1-item list); accepted as the deliberate tradeoff the brief asked for ("hard cap to
   avoid infinite loops on a bad server").
2. **A page includes a non-empty `datasets`/`subscriptions` array but a `total_pages` LOWER than
   the current page number (server bug) — does the loop terminate?** Yes: `paginateAll`'s stop
   condition is `len(items) == 0 || page >= totalPages`, evaluated every iteration — `page >=
   totalPages` is true the moment `page` reaches whatever (possibly-wrong) `totalPages` value the
   server sent, regardless of whether items are non-empty. Covered by
   `TestListMyDatasets_StopsOnEmptyPageDespiteHighTotalPages` / the consumer equivalent for the
   empty-page case; the low-`total_pages`-with-data case is the same code path, one line away.
3. **Does `page=N&limit=100` collide with an existing query param the caller already set (e.g.
   `producer_id`, `dataset_id`, `role`, `status`)?** No: every call site appends `&page=%d&limit=100`
   after the existing query string is built (`fmt.Sprintf` concatenation, or `path += "&role=..."`
   for `ListSubscriptions`), never replaces it — verified by
   `TestListMyDatasets_FollowsThreePages_InOrder`'s per-request `page` query-param assertion,
   which passes alongside the existing `producer_id`/`dataset_id` params in the other new tests.
