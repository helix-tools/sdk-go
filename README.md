# Helix Connect Go SDK

Official Go SDK for the Helix Connect data marketplace platform.

## Overview

The Helix Connect SDK gives data producers and consumers programmatic access
to the Helix Connect data marketplace. Producers upload and price datasets,
manage partner access, and track earnings; consumers browse, subscribe to,
and download the datasets they have access to. Every dataset is encrypted in
transit and at rest, with encryption, compression, and decryption handled
automatically by the SDK.

## Installation

```bash
go get github.com/helix-tools/sdk-go/v2
```

Requires Go 1.25 or later (see `go.mod`). The `/v2` suffix above is
mandatory — see [Versioning & Changelog](#versioning--changelog).

## Authentication & Credentials

Every SDK call is authenticated with three values: `CustomerID`,
`AWSAccessKeyID`, and `AWSSecretAccessKey` on `types.Config`. You get these
from the Helix Connect portal (https://portal.helix.tools) — sign in and
open the Credentials page, where they're revealed only once you're
authenticated.

The SDK does not read these from the environment for you; your application
reads them (e.g. from env vars or a secrets manager) and passes them into
`types.Config`, as shown throughout this README. The one variable the SDK
*does* resolve automatically is `HELIX_API_ENDPOINT`, used as a fallback for
`APIEndpoint` when it's omitted; it otherwise defaults to
`https://api-go.helix.tools`.

```go
p, err := producer.NewProducer(types.Config{
	APIEndpoint:        "https://api-go.helix.tools",
	AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
	AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
	Region:             "us-east-1",
})
```

`NewProducer`/`NewConsumer` validate the AWS credentials against STS at
construction time, so both require real, reachable AWS credentials to
construct.

### STS session credentials (opt-in)

By default, the SDK signs every request with the long-lived AWS key you
provide (`CredentialMode: types.CredentialModeStatic`) — unchanged since the
first release. Opt into short-lived, auto-refreshing AWS STS session
credentials with one config field: `CredentialMode: types.CredentialModeSTS`.
That key is then used only as a bootstrap credential — the SDK mints a
15-minute session credential from the Helix credential broker and refreshes
it automatically before it expires. Everything else (uploads, downloads,
notification polling) works identically in both modes.

```go
cfg := types.Config{
	APIEndpoint:        "https://api-go.helix.tools",
	AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"), // still required: bootstraps the broker mint request
	AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
	Region:             "us-east-1",
	CredentialMode:     types.CredentialModeSTS, // opt-in; default is "static"
}
consumer, err := consumer.NewConsumer(cfg)
```

See `CHANGELOG.md` for details and `credentials/broker.go`'s package doc for
the lower-level `Provider`/`NewCredentialsCache` API.

## Quickstart — Producer

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/helix-tools/sdk-go/v2/producer"
	"github.com/helix-tools/sdk-go/v2/types"
)

func main() {
	ctx := context.Background()

	p, err := producer.NewProducer(types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
		Region:             "us-east-1",
	})
	if err != nil {
		log.Fatalf("producer: %v", err)
	}

	// Encryption and compression are required — NewUploadOptions sets
	// sane defaults for both.
	opts := producer.NewUploadOptions("customer-records")

	dataset, err := p.UploadDataset(ctx, "./data/customers.json", opts)
	if err != nil {
		log.Fatalf("upload: %v", err)
	}
	log.Printf("uploaded dataset %s (%s)", dataset.ID, dataset.Name)

	datasets, err := p.ListMyDatasets(ctx)
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	log.Printf("producer has %d dataset(s)", len(datasets))

	// Update metadata without re-uploading the underlying data
	description := "Updated description with more details"
	if _, err := p.UpdateDataset(ctx, dataset.ID, types.DatasetUpdateInput{
		Description: &description,
	}); err != nil {
		log.Fatalf("update: %v", err)
	}
}
```

## Quickstart — Consumer

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/helix-tools/sdk-go/v2/consumer"
	"github.com/helix-tools/sdk-go/v2/types"
)

func main() {
	ctx := context.Background()

	c, err := consumer.NewConsumer(types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
		Region:             "us-east-1",
	})
	if err != nil {
		log.Fatalf("consumer: %v", err)
	}

	subs, err := c.ListSubscriptions(ctx, nil)
	if err != nil {
		log.Fatalf("list subscriptions: %v", err)
	}
	log.Printf("%d active subscription(s)", len(subs))

	// Long-polls the per-consumer notification queue; messages are
	// auto-acknowledged (deleted) by default after retrieval.
	notifications, err := c.PollNotifications(ctx, consumer.PollNotificationsOptions{})
	if err != nil {
		log.Fatalf("poll: %v", err)
	}

	for _, n := range notifications {
		// DownloadDataset decrypts and decompresses before writing, so the
		// output is always plaintext — never name it .gz.
		outputPath := "./" + n.DatasetName + ".ndjson"
		if err := c.DownloadDataset(ctx, n.DatasetID, outputPath); err != nil {
			log.Printf("download %s: %v", n.DatasetID, err)
			continue
		}
		log.Printf("downloaded %s -> %s", n.DatasetID, outputPath)
	}
}
```

