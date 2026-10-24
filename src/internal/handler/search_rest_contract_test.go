package handler

// search_rest_contract_test.go — sk-282 T015, test-plan row 62.
//
// "INV-2: REST search — key set, types, representative values,
// PRESERVED HIGHLIGHT MARKUP and THE PREVIOUS PAGE SIZE. The last two
// are named separately because they are the concrete T015 leak vectors:
// the search service is shared, and a change in it would leak into REST
// without touching a single key."
//
// That sentence is the whole design of this file. T015 removes <b>…</b>
// from the snippet and drops the page size to 25 — both in the MCP
// projection, both on a service instance the REST endpoint shares. A leak
// either way changes a VALUE, not a key, so a key-set guard alone would
// pass while the web UI quietly lost its highlighting.

import (
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
	"skein-api/internal/testsupport"
)

// restSearchItemKeys is the REST search item's always-present field set.
var restSearchItemKeys = map[string]string{
	"kind": "string", "id": "string", "title": "string",
	"snippet": "string", "score": "number", "exactMatch": "bool",
	"updatedAt": "string",
}

// restSearchItemOptionalKeys carry omitempty.
var restSearchItemOptionalKeys = map[string]string{
	"projectId": "string", "projectName": "string",
	// sk-674.18 T031 — the short id of the space of the hit's project.
	"spaceId": "string",
	"status":  "string", "priority": "number", "type": "string",
	"pinned": "bool", "assignee": "string",
	"parentIssueId": "string", "authorUsername": "string",
	"archived": "bool", "componentArchived": "bool",
}

// restSearchSourceValues is the closed server set of the `source` parameter
// of GET /api/v1/search — sk-421.1 T003, test-plan row 62 (SC-006 server
// half, owner decision D-1). Written out here as literals: the frontend
// (skein-frontend T010) builds its source types from this very set without
// a server round-trip, so a server that narrowed or renamed a value would
// hand the web UI a 400 with nothing in the frontend suite noticing.
var restSearchSourceValues = []string{"human", "agent_directed", "agent_autonomous"}

