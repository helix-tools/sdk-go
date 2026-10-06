package reposafety

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bannedContentPatternB64 is bannedContentPattern's source, base64-encoded.
// This package is a nested module, so this file is not in the published
// module zip (TestGuardIsNotPublished pins that). It is still in the public
// git repository, so the pattern stays encoded: spelling the banned terms
// out as a literal regex would put them in plain text in the repo. The
// encoding hides the terms from a text search, not from a reader, which is
// why the file must also stay out of the published module.
const bannedContentPatternB64 = "KD9pKXJpbmdib29zdHxwaG9uZVwuY29tfGNsaWNrID91cHxkaXNjb3JkfFxiODZbMC05YS16XXs3fVxifC9Vc2Vycy9bQS1aYS16XXwvcHJpdmF0ZS90bXAvY2xhdWRlfGRtZS1wcm9kdWNlci18aGVsaXhbLV8uXSthZG1pbnxoZWxpeC1wcm9kdWNlci18aGVsaXhfc3NtX3wvaGVsaXgoLXRvb2xzKT8vW2EtejAtOSV7fSRfLV0rL2N1c3RvbWVyc1xifFNUUy1QTEFOfEMtc2RrXC5tZHxcYmNvZGV4XGJ8XGJ0aGFsZXNmc3BcYg=="

// bannedContentPattern is the exact banned-content list applied to every
// published Helix SDK artifact (this module's zip, the npm tarball, the
// PyPI wheel/sdist): a customer/project name, a chat-platform mention, an
// internal tracker task id, a maintainer's local machine path, an internal
// test-bucket naming convention, and the private admin SDK's package name
// (matching any run of one or more hyphen/underscore/dot separators
// between the two name components, in any casing — PyPI's PEP 503 name
// normalization collapses any such run to a single separator before
// comparing, so every one of those spellings resolves to the same banned
// package name and must be caught, not just the single-separator form; the
// admin SDK is never published, so pointing at its package name anywhere
// in a public artifact is a dependency-confusion risk, not just an
// internals leak), the platform's internal producer-configuration
// layout — the lookup prefix older releases read, the environment variable
// that overrode it, and the producer bucket naming convention — which no
// published artifact needs now that the API supplies that configuration,
// an internal planning-document name (checked here, not in
// bannedInternalNamesPattern, because a planning-doc reference can appear
// in a test file's doc comment just as easily as in production source —
// unlike the resource-identifier alternatives below, there is no
// legitimate reason for ANY published file, test or not, to name one), an
// internal code-review tool's name (in any surrounding prose — the tool's
// own verdict vocabulary is caught by the same bare-word match, so no
// separate alternative is needed for it), and a maintainer's personal
// source-forge handle. Case-insensitive throughout — none of these belong in
// the published tree in any casing. See bannedContentPatternB64's doc
// comment for why it's encoded,
// TestBannedContentPatternMatchesAdminPackageVariants for the separator
// spellings the admin-package alternative is proven to catch, and
// TestBannedContentPatternMatchesPlanningAndToolingRefs for the
// planning-doc/tool-name/handle alternatives.
var bannedContentPattern = regexp.MustCompile(decodePattern(bannedContentPatternB64))

// bannedInternalNamesPatternB64 is bannedInternalNamesPattern's source,
// base64-encoded for the same reason as bannedContentPatternB64.
const bannedInternalNamesPatternB64 = "KD9pKWNsb3VkWyBfLV0/d2F0Y2h8XGJyZWRpc1xifFxiZGxxXGJ8ZGVhZFsgXy1dbGV0dGVyfHNjcmF0Y2hwYWR8SEVMSVhfW0EtWjAtOV9dKl9FTkFCTEVEfEN1c3RvbWVyQmFzZWRSYXRlTGltaXR8UmVxdWlyZVByb2R1Y2VyT3JCb3RofHJhd1sgXy1dP21lc3NhZ2VbIF8tXT9kZWxpdmVyeXxhcm46YXdzOnNxczpbYS16MC05LV0qOlxkezksMTJ9Oltcdy4tXSt8c3FzXC5bYS16MC05LV0rXC5hbWF6b25hd3NcLmNvbS9cZCsvW1x3Li1dK3xcYlNOU1xifFxiUFIgI1swLTldfGhlbGl4LXRvb2xzLyhhcGl8c2RrLXNjaGVtYXN8aGVsaXgtYWRtaW4p"

