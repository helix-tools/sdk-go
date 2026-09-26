// Package useragent builds the User-Agent header value sent with every
// Helix API request, so server-side logs and the helix-api request
// parser (which reads the first token as "helix-sdk-<lang>/<semver>")
// can attribute traffic to this SDK build.
package useragent

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// modulePath is this SDK's module path, including the major-version
// suffix. It MUST match go.mod's module directive — resolveVersion
// uses it to find this module's own resolved version among a build's
// dependency list. Mirrors consumer.modulePath (see that package's
// sdk_version.go for the full rationale); duplicated here rather than
// imported so this package stays a leaf internal/ dependency instead
// of coupling to the consumer package's telemetry-only surface.
const modulePath = "github.com/helix-tools/sdk-go/v2"

// fallbackVersion is reported when the real build version can't be
// resolved at runtime — an unavailable build info, running this SDK's
// own test suite, or a consumer using a `replace` directive to a local
// checkout. Kept in lockstep with the latest module version tag as a
// best-effort default, mirroring consumer.SDKVersion.
const fallbackVersion = "2.17.0"

// disallowedVersionChars matches any run of characters that can't
// appear in the value's version segment per the wire contract
// (helix-api's parser expects "helix-sdk-go/<version>" to match
// ^helix-sdk-go/\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(...)?$). A version
// string containing anything else (e.g. a "+build" metadata suffix
// some toolchains append) is normalised rather than sent verbatim, so
// the wire value always matches that contract.
var disallowedVersionChars = regexp.MustCompile(`[^0-9A-Za-z.-]+`)

var (
	once  sync.Once
	value string
)

// String returns the User-Agent header value to send on every Helix
// API request: "helix-sdk-go/<version> (go/<runtime>)". It is computed
// once per process (debug.ReadBuildInfo is not free) and cached.
func String() string {
	once.Do(func() {
		info, ok := debug.ReadBuildInfo()
		value = format(resolveVersion(info, ok))
	})
	return value
}

// format renders the final header value from an already-resolved,
// "v"-stripped version string.
func format(version string) string {
	return "helix-sdk-go/" + sanitizeVersion(version) + " (go/" + runtime.Version() + ")"
}

// sanitizeVersion strips anything from version that the wire contract's
// regex can't match, replacing each disallowed run with a single "-" so
// a decorated version (e.g. "2.15.0-dirty+abc") still produces a value
// matching ^helix-sdk-go/\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?...$ instead of
// silently breaking the parser's expected shape.
func sanitizeVersion(version string) string {
	return disallowedVersionChars.ReplaceAllString(version, "-")
}

// resolveVersion picks the SDK version to report, given a binary's
// build info (as returned by runtime/debug.ReadBuildInfo). It mirrors
// consumer.resolveSDKVersion exactly (see that function's doc comment
// for the full reasoning: prefer the real resolved module version off
// the running binary's build info, honoring `replace` directives, and
// fall back to fallbackVersion when unavailable).
func resolveVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return fallbackVersion
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

		if dep.Replace != nil {
			if v, real := realVersion(dep.Replace.Version); real {
				return v
			}
			return fallbackVersion
		}

		if v, real := realVersion(dep.Version); real {
			return v
		}
	}

	return fallbackVersion
}

// realVersion reports whether v is an actual resolved module version —
// not empty, and not the Go toolchain's "(devel)" sentinel for
// un-tagged/local builds — returning it with any leading "v" stripped.
func realVersion(v string) (string, bool) {
	if v == "" || v == "(devel)" {
		return "", false
	}
	return strings.TrimPrefix(v, "v"), true
}
