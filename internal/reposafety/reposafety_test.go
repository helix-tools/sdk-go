// Package reposafety guards against internal-only planning/identity files
// being reintroduced at the repo root of this public SDK repository, and
// against internal references reaching the published module.
//
// It is its own nested module (see go.mod here), so none of it is part of
// the published module zip; CI runs it as a separate step.
package reposafety

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// forbiddenRootFiles must never exist at the repo root: they were internal
// engineering notes (agent identity/memory files, implementation plans, a
// parity-analysis dump) that should never have been committed to this
// public repo. Their content stays in git history; nothing is lost by
// keeping them out of the working tree going forward.
var forbiddenRootFiles = []string{
	"SOUL.md",
	"IDENTITY.md",
	"USER.md",
	"HEARTBEAT.md",
	"TOOLS.md",
	"memory.md",
	"IMPLEMENTATION_REPORT.md",
	"UPLOAD_FLOW_CHANGES.md",
	"STS_C0_INVENTORY.md",
	"PLAN.md",
	"sdk-parity-analysis.json",
	"AGENTS.md",
	// Agent instruction file: it lives under .claude/, which has its own
	// go.mod so it is never part of the published module.
	"CLAUDE.md",
}

// repoRoot locates the published module's root by walking up from the
// parent of this nested module's directory (which has its own go.mod)
// until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("reposafety: could not determine this file's path via runtime.Caller")
	}

	dir := filepath.Dir(filepath.Dir(thisFile))
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("reposafety: walked up to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// TestNoForbiddenRootFiles fails if any internal-only file the content-policy
// cleanup removed from the public repo has reappeared at the repo root.
func TestNoForbiddenRootFiles(t *testing.T) {
	root := repoRoot(t)

	for _, name := range forbiddenRootFiles {
		name := name
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name)
			if _, err := os.Stat(path); err == nil {
				t.Errorf("%s must not exist at the repo root: it is internal-only content that must not ship in this public repo (see the customer-visible content policy in .claude/CLAUDE.md)", name)
			} else if !os.IsNotExist(err) {
				t.Fatalf("unexpected error checking %s: %v", path, err)
			}
		})
	}
}

// TestNoDocsPlansDirectory fails if docs/plans/ (internal planning documents)
// has reappeared anywhere under the repo root.
func TestNoDocsPlansDirectory(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "plans")

	if info, err := os.Stat(path); err == nil {
		t.Errorf("%s must not exist: docs/plans/ holds internal planning documents that must not ship in this public repo (see the customer-visible content policy in .claude/CLAUDE.md); found %v", path, info.Mode())
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking %s: %v", path, err)
	}
}
