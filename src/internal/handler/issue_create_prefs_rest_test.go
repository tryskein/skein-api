package handler

// issue_create_prefs_rest_test.go — sk-565, T026, test matrix rows 7–11
// (C-REST, INV-8, INV-10). Reference - the sk-565 spec, "Integration points and
// contracts": GET -> `{ "assignAgentIssuesToMe": boolean }`, without a row -
// true, and no row is created; PUT is partial, the body is an object with the single
// allowed boolean field; `{}` changes nothing, 200 with the current value;
// a foreign field (including a key in another case), null or a non-boolean
// value, a non-object, an empty or invalid body (including a tail after the
// first JSON value) - 400 `invalid_json`, nothing written; without a
// user - 401.
//
// The handler is called directly, as in review_prefs_rest_test.go, so a
// missing route is invisible here; cmd/api/issue_create_prefs_wiring_test.go
// closes that (row 12).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// callIssueCreatePrefs runs GET (body == nil) or PUT with a RAW body against
// the handler. callerID == uuid.Nil sends the request without a user.
func callIssueCreatePrefs(t *testing.T, pool *pgxpool.Pool, callerID uuid.UUID, body *string) (int, map[string]any) {
	t.Helper()
	h := NewNotificationsHandler(pool)
	method := http.MethodGet
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, "/api/v1/me/issue-create-prefs", nil)
	} else {
		method = http.MethodPut
		req = httptest.NewRequest(method, "/api/v1/me/issue-create-prefs", bytes.NewBufferString(*body))
		req.Header.Set("Content-Type", "application/json")
	}
	if callerID != uuid.Nil {
		ctx := auth.WithUserID(req.Context(), callerID)
		ctx = auth.WithUserRole(ctx, "user")
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		h.GetIssueCreatePrefs(rec, req)
	} else {
		h.PutIssueCreatePrefs(rec, req)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: response is not a JSON object (%d): %v (%s)", method, rec.Code, err, rec.Body.String())
	}
	return rec.Code, out
}

func rawBody(s string) *string { return &s }

// wantIssueCreatePrefsBody checks a 200 body is exactly the GET form with
// the given value.
func wantIssueCreatePrefsBody(t *testing.T, what string, status int, body map[string]any, want bool) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("%s = %d, want 200: %v", what, status, body)
	}
	if len(body) != 1 || body["assignAgentIssuesToMe"] != want {
		t.Errorf("%s body = %v, want {\"assignAgentIssuesToMe\":%v}", what, body, want)
	}
}

func readStoredAssignFlag(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) bool {
	t.Helper()
	var on bool
	if err := pool.QueryRow(context.Background(),
		`SELECT assign_agent_issues_to_me FROM user_notification_prefs WHERE user_id = $1`, user).Scan(&on); err != nil {
		t.Fatalf("read stored flag: %v", err)
	}
	return on
}

// TestIssueCreatePrefsREST_GetWithoutRowIsTrueAndCreatesNoRow — row 7.
func TestIssueCreatePrefsREST_GetWithoutRowIsTrueAndCreatesNoRow(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	status, body := callIssueCreatePrefs(t, pool, user, nil)
	wantIssueCreatePrefsBody(t, "GET without a row", status, body, true)
	if n := countPrefsRows(t, pool, user); n != 0 {
		t.Errorf("after GET the user has %d prefs rows, want 0", n)
	}
}

// TestIssueCreatePrefsREST_PutWritesOnlyForTheCaller — row 8.
func TestIssueCreatePrefsREST_PutWritesOnlyForTheCaller(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	alice, _ := testsupport.SeedUser(t, pool, "user")
	bob, _ := testsupport.SeedUser(t, pool, "user")

	status, body := callIssueCreatePrefs(t, pool, alice, rawBody(`{"assignAgentIssuesToMe":false}`))
	wantIssueCreatePrefsBody(t, "PUT false as Alice", status, body, false)

	status, body = callIssueCreatePrefs(t, pool, alice, nil)
	wantIssueCreatePrefsBody(t, "GET as Alice", status, body, false)
	status, body = callIssueCreatePrefs(t, pool, bob, nil)
	wantIssueCreatePrefsBody(t, "GET as Bob", status, body, true)
}

