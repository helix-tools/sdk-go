package reposafety

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// selfPath is this file's own path relative to the repo root, as
// git ls-files reports it. It is excluded from the content scan below:
// the regex necessarily spells out the literal substrings it looks for
// (a project name, a platform name, a path prefix), and declaring a
// filter is not the same thing as leaking the content that filter
// exists to catch.
const selfPath = "internal/reposafety/published_content_test.go"

// bannedContentPattern is the exact banned-content list applied to every
// published Helix SDK artifact (this module's zip, the npm tarball, the
// PyPI wheel/sdist): a customer/project name, a chat-platform mention, an
// internal tracker task id, a maintainer's local machine path, and an
// internal test-bucket naming convention. Case-insensitive throughout —
// none of these belong in the published tree in any casing.
var bannedContentPattern = regexp.MustCompile(`(?i)ringboost|phone\.com|click ?up|discord|\b86[0-9a-z]{7}\b|/Users/[A-Za-z]|/private/tmp/claude|dme-producer-`)

// bannedFilenames must never appear anywhere in the published file tree —
// not only at the repo root. forbiddenRootFiles (reposafety_test.go)
// already guards the root for a wider, repo-specific set of one-off
// internal dumps; this list is the standing cross-SDK set named by the
// release policy and is checked at every directory depth.
var bannedFilenames = map[string]bool{
	"SOUL.md":      true,
	"IDENTITY.md":  true,
	"USER.md":      true,
	"HEARTBEAT.md": true,
	"TOOLS.md":     true,
	"memory.md":    true,
	"PLAN.md":      true,
	"AGENTS.md":    true,
}

// publishedFiles returns exactly the file set this module publishes: for
// a Go module, `go get`/the module proxy build the module zip from every
// git-tracked file in the repo (there is no separate "files" allowlist
// the way npm or a Python sdist has), so `git ls-files` is the
// authoritative source of truth for "what ships." It skips (via t.Skip)
// when run outside a VCS checkout of this repo — e.g. against an
// extracted module-cache copy with no .git metadata — since this guard's
// job is this repo's own release hygiene, not something every downstream
// consumer's `go test ./...` needs to satisfy.
func publishedFiles(t *testing.T, root string) []string {
	t.Helper()

	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("published_content: no .git metadata found at repo root — not running from a VCS checkout, skipping the published-file-set guard")
	}

	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatalf("published_content: git ls-files failed: %v", err)
	}

	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) == 0 {
		t.Fatal("published_content: git ls-files returned no files — refusing to run a guard that would trivially pass")
	}
	return files
}

// TestNoBannedFilenamesAnywhere fails if a banned filename, or a
// docs/plans/ directory, exists anywhere in the published file tree (any
// depth, not just the repo root).
func TestNoBannedFilenamesAnywhere(t *testing.T) {
	root := repoRoot(t)
	files := publishedFiles(t, root)

	for _, f := range files {
		if bannedFilenames[filepath.Base(f)] {
			t.Errorf("%s: banned filename %q must not exist anywhere in the published file tree", f, filepath.Base(f))
		}
		if strings.Contains(f, "docs/plans/") {
			t.Errorf("%s: docs/plans/ holds internal planning documents that must not ship", f)
		}
	}
}

// TestNoBannedContentInPublishedFiles fails if any git-tracked file
// contains a banned-content hit. It scans exactly the file set
// `git ls-files` reports — the same set the Go module zip ships — so
// nothing published goes unscanned and nothing unpublished (an ignored
// scratch file) gets scanned.
func TestNoBannedContentInPublishedFiles(t *testing.T) {
	root := repoRoot(t)
	files := publishedFiles(t, root)

	for _, f := range files {
		if f == selfPath {
			continue
		}

		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("published_content: reading %s: %v", f, err)
		}
		text := string(data)

		// Match against the whole file, not line-by-line: a per-line scan
		// would miss a banned string deliberately (or accidentally, via a
		// wrapped string literal) split across a line break.
		for _, loc := range bannedContentPattern.FindAllStringIndex(text, -1) {
			lineNo, line := lineContaining(text, loc[0])
			t.Errorf("%s:%d: banned content %q found in published file: %q", f, lineNo, text[loc[0]:loc[1]], strings.TrimSpace(line))
		}
	}
}

// lineContaining returns the 1-indexed line number and the full line text
// containing byte offset idx within text.
func lineContaining(text string, idx int) (lineNo int, line string) {
	lineNo = 1 + strings.Count(text[:idx], "\n")
	start := strings.LastIndexByte(text[:idx], '\n') + 1
	end := strings.IndexByte(text[idx:], '\n')
	if end == -1 {
		return lineNo, text[start:]
	}
	return lineNo, text[start : idx+end]
}
