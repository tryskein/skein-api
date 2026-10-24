package search_test

// cache_visibility_test.go — INV-26: the search result
// cache is partitioned by the caller's effective visibility. A cached pool
// filled while the caller saw more must not answer a cursorless repeat of
// the same search after the caller's access narrowed, and a pool filled
// while it saw less must not hide what it sees again.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// requestCtx is the context one API request gives the service: the caller
// as principal and the per-request memo of its visible set.
func requestCtx(user uuid.UUID) context.Context {
	return authz.WithRequestCache(auth.WithUserID(context.Background(), user))
}

func TestSearch_CacheFollowsVisibilityChange(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	svc, err := search.NewService(search.NewStore(pool), nil, time.Minute)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	caller, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	ownID, ownPrefix := testsupport.SeedProject(t, pool, caller, "tscvo")
	foreignID, foreignPrefix := testsupport.SeedProject(t, pool, other, "tscvf")
	tok := token(t)
	own := testsupport.SeedIssue(t, pool, ownID, caller, ownPrefix+"-1", "cached "+tok, "open", 2)
	foreign := testsupport.SeedIssue(t, pool, foreignID, other, foreignPrefix+"-1", "cached "+tok, "open", 2)

	setRole := func(role string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), `UPDATE users SET role = $2 WHERE id = $1`, caller, role); err != nil {
			t.Fatalf("set role %s: %v", role, err)
		}
	}
	// The same search each time: no cursor, no project filter.
	ids := func(stage string) []string {
		t.Helper()
		res, err := svc.Search(requestCtx(caller), search.Params{
			Query: tok, Kinds: []search.Kind{search.KindIssue}, Limit: 50, CallerID: caller,
		})
		if err != nil {
			t.Fatalf("%s: Search: %v", stage, err)
		}
		var out []string
		for _, it := range res.Items {
			out = append(out, it.ID)
		}
		slices.Sort(out)
		return out
	}
	both := []string{own, foreign}
	slices.Sort(both)

	setRole("admin")
	if got := ids("admin"); !slices.Equal(got, both) {
		t.Fatalf("admin: %v, want %v — the case would prove nothing", got, both)
	}

	setRole("user")
	if got := ids("demoted"); !slices.Equal(got, []string{own}) {
		t.Errorf("after demotion: %v, want only the own project's %s — a pool cached while admin answered", got, own)
	}

	setRole("admin")
	if got := ids("promoted again"); !slices.Equal(got, both) {
		t.Errorf("after promotion: %v, want %v — a pool cached while restricted answered", got, both)
	}
}
