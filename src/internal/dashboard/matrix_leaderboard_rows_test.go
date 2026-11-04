package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-192 — a person a leaderboard of a Scoring response names must have a
// breakdown row in the SAME response (leaderboardUsers), even when more
// than 50 other actors rank above them by closed_human_driven, the axis
// matrixUsers is capped on; matrixUsers itself keeps its 50 rows.
//
// The podium ranks by closed_with_commits and reads each hero's
// created / closed / commits / comments from matrixUsers. A hero whose
// commit-backed closes are on autonomous tasks has closed_human_driven 0,
// so with 50 busier human-driven closers the hero was cut by the matrix
// cap: the podium showed the hero's points and zero in every stat.

// seedCloseWithOutcome writes one issue.closed event whose payload carries
// the close outcome, the shape production writes since sk-489.
func seedCloseWithOutcome(t *testing.T, pool *pgxpool.Pool, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, outcome string, occurredAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'issue.closed', $2, $3, 'human', 'issue', $4, $5, $6::jsonb)`,
		occurredAt, actor, actorUserID, issueID, projectID,
		fmt.Sprintf(`{"closeOutcome": %q}`, outcome)); err != nil {
		t.Fatalf("seed close event for %s: %v", issueID, err)
	}
}

// wireRow / wireScoring decode the JSON the dashboard handler writes for
// a ScoringResult (writeJSON → encoding/json). The tests read the
// response through these, not through the Go struct, so the contract
// checked is the wire one: a response without "leaderboardUsers" decodes
// to an empty list and fails the assertions instead of failing to build.
type wireRow struct {
	Username                   string `json:"username"`
	IssuesCreatedHuman         int64  `json:"issuesCreatedHuman"`
	IssuesCreatedDirected      int64  `json:"issuesCreatedDirected"`
	IssuesCreatedAutonomous    int64  `json:"issuesCreatedAutonomous"`
	IssuesClosedHuman          int64  `json:"issuesClosedHuman"`
	IssuesClosedDirected       int64  `json:"issuesClosedDirected"`
	IssuesClosedAutonomous     int64  `json:"issuesClosedAutonomous"`
	IssuesClosedDoneHuman      int64  `json:"issuesClosedDoneHuman"`
	IssuesClosedDoneDirected   int64  `json:"issuesClosedDoneDirected"`
	IssuesClosedDoneAutonomous int64  `json:"issuesClosedDoneAutonomous"`
	ClosedWithCommitsHuman     int64  `json:"closedWithCommitsHuman"`
	ClosedWithCommitsDir       int64  `json:"closedWithCommitsDir"`
	ClosedWithCommitsAuto      int64  `json:"closedWithCommitsAuto"`
	CommentsHuman              int64  `json:"commentsHuman"`
	CommentsDirected           int64  `json:"commentsDirected"`
	CommentsAutonomous         int64  `json:"commentsAutonomous"`
	ClosedHumanDriven          int64  `json:"closedHumanDriven"`
	ClosedDoneHumanDriven      int64  `json:"closedDoneHumanDriven"`
}

type wireScoring struct {
	Leaderboards map[string][]struct {
		Username string `json:"username"`
		Count    int64  `json:"count"`
	} `json:"leaderboards"`
	MatrixUsers      []wireRow `json:"matrixUsers"`
	LeaderboardUsers []wireRow `json:"leaderboardUsers"`
}

func toWire(t *testing.T, res *ScoringResult) (wireScoring, map[string]json.RawMessage) {
	t.Helper()
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal scoring: %v", err)
	}
	var w wireScoring
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal scoring: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal scoring keys: %v", err)
	}
	return w, raw
}

// ownFixture records the users and projects a test of this file seeds, and
// removes exactly those rows, in foreign-key order, before the shared
// seedUser/seedProject cleanups run. Those helpers cannot remove what these
// tests leave: events.actor_user_id, issues.created_by and projects.created_by
// are ON DELETE RESTRICT, and events carry no foreign key to the project, so
// an event or an issue outlives them and keeps the user. Each test calls
// `defer fx.teardown()` right after newOwnFixture: a deferred call runs when
// the test function returns or stops on t.Fatal, and before any t.Cleanup
// callback (the pool is closed by one), so the shared cleanups that follow
// delete zero rows.
type ownFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	users    []uuid.UUID
	projects []uuid.UUID
}

func newOwnFixture(t *testing.T, pool *pgxpool.Pool) *ownFixture {
	return &ownFixture{t: t, pool: pool}
}

func (fx *ownFixture) user() (uuid.UUID, string) {
	fx.t.Helper()
	id, uname := seedUser(fx.t, fx.pool)
	fx.users = append(fx.users, id)
	return id, uname
}

func (fx *ownFixture) project(owner uuid.UUID) uuid.UUID {
	fx.t.Helper()
	id := seedProject(fx.t, fx.pool, owner)
	fx.projects = append(fx.projects, id)
	return id
}

func (fx *ownFixture) teardown() {
	t := fx.t
	ctx := context.Background()
	exec := func(what, sql string, args ...any) int64 {
		ct, err := fx.pool.Exec(ctx, sql, args...)
		if err != nil {
			t.Errorf("teardown %s: %v", what, err)
			return 0
		}
		return ct.RowsAffected()
	}
	// seedProject inserts the project and only then its membership, so a
	// t.Fatal between the two leaves a project fx.project never recorded.
	// Every project of these tests is owned by one of fx.users, so the
	// projects to remove are found by their owner as well.
	if rows, err := fx.pool.Query(ctx, `SELECT id FROM projects WHERE created_by = ANY($1::uuid[])`, fx.users); err != nil {
		t.Errorf("teardown: listing own projects: %v", err)
	} else {
		known := make(map[uuid.UUID]struct{}, len(fx.projects))
		for _, id := range fx.projects {
			known[id] = struct{}{}
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Errorf("teardown: scanning own project: %v", err)
				break
			}
			if _, ok := known[id]; !ok {
				known[id] = struct{}{}
				fx.projects = append(fx.projects, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Errorf("teardown: reading own projects: %v", err)
		}
	}
	exec("events", `DELETE FROM events WHERE project_id = ANY($1::uuid[]) OR actor_user_id = ANY($2::uuid[])`, fx.projects, fx.users)
	// Leaf-first: issues.parent_id is RESTRICT. Comments and issue_commits
	// go with their issue (ON DELETE CASCADE).
	for range 64 {
		if exec("issues", `
