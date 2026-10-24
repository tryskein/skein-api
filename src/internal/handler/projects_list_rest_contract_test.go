package handler

// projects_list_rest_contract_test.go — sk-282 T016, test-plan row 65;
// sk-674.18 T013/T024, matrix row 199a (the row's spaceId and visibility).
//
// "INV-2: REST project list — key set, types, representative
// values and an UNTRUNCATED PROJECT DESCRIPTION (T016 leak vector — a
// value, not a key), plus the previous page size."
//
// T016 cuts the description at 200 in the MCP projection. Made in the
// store instead, that cut would shorten every description the web UI
// renders without touching a single key — so the assertion that matters
// here is on the VALUE: the REST row must carry the description WHOLE.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// restProjectRowKeys is the REST project row's EXACT always-present field
// set, written by hand — a list derived from the struct it guards would
// agree with any change to that struct. It is asserted on the body of
// GET /projects (TestProjectsListREST_KeepsFullDescriptionAndPageSize).
var restProjectRowKeys = map[string]string{
	"id": "string", "name": "string", "idPrefix": "string",
	"defaultIdMode": "string", "description": "string",
	"createdAt": "string", "updatedAt": "string",
	"openCount": "number", "inProgressCount": "number",
	"blockedCount": "number", "standingCount": "number",
	"myOutboxOpenCount": "number", "myOutboxInProgressCount": "number",
	"myOutboxBlockedCount": "number", "myOutboxStandingCount": "number",
	"onMeOpenCount": "number", "onMeInProgressCount": "number",
	"onMeBlockedCount": "number", "epicsEligibleCount": "number",
	"componentsCount": "number",
	// sk-674.18 T024 (O-s4): the project's space and visibility.
	"spaceId": "string", "visibility": "string",
}

// restProjectRowOptionalKeys carry omitempty.
var restProjectRowOptionalKeys = map[string]string{
	"archivedAt": "string",
}

func callRESTProjectList(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID, role, query string) map[string]any {
	t.Helper()
	h := NewProjectsHandler(pool, nil)
	url := "/api/v1/projects"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.List(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		var problem map[string]any
		_ = json.NewDecoder(res.Body).Decode(&problem)
		t.Fatalf("GET /projects = %d: %v", res.StatusCode, problem)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestProjectsListREST_KeepsFullDescriptionAndPageSize is row 65.
func TestProjectsListREST_KeepsFullDescriptionAndPageSize(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")

	// A description far longer than the MCP cut, so a leak is unmissable.
	long := strings.Repeat("o", 640)
	const rows = 60
	var longID uuid.UUID
	for i := range rows {
		pid, _ := testsupport.SeedProject(t, pool, owner, "restpl"+strconv.Itoa(i))
		if i == 0 {
			longID = pid
			if _, err := pool.Exec(t.Context(),
				`UPDATE projects SET description = $2 WHERE id = $1`, pid, long); err != nil {
				t.Fatalf("set description: %v", err)
			}
		}
	}

	// sk-674.18 T013/T024, row 199a: the row names its space (the short id
	// of the space the project was seeded in) and its visibility (members —
	// the default of a project nobody switched).
	var longSpace string
	if err := pool.QueryRow(t.Context(),
		`SELECT s.short_id FROM projects p JOIN spaces s ON s.id = p.space_id WHERE p.id = $1`, longID).Scan(&longSpace); err != nil {
		t.Fatalf("read the seeded project's space: %v", err)
	}

	body := callRESTProjectList(t, pool, owner, "user", "limit=100")
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing: %#v", body["items"])
	}
	if len(items) < rows {
		t.Errorf("REST project list returned %d of at least %d rows — its page size is its own "+
			"and INV-2 freezes it", len(items), rows)
	}

	sawLong := false
	for _, raw := range items {
		row := raw.(map[string]any)
		if row["id"] == longID.String() {
			sawLong = true
			desc, _ := row["description"].(string)
			if desc != long {
				t.Errorf("REST description is %d runes, want the whole %d — T016 cuts it in "+
					"the MCP projection ONLY; a cut made in the store would shorten every "+
					"description the web UI renders without touching a key, which is why "+
					"this is asserted on the value",
					len([]rune(desc)), len([]rune(long)))
			}
			if row["spaceId"] != longSpace || row["visibility"] != "members" {
				t.Errorf("REST row spaceId=%v visibility=%v, want the seeded space %q and members",
					row["spaceId"], row["visibility"], longSpace)
			}
		}
		if _, ok := row["descriptionTruncated"]; ok {
			t.Errorf("REST project row carries descriptionTruncated — that marker belongs to " +
				"the MCP projection, and INV-2 forbids an additive key here")
		}

		for key, value := range row {
			wantKind, known := restProjectRowKeys[key]
			if !known {
				wantKind, known = restProjectRowOptionalKeys[key]
			}
			if !known {
				t.Errorf("REST project row carries an unclassified key %q = %#v — INV-2 covers "+
					"additive keys", key, value)
				continue
			}
			assertJSONKind(t, "REST project row", key, value, wantKind)
		}
		for key := range restProjectRowKeys {
			if _, ok := row[key]; !ok {
				t.Errorf("REST project row is MISSING %q — the set is exact, not an upper bound", key)
			}
		}
	}
	if !sawLong {
		t.Fatal("the project with the long description did not come back — the assertion above " +
			"never ran")
	}
}
