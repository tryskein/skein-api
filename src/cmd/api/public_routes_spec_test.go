package main

// public_routes_spec_test.go — the assembled public router against the
// OpenAPI document the instance serves (sk-878.3.1 T029, rows 105 and 107;
// US-9, SC-009, R-8).
//
// Row 107 is the source-reading check the owner permitted (AGENTS.md,
// "## Testing"): the routes chi.Walk finds in the product public router and
// the operations of GET /api/v1/openapi.json are the same set both ways; each
// operation names the operationId and the key right of its row of routes.go
// in the requirement of the SkeinApiKey scheme (getOpenApi has none); the
// force=true mode is a query parameter of exactly the guarded operations
// (spec, "Passing force"); the document is a valid OpenAPI 3.1 document for
// kin-openapi.
//
// Row 105 is the contract of the responses: the success of every operation
// of routes.go, asked of the product public server over a seeded database,
// passes the response validation of kin-openapi (openapi3filter) against the
// served document — status, documented headers, body schema and its required
// members — and carries no member the document does not declare; a response
// the document declares without content has no body. Batches with a refused
// item and the null branches are asked too.
//
// kin-openapi is a test-only dependency: no product package imports it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/go-chi/chi/v5"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/publicapi"
)

// apiKeyScheme is the name of the security scheme of the API key in the
// public document.
const apiKeyScheme = "SkeinApiKey"

// servedDocument is the OpenAPI document h answers on GET
// /api/v1/openapi.json, loaded by kin-openapi.
func servedDocument(t *testing.T, h http.Handler) *openapi3.T {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, publicapi.OpenAPIPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", publicapi.OpenAPIPath, rec.Code, rec.Body.String())
	}
	doc, err := openapi3.NewLoader().LoadFromData(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("the served document does not load: %v", err)
	}
	return doc
}

// documentOperation is the operation of doc at the OpenAPI path and method,
// nil when the document has none.
func documentOperation(doc *openapi3.T, method, path string) *openapi3.Operation {
	item := doc.Paths.Value(path)
	if item == nil {
		return nil
	}
	return item.GetOperation(method)
}

// TestPublicRoutesMatchOpenAPI — row 107 (US-9, SC-009, R-8). Red when a
// route of the public router has no operation in the document, an operation
// of the document has no route, an operationId or the key right of an
// operation differs from routes.go, force is a query parameter of an
// operation that is not guarded (or is missing on a guarded one), the scheme
// of the key is not declared, or the document is not valid OpenAPI 3.1.
func TestPublicRoutesMatchOpenAPI(t *testing.T) {
	srv, err := publicServer(publicConfig(true, freePort(t), ""), nil, publicapi.Deps{}, slog.New(&recordingHandler{}))
	if err != nil || srv == nil {
		t.Fatalf("publicServer = %v, %v; want a server", srv, err)
	}
	doc := servedDocument(t, srv.Handler)

	t.Run("valid OpenAPI 3.1", func(t *testing.T) {
		if !doc.IsOpenAPI31OrLater() || doc.IsOpenAPI32OrLater() {
			t.Errorf("openapi %q, want 3.1", doc.OpenAPI)
		}
		if err := doc.Validate(context.Background(), openapi3.EnableMultiError()); err != nil {
			t.Errorf("the document is not valid: %v", err)
		}
	})

	t.Run("the key scheme", func(t *testing.T) {
		var ref *openapi3.SecuritySchemeRef
		if doc.Components != nil {
			ref = doc.Components.SecuritySchemes[apiKeyScheme]
		}
		if ref == nil || ref.Value == nil {
			t.Fatalf("no security scheme %s", apiKeyScheme)
		}
		if s := ref.Value; s.Type != "apiKey" || s.In != "header" || s.Name != auth.HeaderAPIKey {
			t.Errorf("%s: type %q in %q name %q, want apiKey in header %s", apiKeyScheme, s.Type, s.In, s.Name, auth.HeaderAPIKey)
		}
		if len(doc.Security) != 0 {
			t.Errorf("a document-wide security requirement %v: every operation states its own", doc.Security)
		}
	})

	t.Run("routes and operations both ways", func(t *testing.T) {
		mux, ok := srv.Handler.(chi.Routes)
		if !ok {
			t.Fatalf("the public handler %T is not a chi router", srv.Handler)
		}
		walked := map[string]bool{}
		if err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			walked[method+" "+route] = true
			return nil
		}); err != nil {
			t.Fatalf("chi.Walk: %v", err)
		}
		if len(walked) == 0 {
			t.Fatal("the public router has no route")
		}
		described := map[string]bool{}
		for path, item := range doc.Paths.Map() {
			for method := range item.Operations() {
				described[strings.ToUpper(method)+" "+path] = true
			}
		}
		for r := range walked {
			if !described[r] {
				t.Errorf("route %s has no operation in the document", r)
			}
		}
		for d := range described {
			if !walked[d] {
				t.Errorf("operation %s of the document has no route", d)
			}
		}
	})

	t.Run("operationId, key right and force of each route", func(t *testing.T) {
		routes := publicapi.ProductRoutes(publicapi.Deps{})
		if len(routes) == 0 {
			t.Fatal("the product route table is empty")
		}
		for _, rt := range routes {
			op := documentOperation(doc, rt.Method, rt.Path)
			if op == nil {
				t.Errorf("%s: no operation %s %s in the document", rt.OperationID, rt.Method, rt.Path)
				continue
			}
			if op.OperationID != rt.OperationID {
				t.Errorf("%s %s: operationId %q in the document, %q in routes.go", rt.Method, rt.Path, op.OperationID, rt.OperationID)
			}
			wantSecurity(t, rt, op)
			var force *openapi3.Parameter
			for _, p := range op.Parameters {
				if p.Value != nil && p.Value.Name == "force" {
					force = p.Value
				}
			}
			switch {
			case rt.Guarded && (force == nil || force.In != openapi3.ParameterInQuery):
				t.Errorf("%s: guarded, but the document has no query parameter force", rt.OperationID)
			case !rt.Guarded && force != nil:
				t.Errorf("%s: not guarded, but the document has a parameter force in %s", rt.OperationID, force.In)
			}
		}
	})
}

