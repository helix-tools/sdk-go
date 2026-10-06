package reposafety

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	sdkVersionConstPattern      = regexp.MustCompile(`(?m)^const SDKVersion = "([^"]+)"$`)
	fallbackVersionConstPattern = regexp.MustCompile(`(?m)^const fallbackVersion = "([^"]+)"$`)
	changelogHeadingPattern     = regexp.MustCompile(`^## \d{4}-\d{2}-\d{2} \(v(\d+\.\d+\.\d+)\)$`)
)

// TestFallbackVersionsMatchChangelog fails when consumer.SDKVersion
// (consumer/consumer.go), useragent.fallbackVersion
// (internal/useragent/useragent.go) and the newest RELEASED version
// heading in CHANGELOG.md disagree. All three are hand-maintained and
// nothing else keeps them in lockstep, so a release that bumps one and
// forgets the other(s) would silently ship a stale fallback: both
// constants are what a dev build reports (this SDK's own test suite, or a
// consumer using a `replace` directive to a local checkout — see
// resolveSDKVersion/resolveVersion, the only callers of either constant).
func TestFallbackVersionsMatchChangelog(t *testing.T) {
	root := repoRoot(t)

	sdkVersion := extractConst(t, filepath.Join(root, "consumer", "consumer.go"), sdkVersionConstPattern, "SDKVersion")
	fallbackVersion := extractConst(t, filepath.Join(root, "internal", "useragent", "useragent.go"), fallbackVersionConstPattern, "fallbackVersion")

	changelogPath := filepath.Join(root, "CHANGELOG.md")
	data, err := os.ReadFile(changelogPath)
	if err != nil {
		t.Fatalf("reading %s: %v", changelogPath, err)
	}
	changelogVersion, err := newestChangelogVersion(string(data))
	if err != nil {
		t.Fatalf("%s: %v", changelogPath, err)
	}

	if sdkVersion != fallbackVersion {
		t.Errorf("consumer.SDKVersion (%q) and useragent.fallbackVersion (%q) disagree; bump both together on every release", sdkVersion, fallbackVersion)
	}
	if sdkVersion != changelogVersion {
		t.Errorf("consumer.SDKVersion (%q) does not match CHANGELOG.md's newest released heading (v%s); bump SDKVersion, fallbackVersion and CHANGELOG.md together on every release", sdkVersion, changelogVersion)
	}
	if fallbackVersion != changelogVersion {
		t.Errorf("useragent.fallbackVersion (%q) does not match CHANGELOG.md's newest released heading (v%s); bump SDKVersion, fallbackVersion and CHANGELOG.md together on every release", fallbackVersion, changelogVersion)
	}
}

// extractConst extracts a single hand-maintained string constant's value
// from a Go source file by matching the file's own text, rather than
// compiling/importing the package it belongs to: this package is its own
// nested module (see go.mod) specifically so it carries no dependency on
// the published module, and importing consumer or internal/useragent here
// would create exactly that dependency.
func extractConst(t *testing.T, path string, pattern *regexp.Regexp, name string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	m := pattern.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("%s: could not find a %q declaration matching %s", path, name, pattern.String())
	}
	return m[1]
}

// newestChangelogVersion returns the version named by the first version
// heading ("## YYYY-MM-DD (vX.Y.Z)") in changelog, skipping a leading
// "## Unreleased" heading when one is present — an Unreleased section
// names no real version, so the newest heading that actually names one is
// what must agree with the fallback constants. Scanning stops at the
// first "## " line: version headings are two-hash, "### Changed" /
// "### Added" / "### Fixed" subsections are three-hash and are never
// considered, so a release version mentioned in a bullet's prose (e.g.
// "matching the Python SDK's v2.21.0 behavior") can never be picked up as
// the newest heading.
func newestChangelogVersion(changelog string) (string, error) {
	for _, line := range strings.Split(changelog, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line), "## Unreleased") {
			continue
		}
		m := changelogHeadingPattern.FindStringSubmatch(line)
		if m == nil {
			return "", fmt.Errorf("first version heading %q does not match the expected \"## YYYY-MM-DD (vX.Y.Z)\" shape", line)
		}
		return m[1], nil
	}
	return "", fmt.Errorf("no version heading found")
}

// TestNewestChangelogVersion is the bypass test for newestChangelogVersion:
// a CHANGELOG whose newest heading is "## Unreleased" must be skipped, with
// the next versioned heading used instead — the guard must not fail (or
// silently pass) just because a release-prep PR hasn't run yet.
func TestNewestChangelogVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name: "skips a leading Unreleased section and uses the next versioned heading",
			input: "# Changelog\n\n" +
				"## Unreleased\n\n### Added\n- something new\n\n" +
				"## 2026-10-04 (v2.21.0)\n\n### Fixed\n- something else\n",
			want: "2.21.0",
		},
		{
			name:  "no Unreleased section: uses the first versioned heading",
			input: "# Changelog\n\n## 2026-10-06 (v2.22.0)\n\n### Added\n- x\n\n## 2026-10-04 (v2.21.0)\n",
			want:  "2.22.0",
		},
		{
			name: "a version mentioned in prose is not mistaken for a heading",
			input: "# Changelog\n\n" +
				"## 2026-10-06 (v2.22.0)\n\n### Fixed\n" +
				"- matching the Python SDK's v2.21.0 behavior\n",
			want: "2.22.0",
		},
		{
			name:    "no version heading at all",
			input:   "# Changelog\n\n## Unreleased\n\n### Added\n- x\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newestChangelogVersion(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("newestChangelogVersion() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("newestChangelogVersion() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("newestChangelogVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}
