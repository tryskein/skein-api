// search_lookup_id_test.go covers GET /api/v1/search/lookup-id — the
// lightweight ID-prefix endpoint powering the typeahead direct-jump UX
// on the new shell (sk-11).
//
// Mirrors the listTestEnv convention: seed real rows in a postgres
// reachable via TEST_DATABASE_URL, exercise the handler with httptest,
// assert on the JSON wire response. We DO NOT exercise the cache or
// embedder — LookupID is the cache-free fast path.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/search"
)

// lookupIDTestEnv bundles the per-test fixtures.
type lookupIDTestEnv struct {
	pool      *pgxpool.Pool
	handler   *SearchHandler
	owner     uuid.UUID
	ownerName string
	projectID uuid.UUID
	prefix    string
}

// newLookupIDEnv reuses the listTestEnv seed helpers so the fixture
// shape stays identical to the rest of the handler suite.
func newLookupIDEnv(t *testing.T) lookupIDTestEnv {
	pool := newListTestPool(t)
	owner, ownerName := seedListUser(t, pool, "user")
	pid, prefix := seedListProject(t, pool, owner, "lid")

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 1*time.Minute)
	if err != nil {
		t.Fatalf("new search service: %v", err)
	}
	return lookupIDTestEnv{
		pool: pool, handler: NewSearchHandler(svc),
		owner: owner, ownerName: ownerName,
		projectID: pid, prefix: prefix,
	}
}

// callLookupID invokes GET /api/v1/search/lookup-id?q=... with the
// supplied caller context. role="" defaults to "user" via caller().
func callLookupID(t *testing.T, env lookupIDTestEnv, userID uuid.UUID, role, rawQuery string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/lookup-id?"+rawQuery, nil)
	ctx := req.Context()
	if userID != uuid.Nil {
		ctx = auth.WithUserID(ctx, userID)
		if role != "" {
			ctx = auth.WithUserRole(ctx, role)
		}
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	env.handler.LookupID(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return res, body
}

// lookupIDItems extracts items[] as a slice of maps for assertions.
func lookupIDItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing or wrong type: %T (%v)", body["items"], body["items"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("item is not an object: %T", r)
		}
		out = append(out, row)
	}
	return out
}

// TestLookupID_ExactMatchIssue: typing the full id of an existing
// issue returns that single row with exactMatch:true.
func TestLookupID_ExactMatchIssue(t *testing.T) {
	env := newLookupIDEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-14",
		"human", env.ownerName, nil)

	res, body := callLookupID(t, env, env.owner, "user", "q="+env.prefix+"-14")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items := lookupIDItems(t, body)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1; items=%v", len(items), items)
	}
	if items[0]["id"] != env.prefix+"-14" {
		t.Fatalf("id = %v, want %s-14", items[0]["id"], env.prefix)
	}
	if exact, _ := items[0]["exactMatch"].(bool); !exact {
		t.Fatalf("exactMatch = %v, want true", items[0]["exactMatch"])
	}
	if kind, _ := items[0]["kind"].(string); kind != "issue" {
		t.Fatalf("kind = %v, want issue", items[0]["kind"])
	}
}

// TestLookupID_PrefixMatchIssue: typing a partial id prefix returns
// multiple rows ranked by length; only the row matching the prefix
// exactly (when one exists) carries exactMatch:true.
func TestLookupID_PrefixMatchIssue(t *testing.T) {
	env := newLookupIDEnv(t)
	for _, id := range []string{env.prefix + "-1", env.prefix + "-10", env.prefix + "-11"} {
		seedListIssue(t, env.pool, env.projectID, env.owner, id, "human", env.ownerName, nil)
	}

	res, body := callLookupID(t, env, env.owner, "user", "q="+env.prefix+"-1")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items := lookupIDItems(t, body)
	if len(items) < 2 {
		t.Fatalf("len(items) = %d, want >=2; items=%v", len(items), items)
	}
	// The exact-match (`<prefix>-1`) row must be first and flagged.
	if items[0]["id"] != env.prefix+"-1" {
		t.Fatalf("first id = %v, want %s-1", items[0]["id"], env.prefix)
	}
	if exact, _ := items[0]["exactMatch"].(bool); !exact {
		t.Fatalf("first row exactMatch = %v, want true", items[0]["exactMatch"])
	}
	// At least one of the longer rows MUST NOT be flagged exactMatch.
	sawNonExact := false
	for _, it := range items[1:] {
		if exact, _ := it["exactMatch"].(bool); !exact {
			sawNonExact = true
			break
		}
	}
	if !sawNonExact {
		t.Fatalf("expected at least one non-exact row, got: %v", items)
	}
}

