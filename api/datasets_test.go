package api

import (
	"context"
	"testing"

	"github.com/helix-tools/sdk-go/types"
)

// TestDatasets runs CRUD integration tests for the datasets resource.
func TestDatasets(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := LoadTestConfig(t)
	cfg.RequireProducerCredentials(t)

	ctx := context.Background()
	testID := GenerateTestID()
	client := NewTestClient(t, cfg, cfg.ProducerCredentials)
	cleanup := NewCleanupRegistry(t)

	defer cleanup.RunAll(ctx)

	var createdDatasetID string

	t.Run("List_Datasets", func(t *testing.T) {
		var resp struct {
			Datasets []types.Dataset `json:"datasets"`
			Count    int             `json:"count"`
		}

		err := client.Get(ctx, "/v1/datasets", &resp)
		if err != nil {
			t.Fatalf("failed to list datasets: %v", err)
		}

		t.Logf("Found %d datasets", resp.Count)
	})

	t.Run("List_Marketplace_Datasets", func(t *testing.T) {
		var resp struct {
			Datasets   []types.Dataset `json:"datasets"`
			Pagination struct {
				Total      int `json:"total"`
				Page       int `json:"page"`
				PerPage    int `json:"per_page"`
				TotalPages int `json:"total_pages"`
			} `json:"pagination"`
		}

		err := client.Get(ctx, "/v1/datasets/marketplace", &resp)
		if err != nil {
			t.Fatalf("failed to list marketplace datasets: %v", err)
		}

		t.Logf("Marketplace: %d datasets (page %d of %d)",
			resp.Pagination.Total,
			resp.Pagination.Page,
			resp.Pagination.TotalPages)
	})

	t.Run("List_Datasets_ByProducer", func(t *testing.T) {
		var resp struct {
			Datasets []types.Dataset `json:"datasets"`
			Count    int             `json:"count"`
		}

		path := "/v1/datasets?producer_id=" + cfg.ProducerCredentials.CustomerID

		err := client.Get(ctx, path, &resp)
		if err != nil {
			t.Fatalf("failed to list producer datasets: %v", err)
		}

		t.Logf("Producer %s has %d datasets", cfg.ProducerCredentials.CustomerID, resp.Count)
	})

	t.Run("Create_Dataset_Metadata", func(t *testing.T) {
		// Create dataset metadata (note: actual file upload requires Producer SDK).
		payload := NewTestDatasetPayload(testID, cfg.ProducerCredentials.CustomerID)

		var dataset types.Dataset

		err := client.Post(ctx, "/v1/datasets", payload, &dataset)
		if err != nil {
			t.Fatalf("failed to create dataset: %v", err)
		}

		if dataset.ID == "" && dataset.IDAlias == "" {
			t.Error("expected dataset ID to be set")
		}

		createdDatasetID = dataset.ID
		if createdDatasetID == "" {
			createdDatasetID = dataset.IDAlias
		}

		t.Logf("Created dataset: %s (%s)", createdDatasetID, dataset.Name)

		// Register cleanup.
		cleanup.RegisterDatasetCleanup(client, createdDatasetID)
	})

	t.Run("Get_Dataset_ByID", func(t *testing.T) {
		if createdDatasetID == "" {
			t.Skip("no dataset created")
		}

		var dataset types.Dataset

		err := client.Get(ctx, "/v1/datasets/"+createdDatasetID, &dataset)
		if err != nil {
			t.Fatalf("failed to get dataset: %v", err)
		}

		if dataset.ID != createdDatasetID && dataset.IDAlias != createdDatasetID {
			t.Errorf("expected ID %s, got %s", createdDatasetID, dataset.ID)
		}

		t.Logf("Dataset: %s (status: %s, visibility: %s)", dataset.Name, dataset.Status, dataset.Visibility)
	})

	t.Run("Get_Dataset_Details", func(t *testing.T) {
		if createdDatasetID == "" {
			t.Skip("no dataset created")
		}

		var resp struct {
			Dataset         types.Dataset   `json:"dataset"`
			Producer        map[string]any  `json:"producer"`
			Reviews         []any           `json:"reviews"`
			RelatedDatasets []types.Dataset `json:"related_datasets"`
		}

		err := client.Get(ctx, "/v1/datasets/"+createdDatasetID+"/details", &resp)
		if err != nil {
			t.Fatalf("failed to get dataset details: %v", err)
		}

		t.Logf("Dataset details: %s (reviews: %d, related: %d)",
			resp.Dataset.Name,
			len(resp.Reviews),
			len(resp.RelatedDatasets))
	})

	t.Run("Get_Dataset_NotFound", func(t *testing.T) {
		var dataset types.Dataset

		err := client.Get(ctx, "/v1/datasets/nonexistent-dataset-id", &dataset)
		if err == nil {
			t.Error("expected error for nonexistent dataset")
		}

		if !IsNotFoundError(err) {
			t.Logf("Note: API returned %v (expected 404)", err)
		}
	})
}

// TestDatasetPagination tests pagination for dataset listings.
func TestDatasetPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := LoadTestConfig(t)
	cfg.RequireProducerCredentials(t)

	ctx := context.Background()
	client := NewTestClient(t, cfg, cfg.ProducerCredentials)

	t.Run("Marketplace_Pagination", func(t *testing.T) {
		var page1 struct {
			Datasets   []types.Dataset `json:"datasets"`
			Pagination struct {
				Total      int `json:"total"`
				Page       int `json:"page"`
				PerPage    int `json:"per_page"`
				TotalPages int `json:"total_pages"`
			} `json:"pagination"`
		}

		err := client.Get(ctx, "/v1/datasets/marketplace?page=1&per_page=5", &page1)
		if err != nil {
			t.Fatalf("failed to get page 1: %v", err)
		}

		if page1.Pagination.Page != 1 {
			t.Errorf("expected page 1, got %d", page1.Pagination.Page)
		}

		if page1.Pagination.PerPage != 5 {
			t.Errorf("expected per_page 5, got %d", page1.Pagination.PerPage)
		}

		t.Logf("Page 1: %d items, total: %d", len(page1.Datasets), page1.Pagination.Total)
	})

	t.Run("Marketplace_Sorting", func(t *testing.T) {
		var resp struct {
			Datasets []types.Dataset `json:"datasets"`
		}

		// Test different sort options.
		for _, sort := range []string{"newest", "popular"} {
			err := client.Get(ctx, "/v1/datasets/marketplace?sort="+sort, &resp)
			if err != nil {
				t.Logf("Sort by %s: %v", sort, err)
			} else {
				t.Logf("Sort by %s: returned %d datasets", sort, len(resp.Datasets))
			}
		}
	})
}