Both snippets above are compiled (not merely eyeballed) against the
current API by `producer/example_test.go` and `consumer/example_test.go`
— `go vet ./...` / `go test ./...` fail if any constructor, method name, or
field drifts from what's actually exported.

## Marketplace

Consumers can browse the public dataset marketplace and subscribe to a
paid listing via hosted Stripe Checkout; producers can price their datasets
and track earnings. A `404`/`403` from any of these can mean the
`marketplace_payments` feature flag isn't enabled yet for your account (it
can also mean an unrelated cause — e.g. an unknown dataset id, or an
authorization issue) — contact support if it doesn't resolve. Listed
prices are the producer's own; platform terms are at
https://helix.tools/#pricing.

### Browsing and subscribing (consumer)

```go
results, err := c.BrowseMarketplace(ctx, &types.MarketplaceBrowseParams{
	Category: "phone-numbers",
	Page:     1,
})
// ...
details, err := c.GetDatasetDetails(ctx, datasetID) // dataset + reviews + related
// ...
checkoutURL, err := c.CreateSubscriptionCheckout(ctx, types.SubscriptionCheckoutInput{
	DatasetID: datasetID, // exactly one of DatasetID or RequestID
})
// Open checkoutURL in the consumer's browser — the SDK never opens or
// redirects to it itself.
```

### Pricing and earnings (producer)

```go
price, listed := 4900, true // $49.00/mo, in USD cents; 0 = free
_, err = p.SetDatasetMarketplace(ctx, datasetID, types.SetDatasetMarketplaceInput{
	PriceMonthlyCents: &price,
	Listed:            &listed,
})

earnings, err := p.GetEarnings(ctx, "") // optionally GetEarnings(ctx, "2026-07")
```

## Partner Invites

Producers can invite a consumer partner company directly, auto-granting
access to specific datasets — no separate subscription-request/approval
round trip. Gated behind the `partner_invite` feature flag; a `403` can
mean it isn't enabled yet for your account (it can also mean an
unrelated authorization issue) — contact support if it doesn't resolve.

```go
invite, err := p.InviteConsumer(ctx, types.InviteConsumerInput{
	CompanyName:   "Acme Analytics",
	BusinessEmail: "data@acme.example",
	Datasets:      []string{datasetID}, // 1-50 unique dataset ids
	Tier:          "free",              // currently the only supported tier
})
// Check invite.EmailSent — provisioning succeeds even if the welcome
// email fails to send.

consumers, err := p.ListConsumers(ctx) // includes deactivated relations

_, err = p.DeactivateConsumer(ctx, invite.ConsumerID) // revokes access
```

## Payouts (Stripe Connect)

Producers accept payouts through a hosted Stripe Connect Express flow. The
SDK never opens or redirects to any of the returned URLs itself — send the
producer to them.

```go
// One-time: connect a Stripe Express account to receive payouts
onboard, err := p.ConnectOnboard(ctx) // returns a Stripe Account Link URL + AccountID
// Open onboard.URL yourself to submit KYC/bank details.

// Check payout account status any time
status, err := p.GetConnectStatus(ctx)
if status.CanPriceDatasets {
	// datasets can now be priced above $0
}

// Once onboarding is complete, get a one-time link to the Stripe Express
// dashboard (403 until onboarding is complete)
dashboard, err := p.CreateConnectLoginLink(ctx)
```

## Versioning & Changelog

This SDK follows [semantic versioning](https://semver.org/), tagged
`v2.x.y`. See [CHANGELOG.md](./CHANGELOG.md) for the full release history.

**The `/v2` module-path suffix is mandatory.** `github.com/helix-tools/sdk-go`
(no suffix) resolves to the ancient, unmaintained v1.5.0 — always import
`github.com/helix-tools/sdk-go/v2/...` and run `go get
github.com/helix-tools/sdk-go/v2` to add or upgrade this SDK.

## Support

- Documentation: https://dev.helix.tools (sign in at https://portal.helix.tools and open **SDK Docs**)
- Issues: https://github.com/helix-tools/sdk-go/issues
- Email: support@helix.tools

## License

See [LICENSE](./LICENSE) for details.
