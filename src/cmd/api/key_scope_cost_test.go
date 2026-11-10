package main

// key_scope_cost_test.go — the cost of a key's scope on the hot MCP path
// (sk-878.3.1 T042, NFR "latency": /mcp gets no slower after the change).
// Reference — the requirement, written before this file: narrowing a key to
// projects (T010, D-3) is a predicate inside the statements the request runs
// anyway, never extra statements; a request with a key of scope "projects"
// costs the database no more round-trips than the same request with a key of
// scope "all". Measured on the 10k stand (EXPLAIN ANALYZE, 2026-11-10):
// skein_show 17, skein_list 9, skein_update 20 statements under either key.
//
// Red when the projects scope adds a statement to show, list or update — a
// per-call lookup of the scope, a second read of the visible set, a check
// of the key's projects outside the statements of the tool.
//
// Only the statements of the caller's own request are counted (the request
// context names him), so a background worker of the router does not move
// the count.

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// callerStatements counts the statements run on behalf of one user.
type callerStatements struct {
	mu     sync.Mutex
	caller uuid.UUID
	n      int
}

func (c *callerStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if u, ok := auth.UserIDFrom(ctx); ok {
		c.mu.Lock()
		if c.caller != uuid.Nil && u == c.caller {
			c.n++
		}
		c.mu.Unlock()
	}
	return ctx
}

func (c *callerStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// during counts the statements of caller while fn runs.
func (c *callerStatements) during(caller uuid.UUID, fn func()) int {
	c.mu.Lock()
	c.caller, c.n = caller, 0
	c.mu.Unlock()
	fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.n
	c.caller = uuid.Nil
	return n
}

func TestKeyScope_ProjectsCostsNoMoreStatementsThanAll(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	counter := &callerStatements{}
	cfg.ConnConfig.Tracer = counter
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	s := newParityStackOn(t, pool)
	P := s.f.ASpace
	all := mintScopedKey(t, s, testsupport.ActorBoth, apikeys.Scope{Mode: apikeys.ScopeAll})
	projects := mintScopedKey(t, s, testsupport.ActorBoth, apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{P.ID}})
	caller := s.f.User(t, testsupport.ActorBoth).ID

	calls := []struct {
		name string
		tool string
		args func() map[string]any
	}{
		{"show", "skein_show", func() map[string]any { return map[string]any{"id": P.IssueID} }},
		{"list", "skein_list", func() map[string]any { return map[string]any{"project": P.IDPrefix} }},
		{"update", "skein_update", func() map[string]any {
			return map[string]any{"id": P.IssueID, "expectedVersion": issueVersion(t, s, P.IssueID), "notes": uuid.NewString()}
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			cost := func(k *scopedKey) int {
				// The first call warms the statement cache; the second is counted.
				if res := k.tool(t, c.tool, c.args()); res.isError {
					t.Fatalf("%s: %s", c.tool, res.text)
				}
				args := c.args()
				return counter.during(caller, func() {
					if res := k.tool(t, c.tool, args); res.isError {
						t.Fatalf("%s: %s", c.tool, res.text)
					}
				})
			}
			withAll, withProjects := cost(all), cost(projects)
			if withAll == 0 {
				t.Fatalf("%s with scope all ran no statement of the caller — the count sees nothing", c.tool)
			}
			if withProjects > withAll {
				t.Errorf("%s: scope projects runs %d statements, scope all %d — the scope must cost no round-trip of its own",
					c.tool, withProjects, withAll)
			}
		})
	}
}
