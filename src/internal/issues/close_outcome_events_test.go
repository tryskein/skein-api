package issues

// close_outcome_events_test.go — sk-489, T003, matrix row 11.
//
// Close and outcome-change events — as a table over BOTH writers
// (Store.Close and Store.UpdateStatus on a transition, on archiving and on
// closing an already closed issue). They must not be split: each writer writes ITS OWN
// WriteIssueEventWithPayload and its own notifications.FanOut, so half of
// the table for one writer is green while the other diverges. The
// UpdateStatus path for a repeated close is not artificial:
// handler/bulk.go#runOne calls exactly it for both `close` and `archive`, and
// US-11 prescribes a repeated close with an outcome change for the batch.
//
// Two mutations the table must turn red:
//  1. a payload built from the INPUT (input.CloseOutcome) instead of
//     the resolved outcome: green on every explicit value and red exactly
//     on inputs without an outcome — a transition without an outcome and archiving without an outcome;
//  2. previousCloseOutcome and issue.outcome_changed written only in
//     Store.Close: green on half the table, while in the product an outcome change
//     by a batch writes no events at all.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/outbox"
)

type eventRecord struct {
	ID        int64
	Type      string
	Field     *string
	OldData   []byte
	NewData   []byte
	Payload   []byte
	ProjectID *uuid.UUID
}

