package useragent

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"testing"
)

// wireFormatRe is the exact contract the api lane parses (see this
// SDK's PR description / CHANGELOG): the FIRST token of User-Agent
// must be "helix-sdk-go/<semver-without-v>", optionally followed by a
// " (go/<runtime.Version()>)" suffix.
var wireFormatRe = regexp.MustCompile(`^helix-sdk-go/\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?( \(go/go[0-9.]+(rc\d+)?\))?$`)

// TestResolveVersion drives the REAL resolveVersion function (no
// reimplementation) against synthetic debug.BuildInfo values covering
// every path, mirroring consumer.TestResolveSDKVersion exactly since
// this function is a deliberate duplicate of consumer.resolveSDKVersion
// for a leaf internal/ package that both consumer and producer can
// import without a package coupling to consumer's telemetry surface.
func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{
			name: "build info unavailable falls back to const",
			info: nil,
			ok:   false,
			want: fallbackVersion,
		},
		{
			name: "nil info falls back to const even if ok is true",
			info: nil,
			ok:   true,
			want: fallbackVersion,
		},
		{
			name: "this module as main with a real tag strips the v prefix",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: modulePath, Version: "v3.1.4"},
			},
			ok:   true,
			want: "3.1.4",
		},
		{
			name: "this module as main but devel (running this SDK's own tests) falls back to const",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: modulePath, Version: "(devel)"},
			},
			ok:   true,
			want: fallbackVersion,
		},
		{
			name: "consumer binary depends on this module at a real tag",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "(devel)"},
				Deps: []*debug.Module{
					{Path: "github.com/aws/aws-sdk-go-v2", Version: "v1.39.6"},
					{Path: modulePath, Version: "v2.9.0"},
				},
			},
			ok:   true,
			want: "2.9.0",
		},
		{
			name: "consumer binary depends on this module at a pseudo-version",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "v1.0.0"},
				Deps: []*debug.Module{
					{Path: modulePath, Version: "v2.9.1-0.20260101120000-abcdef123456"},
				},
			},
			ok:   true,
			want: "2.9.1-0.20260101120000-abcdef123456",
		},
		{
			name: "consumer binary depends on this module via a local-path replace directive falls back to const",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "(devel)"},
				Deps: []*debug.Module{
					{
						Path:    modulePath,
						Version: "v2.7.0",
						Replace: &debug.Module{Path: "../local-sdk-copy", Version: "(devel)"},
					},
				},
			},
			ok:   true,
			want: fallbackVersion,
		},
		{
			name: "consumer binary depends on this module via a version-pinning replace directive uses the replaced version",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "v1.0.0"},
				Deps: []*debug.Module{
					{
						Path:    modulePath,
						Version: "v2.7.0",
						Replace: &debug.Module{Path: modulePath, Version: "v2.6.0"},
					},
				},
			},
			ok:   true,
			want: "2.6.0",
		},
		{
			name: "consumer binary does not depend on this module at all falls back to const",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "v1.0.0"},
				Deps: []*debug.Module{
					{Path: "github.com/aws/aws-sdk-go-v2", Version: "v1.39.6"},
				},
			},
			ok:   true,
			want: fallbackVersion,
		},
		{
			name: "nil dep entries in Deps are skipped without panicking",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "v1.0.0"},
				Deps: []*debug.Module{
					nil,
					{Path: modulePath, Version: "v2.7.0"},
				},
			},
			ok:   true,
			want: "2.7.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveVersion(tt.info, tt.ok)
			if got != tt.want {
				t.Errorf("resolveVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormat_MatchesWireContract pins format()'s output shape against
// the exact regex the api lane's parser uses, for the plain, expected
// case (a clean tagged version).
func TestFormat_MatchesWireContract(t *testing.T) {
	got := format("2.15.0")
	if !wireFormatRe.MatchString(got) {
		t.Errorf("format(%q) = %q, does not match wire contract %s", "2.15.0", got, wireFormatRe.String())
	}
}

// TestFormat_BypassCheck_DecoratedVersionIsNormalised is the BYPASS
// CHECK from the dispatch brief: a version string carrying a "+build"
// metadata suffix (e.g. what some toolchains stamp onto a dirty build,
// "v2.15.0-dirty+abc") must not be sent verbatim — the "+" character
// falls outside the wire contract's allowed charset and would break
// the api lane's parser. resolveVersion is fed a fake BuildInfo
// reporting exactly that decorated tag as this module's own resolved
// version, and the final format() output must still match the wire
// contract regex, proving sanitizeVersion actually runs on the
// resolved-version path and not just in isolation.
func TestFormat_BypassCheck_DecoratedVersionIsNormalised(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: modulePath, Version: "v2.15.0-dirty+abc"},
	}

	resolved := resolveVersion(info, true)
	if resolved != "2.15.0-dirty+abc" {
		t.Fatalf("resolveVersion() = %q, want the raw decorated tag with only the leading v stripped", resolved)
	}

	got := format(resolved)
	if !wireFormatRe.MatchString(got) {
		t.Errorf("format(%q) = %q, does not match wire contract %s — decorated version was not normalised", resolved, got, wireFormatRe.String())
	}
	if want := "helix-sdk-go/2.15.0-dirty-abc (go/" + runtime.Version() + ")"; got != want {
		t.Errorf("format(%q) = %q, want %q (the '+' replaced with '-')", resolved, got, want)
	}
}

// TestString_MatchesWireContractAndIsCached exercises the REAL
// runtime/debug.ReadBuildInfo() path end-to-end (no faking) and pins
// that String() caches: two calls must return the identical value
// (computed once per process, per the dispatch brief).
func TestString_MatchesWireContractAndIsCached(t *testing.T) {
	first := String()
	if !wireFormatRe.MatchString(first) {
		t.Errorf("String() = %q, does not match wire contract %s", first, wireFormatRe.String())
	}
	second := String()
	if second != first {
		t.Errorf("String() is not cached: first call = %q, second call = %q", first, second)
	}
}
