package notifications

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-142.8 (T8) — issue_watchers.scope semantics. subtree DOMINATES issue
// and the upgrade is monotonic. sk-622.21 (T010, US-1, D2): an explicit
// unwatch deletes the row and records an opt-out; the opt-out blocks an
// automatic add of any scope for exactly its (user, issue) pair and does not
// widen anything.
// DB-backed, skip without TEST_DATABASE_URL (in-package newTestPool).

// readScope reads the raw scope column for one (issue, user) watcher row.
// Returns ("", false) when the row is absent.
func readScope(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) (string, bool) {
	t.Helper()
	var scope string
	err := pool.QueryRow(context.Background(),
		`SELECT scope FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&scope)
	if err == pgx.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read scope: %v", err)
	}
	return scope, true
}

// addScoped runs AddIssueWatcherScoped in its own tx (the helper takes a tx).
func addScoped(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID, reason, scope string) {
	t.Helper()
	err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return AddIssueWatcherScoped(context.Background(), tx, issueID, userID, reason, scope)
	})
	if err != nil {
		t.Fatalf("AddIssueWatcherScoped(%s, %s): %v", reason, scope, err)
	}
}

func seedScopeIssue(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	// Unique per run (project test guidelines): a stand left behind by a
	// killed run must not collide with the next one.
	issueID := "scope-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedTestIssue(t, pool, projectID, owner, issueID, 2)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM issue_watchers WHERE issue_id = $1`, issueID)
	})
	return owner, issueID
}

// TestWatcherScope_UpgradeAndNoDowngrade — issue then subtree upgrades the
// row to 'subtree'; a subsequent issue-scoped add does NOT downgrade it.
func TestWatcherScope_UpgradeAndNoDowngrade(t *testing.T) {
	pool := newTestPool(t)
	owner, issueID := seedScopeIssue(t, pool)

	// First a plain issue-scoped manual watch → scope='issue'.
	addScoped(t, pool, issueID, owner, ReasonManual, ScopeIssue)
	if s, ok := readScope(t, pool, issueID, owner); !ok || s != ScopeIssue {
		t.Fatalf("after issue add: scope=%q ok=%v, want %q", s, ok, ScopeIssue)
	}

	// Subtree dominates → upgrade to 'subtree'.
	addScoped(t, pool, issueID, owner, ReasonManual, ScopeSubtree)
	if s, _ := readScope(t, pool, issueID, owner); s != ScopeSubtree {
		t.Fatalf("after subtree add: scope=%q, want %q (upgrade)", s, ScopeSubtree)
	}

	// Monotonic — an issue-scoped add must NOT downgrade.
	addScoped(t, pool, issueID, owner, ReasonCommenter, ScopeIssue)
	if s, _ := readScope(t, pool, issueID, owner); s != ScopeSubtree {
		t.Fatalf("after issue re-add: scope=%q, want %q (no downgrade)", s, ScopeSubtree)
	}
}

// TestWatcherScope_SubtreeFirstThenIssue — subtree on a fresh row, then a
// plain issue add: stays 'subtree'.
func TestWatcherScope_SubtreeFirstThenIssue(t *testing.T) {
	pool := newTestPool(t)
	owner, issueID := seedScopeIssue(t, pool)

	addScoped(t, pool, issueID, owner, ReasonManual, ScopeSubtree)
	if s, ok := readScope(t, pool, issueID, owner); !ok || s != ScopeSubtree {
		t.Fatalf("after subtree insert: scope=%q ok=%v, want %q", s, ok, ScopeSubtree)
	}

	addScoped(t, pool, issueID, owner, ReasonManual, ScopeIssue)
	if s, _ := readScope(t, pool, issueID, owner); s != ScopeSubtree {
		t.Fatalf("after issue add: scope=%q, want %q (no downgrade)", s, ScopeSubtree)
	}
}

