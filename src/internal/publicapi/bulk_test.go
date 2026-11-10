package publicapi_test

// bulk_test.go — row 94 (sk-878.3.1 T025; «Batches», edge «a batch partly
// succeeds», OST-2-7, US-2): bulkUpdateIssues and bulkCloseIssues answer 207,
// a row with a stale version answers version_conflict with the current
// version and the others are applied; a refusal of the whole request (the
// right, the format, the limit) applies no row; force=true with a write key
// is 403 insufficient_scope and no row is applied; with manage and
// force_unlock it passes, a row of a project without force_unlock is
// refused force_unlock_forbidden; bulkAddDependencies answers 207 and a row
// referencing an issue outside the key's scope has the code MCP gives; a
// repeat of a force batch applies again (documented); the rows are those of
// update_many, close_many and dep_add_many.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// rows are the results of a batch answer, keyed by issue id (fromId for
// edges): the row's error code, "" for a success.
func rows(t *testing.T, raw map[string]any, idKey string) map[string]string {
	t.Helper()
	list, _ := raw["results"].([]any)
	out := map[string]string{}
	for _, it := range list {
		m, _ := it.(map[string]any)
		id, _ := m[idKey].(string)
		code := ""
		if e, ok := m["error"].(map[string]any); ok {
			code, _ = e["code"].(string)
		}
		out[id] = code
	}
	return out
}

