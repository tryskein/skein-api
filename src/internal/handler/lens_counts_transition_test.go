package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

// This file is the S2 (sk-105.3) transition-reassert coverage for the
// cross-project "On me" / "Delegated" lens badges driven by
// MeHandler.countLensBuckets (me.go ~495-530). The pre-existing
// me_test.go snapshots the lens in fixed states; here we take ONE issue
// through the real lifecycle (assign → accept → status transitions →
// delegate → close) and RE-ASSERT the badge after every step. The point
// is leak detection: a snapshot can pass while a transition silently
// fails to move a row between buckets.
//
// All asserts read the badge for the seeded caller only; the lens query
// is inherently caller-scoped (assignee=me / created_by=me) and the
// fixtures live in a fresh per-test project, so concurrent packages can
// never perturb the counts.

// lensCounts calls the NotificationCounts handler and returns the two
// lens badge totals for the caller. Reuses the me_test.go callCounts
// plumbing (same package).
func lensCounts(t *testing.T, h *MeHandler, userID uuid.UUID) (onMe, delegated int) {
	t.Helper()
	res, body := callCounts(t, h, userID)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("notification-counts status = %d, want 200; body=%v", res.StatusCode, body)
	}
	return mustInt(t, body, "onMeOpenCount"), mustInt(t, body, "delegatedOpenCount")
}

// TestLensBuckets_OnMe_FullLifecycle drives a single issue through the
// "On me" lens lifecycle and re-asserts the badge at each step:
//
//	seed open, assigned-to-me by other, UNACCEPTED → On me = 0 (Inbox, not On me)
//	accept                                          → On me = 1
//	open → in_progress                              → On me = 1 (non-closed)
//	in_progress → deferred                          → On me = 1 (non-closed)
//	deferred → closed                               → On me = 0 (closed excluded)
//
// Delegated must stay 0 throughout — the issue is assigned to the caller
// (self-plate), not delegated away.
func TestLensBuckets_OnMe_FullLifecycle(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	other, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsonme")
	h := NewMeHandler(pool, nil, nil)

	id := prefix + "-1"
	// other → caller, open, NOT yet accepted.
	testsupport.SeedIssue(t, pool, pid, other, id, "on-me lifecycle", "open", 2)
	testsupport.SetAssignee(t, pool, id, callerName)

	onMe, deleg := lensCounts(t, h, caller)
	if onMe != 0 {
		t.Fatalf("On me before accept = %d, want 0 (unaccepted = Inbox, not On me)", onMe)
	}
	if deleg != 0 {
		t.Fatalf("Delegated before accept = %d, want 0", deleg)
	}

	// Accept → enters On me.
	testsupport.Accept(t, pool, id, caller, true)
	onMe, deleg = lensCounts(t, h, caller)
	if onMe != 1 {
		t.Fatalf("On me after accept = %d, want 1", onMe)
	}
	if deleg != 0 {
		t.Fatalf("Delegated after accept = %d, want 0", deleg)
	}

	// open → in_progress: stays in On me (non-closed).
	testsupport.SetStatus(t, pool, id, "in_progress")
	if onMe, _ = lensCounts(t, h, caller); onMe != 1 {
		t.Fatalf("On me in_progress = %d, want 1", onMe)
	}

	// in_progress → deferred: stays in On me (non-closed).
	testsupport.SetStatus(t, pool, id, "deferred")
	if onMe, _ = lensCounts(t, h, caller); onMe != 1 {
		t.Fatalf("On me deferred = %d, want 1", onMe)
	}

	// deferred → closed: leaves On me (closed excluded).
	testsupport.SetStatus(t, pool, id, "closed")
	onMe, deleg = lensCounts(t, h, caller)
	if onMe != 0 {
		t.Fatalf("On me after close = %d, want 0", onMe)
	}
	if deleg != 0 {
		t.Fatalf("Delegated after close = %d, want 0", deleg)
	}
}

