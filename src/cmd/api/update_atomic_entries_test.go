package main

// update_atomic_entries_test.go — matrix row 39 (US-14 "in every entry").
//
// Reference — the spec, written before the code: a change of one issue is
// applied whole or not at all in every entry (REST, MCP, UI); in a batch
// each element is atomic and the batch as a whole is a partial result.
// The refusal is injected by a test trigger on the table of the LAST write
// of the change (removed in t.Cleanup), so an entry that still commits its
// earlier writes separately leaves them behind and fails here.
//
// Everything goes through the production router (newParityStack): skein_update,
// skein_close with commit refs, an element of skein_update_many and of
// skein_close_many, the /ui/v1 PATCH of an issue and the /ui/v1 bulk close.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const atomicEntriesRepo = "https://git.example.test/t015.git"

// failRowsOf makes every row-level `timing` statement on table whose
// `column` equals id raise until the test ends.
func failRowsOf(t *testing.T, pool *pgxpool.Pool, table, timing, column, id string) {
	t.Helper()
	ctx := context.Background()
	name := "t015_entry_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN
  RAISE EXCEPTION 'injected failure on %s';
END $f$`, name, table)); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	row := "NEW"
	if strings.Contains(timing, "DELETE") {
		row = "OLD"
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE TRIGGER %s %s ON %s FOR EACH ROW WHEN (%s.%s = '%s') EXECUTE FUNCTION %s()`,
		name, timing, table, row, column, id, name)); err != nil {
		t.Fatalf("create trigger on %s: %v", table, err)
	}
}

// entryState is what a change of the issue may have written.
type entryState struct {
	Version int
	Title   string
	Status  string
	Labels  int
	Commits int
	Events  int
}

func readEntryState(t *testing.T, pool *pgxpool.Pool, id string) entryState {
	t.Helper()
	var st entryState
	if err := pool.QueryRow(context.Background(), `
SELECT i.version, i.title, i.status,
       (SELECT count(*) FROM issue_labels l WHERE l.issue_id = i.id),
       (SELECT count(*) FROM issue_commits c WHERE c.issue_id = i.id),
       (SELECT count(*) FROM events e WHERE e.entity_type = 'issue' AND e.entity_id = i.id)
  FROM issues i WHERE i.id = $1`, id).
		Scan(&st.Version, &st.Title, &st.Status, &st.Labels, &st.Commits, &st.Events); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return st
}

