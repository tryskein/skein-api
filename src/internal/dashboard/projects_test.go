// Package dashboard — sk-183.1 store-level tests for Projects(). Reuses
// the seed helpers from store_test.go (same package, same TEST_DATABASE_URL
// gate). Per-project aggregation is naturally isolated across parallel
// test runs (each test mints its own fresh project uuid), unlike the
// cross-project Scoring aggregates which need the before/after delta
// pattern — so these tests assert exact counts directly.
package dashboard

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestParseProjectsWindow covers the parser's happy path + rejection,
// and the IsAll/Duration helpers — no DB needed.
func TestParseProjectsWindow(t *testing.T) {
	for _, tc := range []struct {
		in     string
		wantOK bool
		want   ProjectsWindow
	}{
		{"24h", true, ProjectsWindow24h},
		{"7d", true, ProjectsWindow7d},
		{"30d", true, ProjectsWindow30d},
		{"all", true, ProjectsWindowAll},
		{"", false, ""},
		{"90d", false, ""},
		{"all-time", false, ""},
	} {
		got, ok := ParseProjectsWindow(tc.in)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("ParseProjectsWindow(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}

	if !ProjectsWindowAll.IsAll() {
		t.Error("ProjectsWindowAll.IsAll() = false, want true")
	}
	if ProjectsWindow24h.IsAll() {
		t.Error("ProjectsWindow24h.IsAll() = true, want false")
	}
	if got := ProjectsWindow24h.Duration(); got != 24*time.Hour {
		t.Errorf("ProjectsWindow24h.Duration() = %v, want 24h", got)
	}
	if got := ProjectsWindow7d.Duration(); got != 7*24*time.Hour {
		t.Errorf("ProjectsWindow7d.Duration() = %v, want 7d", got)
	}
	if got := ProjectsWindow30d.Duration(); got != 30*24*time.Hour {
		t.Errorf("ProjectsWindow30d.Duration() = %v, want 30d", got)
	}
}

// TestProjects_PerProjectAggregation seeds one project with a rich mix
// of events, issues, and a commit, and asserts every metric on the
// returned row — including the countSum vs countDistinctContributors
// split (activeAgents) and the topActor pick.
func TestProjects_PerProjectAggregation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, _ := seedUser(t, pool)
	actorA, actorAUname := seedUser(t, pool)
	actorB, actorBUname := seedUser(t, pool)
	agentUser, agentUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "proj")
	issueA := pre + "-a"
	issueB := pre + "-b"
	seedIssueRow(t, pool, project, issueA, "human", actorA, nil)
	seedIssueRow(t, pool, project, issueB, "human", actorB, nil)

	// events metric: actorA generates 3 field_changed + 1 created + 2
	// closed(both issues) = 6; actorB generates 1 field_changed + 1
	// created = 2; the agent generates 3 field_changed split across TWO
	// MCP sessions. The 3-vs-2-vs-1 spread is deliberate: activeAgents
	// must come out 2 (sessions), which is neither 3 (the agent's event
	// count) nor 1 (the number of PEOPLE behind those sessions — what
	// this metric wrongly counted before sk-214).
	for range 3 {
		seedEventForIssue(t, pool, "issue.field_changed", issueA, project, actorAUname, actorA, "human", now.Add(-10*time.Minute))
	}
	seedEventForIssue(t, pool, "issue.field_changed", issueB, project, actorBUname, actorB, "human", now.Add(-10*time.Minute))
	seedEventForIssue(t, pool, "issue.created", issueA, project, actorAUname, actorA, "human", now.Add(-15*time.Minute))
	seedEventForIssue(t, pool, "issue.created", issueB, project, actorBUname, actorB, "human", now.Add(-15*time.Minute))
	seedEventForIssue(t, pool, "issue.closed", issueA, project, actorAUname, actorA, "human", now.Add(-5*time.Minute))
	seedEventForIssue(t, pool, "issue.closed", issueB, project, actorAUname, actorA, "human", now.Add(-5*time.Minute))
	agentSession1 := pre + "-sess-1"
	agentSession2 := pre + "-sess-2"
	seedAgentEventWithSession(t, pool, "issue.field_changed", issueA, project, agentUname, agentUser, agentSession1, now.Add(-2*time.Minute))
	seedAgentEventWithSession(t, pool, "issue.field_changed", issueB, project, agentUname, agentUser, agentSession1, now.Add(-2*time.Minute))
	seedAgentEventWithSession(t, pool, "issue.field_changed", issueB, project, agentUname, agentUser, agentSession2, now.Add(-2*time.Minute))

	seedIssueCommit(t, pool, issueA, project, actorA, pre+"-hash1", now.Add(-1*time.Minute))

	res, err := store.Projects(ctx, ProjectsWindow24h, nil, true /* unrestricted: no redaction */, nil)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}

	var row *ProjectActivityRow
	for i := range res.Rows {
		if res.Rows[i].ProjectID != nil && *res.Rows[i].ProjectID == project {
			row = &res.Rows[i]
		}
	}
	if row == nil {
		t.Fatalf("project %s missing from Rows (got %d rows)", project, len(res.Rows))
	}
	if row.Redacted {
		t.Error("admin caller: row must not be redacted")
	}
	if row.ProjectID == nil || *row.ProjectID != project {
		t.Errorf("row.ProjectID = %v, want %s", row.ProjectID, project)
	}

	if row.Events.Count != 11 {
		t.Errorf("Events.Count = %d, want 11", row.Events.Count)
	}
	if row.IssuesOpened.Count != 2 {
		t.Errorf("IssuesOpened.Count = %d, want 2", row.IssuesOpened.Count)
	}
	if row.IssuesClosed.Count != 2 {
		t.Errorf("IssuesClosed.Count = %d, want 2", row.IssuesClosed.Count)
	}
	if row.Commits.Count != 1 {
		t.Errorf("Commits.Count = %d, want 1", row.Commits.Count)
	}
	// activeAgents.Count is the DISTINCT SESSION count (2) — not the
	// agent's event count (3) and not the number of people behind them
	// (1, which is all this metric could ever report before sk-214).
	if row.ActiveAgents.Count != 2 {
		t.Errorf("ActiveAgents.Count = %d, want 2 (distinct sessions, not 3 events and not 1 person)", row.ActiveAgents.Count)
	}
	if len(row.ActiveAgents.Contributors) != 1 || row.ActiveAgents.Contributors[0].Username != agentUname {
		t.Fatalf("ActiveAgents.Contributors = %+v, want one entry for %q", row.ActiveAgents.Contributors, agentUname)
	}
	// Contributors stay per-PERSON — the cell's hover answers "whose
	// agents", so the count there is that user's session count.
	if row.ActiveAgents.Contributors[0].Count != 2 {
		t.Errorf("ActiveAgents.Contributors[0].Count = %d, want 2 (that user's session count)", row.ActiveAgents.Contributors[0].Count)
	}

	if row.TopActor == nil || row.TopActor.Username != actorAUname {
		t.Fatalf("TopActor = %+v, want %q (highest events contributor)", row.TopActor, actorAUname)
	}
	if row.TopActor.Count != 6 {
		t.Errorf("TopActor.Count = %d, want 6", row.TopActor.Count)
	}

	// Commits contributor breakdown carries the real user, not the git
	// author string.
	if len(row.Commits.Contributors) != 1 || row.Commits.Contributors[0].Username != actorAUname {
		t.Fatalf("Commits.Contributors = %+v, want one entry for %q", row.Commits.Contributors, actorAUname)
	}
}

