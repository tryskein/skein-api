package publicapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"skein-api/internal/authz"
	"skein-api/internal/errcode"
	"skein-api/internal/publicapi"
)

// productRouter is the router of the product table, as the public server
// serves it.
func productRouter(t *testing.T) http.Handler {
	t.Helper()
	doc, err := publicapi.NewOpenAPIDocument("")
	if err != nil {
		t.Fatal(err)
	}
	return publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second}, publicapi.ProductRoutes(doc))
}

// testRoutes is a table of this test's own: a POST collection that reads
// its body and a GET item, both answering issue_not_found — the collection
// as a reference in the body, the item as the object of the path.
func testRoutes() []publicapi.Route {
	notFound := func(origin publicapi.Origin) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.IssueNotFound, Origin: origin})
		})
	}
	return []publicapi.Route{
		{
			OperationID: "testCreateThing", Method: http.MethodPost, Path: "/api/v1/things",
			Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightWrite},
			Handler: notFound(publicapi.OriginBody),
		},
		{
			OperationID: "testGetThing", Method: http.MethodGet, Path: "/api/v1/things/{thing}",
			Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightRead},
			Handler: notFound(publicapi.OriginPath),
		},
	}
}

func testRouter() http.Handler {
	return publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second}, testRoutes())
}

// problemCode decodes a problem response and returns its code, failing the
// test when the response is not application/problem+json.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != publicapi.ProblemContentType {
		t.Fatalf("Content-Type %q, want %s (body %q)", ct, publicapi.ProblemContentType, rec.Body.String())
	}
	var body struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Status != rec.Code {
		t.Errorf("problem status member %d, response status %d", body.Status, rec.Code)
	}
	return body.Code
}

// TestRouter_ServesNoInternalRoute — row 50 (INV-10, SC-018, public part).
// The handler of the public port, built from the product table, answers 404
// to the internal API, MCP, the agent API, the metrics and the health
// probes: the public router did not inherit the internal routes.
func TestRouter_ServesNoInternalRoute(t *testing.T) {
	h := productRouter(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/ui/v1/issues"},
		{http.MethodGet, "/ui/v1/me"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/mcp"},
		{http.MethodGet, "/agent/v1/inbox"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/ready"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404", rec.Code)
			}
			if code := problemCode(t, rec); code != "route_not_found" {
				t.Errorf("code %q, want route_not_found", code)
			}
		})
	}
}

// TestRouter_NoCORSHeaders — row 51 (SC-029). No response of the public
// router carries an Access-Control-* header: neither a preflight (OPTIONS
// with Origin and Access-Control-Request-Method) nor a request with Origin,
// on a route of the table, the product's document or an unknown path.
func TestRouter_NoCORSHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		h      http.Handler
		method string
		path   string
	}{
		{"preflight on a route", testRouter(), http.MethodOptions, "/api/v1/things"},
		{"preflight on the document", productRouter(t), http.MethodOptions, publicapi.OpenAPIPath},
		{"preflight on an unknown path", productRouter(t), http.MethodOptions, "/api/v1/nothing"},
		{"GET with Origin", productRouter(t), http.MethodGet, publicapi.OpenAPIPath},
		{"POST with Origin", testRouter(), http.MethodPost, "/api/v1/things"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.method == http.MethodPost {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Origin", "https://elsewhere.example")
			req.Header.Set("Access-Control-Request-Method", http.MethodPost)
			req.Header.Set("Access-Control-Request-Headers", "x-skein-api-key, content-type")
			rec := httptest.NewRecorder()
			tc.h.ServeHTTP(rec, req)
			if len(rec.Header()) == 0 {
				t.Fatalf("response without headers (status %d)", rec.Code)
			}
			for name := range rec.Header() {
				if strings.HasPrefix(strings.ToLower(name), "access-control-") {
					t.Errorf("response %d carries CORS header %s: %q", rec.Code, name, rec.Header().Get(name))
				}
			}
		})
	}
}

// TestRouter_TransportCodes — row 52. An unknown route answers 404, a method
// the path does not have 405 (with Allow), a body over 1 MiB 413, a body that
// is not application/json 415 — each as application/problem+json with its
// code, and none reaches the handler of the route.
func TestRouter_TransportCodes(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", publicapi.MaxBodyBytes) + `"}`
	for _, tc := range []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{"unknown route", http.MethodGet, "/api/v1/nothing", "", "", http.StatusNotFound, "route_not_found"},
		{"unknown method", http.MethodDelete, "/api/v1/things", "", "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"body over 1 MiB", http.MethodPost, "/api/v1/things", "application/json", big, http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"body not JSON", http.MethodPost, "/api/v1/things", "text/plain", `{}`, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"body without a type", http.MethodPost, "/api/v1/things", "", `{}`, http.StatusUnsupportedMediaType, "unsupported_media_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()
			testRouter().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if code := problemCode(t, rec); code != tc.code {
				t.Errorf("code %q, want %q", code, tc.code)
			}
			if tc.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodPost {
				t.Errorf("Allow %q, want %q", rec.Header().Get("Allow"), http.MethodPost)
			}
		})
	}
}

// TestRouter_SameCodeByOrigin — row 53 (OST-2-5, OST-3-3). On routes of a
// table passed to the builder, the same code answers 404 when the object is
// the one of the path and 422 when it is a reference in the body: the status
// is chosen by the code and its origin, not by the code alone.
func TestRouter_SameCodeByOrigin(t *testing.T) {
	h := testRouter()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/things/sk-1", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("object of the path: status %d, want 404", rec.Code)
	}
	if code := problemCode(t, rec); code != "issue_not_found" {
		t.Errorf("object of the path: code %q, want issue_not_found", code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/things", strings.NewReader(`{"parent":"sk-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("reference in the body: status %d, want 422", rec.Code)
	}
	if code := problemCode(t, rec); code != "issue_not_found" {
		t.Errorf("reference in the body: code %q, want issue_not_found", code)
	}
}
