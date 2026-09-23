# SDK-GO: POST-First Upload Flow Implementation

**ClickUp Task:** `86e00pauy` (POST-first), `86e3d27v3` (process-before-POST sizes fix)
**Issue:** Race condition where datasets were uploaded to S3 before catalog record was created  
**Status:** ✅ Implemented & Tested

---

## Summary

Refactored the `UploadDataset()` method to follow a **POST-first upload flow** to prevent race conditions. The SDK creates the dataset record in the catalog before any bytes reach S3.

**Revised (v2.16.0):** the POST-first refactor below originally POSTed before processing the
file, so it sent zero sizes, an empty `version`, and dropped `record_count`/`file_format`/
`encoding` — the catalog record lost those fields on every re-upload. The flow now processes
the file (compress + encrypt, no upload) FIRST, so the POST carries the real values, while still
POSTing before any bytes reach S3 (the original race-condition fix is unaffected — see
`CHANGELOG.md`).

---

## Flow Changes

### ❌ Original Flow (Race Condition)
```
1. Read file
2. Compress locally
3. Encrypt locally
4. Upload to S3 (using AWS SDK + credentials)
5. POST to /v1/datasets to register catalog record
   ❌ Problem: If step 5 fails, orphaned data in S3
   ❌ Problem: Race window between S3 upload and catalog registration
```

### ⚠️ v2.15.0 Flow (POST-First, but POST-before-processing)
```
1. POST to /v1/datasets with metadata (sizes unknown yet: 0, version: "")
   → API returns: { id, upload_url (presigned), s3_key }
2. Read file
3. Compress locally
4. Encrypt locally
5. PUT to presigned URL (no AWS credentials needed)
6. GET /v1/datasets/:id to fetch final record
   ✅ Catalog record exists BEFORE upload (no race)
   ❌ Problem: sizes/version/record_count/file_format/encoding are wrong on every POST
```

### ✅ Current Flow (v2.16.0: process-before-POST, still catalog-before-upload)
```
1. Read file, compress locally, encrypt locally (no upload yet)
2. POST to /v1/datasets WITH the real original/compressed/encrypted sizes,
   version (today's UTC date, caller-overridable), record_count, and
   metadata.file_format/.encoding
   → API returns: { id, upload_url (presigned), s3_key }
3. PUT the already-processed bytes to the presigned URL (no AWS credentials needed)
4. GET /v1/datasets/:id to fetch final record
   ✅ Catalog record exists BEFORE upload (no race — step 2 still precedes step 3)
   ✅ Catalog record carries real sizes/version/metadata, matching v1.3.11
   ✅ Presigned URL = scoped, time-limited access
```

---

## Code Changes

### New Types

```go
// CreateDatasetResponse represents the API response when creating a dataset record.
type CreateDatasetResponse struct {
    ID        string `json:"id"`
    UploadURL string `json:"upload_url"`  // Presigned S3 URL
    S3Key     string `json:"s3_key"`
}

// ProcessedFileData contains the processed (encrypted/compressed) file data and metadata.
type ProcessedFileData struct {
    Data         []byte
    OriginalSize int64
    Sizes        map[string]any
    Analysis     *AnalysisResult
}
```

### New Methods

#### 1. `processFile()`
**Step 1:** Processes file (compress + encrypt), no upload
- Validates encryption/compression requirements
- Reads file
- Compresses with gzip
- Encrypts with KMS envelope encryption
- Returns `ProcessedFileData` (now also carries the real sizes used by step 2)

#### 2. `createDatasetRecord(ctx, filePath, opts, processed *ProcessedFileData)`
**Step 2:** Creates catalog record and retrieves presigned URL
- Analyzes data schema (independent read of the same file)
- POSTs to `/v1/datasets` with name, metadata, category, sizes (from
  `processed.Sizes`), `version`, `record_count`, etc.
- Returns `CreateDatasetResponse` with presigned URL

#### 3. `uploadToPresignedURL()`
**Step 3:** Uploads to presigned URL
- PUTs the already-processed data to presigned URL using `http.Client`
- No AWS credentials needed (presigned URL handles auth)
- Returns error if upload fails

### Updated Method

#### `UploadDataset()` - Orchestrates the current flow
```go
func (p *Producer) UploadDataset(ctx context.Context, filePath string, opts UploadOptions) (*types.Dataset, error) {
    // Validate defaults

    // Step 1: Process file (encrypt/compress) — no upload yet, so the real
    // sizes are known before the POST.
    processedData, err := p.processFile(ctx, filePath, opts)

    // Step 2: Create dataset record WITH those sizes, get presigned URL.
    createResp, err := p.createDatasetRecord(ctx, filePath, opts, processedData)

    // Step 3: Upload to presigned URL
    err = p.uploadToPresignedURL(ctx, createResp.UploadURL, processedData.Data)

    // Step 4: Fetch and return dataset
    dataset := &types.Dataset{}
    p.makeAPIRequest(ctx, "GET", fmt.Sprintf("/v1/datasets/%s", createResp.ID), nil, dataset)

    return dataset, nil
}
```

