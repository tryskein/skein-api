package issueops

// update_race_test.go — the reopen race of a close by skein_update under
// force (sk-540.1), moved here with the operation (sk-878.3.1 T024a): the
// seam afterClosePrecheckHook belongs to this package.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// TestUpdate_OutcomeRequiredSurvivesReopenRaceUnderForce — the race the
// in-transaction check exists for: with force=true the version predicate is
// bypassed, so an issue that was closed when the pre-check read it and is
// reopened before the close step must still be refused — never closed with
// a silent `done`. The refusal rolls back the whole change, the field patch
// included (US-14, SC-020): the title is not written and the version stays
// where the reopen left it.
func TestUpdate_OutcomeRequiredSurvivesReopenRaceUnderForce(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "")
	project, prefix := testsupport.SeedProject(t, pool, owner, "race")
	id := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "race probe", "open", 2)
	if _, err := pool.Exec(context.Background(), `UPDATE issues SET status = 'closed', closed_at = now(),
	        closed_reason = 'arrange', close_outcome = 'wont_do', version = version + 1 WHERE id = $1`, id); err != nil {
		t.Fatalf("arrange the closed issue: %v", err)
	}
	read := func() (version int, status, outcome, title string) {
		t.Helper()
		var o *string
		if err := pool.QueryRow(context.Background(),
			`SELECT version, status, close_outcome, title FROM issues WHERE id = $1`, id).Scan(&version, &status, &o, &title); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if o != nil {
			outcome = *o
		}
		return
	}

	reopenedVersion := 0
	afterClosePrecheckHook = func(ctx context.Context, issueID string) {
		if issueID != id {
			return
		}
		// Reopen outside the operation, as a concurrent writer would.
		if err := pool.QueryRow(ctx,
			`UPDATE issues SET status = 'open', closed_at = NULL, closed_reason = NULL,
			        close_outcome = NULL, version = version + 1
			  WHERE id = $1 RETURNING version`, issueID).Scan(&reopenedVersion); err != nil {
			t.Errorf("reopen inside the race window: %v", err)
		}
	}
	t.Cleanup(func() { afterClosePrecheckHook = nil })

	ctx := auth.WithUserID(context.Background(), owner)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, "sess-race")
	ctx = auth.WithAPIKeyID(ctx, uuid.New())
	ctx = auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelManage, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
	v, _, _, _ := read()
	_, f := Update(ctx, Deps{Pool: pool, Issues: issues.NewStore(pool)}, owner, Args{
		"id": id, "expectedVersion": v, "force": true,
		"title": "renamed in race", "status": "closed", "reason": "r",
	})
	if reopenedVersion == 0 {
		t.Fatalf("the race window was not reached")
	}
	if f == nil || f.Code != ErrCodeValidationError || f.Fields["code"] != "close_outcome_required" {
		t.Fatalf("close after the reopen race = %+v, want validation_error/close_outcome_required", f)
	}
	version, status, outcome, title := read()
	if status != "open" || outcome != "" {
		t.Errorf("after the refused close: status %s, outcome %q, want open and none", status, outcome)
	}
	if title == "renamed in race" {
		t.Errorf("title %q, want the field patch rolled back with the refused close", title)
	}
	if version != reopenedVersion {
		t.Errorf("version %d, want %d — the one the reopen left", version, reopenedVersion)
	}
}
