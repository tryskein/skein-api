package issues

// row_seq_test.go — sk-622.21, T026, matrix row No. 55 (US-15 freshness order,
// decision K16), the part of the row that lives in internal/issues. The
// migration up/down/up part is migrations/migration_000064_test.go.
//
// Reference (spec US-15, plan K16): the row of an issue has a freshness
// order that distinguishes EVERY committed change of the row — including
// writes that do not bump `version` (adding and removing a label is the
// confirmed example) — and for one issue a greater order is a later
// commit. The order is handed out on the WRITE, not at the start of the
// transaction. REST rows of lists and of the tree, and the card, carry it as
// `rowSeq`.
//
// What turns each test red:
//   - TestRowSeq_EveryRowWriteMovesTheOrder — a product write of the issue
//     row that does not move the order (a label path that stopped touching
//     the row, a trigger limited to some columns);
//   - TestRowSeq_ConcurrentWrites_RowLockWait (order 1) — Claim's write,
//     made after waiting on A's row lock, that leaves the order at or below
//     A's (its path does not stamp the row). It does NOT catch an order
//     taken at the start of B's transaction: B starts after A has already
//     written, so any value B takes is above A's;
//   - TestRowSeq_ConcurrentWrites_EarlierStartCommitsLater (order 2) —
//     Claim that does not come through a 40001 by replaying (the test
//     requires ≥ 2 attempts), or whose committed write does not land above
//     A. It does NOT catch an order taken at the start of a transaction
//     either: the replay is a new transaction that starts after A committed;
//   - TestRowSeq_ConcurrentWrites_EarlierStartWaitsOnRowLock (order 3) —
//     an order taken at the start of the transaction (or at its first
//     write) rather than on the write of this row: B starts and writes
//     before A, waits on A's row lock in READ COMMITTED and commits after A
//     in the same transaction, so such an order leaves B below A;
//   - TestRowSeq_ParentChildEdgeMovesChildOrder — dep_add / dep_remove of a
//     parent-child edge that does not write the child's row (R26 item 3);
//   - TestRowSeq_ReadPathsCarryIt — a read path that does not project the
//     column (rowSeq absent / zero).
// The read-snapshot part (labels from the same statement as rowSeq) is
// row_seq_snapshot_test.go.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
)

// rowSeqFixture — one user, one project, one issue created through the
// product path.
type rowSeqFixture struct {
	pool    *pgxpool.Pool
	store   *Store
	owner   uuid.UUID
	name    string
	project uuid.UUID
	prefix  string
	issueID string
}

func newRowSeqFixture(t *testing.T) rowSeqFixture {
	t.Helper()
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	name := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	issue, err := store.CreateIssue(context.Background(), pid, prefix, "counter",
		CreateIssueInput{Title: "row seq subject", Type: "task", Priority: 2},
		owner, HumanActor(owner, name))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	return rowSeqFixture{pool: pool, store: store, owner: owner, name: name, project: pid, prefix: prefix, issueID: issue.ID}
}

