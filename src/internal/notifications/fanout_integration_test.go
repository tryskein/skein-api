package notifications

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// Integration tests for the R3.3 FanOut extensions:
//   - 3-arm UNION (issue + project + custom watchers)
//   - project_watchers.event_types whitelist
//   - custom_watchers SQL-side filter eval (event_types, projects, types,
//     priority_max)
//   - per-user cap on custom watchers
//
// Tests skip when TEST_DATABASE_URL is unset so a developer running
// `go test ./...` without the local stack up doesn't see noise.

const testDBURL = "TEST_DATABASE_URL"

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDBURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDBURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedTestUser(t *testing.T, pool *pgxpool.Pool, role string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "tu" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-hash', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, role)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func seedTestProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "fp_" + suffix
	prefix := "fp" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      archived_at, created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, _ = pool.Exec(ctx, `
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

func seedTestIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr string, priority int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type,
                    priority, status, pinned, source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, 'test issue', '', 'task',
        $3, 'open', false, 'human', NULL, NULL,
        '', '', '',
        NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		idStr, projectID, priority, creator)
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
}

// addEvent inserts an events row directly (bypasses eventbus) so the
// fan-out can attach to it. Returns the bigint id.
func addEvent(t *testing.T, tx pgx.Tx, evtType, entityType, entityID string, projectID uuid.UUID, actorUserID uuid.UUID, actorType string) int64 {
	t.Helper()
	var id int64
	err := tx.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now(), $1, $2, $3, $4, 'tester', $5, $6, '{}'::jsonb)
RETURNING id`,
		evtType, entityType, entityID, projectID, actorUserID, actorType,
	).Scan(&id)
	if err != nil {
		t.Fatalf("add event: %v", err)
	}
	return id
}

// enableReasonChannels writes an explicit per-reason channel state for one
// user. Needed since sk-303.13: the fan-out now filters AT WRITE TIME against
// the user's reason preferences, and the narrow default leaves most of the
// subscription reasons ("New issue", "Comment", "Attachment", …) OFF.
// A test about the WATCHER ARMS must therefore say out loud that the reason
// itself is switched on, otherwise it proves nothing about the arm it names.
func enableReasonChannels(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, key ReasonKey, feed, push bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, reason) DO UPDATE
   SET feed_enabled = EXCLUDED.feed_enabled,
       push_enabled = EXCLUDED.push_enabled`, userID, string(key), feed, push)
	if err != nil {
		t.Fatalf("enable reason %s for %s: %v", key, userID, err)
	}
}

// TestFanOut_CustomWatcher_EventTypeFilter verifies a custom watcher
// receives notifications only for its whitelisted event types.
func TestFanOut_CustomWatcher_EventTypeFilter(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	// Subscriber must be a project member to pass the membership recheck.
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, subscriber, owner); err != nil {
		t.Fatalf("seed project_members: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`,
			projectID, subscriber)
	})

	issueID := "fpissue-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// Custom watcher: only comment.created on this project.
	wid := uuid.Must(uuid.NewRandom())
	filter := map[string]any{
		"event_types": []string{"comment.created"},
		"projects":    []string{projectID.String()},
	}
	fb, _ := json.Marshal(filter)
	_, err := pool.Exec(ctx, `
INSERT INTO custom_watchers (id, user_id, name, filter, created_at)
VALUES ($1, $2, 'tw1', $3::jsonb, now())`, wid, subscriber, fb)
	if err != nil {
		t.Fatalf("seed custom watcher: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM custom_watchers WHERE id = $1`, wid) })

	// Drive an issue.created event — should NOT match (not in whitelist).
	enableReasonChannels(t, pool, subscriber, ReasonKeyIssueCreated, true, false)
	enableReasonChannels(t, pool, subscriber, ReasonKeyComment, true, false)

	tx, _ := pool.Begin(ctx)
	eid := addEvent(t, tx, "issue.created", "issue", issueID, projectID, owner, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:     eid,
		EventType:   "issue.created",
		EntityType:  "issue",
		IssueID:     issueID,
		ProjectID:   projectID,
		ActorUserID: owner,
		ActorType:   "human",
	}); err != nil {
		t.Fatalf("fanout issue.created: %v", err)
	}
	_ = tx.Commit(ctx)

	var n int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		subscriber, eid).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 notifications for issue.created (not whitelisted), got %d", n)
	}

	// Drive a comment.created event — SHOULD match.
	commentUUID := uuid.Must(uuid.NewRandom()).String()
	_, _ = pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', 'hi', now(), now())`,
		commentUUID, issueID, owner)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, commentUUID) })

	tx2, _ := pool.Begin(ctx)
	eid2 := addEvent(t, tx2, "comment.created", "comment", commentUUID, projectID, owner, "human")
	if _, err := FanOut(ctx, tx2, FanOutInput{
		EventID:     eid2,
		EventType:   "comment.created",
		EntityType:  "comment",
		IssueID:     issueID,
		ProjectID:   projectID,
		ActorUserID: owner,
		ActorType:   "human",
	}); err != nil {
		t.Fatalf("fanout comment.created: %v", err)
	}
	_ = tx2.Commit(ctx)

	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		subscriber, eid2).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 notification for comment.created, got %d", n)
	}
}

