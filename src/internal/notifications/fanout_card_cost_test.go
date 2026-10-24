package notifications

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fanout_card_cost_test.go — sk-622.21, T012, matrix row No. 14 (SC-003, NFR
// "the fan-out is a fixed number of queries").
//
// The Watching card is computed inside the recipient-selection statement
// (the canonical-parent walk is a recursive CTE there). A card computed by a
// follow-up query per recipient, or per ancestor, would multiply the
// statements the fan-out sends. So, as in fanout_nplusone_test.go, the
// assertion is the statement COUNT of one FanOut, compared for equality
// between corpora that differ only in the dimension under test:
//
//   - 1 vs 200 recipients (each with one subtree subscription);
//   - 1 vs 500 subscriptions held by the single recipient;
//
// with the event's issue at depth 5 under the subscribed root in every
// corpus. Each corpus is checked to actually produce its rows with the
// expected card, so an equality of two empty fan-outs cannot pass.

// cardCostDepth is the depth of the event's issue under the subscribed root.
const cardCostDepth = 5

// cardCostCorpus is one seeded world: a chain root ← c1 ← … ← c5, the
// recipients (subtree subscribers of the root) and the event's issue.
type cardCostCorpus struct {
	projectID  uuid.UUID
	owner      uuid.UUID
	root       string
	leaf       string
	recipients []uuid.UUID
}

// seedCardCostCorpus builds the chain, `recipients` members subscribed to the
// root with subtree scope, and gives the FIRST recipient `extraSubs` further
// issue subscriptions on unrelated issues of the project. Bulk rows go in one
// INSERT … SELECT generate_series per table.
func seedCardCostCorpus(t *testing.T, pool *pgxpool.Pool, tag string, recipients, extraSubs int) cardCostCorpus {
	t.Helper()
	ctx := context.Background()

	owner := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, owner)
	prefix := "cc" + tag + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")

	// Chain: prefix-0 is the root, prefix-k is the child of prefix-(k-1).
	chain := make([]string, 0, cardCostDepth+1)
	for k := 0; k <= cardCostDepth; k++ {
		id := prefix + "-" + strconv.Itoa(k)
		seedTestIssue(t, pool, projectID, owner, id, 2)
		chain = append(chain, id)
		if k > 0 {
			if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`, id, chain[k-1], owner); err != nil {
				t.Fatalf("seed edge %s → %s: %v", id, chain[k-1], err)
			}
		}
	}

	// Recipients: users + memberships + subtree subscriptions on the root.
	userPrefix := "cc" + tag + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:6], "-", "")
	rows, err := pool.Query(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
SELECT gen_random_uuid(), $1 || g || '@test.local', $1 || g, $1 || g, 'user', 'fake-hash', now(), '{}'::jsonb
  FROM generate_series(1, $2) g
RETURNING id`, userPrefix, recipients)
	if err != nil {
		t.Fatalf("seed recipients: %v", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan recipient: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("seed recipients: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1::uuid[])`, ids)
	})
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
SELECT $1, u, 'member', now(), $2 FROM unnest($3::uuid[]) u
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, owner, ids); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since, scope)
SELECT $1, u, ARRAY['manual']::text[], now(), 'subtree' FROM unnest($2::uuid[]) u`, chain[0], ids); err != nil {
		t.Fatalf("seed subtree subscriptions: %v", err)
	}

	// Extra subscriptions of the first recipient, on issues outside the chain.
	if extraSubs > 0 {
		if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT $1 || '-x' || g, $2, NULL, 1, 'extra', '', 'task', 2, 'open',
       false, 'human', NULL, '', '', '', $3, now(), now(), '{}'::jsonb
  FROM generate_series(1, $4) g`, prefix, projectID, owner, extraSubs); err != nil {
			t.Fatalf("seed extra issues: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since, scope)
SELECT $1 || '-x' || g, $2, ARRAY['manual']::text[], now(), 'issue'
  FROM generate_series(1, $3) g`, prefix, ids[0], extraSubs); err != nil {
			t.Fatalf("seed extra subscriptions: %v", err)
		}
	}

	return cardCostCorpus{
		projectID: projectID, owner: owner,
		root: chain[0], leaf: chain[cardCostDepth], recipients: ids,
	}
}

// fanOutCardCost runs ONE fan-out of an issue.status_changed on the leaf with
// the tracer on, commits, and returns the statements FanOut sent. It then
// checks that every recipient got exactly one row carded to the root — the
// corpus is not allowed to be silently empty.
func fanOutCardCost(t *testing.T, pool *pgxpool.Pool, tracer *queryTracer, c cardCostCorpus) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	eid := addEvent(t, tx, "issue.status_changed", "issue", c.leaf, c.projectID, c.owner, "human")

	var fanErr error
	queries := tracer.record(func() {
		_, fanErr = FanOut(ctx, tx, FanOutInput{
			EventID:     eid,
			EventType:   "issue.status_changed",
			EntityType:  "issue",
			IssueID:     c.leaf,
			ProjectID:   c.projectID,
			ActorUserID: c.owner,
			ActorType:   "human",
		})
	})
	if fanErr != nil {
		t.Fatalf("FanOut: %v", fanErr)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var rows, carded int
	if err := pool.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE card_issue_id = $2)
  FROM notifications WHERE event_id = $1 AND user_id = ANY($3::uuid[])`,
		eid, c.root, c.recipients).Scan(&rows, &carded); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	if rows != len(c.recipients) || carded != len(c.recipients) {
		t.Fatalf("corpus of %d recipients produced %d rows, %d carded to the root %s — "+
			"the statement count of an incomplete fan-out proves nothing",
			len(c.recipients), rows, carded, c.root)
	}
	return queries
}

// TestFanOutCardCost_StatementsDoNotGrowWithRecipientsOrSubscriptions — No. 14.
func TestFanOutCardCost_StatementsDoNotGrowWithRecipientsOrSubscriptions(t *testing.T) {
	pool, tracer := newTracedTestPool(t)

	base := fanOutCardCost(t, pool, tracer, seedCardCostCorpus(t, pool, "a", 1, 0))
	manyRecipients := fanOutCardCost(t, pool, tracer, seedCardCostCorpus(t, pool, "b", 200, 0))
	manySubs := fanOutCardCost(t, pool, tracer, seedCardCostCorpus(t, pool, "c", 1, 500))

	for _, run := range []struct {
		label   string
		queries []string
	}{
		{"200 recipients", manyRecipients},
		{"500 subscriptions of the recipient", manySubs},
	} {
		if len(run.queries) != len(base) {
			t.Errorf("%s: FanOut sent %d statements, 1 recipient with 1 subscription sent %d — "+
				"the card is computed per recipient or per ancestor\nbase:\n%s\n%s:\n%s",
				run.label, len(run.queries), len(base), strings.Join(base, "\n---\n"),
				run.label, strings.Join(run.queries, "\n---\n"))
		}
	}
}
