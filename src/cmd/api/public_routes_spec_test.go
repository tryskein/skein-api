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
// served document — status, documented headers and body schema; a response
// the document declares without content has no body.
//
// kin-openapi is a test-only dependency: no product package imports it.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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
		for _, rt := range publicapi.ProductRoutes(publicapi.Deps{}) {
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

// TestPublicResponsesMatchOpenAPI — row 105 (US-9). The success of every
// operation of routes.go, asked with a key of its right by the owner of the
// seeded project, passes openapi3filter.ValidateResponse against the served
// document with the status included; a status the document declares without
// content answers no body. Red when a response of an operation does not
// match the schema the document describes for it.
func TestPublicResponsesMatchOpenAPI(t *testing.T) {
	w := newClassWorld(t)
	doc := servedDocument(t, w.h)
	ops := map[string]publicOp{}
	for _, op := range publicOps {
		ops[op.id] = op
	}
	for _, rt := range publicapi.ProductRoutes(publicapi.Deps{}) {
		t.Run(rt.OperationID, func(t *testing.T) {
			operation := documentOperation(doc, rt.Method, rt.Path)
			if operation == nil {
				t.Fatalf("no operation %s %s in the document", rt.Method, rt.Path)
			}
			var c opCall
			key := ""
			if rt.OperationID == publicapi.OperationGetOpenAPI {
				c = opCall{method: http.MethodGet, path: publicapi.OpenAPIPath}
			} else {
				op, ok := ops[rt.OperationID]
				if !ok {
					t.Fatalf("no request for %s in publicOps", rt.OperationID)
				}
				// A key per operation: the run makes more requests than one
				// key's rate limit lets through in a minute.
				key = w.keyFor(op, w.key(w.owner, levelKey(apikeys.LevelManage)))
				c = w.call(op, w.p, key)
			}
			a := w.do(key, c, "", nil)
			if !success(a.status) {
				t.Fatalf("%s %s: %d %s, want a success", c.method, c.path, a.status, a.body)
			}
			req := httptest.NewRequest(c.method, c.path, http.NoBody)
			in := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{
					Request: req,
					Route: &routers.Route{Spec: doc, Path: rt.Path, PathItem: doc.Paths.Value(rt.Path),
						Method: rt.Method, Operation: operation},
				},
				Status:  a.status,
				Header:  a.header,
				Body:    io.NopCloser(strings.NewReader(string(a.body))),
				Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
			}
			if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
				t.Errorf("%d response does not match the document: %v\nbody: %s", a.status, err, a.body)
			}
			if resp := operation.Responses.Status(a.status); resp != nil && resp.Value != nil && len(resp.Value.Content) == 0 && len(a.body) != 0 {
				t.Errorf("%d is declared without content, the response has a body: %s", a.status, a.body)
			}
		})
	}
}
