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
