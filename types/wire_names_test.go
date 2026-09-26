package types

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// snakeCaseWireName is a wire field name: lowercase words joined by
// underscores. (R3: every wire and stored field name is snake_case.) The one
// non-conforming name is "_id", the record's primary-key field as the API
// stores and returns it.
var snakeCaseWireName = regexp.MustCompile(`^(_id|[a-z][a-z0-9]*(_[a-z0-9]+)*)$`)

// dateLikeWireName recognises the wire names that carry a date-time. Names are
// the only signal a struct field offers, so the set is explicit: a new
// date-time field whose name fits none of these is caught by the type rule in
// TestWireStructs instead (no time.Time on a stored record), and a deliberate
// non-date that happens to fit (cancel_at_period_end is a bool) is why
// current_period_* is spelled out rather than matched by suffix.
var dateLikeWireName = regexp.MustCompile(
	`(_at|_date|_time)$|^(timestamp|last_updated|last_updated_data|current_period_start|current_period_end)$`)

// wireSourceDirs are the packages whose structs travel over the wire to the
// Helix API. internal/api is the SDK's own test harness, so it is left out.
var wireSourceDirs = []string{"types", "producer", "consumer", "agent", "credentials"}

// timeTypedDatesAllowed lists the packages whose date-time fields are
// time.Time on purpose: agent is the agent-runtime client, whose responses are
// server time.Time values (always RFC 3339 on the wire) and whose MCPEnvelope is
// signed over its canonical JSON, so its Go types cannot change without
// breaking both source compatibility and the signature. Every other package
// models stored records, where dates stay strings.
var timeTypedDatesAllowed = map[string]bool{"agent": true}

// requestTypeName matches the structs that are request bodies or inputs — what
// a caller fills in to create, invite, update or approve something. It is
// derived from names, not listed by hand, so a new request type is covered the
// day it is added. Response and record types never match: none of them starts
// with one of these verbs or ends in Input/Payload.
var requestTypeName = regexp.MustCompile(`^(Create|Invite|Update|Approve|Reject|Set)|(Input|Payload)$`)

// wireStruct is one struct declaration of a wire package.
type wireStruct struct {
	dir, name string
	pos       string
	fields    []wireField
}

type wireField struct {
	goName   string
	exported bool
	tagged   bool   // has a json tag at all
	jsonName string // "" when untagged or "-"
	typeExpr ast.Expr
	pos      string
}

// parseWireStructs parses every non-test source file of the wire packages with
// go/parser, so a tag in a comment, a string literal or a test fixture is never
// mistaken for a field.
func parseWireStructs(t *testing.T) []wireStruct {
	t.Helper()
	fset := token.NewFileSet()

	var out []wireStruct
	for _, dir := range wireSourceDirs {
		root := filepath.Join("..", dir)
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s/%s: %v", dir, name, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				spec, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				st, ok := spec.Type.(*ast.StructType)
				if !ok {
					return true
				}
				ws := wireStruct{dir: dir, name: spec.Name.Name, pos: fset.Position(spec.Pos()).String()}
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 { // embedded
						continue
					}
					wf := wireField{
						goName:   field.Names[0].Name,
						exported: field.Names[0].IsExported(),
						typeExpr: field.Type,
						pos:      fset.Position(field.Pos()).String(),
					}
					if field.Tag != nil {
						tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
						if value, present := tag.Lookup("json"); present {
							wf.tagged = true
							if jsonName, _, _ := strings.Cut(value, ","); jsonName != "-" {
								wf.jsonName = jsonName
							}
						}
					}
					ws.fields = append(ws.fields, wf)
				}
				out = append(out, ws)
				return true
			})
		}
	}
	return out
}

// TestWireStructs is R3 and R4 from the SDK's side, over every struct the
// wire packages declare:
//   - a struct that has any json-tagged field is a wire type, and then EVERY
//     exported field carries a json tag (an untagged field marshals as its Go
//     name, e.g. "PostalCode"), and every tagged name is snake_case;
//   - date-time fields of stored-record types are string or *string — dates
//     cross the wire as RFC 3339 strings, and a time.Time (or any) field would
//     reject or mangle the legacy string-typed rows older records still carry;
//   - a request type (name pattern, see requestTypeName) carries no "id" /
//     "_id" field: entity ids are assigned by the API in `<prefix>-<uuid>`
//     form, never chosen by the SDK or a caller. References to existing
//     entities (dataset_id, customer_id, ...) are not the new entity's id.
//
// Packages in timeTypedDatesAllowed are exempt from the date rule only.
func TestWireStructs(t *testing.T) {
	structs := parseWireStructs(t)

	var wireFields, requestTypes int
	sawRequest := map[string]bool{}

	for _, s := range structs {
		isWire := false
		for _, f := range s.fields {
			isWire = isWire || f.tagged
		}
		isRequest := s.dir == "types" && requestTypeName.MatchString(s.name)
		if isRequest {
			requestTypes++
			sawRequest[s.name] = true
		}
		if !isWire {
			continue
		}

		for _, f := range s.fields {
			if f.exported && !f.tagged {
				t.Errorf("%s: %s.%s has no json tag, so it marshals as %q — every exported field of a wire struct needs one",
					f.pos, s.name, f.goName, f.goName)
				continue
			}
			if f.jsonName == "" {
				continue
			}
			wireFields++

			if !snakeCaseWireName.MatchString(f.jsonName) {
				t.Errorf("%s: %s json name %q is not snake_case", f.pos, s.name, f.jsonName)
			}
			if !timeTypedDatesAllowed[s.dir] {
				if isTimeType(f.typeExpr) {
					t.Errorf("%s: %s.%s is a time.Time: stored-record dates are RFC 3339 strings on the wire", f.pos, s.name, f.goName)
				} else if dateLikeWireName.MatchString(f.jsonName) && !isStringType(f.typeExpr) {
					t.Errorf("%s: date-time field %s.%s (%q) must be string or *string, not %s", f.pos, s.name, f.goName, f.jsonName, exprString(f.typeExpr))
				}
			}
			if isRequest && (f.jsonName == "id" || f.jsonName == "_id") {
				t.Errorf("%s: request type %s carries the entity id (%q): ids are server-assigned", f.pos, s.name, f.jsonName)
			}
		}
	}

	// Positive controls: the walk really reached the structs, including the
	// request types that a hand-kept list once missed.
	if wireFields < 100 {
		t.Fatalf("only %d json-tagged fields checked; the source walk is broken", wireFields)
	}
	for _, want := range []string{"CreateCompanyRequest", "CreateSubscriptionRequestPayload", "InviteConsumerInput", "DatasetUpdateInput"} {
		if !sawRequest[want] {
			t.Errorf("request type %s not found by name pattern (%d found); the pattern or the walk is broken", want, requestTypes)
		}
	}
}

func isStringType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == "string"
}

func isTimeType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time" && sel.Sel.Name == "Time"
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.InterfaceType:
		return "interface{}"
	default:
		return reflect.TypeOf(e).String()
	}
}
