package handler

// ready_rest_contract_test.go — sk-282 T017, test-plan row 67.
//
// "INV-2: REST ready-to-work listing — key set, types,
// representative values AND THE PREVIOUS PAGE SIZE (REST has its own
// default, separate from MCP)."
//
// T017 removes `labels` and `componentIds` from the MCP ready row. The
// obvious implementation — dropping them from dependencies.IssueSummary,
// or marshalling through a slimmer type in the store — would remove them
// from the REST response too. INV-2 forbids a REST response changing its
// field set in EITHER direction, and a removal is what the web UI notices
// first, so the key set here is exact and the two names are asserted
// PRESENT by name.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/testsupport"
)

// restReadyRowKeys is the REST ready row's EXACT always-present field set.
var restReadyRowKeys = map[string]string{
	"id": "string", "projectId": "string", "version": "number",
	"title": "string", "type": "string", "priority": "number",
	"status": "string", "pinned": "bool",
	"componentIds": "array", "labels": "array",
	"updatedAt": "string", "createdAt": "string",
}

var restReadyRowOptionalKeys = map[string]string{
	"parentIdCurrent": "string", "assignee": "string",
}

// TestReadyRowKeys_MatchStructTags ratchets the hand-written lists above
// against the carrier's json tags.
func TestReadyRowKeys_MatchStructTags(t *testing.T) {
	declared := map[string]bool{}
	rt := reflect.TypeOf(dependencies.IssueSummary{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		declared[name] = true

		_, required := restReadyRowKeys[name]
		_, optional := restReadyRowOptionalKeys[name]
		switch {
		case !required && !optional:
			t.Errorf("dependencies.IssueSummary declares %q, which neither list classifies", name)
		case required && strings.Contains(opts, "omitempty"):
			t.Errorf("%q is listed as always-present but carries omitempty", name)
		case optional && !strings.Contains(opts, "omitempty"):
			t.Errorf("%q is listed as optional but has no omitempty", name)
		}
	}
	for name := range restReadyRowKeys {
		if !declared[name] {
			t.Errorf("the required-key list names %q, which the carrier does not declare", name)
		}
	}
	for name := range restReadyRowOptionalKeys {
		if !declared[name] {
			t.Errorf("the optional-key list names %q, which the carrier does not declare", name)
		}
	}
}

// TestReadyREST_KeepsLabelAndComponentKeysAndPageSize is row 67.
func TestReadyREST_KeepsLabelAndComponentKeysAndPageSize(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restrdy")

	// More rows than the MCP default so a clamp leaking here is visible.
	const rows = 30
	for i := 1; i <= rows; i++ {
		testsupport.SeedIssue(t, pool, pid, owner, prefix+"-"+strconv.Itoa(i),
			"ready row "+strconv.Itoa(i), "open", 0)
	}

	h := NewReadyHandler(pool, dependencies.NewStore(pool))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/"+pid.String()+"/ready", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", pid.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, owner)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.Ready(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET ready = %d", res.StatusCode)
	}
	body := decodeReadyBody(t, res.Body)

	if len(body) < rows {
		t.Errorf("REST ready returned %d of %d rows — its page size is its own, separate from "+
			"the MCP one", len(body), rows)
	}
	for _, row := range body {
		// The removal vector, asserted by name.
		for _, key := range []string{"labels", "componentIds"} {
			if _, ok := row[key]; !ok {
				t.Errorf("REST ready row LOST %q — T017 removes it in the MCP projection only; "+
					"INV-2 forbids a REST response changing its field set in either direction, "+
					"and a removal is what the web UI notices first", key)
			}
		}
		for key, value := range row {
			wantKind, known := restReadyRowKeys[key]
			if !known {
				wantKind, known = restReadyRowOptionalKeys[key]
			}
			if !known {
				t.Errorf("REST ready row carries an unclassified key %q = %#v", key, value)
				continue
			}
			assertJSONKind(t, "REST ready row", key, value, wantKind)
		}
		for key := range restReadyRowKeys {
			if _, ok := row[key]; !ok {
				t.Errorf("REST ready row is MISSING %q — the set is exact, not an upper bound", key)
			}
		}
	}
}

// decodeReadyBody accepts either a bare array or an {items:[…]} envelope,
// so the test states the row contract without pinning an envelope shape it
// is not the owner of.
func decodeReadyBody(t *testing.T, r interface{ Read([]byte) (int, error) }) []map[string]any {
	t.Helper()
	var raw json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var asArray []map[string]any
	if err := json.Unmarshal(raw, &asArray); err == nil {
		return asArray
	}
	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("ready body is neither an array nor an {items} envelope: %s", raw)
	}
	return envelope.Items
}