// TestProjects_ActiveAgentsSessionDedupeAcrossProjects pins the one
// invariant that makes activeAgents different from every other metric on
// this surface: the All row cannot be the sum of the project rows.
//
// One agent session works in TWO projects. Each project row must count
// it (1 + 1), but the All row must count the SESSION once — summing the
// rows there would report 2 agents where only one ever connected.
func TestProjects_ActiveAgentsSessionDedupeAcrossProjects(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, _ := seedUser(t, pool)
	agentUser, agentUname := seedUser(t, pool)
	projectA := seedProject(t, pool, owner)
	projectB := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "xproj")
	issueA := pre + "-a"
	issueB := pre + "-b"
	seedIssueRow(t, pool, projectA, issueA, "human", owner, nil)
	seedIssueRow(t, pool, projectB, issueB, "human", owner, nil)

	// ONE session, one event in each project.
	session := pre + "-sess"
	seedAgentEventWithSession(t, pool, "issue.field_changed", issueA, projectA, agentUname, agentUser, session, now.Add(-2*time.Minute))
	seedAgentEventWithSession(t, pool, "issue.field_changed", issueB, projectB, agentUname, agentUser, session, now.Add(-2*time.Minute))

	res, err := store.Projects(ctx, ProjectsWindow24h, nil, true /* unrestricted */, nil)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}

	find := func(id uuid.UUID) *ProjectActivityRow {
		for i := range res.Rows {
			if res.Rows[i].ProjectID != nil && *res.Rows[i].ProjectID == id {
				return &res.Rows[i]
			}
		}
		t.Fatalf("project %s missing from Rows", id)
		return nil
	}

	if got := find(projectA).ActiveAgents.Count; got != 1 {
		t.Errorf("projectA ActiveAgents.Count = %d, want 1", got)
	}
	if got := find(projectB).ActiveAgents.Count; got != 1 {
		t.Errorf("projectB ActiveAgents.Count = %d, want 1", got)
	}

	// The All row is org-wide and its contributor list is capped at
	// topContributorsCap, so this agent is not guaranteed to be visible
	// in it against a shared test database — the global dedupe itself is
	// pinned deterministically by TestCountAgentSessionsGlobal_Dedupes
	// instead of asserted here.
}