// TestIssueCreatePrefsREST_MalformedBodyIs400AndWritesNothing — row 9.
func TestIssueCreatePrefsREST_MalformedBodyIs400AndWritesNothing(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	bodies := []struct{ name, body string }{
		{"unknown field", `{"assignAgentIssuesToMe":false,"other":true}`},
		{"null field", `{"assignAgentIssuesToMe":null}`},
		{"string instead of boolean", `{"assignAgentIssuesToMe":"false"}`},
		{"number instead of boolean", `{"assignAgentIssuesToMe":0}`},
		{"array body", `[{"assignAgentIssuesToMe":false}]`},
		{"null body", `null`},
		{"empty body", ``},
		{"invalid JSON", `{"assignAgentIssuesToMe":fal`},
		// A body is ONE JSON value: anything after it but whitespace makes
		// the body malformed, not «the first value plus ignorable noise».
		{"garbage after the object", `{"assignAgentIssuesToMe":false} garbage`},
		{"second object after the object", `{"assignAgentIssuesToMe":false}{"x":1}`},
		{"true then garbage", `{"assignAgentIssuesToMe":true} garbage`},
		// The only allowed key is spelled exactly; another spelling is an
		// unknown key. true, so a wrongful write flips a stored false.
		{"key in upper case", `{"ASSIGNAGENTISSUESTOME":true}`},
		{"key in Pascal case", `{"AssignAgentIssuesToMe":true}`},
	}
	for _, tc := range bodies {
		t.Run(tc.name+" / no row", func(t *testing.T) {
			user, _ := testsupport.SeedUser(t, pool, "user")
			status, body := callIssueCreatePrefs(t, pool, user, rawBody(tc.body))
			if status != http.StatusBadRequest || body["error"] != "invalid_json" {
				t.Errorf("PUT %q = %d %v, want 400 invalid_json", tc.body, status, body)
			}
			if n := countPrefsRows(t, pool, user); n != 0 {
				t.Errorf("PUT %q created %d prefs rows, want 0", tc.body, n)
			}
		})
		t.Run(tc.name+" / stored false", func(t *testing.T) {
			user, _ := testsupport.SeedUser(t, pool, "user")
			if status, body := callIssueCreatePrefs(t, pool, user, rawBody(`{"assignAgentIssuesToMe":false}`)); status != http.StatusOK {
				t.Fatalf("seed PUT false = %d: %v", status, body)
			}
			status, body := callIssueCreatePrefs(t, pool, user, rawBody(tc.body))
			if status != http.StatusBadRequest || body["error"] != "invalid_json" {
				t.Errorf("PUT %q = %d %v, want 400 invalid_json", tc.body, status, body)
			}
			if readStoredAssignFlag(t, pool, user) {
				t.Errorf("PUT %q changed the stored false to true", tc.body)
			}
		})
	}
}

// TestIssueCreatePrefsREST_EmptyObjectWritesNothing — row 10.
func TestIssueCreatePrefsREST_EmptyObjectWritesNothing(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	t.Run("no row", func(t *testing.T) {
		user, _ := testsupport.SeedUser(t, pool, "user")
		status, body := callIssueCreatePrefs(t, pool, user, rawBody(`{}`))
		wantIssueCreatePrefsBody(t, "PUT {} without a row", status, body, true)
		if n := countPrefsRows(t, pool, user); n != 0 {
			t.Errorf("PUT {} created %d prefs rows, want 0", n)
		}
	})

	t.Run("stored false", func(t *testing.T) {
		user, _ := testsupport.SeedUser(t, pool, "user")
		if status, body := callIssueCreatePrefs(t, pool, user, rawBody(`{"assignAgentIssuesToMe":false}`)); status != http.StatusOK {
			t.Fatalf("seed PUT false = %d: %v", status, body)
		}
		status, body := callIssueCreatePrefs(t, pool, user, rawBody(`{}`))
		wantIssueCreatePrefsBody(t, "PUT {} over stored false", status, body, false)
		if readStoredAssignFlag(t, pool, user) {
			t.Errorf("PUT {} changed the stored false to true")
		}
	})
}

// TestIssueCreatePrefsREST_WithoutUserIs401 — row 11.
func TestIssueCreatePrefsREST_WithoutUserIs401(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	if status, body := callIssueCreatePrefs(t, pool, uuid.Nil, nil); status != http.StatusUnauthorized {
		t.Errorf("GET without a user = %d %v, want 401", status, body)
	}
	if status, body := callIssueCreatePrefs(t, pool, uuid.Nil, rawBody(`{"assignAgentIssuesToMe":false}`)); status != http.StatusUnauthorized {
		t.Errorf("PUT without a user = %d %v, want 401", status, body)
	}
}
