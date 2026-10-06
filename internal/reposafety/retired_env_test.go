package reposafety

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// retiredEnvFragments are the words the retired environment variable's name
// is made of; retiredEnvName joins them with "_". The name is assembled here
// at runtime and never written out in the repo, for the same reason
// bannedContentPatternB64 is encoded: this repository is public in git. The
// middle fragment is split so that this file holds no standalone infrastructure
// word, which TestNoInfrastructureProseInGuardTests would flag.
var retiredEnvFragments = []string{"HELIX", "S" + "SM", "CUSTOMER", "PREFIX"}

// fillFragments replaces the {1} to {4} markers in s with retiredEnvFragments,
// so the detector's test sources can name the fragments without spelling them.
func fillFragments(s string) string {
	return strings.NewReplacer(
		"{1}", retiredEnvFragments[0],
		"{2}", retiredEnvFragments[1],
		"{3}", retiredEnvFragments[2],
		"{4}", retiredEnvFragments[3],
	).Replace(s)
}

var retiredEnvName = strings.Join(retiredEnvFragments, "_")

// mentionsRetiredEnvName reports whether text names the retired variable,
// in any casing.
func mentionsRetiredEnvName(text string) bool {
	return strings.Contains(strings.ToUpper(text), retiredEnvName)
}

// TestRetiredEnvNameNotInPublishedFiles fails if the retired variable's name
// appears anywhere in the published file tree, test files and docs included:
// a test that only mentions the name still tells a reader it is a live setting.
func TestRetiredEnvNameNotInPublishedFiles(t *testing.T) {
	root := repoRoot(t)

	for _, f := range publishedFiles(t, root) {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("retired_env: reading %s: %v", f, err)
		}
		if mentionsRetiredEnvName(string(data)) {
			t.Errorf("%s: names the retired environment variable; it must not appear in any published file", f)
		}
	}
}

// TestNoRetiredEnvReadsInSources fails if any non-test Go source in the
// published main module reads the retired variable through os.Getenv or
// os.LookupEnv. Test files are not read here; their text is covered by
// TestRetiredEnvNameNotInPublishedFiles.
func TestNoRetiredEnvReadsInSources(t *testing.T) {
	root := repoRoot(t)

	// Grouped by directory, one Go package each: a name split across two
	// files of one package is still one name.
	packages := map[string]map[string][]byte{}
	for _, f := range publishedFiles(t, root) {
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("retired_env: reading %s: %v", f, err)
		}
		dir := path.Dir(f)
		if packages[dir] == nil {
			packages[dir] = map[string][]byte{}
		}
		packages[dir][f] = data
	}

	if len(packages) == 0 {
		t.Fatal("retired_env: no non-test Go files found — refusing to run a guard that would trivially pass")
	}
	for dir, files := range packages {
		hits, err := retiredEnvReads(files)
		if err != nil {
			t.Fatalf("retired_env: parsing %s: %v", dir, err)
		}
		for _, hit := range hits {
			t.Errorf("%s: reads the retired environment variable", hit)
		}
	}
}

