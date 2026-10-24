package issues

// close_outcome_patch_test.go — sk-489, T004.
//
// Smoke test of the gate "edit the outcome of a closed issue": the Store.PatchIssue path.
// The full tables of matrix rows 14–16 are delivered by the issue's test delivery;
// here is the minimum proving that the edit EDITS EXACTLY THE OUTCOME and nothing
// else: status, date and close reason stay as they were (INV-8),
// the acknowledgement mark is reset by the predicate, not always, and the target
// obeys the same rules as on the close paths.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"skein-api/internal/dependencies"
	"skein-api/internal/outbox"
)

// closedDoneIssue — an author's issue closed with the outcome "done",
// marked as reviewed. The mark is set by a real operation: faking
// it by a direct write would prove a reset on an invented state.
func closedDoneIssue(t *testing.T, env *outcomeEnv) *IssueDetail {
	t.Helper()
	required := true
	dto, err := env.store.CreateIssue(t.Context(), env.pid, env.prefix, "counter",
		CreateIssueInput{Title: "result", Type: "task", Priority: 2, ReviewRequired: &required},
		env.author, env.actor)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := env.store.Close(t.Context(), CloseInput{
		ID: dto.ID, ExpectedVersion: dto.Version, Reason: "done", Actor: env.actor,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := env.store.GetIssue(t.Context(), dto.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if _, err := env.store.MarkReviewed(t.Context(), ReviewMarkInput{
		ID: dto.ID, ExpectedVersion: closed.Version, Actor: env.actor,
	}); err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	marked := readReviewMark(t, env.pool, dto.ID)
	if marked.ReviewedAt == nil {
		t.Fatalf("precondition: the mark was not set")
	}
	out, err := env.store.GetIssue(t.Context(), dto.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	return out
}

func patchOutcome(t *testing.T, env *outcomeEnv, id string, version int,
	outcome string, target string, actor Actor) error {
	t.Helper()
	caller := DependenciesCaller{UserID: actor.UserID, Role: "user"}
	_, err := env.store.PatchIssue(t.Context(), id, PatchIssueInput{
		ExpectedVersion:    version,
		CloseOutcome:       &outcome,
		CloseTarget:        target,
		DependenciesCaller: &caller,
	}, actor)
	return err
}

// TestPatchCloseOutcome_Smoke — smoke test of the T004 gate.
func TestPatchCloseOutcome_Smoke(t *testing.T) {
	env := newOutcomeEnv(t)

	t.Run("an edit by a non-author resets the mark, date and reason are intact", func(t *testing.T) {
		iss := closedDoneIssue(t, env)
		before := readReviewMark(t, env.pool, iss.ID)

		stranger := seedUser(t, env.pool)
		strangerActor := HumanActor(stranger, readUsername(t, env.pool, stranger))
		if err := patchOutcome(t, env, iss.ID, before.Version, CloseOutcomeWontDo, "", strangerActor); err != nil {
			t.Fatalf("PatchIssue(done→wont_do): %v", err)
		}

		wantOutcome(t, env.pool, iss.ID, CloseOutcomeWontDo)
		after := readReviewMark(t, env.pool, iss.ID)
		if after.ReviewedAt != nil || after.ReviewedBy != nil {
			t.Fatalf("mark not reset: reviewedAt=%v reviewedBy=%v", after.ReviewedAt, after.ReviewedBy)
		}
		if after.Status != StatusClosed {
			t.Fatalf("status after the outcome edit: %q", after.Status)
		}
		if after.Version <= before.Version {
			t.Fatalf("version did not grow: was %d, now %d", before.Version, after.Version)
		}

		card, err := env.store.GetIssue(t.Context(), iss.ID)
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if card.ClosedAt == nil || !card.ClosedAt.Equal(*iss.ClosedAt) {
			t.Fatalf("close date changed: was %v, now %v", iss.ClosedAt, card.ClosedAt)
		}
		if card.ClosedReason == nil || *card.ClosedReason != *iss.ClosedReason {
			t.Fatalf("close reason changed: was %v, now %v", iss.ClosedReason, card.ClosedReason)
		}
	})

	t.Run("an edit by the author does not reset the mark", func(t *testing.T) {
		iss := closedDoneIssue(t, env)
		before := readReviewMark(t, env.pool, iss.ID)
		if err := patchOutcome(t, env, iss.ID, before.Version, CloseOutcomeWontDo, "", env.actor); err != nil {
			t.Fatalf("PatchIssue by the author: %v", err)
		}
		wantOutcome(t, env.pool, iss.ID, CloseOutcomeWontDo)
		after := readReviewMark(t, env.pool, iss.ID)
		if after.ReviewedAt == nil || after.ReviewedBy == nil {
			t.Fatalf("the author's mark was reset, and should not be")
		}
	})

	t.Run("an edit of an unclosed issue — refusal, version unchanged", func(t *testing.T) {
		iss := env.issue(t, "open")
		err := patchOutcome(t, env, iss.ID, iss.Version, CloseOutcomeWontDo, "", env.actor)
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Code != "close_outcome_requires_closed" {
			t.Fatalf("want close_outcome_requires_closed, got %v", err)
		}
		after, gerr := env.store.GetIssue(t.Context(), iss.ID)
		if gerr != nil {
			t.Fatalf("GetIssue: %v", gerr)
		}
		if after.Version != iss.Version {
			t.Fatalf("version changed on refusal: was %d, now %d", iss.Version, after.Version)
		}
		if after.CloseOutcome != nil {
			t.Fatalf("an unclosed issue got an outcome %q", *after.CloseOutcome)
		}
	})

	t.Run("target in another visible project for a non-admin — link created", func(t *testing.T) {
		// A second project of the same author: he is its member, but not an admin —
		// the visibility key is the system role, not an administrator flag.
		otherPID, otherPrefix := seedProject(t, env.pool, env.author)
		target, err := env.store.CreateIssue(t.Context(), otherPID, otherPrefix, "counter",
			CreateIssueInput{Title: "target in a neighbouring project", Type: "task", Priority: 2},
			env.author, env.actor)
		if err != nil {
			t.Fatalf("CreateIssue(target): %v", err)
		}

		iss := closedDoneIssue(t, env)
		before := readReviewMark(t, env.pool, iss.ID)
		if err := patchOutcome(t, env, iss.ID, before.Version,
			CloseOutcomeDuplicate, target.ID, env.actor); err != nil {
			t.Fatalf("PatchIssue(duplicate + target): %v", err)
		}
		wantOutcome(t, env.pool, iss.ID, CloseOutcomeDuplicate)
		if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
			t.Fatalf("duplicates edges: %d, want 1", n)
		}
	})

	t.Run("target in an invisible project — refusal, outcome and edge unchanged", func(t *testing.T) {
		stranger := seedUser(t, env.pool)
		strangerActor := HumanActor(stranger, readUsername(t, env.pool, stranger))
		hiddenPID, hiddenPrefix := seedProject(t, env.pool, stranger)
		hidden, err := env.store.CreateIssue(t.Context(), hiddenPID, hiddenPrefix, "counter",
			CreateIssueInput{Title: "target in a foreign project", Type: "task", Priority: 2},
			stranger, strangerActor)
		if err != nil {
			t.Fatalf("CreateIssue(foreign target): %v", err)
		}

		iss := closedDoneIssue(t, env)
		before := readReviewMark(t, env.pool, iss.ID)
		err = patchOutcome(t, env, iss.ID, before.Version,
			CloseOutcomeSuperseded, hidden.ID, env.actor)
		if !errors.Is(err, dependencies.ErrDependencyTargetNotVisible) {
			t.Fatalf("want ErrDependencyTargetNotVisible, got %v", err)
		}
		wantOutcome(t, env.pool, iss.ID, CloseOutcomeDone)
		if n := countEdges(t, env.pool, hidden.ID, iss.ID, dependencies.DepSupersedes); n != 0 {
			t.Fatalf("edges after the refusal %d, want 0", n)
		}
	})

	t.Run("nonexistent target — refusal ErrTargetNotFound", func(t *testing.T) {
		iss := closedDoneIssue(t, env)
		before := readReviewMark(t, env.pool, iss.ID)
		err := patchOutcome(t, env, iss.ID, before.Version,
			CloseOutcomeDuplicate, env.prefix+"-999999", env.actor)
		if !errors.Is(err, dependencies.ErrTargetNotFound) {
			t.Fatalf("want ErrTargetNotFound, got %v", err)
		}
		wantOutcome(t, env.pool, iss.ID, CloseOutcomeDone)
	})
}

// patchOutcomeFull — an outcome edit with an arbitrary set of input fields.
func patchOutcomeFull(t *testing.T, env *outcomeEnv, id string, in PatchIssueInput, actor Actor) (CloseLinkState, error) {
	t.Helper()
	_, state, err := env.store.PatchIssueWithResult(t.Context(), id, in, actor)
	return state, err
}

// TestPatchCloseOutcome_EventAndDiff — matrix row 14: the outcome-change event
// on the edit path, and its absence where there was no change.
//
// The mutation the subcase "not part of the hybrid diff" was written for:
// put the outcome into IssuePatchFields. Then an edit of the outcome alone will start writing
// issue.updated + issue.field_changed, and the author will get TWO notifications for
// one action — exactly what R-7 forbids.
func TestPatchCloseOutcome_EventAndDiff(t *testing.T) {
	env := newOutcomeEnv(t)

	t.Run("X→Y: one issue.outcome_changed, no status and no diff", func(t *testing.T) {
		iss := closedDoneIssue(t, env)
		before := readCloseStamps(t, env.pool, iss.ID)
		// sk-540.2: a change to superseded names its replacement.
		replacement := env.issue(t, "replacement for X→Y")
		if err := patchOutcome(t, env, iss.ID, before.Version, CloseOutcomeSuperseded, replacement.ID, env.actor); err != nil {
			t.Fatalf("PatchIssue: %v", err)
		}

		changed := readIssueEvents(t, env.pool, iss.ID, EventIssueOutcomeChanged)
		if len(changed) != 1 {
			t.Fatalf("issue.outcome_changed events: %d, want 1", len(changed))
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
			t.Fatalf("old/new = %v/%v, want %q/%q", oldVal, newVal,
				CloseOutcomeDone, CloseOutcomeSuperseded)
		}
		// The subject is the same as in row 11: an ordinary issue event, not
		// field_changed.
		subject := outbox.BuildSubject(outbox.SubjectInput{
			EventType: ev.Type, EntityType: "issue", EntityID: iss.ID,
			ProjectID: ev.ProjectID, Field: derefField(ev.Field),
		})
		if !strings.HasSuffix(subject, "issue.outcome_changed."+iss.ID) ||
			strings.Contains(subject, "issue.field.") {
			t.Fatalf("subject of the outcome-change event: %q", subject)
		}

		// An outcome edit is not a status change and not a hybrid-diff row.
		if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueStatusChanged)); n != 1 {
			t.Fatalf("status_changed events: %d — an outcome edit does not change the status (INV-8); "+
				"want exactly one, from the close", n)
		}
		if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueUpdated, EventIssueFieldChanged)); n != 0 {
			t.Fatalf("an edit of the outcome alone wrote %d hybrid-diff events — "+
				"the outcome is not part of issue.updated (R-7: one reason per action)", n)
		}

		after := readCloseStamps(t, env.pool, iss.ID)
		if after.Status != before.Status {
			t.Fatalf("status changed: %q → %q", before.Status, after.Status)
		}
		if after.Version <= before.Version {
			t.Fatalf("version did not grow: %d → %d", before.Version, after.Version)
		}
	})

	t.Run("X→X: neither a change event nor a diff", func(t *testing.T) {
		iss := closedDoneIssue(t, env)
		before := readCloseStamps(t, env.pool, iss.ID)
		if err := patchOutcome(t, env, iss.ID, before.Version, CloseOutcomeDone, "", env.actor); err != nil {
			t.Fatalf("PatchIssue(X→X): %v", err)
		}
		if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueOutcomeChanged)); n != 0 {
			t.Fatalf("passing the same outcome wrote %d change events — there was no change", n)
		}
		if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueUpdated, EventIssueFieldChanged)); n != 0 {
			t.Fatalf("passing the same outcome wrote %d hybrid-diff events", n)
		}
		// The notification is deliberately not checked here: the reason
		// `outcome_changed` is not yet in the registry (T010 adds it), and
		// the assertion "no rows in the feed" would be green for the wrong
		// reason. The fan-out is matrix row 43 under T010; here we check
		// the ONLY thing it can arise from — the event.
		wantOutcome(t, env.pool, iss.ID, CloseOutcomeDone)
	})

	t.Run("X→X with a target: only the link, no change event", func(t *testing.T) {
		target := env.issue(t, "target for X→X")
		iss := closedDoneIssue(t, env)
		before := readCloseStamps(t, env.pool, iss.ID)
		// First move the outcome to duplicate (that is a change — one event).
		// sk-540.2: without a target that change needs an existing
		// `duplicates` edge from the issue — seeded to ANOTHER original,
		// so the target below still creates its own edge.
		seedDep(t, env.pool, iss.ID, env.issue(t, "former original").ID,
			dependencies.DepDuplicates, env.author)
		if err := patchOutcome(t, env, iss.ID, before.Version, CloseOutcomeDuplicate, "", env.actor); err != nil {
			t.Fatalf("PatchIssue(done→duplicate): %v", err)
		}
		mid := readCloseStamps(t, env.pool, iss.ID)
		// Now the same outcome, but with a target: no more change events
		// are added, and the link is created.
		state, err := patchOutcomeFull(t, env, iss.ID, PatchIssueInput{
			ExpectedVersion:    mid.Version,
			CloseOutcome:       strPtr(CloseOutcomeDuplicate),
			CloseTarget:        target.ID,
			DependenciesCaller: &env.caller,
		}, env.actor)
		if err != nil {
			t.Fatalf("PatchIssue(X→X with a target): %v", err)
		}
		if state != CloseLinkCreated {
			t.Fatalf("link flag %q, want %q", state, CloseLinkCreated)
		}
		if n := len(readIssueEvents(t, env.pool, iss.ID, EventIssueOutcomeChanged)); n != 1 {
			t.Fatalf("outcome-change events: %d, want 1 (from done→duplicate, "+
				"and passing the same outcome with a target writes no event)", n)
		}
		if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
			t.Fatalf("duplicates edges: %d, want 1", n)
		}
		if n := countDependencyCreatedEvents(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 1 {
			t.Fatalf("dependency.created events: %d, want 1", n)
		}
	})
}

