package consumer

import (
	"runtime/debug"
	"strings"
)

// modulePath is this SDK's module path, including the major-version
// suffix. It MUST match go.mod's module directive — resolveSDKVersion
// uses it to find this module's own resolved version among a build's
// dependency list, and TestSDKVersionConst_MajorMatchesModulePath pins
// its major-version suffix against the SDKVersion constant so the two
// can never silently disagree.
const modulePath = "github.com/helix-tools/sdk-go/v2"

// effectiveSDKVersion is the SDK version actually reported in the
// download-outcome telemetry callback (see SDKVersion's doc comment for
// why this indirection exists). It wires resolveSDKVersion to the real
// running binary's build info.
func effectiveSDKVersion() string {
	info, ok := debug.ReadBuildInfo()
	return resolveSDKVersion(info, ok)
}

// resolveSDKVersion picks the SDK version to report, given a binary's
// build info (as returned by runtime/debug.ReadBuildInfo).
//
// It prefers the REAL version this binary was built against: the Go
// toolchain stamps every binary with the exact module version (or
// pseudo-version) each dependency resolved to in the consumer's
// go.mod/go.sum. That value can never drift from reality the way a
// hand-maintained constant can — it is what the Go tooling itself
// resolved, not what an SDK maintainer last remembered to bump.
//
// It falls back to the SDKVersion constant when build info is
// unavailable, or reports a non-versioned build ("(devel)") for this
// module specifically — which happens when running this SDK's own test
// suite (this module IS the main module under test, and a module under
// test has no tagged version to report) or when a consumer depends on
// this SDK via a `replace` directive to a local checkout.
func resolveSDKVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return SDKVersion
	}

	if info.Main.Path == modulePath {
		if v, real := realVersion(info.Main.Version); real {
			return v
		}
	}

	for _, dep := range info.Deps {
		if dep == nil || dep.Path != modulePath {
			continue
		}

		// A `replace` directive (go.mod `replace ... => ...`) means
		// dep.Version is only the NOMINAL required version — NOT what's
		// actually compiled into the binary. dep.Replace describes the
		// real thing: either a different real tagged/pseudo version, or
		// (for a local filesystem replace) "(devel)", since there is no
		// tagged version for arbitrary local content. Prefer it whenever
		// present; deliberately do NOT fall through to the misleading
		// nominal dep.Version if the replace target has no real version.
		if dep.Replace != nil {
			if v, real := realVersion(dep.Replace.Version); real {
				return v
			}
			return SDKVersion
		}

		if v, real := realVersion(dep.Version); real {
			return v
		}
	}

	return SDKVersion
}

// realVersion reports whether v is an actual resolved module version —
// not empty, and not the Go toolchain's "(devel)" sentinel for
// un-tagged/local builds — returning it with any leading "v" stripped
// so it matches the existing (non-"v"-prefixed, e.g. "2.7.0") sdk_version
// wire format the producer dashboard already groups by.
func realVersion(v string) (string, bool) {
	if v == "" || v == "(devel)" {
		return "", false
	}
	return strings.TrimPrefix(v, "v"), true
}
