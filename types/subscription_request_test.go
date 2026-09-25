package types

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// subscriptionRequestRequiredKeys mirrors subscription-request.schema.json
// `required`: keys the API always emits. A Go tag carrying omitempty on one of
// them is tag drift (parity audit B-08) — harmless on decode, wrong on encode.
var subscriptionRequestRequiredKeys = []string{
	"_id", "request_id", "consumer_id", "consumer_name", "consumer_email",
	"producer_id", "producer_name", "tier", "status", "created_at", "updated_at",
}

// jsonTags returns json key -> options for every tagged field of a struct.
func jsonTags(t *testing.T, v any) map[string]string {
	t.Helper()
	typ := reflect.TypeOf(v)
	out := map[string]string{}
	for i := 0; i < typ.NumField(); i++ {
		name, opts, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = opts
	}
	return out
}

func TestSubscriptionRequest_RequiredKeysAreNeverOmitted(t *testing.T) {
	tags := jsonTags(t, SubscriptionRequest{})
	for _, key := range subscriptionRequestRequiredKeys {
		opts, ok := tags[key]
		if !ok {
			t.Errorf("SubscriptionRequest has no json key %q (required by the schema)", key)
			continue
		}
		if strings.Contains(opts, "omitempty") {
			t.Errorf("SubscriptionRequest json key %q carries omitempty but the schema requires it", key)
		}
	}

	// Behavioural check, not just tags: a zero value must still emit the keys.
	out, err := json.Marshal(SubscriptionRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := wireMap(t, out)
	for _, key := range subscriptionRequestRequiredKeys {
		if _, present := got[key]; !present {
			t.Errorf("zero SubscriptionRequest omits required key %q: %s", key, out)
		}
	}
}

// TestSubscriptionRequest_MatchesSchemaFile compares the pinned key list above
// to the real subscription-request.schema.json, located beside the schema file
// HELIX_SUBSCRIPTION_SCHEMA already points CI at. Skips visibly when unset
// (fails when HELIX_SCHEMAS_REQUIRED=1, like the dataset_info check).
func TestSubscriptionRequest_MatchesSchemaFile(t *testing.T) {
	base := os.Getenv(datasetInfoSchemaEnv)
	if base == "" {
		if os.Getenv(datasetInfoSchemasRequiredEnv) == "1" {
			t.Fatalf("%s not set but %s=1: CI must compare against the real schema file", datasetInfoSchemaEnv, datasetInfoSchemasRequiredEnv)
		}
		t.Skipf("%s not set: schema-file comparison not run (in-repo pinned key list only)", datasetInfoSchemaEnv)
	}
	path := filepath.Join(filepath.Dir(base), "subscription-request.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var schema struct {
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	want := append([]string(nil), subscriptionRequestRequiredKeys...)
	sort.Strings(want)
	got := append([]string(nil), schema.Required...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schema required = %v, pinned list = %v; update subscriptionRequestRequiredKeys with the schema", got, want)
	}

	tags := jsonTags(t, SubscriptionRequest{})
	for key := range tags {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("SubscriptionRequest json key %q is not a schema property", key)
		}
	}
}
