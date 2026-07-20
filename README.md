# Helix Connect Platform Go SDK

Official Go SDK for the Helix Connect data marketplace platform.

## Overview

The Helix Connect Go SDK enables data producers and consumers to securely exchange and consume datasets through the Helix Connect platform.

## Installation

```bash
go get github.com/helix-tools/sdk-go/v2
```

**The `/v2` suffix is mandatory.** `github.com/helix-tools/sdk-go` (no
suffix) resolves to the ancient, unmaintained v1.5.0 — always import
`github.com/helix-tools/sdk-go/v2/...`.

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
	opts := producer.NewUploadOptions("my-dataset")

	dataset, err := p.UploadDataset(ctx, "./data.ndjson", opts)
	if err != nil {
		log.Fatalf("upload: %v", err)
	}
	log.Printf("uploaded dataset %s (%s)", dataset.ID, dataset.Name)

	datasets, err := p.ListMyDatasets(ctx)
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	log.Printf("producer has %d dataset(s)", len(datasets))
}
```

`NewProducer` authenticates and configures the upload destination
automatically using `CustomerID`, then validates the AWS credentials against
STS — so it requires real, reachable AWS credentials to construct. See
[Credentials](#credentials) below for the two supported credential
modes.

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

	// Long-polls the dedicated per-consumer SQS queue; messages are
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
— `go vet ./...` / `go test ./...` fail if either constructor, method
name, or field drifts from what's actually exported.

## Marketplace (since v2.7.0)

Consumers can browse the public dataset marketplace and subscribe to a
paid listing via hosted Stripe Checkout; producers can accept payouts
and price their datasets. A `404`/`403` from any of these can mean the
`marketplace_payments` feature flag isn't enabled yet for your account
(it can also mean an unrelated cause — e.g. an unknown dataset id, or
an authorization issue) — contact support if it doesn't resolve.

```go
// Consumer: browse, inspect, subscribe.
results, err := c.BrowseMarketplace(ctx, &types.MarketplaceBrowseParams{
	Category: "finance",
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

```go
// Producer: onboard for payouts, price a dataset, check earnings.
onboard, err := p.ConnectOnboard(ctx) // returns a Stripe Account Link URL + AccountID
// Open onboard.URL yourself to submit KYC/bank details.

status, err := p.GetConnectStatus(ctx)
if status.CanPriceDatasets {
	price, listed := 999, true // $9.99/mo, in USD cents
	_, err = p.SetDatasetMarketplace(ctx, datasetID, types.SetDatasetMarketplaceInput{
		PriceMonthlyCents: &price,
		Listed:            &listed,
	})
}

earnings, err := p.GetEarnings(ctx, "") // tolerant pass-through map
```

## Partner Invites (since v2.5.0)

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
})
// Check invite.EmailSent — provisioning succeeds even if the welcome
// email fails to send.

consumers, err := p.ListConsumers(ctx) // includes deactivated relations

_, err = p.DeactivateConsumer(ctx, invite.ConsumerID) // revokes access
```

## Credentials

By default, `Consumer`/`Producer` sign requests with the long-lived static
AWS key you provide (`types.Config.AWSAccessKeyID`/`AWSSecretAccessKey`) —
unchanged behavior, no configuration needed.

Opt in to auto-refreshing, short-lived AWS STS session credentials (15-minute
TTL, minted from the Helix Connect credential broker) by setting
`CredentialMode`:

```go
cfg := types.Config{
	APIEndpoint:        "https://api-go.helix.tools",
	AWSAccessKeyID:     "AKIA...",     // still required: bootstraps the broker mint request
	AWSSecretAccessKey: "...",
	CustomerID:         "customer-123",
	Region:             "us-east-1",
	CredentialMode:     types.CredentialModeSTS, // opt-in; default is "static"
}
consumer, err := consumer.NewConsumer(cfg)
```

Everything else — downloads, notification polling, decryption — handled
automatically, identically in both modes; the SDK refreshes and re-signs
credentials as needed. See
`CHANGELOG.md` for details and `credentials/broker.go`'s package doc for the
lower-level `Provider`/`NewCredentialsCache` API.

## Support

- Documentation: https://docs.helix.tools
- Issues: https://github.com/helix-tools/sdk-go/issues
- Email: support@helix.tools

## License

See LICENSE file for details.
