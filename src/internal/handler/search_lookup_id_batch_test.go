// search_lookup_id_batch_test.go covers POST /api/v1/search/lookup-id-batch
// — the batch exact-id resolve endpoint (sk-47.1) backing the
// "paste a blob of text → extract task codes → render the tasks" surface.
//
// Mirrors the search_lookup_id_test.go harness: seed real rows in a
// postgres reachable via TEST_DATABASE_URL, exercise the handler with
// httptest, assert on the JSON wire response.

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/search"
)

// batchTestEnv bundles the per-test fixtures.
type batchTestEnv struct {
	pool      *pgxpool.Pool
	handler   *SearchHandler
	owner     uuid.UUID
	ownerName string
	projectID uuid.UUID
	prefix    string
}

func newBatchEnv(t *testing.T) batchTestEnv {
	pool := newListTestPool(t)
	owner, ownerName := seedListUser(t, pool, "user")
	pid, prefix := seedListProject(t, pool, owner, "bid")

	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 1*time.Minute)
	if err != nil {
		t.Fatalf("new search service: %v", err)
	}
	return batchTestEnv{
		pool: pool, handler: NewSearchHandler(svc),
		owner: owner, ownerName: ownerName,
		projectID: pid, prefix: prefix,
	}
}

// callBatch POSTs rawBody to /api/v1/search/lookup-id-batch with the
// supplied caller context. role="" defaults to "user" via caller().
// uuid.Nil userID means unauthenticated.
func callBatch(t *testing.T, env batchTestEnv, userID uuid.UUID, role, rawBody string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search/lookup-id-batch",
		bytes.NewReader([]byte(rawBody)))
	req.Header.Set("Content-Type", "application/json")
	ctx := req.Context()
	if userID != uuid.Nil {
		ctx = auth.WithUserID(ctx, userID)
		if role != "" {
			ctx = auth.WithUserRole(ctx, role)
		}
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	env.handler.LookupIDBatch(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	// Some error paths (e.g. 400) still emit JSON; decode best-effort.
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// batchItems extracts items[] as a slice of maps for assertions.
func batchItems(t *testing.T, body map[string]any) []map[string]any {
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

// batchIDSet pulls the set of returned ids for order-independent
// assertions.
func batchIDSet(items []map[string]any) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		if id, ok := it["id"].(string); ok {
			s[id] = true
		}
	}
	return s
}

// TestLookupIDBatch_MixedFoundMissingInvisible: a request mixing a
// found id, a nonexistent id, and an id in a project the caller cannot
// see returns ONLY the found+visible row.
func TestLookupIDBatch_MixedFoundMissingInvisible(t *testing.T) {
	env := newBatchEnv(t)
	found := env.prefix + "-14"
	seedListIssue(t, env.pool, env.projectID, env.owner, found, "human", env.ownerName, nil)

	// A second project owned by a DIFFERENT user that env.owner is not a
	// member of — its issue must be invisible to env.owner.
	otherOwner, otherName := seedListUser(t, env.pool, "user")
	otherPID, otherPrefix := seedListProject(t, env.pool, otherOwner, "oid")
	invisible := otherPrefix + "-9"
	seedListIssue(t, env.pool, otherPID, otherOwner, invisible, "human", otherName, nil)

	missing := env.prefix + "-9999"

	body := `{"ids":["` + found + `","` + missing + `","` + invisible + `"]}`
	res, b := callBatch(t, env, env.owner, "user", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, b)
	}
	items := batchItems(t, b)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1; items=%v", len(items), items)
	}
	got := batchIDSet(items)
	if !got[found] {
		t.Fatalf("found id %s missing from items; got=%v", found, got)
	}
	if got[missing] {
		t.Fatalf("nonexistent id %s leaked into items", missing)
	}
	if got[invisible] {
		t.Fatalf("invisible id %s leaked into items", invisible)
	}
	// Confirm the Item shape: kind=issue, exactMatch=true.
	if k, _ := items[0]["kind"].(string); k != "issue" {
		t.Fatalf("kind = %v, want issue", items[0]["kind"])
	}
	if exact, _ := items[0]["exactMatch"].(bool); !exact {
		t.Fatalf("exactMatch = %v, want true", items[0]["exactMatch"])
	}
}

