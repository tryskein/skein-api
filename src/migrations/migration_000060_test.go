package migrations

// migration_000060_test.go — sk-622.21, T002, test matrix row 7 (INV-15,
// US-16 late binding, edge 18, edge 6). Reference — spec US-16 / INV-15 and
// plan K7, written before the migration: after a committed write of
// issues.assignee, or of users (account creation, e-mail change),
// issues.assignee_user_id equals the resolution of the assignee text against
// the CURRENT users table — username, e-mail or id, case-insensitive,
// deactivated users included; NULL when nothing resolves.
//
// Each handle form belongs to a DIFFERENT user (the reason is in
// internal/issues/assignee_forms_test.go): a resolver whose legs read the wrong
// column would misassign at least one issue and fail by uuid comparison.
// Account creation and the e-mail change go through the real SSO provisioning
// path (auth.Provisioner: insertNew and updateBySub), without HTTP.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

func m60AssigneeUserID(t *testing.T, pool *pgxpool.Pool, issueID string) *uuid.UUID {
	t.Helper()
	var got *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee_user_id FROM issues WHERE id = $1`, issueID).Scan(&got); err != nil {
		t.Fatalf("read assignee_user_id of %s: %v", issueID, err)
	}
	return got
}

func m60Expect(t *testing.T, pool *pgxpool.Pool, issueID, what string, want *uuid.UUID) {
	t.Helper()
	got := m60AssigneeUserID(t, pool, issueID)
	switch {
	case want == nil && got != nil:
		t.Errorf("%s: %s.assignee_user_id = %s, want NULL", what, issueID, got)
	case want != nil && (got == nil || *got != *want):
		t.Errorf("%s: %s.assignee_user_id = %v, want %s", what, issueID, got, want)
	}
}

func m60Username(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (username, email string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT username, email FROM users WHERE id = $1`, id).Scan(&username, &email); err != nil {
		t.Fatalf("read user %s: %v", id, err)
	}
	return username, email
}

