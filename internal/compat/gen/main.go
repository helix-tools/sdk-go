// Command gen writes a Go program that USES every exported identifier of the
// module's public packages — functions and methods with their exact
// signatures, typed constants and variables, every exported struct field with
// its exact type, a keyed composite literal of each struct, and the
// comparability of each comparable type. Compiling that program against a later revision proves
// no previously exported symbol was removed, renamed or re-typed
// (internal/compat runs exactly that check).
//
// Regenerate the pinned snapshot from a released revision:
//
//	git archive <tag-or-sha> | tar -x -C /tmp/base
//	go run ./internal/compat/gen -dir /tmp/base -skip api > internal/compat/testdata/surface_v2.go.txt
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	dir := flag.String("dir", ".", "module root to snapshot")
	skip := flag.String("skip", "", "comma-separated package directories to leave out (e.g. one that was deliberately made internal)")
	flag.Parse()

	skipped := map[string]bool{}
	for _, s := range strings.Split(*skip, ",") {
		if s != "" {
			skipped[s] = true
		}
	}

	root, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}

	modulePath := goList(root, "-m", "-f", "{{.Path}}")[0]
	pkgs := goList(root, "-f", "{{.ImportPath}}|{{.Dir}}|{{.Name}}", "./...")

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil).(types.ImporterFrom)
	g := &gen{aliases: map[string]string{}, modulePath: modulePath}

	sort.Strings(pkgs)

	for _, line := range pkgs {
		parts := strings.Split(line, "|")
		importPath, pkgDir, name := parts[0], parts[1], parts[2]
		rel := strings.TrimPrefix(strings.TrimPrefix(importPath, modulePath), "/")

		if name == "main" || strings.Contains(rel, "internal") || skipped[rel] {
			continue
		}

		g.addPackage(fset, imp, importPath, pkgDir)
	}

	os.Stdout.Write(g.render())
}

func goList(dir string, args ...string) []string {
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		log.Fatalf("go list %v: %v", args, err)
	}

	return strings.Fields(strings.TrimSpace(string(out)))
}

type gen struct {
	modulePath string
	aliases    map[string]string // import path -> alias
	body       bytes.Buffer
}

func (g *gen) alias(p *types.Package) string {
	if a, ok := g.aliases[p.Path()]; ok {
		return a
	}

	a := fmt.Sprintf("%s%d", sanitize(path.Base(p.Path())), len(g.aliases))
	g.aliases[p.Path()] = a

	return a
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
}

func (g *gen) qualifier(p *types.Package) string { return g.alias(p) }

func (g *gen) typeString(t types.Type) string { return types.TypeString(t, g.qualifier) }

func (g *gen) addPackage(fset *token.FileSet, imp types.ImporterFrom, importPath, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}

	var files []*ast.File

	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			log.Fatal(err)
		}

		files = append(files, f)
	}

	conf := types.Config{Importer: importerAt{imp, dir}}

	pkg, err := conf.Check(importPath, fset, files, nil)
	if err != nil {
		log.Fatalf("type-check %s: %v", importPath, err)
	}

	me := g.alias(pkg)
	fmt.Fprintf(&g.body, "\n// ---- package %s ----\n", importPath)

	scope := pkg.Scope()

	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}

		ref := me + "." + name

		switch o := obj.(type) {
		case *types.Func:
			fmt.Fprintf(&g.body, "var _ %s = %s\n", g.typeString(o.Type()), ref)
		case *types.Const:
			if b, ok := o.Type().(*types.Basic); ok && b.Info()&types.IsUntyped != 0 {
				fmt.Fprintf(&g.body, "var _ = %s\n", ref)
			} else {
				fmt.Fprintf(&g.body, "var _ %s = %s\n", g.typeString(o.Type()), ref)
			}
		case *types.Var:
			fmt.Fprintf(&g.body, "var _ %s = %s\n", g.typeString(o.Type()), ref)
		case *types.TypeName:
			g.addType(me, o)
		}
	}
}

