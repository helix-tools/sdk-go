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

// timeTypedDatesAllowed lists the packages whose date-time fields are
// time.Time on purpose: agent is the agent-runtime client, whose responses are
// server time.Time values (always RFC 3339 on the wire) and whose MCPEnvelope is
// signed over its canonical JSON, so its Go types cannot change without
// breaking both source compatibility and the signature. Every other package
// models stored records, where dates stay strings.
var timeTypedDatesAllowed = map[string]bool{"agent": true}

// wireSourceDirs are the packages whose structs travel over the wire to the
// Helix API. internal/api is the SDK's own test harness, so it is left out.
var wireSourceDirs = []string{"types", "producer", "consumer", "agent", "credentials"}

// TestJSONTagsAreSnakeCase parses every non-test source file of the packages
// above and requires each struct field's JSON name to be snake_case, and every
// date-time field (a name ending in _at) of a stored-record type to be a
// string or *string: dates cross the wire as RFC 3339 strings, and a time.Time
// field would reject the legacy string-typed rows older records still carry.
// (Packages in timeTypedDatesAllowed are exempt from the second rule only.)
//
// It walks the AST rather than matching text, so a tag in a comment, a string
// literal or a test fixture is never mistaken for a field.
func TestJSONTagsAreSnakeCase(t *testing.T) {
	fset := token.NewFileSet()

	var checked int
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
				field, ok := n.(*ast.Field)
				if !ok || field.Tag == nil {
					return true
				}
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
				jsonName, _, _ := strings.Cut(tag.Get("json"), ",")
				if jsonName == "" || jsonName == "-" {
					return true
				}
				checked++
				where := fset.Position(field.Pos()).String()
				if !snakeCaseWireName.MatchString(jsonName) {
					t.Errorf("%s: json name %q is not snake_case", where, jsonName)
				}
				if strings.HasSuffix(jsonName, "_at") && !timeTypedDatesAllowed[dir] && !isStringType(field.Type) {
					t.Errorf("%s: date-time field %q must be string or *string (RFC 3339 on the wire), not %s", where, jsonName, exprString(field.Type))
				}
				return true
			})
		}
	}

	// Positive control: the walk really reached the structs (a broken path
	// would "pass" by checking nothing).
	if checked < 100 {
		t.Fatalf("only %d json-tagged fields checked; the source walk is broken", checked)
	}
}

func isStringType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == "string"
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return reflect.TypeOf(e).String()
	}
}

// TestCreateRequestsCarryNoClientIDs is R4 from the SDK's side: entity ids are
// minted by the server in `<prefix>-<uuid>` form, so no request type that
// creates or invites an entity may carry an "id" / "_id" field the SDK (or a
// caller) could fill with an id of their own making. References to existing
// entities (dataset_id, customer_id, ...) are not ids of the new entity and
// stay.
func TestCreateRequestsCarryNoClientIDs(t *testing.T) {
	requests := []any{
		CreateCompanyRequest{},
		UpdateCompanyRequest{},
		InviteUserRequest{},
		CreateSubscriptionRequest{},
		CreateSubscriptionRequestInput{},
		InviteConsumerInput{},
		DatasetUpdateInput{},
	}

	for _, req := range requests {
		typ := reflect.TypeOf(req)
		for i := 0; i < typ.NumField(); i++ {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if name == "id" || name == "_id" {
				t.Errorf("%s.%s carries the entity id (%q) on a request: ids are server-assigned", typ.Name(), typ.Field(i).Name, name)
			}
		}
	}
}
