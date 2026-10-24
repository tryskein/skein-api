package issues

// close_outcome_writers_test.go — sk-489, T003.
//
// A smoke for the gate "all close paths share one semantics": outcome, target and
// reopen on BOTH close writers (Store.Close and
// Store.UpdateStatus). The full tables of matrix rows 8–13 are delivered by the task's test
// delivery; here is the minimum proving that the outcome helper
// is wired to both writers, not to one.
//
// They must not be split by writer: each has its own UPDATE, its own event block
// and its own fan-out call, so half of the claims checked on
// one say nothing about the other.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
)

// closeWriter — one of the two close writers under a common call.
// Archive exists only on UpdateStatus: archiving goes through it
// (handler/bulk.go#runOne), CloseInput has no such field at all.
type closeWriter struct {
	name           string
	supportsArchve bool
	closeIssue     func(t *testing.T, store *Store, id string, in closeAttempt) error
	// reportsLink — the writer returns the link flag (CloseLinkState).
	// Only Store.CloseWithResult returns it: the flag is needed by
	// MCP confirmations, and MCP closes through it. UpdateStatus has
	// no such leg — the REST transition response is the issue card anyway — and
	// this is a recorded "no leg", not an omission.
	reportsLink bool
	closeLink   func(t *testing.T, store *Store, id string, in closeAttempt) (CloseLinkState, error)
}

type closeAttempt struct {
	Version int
	Reason  string
	Outcome *string
	Target  string
	Caller  *DependenciesCaller
	Actor   Actor
	Archive bool
}

// closeReasonOf — the default close reason. A separate
// function, because on Close an empty reason writes NULL, and the case "date
// and reason behave as today" would otherwise compare NULL with NULL.
func closeReasonOf(in closeAttempt) string {
	if in.Reason != "" {
		return in.Reason
	}
	return "smoke"
}

func closeWriters() []closeWriter {
	return []closeWriter{
		{
			name:        "Store.Close",
			reportsLink: true,
			closeLink: func(t *testing.T, store *Store, id string, in closeAttempt) (CloseLinkState, error) {
				t.Helper()
				_, state, err := store.CloseWithResult(t.Context(), CloseInput{
					ID:                 id,
					ExpectedVersion:    in.Version,
					Reason:             closeReasonOf(in),
					Actor:              in.Actor,
					CloseOutcome:       in.Outcome,
					CloseTarget:        in.Target,
					DependenciesCaller: in.Caller,
				})
				return state, err
			},
			closeIssue: func(t *testing.T, store *Store, id string, in closeAttempt) error {
				t.Helper()
				_, err := store.Close(t.Context(), CloseInput{
					ID:                 id,
					ExpectedVersion:    in.Version,
					Reason:             closeReasonOf(in),
					Actor:              in.Actor,
					CloseOutcome:       in.Outcome,
					CloseTarget:        in.Target,
					DependenciesCaller: in.Caller,
				})
				return err
			},
		},
		{
			name:           "Store.UpdateStatus",
			supportsArchve: true,
			reportsLink:    false,
			closeIssue: func(t *testing.T, store *Store, id string, in closeAttempt) error {
				t.Helper()
				reason := closeReasonOf(in)
				_, err := store.UpdateStatus(t.Context(), id, UpdateStatusInput{
					ExpectedVersion:    in.Version,
					Status:             StatusClosed,
					Reason:             &reason,
					CloseOutcome:       in.Outcome,
					CloseTarget:        in.Target,
					DependenciesCaller: in.Caller,
					Archive:            in.Archive,
				}, in.Actor)
				return err
			},
		},
	}
}

// outcomeEnv — the author, their project and an issue builder in it.
type outcomeEnv struct {
	pool   *pgxpool.Pool
	store  *Store
	author uuid.UUID
	actor  Actor
	caller DependenciesCaller
	pid    uuid.UUID
	prefix string
}

func newOutcomeEnv(t *testing.T) *outcomeEnv {
	t.Helper()
	pool := newTestPool(t)
	author := seedUser(t, pool)
	name := readUsername(t, pool, author)
	pid, prefix := seedProject(t, pool, author)
	return &outcomeEnv{
		pool:   pool,
		store:  NewStore(pool),
		author: author,
		actor:  HumanActor(author, name),
		// The user's system role, not Actor.Role: the visibility key
		// of the target is the same as for a manual link.
		caller: DependenciesCaller{UserID: author, Role: "user"},
		pid:    pid,
		prefix: prefix,
	}
}

