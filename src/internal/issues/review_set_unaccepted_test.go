package issues

// review_set_unaccepted_test.go — sk-303.14, task T012.
//
// T012 gate: "every row of the status-resolution table is confirmed
// SEPARATELY for unaccepted".
//
// Why separately, not as one "positive/negative" test. The spec's
// resolution table is six rows, and three of them are counter-intuitive:
// deferred nudges, "in progress" WITHOUT acceptance nudges, and a
// standing task does not nudge, even though it is formally not closed. A
// single summary test on "the set of expected ids" goes red as a whole
// from any breakage and does not say WHICH contract row is broken; a
// per-row subtest does.
//
// Why the check is a DIRECT query of the fragment, not through a
// consumer. The unaccepted set has no public consumers in this group yet
// (the counter and the filter exist only for unreviewed; the nudge
// selection is task T017). The fragment is defined here precisely because
// its consumers live in three different groups and the predicate must
// exist before the first of them. So it can only be checked here
// directly — otherwise it would ship untested and its first reader would
// be a background worker.
//
// Mutations this file is written against:
//   - replace status.ActiveWork with "status <> 'closed'" → only the
//     "standing" row goes red;
//   - drop "accepted_at IS NULL" → the "accepted" row goes red and
//     (through the shared set) nothing else;
//   - replace EXISTS(users …) with LOWER(assignee) = LOWER(name) → the
//     deactivated addressee and the unresolvable assignee go red;
//   - drop "u.deactivated_at IS NULL" → only the deactivated one goes red.

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/review"
)

// unacceptedMember runs the FRAGMENT (not a copy of it) against the
// database and returns membership of one concrete issue.
//
// The query is narrowed by id on purpose: the shared test database is
// shared across packages, and "select the whole set" would return rows
// from other runs. The narrowing sits OUTSIDE the fragment and does not
// weaken its conditions.
func unacceptedMember(t *testing.T, pool *pgxpool.Pool, issueID string, addressee uuid.UUID) bool {
	t.Helper()
	params := []any{}
	bind := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}
	pred := review.UnacceptedPredicate(bind, review.UnacceptedArgs{
		Alias:       "i",
		AddresseeID: addressee,
	})
	idPh := bind(issueID)
	sql := "SELECT EXISTS (SELECT 1 FROM issues i WHERE i.id = " + idPh + " AND " + pred + ")"

	var member bool
	if err := pool.QueryRow(context.Background(), sql, params...).Scan(&member); err != nil {
		t.Fatalf("running the unaccepted fragment: %v\nSQL: %s", err, sql)
	}
	return member
}

// seedAssignedIssue creates an issue assigned to assigneeName, authored
// by author. Returns the id and the current version.
func seedAssignedIssue(t *testing.T, store *Store, pid uuid.UUID, prefix string,
	author uuid.UUID, actor Actor, assigneeName string,
) (string, int) {
	t.Helper()
	assignee := assigneeName
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title: "assigned", Type: "task", Priority: 2,
			Assignee: &assignee,
		}, author, actor)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	return dto.ID, dto.Version
}

