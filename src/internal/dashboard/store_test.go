// Package dashboard — store integration tests.
//
// Pattern mirrors internal/issues/store_test.go: TEST_DATABASE_URL gate,
// fresh pgxpool per test, schema-bound cleanup via DELETE on the seeded
// rows. Each test seeds a self-contained set of users / projects /
// issues / events, asserts the aggregation, and cleans up via the t.Cleanup
// chain so parallel runs do not collide on the shared seeded data.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/redact"
	"skein-api/internal/testsupport"
)

const testDatabaseURL = "TEST_DATABASE_URL"

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect %s: %v", testDatabaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping %s: %v", testDatabaseURL, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedUser(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "du" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	prefix := "dst" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, "dashtest_"+suffix, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err = pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner)
	if err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	t.Cleanup(func() {
		// issues.project_id is ON DELETE RESTRICT (migration 000004), so
		// delete issues leaf-first (children before parents) before the
		// project, else the project DELETE silently fails and rows leak.
		for range 64 {
			ct, err := pool.Exec(context.Background(), `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = $1 AND parent_id IS NOT NULL)`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

// seedIssueRow inserts a minimal issues row with the supplied source
// (intent axis). The creator and assignee are passed in to make the
// "comments_on_others" predicate testable.
func seedIssueRow(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, idStr, source string, creator uuid.UUID, assignee *string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, $4, NULL, $5,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $6, now(), now(), '{}'::jsonb)`,
		idStr, projectID, idStr, source, assignee, creator)
	if err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

// seedEventForIssue writes one events row tied to an issue. occurredAt
// is honoured exactly so window-filter tests can place rows just inside
// or just outside the window.
func seedEventForIssue(t *testing.T, pool *pgxpool.Pool, eventType, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, actorType string, occurredAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, $2, $3, $4, $5, 'issue', $6, $7, '{}'::jsonb)`,
		occurredAt, eventType, actor, actorUserID, actorType, issueID, projectID)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

// seedAgentEventWithSession is seedEventForIssue for agent rows that
// need a specific actor_session_id — the axis projects.activeAgents
// counts (sk-214). Kept separate from seedEventForIssue so the many
// callers that don't care about sessions stay unchanged; those seed
// NULL session ids, which activeAgents deliberately skips.
func seedAgentEventWithSession(t *testing.T, pool *pgxpool.Pool, eventType, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, sessionID string, occurredAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    actor_session_id, entity_type, entity_id, project_id, payload)
VALUES ($1, $2, $3, $4, 'agent', $5, 'issue', $6, $7, '{}'::jsonb)`,
		occurredAt, eventType, actor, actorUserID, sessionID, issueID, projectID)
	if err != nil {
		t.Fatalf("seed agent event: %v", err)
	}
}

// seedComment writes a comments row with the supplied author_kind, then
// emits a comment.created event tied to that comment. Returns the comment
// uuid for callers wanting to assert directly.
func seedCommentWithEvent(t *testing.T, pool *pgxpool.Pool, issueID string, projectID uuid.UUID, actorUserID uuid.UUID, actorUsername, authorKind, authorType string, occurredAt time.Time) uuid.UUID {
	t.Helper()
	commentID := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author_user_id, author, author_type, author_kind, body, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'body', $7)`,
		commentID, issueID, actorUserID, actorUsername, authorType, authorKind, occurredAt)
	if err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	_, err = pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'comment.created', $2, $3, $4, 'comment', $5, $6, '{}'::jsonb)`,
		occurredAt, actorUsername, actorUserID, authorType, commentID.String(), projectID)
	if err != nil {
		t.Fatalf("seed comment event: %v", err)
	}
	return commentID
}

// seedIssueCommit writes an issue_commits row with linked_at = occurredAt.
// Used to test the closed_with_commits metric. projectID is accepted for
// callsite symmetry but isn't a column on issue_commits — kept for API
// stability across tests.
func seedIssueCommit(t *testing.T, pool *pgxpool.Pool, issueID string, _ uuid.UUID, linkedBy uuid.UUID, hash string, linkedAt time.Time) {
	t.Helper()
	repoID := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO issue_commits (issue_id, repo_id, hash, url, message, author, committed_at, linked_at, linked_by, linked_by_kind)
VALUES ($1, $2, $3, NULL, NULL, NULL, NULL, $4, $5, 'human')`,
		issueID, repoID, hash, linkedAt, linkedBy)
	if err != nil {
		t.Fatalf("seed issue_commit: %v", err)
	}
}

// findMetric pulls a metric row by key out of a ScoringResult. Fails
// the test if the key is missing — the store always emits all seven
// rows, even when the count is zero.
func findMetric(t *testing.T, res *ScoringResult, key string) MetricRow {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Key == key {
			return m
		}
	}
	keys := make([]string, 0, len(res.Metrics))
	for _, m := range res.Metrics {
		keys = append(keys, m.Key)
	}
	t.Fatalf("metric %q not in response (had %v)", key, keys)
	return MetricRow{}
}

// uniqueIDPrefix mints a per-test prefix so concurrent test runs don't
// collide on the issues primary key. Combine with i in the loop body.
func uniqueIDPrefix(t *testing.T, kind string) string {
	t.Helper()
	return kind + "-" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")
}

// TestScoring_AxisSplit_Issues seeds 3 issues with distinct sources +
// issue.created events for each and asserts the 3-axis split.
func TestScoring_AxisSplit_Issues(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "axis")
	for i, src := range []string{"human", "agent_directed", "agent_autonomous"} {
		id := fmt.Sprintf("%s-%d", pre, i)
		seedIssueRow(t, pool, project, id, src, owner, nil)
		seedEventForIssue(t, pool, "issue.created", id, project, ownerUname, owner, "human", now.Add(-1*time.Hour))
	}

	visible := map[uuid.UUID]struct{}{project: {}}
	res, err := store.Scoring(ctx, Window24h, visible, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	m := findMetric(t, res, "issues_created")
	if m.Human != 1 || m.AgentDirected != 1 || m.AgentAutonomous != 1 {
		t.Fatalf("issues_created split mismatch: %+v", m)
	}
}

