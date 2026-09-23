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

---

# PLAN — list-pagination round 2: page-mismatch guard, honest fixtures, non-nil empty
# results (ClickUp 86e3d27v3, same PR #28)

Lane: fix/upload-sends-sizes · sdk-go. Base: HEAD of this branch (`7b7127a`), same PR as the
list-pagination fix above. An independent review of `7b7127a` passed item 1 (envelope decode
correctness, unchanged by this round) and failed three:

1. Neither `paginateAll` checked the response's own `page` field against the page it requested,
   so a server that ignores `?page` and keeps re-serving page 1 (while claiming `total_pages=3`)
   would make the loop silently append the same items three times instead of erroring.
2. The consumer dataset test fixture used `_id`, but the real API sends `id`
   (`internal/resources/datasets/types.go:90`'s `DatasetResponse.ID`, `json:"id"`) — and this
   package's own `Dataset.ID` field was *also* tagged `json:"_id"`, so the wrong fixture happened
   to agree with the wrong SDK tag and the test's name-only assertions never caught either. The
   two subscription pagination tests (producer's `GetDatasetSubscribers`, consumer's
   `ListSubscriptions`) advanced their mock's page purely by counting requests, never reading the
   `?page` the client actually sent — a client that kept re-requesting page 1 would still pass.
   Both packages' negative-control tests for criterion 1 also stood apart from the SDK: they
   `json.Unmarshal`ed a hand-rolled struct in the test body instead of calling the public method.
3. `paginateAll` started `all` as `var all []T` (a nil slice). A successful empty list (0 items
   on page 1) never appended anything, so the four list methods now returned `nil` instead of the
   API's `[]` — a caller JSON-re-encoding the result got `null`, a regression from the pre-`paginateAll`
   behavior for `ListDatasets`/`ListSubscriptions`/`GetDatasetSubscribers` (which decoded the API's
   own array field directly).

## Change
- `producer/producer.go`, `consumer/consumer.go`: `paginateAll`'s `fetchPage` callback now also
  returns the response's own page number (`*int`, nil when the field is absent); `paginateAll`
  errors immediately (`"list pagination: requested page %d but server returned page %d"`) on a
  mismatch, and `all` now starts as `[]T{}` instead of `var all []T` so an empty result stays `[]`
  on the wire, not `null`. All four list-method closures (`ListMyDatasets`,
  `GetDatasetSubscribers`, `ListDatasets`, `ListSubscriptions`) gained a `Page *int json:"page"`
  field on their anonymous decode structs and pass it through.
- `consumer/consumer.go`: `Dataset.ID`'s tag changed from `json:"_id"` to `json:"id"`, matching
  what `GET /v1/datasets` actually sends (this type is decoded ONLY by `ListDatasets` — grepped
  for other constructors/readers, found none — so the change is contained to that one path;
  unrelated to `types.Dataset`'s `ID`/`IDAlias` dual-tag, which `GetDataset`/`DownloadDataset` use
  and which review item 1 didn't flag).
- `producer/producer_list_pagination_test.go`, `consumer/consumer_list_pagination_test.go`:
  - Both dataset fixtures/tests already used (producer) or now use (consumer) `id`; the consumer
    tests also assert `.ID` on returned items, not just `.Name`.
  - `TestGetDatasetSubscribers_FollowsAllPages` / `TestListSubscriptions_FollowsAllPages`: the
    mock now keys its response (including the echoed `page` field) on the actual `?page` query
    parameter instead of a request counter, and the test asserts the exact requested-page
    sequence (`[1,2,3]`) via a 3-page fixture — a client that re-requested page 1 would fail this
    assertion instead of being silently satisfied.
  - The two bare-array-shape tests were rewritten to call the public method (`ListMyDatasets` /
    `ListDatasets`) against a bare-array response and assert an error, instead of a standalone
    `json.Unmarshal`. These prove the envelope-object decode target rejects a bare array; they are
    NOT pagination-regression controls (round 3 below renamed them and fixed this description
    after review found them mislabeled as such).
  - New tests per list method (8 total): `*_ServerIgnoresPageParam_ReturnsError` (mock always
    reports `page=1, total_pages=3`; asserts an error, zero returned items, and exactly 2
    requests) and `*_EmptyResult_ReturnsEmptyNotNilSlice` (asserts `json.Marshal` of an empty
    result is `[]`, not `null`).
- `CHANGELOG.md`: documented under `## Unreleased` → `### Fixed`, appended to the existing list-
  pagination entry.
- `PLAN.md`: this section.

## Self-attack
1. **Does the page-mismatch check false-positive on a legitimate response that omits `page`
   entirely (an older API build)?** No: `respPage` is `*int`; when the JSON response has no
   `"page"` key, `json.Decode` leaves it `nil`, and `paginateAll` only compares when
   `respPage != nil`. No existing or new test feeds a response without `page` and expects
   success-with-mismatch, so this path is asserted only by absence of failure in every other test
   (all of which include `page`); the pointer's nil-skips-the-check behavior itself follows
   directly from the Go zero value of an unset `*int` field, not from anything more subtle.
2. **Does the new page-mismatch error ever fire on a CORRECT server because of an off-by-one in
   the comparison?** No: `paginateAll`'s loop variable `page` starts at 1 (matching a 1-based API)
   and every existing positive-path test (`FollowsThreePages`, `StopsOnEmptyPage`,
   `FollowsAllPages`) already asserts `total items` and/or `exact request sequence`, which would
   fail immediately if the comparison misfired on a page that genuinely matched — confirmed by
   running the full pre-existing positive-path suite alongside the new mismatch tests, all green.
3. **Changing `consumer.Dataset.ID`'s tag from `_id` to `id` — does anything outside
   `ListDatasets` construct or read a `consumer.Dataset` expecting the old tag?** Grepped
   `consumer.Dataset\b` and `Dataset{` across the whole module (not just this package): zero
   hits outside its own declaration and `ListDatasets`'s decode target. `types.Dataset` (used by
   `GetDataset`/`DownloadDataset`/producer's `ListMyDatasets`) is a separate type, untouched.

# PLAN — list-pagination round 3: page-omission guard, test relabeling (ClickUp 86e3d27v3,
# same PR #28)

Lane: fix/upload-sends-sizes · sdk-go. Base: HEAD of this branch (`62b41b9`), same PR as both
list-pagination rounds above. A second independent review passed item 4 (unrelated to this fix,
verified untouched) and failed two:

2. Round 2's page-mismatch guard (`respPage != nil && *respPage != page`) only fires when the
   response's `page` field is PRESENT and disagrees. A response that omits `page` entirely
   (`respPage == nil`) skips the comparison and falls straight through — so a server that both
   ignores `?page` AND never sends `page` back bypassed the guard completely, and a 3-page-claimed
   response returning the same single item on every request produced `[A,A,A]` with no error.
   Confirmed against the real API's datasets and subscriptions handlers
   (`internal/resources/datasets/types.go`, `internal/resources/subscriptions/types.go`, both
   `Page int json:"page"`, no `omitempty`) that a genuine response from either endpoint always
   echoes the page it served — an omitted `page` field is never a legitimate single-request
   response once more than one page is in play.
3. `TestListDatasets_RejectsBareArrayShape` / `TestListMyDatasets_RejectsBareArrayShape` were
   documented and (in producer's case) named as regression negative controls, but neither one
   depends on the pagination-following fix (`paginateAll`'s loop, the page-mismatch guard) at all
   — they depend only on the decode target being an envelope object instead of a bare-array slice,
   which is a separate, already-settled part of round 1. Reverting round 2/3's pagination logic
   while keeping the envelope decode target leaves both tests green, so citing them as pagination
   regression coverage overstates what they prove.

## Change
- `producer/producer.go`, `consumer/consumer.go`: `paginateAll`'s per-page check is now a
  `switch`: a present-and-mismatched `page` errors exactly as round 2 left it; a response that
  OMITS `page` while the page being followed is not the only one (`totalPages > 1`) now also
  errors, naming the requested page and total instead of silently trusting the response. A
  single-page result (`totalPages <= 1`) still accepts an absent `page` field unchanged — there is
  nothing ambiguous to detect when there was only ever one page to serve.
- `producer/producer_list_pagination_test.go`, `consumer/consumer_list_pagination_test.go`: added
  `*_ServerOmitsPageWithMultiplePages_ReturnsError` per list method (4 total) — the reviewer's
  exact fixture (`{"datasets":[{"id":"A"}],"total_pages":3}`, `page` absent) — asserting an error,
  zero returned items, and exactly 1 request (the very first response is already disqualifying, so
  there's no second request to make); and `*_SinglePageOmitsPage_Accepted` per package (2 total)
  confirming the unchanged accept-path. Negative control run for all four error tests: with the
  `switch`'s second case reverted, all four fail, reproducing exactly the `[A,A,A]` (or `[sub-1,
  sub-1, sub-1]`) duplication the review described — pasted in the lane report.
- Renamed `TestListDatasets_RejectsBareArrayShape` → `TestListDatasets_EnvelopeDecodeRejectsBareArrayShape`
  and `TestListMyDatasets_RejectsBareArrayShape` → `TestListMyDatasets_EnvelopeDecodeRejectsBareArrayShape`,
  rewrote their doc comments to state plainly what they prove (bare-array rejection by the
  envelope decode target) and to explicitly disclaim pagination-regression coverage, pointing at
  the tests that actually cover that (`*_FollowsThreePages_InOrder`, `*_StopsOnEmptyPage...`,
  `*_ServerIgnoresPageParam_ReturnsError`, `*_ServerOmitsPageWithMultiplePages_ReturnsError`).
  Fixed round 2's PLAN.md bullet that called them "the two negative-control tests" in a context
  that implied pagination coverage.
- `CHANGELOG.md`: extended the existing list-pagination `### Fixed` entry with the page-omission
  case.
- `PLAN.md`: this section.

## Self-attack
1. **`total_pages=1` (or `0`) with no `page` — still accepted, exactly one request?** Yes: the new
   `case respPage == nil && totalPages > 1` only fires when `totalPages > 1`, so a single-page (or
   `totalPages<=0`, which the existing `page >= totalPages` stop condition already treats as
   terminal after one request regardless) response with `page` omitted falls through both `switch`
   cases and is accepted, exactly the pre-round-3 behavior. Covered by
   `*_SinglePageOmitsPage_Accepted`, which asserts both zero error and exactly 1 request.
2. **A first page WITH `page` and a later page WITHOUT it — error, not duplicates?** Yes: each
   iteration re-evaluates `respPage`/`totalPages` from that iteration's own response, so a page 1
   response carrying a correct `page` passes through and is appended, then a page 2 response
   omitting `page` (with `totalPages > 1`, still true) hits the new case and returns `nil, err`
   immediately — discarding everything accumulated so far, matching the existing mismatch case's
   all-or-nothing behavior. No new test pins this exact two-request sequence (the four new tests
   all fail on request 1, since the reviewer's fixture omits `page` from the very first response);
   answered by code inspection — `paginateAll`'s loop has no special-case for "later" pages, the
   same per-iteration check that fires on request 1 in the new tests fires identically on request 2
   here, so nothing more needs to be pinned than "the check runs every iteration," which the
   existing round-2 `*_ServerIgnoresPageParam_ReturnsError` tests already exercise for the
   mismatched-not-omitted variant of the same "wrong on request 2" shape.

# PLAN — list-pagination round 4 (last): lock pagination shape after page 1 (ClickUp 86e3d27v3,
# same PR #28)

Lane: fix/upload-sends-sizes · sdk-go. Base: HEAD of this branch (`dbcc9d8`), same PR as all three
list-pagination rounds above. A third independent review passed item 3 (unrelated to this fix,
verified untouched) and identified the exact gap round 3's self-attack item 2 flagged but left
uncovered by a real test:

2. Both round-2/3 checks (`respPage != nil && *respPage != page`, `respPage == nil &&
   totalPages > 1`) evaluate ONLY the current response against itself — neither compares it
   against what page 1 already reported. A server that answers page 1 honestly
   (`{"datasets":[{"id":"A"}],"page":1,"total_pages":3}`) and then, on page 2, repeats item A
   while OMITTING `page` and reporting `total_pages:1` (or `0`) passes both existing checks:
   `respPage == nil && totalPages > 1` is false because `totalPages` on THIS response is 1, not
   3. The response looks exactly like a legitimate single-page reply in isolation, so
   `paginateAll` appended it and returned `[A,A]` with no error — reproduced locally before any
   fix (`TestPREMISE_...`, deleted after confirming; see lane report for the pre-fix run).

## Change
- `producer/producer.go`, `consumer/consumer.go`: `paginateAll` now locks the pagination shape
  from the first response — its `totalPages` value and whether `page` was present — into two
  loop-scoped variables (`lockedTotalPages`, `lockedPagePresent`) set on `page == 1`. Every
  response after the first is checked against that lock before the existing per-response checks
  run: a `totalPages` that differs from page 1's errors immediately (covers both the reviewer's
  omit-and-drop scenario and a plain `total_pages` change with `page` still present and correct,
  e.g. `3` then `2`); a `page`-field presence that flips (present → absent or vice versa) errors
  immediately. Both new checks return before any item from the offending response is appended, so
  the all-or-nothing behavior of every prior guard is preserved — no duplicates ever reach the
  caller on an error path.
- `producer/producer_list_pagination_test.go`, `consumer/consumer_list_pagination_test.go`: added
  `*_ServerOmitsPageAndDropsTotalPages_ReturnsError` per list method (4 total) — the reviewer's
  exact two-request sequence — and `*_TotalPagesChangesBetweenPages_ReturnsError` per method (2
  total, producer + consumer datasets methods) for the `page`-present-but-`total_pages`-changed
  variant, asserting an error, zero returned items, and exactly 2 requests (page 1 succeeds, page
  2 detects the shape change). The three existing "well-behaved 3-page server" and "single-page
  omits `page`, still accepted" tests per package were re-run unmodified and still pass — the lock
  only ever compares AFTER page 1 sets it, so a consistent server is never affected.
- `CHANGELOG.md`: extended the existing list-pagination `### Fixed` entry with the shape-lock case.
- `PLAN.md`: this section.

## Self-attack
1. **Does locking on page 1 false-positive on a legitimate multi-page server whose LAST page is
   naturally short (fewer items than `limit`) but still reports the same `total_pages` and `page`
   shape?** No: the lock only tracks `totalPages` and `page`-presence, never item count per page —
   a shorter final page changes `len(items)`, not either locked field, so it passes through
   unchanged. Covered by the existing `*_FollowsThreePages_InOrder` tests, whose last page (`{"e"}`,
   `{"sub-4"}`) is shorter than the earlier ones and still succeeds.
2. **Does the lock ever compare page 1 against itself and false-positive on request 1?** No: the
   lock is SET, not checked, when `page == 1` (an `if`/`else if` chain, not a loop-invariant
   comparison) — there is nothing to compare page 1 against yet, so the first response can never
   trip either new check regardless of its shape. This is also why the round-3
   `*_ServerOmitsPageWithMultiplePages_ReturnsError` tests (whose disqualifying response IS page 1)
   still fail on their original, pre-round-4 check (`respPage == nil && totalPages > 1`) and never
   reach the new lock logic — re-run unmodified and confirmed still green.
3. **Could a malicious server pass the lock by matching page 1's `total_pages` and `page`-presence
   exactly, while still re-serving page 1's items forever?** No: that's exactly the round-2 guard's
   job (`respPage != nil && *respPage != page`), which still runs immediately after the new lock
   checks, unchanged — a `page`-present response that keeps claiming `page:1` fails that check on
   request 2 regardless of what the lock does. The two guards are complementary: the lock catches a
   server that changes its SHAPE; the round-2/3 guards catch a server that keeps the shape but
   lies about WHICH page it served.

## Negative control
`lockedTotalPages`/`lockedPagePresent` and the two `else if` branches removed, `paginateAll`
reverted to round 3's exact form: both new tests
(`TestListMyDatasets_ServerOmitsPageAndDropsTotalPages_ReturnsError`,
`TestListMyDatasets_TotalPagesChangesBetweenPages_ReturnsError`) failed with "unexpectedly
succeeded", `datasets` populated with the duplicated item(s) — reproducing exactly the
pre-fix `[A,A]` duplication the review described. Restored; `diff` against the pre-revert file
confirmed byte-identical restoration; full suite re-run green. Output pasted in the lane report.
