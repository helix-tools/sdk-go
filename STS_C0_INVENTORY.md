# C0 — Credential/client/signer bind-site inventory (STS-PLAN.md §P3, codex gate finding #6)

Mechanical grep-inventory of every AWS client construction, signer construction,
and credential bind site in `sdk/go` (this repo), taken **before** writing any
provider code, per STS-PLAN.md P3: *"Before any provider code, each SDK gets a
mechanical inventory ... committed as the checklist that C2/C3 tests are derived
from — NOT this plan's counts."*

Commands run (repo root, `grep -rn ... --include="*.go" .`):

```
NewStaticCredentialsProvider | WithCredentialsProvider | LoadDefaultConfig |
\.NewFromConfig\( | NewSigner\(\)|SignHTTP\( | Credentials.Retrieve |
awsConfig\s*aws\.Config | types\.Config\{ | GetCallerIdentity |
SessionToken|X-Amz-Security-Token | APIKey|CredentialMode
```

## Sites requiring a change

| # | File:line | What | Change |
|---|---|---|---|
| 1 | `types/common.go:8-14` (`Config` struct) | No session-token / provider-mode field at all | **ADD** `APIKey string`, `CredentialMode CredentialMode` fields + `CredentialMode` type + `CredentialModeStatic`/`CredentialModeSTS` constants |
| 2 | `consumer/consumer.go:229-236` (`NewConsumer`) | `config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, ""))` — always static | **REWIRE** to `stscreds.SelectProvider(cfg.APIEndpoint, cfg)`; existing AWS SDK `credentials` import removed (its only use site), replaced by `stscreds "github.com/helix-tools/sdk-go/v2/credentials"` |
| 3 | `producer/producer.go:120-127` (`NewProducer`) | Same static-only pattern | **REWIRE** — same treatment as #2 |
| 4 | `api/config.go` (`NewAWSConfig`, line 181-191) | Test-utility that builds a static `aws.Config` | **UNTOUCHED** (plan: "existing `NewAWSConfig`/`LoadCredentialsFromSSM` untouched") — **ADD** sibling `NewAWSConfigSTS` variant for test/e2e tooling |

## Sites that inherit the change automatically (no edit needed)

These all consume `awsConfig aws.Config` / `awsConfig.Credentials`, which is
whatever provider `NewConsumer`/`NewProducer` selected — switching the provider
at construction time propagates to every one of these without touching the
line itself:

| File:line | What |
|---|---|
| `consumer/consumer.go:243` (`sts.NewFromConfig`) + `:244-249` (`GetCallerIdentity`) | Credential validation at construction — now also validates a freshly-minted STS session when in sts mode |
| `consumer/consumer.go:258-260` (`kms.NewFromConfig`, `sqs.NewFromConfig`, `ssm.NewFromConfig`) | AWS service clients (P3/P4 planes) |
| `consumer/consumer.go:773` (`c.awsConfig.Credentials.Retrieve`) + `:787-789` (`v4.NewSigner().SignHTTP`) | Per-request API SigV4 signing (P1 plane, `makeAPIRequest`) — **zero signing changes needed**: `SignHTTP` already emits `X-Amz-Security-Token` automatically whenever the retrieved `aws.Credentials.SessionToken` is non-empty |
| `producer/producer.go:133` (`sts.NewFromConfig`) + `:134-138` (`GetCallerIdentity`) | Credential validation at construction |
| `producer/producer.go:142` (`ssm.NewFromConfig`) | SSM bucket/KMS-key-id lookup at construction |
| `producer/producer.go:170-171` (`kms.NewFromConfig`, `s3.NewFromConfig`) | AWS service clients (P3 plane) |
| `producer/producer.go:643` (`p.awsConfig.Credentials.Retrieve`) + `:663-664` (`v4.NewSigner().SignHTTP`) | Per-request API SigV4 signing (P1 plane, `makeAPIRequest`) — same zero-signing-change guarantee |
| `api/client.go:118` (`Credentials.Retrieve`) + `:134-135` (`SignHTTP`) | Integration-test HTTP client signing — inherits whatever `aws.Config` `NewClient`/`NewAWSConfig[STS]` built |

## Sites explicitly justified static (out of scope for this PR)

