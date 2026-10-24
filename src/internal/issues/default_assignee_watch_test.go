package issues

// default_assignee_watch_test.go — sk-565, T030, test matrix row 33 (D7,
// INV-7, US-9, SC-005). Reference: spec sk-565, US-9: "right after creation
// the owner's subscription to the issue is the same as this issue would have had without
// the default assignee (author's rule sk-551.10: present for top-level directed
// issues, absent for autonomous and for children). For an explicit assignee the
// existing 'assignee is subscribed on creation' applies".
//
// Each shape is created twice by the same creator: once with the default
// actually assigning them (asserted, so the pair is not vacuous), once as a
// genuinely unassigned twin. Their subscription sets must match. The
// explicit self-assignment arm keeps the `assignee` reason — so a rule
// keyed on «assignee == creator» instead of «assigned by the setting» is red.

import (
	"slices"
	"testing"
)

// TestDefaultAssignee_SubscriptionAtCreateIsTheAuthorRule — row 33.
func TestDefaultAssignee_SubscriptionAtCreateIsTheAuthorRule(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	ownersTask := e.seedAssigned(e.alice, e.pid, e.prefix, e.aName, nil)

	shape := func(title, source string, parent *string) (substituted, twin CreateIssueInput) {
		substituted = e.mcpInput(title)
		substituted.Source = source
		substituted.ParentID = parent
		twin = substituted
		twin.Title = title + " (unassigned twin)"
		twin.AssignCreatorByDefault = false
		return substituted, twin
	}

	t.Run("(a) substituted assignee: single issues", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			source string
			parent *string
		}{
			{"directed top-level", SourceAgentDirected, nil},
			{"directed with a parent", SourceAgentDirected, &ownersTask},
			{"autonomous top-level", SourceAgentAutonomous, nil},
		} {
			sub, twin := shape(tc.name, tc.source, tc.parent)
			a := e.create(sub)
			b := e.create(twin)
			e.wantOwner(t, a.ID, e.aName, e.alice)
			e.wantUnassigned(t, b.ID)
			got := sortedReasons(t, e.plain, a.ID, e.alice)
			want := sortedReasons(t, e.plain, b.ID, e.alice)
			if !slices.Equal(got, want) {
				t.Errorf("%s: creator's reasons with the substituted assignee = %v, unassigned twin = %v — must match",
					tc.name, got, want)
			}
			// The author rule itself (sk-551.10): a top-level directed
			// issue keeps the creator subscription. Without this, losing
			// the rule on both twins would still compare equal.
			if tc.source == SourceAgentDirected && tc.parent == nil && !hasReason(got, "creator") {
				t.Errorf("%s: creator's reasons = %v, want `creator` (author rule)", tc.name, got)
			}
		}
	})

	t.Run("(a) substituted assignee: epic children", func(t *testing.T) {
		_, subs := e.mustEpic(e.mcpInput("epic"), e.mcpInput("child 1"), e.mcpInput("child 2"))
		twinEpic := e.mcpInput("twin epic")
		twinEpic.AssignCreatorByDefault = false
		twinKid := e.mcpInput("twin child")
		twinKid.AssignCreatorByDefault = false
		_, twins := e.mustEpic(twinEpic, twinKid, twinKid)
		for i := range subs {
			e.wantOwner(t, subs[i].ID, e.aName, e.alice)
			e.wantUnassigned(t, twins[i].ID)
			got := sortedReasons(t, e.plain, subs[i].ID, e.alice)
			want := sortedReasons(t, e.plain, twins[i].ID, e.alice)
			if !slices.Equal(got, want) {
				t.Errorf("epic child %d: creator's reasons with the substituted assignee = %v, unassigned twin = %v — must match",
					i, got, want)
			}
		}
	})

	t.Run("(b) explicit self-assignment keeps the assignee subscription", func(t *testing.T) {
		in := e.mcpInput("explicit self-assign")
		in.Assignee = ptrTo(e.aName)
		dto := e.create(in)
		if reasons := watcherReasons(t, e.plain, dto.ID, e.alice); !hasReason(reasons, "assignee") {
			t.Errorf("explicit self-assignment: creator's reasons = %v, want `assignee` among them", reasons)
		}
	})
}