// TestLookupID_HierarchicalID: an id with one or more `.N` segments
// (e.g. sk-10.7) is accepted by the regex gate and returned when the
// row exists.
func TestLookupID_HierarchicalID(t *testing.T) {
	env := newLookupIDEnv(t)
	parent := env.prefix + "-10"
	child := env.prefix + "-10.7"
	seedListIssue(t, env.pool, env.projectID, env.owner, parent, "human", env.ownerName, nil)
	seedListIssue(t, env.pool, env.projectID, env.owner, child, "human", env.ownerName, nil)

	res, body := callLookupID(t, env, env.owner, "user", "q="+child)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items := lookupIDItems(t, body)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1; items=%v", len(items), items)
	}
	if items[0]["id"] != child {
		t.Fatalf("id = %v, want %s", items[0]["id"], child)
	}
}

// TestLookupID_ProjectPrefixOnly: typing just the project id_prefix
// returns the project row (kind=project, exactMatch=true).
func TestLookupID_ProjectPrefixOnly(t *testing.T) {
	env := newLookupIDEnv(t)
	res, body := callLookupID(t, env, env.owner, "user", "q="+env.prefix)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items := lookupIDItems(t, body)
	if len(items) < 1 {
		t.Fatalf("len(items) = %d, want >=1; items=%v", len(items), items)
	}
	// The project row must be present and exactMatch.
	var projectRow map[string]any
	for _, it := range items {
		if k, _ := it["kind"].(string); k == "project" {
			projectRow = it
			break
		}
	}
	if projectRow == nil {
		t.Fatalf("no project row in items: %v", items)
	}
	if exact, _ := projectRow["exactMatch"].(bool); !exact {
		t.Fatalf("project row exactMatch = %v, want true", projectRow["exactMatch"])
	}
}

// TestLookupID_FreeText: free-text queries (with spaces, etc.) fail
// the regex gate; the endpoint returns 200 with an empty items array,
// not an error.
func TestLookupID_FreeText(t *testing.T) {
	env := newLookupIDEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-1",
		"human", env.ownerName, nil)

	res, body := callLookupID(t, env, env.owner, "user", "q=hello+world")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items := lookupIDItems(t, body)
	if len(items) != 0 {
		t.Fatalf("len(items) = %d, want 0 (free text shouldn't match); items=%v",
			len(items), items)
	}
}

// TestLookupID_NonAdminNoMembership: a logged-in user with no
// membership on the project sees an empty result for that project's
// ids — the SQL permission filter excludes the row.
func TestLookupID_NonAdminNoMembership(t *testing.T) {
	env := newLookupIDEnv(t)
	seedListIssue(t, env.pool, env.projectID, env.owner, env.prefix+"-14",
		"human", env.ownerName, nil)

	// Fresh user with no project_members row → cannot see env.projectID.
	outsider, _ := seedListUser(t, env.pool, "user")

	res, body := callLookupID(t, env, outsider, "user", "q="+env.prefix+"-14")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	items := lookupIDItems(t, body)
	if len(items) != 0 {
		t.Fatalf("len(items) = %d, want 0 (outsider should not see project); items=%v",
			len(items), items)
	}
}

// TestLookupID_MissingQ: q is required; missing it returns 422.
func TestLookupID_MissingQ(t *testing.T) {
	env := newLookupIDEnv(t)
	res, body := callLookupID(t, env, env.owner, "user", "")
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%v", res.StatusCode, body)
	}
}

// TestLookupID_Unauthenticated: no UserID in ctx → 401.
func TestLookupID_Unauthenticated(t *testing.T) {
	env := newLookupIDEnv(t)
	res, _ := callLookupID(t, env, uuid.Nil, "", "q="+env.prefix+"-14")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// silence unused import warnings when running with the timeout
// constructor swapped out.
var _ = context.Background
