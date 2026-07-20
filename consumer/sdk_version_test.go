package consumer

import (
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// TestResolveSDKVersion drives the REAL resolveSDKVersion function (no
// reimplementation) against synthetic debug.BuildInfo values covering
// every path: build info unavailable, this module as the main module
// (tagged and "(devel)"), this module as a dependency (tagged,
// pseudo-versioned, and "(devel)" via a replace directive), and this
// module missing entirely from a consumer binary's dependency list.
func TestResolveSDKVersion(t *testing.T) {
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
			want: SDKVersion,
		},
		{
			name: "nil info falls back to const even if ok is true",
			info: nil,
			ok:   true,
			want: SDKVersion,
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
			want: SDKVersion,
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
			// Empirically verified shape (see the replace-directive
			// experiment in the PR description): a `replace X => ../local`
			// directive does NOT set dep.Version to "(devel)" directly —
			// dep.Version keeps the NOMINAL required version ("v2.7.0"
			// here) and dep.Replace holds the module actually compiled in,
			// whose Version is "(devel)" for a local filesystem path. A
			// naive read of dep.Version alone would wrongly report the
			// nominal version as if it were real.
			name: "consumer binary depends on this module via a local-path replace directive falls back to const",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "(devel)"},
				Deps: []*debug.Module{
					{
						Path:    modulePath,
						Version: "v2.7.0", // nominal — NOT what's actually running
						Replace: &debug.Module{Path: "../local-sdk-copy", Version: "(devel)"},
					},
				},
			},
			ok:   true,
			want: SDKVersion,
		},
		{
			// A version-pinning replace (`replace X => X v2.6.0`, no path
			// change) DOES have a real version on the replace target — that
			// real, actually-compiled-in version must win over the nominal
			// required version.
			name: "consumer binary depends on this module via a version-pinning replace directive uses the replaced version",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/some/consumer-app", Version: "v1.0.0"},
				Deps: []*debug.Module{
					{
						Path:    modulePath,
						Version: "v2.7.0", // nominal
						Replace: &debug.Module{Path: modulePath, Version: "v2.6.0"}, // actually compiled in
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
			want: SDKVersion,
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
			got := resolveSDKVersion(tt.info, tt.ok)
			if got != tt.want {
				t.Errorf("resolveSDKVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEffectiveSDKVersion_FallsBackInDevBuild exercises the REAL
// runtime/debug.ReadBuildInfo() path end-to-end (no faking). Running
// under `go test`, this module IS the main module being built, and the
// Go toolchain reports "(devel)" for a module under test (there is no
// tagged version to report) — so effectiveSDKVersion must fall back to
// the SDKVersion constant. This also pins the modulePath constant
// against reality: if go.mod's module directive ever changes without
// updating modulePath, info.Main.Path stops matching and this test
// fails for the right reason.
func TestEffectiveSDKVersion_FallsBackInDevBuild(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("debug.ReadBuildInfo() ok=false; expected true when running under `go test`")
	}
	if info.Main.Path != modulePath {
		t.Fatalf("info.Main.Path = %q, want %q — modulePath const has drifted from go.mod's module directive", info.Main.Path, modulePath)
	}

	got := effectiveSDKVersion()
	if got != SDKVersion {
		t.Errorf("effectiveSDKVersion() = %q, want fallback %q", got, SDKVersion)
	}
}

// TestSDKVersionConst_MajorMatchesModulePath guards against the
// fallback constant silently drifting onto the wrong MAJOR version
// relative to the module path's declared major (the "/v2" suffix) — for
// example if go.mod is ever bumped to /v3 without updating SDKVersion,
// or vice versa.
func TestSDKVersionConst_MajorMatchesModulePath(t *testing.T) {
	const wantMajorSuffix = "/v2" // keep in lockstep with go.mod's module directive
	if !strings.HasSuffix(modulePath, wantMajorSuffix) {
		t.Fatalf("modulePath %q does not end in %q — update this test alongside go.mod's module directive", modulePath, wantMajorSuffix)
	}
	wantMajor := strings.TrimPrefix(wantMajorSuffix, "/v")

	gotMajor, _, ok := strings.Cut(SDKVersion, ".")
	if !ok {
		t.Fatalf("SDKVersion %q is not a dotted semver-shaped string", SDKVersion)
	}
	if gotMajor != wantMajor {
		t.Errorf("SDKVersion %q has major %q, want %q to match module path %q", SDKVersion, gotMajor, wantMajor, modulePath)
	}
}

// sdkVersionCallSiteRe matches the DownloadDataset outcome-callback field
// assignment that must call effectiveSDKVersion() rather than read the
// bare SDKVersion constant. Whitespace-tolerant so a gofmt pass can't
// spuriously break it.
var sdkVersionCallSiteRe = regexp.MustCompile(`SDKVersion:\s*effectiveSDKVersion\(\)`)

// TestDownloadDataset_CallsEffectiveSDKVersion is a STATIC wiring check,
// not a dynamic/black-box one: DownloadDataset's outcome callback must
// call effectiveSDKVersion(), not the bare SDKVersion constant.
//
// This regression class is structurally impossible to catch with a
// black-box test run from within this repo: inside this SDK's own test
// binary, effectiveSDKVersion() ALWAYS resolves to the very same
// fallback value as the bare SDKVersion constant (this module is the
// "(devel)" main module under test — see
// TestEffectiveSDKVersion_FallsBackInDevBuild). So a regression back to
// `SDKVersion: SDKVersion` at the call site would produce a byte-
// identical wire payload and pass every dynamic test in
// download_outcome_callback_test.go, including
// TestDownloadOutcome_SuccessCallback's sdk_version assertion. Only a
// source-shape check can catch it here; the real-world divergence (a
// consumer binary built against a tagged release) is exercised by
// TestResolveSDKVersion's synthetic-BuildInfo cases instead.
func TestDownloadDataset_CallsEffectiveSDKVersion(t *testing.T) {
	src, err := os.ReadFile("consumer.go")
	if err != nil {
		t.Fatalf("reading consumer.go: %v", err)
	}
	if !sdkVersionCallSiteRe.Match(src) {
		t.Error("consumer.go: DownloadDataset's outcome callback must set " +
			"`SDKVersion: effectiveSDKVersion()`, not the bare SDKVersion " +
			"constant — see sdk_version.go")
	}
}