// TestUnacceptedPredicate_StatusTable — six rows of the resolution table,
// each as its own subtest.
func TestUnacceptedPredicate_StatusTable(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	author := seedUser(t, pool)
	authorName := readUsername(t, pool, author)
	authorActor := HumanActor(author, authorName)
	pid, prefix := seedProject(t, pool, author)

	worker := seedUser(t, pool)
	workerName := readUsername(t, pool, worker)
	workerActor := HumanActor(worker, workerName)
	addProjectMember(t, pool, pid, worker, author)

	// Precondition of the whole table: a just-assigned unaccepted issue
	// is IN THE SET. Without it every "no" below would be proven by the
	// fragment finding nothing at all.
	baseID, _ := seedAssignedIssue(t, store, pid, prefix, author, authorActor, workerName)
	if !unacceptedMember(t, pool, baseID, worker) {
		t.Fatalf("precondition: a just-assigned unaccepted issue is outside the set — " +
			"further negative claims would be meaningless")
	}

	cases := []struct {
		name string
		// prepare puts the issue into the state under test.
		prepare func(t *testing.T, id string, version int)
		want    bool
		// why is the table-row wording that lands in the failure text.
		why string
	}{
		{
			name:    "open, no acceptance",
			prepare: func(*testing.T, string, int) {},
			want:    true,
			why:     "an open unaccepted issue hangs on the person",
		},
		{
			name: "deferred, no acceptance",
			prepare: func(t *testing.T, id string, version int) {
				if _, err := store.UpdateStatus(t.Context(), id,
					UpdateStatusInput{Status: StatusDeferred, ExpectedVersion: version},
					authorActor); err != nil {
					t.Fatalf("UpdateStatus(deferred): %v", err)
				}
			},
			want: true,
			why:  "a deferred issue stays hanging on the person — the spec requires \"yes\"",
		},
		{
			name: "in progress, no acceptance",
			prepare: func(t *testing.T, id string, version int) {
				// Status is set EXPLICITLY, without a claim: the spec
				// says this state is reachable besides a claim, and
				// that is why "in progress" by itself does not take
				// the issue out of the set — the acceptance mark
				// does.
				if _, err := store.UpdateStatus(t.Context(), id,
					UpdateStatusInput{Status: StatusInProgress, ExpectedVersion: version},
					authorActor); err != nil {
					t.Fatalf("UpdateStatus(in_progress): %v", err)
				}
			},
			want: true,
			why:  "\"in progress\" without acceptance hangs on the person the same as open",
		},
		{
			name: "accepted by any path",
			prepare: func(t *testing.T, id string, version int) {
				v := version
				if _, err := store.Accept(t.Context(), AcceptInput{
					ID: id, ExpectedVersion: &v, Actor: workerActor,
				}); err != nil {
					t.Fatalf("Accept: %v", err)
				}
			},
			want: false,
			why:  "the appearance of the acceptance mark silences nudges (SC-010)",
		},
		{
			name: "closed",
			prepare: func(t *testing.T, id string, version int) {
				if _, err := store.Close(t.Context(), CloseInput{
					ID: id, ExpectedVersion: version, Reason: "done", Actor: authorActor,
				}); err != nil {
					t.Fatalf("Close: %v", err)
				}
			},
			want: false,
			why:  "a closed issue is not waiting for acceptance",
		},
		{
			name: "standing",
			prepare: func(t *testing.T, id string, version int) {
				if _, err := store.UpdateStatus(t.Context(), id,
					UpdateStatusInput{Status: StatusStanding, ExpectedVersion: version},
					authorActor); err != nil {
					t.Fatalf("UpdateStatus(standing): %v", err)
				}
			},
			want: false,
			why: "a standing task never completes: a perpetual nudge on it is " +
				"pure noise (SC-024). Formally it is NOT closed, so a " +
				"\"status <> closed\" predicate would let it through",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, version := seedAssignedIssue(t, store, pid, prefix, author, authorActor, workerName)
			tc.prepare(t, id, version)

			got := unacceptedMember(t, pool, id, worker)
			if got != tc.want {
				t.Errorf("table row %q: membership %v, want %v — %s",
					tc.name, got, tc.want, tc.why)
			}
		})
	}
}

// TestUnacceptedPredicate_AddresseeResolution — two exceptions that come
// from resolving the assignee to a live user.
//
// Both are about there being NOBODY to nudge. Fail-open here would mean
// a perpetual tick of the background worker over rows that will never be
// delivered.
func TestUnacceptedPredicate_AddresseeResolution(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	author := seedUser(t, pool)
	authorActor := HumanActor(author, readUsername(t, pool, author))
	pid, prefix := seedProject(t, pool, author)

	t.Run("deactivated addressee", func(t *testing.T) {
		worker := seedUser(t, pool)
		workerName := readUsername(t, pool, worker)
		addProjectMember(t, pool, pid, worker, author)
		id, _ := seedAssignedIssue(t, store, pid, prefix, author, authorActor, workerName)

		// Precondition: before deactivation the issue is IN the set.
		// Without it, "no" below would be proven by it never having
		// been in.
		if !unacceptedMember(t, pool, id, worker) {
			t.Fatalf("precondition: an active addressee does not see their unaccepted issue")
		}

		if _, err := pool.Exec(context.Background(),
			`UPDATE users SET deactivated_at = now() WHERE id = $1`, worker); err != nil {
			t.Fatalf("deactivation: %v", err)
		}

		if unacceptedMember(t, pool, id, worker) {
			t.Errorf("deactivated addressee stayed in the set: a nudge will " +
				"form and never be delivered")
		}
	})

	t.Run("unresolvable assignee", func(t *testing.T) {
		worker := seedUser(t, pool)
		workerName := readUsername(t, pool, worker)
		addProjectMember(t, pool, pid, worker, author)
		id, _ := seedAssignedIssue(t, store, pid, prefix, author, authorActor, workerName)

		if !unacceptedMember(t, pool, id, worker) {
			t.Fatalf("precondition: a resolvable assignee did not land in the set")
		}

		// The assignee name stops resolving to a user: a typo, a
		// rename, a deleted account. Written straight into the column
		// — the product does not create this state, but the data
		// allows it, and the background worker must survive it.
		if _, err := pool.Exec(context.Background(),
			`UPDATE issues SET assignee = $2 WHERE id = $1`, id, workerName+"_gone"); err != nil {
			t.Fatalf("corrupting the assignee name: %v", err)
		}

		if unacceptedMember(t, pool, id, worker) {
			t.Errorf("an unresolvable assignee name left the issue in the addressee's set")
		}
	})
}