// TestCountAgentSessionsGlobal_Dedupes pins the All-row aggregation as a
// pure function: no DB, so no interference from rows other tests seeded
// and no topContributorsCap truncation hiding the assertion.
//
// Same shape as the DB test above: one session spanning two projects
// must count ONCE globally, while a second session in one of them adds
// one. Summing the per-project rows would give 3.
func TestCountAgentSessionsGlobal_Dedupes(t *testing.T) {
	userID := uuid.Must(uuid.NewRandom())
	projectA := uuid.Must(uuid.NewRandom())
	projectB := uuid.Must(uuid.NewRandom())

	rows := []projectAgentSessionRow{
		{ProjectID: projectA, UserID: &userID, Name: "agentzilla", SessionID: "sess-1"},
		{ProjectID: projectB, UserID: &userID, Name: "agentzilla", SessionID: "sess-1"},
		{ProjectID: projectA, UserID: &userID, Name: "agentzilla", SessionID: "sess-2"},
	}

	global := countAgentSessionsGlobal(rows)
	if len(global) != 1 {
		t.Fatalf("countAgentSessionsGlobal = %+v, want a single contributor", global)
	}
	if global[0].Count != 2 {
		t.Errorf("global session count = %d, want 2 (sess-1 counted once across both projects, + sess-2)", global[0].Count)
	}

	perProject := countAgentSessionsPerProject(rows)
	byProject := make(map[uuid.UUID]int64)
	for _, r := range perProject {
		byProject[r.ProjectID] += r.Count
	}
	if byProject[projectA] != 2 {
		t.Errorf("projectA sessions = %d, want 2", byProject[projectA])
	}
	if byProject[projectB] != 1 {
		t.Errorf("projectB sessions = %d, want 1", byProject[projectB])
	}
	// The invariant in one line: the rows over-count what All reports.
	if byProject[projectA]+byProject[projectB] <= global[0].Count {
		t.Errorf("per-project sum %d must exceed the deduped global %d for this fixture",
			byProject[projectA]+byProject[projectB], global[0].Count)
	}
}