// TestFanOut_ProjectWatcher_EventTypeWhitelist verifies that setting
// event_types on a project_watchers row scopes which event types fan
// out to that watcher.
func TestFanOut_ProjectWatcher_EventTypeWhitelist(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	_, _ = pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, subscriber, owner)

	// Subscribe with whitelist {issue.status_changed}. The whitelisted type
	// used to be issue.archived; the registry now calls archival audit-only,
	// so a whitelist built on it could no longer tell "filtered out by the
	// whitelist" from "this event delivers to nobody". issue.status_changed is
	// in the subscription catalogue and maps onto "Delegated issue
	// status", whose feed channel is on by default.
	store := NewStore(pool)
	if err := store.AddProjectWatch(ctx, projectID, subscriber); err != nil {
		t.Fatalf("AddProjectWatch: %v", err)
	}
	if err := store.SetProjectWatcherEventTypes(ctx, projectID, subscriber, []string{"issue.status_changed"}); err != nil {
		t.Fatalf("SetProjectWatcherEventTypes: %v", err)
	}
	// "New issue" is off by default; switch it on so the assertion below
	// isolates the whitelist and not the reason.
	enableReasonChannels(t, pool, subscriber, ReasonKeyIssueCreated, true, false)

	issueID := "fpwl-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// issue.created: not in whitelist → no notification.
	tx, _ := pool.Begin(ctx)
	eid := addEvent(t, tx, "issue.created", "issue", issueID, projectID, owner, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:     eid,
		EventType:   "issue.created",
		EntityType:  "issue",
		IssueID:     issueID,
		ProjectID:   projectID,
		ActorUserID: owner,
		ActorType:   "human",
	}); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	_ = tx.Commit(ctx)

	var n int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		subscriber, eid).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 notifications (not whitelisted), got %d", n)
	}

	// issue.status_changed: matches whitelist → 1 notification.
	tx2, _ := pool.Begin(ctx)
	eid2 := addEvent(t, tx2, "issue.status_changed", "issue", issueID, projectID, owner, "human")
	if _, err := FanOut(ctx, tx2, FanOutInput{
		EventID:     eid2,
		EventType:   "issue.status_changed",
		EntityType:  "issue",
		IssueID:     issueID,
		ProjectID:   projectID,
		ActorUserID: owner,
		ActorType:   "human",
	}); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	_ = tx2.Commit(ctx)

	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		subscriber, eid2).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 notification (whitelisted), got %d", n)
	}
}

// TestStore_AddCustomWatcher_CapEnforced verifies the per-user cap of
// 10 custom watchers is enforced in the SERIALIZABLE transaction.
func TestStore_AddCustomWatcher_CapEnforced(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	user := seedTestUser(t, pool, "user")
	store := NewStore(pool)

	for i := 0; i < MaxCustomWatchersPerUser; i++ {
		name := "cap_test_" + uuid.Must(uuid.NewRandom()).String()[:8]
		if _, err := store.AddCustomWatcher(ctx, user, CreateCustomWatcherInput{
			Name:   name,
			Filter: CustomWatcherFilter{},
		}); err != nil {
			t.Fatalf("AddCustomWatcher #%d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM custom_watchers WHERE user_id = $1`, user)
	})

	// 11th must fail.
	_, err := store.AddCustomWatcher(ctx, user, CreateCustomWatcherInput{
		Name:   "cap_test_overflow",
		Filter: CustomWatcherFilter{},
	})
	if err == nil {
		t.Fatalf("expected ErrTooManyWatchers; got nil")
	}
	if err != ErrTooManyWatchers {
		t.Errorf("expected ErrTooManyWatchers; got %v", err)
	}
}

// TestFanOut_AttachmentReachesIssueWatchers verifies that the attachment
// arm of the issue_watchers union fires for entityType="attachment".
// Regression: previously the issue arm short-circuited unless
// entityType was "issue" or "comment", so attachment.added events
// bypassed every issue watcher (assignee + commenter auto-watch +
// manual watchers all silent).
func TestFanOut_AttachmentReachesIssueWatchers(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	watcher := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, watcher, owner); err != nil {
		t.Fatalf("seed project_members: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`,
			projectID, watcher)
	})

	issueID := "fpatt-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// Subscribe via issue_watchers (the auto-watch path used for
	// assignee + commenter in production).
	tx0, _ := pool.Begin(ctx)
	if err := AddIssueWatcher(ctx, tx0, issueID, watcher, "manual"); err != nil {
		t.Fatalf("AddIssueWatcher: %v", err)
	}
	_ = tx0.Commit(ctx)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
			issueID, watcher)
	})

	// attachment.added event with entity_id = the attachment uuid (NOT
	// the issue id). The fan-out call passes issueID separately —
	// before the fix this was discarded for entityType="attachment".
	enableReasonChannels(t, pool, watcher, ReasonKeyAttachment, true, false)

	attachmentID := uuid.Must(uuid.NewRandom()).String()
	tx, _ := pool.Begin(ctx)
	eid := addEvent(t, tx, "attachment.added", "attachment", attachmentID, projectID, owner, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:     eid,
		EventType:   "attachment.added",
		EntityType:  "attachment",
		IssueID:     issueID,
		ProjectID:   projectID,
		ActorUserID: owner,
		ActorType:   "human",
	}); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	_ = tx.Commit(ctx)

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		watcher, eid).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 notification for issue_watcher on attachment.added, got %d", n)
	}
}