// TestLookupIDBatch_AdminSeesAll: an admin caller sees rows from a
// project they are not a member of (admin bypasses project_members).
func TestLookupIDBatch_AdminSeesAll(t *testing.T) {
	env := newBatchEnv(t)
	otherOwner, otherName := seedListUser(t, env.pool, "user")
	otherPID, otherPrefix := seedListProject(t, env.pool, otherOwner, "aid")
	id := otherPrefix + "-3"
	seedListIssue(t, env.pool, otherPID, otherOwner, id, "human", otherName, nil)

	admin, _ := seedListUser(t, env.pool, "admin")
	res, b := callBatch(t, env, admin, "admin", `{"ids":["`+id+`"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, b)
	}
	items := batchItems(t, b)
	if len(items) != 1 || !batchIDSet(items)[id] {
		t.Fatalf("admin should see %s; got items=%v", id, items)
	}
}

// TestLookupIDBatch_Dedupe: the same id supplied twice (with case +
// whitespace variation) collapses to a single returned row.
func TestLookupIDBatch_Dedupe(t *testing.T) {
	env := newBatchEnv(t)
	id := env.prefix + "-7"
	seedListIssue(t, env.pool, env.projectID, env.owner, id, "human", env.ownerName, nil)

	// Same logical id three times: verbatim, upper-cased, padded.
	body := `{"ids":["` + id + `","` + strings.ToUpper(id) + `","  ` + id + `  "]}`
	res, b := callBatch(t, env, env.owner, "user", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, b)
	}
	items := batchItems(t, b)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1 (dedupe); items=%v", len(items), items)
	}
}

// TestLookupIDBatch_TooManyIDs: more than 100 ids → 400.
func TestLookupIDBatch_TooManyIDs(t *testing.T) {
	env := newBatchEnv(t)
	var sb strings.Builder
	sb.WriteString(`{"ids":[`)
	for i := 0; i < 101; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"` + env.prefix + "-" + strconv.Itoa(i) + `"`)
	}
	sb.WriteString(`]}`)
	res, b := callBatch(t, env, env.owner, "user", sb.String())
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, b)
	}
	if code, _ := b["error"].(string); code != "too_many_ids" {
		t.Fatalf("error code = %v, want too_many_ids", b["error"])
	}
}

// TestLookupIDBatch_EmptyArray: ids present but empty → 400.
func TestLookupIDBatch_EmptyArray(t *testing.T) {
	env := newBatchEnv(t)
	res, b := callBatch(t, env, env.owner, "user", `{"ids":[]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, b)
	}
}

// TestLookupIDBatch_AllBlanks: ids present but every entry blank after
// trim → 400 (nothing left to query).
func TestLookupIDBatch_AllBlanks(t *testing.T) {
	env := newBatchEnv(t)
	res, b := callBatch(t, env, env.owner, "user", `{"ids":["  ","",  "\t"]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, b)
	}
}

// TestLookupIDBatch_MissingIDsField: body parses but has no `ids` → 400.
func TestLookupIDBatch_MissingIDsField(t *testing.T) {
	env := newBatchEnv(t)
	res, b := callBatch(t, env, env.owner, "user", `{}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, b)
	}
}

// TestLookupIDBatch_NonArrayIDs: `ids` is the wrong JSON type → 400.
func TestLookupIDBatch_NonArrayIDs(t *testing.T) {
	env := newBatchEnv(t)
	res, b := callBatch(t, env, env.owner, "user", `{"ids":"sk-1"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, b)
	}
}

// TestLookupIDBatch_EmptyBody: an empty / unparseable body → 400.
func TestLookupIDBatch_EmptyBody(t *testing.T) {
	env := newBatchEnv(t)
	res, b := callBatch(t, env, env.owner, "user", ``)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, b)
	}
}

// TestLookupIDBatch_Unauthenticated: no UserID in ctx → 401.
func TestLookupIDBatch_Unauthenticated(t *testing.T) {
	env := newBatchEnv(t)
	res, _ := callBatch(t, env, uuid.Nil, "", `{"ids":["`+env.prefix+`-1"]}`)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}
