package publicapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"skein-api/internal/authz"
	"skein-api/internal/publicapi"
)

// thingsLimit is the limit= range of the test list: [1, 100], default 25.
var thingsLimit = publicapi.Limit{Min: 1, Max: 100, Default: 25}

// createThing is the body of the test write.
type createThing struct {
	Title string  `json:"title"`
	Kind  *string `json:"kind"`
}

// decodeRouter serves a list that reads limit= and the query parameter
// status (answering the limit it got) and a write that decodes its body and
// its kind (answering the origin the write records).
func decodeRouter() http.Handler {
	route := func(id, method, path string, h http.HandlerFunc) publicapi.Route {
		return publicapi.Route{
			OperationID: id, Method: method, Path: path,
			Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Handler: h,
		}
	}
	return publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second}, []publicapi.Route{
		route("testListThings", http.MethodGet, "/api/v1/things", func(w http.ResponseWriter, r *http.Request) {
			if !publicapi.CheckQuery(w, r, "limit", "status") {
				return
			}
			limit, err := publicapi.ParseLimit(r, thingsLimit)
			if err != nil {
				publicapi.WriteParseError(w, err)
				return
			}
			publicapi.WriteJSON(w, r, http.StatusOK, map[string]int{"limit": limit})
		}),
		route("testCreateThing", http.MethodPost, "/api/v1/things", func(w http.ResponseWriter, r *http.Request) {
			var body createThing
			if !publicapi.DecodeBody(w, r, &body) {
				return
			}
			origin, err := publicapi.ParseKind(body.Kind)
			if err != nil {
				publicapi.WriteParseError(w, err)
				return
			}
			publicapi.WriteJSON(w, r, http.StatusCreated, map[string]string{
				"source": origin.Source, "authorKind": origin.CommentAuthorKind,
			})
		}),
	})
}

func send(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func wantValidation400(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != "validation_error" {
		t.Errorf("code %q, want validation_error", code)
	}
}

// TestDecode_StrictRequest — row 72 ("General rules of the surface",
// OST-2-8). limit outside [1, 100] or not an integer is 400 and never
// clamped to the edge; limit inside the range and the default pass as given.
// An unknown query parameter, an unknown body field, malformed JSON and data
// after the JSON value are 400. kind directed and autonomous are accepted
// and give the origins agent_directed and agent_autonomous, no kind gives
// api; any other kind, the empty one included, is 400.
func TestDecode_StrictRequest(t *testing.T) {
	h := decodeRouter()

	for _, limit := range []string{"0", "101", "-1", "1000", "abc", "1.5", ""} {
		t.Run("limit="+limit+": 400", func(t *testing.T) {
			wantValidation400(t, send(h, http.MethodGet, "/api/v1/things?limit="+limit, ""))
		})
	}
	t.Run("limit given twice: 400", func(t *testing.T) {
		wantValidation400(t, send(h, http.MethodGet, "/api/v1/things?limit=5&limit=6", ""))
	})

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 25},
		{"?limit=1", 1},
		{"?limit=100", 100},
		{"?limit=40&status=open", 40},
	} {
		t.Run("limit "+tc.query+" passes as "+strconv.Itoa(tc.want), func(t *testing.T) {
			rec := send(h, http.MethodGet, "/api/v1/things"+tc.query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			var got struct{ Limit int }
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Limit != tc.want {
				t.Errorf("limit %d, want %d", got.Limit, tc.want)
			}
		})
	}

	t.Run("unknown query parameter: 400", func(t *testing.T) {
		wantValidation400(t, send(h, http.MethodGet, "/api/v1/things?colour=red", ""))
	})

	for name, body := range map[string]string{
		"unknown body field":   `{"title":"x","colour":"red"}`,
		"malformed JSON":       `{"title":`,
		"data after the value": `{"title":"x"} {"title":"y"}`,
		"wrong type":           `{"title":5}`,
	} {
		t.Run(name+": 400", func(t *testing.T) {
			wantValidation400(t, send(h, http.MethodPost, "/api/v1/things", body))
		})
	}

	for _, tc := range []struct {
		body, source, authorKind string
	}{
		{`{"title":"x"}`, "api", "api"},
		{`{"title":"x","kind":"directed"}`, "agent_directed", "agent_directed"},
		{`{"title":"x","kind":"autonomous"}`, "agent_autonomous", "agent_autonomous"},
	} {
		t.Run("kind in "+tc.body+": accepted as "+tc.source, func(t *testing.T) {
			rec := send(h, http.MethodPost, "/api/v1/things", tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d, want 201 (body %q)", rec.Code, rec.Body.String())
			}
			var got struct{ Source, AuthorKind string }
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Source != tc.source || got.AuthorKind != tc.authorKind {
				t.Errorf("source %q, authorKind %q; want %q, %q", got.Source, got.AuthorKind, tc.source, tc.authorKind)
			}
		})
	}
	for _, kind := range []string{`""`, `"human"`, `"Directed"`, `"api"`, `5`} {
		t.Run("kind "+kind+": 400", func(t *testing.T) {
			wantValidation400(t, send(h, http.MethodPost, "/api/v1/things", `{"title":"x","kind":`+kind+`}`))
		})
	}
}
