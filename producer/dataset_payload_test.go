package producer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/helix-tools/sdk-go/types"
)

func TestBuildDatasetPayloadDefaults(t *testing.T) {
	p := &Producer{
		CustomerID: "customer-123",
		BucketName: "helix-producer-sample-production",
	}

	metadata := map[string]any{
		"compression_enabled": true,
	}

	analysis := &AnalysisResult{
		Schema: map[string]any{
			"type": "object",
		},
		FieldEmptiness: map[string]float64{
			"name": 0,
		},
		RecordCount:    2,
		AnalysisErrors: 0,
	}

	payload := p.buildDatasetPayload(
		"Sample Dataset",
		"Desc",
		"general",
		types.DataFreshnessDaily,
		"datasets/sample/data.ndjson",
		1024,
		metadata,
		analysis,
		nil,
	)

	id, ok := payload["_id"].(string)
	if !ok || id == "" {
		t.Fatalf("expected generated dataset id")
	}
	if payload["visibility"] != "private" {
		t.Fatalf("expected visibility private, got %v", payload["visibility"])
	}
	if payload["status"] != "active" {
		t.Fatalf("expected status active, got %v", payload["status"])
	}
	if payload["record_count"] != 2 {
		t.Fatalf("expected record_count 2, got %v", payload["record_count"])
	}

	meta := payload["metadata"].(map[string]any)
	if meta["record_count"].(int) != 2 {
		t.Fatalf("expected metadata record_count 2")
	}
	if meta["schema"] == nil {
		t.Fatalf("expected schema in metadata")
	}

	schemaPath := filepath.Join("..", "..", "schemas", "dataset.schema.json")
	content, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("failed to read dataset schema: %v", err)
	}

	var datasetSchema map[string]any
	if err := json.Unmarshal(content, &datasetSchema); err != nil {
		t.Fatalf("failed to parse dataset schema: %v", err)
	}

	required, ok := datasetSchema["required"].([]any)
	if !ok {
		t.Fatalf("dataset schema missing required list")
	}
	for _, field := range required {
		key := field.(string)
		if _, ok := payload[key]; !ok {
			t.Fatalf("expected payload to include required field %s", key)
		}
	}
}

func TestBuildDatasetPayloadOverrides(t *testing.T) {
	p := &Producer{CustomerID: "customer-123", BucketName: "bucket"}
	overrides := map[string]any{
		"visibility": "restricted",
		"pricing": map[string]any{
			"basic": map[string]any{"amount": 99},
		},
		"_id": "custom-123",
	}
	payload := p.buildDatasetPayload(
		"Sample",
		"Desc",
		"general",
		types.DataFreshnessDaily,
		"datasets/sample/data.ndjson",
		100,
		map[string]any{},
		&AnalysisResult{},
		overrides,
	)

	if payload["visibility"] != "restricted" {
		t.Fatalf("override visibility not applied")
	}
	if payload["_id"] != "custom-123" {
		t.Fatalf("override id not applied")
	}
	pricing := payload["pricing"].(map[string]any)
	basic := pricing["basic"].(map[string]any)
	if basic["amount"].(int) != 99 {
		t.Fatalf("override pricing not applied")
	}
}
