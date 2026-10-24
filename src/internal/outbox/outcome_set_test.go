package outbox

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// outcome_set_test.go — sk-303.14, follow-up from review A/7.
//
// The closed set of delivery outcomes lives in the code as ONE list of
// constants (push.go) and one schema constraint
// (notifications_push_outcome_chk). Until this file EVERY test that needed
// the set typed it BY HAND — and when sk-303.14 introduced the seventh value,
// four such lists fell behind the code the same day and stayed green. Green
// on a hand-written list does not mean "everything is fine"; it means "the
// check is not looking".
//
// So the set here is READ FROM THE PACKAGE SOURCE, not re-typed. The same
// trick signals_test.go already uses to read metric `@label` lines: go/ast
// over the package's non-test files. An eighth value will enter every check
// at once, without a single list edit — and if it does not, the scan itself
// is broken, which a separate assertion makes visible.

// scanAnchors — constants the scan MUST find to be considered working.
//
// This is NOT an enumeration of the set (that is the point of the file), it
// is a lower bound on the scan working: if push.go is renamed, moved or
// stops being parsed, the scan returns emptiness, and every "for every
// outcome" check becomes tautologically green — exactly the failure this
// file was written against. The anchors are two values that have existed
// since the first wave and are not tied to the digest: their disappearance
// means the scan is broken, not that the set has grown.
var scanAnchors = map[string][]string{
	"outcome": {"success", "transport_exhausted"},
	"cause":   {"sender_disabled", "channel_disabled"},
}

// packageStringConsts reads the package's non-test files and returns the
// values of string constants whose name starts with prefix, in "file, then
// declaration" order — the order is stable across runs so a failure reads
// the same.
func packageStringConsts(t *testing.T, prefix string) []string {
	t.Helper()
	_, values := packageStringConstPairs(t, prefix)
	return values
}

// packageStringConstPairs — the shared parse: names and values of the
// package's string constants with the given prefix, in "file, then
// declaration" order.
func packageStringConstPairs(t *testing.T, prefix string) (names, values []string) {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing package: %v", err)
	}

	type namedConst struct{ name, value string }
	byFile := map[string][]namedConst{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, prefix) || i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						v, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("%s: constant %s does not parse as a string: %v", path, name.Name, err)
						}
						byFile[path] = append(byFile[path], namedConst{name.Name, v})
					}
				}
			}
		}
	}

	files := make([]string, 0, len(byFile))
	for path := range byFile {
		files = append(files, path)
	}
	sort.Strings(files)

	for _, path := range files {
		for _, c := range byFile[path] {
			names = append(names, c.name)
			values = append(values, c.value)
		}
	}

	for _, anchor := range scanAnchors[prefix] {
		found := false
		for _, v := range values {
			if v == anchor {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the %s* constant scan did not find the anchor %q — the scan is broken, not the code: "+
				"every \"for every value\" check without it is tautologically green",
				prefix, anchor)
		}
	}
	return names, values
}

// allOutcomeValues — values of EVERY outcome* constant in the package: the
// closed set of terminal delivery outcomes as the code knows it.
func allOutcomeValues(t *testing.T) []string {
	t.Helper()
	return packageStringConsts(t, "outcome")
}

// packageConstNames — the same as packageStringConsts, but returns constant
// names. The parse is the same; only what is taken from the pair differs.
func packageConstNames(t *testing.T, prefix string) []string {
	t.Helper()
	names, _ := packageStringConstPairs(t, prefix)
	return names
}

// allOutcomeConstNames — NAMES of the outcome* constants, in the same order
// allOutcomeValues returns their values. Needed where what is checked is
// not the value but the fact that someone reads the constant.
func allOutcomeConstNames(t *testing.T) []string {
	t.Helper()
	return packageConstNames(t, "outcome")
}

// allNamedCauseValues — values of the cause* constants, except empty
// causeNone: sub-causes exist only inside configuration_no_send, and "no
// sub-cause" is not a sub-cause.
func allNamedCauseValues(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, v := range packageStringConsts(t, "cause") {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// quotedLiteral cuts values in single quotes out of a schema constraint
// definition.
var quotedLiteral = regexp.MustCompile(`'([^']*)'`)

// TestOutcomeSet_CodeAndSchemaAgree — the claim push.go makes as a comment
// ("the closed outcome set lives in the code as ONE list, and a list that
// has fallen behind the schema is silent on a miss"), checked as an
// assertion.
//
// Two directions, and both are real: a value added by a migration and
// forgotten in the code leaves the outcome unreachable; a value added in
// the code and forgotten in the migration will fail the row write on the
// CHECK — but in production, for the first recipient, not here.
func TestOutcomeSet_CodeAndSchemaAgree(t *testing.T) {
	pool := newTestPool(t)

	var def string
	if err := pool.QueryRow(context.Background(), `
SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`,
		"notifications_push_outcome_chk").Scan(&def); err != nil {
		t.Fatalf("read constraint notifications_push_outcome_chk: %v", err)
	}

	inSchema := map[string]bool{}
	for _, m := range quotedLiteral.FindAllStringSubmatch(def, -1) {
		inSchema[m[1]] = true
	}
	if len(inSchema) == 0 {
		t.Fatalf("the constraint definition has no values: %q — the parse is broken, not the schema", def)
	}

	inCode := map[string]bool{}
	for _, v := range allOutcomeValues(t) {
		inCode[v] = true
		if !inSchema[v] {
			t.Errorf("outcome %q is declared in code, but the schema does not accept it — the first row with it will fail the CHECK", v)
		}
	}
	for v := range inSchema {
		if !inCode[v] {
			t.Errorf("outcome %q is allowed by the schema but is not in the code — the value is unreachable, "+
				"and the outcome list in the code has fallen behind the schema", v)
		}
	}
}