// TestProjects_MoreThanCapContributors seeds topContributorsCap+3
// distinct actors generating exactly one event each, and asserts the
// contributors list is capped and OtherContributorsCount reports the
// overflow.
func TestProjects_MoreThanCapContributors(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, _ := seedUser(t, pool)
	project := seedProject(t, pool, owner)
	pre := uniqueIDPrefix(t, "cap")
	issueID := pre + "-0"
	seedIssueRow(t, pool, project, issueID, "human", owner, nil)

	now := time.Now().UTC()
	total := topContributorsCap + 3
	for i := range total {
		actor, actorUname := seedUser(t, pool)
		seedEventForIssue(t, pool, "issue.field_changed", issueID, project, actorUname, actor, "human", now.Add(-time.Duration(i+1)*time.Minute))
	}

	res, err := store.Projects(ctx, ProjectsWindow24h, nil, true, nil)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	var row *ProjectActivityRow
	for i := range res.Rows {
		if res.Rows[i].ProjectID != nil && *res.Rows[i].ProjectID == project {
			row = &res.Rows[i]
		}
	}
	if row == nil {
		t.Fatalf("project %s missing from Rows", project)
	}
	if len(row.Events.Contributors) != topContributorsCap {
		t.Errorf("len(Events.Contributors) = %d, want %d (capped)", len(row.Events.Contributors), topContributorsCap)
	}
	if row.Events.OtherContributorsCount != 3 {
		t.Errorf("Events.OtherContributorsCount = %d, want 3", row.Events.OtherContributorsCount)
	}
	if row.Events.Count != int64(total) {
		t.Errorf("Events.Count = %d, want %d (sum survives the cap)", row.Events.Count, total)
	}
}

// TestProjects_Redaction verifies the project-identity-only redaction
// rule directly at the store layer: a non-admin caller with the project
// absent from visibleProjects gets projectId/projectName/projectPrefix
// stripped and redacted=true, but every metric and contributor username
// stays exactly as an admin would see it.
func TestProjects_Redaction(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)
	pre := uniqueIDPrefix(t, "redact")
	issueID := pre + "-0"
	seedIssueRow(t, pool, project, issueID, "human", owner, nil)
	now := time.Now().UTC()
	seedEventForIssue(t, pool, "issue.created", issueID, project, ownerUname, owner, "human", now.Add(-5*time.Minute))

	adminRes, err := store.Projects(ctx, ProjectsWindow24h, nil, true, nil)
	if err != nil {
		t.Fatalf("Projects (admin): %v", err)
	}
	var adminRow *ProjectActivityRow
	for i := range adminRes.Rows {
		if adminRes.Rows[i].ProjectID != nil && *adminRes.Rows[i].ProjectID == project {
			adminRow = &adminRes.Rows[i]
		}
	}
	if adminRow == nil {
		t.Fatalf("admin: project %s missing from Rows", project)
	}

	// Non-admin caller, empty visible set (not a member of `project`).
	// The project is in the caller's reach without being visible (sk-674.18
	// T030: a project of his own space) — its row comes anonymized.
	nonMemberRes, err := store.Projects(ctx, ProjectsWindow24h, map[uuid.UUID]struct{}{}, false,
		map[uuid.UUID]struct{}{project: {}})
	if err != nil {
		t.Fatalf("Projects (non-member): %v", err)
	}
	var hidden *ProjectActivityRow
	for i := range nonMemberRes.Rows {
		// The row's identity is stripped, so find it by its (still
		// populated) contributor username instead of by project id.
		for _, c := range nonMemberRes.Rows[i].Events.Contributors {
			if c.Username == ownerUname {
				hidden = &nonMemberRes.Rows[i]
			}
		}
	}
	if hidden == nil {
		t.Fatalf("non-member: row with contributor %q missing from Rows", ownerUname)
	}
	if !hidden.Redacted {
		t.Error("non-member row must have Redacted=true")
	}
	if hidden.ProjectID != nil {
		t.Errorf("non-member row ProjectID = %v, want nil", hidden.ProjectID)
	}
	if hidden.ProjectName != "" || hidden.ProjectPrefix != "" {
		t.Errorf("non-member row identity not stripped: name=%q prefix=%q", hidden.ProjectName, hidden.ProjectPrefix)
	}
	if hidden.Events.Count != adminRow.Events.Count {
		t.Errorf("non-member Events.Count = %d, want unredacted %d", hidden.Events.Count, adminRow.Events.Count)
	}
	if len(hidden.Events.Contributors) != len(adminRow.Events.Contributors) {
		t.Errorf("non-member Events.Contributors len = %d, want %d", len(hidden.Events.Contributors), len(adminRow.Events.Contributors))
	}

	// Member caller (project IS in the visible set): row must NOT be
	// redacted.
	visibleRes, err := store.Projects(ctx, ProjectsWindow24h, map[uuid.UUID]struct{}{project: {}}, false, nil)
	if err != nil {
		t.Fatalf("Projects (member): %v", err)
	}
	var visibleRow *ProjectActivityRow
	for i := range visibleRes.Rows {
		if visibleRes.Rows[i].ProjectID != nil && *visibleRes.Rows[i].ProjectID == project {
			visibleRow = &visibleRes.Rows[i]
		}
	}
	if visibleRow == nil {
		t.Fatalf("member: project %s missing from Rows", project)
	}
	if visibleRow.Redacted {
		t.Error("member row must not be redacted")
	}
}