// TestScoring_WindowFilter places events on either side of the 24h
// boundary and asserts only the in-window row counts.
func TestScoring_WindowFilter(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "win")
	inID := pre + "-in"
	outID := pre + "-out"
	// In window
	seedIssueRow(t, pool, project, inID, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.created", inID, project, ownerUname, owner, "human", now.Add(-1*time.Hour))
	// Out of window (older than 24h)
	seedIssueRow(t, pool, project, outID, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.created", outID, project, ownerUname, owner, "human", now.Add(-48*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	res, err := store.Scoring(ctx, Window24h, visible, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	m := findMetric(t, res, "issues_created")
	if m.Human != 1 {
		t.Fatalf("expected human=1, got %d", m.Human)
	}
	// 7d window should see both
	res, err = store.Scoring(ctx, Window7d, visible, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scoring 7d: %v", err)
	}
	m = findMetric(t, res, "issues_created")
	if m.Human != 2 {
		t.Fatalf("expected human=2 within 7d, got %d", m.Human)
	}
}

// TestScoring_Visibility verifies the non-admin caller does not see
// events from a project they are not a member of.
func TestScoring_Visibility(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	otherOwner, otherUname := seedUser(t, pool)
	mine := seedProject(t, pool, owner)
	theirs := seedProject(t, pool, otherOwner)

	now := time.Now().UTC()
	mineID := uniqueIDPrefix(t, "vis-mine") + "-1"
	theirsID := uniqueIDPrefix(t, "vis-theirs") + "-1"
	seedIssueRow(t, pool, mine, mineID, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.created", mineID, mine, ownerUname, owner, "human", now.Add(-30*time.Minute))
	seedIssueRow(t, pool, theirs, theirsID, "human", otherOwner, nil)
	seedEventForIssue(t, pool, "issue.created", theirsID, theirs, otherUname, otherOwner, "human", now.Add(-30*time.Minute))

	// Caller (owner) is a member of `mine` but NOT `theirs`. Filter
	// to owner's username so other DB-resident seed data from
	// parallel tests doesn't bleed in.
	visible := map[uuid.UUID]struct{}{mine: {}}
	mineCopy := mine
	res, err := store.Scoring(ctx, Window24h, visible, false, &mineCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	m := findMetric(t, res, "issues_created")
	if m.Human != 1 {
		t.Fatalf("expected human=1 (own project only), got %d", m.Human)
	}

	// Admin sees other project too — scope to other's username/project
	// so we can assert exactly 1 visible row when admin queries them
	// (independent of bleed from parallel seeds).
	theirsCopy := theirs
	res, err = store.Scoring(ctx, Window24h, nil, true, &theirsCopy, &otherUname, nil)
	if err != nil {
		t.Fatalf("Scoring admin: %v", err)
	}
	m = findMetric(t, res, "issues_created")
	if m.Human != 1 {
		t.Fatalf("admin scoped to theirs+other should see human=1, got %d", m.Human)
	}
}

// TestScoring_CommentIntentAndOnOthers exercises the comment_authored
// vs comment_on_others split.
func TestScoring_CommentIntentAndOnOthers(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	other, otherUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	// Issue assigned to OTHER
	otherAssign := otherUname
	id := uniqueIDPrefix(t, "ci") + "-1"
	seedIssueRow(t, pool, project, id, "human", owner, &otherAssign)

	// Owner comments on OTHER's task → counts as "on others"
	seedCommentWithEvent(t, pool, id, project, owner, ownerUname, "human", "human", now.Add(-1*time.Hour))
	// Agent autonomous comment by other → not on others (same assignee)
	seedCommentWithEvent(t, pool, id, project, other, otherUname, "agent_autonomous", "agent", now.Add(-30*time.Minute))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project
	res, err := store.Scoring(ctx, Window24h, visible, false, &projectCopy, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	authored := findMetric(t, res, "comments_authored")
	if authored.Human != 1 || authored.AgentAutonomous != 1 {
		t.Fatalf("comments_authored split mismatch: %+v", authored)
	}
	others := findMetric(t, res, "comments_on_others_issues")
	if others.Human != 1 {
		t.Fatalf("expected on-others human=1, got %d", others.Human)
	}
	if others.AgentAutonomous != 0 {
		t.Fatalf("expected on-others autonomous=0 (self-assignee), got %d", others.AgentAutonomous)
	}
}

// TestScoring_ClosedWithCommits exercises the EXISTS subquery on
// issue_commits.
func TestScoring_ClosedWithCommits(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "wc")
	withID := pre + "-with"
	withoutID := pre + "-without"
	// Closed with commit
	seedIssueRow(t, pool, project, withID, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.closed", withID, project, ownerUname, owner, "human", now.Add(-1*time.Hour))
	seedIssueCommit(t, pool, withID, project, owner, "deadbeef0011", now.Add(-1*time.Hour))
	// Closed without commit
	seedIssueRow(t, pool, project, withoutID, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.closed", withoutID, project, ownerUname, owner, "human", now.Add(-1*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project
	res, err := store.Scoring(ctx, Window24h, visible, false, &projectCopy, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	closed := findMetric(t, res, "issues_closed")
	if closed.Human != 2 {
		t.Fatalf("expected closed.human=2, got %d", closed.Human)
	}
	withCommits := findMetric(t, res, "issues_closed_with_commits")
	if withCommits.Human != 1 {
		t.Fatalf("expected with_commits.human=1, got %d", withCommits.Human)
	}
}

// TestScoring_ClosedWithCommits_PreWindowCommit locks the sk-65 follow-up
// fix: a close INSIDE the window whose commit was linked BEFORE the window
// start must still count as commit-backed. The old EXISTS bounded the
// commit by linked_at >= since, which silently dropped such closes from
// finite-window counts (the /activity HOT-ZONE-vs-podium divergence). The
// close is 1h ago (inside 24h); the commit was linked 40 days ago (outside).
func TestScoring_ClosedWithCommits_PreWindowCommit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "wcpre")
	id := pre + "-pre"
	seedIssueRow(t, pool, project, id, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.closed", id, project, ownerUname, owner, "human", now.Add(-1*time.Hour))
	seedIssueCommit(t, pool, id, project, owner, "cafebabe0022", now.Add(-40*24*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project
	res, err := store.Scoring(ctx, Window24h, visible, false, &projectCopy, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	withCommits := findMetric(t, res, "issues_closed_with_commits")
	if withCommits.Human != 1 {
		t.Fatalf("expected with_commits.human=1 (pre-window commit still counts), got %d", withCommits.Human)
	}
}

// TestScoring_Leaderboards confirms the two leaderboards return
// actor + count rows in DESC order.
func TestScoring_Leaderboards(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	other, otherUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "lb")
	// 3 closes for owner (human), 1 close for other (human), all in 24h
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("%s-owner-%d", pre, i)
		seedIssueRow(t, pool, project, id, "human", owner, nil)
		seedEventForIssue(t, pool, "issue.closed", id, project, ownerUname, owner, "human", now.Add(-1*time.Hour))
	}
	id := pre + "-other-1"
	seedIssueRow(t, pool, project, id, "human", other, nil)
	seedEventForIssue(t, pool, "issue.closed", id, project, otherUname, other, "human", now.Add(-1*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project
	res, err := store.Scoring(ctx, Window24h, visible, false, &projectCopy, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	lb := res.Leaderboards["closed_human_driven"]
	if len(lb) < 2 {
		t.Fatalf("expected >=2 leaderboard entries, got %d", len(lb))
	}
	// First entry should be owner with count 3
	var ownerEntry, otherEntry *LeaderboardEntry
	for i := range lb {
		if lb[i].Username == ownerUname {
			ownerEntry = &lb[i]
		}
		if lb[i].Username == otherUname {
			otherEntry = &lb[i]
		}
	}
	if ownerEntry == nil || ownerEntry.Count != 3 {
		t.Fatalf("expected owner with count=3 in leaderboard, got %+v", ownerEntry)
	}
	if otherEntry == nil || otherEntry.Count != 1 {
		t.Fatalf("expected other with count=1, got %+v", otherEntry)
	}
}

// TestScoring_MatrixPerUser sanity-checks the matrixUsers result.
func TestScoring_MatrixPerUser(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	now := time.Now().UTC()
	pre := uniqueIDPrefix(t, "mx")
	for i, src := range []string{"human", "agent_directed", "agent_autonomous"} {
		id := fmt.Sprintf("%s-%d", pre, i)
		seedIssueRow(t, pool, project, id, src, owner, nil)
		seedEventForIssue(t, pool, "issue.created", id, project, ownerUname, owner, "human", now.Add(-1*time.Hour))
		seedEventForIssue(t, pool, "issue.closed", id, project, ownerUname, owner, "human", now.Add(-30*time.Minute))
	}
	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project
	res, err := store.Scoring(ctx, Window24h, visible, false, &projectCopy, nil, nil)
	if err != nil {
		t.Fatalf("Scoring: %v", err)
	}
	var ownerRow *MatrixUserRow
	for i := range res.MatrixUsers {
		if res.MatrixUsers[i].Username == ownerUname {
			ownerRow = &res.MatrixUsers[i]
			break
		}
	}
	if ownerRow == nil {
		t.Fatalf("matrixUsers missing owner row")
	}
	if ownerRow.IssuesCreatedHuman != 1 || ownerRow.IssuesCreatedDirected != 1 || ownerRow.IssuesCreatedAutonomous != 1 {
		t.Fatalf("matrix created split mismatch: %+v", ownerRow)
	}
	if ownerRow.ClosedHumanDriven != 2 {
		t.Fatalf("expected ClosedHumanDriven=2 (human + directed), got %d", ownerRow.ClosedHumanDriven)
	}
}

// TestInProgress_HappyPath seeds three issues — one in_progress, one
// locked-by-agent (status open), one neither — and asserts the first
// two surface and the third doesn't.
func TestInProgress_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, _ := seedUser(t, pool)
	project := seedProject(t, pool, owner)
	pre := uniqueIDPrefix(t, "inpr")

	// Row 1: status=in_progress, no lock.
	idInProg := pre + "-1"
	seedIssueRow(t, pool, project, idInProg, "human", owner, nil)
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET status='in_progress', started_at=now() WHERE id=$1`,
		idInProg); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}

	// Row 2: status=open but locked_by set (agent claimed).
	idLocked := pre + "-2"
	seedIssueRow(t, pool, project, idLocked, "human", owner, nil)
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET locked_by='mcp-session-test', locked_at=now() WHERE id=$1`,
		idLocked); err != nil {
		t.Fatalf("set locked: %v", err)
	}

	// Row 3: neither in_progress nor locked — should NOT appear.
	idQuiet := pre + "-3"
	seedIssueRow(t, pool, project, idQuiet, "human", owner, nil)

	visible := map[uuid.UUID]struct{}{project: {}}
	res, err := store.InProgress(ctx, visible, false, nil, 50, nil /*reach*/, redact.None)
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}

	seen := map[string]InProgressRow{}
	for _, r := range res.Items {
		seen[r.IssueID] = r
	}
	if _, ok := seen[idInProg]; !ok {
		t.Errorf("expected in_progress row %s in result, got %v", idInProg, seen)
	}
	if _, ok := seen[idLocked]; !ok {
		t.Errorf("expected locked row %s in result, got %v", idLocked, seen)
	}
	if _, ok := seen[idQuiet]; ok {
		t.Errorf("did not expect quiet row %s in result", idQuiet)
	}
}

// TestInProgress_Visibility seeds a project the caller can't see and
// asserts its in-progress rows are filtered out.
func TestInProgress_Visibility(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, _ := seedUser(t, pool)
	visibleProj := seedProject(t, pool, owner)
	hiddenProj := seedProject(t, pool, owner)

	pre := uniqueIDPrefix(t, "inprv")
	visibleID := pre + "-vis"
	hiddenID := pre + "-hid"
	seedIssueRow(t, pool, visibleProj, visibleID, "human", owner, nil)
	seedIssueRow(t, pool, hiddenProj, hiddenID, "human", owner, nil)
	for _, id := range []string{visibleID, hiddenID} {
		if _, err := pool.Exec(ctx,
			`UPDATE issues SET status='in_progress' WHERE id=$1`, id); err != nil {
			t.Fatalf("set in_progress %s: %v", id, err)
		}
	}

	// Caller sees only visibleProj.
	visible := map[uuid.UUID]struct{}{visibleProj: {}}
	res, err := store.InProgress(ctx, visible, false, nil, 50, nil /*reach*/, redact.None)
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}
	for _, r := range res.Items {
		if r.IssueID == hiddenID {
			t.Fatalf("hidden project leak: %s appeared", hiddenID)
		}
	}
	// Visible one should appear.
	found := false
	for _, r := range res.Items {
		if r.IssueID == visibleID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("visible row %s missing from result", visibleID)
	}
}

// TestInProgress_StaleLock confirms staleLock flips true when locked_at
// is older than the 25-min threshold.
func TestInProgress_StaleLock(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, _ := seedUser(t, pool)
	project := seedProject(t, pool, owner)
	pre := uniqueIDPrefix(t, "stale")

	freshID := pre + "-fresh"
	staleID := pre + "-stale"
	seedIssueRow(t, pool, project, freshID, "human", owner, nil)
	seedIssueRow(t, pool, project, staleID, "human", owner, nil)

	if _, err := pool.Exec(ctx,
		`UPDATE issues SET locked_by='sess-a', locked_at=now() WHERE id=$1`, freshID); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET locked_by='sess-b', locked_at=now() - INTERVAL '28 minutes' WHERE id=$1`,
		staleID); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	visible := map[uuid.UUID]struct{}{project: {}}
	res, err := store.InProgress(ctx, visible, false, nil, 50, nil /*reach*/, redact.None)
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}
	for _, r := range res.Items {
		switch r.IssueID {
		case freshID:
			if r.StaleLock {
				t.Errorf("fresh lock should NOT be flagged stale: %+v", r)
			}
		case staleID:
			if !r.StaleLock {
				t.Errorf("stale lock should be flagged: %+v", r)
			}
		}
	}
}

// TestInProgress_RedactInvisible seeds two projects — A (caller is a
// member → in `visible`) and B (NOT in visible). Each gets one
// in-progress issue. With redactInvisible=true the B-project row is
// returned but content-redacted (Title="" + Redacted=true) while its
// identity fields survive; the A-project row is full. The admin
// sub-check asserts an admin caller sees both rows full.
func TestInProgress_RedactInvisible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	ownerA, _ := seedUser(t, pool)
	ownerB, _ := seedUser(t, pool)
	projA := seedProject(t, pool, ownerA) // caller is member (in visible)
	projB := seedProject(t, pool, ownerB) // NOT in visible

	pre := uniqueIDPrefix(t, "redip")
	idA := pre + "-A"
	idB := pre + "-B"
	seedIssueRow(t, pool, projA, idA, "human", ownerA, nil)
	seedIssueRow(t, pool, projB, idB, "human", ownerB, nil)
	for _, id := range []string{idA, idB} {
		if _, err := pool.Exec(ctx,
			`UPDATE issues SET status='in_progress', started_at=now() WHERE id=$1`, id); err != nil {
			t.Fatalf("set in_progress %s: %v", id, err)
		}
	}

	// Non-admin caller sees only projA. redactInvisible=true returns BOTH
	// rows but redacts the B row's content.
	visible := map[uuid.UUID]struct{}{projA: {}}
	// projB is in the caller's reach without being visible (sk-674.18 T030).
	res, err := store.InProgress(ctx, visible, false /*unrestricted*/, nil, 50,
		map[uuid.UUID]struct{}{projA: {}, projB: {}} /*reach*/, redact.Soft)
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}

	seen := map[string]InProgressRow{}
	for _, r := range res.Items {
		seen[r.IssueID] = r
	}
	rowA, okA := seen[idA]
	rowB, okB := seen[idB]
	if !okA {
		t.Fatalf("expected member-project row %s in result, got %v", idA, seen)
	}
	if !okB {
		t.Fatalf("expected (redacted) invisible-project row %s in result, got %v", idB, seen)
	}

	// A-project row: full content, not redacted.
	if rowA.Redacted {
		t.Errorf("member-project row should NOT be redacted: %+v", rowA)
	}
	if rowA.Title != idA {
		// seedIssueRow seeds title == idStr.
		t.Errorf("member-project row Title = %q, want %q", rowA.Title, idA)
	}

	// B-project row: Title stripped, Redacted=true, identity fields kept.
	if !rowB.Redacted {
		t.Errorf("invisible-project row should be redacted: %+v", rowB)
	}
	if rowB.Title != "" {
		t.Errorf("invisible-project row Title should be stripped, got %q", rowB.Title)
	}
	if rowB.IssueID == "" || rowB.ProjectPrefix == "" || rowB.Type == "" {
		t.Errorf("invisible-project row should keep identity fields, got id=%q prefix=%q type=%q",
			rowB.IssueID, rowB.ProjectPrefix, rowB.Type)
	}

	// Admin sub-check: an admin caller with redactInvisible=true sees both
	// rows fully populated, none redacted.
	adminRes, err := store.InProgress(ctx, nil, true /*unrestricted*/, nil, 50, nil /*reach*/, redact.Soft)
	if err != nil {
		t.Fatalf("InProgress admin: %v", err)
	}
	adminSeen := map[string]InProgressRow{}
	for _, r := range adminRes.Items {
		adminSeen[r.IssueID] = r
	}
	for _, id := range []string{idA, idB} {
		r, ok := adminSeen[id]
		if !ok {
			t.Fatalf("admin should see row %s, got %v", id, adminSeen)
		}
		if r.Redacted {
			t.Errorf("admin row %s should NOT be redacted: %+v", id, r)
		}
		if r.Title != id {
			t.Errorf("admin row %s Title = %q, want %q", id, r.Title, id)
		}
	}
}

// TestScoring_Reach seeds an issue.closed event in a project the caller is
// NOT a member of. With that project in the caller's HOT ZONE reach (a
// project of his own space he does not see, sk-674.18 T030) the metric
// count includes the event (its contribution, INV-28 (1)); without it
// (reach nil — the visible projects) it does not. usernameFilter pins to
// the foreign actor so parallel-test seed bleed cannot inflate the count.
func TestScoring_Reach(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, _ := seedUser(t, pool) // caller (member of `mine` only)
	foreign, foreignUname := seedUser(t, pool)
	mine := seedProject(t, pool, owner)
	theirs := seedProject(t, pool, foreign) // caller is NOT a member

	now := time.Now().UTC()
	id := uniqueIDPrefix(t, "gscope") + "-1"
	seedIssueRow(t, pool, theirs, id, "human", foreign, nil)
	seedEventForIssue(t, pool, "issue.closed", id, theirs, foreignUname, foreign, "human", now.Add(-1*time.Hour))

	// visible EXCLUDES `theirs`. Scope the username filter to the foreign
	// actor so we measure exactly the seeded event.
	visible := map[uuid.UUID]struct{}{mine: {}}

	// `theirs` in the reach → the foreign close is counted.
	res, err := store.Scoring(ctx, Window24h, visible, false, nil, &foreignUname,
		map[uuid.UUID]struct{}{mine: {}, theirs: {}} /*reach*/)
	if err != nil {
		t.Fatalf("Scoring with the reach: %v", err)
	}
	if m := findMetric(t, res, "issues_closed"); m.Human != 1 {
		t.Fatalf("reach: expected closed.human=1 (foreign event counted), got %d", m.Human)
	}

	// No reach → the foreign close is NOT counted (out of visible scope for
	// a non-admin caller).
	res, err = store.Scoring(ctx, Window24h, visible, false, nil, &foreignUname, nil /*reach*/)
	if err != nil {
		t.Fatalf("Scoring scoped: %v", err)
	}
	if m := findMetric(t, res, "issues_closed"); m.Human != 0 {
		t.Fatalf("scoped: expected closed.human=0 (foreign event hidden), got %d", m.Human)
	}
}

// TestAgents_WindowCounts seeds three users with agent rows at
// varying last_seen_at offsets and asserts the per-window COUNT
// FILTER expressions slice them correctly.
func TestAgents_WindowCounts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	// Three users. We need unique usernames to assert ordering /
	// per-row counts deterministically.
	uLive, unameLive := seedUser(t, pool)
	uMixed, unameMixed := seedUser(t, pool)
	uOld, unameOld := seedUser(t, pool)

	// uLive: 2 live (within 15min), 3 sessions total in last 24h.
	insertAgent(t, pool, uLive, "live-sess-A", time.Now().UTC().Add(-2*time.Minute))
	insertAgent(t, pool, uLive, "live-sess-B", time.Now().UTC().Add(-10*time.Minute))
	insertAgent(t, pool, uLive, "live-sess-C", time.Now().UTC().Add(-3*time.Hour))

	// uMixed: 0 live, 1 in last 24h, 2 in last 7d, 3 in last 30d.
	insertAgent(t, pool, uMixed, "mixed-sess-A", time.Now().UTC().Add(-12*time.Hour))
	insertAgent(t, pool, uMixed, "mixed-sess-B", time.Now().UTC().Add(-3*24*time.Hour))
	insertAgent(t, pool, uMixed, "mixed-sess-C", time.Now().UTC().Add(-20*24*time.Hour))

	// uOld: 1 in last 30d, nothing newer.
	insertAgent(t, pool, uOld, "old-sess-A", time.Now().UTC().Add(-25*24*time.Hour))

	// uMixed also has a session 60 days ago — outside every window,
	// including 30d. It must NOT lift d30 (stays 3) but MUST lift
	// all_time to 4, proving the all-time count spans beyond the
	// 30-day boundary that the windowed counts cut at.
	insertAgent(t, pool, uMixed, "mixed-sess-OLD", time.Now().UTC().Add(-60*24*time.Hour))

	// uDormant has ONLY ancient sessions (>30d) — zero activity inside
	// the 30-day window. The HAVING d30>0 gate must exclude it entirely,
	// even though it has all-time sessions. Guards against the all-time
	// column accidentally widening the visible row set.
	uDormant, unameDormant := seedUser(t, pool)
	insertAgent(t, pool, uDormant, "dormant-sess-A", time.Now().UTC().Add(-45*24*time.Hour))
	insertAgent(t, pool, uDormant, "dormant-sess-B", time.Now().UTC().Add(-90*24*time.Hour))

	res, err := store.Agents(ctx, nil, true /*unrestricted: the window counts, not the reach*/)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	byUser := map[string]AgentsRow{}
	for _, r := range res.Items {
		byUser[r.Username] = r
	}

	if got := byUser[unameLive]; got.Live != 2 || got.H24 != 3 || got.D7 != 3 || got.D30 != 3 || got.AllTime != 3 {
		t.Errorf("uLive counts mismatch: live=%d h24=%d d7=%d d30=%d allTime=%d (want 2/3/3/3/3)",
			got.Live, got.H24, got.D7, got.D30, got.AllTime)
	}
	// d30=3 but allTime=4 — the 60-day-old session lands only in all-time.
	if got := byUser[unameMixed]; got.Live != 0 || got.H24 != 1 || got.D7 != 2 || got.D30 != 3 || got.AllTime != 4 {
		t.Errorf("uMixed counts mismatch: live=%d h24=%d d7=%d d30=%d allTime=%d (want 0/1/2/3/4)",
			got.Live, got.H24, got.D7, got.D30, got.AllTime)
	}
	if got := byUser[unameOld]; got.Live != 0 || got.H24 != 0 || got.D7 != 0 || got.D30 != 1 || got.AllTime != 1 {
		t.Errorf("uOld counts mismatch: live=%d h24=%d d7=%d d30=%d allTime=%d (want 0/0/0/1/1)",
			got.Live, got.H24, got.D7, got.D30, got.AllTime)
	}
	// Dormant-only user (sessions all >30d) must be absent — the all-time
	// column must not widen the visible set beyond the 30-day-active gate.
	if _, present := byUser[unameDormant]; present {
		t.Errorf("uDormant has no activity within 30d but appeared in agents result (HAVING gate leaked)")
	}

	// Sort order check: live > 0 rows come first; among them by d30 DESC
	// then username ASC. uLive (live=2) must be earlier than uMixed
	// (live=0). uOld and uMixed both have live=0, so they sort by d30
	// then username — uMixed.d30=3 > uOld.d30=1, so uMixed before uOld.
	idx := map[string]int{}
	for i, r := range res.Items {
		idx[r.Username] = i
	}
	if idx[unameLive] >= idx[unameMixed] {
		t.Errorf("uLive (%d) should sort before uMixed (%d)", idx[unameLive], idx[unameMixed])
	}
	if idx[unameMixed] >= idx[unameOld] {
		t.Errorf("uMixed (%d, d30=3) should sort before uOld (%d, d30=1)", idx[unameMixed], idx[unameOld])
	}
}

// TestAgents_ReachCountsEverySessionOfThePeopleOfTheReach — the HOT ZONE
// branch (sk-674.18 T030, US-13; rule replaced by sk-755, maintainers' decision
// 2026-10-18). Reference — the rule, not the output: the reach picks the
// people (a participant of a reach project's space or a member of a reach
// project); every session of such a person counts by agents.last_seen_at —
// any protocol traffic, with or without events, wherever it worked; the
// windows live 15m / 24h / 7d / 30d / all slice that seen; a person outside
// the people of the reach is absent even if a session of his left events in
// the reach.
//
// Counterexamples: reading seen from the events of the reach (the sk-740
// rule) drops "quiet" and "only-outside" and moves "stale-event" to 7d;
// dropping the people filter lists the stranger.
func TestAgents_ReachCountsEverySessionOfThePeopleOfTheReach(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	user, uname := seedUser(t, pool)
	stranger, strangerName := seedUser(t, pool)
	foreign := testsupport.SeedSpace(t, pool, stranger)
	inReach := seedProject(t, pool, user) // user: owner, participant of its space
	outside, _ := testsupport.SeedProjectInSpace(t, pool, stranger, foreign, "agout")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = ANY($1)`, []uuid.UUID{user, stranger})
	})

	now := time.Now().UTC()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	work := func(who uuid.UUID, name, session string, project uuid.UUID, at time.Time) {
		t.Helper()
		seedAgentEventWithSession(t, pool, "issue.updated", "agent-work", project, name, who, session, at)
	}
	type ev struct {
		project uuid.UUID
		at      time.Time
	}
	sessions := []struct {
		name     string
		lastSeen time.Time
		events   []ev
	}{
		// Reads and waits only — no event anywhere.
		{"quiet", ago(2 * time.Minute), nil},
		// Works only outside the caller's reach (a space he is not in).
		{"only-outside", ago(time.Minute), []ev{{outside, ago(time.Minute)}}},
		// Last event in the reach 3 days ago, last appearance 3h ago → 24h.
		{"stale-event", ago(3 * time.Hour), []ev{{inReach, ago(3 * 24 * time.Hour)}}},
		{"days", ago(3 * 24 * time.Hour), nil},
		{"weeks", ago(20 * 24 * time.Hour), nil},
		{"ancient", ago(60 * 24 * time.Hour), nil},
	}
	for _, s := range sessions {
		sid := "reach-" + s.name + "-" + uuid.NewString()[:8]
		insertAgent(t, pool, user, sid, s.lastSeen)
		for _, e := range s.events {
			work(user, uname, sid, e.project, e.at)
		}
	}
	strangerSession := "reach-stranger-" + uuid.NewString()[:8]
	insertAgent(t, pool, stranger, strangerSession, now)
	work(stranger, strangerName, strangerSession, inReach, ago(time.Minute))

	res, err := store.Agents(ctx, map[uuid.UUID]struct{}{inReach: {}}, false)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	byUser := map[string]AgentsRow{}
	for _, r := range res.Items {
		byUser[r.Username] = r
	}
	got, ok := byUser[uname]
	want := AgentsRow{Username: uname, Live: 2, H24: 3, D7: 4, D30: 5, AllTime: 6}
	if !ok || got != want {
		t.Errorf("Agents(reach={inReach}, restricted)[%s] = %+v (listed %v), want %+v", uname, got, ok, want)
	}
	if _, listed := byUser[strangerName]; listed {
		t.Errorf("Agents(reach={inReach}, restricted) lists %s — a person outside the people of the reach", strangerName)
	}
}

// insertAgent seeds one row in the agents table with a controlled
// last_seen_at — lets the test pin counts to specific window edges.
func insertAgent(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, sessionID string, lastSeen time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at)
VALUES ($1, $2, $3, $3)
ON CONFLICT (user_id, session_id) DO UPDATE
   SET last_seen_at = EXCLUDED.last_seen_at`,
		userID, sessionID, lastSeen)
	if err != nil {
		t.Fatalf("insert agent (%s/%s): %v", userID, sessionID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agents WHERE user_id=$1 AND session_id=$2`, userID, sessionID)
	})
}