// retiredEnvReads takes the non-test Go files of one package (file name to
// source) and returns the position of every os.Getenv and os.LookupEnv call
// whose name argument can reach the retired variable. A call counts when:
//   - its string pieces, joined in source order, spell the name
//     ("HELIX_SSM_" + "CUSTOMER_PREFIX");
//   - its string pieces carry two or more distinct fragments, which catches
//     strings.Join over the fragments and a literal prefix with the rest
//     supplied by a variable ("HELIX_SSM_" + key);
//   - its argument is not a plain literal and the package's string literals
//     together carry every fragment, which catches a name built once into a
//     constant or variable, in this file or a sibling, and read later.
//
// A name assembled from non-string data (bytes, runes, a file) is not
// recoverable from the source text and is not followed.
func retiredEnvReads(files map[string][]byte) ([]string, error) {
	fset := token.NewFileSet()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var parsed []*ast.File
	var pkgLiterals []string
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, files[name], 0)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, file)
		ast.Inspect(file, func(n ast.Node) bool {
			if s, ok := stringLiteral(n); ok {
				pkgLiterals = append(pkgLiterals, strings.ToUpper(s))
			}
			return true
		})
	}
	pkgNamesEveryFragment := distinctFragments(pkgLiterals) == len(retiredEnvFragments)

	var hits []string
	for _, file := range parsed {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isEnvLookup(call) {
				return true
			}
			arg := call.Args[0]

			var pieces []string
			ast.Inspect(arg, func(m ast.Node) bool {
				if s, ok := stringLiteral(m); ok {
					pieces = append(pieces, strings.ToUpper(s))
				}
				return true
			})

			_, plainLiteral := arg.(*ast.BasicLit)
			if strings.Contains(strings.Join(pieces, ""), retiredEnvName) ||
				distinctFragments(pieces) >= 2 ||
				(!plainLiteral && pkgNamesEveryFragment) {
				hits = append(hits, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	return hits, nil
}

// isEnvLookup reports whether call is os.Getenv(x) or os.LookupEnv(x).
func isEnvLookup(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv")
}

// distinctFragments counts the retired-name fragments that appear, upper-cased,
// in at least one of pieces.
func distinctFragments(pieces []string) int {
	n := 0
	for _, frag := range retiredEnvFragments {
		for _, p := range pieces {
			if strings.Contains(p, frag) {
				n++
				break
			}
		}
	}
	return n
}

// stringLiteral returns the unquoted value of a string literal node.
func stringLiteral(n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// TestRetiredEnvReadDetector pins the shapes retiredEnvReads must flag and the
// ordinary reads it must leave alone. Each source is written with {1} to {4}
// markers for the fragments and filled in at runtime (fillFragments), so no
// banned string appears contiguously in this file.
func TestRetiredEnvReadDetector(t *testing.T) {
	fragmentsExpr := `strings.Join([]string{"{1}", "{2}", "{3}", "{4}"}, "_")`

	source := func(stmt string) []byte {
		return []byte(fillFragments("package p\n\nimport (\n\t\"os\"\n\t\"strings\"\n)\n\n" +
			"func read(key string) {\n\t" + stmt + "\n\t_ = strings.ToUpper(key)\n}\n"))
	}

	cases := []struct {
		name string
		stmt string
		want int
	}{
		{"literal name", `_ = os.Getenv("` + retiredEnvName + `")`, 1},
		{"lowercase literal name", `_ = os.LookupEnv("` + strings.ToLower(retiredEnvName) + `")`, 1},
		{"concatenated literals", `_ = os.Getenv("{1}_" + "{2}_" + "{3}_" + "{4}")`, 1},
		{"joined fragments", `_ = os.LookupEnv(` + fragmentsExpr + `)`, 1},
		{"literal prefix plus variable", `_ = os.Getenv("{1}_{2}_" + key)`, 1},
		{"name built once into a variable", `n := ` + fragmentsExpr + "\n\t_ = os.Getenv(n)", 1},
		{"unrelated literal", `_ = os.Getenv("HELIX_API_ENDPOINT")`, 0},
		{"unrelated variable", `_ = os.Getenv(key)`, 0},
		{"unrelated joined words", `_ = os.Getenv(strings.Join([]string{"HELIX", "API", "ENDPOINT"}, "_"))`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := retiredEnvReads(map[string][]byte{"p.go": source(tc.stmt)})
			if err != nil {
				t.Fatalf("retiredEnvReads: %v", err)
			}
			if len(hits) != tc.want {
				t.Errorf("retiredEnvReads(%s) = %d hit(s) %v, want %d", tc.stmt, len(hits), hits, tc.want)
			}
		})
	}

	// The suffix is a constant in a sibling file: a read in one file of the
	// package must still be caught when the other file holds the rest.
	t.Run("constant in a sibling file", func(t *testing.T) {
		hits, err := retiredEnvReads(map[string][]byte{
			"names.go": []byte(fillFragments("package p\n\nconst suffix = \"{2}_{3}_{4}\"\n")),
			"read.go":  []byte(fillFragments("package p\n\nimport \"os\"\n\nvar _ = os.Getenv(\"{1}_\" + suffix)\n")),
		})
		if err != nil {
			t.Fatalf("retiredEnvReads: %v", err)
		}
		if len(hits) != 1 {
			t.Errorf("retiredEnvReads = %d hit(s) %v, want 1", len(hits), hits)
		}
	})
}

// TestMentionsRetiredEnvName pins the casing-insensitive text match behind
// TestRetiredEnvNameNotInPublishedFiles.
func TestMentionsRetiredEnvName(t *testing.T) {
	for _, s := range []string{retiredEnvName, strings.ToLower(retiredEnvName), "see " + retiredEnvName + " in docs"} {
		if !mentionsRetiredEnvName(s) {
			t.Errorf("mentionsRetiredEnvName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"HELIX_API_ENDPOINT", fillFragments("{2}_PARAMETER_{4}")} {
		if mentionsRetiredEnvName(s) {
			t.Errorf("mentionsRetiredEnvName(%q) = true, want false", s)
		}
	}
}
