package notifications

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fanout_card_plan_test.go — sk-622.21, T012, matrix row No. 15 (SC-012 in its
// scaled form — decision R16 item 2 — and SC-002 for the fan-out). Run: final.
//
// The property: the work of one fan-out does not grow with how many
// subscriptions each recipient holds, and its plan reads no big table
// sequentially. Measured, not timed:
//
//   - the statements are the ones FanOut actually sends, captured with their
//     arguments by a pgx tracer — the test never re-types the SQL;
//   - each captured statement is re-run as EXPLAIN (ANALYZE, BUFFERS, FORMAT
//     JSON) inside a transaction that is rolled back, and the pages of the
//     top node (Shared Hit Blocks + Shared Read Blocks, inclusive of the
//     subtree) are summed over the statements;
//   - corpora: 200 recipients, the event's issue at depth 5 under the root
//     each of them watches with subtree scope, and every recipient holding
//     10 vs 1 000 further issue subscriptions. Literal bound of R16 item 2:
//     pages(1 000) ≤ 3 × pages(10);
//   - no plan node is a Seq Scan on notifications, issue_watchers,
//     issue_watch_optouts, issues, dependencies or users (JSON plan walked by
//     Node Type and Relation Name). Tables are ANALYZEd after seeding —
//     on unanalysed small tables the planner rightly picks Seq Scan.

const (
	cardPlanRecipients = 200
	cardPlanDepth      = 5
	cardPlanFewSubs    = 10
	cardPlanManySubs   = 1000
)

// cardPlanTables are the relations no fan-out plan node may Seq Scan.
var cardPlanTables = map[string]bool{
	"notifications":       true,
	"issue_watchers":      true,
	"issue_watch_optouts": true,
	"issues":              true,
	"dependencies":        true,
	"users":               true,
}

type cardPlanStmt struct {
	sql  string
	args []any
}

// cardPlanCapture records every statement of the connection with its
// arguments while on.
type cardPlanCapture struct {
	mu    sync.Mutex
	on    bool
	stmts []cardPlanStmt
}

func (c *cardPlanCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on {
		c.stmts = append(c.stmts, cardPlanStmt{sql: d.SQL, args: d.Args})
	}
	return ctx
}

func (c *cardPlanCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *cardPlanCapture) record(fn func()) []cardPlanStmt {
	c.mu.Lock()
	c.stmts, c.on = nil, true
	c.mu.Unlock()
	fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on = false
	return append([]cardPlanStmt(nil), c.stmts...)
}

func newCardPlanPool(t *testing.T) (*pgxpool.Pool, *cardPlanCapture) {
	t.Helper()
	url := os.Getenv(testDBURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDBURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	c := &cardPlanCapture{}
	cfg.ConnConfig.Tracer = c
	cfg.MinConns, cfg.MaxConns = 1, 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, c
}

type cardPlanNode struct {
	NodeType     string         `json:"Node Type"`
	RelationName string         `json:"Relation Name"`
	SharedHit    int64          `json:"Shared Hit Blocks"`
	SharedRead   int64          `json:"Shared Read Blocks"`
	Plans        []cardPlanNode `json:"Plans"`
}

// seqScans lists "Seq Scan on <relation>" for the guarded relations.
func (n cardPlanNode) seqScans() []string {
	var out []string
	if n.NodeType == "Seq Scan" && cardPlanTables[n.RelationName] {
		out = append(out, n.RelationName)
	}
	for _, c := range n.Plans {
		out = append(out, c.seqScans()...)
	}
	return out
}

// seedCardPlanCorpus: chain root ← … ← leaf of depth cardPlanDepth, the
// recipients (members, subtree subscription on the root), and `subs` further
// issues every recipient is subscribed to. One INSERT … SELECT per table.
func seedCardPlanCorpus(t *testing.T, pool *pgxpool.Pool, tag string, subs int) (projectID, owner uuid.UUID, root, leaf string) {
	t.Helper()
	ctx := context.Background()
	owner = seedTestUser(t, pool, "user")
	projectID = seedTestProject(t, pool, owner)
	prefix := "cp" + tag + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")

	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT $1 || '-' || g, $2, NULL, 1, 'plan', '', 'task', 2, 'open',
       false, 'human', NULL, '', '', '', $3, now(), now(), '{}'::jsonb
  FROM generate_series(0, $4) g`, prefix, projectID, owner, cardPlanDepth); err != nil {
		t.Fatalf("seed chain issues: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
SELECT $1 || '-' || g, $1 || '-' || (g - 1), 'parent-child', '{}'::jsonb, 1, $2, now()
  FROM generate_series(1, $3) g`, prefix, owner, cardPlanDepth); err != nil {
		t.Fatalf("seed chain edges: %v", err)
	}
	root, leaf = prefix+"-0", prefix+"-"+strconv.Itoa(cardPlanDepth)

	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT $1 || '-x' || g, $2, NULL, 1, 'other', '', 'task', 2, 'open',
       false, 'human', NULL, '', '', '', $3, now(), now(), '{}'::jsonb
  FROM generate_series(1, $4) g`, prefix, projectID, owner, subs); err != nil {
		t.Fatalf("seed other issues: %v", err)
	}

	var ids []uuid.UUID
	rows, err := pool.Query(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
SELECT gen_random_uuid(), $1 || g || '@test.local', $1 || g, $1 || g, 'user', 'fake-hash', now(), '{}'::jsonb
  FROM generate_series(1, $2) g
RETURNING id`, prefix+"u", cardPlanRecipients)
	if err != nil {
		t.Fatalf("seed recipients: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan recipient: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1::uuid[])`, ids)
	})
	// A broken RETURNING stream must fail here with its own cause, not later
	// as a short recipient count.
	if err := rows.Err(); err != nil {
		t.Fatalf("seed recipients: %v", err)
	}
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
SELECT $1, u, ARRAY['manual']::text[], now(), 'subtree' FROM unnest($2::uuid[]) u`, root, ids); err != nil {
		t.Fatalf("seed subtree subscriptions: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since, scope)
SELECT $1 || '-x' || g, u, ARRAY['manual']::text[], now(), 'issue'
  FROM unnest($2::uuid[]) u CROSS JOIN generate_series(1, $3) g`, prefix, ids, subs); err != nil {
		t.Fatalf("seed other subscriptions: %v", err)
	}
	return projectID, owner, root, leaf
}