// TestFanOut_3ArmDedup verifies that a user matched by both project_watchers
// AND custom_watchers becomes ONE candidate and gets ONE notification row.
//
// sk-338 rewrote what it rests on. The old form asserted the row count only
// and credited it to «UNIQUE (user_id, event_id) deduplicates» — a claim the
// assertion could not check: the arms are deduplicated well before the INSERT
// (each arm is UNIONed and the outer SELECT groups by user), so the conflict
// the unique index guards against never arises here. Mutation confirmed it
// from both sides: deleting ON CONFLICT DO NOTHING left this test green, and
// so did deleting the candidate-level dedup. The negative was produced by
// whichever mechanism the mutation had left standing, never by the named one.
//
// Worse, the second arm was never there: the custom-watcher INSERT below was
// rejected by Postgres on every run and its error was discarded into `_`, so
// this «two arms» test only ever had one. Both are fixed here — the seed is
// checked, and the assertion moved to the candidate level, where a lost dedup
// is visible at all.
//
// What the candidate assertion can and cannot prove, stated so the next reader
// does not over-trust it: candidate-level dedup is itself doubled — each arm
// is UNIONed (set semantics) and the outer SELECT groups by user — so turning
// exactly one of the two off leaves the count at 1. It goes red when the dedup
// as a whole is gone (verified: UNION -> UNION ALL together with dropping the
// outer GROUP BY). The single-guard case lives in the matrix test for the
// subscription/role union, where only the GROUP BY stands between one
// candidate and two.
func TestFanOut_3ArmDedup(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	subscriber := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, subscriber, owner); err != nil {
		t.Fatalf("seed project member: %v", err)
	}

	issueID := "fpdedup-1"
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	// Both a project watch AND a custom watch on the same user / project.
	store := NewStore(pool)
	if err := store.AddProjectWatch(ctx, projectID, subscriber); err != nil {
		t.Fatalf("AddProjectWatch: %v", err)
	}
	wid := uuid.Must(uuid.NewRandom())
	filter := map[string]any{"projects": []string{projectID.String()}}
	fb, _ := json.Marshal(filter)
	// The name is built in Go on purpose. Its previous form — `'dup_' ||
	// $1::text` reusing the id parameter — made Postgres deduce $1 as both
	// uuid and text and rejected the whole INSERT with SQLSTATE 42P08, and the
	// error was discarded into `_`. The custom watcher therefore never
	// existed: this test matched ONE arm while claiming to test the dedup of
	// two (sk-338).
	if _, err := pool.Exec(ctx, `
INSERT INTO custom_watchers (id, user_id, name, filter, created_at)
VALUES ($1, $2, $3, $4::jsonb, now())`, wid, subscriber, "dup_"+wid.String(), fb); err != nil {
		t.Fatalf("seed custom watcher: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM custom_watchers WHERE id = $1`, wid) })

	enableReasonChannels(t, pool, subscriber, ReasonKeyIssueCreated, true, false)

	in := FanOutInput{
		EventType:   "issue.created",
		EntityType:  "issue",
		IssueID:     issueID,
		ProjectID:   projectID,
		ActorUserID: owner,
		ActorType:   "human",
	}

	// The dedup itself, where it happens: two matching subscription arms, one
	// candidate.
	matched := 0
	for _, c := range fanOutCandidatesFor(t, pool, in) {
		if c.userID == subscriber {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("candidates for a user matched by TWO arms = %d, want 1 — the arms must be "+
			"deduplicated before composition, not left to the INSERT", matched)
	}

	tx, _ := pool.Begin(ctx)
	eid := addEvent(t, tx, "issue.created", "issue", issueID, projectID, owner, "human")
	in.EventID = eid
	if _, err := FanOut(ctx, tx, in); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	_ = tx.Commit(ctx)

	var n int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND event_id = $2`,
		subscriber, eid).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 notification (deduped across arms), got %d", n)
	}
}

// TestListInbox_MentionsScope verifies the sk-38 scope='mentions'
// filter: unread mention-typed notifications (comment.mention /
// issue.mention). Excludes non-mention rows. Also verifies the
// MentionContext snippet hydration: comment.mention pulls comment body,
// issue.mention pulls the field named in payload.fields[0].
func TestListInbox_MentionsScope(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// userA is the caller (mention recipient). userB authors content.
	userA := seedTestUser(t, pool, "user")
	userB := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, userA)

	// Look up userA's username so we can embed @<username> in the
	// seeded comment/issue bodies — extractMentionContext keys off it.
	var userAName string
	if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, userA).Scan(&userAName); err != nil {
		t.Fatalf("lookup userA username: %v", err)
	}

	issueA := "fpmnA-1"
	commentBody := "Hey @" + userAName + " could you take a look at this proposal when you get a chance? Thanks."
	descBody := "Implementation plan: ping @" + userAName + " for design review before merging to main. Adds tests and docs."
	seedTestIssue(t, pool, projectID, userB, issueA, 2)

	// Patch the issue description so the issue.mention snippet hydration
	// has something to pull. seedTestIssue inserts empty defaults.
	if _, err := pool.Exec(ctx, `UPDATE issues SET description = $1 WHERE id = $2`, descBody, issueA); err != nil {
		t.Fatalf("patch issue description: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueA)
	})

	// Insert a comment row + comment.mention notification.
	commentID := uuid.New()
	if _, err := pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', $4, now(), now())`,
		commentID, issueA, userB, commentBody); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, commentID)
	})

	insertNotif := func(t *testing.T, evtType, entityType, entityID string, payload []byte) int64 {
		t.Helper()
		if payload == nil {
			payload = []byte("{}")
		}
		var eid int64
		if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now(), $1, $2, $3, $4, 'tester', $5, 'human', $6::jsonb)
