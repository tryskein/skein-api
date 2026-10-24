package dashboard

// close_outcome_test.go — sk-494: "closed" is not "done" on the
// dashboard surfaces.
//
// What is guarded. To every closes number the dashboard already
// published, a "done" half is added by the rule normalized in
// sk-489: the `done` outcome is work, the other three (`wont_do`,
// `duplicate`, `superseded`) are "closed without work". The existing numbers
// do not change on any input - this is the second half of the guard, and
// without it "the new field counts correctly" would be worth nothing.
//
// ⚠ THE MAIN DISTINGUISHING CASE - the EVENT and the ROW disagree. The query
// must read the outcome from the payload of the close event
// (`store.go#closeOutcomeExpr`), not from `issues.close_outcome`. On an
// "ordinary" fixture, where the row and the event agree, both implementations are
// green, so there are two tests here where they are deliberately apart:
// a reopened issue (the row has NO outcome at all - INV-1) and an issue whose
// outcome was edited after the close (US-3). On the first, reading the row would break
// the identity "closed = done + no_work", on the second it would rewrite
// the past window retroactively.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedClosedIssueAndEvent seeds one issue that is genuinely closed — the
// issues row carries the status and the outcome, and the event carries
// the same outcome in its payload. This is what production produces, so
// it is what the ordinary tests below run on.
func seedClosedIssueAndEvent(
	t *testing.T,
	env *outcomeEnv,
	issueID, source, outcome string,
	occurredAt time.Time,
) {
	t.Helper()
	seedIssueRow(t, env.pool, env.project, issueID, source, env.userID, nil)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE issues SET status = 'closed', closed_at = $2, close_outcome = $3 WHERE id = $1`,
		issueID, occurredAt, outcome); err != nil {
		t.Fatalf("close issue %s as %s: %v", issueID, outcome, err)
	}
	seedClosedEvent(t, env, issueID, outcome, occurredAt)
}

// seedClosedEvent writes only the event, with the outcome in the payload.
// An empty `outcome` writes the pre-sk-489 shape: payload '{}'.
func seedClosedEvent(t *testing.T, env *outcomeEnv, issueID, outcome string, occurredAt time.Time) {
	t.Helper()
	payload := `{}`
	if outcome != "" {
		payload = fmt.Sprintf(`{"closeOutcome": %q}`, outcome)
	}
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'issue.closed', $2, $3, 'human', 'issue', $4, $5, $6::jsonb)`,
		occurredAt, env.username, env.userID, issueID, env.project, payload); err != nil {
		t.Fatalf("seed close event for %s: %v", issueID, err)
	}
}

// outcomeEnv bundles the per-test fixture handles so the seeders above
// take one argument instead of five.
type outcomeEnv struct {
	pool     *pgxpool.Pool
	store    *Store
	project  uuid.UUID
	userID   uuid.UUID
	username string
}

func newOutcomeEnv(t *testing.T) *outcomeEnv {
	t.Helper()
	pool := newTestPool(t)
	user, uname := seedUser(t, pool)
	return &outcomeEnv{
		pool:     pool,
		store:    New(pool),
		project:  seedProject(t, pool, user),
		userID:   user,
		username: uname,
	}
}

// scoped runs Scoring narrowed to this fixture's project so the shared
// test database's unrelated rows cannot reach the assertions.
func (e *outcomeEnv) scoped(t *testing.T) *ScoringResult {
	t.Helper()
	project := e.project
	res, err := e.store.Scoring(context.Background(), Window24h,
		map[uuid.UUID]struct{}{e.project: {}}, false, &project, nil, nil /*reach*/)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	return res
}

