package testsupport

// plan_shadow.go — a twin schema of public that holds ONLY the rows a plan
// test seeded (sk-718, the row 86 gate of internal/nudges; the construction
// of handler/review_unreviewed_index_plan_test.go#newFixtureShadow, lifted
// here so a second package can ask its plan against its own rows).
//
// # Why a twin and not the shared tables
//
// A plan is a function of the statistics of every table the query reads.
// In the shared test database those tables also hold whatever the packages
// that ran before left there, so a gate asked against public judges their
// rows, not the fixture — green alone, red after a full run (§4a: a test is
// self-sufficient). Deleting foreign rows is not an option: the test would
// then depend on nobody writing them. The twin touches nothing foreign; it
// simply does not see it.
//
// # What the twin is
//
// Every public table, by LIKE (defaults, generated columns and CHECK
// constraints — the planner reads constraints), and every public index by
// its own definition from pg_indexes, names preserved, so the planner keeps
// its whole choice, foreign indexes included. Foreign keys are not carried:
// the twin is a controlled environment. Rows: exactly what the caller names
// (ShadowRows), copied in the given order; every other table stays empty.
// Statistics: ANALYZE of every table of the twin.
//
// The plan is asked over Conn — its own connection whose search_path puts
// the twin first and keeps public only as the tail for extension types and
// operators. Its own, because pgx caches prepared statements on the
// connection with the relation bound at prepare time: the same text
// prepared earlier against public would come back from a pool connection's
// cache and silently read public. A caller asserts the twin was read by
// EXPLAIN (VERBOSE): its scan nodes carry "Schema".

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlanShadow is a twin schema and the connection that reads it.
type PlanShadow struct {
	// Schema is the twin's name.
	Schema string
	// Conn reads the twin first (search_path), public only as the tail.
	Conn *pgx.Conn
}

// ShadowRows names the rows of one table the twin receives: the rows of
// public.<Table> matching Where. Where may name the twin as {shadow} (to
// copy the rows a previously copied table refers to); an empty Where copies
// nothing.
type ShadowRows struct {
	Table string
	Where string
	Args  []any
}

// NewPlanShadow builds the twin, copies rows in order, ANALYZEs it and
// opens its connection. The twin is dropped at cleanup, and its
// disappearance is checked: a leaked schema would go to the next packages.
func NewPlanShadow(t *testing.T, pool *pgxpool.Pool, rows []ShadowRows) *PlanShadow {
	t.Helper()
	ctx := t.Context()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}

	schema := "plan_shadow_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("plan shadow: creating schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("plan shadow: schema %s was not dropped: %v — it stays in the shared database", schema, err)
			return
		}
		var left bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&left); err != nil {
			t.Errorf("plan shadow: drop of %s not confirmed: %v", schema, err)
			return
		}
		if left {
			t.Errorf("plan shadow: schema %s survived its own DROP", schema)
		}
	})

	tables := shadowStrings(t, pool, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if len(tables) == 0 {
		t.Fatal("plan shadow: public has no table")
	}
	for _, tb := range tables {
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE %s.%q (LIKE public.%q INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS)`,
			schema, tb, tb)); err != nil {
			t.Fatalf("plan shadow: table %s: %v", tb, err)
		}
	}

	idx, err := pool.Query(ctx, `SELECT tablename, indexdef FROM pg_indexes WHERE schemaname = 'public'`)
	if err != nil {
		t.Fatalf("plan shadow: index list: %v", err)
	}
	type indexDef struct{ table, def string }
	var defs []indexDef
	for idx.Next() {
		var d indexDef
		if err := idx.Scan(&d.table, &d.def); err != nil {
			t.Fatalf("plan shadow: reading index: %v", err)
		}
		defs = append(defs, d)
	}
	idx.Close()
	if err := idx.Err(); err != nil {
		t.Fatalf("plan shadow: index list: %v", err)
	}
	for _, d := range defs {
		on := " ON public." + d.table + " "
		if !strings.Contains(d.def, on) {
			t.Fatalf("plan shadow: index definition cannot be retargeted: %s", d.def)
		}
		if _, err := pool.Exec(ctx, strings.Replace(d.def, on, " ON "+schema+"."+d.table+" ", 1)); err != nil {
			t.Fatalf("plan shadow: index %s: %v", d.def, err)
		}
	}

	for _, r := range rows {
		if r.Where == "" {
			continue
		}
		cols := shadowStrings(t, pool, `
SELECT quote_ident(column_name) FROM information_schema.columns
 WHERE table_schema = 'public' AND table_name = $1 AND is_generated = 'NEVER'
 ORDER BY ordinal_position`, r.Table)
		if len(cols) == 0 {
			t.Fatalf("plan shadow: table %s has no writable column (does it exist?)", r.Table)
		}
		list := strings.Join(cols, ", ")
		where := strings.ReplaceAll(r.Where, "{shadow}", schema)
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.%q (%s) SELECT %s FROM public.%q %s`,
			schema, r.Table, list, list, r.Table, where), r.Args...); err != nil {
			t.Fatalf("plan shadow: copying %s: %v", r.Table, err)
		}
	}

	for _, tb := range tables {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`ANALYZE %s.%q`, schema, tb)); err != nil {
			t.Fatalf("plan shadow: ANALYZE %s: %v", tb, err)
		}
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("plan shadow: connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `SET search_path TO `+schema+`, public`); err != nil {
		t.Fatalf("plan shadow: search_path: %v", err)
	}
	return &PlanShadow{Schema: schema, Conn: conn}
}

// shadowStrings is a one-column list of strings.
func shadowStrings(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatalf("plan shadow: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("plan shadow: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan shadow: %v", err)
	}
	return out
}