func (e *outcomeEnv) issue(t *testing.T, title string) *IssueDetail {
	t.Helper()
	dto, err := e.store.CreateIssue(t.Context(), e.pid, e.prefix, "counter",
		CreateIssueInput{Title: title, Type: "task", Priority: 2},
		e.author, e.actor)
	if err != nil {
		t.Fatalf("CreateIssue(%s): %v", title, err)
	}
	return dto
}

// readOutcome reads the column directly: the smoke must see what lies in
// the database, not what the serializer decided to return.
func readOutcome(t *testing.T, pool *pgxpool.Pool, id string) *string {
	t.Helper()
	var out *string
	if err := pool.QueryRow(context.Background(),
		`SELECT close_outcome FROM issues WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatalf("read close_outcome %s: %v", id, err)
	}
	return out
}

func countEdges(t *testing.T, pool *pgxpool.Pool, fromID, toID, depType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND to_id = $2 AND type = $3`, fromID, toID, depType).Scan(&n); err != nil {
		t.Fatalf("count %s edges: %v", depType, err)
	}
	return n
}

func wantOutcome(t *testing.T, pool *pgxpool.Pool, id, want string) {
	t.Helper()
	got := readOutcome(t, pool, id)
	if got == nil {
		t.Fatalf("%s: the outcome is missing, expected %q", id, want)
	}
	if *got != want {
		t.Fatalf("%s: outcome %q, expected %q", id, *got, want)
	}
}

