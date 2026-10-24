package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestUpdate_HappyPath_FieldPatch sets a couple of fields and asserts
// the response carries the post-state with version+1.
func TestUpdate_HappyPath_FieldPatch(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "uhp")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "before", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"title":           "after",
		"description":     "edited body",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	if v, _ := body["version"].(float64); int(v) != 2 {
		t.Fatalf("version = %v, want 2", body["version"])
	}

	// sk-233, tightened by sk-282 T020: the response is an
	// acknowledgement, so neither the prose the caller just sent nor the
	// short fields it patched come back. Assert the omission
	// explicitly...
	for _, echoed := range []string{"description", "title"} {
		if _, present := body[echoed]; present {
			t.Fatalf("skein_update must not echo %q, got %v", echoed, body[echoed])
		}
	}
	// ...and verify the patch actually landed by reading the row. This
	// is what the old response-echo assertion was really testing, and
	// checking the database instead makes it independent of the wire
	// shape entirely.
	var persistedTitle, persistedDescription string
	if err := pool.QueryRow(context.Background(),
		`SELECT title, description FROM issues WHERE id = $1`, issueID).
		Scan(&persistedTitle, &persistedDescription); err != nil {
		t.Fatalf("read back patched fields: %v", err)
	}
	if persistedTitle != "after" {
		t.Fatalf("persisted title = %q, want \"after\"", persistedTitle)
	}
	if persistedDescription != "edited body" {
		t.Fatalf("persisted description = %q, want \"edited body\"", persistedDescription)
	}
}

// TestUpdate_VersionConflict re-uses expectedVersion=1 after the issue
// was already bumped — must return version_conflict + currentVersion.
func TestUpdate_VersionConflict(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "uvc")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "v1", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	// First write: bumps to v2.
	first := mcpgo.CallToolRequest{}
	first.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"title":           "v2",
	}
	if res, err := handler(ctx, first); err != nil || res.IsError {
		t.Fatalf("first patch failed: err=%v res=%+v", err, res)
	}

	// Second write with stale expectedVersion=1.
	second := mcpgo.CallToolRequest{}
	second.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"title":           "v3-collision",
	}
	res, err := handler(ctx, second)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on stale version")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeVersionConflict {
		t.Fatalf("error = %v, want %s", got, ErrCodeVersionConflict)
	}
	cvRaw, isNum := body["currentVersion"].(float64)
	if !isNum || int(cvRaw) != 2 {
		t.Fatalf("currentVersion = %v, want 2", body["currentVersion"])
	}

	// Matrix row 91, second half (sk-282 T020): "a retry does not
	// require a full issue read". The retry is built from the
	// conflict body ALONE — no skein_show, no store read between the two
	// calls — and must succeed. Written this way on purpose: an
	// assertion that only checks the number is satisfied by a conflict
	// body that carries a version the retry would be rejected with.
	retry := mcpgo.CallToolRequest{}
	retry.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": int(cvRaw),
		"title":           "v3-after-conflict",
	}
	retryRes, err := handler(ctx, retry)
	if err != nil {
		t.Fatalf("retry handler error: %v", err)
	}
	if retryRes.IsError {
		t.Fatalf("retry with the conflict's own currentVersion was rejected: %+v",
			extractToolJSON(t, retryRes))
	}
	retryBody := extractToolJSON(t, retryRes)
	assertAcknowledgementShape(t, retryBody)
	if v, _ := retryBody["version"].(float64); int(v) != int(cvRaw)+1 {
		t.Fatalf("version after retry = %v, want %d", retryBody["version"], int(cvRaw)+1)
	}
	var persisted string
	if err := pool.QueryRow(context.Background(),
		`SELECT title FROM issues WHERE id = $1`, issueID).Scan(&persisted); err != nil {
		t.Fatalf("read back title: %v", err)
	}
	if persisted != "v3-after-conflict" {
		t.Fatalf("persisted title = %q, want the retried value", persisted)
	}
}

// TestUpdate_StatusChangeEmitsEvent routes through UpdateStatus and
// verifies an issue.status_changed event landed.
func TestUpdate_StatusChangeEmitsEvent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ust")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "before close", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"status":          "in_progress",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	if body["status"] != "in_progress" {
		t.Fatalf("status = %v, want in_progress", body["status"])
	}

	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type = 'issue' AND entity_id = $1
   AND type = 'issue.status_changed' AND actor_type = 'agent'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected one issue.status_changed event with actor_type=agent, got %d", n)
	}
}

