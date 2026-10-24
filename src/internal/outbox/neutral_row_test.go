package outbox

// neutral_row_test.go — sk-622.21, T027, matrix row No. 56 (US-15 normative
// row fields, INV-14, INV-17 for the row, «one statement per tick»).
//
// Reference — spec US-15 / INV-14 / INV-17 and plan K16 / K17, written
// before the code:
//   - an issue event published on the bus carries `row`: the issue's list
//     row — title, priority, type, labels, components, pinned (the
//     normative list) plus `rowSeq` not smaller than at the moment of the
//     event;
//   - the row holds no reader-dependent field: no `iAssigned`, `isBlocked`,
//     `epicProgress` (INV-14) — the fixture makes each of them non-empty
//     for the creator, so a leak would show;
//   - `parentIdCurrent` is absent when the parent is in another project and
//     present when it is in the event's project (INV-17, K17); an event
//     filed under a project other than the issue's carries no row at all;
//   - the rows of a tick come from ONE statement: a tick with 1 issue event
//     and a tick with 100 send the same number of statements.
//
// Red if the row is built per event, carries another project's issue, or
// leaks a reader field.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// collectEnvelopes subscribes to every event subject of one project and
// returns a reader of what arrived, keyed by entity id (several envelopes
// per entity are kept in arrival order).
func collectEnvelopes(t *testing.T, nc *nats.Conn, projectID uuid.UUID) func() map[string][]map[string]json.RawMessage {
	t.Helper()
	var (
		mu  sync.Mutex
		got = map[string][]map[string]json.RawMessage{}
	)
	sub, err := nc.Subscribe(fmt.Sprintf("skein.events.project.%s.>", projectID), func(m *nats.Msg) {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var id string
		_ = json.Unmarshal(env["entityId"], &id)
		mu.Lock()
		got[id] = append(got[id], env)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush sub: %v", err)
	}
	return func() map[string][]map[string]json.RawMessage {
		_ = nc.Flush()
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string][]map[string]json.RawMessage, len(got))
		for k, v := range got {
			out[k] = append([]map[string]json.RawMessage(nil), v...)
		}
		return out
	}
}

// waitEnvelopes polls read until want(entity) holds for every entity.
func waitEnvelopes(t *testing.T, read func() map[string][]map[string]json.RawMessage, entities ...string) map[string][]map[string]json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := read()
		missing := 0
		for _, e := range entities {
			if len(got[e]) == 0 {
				missing++
			}
		}
		if missing == 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d expected envelopes did not arrive", missing, len(entities))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func rowSeqOf(t *testing.T, pool *pgxpool.Pool, id string) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(), `SELECT row_seq FROM issues WHERE id = $1`, id).Scan(&seq); err != nil {
		t.Fatalf("read row_seq of %s: %v", id, err)
	}
	return seq
}

func decodeRow(t *testing.T, where string, env map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	raw, ok := env["row"]
	if !ok {
		t.Fatalf("%s: envelope has no `row`", where)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("%s: row is not an object: %v (%s)", where, err, raw)
	}
	return row
}

func rowString(t *testing.T, row map[string]json.RawMessage, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(row[key], &s); err != nil {
		t.Fatalf("row.%s = %s, want a string", key, row[key])
	}
	return s
}