// wantSecurity asserts the security requirement of op against its route:
// none for an operation without a key; otherwise exactly one requirement,
// the SkeinApiKey scheme with the route's key right as its one scope, or
// with no scope for an operation that needs no right of the key (getMe).
func wantSecurity(t *testing.T, rt publicapi.Route, op *openapi3.Operation) {
	t.Helper()
	if rt.Auth == publicapi.AuthNone {
		if op.Security != nil && len(*op.Security) != 0 {
			t.Errorf("%s: needs no key, but the document requires %v", rt.OperationID, *op.Security)
		}
		return
	}
	if op.Security == nil || len(*op.Security) != 1 {
		t.Errorf("%s: security %v, want one requirement of %s", rt.OperationID, op.Security, apiKeyScheme)
		return
	}
	req := (*op.Security)[0]
	scopes, ok := req[apiKeyScheme]
	if !ok || len(req) != 1 {
		t.Errorf("%s: security %v, want only %s", rt.OperationID, req, apiKeyScheme)
		return
	}
	want := []string{}
	if right := rt.Key.Right.String(); right != "none" {
		want = []string{right}
	}
	if !slices.Equal(scopes, want) {
		t.Errorf("%s: %s scopes %v in the document, right %v in routes.go", rt.OperationID, apiKeyScheme, scopes, want)
	}
}

// responseProblems are the ways answer a, the response of rt to c, differs
// from the document: openapi3filter.ValidateResponse with the status
// included (type, nullable, required members, documented headers, content
// type), a body on a status declared without content, and every member of a
// response object the schema of that object does not declare (kin-openapi
// has no option to refuse them: the schemas leave additionalProperties
// open). None — the response matches.
func responseProblems(doc *openapi3.T, rt publicapi.Route, c opCall, a answer) []string {
	operation := documentOperation(doc, rt.Method, rt.Path)
	if operation == nil {
		return []string{"no operation " + rt.Method + " " + rt.Path + " in the document"}
	}
	var problems []string
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: httptest.NewRequest(c.method, c.path, http.NoBody),
			Route: &routers.Route{Spec: doc, Path: rt.Path, PathItem: doc.Paths.Value(rt.Path),
				Method: rt.Method, Operation: operation},
		},
		Status:  a.status,
		Header:  a.header,
		Body:    io.NopCloser(bytes.NewReader(a.body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		problems = append(problems, err.Error())
	}
	resp := operation.Responses.Status(a.status)
	if resp == nil || resp.Value == nil {
		return problems
	}
	if len(resp.Value.Content) == 0 {
		if len(a.body) != 0 {
			problems = append(problems, "the status is declared without content, the response has a body")
		}
		return problems
	}
	media := resp.Value.Content.Get(a.header.Get("Content-Type"))
	if media == nil || media.Schema == nil {
		return problems
	}
	var body any
	if err := json.Unmarshal(a.body, &body); err != nil {
		return append(problems, "the body is not JSON: "+err.Error())
	}
	undeclaredMembers(media.Schema, body, "", &problems)
	return problems
}

