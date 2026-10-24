package issues

import (
	"slices"
	"testing"
)

// sk-540.4 — the archive preflight query and the writer-side guard against a
// row reopened between that preflight and the write.

// TestUnclosedWorkIDsInProject — the preflight reports exactly the rows of
// the project that are neither closed nor standing, in caller order, without
// repeats; unknown ids are absent (they fail later per row, as before).
func TestUnclosedWorkIDsInProject(t *testing.T) {
	env := newOutcomeEnv(t)

	open := env.issue(t, "preflight: open")
	inProgress := env.issue(t, "preflight: in progress")
	if _, err := env.store.UpdateStatus(t.Context(), inProgress.ID, UpdateStatusInput{
		ExpectedVersion: inProgress.Version, Status: StatusInProgress,
	}, env.actor); err != nil {
		t.Fatalf("to in_progress: %v", err)
	}
	closed := env.issue(t, "preflight: closed")
	if _, err := env.store.UpdateStatus(t.Context(), closed.ID, UpdateStatusInput{
		ExpectedVersion: closed.Version, Status: StatusClosed,
	}, env.actor); err != nil {
		t.Fatalf("close: %v", err)
	}
	standing := env.issue(t, "preflight: standing")
	if _, err := env.store.UpdateStatus(t.Context(), standing.ID, UpdateStatusInput{
		ExpectedVersion: standing.Version, Status: StatusStanding,
	}, env.actor); err != nil {
		t.Fatalf("to standing: %v", err)
	}
	unknown := env.prefix + "-99999"

	ids := []string{closed.ID, inProgress.ID, unknown, standing.ID, open.ID, inProgress.ID}
	got, err := env.store.UnclosedWorkIDsInProject(t.Context(), env.pid, ids)
	if err != nil {
		t.Fatalf("UnclosedWorkIDsInProject: %v", err)
	}
	want := []string{inProgress.ID, open.ID}
	if !slices.Equal(got, want) {
		t.Fatalf("UnclosedWorkIDsInProject(%v) = %v, want %v", ids, got, want)
	}

	got, err = env.store.UnclosedWorkIDsInProject(t.Context(), env.pid, []string{closed.ID, standing.ID})
	if err != nil {
		t.Fatalf("UnclosedWorkIDsInProject: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("closed/standing-only batch reported unclosed rows %v, want none", got)
	}
}

// TestArchive_RowReopenedAfterPreflightIsRefused — the race from the task's
// acceptance: the batch preflight sees a closed row, the row is reopened
// before its write, and the archive without an outcome must refuse THAT row
// (close_outcome_required) rather than archive it with a silent wont_do.
func TestArchive_RowReopenedAfterPreflightIsRefused(t *testing.T) {
	env := newOutcomeEnv(t)

	iss := env.issue(t, "reopened after preflight")
	if _, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
		ExpectedVersion: iss.Version, Status: StatusClosed,
	}, env.actor); err != nil {
		t.Fatalf("close: %v", err)
	}

	unclosed, err := env.store.UnclosedWorkIDsInProject(t.Context(), env.pid, []string{iss.ID})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(unclosed) != 0 {
		t.Fatalf("preflight on a closed row reported %v — the race is not reached", unclosed)
	}

	// The reopen lands between the preflight and the write.
	closedNow := readCloseStamps(t, env.pool, iss.ID)
	if _, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
		ExpectedVersion: closedNow.Version, Status: StatusOpen,
	}, env.actor); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened := readCloseStamps(t, env.pool, iss.ID)

	// The batch writes with the version it reads right before the write
	// (handler/bulk.go#runOne), so the version predicate does not save us.
	reason := "archived"
	_, err = env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
		ExpectedVersion: reopened.Version, Status: StatusClosed, Reason: &reason, Archive: true,
	}, env.actor)
	if ve, ok := IsValidationError(err); !ok || ve.Code != "close_outcome_required" {
		t.Fatalf("archive of the reopened row: err = %v, want close_outcome_required", err)
	}
	after := readCloseStamps(t, env.pool, iss.ID)
	if after.Status != StatusOpen || after.Outcome != nil || after.Version != reopened.Version {
		t.Fatalf("refused row changed: %+v (before %+v)", after, reopened)
	}
}