// callRESTSearchStatus is callRESTSearch without the 200 assertion: the
// source contract needs the refusal's status and body too.
func callRESTSearchStatus(t *testing.T, svc *search.Service, caller uuid.UUID, role, rawQuery string) (int, map[string]any) {
	t.Helper()
	h := NewSearchHandler(svc)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?"+rawQuery, nil)
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.Search(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestSearchREST_SourceParameterContract pins the closed `source` set:
// each of the three server values → 200, no parameter → 200, anything else
// (`agent`, the value the frontend used to send) → 400 invalid_source.
// The server does not change; this guards the set the frontend relies on.
func TestSearchREST_SourceParameterContract(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	svc := newRESTSearchService(t, pool)

	for _, source := range restSearchSourceValues {
		t.Run("source="+source+" is accepted", func(t *testing.T) {
			status, body := callRESTSearchStatus(t, svc, caller, "user", "q=wombat&source="+source)
			if status != http.StatusOK {
				t.Errorf("GET /search?source=%s = %d, want 200: %v", source, status, body)
			}
		})
	}

	t.Run("no source parameter is accepted", func(t *testing.T) {
		status, body := callRESTSearchStatus(t, svc, caller, "user", "q=wombat")
		if status != http.StatusOK {
			t.Errorf("GET /search without source = %d, want 200: %v", status, body)
		}
	})

	t.Run("source=agent is refused with invalid_source", func(t *testing.T) {
		status, body := callRESTSearchStatus(t, svc, caller, "user", "q=wombat&source=agent")
		if status != http.StatusBadRequest {
			t.Fatalf("GET /search?source=agent = %d, want 400 — the server set is no longer closed: %v", status, body)
		}
		if body["error"] != "invalid_source" {
			t.Errorf("GET /search?source=agent error = %v, want invalid_source", body["error"])
		}
	})
}

func callRESTSearch(t *testing.T, svc *search.Service, caller uuid.UUID, role, query string) map[string]any {
	t.Helper()
	h := NewSearchHandler(svc)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+query, nil)
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.Search(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		var problem map[string]any
		_ = json.NewDecoder(res.Body).Decode(&problem)
		t.Fatalf("GET /search = %d: %v", res.StatusCode, problem)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func newRESTSearchService(t *testing.T, pool *pgxpool.Pool) *search.Service {
	t.Helper()
	svc, err := search.NewService(search.NewStore(pool), nil, 60*time.Second)
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	return svc
}

// TestSearchREST_KeepsHighlightAndPageSize is row 62.
func TestSearchREST_KeepsHighlightAndPageSize(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restsrch")

	// More matches than the MCP page size, so a clamp leaking from T015
	// would show up as a short page here.
	const term = "wombat"
	const rows = 30
	body := "prelude words before the match, then " + term + ", then words after"
	for i := 1; i <= rows; i++ {
		id := prefix + "-" + strconv.Itoa(i)
		testsupport.SeedIssue(t, pool, pid, owner, id, "rest search fixture", "open", 2)
		if _, err := pool.Exec(t.Context(),
			`UPDATE issues SET description = $2 WHERE id = $1`, id, body); err != nil {
			t.Fatalf("set description: %v", err)
		}
	}

	svc := newRESTSearchService(t, pool)
	res := callRESTSearch(t, svc, owner, "admin", term)

	items, ok := res["items"].([]any)
	if !ok {
		t.Fatalf("items missing: %#v", res["items"])
	}
	if len(items) != rows {
		t.Errorf("REST search returned %d of %d matches — its page size is its own and INV-2 "+
			"freezes it; T015's 25 is applied in the MCP handler precisely so it cannot reach "+
			"here", len(items), rows)
	}

	// The leak vector that changes a VALUE, not a key.
	highlighted := 0
	for _, raw := range items {
		item := raw.(map[string]any)
		snippet, _ := item["snippet"].(string)
		if strings.Contains(snippet, "<b>") && strings.Contains(snippet, "</b>") {
			highlighted++
		}
	}
	if highlighted == 0 {
		t.Errorf("no REST search result carries <b>…</b> highlight markup — the web UI renders "+
			"it, and T015 strips it in the MCP projection ONLY. A change made in the shared "+
			"service instead would un-highlight the interface without touching a single key, "+
			"which is why this is asserted on the value.\nfirst snippet: %v",
			items[0].(map[string]any)["snippet"])
	}

	// Key set and types.
	for _, raw := range items {
		item := raw.(map[string]any)
		for key, value := range item {
			wantKind, known := restSearchItemKeys[key]
			if !known {
				wantKind, known = restSearchItemOptionalKeys[key]
			}
			if !known {
				t.Errorf("REST search item carries an unclassified key %q = %#v", key, value)
				continue
			}
			assertJSONKind(t, "REST search item", key, value, wantKind)
		}
		for key := range restSearchItemKeys {
			if _, ok := item[key]; !ok {
				t.Errorf("REST search item is MISSING %q — the set is exact, not an upper bound", key)
			}
		}
	}

	// Envelope. nextCursor is the one key here that is legitimately null:
	// search.Result declares it *string WITHOUT omitempty, so «no next
	// page» is spelled `"nextCursor": null` on the wire and the frontend
	// reads it that way. It is therefore declared nullable rather than
	// tolerated, which is the difference between a contract and a blind
	// spot — every other key is refused a null.
	envelope := map[string]string{
		"items": "array", "hasMore": "bool", "degraded": "bool",
		"tookMs": "number", "nextCursor": "string|null",
	}
	for key, want := range envelope {
		assertJSONKind(t, "REST search envelope", key, res[key], want)
	}
	for key := range res {
		if _, known := envelope[key]; !known {
			t.Errorf("REST search envelope carries an unclassified key %q", key)
		}
	}
}
