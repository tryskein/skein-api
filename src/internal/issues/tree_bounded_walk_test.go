package issues

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
)

// sk-721 — the tree walk is cut at MaxNodes+1 rows, not after enumerating
// every path.
//
// A walk row is one path to an issue. On the full transitive DAG of blocks
// edges over 22 issues (issue i blocks every issue j > i, 231 edges) there
// are 2^21 paths from the first issue and only 22 issues. The tree query used
// to sort the whole walk before its LIMIT, so Postgres enumerated all paths
// and spilled them to pgsql_tmp — the sk-647 disk-exhaustion pattern, reached
// here by a user switching on the "blocks" lens in the tree view.
//
// Resource criterion, deterministic and not a clock: the store runs on a pool
// whose sessions start with work_mem = 1MB and temp_file_limit = 1MB (the test
// role is a superuser, so the startup packet may set both). 1001 rows fit in
// memory far below that; two million cannot, and Postgres cancels the query
// with 53400. The settings are read back before the run, so the verdict never
// depends on server defaults.

// newResourceCappedPool opens a pool on TEST_DATABASE_URL whose every session
// starts with work_mem and temp_file_limit at 1MB.
func newResourceCappedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv(testDatabaseURL))
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURL, err)
	}
	cfg.ConnConfig.RuntimeParams["work_mem"] = "1MB"
	cfg.ConnConfig.RuntimeParams["temp_file_limit"] = "1MB"
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open resource-capped pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, name := range []string{"work_mem", "temp_file_limit"} {
		var got string
		if err := pool.QueryRow(context.Background(), "SELECT current_setting($1)", name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got != "1MB" {
			t.Fatalf("%s = %q on the capped pool, want 1MB", name, got)
		}
	}
	return pool
}

// seedTransitiveBlocksDAG creates n issues and a blocks edge from every
// issue to every later one, returning the ids in creation order.
func seedTransitiveBlocksDAG(t *testing.T, pool *pgxpool.Pool, pid, owner uuid.UUID, prefix string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		seedIssue(t, pool, pid, owner, ids[i], ids[i])
	}
	var froms, tos []string
	for i := range n {
		for j := i + 1; j < n; j++ {
			froms = append(froms, ids[i])
			tos = append(tos, ids[j])
		}
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
SELECT f, t, 'blocks', '{}'::jsonb, 1, $3, now()
  FROM unnest($1::text[], $2::text[]) AS e(f, t)`, froms, tos, owner); err != nil {
		t.Fatalf("seed %d blocks edges: %v", len(froms), err)
	}
	return ids
}

func TestIssueTree_DenseBlocksDAG_BoundedResources(t *testing.T) {
	seedPool := newTestPool(t)
	owner := seedUser(t, seedPool)
	pid, prefix := seedProject(t, seedPool, owner)
	store := NewStore(newResourceCappedPool(t))

	treeOf := func(root string) (*IssueTreeResult, error) {
		return store.IssueTree(storeFixtureCtx(), IssueTreeArgs{
			Caller: owner,
			Scope:  authz.ForUser(owner),
			RootID: &root,
			Types:  []string{"parent-child", "blocks"},
		})
	}

	t.Run("over the node limit answers tree_too_large", func(t *testing.T) {
		const n = 22 // 2^21 paths from the first issue
		ids := seedTransitiveBlocksDAG(t, seedPool, pid, owner, prefix+"a", n)

		_, err := treeOf(ids[0])
		var tooLarge *ErrTreeTooLarge
		if !errors.As(err, &tooLarge) {
			t.Fatalf("IssueTree(%s) over the %d-issue transitive blocks DAG = %v, want ErrTreeTooLarge within work_mem/temp_file_limit 1MB",
				ids[0], n, err)
		}
		if tooLarge.Limit != IssueTreeDefaultMaxNodes {
			t.Fatalf("ErrTreeTooLarge.Limit = %d, want %d", tooLarge.Limit, IssueTreeDefaultMaxNodes)
		}
	})

	// Under the limit the whole walk still comes back, deduplicated by the
	// sk-36 rule: the first (shallowest) occurrence is canonical, a later one
	// is a reference row with no subtree. Every issue is a direct child of
	// the root, so the root's six-issue DAG renders as 1 root + 5 canonical
	// children + one reference row for each pair j < k (k reached again under
	// j) = C(5,2) = 10, each pointing back at the root.
	t.Run("under the node limit returns the deduplicated walk", func(t *testing.T) {
		const n = 6
		ids := seedTransitiveBlocksDAG(t, seedPool, pid, owner, prefix+"b", n)
		root := ids[0]

		res, err := treeOf(root)
		if err != nil {
			t.Fatalf("IssueTree(%s) over the %d-issue transitive blocks DAG: %v", root, n, err)
		}
		canonical := map[string]int{}
		references := 0
		for _, row := range res.Items {
			if !row.IsReference {
				canonical[row.Item.ID]++
				continue
			}
			references++
			if row.FirstSeenAt == nil || *row.FirstSeenAt != root {
				t.Errorf("reference row %s (depth %d) FirstSeenAt = %v, want %s", row.Item.ID, row.Depth, row.FirstSeenAt, root)
			}
		}
		for _, id := range ids {
			if canonical[id] != 1 {
				t.Errorf("canonical rows for %s = %d, want 1", id, canonical[id])
			}
		}
		if references != 10 {
			t.Errorf("reference rows = %d, want 10", references)
		}
	})
}
