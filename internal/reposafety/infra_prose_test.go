package reposafety

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// bannedInfraPatternB64 is bannedInfraPattern's source, base64-encoded for the
// same reason as bannedContentPatternB64: these terms are banned from shipped
// prose, so they must not appear as plain text in this public repository.
const bannedInfraPatternB64 = "KD9pOlxiKD86a21zfGFlc3xnY218c3NtfGlhbXxzM3xhcm5zP3xhbWF6b25hd3N8bGFtYmRhfGR5bmFtb2RifGNsb3VkZnJvbnR8ZG1lLXByb2R1Y2VyfG1vbmdvKD86ZGIpPylcYnxcYmJ1Y2tldCBuYW1lcz9cYil8XGIoPzpTVFN8RUNTKVxifFxiKD86QUtJQXxBU0lBKSg/OlswLTlBLVpdezE2fSk/XGJ8XGJcZHsxMn1cYg=="

// bannedInfraPattern is the customer-visible content policy's infrastructure
// list, applied to shipped prose: the comments and string-literal values of
// published non-test Go, and the full text of every published Markdown file.
// It covers encryption algorithms and key-management services, AWS resource
// names and patterns (identity-service resource names, storage identifiers,
// account numbers, access-key prefixes), the service names that reveal
// infrastructure topology, and the backend database engine. Cipher-mode names
// count as encryption algorithms.
//
// Most alternatives are case-insensitive, so a lowercase spelling in prose is
// caught too. The uppercase-only group is the exception: the lowercase
// credential-mode value is public API and must stay. Identifiers that embed a
// term, such as an error-category constant or a storage field name, never
// match, because the word boundaries do not fall inside them. SQS is absent on
// purpose: the policy allows naming it as the consumer notification mechanism,
// and queue resource names and URLs are guarded by bannedInternalNamesPattern.
var bannedInfraPattern = regexp.MustCompile(decodePattern(bannedInfraPatternB64))

// infraExactIdentifiers are the only strings that may contain a banned term in
// a published Markdown file. Each is an exact import path or code identifier,
// and is blanked before the scan, so it cannot hide a longer leak next to it.
// Go sources need no entries: their import paths and struct tags are dropped
// before the scan, and their code identifiers are not prose.
var infraExactIdentifiers = []string{
	"github.com/aws/aws-sdk-go-v2/service/" + frag("s", "3"),
}

// proseSpan is one piece of shipped prose and the file line it starts on.
type proseSpan struct {
	line int
	text string
}

// infraHit is one banned term found in shipped prose.
type infraHit struct {
	line    int
	term    string
	excerpt string
}

// goProse returns the comments and string-literal values of one Go source
// file. Import paths and struct tags are left out: they are code (package
// paths and wire field names), not prose.
func goProse(t *testing.T, path string, src []byte) []proseSpan {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("published_content: parsing %s: %v", path, err)
	}

	code := map[token.Pos]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			code[n.Path.Pos()] = true
		case *ast.Field:
			if n.Tag != nil {
				code[n.Tag.Pos()] = true
			}
		}
		return true
	})

	var spans []proseSpan
	for _, group := range file.Comments {
		for _, c := range group.List {
			spans = append(spans, proseSpan{line: fset.Position(c.Pos()).Line, text: c.Text})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || code[lit.Pos()] {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("published_content: unquoting a string literal in %s: %v", path, err)
		}
		spans = append(spans, proseSpan{line: fset.Position(lit.Pos()).Line, text: value})
		return true
	})
	return spans
}

// infraHitsIn returns every banned infrastructure term in spans, with the file
// line it sits on.
func infraHitsIn(spans []proseSpan) []infraHit {
	var hits []infraHit
	for _, s := range spans {
		text := s.text
		for _, id := range infraExactIdentifiers {
			text = strings.ReplaceAll(text, id, strings.Repeat(" ", len(id)))
		}
		for _, loc := range bannedInfraPattern.FindAllStringIndex(text, -1) {
			lineNo, line := lineContaining(text, loc[0])
			hits = append(hits, infraHit{
				line:    s.line + lineNo - 1,
				term:    text[loc[0]:loc[1]],
				excerpt: strings.TrimSpace(line),
			})
		}
	}
	return hits
}

// TestNoInfrastructureProseInSources fails if a published non-test Go source or
// Markdown file names an encryption algorithm, a key-management service, an AWS
// resource pattern or infrastructure topology in its prose. Those files ship
// with the module and are public on GitHub, so the customer-visible content
// policy applies to them. Fix the wording; do not add an identifier to
// infraExactIdentifiers unless it is an exact code name that cannot change.
func TestNoInfrastructureProseInSources(t *testing.T) {
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

		spans := []proseSpan{{line: 1, text: string(data)}}
		if strings.HasSuffix(f, ".go") {
			spans = goProse(t, f, data)
		}
		for _, h := range infraHitsIn(spans) {
			t.Errorf("%s:%d: infrastructure term %q in shipped prose: %q", f, h.line, h.term, h.excerpt)
		}
	}

	if scanned == 0 {
		t.Fatal("published_content: no non-test Go or Markdown files found — refusing to run a guard that would trivially pass")
	}
}

// frag joins fragments into one string. The banned terms in the samples below
// are split this way so that no banned term appears contiguously in this file.
func frag(parts ...string) string {
	return strings.Join(parts, "")
}

