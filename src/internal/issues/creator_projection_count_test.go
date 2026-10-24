package issues

// creator_projection_count_test.go — sk-565, T028, test matrix rows 13–15
// ("Perf", INV-4, INV-5, P3, P13). Reference: spec sk-565, "Constraints":
// "no more than one read of the settings row by primary key per create
// call (an epic of up to 50 children inclusive), inside the create transaction"; INV-4:
// "REST create does not read the setting".
//
// What is observed is the traffic the database actually received — every
// statement pgx sends, batched ones included — not the source text. A
// statement counts as «a primary-key read of the creator's settings row»
// when it touches user_notification_prefs and its only argument is the
// creator: that is both the projection read and the historical
// `SELECT review_required_default … WHERE user_id = $1`, and it is NOT the
// fan-out JOINs of the same table (notifications/fanout.go), which carry
// the event type, the project and more.
//
// Beyond the progressTracer sample (progress_statement_count_test.go), which
// compares two corpus sizes: an absolute count and a statement filter.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tracedStatement is one statement the database received.
type tracedStatement struct {
	sql  string
	args []any
}

// statementTracer records every statement sent through the pool, single
// and batched, while switched on.
type statementTracer struct {
	mu         sync.Mutex
	on         bool
	statements []tracedStatement
	// hook, when set, runs synchronously on the issuing connection before
	// each single statement executes (default_assignee_test.go drives
	// out-of-band commits through it).
	hook func(sql string, args []any)
}

var (
	_ pgx.QueryTracer = (*statementTracer)(nil)
	_ pgx.BatchTracer = (*statementTracer)(nil)
)

func (tr *statementTracer) add(sql string, args []any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.on {
		tr.statements = append(tr.statements, tracedStatement{sql: sql, args: append([]any(nil), args...)})
	}
}

func (tr *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.add(data.SQL, data.Args)
	tr.mu.Lock()
	hook := tr.hook
	tr.mu.Unlock()
	if hook != nil {
		hook(data.SQL, data.Args)
	}
	return ctx
}

func (tr *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *statementTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (tr *statementTracer) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	tr.add(data.SQL, data.Args)
}