// TestCloseOutcome_Smoke_BothWriters — the T003 gate smoke.
func TestCloseOutcome_Smoke_BothWriters(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			env := newOutcomeEnv(t)

			t.Run("transition without an outcome -> done", func(t *testing.T) {
				iss := env.issue(t, "no outcome")
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
				}); err != nil {
					t.Fatalf("close: %v", err)
				}
				wantOutcome(t, env.pool, iss.ID, CloseOutcomeDone)

				card, err := env.store.GetIssue(t.Context(), iss.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if card.CloseOutcome == nil || *card.CloseOutcome != CloseOutcomeDone {
					t.Fatalf("the closed issue's card does not carry the outcome: %v", card.CloseOutcome)
				}
			})

			t.Run("duplicate with an open target -> link created, repeat -> existed", func(t *testing.T) {
				target := env.issue(t, "target, open")
				iss := env.issue(t, "duplicate")
				outcome := CloseOutcomeDuplicate
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &outcome, Target: target.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("close with a target: %v", err)
				}
				wantOutcome(t, env.pool, iss.ID, CloseOutcomeDuplicate)
				if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("duplicates edges %s->%s: %d, expected 1", iss.ID, target.ID, n)
				}

				// Closing again with the same target: a second link is not
				// created, the close goes through (US-2).
				reread, err := env.store.GetIssue(t.Context(), iss.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: reread.Version, Actor: env.actor,
					Outcome: &outcome, Target: target.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("repeated close with a target: %v", err)
				}
				if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
					t.Fatalf("after the repeat edges %d, expected 1", n)
				}
			})

			t.Run("invisible target -> rejection, the issue is not closed", func(t *testing.T) {
				stranger := seedUser(t, env.pool)
				otherPID, otherPrefix := seedProject(t, env.pool, stranger)
				strangerActor := HumanActor(stranger, readUsername(t, env.pool, stranger))
				hidden, err := env.store.CreateIssue(t.Context(), otherPID, otherPrefix, "counter",
					CreateIssueInput{Title: "target in a foreign project", Type: "task", Priority: 2},
					stranger, strangerActor)
				if err != nil {
					t.Fatalf("CreateIssue in a foreign project: %v", err)
				}

				iss := env.issue(t, "duplicate of an invisible target")
				outcome := CloseOutcomeDuplicate
				err = w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &outcome, Target: hidden.ID, Caller: &env.caller,
				})
				if !errors.Is(err, dependencies.ErrDependencyTargetNotVisible) {
					t.Fatalf("expected ErrDependencyTargetNotVisible, got %v", err)
				}
				if got := readOutcome(t, env.pool, iss.ID); got != nil {
					t.Fatalf("after the rejection the outcome is %q — the issue must not be closed", *got)
				}
				if n := countEdges(t, env.pool, iss.ID, hidden.ID, dependencies.DepDuplicates); n != 0 {
					t.Fatalf("after the rejection edges %d, expected 0", n)
				}
			})

			t.Run("reopening erases the outcome", func(t *testing.T) {
				iss := env.issue(t, "to be reopened")
				outcome := CloseOutcomeWontDo
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor, Outcome: &outcome,
				}); err != nil {
					t.Fatalf("close: %v", err)
				}
				wantOutcome(t, env.pool, iss.ID, CloseOutcomeWontDo)

				closed, err := env.store.GetIssue(t.Context(), iss.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if _, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
					ExpectedVersion: closed.Version, Status: StatusOpen,
				}, env.actor); err != nil {
					t.Fatalf("UpdateStatus(open): %v", err)
				}
				if got := readOutcome(t, env.pool, iss.ID); got != nil {
					t.Fatalf("after reopening the outcome is %q, expected none", *got)
				}
				reopened, err := env.store.GetIssue(t.Context(), iss.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if reopened.CloseOutcome != nil {
					t.Fatalf("the reopened card carries the outcome %q", *reopened.CloseOutcome)
				}
			})

			t.Run("repeated close without an outcome does not change the outcome", func(t *testing.T) {
				iss := env.issue(t, "repeat without an outcome")
				outcome := CloseOutcomeDuplicate
				targetIssue := env.issue(t, "repeat target")
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
					Outcome: &outcome, Target: targetIssue.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("close: %v", err)
				}
				reread, err := env.store.GetIssue(t.Context(), iss.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: reread.Version, Actor: env.actor,
				}); err != nil {
					t.Fatalf("repeated close without an outcome: %v", err)
				}
				wantOutcome(t, env.pool, iss.ID, CloseOutcomeDuplicate)
			})

			if !w.supportsArchve {
				return
			}
			t.Run("archive without outcome: unclosed refused, standing → done", func(t *testing.T) {
				// sk-540.4: no silent wont_do any more.
				iss := env.issue(t, "to be archived")
				err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor, Archive: true,
				})
				if ve, ok := IsValidationError(err); !ok || ve.Code != "close_outcome_required" {
					t.Fatalf("archive of an unclosed issue without outcome: err = %v, want close_outcome_required", err)
				}
				if got := readCloseStamps(t, env.pool, iss.ID); got.Status != StatusOpen || got.Version != iss.Version {
					t.Fatalf("refused archive changed the row: %+v", got)
				}

				standing := env.issue(t, "standing")
				if _, err := env.store.UpdateStatus(t.Context(), standing.ID, UpdateStatusInput{
					ExpectedVersion: standing.Version, Status: StatusStanding,
				}, env.actor); err != nil {
					t.Fatalf("promotion to standing: %v", err)
				}
				promoted, err := env.store.GetIssue(t.Context(), standing.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if err := w.closeIssue(t, env.store, standing.ID, closeAttempt{
					Version: promoted.Version, Actor: env.actor, Archive: true,
				}); err != nil {
					t.Fatalf("archiving a standing issue: %v", err)
				}
				wantOutcome(t, env.pool, standing.ID, CloseOutcomeDone)
			})
		})
	}
}

// readCloseStamps reads the triple "status, date, reason" — the part of the row
// that closing touches besides the outcome.
type closeStamps struct {
	Status   string
	Outcome  *string
	ClosedAt *time.Time
	Reason   *string
	Version  int
}

func readCloseStamps(t *testing.T, pool *pgxpool.Pool, id string) closeStamps {
	t.Helper()
	var s closeStamps
	if err := pool.QueryRow(context.Background(), `
SELECT status, close_outcome, closed_at, closed_reason, version
  FROM issues WHERE id = $1`, id).
		Scan(&s.Status, &s.Outcome, &s.ClosedAt, &s.Reason, &s.Version); err != nil {
		t.Fatalf("read close stamps %s: %v", id, err)
	}
	return s
}

