// Package testsupport is a shared test-fixtures factory for skein-api
// integration tests. It is a NORMAL (non-_test) package so it can be
// imported from multiple packages' _test.go files, but it is imported
// ONLY by _test.go files — importing the testing package here is fine
// because it is never linked into the production binary.
//
// The factory funcs mirror the per-package seed helpers that grew up
// independently in internal/mcp/tools, internal/issues, internal/outbox,
// etc. (canonical copy: internal/mcp/tools/testhelper_test.go). They
// preserve the same semantics:
//   - random UUID / suffix isolation so concurrent packages never collide;
//   - t.Cleanup(DELETE) tears down child rows (issues are ON DELETE
//     RESTRICT, so SeedProject cleanup deletes them leaf-first);
//   - skip the test (via NewTestPool) when TEST_DATABASE_URL is unset.
//
// # Parallel-safety invariant (sk-105.2 / spec §2.2, §5 M7)
//
// Asserts must be scoped to the test's own project/entity. The
// project-scoped assert helpers here (CountEventsForProject,
// CountIssuesByStatus, ...) NEVER issue a bare global COUNT(*) or a
// WHERE type='...' predicate — they always filter by the seeded
// project_id (or entity_id) so a concurrent package's rows can never
// perturb the count.
//
// # No-background-goroutine invariant (spec §5 M7)
//
// This package starts NO background goroutine in test setup. The three
// global scanners — the outbox dispatcher, the stale-lock reaper
// (internal/lockreap), and the embedding backfill worker
// (internal/search) — all start only in cmd/api/main.go, never under
// `go test`. Tests must drive them synchronously via their exported
// tick()/runOnce() seams instead of spawning the production loop.
package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDatabaseURL is the env var an operator sets to point integration
// tests at a real Postgres instance.
const TestDatabaseURL = "TEST_DATABASE_URL"

// NewTestPool returns a *pgxpool.Pool against TEST_DATABASE_URL, or skips
// the test when the env var is unset. The pool is closed via t.Cleanup.
func NewTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(TestDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", TestDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", TestDatabaseURL, err)
	}
	// pgxpool defaults to max(4, NumCPU) connections, so the same suite had
	// 4 on a 2-vCPU CI runner and dozens on a workstation. Tests that park
	// transactions on row locks (account deletion, seat limits) hold one
	// connection per parked backend and still need a free one to observe
	// the wait; with 4 the observer queued forever behind its own holders.
	// An explicit size makes the suite independent of the machine.
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", TestDatabaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping %s: %v", TestDatabaseURL, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SeedUser inserts a fresh users row with a random uuid + username and
// registers cleanup. Returns the (id, username) pair. role defaults to
// "user" when empty.
func SeedUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	if role == "" {
		role = "user"
	}
	id := uuid.Must(uuid.NewRandom())
	// A tiny static prefix + a chunk of the random v4 id keeps the
	// username within the [a-z0-9][a-z0-9_-]{2,29} regex while staying
	// unique across concurrent packages.
	uname := "ts" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)
ON CONFLICT (id) DO NOTHING`,
		id, uname+"@test.local", uname, uname, role)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		// The user's space participation goes with the row (000074).
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

// SeedSpace creates a regular space owned by owner, who is its participant
// (role owner), and returns its id. Seed the space BEFORE the projects
// placed in it: t.Cleanup runs last-registered first, so the projects are
// gone by the time the space's own cleanup removes its participants and
// the space (projects.space_id is ON DELETE RESTRICT, 000069).
func SeedSpace(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	return SeedSpaceOfKind(t, pool, owner, "regular")
}

// SeedSpaceOfKind is SeedSpace for a given kind: "regular" or "personal"
// (the owner's "Personal", INV-8 — one per user, so seed it once).
func SeedSpaceOfKind(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, kind string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var spaceID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO spaces (short_id, kind, owner_id, name)
VALUES ($1, $2, $3, 'ts space') RETURNING id`, NewShortID(t), kind, owner).Scan(&spaceID); err != nil {
		t.Fatalf("seed %s space: %v", kind, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'owner')`, spaceID, owner); err != nil {
		t.Fatalf("seed space owner: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM space_members WHERE space_id = $1`, spaceID)
		_, _ = pool.Exec(ctx, `DELETE FROM space_storage_usage WHERE space_id = $1`, spaceID)
		_, _ = pool.Exec(ctx, `DELETE FROM spaces WHERE id = $1`, spaceID)
	})
	return spaceID
}

