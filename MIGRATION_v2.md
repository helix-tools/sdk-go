# sdk-go v2.0.0 — Migration Guide

**Release date**: 2026-04-21
**Breaking change**: none — additive only.

## TL;DR

```bash
go get github.com/helix-tools/sdk-go/v2@v2.22.1
```

No code changes required.

## Why the major bump

Coordinated with the other Helix SDKs:

| SDK | v2.0.0 meaning |
|---|---|
| `@helix-tools/sdk-typescript` | version-only (already matched target contract) |
| `helix-connect` (Python) | **breaking** — operator-only operations removed |
| `sdk-go` (this) | version-only confirmation of full parity |

From 2.0.0 forward, the shared Helix SDK schemas are the single source of truth and CI blocks drift across any SDK.

## Verified full parity

This SDK was audited against schemas and the other SDKs on 2026-04-21. All expected methods and types are present. Notably:

- `Client.SendA2A(ctx, recipientAgentID, correlationID, payload, opts...)` — for sending A2A messages to a peer agent (was already implemented; the earlier parity table flagged it as missing — that was a false positive).
- `Client.SendMCPContext(ctx, envelope)` — MCP context broadcast.
- `Client.Me()`, `Client.ListPeers()`, `Client.MyAuditTrail()` — agent introspection.
- Full producer surface: `UploadDataset`, `UpdateDataset`, `DeleteDataset`, `ListMyDatasets`, `ApproveSubscriptionRequest`, `RejectSubscriptionRequest`, `GetDatasetSubscribers`, `ListSubscribers`, `RevokeSubscription`, `ListSubscriptionRequests`.
- Full consumer surface: `ListDatasets`, `GetDataset`, `GetDownloadURL`, `DownloadDataset`, `ListSubscriptions`, `GetSubscription`, `CancelSubscription`, `CreateSubscriptionRequest`, `ListMySubscriptionRequests`, `GetSubscriptionRequest`, `PollNotifications`, `DeleteNotification`, `ClearQueue`.

## What's not here

- **No operator-only operations.** This SDK covers the producer, consumer and agent APIs; operator-only operations are not part of it, and none are on the roadmap.

## New CI guardrails (informational)

From v2.0.0, CI runs two blocking checks that help keep this SDK in sync:

1. The public packages (`agent`, `consumer`, `producer`) may call only customer-facing routes.
2. Every public struct must cover its schema's fields.

No action needed from you — just be aware the CI is stricter now.