// TestScoring_ClosedByOutcome_Split — the base case: closes with different
// outcomes on all three intent axes.
//
// The numbers per axis are DIFFERENT (3/3/2 closes, of them 2/1/0 done) on purpose:
// on a symmetric fixture swapped columns would pass
// green. All three "no work" values occur, because the
// predicate is written as a blacklist ("everything that is not done"), and a fixture of
// a single `wont_do` would not distinguish it from a predicate for exactly that value.
func TestScoring_ClosedByOutcome_Split(t *testing.T) {
	env := newOutcomeEnv(t)
	now := time.Now().UTC()
	at := now.Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "cbo")

	// human: 2 done + 1 wont_do
	seedClosedIssueAndEvent(t, env, pre+"-h1", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-h2", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-h3", "human", "wont_do", at)
	// agent_directed: 1 done + 2 duplicate
	seedClosedIssueAndEvent(t, env, pre+"-d1", "agent_directed", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-d2", "agent_directed", "duplicate", at)
	seedClosedIssueAndEvent(t, env, pre+"-d3", "agent_directed", "duplicate", at)
	// agent_autonomous: 0 done + 2 superseded
	seedClosedIssueAndEvent(t, env, pre+"-a1", "agent_autonomous", "superseded", at)
	seedClosedIssueAndEvent(t, env, pre+"-a2", "agent_autonomous", "superseded", at)

	res := env.scoped(t)
	closed := findMetric(t, res, "issues_closed")
	done := findMetric(t, res, "issues_closed_done")
	noWork := findMetric(t, res, "issues_closed_no_work")

	assertAxes(t, "issues_closed", closed, 3, 3, 2)
	assertAxes(t, "issues_closed_done", done, 2, 1, 0)
	assertAxes(t, "issues_closed_no_work", noWork, 1, 2, 2)
	assertClosedIdentity(t, closed, done, noWork)
}

// TestScoring_ClosedByOutcome_LegacyEventIsDone — an event from BEFORE sk-489
// (empty payload) counts as "done".
//
// This is not leniency to old data but the same history record that is already
// made twice: the seed of migration 000050 and its trigger normalizer. Next to it
// is the CONTROL: the same selection holds a close "without work", and it does not
// fall into "done". Without the control "legacy = done" would pass for an
// implementation that counts everything as done.
func TestScoring_ClosedByOutcome_LegacyEventIsDone(t *testing.T) {
	env := newOutcomeEnv(t)
	at := time.Now().UTC().Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "cbl")

	// An old close: the row is closed, the outcome is seeded by the migration as `done`,
	// and the event carries no payload.
	seedIssueRow(t, env.pool, env.project, pre+"-old", "human", env.userID, nil)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE issues SET status = 'closed', closed_at = $2 WHERE id = $1`,
		pre+"-old", at); err != nil {
		t.Fatalf("close legacy issue: %v", err)
	}
	seedClosedEvent(t, env, pre+"-old", "", at)
	// A second old close - so that the halves are DIFFERENT (2 and 1):
	// at 1:1 swapped halves would pass green.
	seedIssueRow(t, env.pool, env.project, pre+"-old2", "human", env.userID, nil)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE issues SET status = 'closed', closed_at = $2 WHERE id = $1`,
		pre+"-old2", at); err != nil {
		t.Fatalf("close second legacy issue: %v", err)
	}
	seedClosedEvent(t, env, pre+"-old2", "", at)
	// Control - a new close without work in the same window.
	seedClosedIssueAndEvent(t, env, pre+"-new", "human", "wont_do", at)

	res := env.scoped(t)
	closed := findMetric(t, res, "issues_closed")
	done := findMetric(t, res, "issues_closed_done")
	noWork := findMetric(t, res, "issues_closed_no_work")

	assertAxes(t, "issues_closed", closed, 3, 0, 0)
	assertAxes(t, "issues_closed_done", done, 2, 0, 0)
	assertAxes(t, "issues_closed_no_work", noWork, 1, 0, 0)
	assertClosedIdentity(t, closed, done, noWork)
}

