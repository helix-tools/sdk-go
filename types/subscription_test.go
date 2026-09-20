package types

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSubscription_ConsumerInfoRoundTrip pins subscription.consumer_info
// (subscription.schema.json) to Subscription.ConsumerInfo. Without this
// field, producers reading GET /v1/subscriptions silently lose the
// server-side consumer enrichment used to render the Subscribers tab.
func TestSubscription_ConsumerInfoRoundTrip(t *testing.T) {
	raw := `{
		"_id": "sub-1",
		"consumer_id": "cons-1",
		"dataset_id": "ds-1",
		"producer_id": "prod-1",
		"tier": "free",
		"status": "active",
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z",
		"consumer_info": {
			"company_name": "Acme Data Co",
			"email": "buyer@acme.example"
		}
	}`

	var sub Subscription
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		t.Fatalf("unmarshal Subscription: %v", err)
	}
	if sub.ConsumerInfo == nil {
		t.Fatal("ConsumerInfo = nil, want populated struct")
	}
	if sub.ConsumerInfo.CompanyName != "Acme Data Co" {
		t.Errorf("ConsumerInfo.CompanyName = %q, want %q", sub.ConsumerInfo.CompanyName, "Acme Data Co")
	}
	if sub.ConsumerInfo.Email != "buyer@acme.example" {
		t.Errorf("ConsumerInfo.Email = %q, want %q", sub.ConsumerInfo.Email, "buyer@acme.example")
	}
}

// TestSubscription_ConsumerInfoAbsentIsNil proves subscriptions read on a
// path that doesn't populate the enrichment decode to a nil pointer, not a
// zero-value struct that would look like an empty-but-present record.
func TestSubscription_ConsumerInfoAbsentIsNil(t *testing.T) {
	raw := `{
		"_id": "sub-2",
		"consumer_id": "cons-2",
		"dataset_id": "ds-1",
		"producer_id": "prod-1",
		"tier": "free",
		"status": "active",
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z"
	}`

	var sub Subscription
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		t.Fatalf("unmarshal Subscription: %v", err)
	}
	if sub.ConsumerInfo != nil {
		t.Errorf("ConsumerInfo = %+v, want nil", sub.ConsumerInfo)
	}
}

// datasetInfoContractKeys is the ONE cross-SDK contract for
// subscription.dataset_info: exactly these five keys, all optional. The
// schema (subscription.schema.json), the API, the portal and the
// TypeScript/Python SDKs pin the same set. Never add or rename a key here
// without the schema changing first; sibling fields belong NEXT TO
// dataset_info on Subscription, never inside it.
var datasetInfoContractKeys = []string{"last_updated", "name", "record_count", "size_bytes", "updated_at"}

// subscriptionJSON builds a minimal valid Subscription wire body, appending
// datasetInfo verbatim as the dataset_info value when non-empty.
func subscriptionJSON(datasetInfo string) string {
	raw := `{
		"_id": "sub-1",
		"consumer_id": "cons-1",
		"dataset_id": "ds-1",
		"producer_id": "prod-1",
		"tier": "free",
		"status": "active",
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z"`
	if datasetInfo != "" {
		raw += `, "dataset_info": ` + datasetInfo
	}
	return raw + "}"
}

// wireMap decodes JSON into a generic map so assertions compare the exact
// wire keys (case-sensitive), which a typed decode cannot see.
func wireMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal into map: %v\n%s", err, raw)
	}
	return m
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const fullDatasetInfoJSON = `{
	"name": "example-dataset",
	"last_updated": "2026-09-20T00:01:43Z",
	"updated_at": "2026-09-20T00:01:44Z",
	"record_count": 1604854,
	"size_bytes": 12247173
}`