// bannedInternalNamesPattern is the list of internal names that must not
// appear in the doc comments or comments of a published non-test Go or
// Markdown source. Those files ship with the module and are public on GitHub,
// so a reader of pkg.go.dev or the repository sees them. The list covers the
// message-queue, cache and dead-letter components, the observability
// service, server-side feature flags, sandbox paths, private repository
// names, PR numbers, the rate-limit configuration, the internal middleware
// name, the internal subscription-delivery setting
// name (`raw_message_delivery` — an internal config knob, not a capability
// description, so it is banned the same way the feature flags are, while
// the capability it enables, "raw, unwrapped SQS delivery", stays
// describable in customer-visible text), and an actual SQS queue resource
// name or queue URL. The customer-visible content policy (.claude/CLAUDE.md)
// allows naming SQS as the consumer notification mechanism, but a specific
// queue name/URL/resource name is a banned AWS resource pattern; matching on
// the resource-name/URL structure (not the word "SQS") keeps the generic
// mechanism mention (e.g. "SQS queue", the `SQSQueueURL` field) allowed.
// This scan is non-test sources only, deliberately: unit tests legitimately
// construct synthetic/placeholder resource identifiers to exercise
// error-message parsing (see
// consumer/error_cause_test.go), and those are not a published-content leak.
// Matching is case-insensitive and tolerates a space, underscore or hyphen
// in place of the separator in the multi-word names. See
// TestBannedInternalNamesPatternMatches for the spellings it must catch and
// the public names it must leave alone.
var bannedInternalNamesPattern = regexp.MustCompile(decodePattern(bannedInternalNamesPatternB64))

func decodePattern(encoded string) string {
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("published_content: encoded pattern does not decode: " + err.Error())
	}
	return string(b)
}

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
	"CLAUDE.md":    true,
}

// publishedFiles returns exactly the file set this module publishes: for
// a Go module, `go get`/the module proxy build the module zip from every
// git-tracked file in the repo (there is no separate "files" allowlist
// the way npm or a Python sdist has) except files under a subdirectory
// that has its own go.mod (a nested module, such as this guard package and
// .claude/). So `git ls-files` minus nested modules is the authoritative
// source of truth for "what ships." It skips (via t.Skip)
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

	var tracked []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			tracked = append(tracked, line)
		}
	}
	files := excludeNestedModules(tracked)
	if len(files) == 0 {
		t.Fatal("published_content: git ls-files returned no files — refusing to run a guard that would trivially pass")
	}
	return files
}

