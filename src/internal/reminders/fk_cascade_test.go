package reminders_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// sk-105.5 (S4) — FK-cascade data-integrity tests.
//
// These assert the ON DELETE CASCADE edges declared by the schema that
// nothing previously tested. The pattern is uniform: seed parent + child,
// hard-DELETE the parent row directly, then assert the child COUNT drops
// to 0. Each assert is entity-scoped (WHERE on the seeded parent id), so a
// concurrent package's rows can never perturb the count.
//
// FK matrix covered here:
//   issues  → reminders     ON DELETE CASCADE  (000026)
//   issues  → attachments   ON DELETE CASCADE  (000014)
//   users   → reminders     ON DELETE CASCADE  (000026)
//   users   → push_subscriptions  ON DELETE CASCADE  (000027)
//
// This file lives in package reminders_test (external) so it can import
// testsupport without an import cycle.

// deleteIssueRaw hard-deletes a single issue row by id. We bypass the
// store path deliberately — the point is to exercise the DB-level FK
// CASCADE, not any application-layer reaping.
func deleteIssueRaw(t *testing.T, pool *pgxpool.Pool, issueID string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID)
	if err != nil {
		t.Fatalf("delete issue %s: %v", issueID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete issue %s: %d rows affected, want 1", issueID, tag.RowsAffected())
	}
}

// TestFKCascade_DeleteIssue_ReapsRemindersAndAttachments verifies that
// hard-deleting an issue removes its reminders (000026) and attachments
// (000014) child rows via the ON DELETE CASCADE edges.
func TestFKCascade_DeleteIssue_ReapsRemindersAndAttachments(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "fkdi")
	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, owner, issueID, "cascade target", "open", 2)

	// Seed two reminders (multiple per issue are allowed) + two
	// attachments (one pending, one ready) hanging off the issue.
	testsupport.SeedReminder(t, pool, issueID, owner)
	testsupport.SeedReminder(t, pool, issueID, owner)
	testsupport.SeedAttachment(t, pool, issueID, owner, "ready")
	testsupport.SeedAttachment(t, pool, issueID, owner, "pending")

	if got := testsupport.CountRows(t, pool, "reminders", "issue_id", issueID); got != 2 {
		t.Fatalf("pre-delete reminders for %s = %d, want 2", issueID, got)
	}
	if got := testsupport.CountRows(t, pool, "attachments", "issue_id", issueID); got != 2 {
		t.Fatalf("pre-delete attachments for %s = %d, want 2", issueID, got)
	}

	deleteIssueRaw(t, pool, issueID)

	if got := testsupport.CountRows(t, pool, "reminders", "issue_id", issueID); got != 0 {
		t.Fatalf("post-delete reminders for %s = %d, want 0 (issue→reminders CASCADE, 000026)", issueID, got)
	}
	if got := testsupport.CountRows(t, pool, "attachments", "issue_id", issueID); got != 0 {
		t.Fatalf("post-delete attachments for %s = %d, want 0 (issue→attachments CASCADE, 000014)", issueID, got)
	}
}

// TestFKCascade_DeleteUser_ReapsRemindersAndPush verifies that
// hard-deleting a user removes their reminders (000026) and
// push_subscriptions (000027) child rows via ON DELETE CASCADE.
//
// The user we delete is a SECOND user (not the project owner) who owns
// only personal-lens rows — no audit-RESTRICT edges (created_by,
// uploaded_by, themes.created_by) point at them — so the user DELETE is
// not blocked. The project owner remains the issue creator/uploader.
func TestFKCascade_DeleteUser_ReapsRemindersAndPush(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "fkdu")
	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, owner, issueID, "reminder host", "open", 2)

	// The user under test: hangs a personal reminder on the issue and
	// registers push subscriptions. We deliberately do NOT add them as a
	// project member — reminders/push are personal lenses that need no
	// membership, and keeping the victim out of project_members avoids the
	// added_by RESTRICT edge muddying the clean user→child CASCADE assert.
	// Seeding them via SeedUser registers a t.Cleanup DELETE, but we
	// delete the row explicitly inside the test first; the cleanup DELETE
	// then no-ops (0 rows).
	victim, _ := testsupport.SeedUser(t, pool, "user")
	testsupport.SeedReminder(t, pool, issueID, victim)
	testsupport.SeedPushSubscription(t, pool, victim)
	testsupport.SeedPushSubscription(t, pool, victim)

	if got := testsupport.CountRows(t, pool, "reminders", "user_id", victim); got != 1 {
		t.Fatalf("pre-delete reminders for user %s = %d, want 1", victim, got)
	}
	if got := testsupport.CountRows(t, pool, "push_subscriptions", "user_id", victim); got != 2 {
		t.Fatalf("pre-delete push subs for user %s = %d, want 2", victim, got)
	}

	tag, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, victim)
	if err != nil {
		t.Fatalf("delete user %s: %v", victim, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete user %s: %d rows affected, want 1", victim, tag.RowsAffected())
	}

	if got := testsupport.CountRows(t, pool, "reminders", "user_id", victim); got != 0 {
		t.Fatalf("post-delete reminders for user %s = %d, want 0 (user→reminders CASCADE, 000026)", victim, got)
	}
	if got := testsupport.CountRows(t, pool, "push_subscriptions", "user_id", victim); got != 0 {
		t.Fatalf("post-delete push subs for user %s = %d, want 0 (user→push_subscriptions CASCADE, 000027)", victim, got)
	}
}