func (tr *statementTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// record runs fn with the tracer on and returns what it saw.
func (tr *statementTracer) record(fn func()) []tracedStatement {
	tr.mu.Lock()
	tr.statements = nil
	tr.on = true
	tr.mu.Unlock()

	fn()

	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.on = false
	return append([]tracedStatement(nil), tr.statements...)
}

// newStatementTracedPool is newTestPool with a statementTracer attached.
func newStatementTracedPool(t *testing.T) (*pgxpool.Pool, *statementTracer) {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tr := &statementTracer{}
	cfg.ConnConfig.Tracer = tr
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, tr
}

// argIsUser reports whether a statement argument is the given user id, in
// any of the forms pgx accepts for a uuid parameter.
func argIsUser(arg any, user uuid.UUID) bool {
	switch v := arg.(type) {
	case uuid.UUID:
		return v == user
	case *uuid.UUID:
		return v != nil && *v == user
	case string:
		return strings.EqualFold(v, user.String())
	case [16]byte:
		return uuid.UUID(v) == user
	}
	return false
}

// creatorPrefsReads returns the statements that read the creator's
// settings row by its primary key.
func creatorPrefsReads(stmts []tracedStatement, creator uuid.UUID) []string {
	var out []string
	for _, s := range stmts {
		if strings.Contains(s.sql, "user_notification_prefs") &&
			len(s.args) == 1 && argIsUser(s.args[0], creator) {
			out = append(out, s.sql)
		}
	}
	return out
}

// statementsNaming returns the statements whose text names the column.
func statementsNaming(stmts []tracedStatement, column string) []string {
	var out []string
	for _, s := range stmts {
		if strings.Contains(s.sql, column) {
			out = append(out, s.sql)
		}
	}
	return out
}

// TestCreatorProjection_EpicReadsTheSettingsRowOncePerCall — row 13.
func TestCreatorProjection_EpicReadsTheSettingsRowOncePerCall(t *testing.T) {
	pool, tr := newStatementTracedPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	children := make([]CreateIssueInput, 50)
	for i := range children {
		children[i] = CreateIssueInput{Title: "child", Type: "task", Priority: 2, AssignCreatorByDefault: true}
	}
	var (
		epic    *IssueDetail
		created []*IssueDetail
		err     error
	)
	stmts := tr.record(func() {
		epic, created, err = store.CreateEpicWithChildren(t.Context(), pid, prefix, "counter",
			CreateIssueInput{Title: "epic", Type: TypeEpic, Priority: 2, AssignCreatorByDefault: true},
			children, SourceAgentDirected, creator, agentActor(creator, name))
	})
	if err != nil {
		t.Fatalf("CreateEpicWithChildren: %v", err)
	}
	if epic == nil {
		t.Fatalf("CreateEpicWithChildren returned no epic")
	}
	if reads := creatorPrefsReads(stmts, creator); len(reads) != 1 {
		t.Errorf("epic with 50 children: %d primary-key reads of the creator's settings row, want exactly 1:\n%s",
			len(reads), strings.Join(reads, "\n---\n"))
	}
	// All 51 issues decided by that one read: on the creator.
	all := append([]*IssueDetail{epic}, created...)
	if len(all) != 51 {
		t.Fatalf("created %d issues, want 51", len(all))
	}
	for _, d := range all {
		if d.Assignee == nil || *d.Assignee != name {
			t.Errorf("%s: assignee = %v, want the creator %q", d.ID, d.Assignee, name)
		}
	}
}

// TestCreatorProjection_SingleCreateReadsTheSettingsRowOnce — row 14.
func TestCreatorProjection_SingleCreateReadsTheSettingsRowOnce(t *testing.T) {
	pool, tr := newStatementTracedPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	var (
		dto *IssueDetail
		err error
	)
	stmts := tr.record(func() {
		dto, err = store.CreateIssue(t.Context(), pid, prefix, "counter",
			CreateIssueInput{Title: "mcp create", Type: "task", Priority: 2, AssignCreatorByDefault: true},
			creator, agentActor(creator, name))
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.Assignee == nil || *dto.Assignee != name {
		t.Errorf("assignee = %v, want the creator %q", dto.Assignee, name)
	}
	if reads := creatorPrefsReads(stmts, creator); len(reads) != 1 {
		t.Errorf("single create with P1: %d primary-key reads of the creator's settings row, want exactly 1:\n%s",
			len(reads), strings.Join(reads, "\n---\n"))
	}
}

// TestCreatorProjection_RESTEquivalentNeverNamesTheNewFlag — row 15.
func TestCreatorProjection_RESTEquivalentNeverNamesTheNewFlag(t *testing.T) {
	pool, tr := newStatementTracedPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	t.Run("no explicit review flag", func(t *testing.T) {
		var err error
		stmts := tr.record(func() {
			_, err = store.CreateIssue(t.Context(), pid, prefix, "counter",
				CreateIssueInput{Title: "rest create", Type: "task", Priority: 2},
				creator, HumanActor(creator, name))
		})
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if named := statementsNaming(stmts, "assign_agent_issues_to_me"); len(named) != 0 {
			t.Errorf("REST-equivalent create executed %d statements naming assign_agent_issues_to_me:\n%s",
				len(named), strings.Join(named, "\n---\n"))
		}
		reads := creatorPrefsReads(stmts, creator)
		if len(reads) != 1 {
			t.Fatalf("REST-equivalent create: %d primary-key reads of the creator's settings row, want 1 (review_required_default):\n%s",
				len(reads), strings.Join(reads, "\n---\n"))
		}
		if !strings.Contains(reads[0], "review_required_default") {
			t.Errorf("the one settings read does not select review_required_default:\n%s", reads[0])
		}
	})

	t.Run("explicit review flag", func(t *testing.T) {
		explicit := false
		var err error
		stmts := tr.record(func() {
			_, err = store.CreateIssue(t.Context(), pid, prefix, "counter",
				CreateIssueInput{Title: "rest create explicit", Type: "task", Priority: 2, ReviewRequired: &explicit},
				creator, HumanActor(creator, name))
		})
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if reads := creatorPrefsReads(stmts, creator); len(reads) != 0 {
			t.Errorf("explicit ReviewRequired and no P1: %d reads of the creator's settings row, want 0:\n%s",
				len(reads), strings.Join(reads, "\n---\n"))
		}
	})
}