func strPtr(s string) *string { return &s }

// TestPatchCloseOutcome_TargetCells — row 14: the target cells on the edit
// writer. The same seven as in row 9, with one specially named pair:
// an invisible target and a nonexistent one are TWO cells and TWO DIFFERENT errors.
func TestPatchCloseOutcome_TargetCells(t *testing.T) {
	env := newOutcomeEnv(t)

	t.Run("cross-project visible target for a non-admin: exactly one edge, a repeat does not double it", func(t *testing.T) {
		otherPID, otherPrefix := seedProject(t, env.pool, env.author)
		target, err := env.store.CreateIssue(t.Context(), otherPID, otherPrefix, "counter",
			CreateIssueInput{Title: "cross-project target", Type: "task", Priority: 2},
			env.author, env.actor)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		iss := closedDoneIssue(t, env)
		v := readCloseStamps(t, env.pool, iss.ID)
		state, err := patchOutcomeFull(t, env, iss.ID, PatchIssueInput{
			ExpectedVersion:    v.Version,
			CloseOutcome:       strPtr(CloseOutcomeSuperseded),
			CloseTarget:        target.ID,
			DependenciesCaller: &env.caller,
		}, env.actor)
		if err != nil {
			t.Fatalf("PatchIssue with a cross-project target: %v", err)
		}
		if state != CloseLinkCreated {
			t.Fatalf("link flag %q, want created", state)
		}
		// Direction: the target SUPERSEDES the closing issue.
		if n := countEdges(t, env.pool, target.ID, iss.ID, dependencies.DepSupersedes); n != 1 {
			t.Fatalf("supersedes edges %s→%s: %d, want 1", target.ID, iss.ID, n)
		}
		if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepSupersedes); n != 0 {
			t.Fatalf("an edge of the reverse direction was created")
		}

		v2 := readCloseStamps(t, env.pool, iss.ID)
		state, err = patchOutcomeFull(t, env, iss.ID, PatchIssueInput{
			ExpectedVersion:    v2.Version,
			CloseOutcome:       strPtr(CloseOutcomeSuperseded),
			CloseTarget:        target.ID,
			DependenciesCaller: &env.caller,
		}, env.actor)
		if err != nil {
			t.Fatalf("repeated edit with the same target: %v", err)
		}
		if state != CloseLinkExisted {
			t.Fatalf("link flag on the repeat %q, want existed", state)
		}
		if n := countEdges(t, env.pool, target.ID, iss.ID, dependencies.DepSupersedes); n != 1 {
			t.Fatalf("edges after the repeat %d, want 1", n)
		}
	})

	t.Run("target refusals", func(t *testing.T) {
		stranger := seedUser(t, env.pool)
		strangerActor := HumanActor(stranger, readUsername(t, env.pool, stranger))
		hiddenPID, hiddenPrefix := seedProject(t, env.pool, stranger)
		hidden, err := env.store.CreateIssue(t.Context(), hiddenPID, hiddenPrefix, "counter",
			CreateIssueInput{Title: "foreign target", Type: "task", Priority: 2},
			stranger, strangerActor)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		visible := env.issue(t, "visible target")

		cases := []struct {
			name    string
			input   func(iss *IssueDetail, version int) PatchIssueInput
			wantIs  error
			wantVal string
		}{
			{
				name: "invisible target",
				input: func(iss *IssueDetail, v int) PatchIssueInput {
					return PatchIssueInput{ExpectedVersion: v,
						CloseOutcome: strPtr(CloseOutcomeDuplicate), CloseTarget: hidden.ID,
						DependenciesCaller: &env.caller}
				},
				wantIs: dependencies.ErrDependencyTargetNotVisible,
			},
			{
				name: "nonexistent target",
				input: func(iss *IssueDetail, v int) PatchIssueInput {
					return PatchIssueInput{ExpectedVersion: v,
						CloseOutcome: strPtr(CloseOutcomeDuplicate), CloseTarget: env.prefix + "-999999",
						DependenciesCaller: &env.caller}
				},
				wantIs: dependencies.ErrTargetNotFound,
			},
			{
				name: "target without an explicit outcome",
				input: func(iss *IssueDetail, v int) PatchIssueInput {
					return PatchIssueInput{ExpectedVersion: v,
						CloseTarget: visible.ID, DependenciesCaller: &env.caller}
				},
				wantVal: "close_target_requires_outcome",
			},
			{
				name: "target with done",
				input: func(iss *IssueDetail, v int) PatchIssueInput {
					return PatchIssueInput{ExpectedVersion: v,
						CloseOutcome: strPtr(CloseOutcomeDone), CloseTarget: visible.ID,
						DependenciesCaller: &env.caller}
				},
				wantVal: "close_target_not_allowed",
			},
			{
				name: "target with wont_do",
				input: func(iss *IssueDetail, v int) PatchIssueInput {
					return PatchIssueInput{ExpectedVersion: v,
						CloseOutcome: strPtr(CloseOutcomeWontDo), CloseTarget: visible.ID,
						DependenciesCaller: &env.caller}
				},
				wantVal: "close_target_not_allowed",
			},
			{
				name: "target — the issue itself",
				input: func(iss *IssueDetail, v int) PatchIssueInput {
					return PatchIssueInput{ExpectedVersion: v,
						CloseOutcome: strPtr(CloseOutcomeDuplicate), CloseTarget: iss.ID,
						DependenciesCaller: &env.caller}
				},
				wantVal: "close_target_self",
			},
		}

		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				iss := closedDoneIssue(t, env)
				before := readCloseStamps(t, env.pool, iss.ID)
				_, err := patchOutcomeFull(t, env, iss.ID, c.input(iss, before.Version), env.actor)
				if c.wantIs != nil {
					if !errors.Is(err, c.wantIs) {
						t.Fatalf("want %v, got %v", c.wantIs, err)
					}
				} else {
					var verr *ValidationError
					if !errors.As(err, &verr) || verr.Code != c.wantVal {
						t.Fatalf("want %s, got %v", c.wantVal, err)
					}
				}
				after := readCloseStamps(t, env.pool, iss.ID)
				if after.Version != before.Version {
					t.Fatalf("version changed on refusal: %d → %d", before.Version, after.Version)
				}
				if after.Outcome == nil || *after.Outcome != CloseOutcomeDone {
					t.Fatalf("outcome changed on refusal: %v", after.Outcome)
				}
				for _, pair := range [][2]string{
					{iss.ID, hidden.ID}, {iss.ID, visible.ID}, {visible.ID, iss.ID},
				} {
					for _, depType := range []string{dependencies.DepDuplicates, dependencies.DepSupersedes} {
						if n := countEdges(t, env.pool, pair[0], pair[1], depType); n != 0 {
							t.Fatalf("an edge %s %s→%s was created on refusal", depType, pair[0], pair[1])
						}
					}
				}
			})
		}
	})

	t.Run("version conflict and a foreign lock — like any field edit", func(t *testing.T) {
		target := env.issue(t, "target for conflicts")

		t.Run("version conflict", func(t *testing.T) {
			iss := closedDoneIssue(t, env)
			before := readCloseStamps(t, env.pool, iss.ID)
			_, err := patchOutcomeFull(t, env, iss.ID, PatchIssueInput{
				ExpectedVersion:    before.Version + 7,
				CloseOutcome:       strPtr(CloseOutcomeDuplicate),
				CloseTarget:        target.ID,
				DependenciesCaller: &env.caller,
			}, env.actor)
			var vc *ErrVersionConflict
			if !errors.As(err, &vc) {
				t.Fatalf("want a version conflict, got %v", err)
			}
			after := readCloseStamps(t, env.pool, iss.ID)
			if after.Version != before.Version || *after.Outcome != CloseOutcomeDone {
				t.Fatalf("the version conflict changed the row: %+v", after)
			}
			if n := countEdges(t, env.pool, iss.ID, target.ID, dependencies.DepDuplicates); n != 0 {
				t.Fatalf("an edge was created on a version conflict — atomicity violated")
			}
		})

		t.Run("foreign lock", func(t *testing.T) {
			iss := closedDoneIssue(t, env)
			holder := seedUser(t, env.pool)
			if _, err := env.pool.Exec(t.Context(), `
UPDATE issues SET locked_by = 'sess-other', locked_at = now(), locked_by_user_id = $2
 WHERE id = $1`, iss.ID, holder); err != nil {
				t.Fatalf("setting a foreign lock: %v", err)
			}
			before := readCloseStamps(t, env.pool, iss.ID)
			_, err := patchOutcomeFull(t, env, iss.ID, PatchIssueInput{
				ExpectedVersion: before.Version,
				SessionID:       "sess-mine",
				CloseOutcome:    strPtr(CloseOutcomeWontDo),
			}, env.actor)
			var locked *ErrLockedByOther
			if !errors.As(err, &locked) {
				t.Fatalf("want ErrLockedByOther, got %v", err)
			}
			after := readCloseStamps(t, env.pool, iss.ID)
			if after.Version != before.Version || *after.Outcome != CloseOutcomeDone {
				t.Fatalf("the foreign lock did not stop the edit: %+v", after)
			}
		})
	})
}

