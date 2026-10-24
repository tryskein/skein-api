package authz_test

// plan_test.go — sk-674.18, matrix row 14 (D-1, K-1): the access relation
// is computed once per request, never per row.
//
// Reference — D-1 and K-1, written before this file: the relation is a set
// computed once and joined by index; the per-request visible set is read
// once and never kept across requests (INV-26). Two observable halves:
//
//   - the JOIN form (authz.VisibleJoin) — the package's own fragment, in a
//     query over issues on the T003 corpus — is planned with the relation
//     inlined: the EXPLAIN ANALYZE plan (testsupport.ExplainAnalyze) has no
//     Function Scan of skein_project_access_set, so nothing calls it per
//     row. Red when the SQL function stops being inlined;
//   - the array form (Scope.Bind) reads the relation once per request: two
//     Binds under one request context (WithRequestCache) read it once, the
//     next request reads it again. Red when the memo is lost (a read per
//     fragment) or kept across requests.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
	"skein-api/internal/testsupport"
)

// TestPlan_VisibleJoinInlinesTheRelation — the first half of the header.
func TestPlan_VisibleJoinInlinesTheRelation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)

	sample := `
SELECT i.id, va.access
  FROM issues i
  ` + authz.VisibleJoin("i", "$1", "$2", "va") + `
 WHERE i.status <> 'closed'`
	for _, a := range []testsupport.Actor{testsupport.ActorOnlyA, testsupport.ActorExternal, testsupport.ActorAdminA} {
		plan := testsupport.ExplainAnalyze(t, pool, sample, false, f.User(t, a).ID)
		if !strings.Contains(plan, "issues") {
			t.Fatalf("%s: the plan does not read issues — not the sample's plan:\n%s", a, plan)
		}
		for _, line := range strings.Split(plan, "\n") {
			if strings.Contains(line, "Function Scan") && strings.Contains(line, "skein_project_access_set") {
				t.Errorf("%s: the relation is a Function Scan — called, not inlined:\n%s", a, plan)
				break
			}
		}
	}
}

// relationReads counts the statements that read the access relation.
type relationReads struct {
	q     authz.RowsQuerier
	reads int
}

func (r *relationReads) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "skein_project_access_set(") {
		r.reads++
	}
	return r.q.Query(ctx, sql, args...)
}

// TestPlan_VisibleSetReadOncePerRequest — the second half of the header.
func TestPlan_VisibleSetReadOncePerRequest(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	q := &relationReads{q: pool}
	want := len(testsupport.TenancyVisible[testsupport.ActorOnlyA])

	request := func() context.Context {
		return authz.WithRequestCache(callerCtx(f, t, testsupport.ActorOnlyA))
	}
	ctx := request()
	for i := 0; i < 2; i++ {
		_, visible, err := authz.FromContext(ctx).Bind(ctx, q)
		if err != nil {
			t.Fatalf("Bind %d: %v", i+1, err)
		}
		if len(visible) != want {
			t.Fatalf("Bind %d: %d visible projects, want %d", i+1, len(visible), want)
		}
	}
	if q.reads != 1 {
		t.Errorf("one request read the relation %d times, want 1", q.reads)
	}

	next := request()
	if _, _, err := authz.FromContext(next).Bind(next, q); err != nil {
		t.Fatalf("Bind in the next request: %v", err)
	}
	if q.reads != 2 {
		t.Errorf("after the next request the relation was read %d times in all, want 2 (no reuse across requests)", q.reads)
	}
}
