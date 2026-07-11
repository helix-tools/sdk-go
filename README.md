# Helix Connect Platform Go SDK

Official Go SDK for the Helix Connect data marketplace platform.

## Overview

The Helix Connect Go SDK enables data producers and consumers to securely exchange and consume datasets through the Helix Connect platform.

## Installation

```bash
go get github.com/helix-tools/sdk-go/v2
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

Everything else — dataset downloads, SQS polling, KMS decrypt — works
identically in both modes; the SDK refreshes and re-signs automatically. See
`CHANGELOG.md` for details and `credentials/broker.go`'s package doc for the
lower-level `Provider`/`NewCredentialsCache` API.

## Support

- Documentation: https://docs.helix.tools
- Issues: https://github.com/helix-tools/sdk-go/issues
- Email: support@helix.tools

## License

See LICENSE file for details.