// cardPlanBackground is the number of unrelated issues, in parent-child
// chains, seeded beside the corpus: the ancestor walk looks edges up by
// from_id, and on a few dozen rows a sequential scan of dependencies is the
// planner's right answer — the Seq Scan gate would judge the fixture, not
// the product.
const cardPlanBackground = 20000

// cardPlanBackgroundUsers is the size of the user directory seeded beside
// the corpora, for the same reason. The admin fallback of the candidate
// query (`EXISTS (SELECT 1 FROM users u WHERE u.id = <watcher> AND
// u.role = 'admin' …)`) is planned either as one primary-key probe per
// candidate row or as a hashed subplan built by ONE sequential pass over
// users; the planner takes the cheaper. The pass costs in proportion to the
// directory and not to anyone's subscriptions; the probes cost in proportion
// to the estimated candidate rows, which the 1 000-subscription corpus puts
// at ~150 for the issue's own watchers. On the few hundred users of the
// bare fixture the pass is the honest choice and the gate judged the
// fixture. Measured by EXPLAIN at stage 7 (sk-634.7): the 10-subscription
// corpus probes by key from 10 000 users on, the 1 000-subscription one
// still passes over users at 50 000 and probes by key at 100 000. So the
// directory is a size at which a pass would matter, and the gate keeps its
// full meaning there: no Seq Scan on users.
//
// Cost: seeding is in statements of 500 (the users trigger of 000060 takes
// an advisory lock per handle, three per user — one statement of the whole
// directory exhausts the lock table); the cleanup DELETE is the slow half,
// every row running the users foreign-key checks.
const cardPlanBackgroundUsers = 100000

// seedCardPlanBackgroundUsers adds cardPlanBackgroundUsers plain users,
// members of nothing, and removes them at cleanup.
func seedCardPlanBackgroundUsers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	tag := "cpbu" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE username LIKE $1 || '%'`, tag)
	})
	for from := 1; from <= cardPlanBackgroundUsers; from += 500 {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
SELECT gen_random_uuid(), $1 || g || '@test.local', $1 || g, $1 || g, 'user', 'fake-hash', now(), '{}'::jsonb
  FROM generate_series($2::int, LEAST($2::int + 499, $3::int)) g`, tag, from, cardPlanBackgroundUsers); err != nil {
			t.Fatalf("seed background users: %v", err)
		}
	}
}

// seedCardPlanBackground adds cardPlanBackground issues in parent-child
// chains, unrelated to the corpus; the project's cleanup removes them and
// their edges.
func seedCardPlanBackground(t *testing.T, pool *pgxpool.Pool, projectID, owner uuid.UUID, tag string) {
	t.Helper()
	ctx := context.Background()
	prefix := "cpbg" + tag + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT $1 || '-b' || g, $2, NULL, 1, 'background', '', 'task', 2, 'open',
       false, 'human', NULL, '', '', '', $3, now(), now(), '{}'::jsonb
  FROM generate_series(1, $4) g`, prefix, projectID, owner, cardPlanBackground); err != nil {
		t.Fatalf("seed background issues: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
SELECT $1 || '-b' || g, $1 || '-b' || (g - 1), 'parent-child', '{}'::jsonb, 1, $2, now()
  FROM generate_series(2, $3) g
 WHERE g % 20 <> 1`, prefix, owner, cardPlanBackground); err != nil {
		t.Fatalf("seed background edges: %v", err)
	}
}