func m60SetAssignee(t *testing.T, pool *pgxpool.Pool, issueID, handle string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET assignee = $2 WHERE id = $1`, issueID, handle); err != nil {
		t.Fatalf("assign %s to %q: %v", issueID, handle, err)
	}
}

// m60DeleteUserOnCleanup removes a user the provisioner created.
func m60DeleteUserOnCleanup(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
}

// TestMigration000060_AssigneeTextFormsResolve — the three handle forms in any
// letter case, a deactivated assignee, a handle naming nobody, and a deleted
// user.
func TestMigration000060_AssigneeTextFormsResolve(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool, "m60o")
	pid, prefix := seedProject(t, pool, owner, "m60")

	byName := seedUser(t, pool, "m60n")
	byMail := seedUser(t, pool, "m60m")
	byID := seedUser(t, pool, "m60i")
	deactivated := seedUser(t, pool, "m60d")
	doomed := seedUser(t, pool, "m60x")
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, deactivated); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	nameHandle, _ := m60Username(t, pool, byName)
	_, mailHandle := m60Username(t, pool, byMail)
	deactName, _ := m60Username(t, pool, deactivated)
	doomedName, _ := m60Username(t, pool, doomed)

	cases := []struct {
		issue, handle, what string
		want                *uuid.UUID
	}{
		{prefix + "-1", strings.ToUpper(nameHandle), "username, upper case", &byName},
		{prefix + "-2", strings.ToUpper(mailHandle), "e-mail, upper case", &byMail},
		{prefix + "-3", strings.ToUpper(byID.String()), "uuid, upper case", &byID},
		{prefix + "-4", deactName, "deactivated user's username", &deactivated},
		{prefix + "-5", "nobody-by-this-name", "handle naming nobody", nil},
	}
	for _, c := range cases {
		seedIssue(t, pool, c.issue, pid, owner, "")
		m60SetAssignee(t, pool, c.issue, c.handle)
		m60Expect(t, pool, c.issue, c.what, c.want)
	}

	// INSERT carries the assignee too (a writer that sets it on creation).
	created := prefix + "-6"
	seedIssue(t, pool, created, pid, owner, nameHandle)
	m60Expect(t, pool, created, "assignee written on INSERT", &byName)

	// A direct write of the column does not survive: it is re-resolved.
	if _, err := pool.Exec(ctx, `UPDATE issues SET assignee_user_id = $2 WHERE id = $1`, created, byMail); err != nil {
		t.Fatalf("direct write: %v", err)
	}
	m60Expect(t, pool, created, "direct write of the column", &byName)

	// Deleting the user nulls the link (FK ON DELETE SET NULL).
	gone := prefix + "-7"
	seedIssue(t, pool, gone, pid, owner, doomedName)
	m60Expect(t, pool, gone, "before the user is deleted", &doomed)
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, doomed); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	m60Expect(t, pool, gone, "after the user is deleted", nil)
}

// TestMigration000060_LateBindingAndEmailChange — an issue assigned to an
// e-mail before the account exists is linked when the account is created; an
// SSO e-mail change re-links by the old and the new e-mail. Both through
// auth.Provisioner, the production path.
func TestMigration000060_LateBindingAndEmailChange(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool, "m60p")
	pid, prefix := seedProject(t, pool, owner, "m60")
	prov := auth.NewProvisioner(pool, true, "user", "")
	tag := strings.ReplaceAll(uuid.NewString()[:8], "-", "")

	// Late binding: assigned first, the account appears later.
	lateMail := "late" + tag + "@m60.test"
	early := prefix + "-1"
	seedIssue(t, pool, early, pid, owner, strings.ToUpper(lateMail))
	m60Expect(t, pool, early, "before the account exists", nil)

	res, err := prov.Provision(ctx, auth.ProvisionInput{Email: lateMail, Username: "late" + tag})
	if err != nil {
		t.Fatalf("provision new account: %v", err)
	}
	if !res.Provisioned {
		t.Fatalf("precondition: the account must be created by this call, got an existing one")
	}
	m60DeleteUserOnCleanup(t, pool, res.UserID)
	m60Expect(t, pool, early, "after the account is created", &res.UserID)

	// E-mail change through SSO (sub-match → updateBySub).
	sub := "m60-sub-" + tag
	oldMail := "old" + tag + "@m60.test"
	newMail := "new" + tag + "@m60.test"
	moved, err := prov.Provision(ctx, auth.ProvisionInput{Sub: sub, Email: oldMail, Username: "mv" + tag})
	if err != nil {
		t.Fatalf("provision account to move: %v", err)
	}
	m60DeleteUserOnCleanup(t, pool, moved.UserID)

	toOld := prefix + "-2"
	toNew := prefix + "-3"
	seedIssue(t, pool, toOld, pid, owner, oldMail)
	seedIssue(t, pool, toNew, pid, owner, newMail)
	m60Expect(t, pool, toOld, "old e-mail before the change", &moved.UserID)
	m60Expect(t, pool, toNew, "new e-mail before the change", nil)

	after, err := prov.Provision(ctx, auth.ProvisionInput{Sub: sub, Email: newMail, Username: "mv" + tag})
	if err != nil {
		t.Fatalf("provision with changed e-mail: %v", err)
	}
	if after.UserID != moved.UserID || after.Email != newMail {
		t.Fatalf("precondition: the SSO login must update the same account's e-mail, got %+v", after)
	}
	m60Expect(t, pool, toOld, "old e-mail after the change", nil)
	m60Expect(t, pool, toNew, "new e-mail after the change", &moved.UserID)
}

// m60WaitForPIDBlocked polls pg_locks until the backend holds an ungranted
// lock — the connection is parked in a lock queue, not merely "called".
func m60WaitForPIDBlocked(t *testing.T, pool *pgxpool.Pool, pid uint32) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE pid = $1 AND NOT granted`, int32(pid)).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func m60SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// TestMigration000060_AssignmentRacingEmailChangeNeverDeadlocks — sk-622.21
// review fix : an assignment that already holds its
// issue row and a user e-mail change that re-resolves that issue must not
// form a lock cycle. Reference — INV-15 and the 000060 header ("RACES"):
// neither side ever gets 40P01; the assignment side may be told to retry
// (40001); after the retry the link equals the resolution against the
// committed users table.
//
// Order of the race (the reproduction): A locks issue X; B changes bob's
// e-mail and parks on X; A then writes X's assignee. With the advisory lock
// waited for on the issue side this is a deadlock.
func TestMigration000060_AssignmentRacingEmailChangeNeverDeadlocks(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := seedUser(t, pool, "m60r")
	bob := seedUser(t, pool, "m60b")
	pid, prefix := seedProject(t, pool, owner, "m60")
	tag := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	oldMail, newMail := "old"+tag+"@m60.test", "new"+tag+"@m60.test"
	if _, err := pool.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, bob, oldMail); err != nil {
		t.Fatalf("set bob's e-mail: %v", err)
	}
	x := prefix + "-1"
	seedIssue(t, pool, x, pid, owner, oldMail)
	m60Expect(t, pool, x, "before the race", &bob)

	// A: holds X.
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(context.Background()) }()
	if _, err := txA.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR UPDATE`, x); err != nil {
		t.Fatalf("A: lock X: %v", err)
	}

	// B: bob's e-mail change, parked on X.
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = txB.Rollback(context.Background()) }()
	var pidB uint32
	if err := txB.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pidB); err != nil {
		t.Fatalf("B: pid: %v", err)
	}
	bDone := make(chan error, 1)
	go func() {
		_, err := txB.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, bob, newMail)
		if err == nil {
			err = txB.Commit(ctx)
		}
		bDone <- err
	}()
	if !m60WaitForPIDBlocked(t, pool, pidB) {
		t.Fatalf("precondition: B did not park behind A's issue row")
	}

	// A: writes the assignee of X while B is parked.
	_, errA := txA.Exec(ctx, `UPDATE issues SET assignee = $2 WHERE id = $1`, x, oldMail)
	if errA == nil {
		errA = txA.Commit(ctx)
	} else {
		_ = txA.Rollback(ctx)
	}
	errB := <-bDone

	if st := m60SQLState(errA); st == "40P01" {
		t.Fatalf("assignment side got a deadlock (40P01): %v", errA)
	}
	if st := m60SQLState(errB); st == "40P01" {
		t.Fatalf("user side got a deadlock (40P01): %v", errB)
	}
	if errB != nil {
		t.Fatalf("user e-mail change failed: %v", errB)
	}
	if errA != nil && m60SQLState(errA) != "40001" {
		t.Fatalf("assignment side failed with %v, want success or 40001 (retry)", errA)
	}
	if errA != nil {
		// The retry the 40001 asks for, against the committed users table.
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE issues SET assignee = $2 WHERE id = $1`, x, oldMail)
			return err
		}); err != nil {
			t.Fatalf("retry of the assignment: %v", err)
		}
	}
	m60Expect(t, pool, x, "old e-mail after bob moved off it", nil)
	m60SetAssignee(t, pool, x, newMail)
	m60Expect(t, pool, x, "new e-mail", &bob)
}