// TestNeutralRow_EnvelopeCarriesTheReaderIndependentRow — see the file
// header, every bullet but the last.
func TestNeutralRow_EnvelopeCarriesTheReaderIndependentRow(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	other := seedUser(t, pool)
	otherName := testsupport.Username(t, pool, other)
	pid, prefix := testsupport.SeedProject(t, pool, owner, "nrow")
	qid, qprefix := testsupport.SeedProject(t, pool, owner, "nrowq")
	// The assignee resolves only to a person who may work in the project
	// (INV-30, sk-674.18 T028): other is a member of P.
	testsupport.AddProjectMember(t, pool, pid, other, "member")
	resetOutbox(t, pool, pid)
	resetOutbox(t, pool, qid)

	// X — the subject: in P, parent Y lives in Q; carries labels, a
	// component, pinned; assigned by the creator to someone else
	// (iAssigned for the creator) and blocked by an open issue
	// (isBlocked).
	x := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "neutral row subject", "open", 1)
	y := testsupport.SeedIssue(t, pool, qid, owner, qprefix+"-1", "parent elsewhere", "open", 2)
	testsupport.SeedDep(t, pool, x, y, "parent-child", owner)
	testsupport.SetPinned(t, pool, x, true)
	testsupport.SetAssignee(t, pool, x, otherName)
	blocker := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-2", "blocker", "open", 2)
	testsupport.SeedDep(t, pool, x, blocker, "blocks", owner)
	comp := testsupport.SeedComponent(t, pool, pid, owner, "nrow-comp", "")
	if _, err := pool.Exec(ctx, `INSERT INTO issue_labels (issue_id, label) VALUES ($1, 'beta'), ($1, 'alpha')`, x); err != nil {
		t.Fatalf("seed labels: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`, x, comp); err != nil {
		t.Fatalf("seed component link: %v", err)
	}
	// W — an epic in P (epicProgress for any reader) and the parent of Z,
	// so Z's parent is in the event's project.
	w := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-3", "epic in P", "open", 2)
	if _, err := pool.Exec(ctx, `UPDATE issues SET type = 'epic' WHERE id = $1`, w); err != nil {
		t.Fatalf("make epic: %v", err)
	}
	z := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-4", "child in P", "open", 3)
	testsupport.SeedDep(t, pool, z, w, "parent-child", owner)

	seqXAtEvent := rowSeqOf(t, pool, x)
	readP := collectEnvelopes(t, nc, pid)
	readQ := collectEnvelopes(t, nc, qid)
	insertEvent(t, pool, owner, pid, "issue.updated", "issue", x, "", time.Time{})
	insertEvent(t, pool, owner, pid, "issue.updated", "issue", z, "", time.Time{})
	insertEvent(t, pool, owner, pid, "issue.updated", "issue", w, "", time.Time{})
	// X's event filed under the foreign project Q.
	insertEvent(t, pool, owner, qid, "issue.updated", "issue", x, "", time.Time{})

	d := New(pool, nc, Options{BatchSize: 50, Rows: issues.NeutralRowSource{}})
	drainProject(t, d, pool, pid)
	drainProject(t, d, pool, qid)

	gotP := waitEnvelopes(t, readP, x, z, w)
	gotQ := waitEnvelopes(t, readQ, x)

	// --- X: the normative fields, rowSeq, no reader fields, no foreign parent.
	row := decodeRow(t, "X", gotP[x][0])
	if s := rowString(t, row, "id"); s != x {
		t.Errorf("row.id = %q, want %q", s, x)
	}
	if s := rowString(t, row, "projectId"); s != pid.String() {
		t.Errorf("row.projectId = %q, want the event's project %s", s, pid)
	}
	if s := rowString(t, row, "title"); s != "neutral row subject" {
		t.Errorf("row.title = %q", s)
	}
	if s := rowString(t, row, "type"); s != "task" {
		t.Errorf("row.type = %q, want task", s)
	}
	var priority int
	if err := json.Unmarshal(row["priority"], &priority); err != nil || priority != 1 {
		t.Errorf("row.priority = %s, want 1", row["priority"])
	}
	var pinned bool
	if err := json.Unmarshal(row["pinned"], &pinned); err != nil || !pinned {
		t.Errorf("row.pinned = %s, want true", row["pinned"])
	}
	var labels []string
	if err := json.Unmarshal(row["labels"], &labels); err != nil {
		t.Fatalf("row.labels = %s: %v", row["labels"], err)
	}
	sort.Strings(labels)
	if fmt.Sprint(labels) != "[alpha beta]" {
		t.Errorf("row.labels = %v, want [alpha beta]", labels)
	}
	var comps []string
	if err := json.Unmarshal(row["componentIds"], &comps); err != nil || len(comps) != 1 || comps[0] != comp.String() {
		t.Errorf("row.componentIds = %s, want [%s]", row["componentIds"], comp)
	}
	if s := rowString(t, row, "assigneeUserId"); s != other.String() {
		t.Errorf("row.assigneeUserId = %q, want %s — the client derives iAssigned from it", s, other)
	}
	var seq int64
	if err := json.Unmarshal(row["rowSeq"], &seq); err != nil {
		t.Fatalf("row.rowSeq = %s: %v", row["rowSeq"], err)
	}
	if seq < seqXAtEvent || seq <= 0 {
		t.Errorf("row.rowSeq = %d, want ≥ %d (the order at the moment of the event)", seq, seqXAtEvent)
	}
	for _, k := range []string{"iAssigned", "isBlocked", "epicProgress"} {
		if _, ok := row[k]; ok {
			t.Errorf("X: row carries reader field %q = %s (INV-14)", k, row[k])
		}
	}
	if _, ok := row["parentIdCurrent"]; ok {
		t.Errorf("X: row.parentIdCurrent = %s, want absent — the parent is in another project (INV-17)",
			row["parentIdCurrent"])
	}

	// --- Z: the parent in the event's project is carried.
	rowZ := decodeRow(t, "Z", gotP[z][0])
	if s := rowString(t, rowZ, "parentIdCurrent"); s != w {
		t.Errorf("Z: row.parentIdCurrent = %q, want %q (parent in the same project)", s, w)
	}

	// --- W: an epic row has no epicProgress either.
	rowW := decodeRow(t, "W", gotP[w][0])
	if _, ok := rowW["epicProgress"]; ok {
		t.Errorf("W: row carries epicProgress = %s (INV-14)", rowW["epicProgress"])
	}

	// --- X under Q: no row at all.
	if raw, ok := gotQ[x][0]["row"]; ok {
		t.Errorf("event of %s filed under project %s carries row %s — the row would carry another "+
			"project's issue to that project's readers (INV-17)", x, qid, raw)
	}
}

