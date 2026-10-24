package issues

import (
	"context"
	"testing"
)

// autowatch_test.go — sk-551.10, item 4: who is auto-subscribed to a NEW
// issue. Expected values are the maintainers' decision recorded on the task
// (2026-10-03), not the current output of the code:
//
//   - top-level agent_directed → the human author watches it (`creator`);
//   - agent_directed with a parent, and agent_autonomous at any level →
//     no `creator` subscription;
//   - an issue assigned to a person subscribes them as `assignee`, whoever
//     created it.

func TestCreatorAutoWatches(t *testing.T) {
	cases := []struct {
		name      string
		source    string
		hasParent bool
		want      bool
	}{
		{"human top level", SourceHuman, false, true},
		{"human child", SourceHuman, true, true},
		{"directed top level — the errand", SourceAgentDirected, false, true},
		{"directed child — a step inside the errand", SourceAgentDirected, true, false},
		{"autonomous top level", SourceAgentAutonomous, false, false},
		{"autonomous child", SourceAgentAutonomous, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := creatorAutoWatches(tc.source, tc.hasParent); got != tc.want {
				t.Errorf("creatorAutoWatches(%q, parent=%v) = %v, want %v",
					tc.source, tc.hasParent, got, tc.want)
			}
		})
	}
}

// TestCreateIssue_CreatorAutoWatchFollowsSource runs the rule through the
// real create path: an agent of the owner files a top-level errand, a
// directed child, an autonomous child and an autonomous top-level issue.
// Only the errand may subscribe the owner. The directed child is assigned
// to another person, who must be subscribed as `assignee` regardless.
func TestCreateIssue_CreatorAutoWatchFollowsSource(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	worker := seedUser(t, pool)
	workerName := usernameFor(t, pool, worker)
	pid, prefix := seedProject(t, pool, owner)
	addProjectMember(t, pool, pid, worker, owner)

	store := NewStore(pool)
	agent := agentActor(owner, ownerName)

	errand, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "errand", Type: "task", Priority: 2, Source: SourceAgentDirected},
		owner, agent)
	if err != nil {
		t.Fatalf("CreateIssue(errand): %v", err)
	}
	if !hasReason(watcherReasons(t, pool, errand.ID, owner), "creator") {
		t.Errorf("top-level agent_directed %s: owner has no creator subscription, want one", errand.ID)
	}

	directedChild, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title: "step", Type: "task", Priority: 2, Source: SourceAgentDirected,
			ParentID: &errand.ID, Assignee: ptrStr(workerName),
		},
		owner, agent)
	if err != nil {
		t.Fatalf("CreateIssue(directed child): %v", err)
	}
	if n := countWatcherRows(t, pool, directedChild.ID, owner); n != 0 {
		t.Errorf("agent_directed child %s: owner has %d subscription rows, want 0", directedChild.ID, n)
	}
	if !hasReason(watcherReasons(t, pool, directedChild.ID, worker), "assignee") {
		t.Errorf("agent_directed child %s assigned to %s: no assignee subscription", directedChild.ID, workerName)
	}

	autonomousChild, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title: "side quest", Type: "task", Priority: 2, Source: SourceAgentAutonomous,
			ParentID: &errand.ID,
		},
		owner, agent)
	if err != nil {
		t.Fatalf("CreateIssue(autonomous child): %v", err)
	}
	if n := countWatcherRows(t, pool, autonomousChild.ID, owner); n != 0 {
		t.Errorf("agent_autonomous child %s: owner has %d subscription rows, want 0", autonomousChild.ID, n)
	}

	autonomousTop, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "found it myself", Type: "task", Priority: 2, Source: SourceAgentAutonomous},
		owner, agent)
	if err != nil {
		t.Fatalf("CreateIssue(autonomous top): %v", err)
	}
	if n := countWatcherRows(t, pool, autonomousTop.ID, owner); n != 0 {
		t.Errorf("top-level agent_autonomous %s: owner has %d subscription rows, want 0", autonomousTop.ID, n)
	}

	// The owner assigning an agent-made issue to THEMSELVES still
	// subscribes them — as the assignee, which is a different reason.
	selfAssigned, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title: "mine to do", Type: "task", Priority: 2, Source: SourceAgentAutonomous,
			ParentID: &errand.ID, Assignee: ptrStr(ownerName),
		},
		owner, agent)
	if err != nil {
		t.Fatalf("CreateIssue(self-assigned): %v", err)
	}
	got := watcherReasons(t, pool, selfAssigned.ID, owner)
	if !hasReason(got, "assignee") || hasReason(got, "creator") {
		t.Errorf("self-assigned agent child %s: reasons %v, want assignee without creator", selfAssigned.ID, got)
	}
}

// TestCreateIssue_InlineParentChildIsAParentToo — review finding (sk-551.10): the hierarchy of a new issue can also come from an
// inline `dependencies:[{type:"parent-child"}]` with no ParentID. Such an
// issue IS a child, so the creator rule must treat it as one: the human
// author gets no `creator` row, the assigned person still gets `assignee`.
func TestCreateIssue_InlineParentChildIsAParentToo(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	worker := seedUser(t, pool)
	workerName := usernameFor(t, pool, worker)
	pid, prefix := seedProject(t, pool, owner)
	addProjectMember(t, pool, pid, worker, owner)

	store := NewStore(pool)
	agent := agentActor(owner, ownerName)

	errand, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "errand", Type: "task", Priority: 2, Source: SourceAgentDirected},
		owner, agent)
	if err != nil {
		t.Fatalf("CreateIssue(errand): %v", err)
	}

	child, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{
			Title: "step via inline edge", Type: "task", Priority: 2,
			Source:             SourceAgentDirected,
			Assignee:           ptrStr(workerName),
			Dependencies:       []DependencyEdgeInput{{ToID: errand.ID, Type: "parent-child"}},
			DependenciesCaller: &DependenciesCaller{UserID: owner, Role: "user"},
		},
		owner, agent)
	if err != nil {
		t.Fatalf("CreateIssue(inline child): %v", err)
	}

	var parent *string
	if err := pool.QueryRow(ctx, `
SELECT to_id FROM dependencies WHERE from_id = $1 AND type = 'parent-child'`, child.ID).Scan(&parent); err != nil {
		t.Fatalf("read parent edge of %s: %v", child.ID, err)
	}
	if parent == nil || *parent != errand.ID {
		t.Fatalf("%s is not under %s (parent %v) — the fixture does not build a child", child.ID, errand.ID, parent)
	}
	if n := countWatcherRows(t, pool, child.ID, owner); n != 0 {
		t.Errorf("agent_directed child via inline parent-child: owner has %d subscription rows (reasons %v), want 0",
			n, watcherReasons(t, pool, child.ID, owner))
	}
	if !hasReason(watcherReasons(t, pool, child.ID, worker), "assignee") {
		t.Errorf("assignee of %s has no assignee subscription", child.ID)
	}
}
