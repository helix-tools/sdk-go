package consumer_test

// These Example functions back the README's consumer quickstart and
// Marketplace snippets. They are compiled by `go test` (proving the
// snippets match the real, current API) but deliberately have no
// "Output:" comment, so the Go testing package compiles them without
// ever calling them. NewConsumer validates AWS credentials against STS
// at construction time, which none of these examples can do in CI.

import (
	"context"
	"fmt"
	"os"

	"github.com/helix-tools/sdk-go/v2/consumer"
	"github.com/helix-tools/sdk-go/v2/types"
)

// Example_quickstart checks active subscriptions, long-polls for
// new-data notifications, and downloads what arrives.
func Example_quickstart() {
	ctx := context.Background()

	c, err := consumer.NewConsumer(types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
		Region:             "us-east-1",
	})
	if err != nil {
		panic(err)
	}

	subs, err := c.ListSubscriptions(ctx, nil)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d active subscription(s)\n", len(subs))

	// Long-polls the dedicated per-consumer SQS queue; messages are
	// auto-acknowledged (deleted) by default after retrieval.
	notifications, err := c.PollNotifications(ctx, consumer.PollNotificationsOptions{})
	if err != nil {
		panic(err)
	}

	for _, n := range notifications {
		// DownloadDataset decrypts and decompresses before writing, so the
		// output is always plaintext — never name it .gz.
		outputPath := "./" + n.DatasetName + ".ndjson"
		if err := c.DownloadDataset(ctx, n.DatasetID, outputPath); err != nil {
			fmt.Printf("download %s: %v\n", n.DatasetID, err)
			continue
		}
		fmt.Printf("downloaded %s -> %s\n", n.DatasetID, outputPath)
	}
}

// Example_marketplace browses the public marketplace and subscribes to
// a paid dataset via hosted Stripe Checkout. All three methods shipped
// in v2.7.0; a 404 on CreateSubscriptionCheckout means the
// marketplace_payments feature flag isn't enabled yet.
func Example_marketplace() {
	ctx := context.Background()

	c, err := consumer.NewConsumer(types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
		Region:             "us-east-1",
	})
	if err != nil {
		panic(err)
	}

	results, err := c.BrowseMarketplace(ctx, &types.MarketplaceBrowseParams{
		Category: "finance",
		Page:     1,
	})
	if err != nil {
		panic(err)
	}

	for _, ds := range results.Datasets {
		details, err := c.GetDatasetDetails(ctx, ds.ID)
		if err != nil {
			panic(err)
		}
		fmt.Println(details.Dataset.Name)
	}

	// Exactly one of DatasetID/RequestID. The SDK never opens or
	// redirects to the returned URL itself.
	checkoutURL, err := c.CreateSubscriptionCheckout(ctx, types.SubscriptionCheckoutInput{
		DatasetID: "dataset-id",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("open this URL to complete checkout:", checkoutURL)
}
