# Helix Connect Go SDK Makefile

.PHONY: all build test test-unit test-integration test-integration-local test-integration-prod lint clean

# Default target
all: build test-unit

# Build all packages
build:
	go build ./...

# Run all tests (unit tests only by default)
test: test-unit

# Run unit tests (skip integration tests)
test-unit:
	go test ./... -short -v

# Run integration tests against the configured endpoint
# Requires environment variables:
#   HELIX_TEST_BASE_URL (default: https://api-go.helix.tools)
#   HELIX_TEST_PRODUCER_ID, HELIX_TEST_PRODUCER_AWS_ACCESS_KEY_ID, HELIX_TEST_PRODUCER_AWS_SECRET_ACCESS_KEY
#   HELIX_TEST_CONSUMER_ID, HELIX_TEST_CONSUMER_AWS_ACCESS_KEY_ID, HELIX_TEST_CONSUMER_AWS_SECRET_ACCESS_KEY
test-integration:
	go test ./api/... -v -timeout 5m

# Run integration tests against local API
test-integration-local:
	HELIX_TEST_BASE_URL="http://localhost:8080" go test ./api/... -v -timeout 5m

# Run integration tests against production API
test-integration-prod:
	HELIX_TEST_BASE_URL="https://api-go.helix.tools" go test ./api/... -v -timeout 5m

# Run specific test suite
test-companies:
	go test ./api/... -v -run TestCompanies -timeout 5m

test-datasets:
	go test ./api/... -v -run TestDatasets -timeout 5m

test-subscription-requests:
	go test ./api/... -v -run TestSubscriptionRequests -timeout 5m

test-subscriptions:
	go test ./api/... -v -run TestSubscriptions -timeout 5m

# Run linting
lint:
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, skipping"

# Clean build artifacts
clean:
	go clean -cache -testcache

# Format code
fmt:
	go fmt ./...

# Generate coverage report
coverage:
	go test ./... -short -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Help
help:
	@echo "Helix Connect Go SDK"
	@echo ""
	@echo "Usage:"
	@echo "  make build                 - Build all packages"
	@echo "  make test                  - Run unit tests"
	@echo "  make test-unit             - Run unit tests (explicit)"
	@echo "  make test-integration      - Run integration tests"
	@echo "  make test-integration-local - Run integration tests against localhost"
	@echo "  make test-integration-prod  - Run integration tests against production"
	@echo "  make test-companies        - Run company CRUD tests"
	@echo "  make test-datasets         - Run dataset CRUD tests"
	@echo "  make test-subscription-requests - Run subscription request tests"
	@echo "  make test-subscriptions    - Run subscription tests"
	@echo "  make lint                  - Run linting"
	@echo "  make fmt                   - Format code"
	@echo "  make coverage              - Generate coverage report"
	@echo "  make clean                 - Clean build artifacts"
	@echo ""
	@echo "Environment variables for integration tests:"
	@echo "  HELIX_TEST_BASE_URL        - API endpoint (default: https://api-go.helix.tools)"
	@echo "  HELIX_TEST_PRODUCER_ID     - Producer customer ID"
	@echo "  HELIX_TEST_PRODUCER_AWS_ACCESS_KEY_ID"
	@echo "  HELIX_TEST_PRODUCER_AWS_SECRET_ACCESS_KEY"
	@echo "  HELIX_TEST_CONSUMER_ID     - Consumer customer ID"
	@echo "  HELIX_TEST_CONSUMER_AWS_ACCESS_KEY_ID"
	@echo "  HELIX_TEST_CONSUMER_AWS_SECRET_ACCESS_KEY"
	@echo "  HELIX_TEST_DATASET_ID      - Optional: Known dataset ID for subscription tests"