DELETE FROM issues
 WHERE project_id = ANY($1::uuid[])
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = ANY($1::uuid[]) AND parent_id IS NOT NULL)`, fx.projects) == 0 {
			break
		}
	}
	exec("projects", `DELETE FROM projects WHERE id = ANY($1::uuid[])`, fx.projects)
	exec("users", `DELETE FROM users WHERE id = ANY($1::uuid[])`, fx.users)

	var users, projects, issues, events int64
	if err := fx.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM users    WHERE id = ANY($1::uuid[])),
       (SELECT count(*) FROM projects WHERE id = ANY($2::uuid[])),
       (SELECT count(*) FROM issues   WHERE project_id = ANY($2::uuid[])),
       (SELECT count(*) FROM events   WHERE project_id = ANY($2::uuid[]) OR actor_user_id = ANY($1::uuid[]))`,
		fx.users, fx.projects).Scan(&users, &projects, &issues, &events); err != nil {
		t.Errorf("teardown residue check: %v", err)
		return
	}
	if users+projects+issues+events != 0 {
		t.Errorf("teardown left users=%d projects=%d issues=%d events=%d", users, projects, issues, events)
	}
}

func TestScoring_LeaderboardUsersCarryRowsPastTheMatrixCap(t *testing.T) {
	pool := newTestPool(t)
	fx := newOwnFixture(t, pool)
	defer fx.teardown()
	ctx := context.Background()
	store := New(pool)

	owner, _ := fx.user()
	project := fx.project(owner)
	now := time.Now().UTC()
	at := now.Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "mxc")

	// 51 fillers, each with two human-source closes that did no work
	// (wont_do): closed_human_driven 2, nothing "done", no commits. They
	// fill the 50 matrix slots ahead of both heroes; the 51st is the one
	// the cap legitimately drops.
	const fillerCount = 51
	fillers := make([]string, 0, fillerCount)
	for f := 0; f < fillerCount; f++ {
		uid, uname := fx.user()
		fillers = append(fillers, uname)
		for k := 0; k < 2; k++ {
			id := fmt.Sprintf("%s-f%d-%d", pre, f, k)
			seedIssueRow(t, pool, project, id, "human", uid, nil)
			seedCloseWithOutcome(t, pool, id, project, uname, uid, "wont_do", at)
		}
	}
	sort.Strings(fillers)

	// Commits hero: creates and closes three autonomous tasks, each with a
	// linked commit, and writes two autonomous comments. closed_human_driven
	// is 0, closed_with_commits is 3 — first on that leaderboard.
	commitsHeroID, commitsHero := fx.user()
	for k := 0; k < 3; k++ {
		id := fmt.Sprintf("%s-c%d", pre, k)
		seedIssueRow(t, pool, project, id, "agent_autonomous", commitsHeroID, nil)
		seedEventForIssue(t, pool, "issue.created", id, project, commitsHero, commitsHeroID, "agent", at)
		seedIssueCommit(t, pool, id, project, commitsHeroID, fmt.Sprintf("%040d", k+1), at)
		seedCloseWithOutcome(t, pool, id, project, commitsHero, commitsHeroID, "done", at)
	}
	for k := 0; k < 2; k++ {
		seedCommentWithEvent(t, pool, pre+"-c0", project, commitsHeroID, commitsHero, "agent_autonomous", "agent", at)
	}

	// Done hero: one human-source close that finished the work.
	// closed_human_driven 1 (below every filler), closed_done_human_driven 1
	// — first on that leaderboard, since no filler has a "done" close.
	doneHeroID, doneHero := fx.user()
	doneID := pre + "-d0"
	seedIssueRow(t, pool, project, doneID, "human", doneHeroID, nil)
	seedCloseWithOutcome(t, pool, doneID, project, doneHero, doneHeroID, "done", at)

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project
	res, err := store.Scoring(ctx, Window24h, visible, false, &projectCopy, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	w, _ := toWire(t, res)

	// Preconditions: both heroes are on a leaderboard of this response,
	// and closed_human_driven is the first ten fillers by name (all tied).
	assertLeaderboardEntry(t, w, "closed_with_commits", commitsHero, 3)
	assertLeaderboardEntry(t, w, "closed_done_human_driven", doneHero, 1)
	for _, f := range fillers[:10] {
		assertLeaderboardEntry(t, w, "closed_human_driven", f, 2)
	}

	// matrixUsers is what it was before sk-192: the first 50 of the 51
	// tied fillers by name, in that order, and neither hero.
	if got, want := usernamesOf(w.MatrixUsers), fillers[:50]; !slices.Equal(got, want) {
		t.Fatalf("matrixUsers usernames:\n got  %v\n want %v", got, want)
	}
	for _, r := range w.MatrixUsers {
		if r.ClosedHumanDriven != 2 || r.IssuesClosedHuman != 2 || r.ClosedDoneHumanDriven != 0 {
			t.Fatalf("filler %s matrix row: %+v, want 2 human closes, none done", r.Username, r)
		}
	}

	// leaderboardUsers: one row per person on any leaderboard, ordered
	// like the matrix (closed_human_driven DESC, then name) — the ten
	// fillers of closed_human_driven, then the done hero (1), then the
	// commits hero (0). The 51st filler is on no leaderboard.
	wantNames := append(append([]string{}, fillers[:10]...), doneHero, commitsHero)
	if got := usernamesOf(w.LeaderboardUsers); !slices.Equal(got, wantNames) {
		t.Fatalf("leaderboardUsers usernames:\n got  %v\n want %v", got, wantNames)
	}
	rows := make(map[string]wireRow, len(w.LeaderboardUsers))
	for _, r := range w.LeaderboardUsers {
		rows[r.Username] = r
	}
	want := wireRow{
		Username:                   commitsHero,
		IssuesCreatedAutonomous:    3,
		IssuesClosedAutonomous:     3,
		IssuesClosedDoneAutonomous: 3,
		ClosedWithCommitsAuto:      3,
		CommentsAutonomous:         2,
	}
	if got := rows[commitsHero]; got != want {
		t.Fatalf("commits hero row:\n got  %+v\n want %+v", got, want)
	}
	want = wireRow{
		Username:              doneHero,
		IssuesClosedHuman:     1,
		IssuesClosedDoneHuman: 1,
		ClosedHumanDriven:     1,
		ClosedDoneHumanDriven: 1,
	}
	if got := rows[doneHero]; got != want {
		t.Fatalf("done hero row:\n got  %+v\n want %+v", got, want)
	}
	want = wireRow{Username: fillers[0], IssuesClosedHuman: 2, ClosedHumanDriven: 2}
	if got := rows[fillers[0]]; got != want {
		t.Fatalf("filler row on a leaderboard:\n got  %+v\n want %+v", got, want)
	}
}

