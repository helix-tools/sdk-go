package api

import (
	"fmt"
	"time"

	"github.com/helix-tools/sdk-go/types"
)

// TestPrefix is used to identify test resources for cleanup.
const TestPrefix = "TEST_"

// GenerateTestID generates a unique test run identifier.
func GenerateTestID() string {
	return fmt.Sprintf("int-%s-%d", time.Now().Format("20060102150405"), time.Now().UnixMilli()%10000)
}

// NewTestCompany creates a test company request with a unique name.
func NewTestCompany(testID string, customerType string) types.CreateCompanyRequest {
	email := fmt.Sprintf("%scompany-%s@test.helix-integration.local", TestPrefix, testID)
	phone := "+15551234567"

	return types.CreateCompanyRequest{
		CompanyName:   fmt.Sprintf("%sCompany_%s", TestPrefix, testID),
		BusinessEmail: email,
		CustomerType:  customerType,
		Phone:         &phone,
		Address: &types.Address{
			Street:     "123 Integration Test Street",
			City:       "Testville",
			State:      "CA",
			PostalCode: "90210",
			Country:    "US",
		},
	}
}

// NewTestProducerCompany creates a test producer company.
func NewTestProducerCompany(testID string) types.CreateCompanyRequest {
	return NewTestCompany(testID, "producer")
}

// NewTestConsumerCompany creates a test consumer company.
func NewTestConsumerCompany(testID string) types.CreateCompanyRequest {
	return NewTestCompany(testID, "consumer")
}

// NewTestDatasetPayload creates a test dataset registration payload.
func NewTestDatasetPayload(testID, producerID string) map[string]any {
	return map[string]any{
		"name":           fmt.Sprintf("%sdataset_%s", TestPrefix, testID),
		"description":    fmt.Sprintf("Integration test dataset - %s", testID),
		"producer_id":    producerID,
		"category":       "test",
		"data_freshness": "daily",
		"visibility":     "private",
		"status":         "active",
		"s3_key":         fmt.Sprintf("datasets/%sdataset_%s/data.ndjson.gz", TestPrefix, testID),
		"s3_bucket_name": "test-bucket",
		"s3_bucket":      "test-bucket",
		"size_bytes":     1024,
		"record_count":   10,
	}
}

// NewTestSubscriptionRequest creates a test subscription request payload.
func NewTestSubscriptionRequest(producerID string, datasetID *string) types.CreateSubscriptionRequestPayload {
	message := "Integration test subscription request"

	return types.CreateSubscriptionRequestPayload{
		ProducerID: producerID,
		DatasetID:  datasetID,
		Tier:       "basic",
		Message:    &message,
	}
}

// NewTestUserInvite creates a test user invite payload.
func NewTestUserInvite(testID string) types.InviteUserRequest {
	return types.InviteUserRequest{
		Email:     fmt.Sprintf("%suser-%s@test.helix-integration.local", TestPrefix, testID),
		FirstName: "Test",
		LastName:  "User",
		Role:      "member",
		Permissions: &types.UserPermissions{
			CanCreateDatasets: false,
			CanDeleteDatasets: false,
			CanManageBilling:  false,
			CanInviteUsers:    false,
		},
	}
}

// StringPtr returns a pointer to a string.
func StringPtr(s string) *string {
	return &s
}

// IntPtr returns a pointer to an int.
func IntPtr(i int) *int {
	return &i
}