// TestUpdate_ComponentsReplace asserts that supplying componentIds
// replaces the set on the issue (and rejects components in the wrong
// project).
func TestUpdate_ComponentsReplace(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ucr")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)

	cid := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, 'comp-a', '', '[]'::jsonb, NULL, $3, now(), now())`,
		cid, pid, userID); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"componentIds":    []string{cid.String()},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	// sk-282 T020: componentIds is card data, not post-state, so the
	// acknowledgement does not carry it — the replace is proven on the
	// link table.
	rows, err := pool.Query(context.Background(),
		`SELECT component_id FROM issue_components WHERE issue_id = $1`, issueID)
	if err != nil {
		t.Fatalf("read issue_components: %v", err)
	}
	defer rows.Close()
	var comps []uuid.UUID
	for rows.Next() {
		var got uuid.UUID
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan component: %v", err)
		}
		comps = append(comps, got)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate components: %v", err)
	}
	if len(comps) != 1 || comps[0] != cid {
		t.Fatalf("components = %v, want exactly [%s]", comps, cid)
	}
}

// TestUpdate_ComponentsWrongProject rejects components that do not
// belong to the issue's project with components_wrong_project.
func TestUpdate_ComponentsWrongProject(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid1, prefix1 := seedProject(t, pool, userID, "ucwpA")
	pid2, _ := seedProject(t, pool, userID, "ucwpB")
	issueID := prefix1 + "-1"
	seedIssue(t, pool, pid1, userID, issueID, "issue", "open", 2)

	cid := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, 'wrong', '', '[]'::jsonb, NULL, $3, now(), now())`,
		cid, pid2, userID); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"componentIds":    []string{cid.String()},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for wrong-project component")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeComponentsWrongProject {
		t.Fatalf("error = %v, want %s", got, ErrCodeComponentsWrongProject)
	}
}

// TestUpdate_LabelsAddAndRemove combines labelsAdd + labelsRemove in
// one call. RemoveLabel runs after AddLabel so we add the label, then
// remove a different one we pre-seed.
func TestUpdate_LabelsAddAndRemove(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ular")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)
	// Seed a pre-existing label so we have something to remove.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO issue_labels (issue_id, label) VALUES ($1, 'pre-existing')`,
		issueID); err != nil {
		t.Fatalf("seed label: %v", err)
	}

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"labelsAdd":       []string{"newly-added"},
		"labelsRemove":    []string{"pre-existing"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	// sk-282 T020: labels are card data — read them back rather than
	// expecting the acknowledgement to echo them.
	var labels []string
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(array_agg(label ORDER BY label), '{}')
		   FROM issue_labels WHERE issue_id = $1`, issueID).Scan(&labels); err != nil {
		t.Fatalf("read back labels: %v", err)
	}
	hasNew := false
	hasOld := false
	for _, l := range labels {
		if l == "newly-added" {
			hasNew = true
		}
		if l == "pre-existing" {
			hasOld = true
		}
	}
	if !hasNew {
		t.Fatalf("labels missing newly-added: %v", labels)
	}
	if hasOld {
		t.Fatalf("labels still has pre-existing: %v", labels)
	}
}

// TestUpdate_NotFound asserts that an unknown id surfaces issue_not_found.
func TestUpdate_NotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              "no-such-9999",
		"expectedVersion": 1,
		"title":           "irrelevant",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing issue")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeIssueNotFound)
	}
}

// TestUpdate_LockedByOther: lock held by another session, update without
// force → locked_by_other.
func TestUpdate_LockedByOther(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulbo")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "owned", "open", 2)

	issuesStore := issues.NewStore(pool)
	claimHandler := handleClaim(pool, issuesStore)
	updateHandler := handleUpdate(pool, issuesStore)

	// sess-A claims.
	ctxA := withCtxSession(t, userID, "admin", "sess-A")
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctxA, cl); err != nil || r.IsError {
		t.Fatalf("seed claim: %v %+v", err, r)
	}

	// sess-B (same user, different session) tries to update.
	ctxB := withCtxSession(t, userID, "admin", "sess-B")
	upd := mcpgo.CallToolRequest{}
	upd.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"title":           "blocked",
	}
	res, err := updateHandler(ctxB, upd)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for locked-by-other update")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeLockedByOther {
		t.Fatalf("error = %v, want %s", got, ErrCodeLockedByOther)
	}
}

// TestUpdate_LockedBySelf: I hold the lock → update succeeds.
func TestUpdate_LockedBySelf(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulbs")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "mine", "open", 2)

	issuesStore := issues.NewStore(pool)
	claimHandler := handleClaim(pool, issuesStore)
	updateHandler := handleUpdate(pool, issuesStore)
	ctx := withCtxSession(t, userID, "admin", "sess-self")

	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctx, cl); err != nil || r.IsError {
		t.Fatalf("seed claim: %v %+v", err, r)
	}

	upd := mcpgo.CallToolRequest{}
	upd.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"title":           "self-update",
	}
	res, err := updateHandler(ctx, upd)
	if err != nil || res.IsError {
		t.Fatalf("self-locked update failed: err=%v body=%+v", err,
			func() any {
				if res == nil {
					return nil
				}
				return extractToolJSON(t, res)
			}())
	}
}

// TestUpdate_AdminForceOverridesLock: admin force=true on a locked-by-
// other issue → success, every emitted event reason="force-overridden".
func TestUpdate_AdminForceOverridesLock(t *testing.T) {
	pool := newTestPool(t)
	admin, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, admin, "ulafol")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, admin, issueID, "force-it", "open", 2)

	issuesStore := issues.NewStore(pool)
	claimHandler := handleClaim(pool, issuesStore)
	updateHandler := handleUpdate(pool, issuesStore)

	ctxA := withCtxSession(t, admin, "admin", "sess-A")
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctxA, cl); err != nil || r.IsError {
		t.Fatalf("seed claim: %v %+v", err, r)
	}

	ctxB := withCtxSession(t, admin, "admin", "sess-B")
	upd := mcpgo.CallToolRequest{}
	upd.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"priority":        4, // change priority so we get an issue.field_changed (field=priority) row
		"force":           true,
	}
	res, err := updateHandler(ctxB, upd)
	if err != nil || res.IsError {
		t.Fatalf("admin force update failed: %v %+v", err, res)
	}

	// R3.2b hybrid event layout: PatchIssue now writes one issue.updated
	// parent + N issue.field_changed children. The priority change shows
	// up as an issue.field_changed row with field='priority'; force=true
	// writes reason='force-overridden' on every row in the family.
	var reason string
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(reason, '') FROM events
 WHERE entity_type='issue' AND entity_id=$1
   AND type='issue.field_changed' AND field='priority'
 ORDER BY id DESC LIMIT 1`,
		issueID).Scan(&reason); err != nil {
		t.Fatalf("query event reason: %v", err)
	}
	if reason != "force-overridden" {
		t.Fatalf("issue.field_changed (priority) reason = %q, want \"force-overridden\"", reason)
	}
}

