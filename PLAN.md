# PLAN — additive usage-counter fields on `types.Subscription` (ClickUp 86e3bcy33)

Lane: F2 · sdk-go. Adds the five optional subscription usage counters, following the
`dataset_info` precedent (commit `38202db`) exactly: pointer fields for absent-vs-zero
distinction, a reflect-based Go-side contract test, and a schema-file contract test against
the shared `subscription.schema.json` (pinned at commit `6f88b95` in the sdk-schemas repo).
Base `origin/main` @ `38202db`.

## Contract
Five TOP-LEVEL optional `Subscription` fields — siblings of `dataset_info`, `billing`,
`created_at` — never nested inside a new object: `access_count`, `accesses_this_month`,
`monthly_access_cap`, `remaining_accesses` (integer, minimum -1; -1 = unlimited),
`last_accessed_at` (RFC 3339 string, omitted when never downloaded). Never add or rename a
key without the schema changing first.

## Files
- types/subscription.go
- types/subscription_test.go
- .github/workflows/go.yml
- CHANGELOG.md
- PLAN.md
