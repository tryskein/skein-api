package reminders

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/projects"
)

// TestRemoveMember_DeletesUserReminders is the sk-62.2 regression guard:
// when a user loses project membership, their reminders (alarms) on
// that project's issues are deleted in the same transaction — they must not
// keep ringing for, or seeing the metadata of, issues they can no longer
// open. A reminder another user owns on the same project is untouched.
func TestRemoveMember_DeletesUserReminders(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	member := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner) // owner is a member by seedProject

	// Add `member` as a plain member of the project.
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, member, owner); err != nil {
		t.Fatalf("add member: %v", err)
	}

	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "shared issue")
	store := NewStore(pool)
	// member has a reminder; owner has one too on the same issue.
	if _, err := store.Create(ctx, iid, member, time.Now().Add(time.Hour), "", testActor(member)); err != nil {
		t.Fatalf("create member reminder: %v", err)
	}
	if _, err := store.Create(ctx, iid, owner, time.Now().Add(time.Hour), "", testActor(owner)); err != nil {
		t.Fatalf("create owner reminder: %v", err)
	}

	countFor := func(uid uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM reminders WHERE user_id = $1`, uid).Scan(&n); err != nil {
			t.Fatalf("count reminders: %v", err)
		}
		return n
	}
	if got := countFor(member); got != 1 {
		t.Fatalf("member reminders before removal = %d, want 1", got)
	}

	// Remove the member.
	pstore := projects.NewStore(pool)
	if err := pstore.RemoveMember(ctx, pid, member, projects.Actor{UserID: owner, Username: "owner", Role: "human"}); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	// Member's reminders gone; owner's reminder (still a member) survives.
	if got := countFor(member); got != 0 {
		t.Fatalf("member reminders after removal = %d, want 0", got)
	}
	if got := countFor(owner); got != 1 {
		t.Fatalf("owner reminders after member removal = %d, want 1 (untouched)", got)
	}
}
