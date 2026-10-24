package projects

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

// TestListProjects_ComponentsCount — sk-551.53 acceptance: every project
// list row carries componentsCount, the number of the project's
// NON-archived components. A project with two active components and one
// archived one reports 2; a project without components reports 0.
//
// The row is produced by three separate SELECTs (plain listing, FTS search,
// semantic rerank), each with its own scan list, so each path is driven:
// a path that forgot the column would report 0 (or fail the scan) for the
// project with components, and one that dropped the archived_at filter
// would report 3.
func TestListProjects_ComponentsCount(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := storeFixtureCtx()
	owner, _ := testsupport.SeedUser(t, pool, "user")
	store := NewStore(pool)
	actor := HumanActor(owner, "owner")

	withComps, withCompsPrefix := testsupport.SeedProject(t, pool, owner, "lcomp")
	noComps, noCompsPrefix := testsupport.SeedProject(t, pool, owner, "lcomp")

	for _, name := range []string{"alpha", "beta", "gamma"} {
		c, err := store.CreateComponent(ctx, withComps, owner, CreateComponentArgs{Name: name}, actor)
		if err != nil {
			t.Fatalf("create component %s: %v", name, err)
		}
		if name == "gamma" {
			if err := store.ArchiveComponent(ctx, withComps, c.ID, actor); err != nil {
				t.Fatalf("archive component %s: %v", name, err)
			}
		}
	}

	// The semantic path needs the stored embedding to match the vector the
	// fake embedder returns (see list_search_semantic_test.go).
	for _, pid := range []uuid.UUID{withComps, noComps} {
		if _, err := pool.Exec(context.Background(),
			`UPDATE projects SET embedding = $1::halfvec WHERE id = $2`,
			literalUnitVector(), pid); err != nil {
			t.Fatalf("seed embedding: %v", err)
		}
	}

	paths := []struct {
		name string
		args func(prefix string) ListProjectsArgs
	}{
		{name: "plain", args: func(string) ListProjectsArgs {
			return ListProjectsArgs{CallerID: owner}
		}},
		{name: "fts", args: func(prefix string) ListProjectsArgs {
			return ListProjectsArgs{CallerID: owner, Query: prefix}
		}},
		// A query no FTS / trigram leg can match: the row can only arrive
		// through the semantic rerank SELECT.
		{name: "semantic", args: func(string) ListProjectsArgs {
			return ListProjectsArgs{CallerID: owner,
				Query: "zzqxwvunmatchable", Embedder: fakeEmbedder{}}
		}},
	}
	projectsUnderTest := []struct {
		name   string
		pid    uuid.UUID
		prefix string
		want   int
	}{
		{name: "two active one archived", pid: withComps, prefix: withCompsPrefix, want: 2},
		{name: "no components", pid: noComps, prefix: noCompsPrefix, want: 0},
	}

	for _, p := range paths {
		for _, pr := range projectsUnderTest {
			t.Run(p.name+"/"+pr.name, func(t *testing.T) {
				res, err := store.ListProjects(ctx, p.args(pr.prefix))
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				got := findProjectRow(t, res.Items, pr.pid)
				if got.ComponentsCount != pr.want {
					t.Errorf("componentsCount = %d, want %d", got.ComponentsCount, pr.want)
				}
			})
		}
	}
}
