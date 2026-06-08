package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRateLimitConfig_PointerBucketStatesRoundTrip proves that the three
// wire states the server distinguishes survive a full JSON marshal →
// unmarshal cycle, which is only possible because the buckets are
// pointers (DRIFT-GOSDK-RATELIMIT-1):
//
//   - nil            → unlimited; the field must be OMITTED on the wire
//     and decode back to nil.
//   - present {rpm:0} → blocked; the field must be present and decode
//     back to a non-nil bucket with RPM == 0 — distinct from nil.
//   - present {rpm>0} → limited; the field must be present and decode
//     back to a non-nil bucket carrying the rate.
//
// With value (non-pointer) buckets, omitempty is a no-op on a struct, so
// nil and present-zero would both serialize as {"rpm":0} and the
// unlimited-vs-blocked distinction would be lost.
func TestRateLimitConfig_PointerBucketStatesRoundTrip(t *testing.T) {
	cfg := RateLimitConfig{
		Read:   nil,                                   // unlimited
		Write:  &RateLimitBucket{RPM: 0},              // blocked
		Delete: &RateLimitBucket{RPM: 120, Burst: 30}, // limited
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	// nil bucket must be omitted entirely (unlimited != present-zero).
	if strings.Contains(got, `"read"`) {
		t.Errorf("nil Read bucket leaked onto the wire: %s", got)
	}
	// blocked + limited buckets must be present.
	if !strings.Contains(got, `"write":{"rpm":0}`) {
		t.Errorf("blocked Write bucket not serialized as present {rpm:0}: %s", got)
	}
	if !strings.Contains(got, `"delete":{"rpm":120,"burst":30}`) {
		t.Errorf("limited Delete bucket mis-serialized: %s", got)
	}

	var back RateLimitConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// unlimited stays nil.
	if back.Read != nil {
		t.Errorf("Read: got %+v, want nil (unlimited)", back.Read)
	}
	// blocked decodes to a NON-nil pointer with RPM 0 — the whole point
	// of the pointer: present-zero is distinguishable from absent.
	if back.Write == nil {
		t.Fatal("Write: got nil, want present {rpm:0} (blocked) — nil-vs-zero distinction lost")
	}
	if back.Write.RPM != 0 {
		t.Errorf("Write.RPM: got %d, want 0 (blocked)", back.Write.RPM)
	}
	// limited round-trips its rate and burst.
	if back.Delete == nil {
		t.Fatal("Delete: got nil, want present (limited)")
	}
	if back.Delete.RPM != 120 || back.Delete.Burst != 30 {
		t.Errorf("Delete: got %+v, want {RPM:120 Burst:30}", *back.Delete)
	}
}

// TestRateLimitConfig_AbsentFieldsDecodeToNil confirms the inbound
// direction: a server payload that omits a bucket (the unlimited case)
// must decode to a nil pointer, never a zero-value bucket that would be
// misread as "blocked".
func TestRateLimitConfig_AbsentFieldsDecodeToNil(t *testing.T) {
	var cfg RateLimitConfig
	if err := json.Unmarshal([]byte(`{"write":{"rpm":5}}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Read != nil {
		t.Errorf("absent Read decoded to %+v, want nil", cfg.Read)
	}
	if cfg.Delete != nil {
		t.Errorf("absent Delete decoded to %+v, want nil", cfg.Delete)
	}
	if cfg.Write == nil || cfg.Write.RPM != 5 {
		t.Errorf("Write: got %+v, want {RPM:5}", cfg.Write)
	}
}
