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

// dateLikeWireName recognises the wire names that carry a date-time. A struct
// field's name is the only signal it offers, so the set is explicit and a name
// outside it is caught only by the type rule in TestWireStructs (no time.Time
// on a stored record). Knowing which string field is a date for certain is the
// schemas' job (format: date-time), checked by the schema contract tests; this
// guard is the SDK-side net. A deliberate non-date that fits a suffix
// (cancel_at_period_end is a bool) is why current_period_* is spelled out.
var dateLikeWireName = regexp.MustCompile(
	`(_at|_date|_time)$|^(since|until|timestamp|last_updated|last_updated_data|current_period_start|current_period_end)$`)

// notWireTypes are the structs of package types that carry no json tags on
// purpose: SDK-side configuration, query parameters and inputs that the SDK
// converts into a tagged payload before anything is sent. Every OTHER struct of
// package types is a wire type even if all its tags were deleted, which is how
// a wholly untagged one is still caught.
var notWireTypes = map[string]bool{
	"Config":                            true,
	"MarketplaceBrowseParams":           true,
	"SubscriptionCheckoutInput":         true,
	"CreateSubscriptionRequestInput":    true,
	"ApproveSubscriptionRequestOptions": true,
}

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
// a caller fills in to create, invite, update or approve something, or what the
// SDK posts. It is derived from names, not listed by hand, so a new request
// type is covered the day it is added. Names ending in Response are never
// requests (CreateDatasetResponse carries the id the API just assigned). The one
// stored record that ends in "Request" is SubscriptionRequest (a subscription
// request AS STORED, which legitimately has an _id), named in storedRecords.
var requestTypeName = regexp.MustCompile(`^(Create|Invite|Update|Approve|Reject|Set)[A-Z]|(Input|Payload|Request)$`)

var storedRecords = map[string]bool{"SubscriptionRequest": true}

// wireStruct is one struct declaration of a wire package. Anonymous structs
// nested in a field (`Dataset *struct{...}`) are collected too, named
// Outer.Field.
type wireStruct struct {
	dir, name string
	fields    []wireField
}

type wireField struct {
	goName    string
	exported  bool
	tagged    bool   // has a json tag at all
	emptyName bool   // tagged, but the name part is empty (`json:",omitempty"`)
	jsonName  string // "" when untagged, "-" or unnamed
	typeExpr  ast.Expr
	pos       string
}

// collectStruct records st and, recursively, every anonymous struct type used
// by one of its fields (through pointers, slices, arrays and map values).
func collectStruct(fset *token.FileSet, dir, name string, st *ast.StructType, out *[]wireStruct) {
	ws := wireStruct{dir: dir, name: name}
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
				jsonName, _, _ := strings.Cut(value, ",")
				switch jsonName {
				case "-":
				case "":
					wf.emptyName = true
				default:
					wf.jsonName = jsonName
				}
			}
		}
		ws.fields = append(ws.fields, wf)

		if nested := nestedStruct(field.Type); nested != nil {
			collectStruct(fset, dir, name+"."+wf.goName, nested, out)
		}
	}
	*out = append(*out, ws)
}

// nestedStruct returns the anonymous struct type behind e, looking through
// pointers, slices, arrays and map values.
func nestedStruct(e ast.Expr) *ast.StructType {
	switch v := e.(type) {
	case *ast.StructType:
		return v
	case *ast.StarExpr:
		return nestedStruct(v.X)
	case *ast.ArrayType:
		return nestedStruct(v.Elt)
	case *ast.MapType:
		return nestedStruct(v.Value)
	}
	return nil
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
				if spec, ok := n.(*ast.TypeSpec); ok {
					if st, ok := spec.Type.(*ast.StructType); ok {
						collectStruct(fset, dir, spec.Name.Name, st, &out)
					}
				}
				return true
			})
		}
	}
	return out
}

// TestWireStructs is R3 and R4 from the SDK's side, over every struct the
// wire packages declare (anonymous nested structs included):
//   - a struct is a wire type when any of its fields has a json tag — and every
//     struct of package types is one unless it is listed in notWireTypes; every
//     exported field of a wire struct carries a json tag with a name (an
//     untagged or unnamed field marshals as its Go name, e.g. "PostalCode"), and
//     every name is snake_case;
//   - date-time fields of stored-record types are string or *string — dates
//     cross the wire as RFC 3339 strings, and a time.Time (or any) field would
//     reject or mangle the legacy string-typed rows older records still carry;
//   - a request type (name pattern, see requestTypeName) carries no "id" /
//     "_id" field: entity ids are assigned by the API in `<prefix>-<uuid>`
//     form, never chosen by the SDK or a caller. References to existing
//     entities (dataset_id, customer_id, ...) are not the new entity's id.
//
// Packages in timeTypedDatesAllowed may declare a date as time.Time as well as
// string, and nothing else.
func TestWireStructs(t *testing.T) {
	structs := parseWireStructs(t)

	var wireFields, requestTypes, nestedStructs int
	sawRequest := map[string]bool{}

	for _, s := range structs {
		if strings.Contains(s.name, ".") {
			nestedStructs++
		}
		isWire := s.dir == "types" && !notWireTypes[s.name] && !strings.Contains(s.name, ".")
		for _, f := range s.fields {
			isWire = isWire || f.tagged
		}
		isRequest := requestTypeName.MatchString(s.name) && !strings.HasSuffix(s.name, "Response") &&
			!storedRecords[s.name] && !strings.Contains(s.name, ".")
		if isRequest {
			requestTypes++
			sawRequest[s.dir+"."+s.name] = true
		}
		if !isWire {
			continue
		}

		for _, f := range s.fields {
			if f.exported && (!f.tagged || f.emptyName) {
				t.Errorf("%s: %s.%s has no json name, so it marshals as %q — every exported field of a wire struct needs a tagged name",
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
			switch {
			case timeTypedDatesAllowed[s.dir]:
				if dateLikeWireName.MatchString(f.jsonName) && !isStringType(f.typeExpr) && !isTimeType(f.typeExpr) {
					t.Errorf("%s: date-time field %s.%s (%q) must be time.Time or string, not %s", f.pos, s.name, f.goName, f.jsonName, exprString(f.typeExpr))
				}
			case isTimeType(f.typeExpr):
				t.Errorf("%s: %s.%s is a time.Time: stored-record dates are RFC 3339 strings on the wire", f.pos, s.name, f.goName)
			case dateLikeWireName.MatchString(f.jsonName) && !isStringType(f.typeExpr):
				t.Errorf("%s: date-time field %s.%s (%q) must be string or *string, not %s", f.pos, s.name, f.goName, f.jsonName, exprString(f.typeExpr))
			}
			if isRequest && (f.jsonName == "id" || f.jsonName == "_id") {
				t.Errorf("%s: request type %s carries the entity id (%q): ids are server-assigned", f.pos, s.name, f.jsonName)
			}
		}
	}

	// Positive controls: the walk really reached the structs — including
	// anonymous nested ones and the request types a hand-kept list once missed.
	if wireFields < 100 {
		t.Fatalf("only %d json-tagged fields checked; the source walk is broken", wireFields)
	}
	if nestedStructs == 0 {
		t.Fatal("no anonymous nested struct was collected; the nested walk is broken")
	}
	for _, want := range []string{"types.CreateCompanyRequest", "types.CreateSubscriptionRequestPayload", "types.InviteConsumerInput", "types.DatasetUpdateInput", "consumer.RecordOutcomeRequest"} {
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