// TestCloseOutcome_Writers_OutcomeTable — matrix row 8: one semantics
// of the OUTCOME as a table over both close writers.
//
// They must not be split. Store.Close and Store.UpdateStatus have THEIR OWN UPDATEs and their own
// event blocks; a table run over one is green under any
// divergence of the other — and the other is the human path (REST transition and
// REST batch, handler/bulk.go#runOne).
//
// The mutation the table was written for: allow the outcome from the input before
// the issue status check (that is, remove the AlreadyClosed branch from
// DecideCloseOutcome). Then "repeated close without an outcome" turns red —
// the outcome becomes `done` — while everything else stays green.
func TestCloseOutcome_Writers_OutcomeTable(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			env := newOutcomeEnv(t)

			t.Run("an explicit outcome is accepted on a transition — all four values", func(t *testing.T) {
				for _, outcome := range ValidCloseOutcomes() {
					outcome := outcome
					t.Run(outcome, func(t *testing.T) {
						iss := env.issue(t, "transition with "+outcome)
						// sk-540.2: duplicate / superseded on a transition
						// require a target; the other two take none.
						target, caller := env.targetFor(t, outcome)
						if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
							Version: iss.Version, Actor: env.actor, Outcome: &outcome,
							Target: target, Caller: caller,
						}); err != nil {
							t.Fatalf("close with outcome %s: %v", outcome, err)
						}
						wantOutcome(t, env.pool, iss.ID, outcome)
					})
				}
			})

			t.Run("a value outside the four -> rejection, nothing changed", func(t *testing.T) {
				// "done " is deliberately NOT in the list: ValidateCloseOutcome
				// trims spaces at the edges, so such an input is
				// the canonical `done`, not a rejection. Case, however, is not
				// normalised: "Done" and "DONE" are a rejection, and this is a recorded
				// T001 decision, not an oversight.
				for _, bad := range []string{"bogus", "Done", "DONE", " ", "", "wontdo", "done,wont_do"} {
					bad := bad
					t.Run("«"+bad+"»", func(t *testing.T) {
						iss := env.issue(t, "bad outcome")
						before := readCloseStamps(t, env.pool, iss.ID)
						err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
							Version: iss.Version, Actor: env.actor, Outcome: &bad,
						})
						var verr *ValidationError
						if !errors.As(err, &verr) || verr.Code != "invalid_close_outcome" {
							t.Fatalf("expected invalid_close_outcome, got %v", err)
						}
						after := readCloseStamps(t, env.pool, iss.ID)
						if after.Status != before.Status || after.Version != before.Version ||
							after.Outcome != nil || after.ClosedAt != nil {
							t.Fatalf("the row changed after the rejection: %+v -> %+v", before, after)
						}
					})
				}
				// The reverse half of the list: a value with spaces at the edges
				// is ACCEPTED and canonicalised. Without it "reject everything"
				// would pass the whole table above.
				t.Run("edge spaces are trimmed", func(t *testing.T) {
					iss := env.issue(t, "outcome with spaces")
					padded := "  " + CloseOutcomeWontDo + " "
					if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor, Outcome: &padded,
					}); err != nil {
						t.Fatalf("close with \"%s\": %v", padded, err)
					}
					wantOutcome(t, env.pool, iss.ID, CloseOutcomeWontDo)
				})
			})

			t.Run("repeated close with a different outcome -> new outcome, date and reason as today", func(t *testing.T) {
				iss := env.issue(t, "repeat with a change")
				first := CloseOutcomeDone
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor, Outcome: &first, Reason: "first reason",
				}); err != nil {
					t.Fatalf("first close: %v", err)
				}
				before := readCloseStamps(t, env.pool, iss.ID)

				second := CloseOutcomeWontDo
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: before.Version, Actor: env.actor, Outcome: &second, Reason: "second reason",
				}); err != nil {
					t.Fatalf("repeated close: %v", err)
				}
				after := readCloseStamps(t, env.pool, iss.ID)
				if after.Outcome == nil || *after.Outcome != CloseOutcomeWontDo {
					t.Fatalf("outcome after the repeat: %v, expected wont_do", after.Outcome)
				}
				// "As today" for a repeated close is an overwrite of the reason
				// and the date; the feature does not change that.
				if after.Reason == nil || *after.Reason != "second reason" {
					t.Fatalf("reason after the repeat: %v, expected an overwrite", after.Reason)
				}
				if after.ClosedAt == nil || before.ClosedAt == nil || after.ClosedAt.Before(*before.ClosedAt) {
					t.Fatalf("the close date went backwards after the repeat: %v -> %v", before.ClosedAt, after.ClosedAt)
				}
			})

			t.Run("reopening into any unclosed status erases the outcome, leaves the mark alone", func(t *testing.T) {
				for _, status := range []string{StatusOpen, StatusInProgress, StatusDeferred, StatusStanding} {
					status := status
					t.Run(status, func(t *testing.T) {
						iss := closedDoneIssue(t, env)
						outcome := CloseOutcomeDuplicate
						stamps := readCloseStamps(t, env.pool, iss.ID)
						target, caller := env.targetFor(t, outcome)
						if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
							Version: stamps.Version, Actor: env.actor, Outcome: &outcome,
							Target: target, Caller: caller,
						}); err != nil {
							t.Fatalf("close with %s: %v", outcome, err)
						}
						markBefore := readReviewMark(t, env.pool, iss.ID)

						if _, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
							ExpectedVersion: markBefore.Version, Status: status,
						}, env.actor); err != nil {
							t.Fatalf("reopen into %s: %v", status, err)
						}
						after := readCloseStamps(t, env.pool, iss.ID)
						if after.Outcome != nil {
							t.Fatalf("after the transition to %s the outcome is %q, expected none", status, *after.Outcome)
						}
						if after.ClosedAt != nil || after.Reason != nil {
							t.Fatalf("after the transition to %s the date/reason are not erased: %v / %v",
								status, after.ClosedAt, after.Reason)
						}
						markAfter := readReviewMark(t, env.pool, iss.ID)
						if (markAfter.ReviewedAt == nil) != (markBefore.ReviewedAt == nil) {
							t.Fatalf("reopening touched the review mark: was %v, now %v",
								markBefore.ReviewedAt, markAfter.ReviewedAt)
						}
					})
				}
			})

			t.Run("the card returns closeOutcome only for a closed issue", func(t *testing.T) {
				iss := env.issue(t, "open card")
				open, err := env.store.GetIssue(t.Context(), iss.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if open.CloseOutcome != nil {
					t.Fatalf("an unclosed card has the outcome %q", *open.CloseOutcome)
				}
				outcome := CloseOutcomeSuperseded
				target, caller := env.targetFor(t, outcome)
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: open.Version, Actor: env.actor, Outcome: &outcome,
					Target: target, Caller: caller,
				}); err != nil {
					t.Fatalf("close: %v", err)
				}
				closed, err := env.store.GetIssue(t.Context(), iss.ID)
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				if closed.CloseOutcome == nil || *closed.CloseOutcome != CloseOutcomeSuperseded {
					t.Fatalf("closed card: outcome %v, expected superseded", closed.CloseOutcome)
				}
			})

			// --- legs that Store.Close does not have at all ---
			//
			// An outcome with a status other than "closed", and the archive
			// default are inputs that ONLY UpdateStatus can express:
			// CloseInput has neither a status nor an archive flag.
			// The table cell for Close is closed by this note, not
			// left empty.
			if !w.supportsArchve {
				return
			}

			t.Run("outcome with a status != closed -> rejection, nothing changed", func(t *testing.T) {
				for _, status := range []string{StatusOpen, StatusInProgress, StatusDeferred, StatusStanding} {
					status := status
					t.Run(status, func(t *testing.T) {
						iss := env.issue(t, "outcome not on a close")
						outcome := CloseOutcomeWontDo
						_, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
							ExpectedVersion: iss.Version, Status: status, CloseOutcome: &outcome,
						}, env.actor)
						var verr *ValidationError
						if !errors.As(err, &verr) || verr.Code != "close_outcome_not_a_close" {
							t.Fatalf("expected close_outcome_not_a_close, got %v", err)
						}
						after := readCloseStamps(t, env.pool, iss.ID)
						if after.Version != iss.Version || after.Outcome != nil {
							t.Fatalf("the row changed after the rejection: %+v", after)
						}
					})
				}
			})

			// The second half of the same pre-check. The rejection also fires on a
			// TARGET without an outcome, and checking it separately is mandatory:
			// a mutant with the CloseTarget part removed from the condition
			// leaves the cell above green, while a transition with a target goes through
			// SUCCESSFULLY and silently loses the target — linking lives inside the branch
			// "transition to closed". The caller is passed deliberately: without it the
			// mutant would run into "DependenciesCaller is required" and
			// turn red for the wrong defect.
			t.Run("target with a status != closed -> rejection, nothing changed and no edge", func(t *testing.T) {
				for _, status := range []string{StatusOpen, StatusInProgress, StatusDeferred, StatusStanding} {
					status := status
					t.Run(status, func(t *testing.T) {
						target := env.issue(t, "target not on a close")
						iss := env.issue(t, "transition with a target")
						_, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
							ExpectedVersion: iss.Version, Status: status,
							CloseTarget: target.ID, DependenciesCaller: &env.caller,
						}, env.actor)
						var verr *ValidationError
						if !errors.As(err, &verr) || verr.Code != "close_outcome_not_a_close" {
							t.Fatalf("expected close_outcome_not_a_close, got %v", err)
						}
						after := readCloseStamps(t, env.pool, iss.ID)
						if after.Version != iss.Version || after.Status != StatusOpen || after.Outcome != nil {
							t.Fatalf("the row changed after the rejection: %+v", after)
						}
						if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 0 {
							t.Fatalf("duplicates edges %d, expected zero", n)
						}
						if n := countEdges(t, env.pool, target.ID, iss.ID, dependencies.DepSupersedes); n != 0 {
							t.Fatalf("supersedes edges %d, expected zero", n)
						}
					})
				}
			})

			t.Run("archiving", func(t *testing.T) {
				// sk-540.4: the unclosed non-standing row needs an outcome.
				// The refusal is decided on the row read INSIDE the
				// transaction, which is also what refuses a row reopened
				// after the REST batch preflight
				// (TestArchive_RowReopenedAfterPreflightIsRefused).
				t.Run("unclosed non-standing without an outcome -> close_outcome_required", func(t *testing.T) {
					iss := env.issue(t, "archive: ordinary")
					err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor, Archive: true,
					})
					if ve, ok := IsValidationError(err); !ok || ve.Code != "close_outcome_required" {
						t.Fatalf("err = %v, want close_outcome_required", err)
					}
					if got := readCloseStamps(t, env.pool, iss.ID); got.Status != StatusOpen ||
						got.Outcome != nil || got.Version != iss.Version {
						t.Fatalf("refused archive changed the row: %+v", got)
					}
				})

				t.Run("an explicit outcome and target do not apply to an already closed or standing issue", func(t *testing.T) {
					target := env.issue(t, "archive: target")
					dup := CloseOutcomeDuplicate

					closed := env.issue(t, "archive: closed with done")
					done := CloseOutcomeDone
					if err := w.closeIssue(t, env.store, closed.ID, closeAttempt{
						Version: closed.Version, Actor: env.actor, Outcome: &done,
					}); err != nil {
						t.Fatalf("close: %v", err)
					}
					closedStamps := readCloseStamps(t, env.pool, closed.ID)
					if err := w.closeIssue(t, env.store, closed.ID, closeAttempt{
						Version: closedStamps.Version, Actor: env.actor, Archive: true,
						Outcome: &dup, Target: target.ID, Caller: &env.caller,
					}); err != nil {
						t.Fatalf("archiving a closed issue with an outcome and a target: %v", err)
					}
					wantOutcome(t, env.pool, closed.ID, CloseOutcomeDone)
					if n := countEdges(t, env.pool, closed.ID, target.ID, dependencies.DepDuplicates); n != 0 {
						t.Fatalf("archive of a closed row created %d duplicates edges, want 0", n)
					}

					standing := env.issue(t, "archive: standing with an outcome")
					if _, err := env.store.UpdateStatus(t.Context(), standing.ID, UpdateStatusInput{
						ExpectedVersion: standing.Version, Status: StatusStanding,
					}, env.actor); err != nil {
						t.Fatalf("promotion to standing: %v", err)
					}
					promoted := readCloseStamps(t, env.pool, standing.ID)
					if err := w.closeIssue(t, env.store, standing.ID, closeAttempt{
						Version: promoted.Version, Actor: env.actor, Archive: true,
						Outcome: &dup, Target: target.ID, Caller: &env.caller,
					}); err != nil {
						t.Fatalf("archiving a standing issue with an outcome and a target: %v", err)
					}
					wantOutcome(t, env.pool, standing.ID, CloseOutcomeDone)
					if n := countEdges(t, env.pool, standing.ID, target.ID, dependencies.DepDuplicates); n != 0 {
						t.Fatalf("archive of a standing row created %d duplicates edges, want 0", n)
					}
				})

				t.Run("standing -> done", func(t *testing.T) {
					iss := env.issue(t, "archive: standing")
					if _, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
						ExpectedVersion: iss.Version, Status: StatusStanding,
					}, env.actor); err != nil {
						t.Fatalf("promotion to standing: %v", err)
					}
					promoted := readCloseStamps(t, env.pool, iss.ID)
					if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: promoted.Version, Actor: env.actor, Archive: true,
					}); err != nil {
						t.Fatalf("archiving a standing issue: %v", err)
					}
					wantOutcome(t, env.pool, iss.ID, CloseOutcomeDone)
				})

				t.Run("already closed -> outcome unchanged", func(t *testing.T) {
					iss := env.issue(t, "archive: already closed")
					outcome := CloseOutcomeDuplicate
					target, caller := env.targetFor(t, outcome)
					if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor, Outcome: &outcome,
						Target: target, Caller: caller,
					}); err != nil {
						t.Fatalf("close: %v", err)
					}
					closed := readCloseStamps(t, env.pool, iss.ID)
					if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: closed.Version, Actor: env.actor, Archive: true,
					}); err != nil {
						t.Fatalf("archiving a closed issue: %v", err)
					}
					wantOutcome(t, env.pool, iss.ID, CloseOutcomeDuplicate)
				})

				t.Run("an explicit outcome applies to an unclosed issue", func(t *testing.T) {
					iss := env.issue(t, "archive with an explicit outcome")
					outcome := CloseOutcomeDone
					if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor,
						Outcome: &outcome, Archive: true,
					}); err != nil {
						t.Fatalf("archiving with an explicit outcome: %v", err)
					}
					wantOutcome(t, env.pool, iss.ID, CloseOutcomeDone)
				})
			})
		})
	}
}