// TestLensBuckets_Delegated_FullLifecycle drives a single issue through
// the "Delegated" lens lifecycle and re-asserts after each step:
//
//	seed open, created-by-me, assigned to OTHER → Delegated = 1
//	open → in_progress                          → Delegated = 1 (non-closed)
//	in_progress → deferred                      → Delegated = 1 (non-closed)
//	re-assign back to me                        → Delegated = 0 (self-plate)
//	re-assign to other again                    → Delegated = 1
//	close                                       → Delegated = 0 (closed excluded)
//
// On me must stay 0 throughout (the caller created the issue; the
// On-me lens requires created_by <> me).
func TestLensBuckets_Delegated_FullLifecycle(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsdeleg")
	h := NewMeHandler(pool, nil, nil)

	id := prefix + "-1"
	// created by caller, delegated to other, open.
	testsupport.SeedIssue(t, pool, pid, caller, id, "delegated lifecycle", "open", 2)
	testsupport.SetAssignee(t, pool, id, otherName)

	onMe, deleg := lensCounts(t, h, caller)
	if deleg != 1 {
		t.Fatalf("Delegated open = %d, want 1", deleg)
	}
	if onMe != 0 {
		t.Fatalf("On me open = %d, want 0 (created by me)", onMe)
	}

	testsupport.SetStatus(t, pool, id, "in_progress")
	if _, deleg = lensCounts(t, h, caller); deleg != 1 {
		t.Fatalf("Delegated in_progress = %d, want 1", deleg)
	}

	testsupport.SetStatus(t, pool, id, "deferred")
	if _, deleg = lensCounts(t, h, caller); deleg != 1 {
		t.Fatalf("Delegated deferred = %d, want 1", deleg)
	}

	// Re-assign back to the caller → drops out of Delegated (now self-plate).
	testsupport.SetAssignee(t, pool, id, callerName)
	if _, deleg = lensCounts(t, h, caller); deleg != 0 {
		t.Fatalf("Delegated after re-assign-to-self = %d, want 0", deleg)
	}

	// Delegate again → back in Delegated.
	testsupport.SetAssignee(t, pool, id, otherName)
	if _, deleg = lensCounts(t, h, caller); deleg != 1 {
		t.Fatalf("Delegated after re-delegate = %d, want 1", deleg)
	}

	// Close → leaves Delegated.
	testsupport.SetStatus(t, pool, id, "closed")
	if _, deleg = lensCounts(t, h, caller); deleg != 0 {
		t.Fatalf("Delegated after close = %d, want 0", deleg)
	}
}

// TestLensBuckets_OnMe_UnassignBackToInbox verifies the Inbox/On-me edge
// the idx_issues_unaccepted gate guards: re-clearing accepted_at returns
// an On-me issue to the Inbox surface (On me badge drops). This is the
// acceptance-gate flip in reverse, asserted on the lens badge.
func TestLensBuckets_OnMe_UnacceptDropsBadge(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	callerName := testsupport.Username(t, pool, caller)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsunacc")
	h := NewMeHandler(pool, nil, nil)

	id := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, other, id, "accept toggle", "open", 2)
	testsupport.SetAssignee(t, pool, id, callerName)
	testsupport.Accept(t, pool, id, caller, true)

	if onMe, _ := lensCounts(t, h, caller); onMe != 1 {
		t.Fatalf("On me after accept = %d, want 1", onMe)
	}

	// Un-accept → back to Inbox, off the On-me badge.
	testsupport.Accept(t, pool, id, caller, false)
	if onMe, _ := lensCounts(t, h, caller); onMe != 0 {
		t.Fatalf("On me after un-accept = %d, want 0 (back in Inbox)", onMe)
	}
}

// ---------------------------------------------------------------------------
// sk-303.14, task T014, matrix row 60 — SC-014.
// ---------------------------------------------------------------------------

