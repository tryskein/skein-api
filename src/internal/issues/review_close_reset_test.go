package issues

// review_close_reset_test.go — sk-303.14 T009, matrix rows 42, 43, 44.
//
// Lifecycle of the review mark on close paths.
//
// The requirement distinguishes two events that are easy to glue into one:
// the TRANSITION into closed, and a CLOSING CALL. The first resets the
// mark, not the second — otherwise an author who marked the result would
// see it as unreviewed again because someone else rewrote the close
// reason.
//
// The mutation this file is written against: replace the reset predicate
// `old.Status != 'closed'` with `newStatus == 'closed'` (i.e. reset on
// every closing call). Then "re-close of an already-closed issue" goes
// red, and "reopened and closed" stays green. The inverse mutation — do
// not reset at all — reddens the second test and leaves the first green.
// The tests split those two breakages; they do not catch "something
// broke".
//
// On the wording "returns to the set". The unreviewed set (its SQL
// fragment and its readers) is introduced by T012; it is not here yet.
// So the assertions below talk about the STATE on which the set is
// defined — a closed result with the flag on and an empty mark — not
// about membership. The full membership gate is matrix rows 49–51 of
// T012.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type reviewMarkState struct {
	Status     string
	Required   bool
	ReviewedAt *time.Time
	ReviewedBy *uuid.UUID
	Version    int
}

func readReviewMark(t *testing.T, pool *pgxpool.Pool, issueID string) reviewMarkState {
	t.Helper()
	var s reviewMarkState
	if err := pool.QueryRow(context.Background(), `
SELECT status, review_required, reviewed_at, reviewed_by, version
  FROM issues WHERE id = $1`, issueID).
		Scan(&s.Status, &s.Required, &s.ReviewedAt, &s.ReviewedBy, &s.Version); err != nil {
		t.Fatalf("read review mark %s: %v", issueID, err)
	}
	return s
}

// closedAndMarked builds the shared starting point: the author's issue is
// closed, the flag is on, the result is marked reviewed.
//
// The mark is set by the real operation (Store.MarkReviewed), not by an
// UPDATE: forging the state with a direct write would prove the
// assertions below about a made-up state, not about the one the product
// can create.
func closedAndMarked(t *testing.T, pool *pgxpool.Pool, store *Store) (issueID string, author uuid.UUID, actor Actor) {
	t.Helper()
	author = seedUser(t, pool)
	name := readUsername(t, pool, author)
	pid, prefix := seedProject(t, pool, author)
	actor = HumanActor(author, name)

	required := true
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title: "result", Type: "task", Priority: 2,
			ReviewRequired: &required,
		},
		author, actor)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	issueID = dto.ID

	if _, err := store.Close(t.Context(), CloseInput{
		ID: issueID, ExpectedVersion: dto.Version, Reason: "done", Actor: actor,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	row := readReviewMark(t, pool, issueID)
	if _, err := store.MarkReviewed(t.Context(), ReviewMarkInput{
		ID: issueID, ExpectedVersion: row.Version, Actor: actor,
	}); err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}

	// Precondition: the mark IS set. Without it, "the mark was reset"
	// below would be proven by it never having been there.
	marked := readReviewMark(t, pool, issueID)
	if marked.ReviewedAt == nil || marked.ReviewedBy == nil {
		t.Fatalf("precondition: mark was not set (reviewedAt=%v reviewedBy=%v)",
			marked.ReviewedAt, marked.ReviewedBy)
	}
	return issueID, author, actor
}

// reopen reopens the issue — EXPLICITLY, by the same path the product
// uses (skein_update(status=open) → Store.UpdateStatus).
func reopen(t *testing.T, store *Store, issueID string, actor Actor, version int) {
	t.Helper()
	if _, err := store.UpdateStatus(t.Context(), issueID, UpdateStatusInput{
		Status: StatusOpen, ExpectedVersion: version,
	}, actor); err != nil {
		t.Fatalf("UpdateStatus(open): %v", err)
	}
}