RETURNING id`, evtType, entityType, entityID, projectID, userB, payload).Scan(&eid); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		var nid int64
		if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, $3, $4, now(), true)
RETURNING id`, userA, eid, evtType, projectID).Scan(&nid); err != nil {
			t.Fatalf("insert notification: %v", err)
		}
		return nid
	}

	nCommentMention := insertNotif(t, "comment.mention", "comment", commentID.String(), nil)
	nIssueMention := insertNotif(t, "issue.mention", "issue", issueA, []byte(`{"fields":["description"]}`))
	nOther := insertNotif(t, "comment.created", "issue", issueA, nil) // non-mention, must be excluded
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM notifications WHERE id IN ($1, $2, $3)`,
			nCommentMention, nIssueMention, nOther)
	})

	store := NewStore(pool)
	res, err := store.ListInbox(ctx, ListInboxArgs{
		UserID: userA,
		Scope:  "mentions",
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListInbox mentions: %v", err)
	}

	if len(res.Items) != 2 {
		t.Fatalf("expected exactly 2 mention items, got %d", len(res.Items))
	}

	// id DESC order — the most recently inserted (nIssueMention) appears
	// first; the comment-mention is the older row.
	var sawComment, sawIssue bool
	for _, it := range res.Items {
		switch it.Type {
		case "comment.mention":
			sawComment = true
			if !strings.Contains(it.MentionContext, "@"+userAName) {
				t.Errorf("comment.mention snippet missing @username marker: %q", it.MentionContext)
			}
		case "issue.mention":
			sawIssue = true
			if !strings.Contains(it.MentionContext, "@"+userAName) {
				t.Errorf("issue.mention snippet missing @username marker: %q", it.MentionContext)
			}
		default:
			t.Errorf("unexpected mention type in result: %s", it.Type)
		}
	}
	if !sawComment {
		t.Error("expected comment.mention in result")
	}
	if !sawIssue {
		t.Error("expected issue.mention in result")
	}
}
