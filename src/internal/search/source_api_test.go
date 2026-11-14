package search_test

// source_api_test.go — test matrix row 19 ("Data: the people group", the
// exact filter, search). Reference — the spec, written before the code: the
// people (human-driven) group by issues.source and by comments.author_kind
// includes api; the exact filter api returns only api records. Same FTS-only
// path and per-test token as comment_author_kind_test.go.

import (
	"context"
	"testing"

	"skein-api/internal/issues"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

func TestSearch_SourceAPI_PeopleGroupAndExactFilter(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsapi")
	tok := token(t)

	seedIssueSource := func(n, source string) string {
		id := prefix + "-" + n
		testsupport.SeedIssue(t, pool, projID, owner, id, "issue "+tok+" "+source, "open", 2)
		if _, err := pool.Exec(context.Background(), `UPDATE issues SET source = $2 WHERE id = $1`, id, source); err != nil {
			t.Fatalf("set source=%s on %s: %v", source, id, err)
		}
		return id
	}
	humanIssue := seedIssueSource("1", issues.SourceHuman)
	apiIssue := seedIssueSource("2", issues.SourceAPI)
	autoIssue := seedIssueSource("3", issues.SourceAgentAutonomous)

	host := prefix + "-9"
	testsupport.SeedIssue(t, pool, projID, owner, host, "comment host", "open", 2)
	humanComment := seedCommentKind(t, pool, host, owner, "human note "+tok, issues.CommentAuthorKindHuman)
	apiComment := seedCommentKind(t, pool, host, owner, "api note "+tok, issues.CommentAuthorKindAPI)
	autoComment := seedCommentKind(t, pool, host, owner, "agent note "+tok, issues.CommentAuthorKindAgentAutonomous)

	run := func(kind search.Kind, f search.Filters) map[string]bool {
		t.Helper()
		res, err := svc.Search(ctx(), search.Params{
			Query: tok, Project: &projID, Kinds: []search.Kind{kind}, Limit: 50, CallerID: owner, Filters: f,
		})
		if err != nil {
			t.Fatalf("Search %s %+v: %v", kind, f, err)
		}
		return idSet(res.Items)
	}
	people, noFilter := issues.OriginGroupsToSources([]string{issues.OriginHumanDriven})
	if noFilter {
		t.Fatal("human-driven expands to no filter")
	}

	t.Run("issues: the people group includes api", func(t *testing.T) {
		got := run(search.KindIssue, search.Filters{OriginSources: people})
		if len(got) != 2 || !got[humanIssue] || !got[apiIssue] || got[autoIssue] {
			t.Errorf("people issues %v, want human(%s) and api(%s) only", got, humanIssue, apiIssue)
		}
	})
	t.Run("issues: the exact filter api returns only api", func(t *testing.T) {
		src := issues.SourceAPI
		got := run(search.KindIssue, search.Filters{Source: &src})
		if len(got) != 1 || !got[apiIssue] {
			t.Errorf("source=api issues %v, want only %s", got, apiIssue)
		}
	})
	t.Run("comments: the people group includes api", func(t *testing.T) {
		got := run(search.KindComment, search.Filters{CommentAuthorKinds: issues.HumanDrivenCommentAuthorKinds})
		if len(got) != 2 || !got[humanComment] || !got[apiComment] || got[autoComment] {
			t.Errorf("people comments %v, want human(%s) and api(%s) only", got, humanComment, apiComment)
		}
	})
	t.Run("comments: the exact filter api returns only api", func(t *testing.T) {
		got := run(search.KindComment, search.Filters{CommentAuthorKinds: []string{issues.CommentAuthorKindAPI}})
		if len(got) != 1 || !got[apiComment] {
			t.Errorf("authorKind=api comments %v, want only %s", got, apiComment)
		}
	})
}
