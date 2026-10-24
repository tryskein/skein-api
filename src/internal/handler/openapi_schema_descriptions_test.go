package handler

// openapi_schema_descriptions_test.go — sk-303.14, A/9 repair (finding K-1):
// a gate on a directive that landed in a TYPE schema by accident.
//
// # What happened and why nobody saw it
//
// Swag directives do not live only on functions. A TYPE doc-comment line
// starting with `@Description` becomes the SCHEMA DESCRIPTION of that type
// in the spec. An earlier comment named the directive by its name with a
// sigil in the middle of Russian prose, a line wrap put the name at the
// start of the line — and a phrase fragment ("of the operation — but that
// is already the truth, not an excuse for the missing") went into the spec
// as the official description of the request-body schema
// internal_handler.putReviewPrefsRequest. Verified by running `swag init`,
// not by reasoning.
//
// No tree guard saw this: review_prefs_schema_test.go looks at the field
// TAG, and nothing looked at types.
//
// # What is checked here
//
// Schema descriptions are matched against the EXPECTED, in both
// directions. A type whose doc-comment carries a swag directive MUST sit
// in expectedSchemaDescriptions with the exact text; a type that is not
// there has no right to carry a directive at all. The list is currently
// EMPTY, and that is a meaningful state: no type in the module declares a
// schema description on purpose. Want to declare one — write it here
// together with the text, and the schema description becomes a decision,
// not a side effect of a line wrap.
//
// ⚠ WHY THE SOURCE IS CHECKED, NOT THE GENERATED SPEC.
// The generator (`swag`, v1.16.4) DOES exist in the development
// environment, and the defect was found by it — but it is not in the
// integration harness or in CI jobs, so a one-shot run does not survive
// the next edit. What is checked is what the generator BUILDS the
// description FROM, with the same parse it uses itself.
//
// ⚠ THE WALK IS BY FACT, NOT BY A CATALOGUE LIST. Types that land in the
// spec do not live only in internal/handler: response schemas live in
// internal/notifications, internal/themes, internal/reminders and beyond.
// A catalogue list here would become the thirteenth hand-written run list,
// silent on a miss (exactly the trouble the T021 metrics-gate fix
// repaired). So the whole module tree is walked.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// schemaModuleRoot is the Go-module root relative to this package's directory.
const schemaModuleRoot = "../.."

// expectedSchemaDescriptions — types that DELIBERATELY declare a
// description of their schema, and the text they are required to declare.
//
// Key is the type name; value is the expected description in full. An
// empty list means "no type declares a schema description", and that is
// exactly what is currently true.
var expectedSchemaDescriptions = map[string]string{}

// schemaDirectives are the swag directives that, on a TYPE, change its
// schema or otherwise land in the spec. A doc-comment line starting with
// any of them is not prose.
var schemaDirectives = []string{
	"@Description", "@description",
	"@Summary", "@summary",
	"@name", "@Name",
	"@Router", "@Success", "@Failure", "@Param", "@Tags",
}

// typeDocDirective is a type whose doc-comment carries a directive.
type typeDocDirective struct {
	typeName  string
	file      string
	line      int
	directive string
	// text is what the generator will take as the schema description: the
	// join of lines starting with @Description, without the directive itself.
	text string
}

// TestOpenAPI_SchemaDescriptionsAreDeliberate — a type's schema
// description must be a decision, not a side effect of a line wrap.
func TestOpenAPI_SchemaDescriptionsAreDeliberate(t *testing.T) {
	t.Parallel()
	found, scanned := typeDocDirectives(t)
	if scanned == 0 {
		t.Fatal("the module-tree walk found not a single type declaration — the walk " +
			"is broken, not the code; without it the check is tautologically green")
	}
	for _, d := range found {
		want, deliberate := expectedSchemaDescriptions[d.typeName]
		if !deliberate {
			t.Errorf("%s:%d: type %s's doc-comment carries directive %s — the generator "+
				"will read this line as a SCHEMA DESCRIPTION and put it in the spec: %q. "+
				"If a schema description is needed on purpose — write the type into "+
				"expectedSchemaDescriptions together with the text. If this is prose in which "+
				"the directive name simply rode to the start of the line on a wrap — do not "+
				"write the directive name with a sigil in a TYPE doc-comment at all",
				d.file, d.line, d.typeName, d.directive, d.text)
			continue
		}
		if d.text != want {
			t.Errorf("%s:%d: schema description of type %s diverged from the expected.\n  tree: %q\n  want: %q",
				d.file, d.line, d.typeName, d.text, want)
		}
	}
	// The other side: an entry whose subject is no longer in the tree must
	// disappear with it, not guard emptiness.
	have := map[string]bool{}
	for _, d := range found {
		have[d.typeName] = true
	}
	for name := range expectedSchemaDescriptions {
		if !have[name] {
			t.Errorf("expectedSchemaDescriptions names type %s, but the tree has no such type "+
				"with a schema description — the entry outlived its subject and guards emptiness",
				name)
		}
	}
}

// typeDocDirectives walks the module tree and returns type declarations
// whose doc-comment carries a swag directive. The second result is how
// many type declarations were inspected at all (insurance against a
// broken walk).
func typeDocDirectives(t *testing.T) ([]typeDocDirective, int) {
	t.Helper()
	var out []typeDocDirective
	scanned := 0

	err := filepath.WalkDir(schemaModuleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Directories without module source are not worth walking.
			switch d.Name() {
			case ".git", "migrations", "docs", "scripts":
				if path != schemaModuleRoot {
					return filepath.SkipDir
				}
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			// A file that does not compile is the compiler's concern, not this gate's.
			return nil
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				scanned++
				// The generator looks at the declaration's doc-comment: for a
				// lone `type X …` it is on the GenDecl, for an element of a
				// `type ( … )` group — on the TypeSpec itself.
				var doc strings.Builder
				for _, g := range []*ast.CommentGroup{ts.Doc, gd.Doc} {
					if g != nil {
						doc.WriteString(g.Text())
						doc.WriteString("\n")
					}
				}
				directive, text := schemaDirectiveIn(doc.String())
				if directive == "" {
					continue
				}
				pos := fset.Position(ts.Pos())
				out = append(out, typeDocDirective{
					typeName:  ts.Name.Name,
					file:      filepath.ToSlash(path),
					line:      pos.Line,
					directive: directive,
					text:      text,
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("module-tree walk: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, scanned
}

// schemaDirectiveIn returns the first directive found at the START of a
// doc-comment line, and the schema-description text the generator will
// assemble from it.
//
// Start of the line is the load-bearing condition: a directive name
// sitting in the middle of a line is not counted as a directive by the
// generator, and forbidding it here would mean reddening on prose that
// EXPLAINS this very trap.
func schemaDirectiveIn(doc string) (directive, text string) {
	var parts []string
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, d := range schemaDirectives {
			if trimmed != d && !strings.HasPrefix(trimmed, d+" ") &&
				!strings.HasPrefix(trimmed, d+"\t") {
				continue
			}
			if directive == "" {
				directive = d
			}
			if strings.EqualFold(d, "@Description") {
				parts = append(parts, strings.TrimSpace(strings.TrimPrefix(trimmed, d)))
			}
		}
	}
	return directive, strings.Join(parts, " ")
}