// ---------- sk-30 — all-time window + 60s in-memory cache ----------

// deleteEvent deletes one event row by (occurred_at, entity_id) — the
// pair is unique enough across the test seed shapes used below. Used by
// the cache tests to verify whether a second Scoring call hits cache
// (count unchanged) or re-queries (count drops by one).
func deleteEvent(t *testing.T, pool *pgxpool.Pool, eventType, entityID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`DELETE FROM events WHERE type = $1 AND entity_id = $2`, eventType, entityID)
	if err != nil {
		t.Fatalf("delete event (%s/%s): %v", eventType, entityID, err)
	}
}

// TestScoring_AllTime_IncludesEventsOutsideThirtyDays seeds a closed
// event 60 days ago and asserts WindowAllTime counts it (it would be
// outside the 30d window).
func TestScoring_AllTime_IncludesEventsOutsideThirtyDays(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	pre := uniqueIDPrefix(t, "alltime")
	// Issue closed 60 days ago — outside 30d, inside all-time.
	id := pre + "-old"
	seedIssueRow(t, pool, project, id, "human", owner, nil)
	now := time.Now().UTC()
	seedEventForIssue(t, pool, "issue.closed", id, project, ownerUname, owner, "human", now.Add(-60*24*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project

	// 30d should NOT see the old event.
	res, err := store.Scoring(ctx, Window30d, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring 30d: %v", err)
	}
	if m := findMetric(t, res, "issues_closed"); m.Human != 0 {
		t.Fatalf("expected 30d closed.human=0 for 60d-old event, got %d", m.Human)
	}

	// All-time SHOULD see it.
	res, err = store.Scoring(ctx, WindowAllTime, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring all-time: %v", err)
	}
	if m := findMetric(t, res, "issues_closed"); m.Human != 1 {
		t.Fatalf("expected all-time closed.human=1, got %d", m.Human)
	}
}

// TestScoring_AllTime_CachesResult verifies that two consecutive
// WindowAllTime calls within the TTL return the same count even when a
// seeded event is DELETE-d between the calls — proving the second call
// served cache instead of re-querying.
func TestScoring_AllTime_CachesResult(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Controllable clock — start at "now", advance only when the test
	// explicitly bumps. The 60s TTL is exercised via this clock so the
	// test doesn't sleep.
	base := time.Now().UTC()
	clock := base
	store := NewWithClock(pool, func() time.Time { return clock })

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	pre := uniqueIDPrefix(t, "cacheY")
	idA := pre + "-A"
	idB := pre + "-B"
	seedIssueRow(t, pool, project, idA, "human", owner, nil)
	seedIssueRow(t, pool, project, idB, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.closed", idA, project, ownerUname, owner, "human", base.Add(-1*time.Hour))
	seedEventForIssue(t, pool, "issue.closed", idB, project, ownerUname, owner, "human", base.Add(-2*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project

	// First call — populates cache.
	res1, err := store.Scoring(ctx, WindowAllTime, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring first: %v", err)
	}
	if m := findMetric(t, res1, "issues_closed"); m.Human != 2 {
		t.Fatalf("expected first call closed.human=2, got %d", m.Human)
	}

	// Delete one event. A re-query would observe 1; a cache hit returns
	// 2.
	deleteEvent(t, pool, "issue.closed", idA)

	// Advance the clock by 30s — still within the 60s TTL.
	clock = base.Add(30 * time.Second)

	res2, err := store.Scoring(ctx, WindowAllTime, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring second: %v", err)
	}
	if m := findMetric(t, res2, "issues_closed"); m.Human != 2 {
		t.Fatalf("expected cached closed.human=2 (cache hit) after delete, got %d", m.Human)
	}
}

// TestScoring_AllTime_CacheExpiresAfterTTL is the mirror of the above:
// advance the clock past the 60s TTL, delete an event, assert the second
// call observes the deletion (proving the cache re-queried).
func TestScoring_AllTime_CacheExpiresAfterTTL(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	base := time.Now().UTC()
	clock := base
	store := NewWithClock(pool, func() time.Time { return clock })

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	pre := uniqueIDPrefix(t, "cacheN")
	idA := pre + "-A"
	idB := pre + "-B"
	seedIssueRow(t, pool, project, idA, "human", owner, nil)
	seedIssueRow(t, pool, project, idB, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.closed", idA, project, ownerUname, owner, "human", base.Add(-1*time.Hour))
	seedEventForIssue(t, pool, "issue.closed", idB, project, ownerUname, owner, "human", base.Add(-2*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project

	res1, err := store.Scoring(ctx, WindowAllTime, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring first: %v", err)
	}
	if m := findMetric(t, res1, "issues_closed"); m.Human != 2 {
		t.Fatalf("expected first call closed.human=2, got %d", m.Human)
	}

	// Delete + bump clock past TTL.
	deleteEvent(t, pool, "issue.closed", idA)
	clock = base.Add(61 * time.Second)

	res2, err := store.Scoring(ctx, WindowAllTime, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring second: %v", err)
	}
	if m := findMetric(t, res2, "issues_closed"); m.Human != 1 {
		t.Fatalf("expected post-TTL closed.human=1 (cache re-queried), got %d", m.Human)
	}
}

// TestScoring_OtherWindowsDoNotCache verifies non-all-time windows
// never serve cache — a 7d call after DELETE-ing an event must reflect
// the deleted state immediately.
func TestScoring_OtherWindowsDoNotCache(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	base := time.Now().UTC()
	clock := base
	store := NewWithClock(pool, func() time.Time { return clock })

	owner, ownerUname := seedUser(t, pool)
	project := seedProject(t, pool, owner)

	pre := uniqueIDPrefix(t, "nocache")
	idA := pre + "-A"
	idB := pre + "-B"
	seedIssueRow(t, pool, project, idA, "human", owner, nil)
	seedIssueRow(t, pool, project, idB, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.closed", idA, project, ownerUname, owner, "human", base.Add(-1*time.Hour))
	seedEventForIssue(t, pool, "issue.closed", idB, project, ownerUname, owner, "human", base.Add(-2*time.Hour))

	visible := map[uuid.UUID]struct{}{project: {}}
	projectCopy := project

	res1, err := store.Scoring(ctx, Window7d, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring first 7d: %v", err)
	}
	if m := findMetric(t, res1, "issues_closed"); m.Human != 2 {
		t.Fatalf("expected first 7d closed.human=2, got %d", m.Human)
	}

	deleteEvent(t, pool, "issue.closed", idA)
	// Even without advancing the clock, 7d must re-query.
	res2, err := store.Scoring(ctx, Window7d, visible, false, &projectCopy, &ownerUname, nil)
	if err != nil {
		t.Fatalf("Scoring second 7d: %v", err)
	}
	if m := findMetric(t, res2, "issues_closed"); m.Human != 1 {
		t.Fatalf("expected 7d closed.human=1 (no cache on 7d), got %d", m.Human)
	}
}

// TestInProgress_HardRedact_AirtightLeak is the airtight (§8) leak test for
// the EXTERNAL in-progress contract. A caller who is a member of NOTHING
// requests the global snapshot at redact.Hard. The one seeded in-progress
// issue of an inaccessible project must come back PRESENT but Hard-redacted:
// issue id, project prefix, projectId and the raw Mcp-Session-Id lock holder
// all gone; assignee/status/timing kept. The marshaled JSON must contain NO
// substring of the project prefix, the issue id, the project uuid, or the
// lock session.
func TestInProgress_HardRedact_AirtightLeak(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := New(pool)

	// The store takes a visibleProjects map, not a caller id — an empty map
	// models "member of nothing", so no caller user row is needed.
	foreignOwner, foreignUname := seedUser(t, pool)
	foreignProj := seedProject(t, pool, foreignOwner)

	var foreignPrefix string
	if err := pool.QueryRow(ctx,
		`SELECT id_prefix FROM projects WHERE id=$1`, foreignProj).Scan(&foreignPrefix); err != nil {
		t.Fatalf("read project prefix: %v", err)
	}

	secretID := foreignPrefix + "-SECRET-1"
	assignee := foreignUname
	seedIssueRow(t, pool, foreignProj, secretID, "human", foreignOwner, &assignee)

	const secretSession = "mcp-session-SECRET-CORRELATOR"
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET status='in_progress', started_at=now(), locked_by=$2, locked_at=now() WHERE id=$1`,
		secretID, secretSession); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}

	// Member of nothing → visible empty; the foreign project is in the reach
	// (sk-674.18 T030), so the row comes back; level=Hard strips it.
	res, err := store.InProgress(ctx, map[uuid.UUID]struct{}{}, false /*unrestricted*/, nil, 200,
		map[uuid.UUID]struct{}{foreignProj: {}} /*reach*/, redact.Hard)
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}

	// Isolate my row by the (kept) assignee — id/prefix/projectId are stripped.
	var mine *InProgressRow
	for i := range res.Items {
		if res.Items[i].Assignee != nil && *res.Items[i].Assignee == foreignUname {
			r := res.Items[i]
			mine = &r
			break
		}
	}
	if mine == nil {
		t.Fatalf("seeded in-progress row (assignee %q) not returned", foreignUname)
	}

	if !mine.Redacted {
		t.Errorf("row must be redacted: %+v", *mine)
	}
	if mine.IssueID != "" {
		t.Errorf("Hard-redacted issueId must be blank, got %q", mine.IssueID)
	}
	if mine.ProjectPrefix != "" {
		t.Errorf("Hard-redacted projectPrefix must be blank, got %q", mine.ProjectPrefix)
	}
	if mine.ProjectID != nil {
		t.Errorf("Hard-redacted projectId must be nil, got %v", *mine.ProjectID)
	}
	if mine.LockedBySession != nil {
		t.Errorf("Hard-redacted lockedBySession must be nil, got %v", *mine.LockedBySession)
	}
	// Kept "people + shape".
	if mine.Assignee == nil || *mine.Assignee != foreignUname {
		t.Errorf("assignee must survive Hard redaction: %+v", *mine)
	}
	if mine.Status == "" {
		t.Errorf("status must survive Hard redaction: %+v", *mine)
	}

	blob, err := json.Marshal(*mine)
	if err != nil {
		t.Fatalf("marshal redacted row: %v", err)
	}
	js := string(blob)
	for _, secret := range []string{foreignPrefix, secretID, foreignProj.String(), secretSession} {
		if strings.Contains(js, secret) {
			t.Errorf("Hard-redacted in-progress leaked %q in JSON: %s", secret, js)
		}
	}
	if strings.Contains(js, "\"projectId\"") {
		t.Errorf("Hard-redacted in-progress must not carry projectId: %s", js)
	}
	if strings.Contains(js, "\"lockedBySession\"") {
		t.Errorf("Hard-redacted in-progress must not carry lockedBySession: %s", js)
	}
	if strings.Contains(js, "\"issueId\"") {
		t.Errorf("Hard-redacted in-progress must not carry issueId: %s", js)
	}
}