// TestProjects_AllCountsTheReach asserts the All summary row counts exactly
// the caller's HOT ZONE reach (sk-674.18 T030, US-13): the same reach gives
// the same All whether its project is visible to the caller or only
// contributes (INV-28 (1)); a project outside the reach adds nothing
// (INV-23); the row carries no project-identity fields. (It replaces
// TestProjects_AllIsGlobalAndCallerIndependent: a global, caller-independent
// All is what US-13 retires.)
func TestProjects_AllCountsTheReach(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)
	other := seedProject(t, pool, owner)
	pre := uniqueIDPrefix(t, "allrow")
	issueID := pre + "-0"
	seedIssueRow(t, pool, project, issueID, "human", owner, nil)
	now := time.Now().UTC()

	const nNewEvents = 4
	for i := range nNewEvents {
		seedEventForIssue(t, pool, "issue.field_changed", issueID, project, ownerUname, owner, "human", now.Add(-time.Duration(i+1)*time.Minute))
	}

	reach := map[uuid.UUID]struct{}{project: {}}
	visibleAll, err := store.Projects(ctx, ProjectsWindow24h, map[uuid.UUID]struct{}{project: {}}, false, reach)
	if err != nil {
		t.Fatalf("Projects (visible): %v", err)
	}
	contributingAll, err := store.Projects(ctx, ProjectsWindow24h, map[uuid.UUID]struct{}{}, false, reach)
	if err != nil {
		t.Fatalf("Projects (contributing): %v", err)
	}
	outside, err := store.Projects(ctx, ProjectsWindow24h, map[uuid.UUID]struct{}{}, false,
		map[uuid.UUID]struct{}{other: {}})
	if err != nil {
		t.Fatalf("Projects (outside): %v", err)
	}

	if got := visibleAll.All.Events.Count; got != nNewEvents {
		t.Errorf("All.Events.Count over the reach = %d, want %d (the reach's events only)", got, nNewEvents)
	}
	if visibleAll.All.Events.Count != contributingAll.All.Events.Count {
		t.Errorf("All.Events.Count differs by visibility within one reach: visible=%d contributing=%d",
			visibleAll.All.Events.Count, contributingAll.All.Events.Count)
	}
	if got := outside.All.Events.Count; got != 0 {
		t.Errorf("All.Events.Count with the project outside the reach = %d, want 0", got)
	}
	if visibleAll.All.Redacted {
		t.Error("All row must never be redacted")
	}
	if visibleAll.All.ProjectID != nil || visibleAll.All.ProjectName != "" || visibleAll.All.ProjectPrefix != "" {
		t.Errorf("All row must carry no project-identity fields, got id=%v name=%q prefix=%q",
			visibleAll.All.ProjectID, visibleAll.All.ProjectName, visibleAll.All.ProjectPrefix)
	}
}