// TestSubscription_DatasetInfoRoundTrip pins subscription.dataset_info
// (subscription.schema.json) to Subscription.DatasetInfo. Without it, a
// consumer reading GET /v1/subscriptions silently loses the dataset's
// freshness (last_updated) and size, and its UI falls back to "N/A".
func TestSubscription_DatasetInfoRoundTrip(t *testing.T) {
	var sub Subscription
	if err := json.Unmarshal([]byte(subscriptionJSON(fullDatasetInfoJSON)), &sub); err != nil {
		t.Fatalf("unmarshal Subscription: %v", err)
	}
	di := sub.DatasetInfo
	if di == nil {
		t.Fatal("DatasetInfo = nil, want populated struct")
	}
	if di.Name != "example-dataset" {
		t.Errorf("DatasetInfo.Name = %q, want %q", di.Name, "example-dataset")
	}
	if di.LastUpdated != "2026-09-20T00:01:43Z" {
		t.Errorf("DatasetInfo.LastUpdated = %q, want %q", di.LastUpdated, "2026-09-20T00:01:43Z")
	}
	if di.UpdatedAt != "2026-09-20T00:01:44Z" {
		t.Errorf("DatasetInfo.UpdatedAt = %q, want %q", di.UpdatedAt, "2026-09-20T00:01:44Z")
	}
	if di.RecordCount != 1604854 {
		t.Errorf("DatasetInfo.RecordCount = %d, want 1604854", di.RecordCount)
	}
	if di.SizeBytes != 12247173 {
		t.Errorf("DatasetInfo.SizeBytes = %d, want 12247173", di.SizeBytes)
	}

	// Re-encode and compare against the input on the wire: every one of the
	// five keys must survive decode -> encode under its exact schema name.
	out, err := json.Marshal(sub)
	if err != nil {
		t.Fatalf("marshal Subscription: %v", err)
	}
	got, ok := wireMap(t, out)["dataset_info"].(map[string]any)
	if !ok {
		t.Fatalf("re-encoded Subscription has no dataset_info object: %s", out)
	}
	want := wireMap(t, []byte(fullDatasetInfoJSON))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("re-encoded dataset_info = %v, want %v", got, want)
	}
}

// TestSubscription_DatasetInfoAbsentIsNil proves a subscription read on a
// path that does not enrich decodes to a nil pointer (not a zero-value struct
// that looks like an empty-but-present record) and re-encodes without the key.
func TestSubscription_DatasetInfoAbsentIsNil(t *testing.T) {
	var sub Subscription
	if err := json.Unmarshal([]byte(subscriptionJSON("")), &sub); err != nil {
		t.Fatalf("unmarshal Subscription: %v", err)
	}
	if sub.DatasetInfo != nil {
		t.Errorf("DatasetInfo = %+v, want nil", sub.DatasetInfo)
	}
	out, err := json.Marshal(sub)
	if err != nil {
		t.Fatalf("marshal Subscription: %v", err)
	}
	if _, present := wireMap(t, out)["dataset_info"]; present {
		t.Errorf("nil DatasetInfo re-encoded a dataset_info key: %s", out)
	}
}

// TestSubscription_DatasetInfoNullIsNil is self-attack (a): the API may put an
// explicit dataset_info:null on the wire (a subscription whose dataset could
// not be resolved). It must decode to nil, not error and not a zero struct.
func TestSubscription_DatasetInfoNullIsNil(t *testing.T) {
	var sub Subscription
	if err := json.Unmarshal([]byte(subscriptionJSON("null")), &sub); err != nil {
		t.Fatalf("unmarshal Subscription with dataset_info:null: %v", err)
	}
	if sub.DatasetInfo != nil {
		t.Errorf("DatasetInfo = %+v, want nil for dataset_info:null", sub.DatasetInfo)
	}
	if sub.ID != "sub-1" {
		t.Errorf("ID = %q, want %q (null dataset_info must not disturb siblings)", sub.ID, "sub-1")
	}
}

// TestSubscription_DatasetInfoAllKeysOptional pins that every key inside
// dataset_info is independently optional: a partial object decodes and
// re-encodes only what was sent, and an empty object is present-but-empty.
func TestSubscription_DatasetInfoAllKeysOptional(t *testing.T) {
	for _, key := range datasetInfoContractKeys {
		key := key
		t.Run("only_"+key, func(t *testing.T) {
			value := `"x"`
			if key == "record_count" || key == "size_bytes" {
				value = `7`
			}
			raw := subscriptionJSON(`{"` + key + `": ` + value + `}`)
			var sub Subscription
			if err := json.Unmarshal([]byte(raw), &sub); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if sub.DatasetInfo == nil {
				t.Fatal("DatasetInfo = nil, want non-nil for a partial object")
			}
			out, err := json.Marshal(sub)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := wireMap(t, out)["dataset_info"].(map[string]any)
			if !reflect.DeepEqual(sortedKeys(got), []string{key}) {
				t.Errorf("re-encoded keys = %v, want only [%s]", sortedKeys(got), key)
			}
		})
	}

	var sub Subscription
	if err := json.Unmarshal([]byte(subscriptionJSON(`{}`)), &sub); err != nil {
		t.Fatalf("unmarshal dataset_info:{}: %v", err)
	}
	if sub.DatasetInfo == nil {
		t.Fatal("DatasetInfo = nil for dataset_info:{}, want present-but-empty")
	}
	if *sub.DatasetInfo != (DatasetInfo{}) {
		t.Errorf("DatasetInfo = %+v, want zero value", *sub.DatasetInfo)
	}
}