func TestBulk_Batches(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	kw := w.key(w.owner, apikeys.LevelWrite)
	km := w.key(w.owner, apikeys.LevelManage)
	kr := w.key(w.owner, apikeys.LevelRead)

	t.Run("a stale row does not stop the others", func(t *testing.T) {
		a, b := w.issue("a"), w.issue("b")
		va, vb := w.version(a), w.version(b)
		body := map[string]any{"items": []map[string]any{
			{"id": a, "expectedVersion": va, "title": "batched a"},
			{"id": b, "expectedVersion": vb + 3, "title": "batched b"},
		}}
		r := rg.rest(kw, http.MethodPost, "/api/v1/issues/bulk-update", body, nil)
		wantOK(t, "bulkUpdateIssues", r, http.StatusMultiStatus)
		got := rows(t, r.json(t), "id")
		if got[a] != "" || got[b] != "version_conflict" {
			t.Errorf("rows %v", got)
		}
		for _, it := range r.json(t)["results"].([]any) {
			m := it.(map[string]any)
			if m["id"] == b {
				d, _ := m["error"].(map[string]any)["details"].(map[string]any)
				if cur, _ := d["currentVersion"].(float64); int(cur) != vb {
					t.Errorf("the stale row's currentVersion %v, want %d", d["currentVersion"], vb)
				}
			}
		}
		if w.version(a) != va+1 || w.version(b) != vb {
			t.Errorf("versions after the batch: a %d (was %d), b %d (was %d)", w.version(a), va, w.version(b), vb)
		}
		// Parity: the same batch on twins through skein_update_many.
		ma, mb := w.issue("a"), w.issue("b")
		m := rg.tool(kw, "skein_update_many", map[string]any{"items": []map[string]any{
			{"id": ma, "expectedVersion": w.version(ma), "title": "batched a"},
			{"id": mb, "expectedVersion": w.version(mb) + 3, "title": "batched b"},
		}})
		mr := rows(t, m.json(t), "id")
		if mr[ma] != got[a] || mr[mb] != got[b] {
			t.Errorf("rows %v, MCP %v", got, mr)
		}
	})

	t.Run("bulkCloseIssues", func(t *testing.T) {
		a, b := w.issue("ca"), w.issue("cb")
		r := rg.rest(kw, http.MethodPost, "/api/v1/issues/bulk-close", map[string]any{"items": []map[string]any{
			{"id": a, "expectedVersion": w.version(a), "reason": "done", "closeOutcome": "done"},
			{"id": b, "expectedVersion": w.version(b) + 2, "reason": "done", "closeOutcome": "done"},
		}}, nil)
		wantOK(t, "bulkCloseIssues", r, http.StatusMultiStatus)
		got := rows(t, r.json(t), "id")
		ma, mb := w.issue("ca"), w.issue("cb")
		m := rows(t, rg.tool(kw, "skein_close_many", map[string]any{"items": []map[string]any{
			{"id": ma, "expectedVersion": w.version(ma), "reason": "done", "closeOutcome": "done"},
			{"id": mb, "expectedVersion": w.version(mb) + 2, "reason": "done", "closeOutcome": "done"},
		}}).json(t), "id")
		if got[a] != "" || got[b] != "version_conflict" || m[ma] != got[a] || m[mb] != got[b] {
			t.Errorf("rows %v, MCP %v", got, m)
		}
	})

	t.Run("a refusal of the whole request applies no row", func(t *testing.T) {
		a := w.issue("whole")
		v := w.version(a)
		row := map[string]any{"id": a, "expectedVersion": v, "title": "never"}
		for _, c := range []struct {
			name   string
			key    string
			body   any
			status int
		}{
			{"the right", kr, map[string]any{"items": []map[string]any{row}}, http.StatusForbidden},
			{"the format", kw, map[string]any{"items": []map[string]any{row}, "nosuchfield": 1}, http.StatusBadRequest},
			{"no rows", kw, map[string]any{"items": []map[string]any{}}, http.StatusBadRequest},
			{"the limit", kw, map[string]any{"items": make([]map[string]any, 101)}, http.StatusBadRequest},
		} {
			if a := rg.rest(c.key, http.MethodPost, "/api/v1/issues/bulk-update", c.body, nil); a.status != c.status {
				t.Errorf("%s: %d %s, want %d", c.name, a.status, a.body, c.status)
			}
		}
		if w.version(a) != v {
			t.Errorf("a refused batch changed the issue")
		}
	})

	t.Run("force", func(t *testing.T) {
		mine := w.issue("forced")
		w.exec(`UPDATE issues SET locked_by = 'mcp-session-other', locked_at = now(), status = 'in_progress' WHERE id = $1`, mine)
		// A project where the owner is a member only, in a space he does not
		// own: no force_unlock there.
		qSpace, _ := seedOwnSpace(t, pool, w.member)
		testsupport.AddSpaceMember(t, pool, qSpace, w.owner)
		q, qPrefix := seedProject(t, pool, w.member, qSpace, "wrf")
		addMember(t, pool, q, w.owner, "member")
		theirs := testsupport.SeedIssue(t, pool, q, w.member, qPrefix+"-1", "theirs "+uuid.NewString()[:6], "open", 2)
		items := func() map[string]any {
			return map[string]any{"items": []map[string]any{
				{"id": mine, "expectedVersion": 1, "title": "forced " + uuid.NewString()[:6]},
				{"id": theirs, "expectedVersion": 1, "title": "forced"},
			}}
		}
		vMine := w.version(mine)
		if a := rg.rest(kw, http.MethodPost, "/api/v1/issues/bulk-update?force=true", items(), nil); a.status != http.StatusForbidden || a.code() != "insufficient_scope" {
			t.Errorf("force with write: %d %s", a.status, a.body)
		}
		if w.version(mine) != vMine {
			t.Fatalf("a refused force batch applied a row")
		}
		r := rg.rest(km, http.MethodPost, "/api/v1/issues/bulk-update?force=true", items(), nil)
		wantOK(t, "force with manage", r, http.StatusMultiStatus)
		got := rows(t, r.json(t), "id")
		if got[mine] != "" || got[theirs] != "force_unlock_forbidden" {
			t.Errorf("force rows %v", got)
		}
		// The repeat of a force batch applies again: it does not check the
		// version (documented).
		v := w.version(mine)
		wantOK(t, "the repeat", rg.rest(km, http.MethodPost, "/api/v1/issues/bulk-update?force=true", items(), nil), http.StatusMultiStatus)
		if w.version(mine) != v+1 {
			t.Errorf("the repeat of a force batch did not apply: version %d, was %d", w.version(mine), v)
		}
	})

	// A batch does not accept Idempotency-Key (spec, edge cases; OST-2-7):
	// 400 idempotency_not_supported, nothing runs.
	t.Run("Idempotency-Key is refused", func(t *testing.T) {
		a := w.issue("idem")
		v := w.version(a)
		for _, c := range []struct {
			path string
			body map[string]any
		}{
			{"/api/v1/issues/bulk-update", map[string]any{"items": []map[string]any{{"id": a, "expectedVersion": v, "title": "idem"}}}},
			{"/api/v1/issues/bulk-close", map[string]any{"items": []map[string]any{{"id": a, "expectedVersion": v, "reason": "r", "closeOutcome": "done"}}}},
			{"/api/v1/issues/dependencies/bulk-add", map[string]any{"items": []map[string]any{{"fromId": a, "toId": w.issue("idem to"), "type": "related"}}}},
		} {
			since := lastEventID(t, pool)
			r := rg.rest(kw, http.MethodPost, c.path, c.body, map[string]string{"Idempotency-Key": "k-" + uuid.NewString()})
			if r.status != http.StatusBadRequest || r.code() != "idempotency_not_supported" {
				t.Errorf("%s with Idempotency-Key: %d %s, want 400 idempotency_not_supported", c.path, r.status, r.body)
			}
			if w.version(a) != v || len(w.issueEvents(a, since)) != 0 {
				t.Errorf("%s with Idempotency-Key wrote", c.path)
			}
		}
	})

	t.Run("bulkAddDependencies", func(t *testing.T) {
		q, qPrefix := seedProject(t, pool, w.owner, w.space, "wrd")
		outside := testsupport.SeedIssue(t, pool, q, w.owner, qPrefix+"-1", "outside", "open", 2)
		scoped := projectsKey(t, pool, w.owner, apikeys.LevelWrite, w.project)
		missing := w.prefix + "-99999"
		a, b, c := w.issue("da"), w.issue("db"), w.issue("dc")
		items := []map[string]any{{"fromId": a, "toId": b, "type": "related"}, {"fromId": b, "toId": outside, "type": "related"},
			{"fromId": c, "toId": missing, "type": "related"}}
		r := rg.rest(scoped, http.MethodPost, "/api/v1/issues/dependencies/bulk-add", map[string]any{"items": items}, nil)
		wantOK(t, "bulkAddDependencies", r, http.StatusMultiStatus)
		got := rows(t, r.json(t), "fromId")
		ma, mb, mc := w.issue("da"), w.issue("db"), w.issue("dc")
		m := rows(t, rg.tool(scoped, "skein_dep_add_many", map[string]any{"items": []map[string]any{
			{"fromId": ma, "toId": mb, "type": "related"}, {"fromId": mb, "toId": outside, "type": "related"},
			{"fromId": mc, "toId": missing, "type": "related"}}}).json(t), "fromId")
		// A target outside the key's scope is refused as a missing one, with
		// the code of a missing id (US-3), on both channels.
		if got[a] != "" || got[b] != "issue_not_found" || got[c] != "issue_not_found" {
			t.Errorf("rows %v, want the first applied and the other two issue_not_found", got)
		}
		if m[ma] != got[a] || m[mb] != got[b] || m[mc] != got[c] {
			t.Errorf("rows %v, MCP %v", got, m)
		}
	})
}