func TestUpdateAtomic_EveryEntryWritesNothingOnRefusal(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	p := s.f.AMembers
	base := "/ui/v1/projects/" + p.ID.String() + "/issues"
	testsupport.SeedRepo(t, s.pool, p.ID, "t015", atomicEntriesRepo)
	actor := testsupport.ActorOnlyA
	sid := s.session(actor)
	hash := func(i int) string { return fmt.Sprintf("%040x", 0xa7000+i) }

	fresh := func() string {
		var n int
		if err := s.pool.QueryRow(ctx, `UPDATE project_counters SET counter = counter + 1
 WHERE project_id = $1 RETURNING counter`, p.ID).Scan(&n); err != nil {
			t.Fatalf("next number of %s: %v", p.IDPrefix, err)
		}
		id := fmt.Sprintf("%s-%d", p.IDPrefix, n)
		return testsupport.SeedIssue(t, s.pool, p.ID, p.OwnerID, id, "t015 "+id, "open", 2)
	}
	toolBody := func(r toolResult) map[string]any {
		var body map[string]any
		if err := json.Unmarshal([]byte(r.text), &body); err != nil {
			t.Fatalf("tool answer is not JSON: %q", r.text)
		}
		return body
	}
	unchanged := func(t *testing.T, id string, before entryState) {
		t.Helper()
		if after := readEntryState(t, s.pool, id); after != before {
			t.Errorf("%s changed after the refusal: %+v → %+v, want nothing written", id, before, after)
		}
	}
	rowOK := func(t *testing.T, body map[string]any, i int) bool {
		t.Helper()
		rows, _ := body["results"].([]any)
		if len(rows) <= i {
			t.Fatalf("results = %v, want a row %d", body["results"], i)
		}
		row, _ := rows[i].(map[string]any)
		ok, _ := row["ok"].(bool)
		return ok
	}

	t.Run("skein_update: the label step fails after the field patch", func(t *testing.T) {
		id := fresh()
		failRowsOf(t, s.pool, "issue_labels", "BEFORE INSERT", "issue_id", id)
		before := readEntryState(t, s.pool, id)
		r := s.tool(actor, sid, "skein_update", map[string]any{
			"id": id, "expectedVersion": before.Version,
			"title": "t015 renamed", "status": "deferred", "labelsAdd": []any{"t015"},
		})
		if !r.isError {
			t.Fatalf("skein_update answered ok although its label step failed: %s", r.text)
		}
		unchanged(t, id, before)
	})

	t.Run("skein_close with commit refs: the commit step fails", func(t *testing.T) {
		id := fresh()
		failRowsOf(t, s.pool, "issue_commits", "BEFORE INSERT", "issue_id", id)
		before := readEntryState(t, s.pool, id)
		r := s.tool(actor, sid, "skein_close", map[string]any{
			"id": id, "expectedVersion": before.Version, "reason": "t015", "closeOutcome": "done",
			"commits": []any{map[string]any{"hash": hash(1), "repoUrl": atomicEntriesRepo}},
		})
		if !r.isError {
			t.Fatalf("skein_close answered ok although its commit step failed: %s", r.text)
		}
		unchanged(t, id, before)
	})

	t.Run("skein_update_many: the failed element is empty, the other applied", func(t *testing.T) {
		failing, other := fresh(), fresh()
		failRowsOf(t, s.pool, "issue_labels", "BEFORE INSERT", "issue_id", failing)
		before, otherBefore := readEntryState(t, s.pool, failing), readEntryState(t, s.pool, other)
		item := func(id string, v int) map[string]any {
			return map[string]any{"id": id, "expectedVersion": v, "title": "t015 batch", "labelsAdd": []any{"t015"}}
		}
		body := toolBody(s.tool(actor, sid, "skein_update_many", map[string]any{"items": []any{
			item(failing, before.Version), item(other, otherBefore.Version),
		}}))
		if rowOK(t, body, 0) {
			t.Errorf("the failing element answered ok: %v", body)
		}
		unchanged(t, failing, before)
		if !rowOK(t, body, 1) {
			t.Fatalf("the other element was not applied: %v", body)
		}
		if got := readEntryState(t, s.pool, other); got.Title != "t015 batch" || got.Labels != otherBefore.Labels+1 {
			t.Errorf("other element = %+v, want title and label written", got)
		}
	})

	t.Run("skein_close_many: the failed element is empty, the other applied", func(t *testing.T) {
		failing, other := fresh(), fresh()
		failRowsOf(t, s.pool, "issue_commits", "BEFORE INSERT", "issue_id", failing)
		before, otherBefore := readEntryState(t, s.pool, failing), readEntryState(t, s.pool, other)
		item := func(id string, v, n int) map[string]any {
			return map[string]any{"id": id, "expectedVersion": v, "reason": "t015", "closeOutcome": "done",
				"commits": []any{map[string]any{"hash": hash(n), "repoUrl": atomicEntriesRepo}}}
		}
		body := toolBody(s.tool(actor, sid, "skein_close_many", map[string]any{"items": []any{
			item(failing, before.Version, 2), item(other, otherBefore.Version, 3),
		}}))
		if rowOK(t, body, 0) {
			t.Errorf("the failing element answered ok: %v", body)
		}
		unchanged(t, failing, before)
		if !rowOK(t, body, 1) {
			t.Fatalf("the other element was not applied: %v", body)
		}
		if got := readEntryState(t, s.pool, other); got.Status != "closed" || got.Commits != 1 {
			t.Errorf("other element = %+v, want closed with its commit ref", got)
		}
	})

	t.Run("/ui/v1 PATCH: the event write fails after the row update", func(t *testing.T) {
		id := fresh()
		failRowsOf(t, s.pool, "events", "BEFORE INSERT", "entity_id", id)
		before := readEntryState(t, s.pool, id)
		status, raw := s.restWith(actor, http.MethodPatch, base+"/"+id,
			map[string]any{"title": "t015 rest"}, map[string]string{"If-Match": fmt.Sprint(before.Version)})
		if status >= 200 && status < 300 {
			t.Fatalf("PATCH answered %d although its event write failed: %s", status, raw)
		}
		unchanged(t, id, before)
	})

	t.Run("/ui/v1 bulk close: the failed element is empty, the other applied", func(t *testing.T) {
		failing, other := fresh(), fresh()
		failRowsOf(t, s.pool, "events", "BEFORE INSERT", "entity_id", failing)
		before := readEntryState(t, s.pool, failing)
		status, raw := s.rest(actor, http.MethodPost, base+"/bulk", map[string]any{
			"ids": []string{failing, other}, "action": "close", "closeOutcome": "done",
		})
		var resp struct {
			Succeeded []string `json:"succeeded"`
			Failed    []struct {
				ID string `json:"id"`
			} `json:"failed"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("bulk answered %d %s: %v", status, raw, err)
		}
		if len(resp.Failed) != 1 || resp.Failed[0].ID != failing {
			t.Errorf("failed = %+v, want exactly %s", resp.Failed, failing)
		}
		unchanged(t, failing, before)
		if len(resp.Succeeded) != 1 || resp.Succeeded[0] != other {
			t.Errorf("succeeded = %v, want exactly %s", resp.Succeeded, other)
		}
		if got := readEntryState(t, s.pool, other); got.Status != "closed" {
			t.Errorf("other element = %+v, want closed", got)
		}
	})
}
