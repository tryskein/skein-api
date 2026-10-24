package handler

// issues_check_duplicates_test.go — sk-228.
//
// POST /projects/{projectID}/issues/check-duplicates is the only surface
// that returns `degradedReason`, and it is the surface a human UI reads.
// It had no test: the store tests cover the numbers and the MCP tests
// cover skein_create's serialisation, but nothing pinned this response's
// JSON — and `omitempty` versus an always-present `null` is precisely the
// class of bug sk-228 was filed about (a caller could not tell "this
// layer did not score the row" from "this key is missing today").
//
// So the assertions below are about KEYS and JSON TYPES, not about
// similarity arithmetic:
//
//   - every candidate carries all eight keys, with `semanticSimilarity`
//     serialised as JSON null rather than omitted;
//   - `degraded` is present on both branches;
//   - `degradedReason` is present exactly when `degraded` is true.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// stubEmbedder is an available embedder that returns a constant vector.
// It exists so the handler test can reach the NON-degraded branch: the
// kNN query then runs for real and simply finds nothing (no seeded row
// carries an embedding), which is exactly the "both layers ran, one had
// nothing to say" shape.
type stubEmbedder struct{}

func (stubEmbedder) EmbedSingle(_ context.Context, _ string) ([]float32, error) {
	v := make([]float32, 1024)
	v[0] = 1
	return v, nil
}

func (stubEmbedder) Available() bool { return true }

// callCheckDuplicates posts a title to the endpoint and returns the
// decoded body together with the raw bytes (needed to tell a JSON null
// from an absent key).
func callCheckDuplicates(
	t *testing.T,
	pool *pgxpool.Pool,
	depsStore *dependencies.Store,
	caller uuid.UUID,
	projectID uuid.UUID,
	title string,
) (map[string]any, []byte) {
	t.Helper()
	h := NewIssuesHandler(pool, projects.NewStore(pool), depsStore)

	payload, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	url := "/api/v1/projects/" + projectID.String() + "/issues/check-duplicates"
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.CheckDuplicates(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })

	raw := rec.Body.Bytes()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST check-duplicates = %d, want 200: %s", res.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, raw)
	}
	return body, raw
}

// TestCheckDuplicatesREST_ShapeOnBothBranches pins the response contract.
func TestCheckDuplicatesREST_ShapeOnBothBranches(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "duprest")
	testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1",
		"Add reports CSV export feature", "open", 2)

	// A one-character typo: pg_trgm scores the pair at 0.8333 — over
	// duplicateMinReport (0.80) so the row is reportable, but under
	// duplicateStrongMatch (0.85) so layer 1 does not decide alone and
	// the embedder branch is really taken. An exact title would
	// short-circuit and both sub-tests below would answer degraded=false.
	const query = "Add reports CSV export featire"

	t.Run("semantic layer ran: degraded false, degradedReason absent", func(t *testing.T) {
		body, raw := callCheckDuplicates(t, pool,
			dependencies.NewStoreWithEmbedder(pool, stubEmbedder{}), owner, pid, query)

		degraded, ok := body["degraded"].(bool)
		if !ok {
			t.Fatalf("degraded missing or not a bool: %s", raw)
		}
		if degraded {
			t.Fatalf("an available embedder must not report a degraded check: %s", raw)
		}
		if _, present := body["degradedReason"]; present {
			t.Errorf("degradedReason must be absent while degraded is false: %s", raw)
		}

		cands, ok := body["candidates"].([]any)
		if !ok {
			t.Fatalf("candidates missing or not an array: %s", raw)
		}
		if len(cands) != 1 {
			t.Fatalf("expected the exact-title match, got %d candidates: %s", len(cands), raw)
		}
		row, ok := cands[0].(map[string]any)
		if !ok {
			t.Fatalf("candidate is not an object: %s", raw)
		}
		for _, k := range []string{"id", "title", "status", "type", "similarity",
			"trigramSimilarity", "semanticSimilarity", "matchedBy"} {
			if _, present := row[k]; !present {
				t.Errorf("candidate is missing key %q — the whole point of sk-228 is that "+
					"provenance is stated, not inferred from which keys exist: %s", k, raw)
			}
		}
		// The distinction the wire has to carry: "this layer did not
		// score the row" is null, never an absent key and never 0.
		if v, present := row["semanticSimilarity"]; !present || v != nil {
			t.Errorf("semanticSimilarity = %#v (present=%v), want JSON null on a "+
				"trigram-only row: %s", v, present, raw)
		}
		if _, isNum := row["trigramSimilarity"].(float64); !isNum {
			t.Errorf("trigramSimilarity = %#v, want a JSON number on a trigram hit: %s",
				row["trigramSimilarity"], raw)
		}
		if got := row["matchedBy"]; got != dependencies.MatchedByTrigram {
			t.Errorf("matchedBy = %#v, want %q: %s", got, dependencies.MatchedByTrigram, raw)
		}
	})

	t.Run("no embedder: degraded true, degradedReason present", func(t *testing.T) {
		body, raw := callCheckDuplicates(t, pool,
			dependencies.NewStore(pool), owner, pid, query)

		degraded, ok := body["degraded"].(bool)
		if !ok {
			t.Fatalf("degraded missing or not a bool: %s", raw)
		}
		if !degraded {
			t.Fatalf("no embedder means the semantic layer never ran; degraded must be "+
				"true: %s", raw)
		}
		reason, ok := body["degradedReason"].(string)
		if !ok || reason == "" {
			t.Fatalf("degraded=true must come with a non-empty degradedReason — it is the "+
				"only surface that returns one: %s", raw)
		}
	})
}