// TestSubscription_DatasetInfoUnknownKeysAreIgnored is self-attack (b): a
// future sibling key, or the legacy Lambda-era keys the portal once planted
// (category/data_freshness/pricing), arriving INSIDE dataset_info must not
// break decoding, must not disturb the five known keys, and must not be
// smuggled back out on re-encode (the contract is exactly five keys).
func TestSubscription_DatasetInfoUnknownKeysAreIgnored(t *testing.T) {
	raw := subscriptionJSON(`{
		"name": "example-dataset",
		"last_updated": "2026-09-20T00:01:43Z",
		"updated_at": "2026-09-20T00:01:44Z",
		"record_count": 10,
		"size_bytes": 20,
		"category": "telecom",
		"data_freshness": "daily",
		"pricing": {"free": true},
		"some_future_key": [1, 2, 3]
	}`)
	var sub Subscription
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		t.Fatalf("unknown keys inside dataset_info must not error: %v", err)
	}
	if sub.DatasetInfo == nil || sub.DatasetInfo.Name != "example-dataset" ||
		sub.DatasetInfo.RecordCount != 10 || sub.DatasetInfo.SizeBytes != 20 {
		t.Fatalf("known keys disturbed by unknown ones: %+v", sub.DatasetInfo)
	}
	out, err := json.Marshal(sub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := sortedKeys(wireMap(t, out)["dataset_info"].(map[string]any))
	if !reflect.DeepEqual(got, datasetInfoContractKeys) {
		t.Errorf("re-encoded dataset_info keys = %v, want exactly %v", got, datasetInfoContractKeys)
	}
}

// TestSubscription_DatasetInfoNumericWireForms is self-attack (c): how
// record_count / size_bytes behave for every shape the wire could carry.
// Integers (including a value stored as int32 and one past int32) and null
// decode; a float or string is a contract violation (the schema declares
// integer) and must surface as a *json.UnmarshalTypeError naming the field,
// never as a silently wrong or zeroed count.
func TestSubscription_DatasetInfoNumericWireForms(t *testing.T) {
	ok := []struct {
		name, value string
		want        int64
	}{
		{"int32_max", `2147483647`, 2147483647},
		{"past_int32", `12247173999`, 12247173999},
		{"zero", `0`, 0},
		{"null", `null`, 0},
	}
	for _, field := range []string{"record_count", "size_bytes"} {
		for _, tc := range ok {
			field, tc := field, tc
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				raw := subscriptionJSON(`{"` + field + `": ` + tc.value + `}`)
				var sub Subscription
				if err := json.Unmarshal([]byte(raw), &sub); err != nil {
					t.Fatalf("unmarshal %s=%s: %v", field, tc.value, err)
				}
				got := sub.DatasetInfo.RecordCount
				if field == "size_bytes" {
					got = sub.DatasetInfo.SizeBytes
				}
				if got != tc.want {
					t.Errorf("%s = %d, want %d", field, got, tc.want)
				}
			})
		}

		for _, bad := range []struct{ name, value string }{
			{"fractional_float", `1604854.5`},
			{"integral_float", `1604854.0`},
			{"exponent_float", `1.604854e6`},
			{"string", `"1604854"`},
			{"bool", `true`},
		} {
			field, bad := field, bad
			t.Run(field+"/rejects_"+bad.name, func(t *testing.T) {
				raw := subscriptionJSON(`{"` + field + `": ` + bad.value + `}`)
				var sub Subscription
				err := json.Unmarshal([]byte(raw), &sub)
				var typeErr *json.UnmarshalTypeError
				if !errors.As(err, &typeErr) {
					t.Fatalf("unmarshal %s=%s: err = %v, want *json.UnmarshalTypeError", field, bad.value, err)
				}
				if !strings.HasSuffix(typeErr.Field, "dataset_info."+field) {
					t.Errorf("error names field %q, want it to end in dataset_info.%s", typeErr.Field, field)
				}
			})
		}
	}
}

