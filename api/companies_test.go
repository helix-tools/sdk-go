package api

import (
	"context"
	"testing"

	"github.com/helix-tools/sdk-go/types"
)

// TestCompanies runs CRUD integration tests for the companies resource.
// These tests require admin credentials to create/update/delete companies.
func TestCompanies(t *testing.T) {
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

	var createdCompanyID string

	t.Run("List_Companies", func(t *testing.T) {
		var resp types.CompaniesResponse

		err := client.Get(ctx, "/v1/companies", &resp)
		if err != nil {
			t.Fatalf("failed to list companies: %v", err)
		}

		t.Logf("Found %d companies", resp.Count)

		if resp.Count < 0 {
			t.Error("company count should not be negative")
		}
	})

	t.Run("Create_Company", func(t *testing.T) {
		req := NewTestConsumerCompany(testID)

		var resp types.CreateCompanyResponse

		err := client.Post(ctx, "/v1/companies", req, &resp)
		if err != nil {
			t.Fatalf("failed to create company: %v", err)
		}

		if !resp.Success {
			t.Error("expected success to be true")
		}

		if resp.CompanyID == "" {
			t.Error("expected company ID to be set")
		}

		createdCompanyID = resp.CompanyID
		t.Logf("Created company: %s", createdCompanyID)

		// Register cleanup.
		cleanup.RegisterCompanyCleanup(client, createdCompanyID)
	})

	t.Run("Get_Company_ByID", func(t *testing.T) {
		if createdCompanyID == "" {
			t.Skip("no company created")
		}

		var company types.Company

		err := client.Get(ctx, "/v1/companies/"+createdCompanyID, &company)
		if err != nil {
			t.Fatalf("failed to get company: %v", err)
		}

		if company.ID != createdCompanyID {
			t.Errorf("expected ID %s, got %s", createdCompanyID, company.ID)
		}

		if company.CustomerType != "consumer" {
			t.Errorf("expected customer_type consumer, got %s", company.CustomerType)
		}

		t.Logf("Company: %s (%s)", company.CompanyName, company.Status)
	})

	t.Run("Update_Company", func(t *testing.T) {
		if createdCompanyID == "" {
			t.Skip("no company created")
		}

		newName := TestPrefix + "UpdatedCompany_" + testID
		req := types.UpdateCompanyRequest{
			CompanyName: &newName,
		}

		var company types.Company

		err := client.Patch(ctx, "/v1/companies/"+createdCompanyID, req, &company)
		if err != nil {
			t.Fatalf("failed to update company: %v", err)
		}

		if company.CompanyName != newName {
			t.Errorf("expected name %s, got %s", newName, company.CompanyName)
		}

		t.Logf("Updated company name to: %s", company.CompanyName)
	})

	t.Run("List_Company_Users", func(t *testing.T) {
		if createdCompanyID == "" {
			t.Skip("no company created")
		}

		var resp types.CompanyUsersResponse

		err := client.Get(ctx, "/v1/companies/"+createdCompanyID+"/users", &resp)
		if err != nil {
			t.Fatalf("failed to list company users: %v", err)
		}

		t.Logf("Company has %d users", resp.Count)
	})

	t.Run("Get_Company_NotFound", func(t *testing.T) {
		var company types.Company

		err := client.Get(ctx, "/v1/companies/nonexistent-company-id", &company)
		if err == nil {
			t.Error("expected error for nonexistent company")
		}

		if !IsNotFoundError(err) {
			t.Logf("Note: API returned %v (expected 404)", err)
		}
	})

	t.Run("Delete_Company", func(t *testing.T) {
		if createdCompanyID == "" {
			t.Skip("no company created")
		}

		err := client.Delete(ctx, "/v1/companies/"+createdCompanyID)
		if err != nil {
			t.Fatalf("failed to delete company: %v", err)
		}

		t.Logf("Deleted company: %s", createdCompanyID)

		// Verify deletion (should return 404 or company with cancelled status).
		var company types.Company

		err = client.Get(ctx, "/v1/companies/"+createdCompanyID, &company)
		if err != nil {
			if IsNotFoundError(err) {
				t.Log("Company deleted (404)")
			} else {
				t.Logf("Get after delete returned: %v", err)
			}
		} else {
			// Company might still exist with cancelled status.
			if company.Status == "cancelled" || company.Status == "inactive" {
				t.Logf("Company status: %s", company.Status)
			} else {
				t.Errorf("expected company to be deleted or cancelled, got status: %s", company.Status)
			}
		}
	})
}

// TestCompanyValidation tests input validation for company operations.
func TestCompanyValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := LoadTestConfig(t)
	cfg.RequireProducerCredentials(t)

	ctx := context.Background()
	client := NewTestClient(t, cfg, cfg.ProducerCredentials)

	t.Run("Create_Company_MissingName", func(t *testing.T) {
		req := types.CreateCompanyRequest{
			BusinessEmail: "test@example.com",
			CustomerType:  "consumer",
		}

		var resp types.CreateCompanyResponse

		err := client.Post(ctx, "/v1/companies", req, &resp)
		if err == nil {
			t.Error("expected error for missing company name")
		}

		if !IsBadRequestError(err) {
			t.Logf("Note: API returned %v (expected 400)", err)
		}
	})

	t.Run("Create_Company_InvalidType", func(t *testing.T) {
		req := types.CreateCompanyRequest{
			CompanyName:   "Test Company",
			BusinessEmail: "test@example.com",
			CustomerType:  "invalid_type",
		}

		var resp types.CreateCompanyResponse

		err := client.Post(ctx, "/v1/companies", req, &resp)
		if err == nil {
			t.Error("expected error for invalid customer type")
		}

		if !IsBadRequestError(err) {
			t.Logf("Note: API returned %v (expected 400)", err)
		}
	})
}
