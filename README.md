# Helix Connect SDK for Go

Official Go SDK for producing and consuming datasets from Helix Connect data marketplace.

## Features

### Producer SDK
- 📤 **Dataset Upload** - Upload datasets with compression and encryption
- 🗜️ **Gzip Compression** - Configurable compression levels (1-9)
- 🔐 **KMS Encryption** - Envelope encryption with AES-256-GCM
- 📝 **Dataset Management** - Create and list datasets
- ✅ **Production Ready** - Tested and validated

### Consumer SDK
- 🔐 **AWS SigV4 Authentication** - Secure IAM-based API authentication
- 📦 **Dataset Download** - Stream and download datasets efficiently
- 🔑 **KMS Decryption** - Automatic envelope encryption decryption
- 🗜️ **Gzip Decompression** - Built-in compression support
- ✅ **Production Ready** - Tested and validated

## Installation

```bash
go get github.com/helix-tools/sdk-go
```

## Quick Start

### Producer - Upload Datasets

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/helix-tools/sdk-go/producer"
)

func main() {
    // Initialize producer
    // IMPORTANT: Never hardcode credentials. Use environment variables.
    cfg := producer.Config{
        CustomerID:          os.Getenv("HELIX_CUSTOMER_ID"),
        AWSAccessKeyID:      os.Getenv("HELIX_ACCESS_KEY_ID"),
        AWSSecretAccessKey:  os.Getenv("HELIX_SECRET_ACCESS_KEY"),
        APIEndpoint:         "https://api.helix.tools",
        Region:              "us-east-1",
    }

    prod, err := producer.NewProducer(cfg)
    if err != nil {
        log.Fatal(err)
    }

    // Upload dataset with compression and encryption
    ctx := context.Background()
    dataset, err := prod.UploadDataset(ctx, "./data.json", producer.UploadOptions{
        DatasetName:      "My Dataset",
        Description:      "Description of my dataset",
        Category:         "analytics",
        DataFreshness:    "daily",
        Encrypt:          true,
        Compress:         true,
        CompressionLevel: 6,
        Metadata: map[string]interface{}{
            "version": "1.0",
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    fmt.Printf("Dataset uploaded! ID: %s\n", dataset.ID)
}
```

### Consumer - Download Datasets

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/helix-tools/sdk-go/consumer"
)

func main() {
    // Initialize consumer
    // IMPORTANT: Never hardcode credentials. Use environment variables.
    cfg := consumer.Config{
        CustomerID:          os.Getenv("HELIX_CUSTOMER_ID"),
        AWSAccessKeyID:      os.Getenv("HELIX_ACCESS_KEY_ID"),
        AWSSecretAccessKey:  os.Getenv("HELIX_SECRET_ACCESS_KEY"),
        APIEndpoint:         "https://api.helix.tools",
        Region:              "us-east-1",
    }

    c, err := consumer.NewConsumer(cfg)
    if err != nil {
        log.Fatal(err)
    }

    // Download dataset (auto-decrypt + auto-decompress)
    ctx := context.Background()
    err = c.DownloadDataset(ctx, "dataset-id", "./output.json")
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println("Dataset downloaded successfully!")
}
```

## Security Best Practices

### Credential Management

**⚠️ NEVER hardcode credentials in your application code.**

Always use one of these secure methods:

1. **Environment Variables** (Recommended)
```bash
export HELIX_CUSTOMER_ID="your-customer-id"
export HELIX_ACCESS_KEY_ID="your-access-key"
export HELIX_SECRET_ACCESS_KEY="your-secret-key"
```

2. **AWS Secrets Manager**
```go
// Fetch credentials from AWS Secrets Manager at runtime
secret := fetchFromSecretsManager("helix/credentials")
cfg := producer.Config{
    CustomerID:         secret["customer_id"],
    AWSAccessKeyID:     secret["access_key_id"],
    AWSSecretAccessKey: secret["secret_access_key"],
}
```

3. **Configuration Files** (with proper .gitignore)
```go
// Load from config file (ensure config file is in .gitignore)
cfg := loadConfigFromFile(".helix-config")
```

### .gitignore Requirements

Ensure your `.gitignore` includes:
```
.env
.env.*
*credentials*
*secrets*
*.config
config.json
```

## License

See [LICENSE](LICENSE) file for details.

## Support

For issues and questions:
- GitHub: https://github.com/helix-tools/helix-connect
- Email: support@helix.tools
- Documentation: https://docs.helix.tools

## Contributing

This is a proprietary SDK maintained by Helix. For feature requests or bug reports, please contact support@helix.tools.
