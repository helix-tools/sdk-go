package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestCreateSubscriptionRequest_RejectsNonFreeTierBeforeAnyRequest: only the
// free tier exists. Any other tier must fail on the client as a
// *ValidationError, and no request may reach the API (the server would answer
// 400, but only after a round trip).
func TestCreateSubscriptionRequest_RejectsNonFreeTierBeforeAnyRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	for _, tier := range []string{"basic", "starter", "paid", "free-trial", "freeplus"} {
		t.Run(tier, func(t *testing.T) {
			_, err := c.CreateSubscriptionRequest(context.Background(), types.CreateSubscriptionRequestInput{
				ProducerID: "producer-1",
				Tier:       tier,
			})

			var vErr *ValidationError
			if !errors.As(err, &vErr) {
				t.Fatalf("err = %v (%T), want a *ValidationError", err, err)
			}
			if vErr.Field != "tier" {
				t.Errorf("ValidationError.Field = %q, want \"tier\"", vErr.Field)
			}
			if want := `validation error: tier: "` + tier + `" is not supported: the only tier is "free"`; err.Error() != want {
				t.Errorf("Error() = %q, want %q", err.Error(), want)
			}
		})
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("%d request(s) reached the API for a non-free tier; want 0", got)
	}
}

// TestCreateSubscriptionRequest_AcceptsWhatTheAPIAccepts is the positive
// control. The API trims and lower-cases the tier, so "Free", "FREE" and
// " free " succeed today and must keep succeeding; the SDK sends each one
// through unchanged.
func TestCreateSubscriptionRequest_AcceptsWhatTheAPIAccepts(t *testing.T) {
	for _, tier := range []string{"", "free", "Free", "FREE", " free "} {
		t.Run("tier="+tier, func(t *testing.T) {
			var sent map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Errorf("decode body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"_id":"req-1","request_id":"req-1","status":"pending","tier":"free"}`))
			}))
			defer server.Close()

			c := newTestConsumer(server.URL)
			if _, err := c.CreateSubscriptionRequest(context.Background(), types.CreateSubscriptionRequestInput{
				ProducerID: "producer-1",
				Tier:       tier,
			}); err != nil {
				t.Fatalf("CreateSubscriptionRequest(tier=%q): %v", tier, err)
			}
			// An empty tier is defaulted to "free"; any other value is sent as given.
			want := tier
			if tier == "" {
				want = "free"
			}
			if sent["tier"] != want {
				t.Errorf("outgoing tier = %q, want %q", sent["tier"], want)
			}
		})
	}
}