// TestPatchCloseOutcome_RaceWithReopen — matrix row 16: a spec edge case
// "an outcome edit and a reopen at the same time".
//
// Two branches of the edge case, and they differ:
//
//   - the SAME expectedVersion on both operations — exactly one is applied,
//     the other gets a version conflict;
//   - the reopen RE-READS the version before the call (as
//     handler/bulk.go#Bulk does: GetIssue → UpdateStatus) — then both can be
//     applied, but only in the order "edit, then reopen".
//
// Forbidden outcomes in both branches: a successful edit without a change event and
// a change event on an edit that applied to an ALREADY OPEN issue. The second is
// especially treacherous: trigger 000050 would zero such an outcome silently, and
// the masking would be introduced by the feature itself.
func TestPatchCloseOutcome_RaceWithReopen(t *testing.T) {
	t.Run("the same expectedVersion — exactly one is applied", func(t *testing.T) {
		env := newOutcomeEnv(t)
		for range 5 {
			iss := closedDoneIssue(t, env)
			start := readCloseStamps(t, env.pool, iss.ID)

			var (
				wg        sync.WaitGroup
				patchErr  atomic.Value
				reopenErr atomic.Value
				patchOK   atomic.Bool
				reopenOK  atomic.Bool
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := env.store.PatchIssue(context.Background(), iss.ID, PatchIssueInput{
					ExpectedVersion: start.Version,
					CloseOutcome:    strPtr(CloseOutcomeWontDo),
				}, env.actor)
				if err != nil {
					patchErr.Store(err)
					return
				}
				patchOK.Store(true)
			}()
			go func() {
				defer wg.Done()
				_, err := env.store.UpdateStatus(context.Background(), iss.ID, UpdateStatusInput{
					ExpectedVersion: start.Version, Status: StatusOpen,
				}, env.actor)
				if err != nil {
					reopenErr.Store(err)
					return
				}
				reopenOK.Store(true)
			}()
			wg.Wait()

			if patchOK.Load() && reopenOK.Load() {
				t.Fatalf("both operations were applied to the same version %d", start.Version)
			}
			if !patchOK.Load() && !reopenOK.Load() {
				t.Fatalf("neither was applied: edit %v, reopen %v",
					patchErr.Load(), reopenErr.Load())
			}
			for _, v := range []any{patchErr.Load(), reopenErr.Load()} {
				if v == nil {
					continue
				}
				var vc *ErrVersionConflict
				if !errors.As(v.(error), &vc) {
					t.Fatalf("the losing operation returned %v, want a version conflict", v)
				}
			}
			assertOutcomeInvariantAndEvents(t, env, iss.ID)
		}
	})

	t.Run("reopen with a re-read version", func(t *testing.T) {
		env := newOutcomeEnv(t)
		for range 5 {
			iss := closedDoneIssue(t, env)
			start := readCloseStamps(t, env.pool, iss.ID)

			var (
				wg       sync.WaitGroup
				patchErr atomic.Value
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := env.store.PatchIssue(context.Background(), iss.ID, PatchIssueInput{
					ExpectedVersion: start.Version,
					CloseOutcome:    strPtr(CloseOutcomeWontDo),
				}, env.actor)
				if err != nil {
					patchErr.Store(err)
				}
			}()
			go func() {
				defer wg.Done()
				// As handler/bulk.go#Bulk: re-read the version, then
				// apply the transition.
				for range 20 {
					dto, err := env.store.GetIssue(context.Background(), iss.ID)
					if err != nil {
						return
					}
					if dto.Status != StatusClosed {
						return
					}
					_, err = env.store.UpdateStatus(context.Background(), iss.ID, UpdateStatusInput{
						ExpectedVersion: dto.Version, Status: StatusOpen,
					}, env.actor)
					if err == nil {
						return
					}
					var vc *ErrVersionConflict
					if !errors.As(err, &vc) {
						return
					}
				}
			}()
			wg.Wait()

			if v := patchErr.Load(); v != nil {
				var vc *ErrVersionConflict
				var verr *ValidationError
				err := v.(error)
				if !errors.As(err, &vc) &&
					!(errors.As(err, &verr) && verr.Code == "close_outcome_requires_closed") {
					t.Fatalf("the edit returned %v — want a version conflict or "+
						"close_outcome_requires_closed", err)
				}
			}
			assertOutcomeInvariantAndEvents(t, env, iss.ID)
		}
	})
}

