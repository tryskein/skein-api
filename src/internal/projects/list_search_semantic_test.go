package projects

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"skein-api/internal/testsupport"
)

// Regression: the semantic-rerank branch of listProjectsWithSearch used to
// hand semanticRerank the FULL whereParams slice while stripping the FTS
// match clause out of the SQL. The match-clause params then travelled to
// Postgres unreferenced, type inference had nothing to work from, and every
// call died with 42P18 "could not determine data type of parameter $1".
//
// Because the rerank is best-effort (the error is logged and swallowed),
// ListProjects still returned 200 with FTS-only results — so the failure was
// invisible to any assertion on err. It went unnoticed in production for the
// whole life of the feature: semantic project search never once ran.
//
// That is exactly why this test asserts on RESULT CONTENT, not on err. It
// seeds a project whose embedding matches the query vector but whose text
// cannot possibly satisfy the FTS/trigram match clause, then requires the
// project to come back. Only the semantic layer can produce that row:
//
//	pre-fix  → rerank raises 42P18, degrades to FTS-only, project missing
//	post-fix → rerank runs, cosine similarity 1.0 clears the 0.6 floor, hit
//
// No Ollama needed. The bug is a SQL-parse failure, so a deterministic fake
// embedder and a hand-written embedding row reproduce it exactly.
//
// Note the admin caller: for an admin the project_members clause is skipped,
// which is what puts the query param at $1 and makes this the shape seen in
// the production logs. The non-admin path is broken identically, just with
// the error naming $2 — covered by the second subtest below.

// fakeEmbedder is a deterministic QueryEmbedder: it always returns the same
// unit vector, so a row stored with that same vector scores cosine
// similarity 1.0 and clears searchSemanticThreshold.
type fakeEmbedder struct{}

func (fakeEmbedder) Available() bool { return true }

func (fakeEmbedder) EmbedSingle(_ context.Context, _ string) ([]float32, error) {
	return fixedUnitVector(), nil
}

// fixedUnitVector builds the 1024-dim halfvec the projects.embedding column
// expects. A single non-zero component keeps it a unit vector, which makes
// the cosine distance against an identical stored row exactly 0.
func fixedUnitVector() []float32 {
	v := make([]float32, 1024)
	v[0] = 1
	return v
}

// literalUnitVector renders fixedUnitVector in pgvector's text input format
// so it can be written straight into the halfvec column from SQL.
func literalUnitVector() string {
	parts := make([]string, 1024)
	for i := range parts {
		parts[i] = "0"
	}
	parts[0] = "1"
	return "[" + strings.Join(parts, ",") + "]"
}

func TestListProjectsSearch_SemanticRerankRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
	}{
		// Admin: no project_members clause → the query param lands at $1,
		// reproducing the exact production error text.
		{name: "admin", role: "admin"},
		// Member: the project_members clause survives the strip and keeps
		// $1 referenced, so the first orphan param is $2. Same bug.
		{name: "member", role: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			ctx := context.Background()

			caller, _ := testsupport.SeedUser(t, pool, tc.role)
			pid, _ := testsupport.SeedProject(t, pool, caller, "semrerank")

			// Give the project the exact vector the fake embedder returns.
			if _, err := pool.Exec(ctx,
				`UPDATE projects SET embedding = $1::halfvec WHERE id = $2`,
				literalUnitVector(), pid,
			); err != nil {
				t.Fatalf("seeding project embedding: %v", err)
			}

			// A query that shares no token, substring or trigram with the
			// project's name/description/id_prefix. FTS and pg_trgm both
			// miss it, so the seeded project can only arrive via semantic.
			const unmatchableQuery = "zzqxwvunmatchable"

			res, err := NewStore(pool).ListProjects(ctx, ListProjectsArgs{
				CallerID: caller,
				Query:    unmatchableQuery,
				Embedder: fakeEmbedder{},
			})
			if err != nil {
				t.Fatalf("ListProjects: %v", err)
			}

			for _, it := range res.Items {
				if it.ID == pid {
					return // semantic layer delivered the row
				}
			}
			t.Fatalf("project %s absent from results for query %q — the "+
				"semantic rerank did not contribute it. Pre-fix this is "+
				"42P18 (orphan match-clause params sent to a SQL that no "+
				"longer references them), swallowed by the best-effort "+
				"degrade path. Got %d items: %s",
				pid, unmatchableQuery, len(res.Items), itemIDs(res))
		})
	}
}

func itemIDs(res *ListResult) string {
	ids := make([]string, 0, len(res.Items))
	for _, it := range res.Items {
		ids = append(ids, it.ID.String())
	}
	if len(ids) == 0 {
		return "(none)"
	}
	return fmt.Sprint(ids)
}