// TestCloseOutcome_INV7_BlockerReleasesAtAnyOutcome — matrix row 13,
// the "row and card" half.
//
// INV-7: readiness, blocking and the "blocked" flag do NOT depend on
// the outcome — a closed blocker releases the block under ANY outcome. The
// "readiness" half (dependencies.Store.ListReady) is in
// dependencies/ready_close_outcome_test.go: the predicate there is its own, and
// a claim about one says nothing about the other.
//
// The claim is not a tautology: the blocking predicate reads the target's STATUS
// (status.GatesReadiness), and exactly this is pinned here — an implementation
// that taught it to distinguish outcomes (for example "closed as duplicate is not
// considered closed") turns red on three cells out of four.
func TestCloseOutcome_INV7_BlockerReleasesAtAnyOutcome(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			for _, outcome := range ValidCloseOutcomes() {
				outcome := outcome
				t.Run(outcome, func(t *testing.T) {
					env := newOutcomeEnv(t)
					blocked := env.issue(t, "blocked")
					blocker := env.issue(t, "blocker")
					seedDep(t, env.pool, blocked.ID, blocker.ID, "blocks", env.author)

					ctx := callerCtx(t, env.author, "user")
					before, err := env.store.GetIssue(ctx, blocked.ID)
					if err != nil {
						t.Fatalf("GetIssue: %v", err)
					}
					if !before.IsBlocked {
						t.Fatalf("precondition: the issue is not blocked by an open blocker")
					}

					target, caller := env.targetFor(t, outcome)
					if err := w.closeIssue(t, env.store, blocker.ID, closeAttempt{
						Version: blocker.Version, Actor: env.actor, Outcome: &outcome,
						Target: target, Caller: caller,
					}); err != nil {
						t.Fatalf("closing the blocker with %s: %v", outcome, err)
					}

					card, err := env.store.GetIssue(ctx, blocked.ID)
					if err != nil {
						t.Fatalf("GetIssue: %v", err)
					}
					if card.IsBlocked {
						t.Fatalf("card: isBlocked=true with a closed blocker with outcome %s", outcome)
					}
					if len(card.Blockers) != 0 {
						t.Fatalf("card: blockers %+v with a closed blocker", card.Blockers)
					}

					res, err := env.store.ListIssues(ctx, ListIssuesArgs{ProjectID: env.pid, Limit: 50})
					if err != nil {
						t.Fatalf("ListIssues: %v", err)
					}
					var seen bool
					for _, it := range res.Items {
						if it.ID != blocked.ID {
							continue
						}
						seen = true
						if it.IsBlocked {
							t.Fatalf("row: isBlocked=true with a closed blocker with outcome %s", outcome)
						}
					}
					if !seen {
						t.Fatalf("row %s not found in the list", blocked.ID)
					}
				})
			}
		})
	}
}