// excludeNestedModules drops every file under a directory that has its own
// go.mod (other than the repo root's), mirroring the module zip rule that
// such a directory is a separate module and is never published with this
// one. Paths are git's slash-separated form.
func excludeNestedModules(tracked []string) []string {
	nested := map[string]bool{}
	for _, f := range tracked {
		if path.Base(f) == "go.mod" && f != "go.mod" {
			nested[path.Dir(f)] = true
		}
	}

	var published []string
	for _, f := range tracked {
		inNested := false
		for dir := path.Dir(f); dir != "."; dir = path.Dir(dir) {
			if nested[dir] {
				inNested = true
				break
			}
		}
		if !inNested {
			published = append(published, f)
		}
	}
	return published
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

// TestBannedContentPatternMatchesAdminPackageVariants is a regression test
// for the admin-package alternative in bannedContentPatternB64: it proves
// the decoded pattern actually matches every separator spelling PyPI's PEP
// 503 name normalization resolves to the same banned package name (a run
// of one or more `-`/`_`/`.`, any casing), so a future edit to the encoded
// pattern that silently drops or narrows this alternative fails a test
// instead of only being noticed by accident. The variant strings are built
// here from separate literals joined at runtime — never a contiguous
// banned substring in this file's own source — so this test doesn't trip
// the very guard (TestNoBannedContentInPublishedFiles) it exercises.
func TestBannedContentPatternMatchesAdminPackageVariants(t *testing.T) {
	prefix, suffix := "helix", "admin"
	separators := []string{"-", "_", ".", "__", "--", "._", "...", "-_"}

	for _, sep := range separators {
		variant := prefix + sep + suffix
		if !bannedContentPattern.MatchString(variant) {
			t.Errorf("bannedContentPattern does not match %q, a PEP-503-equivalent spelling of the private admin package name", variant)
		}
	}

	if unrelated := prefix + suffix; bannedContentPattern.MatchString(unrelated) {
		t.Errorf("bannedContentPattern unexpectedly matched %q (no separator) — it should require a separator between %q and %q, not flag an unrelated word", unrelated, prefix, suffix)
	}
}

// TestBannedContentPatternMatchesProducerConfigLayout is a regression test
// for the producer-configuration alternatives in bannedContentPatternB64: the
// old lookup prefix (literal environment segment or a format verb), the
// override variable, and the bucket naming convention must each be caught,
// while an ordinary module import path that shares the organization name is
// not. Every sample is assembled at runtime from separate fragments, so no
// banned string appears contiguously in this file.
func TestBannedContentPatternMatchesProducerConfigLayout(t *testing.T) {
	org := "helix" + "-tools"
	mustMatch := []string{
		"/" + org + "/production/" + "customers/c-1/kms_key_id",
		"/" + org + "/%s/" + "customers",
		"/helix/production/" + "customers/c-1/aws_access_key_id",
		"HELIX" + "_SSM_" + "CUSTOMER_PREFIX",
		"helix" + "-producer-" + "company-123-production",
	}
	for _, s := range mustMatch {
		if !bannedContentPattern.MatchString(s) {
			t.Errorf("bannedContentPattern does not match %q, a piece of the internal producer-configuration layout", s)
		}
	}

	mustNotMatch := []string{
		"github.com/" + org + "/sdk-go/v2/producer",
		"/v1/self/producer-config",
		"example-bucket-company-123",
	}
	for _, s := range mustNotMatch {
		if bannedContentPattern.MatchString(s) {
			t.Errorf("bannedContentPattern unexpectedly matched %q, which is public and must stay allowed", s)
		}
	}
}

// TestBannedContentPatternMatchesPlanningAndToolingRefs is a regression test
// for the planning-document, code-review-tool, and maintainer-handle
// alternatives in bannedContentPatternB64. Unlike the resource-identifier
// alternatives above, these three are banned in EVERY published file,
// test or not — a test's doc comment naming an internal planning document
// or reviewer is exactly as much of a leak as production source doing the
// same, so TestNoBannedContentInPublishedFiles (which scans the whole
// published tree, not just non-test Go/Markdown) is what enforces them.
// Every sample is assembled at runtime from fragments, so no banned string
// appears contiguously in this file.
func TestBannedContentPatternMatchesPlanningAndToolingRefs(t *testing.T) {
	mustMatch := []string{
		"S" + "TS-" + "PLAN.md §9",
		"C-" + "sdk.md C.1",
		"co" + "dex",
		"Co" + "dex-REFUTE finding",
		"caught by " + "co" + "dex" + " 2026-07-06",
		"github.com/" + "thales" + "fsp/sypl",
		"TODO: Use " + "thales" + "fsp/sypl logger",
	}
	for _, s := range mustMatch {
		if !bannedContentPattern.MatchString(s) {
			t.Errorf("bannedContentPattern does not match %q, a planning-doc/tool-name/handle reference", s)
		}
	}

	mustNotMatch := []string{
		"video " + "codecs",
		"the design document, §9",
		"an independent adversarial review caught it",
	}
	for _, s := range mustNotMatch {
		if bannedContentPattern.MatchString(s) {
			t.Errorf("bannedContentPattern unexpectedly matched %q, which is public and must stay allowed", s)
		}
	}
}

// TestBannedInternalNamesPatternMatchesSQSResourcePatterns is a regression
// test for the SQS queue-identifier/queue-URL alternatives in
// bannedInternalNamesPatternB64: an actual queue identifier or queue URL in a
// published non-test source must be caught, while the generic "SQS" word
// and the SDK's own SQSQueueURL field/import path — allowed by the
// customer-visible content policy (.claude/CLAUDE.md) as the consumer
// notification mechanism — must stay allowed. Every sample is assembled at
// runtime from fragments, so no banned string appears contiguously in this
// file.
func TestBannedInternalNamesPatternMatchesSQSResourcePatterns(t *testing.T) {
	region, account, queue := "us-east-1", "1234"+"56789012", "helix-notifications-queue"
	mustMatch := []string{
		"ar" + "n:aws:sqs:" + region + ":" + account + ":" + queue,
		"https://sqs." + region + "." + "amazon" + "aws.com/" + account + "/" + queue,
		"sqs." + region + "." + "amazon" + "aws.com/" + account + "/" + queue + ".fifo",
	}
	for _, s := range mustMatch {
		if !bannedInternalNamesPattern.MatchString(s) {
			t.Errorf("bannedInternalNamesPattern does not match %q, an SQS queue identifier/URL", s)
		}
	}

	mustNotMatch := []string{
		"SQS" + " queue",
		"the consumer notification mechanism is " + "SQS",
		"SQS" + "QueueURL",
		"github.com/aws/aws-sdk-go-v2/service/" + "sqs",
		"sqs" + ".NewFromConfig(awsCfg)",
	}
	for _, s := range mustNotMatch {
		if bannedInternalNamesPattern.MatchString(s) {
			t.Errorf("bannedInternalNamesPattern unexpectedly matched %q, which is public and must stay allowed", s)
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
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("published_content: reading %s: %v", f, err)
		}
		text := string(data)

		// Match against the whole file rather than splitting into lines
		// first and matching each with FindString: FindString only returns
		// the first hit per line, so a line-by-line scan under-reports a
		// line with more than one banned term. FindAllStringIndex over the
		// whole file reports every hit; it still cannot match a term split
		// across an actual newline (no pattern here spans "\n").
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

// TestBannedInternalNamesPatternMatches is a regression test for
// bannedInternalNamesPatternB64. Each internal name must be caught, including
// the spacing and separator variants a rename would use, and an ordinary public
// name that shares a fragment with one of them must not be. Every sample is
// assembled at runtime from fragments, so no banned string appears
// contiguously in this file.
func TestBannedInternalNamesPatternMatches(t *testing.T) {
	mustMatch := []string{
		"Cloud" + "Watch FilterByAgent reader",
		"cloud" + " " + "watch reader",
		"CLOUD" + "_" + "WATCH",
		"Re" + "dis-backed idempotency store",
		"eventual D" + "LQ",
		"dead" + "-letter handling",
		"dead" + " letter",
		"scratch" + "pad/briefs/mint-error-contract.md",
		"HELIX" + "_MODEL2_" + "ENABLED",
		"helix" + "_agents_" + "enabled",
		"Customer" + "BasedRateLimit(0.33 req/s, burst 5)",
		"Require" + "ProducerOrBoth",
		"raw" + "_message_delivery",
		"raw" + "-message-delivery",
		"raw" + " message " + "delivery",
		"RAW" + "_MESSAGE_" + "DELIVERY",
		"S" + "NS-wrapped messages",
		"PR" + " #129",
		"helix-" + "tools/api PR",
		"helix-" + "tools/sdk-schemas",
	}
	for _, s := range mustMatch {
		if !bannedInternalNamesPattern.MatchString(s) {
			t.Errorf("bannedInternalNamesPattern does not match %q", s)
		}
	}

	mustNotMatch := []string{
		"HELIX_API_ENDPOINT",
		"RateLimitBucket",
		"RequireProducerCredentials",
		"Redistribute the result",
		"SQS queue",
		"a raw " + "message was received",
		"raw notification " + "payload",
		"message " + "delivery guarantees",
		"github.com/" + "helix-tools/sdk-go/v2/producer",
	}
	for _, s := range mustNotMatch {
		if bannedInternalNamesPattern.MatchString(s) {
			t.Errorf("bannedInternalNamesPattern unexpectedly matched %q, which is public and must stay allowed", s)
		}
	}
}

// TestNoBannedInternalNamesInSources fails if a published non-test Go or
// Markdown file names an internal component, flag, document or repository.
// Those files ship with the module and are public on GitHub.
func TestNoBannedInternalNamesInSources(t *testing.T) {
	root := repoRoot(t)
	files := publishedFiles(t, root)

	scanned := 0
	for _, f := range files {
		if !isNonTestGoOrMarkdown(f) {
			continue
		}
		scanned++

		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("published_content: reading %s: %v", f, err)
		}
		text := string(data)

		for _, loc := range bannedInternalNamesPattern.FindAllStringIndex(text, -1) {
			lineNo, line := lineContaining(text, loc[0])
			t.Errorf("%s:%d: internal name %q in a published source: %q", f, lineNo, text[loc[0]:loc[1]], strings.TrimSpace(line))
		}
	}

	if scanned == 0 {
		t.Fatal("published_content: no non-test Go or Markdown files found — refusing to run a guard that would trivially pass")
	}
}

// isNonTestGoOrMarkdown reports whether f is a non-test Go source or a Markdown
// document, the file types TestNoBannedInternalNamesInSources scans.
func isNonTestGoOrMarkdown(f string) bool {
	if strings.HasSuffix(f, ".md") {
		return true
	}
	return strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go")
}

// TestExcludeNestedModules pins the module zip rule publishedFiles relies
// on: files under a directory with its own go.mod are dropped, at any depth,
// and nothing else is.
func TestExcludeNestedModules(t *testing.T) {
	tracked := []string{
		"go.mod",
		"README.md",
		"producer/producer.go",
		".claude/go.mod",
		".claude/CLAUDE.md",
		"internal/reposafety/go.mod",
		"internal/reposafety/deep/x_test.go",
		"internal/reposafetyish/kept.go", // a sibling sharing a name prefix
		"internal/sdkerr/sdkerr.go",
	}
	got := strings.Join(excludeNestedModules(tracked), ",")
	want := "go.mod,README.md,producer/producer.go,internal/reposafetyish/kept.go,internal/sdkerr/sdkerr.go"
	if got != want {
		t.Errorf("excludeNestedModules = %s\nwant %s", got, want)
	}
}

// TestGuardIsNotPublished fails if this guard's own files, or the agent
// instruction file, would ship in the published module — e.g. because a
// nested go.mod was deleted.
func TestGuardIsNotPublished(t *testing.T) {
	root := repoRoot(t)
	files := publishedFiles(t, root)

	for _, f := range files {
		if strings.HasPrefix(f, "internal/reposafety/") || path.Base(f) == "CLAUDE.md" {
			t.Errorf("%s is in the published module; it must stay in a nested module", f)
		}
	}
}