// undeclaredMembers appends to out each member of the objects of v that s
// does not declare, recursively through properties, items, additional
// properties and the non-null variant of anyOf / oneOf. An object schema
// without properties (free-form) declares any member.
func undeclaredMembers(s *openapi3.SchemaRef, v any, at string, out *[]string) {
	if s == nil || s.Value == nil || v == nil {
		return
	}
	sch := s.Value
	for _, variants := range [][]*openapi3.SchemaRef{sch.AnyOf, sch.OneOf} {
		for _, variant := range variants {
			if variant.Value != nil && !variant.Value.Type.Is(openapi3.TypeNull) {
				undeclaredMembers(variant, v, at, out)
			}
		}
	}
	switch val := v.(type) {
	case map[string]any:
		if len(sch.Properties) == 0 {
			if extra := sch.AdditionalProperties.Schema; extra != nil {
				for k, m := range val {
					undeclaredMembers(extra, m, at+"/"+k, out)
				}
			}
			return
		}
		for k, m := range val {
			prop, ok := sch.Properties[k]
			if !ok {
				*out = append(*out, "member "+at+"/"+k+" is not declared in the schema")
				continue
			}
			undeclaredMembers(prop, m, at+"/"+k, out)
		}
	case []any:
		for i, m := range val {
			undeclaredMembers(sch.Items, m, at+"/"+strconv.Itoa(i), out)
		}
	}
}