// With nobody on any leaderboard, leaderboardUsers is an empty JSON array
// — the same shape as an empty matrixUsers — whether the scope has no
// activity at all or only creates and comments (no close, so no
// leaderboard); in the second case matrixUsers keeps its real rows.
func TestScoring_LeaderboardUsersEmptyWithoutLeaderboards(t *testing.T) {
	for _, tc := range []struct {
		name     string
		active   bool
		wantRows []wireRow
	}{
		{name: "no activity"},
		{name: "creates and comments only", active: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			fx := newOwnFixture(t, pool)
			defer fx.teardown()
			store := New(pool)
			actorID, actor := fx.user()
			project := fx.project(actorID)
			var want []wireRow
			if tc.active {
				at := time.Now().UTC().Add(-time.Hour)
				id := uniqueIDPrefix(t, "mxe") + "-0"
				seedIssueRow(t, pool, project, id, "human", actorID, nil)
				seedEventForIssue(t, pool, "issue.created", id, project, actor, actorID, "human", at)
				seedCommentWithEvent(t, pool, id, project, actorID, actor, "human", "human", at)
				seedCommentWithEvent(t, pool, id, project, actorID, actor, "agent_directed", "agent", at)
				want = []wireRow{{Username: actor, IssuesCreatedHuman: 1, CommentsHuman: 1, CommentsDirected: 1}}
			}

			visible := map[uuid.UUID]struct{}{project: {}}
			projectCopy := project
			res, err := store.Scoring(context.Background(), Window24h, visible, false, &projectCopy, nil, nil)
			if err != nil {
				t.Fatalf("Scoring: %v", err)
			}
			w, raw := toWire(t, res)
			for board, entries := range w.Leaderboards {
				if len(entries) != 0 {
					t.Fatalf("leaderboard %s: %+v, want empty", board, entries)
				}
			}
			if string(raw["leaderboardUsers"]) != "[]" {
				t.Fatalf("leaderboardUsers=%s, want []", raw["leaderboardUsers"])
			}
			if !slices.Equal(w.MatrixUsers, want) {
				t.Fatalf("matrixUsers:\n got  %+v\n want %+v", w.MatrixUsers, want)
			}
		})
	}
}

