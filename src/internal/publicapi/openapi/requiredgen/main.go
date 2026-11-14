// Command requiredgen declares the required members of the object schemas of
// the public OpenAPI document. It is a step of
// scripts/generate-public-openapi.sh, run after swag v2.
//
// swag v2 marks a field required only by a binding or validate tag, or every
// field with --requiredByDefault, omitempty included. Neither says what a
// response carries: encoding/json writes every field without omitempty (or
// omitzero) on every response, and may leave out a field with it. So for each
// schema a response refers to (directly or through other schemas), named after
// a struct of this module (pkg.Type, or pkg.Type-args for an instance of a
// generic type), this command reads the struct with go/ast —
// promoted fields of embedded structs included, json "-" and
// swaggerignore:"true" fields left out — and sets "required" to the members
// written without omitempty or omitzero. Request bodies are left as swag
// wrote them: a client may leave out a member the struct has no omitempty
// on. A response schema with no such struct, a struct named twice, a schema
// shared by a request and a response, or a required member the schema does
// not describe stops the command: the document would otherwise describe a
// response it does not match.
//
// A response shortened on request (fields=, Prefer: return=minimal) leaves
// out required members by design; the document describes the full
// response.
//
// Usage (from src/): go run ./internal/publicapi/openapi/requiredgen <document.json>
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// modulePath is the module of this repository (src/go.mod).
const modulePath = "skein-api"

// pkgFile is one parsed file of a package.
type pkgFile struct {
	dir  string
	file *ast.File
}

// typeDecl is a declared type with the file it is declared in.
type typeDecl struct {
	spec *ast.TypeSpec
	in   pkgFile
}

// index is the types of the module's packages by package name, and the
// package name of each directory.
type index struct {
	types   map[string][]typeDecl // "pkg.Type"
	dirName map[string]string     // directory → package name
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: requiredgen <document.json>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "requiredgen:", err)
		os.Exit(1)
	}
}

func run(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if len(schemas) == 0 {
		return errors.New("the document has no components.schemas")
	}
	idx, err := buildIndex(".")
	if err != nil {
		return err
	}
	answered, asked := map[string]bool{}, map[string]bool{}
	paths, _ := doc["paths"].(map[string]any)
	for _, item := range paths {
		ops, _ := item.(map[string]any)
		for _, o := range ops {
			op, _ := o.(map[string]any)
			collectRefs(op["responses"], schemas, answered)
			collectRefs(op["requestBody"], schemas, asked)
			collectRefs(op["parameters"], schemas, asked)
		}
	}
	for name := range answered {
		if asked[name] {
			return fmt.Errorf("schema %s describes both a request and a response", name)
		}
	}
	for name, s := range schemas {
		if !answered[name] {
			continue // a request body: a member without omitempty may still be left out by the client
		}
		schema, _ := s.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			continue // not an object with members
		}
		fields, err := idx.schemaFields(name)
		if err != nil {
			return fmt.Errorf("schema %s: %w", name, err)
		}
		var required []string
		for _, f := range fields {
			if !f.required {
				continue
			}
			if _, ok := props[f.name]; !ok {
				return fmt.Errorf("schema %s: member %q is always written but not described", name, f.name)
			}
			required = append(required, f.name)
		}
		slices.Sort(required)
		if len(required) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = required
		}
	}
	out, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// collectRefs adds to into the component schemas node refers to, and the
// schemas those refer to.
func collectRefs(node any, schemas map[string]any, into map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			if name, ok := strings.CutPrefix(ref, "#/components/schemas/"); ok && !into[name] {
				into[name] = true
				collectRefs(schemas[name], schemas, into)
			}
		}
		for _, v := range n {
			collectRefs(v, schemas, into)
		}
	case []any:
		for _, v := range n {
			collectRefs(v, schemas, into)
		}
	}
}

// buildIndex parses every non-test Go file under root/internal.
func buildIndex(root string) (*index, error) {
	idx := &index{types: map[string][]typeDecl{}, dirName: map[string]string{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(p))
		idx.dirName[dir] = f.Name.Name
		in := pkgFile{dir: dir, file: f}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				key := f.Name.Name + "." + ts.Name.Name
				idx.types[key] = append(idx.types[key], typeDecl{spec: ts, in: in})
			}
		}
		return nil
	})
	return idx, err
}

// field is one member a struct writes.
type field struct {
	name     string
	required bool
}

