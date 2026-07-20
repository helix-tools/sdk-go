package producer_test

// These Example functions back the README's producer quickstart,
// Marketplace, and Partner Invite snippets. They are compiled by `go
// test` (proving the snippets match the real, current API — constructor
// names, method signatures, field names) but deliberately have no
// "Output:" comment, so the Go testing package compiles them without
// ever calling them. NewProducer validates AWS credentials against STS
// at construction time, which none of these examples can do in CI.

import (
	"context"
	"fmt"
	"os"

	"github.com/helix-tools/sdk-go/v2/producer"
	"github.com/helix-tools/sdk-go/v2/types"
)

// Example_quickstart uploads a dataset and lists what this producer has
// published so far.
func Example_quickstart() {
	ctx := context.Background()

	p, err := producer.NewProducer(types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
		Region:             "us-east-1",
	})
	if err != nil {
		panic(err)
	}

	// Encryption and compression are required — NewUploadOptions sets
	// sane defaults for both.
	opts := producer.NewUploadOptions("my-dataset")

	dataset, err := p.UploadDataset(ctx, "./data.ndjson", opts)
	if err != nil {
		panic(err)
	}
	fmt.Printf("uploaded dataset %s (%s)\n", dataset.ID, dataset.Name)

	datasets, err := p.ListMyDatasets(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("producer has %d dataset(s)\n", len(datasets))
}

// Example_marketplace onboards for payouts, prices a dataset, and reads
// the earnings rollup. ConnectOnboard/SetDatasetMarketplace/GetEarnings
// shipped in v2.7.0; a 404 means the marketplace_payments feature flag
// isn't enabled yet for this producer.
func Example_marketplace() {
	ctx := context.Background()

	p, err := producer.NewProducer(types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
		Region:             "us-east-1",
	})
	if err != nil {
		panic(err)
	}

	// Start (or resume) Stripe Connect Express payout onboarding. The
	// producer must open onboard.URL themselves to submit KYC/bank
	// details — the SDK never opens or redirects to it.
	onboard, err := p.ConnectOnboard(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println("open this URL to complete payout onboarding:", onboard.URL)

	status, err := p.GetConnectStatus(ctx)
	if err != nil {
		panic(err)
	}

	if status.CanPriceDatasets {
		price := 999 // $9.99/mo, in USD cents
		listed := true
		if _, err := p.SetDatasetMarketplace(ctx, "dataset-id", types.SetDatasetMarketplaceInput{
			PriceMonthlyCents: &price,
			Listed:            &listed,
		}); err != nil {
			panic(err)
		}
	}

	earnings, err := p.GetEarnings(ctx, "")
	if err != nil {
		panic(err)
	}
	fmt.Println(earnings)
}

// Example_partnerInvite invites a consumer partner and grants access to
// a dataset. All three methods shipped in v2.5.0 and are gated behind
// the partner_invite feature flag — a 403 means it isn't enabled yet
// for this producer.
func Example_partnerInvite() {
	ctx := context.Background()

	p, err := producer.NewProducer(types.Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		CustomerID:         os.Getenv("HELIX_CUSTOMER_ID"),
		Region:             "us-east-1",
	})
	if err != nil {
		panic(err)
	}

	invite, err := p.InviteConsumer(ctx, types.InviteConsumerInput{
		CompanyName:   "Acme Analytics",
		BusinessEmail: "data@acme.example",
		Datasets:      []string{"dataset-id"},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("invited consumer %s (email sent: %v)\n", invite.ConsumerID, invite.EmailSent)

	consumers, err := p.ListConsumers(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d partner consumer(s)\n", len(consumers))
}