// measureCardPlan runs one fan-out on the leaf, captures its statements, and
// returns the summed pages of their EXPLAIN (ANALYZE, BUFFERS) re-runs and
// the guarded Seq Scans found. It also checks that the fan-out wrote every
// recipient's row with the root as card, so the measurement is of real work.
func measureCardPlan(t *testing.T, pool *pgxpool.Pool, capture *cardPlanCapture, tag string, subs int) (int64, []string) {
	t.Helper()
	ctx := context.Background()
	projectID, owner, root, leaf := seedCardPlanCorpus(t, pool, tag, subs)
	seedCardPlanBackground(t, pool, projectID, owner, tag)
	for _, tbl := range []string{"notifications", "issue_watchers", "issue_watch_optouts", "issues", "dependencies", "users", "project_members"} {
		if _, err := pool.Exec(ctx, "ANALYZE "+tbl); err != nil {
			t.Fatalf("analyze %s: %v", tbl, err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	eid := addEvent(t, tx, "issue.status_changed", "issue", leaf, projectID, owner, "human")
	var fanErr error
	stmts := capture.record(func() {
		_, fanErr = FanOut(ctx, tx, FanOutInput{
			EventID: eid, EventType: "issue.status_changed", EntityType: "issue",
			IssueID: leaf, ProjectID: projectID, ActorUserID: owner, ActorType: "human",
		})
	})
	if fanErr != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", fanErr)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var carded int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE event_id = $1 AND card_issue_id = $2`, eid, root).
		Scan(&carded); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	if carded != cardPlanRecipients {
		t.Fatalf("fan-out wrote %d rows carded %s, want %d — the measurement would be of an incomplete fan-out",
			carded, root, cardPlanRecipients)
	}

	// Re-run every captured statement under EXPLAIN in a rolled-back
	// transaction (the INSERT then conflicts with the committed rows, which
	// still reads the same pages to find out).
	etx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain: %v", err)
	}
	defer func() { _ = etx.Rollback(ctx) }()
	var pages int64
	var seqs []string
	explained := 0
	for _, s := range stmts {
		head := strings.ToUpper(strings.TrimSpace(s.sql))
		if strings.HasPrefix(head, "SAVEPOINT") || strings.HasPrefix(head, "RELEASE") ||
			strings.HasPrefix(head, "ROLLBACK") {
			continue
		}
		var raw []byte
		if err := etx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+s.sql, s.args...).Scan(&raw); err != nil {
			t.Fatalf("explain %q: %v", firstLine(s.sql), err)
		}
		var plan []struct {
			Plan cardPlanNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plan); err != nil || len(plan) == 0 {
			t.Fatalf("parse plan of %q: %v", firstLine(s.sql), err)
		}
		pages += plan[0].Plan.SharedHit + plan[0].Plan.SharedRead
		seqs = append(seqs, plan[0].Plan.seqScans()...)
		explained++
	}
	if explained == 0 {
		t.Fatalf("no fan-out statement was captured — nothing measured")
	}
	return pages, seqs
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestFanOutCardPlan_PagesBoundedBySubscriptionsAndNoSeqScan — No. 15.
func TestFanOutCardPlan_PagesBoundedBySubscriptionsAndNoSeqScan(t *testing.T) {
	pool, capture := newCardPlanPool(t)
	seedCardPlanBackgroundUsers(t, pool)

	few, fewSeq := measureCardPlan(t, pool, capture, "f", cardPlanFewSubs)
	many, manySeq := measureCardPlan(t, pool, capture, "m", cardPlanManySubs)

	if few == 0 {
		t.Fatalf("pages with %d subscriptions = 0 — the measurement read nothing", cardPlanFewSubs)
	}
	if many > 3*few {
		t.Errorf("fan-out pages: %d with %d subscriptions per recipient, %d with %d — more than 3× (SC-012, R16 item 2)",
			many, cardPlanManySubs, few, cardPlanFewSubs)
	}
	for label, seqs := range map[string][]string{"10 subscriptions": fewSeq, "1000 subscriptions": manySeq} {
		if len(seqs) > 0 {
			t.Errorf("%s: fan-out plan has Seq Scan on %v (SC-002)", label, seqs)
		}
	}
}
