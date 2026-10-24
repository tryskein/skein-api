package dependencies

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-483 — what `waits-for` does to readiness TODAY, pinned.
//
// The edge's `gate` (all-children / any-children) is validated and stored in
// metadata.gate, but ListReady does not read it: a waits-for edge holds its
// FROM issue out of the ready set exactly like a `blocks` edge — for as long
// as the TARGET itself is neither closed nor standing. The target's children
// play no part. The agent guide, the skein_dep_add / skein_dep_add_many /
// skein_create schema text say exactly this; this test is
// what keeps them honest.
//
// The fixtures are chosen so that a gate-aware readiness WOULD release the
// waiter: with any-children one child of the target is closed, with
// all-children every child is. Both must still leave the waiter out while the
// target is open. If the gate is ever evaluated (R2.4), this test goes red on
// purpose — and the descriptions it guards have to change in the same commit.

// closeIssue flips an issue to closed directly (this package cannot import
// the issues-store lifecycle). closed_at is set because
// issues_closed_consistency requires it.
func closeIssue(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET status = 'closed', closed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
}

func TestListReady_WaitsForGateIsNotEvaluated(t *testing.T) {
	cases := []struct {
		name string
		gate string
		// closeChildren is how many of the target's two children are
		// closed before the first readiness read — enough for the gate
		// to be "satisfied" if it were evaluated.
		closeChildren int
	}{
		{name: "any-children, one child closed", gate: GateAnyChildren, closeChildren: 1},
		{name: "all-children, every child closed", gate: GateAllChildren, closeChildren: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			uid := seedUser(t, pool)
			pid, prefix := seedProject(t, pool, uid)
			waiter := prefix + "-1"
			target := prefix + "-2"
			children := []string{prefix + "-2.1", prefix + "-2.2"}
			seedOpenIssue(t, pool, pid, uid, waiter, "waiter")
			seedOpenIssue(t, pool, pid, uid, target, "awaited target")
			for _, c := range children {
				seedOpenIssue(t, pool, pid, uid, c, "child of target")
				seedParentChildDep(t, pool, c, target, uid)
			}

			// The edge goes through the real write path, so the stored
			// gate is the one a caller of skein_dep_add would leave.
			store := NewStore(pool)
			gate := tc.gate
			if _, err := store.Create(ctx, waiter,
				CreateDepInput{ToID: target, Type: DepWaitsFor, Gate: &gate},
				CallerAccess{UserID: uid, Role: "user"}, HumanActor(uid, "tester")); err != nil {
				t.Fatalf("create waits-for %s → %s gate=%s: %v", waiter, target, tc.gate, err)
			}
			var stored string
			if err := pool.QueryRow(ctx,
				`SELECT metadata->>'gate' FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'waits-for'`,
				waiter, target).Scan(&stored); err != nil {
				t.Fatalf("read stored gate: %v", err)
			}
			if stored != tc.gate {
				t.Fatalf("stored gate = %q, want %q — the gate is described as validated AND stored", stored, tc.gate)
			}

			for _, c := range children[:tc.closeChildren] {
				closeIssue(t, pool, c)
			}

			readyIDs := func() []string {
				t.Helper()
				res, err := store.ListReady(ctx, pid, ReadyFilters{}, 100)
				if err != nil {
					t.Fatalf("ListReady: %v", err)
				}
				return idsOf(res)
			}

			if got := readyIDs(); slices.Contains(got, waiter) {
				t.Errorf("waiter %s is ready with gate=%s, %d of 2 target children closed and target %s OPEN "+
					"(ready = %v) — readiness now evaluates the gate; the guide and tool schemas say it does not",
					waiter, tc.gate, tc.closeChildren, target, got)
			}

			closeIssue(t, pool, target)
			if got := readyIDs(); !slices.Contains(got, waiter) {
				t.Errorf("waiter %s is NOT ready after its waits-for target %s closed (gate=%s, ready = %v) — "+
					"the edge must stop holding once the target itself is closed",
					waiter, target, tc.gate, got)
			}
		})
	}
}
