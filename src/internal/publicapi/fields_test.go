package publicapi_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"skein-api/internal/authz"
	"skein-api/internal/publicapi"
)

// thing is the resource of the fields= test routes.
type thing struct {
	ID string `json:"id"`
	A  string `json:"a"`
	B  string `json:"b"`
	C  string `json:"c"`
}

// fieldsRouter serves a read of one thing, a read of a list of two things
// (more pages follow) and a write answering a thing; called reports whether
// the write ran.
func fieldsRouter(called *bool) http.Handler {
	one := thing{ID: "t1", A: "a1", B: "b1", C: "c1"}
	next := "page-2"
	list := publicapi.List[thing]{
		Items:      []thing{one, {ID: "t2", A: "a2", B: "b2", C: "c2"}},
		NextCursor: &next,
		HasMore:    true,
	}
	route := func(id, method, path string, h http.HandlerFunc) publicapi.Route {
		return publicapi.Route{
			OperationID: id, Method: method, Path: path,
			Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightRead}, Handler: h,
		}
	}
	return publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second}, []publicapi.Route{
		route("testGetThing", http.MethodGet, "/api/v1/things/{thing}", func(w http.ResponseWriter, r *http.Request) {
			publicapi.WriteJSON(w, r, http.StatusOK, one)
		}),
		route("testListThings", http.MethodGet, "/api/v1/things", func(w http.ResponseWriter, r *http.Request) {
			publicapi.WriteJSON(w, r, http.StatusOK, list)
		}),
		route("testUpdateThing", http.MethodPatch, "/api/v1/things/{thing}", func(w http.ResponseWriter, r *http.Request) {
			*called = true
			publicapi.WriteJSON(w, r, http.StatusOK, one)
		}),
	})
}

func getJSON(t *testing.T, h http.Handler, method, target string) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body is not a JSON object: %v (%q)", err, rec.Body.String())
	}
	return rec, obj
}

func keysOf(m map[string]json.RawMessage) string {
	return strings.Join(slices.Sorted(maps.Keys(m)), ",")
}

// TestFields_Projection — row 70 (US-8, SC-008, edge "unknown fields",
// "fields on a write"). fields=a,b on a read answers a, b and id only;
// without fields= the response is whole. A field the resource does not have
// is 400; fields= on a write is 400 and the write does not run. On a list
// the items are projected and the envelope — nextCursor, hasMore — stays.
func TestFields_Projection(t *testing.T) {
	var called bool
	h := fieldsRouter(&called)

	t.Run("read with fields=a,b keeps a, b and id", func(t *testing.T) {
		rec, obj := getJSON(t, h, http.MethodGet, "/api/v1/things/t1?fields=a,b")
		if obj == nil {
			t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if got := keysOf(obj); got != "a,b,id" {
			t.Errorf("members %s, want a,b,id", got)
		}
		if string(obj["id"]) != `"t1"` || string(obj["a"]) != `"a1"` || string(obj["b"]) != `"b1"` {
			t.Errorf("values %s, want id t1, a a1, b b1", rec.Body.String())
		}
	})

	t.Run("read without fields is whole", func(t *testing.T) {
		rec, obj := getJSON(t, h, http.MethodGet, "/api/v1/things/t1")
		if obj == nil {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		if got := keysOf(obj); got != "a,b,c,id" {
			t.Errorf("members %s, want a,b,c,id", got)
		}
	})

	t.Run("unknown field: 400", func(t *testing.T) {
		rec, _ := getJSON(t, h, http.MethodGet, "/api/v1/things/t1?fields=a,nope")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rec.Code)
		}
		if code := problemCode(t, rec); code != "validation_error" {
			t.Errorf("code %q, want validation_error", code)
		}
	})

	t.Run("fields on a write: 400, the write does not run", func(t *testing.T) {
		called = false
		rec, _ := getJSON(t, h, http.MethodPatch, "/api/v1/things/t1?fields=a")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rec.Code)
		}
		if code := problemCode(t, rec); code != "validation_error" {
			t.Errorf("code %q, want validation_error", code)
		}
		if called {
			t.Error("the write ran")
		}
	})

	t.Run("list: items projected, envelope kept", func(t *testing.T) {
		rec, env := getJSON(t, h, http.MethodGet, "/api/v1/things?fields=a")
		if env == nil {
			t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if got := keysOf(env); got != "hasMore,items,nextCursor" {
			t.Errorf("envelope members %s, want hasMore,items,nextCursor", got)
		}
		if string(env["nextCursor"]) != `"page-2"` || string(env["hasMore"]) != "true" {
			t.Errorf("nextCursor %s, hasMore %s; want \"page-2\", true", env["nextCursor"], env["hasMore"])
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(env["items"], &items); err != nil {
			t.Fatalf("items: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("%d items, want 2", len(items))
		}
		for i, it := range items {
			if got := keysOf(it); got != "a,id" {
				t.Errorf("item %d members %s, want a,id", i, got)
			}
		}
	})
}
