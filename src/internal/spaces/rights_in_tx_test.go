package spaces_test

// rights_in_tx_test.go — sk-674.18. Reference
// — US-4, US-6 and K-7 of the spec and plan: an operation on a space is
// decided by the rights its caller has AT THE MOMENT OF THE WRITE — read in
// the same SERIALIZABLE transaction, under the space's lock, as the write.
// The interleaving is staged, not hoped for: another transaction holds the
// space's row; the operation makes its preliminary read and waits on the
// lock (seen in pg_stat_activity); meanwhile the holder takes the right away
// — the owner is changed, the admin demoted — and commits. The operation
// must then be refused and change nothing.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// stmt is one statement of the holder's revocation.
type stmt struct {
	sql  string
	args []any
}

// holdSpaceRow opens a transaction holding the space's row (FOR UPDATE);
// finish runs the revocation in it and commits.
type spaceRowHold struct {
	pid    int32
	finish func(t *testing.T, revoke ...stmt)
}

func holdSpaceRow(t *testing.T, space uuid.UUID) *spaceRowHold {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &spaceRowHold{}
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM spaces WHERE id = $1 FOR UPDATE`, space); err != nil {
		t.Fatal(err)
	}
	done := false
	t.Cleanup(func() {
		if !done {
			_ = tx.Rollback(context.Background())
		}
	})
	h.finish = func(t *testing.T, revoke ...stmt) {
		t.Helper()
		for _, st := range revoke {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				t.Fatalf("revoking the right: %v", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("committing the revocation: %v", err)
		}
		done = true
	}
	return h
}

func TestRights_DecidedInsideTheWriteTransaction(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	svc := spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll})

	// changeOwner moves the ownership of space from a to b — the rows the
	// owner change writes — inside the holder.
	changeOwner := func(space, a, b uuid.UUID) []stmt {
		return []stmt{
			{`UPDATE space_members SET role = 'member' WHERE space_id = $1 AND user_id = $2`, []any{space, a}},
			{`UPDATE space_members SET role = 'owner' WHERE space_id = $1 AND user_id = $2`, []any{space, b}},
			{`UPDATE spaces SET owner_id = $2 WHERE id = $1`, []any{space, b}},
		}
	}

	type world struct {
		space      uuid.UUID
		short      string
		name       string
		owner, nxt sigUser
		member     sigUser
	}
	setup := func(t *testing.T) world {
		w := world{owner: seedSigUser(t, "user"), nxt: seedSigUser(t, "user"), member: seedSigUser(t, "user")}
		w.space = testsupport.SeedSpace(t, pool, w.owner.id)
		testsupport.AddSpaceMember(t, pool, w.space, w.nxt.id)
		testsupport.AddSpaceMember(t, pool, w.space, w.member.id)
		w.short = shortIDOf(t, pool, w.space)
		if err := pool.QueryRow(ctx, `SELECT name FROM spaces WHERE id = $1`, w.space).Scan(&w.name); err != nil {
			t.Fatal(err)
		}
		return w
	}
	refused := func(t *testing.T, err error, code string) {
		t.Helper()
		var r *spaces.Refusal
		if !errors.As(err, &r) || r.Code != code {
			t.Fatalf("the operation after the right was taken away: %v, want the refusal %s", err, code)
		}
	}
	count := func(t *testing.T, sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("the previous owner deletes", func(t *testing.T) {
		w := setup(t)
		hold := holdSpaceRow(t, w.space)
		op := async(func() error { return svc.Delete(ctx, w.owner.caller(), w.short, w.name) })
		if !blocked(t, pool, hold.pid) {
			t.Fatal("the deletion did not wait for the space's row")
		}
		hold.finish(t, changeOwner(w.space, w.owner.id, w.nxt.id)...)
		refused(t, awaitErr(t, op, "the deletion"), "forbidden")
		if count(t, `SELECT count(*) FROM spaces WHERE id = $1`, w.space) != 1 {
			t.Error("the space was deleted by its former owner")
		}
	})

	t.Run("the previous owner renames", func(t *testing.T) {
		w := setup(t)
		hold := holdSpaceRow(t, w.space)
		op := async(func() error {
			_, err := svc.Rename(ctx, w.owner.caller(), w.short, "Renamed by the former owner")
			return err
		})
		if !blocked(t, pool, hold.pid) {
			t.Fatal("the rename did not wait for the space's row")
		}
		hold.finish(t, changeOwner(w.space, w.owner.id, w.nxt.id)...)
		refused(t, awaitErr(t, op, "the rename"), "forbidden")
		if count(t, `SELECT count(*) FROM spaces WHERE id = $1 AND name = $2`, w.space, w.name) != 1 {
			t.Error("the space was renamed by its former owner")
		}
	})

	t.Run("the previous owner removes a member", func(t *testing.T) {
		w := setup(t)
		hold := holdSpaceRow(t, w.space)
		op := async(func() error { return svc.RemoveMember(ctx, w.owner.caller(), w.short, w.member.id) })
		if !blocked(t, pool, hold.pid) {
			t.Fatal("the removal did not wait for the space's row")
		}
		hold.finish(t, changeOwner(w.space, w.owner.id, w.nxt.id)...)
		refused(t, awaitErr(t, op, "the removal"), "forbidden")
		if count(t, `SELECT count(*) FROM space_members WHERE space_id = $1 AND user_id = $2`, w.space, w.member.id) != 1 {
			t.Error("the member was removed by the former owner")
		}
	})

	t.Run("a demoted admin adds a member", func(t *testing.T) {
		w := setup(t)
		admin := seedSigUser(t, "admin")
		newcomer := seedSigUser(t, "user")
		hold := holdSpaceRow(t, w.space)
		op := async(func() error {
			_, err := svc.AddMember(ctx, admin.caller(), w.short, newcomer.id)
			return err
		})
		if !blocked(t, pool, hold.pid) {
			t.Fatal("the addition did not wait for the space's row")
		}
		hold.finish(t, stmt{`UPDATE users SET role = 'user' WHERE id = $1`, []any{admin.id}})
		refused(t, awaitErr(t, op, "the addition"), "not_admin")
		if count(t, `SELECT count(*) FROM space_members WHERE space_id = $1 AND user_id = $2`, w.space, newcomer.id) != 0 {
			t.Error("the demoted admin added a member")
		}
	})

	t.Run("a demoted admin changes the owner", func(t *testing.T) {
		w := setup(t)
		admin := seedSigUser(t, "admin")
		hold := holdSpaceRow(t, w.space)
		op := async(func() error {
			_, err := svc.ChangeOwner(ctx, admin.caller(), w.short, w.member.id)
			return err
		})
		if !blocked(t, pool, hold.pid) {
			t.Fatal("the owner change did not wait for the space's row")
		}
		hold.finish(t, stmt{`UPDATE users SET role = 'user' WHERE id = $1`, []any{admin.id}})
		refused(t, awaitErr(t, op, "the owner change"), "not_admin")
		if count(t, `SELECT count(*) FROM spaces WHERE id = $1 AND owner_id = $2`, w.space, w.owner.id) != 1 {
			t.Error("the demoted admin changed the owner")
		}
	})
}