// TestLensBuckets_Delegated_UnaffectedByReviewMechanic — the existing
// field of the active-delegated-issue count keeps its value and meaning
// BYTE FOR BYTE when the review workflow appears.
//
// Why this file. It already was the SC-014 guard before the review flag: the
// lifecycle of the active-delegated counter is checked here. The review flag
// adds a SECOND number from a different set next to it, and the main
// risk is that a new condition leaks into the old bucket: for example
// someone "while at it" subtracts reviewed issues from delegated, or
// drops issues with the flag cleared. Both numbers would then stay
// plausible, and the old one would stop meaning what it meant.
//
// Why every step has a control assertion. A test of the form "we
// turned the mechanic knobs — delegated did not change" also passes
// when the knobs do ABSOLUTELY NOTHING. So each step checks that the
// knob actually moved the SECOND number: only then does the first
// number's stillness prove anything.
//
// Why the first number is accumulated from FOUR open issues, not one.
// One issue covers one combination of mechanic fields, and a leak of a
// condition it happens to satisfy stays unnoticed: verified by mutation
// — inserting `AND NOT review_required` into the delegated bucket
// slipped past the single-issue edition of this test, because that
// only issue already had the flag cleared. The four cover the mechanic
// state space in full (flag × mark × dismissal), and any of those
// conditions leaking into the bucket must drop at least one issue.
func TestLensBuckets_Delegated_UnaffectedByReviewMechanic(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, caller, "tsrevsc")
	h := NewMeHandler(pool, nil, nil)

	// O1..O4 — OPEN delegated issues: the entire contribution to the first
	// number. None of them is in the unreviewed set (it is made of closed
	// ones), so the second number is not moved by them — and vice versa.
	//
	// They differ EXACTLY in mechanic state, and that is the space under
	// test: flag cleared, flag on, reviewed mark, author's explicit
	// dismissal.
	openStates := []struct {
		id    string
		what  string
		setup string
		args  []any
	}{
		{prefix + "-1", "flag cleared", `review_required = false`, nil},
		{prefix + "-2", "flag on", `review_required = true`, nil},
		{prefix + "-3", "marked reviewed",
			`review_required = true, reviewed_at = now(), reviewed_by = $2`, []any{caller}},
		{prefix + "-4", "author dismissed",
			`review_required = true, dismissed_by = ARRAY[$2]::uuid[]`, []any{caller}},
	}
	for i, st := range openStates {
		testsupport.SeedIssue(t, pool, pid, caller, st.id,
			fmt.Sprintf("delegated open %d (%s)", i+1, st.what), "open", 2)
		testsupport.SetAssignee(t, pool, st.id, otherName)
		args := append([]any{st.id}, st.args...)
		if _, err := pool.Exec(t.Context(),
			`UPDATE issues SET `+st.setup+` WHERE id = $1`, args...); err != nil {
			t.Fatalf("mechanic state %q for %s: %v", st.what, st.id, err)
		}
	}

	// D2 — a CLOSED delegated issue with the flag on: the only
	// contribution to the second number. It is not in the first (closed).
	closed := prefix + "-9"
	testsupport.SeedIssue(t, pool, pid, caller, closed, "delegated closed", "open", 2)
	testsupport.SetAssignee(t, pool, closed, otherName)
	testsupport.SetStatus(t, pool, closed, "closed")
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET review_required = true, reviewed_at = NULL, reviewed_by = NULL
          WHERE id = $1`, closed); err != nil {
		t.Fatalf("turning the flag on: %v", err)
	}

	// counts returns the first number and the second (nil with the mechanic off).
	counts := func(t *testing.T) (int, *int) {
		t.Helper()
		res, body := callCounts(t, h, caller)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("notification-counts status = %d; body=%v", res.StatusCode, body)
		}
		raw, ok := body["delegatedUnreviewedCount"]
		if !ok {
			t.Fatalf("no delegatedUnreviewedCount key: %v", body)
		}
		if raw == nil {
			return mustInt(t, body, "delegatedOpenCount"), nil
		}
		f, ok := raw.(float64)
		if !ok {
			t.Fatalf("delegatedUnreviewedCount is not numeric: %v (%T)", raw, raw)
		}
		n := int(f)
		return mustInt(t, body, "delegatedOpenCount"), &n
	}

	// O1..O4, and only they: a closed issue is not in active work.
	wantDelegated := len(openStates)

	deleg, unreviewed := counts(t)
	if deleg != wantDelegated {
		t.Fatalf("baseline: Delegated = %d, want %d — open delegated "+
			"issues in every mechanic state must count the same", deleg, wantDelegated)
	}
	if unreviewed == nil || *unreviewed != 1 {
		t.Fatalf("baseline: unreviewed %v, want 1 — without this the steps below "+
			"cannot prove the mechanic knobs work at all", unreviewed)
	}

	// Each step: one mechanic knob, the expected second number, the first
	// unmoved.
	steps := []struct {
		name         string
		turn         func(t *testing.T)
		wantSecond   *int // nil = "there is no number" (mechanic off)
		whySecondSet string
	}{
		{
			name: "clearing the flag",
			turn: func(t *testing.T) {
				if _, err := pool.Exec(t.Context(),
					`UPDATE issues SET review_required = false WHERE id = $1`, closed); err != nil {
					t.Fatalf("clearing the flag: %v", err)
				}
			},
			wantSecond:   intPtr(0),
			whySecondSet: "an issue with the flag cleared leaves the set (INV-5)",
		},
		{
			name: "restoring the flag and the reviewed mark",
			turn: func(t *testing.T) {
				if _, err := pool.Exec(t.Context(),
					`UPDATE issues SET review_required = true, reviewed_at = now(), reviewed_by = $2
                      WHERE id = $1`, closed, caller); err != nil {
					t.Fatalf("reviewed mark: %v", err)
				}
			},
			wantSecond:   intPtr(0),
			whySecondSet: "a reviewed issue leaves the set (SC-002)",
		},
		{
			name: "clearing the mark",
			turn: func(t *testing.T) {
				if _, err := pool.Exec(t.Context(),
					`UPDATE issues SET reviewed_at = NULL, reviewed_by = NULL WHERE id = $1`,
					closed); err != nil {
					t.Fatalf("clearing the mark: %v", err)
				}
			},
			wantSecond:   intPtr(1),
			whySecondSet: "the issue returned to the set",
		},
		{
			name: "switching the mechanic off entirely",
			turn: func(t *testing.T) {
				if _, err := pool.Exec(t.Context(), `
