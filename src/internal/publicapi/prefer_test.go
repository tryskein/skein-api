package publicapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"skein-api/internal/authz"
	"skein-api/internal/publicapi"
)

// versioned is a resource with its own version (as an issue or a comment).
type versioned struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Title   string `json:"title"`
}

// unversioned is a resource without a version (as a project).
type unversioned struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// batchResult is the answer of a batch operation.
type batchResult struct {
	Results []versioned `json:"results"`
}

func preferRouter() http.Handler {
	route := func(id, path string, prefer bool, v any) publicapi.Route {
		return publicapi.Route{
			OperationID: id, Method: http.MethodPost, Path: path,
			Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Prefer: prefer,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				publicapi.WriteJSON(w, r, http.StatusOK, v)
			}),
		}
	}
	return publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second}, []publicapi.Route{
		route("testUpdateVersioned", "/api/v1/versioned", true, versioned{ID: "i1", Version: 7, Title: "t"}),
		route("testUpdateUnversioned", "/api/v1/unversioned", false, unversioned{ID: "p1", Name: "n"}),
		route("testBatch", "/api/v1/batch", false, batchResult{Results: []versioned{{ID: "i1", Version: 7, Title: "t"}}}),
	})
}

func postPrefer(h http.Handler, path, prefer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestPrefer_ReturnMinimal — row 71 (US-8, SC-008). Prefer: return=minimal
// on a single operation returning a resource with its own version answers
// {id, version} and Preference-Applied: return=minimal. On an operation
// whose resource has no version and on a batch the same request answers the
// whole response and no Preference-Applied; without Prefer the applicable
// operation answers whole too.
func TestPrefer_ReturnMinimal(t *testing.T) {
	h := preferRouter()

	t.Run("applicable: {id, version} and Preference-Applied", func(t *testing.T) {
		rec := postPrefer(h, "/api/v1/versioned", "return=minimal")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Preference-Applied"); got != "return=minimal" {
			t.Errorf("Preference-Applied %q, want return=minimal", got)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		if got := keysOf(obj); got != "id,version" {
			t.Errorf("members %s, want id,version", got)
		}
		if string(obj["id"]) != `"i1"` || string(obj["version"]) != "7" {
			t.Errorf("body %s, want id i1, version 7", rec.Body.String())
		}
	})

	for _, tc := range []struct {
		name, path, prefer, want string // want: the members of the whole response
	}{
		{"applicable without Prefer", "/api/v1/versioned", "", "id,title,version"},
		{"resource without a version", "/api/v1/unversioned", "return=minimal", "id,name"},
		{"batch", "/api/v1/batch", "return=minimal", "results"},
	} {
		t.Run(tc.name+": whole, no Preference-Applied", func(t *testing.T) {
			rec := postPrefer(h, tc.path, tc.prefer)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			if _, set := rec.Header()["Preference-Applied"]; set {
				t.Errorf("Preference-Applied set to %q", rec.Header().Get("Preference-Applied"))
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
				t.Fatal(err)
			}
			if got := keysOf(obj); got != tc.want {
				t.Errorf("members %s, want %s (body %s)", got, tc.want, rec.Body.String())
			}
		})
	}
}