// TestWatcherScope_StickyUnwatchBlocksScopedAutoAdd — sk-622.21 (No. 23):
// after an explicit unwatch the opt-out (U, X) blocks an automatic add of
// ANY scope — no subscription row appears, so no scope is widened through
// the back door — while in the same fixture the neighbouring pairs (U, Y)
// and (V, X) are subscribed by the same call (without that control a block
// is indistinguishable from "the automatic add does not work").
func TestWatcherScope_StickyUnwatchBlocksScopedAutoAdd(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedTestUser(t, pool, "user")
	other := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	tag := "scopeoo" + uuid.Must(uuid.NewRandom()).String()[:8]
	x, y := tag+"-1", tag+"-2"
	seedTestIssue(t, pool, projectID, owner, x, 2)
	seedTestIssue(t, pool, projectID, owner, y, 2)
	store := NewStore(pool)

	// An issue-scoped manual watch, then the explicit unwatch.
	addScoped(t, pool, x, owner, ReasonManual, ScopeIssue)
	if err := store.RemoveIssueWatch(ctx, x, owner); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}

	// Automatic add (commenter) requesting SUBTREE scope on the opted-out pair.
	addScoped(t, pool, x, owner, ReasonCommenter, ScopeSubtree)
	if s, ok := readScope(t, pool, x, owner); ok {
		t.Fatalf("the opt-out did not block the automatic add: a row with scope %q exists", s)
	}

	// Neighbouring pairs are subscribed by the same automatic add.
	addScoped(t, pool, y, owner, ReasonCommenter, ScopeSubtree)
	if s, ok := readScope(t, pool, y, owner); !ok || s != ScopeSubtree {
		t.Errorf("(U, Y): scope=%q ok=%v, want a %q row — the opt-out is read by user, not by pair", s, ok, ScopeSubtree)
	}
	addScoped(t, pool, x, other, ReasonCommenter, ScopeSubtree)
	if s, ok := readScope(t, pool, x, other); !ok || s != ScopeSubtree {
		t.Errorf("(V, X): scope=%q ok=%v, want a %q row — the opt-out is read by issue, not by pair", s, ok, ScopeSubtree)
	}
}

// TestWatcherScope_AddIssueWatch_SubtreeDominance exercises the store entry
// point that skein_watchers(action="watch") drives:
// AddIssueWatch(subtree=true) writes
// scope='subtree'; a later AddIssueWatch(subtree=false) does not downgrade.
func TestWatcherScope_AddIssueWatch_SubtreeDominance(t *testing.T) {
	pool := newTestPool(t)
	owner, issueID := seedScopeIssue(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.AddIssueWatch(ctx, issueID, owner, true); err != nil {
		t.Fatalf("AddIssueWatch(subtree=true): %v", err)
	}
	if s, ok := readScope(t, pool, issueID, owner); !ok || s != ScopeSubtree {
		t.Fatalf("AddIssueWatch(subtree=true): scope=%q ok=%v, want %q", s, ok, ScopeSubtree)
	}

	if err := store.AddIssueWatch(ctx, issueID, owner, false); err != nil {
		t.Fatalf("AddIssueWatch(subtree=false): %v", err)
	}
	if s, _ := readScope(t, pool, issueID, owner); s != ScopeSubtree {
		t.Fatalf("AddIssueWatch(subtree=false) downgraded scope to %q, want %q", s, ScopeSubtree)
	}
}

// TestWatcherScope_RemoveIsStickyNotDelete — sk-622.21 (No. 23): the explicit
// unwatch of a subtree subscription DELETES the row (no 'unwatched' marker
// is left, INV-1) and records the opt-out — the "stickiness" lives in the
// opt-out now, not in the row.
func TestWatcherScope_RemoveIsStickyNotDelete(t *testing.T) {
	pool := newTestPool(t)
	owner, issueID := seedScopeIssue(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.AddIssueWatch(ctx, issueID, owner, true); err != nil {
		t.Fatalf("AddIssueWatch: %v", err)
	}
	if err := store.RemoveIssueWatch(ctx, issueID, owner); err != nil {
		t.Fatalf("RemoveIssueWatch: %v", err)
	}

	if s, ok := readScope(t, pool, issueID, owner); ok {
		t.Fatalf("RemoveIssueWatch left the row (scope %q) — want it deleted, no tombstone", s)
	}
	var optouts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`,
		issueID, owner).Scan(&optouts); err != nil {
		t.Fatalf("read opt-out: %v", err)
	}
	if optouts != 1 {
		t.Fatalf("opt-outs after the unwatch = %d, want 1", optouts)
	}
}

// TestWatcherScope_ReadersTolerateScopeColumn — the watcher readers
// (ListIssueWatchers, ListAllWatchers / me/watchers, ListWatchingFeed) run
// without scan error on a row carrying scope='subtree'. scope is internal
// for v1, so it must not break the existing surfaces.
func TestWatcherScope_ReadersTolerateScopeColumn(t *testing.T) {
	pool := newTestPool(t)
	owner, issueID := seedScopeIssue(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.AddIssueWatch(ctx, issueID, owner, true); err != nil {
		t.Fatalf("AddIssueWatch(subtree): %v", err)
	}

	ws, err := store.ListIssueWatchers(ctx, issueID)
	if err != nil {
		t.Fatalf("ListIssueWatchers: %v", err)
	}
	found := false
	for _, w := range ws {
		if w.UserID == owner {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListIssueWatchers did not return the subtree watcher")
	}

	all, err := store.ListAllWatchers(ctx, owner)
	if err != nil {
		t.Fatalf("ListAllWatchers (/me/watchers): %v", err)
	}
	if all == nil {
		t.Fatalf("ListAllWatchers returned nil")
	}

	if _, err := store.ListWatchingFeed(ctx, owner, WatchingStateAll); err != nil {
		t.Fatalf("ListWatchingFeed: %v", err)
	}
}
