# PLAN — strict schema type check for dataset_info counters (ClickUp 86e3c1uy2)

Lane: fix/dataset-info-strict-schema-type · sdk-go. Test-only change. Base `origin/main` @
`54c8027`.

## Problem
`TestSubscription_DatasetInfoMatchesSchemaFile` (types/subscription_test.go) checked
`dataset_info.record_count`/`size_bytes` schema types via `schemaTypeIncludes`, which
returns true for a union type like `["integer","number"]`. The sibling usage-counter
contract test in the same file already compares `type` with strict string equality
(`prop["type"].(string) == wantType`, added on the F2 branch, commit `013e7df`). The
`dataset_info` check should use the same strict pattern so a schema drifted to a union
type fails the test instead of silently passing.

## Contract
`subscription.schema.json` (sdk-schemas v1.9.0, commit `68ef733`) declares
`dataset_info.properties.record_count.type` and `.size_bytes.type` as the plain string
`"integer"` — confirmed by reading the file before making this change.

## Change
- Replace the `schemaTypeIncludes(...)` call with the strict `gotType, ok :=
  di.Properties[k]["type"].(string); !ok || gotType != "integer"` pattern.
- Delete the now-unused `schemaTypeIncludes` helper (no other callers in the repo).

## Files
- types/subscription_test.go
- PLAN.md