// testSpaceShortID and testSpaceOwnerID name the shared test space — the
// space every SeedProject project lives in, the way every project of an
// installation lives in the common space after the transition (K-23). Its
// owner is a dedicated user that no test acts as; the space and its owner
// are created on first use and never removed (they hold no data of their
// own), and a user's participation goes with the user (000074).
const testSpaceShortID = "tstspace"

var testSpaceOwnerID = uuid.MustParse("5ace0000-0000-4000-8000-000000000001")

// TestSpace returns the shared test space, creating it on first use.
func TestSpace(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, 'ts-space-owner@test.local', 'ts-space-owner', 'ts space owner', 'user',
        'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)
ON CONFLICT DO NOTHING`, testSpaceOwnerID); err != nil {
		t.Fatalf("seed the shared test space owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO spaces (short_id, kind, owner_id, name) VALUES ($1, 'regular', $2, 'ts shared space')
ON CONFLICT DO NOTHING`, testSpaceShortID, testSpaceOwnerID); err != nil {
		t.Fatalf("seed the shared test space: %v", err)
	}
	var spaceID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM spaces WHERE short_id = $1`, testSpaceShortID).Scan(&spaceID); err != nil {
		t.Fatalf("read the shared test space: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'owner')
ON CONFLICT DO NOTHING`, spaceID, testSpaceOwnerID); err != nil {
		t.Fatalf("seed the shared test space's owner participation: %v", err)
	}
	return spaceID
}

// ProjectSpace returns the space of project (sk-674.18: every project has
// one since T024).
func ProjectSpace(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var spaceID uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT space_id FROM projects WHERE id = $1`, projectID).
		Scan(&spaceID); err != nil {
		t.Fatalf("read the space of project %s: %v", projectID, err)
	}
	return spaceID
}

// JoinProjectSpace makes user a participant of the space of project — the
// precondition of a project membership since T024 (INV-5).
func JoinProjectSpace(t *testing.T, pool *pgxpool.Pool, projectID, userID uuid.UUID) {
	t.Helper()
	AddSpaceMember(t, pool, ProjectSpace(t, pool, projectID), userID)
}

// AddSpaceMember makes user a participant of space with role "member"
// (idempotent). The space's cleanup removes the row.
func AddSpaceMember(t *testing.T, pool *pgxpool.Pool, spaceID, userID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')
ON CONFLICT (space_id, user_id) DO NOTHING`, spaceID, userID); err != nil {
		t.Fatalf("add space participant %s to %s: %v", userID, spaceID, err)
	}
}

// NewShortID draws a fresh spaces.short_id: 8 characters of lowercase
// base32 (K-20).
func NewShortID(t *testing.T) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("drawing a short id: %v", err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// SeedProject creates a project row, bootstraps the owner as a member,
// and seeds the project_counters row. Returns the project id and
// id_prefix. Cleanup deletes the project's issues leaf-first (they are
// ON DELETE RESTRICT, not cascade) then the project (which cascades
// project_members, components, deps). namePrefix defaults to
// "tsproj" when empty.
func SeedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, namePrefix string) (uuid.UUID, string) {
	t.Helper()
	// A project lives in a space (sk-674.18): the shared test space, the
	// owner its participant — a non-personal project, so an instance admin
	// still sees it (INV-1 g.
	return SeedProjectInSpace(t, pool, owner, TestSpace(t, pool), namePrefix)
}