// statementCounter counts the statements a pool sends while recording.
type statementCounter struct {
	mu  sync.Mutex
	on  bool
	sql []string
}

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	if c.on {
		c.sql = append(c.sql, d.SQL)
	}
	c.mu.Unlock()
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *statementCounter) record(fn func()) []string {
	c.mu.Lock()
	c.on, c.sql = true, nil
	c.mu.Unlock()
	fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on = false
	return append([]string(nil), c.sql...)
}

// TestNeutralRow_OneStatementPerTick — a tick with 1 issue event and a tick
// with 100 send the same statements; every published envelope carries its
// row.
func TestNeutralRow_OneStatementPerTick(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	owner := seedUser(t, pool)
	pid, prefix := testsupport.SeedProject(t, pool, owner, "nrowc")
	resetOutbox(t, pool, pid)

	ids := make([]string, 100)
	for i := range ids {
		ids[i] = testsupport.SeedIssue(t, pool, pid, owner, fmt.Sprintf("%s-%d", prefix, i+1),
			fmt.Sprintf("count %d", i+1), "open", 2)
	}

	cfg := pool.Config().Copy()
	counter := &statementCounter{}
	cfg.ConnConfig.Tracer = counter
	tracedPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(tracedPool.Close)
	d := New(tracedPool, nc, Options{BatchSize: 200, Rows: issues.NeutralRowSource{}})

	// Empty the shared queue first, so the measured ticks see only ours.
	for i := 0; ; i++ {
		n, err := d.tick(context.Background())
		if err != nil {
			t.Fatalf("pre-drain tick: %v", err)
		}
		if n == 0 {
			break
		}
		if i > 1000 {
			t.Fatalf("shared outbox does not empty")
		}
	}

	read := collectEnvelopes(t, nc, pid)
	measure := func(events []string) []string {
		for _, id := range events {
			insertEvent(t, pool, owner, pid, "issue.updated", "issue", id, "", time.Time{})
		}
		var n int
		stmts := counter.record(func() {
			var err error
			n, err = d.tick(context.Background())
			if err != nil {
				t.Fatalf("tick: %v", err)
			}
		})
		if n != len(events) {
			t.Fatalf("tick retired %d events, want exactly the %d inserted — another writer "+
				"disturbed the measurement", n, len(events))
		}
		return stmts
	}

	one := measure(ids[:1])
	hundred := measure(ids)
	if len(one) != len(hundred) {
		t.Errorf("tick with 1 issue event sent %d statements, with 100 — %d; want equal (one row "+
			"statement per tick)\n1:   %q\n100: %q", len(one), len(hundred), one, hundred)
	}

	// 1 + 100 envelopes were published; wait for all of them.
	waitEnvelopes(t, read, ids...)
	var got map[string][]map[string]json.RawMessage
	for deadline := time.Now().Add(5 * time.Second); ; {
		got = read()
		total := 0
		for _, envs := range got {
			total += len(envs)
		}
		if total >= 101 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 101 envelopes arrived", total)
		}
		time.Sleep(20 * time.Millisecond)
	}
	withRow := 0
	for _, id := range ids {
		for _, env := range got[id] {
			if _, ok := env["row"]; ok {
				withRow++
			}
		}
	}
	if withRow != 101 {
		t.Errorf("%d of 101 published issue envelopes carry a row, want all", withRow)
	}
}