// assertOutcomeInvariantAndEvents — INV-1 on the final state plus two
// forbidden race outcomes.
func assertOutcomeInvariantAndEvents(t *testing.T, env *outcomeEnv, issueID string) {
	t.Helper()
	final := readCloseStamps(t, env.pool, issueID)
	if (final.Status == StatusClosed) != (final.Outcome != nil) {
		t.Fatalf("INV-1 violated: status %q, outcome %v", final.Status, final.Outcome)
	}
	events := readIssueEvents(t, env.pool, issueID, EventIssueOutcomeChanged)
	if final.Status == StatusClosed {
		// A closed issue with a changed outcome must have exactly one change
		// event; a successful edit without an event is a forbidden outcome.
		if final.Outcome != nil && *final.Outcome == CloseOutcomeWontDo && len(events) != 1 {
			t.Fatalf("the outcome changed to wont_do, and the change events are %d", len(events))
		}
		return
	}
	// The issue is open. Exactly one change event is acceptable — if the edit
	// managed to apply to the CLOSED issue before the reopen.
	if len(events) > 1 {
		t.Fatalf("an open issue has %d outcome-change events", len(events))
	}
	if len(events) == 0 {
		return
	}
	// The discriminator of the forbidden outcome of row 16 — "an event on an edit
	// that applied to an OPEN issue". Counting events is useless for it:
	// there are EXACTLY ONE, as many as in the legal
	// order "edit to the closed one, then reopen" — so the condition
	// "more than one" above is false on the forbidden outcome and catches nothing.
	//
	// The observable difference between the legal and the forbidden is the ORDER of writes:
	// the legal edit landed BEFORE the reopen, the forbidden one — after. Events
	// are ordered by id, the issue's status is now unclosed, so the
	// last issue.status_changed is the reopen. The outcome of such an
	// edit would be silently zeroed by trigger 000050 — the masking would be introduced by the
	// feature itself, and without this comparison it would stay unobservable.
	statuses := readIssueEvents(t, env.pool, issueID, EventIssueStatusChanged)
	if len(statuses) == 0 {
		t.Fatalf("the issue is not closed, and there are no status-change events at all")
	}
	reopen := statuses[len(statuses)-1]
	if events[0].ID > reopen.ID {
		t.Fatalf("the outcome-change event (id %d) landed AFTER the reopen (id %d): "+
			"the edit applied to an OPEN issue, and its outcome was silently zeroed by the trigger",
			events[0].ID, reopen.ID)
	}
}
