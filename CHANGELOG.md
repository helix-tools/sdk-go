# Changelog

## 2025-12-18

### Added
- **Empty File Validation**: Added validation in `UploadDataset()` to reject empty files with a clear error message: `"file is empty: {path} (no data to upload)"`. This prevents uploading zero-byte files which would fail downstream processing.
- **Unit Tests**: Added `producer/upload_validation_test.go` with tests for empty file validation.

## 2025-12-14

### Fixed
- **Notification Parsing Bug**: Fixed notification parsing error when receiving raw SQS messages. The consumer now handles both SNS-wrapped messages (default) and raw notification payloads (when `raw_message_delivery = true` or direct SQS). Previously, when a raw message was received, the code attempted to parse an empty `snsMessage.Message` string, causing the notification parsing to fail.

### Added
- **Unit Tests**: Added notification parsing tests to `consumer/consumer_test.go` covering both SNS-wrapped and raw message formats.
