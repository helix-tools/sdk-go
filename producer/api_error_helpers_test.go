package producer

import (
	"errors"
	"fmt"
	"testing"
)

// TestAPIError_StatusHelpers pins each status helper to its own code and no
// other, across the codes a caller meets, so a helper cannot drift onto a
// neighbouring status. It matches the consumer package's helpers.
func TestAPIError_StatusHelpers(t *testing.T) {
	helpers := map[string]func(*APIError) bool{
		"IsConflict":     (*APIError).IsConflict,
		"IsUnauthorized": (*APIError).IsUnauthorized,
		"IsForbidden":    (*APIError).IsForbidden,
		"IsNotFound":     (*APIError).IsNotFound,
		"IsRateLimited":  (*APIError).IsRateLimited,
	}
	wantCode := map[string]int{
		"IsConflict":     409,
		"IsUnauthorized": 401,
		"IsForbidden":    403,
		"IsNotFound":     404,
		"IsRateLimited":  429,
	}
	codes := []int{200, 400, 401, 403, 404, 409, 429, 500, 503}

	for name, helper := range helpers {
		for _, code := range codes {
			got := helper(&APIError{StatusCode: code})
			want := code == wantCode[name]
			if got != want {
				t.Errorf("%s() with StatusCode %d = %v, want %v", name, code, got, want)
			}
		}
	}
}

// TestAPIError_HelpersWorkThroughErrorsAs is the README's own pattern: a
// wrapped producer error is still classified by its status.
func TestAPIError_HelpersWorkThroughErrorsAs(t *testing.T) {
	err := fmt.Errorf("upload failed: %w", &APIError{StatusCode: 429, Body: "slow down"})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("errors.As did not find the *APIError")
	}
	if !apiErr.IsRateLimited() {
		t.Errorf("IsRateLimited() = false for a 429")
	}
}
