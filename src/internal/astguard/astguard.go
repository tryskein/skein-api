// Package astguard — parse a package's sources for STRUCTURAL GUARDS:
// checks of the form "someone reads the declared value", "the stage is
// incremented somewhere".
//
// # Why a package, not a copy in every test
//
// Before this, such guards counted IDENTIFIER OCCURRENCES IN THE TEXT of
// the concatenated source and required two: "the declaration plus at
// least one call". That method is unsound, and it is unsound in this
// repository specifically: house style is to declare with a doc comment
// that starts with the identifier's own name
//
//	// stageForm — the form transaction rolled back …
//	stageForm = registerSweepStage("form")
//
// The comment IS the second occurrence. The threshold is met BEFORE the
// code ever reaches a call, so both such checks could never go red: a
// stage with not a single increment, and an outcome read only by its own
// comment, stayed green (review A/9, findings A.1 and A.2).
//
// Raising the threshold is not an option — it would break on an
// identifier mentioned in two comments, i.e. swap blindness for a false
// reject. What must be counted is USES, and comments are not tree nodes
// at all: go/ast never puts them in expressions, so the question
// "comment or code" stops arising.
//
// go/ast already carries similar gates in this repo
// (issues/review_visibility_test.go, outbox/outcome_set_test.go); a second
// method is not started
// here — ONE shared one is, so a third copy of the parse does not drift
// from the first two.
package astguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
)

// Package — parsed non-test files of one directory.
type Package struct {
	Fset  *token.FileSet
	Files []*ast.File

	// declared — nodes standing in a name DECLARATION position. They are
	// not counted as uses: a declaration with not a single read is exactly
	// the case the guards catch.
	declared map[*ast.Ident]bool
	// selected — `Sel` nodes in `x.Sel` expressions. This is a MEMBER
	// name of another value, not a reference to this package's
	// declaration: the struct field `c.stage` does not make the constant
	// `stage` used.
	selected map[*ast.Ident]bool
}

// Parse parses the non-test files of a directory. An empty result is an
// error, not an empty package: a guard with nothing to look at is
// tautologically green.
func Parse(dir string) (*Package, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", dir, err)
	}
	p := &Package{
		Fset:     fset,
		declared: map[*ast.Ident]bool{},
		selected: map[*ast.Ident]bool{},
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			p.Files = append(p.Files, file)
		}
	}
	if len(p.Files) == 0 {
		return nil, fmt.Errorf("no non-test file parsed in %s", dir)
	}
	p.index()
	return p, nil
}

// ParseFile parses ONE file.
//
// Needed where the assertion addresses a specific file, not a package:
// "in this operation the role does not participate in the decision" is a
// property of the operation's file, and a neighbouring file of the same
// package is allowed to read the role.
func ParseFile(path string) (*Package, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	p := &Package{
		Fset:     fset,
		Files:    []*ast.File{file},
		declared: map[*ast.Ident]bool{},
		selected: map[*ast.Ident]bool{},
	}
	p.index()
	return p, nil
}

// UsedIdents — every USED name with its use count.
//
// Exists for assertions of the form "not a single identifier of this
// kind is here": the forbidden names cannot be listed in advance, but
// the tree can be asked what is in it. Comments do not land here, and
// that is a load-bearing property: a ban checked as a substring goes
// red on a comment EXPLAINING why the forbidden thing is not here
// (observed during repair A/9).
func (p *Package) UsedIdents() map[string]int {
	out := map[string]int{}
	for _, file := range p.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			id, ok := node.(*ast.Ident)
			if !ok || p.declared[id] || p.selected[id] {
				return true
			}
			out[id.Name]++
			return true
		})
	}
	return out
}

// SelectedNames — member names in `x.Sel` expressions, with occurrence
// counts.
//
// Complements UsedIdents: `auth.IsAdmin(ctx)` puts `auth` in the used
// set, and `IsAdmin` here. An assertion "the role does not participate
// in the decision" must see both halves.
func (p *Package) SelectedNames() map[string]int {
	out := map[string]int{}
	for id := range p.selected {
		out[id.Name]++
	}
	return out
}

// index marks declaration positions and selector-member positions.
func (p *Package) index() {
	mark := func(set map[*ast.Ident]bool, ids ...*ast.Ident) {
		for _, id := range ids {
			if id != nil {
				set[id] = true
			}
		}
	}
	for _, file := range p.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec:
				mark(p.declared, node.Names...)
			case *ast.TypeSpec:
				mark(p.declared, node.Name)
			case *ast.FuncDecl:
				mark(p.declared, node.Name)
			case *ast.Field:
				mark(p.declared, node.Names...)
			case *ast.ImportSpec:
				mark(p.declared, node.Name)
			case *ast.LabeledStmt:
				mark(p.declared, node.Label)
			case *ast.AssignStmt:
				if node.Tok == token.DEFINE {
					for _, lhs := range node.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							mark(p.declared, id)
						}
					}
				}
			case *ast.RangeStmt:
				if node.Tok == token.DEFINE {
					for _, e := range []ast.Expr{node.Key, node.Value} {
						if id, ok := e.(*ast.Ident); ok {
							mark(p.declared, id)
						}
					}
				}
			case *ast.SelectorExpr:
				mark(p.selected, node.Sel)
			}
			return true
		})
	}
}

// CountUses returns the number of USES of an identifier: *ast.Ident
// nodes with that name that are not in a declaration position and not a
// selector member.
//
// Comments do not land here by construction — they are not expression
// nodes. That is why a use count answers "does the code read this?",
// which a text-occurrence count did not.
func (p *Package) CountUses(ident string) int {
	n := 0
	for _, file := range p.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			id, ok := node.(*ast.Ident)
			if !ok || id.Name != ident {
				return true
			}
			if p.declared[id] || p.selected[id] {
				return true
			}
			n++
			return true
		})
	}
	return n
}

// CallArgIdents returns the names of identifiers that appear in the
// ARGUMENTS of calls of the form `recv.method(...)`, one name per
// occurrence.
//
// Needed where "use" is too weak: a sweep stage named in an enum or in
// its own String() is used — but not incremented. The guard's question
// is "is there a branch that puts this stage into the counter", and
// only the call form answers it.
func (p *Package) CallArgIdents(recv, method string) []string {
	var out []string
	for _, file := range p.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != method {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || x.Name != recv {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(a ast.Node) bool {
					if id, ok := a.(*ast.Ident); ok && !p.selected[id] {
						out = append(out, id.Name)
					}
					return true
				})
			}
			return true
		})
	}
	return out
}