INSERT INTO user_notification_prefs (user_id, review_mechanic_enabled, updated_at)
VALUES ($1, false, now())
ON CONFLICT (user_id) DO UPDATE SET review_mechanic_enabled = false`, caller); err != nil {
					t.Fatalf("switching the mechanic off: %v", err)
				}
				t.Cleanup(func() {
					_, _ = pool.Exec(context.Background(),
						`DELETE FROM user_notification_prefs WHERE user_id = $1`, caller)
				})
			},
			wantSecond:   nil,
			whySecondSet: "the switch removes the mechanic entirely (INV-6)",
		},
	}

	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			st.turn(t)
			deleg, second := counts(t)

			// Control: the knob actually fired. Without it the first
			// number's stillness proves nothing.
			switch {
			case st.wantSecond == nil && second != nil:
				t.Fatalf("step control: second number %d, want absence — %s",
					*second, st.whySecondSet)
			case st.wantSecond != nil && second == nil:
				t.Fatalf("step control: no second number, want %d — %s",
					*st.wantSecond, st.whySecondSet)
			case st.wantSecond != nil && *second != *st.wantSecond:
				t.Fatalf("step control: second number %d, want %d — %s",
					*second, *st.wantSecond, st.whySecondSet)
			}

			// And SC-014 itself.
			if deleg != wantDelegated {
				t.Errorf("SC-014: mechanic knob %q moved the active-delegated count "+
					"from %d to %d. The field must keep its previous value and meaning: "+
					"this is UNCLOSED work sitting with others, unrelated to the review "+
					"mechanic", st.name, wantDelegated, deleg)
			}
		})
	}
}