// SeedProjectInSpace is SeedProject in a given space (seeded before it,
// see SeedSpace), visibility members, the owner its owner member and a
// participant of the space.
func SeedProjectInSpace(t *testing.T, pool *pgxpool.Pool, owner, spaceID uuid.UUID, namePrefix string) (uuid.UUID, string) {
	t.Helper()
	if namePrefix == "" {
		namePrefix = "tsproj"
	}
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := namePrefix + "_" + suffix
	prefix := namePrefix + suffix[:6]
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, spaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)`, id, owner)
	if err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role)
SELECT $1, u.id, 'member' FROM users u WHERE u.id = $2 AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, spaceID, owner); err != nil {
		t.Fatalf("seed project owner's space participation: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed project counter: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// issues.project_id / parent_id / created_by are ON DELETE RESTRICT
		// (NOT cascade — migration 000004), so the project row cannot be
		// removed while any of its issues exist. Delete issues leaf-first
		// (children before parents; dependencies/comments/reminders/
		// attachments cascade on issue delete), then the project. This also
		// reaps issues created via the store/API in the test, not just
		// SeedIssue rows. Bounded loop guards against unexpected
		// cross-project parent refs.
		for range 64 {
			ct, err := pool.Exec(ctx, `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (
       SELECT parent_id FROM issues
        WHERE project_id = $1 AND parent_id IS NOT NULL
   )`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, prefix
}

// SeedIssue inserts an issues row with the given (already-prefixed) id
// and minimal fields. Returns the id.
//
// A closed status writes the closed_at/closed_reason consistency columns
// because the issues_closed_consistency CHECK requires them. The
// project_counters.counter is advanced to MAX(existing, parsed-suffix)
// for numeric-suffix ids so later counter-mode CreateIssue calls don't
// collide; non-numeric (custom-mode) suffixes are ignored.
func SeedIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title, status string, priority int) string {
	t.Helper()
	ctx := context.Background()

	closedAt := "NULL"
	closedReason := "NULL"
	if status == "closed" {
		closedAt = "now()"
		closedReason = "''"
	}

	sql := `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', $4, $5, false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, ` + closedAt + `, ` + closedReason + `,
        NULL, $6, now(), now(), '{}'::jsonb)`
	if _, err := pool.Exec(ctx, sql, idStr, projectID, title, priority, status, creator); err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}

	// Best-effort counter bump for ids shaped "<prefix>-<N>".
	if dash := strings.LastIndex(idStr, "-"); dash > 0 {
		suffix := idStr[dash+1:]
		var n int
		if _, perr := fmt.Sscanf(suffix, "%d", &n); perr == nil && n > 0 {
			_, _ = pool.Exec(ctx, `
UPDATE project_counters
   SET counter = GREATEST(counter, $2)
 WHERE project_id = $1`, projectID, n)
		}
	}
	return idStr
}

// SeedRepo appends one git_repos[] entry to an existing project and
// returns the generated repo id. Used by commit-attach / close-with-commit
// tests that need ResolveRepoForCommit to find exactly one repo (so a
// commit ref with no repoUrl/repoId resolves unambiguously). The project's
// git_repos column is patched in place — no separate cleanup is registered
// (the project's own t.Cleanup CASCADE-drops it).
func SeedRepo(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, name, url string) uuid.UUID {
	t.Helper()
	repoID := uuid.Must(uuid.NewV7())
	repo := map[string]any{"id": repoID.String(), "name": name, "url": url}
	repoBytes, err := json.Marshal([]any{repo})
	if err != nil {
		t.Fatalf("marshal repo: %v", err)
	}
	// COALESCE handles the '[]' default seeded by SeedProject; the append
	// keeps any pre-existing repos.
	_, err = pool.Exec(context.Background(), `
UPDATE projects
   SET git_repos = COALESCE(git_repos, '[]'::jsonb) || $2::jsonb
 WHERE id = $1`, projectID, string(repoBytes))
	if err != nil {
		t.Fatalf("seed repo on project %s: %v", projectID, err)
	}
	return repoID
}

// SeedLockedIssue inserts an open issue that is already lock-held by
// sessionID, with locked_at placed lockAge in the past. Used by the
// stale-lock reaper tests: a lockAge greater than the reaper threshold
// makes the row eligible for auto-release. The counter bump mirrors
// SeedIssue so later counter-mode creates don't collide.
func SeedLockedIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, sessionID string, lockAge time.Duration) string {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee, locked_by, locked_at,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, 'human', NULL, NULL,
        $4, now() - $5::interval,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $6, now(), now(), '{}'::jsonb)`,
		idStr, projectID, "locked "+idStr, sessionID, lockAge.String(), creator)
	if err != nil {
		t.Fatalf("seed locked issue %s: %v", idStr, err)
	}
	if dash := strings.LastIndex(idStr, "-"); dash > 0 {
		var n int
		if _, perr := fmt.Sscanf(idStr[dash+1:], "%d", &n); perr == nil && n > 0 {
			_, _ = pool.Exec(ctx, `
UPDATE project_counters SET counter = GREATEST(counter, $2) WHERE project_id = $1`, projectID, n)
		}
	}
	return idStr
}