// TestClose_AfterExplicitReopen_ResetsReviewMark — row 42 (SC-023) on
// BOTH close paths.
//
// The paths are checked separately: they have different UPDATEs, and a
// claim about one says nothing about the other — while the spec requires
// one semantics for ALL close paths.
func TestClose_AfterExplicitReopen_ResetsReviewMark(t *testing.T) {
	for name, closeAgain := range map[string]func(t *testing.T, store *Store, id string, actor Actor, version int){
		"skein_close": func(t *testing.T, store *Store, id string, actor Actor, version int) {
			if _, err := store.Close(t.Context(), CloseInput{
				ID: id, ExpectedVersion: version, Reason: "done again", Actor: actor,
			}); err != nil {
				t.Fatalf("Close: %v", err)
			}
		},
		"skein_update(status=closed)": func(t *testing.T, store *Store, id string, actor Actor, version int) {
			if _, err := store.UpdateStatus(t.Context(), id, UpdateStatusInput{
				Status: StatusClosed, ExpectedVersion: version,
			}, actor); err != nil {
				t.Fatalf("UpdateStatus(closed): %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := newTestPool(t)
			store := NewStore(pool)
			issueID, _, actor := closedAndMarked(t, pool, store)

			reopen(t, store, issueID, actor, readReviewMark(t, pool, issueID).Version)

			// Row 44, first half: while the issue is reopened and not
			// yet closed, the previous mark is KEPT, and the issue is
			// not in the set — the set counts only closed results.
			reopened := readReviewMark(t, pool, issueID)
			if reopened.ReviewedAt == nil {
				t.Fatalf("reopen erased the mark — the spec requires the previous "+
					"mark to survive until the next close (edge case \"issue "+
					"reopened and not yet closed\"); status %q", reopened.Status)
			}
			if reopened.Status == StatusClosed {
				t.Fatalf("after reopen status %q — the issue did not open, and "+
					"the check below would be measuring the wrong transition", reopened.Status)
			}

			closeAgain(t, store, issueID, actor, reopened.Version)

			after := readReviewMark(t, pool, issueID)
			if after.Status != StatusClosed {
				t.Fatalf("after re-close status %q", after.Status)
			}
			if after.ReviewedAt != nil || after.ReviewedBy != nil {
				t.Errorf("the mark survived the transition into closed after an explicit "+
					"reopen (reviewedAt=%v reviewedBy=%v) — the result was redone, but "+
					"the author will not see it: the issue did not return to the "+
					"unreviewed set (SC-023)", after.ReviewedAt, after.ReviewedBy)
			}
			if !after.Required {
				t.Errorf("the \"review required\" flag was cleared by close — close must " +
					"not touch it, it resets only the mark")
			}
		})
	}
}

// TestClose_RepeatOfAlreadyClosed_KeepsReviewMark — row 43 (edge) on both
// close paths.
//
// A technical re-close is rewriting the close reason and appending
// commits. It must leave the mark alone: otherwise any other user who
// corrected the close reason would put a reviewed result back on the
// author's unreviewed list.
func TestClose_RepeatOfAlreadyClosed_KeepsReviewMark(t *testing.T) {
	for name, closeAgain := range map[string]func(t *testing.T, store *Store, id string, actor Actor, version int){
		"skein_close": func(t *testing.T, store *Store, id string, actor Actor, version int) {
			if _, err := store.Close(t.Context(), CloseInput{
				ID: id, ExpectedVersion: version, Reason: "reason rewritten", Actor: actor,
			}); err != nil {
				t.Fatalf("Close: %v", err)
			}
		},
		"skein_update(status=closed)": func(t *testing.T, store *Store, id string, actor Actor, version int) {
			if _, err := store.UpdateStatus(t.Context(), id, UpdateStatusInput{
				Status: StatusClosed, ExpectedVersion: version,
			}, actor); err != nil {
				t.Fatalf("UpdateStatus(closed): %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := newTestPool(t)
			store := NewStore(pool)
			issueID, _, actor := closedAndMarked(t, pool, store)

			before := readReviewMark(t, pool, issueID)
			if before.Status != StatusClosed {
				t.Fatalf("precondition: issue is not closed (%q) — a re-close would not "+
					"be a repeat", before.Status)
			}

			closeAgain(t, store, issueID, actor, before.Version)

			after := readReviewMark(t, pool, issueID)
			if after.ReviewedAt == nil || after.ReviewedBy == nil {
				t.Fatalf("a technical re-close of an already-closed issue erased the mark "+
					"(reviewedAt=%v reviewedBy=%v) — a reviewed result returned to the "+
					"unreviewed set because of a close-reason edit the author did not "+
					"make (SC-023, edge case)",
					after.ReviewedAt, after.ReviewedBy)
			}
			if !before.ReviewedAt.Equal(*after.ReviewedAt) {
				t.Errorf("mark time changed: was %v, became %v — re-close rewrote the "+
					"mark instead of leaving it alone",
					before.ReviewedAt, after.ReviewedAt)
			}
			if *before.ReviewedBy != *after.ReviewedBy {
				t.Errorf("mark author changed: was %v, became %v",
					before.ReviewedBy, after.ReviewedBy)
			}
		})
	}
}

// TestClose_OutcomeChange_ResetsReviewMarkOnlyWhenLeavingDone — sk-489,
// matrix row 12: the SECOND branch of the reset predicate.
//
// Branch (1) — reset on the transition to closed — lives in the two tests above and
// was not changed by this task. Branch (2) appeared together with the outcome: the mark
// is reset ONLY when the outcome leaves "done" and the change is NOT made by the author.
// Hence four cells per writer, and all four are needed:
//
//   - done → wont_do by a non-author → RESET (the requester marked the result,
//     and the result no longer exists);
//   - the same by the author → no reset (they know about their own change);
//   - wont_do → duplicate by a non-author → no reset (nothing to void);
//   - repeat without an outcome → no reset (the outcome did not change, INV-10).
//
// The mutation the table turns red: the predicate "reset on any outcome
// change" — cells two and three are red; the predicate "reset on any
// repeated close" — three and four are red; no branch (2)
// at all — the first is red.
func TestClose_OutcomeChange_ResetsReviewMarkOnlyWhenLeavingDone(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			pool := newTestPool(t)
			store := NewStore(pool)

			// An outsider: not the issue author. There is no lock on a closed
			// issue, so they can close it again.
			stranger := seedUser(t, pool)
			strangerActor := HumanActor(stranger, readUsername(t, pool, stranger))

			t.Run("done → wont_do by a non-author resets the mark", func(t *testing.T) {
				issueID, _, _ := closedAndMarked(t, pool, store)
				row := readReviewMark(t, pool, issueID)
				outcome := CloseOutcomeWontDo
				if err := w.closeIssue(t, store, issueID, closeAttempt{
					Version: row.Version, Actor: strangerActor, Outcome: &outcome,
				}); err != nil {
					t.Fatalf("repeated close with wont_do: %v", err)
				}
				after := readReviewMark(t, pool, issueID)
				if after.ReviewedAt != nil || after.ReviewedBy != nil {
					t.Fatalf("mark not reset: reviewedAt=%v reviewedBy=%v",
						after.ReviewedAt, after.ReviewedBy)
				}
			})

			t.Run("done → wont_do by the author leaves the mark alone", func(t *testing.T) {
				issueID, _, actor := closedAndMarked(t, pool, store)
				row := readReviewMark(t, pool, issueID)
				outcome := CloseOutcomeWontDo
				if err := w.closeIssue(t, store, issueID, closeAttempt{
					Version: row.Version, Actor: actor, Outcome: &outcome,
				}); err != nil {
					t.Fatalf("repeated close by the author: %v", err)
				}
				after := readReviewMark(t, pool, issueID)
				if after.ReviewedAt == nil || after.ReviewedBy == nil {
					t.Fatalf("the author's mark was reset, though they know about the change")
				}
			})

			t.Run("wont_do → duplicate by a non-author leaves the mark alone", func(t *testing.T) {
				issueID, _, actor := closedAndMarked(t, pool, store)
				// Move the outcome away from "done" WITH THE AUTHOR'S HANDS — this change
				// leaves the mark alone (the cell above), so the starting
				// state of the next cell is created by the writer, not
				// forged by a raw write.
				row := readReviewMark(t, pool, issueID)
				wontDo := CloseOutcomeWontDo
				if err := w.closeIssue(t, store, issueID, closeAttempt{
					Version: row.Version, Actor: actor, Outcome: &wontDo,
				}); err != nil {
					t.Fatalf("change to wont_do by the author: %v", err)
				}
				mid := readReviewMark(t, pool, issueID)
				if mid.ReviewedAt == nil {
					t.Fatalf("precondition: the mark vanished before the change under test")
				}

				dup := CloseOutcomeDuplicate
				// sk-540.2: a closed issue's outcome may change to duplicate
				// without a target only when the `duplicates` edge exists.
				original, _, _ := closedAndMarked(t, pool, store)
				seedDep(t, pool, issueID, original, "duplicates", stranger)
				if err := w.closeIssue(t, store, issueID, closeAttempt{
					Version: mid.Version, Actor: strangerActor, Outcome: &dup,
				}); err != nil {
					t.Fatalf("change wont_do → duplicate: %v", err)
				}
				after := readReviewMark(t, pool, issueID)
				if after.ReviewedAt == nil || after.ReviewedBy == nil {
					t.Fatalf("the mark was reset by a change BETWEEN two departed outcomes — there was nothing to void")
				}
			})

			t.Run("repeat without an outcome leaves the mark alone", func(t *testing.T) {
				issueID, _, _ := closedAndMarked(t, pool, store)
				row := readReviewMark(t, pool, issueID)
				if err := w.closeIssue(t, store, issueID, closeAttempt{
					Version: row.Version, Actor: strangerActor,
				}); err != nil {
					t.Fatalf("repeated close without an outcome: %v", err)
				}
				after := readReviewMark(t, pool, issueID)
				if after.ReviewedAt == nil || after.ReviewedBy == nil {
					t.Fatalf("a repeat without an outcome reset the mark: the outcome did not change (INV-10)")
				}
				wantOutcome(t, pool, issueID, CloseOutcomeDone)
			})
		})
	}
}