// TestPublicResponsesMatchOpenAPI — row 105 (US-9). The success of every
// operation of routes.go, asked with a key of its right by the owner of the
// seeded project, matches the served document (responseProblems): status,
// headers, type and nullability of each member, the members the document
// requires, and no member the document does not declare. The branches one
// success does not reach are asked as well: a batch with a refused item,
// resolveProject without a match, releaseIssueLock of an issue nobody holds.
// A response changed by one undeclared or one missing required member is
// refused by the same check. Red when a response of an operation does not
// match the schema the document describes for it.
func TestPublicResponsesMatchOpenAPI(t *testing.T) {
	w := newClassWorld(t)
	doc := servedDocument(t, w.h)
	ops := map[string]publicOp{}
	for _, op := range publicOps {
		ops[op.id] = op
	}
	routes := map[string]publicapi.Route{}
	for _, rt := range publicapi.ProductRoutes(publicapi.Deps{}) {
		routes[rt.OperationID] = rt
	}
	if len(routes) == 0 {
		t.Fatal("the product route table is empty")
	}
	// keyOf is a fresh key for op: the run makes more requests than one
	// key's rate limit lets through in a minute.
	keyOf := func(op publicOp) string { return w.keyFor(op, w.key(w.owner, levelKey(apikeys.LevelManage))) }
	check := func(t *testing.T, rt publicapi.Route, c opCall, a answer) {
		t.Helper()
		if !success(a.status) {
			t.Fatalf("%s %s: %d %s, want a success", c.method, c.path, a.status, a.body)
		}
		for _, p := range responseProblems(doc, rt, c, a) {
			t.Errorf("%d response does not match the document: %s\nbody: %s", a.status, p, a.body)
		}
	}
	for id, rt := range routes {
		t.Run(id, func(t *testing.T) {
			if id == publicapi.OperationGetOpenAPI {
				c := opCall{method: http.MethodGet, path: publicapi.OpenAPIPath}
				check(t, rt, c, w.do("", c, "", nil))
				return
			}
			op, ok := ops[id]
			if !ok {
				t.Fatalf("no request for %s in publicOps", id)
			}
			key := keyOf(op)
			c := w.call(op, w.p, key)
			check(t, rt, c, w.do(key, c, "", nil))
		})
	}

	// The branches one success does not reach. want asserts that the
	// answer took the branch, so the check is not of the plain success.
	refusedItem := func(t *testing.T, body map[string]any) {
		t.Helper()
		results, _ := body["results"].([]any)
		for _, r := range results {
			if row, _ := r.(map[string]any); row["ok"] == false && row["error"] != nil {
				return
			}
		}
		t.Fatalf("no refused item among the results: %v", body)
	}
	base := "/api/v1/"
	for _, b := range []struct {
		name, op string
		call     func(key string) opCall
		want     func(t *testing.T, body map[string]any)
	}{
		{"bulkUpdateIssues with a stale item", "bulkUpdateIssues", func(string) opCall {
			ok, stale := w.freshIssue(w.p), w.freshIssue(w.p)
			return opCall{http.MethodPost, base + "issues/bulk-update", map[string]any{"items": []map[string]any{
				{"id": ok, "expectedVersion": w.issueVersion(ok), "title": w.fresh("bulk")},
				{"id": stale, "expectedVersion": w.issueVersion(stale) + 5, "title": w.fresh("bulk")}}}, nil}
		}, refusedItem},
		{"bulkCloseIssues with a stale item", "bulkCloseIssues", func(string) opCall {
			ok, stale := w.freshIssue(w.p), w.freshIssue(w.p)
			return opCall{http.MethodPost, base + "issues/bulk-close", map[string]any{"items": []map[string]any{
				{"id": ok, "expectedVersion": w.issueVersion(ok), "reason": "done", "closeOutcome": "done"},
				{"id": stale, "expectedVersion": w.issueVersion(stale) + 5, "reason": "done", "closeOutcome": "done"}}}, nil}
		}, refusedItem},
		{"bulkAddDependencies with an item to a missing issue", "bulkAddDependencies", func(string) opCall {
			from := w.freshIssue(w.p)
			return opCall{http.MethodPost, base + "issues/dependencies/bulk-add", map[string]any{"items": []map[string]any{
				{"fromId": from, "toId": w.pPrefix + "-999999", "type": "related"},
				{"fromId": from, "toId": w.freshIssue(w.p), "type": "related"}}}, nil}
		}, refusedItem},
		{"resolveProject without a match", "resolveProject", func(string) opCall {
			return opCall{http.MethodGet, base + "projects/resolve?q=" + w.fresh("nomatch"), nil, nil}
		}, func(t *testing.T, body map[string]any) {
			if m, present := body["match"]; !present || m != nil {
				t.Fatalf("match %v, want null: %v", m, body)
			}
		}},
		{"releaseIssueLock of an issue nobody holds", "releaseIssueLock", func(string) opCall {
			return opCall{http.MethodPost, base + "issues/" + w.freshIssue(w.p) + "/release-lock", nil, nil}
		}, func(t *testing.T, body map[string]any) {
			if body["lockedBy"] != nil {
				t.Fatalf("lockedBy %v, want none: %v", body["lockedBy"], body)
			}
		}},
	} {
		t.Run(b.name, func(t *testing.T) {
			op := ops[b.op]
			key := keyOf(op)
			c := b.call(key)
			a := w.do(key, c, "", nil)
			check(t, routes[b.op], c, a)
			b.want(t, a.object(t))
		})
	}

	// The check itself: a real response with one undeclared member, and
	// with one required member removed, is refused.
	t.Run("a changed response is refused", func(t *testing.T) {
		op := ops[publicapi.OperationGetIssue]
		key := keyOf(op)
		c := w.call(op, w.p, key)
		a := w.do(key, c, "", nil)
		check(t, routes[publicapi.OperationGetIssue], c, a)
		for name, change := range map[string]func(m map[string]any){
			"an undeclared member":   func(m map[string]any) { m["undeclaredMember"] = 1 },
			"a required member gone": func(m map[string]any) { delete(m, "title") },
		} {
			m := a.object(t)
			change(m)
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			changed := answer{status: a.status, header: a.header, body: raw}
			if len(responseProblems(doc, routes[publicapi.OperationGetIssue], c, changed)) == 0 {
				t.Errorf("%s: the changed response passes the check", name)
			}
		}
	})
}
