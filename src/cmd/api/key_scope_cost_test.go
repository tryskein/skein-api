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
// Every statement of the counted request is counted — the authentication of
// the key (its lookup, grant and scope), the MCP middleware and the tool —
// and nothing else: the request carries a marker in its context, which
// every statement it makes inherits, so a background worker of the router
// does not move the count (review R8-08).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// countedRequest marks the context of the request whose statements count.
type countedRequest struct{}

// requestStatements counts the statements of the marked request: queries,
// and the queries of a batch.
type requestStatements struct {
	mu sync.Mutex
	n  int
}

func (c *requestStatements) count(ctx context.Context, k int) {
	if on, _ := ctx.Value(countedRequest{}).(bool); on {
		c.mu.Lock()
		c.n += k
		c.mu.Unlock()
	}
}

func (c *requestStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count(ctx, 1)
	return ctx
}

func (c *requestStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *requestStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceBatchStartData) context.Context {
	if d.Batch != nil {
		c.count(ctx, d.Batch.Len())
	}
	return ctx
}

func (c *requestStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (c *requestStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// call is the statements of one marked tools/call of k.
func (c *requestStatements) call(t *testing.T, k *scopedKey, name string, args map[string]any) int {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), countedRequest{}, true))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderAPIKey, k.plain)
	req.Header.Set(auth.HeaderSurface, auth.SurfaceInternal)
	req.Header.Set("Mcp-Session-Id", k.sid)
	c.mu.Lock()
	c.n = 0
	c.mu.Unlock()
	rec := httptest.NewRecorder()
	k.s.router.ServeHTTP(rec, req)
	var out struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Error != nil || out.Result.IsError {
		t.Fatalf("tools/call %s: %d %s", name, rec.Code, rec.Body.String())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
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
	counter := &requestStatements{}
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
				return counter.call(t, k, c.tool, c.args())
			}
			withAll, withProjects := cost(all), cost(projects)
			if withAll == 0 {
				t.Fatalf("%s with scope all ran no statement — the count sees nothing", c.tool)
			}
			if withProjects > withAll {
				t.Errorf("%s: scope projects runs %d statements, scope all %d — the scope must cost no round-trip of its own",
					c.tool, withProjects, withAll)
			}
		})
	}
}
