package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// reconcile_plan_test.go — sk-622.21, T020 (found in review): the re-resolution of issues.assignee_user_id (step (d) of the R1
// reconciliation; since R2 the page of AssigneeResyncRunner,
// assignee_resync.go#resolveAssigneesSQL) computes the resolution once per
// assigned issue of a page and not at all for an unassigned one.

func TestReconcilePlan_AssigneeResolvedOncePerAssignedIssue(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	var ownerName string
	if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, owner).Scan(&ownerName); err != nil {
		t.Fatalf("read username: %v", err)
	}
	prefix := "rpa" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT $1 || '-' || g, $2, NULL, 1, 'assignee plan', '', 'task', 2, 'open',
       false, 'human', CASE WHEN g <= 3 THEN $4 END, '', '', '', $3, now(), now(), '{}'::jsonb
  FROM generate_series(1, 10) g`, prefix, projectID, owner, ownerName); err != nil {
		t.Fatalf("seed issues: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	// The page starts right before this test's ids and holds exactly them.
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, VERBOSE, FORMAT JSON) "+resolveAssigneesSQL, prefix, 10).Scan(&raw); err != nil {
		t.Fatalf("explain step (d): %v", err)
	}
	var plan []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) == 0 {
		t.Fatalf("parse plan: %v", err)
	}
	type computing struct{ rows, loops float64 }
	var nodes []computing
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		calls := false
		for key, v := range n {
			if key == "Plans" {
				continue
			}
			if b, err := json.Marshal(v); err == nil && strings.Contains(string(b), "resolve_assignee_user_id(") {
				calls = true
			}
		}
		if calls {
			rows, _ := n["Actual Rows"].(float64)
			loops, _ := n["Actual Loops"].(float64)
			nodes = append(nodes, computing{rows, loops})
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(plan[0].Plan)
	if len(nodes) != 1 {
		t.Fatalf("resolve_assignee_user_id is computed in %d plan nodes, want exactly 1", len(nodes))
	}
	if got := nodes[0].rows * nodes[0].loops; got != 3 {
		t.Errorf("the resolving node produced %v rows, want 3 (the assigned issues of the page only)", got)
	}
}
