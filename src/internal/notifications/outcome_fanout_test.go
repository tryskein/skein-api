package notifications_test

// outcome_fanout_test.go — sk-489 / T010, coverage matrix row 43
// (SC-009, SC-011 for the API field, R-7).
//
// ⚠ WHY AN EXTERNAL TEST PACKAGE. The fan-out has to be checked THROUGH THE REAL
// WRITERS — issues.Store.Close, issues.Store.UpdateStatus and
// issues.Store.PatchIssue — and internal/issues imports
// internal/notifications, so a file in package notifications would close a cycle.
// package notifications_test creates no cycle and allows calling exactly the
// functions the product uses.
//
// ⚠ WHY THE FEED DTO IS READ, NOT THE notifications TABLE. The table has no
// close_outcome column: the value is materialised from events.payload on
// read (Store.hydrateNotificationDTO). A check against the table would be green
// even with the materialisation unwired — and the frontend reads exactly this DTO, by the same
// path /me/inbox answers by.
//
// ⚠ WHY A TABLE OVER TWO WRITERS, NOT ONE CALL. Store.Close and
// Store.UpdateStatus have THEIR OWN calls of WriteIssueEvent and notifications.FanOut.
// Half a table over one writer is green when the other diverges, and
// a human closes through UpdateStatus: that is the path of the modal, the board and the REST batch
// (handler/bulk.go#runOne, close/archive branch).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// outcomeEnv — a closed issue with the outcome "done", its author, an OUTSIDE
// subscriber and an actor who is not the author.
type outcomeEnv struct {
	pool       *pgxpool.Pool
	issues     *issues.Store
	notifs     *notifications.Store
	issueID    string
	projectID  uuid.UUID
	author     uuid.UUID
	authorAct  issues.Actor
	other      uuid.UUID
	otherAct   issues.Actor
	subscriber uuid.UUID
	version    int
}

// newOutcomeEnv builds the starting point with REAL operations: the issue
// is created by CreateIssue and closed by Close. Faking the state with a direct
// UPDATE would prove claims about an invented state, not about the one
// the product can create.
//
// The subscriber is a THIRD person, neither author nor actor: only on them does it show that
// rule R-7 silences the status row for the author SPECIFICALLY, not for everyone.
func newOutcomeEnv(t *testing.T) *outcomeEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	env := &outcomeEnv{
		pool:   pool,
		issues: issues.NewStore(pool),
		notifs: notifications.NewStore(pool),
	}

	var authorName, otherName string
	env.author, authorName = testsupport.SeedUser(t, pool, "user")
	env.other, otherName = testsupport.SeedUser(t, pool, "user")
	env.subscriber, _ = testsupport.SeedUser(t, pool, "user")
	env.authorAct = issues.HumanActor(env.author, authorName)
	env.otherAct = issues.HumanActor(env.other, otherName)

	var prefix string
	env.projectID, prefix = testsupport.SeedProject(t, pool, env.author, "ofn")
	testsupport.AddProjectMember(t, pool, env.projectID, env.other, "member")
	testsupport.AddProjectMember(t, pool, env.projectID, env.subscriber, "member")

	dto, err := env.issues.CreateIssue(t.Context(), env.projectID, prefix, "counter",
		issues.CreateIssueInput{Title: "result", Type: "task", Priority: 2},
		env.author, env.authorAct)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	env.issueID = dto.ID
	watch(t, pool, env.issueID, env.subscriber)

	// The author stays a subscriber of their own issue after it is closed —
	// the claims about their rows rest on that (composite patch, repeated
	// close). Since sk-622.21 (US-2) auto-unsubscribe on close is on by
	// default, so the precondition is set explicitly, not inherited from the
	// default.
	keepAutoWatch := false
	if _, err := env.notifs.UpdateReasonPrefs(t.Context(), env.author,
		notifications.UpdateReasonPrefsInput{UnwatchOnClose: &keepAutoWatch}); err != nil {
		t.Fatalf("UpdateReasonPrefs(unwatchOnClose=false): %v", err)
	}

	closed, err := env.issues.Close(t.Context(), issues.CloseInput{
		ID: env.issueID, ExpectedVersion: dto.Version, Reason: "done", Actor: env.authorAct,
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.CloseOutcome == nil || *closed.CloseOutcome != issues.CloseOutcomeDone {
		t.Fatalf("precondition: outcome after closing = %v, want %q",
			closed.CloseOutcome, issues.CloseOutcomeDone)
	}
	env.version = closed.Version

	// The rows written by the setup are irrelevant to the claims:
	// what is checked is what the OUTCOME CHANGE produced. Wiping them is the only
	// way to tell "one row" from "one of three".
	clearInbox(t, pool, env.issueID)
	return env
}

// watch subscribes a person to the issue. The insert is direct: AddIssueWatcher
// needs a transaction, and the subscription here is a fixture, not the subject of the check.
func watch(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, ARRAY['manual'])
ON CONFLICT (issue_id, user_id) DO NOTHING`, issueID, userID); err != nil {
		t.Fatalf("watch %s by %s: %v", issueID, userID, err)
	}
}

// clearInbox deletes the notification rows of the issue.
func clearInbox(t *testing.T, pool *pgxpool.Pool, issueID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
DELETE FROM notifications n
 USING events e
 WHERE n.event_id = e.id AND e.entity_type = 'issue' AND e.entity_id = $1`, issueID); err != nil {
		t.Fatalf("clear inbox for %s: %v", issueID, err)
	}
}