// SetPinned flips the pinned flag on an issue (bumps version + updated_at).
// Drives the pinned-close protection invariant — a pinned issue rejects
// Close without force=true.
func SetPinned(t *testing.T, pool *pgxpool.Pool, idStr string, pinned bool) {
	t.Helper()
	tag, err := pool.Exec(context.Background(), `
UPDATE issues SET pinned = $2, version = version + 1, updated_at = now()
 WHERE id = $1`, idStr, pinned)
	if err != nil {
		t.Fatalf("set pinned=%v on %s: %v", pinned, idStr, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("set pinned on %s: %d rows affected, want 1", idStr, tag.RowsAffected())
	}
}

// AddProjectMember adds a user to a project's membership (idempotent).
// Used by cross-project visibility tests that need a second user who IS a
// member of one project but NOT another. role is 'owner' or 'member'.
func AddProjectMember(t *testing.T, pool *pgxpool.Pool, projectID, userID uuid.UUID, role string) {
	t.Helper()
	if role == "" {
		role = "member"
	}
	ctx := context.Background()
	// A member of a project of a space is a participant of that space
	// (INV-5); external users stay outside spaces (E-16).
	if _, err := pool.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, u.id, 'member'
  FROM projects p, users u
 WHERE p.id = $1 AND u.id = $2 AND p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, userID); err != nil {
		t.Fatalf("add space participant %s for project %s: %v", userID, projectID, err)
	}
	_, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, $3, now(), $2)
ON CONFLICT (project_id, user_id) DO NOTHING`, projectID, userID, role)
	if err != nil {
		t.Fatalf("add project member %s to %s: %v", userID, projectID, err)
	}
}

// SeedDep inserts one dependency edge directly (bypassing the Create
// path / cycle check). Both issues must already exist — the FK is
// ON DELETE CASCADE, so deleting either issue (or its project) tears the
// edge down; no explicit cleanup is registered.
func SeedDep(t *testing.T, pool *pgxpool.Pool, fromID, toID, depType string, creator uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`,
		fromID, toID, depType, creator)
	if err != nil {
		t.Fatalf("seed dep %s->%s (%s): %v", fromID, toID, depType, err)
	}
}

// CountEventsForProject returns the number of events rows scoped to a
// single project. This is the project-scoped assert primitive: it never
// counts globally, so a concurrent package's events cannot perturb it.
// Pass an empty eventType to count every event for the project.
func CountEventsForProject(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, eventType string) int {
	t.Helper()
	var n int
	var err error
	if eventType == "" {
		err = pool.QueryRow(context.Background(),
			`SELECT count(*) FROM events WHERE project_id = $1`, projectID).Scan(&n)
	} else {
		err = pool.QueryRow(context.Background(),
			`SELECT count(*) FROM events WHERE project_id = $1 AND type = $2`,
			projectID, eventType).Scan(&n)
	}
	if err != nil {
		t.Fatalf("count events for project %s: %v", projectID, err)
	}
	return n
}

// CountUnpublishedEventsForProject returns the number of events rows for
// a single project whose published_at is still NULL. Project-scoped so
// it ignores rows written by concurrent packages.
func CountUnpublishedEventsForProject(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE project_id = $1 AND published_at IS NULL`,
		projectID).Scan(&n); err != nil {
		t.Fatalf("count unpublished events for project %s: %v", projectID, err)
	}
	return n
}

// CountIssuesByStatus returns the number of issues in a single project
// with the given status. Project-scoped — never a global COUNT(*).
func CountIssuesByStatus(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1 AND status = $2`,
		projectID, status).Scan(&n); err != nil {
		t.Fatalf("count issues by status %q for project %s: %v", status, projectID, err)
	}
	return n
}

// Username resolves the stored username for a seeded user id. The assignee
// column takes any of three handle forms (username, e-mail, UUID) and the
// count surfaces resolve all three (sk-523), so tests need the synthesised
// username both to craft matching/non-matching assignee strings and to pass
// a CallerUsername where a surface still gates on one.
func Username(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	var uname string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, userID).Scan(&uname); err != nil {
		t.Fatalf("resolve username for %s: %v", userID, err)
	}
	return uname
}