// rowState reads the committed order and version of the issue straight
// from the table.
func rowState(t *testing.T, pool *pgxpool.Pool, id string) (seq int64, version int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT row_seq, version FROM issues WHERE id = $1`, id).Scan(&seq, &version); err != nil {
		t.Fatalf("read row_seq of %s: %v", id, err)
	}
	return seq, version
}

// TestRowSeq_EveryRowWriteMovesTheOrder — each product write of the issue
// row, applied in turn, leaves a strictly greater order than the one
// before it. The two label writes additionally keep `version` where it
// was: that is the case US-15 names, where version cannot tell the states
// apart and the order must.
func TestRowSeq_EveryRowWriteMovesTheOrder(t *testing.T) {
	f := newRowSeqFixture(t)
	ctx := context.Background()
	actor := HumanActor(f.owner, f.name)
	comp := seedComponent(t, f.pool, f.project, f.owner, "rowseq-"+uniqueTag())

	steps := []struct {
		name         string
		keepsVersion bool
		write        func() error
	}{
		{"AddLabel", true, func() error {
			_, err := f.store.AddLabelNoLoad(ctx, f.issueID, "rowseq", actor, LockOpts{})
			return err
		}},
		{"RemoveLabel", true, func() error {
			_, err := f.store.RemoveLabelNoLoad(ctx, f.issueID, "rowseq", actor, LockOpts{})
			return err
		}},
		{"ReplaceComponents", false, func() error {
			_, err := f.store.ReplaceComponentsNoLoad(ctx, f.issueID, []uuid.UUID{comp}, actor, LockOpts{})
			return err
		}},
		{"PatchIssue title", false, func() error {
			_, version := rowState(t, f.pool, f.issueID)
			title := "row seq subject, renamed"
			_, err := f.store.PatchIssue(ctx, f.issueID, PatchIssueInput{ExpectedVersion: version, Title: &title}, actor)
			return err
		}},
		{"Claim", false, func() error {
			_, err := f.store.Claim(ctx, ClaimInput{ID: f.issueID, SessionID: "rowseq-" + uniqueTag(), Actor: actor})
			return err
		}},
	}

	for _, s := range steps {
		beforeSeq, beforeVersion := rowState(t, f.pool, f.issueID)
		if err := s.write(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		afterSeq, afterVersion := rowState(t, f.pool, f.issueID)
		if afterSeq <= beforeSeq {
			t.Errorf("%s: row_seq %d → %d, want it to grow — every committed write of the row moves the order",
				s.name, beforeSeq, afterSeq)
		}
		if s.keepsVersion && afterVersion != beforeVersion {
			t.Fatalf("%s: version %d → %d, want unchanged — the case under test is a write that does "+
				"not move version", s.name, beforeVersion, afterVersion)
		}
	}
}

// TestRowSeq_ConcurrentWrites_RowLockWait — order 1: transaction A has
// written the row and holds its lock; Claim (B) blocks on that lock. A
// commits, B finishes (through its retry). B's order is greater than A's.
func TestRowSeq_ConcurrentWrites_RowLockWait(t *testing.T) {
	f := newRowSeqFixture(t)
	ctx := context.Background()

	appName := "rowseq-wait-" + uniqueTag()
	claimStore := NewStore(newTaggedPool(t, appName))

	a, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = a.Rollback(ctx) }()
	var seqA int64
	if err := a.QueryRow(ctx,
		`UPDATE issues SET updated_at = now() WHERE id = $1 RETURNING row_seq`, f.issueID).Scan(&seqA); err != nil {
		t.Fatalf("A: UPDATE: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := claimStore.Claim(context.WithoutCancel(ctx), ClaimInput{
			ID: f.issueID, SessionID: "rowseq-" + uniqueTag(), Actor: HumanActor(f.owner, f.name),
		})
		done <- err
	}()
	if !waitForBlocked(t, f.pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate order 1: Claim never blocked on A's row lock")
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Claim after A committed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Claim did not return within 30s")
	}

	seqB, _ := rowState(t, f.pool, f.issueID)
	if seqB <= seqA {
		t.Errorf("row_seq after B = %d, A's = %d; want B > A — B committed after A", seqB, seqA)
	}
}

// rowSeqHoldTracer parks its connection at the END of the first statement
// containing the armed marker and waits for release; one-shot, so the retry
// of the same operation runs free. It also counts how many times a
// statement containing the marker finished — i.e. how many attempts ran.
type rowSeqHoldTracer struct {
	mu      sync.Mutex
	marker  string
	armed   bool
	runs    int
	reached chan struct{}
	release chan struct{}
}

type rowSeqSQLKey struct{}

func (h *rowSeqHoldTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, rowSeqSQLKey{}, d.SQL)
}

func (h *rowSeqHoldTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(rowSeqSQLKey{}).(string)
	h.mu.Lock()
	if h.marker == "" || !strings.Contains(sql, h.marker) {
		h.mu.Unlock()
		return
	}
	h.runs++
	if !h.armed {
		h.mu.Unlock()
		return
	}
	h.armed = false
	reached, release := h.reached, h.release
	h.mu.Unlock()
	close(reached)
	<-release
}

func (h *rowSeqHoldTracer) arm(marker string) (reached, release chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.marker, h.armed, h.runs = marker, true, 0
	h.reached, h.release = make(chan struct{}), make(chan struct{})
	return h.reached, h.release
}

func (h *rowSeqHoldTracer) attempts() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs
}

// fetchIssueInTxMarker is a fragment of store.go#fetchIssueInTx's SELECT —
// the read with which Claim's transaction opens.
const fetchIssueInTxMarker = "estimated_minutes, due_at, defer_until, external_ref, source_context"

// TestRowSeq_ConcurrentWrites_EarlierStartCommitsLater — order 2: Claim (B)
// opens its SERIALIZABLE transaction and reads the issue, then is held by
// the test pool's tracer (first attempt only). AddLabel (A) changes the row
// and commits. B is released, its write takes 40001, and the retry wrapper
// replays it. B started first and committed last: its order must be the
// greater one. An order handed out at the start of a transaction would give
// B the smaller value.
func TestRowSeq_ConcurrentWrites_EarlierStartCommitsLater(t *testing.T) {
	f := newRowSeqFixture(t)
	ctx := context.Background()

	url := os.Getenv(testDatabaseURL)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURL, err)
	}
	tracer := &rowSeqHoldTracer{}
	cfg.ConnConfig.Tracer = tracer
	heldPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(heldPool.Close)
	claimStore := NewStore(heldPool)

	reached, release := tracer.arm(fetchIssueInTxMarker)
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})

	done := make(chan error, 1)
	go func() {
		_, err := claimStore.Claim(context.WithoutCancel(ctx), ClaimInput{
			ID: f.issueID, SessionID: "rowseq-" + uniqueTag(), Actor: HumanActor(f.owner, f.name),
		})
		done <- err
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("could not orchestrate order 2: Claim never read the issue")
	}

	if _, err := f.store.AddLabelNoLoad(ctx, f.issueID, "rowseq-a", HumanActor(f.owner, f.name), LockOpts{}); err != nil {
		t.Fatalf("A: AddLabel: %v", err)
	}
	seqA, _ := rowState(t, f.pool, f.issueID)

	close(release)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Claim (B): %v, want success through the retry wrapper", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Claim (B) did not return within 30s")
	}
	if n := tracer.attempts(); n < 2 {
		t.Fatalf("Claim read the issue %d time(s), want ≥ 2 — B was meant to take 40001 and replay; "+
			"the scenario did not happen", n)
	}

	seqB, _ := rowState(t, f.pool, f.issueID)
	if seqB <= seqA {
		t.Errorf("row_seq after B = %d, after A = %d; want B > A — B started first but committed last", seqB, seqA)
	}
}

// TestRowSeq_ConcurrentWrites_EarlierStartWaitsOnRowLock — order 3, the one
// without a rollback (order 2 in review). Transaction B (READ COMMITTED)
// starts first and writes another issue — from then on it is a transaction
// that has begun writing. Transaction A then writes the subject issue and
// holds its row lock. B's write of the subject waits on that lock; A
// commits; B's write goes through and B commits — the SAME transaction
// that started before A, no 40001 and no replay. B committed last: its
// order must be the greater one. An order handed out when the transaction
// starts (or at its first write) gives B the value it took before A, i.e.
// the smaller one.
func TestRowSeq_ConcurrentWrites_EarlierStartWaitsOnRowLock(t *testing.T) {
	f := newRowSeqFixture(t)
	ctx := context.Background()
	other, err := f.store.CreateIssue(ctx, f.project, f.prefix, "counter",
		CreateIssueInput{Title: "row seq other", Type: "task", Priority: 2},
		f.owner, HumanActor(f.owner, f.name))
	if err != nil {
		t.Fatalf("CreateIssue other: %v", err)
	}

	b, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = b.Rollback(ctx) }()
	var bPID int32
	if err := b.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&bPID); err != nil {
		t.Fatalf("B: pid: %v", err)
	}
	if _, err := b.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, other.ID); err != nil {
		t.Fatalf("B: first write (other issue): %v", err)
	}

	a, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = a.Rollback(ctx) }()
	if _, err := a.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, f.issueID); err != nil {
		t.Fatalf("A: UPDATE: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(context.WithoutCancel(ctx), `UPDATE issues SET updated_at = now() WHERE id = $1`, f.issueID)
		done <- err
	}()
	if !waitForPIDBlocked(t, f.pool, bPID, 10*time.Second) {
		t.Fatalf("could not orchestrate order 3: B never waited on A's row lock")
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	seqA, _ := rowState(t, f.pool, f.issueID)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("B: UPDATE after A committed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("B's UPDATE did not return within 30s")
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatalf("commit B: %v", err)
	}

	seqB, _ := rowState(t, f.pool, f.issueID)
	if seqB <= seqA {
		t.Errorf("row_seq after B = %d, after A = %d; want B > A — B started before A and committed after it "+
			"in the same transaction", seqB, seqA)
	}
}

// TestRowSeq_ParentChildEdgeMovesChildOrder — decision R26 item 3 (found in
// review). The child's row carries `parentIdCurrent`, derived
// from the parent-child edge; adding the edge without Reparent (dep_add,
// and the MCP / REST wrappers over dependencies.Store.Create) and removing
// it (dep_remove → Delete) change that field, so each must move the child's
// order. The child's `version` stays: an edge change is not a versioned
// edit of the issue (no expectedVersion on dep_add / dep_remove).
func TestRowSeq_ParentChildEdgeMovesChildOrder(t *testing.T) {
	f := newRowSeqFixture(t)
	ctx := context.Background()
	parent, err := f.store.CreateIssue(ctx, f.project, f.prefix, "counter",
		CreateIssueInput{Title: "row seq parent", Type: "epic", Priority: 2},
		f.owner, HumanActor(f.owner, f.name))
	if err != nil {
		t.Fatalf("CreateIssue parent: %v", err)
	}
	deps := dependencies.NewStore(f.pool)
	caller := dependencies.CallerAccess{UserID: f.owner, Role: "user"}
	actor := dependencies.HumanActor(f.owner, f.name)

	steps := []struct {
		name  string
		write func() error
	}{
		{"dep_add parent-child", func() error {
			_, err := deps.Create(ctx, f.issueID, dependencies.CreateDepInput{
				ToID: parent.ID, Type: dependencies.DepParentChild,
			}, caller, actor)
			return err
		}},
		{"dep_remove parent-child", func() error {
			return deps.Delete(ctx, f.issueID, parent.ID, dependencies.DepParentChild, actor)
		}},
	}
	for _, s := range steps {
		beforeSeq, beforeVersion := rowState(t, f.pool, f.issueID)
		if err := s.write(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		afterSeq, afterVersion := rowState(t, f.pool, f.issueID)
		if afterSeq <= beforeSeq {
			t.Errorf("%s: child row_seq %d → %d, want it to grow — parentIdCurrent of the row changed",
				s.name, beforeSeq, afterSeq)
		}
		if afterVersion != beforeVersion {
			t.Errorf("%s: child version %d → %d, want unchanged — an edge change is not a versioned edit",
				s.name, beforeVersion, afterVersion)
		}
	}
}

// TestRowSeq_ReadPathsCarryIt — the list rows (project list, cross-project
// list, the /me lists), the tree rows and the card carry the committed
// order of the issue, and the wire name is `rowSeq`.
func TestRowSeq_ReadPathsCarryIt(t *testing.T) {
	f := newRowSeqFixture(t)
	ctx := callerCtx(t, f.owner, "user")
	// A label write: the order moves without version, so a read path that
	// derived the value from anything but the column would disagree.
	if _, err := f.store.AddLabelNoLoad(ctx, f.issueID, "rowseq", HumanActor(f.owner, f.name), LockOpts{}); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	want, _ := rowState(t, f.pool, f.issueID)
	if want <= 0 {
		t.Fatalf("row_seq = %d, want positive", want)
	}

	find := func(items []IssueSummary) (IssueSummary, bool) {
		for _, it := range items {
			if it.ID == f.issueID {
				return it, true
			}
		}
		return IssueSummary{}, false
	}
	lists := []struct {
		name  string
		fetch func() ([]IssueSummary, error)
	}{
		{"ListIssues", func() ([]IssueSummary, error) {
			r, err := f.store.ListIssues(ctx, ListIssuesArgs{ProjectID: f.project, Limit: 50})
			if err != nil {
				return nil, err
			}
			return r.Items, nil
		}},
		{"ListAllIssues", func() ([]IssueSummary, error) {
			r, err := f.store.ListAllIssues(ctx, ListAllIssuesArgs{
				Scope:  authz.ForUser(f.owner),
				Caller: f.owner, Limit: 50,
			})
			if err != nil {
				return nil, err
			}
			return r.Items, nil
		}},
		{"ListByUser", func() ([]IssueSummary, error) {
			r, err := f.store.ListByUser(ctx, ListByUserArgs{
				Scope:  authz.ForUser(f.owner),
				UserID: f.owner, Mode: ModeCreatedBy, Limit: 50,
			})
			if err != nil {
				return nil, err
			}
			return r.Items, nil
		}},
		{"IssueTree", func() ([]IssueSummary, error) {
			r, err := f.store.IssueTree(ctx, IssueTreeArgs{
				Scope:     authz.ForUser(f.owner),
				Caller:    f.owner,
				ProjectID: &f.project, Types: []string{"parent-child"},
			})
			if err != nil {
				return nil, err
			}
			out := make([]IssueSummary, 0, len(r.Items))
			for _, n := range r.Items {
				out = append(out, n.Item)
			}
			return out, nil
		}},
	}
	for _, l := range lists {
		items, err := l.fetch()
		if err != nil {
			t.Fatalf("%s: %v", l.name, err)
		}
		row, ok := find(items)
		if !ok {
			t.Fatalf("%s: the issue is not in the answer", l.name)
		}
		if row.RowSeq != want {
			t.Errorf("%s: rowSeq = %d, want %d (the committed row_seq)", l.name, row.RowSeq, want)
		}
		assertWireRowSeq(t, l.name, row, want)
	}

	card, err := f.store.GetIssue(ctx, f.issueID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if card.RowSeq != want {
		t.Errorf("GetIssue: rowSeq = %d, want %d (the committed row_seq)", card.RowSeq, want)
	}
	assertWireRowSeq(t, "GetIssue", card, want)
}

// assertWireRowSeq checks that the value reaches the JSON under `rowSeq`.
func assertWireRowSeq(t *testing.T, where string, v any, want int64) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", where, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: unmarshal: %v", where, err)
	}
	raw, ok := m["rowSeq"]
	if !ok {
		t.Errorf("%s: no `rowSeq` key on the wire", where)
		return
	}
	var got int64
	if err := json.Unmarshal(raw, &got); err != nil || got != want {
		t.Errorf("%s: wire rowSeq = %s, want %d", where, raw, want)
	}
}
