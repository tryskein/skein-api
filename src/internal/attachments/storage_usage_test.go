package attachments

// storage_usage_test.go — sk-674.18 T026, matrix row 112 (K-8). Reference —
// K-8 of the plan, written before the code: the denormalized counter
// space_storage_usage(space_id, bytes) is kept in the transactions of
// Confirm, Delete / DeleteByID and of the deletion cascade, so it always
// equals the sum of the confirmed attachments of the space's projects;
// pending uploads are not counted. The cascade of the product is the
// deletion of a space (spaces/delete.go): its files and its counter go
// together. Another space's counter is never touched.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

func TestStorageUsage_EqualsTheConfirmedSum(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	owner := seedUser(t, pool)
	var ownerName string
	if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, owner).Scan(&ownerName); err != nil {
		t.Fatal(err)
	}
	actor := Actor{UserID: owner, Username: ownerName, Role: "human"}
	space := testsupport.SeedSpace(t, pool, owner)
	other := testsupport.SeedSpace(t, pool, owner)

	upload := func(space uuid.UUID, size int64) uuid.UUID {
		t.Helper()
		project, prefix := testsupport.SeedProjectInSpace(t, pool, owner, space, "su")
		issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "usage", "open", 2)
		id := testsupport.SeedAttachment(t, pool, issue, owner, "pending")
		if _, err := pool.Exec(ctx, `UPDATE attachments SET size_bytes = $2 WHERE id = $1`, id, size); err != nil {
			t.Fatal(err)
		}
		return id
	}
	confirm := func(id uuid.UUID) {
		t.Helper()
		if _, err := store.Confirm(ctx, id, owner, "user", actor); err != nil {
			t.Fatalf("confirm %s: %v", id, err)
		}
	}
	check := func(step string, space uuid.UUID, want int64) {
		t.Helper()
		var counter, sum int64
		if err := pool.QueryRow(ctx, `
SELECT COALESCE((SELECT bytes FROM space_storage_usage WHERE space_id = $1), 0),
       COALESCE((SELECT sum(a.size_bytes) FROM attachments a
                   JOIN issues i ON i.id = a.issue_id JOIN projects p ON p.id = i.project_id
                  WHERE p.space_id = $1 AND a.status = 'ready'), 0)`, space).Scan(&counter, &sum); err != nil {
			t.Fatal(err)
		}
		if counter != sum || counter != want {
			t.Errorf("%s: the counter %d, the confirmed sum %d, want both %d", step, counter, sum, want)
		}
	}

	elsewhere := upload(other, 7)
	confirm(elsewhere)
	a, b, c := upload(space, 100), upload(space, 250), upload(space, 40)
	pending := upload(space, 999)
	check("pending uploads only", space, 0)
	confirm(a)
	confirm(b)
	confirm(c)
	check("three confirmations over three projects", space, 390)
	confirm(a)
	check("a repeated confirmation", space, 390)

	if _, _, err := store.Delete(ctx, b, owner, "user", actor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("Delete of a confirmed file", space, 140)
	if err := store.DeleteByID(ctx, pending); err != nil {
		t.Fatalf("delete by id (pending): %v", err)
	}
	check("DeleteByID of a pending upload", space, 140)
	if err := store.DeleteByID(ctx, c); err != nil {
		t.Fatalf("delete by id (ready): %v", err)
	}
	check("DeleteByID of a confirmed file", space, 100)
	check("the other space throughout", other, 7)

	// The cascade: deleting the space takes its files and its counter.
	var short, name string
	if err := pool.QueryRow(ctx, `SELECT short_id, name FROM spaces WHERE id = $1`, space).Scan(&short, &name); err != nil {
		t.Fatal(err)
	}
	svc := spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll})
	if err := svc.Delete(ctx, spaces.Caller{UserID: owner, Username: ownerName,
		Actor: spaces.Actor{UserID: owner, Username: ownerName, Type: "human"}}, short, name); err != nil {
		t.Fatalf("delete the space: %v", err)
	}
	if rows := countRows(t, pool, `SELECT count(*) FROM space_storage_usage WHERE space_id = $1`, space); rows != 0 {
		t.Errorf("the deleted space keeps %d counter rows", rows)
	}
	check("the deleted space", space, 0)
	check("the other space after the cascade", other, 7)
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