// TestUpdate_FreeIssueUnchanged: update on a free issue continues to
// work as in R2.2 (no lock, no force, plain happy path).
func TestUpdate_FreeIssueUnchanged(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulfu")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "free", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtxSession(t, userID, "admin", "sess-free")

	upd := mcpgo.CallToolRequest{}
	upd.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"title":           "free-update",
	}
	res, err := handler(ctx, upd)
	if err != nil || res.IsError {
		t.Fatalf("free-issue update failed: %v %+v", err, res)
	}
}

// TestUpdate_StatusClosedRoutesToClose: skein_update(status=closed)
// should route to Close() — version+1, locked_by cleared,
// issue.closed event written.
func TestUpdate_StatusClosedRoutesToClose(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulscr")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "to-close", "open", 2)

	issuesStore := issues.NewStore(pool)
	claimHandler := handleClaim(pool, issuesStore)
	updateHandler := handleUpdate(pool, issuesStore)
	ctx := withCtxSession(t, userID, "admin", "sess-close")

	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctx, cl); err != nil || r.IsError {
		t.Fatalf("seed claim: %v %+v", err, r)
	}

	upd := mcpgo.CallToolRequest{}
	upd.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"status":          "closed",
		"closeOutcome":    "done",
		"reason":          "done-via-update",
	}
	res, err := updateHandler(ctx, upd)
	if err != nil || res.IsError {
		t.Fatalf("update→close failed: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	if body["status"] != "closed" {
		t.Fatalf("status = %v, want closed", body["status"])
	}
	if body["lockedBy"] != nil {
		t.Fatalf("lockedBy = %v, want nil after close", body["lockedBy"])
	}

	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.closed'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 issue.closed event from update→close, got %d", n)
	}
}

// TestUpdate_StatusStandingAcceptedByEnum exercises the §11.1 promote
// path: skein_update(status="standing") on a plain OPEN issue (no
// standing parent/child) passes the wire-level enum schema validation,
// reaches the store, and the issue becomes `standing`. open→standing is
// an allowed transition per the matrix (F2c adds the reject cases; this
// happy path stays valid).
func TestUpdate_StatusStandingAcceptedByEnum(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "usse")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "to-promote", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"status":          "standing",
		"reason":          "promote to perpetual obligation",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true (enum should accept standing): %+v",
			extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if body["status"] != "standing" {
		t.Fatalf("status = %v, want standing", body["status"])
	}

	// Confirm the new status actually landed in the store.
	var dbStatus string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM issues WHERE id = $1`, issueID).Scan(&dbStatus); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if dbStatus != "standing" {
		t.Fatalf("db status = %q, want standing", dbStatus)
	}
}

// TestUpdate_MissingExpectedVersion exercises the explicit guard.
func TestUpdate_MissingExpectedVersion(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "umev")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "issue", "open", 2)

	issuesStore := issues.NewStore(pool)
	handler := handleUpdate(pool, issuesStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":    issueID,
		"title": "no version",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing expectedVersion")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}