// TestInfraPatternMatchesProse is a regression test for
// bannedInfraPatternB64. Each banned shape must be caught in prose, in any
// casing that the pattern promises, and the identifiers and public names that
// share a fragment with one must not be. Every sample is assembled at runtime
// from fragments.
func TestInfraPatternMatchesProse(t *testing.T) {
	mustMatch := []string{
		"encrypted with " + frag("A", "ES-256-", "G", "CM"),
		"cipher " + frag("G", "CM") + " mode",
		"the " + frag("K", "MS") + " Decrypt call",
		"lower-case " + frag("k", "ms") + " in prose",
		"stored in an " + frag("S", "3") + " bucket",
		"read from " + frag("S", "SM") + " parameters",
		"role " + frag("I", "AM") + " policy",
		"the " + frag("S", "TS") + " session",
		"the key " + frag("A", "RN"),
		"resource " + frag("a", "rn") + ":example",
		"the " + frag("bucket ", "name"),
		"account " + frag("1234", "56789012"),
		"https://" + frag("s", "3.", "amazon", "aws.com"),
		"resource " + frag("amazon", "aws") + " host",
		"dme-" + "producer",
		"ships to " + frag("s", "3", "://example"),
		"access key " + frag("AK", "IAEXAMPLE000000000"),
		"see " + frag("Dynamo", "DB"),
		"behind " + frag("Cloud", "Front"),
		"runs on " + frag("E", "CS"),
		"a " + frag("Lamb", "da") + " function",
		"records in " + frag("Mon", "go") + "DB",
		"the " + frag("Mon", "go") + " upsert",
	}
	for _, s := range mustMatch {
		if !bannedInfraPattern.MatchString(s) {
			t.Errorf("bannedInfraPattern does not match %q", s)
		}
	}

	mustNotMatch := []string{
		"ErrorCategory" + frag("K", "MS", "Decrypt"),
		"Producer." + frag("K", "MS", "KeyID"),
		frag("s", "3", "_bucket_name"),
		"sqs_queue_url",
		"RateLimit" + frag("Bucket"),
		"Load" + frag("Credentials", "From", "S", "SM"),
		"I" + frag("AMUser", "A", "RN"),
		"Credential" + frag("Mode", "S", "TS"),
		`"` + frag("s", "ts") + `"`,
		"the consumer notification mechanism is " + frag("SQS"),
		"SQS queue",
		"Connect account id for the producer",
		"github.com/aws/aws-sdk-go-v2/service/" + frag("sqs"),
		"Asia region",
	}
	for _, s := range mustNotMatch {
		if bannedInfraPattern.MatchString(s) {
			t.Errorf("bannedInfraPattern unexpectedly matched %q, which is public and must stay allowed", s)
		}
	}
}

// TestInfraScanCoversEverySourceKind is a regression test for the scan itself.
// A banned term is caught in a Go comment, in a string literal with no spaces
// (a value a regex on prose would miss), and in Markdown. It is not caught in
// an import path or a struct tag, which are code. The synthetic file is built
// from fragments so no banned term appears in this file.
func TestInfraScanCoversEverySourceKind(t *testing.T) {
	src := "package p\n" +
		"\n" +
		"// Encrypts with " + frag("A", "ES") + " before upload.\n" +
		"import \"github.com/aws/aws-sdk-go-v2/service/" + frag("k", "ms") + "\"\n" +
		"\n" +
		"type T struct {\n" +
		"\tX string `json:\"" + frag("k", "ms") + "\"`\n" +
		"}\n" +
		"\n" +
		"var u = \"" + frag("a", "rn:aws:") + "x\"\n"

	spans := goProse(t, "synthetic.go", []byte(src))
	hits := infraHitsIn(spans)

	want := []struct {
		line int
		term string
	}{
		{line: 3, term: frag("A", "ES")},
		{line: 10, term: frag("a", "rn")},
	}
	if len(hits) != len(want) {
		t.Fatalf("infraHitsIn returned %d hits, want %d: %+v", len(hits), len(want), hits)
	}
	for i, w := range want {
		if hits[i].line != w.line || hits[i].term != w.term {
			t.Errorf("hit %d = line %d term %q, want line %d term %q", i, hits[i].line, hits[i].term, w.line, w.term)
		}
	}

	md := "Shipped docs say the key is in " + frag("S", "SM") + ".\n" +
		"Use `github.com/aws/aws-sdk-go-v2/service/" + frag("s", "3") + "` to upgrade.\n"
	mdHits := infraHitsIn([]proseSpan{{line: 1, text: md}})
	if len(mdHits) != 1 || mdHits[0].term != frag("S", "SM") || mdHits[0].line != 1 {
		t.Errorf("markdown scan = %+v, want exactly one hit on line 1 for the banned term; the allowlisted import path must not count", mdHits)
	}
}

// TestNoInfrastructureProseInGuardTests applies the same term list to the raw
// text of every test source in this package, this file included. The Go-prose
// scan above reads only published non-test sources, so without this a plain
// spelling in a test file, even in a comment, would go unchecked. The text is
// matched raw on purpose: the samples above are split into fragments, and a
// plain spelling anywhere in the file is the mistake this catches.
func TestNoInfrastructureProseInGuardTests(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("published_content: could not determine this file's path via runtime.Caller")
	}

	files, err := filepath.Glob(filepath.Join(filepath.Dir(thisFile), "*_test.go"))
	if err != nil {
		t.Fatalf("published_content: listing the guard's test sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("published_content: no guard test sources found — refusing to run a guard that would trivially pass")
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("published_content: reading %s: %v", filepath.Base(f), err)
		}

		for _, h := range infraHitsIn([]proseSpan{{line: 1, text: string(data)}}) {
			t.Errorf("%s:%d: infrastructure term %q in guard test source: %q", filepath.Base(f), h.line, h.term, h.excerpt)
		}
	}
}
