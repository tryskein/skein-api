package search_test

// Integration coverage for the comment-author-kind filter (sk-142.10 / T10).
//
// Drives the FTS-only branch (nil embedder) exactly like
// service_integration_test.go: every request degrades to FTS-only, so the
// store's `in.Vec == nil` comment CTE runs and the author_kind predicate is
// asserted on that path. The vector hybrid CTE shares the same whereCommon
// fragment, so the filter is wired identically there; it is not exercised
// here because no Ollama embedder is available in the test image (the suite
// notes degraded=true is expected).
//
// All asserts are scoped to a per-test-unique token embedded in the comment
// bodies AND the seed project, so concurrent packages can never perturb a
// count.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// seedCommentKind seeds a comment whose body carries tok and then stamps
// the author_kind column (testsupport.SeedComment always writes 'human').
// Returns the comment id (as text) for membership asserts.
func seedCommentKind(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID, body, kind string) string {
	t.Helper()
	id := testsupport.SeedComment(t, pool, issueID, author, body)
	if _, err := pool.Exec(context.Background(),
		`UPDATE comments SET author_kind = $2 WHERE id = $1`, id, kind); err != nil {
		t.Fatalf("set author_kind=%s on comment %s: %v", kind, id, err)
	}
	return id.String()
}

// TestSearch_CommentAuthorKinds_Filters asserts the comment-only filter:
// a query matching both a human and an agent_autonomous comment returns
// both when unfiltered, only the human comment when filtered to `human`,
// and only the agent_autonomous comment when filtered to that kind. The
// FTS-only (degraded) comment CTE is the path under test.
func TestSearch_CommentAuthorKinds_Filters(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tscak")
	tok := token(t)

	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, issueID, "host issue", "open", 2)

	humanID := seedCommentKind(t, pool, issueID, owner, "human note "+tok, issues.CommentAuthorKindHuman)
	autoID := seedCommentKind(t, pool, issueID, owner, "agent note "+tok, issues.CommentAuthorKindAgentAutonomous)

	base := search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindComment},
		Limit:    50,
		CallerID: owner,
	}

	// Unfiltered → both comments.
	res, err := svc.Search(ctx(), base)
	if err != nil {
		t.Fatalf("Search unfiltered: %v", err)
	}
	if !res.Degraded {
		t.Errorf("degraded=false, want true (nil embedder ⇒ FTS-only)")
	}
	got := idSet(res.Items)
	if len(got) != 2 || !got[humanID] || !got[autoID] {
		t.Fatalf("unfiltered got %v, want both human(%s) + auto(%s)", got, humanID, autoID)
	}

	// Filter human → only the human comment.
	p := base
	p.Filters = search.Filters{CommentAuthorKinds: []string{issues.CommentAuthorKindHuman}}
	res, err = svc.Search(ctx(), p)
	if err != nil {
		t.Fatalf("Search human: %v", err)
	}
	got = idSet(res.Items)
	if len(got) != 1 || !got[humanID] {
		t.Fatalf("human filter got %v, want only human(%s); auto(%s) must be excluded", got, humanID, autoID)
	}

	// Filter agent_autonomous → only the agent comment.
	p.Filters = search.Filters{CommentAuthorKinds: []string{issues.CommentAuthorKindAgentAutonomous}}
	res, err = svc.Search(ctx(), p)
	if err != nil {
		t.Fatalf("Search agent_autonomous: %v", err)
	}
	got = idSet(res.Items)
	if len(got) != 1 || !got[autoID] {
		t.Fatalf("agent_autonomous filter got %v, want only auto(%s); human(%s) must be excluded", got, autoID, humanID)
	}
}