// inbox reads the feed rows of one person for one issue — by the same path
// /me/inbox answers by.
func inbox(t *testing.T, env *outcomeEnv, userID uuid.UUID) []notifications.NotificationDTO {
	t.Helper()
	res, err := env.notifs.ListInbox(t.Context(), notifications.ListInboxArgs{
		UserID: userID, Scope: "all", ProjectID: &env.projectID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	out := make([]notifications.NotificationDTO, 0, len(res.Items))
	for _, n := range res.Items {
		if n.Entity.ID == env.issueID {
			out = append(out, n)
		}
	}
	return out
}

// typesOf renders the row types for an error message.
func typesOf(rows []notifications.NotificationDTO) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Type)
	}
	return out
}

// onlyRow requires exactly one row of the given type and returns it.
func onlyRow(t *testing.T, who string, rows []notifications.NotificationDTO, evType string) notifications.NotificationDTO {
	t.Helper()
	var hit []notifications.NotificationDTO
	for _, r := range rows {
		if r.Type == evType {
			hit = append(hit, r)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("%s: rows of type %s — %d, want exactly one (all rows: %v)",
			who, evType, len(hit), typesOf(rows))
	}
	return hit[0]
}

// noRowsOfType requires that no rows of the named type exist.
func noRowsOfType(t *testing.T, who string, rows []notifications.NotificationDTO, evType string) {
	t.Helper()
	for _, r := range rows {
		if r.Type == evType {
			t.Errorf("%s: a row of type %s arrived, which must not be there (all rows: %v)",
				who, evType, typesOf(rows))
		}
	}
}

// reviewedAt reads the review mark.
func reviewedAt(t *testing.T, pool *pgxpool.Pool, issueID string) *string {
	t.Helper()
	var at *string
	if err := pool.QueryRow(context.Background(),
		`SELECT reviewed_at::text FROM issues WHERE id = $1`, issueID).Scan(&at); err != nil {
		t.Fatalf("read reviewed_at: %v", err)
	}
	return at
}

// markReviewed sets the mark with a real operation.
func markReviewed(t *testing.T, env *outcomeEnv) {
	t.Helper()
	dto, err := env.issues.MarkReviewed(t.Context(), issues.ReviewMarkInput{
		ID: env.issueID, ExpectedVersion: env.version, Actor: env.authorAct,
	})
	if err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	env.version = dto.Version
	if reviewedAt(t, env.pool, env.issueID) == nil {
		t.Fatal("precondition: the mark is not set")
	}
}

// patchOutcome edits the outcome of a closed issue (the US-12 / skein_update path).
func patchOutcome(t *testing.T, env *outcomeEnv, actor issues.Actor, outcome string) {
	t.Helper()
	dto, err := env.issues.PatchIssue(t.Context(), env.issueID, issues.PatchIssueInput{
		ExpectedVersion: env.version, CloseOutcome: &outcome,
	}, actor)
	if err != nil {
		t.Fatalf("PatchIssue(closeOutcome=%s): %v", outcome, err)
	}
	env.version = dto.Version
}

// seedOriginalEdge gives the issue a `duplicates` edge to another issue of
// its project: since sk-540.2 a closed issue's outcome may change to
// duplicate without a target only when that edge already exists. Inserted
// directly — the edge is a fixture here, and a direct row writes no event
// and no notification that could mix into the assertions.
func seedOriginalEdge(t *testing.T, env *outcomeEnv) {
	t.Helper()
	var projectPrefix string
	if err := env.pool.QueryRow(t.Context(),
		`SELECT id_prefix FROM projects WHERE id = $1`, env.projectID).Scan(&projectPrefix); err != nil {
		t.Fatalf("read project prefix: %v", err)
	}
	original, err := env.issues.CreateIssue(t.Context(), env.projectID, projectPrefix, "counter",
		issues.CreateIssueInput{Title: "original", Type: "task", Priority: 2},
		env.author, env.authorAct)
	if err != nil {
		t.Fatalf("CreateIssue(original): %v", err)
	}
	if _, err := env.pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'duplicates', '{}'::jsonb, 1, $3, now())`, env.issueID, original.ID, env.author); err != nil {
		t.Fatalf("seed duplicates edge: %v", err)
	}
	clearInbox(t, env.pool, env.issueID)
}

// ---------------------------------------------------------------------------
// (a) outcome edit by a non-author
// ---------------------------------------------------------------------------

// TestOutcomeFanOut_PatchByOther — row 43 (a) and (d).
func TestOutcomeFanOut_PatchByOther(t *testing.T) {
	for name, to := range map[string]struct{ first, second string }{
		"done → wont_do":      {issues.CloseOutcomeDone, issues.CloseOutcomeWontDo},
		"wont_do → duplicate": {issues.CloseOutcomeWontDo, issues.CloseOutcomeDuplicate},
	} {
		t.Run(name, func(t *testing.T) {
			env := newOutcomeEnv(t)
			if to.first != issues.CloseOutcomeDone {
				// (d) does not start from "done": the intermediate edit is made
				// BY THE AUTHOR, so that its own fan-out writes nothing.
				patchOutcome(t, env, env.authorAct, to.first)
				clearInbox(t, env.pool, env.issueID)
			}

			if to.second == issues.CloseOutcomeDuplicate {
				seedOriginalEdge(t, env)
			}
			patchOutcome(t, env, env.otherAct, to.second)

			authorRows := inbox(t, env, env.author)
			if len(authorRows) != 1 {
				t.Fatalf("the author got %d rows, want exactly one: %v",
					len(authorRows), typesOf(authorRows))
			}
			row := onlyRow(t, "author", authorRows, "issue.outcome_changed")
			if row.Field == nil || *row.Field != "closeOutcome" {
				t.Errorf("field = %v, want closeOutcome", row.Field)
			}
			if row.OldValue != to.first {
				t.Errorf("oldValue = %v, want %q", row.OldValue, to.first)
			}
			if row.NewValue != to.second {
				t.Errorf("newValue = %v, want %q", row.NewValue, to.second)
			}
			if !strings.Contains(row.Summary, "outcome") {
				t.Errorf("short summary = %q, want it to name the outcome change", row.Summary)
			}

			// A subscriber who is not the author does not learn of the outcome edit at all: the edit does not
			// change the status, and the reason is addressed to one person.
			if rows := inbox(t, env, env.subscriber); len(rows) != 0 {
				t.Errorf("the subscriber got %d rows, want 0: %v", len(rows), typesOf(rows))
			}
		})
	}
}

// TestOutcomeFanOut_PatchByAuthorIsSilent — row 43 (c) on the edit path.
//
// The silence comes from the recipient SELECTION (excluding one's own action),
// not from the registry: the registry addresses the reason to the author without reservations. This has to be checked
// through the real writer precisely because the classifier corpus
// does not see such a layer.
func TestOutcomeFanOut_PatchByAuthorIsSilent(t *testing.T) {
	env := newOutcomeEnv(t)
	markReviewed(t, env)

	patchOutcome(t, env, env.authorAct, issues.CloseOutcomeWontDo)

	if rows := inbox(t, env, env.author); len(rows) != 0 {
		t.Errorf("the author changed the outcome themselves but got %d rows: %v", len(rows), typesOf(rows))
	}
	// And the review mark is NOT reset (US-3: they know about the change).
	if reviewedAt(t, env.pool, env.issueID) == nil {
		t.Error("the review mark was reset by the author's own outcome change")
	}
}

// TestOutcomeFanOut_DisabledReasonSilencesTheRowButNotTheReset — row 43 (e).
//
// Two claims, and the second is what the case exists for: the mark reset
// obeys the writer's GENERAL rule and does not live in the fan-out branch. An implementation
// that hung the reset on sending the notification goes red here and nowhere else.
func TestOutcomeFanOut_DisabledReasonSilencesTheRowButNotTheReset(t *testing.T) {
	env := newOutcomeEnv(t)
	markReviewed(t, env)

	off := false
	if _, err := env.notifs.UpdateReasonPrefs(t.Context(), env.author,
		notifications.UpdateReasonPrefsInput{Reasons: []notifications.ReasonChannelUpdate{
			{Reason: string(notifications.ReasonKeyOutcomeChanged), Feed: &off, Push: &off},
		}}); err != nil {
		t.Fatalf("UpdateReasonPrefs: %v", err)
	}

	patchOutcome(t, env, env.otherAct, issues.CloseOutcomeWontDo)

	rows := inbox(t, env, env.author)
	if len(rows) != 0 {
		t.Errorf("the author disabled the reason but got %d rows: %v — the reason must not be substituted",
			len(rows), typesOf(rows))
	}
	if at := reviewedAt(t, env.pool, env.issueID); at != nil {
		t.Errorf("reviewed_at = %v, want a reset: it obeys the general rule, "+
			"not the fan-out branch", *at)
	}
}

// TestOutcomeFanOut_CompositePatch — row 43 (h), R-7 "one notification per
// reason".
//
// One call changes TWO reasons at once: the outcome (for the author) and the assignee (for the new
// assignee). Neither is suppressed and neither is doubled.
func TestOutcomeFanOut_CompositePatch(t *testing.T) {
	env := newOutcomeEnv(t)
	assignee, assigneeName := testsupport.SeedUser(t, env.pool, "user")
	testsupport.AddProjectMember(t, env.pool, env.projectID, assignee, "member")

	outcome := issues.CloseOutcomeWontDo
	dto, err := env.issues.PatchIssue(t.Context(), env.issueID, issues.PatchIssueInput{
		ExpectedVersion: env.version,
		CloseOutcome:    &outcome,
		Assignee:        &assigneeName,
	}, env.otherAct)
	if err != nil {
		t.Fatalf("PatchIssue(composite): %v", err)
	}
	env.version = dto.Version

	// Author: EXACTLY ONE row about the outcome change — not two — and none about
	// the status. The issue.updated row MUST exist for them: they are subscribed to
	// their own issue, the assignee change reaches subscribers with the reason
	// "Issue assignment", and R-7 says outright that a composite call behaves
	// as today — one notification per reason. Suppressing it would mean
	// suppressing someone else's reason for the sake of one's own.
	authorRows := inbox(t, env, env.author)
	onlyRow(t, "author", authorRows, "issue.outcome_changed")
	noRowsOfType(t, "author", authorRows, "issue.status_changed")
	onlyRow(t, "author", authorRows, "issue.updated")
	if len(authorRows) != 2 {
		t.Errorf("the author got %d rows, want exactly two (outcome change + assignee change): %v",
			len(authorRows), typesOf(authorRows))
	}
	if got := reasonsOf(t, env, env.author); got["outcome_changed"] != 1 || got["assignment"] != 1 {
		t.Errorf("reasons of the author's rows = %v, want one outcome_changed and one assignment row", got)
	}
	if reasonsOf(t, env, env.author)["delegated_status"] != 0 {
		t.Error("the author got the delegated_status reason — an outcome edit does not change the status")
	}

	// New assignee: exactly one row of their own present-day reason. They do not learn about the
	// outcome — the reason is addressed to the requester.
	assigneeRows := inbox(t, env, assignee)
	onlyRow(t, "new assignee", assigneeRows, "issue.updated")
	noRowsOfType(t, "new assignee", assigneeRows, "issue.outcome_changed")
	if got := reasonsOf(t, env, assignee); got["assignment"] != 1 {
		t.Errorf("reasons of the new assignee's rows = %v, want one assignment row", got)
	}
}

// reasonsOf counts the notification rows of one person for an issue by
// REASON. The reason is not on the feed DTO (the client does not need it), and R-7 is worded
// in reasons — so the column is read directly here.
func reasonsOf(t *testing.T, env *outcomeEnv, userID uuid.UUID) map[string]int {
	t.Helper()
	rows, err := env.pool.Query(context.Background(), `
SELECT COALESCE(n.reason, ''), count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1 AND e.entity_type = 'issue' AND e.entity_id = $2
 GROUP BY 1`, userID, env.issueID)
	if err != nil {
		t.Fatalf("read reasons: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			t.Fatalf("scan reason: %v", err)
		}
		out[reason] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reason rows: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// (b), (f), (g) — repeated close and transition, A TABLE OVER BOTH WRITERS
// ---------------------------------------------------------------------------

// closeWriter — one of the two close paths. They cannot be merged: each
// calls ITS OWN WriteIssueEvent and ITS OWN notifications.FanOut, and half
// a table over one writer is green when the other diverges.
type closeWriter struct {
	name string
	// closeAgain closes an already closed issue with the given outcome.
	closeAgain func(t *testing.T, env *outcomeEnv, actor issues.Actor, outcome *string)
}

func closeWriters() []closeWriter {
	return []closeWriter{
		{
			// The agent path: skein_close.
			name: "Store.Close",
			closeAgain: func(t *testing.T, env *outcomeEnv, actor issues.Actor, outcome *string) {
				t.Helper()
				dto, err := env.issues.Close(t.Context(), issues.CloseInput{
					ID: env.issueID, ExpectedVersion: env.version,
					Reason: "again", Actor: actor, CloseOutcome: outcome,
				})
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
				env.version = dto.Version
			},
		},
		{
			// The human path: modal, board and REST batch
			// (handler/bulk.go#runOne, close/archive branch).
			name: "Store.UpdateStatus",
			closeAgain: func(t *testing.T, env *outcomeEnv, actor issues.Actor, outcome *string) {
				t.Helper()
				dto, err := env.issues.UpdateStatus(t.Context(), env.issueID, issues.UpdateStatusInput{
					Status: issues.StatusClosed, ExpectedVersion: env.version,
					CloseOutcome: outcome,
				}, actor)
				if err != nil {
					t.Fatalf("UpdateStatus(closed): %v", err)
				}
				env.version = dto.Version
			},
		},
	}
}

// TestOutcomeFanOut_RepeatCloseWithChange — row 43 (b) and (c) on a repeated
// close, over both writers.
func TestOutcomeFanOut_RepeatCloseWithChange(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name+"/by non-author", func(t *testing.T) {
			env := newOutcomeEnv(t)
			outcome := issues.CloseOutcomeWontDo
			w.closeAgain(t, env, env.otherAct, &outcome)

			authorRows := inbox(t, env, env.author)
			onlyRow(t, "author", authorRows, "issue.outcome_changed")
			// R-7 in its sharpest form: the author is SUBSCRIBED to their issue
			// (the requester's auto-watch), and without the rule "IsCreator beats
			// IsSubscriber" they would get a second row — about the status.
			noRowsOfType(t, "author", authorRows, "issue.status_changed")
			if len(authorRows) != 1 {
				t.Errorf("the author got %d rows, want exactly one: %v",
					len(authorRows), typesOf(authorRows))
			}

			// A subscriber who is not the author gets the status row, and it carries the NEW
			// outcome — that is the API field of SC-011.
			subRow := onlyRow(t, "subscriber", inbox(t, env, env.subscriber), "issue.status_changed")
			if subRow.CloseOutcome == nil || *subRow.CloseOutcome != issues.CloseOutcomeWontDo {
				t.Errorf("closeOutcome of the subscriber's row = %v, want %q",
					subRow.CloseOutcome, issues.CloseOutcomeWontDo)
			}
			if !strings.Contains(subRow.Summary, "won't do") {
				t.Errorf("short summary of the subscriber = %q, want it to name the outcome", subRow.Summary)
			}
			noRowsOfType(t, "subscriber", inbox(t, env, env.subscriber), "issue.outcome_changed")
		})

		t.Run(w.name+"/by author", func(t *testing.T) {
			env := newOutcomeEnv(t)
			markReviewed(t, env)
			outcome := issues.CloseOutcomeWontDo
			w.closeAgain(t, env, env.authorAct, &outcome)

			if rows := inbox(t, env, env.author); len(rows) != 0 {
				t.Errorf("the author re-closed it themselves but got %d rows: %v",
					len(rows), typesOf(rows))
			}
			if reviewedAt(t, env.pool, env.issueID) == nil {
				t.Error("the mark was reset by the author's own outcome change")
			}
		})
	}
}

// TestOutcomeFanOut_RepeatCloseWithoutChange — row 43 (f): a repeated
// close WITHOUT an outcome change behaves as today.
//
// This is a guard, and without it the previous test is worth nothing: an implementation
// writing issue.outcome_changed on every repeated close would be
// green there. Here the author must get an ordinary status row — that is,
// rule R-7 has no right to silence closes that changed nothing.
func TestOutcomeFanOut_RepeatCloseWithoutChange(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			env := newOutcomeEnv(t)
			w.closeAgain(t, env, env.otherAct, nil)

			authorRows := inbox(t, env, env.author)
			noRowsOfType(t, "author", authorRows, "issue.outcome_changed")
			row := onlyRow(t, "author", authorRows, "issue.status_changed")
			if row.CloseOutcome == nil || *row.CloseOutcome != issues.CloseOutcomeDone {
				t.Errorf("closeOutcome = %v, want %q (INV-10: a repeat without an outcome does not change the outcome)",
					row.CloseOutcome, issues.CloseOutcomeDone)
			}
			// "Done" does not lengthen the text — the former line byte for byte.
			if strings.Contains(row.Summary, "outcome:") {
				t.Errorf("short summary = %q, a \"done\" must have no tail", row.Summary)
			}
		})
	}
}

// TestOutcomeFanOut_TransitionCarriesTheOutcome — row 43 (g), over both
// writers: the FIRST transition to "closed".
//
// Three cases, and the third is the one that tells apart an implementation that put the outcome into the
// payload only for Store.Close: archiving an unclosed issue goes through
// UpdateStatus (the REST batch path), and without the outcome in ITS payload a subscriber
// would get a row without the outcome on a close from the UI — SC-011 red
// with a green matrix.
func TestOutcomeFanOut_TransitionCarriesTheOutcome(t *testing.T) {
	wontDo := issues.CloseOutcomeWontDo

	type transition struct {
		name        string
		wantOutcome string
		// closeFresh closes an OPEN issue.
		closeFresh func(t *testing.T, env *outcomeEnv)
	}
	transitions := []transition{
		{
			name:        "Store.Close with an explicit \"won't do\"",
			wantOutcome: issues.CloseOutcomeWontDo,
			closeFresh: func(t *testing.T, env *outcomeEnv) {
				t.Helper()
				dto, err := env.issues.Close(t.Context(), issues.CloseInput{
					ID: env.issueID, ExpectedVersion: env.version,
					Reason: "won't do", Actor: env.otherAct, CloseOutcome: &wontDo,
				})
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
				env.version = dto.Version
			},
		},
		{
			name:        "Store.Close without an outcome — \"done\", text as today",
			wantOutcome: issues.CloseOutcomeDone,
			closeFresh: func(t *testing.T, env *outcomeEnv) {
				t.Helper()
				dto, err := env.issues.Close(t.Context(), issues.CloseInput{
					ID: env.issueID, ExpectedVersion: env.version,
					Reason: "done", Actor: env.otherAct,
				})
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
				env.version = dto.Version
			},
		},
		{
			// sk-540.4: archiving an unclosed issue names its outcome.
			name:        "Store.UpdateStatus, archiving an unclosed issue with an explicit \"won't do\"",
			wantOutcome: issues.CloseOutcomeWontDo,
			closeFresh: func(t *testing.T, env *outcomeEnv) {
				t.Helper()
				wontDo := issues.CloseOutcomeWontDo
				dto, err := env.issues.UpdateStatus(t.Context(), env.issueID, issues.UpdateStatusInput{
					Status: issues.StatusClosed, ExpectedVersion: env.version,
					CloseOutcome: &wontDo, Archive: true,
				}, env.otherAct)
				if err != nil {
					t.Fatalf("UpdateStatus(archive): %v", err)
				}
				env.version = dto.Version
			},
		},
	}

	for _, tc := range transitions {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpenOutcomeEnv(t)
			tc.closeFresh(t, env)

			row := onlyRow(t, "subscriber", inbox(t, env, env.subscriber), "issue.status_changed")
			if row.CloseOutcome == nil || *row.CloseOutcome != tc.wantOutcome {
				t.Fatalf("closeOutcome of the subscriber's row = %v, want %q",
					row.CloseOutcome, tc.wantOutcome)
			}
			if tc.wantOutcome == issues.CloseOutcomeDone {
				if strings.Contains(row.Summary, "outcome:") {
					t.Errorf("short summary = %q, a \"done\" must have no tail", row.Summary)
				}
			} else if !strings.Contains(row.Summary, "won't do") {
				t.Errorf("short summary = %q, want it to name the outcome", row.Summary)
			}
			// A transition is not an outcome change: no issue.outcome_changed events
			// must exist here for anyone.
			noRowsOfType(t, "subscriber", inbox(t, env, env.subscriber), "issue.outcome_changed")
			noRowsOfType(t, "author", inbox(t, env, env.author), "issue.outcome_changed")
		})
	}
}

// newOpenOutcomeEnv — the same fixture, but the issue stays OPEN: the transition cases
// close it themselves.
func newOpenOutcomeEnv(t *testing.T) *outcomeEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	env := &outcomeEnv{
		pool:   pool,
		issues: issues.NewStore(pool),
		notifs: notifications.NewStore(pool),
	}
	var authorName, otherName string
	env.author, authorName = testsupport.SeedUser(t, pool, "user")
	env.other, otherName = testsupport.SeedUser(t, pool, "user")
	env.subscriber, _ = testsupport.SeedUser(t, pool, "user")
	env.authorAct = issues.HumanActor(env.author, authorName)
	env.otherAct = issues.HumanActor(env.other, otherName)

	var prefix string
	env.projectID, prefix = testsupport.SeedProject(t, pool, env.author, "ofo")
	testsupport.AddProjectMember(t, pool, env.projectID, env.other, "member")
	testsupport.AddProjectMember(t, pool, env.projectID, env.subscriber, "member")

	dto, err := env.issues.CreateIssue(t.Context(), env.projectID, prefix, "counter",
		issues.CreateIssueInput{Title: "work", Type: "task", Priority: 2},
		env.author, env.authorAct)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	env.issueID = dto.ID
	env.version = dto.Version
	watch(t, pool, env.issueID, env.subscriber)
	clearInbox(t, pool, env.issueID)
	return env
}

// ---------------------------------------------------------------------------
// Gate of the owner: the ERROR window is closed
// ---------------------------------------------------------------------------

// unmappedCounterName — the name of the metric the fan-out moves on the
// OutcomeUnmapped outcome. Read BY NAME from the shared registry, as monitoring
// reads it: the variable is not exported, and it has to be asserted about from an
// external test package.
const unmappedCounterName = "skein_notifications_fanout_unmapped_events_total"

// unmappedCounter takes the current value of the counter.
func unmappedCounter(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != unmappedCounterName {
			continue
		}
		var sum float64
		for _, m := range f.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
		return sum
	}
	t.Fatalf("metric %s is not in the registry — the snapshot would measure nothing", unmappedCounterName)
	return 0
}

// TestOutcomeFanOut_NoUnmappedSignal — THE MAIN GATE OF THIS TASK, a decision
// of the owner, not a wish.
//
// Between the rollout of the writers (T003/T004) and the registry entry (T010) every outcome
// change cost one ERROR-level record and one increment of
// skein_notifications_fanout_unmapped_events_total, whose own annotation
// calls ANY non-zero value a defect to fix, not traffic to
// watch. The wave A/2 critics measured 35 and 39 such records in one
// integration run. This test is the permanent proof that the window
// is closed, and it goes red exactly when it is opened again.
//
// ⚠ MEASURED THROUGH THE REAL WRITERS, once for each path
// that the outcome change event produces: an edit of a closed issue and a repeated
// close by both writers. A check "the classifier knows the type" would be
// weaker: the signal is raised by the fan-out, and it raises it by the fact of the event, not by
// the answer to the question about the type.
func TestOutcomeFanOut_NoUnmappedSignal(t *testing.T) {
	logs := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	before := unmappedCounter(t)

	// 1. Edit of the outcome of a closed issue.
	env := newOutcomeEnv(t)
	patchOutcome(t, env, env.otherAct, issues.CloseOutcomeWontDo)

	// 2-3. Repeated close with an outcome change — by both writers.
	for _, w := range closeWriters() {
		e := newOutcomeEnv(t)
		outcome := issues.CloseOutcomeDuplicate
		seedOriginalEdge(t, e) // sk-540.2: the change needs the edge
		w.closeAgain(t, e, e.otherAct, &outcome)
	}

	if got := unmappedCounter(t) - before; got != 0 {
		t.Errorf("counter %s moved by %v — an outcome change is again classified as a configuration "+
			"error; the metric annotation calls any non-zero value a defect",
			unmappedCounterName, got)
	}

	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		if msg, _ := rec["msg"].(string); strings.Contains(msg, "not mapped to any reason") {
			t.Errorf("the fan-out wrote ERROR about an unmapped type: %v", rec)
		}
		if lvl, _ := rec["level"].(string); lvl == "ERROR" {
			t.Errorf("the outcome change wrote an ERROR-level record: %v", rec)
		}
	}
}

// TestOutcomeFanOut_UnmappedSignalStillWorks — a control shot against
// tautology. The claim above makes sense only if the signal is
// reachable at all: a counter that NEVER moves "did not move" for free.
func TestOutcomeFanOut_UnmappedSignalStillWorks(t *testing.T) {
	before := unmappedCounter(t)
	if _, err := notifications.FanOut(t.Context(), nil, notifications.FanOutInput{
		EventID:    1,
		EventType:  "issue.teleported",
		EntityType: "issue",
		IssueID:    "gate-1",
		ProjectID:  uuid.Must(uuid.NewRandom()),
	}); err != nil {
		t.Fatalf("FanOut on an unknown type must not crash the caller: %v", err)
	}
	if got := unmappedCounter(t) - before; got != 1 {
		t.Fatalf("counter %s moved by %v on a deliberately unknown type — "+
			"the previous test does not prove anything", unmappedCounterName, got)
	}
}