// SetStatus transitions one issue's status, keeping the
// issues_closed_consistency CHECK satisfied (closed stamps
// closed_at/closed_reason; any non-closed status clears them). It also
// stamps started_at on the first move into in_progress to mirror the
// production claim path. updated_at is bumped so last-activity / ordering
// surfaces observe the change. The version is incremented so a re-read
// reflects a fresh row.
//
// This is the transition lever for the canonical seed→assert→transition
// →re-assert pattern. It writes the row directly (no service layer) so a
// test can drive a pure-counts regression without standing up handlers.
func SetStatus(t *testing.T, pool *pgxpool.Pool, idStr, status string) {
	t.Helper()
	const sql = `
UPDATE issues
   SET status        = $2,
       closed_at     = CASE WHEN $2 = 'closed'      THEN now()   ELSE NULL END,
       closed_reason = CASE WHEN $2 = 'closed'      THEN 'test'  ELSE NULL END,
       started_at    = CASE WHEN $2 = 'in_progress' THEN COALESCE(started_at, now())
                            ELSE started_at END,
       version       = version + 1,
       updated_at    = now()
 WHERE id = $1`
	tag, err := pool.Exec(context.Background(), sql, idStr, status)
	if err != nil {
		t.Fatalf("set status %q on %s: %v", status, idStr, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("set status %q on %s: %d rows affected, want 1", status, idStr, tag.RowsAffected())
	}
}

// SetAssignee replaces an issue's assignee handle (empty string clears
// it). Bumps version + updated_at. Used to drive delegate / re-assign
// transitions on the On-me / Delegated / outbox lenses.
func SetAssignee(t *testing.T, pool *pgxpool.Pool, idStr, assignee string) {
	t.Helper()
	var arg any
	if assignee != "" {
		arg = assignee
	}
	tag, err := pool.Exec(context.Background(), `
UPDATE issues SET assignee = $2, version = version + 1, updated_at = now()
 WHERE id = $1`, idStr, arg)
	if err != nil {
		t.Fatalf("set assignee %q on %s: %v", assignee, idStr, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("set assignee on %s: %d rows affected, want 1", idStr, tag.RowsAffected())
	}
}

// Accept stamps accepted_at = now() and accepted_by on an issue,
// simulating the assignee acknowledging the task. Pass accepted=false to
// clear it (return the issue to the Inbox surface). Bumps version +
// updated_at.
func Accept(t *testing.T, pool *pgxpool.Pool, idStr string, acceptedBy uuid.UUID, accepted bool) {
	t.Helper()
	var atSQL string
	var byArg any
	if accepted {
		atSQL = "now()"
		byArg = acceptedBy
	} else {
		atSQL = "NULL"
		byArg = nil
	}
	sql := `
UPDATE issues SET accepted_at = ` + atSQL + `, accepted_by = $2,
       version = version + 1, updated_at = now()
 WHERE id = $1`
	tag, err := pool.Exec(context.Background(), sql, idStr, byArg)
	if err != nil {
		t.Fatalf("set accepted=%v on %s: %v", accepted, idStr, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("set accepted on %s: %d rows affected, want 1", idStr, tag.RowsAffected())
	}
}

// SeedBlocker creates a fresh open blocker issue in a SEPARATE sentinel
// project and wires a typed 'blocks' edge from fromID to it, so the
// derived-blocked predicate (open outgoing blocks/waits-for edge to a
// non-closed target) fires on fromID. The blocker lives outside the
// asserted project so it never perturbs that project's own pills.
//
// Returns the blocker issue id. The sentinel project is created fresh
// per call and torn down via t.Cleanup (CASCADE drops the blocker +
// edge). The edge's from-side issue (fromID) must already exist.
func SeedBlocker(t *testing.T, pool *pgxpool.Pool, fromID string, creator uuid.UUID) string {
	t.Helper()
	sentPID, sentPrefix := SeedProject(t, pool, creator, "tsblk")
	blockerID := sentPrefix + "-1"
	SeedIssue(t, pool, sentPID, creator, blockerID, "blocker", "open", 2)
	SeedDep(t, pool, fromID, blockerID, "blocks", creator)
	return blockerID
}

// CountUnacceptedInbox returns the number of issues in a single project
// that sit in the inbox-needs-acceptance surface: status='open' AND
// accepted_at IS NULL AND assigned to `assignee` BY SOMEONE ELSE
// (created_by <> caller). Mirrors the idx_issues_unaccepted gate plus the
// "from others" semantics of the Inbox badge. Project-scoped.
func CountUnacceptedInbox(t *testing.T, pool *pgxpool.Pool, projectID, caller uuid.UUID, assignee string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM issues
 WHERE project_id = $1
   AND status = 'open'
   AND accepted_at IS NULL
   AND assignee IS NOT NULL
   AND LOWER(assignee) = LOWER($2)
   AND created_by <> $3`, projectID, assignee, caller).Scan(&n); err != nil {
		t.Fatalf("count unaccepted inbox for project %s: %v", projectID, err)
	}
	return n
}

// SeedReminder inserts one reminders row for (issueID, userID). The row
// is the FK child of BOTH issues (ON DELETE CASCADE, 000026) and users
// (ON DELETE CASCADE, 000026) — deleting either parent must reap it.
// No explicit cleanup is registered: the parent issue/project/user
// cleanups CASCADE it down. Returns the reminder id.
func SeedReminder(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO reminders (id, issue_id, user_id, remind_at, notified_at, snooze_count, last_reason, created_at, updated_at)
VALUES ($1, $2, $3, now() + interval '1 hour', NULL, 0, NULL, now(), now())`,
		id, issueID, userID)
	if err != nil {
		t.Fatalf("seed reminder for issue %s / user %s: %v", issueID, userID, err)
	}
	return id
}

// SeedAttachment inserts one attachments row in the given status
// ('pending' or 'ready') for issueID, uploaded by uploadedBy. The row is
// the FK child of issues (ON DELETE CASCADE, 000014) — deleting the issue
// must reap it. uploaded_by is ON DELETE RESTRICT, so the user cannot be
// deleted while the row exists. A random s3_key keeps the
// UNIQUE(s3_bucket,s3_key) constraint satisfied across concurrent seeds.
// Returns the attachment id.
func SeedAttachment(t *testing.T, pool *pgxpool.Pool, issueID string, uploadedBy uuid.UUID, status string) uuid.UUID {
	t.Helper()
	if status == "" {
		status = "ready"
	}
	id := uuid.Must(uuid.NewV7())
	s3Key := "test/" + id.String()
	_, err := pool.Exec(context.Background(), `
INSERT INTO attachments (id, issue_id, comment_id, filename, content_type, size_bytes,
                         s3_bucket, s3_key, status, uploaded_by, uploaded_at, metadata)
VALUES ($1, $2, NULL, 'f.png', 'image/png', 1024,
        'skein-attachments', $3, $4, $5, now(), '{}'::jsonb)`,
		id, issueID, s3Key, status, uploadedBy)
	if err != nil {
		t.Fatalf("seed attachment for issue %s: %v", issueID, err)
	}
	return id
}

// SeedPushSubscription inserts one push_subscriptions row for userID. The
// row is the FK child of users (ON DELETE CASCADE, 000027) — deleting the
// user must reap it. The endpoint URL is the UNIQUE key (000027), so a
// random suffix keeps concurrent seeds distinct. Returns the row id.
func SeedPushSubscription(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	endpoint := "https://push.test.local/" + id.String()
	_, err := pool.Exec(context.Background(), `
INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth, ua, created_at, last_seen_at)
VALUES ($1, $2, $3, 'fake-p256dh', 'fake-auth', 'test-ua', now(), now())`,
		id, userID, endpoint)
	if err != nil {
		t.Fatalf("seed push subscription for user %s: %v", userID, err)
	}
	return id
}

// pushSettleMaxTicks bounds TickPushUntilSettled: a queue the runner cannot
// get through in this many batches is a defect to look at, not to wait out.
const pushSettleMaxTicks = 200

// TickPushUntilSettled drives a push runner until userID has no push row
// that is due now (pending, next attempt not in the future): it ticks at
// least once, then again while such a row remains and the last tick claimed
// something.
//
// Why: the runner claims a bounded batch, oldest-due first, from the ONE
// shared queue. Rows left pending by other tests and other packages of the
// same -p 1 run sit ahead of a fixture's freshly seeded row, and once they
// outnumber the batch a single Tick never reaches it — the test then fails
// on "sent 0" for reasons that are not its own (§4a: a test must not depend
// on other tests' data). Ticking until the fixture's OWN due rows are gone
// makes the outcome independent of what is ahead in the queue. A row the
// runner put back for a retry gets a later next-attempt time and is no
// longer due, so retries do not loop here.
func TickPushUntilSettled(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, tick func(context.Context) (int, error)) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < pushSettleMaxTicks; i++ {
		claimed, err := tick(ctx)
		if err != nil {
			t.Fatalf("push runner tick: %v", err)
		}
		var due int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM notifications
 WHERE user_id = $1
   AND push_state = 'pending'
   AND (push_next_attempt_at IS NULL OR push_next_attempt_at <= now())`, userID).Scan(&due); err != nil {
			t.Fatalf("count due push rows of %s: %v", userID, err)
		}
		if due == 0 || claimed == 0 {
			return
		}
	}
	t.Fatalf("push rows of %s still due after %d runner ticks", userID, pushSettleMaxTicks)
}

// SeedTheme inserts one themes row authored by createdBy (also set as
// updated_by). themes.created_by / updated_by are ON DELETE RESTRICT
// (000023) — deleting the author must be BLOCKED while the row exists.
// users.custom_theme_id is ON DELETE SET NULL (000023) — deleting the
// theme degrades any user pointing at it back to the builtin default.
// The theme is createdBy's (author_id, sk-674.18 T035 — NOT NULL since
// 000082); its name is unique per author, and a random suffix keeps
// concurrent seeds distinct. Returns the theme id.
//
// No explicit cleanup is registered here because the RESTRICT FK would
// block the author's own t.Cleanup DELETE; the consuming test must delete
// the theme itself (and clear any custom_theme_id pointer) before the
// user cleanup runs — register that via t.Cleanup in the test.
func SeedTheme(t *testing.T, pool *pgxpool.Pool, createdBy uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	name := "ts_theme_" + strings.ReplaceAll(id.String()[:13], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO themes (id, name, base, variables, created_by, updated_by, author_id, created_at, updated_at)
VALUES ($1, $2, 'light', '{"--bg-base":"#fdf6e3"}'::jsonb, $3, $3, $3, now(), now())`,
		id, name, createdBy)
	if err != nil {
		t.Fatalf("seed theme by %s: %v", createdBy, err)
	}
	return id
}

// SeedComment inserts one comments row authored by authorUserID on
// issueID. The row is the FK child of issues (ON DELETE CASCADE,
// 000004:128) — deleting the issue (or its project, via SeedProject's
// leaf-first issue cleanup) reaps it, so no explicit cleanup is
// registered. author_user_id is ON DELETE RESTRICT, so the user cannot
// be deleted while the comment exists; consuming tests that want to
// remove the user must let the issue/project cleanup run first. The
// generated tsv_ru/tsv_en columns (000006) populate automatically from
// body, so a freshly-seeded comment is immediately FTS-searchable.
// Returns the comment id.
func SeedComment(t *testing.T, pool *pgxpool.Pool, issueID string, authorUserID uuid.UUID, body string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at)
VALUES ($1, $2, 'tester', $3, 'human', $4, now(), now())`,
		id, issueID, authorUserID, body)
	if err != nil {
		t.Fatalf("seed comment on issue %s: %v", issueID, err)
	}
	return id
}

// SeedComponent inserts one components row in projectID, created by
// createdBy. The row is the FK child of projects (ON DELETE CASCADE,
// 000003:63) — deleting the project reaps it, so no explicit cleanup is
// registered. created_by is ON DELETE RESTRICT (000003:68), so the
// creator cannot be deleted while the component exists. The name is
// UNIQUE(project_id, name); a random suffix keeps concurrent seeds
// distinct. The generated tsv_ru/tsv_en columns (000006) populate from
// name+description, so the component is immediately FTS-searchable.
// Returns the component id.
func SeedComponent(t *testing.T, pool *pgxpool.Pool, projectID, createdBy uuid.UUID, name, description string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uniqueName := name + "_" + strings.ReplaceAll(id.String()[:8], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, '[]'::jsonb, NULL, $5, now(), now())`,
		id, projectID, uniqueName, description, createdBy)
	if err != nil {
		t.Fatalf("seed component %q on project %s: %v", uniqueName, projectID, err)
	}
	return id
}

// CountRows returns the number of rows in `table` whose `col` equals
// `val`. A narrow primitive for FK-cascade asserts: seed parent + child,
// DELETE the parent, then assert CountRows(childTable, fkCol, parentID)==0.
// Entity-scoped (always a WHERE on the seeded id), never a bare global
// COUNT(*), so a concurrent package's rows can never perturb it. `table`
// and `col` are fixed identifiers supplied by the test (never user
// input), so the fmt.Sprintf interpolation carries no injection surface.
func CountRows(t *testing.T, pool *pgxpool.Pool, table, col string, val any) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = $1`, table, col)
	if err := pool.QueryRow(context.Background(), q, val).Scan(&n); err != nil {
		t.Fatalf("count %s where %s=%v: %v", table, col, val, err)
	}
	return n
}