func (g *gen) addType(me string, o *types.TypeName) {
	ref := me + "." + o.Name()
	named, ok := o.Type().(*types.Named)

	if !ok {
		fmt.Fprintf(&g.body, "var _ %s\n", ref)
		return
	}

	if named.TypeParams().Len() > 0 {
		return
	}

	switch u := named.Underlying().(type) {
	case *types.Struct:
		fmt.Fprintf(&g.body, "var _ = func(v %s) {\n", ref)

		var keyed []string

		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			if f.Exported() {
				fmt.Fprintf(&g.body, "\tvar _ %s = v.%s\n", g.typeString(f.Type()), f.Name())
				keyed = append(keyed, fmt.Sprintf("%s: *new(%s)", f.Name(), g.typeString(f.Type())))
			}
		}

		fmt.Fprintln(&g.body, "}")

		// Callers build these values with keyed literals (a promoted field
		// cannot be a literal key) and, when the type is comparable, compare
		// them or use them as map keys — none of which a selector check sees.
		if len(keyed) > 0 {
			fmt.Fprintf(&g.body, "var _ = %s{%s}\n", ref, strings.Join(keyed, ", "))
		}

		if types.Comparable(named) {
			fmt.Fprintf(&g.body, "var _ = func(a, b %s) bool { return a == b }\n", ref)
		}
	case *types.Interface:
		fmt.Fprintf(&g.body, "var _ = func(v %s) {\n", ref)

		for i := 0; i < u.NumMethods(); i++ {
			m := u.Method(i)
			if m.Exported() {
				fmt.Fprintf(&g.body, "\tvar _ %s = v.%s\n", g.typeString(m.Type()), m.Name())
			}
		}

		fmt.Fprintln(&g.body, "}")
	default:
		fmt.Fprintf(&g.body, "var _ %s\n", ref)
	}

	// Methods: value-receiver methods are pinned through T's own method set,
	// so a change to a pointer receiver is caught too.
	valueSet := types.NewMethodSet(named)
	ptrSet := types.NewMethodSet(types.NewPointer(named))

	var names []string

	for i := 0; i < ptrSet.Len(); i++ {
		if fn, ok := ptrSet.At(i).Obj().(*types.Func); ok && fn.Exported() {
			names = append(names, fn.Name())
		}
	}

	sort.Strings(names)

	for _, n := range names {
		if _, isInterface := named.Underlying().(*types.Interface); isInterface {
			continue
		}

		sel := ptrSet.Lookup(o.Pkg(), n)
		fn := sel.Obj().(*types.Func)
		sig := fn.Type().(*types.Signature)

		recv, expr := "*"+ref, "(*"+ref+")."+n
		if valueSet.Lookup(o.Pkg(), n) != nil {
			recv, expr = ref, ref+"."+n
		}

		fmt.Fprintf(&g.body, "var _ %s = %s\n", g.methodType(recv, sig), expr)
	}
}

// methodType renders a method expression's type: func(recv, params...) results.
func (g *gen) methodType(recv string, sig *types.Signature) string {
	var params []string

	for i := 0; i < sig.Params().Len(); i++ {
		t := g.typeString(sig.Params().At(i).Type())

		if sig.Variadic() && i == sig.Params().Len()-1 {
			t = "..." + strings.TrimPrefix(t, "[]")
		}

		params = append(params, t)
	}

	var results []string
	for i := 0; i < sig.Results().Len(); i++ {
		results = append(results, g.typeString(sig.Results().At(i).Type()))
	}

	out := "func(" + strings.Join(append([]string{recv}, params...), ", ") + ")"

	switch len(results) {
	case 0:
	case 1:
		out += " " + results[0]
	default:
		out += " (" + strings.Join(results, ", ") + ")"
	}

	return out
}

func (g *gen) render() []byte {
	var out bytes.Buffer

	fmt.Fprintf(&out, "// Code generated by internal/compat/gen; DO NOT EDIT.\n//\n")
	fmt.Fprintf(&out, "// A consumer program that uses every exported identifier of the module's\n")
	fmt.Fprintf(&out, "// public packages, at the revision it was generated from.\n\n")
	fmt.Fprintf(&out, "package main\n\nimport (\n")

	var paths []string
	for p := range g.aliases {
		paths = append(paths, p)
	}

	sort.Strings(paths)

	for _, p := range paths {
		fmt.Fprintf(&out, "\t%s %q\n", g.aliases[p], p)
	}

	fmt.Fprintf(&out, ")\n\nfunc main() {}\n")
	out.Write(g.body.Bytes())

	return out.Bytes()
}

// importerAt resolves imports relative to the package directory so the source
// importer finds modules the way `go build` would.
type importerAt struct {
	imp types.ImporterFrom
	dir string
}

func (i importerAt) Import(p string) (*types.Package, error) { return i.imp.ImportFrom(p, i.dir, 0) }
