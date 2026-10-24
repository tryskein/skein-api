package attachments

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "atu" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "atttest_" + suffix
	prefix := "att" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
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
	_, err = pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed project counter: %v", err)
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
	return id, prefix
}

func seedIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		idStr, projectID, title, creator)
	if err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

func TestStore_CreatePending_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "test")

	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	att, err := store.CreatePending(context.Background(),
		id, iid, nil, "screenshot.png", "image/png", 1234,
		"skein-attachments", BuildS3Key(pid, iid, id, "screenshot.png"),
		owner)
	if err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	if att.ID != id {
		t.Fatalf("got id %s, want %s", att.ID, id)
	}
	if att.Status != StatusPending {
		t.Fatalf("got status %s, want pending", att.Status)
	}
}

func TestStore_CreatePending_IssueNotFound(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	pid := uuid.New()
	_, err := store.CreatePending(context.Background(),
		id, "no-such-issue", nil, "f.png", "image/png", 1,
		"b", BuildS3Key(pid, "no-such-issue", id, "f.png"),
		owner)
	if !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("expected ErrIssueNotFound, got %v", err)
	}
}

func TestStore_Confirm_FlipsStatusAndIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "test")

	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	if _, err := store.CreatePending(context.Background(),
		id, iid, nil, "f.png", "image/png", 1,
		"b", BuildS3Key(pid, iid, id, "f.png"), owner); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	actor := Actor{UserID: owner, Username: "tester", Role: "human"}
	out, err := store.Confirm(context.Background(), id, owner, "user", actor)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if out.Status != StatusReady {
		t.Fatalf("first confirm: got status %s, want ready", out.Status)
	}

	// Second confirm — idempotent.
	out2, err := store.Confirm(context.Background(), id, owner, "user", actor)
	if err != nil {
		t.Fatalf("Confirm idempotent: %v", err)
	}
	if out2.Status != StatusReady {
		t.Fatalf("second confirm: got status %s, want ready", out2.Status)
	}

	// Verify exactly one attachment.added event was emitted — recorded ON THE
	// ISSUE (sk-303.13 / INV-13), with the attachment id in the payload.
	// Recording it on the attachment entity is what left the "Attachment" reason
	// with an empty card and an uncountable badge on every reading surface.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'attachment.added'
   AND payload->>'id' = $2`,
		iid, id.String()).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 attachment.added event, got %d", n)
	}
}

func TestStore_ListByIssue_OnlyReady(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "test")

	store := NewStore(pool)
	actor := Actor{UserID: owner, Username: "tester", Role: "human"}

	// Two ready, one pending.
	for i := 0; i < 3; i++ {
		id := uuid.Must(uuid.NewV7())
		if _, err := store.CreatePending(context.Background(),
			id, iid, nil, "f.png", "image/png", 1,
			"b", BuildS3Key(pid, iid, id, "f.png"), owner); err != nil {
			t.Fatalf("CreatePending: %v", err)
		}
		if i < 2 {
			if _, err := store.Confirm(context.Background(), id, owner, "user", actor); err != nil {
				t.Fatalf("Confirm: %v", err)
			}
		}
	}

	res, err := store.ListByIssue(context.Background(), iid)
	if err != nil {
		t.Fatalf("ListByIssue: %v", err)
	}
	if res.Total != 2 {
		t.Fatalf("expected 2 ready, got %d", res.Total)
	}
}

func TestStore_Delete_HardDeletesRow(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "test")

	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	if _, err := store.CreatePending(context.Background(),
		id, iid, nil, "f.png", "image/png", 1,
		"skein-attachments", BuildS3Key(pid, iid, id, "f.png"), owner); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	bucket, key, err := store.Delete(context.Background(), id, owner, "user", Actor{
		UserID: owner, Username: "tester", Role: "human",
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if bucket != "skein-attachments" || key == "" {
		t.Fatalf("expected bucket+key from Delete, got %q %q", bucket, key)
	}
	if _, err := store.Get(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Delete, got %v", err)
	}
}

func TestStore_ListStalePending(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "test")

	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	if _, err := store.CreatePending(context.Background(),
		id, iid, nil, "f.png", "image/png", 1,
		"b", BuildS3Key(pid, iid, id, "f.png"), owner); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	// Backdate the row.
	if _, err := pool.Exec(context.Background(),
		`UPDATE attachments SET uploaded_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	atts, _, _, err := store.ListStalePending(context.Background(), 1*time.Hour, 10)
	if err != nil {
		t.Fatalf("ListStalePending: %v", err)
	}
	found := false
	for _, a := range atts {
		if a.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected stale row %s in result, got %d rows", id, len(atts))
	}
}