// TestScoring_ClosedByOutcome_ReopenedIssueKeepsItsClose — DISTINGUISHING
// case No. 1: a reopened issue has no outcome at all (INV-1, migration
// 000050 clears it on any unclosed status), while its close in the window
// happened and must stay counted.
//
// An implementation reading `issues.close_outcome` would put such a close
// in `issues_closed` and in NEITHER half - the identity would break. This
// test is the only place where this shows: on a consistent fixture both
// implementations are green.
func TestScoring_ClosedByOutcome_ReopenedIssueKeepsItsClose(t *testing.T) {
	env := newOutcomeEnv(t)
	at := time.Now().UTC().Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "cbr")

	// Closed without work in the window - and then reopened.
	seedClosedIssueAndEvent(t, env, pre+"-re", "human", "wont_do", at)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE issues SET status = 'open', closed_at = NULL WHERE id = $1`,
		pre+"-re"); err != nil {
		t.Fatalf("reopen issue: %v", err)
	}
	// Insurance against a silent change of the fixture's meaning: the reopening must
	// clear the outcome, otherwise the "distinguishing" case distinguishes nothing.
	var outcome *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT close_outcome FROM issues WHERE id = $1`, pre+"-re").Scan(&outcome); err != nil {
		t.Fatalf("read close_outcome back: %v", err)
	}
	if outcome != nil {
		t.Fatalf("the reopened issue kept outcome %q — the fixture no longer distinguishes "+
			"reading the event from reading the row", *outcome)
	}
	// Control - two ordinary done closes nearby. There are TWO, not one,
	// so that the halves differ in size: at 1:1 swapping them would pass
	// green.
	seedClosedIssueAndEvent(t, env, pre+"-ok1", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-ok2", "human", "done", at)

	res := env.scoped(t)
	closed := findMetric(t, res, "issues_closed")
	done := findMetric(t, res, "issues_closed_done")
	noWork := findMetric(t, res, "issues_closed_no_work")

	assertAxes(t, "issues_closed", closed, 3, 0, 0)
	assertAxes(t, "issues_closed_done", done, 2, 0, 0)
	assertAxes(t, "issues_closed_no_work", noWork, 1, 0, 0)
	assertClosedIdentity(t, closed, done, noWork)
}

// TestScoring_ClosedByOutcome_EditedOutcomeDoesNotRewriteHistory —
// DISTINGUISHING case No. 2: the outcome of a closed issue is edited without reopening
// (US-3). The past window has no right to change because of it: the close
// happened with the outcome it happened with.
func TestScoring_ClosedByOutcome_EditedOutcomeDoesNotRewriteHistory(t *testing.T) {
	env := newOutcomeEnv(t)
	at := time.Now().UTC().Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "cbe")

	// Closed as "done"; the outcome was later changed to "duplicate".
	seedClosedIssueAndEvent(t, env, pre+"-ed", "human", "done", at)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE issues SET close_outcome = 'duplicate' WHERE id = $1`, pre+"-ed"); err != nil {
		t.Fatalf("edit close_outcome: %v", err)
	}

	res := env.scoped(t)
	closed := findMetric(t, res, "issues_closed")
	done := findMetric(t, res, "issues_closed_done")
	noWork := findMetric(t, res, "issues_closed_no_work")

	assertAxes(t, "issues_closed", closed, 1, 0, 0)
	assertAxes(t, "issues_closed_done", done, 1, 0, 0)
	assertAxes(t, "issues_closed_no_work", noWork, 0, 0, 0)
	assertClosedIdentity(t, closed, done, noWork)
}

// TestScoring_Leaderboard_ClosedDoneHumanDriven — a new podium next to
// the old one, and the old one did NOT narrow.
func TestScoring_Leaderboard_ClosedDoneHumanDriven(t *testing.T) {
	env := newOutcomeEnv(t)
	other, otherUname := seedUser(t, env.pool)
	at := time.Now().UTC().Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "cbd")

	// Owner: 3 closes, of them 2 done.
	seedClosedIssueAndEvent(t, env, pre+"-o1", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-o2", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-o3", "human", "wont_do", at)
	// The second actor: one close, and it is without work - he must not be
	// in the new podium AT ALL, not even with zero.
	seedIssueRow(t, env.pool, env.project, pre+"-x1", "human", other, nil)
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE issues SET status = 'closed', closed_at = $2, close_outcome = 'duplicate' WHERE id = $1`,
		pre+"-x1", at); err != nil {
		t.Fatalf("close other's issue: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'issue.closed', $2, $3, 'human', 'issue', $4, $5, '{"closeOutcome": "duplicate"}'::jsonb)`,
		at, otherUname, other, pre+"-x1", env.project); err != nil {
		t.Fatalf("seed other's close event: %v", err)
	}

	res := env.scoped(t)

	if got := leaderboardCount(res, "closed_human_driven", env.username); got != 3 {
		t.Errorf("closed_human_driven[%s] = %d, want 3 — the old podium must stay "+
			"a ranking of CLOSES", env.username, got)
	}
	if got := leaderboardCount(res, "closed_human_driven", otherUname); got != 1 {
		t.Errorf("closed_human_driven[%s] = %d, want 1", otherUname, got)
	}
	if got := leaderboardCount(res, "closed_done_human_driven", env.username); got != 2 {
		t.Errorf("closed_done_human_driven[%s] = %d, want 2", env.username, got)
	}
	if got := leaderboardCount(res, "closed_done_human_driven", otherUname); got != 0 {
		t.Errorf("closed_done_human_driven[%s] = %d, want 0 (there must be no row)",
			otherUname, got)
	}
}