// TestSubscription_DatasetInfoContract pins the SDK type to the five-key
// dataset_info contract from the Go side: the exported DatasetInfo type
// carries exactly the five json keys, every one omitempty (all optional),
// and Subscription exposes it as an optional *DatasetInfo under
// "dataset_info". Both sides are checked — reflected tags AND the keys a
// fully populated value actually emits — because Go decodes JSON keys
// case-insensitively: a wrong-case tag (json:"Record_Count") still decodes
// the schema's record_count, so a decode-only test would pass while every
// other SDK and the portal saw a different key on the wire.
func TestSubscription_DatasetInfoContract(t *testing.T) {
	typ := reflect.TypeOf(DatasetInfo{})
	if !typ.Field(0).IsExported() || typ.Name() != "DatasetInfo" {
		t.Fatalf("DatasetInfo must be an exported struct type, got %v", typ)
	}

	var tagKeys []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			t.Errorf("field %s has no json key (tag %q)", typ.Field(i).Name, tag)
		}
		if opts != "omitempty" {
			t.Errorf("field %s json tag %q must be exactly <key>,omitempty (all keys optional)", typ.Field(i).Name, tag)
		}
		tagKeys = append(tagKeys, name)
	}
	sort.Strings(tagKeys)
	if !reflect.DeepEqual(tagKeys, datasetInfoContractKeys) {
		t.Errorf("DatasetInfo json tags = %v, want exactly %v", tagKeys, datasetInfoContractKeys)
	}

	full := DatasetInfo{
		Name: "n", LastUpdated: "2026-09-20T00:01:43Z", UpdatedAt: "2026-09-20T00:01:44Z",
		RecordCount: 1, SizeBytes: 2,
	}
	out, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal DatasetInfo: %v", err)
	}
	if got := sortedKeys(wireMap(t, out)); !reflect.DeepEqual(got, datasetInfoContractKeys) {
		t.Errorf("fully populated DatasetInfo emits keys %v, want exactly %v", got, datasetInfoContractKeys)
	}

	field, found := reflect.TypeOf(Subscription{}).FieldByName("DatasetInfo")
	if !found {
		t.Fatal("Subscription has no DatasetInfo field")
	}
	if field.Tag.Get("json") != "dataset_info,omitempty" {
		t.Errorf("Subscription.DatasetInfo json tag = %q, want %q", field.Tag.Get("json"), "dataset_info,omitempty")
	}
	if field.Type != reflect.TypeOf((*DatasetInfo)(nil)) {
		t.Errorf("Subscription.DatasetInfo type = %v, want *DatasetInfo", field.Type)
	}
}

// datasetInfoSchemaEnv names the env var that points the schema-file check at
// a subscription.schema.json. The schema lives in the sdk-schemas repo, not
// here, so a fixed path would only work on one machine (a clean CI checkout
// would go red for the wrong reason); the in-repo pinned key set above is the
// always-on gate, and this compares it against the real file when given one.
const datasetInfoSchemaEnv = "HELIX_SUBSCRIPTION_SCHEMA"

// TestSubscription_DatasetInfoMatchesSchemaFile is the category-6 contract
// test: the reflected DatasetInfo keys must equal
// subscription.schema.json#properties.dataset_info.properties, dataset_info
// must be optional on Subscription, and no key inside it may be required.
// When datasetInfoSchemaEnv is unset it logs that it did not run the
// comparison; when set to an unreadable or dataset_info-less file it FAILS.
func TestSubscription_DatasetInfoMatchesSchemaFile(t *testing.T) {
	path := os.Getenv(datasetInfoSchemaEnv)
	if path == "" {
		t.Logf("%s not set: schema-file comparison NOT run (in-repo pinned key set only)", datasetInfoSchemaEnv)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s=%s: %v", datasetInfoSchemaEnv, path, err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type       any                       `json:"type"`
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	di, ok := schema.Properties["dataset_info"]
	if !ok {
		t.Fatalf("%s has no properties.dataset_info", path)
	}
	if di.Type != "object" {
		t.Errorf("schema dataset_info.type = %v, want object", di.Type)
	}
	for _, r := range schema.Required {
		if r == "dataset_info" {
			t.Error("schema lists dataset_info as required; it is optional")
		}
	}
	if len(di.Required) != 0 {
		t.Errorf("schema dataset_info.required = %v, want none (all five keys optional)", di.Required)
	}
	schemaKeys := make([]string, 0, len(di.Properties))
	for k := range di.Properties {
		schemaKeys = append(schemaKeys, k)
	}
	sort.Strings(schemaKeys)
	if !reflect.DeepEqual(schemaKeys, datasetInfoContractKeys) {
		t.Errorf("schema dataset_info keys = %v, want exactly %v", schemaKeys, datasetInfoContractKeys)
	}

	// The Go type is int64 for the two counters, so the schema must declare
	// them integer: a "number" schema would let the API emit 1.5, which this
	// SDK rejects.
	for _, k := range []string{"record_count", "size_bytes"} {
		if !schemaTypeIncludes(di.Properties[k]["type"], "integer") {
			t.Errorf("schema dataset_info.%s.type = %v, want integer", k, di.Properties[k]["type"])
		}
	}
	typ := reflect.TypeOf(DatasetInfo{})
	var goKeys []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		goKeys = append(goKeys, name)
	}
	sort.Strings(goKeys)
	if !reflect.DeepEqual(goKeys, schemaKeys) {
		t.Errorf("DatasetInfo json tags = %v, schema dataset_info keys = %v; must be equal", goKeys, schemaKeys)
	}
}

func schemaTypeIncludes(typ any, want string) bool {
	switch v := typ.(type) {
	case string:
		return v == want
	case []any:
		for _, e := range v {
			if e == want {
				return true
			}
		}
	}
	return false
}