// TestStore_Confirm_AttachmentNotificationResolvesToTheIssue — sk-303.13 /
// T009, coverage-matrix row 17 (INV-13).
//
// "Attachment" is a feed-capable reason, so every reading surface has to be able
// to show its notification AS A TASK: a card that names the issue and leads to
// it, and a badge that counts it. The surfaces resolve a notification through
// events.entity_type/entity_id, and they understand issues and comments —
// which is why an event recorded on the attachment entity produced "an empty
// card and an uncountable badge".
//
// The assertion is therefore the join the readers actually perform: from the
// notification row, through the event, to the ISSUE — and it must find the
// issue's title without any reader learning a third entity type. The
// attachment's own identity is not lost: it travels in the payload.
func TestStore_Confirm_AttachmentNotificationResolvesToTheIssue(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	watcher := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "issue with a file")

	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, watcher, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual'], now())`, iid, watcher); err != nil {
		t.Fatalf("seed watcher: %v", err)
	}
	// "Attachment" is off in the narrow default; the test is about the
	// projection, so the reason is switched on explicitly.
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'attachment', true, false)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = true`, watcher); err != nil {
		t.Fatalf("enable attachment reason: %v", err)
	}

	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	if _, err := store.CreatePending(ctx,
		id, iid, nil, "diagram.png", "image/png", 42,
		"b", BuildS3Key(pid, iid, id, "diagram.png"), owner); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	if _, err := store.Confirm(ctx, id, owner, "user",
		Actor{UserID: owner, Username: "tester", Role: "human"}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	// The reader's join: notification → event → issue.
	var (
		issueTitle      string
		reason          *string
		attachInPayload string
	)
	err := pool.QueryRow(ctx, `
SELECT i.title, n.reason, e.payload->>'id'
  FROM notifications n
  JOIN events e ON e.id = n.event_id
  JOIN issues i ON e.entity_type = 'issue' AND i.id = e.entity_id
 WHERE n.user_id = $1 AND e.type = 'attachment.added' AND e.entity_id = $2`,
		watcher, iid).Scan(&issueTitle, &reason, &attachInPayload)
	if err != nil {
		t.Fatalf("an attachment notification did not resolve to its issue: %v", err)
	}
	if issueTitle != "issue with a file" {
		t.Errorf("card title = %q, want the issue's title", issueTitle)
	}
	if reason == nil || *reason != "attachment" {
		t.Errorf("reason = %v, want \"attachment\"", reason)
	}
	if attachInPayload != id.String() {
		t.Errorf("payload.id = %q, want the attachment id %s — projecting onto the issue must not lose it",
			attachInPayload, id)
	}
}

// TestStore_Confirm_SerializationFailure_AbsorbedOnce — sk-622.21, T012,
// matrix row No. 22a (Confirm part, regression only). R17 does not move Confirm
// — it already replays through this package's withSerializableRetry — but
// T012 changed the fan-out it runs, so the absorption of one conflict must
// still leave exactly one attachment.added event and exactly one
// notification for the watcher (the rolled-back first attempt duplicated
// nothing). Built as in issues/serialization_repro_test.go: a concurrent
// writer touches the attachment row Confirm flips, without changing what
// Confirm checks; Confirm blocks on it (the positive control polls
// pg_locks by the tagged application_name); the writer commits; the first
// attempt takes 40001.
func TestStore_Confirm_SerializationFailure_AbsorbedOnce(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	watcher := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "confirm under contention")
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, watcher, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual'], now())`, iid, watcher); err != nil {
		t.Fatalf("seed watcher: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'attachment', true, false)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = true`, watcher); err != nil {
		t.Fatalf("enable attachment reason: %v", err)
	}

	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	if _, err := store.CreatePending(ctx,
		id, iid, nil, "contended.png", "image/png", 7,
		"b", BuildS3Key(pid, iid, id, "contended.png"), owner); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}

	appName := "sk63415-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	opPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect tagged pool: %v", err)
	}
	t.Cleanup(opPool.Close)
	opStore := NewStore(opPool)

	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `UPDATE attachments SET filename = filename WHERE id = $1`, id); err != nil {
		t.Fatalf("writer touch: %v", err)
	}

	type confirmResult struct {
		att *Attachment
		err error
	}
	resCh := make(chan confirmResult, 1)
	go func() {
		a, err := opStore.Confirm(ctx, id, owner, "user", Actor{UserID: owner, Username: "tester", Role: "human"})
		resCh <- confirmResult{a, err}
	}()

	blocked := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE NOT l.granted AND a.application_name = $1`, appName).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatalf("Confirm never blocked on the writer's row lock — the race was not built")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}

	var got confirmResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("Confirm did not return within 30s")
	}
	if got.err != nil {
		t.Fatalf("Confirm surfaced %v after one concurrent writer", got.err)
	}
	if got.att == nil || got.att.Status != StatusReady {
		t.Fatalf("Confirm result = %+v, want status ready", got.att)
	}

	var events, rows int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM events
         WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'attachment.added' AND payload->>'id' = $2),
       (SELECT count(*) FROM notifications n JOIN events e ON e.id = n.event_id
         WHERE n.user_id = $3 AND e.type = 'attachment.added' AND e.entity_id = $1)`,
		iid, id.String(), watcher).Scan(&events, &rows); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if events != 1 {
		t.Errorf("attachment.added events = %d, want exactly 1", events)
	}
	if rows != 1 {
		t.Errorf("watcher's attachment notifications = %d, want exactly 1", rows)
	}
}