// readIssueEvents reads the issue's events in write order. It reads RAW
// rows: the row 11 assertions are about what landed in `events`, not about
// what some serializer on top returns.
func readIssueEvents(t *testing.T, pool *pgxpool.Pool, issueID string, types ...string) []eventRecord {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT id, type, field, old_data, new_data, payload, project_id
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = ANY($2)
 ORDER BY id`, issueID, types)
	if err != nil {
		t.Fatalf("read events %s: %v", issueID, err)
	}
	defer rows.Close()
	var out []eventRecord
	for rows.Next() {
		var r eventRecord
		if err := rows.Scan(&r.ID, &r.Type, &r.Field, &r.OldData, &r.NewData, &r.Payload, &r.ProjectID); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return out
}

func decodePayload(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode payload %q: %v", string(raw), err)
	}
	return m
}

// wantClosePayload — in BOTH close events the payload carries the resolved
// outcome; previousCloseOutcome is present exactly when there was a change.
func wantClosePayload(t *testing.T, pool *pgxpool.Pool, issueID, wantOutcome, wantPrevious string) {
	t.Helper()
	evs := readIssueEvents(t, pool, issueID, EventIssueStatusChanged, EventIssueClosed)
	if len(evs) == 0 {
		t.Fatalf("%s: no close events at all", issueID)
	}
	// Take the last pair: a repeated close has several events.
	last := map[string]eventRecord{}
	for _, e := range evs {
		last[e.Type] = e
	}
	for _, typ := range []string{EventIssueStatusChanged, EventIssueClosed} {
		e, ok := last[typ]
		if !ok {
			t.Fatalf("%s: no %s event", issueID, typ)
		}
		p := decodePayload(t, e.Payload)
		if got, _ := p["closeOutcome"].(string); got != wantOutcome {
			t.Fatalf("%s/%s: payload.closeOutcome = %v, want %q (payload %s)",
				issueID, typ, p["closeOutcome"], wantOutcome, string(e.Payload))
		}
		prev, hasPrev := p["previousCloseOutcome"]
		if wantPrevious == "" {
			if hasPrev {
				t.Fatalf("%s/%s: previousCloseOutcome is present (%v), but there was no outcome change",
					issueID, typ, prev)
			}
		} else {
			if !hasPrev {
				t.Fatalf("%s/%s: previousCloseOutcome is absent, want %q", issueID, typ, wantPrevious)
			}
			if got, _ := prev.(string); got != wantPrevious {
				t.Fatalf("%s/%s: previousCloseOutcome = %v, want %q", issueID, typ, prev, wantPrevious)
			}
		}
	}
	// new_data of status_changed is still the JSON string of the status: it is
	// read by outbox/push.go#delegatedStatusHead, including on an old
	// pod in the rollout window.
	sc := last[EventIssueStatusChanged]
	var newData any
	if err := json.Unmarshal(sc.NewData, &newData); err != nil {
		t.Fatalf("new_data of status_changed is not JSON: %q", string(sc.NewData))
	}
	if s, ok := newData.(string); !ok || s != StatusClosed {
		t.Fatalf("new_data of status_changed = %v (%T), want string %q", newData, newData, StatusClosed)
	}
}

// TestCloseOutcome_EventsTable — row 11 over both writers.
func TestCloseOutcome_EventsTable(t *testing.T) {
	for _, w := range closeWriters() {
		t.Run(w.name, func(t *testing.T) {
			env := newOutcomeEnv(t)

			t.Run("transition with an explicit outcome: payload without previousCloseOutcome", func(t *testing.T) {
				iss := env.issue(t, "transition with explicit")
				outcome := CloseOutcomeWontDo
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor, Outcome: &outcome,
				}); err != nil {
					t.Fatalf("close: %v", err)
				}
				wantClosePayload(t, env.pool, iss.ID, CloseOutcomeWontDo, "")
				if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueOutcomeChanged)); n != 0 {
					t.Fatalf("the transition wrote %d outcome-change events — there was nothing to change", n)
				}
			})

			t.Run("repeated close with a change X→Y", func(t *testing.T) {
				iss := env.issue(t, "outcome change by repeat")
				first := CloseOutcomeDone
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor, Outcome: &first,
				}); err != nil {
					t.Fatalf("first close: %v", err)
				}
				closed := readCloseStamps(t, env.pool, iss.ID)
				second := CloseOutcomeSuperseded
				// sk-540.2: a change to superseded names its replacement.
				replacement := env.issue(t, "replacement")
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: closed.Version, Actor: env.actor, Outcome: &second,
					Target: replacement.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("repeated close: %v", err)
				}

				wantClosePayload(t, env.pool, iss.ID, CloseOutcomeSuperseded, CloseOutcomeDone)

				changed := readIssueEvents(t, env.pool, iss.ID, EventIssueOutcomeChanged)
				if len(changed) != 1 {
					t.Fatalf("issue.outcome_changed events: %d, want exactly 1", len(changed))
				}
				ev := changed[0]
				if ev.Field == nil || *ev.Field != "closeOutcome" {
					t.Fatalf("field = %v, want closeOutcome", ev.Field)
				}
				var oldVal, newVal any
				if err := json.Unmarshal(ev.OldData, &oldVal); err != nil {
					t.Fatalf("old_data is not JSON: %q", string(ev.OldData))
				}
				if err := json.Unmarshal(ev.NewData, &newVal); err != nil {
					t.Fatalf("new_data is not JSON: %q", string(ev.NewData))
				}
				if oldVal != CloseOutcomeDone || newVal != CloseOutcomeSuperseded {
					t.Fatalf("old/new = %v/%v, want outcome strings %q/%q",
						oldVal, newVal, CloseOutcomeDone, CloseOutcomeSuperseded)
				}
				p := decodePayload(t, ev.Payload)
				if p["closeOutcome"] != CloseOutcomeSuperseded || p["previousCloseOutcome"] != CloseOutcomeDone {
					t.Fatalf("payload of the change event: %s", string(ev.Payload))
				}

				// Bus subject: this is an ORDINARY issue event, not
				// field_changed. An event written as field_changed
				// would get the subject issue.field.close-outcome, which the
				// frontend's live feed skips.
				subject := outbox.BuildSubject(outbox.SubjectInput{
					EventType:  ev.Type,
					EntityType: "issue",
					EntityID:   iss.ID,
					ProjectID:  ev.ProjectID,
					Field:      derefField(ev.Field),
				})
				wantTail := "issue.outcome_changed." + iss.ID
				if !strings.HasSuffix(subject, wantTail) {
					t.Fatalf("subject %q does not end with %q", subject, wantTail)
				}
				if strings.Contains(subject, "issue.field.") {
					t.Fatalf("subject %q went into the field_changed branch", subject)
				}

				// History: the "was → became" line is read from old/new.
				entries, _, err := env.store.ListIssueActivity(t.Context(), env.pid, iss.ID,
					[]string{EventIssueOutcomeChanged}, nil, 20)
				if err != nil {
					t.Fatalf("ListIssueActivity: %v", err)
				}
				if len(entries) != 1 {
					t.Fatalf("history entries: %d, want 1", len(entries))
				}
				if entries[0].OldValue != CloseOutcomeDone || entries[0].NewValue != CloseOutcomeSuperseded {
					t.Fatalf("history: oldValue/newValue = %v/%v, want outcome strings",
						entries[0].OldValue, entries[0].NewValue)
				}
			})

			t.Run("repeated close with the same outcome — neither previous nor a change event", func(t *testing.T) {
				iss := env.issue(t, "repeat without change")
				outcome := CloseOutcomeDuplicate
				// sk-540.2: the transition names the original; the repeat
				// below keeps the outcome and needs no target (exception (b)).
				original := env.issue(t, "original")
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor, Outcome: &outcome,
					Target: original.ID, Caller: &env.caller,
				}); err != nil {
					t.Fatalf("first close: %v", err)
				}
				closed := readCloseStamps(t, env.pool, iss.ID)
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: closed.Version, Actor: env.actor, Outcome: &outcome,
				}); err != nil {
					t.Fatalf("repeated close: %v", err)
				}
				wantClosePayload(t, env.pool, iss.ID, CloseOutcomeDuplicate, "")
				if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueOutcomeChanged)); n != 0 {
					t.Fatalf("a repeat with the same outcome wrote %d change events", n)
				}
			})

			// --- inputs without an explicit outcome: only the UpdateStatus writer ---
			//
			// A transition without an outcome and archiving without an outcome are expressible only
			// through it: CloseInput has no archive flag, and
			// "a transition without an outcome" on Store.Close is checked by row 8 and
			// by the payload below. The cell for Close is closed by this note.
			if !w.supportsArchve {
				t.Run("transition without an outcome: payload carries the resolved done", func(t *testing.T) {
					iss := env.issue(t, "transition without an outcome (Close)")
					if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
						Version: iss.Version, Actor: env.actor,
					}); err != nil {
						t.Fatalf("close: %v", err)
					}
					wantClosePayload(t, env.pool, iss.ID, CloseOutcomeDone, "")
				})
				return
			}

			t.Run("transition without an outcome: payload carries the resolved done", func(t *testing.T) {
				iss := env.issue(t, "transition without an outcome")
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor,
				}); err != nil {
					t.Fatalf("close: %v", err)
				}
				wantClosePayload(t, env.pool, iss.ID, CloseOutcomeDone, "")
			})

			t.Run("archiving a standing issue without an outcome: payload carries the resolved done, not the emptiness of the input", func(t *testing.T) {
				// sk-540.4: the only archive that resolves an outcome the
				// caller did not name is a standing row (→ done).
				iss := env.issue(t, "archiving a standing issue without an outcome")
				if _, err := env.store.UpdateStatus(t.Context(), iss.ID, UpdateStatusInput{
					ExpectedVersion: iss.Version, Status: StatusStanding,
				}, env.actor); err != nil {
					t.Fatalf("promotion to standing: %v", err)
				}
				promoted := readCloseStamps(t, env.pool, iss.ID)
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: promoted.Version, Actor: env.actor, Archive: true,
				}); err != nil {
					t.Fatalf("archiving: %v", err)
				}
				wantClosePayload(t, env.pool, iss.ID, CloseOutcomeDone, "")
			})

			t.Run("an outcome change by a batch repeat writes the event on this writer too", func(t *testing.T) {
				// US-11: the batch re-closes an already closed issue with
				// an explicit outcome. The path is the same as handler/bulk.go#runOne
				// (action=close: since sk-540.4 an archive no longer
				// re-closes a closed row with the batch outcome).
				iss := env.issue(t, "outcome change by batch")
				wontDo := CloseOutcomeWontDo
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: iss.Version, Actor: env.actor, Outcome: &wontDo, Archive: true,
				}); err != nil {
					t.Fatalf("archiving: %v", err)
				}
				wantOutcome(t, env.pool, iss.ID, CloseOutcomeWontDo)
				closed := readCloseStamps(t, env.pool, iss.ID)
				done := CloseOutcomeDone
				if err := w.closeIssue(t, env.store, iss.ID, closeAttempt{
					Version: closed.Version, Actor: env.actor,
					Outcome: &done,
				}); err != nil {
					t.Fatalf("repeated close with an explicit outcome: %v", err)
				}
				wantClosePayload(t, env.pool, iss.ID, CloseOutcomeDone, CloseOutcomeWontDo)
				if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueOutcomeChanged)); n != 1 {
					t.Fatalf("outcome-change events: %d, want 1", n)
				}
			})
		})
	}
}

func derefField(f *string) string {
	if f == nil {
		return ""
	}
	return *f
}
