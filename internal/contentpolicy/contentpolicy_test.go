// Package contentpolicy enforces this repo's customer-visible content
// policy (see CLAUDE.md, "Customer-visible content policy"): no runtime
// message, and no doc comment on an exported symbol, may name an internal
// mechanism such as the key-management service or the compression format.
// Structural (AST-based) checks, not a bare substring grep, so the scan
// distinguishes an actual message/doc string from an identifier, an import
// path, or a struct tag that happens to contain the same letters.
package contentpolicy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenTerms names internals that customer-visible text must never
// name directly (see CLAUDE.md rule 3: no encryption algorithms/mechanics,
// never name key-management services; no compression format as mechanics).
var forbiddenTerms = []string{"KMS", "gzip", "AES-GCM", "SNS"}

// scanPackageDirs are the packages that make up this SDK's public surface.
// internal/ is deliberately excluded: it is test-harness-only and
// unimportable outside this module (Go's internal/ convention), and
// test/ holds a manual, non-published E2E script — neither ships to, or
// is read by, a customer.
var scanPackageDirs = []string{"producer", "consumer", "types", "credentials", "agent"}

// loggingCallNames are the fmt/errors calls whose string-literal arguments
// become a runtime message a customer can see (a printed progress line, a
// warning, or a returned error).
var loggingCallNames = map[string]map[string]bool{
	"fmt":    {"Errorf": true, "Sprintf": true, "Printf": true, "Println": true, "Fprintf": true, "Fprintln": true, "Sprint": true, "Print": true},
	"errors": {"New": true},
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("contentpolicy: could not determine this file's path via runtime.Caller")
	}

	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("contentpolicy: walked up to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

type violation struct {
	pos  token.Position
	kind string
	term string
	text string
}

func containsForbiddenTerm(s string) string {
	for _, term := range forbiddenTerms {
		if strings.Contains(s, term) {
			return term
		}
	}
	return ""
}

// isLoggingCall reports whether call is one of fmt.Errorf/Printf/.../
// errors.New — a call whose string-literal arguments become customer-
// visible runtime text.
func isLoggingCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	fns, ok := loggingCallNames[pkgIdent.Name]
	if !ok {
		return false
	}
	return fns[sel.Sel.Name]
}

func checkFile(t *testing.T, fset *token.FileSet, path string) []violation {
	t.Helper()

	src, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("contentpolicy: failed to parse %s: %v", path, err)
	}

	var violations []violation

	// Package doc comment: always customer-visible (godoc/pkg.go.dev).
	if src.Doc != nil {
		text := src.Doc.Text()
		if term := containsForbiddenTerm(text); term != "" {
			violations = append(violations, violation{fset.Position(src.Doc.Pos()), "package doc", term, text})
		}
	}

	ast.Inspect(src, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if !isLoggingCall(node) {
				return true
			}
			for _, arg := range node.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if term := containsForbiddenTerm(lit.Value); term != "" {
					violations = append(violations, violation{fset.Position(lit.Pos()), "message", term, lit.Value})
				}
			}
		case *ast.FuncDecl:
			if node.Doc == nil || !node.Name.IsExported() {
				return true
			}
			text := node.Doc.Text()
			if term := containsForbiddenTerm(text); term != "" {
				violations = append(violations, violation{fset.Position(node.Doc.Pos()), "func doc " + node.Name.Name, term, text})
			}
		case *ast.GenDecl:
			if node.Tok == token.IMPORT {
				return true
			}
			if node.Doc != nil {
				if exportedSpecName(node) != "" {
					text := node.Doc.Text()
					if term := containsForbiddenTerm(text); term != "" {
						violations = append(violations, violation{fset.Position(node.Doc.Pos()), "decl doc " + exportedSpecName(node), term, text})
					}
				}
			}
			for _, spec := range node.Specs {
				var doc *ast.CommentGroup
				var name string
				switch s := spec.(type) {
				case *ast.TypeSpec:
					doc, name = s.Doc, s.Name.Name
				case *ast.ValueSpec:
					doc = s.Doc
					if len(s.Names) > 0 {
						name = s.Names[0].Name
					}
				}
				if doc == nil || name == "" || !ast.IsExported(name) {
					continue
				}
				text := doc.Text()
				if term := containsForbiddenTerm(text); term != "" {
					violations = append(violations, violation{fset.Position(doc.Pos()), "spec doc " + name, term, text})
				}
			}
		case *ast.Field:
			// Exported struct field doc/line comments (visible on IDE hover
			// and godoc for an exported struct).
			if node.Doc == nil && node.Comment == nil {
				return true
			}
			exported := false
			for _, n := range node.Names {
				if n.IsExported() {
					exported = true
				}
			}
			if !exported {
				return true
			}
			for _, cg := range []*ast.CommentGroup{node.Doc, node.Comment} {
				if cg == nil {
					continue
				}
				text := cg.Text()
				if term := containsForbiddenTerm(text); term != "" {
					fieldName := "<field>"
					if len(node.Names) > 0 {
						fieldName = node.Names[0].Name
					}
					violations = append(violations, violation{fset.Position(cg.Pos()), "field doc " + fieldName, term, text})
				}
			}
		}
		return true
	})

	return violations
}

// exportedSpecName returns the name of the first exported spec in a
// GenDecl, used only to label a GenDecl-level (not spec-level) doc comment
// meaningfully; it returns "" when nothing in the decl is exported, in
// which case the GenDecl-level doc is skipped as non-customer-visible.
func exportedSpecName(decl *ast.GenDecl) string {
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if s.Name.IsExported() {
				return s.Name.Name
			}
		case *ast.ValueSpec:
			for _, n := range s.Names {
				if n.IsExported() {
					return n.Name
				}
			}
		}
	}
	return ""
}

// TestNoInternalMechanismNamesInCustomerVisibleContent walks this SDK's
// public package directories and fails if any runtime message (a
// fmt.Errorf/Printf/... or errors.New string literal) or any doc comment
// attached to an exported symbol names an internal mechanism (the
// key-management service or the compression format) instead of describing
// it in capability language.
func TestNoInternalMechanismNamesInCustomerVisibleContent(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	var allViolations []violation

	for _, dir := range scanPackageDirs {
		absDir := filepath.Join(root, dir)
		err := filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			allViolations = append(allViolations, checkFile(t, fset, path)...)
			return nil
		})
		if err != nil {
			t.Fatalf("contentpolicy: failed walking %s: %v", absDir, err)
		}
	}

	for _, v := range allViolations {
		t.Errorf("%s: %s names forbidden internal term %q (customer-visible content must use capability language instead): %q",
			v.pos, v.kind, v.term, strings.TrimSpace(v.text))
	}
}