// TestScoring_MatrixPerUser_ClosedDoneColumns — three "done" cells and the
// sum axis on the user's row; the old close cells did not move.
func TestScoring_MatrixPerUser_ClosedDoneColumns(t *testing.T) {
	env := newOutcomeEnv(t)
	at := time.Now().UTC().Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "cbm")

	// human: 2 closes, 1 done. directed: 2 closes, 2 done.
	// autonomous: 1 close, 0 done.
	seedClosedIssueAndEvent(t, env, pre+"-h1", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-h2", "human", "wont_do", at)
	seedClosedIssueAndEvent(t, env, pre+"-d1", "agent_directed", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-d2", "agent_directed", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-a1", "agent_autonomous", "superseded", at)

	res := env.scoped(t)
	var row *MatrixUserRow
	for i := range res.MatrixUsers {
		if res.MatrixUsers[i].Username == env.username {
			row = &res.MatrixUsers[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("matrixUsers missing row for %s", env.username)
	}
	if row.IssuesClosedHuman != 2 || row.IssuesClosedDirected != 2 || row.IssuesClosedAutonomous != 1 {
		t.Errorf("the old close cells moved: h=%d d=%d a=%d, want 2/2/1",
			row.IssuesClosedHuman, row.IssuesClosedDirected, row.IssuesClosedAutonomous)
	}
	if row.IssuesClosedDoneHuman != 1 || row.IssuesClosedDoneDirected != 2 || row.IssuesClosedDoneAutonomous != 0 {
		t.Errorf("the \"done\" cells: h=%d d=%d a=%d, want 1/2/0",
			row.IssuesClosedDoneHuman, row.IssuesClosedDoneDirected, row.IssuesClosedDoneAutonomous)
	}
	if row.ClosedHumanDriven != 4 {
		t.Errorf("ClosedHumanDriven = %d, want 4 (2 human + 2 directed closes)", row.ClosedHumanDriven)
	}
	if row.ClosedDoneHumanDriven != 3 {
		t.Errorf("ClosedDoneHumanDriven = %d, want 3 (1 human + 2 directed done)",
			row.ClosedDoneHumanDriven)
	}
}

// TestRace_ClosedDoneMetric — the race on the new metric counts only
// done closes, and on the old "closed" - all, as it always did.
func TestRace_ClosedDoneMetric(t *testing.T) {
	env := newOutcomeEnv(t)
	at := time.Now().UTC().Add(-2 * time.Hour)
	pre := uniqueIDPrefix(t, "cbrace")

	seedClosedIssueAndEvent(t, env, pre+"-1", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-2", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-3", "human", "wont_do", at)
	seedClosedIssueAndEvent(t, env, pre+"-4", "human", "duplicate", at)

	total := func(metric RaceMetric) int64 {
		t.Helper()
		project := env.project
		res, err := env.store.Race(context.Background(), RaceArgs{
			Metric:          metric,
			Window:          RaceWindow7d,
			TopN:            20,
			VisibleProjects: map[uuid.UUID]struct{}{env.project: {}},
			ProjectFilter:   &project,
		})
		if err != nil {
			t.Fatalf("Race(%s): %v", metric, err)
		}
		if res.Metric != string(metric) {
			t.Fatalf("Race echoed metric %q, want %q", res.Metric, metric)
		}
		for _, u := range res.Users {
			if u.Username == env.username {
				return u.Total
			}
		}
		return 0
	}

	if got := total(RaceMetricClosed); got != 4 {
		t.Errorf("metric=closed total = %d, want 4 — the old metric counts ALL closes", got)
	}
	if got := total(RaceMetricClosedDone); got != 2 {
		t.Errorf("metric=closed_done total = %d, want 2", got)
	}
}

// TestParseRaceMetric_ClosedDone — the new metric is accepted on the wire,
// while garbage is still rejected (control: without it the test would pass for
// a parser that accepts everything).
func TestParseRaceMetric_ClosedDone(t *testing.T) {
	if m, ok := ParseRaceMetric("closed_done"); !ok || m != RaceMetricClosedDone {
		t.Errorf("ParseRaceMetric(closed_done) = %q,%v; want closed_done,true", m, ok)
	}
	for _, bad := range []string{"closed_wont_do", "done", "closeddone", ""} {
		if _, ok := ParseRaceMetric(bad); ok {
			t.Errorf("ParseRaceMetric(%q) accepted, want rejected", bad)
		}
	}
}

// assertAxes compares one metric row against the expected per-axis triple.
func assertAxes(t *testing.T, key string, row MetricRow, human, directed, autonomous int64) {
	t.Helper()
	if row.Human != human || row.AgentDirected != directed || row.AgentAutonomous != autonomous {
		t.Errorf("%s = h:%d d:%d a:%d, want h:%d d:%d a:%d",
			key, row.Human, row.AgentDirected, row.AgentAutonomous, human, directed, autonomous)
	}
}

// assertClosedIdentity checks, per axis, that the two sk-494 halves add
// back up to the untouched issues_closed row. It is the property that
// says the split is a partition rather than two loosely related counts.
func assertClosedIdentity(t *testing.T, closed, done, noWork MetricRow) {
	t.Helper()
	axes := []struct {
		name       string
		cl, dn, nw int64
	}{
		{"human", closed.Human, done.Human, noWork.Human},
		{"agent_directed", closed.AgentDirected, done.AgentDirected, noWork.AgentDirected},
		{"agent_autonomous", closed.AgentAutonomous, done.AgentAutonomous, noWork.AgentAutonomous},
	}
	for _, a := range axes {
		if a.dn+a.nw != a.cl {
			t.Errorf("identity broken on axis %s: done(%d) + no_work(%d) != closed(%d)",
				a.name, a.dn, a.nw, a.cl)
		}
	}
}

// leaderboardCount returns the count for `username` in the named
// leaderboard, or 0 when the row is absent.
func leaderboardCount(res *ScoringResult, key, username string) int64 {
	for _, e := range res.Leaderboards[key] {
		if e.Username == username {
			return e.Count
		}
	}
	return 0
}