---

## Breaking Changes

### ⚠️ API Changes Required

The implementation **assumes** the API returns the following response from `POST /v1/datasets`:

```json
{
  "id": "dataset-123",
  "upload_url": "https://s3.amazonaws.com/bucket/key?X-Amz-...",
  "s3_key": "datasets/my-dataset/data.ndjson.gz"
}
```

**If the API doesn't yet support this:**
1. The API needs to be updated to:
   - Generate presigned URL on POST
   - Return `upload_url` in the response
2. The API should NOT expect `s3_key` or `size_bytes` in the POST payload
3. The API should allow updating these fields after upload completes

### ⚠️ SDK Behavior Changes

1. **Error Messages:** 
   - Old: `"file uploaded to S3 but catalog registration failed"`
   - New: `"dataset record created but upload failed"`
   
2. **S3 Upload Method:**
   - Old: Direct S3 upload using AWS SDK (`s3Client.PutObject`)
   - New: HTTP PUT to presigned URL (no S3 SDK needed)

3. **No More 409 Handling in UploadDataset:**
   - Old: Caught 409 conflicts and updated existing datasets
   - New: POST creates a new record each time (API should handle idempotency if needed)

---

## Tests Added

### `upload_post_first_test.go`

✅ **TestProcessFileCompression** - Validates compression logic  
✅ **TestProcessFileEmptyFile** - Ensures empty files are rejected  
✅ **TestProcessFileMissingFile** - Validates missing file error handling  
✅ **TestUploadToPresignedURL/successful_upload** - Tests successful PUT to presigned URL  
✅ **TestUploadToPresignedURL/upload_failure_-_403_Forbidden** - Tests failed upload (403)  
✅ **TestUploadToPresignedURL/empty_data** - Edge case for empty data upload  
✅ **TestUploadDatasetValidation/encryption_required** - Validates encryption requirement  
✅ **TestUploadDatasetValidation/compression_required** - Validates compression requirement  
✅ **TestUploadDatasetValidation/KMS_key_required_for_encryption** - Validates KMS key requirement  
✅ **TestCreateDatasetResponseStructure** - Validates API response unmarshaling  

### Test Results
```bash
$ cd ~/Dev/thalesfsp/dme/sdk/go && go test ./...
ok  	github.com/helix-tools/sdk-go/v2/api	(cached)
ok  	github.com/helix-tools/sdk-go/v2/consumer	(cached)
ok  	github.com/helix-tools/sdk-go/v2/producer	0.288s
```

**All tests pass** ✅

---

## Migration Notes

### For SDK Users

**No changes required** if you're using the public `UploadDataset()` API.

The method signature remains the same:
```go
dataset, err := producer.UploadDataset(ctx, filePath, opts)
```

### For API Developers

**Action Required:**

1. **Update POST /v1/datasets endpoint:**
   - Generate presigned S3 URL
   - Return `CreateDatasetResponse` structure:
     ```json
     {
       "id": "dataset-id",
       "upload_url": "https://s3.amazonaws.com/...",
       "s3_key": "datasets/name/data.ndjson.gz"
     }
     ```

2. **Remove s3_key requirement from POST payload:**
   - Dataset record should be created WITHOUT the file existing yet
   - S3 key can be generated server-side based on dataset name

3. **Add presigned URL generation:**
   - Use AWS SDK to generate presigned PUT URL
   - Set appropriate expiration (e.g., 15 minutes)
   - Scope to specific S3 key

---

## Benefits

✅ **Eliminates race condition** - Catalog record exists before upload  
✅ **Better error handling** - Know dataset ID even if upload fails  
✅ **Idempotency** - API can enforce duplicate checks at POST time  
✅ **Security** - Presigned URL = temporary, scoped access (no credentials in SDK)  
✅ **Simpler SDK** - No need to manage S3 client configuration for uploads  
✅ **Atomic operations** - Record creation and upload are separate, clear steps  

---

## Rollback Plan

If the API changes aren't ready, the old flow can be restored by:
1. Reverting `producer/producer.go` to the previous version
2. Removing `upload_post_first_test.go`

However, **this is not recommended** as it reintroduces the race condition.

---

## Files Modified

- ✏️ `producer/producer.go` - Refactored `UploadDataset()`, added 3 new methods
- ➕ `producer/upload_post_first_test.go` - Added comprehensive test coverage
- 🔧 Removed unused import: `"errors"` (no longer needed)

---

## Next Steps

1. ✅ **SDK Implementation** - Complete
2. ⏳ **API Implementation** - Requires backend changes (see "Migration Notes")
3. ⏳ **Integration Testing** - Test with updated API once deployed
4. ⏳ **Documentation Update** - Update API docs to reflect new response format

---

**Author:** Nova (SDK-GO Agent)  
**Date:** 2026-02-23  
**ClickUp:** `86e00pauy`