// A cached all-time result carries leaderboardUsers too: the window the
// heroes zone can be switched to must not lose the breakdown on a hit.
func TestScoring_AllTime_CacheHitKeepsLeaderboardUsers(t *testing.T) {
	pool := newTestPool(t)
	fx := newOwnFixture(t, pool)
	defer fx.teardown()
	ctx := context.Background()
	base := time.Now().UTC()
	clock := base
	store := NewWithClock(pool, func() time.Time { return clock })

	heroID, hero := fx.user()
	project := fx.project(heroID)
	id := uniqueIDPrefix(t, "mxa") + "-0"
	seedIssueRow(t, pool, project, id, "agent_autonomous", heroID, nil)
	seedIssueCommit(t, pool, id, project, heroID, fmt.Sprintf("%040d", 7), base.Add(-time.Hour))
	seedCloseWithOutcome(t, pool, id, project, hero, heroID, "done", base.Add(-time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project
	if _, err := store.Scoring(ctx, WindowAllTime, visible, false, &projectCopy, nil, nil); err != nil {
		t.Fatalf("Scoring first: %v", err)
	}
	// A re-query would now find nothing; only a cache hit still has the row.
	deleteEvent(t, pool, "issue.closed", id)
	clock = base.Add(30 * time.Second)

	res, err := store.Scoring(ctx, WindowAllTime, visible, false, &projectCopy, nil, nil)
	if err != nil {
		t.Fatalf("Scoring second: %v", err)
	}
	w, _ := toWire(t, res)
	assertLeaderboardEntry(t, w, "closed_with_commits", hero, 1)
	want := []wireRow{{
		Username:                   hero,
		IssuesClosedAutonomous:     1,
		IssuesClosedDoneAutonomous: 1,
		ClosedWithCommitsAuto:      1,
	}}
	if len(w.LeaderboardUsers) != 1 || w.LeaderboardUsers[0] != want[0] {
		t.Fatalf("cached leaderboardUsers:\n got  %+v\n want %+v", w.LeaderboardUsers, want)
	}
}

// leaderboardUsers is scoped like every other number of the response: a
// leaderboard person's row counts only the projects the caller sees, not
// their activity in a project outside the visible set.
func TestScoring_LeaderboardUsersCountOnlyVisibleProjects(t *testing.T) {
	pool := newTestPool(t)
	fx := newOwnFixture(t, pool)
	defer fx.teardown()
	store := New(pool)
	heroID, hero := fx.user()
	seen := fx.project(heroID)
	hidden := fx.project(heroID)
	at := time.Now().UTC().Add(-time.Hour)
	pre := uniqueIDPrefix(t, "mxv")
	for k, p := range []uuid.UUID{seen, hidden, hidden} {
		id := fmt.Sprintf("%s-%d", pre, k)
		seedIssueRow(t, pool, p, id, "agent_autonomous", heroID, nil)
		seedIssueCommit(t, pool, id, p, heroID, fmt.Sprintf("%040d", k+11), at)
		seedCloseWithOutcome(t, pool, id, p, hero, heroID, "done", at)
	}

	visible := map[uuid.UUID]struct{}{seen: {}}
	res, err := store.Scoring(context.Background(), Window24h, visible, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	w, _ := toWire(t, res)
	assertLeaderboardEntry(t, w, "closed_with_commits", hero, 1)
	want := wireRow{
		Username:                   hero,
		IssuesClosedAutonomous:     1,
		IssuesClosedDoneAutonomous: 1,
		ClosedWithCommitsAuto:      1,
	}
	if len(w.LeaderboardUsers) != 1 || w.LeaderboardUsers[0] != want {
		t.Fatalf("leaderboardUsers:\n got  %+v\n want [%+v]", w.LeaderboardUsers, want)
	}
}

func usernamesOf(rows []wireRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Username)
	}
	return out
}

func assertLeaderboardEntry(t *testing.T, w wireScoring, board, username string, count int64) {
	t.Helper()
	for _, e := range w.Leaderboards[board] {
		if e.Username == username {
			if e.Count != count {
				t.Fatalf("leaderboard %s: %s has count %d, want %d", board, username, e.Count, count)
			}
			return
		}
	}
	t.Fatalf("leaderboard %s has no entry for %s: %+v", board, username, w.Leaderboards[board])
}