// TestSearch_CommentAuthorKinds_DoesNotAffectIssues asserts the filter is
// comment-only: an issue whose text matches the token is returned even when
// a comment-author-kind filter is set, with the issue surfaced via the
// default cross-kind path. The filter must not leak into the issues query.
func TestSearch_CommentAuthorKinds_DoesNotAffectIssues(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tscaki")
	tok := token(t)

	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, issueID, "issue "+tok+" body", "open", 2)
	// An agent_autonomous comment that also carries the token — it must be
	// filtered OUT by the human filter, while the issue stays.
	seedCommentKind(t, pool, issueID, owner, "agent comment "+tok, issues.CommentAuthorKindAgentAutonomous)

	res, err := svc.Search(ctx(), search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindIssue, search.KindComment},
		Limit:    50,
		CallerID: owner,
		Filters:  search.Filters{CommentAuthorKinds: []string{issues.CommentAuthorKindHuman}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var sawIssue, sawComment bool
	for _, it := range res.Items {
		switch it.Kind {
		case search.KindIssue:
			if it.ID == issueID {
				sawIssue = true
			}
		case search.KindComment:
			sawComment = true
		}
	}
	if !sawIssue {
		t.Errorf("issue %s missing — comment filter must not affect issue results; items=%+v", issueID, res.Items)
	}
	if sawComment {
		t.Errorf("agent_autonomous comment leaked under human filter; items=%+v", res.Items)
	}
}

// TestSearch_CommentAuthorKinds_CacheKeyDistinguishes proves the result
// cache key distinguishes filtered from unfiltered. We run the UNFILTERED
// query first (warming the cache under its key), then the FILTERED query;
// if the key did NOT incorporate the filter, the filtered call would hit
// the warm unfiltered entry and wrongly return both comments. Asserting the
// filtered call is actually filtered proves the keys differ.
func TestSearch_CommentAuthorKinds_CacheKeyDistinguishes(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tscakk")
	tok := token(t)

	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, issueID, "host issue", "open", 2)
	humanID := seedCommentKind(t, pool, issueID, owner, "human "+tok, issues.CommentAuthorKindHuman)
	autoID := seedCommentKind(t, pool, issueID, owner, "auto "+tok, issues.CommentAuthorKindAgentAutonomous)

	base := search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindComment},
		Limit:    50,
		CallerID: owner,
	}

	// 1) Warm the cache with the unfiltered query.
	res, err := svc.Search(ctx(), base)
	if err != nil {
		t.Fatalf("Search unfiltered (warm): %v", err)
	}
	if got := idSet(res.Items); len(got) != 2 {
		t.Fatalf("warm unfiltered got %v, want both comments", got)
	}

	// 2) Same query but filtered to human. If the cache key did not include
	// the filter, this would return the warm 2-item unfiltered entry.
	p := base
	p.Filters = search.Filters{CommentAuthorKinds: []string{issues.CommentAuthorKindHuman}}
	res, err = svc.Search(ctx(), p)
	if err != nil {
		t.Fatalf("Search human (post-warm): %v", err)
	}
	got := idSet(res.Items)
	if len(got) != 1 || !got[humanID] {
		t.Fatalf("filtered call returned %v — cache key did NOT distinguish filtered from unfiltered (auto=%s leaked)", got, autoID)
	}
}

// TestCacheKey_CommentAuthorKinds is a pure unit assert: two Params equal
// in every field except CommentAuthorKinds hash to different keys, and the
// slice is order-normalized (sort) so [a,b] and [b,a] collapse to one key.
func TestCacheKey_CommentAuthorKinds(t *testing.T) {
	uid := uuid.Must(uuid.NewRandom())
	mk := func(kinds ...string) search.Params {
		p := search.Params{
			Query:    "alpha",
			Kinds:    search.AllKinds,
			CallerID: uid,
		}
		if len(kinds) > 0 {
			p.Filters.CommentAuthorKinds = kinds
		}
		return p
	}

	none := search.CacheKey(mk())
	human := search.CacheKey(mk(issues.CommentAuthorKindHuman))
	if none == human {
		t.Fatalf("cache key did not change when commentAuthorKinds was set")
	}

	ab := search.CacheKey(mk(issues.CommentAuthorKindHuman, issues.CommentAuthorKindAgentAutonomous))
	ba := search.CacheKey(mk(issues.CommentAuthorKindAgentAutonomous, issues.CommentAuthorKindHuman))
	if ab != ba {
		t.Fatalf("cache key is order-sensitive on commentAuthorKinds: %s != %s", ab, ba)
	}
}

func idSet(items []search.Item) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it.ID] = true
	}
	return out
}
