package accounts_test

// enrol_lock_test.go — K-7, INV-6, INV-7.
// Reference — K-7, written before the primitive: "every
// space operation first takes the row lock on spaces" and a
// conflicting SERIALIZABLE transaction is retried. Joining the common space
// at account creation is such an operation: while a visibility switch of a
// common project holds the space, the creation waits, and after the switch
// commits it gives the outcome of the switched state — no "from the space"
// row on a project that is now `members`, and no error.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// lockWaiters counts the sessions of this database waiting on a lock.
func lockWaiters(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateUser_WaitsForTheCommonSpaceLock(t *testing.T) {
	ctx := context.Background()
	pool := freshDatabase(t)
	seedCommon(t, pool) // an admin, the flag on, the common space
	var common, admin uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id, owner_id FROM spaces WHERE kind = 'common'`).Scan(&common, &admin); err != nil {
		t.Fatal(err)
	}
	project := uuid.New()
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos, created_by, created_at, updated_at, metadata, space_id, visibility)
VALUES ($1, 'enrol lock', 'enl', 'counter', '', '[]'::jsonb, $2, now(), now(), '{}'::jsonb, $3, 'space')`,
		project, admin, common); err != nil {
		t.Fatalf("seed a common project: %v", err)
	}

	// The switch space → members holds the common space, uncommitted.
	switchTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = switchTx.Rollback(context.Background()) }()
	if err := spaces.SetVisibilityInTx(ctx, switchTx, project, spaces.VisibilityMembers,
		spaces.Actor{UserID: admin, Username: "owner", Type: "human"}); err != nil {
		t.Fatalf("switch: %v", err)
	}

	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	type result struct {
		id  uuid.UUID
		err error
	}
	done := make(chan result, 1)
	go func() {
		dto, err := store.CreateUser(context.Background(), users.CreateUserInput{
			Email: "u-" + uuid.NewString()[:8] + "@enrol.test", Name: "New Account",
			Password: "correct horse battery staple 42", Role: "user",
		}, users.HumanActor(uuid.Nil, "admin"))
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{id: dto.ID}
	}()

	waited := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !waited {
		select {
		case r := <-done:
			done <- r
			deadline = time.Now() // finished without waiting
		default:
			waited = lockWaiters(t, pool) > 0
			if !waited {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	if !waited {
		t.Error("the account creation did not wait for the lock of the common space (K-7)")
	}
	if err := switchTx.Commit(ctx); err != nil {
		t.Fatalf("commit the switch: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the account creation did not finish")
	}
	if r.err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(r.err, &pgErr) {
			t.Fatalf("CreateUser after the switch: %v (SQLSTATE %s) — a serialization conflict must be retried", r.err, pgErr.SQLState())
		}
		t.Fatalf("CreateUser after the switch: %v", r.err)
	}
	var row, inCommon bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM project_members WHERE project_id = $1 AND user_id = $2),
       EXISTS (SELECT 1 FROM space_members WHERE space_id = $3 AND user_id = $2)`,
		project, r.id, common).Scan(&row, &inCommon); err != nil {
		t.Fatal(err)
	}
	if row {
		t.Error("the new account got a row on a project that is `members` now (INV-6, INV-7)")
	}
	if !inCommon {
		t.Error("the new account is not a participant of the common space")
	}
}