// schemaFields are the members of the struct a schema is named after.
func (x *index) schemaFields(schema string) ([]field, error) {
	name, _, _ := strings.Cut(schema, "-") // pkg.Type-args → pkg.Type
	decl, err := x.lookup(name)
	if err != nil {
		return nil, err
	}
	return x.declFields(decl, map[string]bool{})
}

func (x *index) lookup(name string) (typeDecl, error) {
	decls := x.types[name]
	switch len(decls) {
	case 0:
		return typeDecl{}, fmt.Errorf("no type %s in the module", name)
	case 1:
		return decls[0], nil
	}
	return typeDecl{}, fmt.Errorf("type %s is declared in more than one package", name)
}

// declFields are the members written by the struct decl declares, after
// alias and defined-type resolution.
func (x *index) declFields(decl typeDecl, seen map[string]bool) ([]field, error) {
	key := decl.in.dir + "." + decl.spec.Name.Name
	if seen[key] {
		return nil, fmt.Errorf("type %s embeds itself", decl.spec.Name.Name)
	}
	seen[key] = true
	defer delete(seen, key)
	st, ok := decl.spec.Type.(*ast.StructType)
	if !ok {
		target, err := x.resolve(decl.spec.Type, decl.in)
		if err != nil {
			return nil, fmt.Errorf("%s is not a struct: %w", decl.spec.Name.Name, err)
		}
		return x.declFields(target, seen)
	}
	var own, promoted []field
	for _, f := range st.Fields.List {
		tag := reflect.StructTag("")
		if f.Tag != nil {
			v, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				return nil, err
			}
			tag = reflect.StructTag(v)
		}
		jsonName, opts, _ := strings.Cut(tag.Get("json"), ",")
		if jsonName == "-" && opts == "" || tag.Get("swaggerignore") == "true" {
			continue
		}
		optional := slices.Contains(strings.Split(opts, ","), "omitempty") ||
			slices.Contains(strings.Split(opts, ","), "omitzero")
		if len(f.Names) == 0 && jsonName == "" {
			target, err := x.resolve(f.Type, decl.in)
			if err != nil {
				return nil, fmt.Errorf("embedded field of %s: %w", decl.spec.Name.Name, err)
			}
			sub, err := x.declFields(target, seen)
			if err != nil {
				return nil, err
			}
			promoted = append(promoted, sub...)
			continue
		}
		names := f.Names
		if len(names) == 0 {
			names = []*ast.Ident{ast.NewIdent(typeName(f.Type))}
		}
		for _, n := range names {
			if !n.IsExported() {
				continue
			}
			name := jsonName
			if name == "" {
				name = n.Name
			}
			own = append(own, field{name: name, required: !optional})
		}
	}
	// A member of the struct itself hides a promoted one of the same name.
	out := own
	for _, p := range promoted {
		if !slices.ContainsFunc(out, func(f field) bool { return f.name == p.name }) {
			out = append(out, p)
		}
	}
	return out, nil
}

// resolve finds the declaration of the type expression e, written in file
// in: a type of the same package, of another package of the module, a
// pointer to one, or an instance of a generic one.
func (x *index) resolve(e ast.Expr, in pkgFile) (typeDecl, error) {
	switch t := e.(type) {
	case *ast.StarExpr:
		return x.resolve(t.X, in)
	case *ast.IndexExpr:
		return x.resolve(t.X, in)
	case *ast.IndexListExpr:
		return x.resolve(t.X, in)
	case *ast.Ident:
		return x.lookup(x.dirName[in.dir] + "." + t.Name)
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok {
			return typeDecl{}, fmt.Errorf("unsupported type expression %T", t.X)
		}
		dir, err := importDir(in.file, pkg.Name)
		if err != nil {
			return typeDecl{}, err
		}
		name, ok := x.dirName[dir]
		if !ok {
			return typeDecl{}, fmt.Errorf("package %s is not a package of the module", pkg.Name)
		}
		return x.lookup(name + "." + t.Sel.Name)
	}
	return typeDecl{}, fmt.Errorf("unsupported type expression %T", e)
}

// importDir is the directory, relative to src/, of the module package f
// imports as name.
func importDir(f *ast.File, name string) (string, error) {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return "", err
		}
		as := filepath.Base(p)
		if imp.Name != nil {
			as = imp.Name.Name
		}
		if as != name {
			continue
		}
		rel, ok := strings.CutPrefix(p, modulePath+"/")
		if !ok {
			return "", fmt.Errorf("%s (%s) is outside the module", name, p)
		}
		return rel, nil
	}
	return "", fmt.Errorf("no import named %s", name)
}

// typeName is the name of an embedded field of type e (its type's name).
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return ""
}
