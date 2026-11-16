package dependencies

// sk-1007 US-4, matrix rows No. 43 and No. 44. Reference — spec US-4 (D3),
// plan K7 and the NF bound "visibility is read only on the refusal path",
// written before the code:
//
//   - the path of a cycle is given only when the reader sees every issue on
//     it now, with the key's scope; otherwise the refusal has no path;
//   - a context without a principal sees nothing (fail-closed); declared
//     internal work sees everything;
//   - a check that finds no cycle reads nothing but the existence walk.
//
// Fixture (the spec's example): P-2 → H-5 → H-9 → P-1, H a project the
// reader does not see; the new edge P-1 → P-2 would close
// P-1 → P-2 → H-5 → H-9 → P-1.

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

// cycleVisEnv is the fixture above for one type-class.
type cycleVisEnv struct {
	pool           *pgxpool.Pool
	reader         uuid.UUID
	p, h           uuid.UUID
	p1, p2, h5, h9 string
	check          func(context.Context, pgx.Tx, string, string) error
	wantPath       []string
}

func newCycleVisEnv(t *testing.T, depType string, check func(context.Context, pgx.Tx, string, string) error) *cycleVisEnv {
	t.Helper()
	pool := newTestPool(t)
	reader := seedUser(t, pool)
	hOwner := seedUser(t, pool)
	p, pPrefix := seedProject(t, pool, reader)
	h, hPrefix := seedProject(t, pool, hOwner)
	e := &cycleVisEnv{pool: pool, reader: reader, p: p, h: h,
		p1: pPrefix + "-1", p2: pPrefix + "-2", h5: hPrefix + "-5", h9: hPrefix + "-9", check: check}
	seedOpenIssue(t, pool, p, reader, e.p1, "p1")
	seedOpenIssue(t, pool, p, reader, e.p2, "p2")
	seedOpenIssue(t, pool, h, hOwner, e.h5, "h5")
	seedOpenIssue(t, pool, h, hOwner, e.h9, "h9")
	seedEdges(t, pool, depType, [][2]string{{e.p2, e.h5}, {e.h5, e.h9}, {e.h9, e.p1}}, hOwner)
	e.wantPath = []string{e.p1, e.p2, e.h5, e.h9, e.p1}
	return e
}

// run checks the edge from -> to in a SERIALIZABLE transaction under ctx.
func (e *cycleVisEnv) run(t *testing.T, ctx context.Context, from, to string) *ErrDepCycle {
	t.Helper()
	err := pgx.BeginTxFunc(context.Background(), e.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return e.check(ctx, tx, from, to)
	})
	ce, ok := IsCycle(err)
	if !ok {
		t.Fatalf("check(%s -> %s) = %v, want ErrDepCycle", from, to, err)
	}
	return ce
}

func (e *cycleVisEnv) userCtx() context.Context {
	return auth.WithUserRole(auth.WithUserID(context.Background(), e.reader), "user")
}

func (e *cycleVisEnv) keyCtx(sc apikeys.Scope) context.Context {
	ctx := auth.WithAPIKeyID(e.userCtx(), uuid.New())
	return auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelManage, Scope: sc})
}

// TestCycleVisibility_PathAllOrNothing — No. 43.
func TestCycleVisibility_PathAllOrNothing(t *testing.T) {
	for _, c := range cycleCheckers {
		t.Run(c.name, func(t *testing.T) {
			e := newCycleVisEnv(t, c.depType, c.check)

			if ce := e.run(t, e.userCtx(), e.p1, e.p2); len(ce.Path) != 0 {
				t.Errorf("reader who does not see H: path %v, want none", ce.Path)
			}
			if ce := e.run(t, context.Background(), e.p1, e.p2); len(ce.Path) != 0 {
				t.Errorf("context without a principal: path %v, want none (fail-closed)", ce.Path)
			}
			if ce := e.run(t, authz.WithUnrestricted(context.Background()), e.p1, e.p2); !slices.Equal(ce.Path, e.wantPath) {
				t.Errorf("declared internal work: path %v, want %v", ce.Path, e.wantPath)
			}
			if ce := e.run(t, e.userCtx(), e.p1, e.p1); !slices.Equal(ce.Path, []string{e.p1}) {
				t.Errorf("self-reference of a visible issue: path %v, want [%s]", ce.Path, e.p1)
			}

			testsupport.AddProjectMember(t, e.pool, e.h, e.reader, "member")

			if ce := e.run(t, e.userCtx(), e.p1, e.p2); !slices.Equal(ce.Path, e.wantPath) {
				t.Errorf("reader who sees P and H: path %v, want %v", ce.Path, e.wantPath)
			}
			keyP := e.keyCtx(apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{e.p}})
			if ce := e.run(t, keyP, e.p1, e.p2); len(ce.Path) != 0 {
				t.Errorf("key of scope projects=[P], H visible to its owner: path %v, want none", ce.Path)
			}
		})
	}
}

// cycleTracer records the statements pgx sends while on.
type cycleTracer struct {
	mu  sync.Mutex
	on  bool
	sql []string
}

func (tr *cycleTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.on {
		tr.sql = append(tr.sql, d.SQL)
	}
	return ctx
}

func (tr *cycleTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *cycleTracer) set(on bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.on = on
	if on {
		tr.sql = nil
	}
}

// TestCycleVisibility_NoReadWithoutACycle — No. 44: a check that finds no
// cycle, with a principal in the context, sends exactly one statement — the
// existence walk; visibility is not read.
func TestCycleVisibility_NoReadWithoutACycle(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tr := &cycleTracer{}
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)

	for _, c := range cycleCheckers {
		t.Run(c.name, func(t *testing.T) {
			e := newCycleVisEnv(t, c.depType, c.check)
			ctx := e.userCtx()
			var sent []string
			err := pgx.BeginTxFunc(context.Background(), traced, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
				tr.set(true)
				defer tr.set(false)
				err := c.check(ctx, tx, e.p2, e.p1) // P-1 reaches nothing: no cycle
				tr.mu.Lock()
				sent = append([]string(nil), tr.sql...)
				tr.mu.Unlock()
				return err
			})
			if err != nil {
				t.Fatalf("check(%s -> %s) = %v, want nil", e.p2, e.p1, err)
			}
			if len(sent) != 1 {
				t.Errorf("a check without a cycle sent %d statements, want 1 (the existence walk):\n%s",
					len(sent), strings.Join(sent, "\n---\n"))
			}
		})
	}
}
