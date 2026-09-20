# PLAN — additive `DatasetInfo` on `types.Subscription` (ClickUp 86e3bcy37)

Lane: EXEC UI · sdk-go. Executes the sdk-go slice of PLAN-UI (F7 freshness "N/A") and its
Codex §10 amendments. Base `origin/main` @ `25686f4`.

## Contract
`Subscription.dataset_info` is ONE optional object with exactly
`{name, last_updated, updated_at, record_count, size_bytes}`, all optional. Never add or
rename a key. Lane F2 adds SIBLING fields next to `dataset_info`, never keys inside it.

## Files
- types/subscription.go
- types/subscription_test.go
- CHANGELOG.md
- PLAN.md