| File:line | Why justified |
|---|---|
| `producer/update_dataset_parity_test.go:23` | Test fixture — constructs `aws.Config{Credentials: credentials.NewStaticCredentialsProvider(...)}` directly and builds a `Producer{}` struct literal, bypassing `NewProducer` entirely. Unaffected by the config-surface change; not a production bind site. |
| `consumer/download_outcome_callback_test.go:192` (`newTestConsumer`) | Same pattern — bypasses `NewConsumer`, builds `Consumer{}` directly. |
| `agent/*.go` | Zero AWS credential references (grep-confirmed). Agent modules use JWT bearer auth exclusively — untouched per plan ("Agent modules use JWT bearer, NOT AWS creds"). |
| `producer/analyze.go`, `producer/dataset_payload.go`, `producer/partner_invite.go`, `api/fixtures.go`, `api/cleanup.go` | Zero AWS client/credential references (grep-confirmed) — pure data/HTTP-via-existing-client logic, no independent bind site. |
| `test/e2e_main.go:44,53` (`types.Config{...}`) | Constructs config for the manual e2e smoke entrypoint; sets static `AWSAccessKeyID`/`AWSSecretAccessKey`, so the new mode-inference logic infers `static` — behavior unchanged. Not modified. |

## Confirmed zero-hit surfaces (ground truth, pre-change)

- `SessionToken` / `X-Amz-Security-Token`: **zero** references anywhere in `*.go` — confirms the "zero-diff SigV4" premise was true on the **Go SDK signing side** specifically (unlike the API's `sigv4.go`/`credentials.go`/`unified_auth.go`, which codex gate finding #1 found DID need session-token support added — that work is `helix-tools/api` PR #129, a different repo/surface, out of scope here).
- `APIKey` / `CredentialMode` / `credential_mode`: **zero** references — confirms Go's config surface genuinely has no provider-mode field yet (codex gate finding #6), matching the task's framing.

## New package (not a rewire — net-new file)

- `credentials/broker.go` (+ `credentials/broker_test.go`): implements `aws.CredentialsProvider` for `POST /v1/credentials/session`, plus `NewCredentialsCache` (tuned `aws.CredentialsCacheOptions`) and `SelectProvider` (the mode-inference/wiring helper consumed by `NewConsumer`/`NewProducer`).

## Ground-truth correction applied during C0 (verified against real code, not just the plan doc)

`helix-tools/api` PR #129 ("STS broker DARK · flag-gated · no customer impact",
open, not yet merged) is the actual P2/B0 broker build referenced by this plan.
Reading its `internal/resources/credentials/{routes,session_controller,session_types}.go`
directly (not just STS-PLAN.md's prose) settles two things this Go PR depends on,
superseding an older assumption in `C-sdk.md` (C.1 bullet 5, "Bootstrap auth:
steady-state mint authenticates with the Helix API key") that STS-PLAN.md §9
decision #1 had already flagged as corrected:

1. **Mint-request auth is SigV4-only.** The route chain is
   `STSBrokerKillSwitchGate → RequireMachineAuth(AuthMethodSigV4) → CustomerBasedRateLimit → RequireCompanyFeature`.
   `RequireMachineAuth(AuthMethodSigV4)` rejects anything that isn't a SigV4
   request (including JWT and admin-impersonation JWT) with 403. There is no
   `X-API-Key` / bearer-token code path anywhere in the broker. Consequently
   this PR's `credentials.Provider` bootstraps every mint call by SigV4-signing
   it with the caller's **existing static `AWSAccessKeyID`/`AWSSecretAccessKey`**
   (service `"execute-api"`, the exact pattern already used by
   `consumer.go`/`producer.go`/`api/client.go`) — not a bearer API key. `APIKey`
   is still added to `types.Config` per this plan's file list (additive,
   forward-compatible field for the P5 Helix-API-key bootstrap), but is
   currently **inert**: `credentials.SelectProvider` returns a clear
   "not yet supported by this SDK version" error if `APIKey` is set without
   static keys, rather than silently sending a header the real broker cannot
   authenticate (a guessed header name for a mechanism that does not exist
   server-side yet would be dead/misleading code, not forward compatibility).
2. **Request body is fully optional.** `session_controller.go` only calls
   `c.Bind(&req)` when `Content-Length != 0`; `SessionRequest{Scopes,
   TTLSeconds}` are both `omitempty` and default to "all owned planes, standard
   TTL" when omitted. This SDK version has no scoping config surface, so the
   mint request is sent with **no body** (`Content-Length: 0`), matching the
   existing zero-body GET/DELETE pattern already used elsewhere in this SDK
   (`types.EmptyPayloadHash`).
3. **Response shape matches exactly** what was already frozen in
   `sdk-schemas` PR #17 / `credential_session.schema.json` (verified directly:
   `SessionResponse{AccessKeyID, SecretAccessKey, SessionToken, Expiration,
   TTLSeconds, Region}`, `ttl_seconds` pinned to the integer `900`, nested error
   envelope `{message, error:{code, message, request_id}}` with the closed
   5-value `error.code` taxonomy) — no drift, no changes needed to the
   already-planned response parsing.
