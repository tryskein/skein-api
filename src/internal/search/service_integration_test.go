package search_test

// Integration coverage for the search Service + Store FTS path WITHOUT a
// live Ollama/embedder (sk-105.6 / spec §3.4, §5 M8).
//
// The whole suite drives the FTS-only branch: the Service is constructed
// with a NIL embedder, so every request degrades to FTS-only candidate
// lists (service.go ~120-122) and the store's `in.Vec == nil` branches run.
// No vector column is ever written, no Ollama is contacted.
//
// All asserts are scoped to the test's own seeded project (every seeded
// issue/comment/component carries a project-unique token in its text, and
// every search is run with Params.Project set to the seed project) so a
// concurrent package's rows can never perturb a count or ordering check.
// This honours the M7 "never assert a bare global / WHERE type=… count"
// invariant: we filter by the seeded project, and additionally by a
// random per-test token embedded in the searched text.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// newFTSService builds a Service with a nil embedder (FTS-only / degraded
// mode) over the shared test pool. A tiny cache TTL keeps cursor tests
// deterministic without cross-test bleed.
func newFTSService(t *testing.T) *search.Service {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	store := search.NewStore(pool)
	svc, err := search.NewService(store, nil, 60*time.Second)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// token returns a short, FTS-friendly, per-test-unique lowercase token.
// Embedded into seeded text it lets a query target ONLY this test's rows,
// so asserts stay isolated under package parallelism.
func token(t *testing.T) string {
	t.Helper()
	return "zql" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")
}

func ctx() context.Context { return context.Background() }

// TestSearch_FTSDegraded_IssuesHit verifies the core FTS path: an issue
// whose title contains the query token is returned, with degraded=true
// (nil embedder) and the issue-specific fields populated.
func TestSearch_FTSDegraded_IssuesHit(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tssrch")
	tok := token(t)

	hitID := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, hitID, "alpha "+tok+" widget", "open", 2)
	// A decoy in the SAME project that does NOT contain the token — must
	// not appear for a token query.
	testsupport.SeedIssue(t, pool, projID, owner, prefix+"-2", "unrelated banana", "open", 3)

	res, err := svc.Search(ctx(), search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindIssue},
		Limit:    50,
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !res.Degraded {
		t.Errorf("degraded = false, want true (nil embedder ⇒ FTS-only)")
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want exactly 1 (the token issue): %+v", len(res.Items), res.Items)
	}
	got := res.Items[0]
	if got.ID != hitID {
		t.Errorf("item id = %q, want %q", got.ID, hitID)
	}
	if got.Kind != search.KindIssue {
		t.Errorf("kind = %q, want issue", got.Kind)
	}
	if got.Status == nil || *got.Status != "open" {
		t.Errorf("status = %v, want open", got.Status)
	}
	if got.ProjectID == nil || *got.ProjectID != projID.String() {
		t.Errorf("projectId = %v, want %s", got.ProjectID, projID)
	}
}

// TestSearch_KindFilter_ExcludesOtherKinds seeds an issue, a comment, and
// a component that all share the query token, then asserts that a
// kind-scoped query returns ONLY the requested kind.
func TestSearch_KindFilter_ExcludesOtherKinds(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tskind")
	tok := token(t)

	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, issueID, "issue "+tok, "open", 2)
	testsupport.SeedComment(t, pool, issueID, owner, "comment body "+tok+" here")
	testsupport.SeedComponent(t, pool, projID, owner, "comp"+tok, "component about "+tok)

	// kind=component only.
	res, err := svc.Search(ctx(), search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindComponent},
		Limit:    50,
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("Search component: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("component-only query returned %d items, want 1: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].Kind != search.KindComponent {
		t.Errorf("kind = %q, want component", res.Items[0].Kind)
	}

	// kind=comment only.
	res, err = svc.Search(ctx(), search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindComment},
		Limit:    50,
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("Search comment: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("comment-only query returned %d items, want 1: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].Kind != search.KindComment {
		t.Errorf("kind = %q, want comment", res.Items[0].Kind)
	}
	if res.Items[0].ParentIssueID == nil || *res.Items[0].ParentIssueID != issueID {
		t.Errorf("comment parentIssueId = %v, want %s", res.Items[0].ParentIssueID, issueID)
	}
}

// TestSearch_AllKinds_MergesEveryKind asserts the cross-kind merge: with
// the default kind set, the token query surfaces the issue, the comment,
// the project, and the component — all four kinds in one result list.
func TestSearch_AllKinds_MergesEveryKind(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	tok := token(t)
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsall")
	// SeedProject glues namePrefix+suffix into one non-tokenizable
	// identifier, so the token must land in the project DESCRIPTION as a
	// standalone word for FTS (whole-lexeme, not substring) to match the
	// project kind. The projects tsvector (000006) weights description.
	if _, err := pool.Exec(ctx(),
		`UPDATE projects SET description = $2 WHERE id = $1`,
		projID, "project about "+tok); err != nil {
		t.Fatalf("set project description: %v", err)
	}

	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, issueID, "the "+tok+" task", "open", 2)
	testsupport.SeedComment(t, pool, issueID, owner, "discussing "+tok)
	testsupport.SeedComponent(t, pool, projID, owner, "c"+tok, tok+" module")

	res, err := svc.Search(ctx(), search.Params{
		Query:    tok,
		Project:  &projID,
		Limit:    50,
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("Search all kinds: %v", err)
	}

	seen := map[search.Kind]int{}
	for _, it := range res.Items {
		seen[it.Kind]++
	}
	for _, k := range []search.Kind{search.KindIssue, search.KindComment, search.KindProject, search.KindComponent} {
		if seen[k] == 0 {
			t.Errorf("kind %q missing from merged result; got kinds %v (items %+v)", k, seen, res.Items)
		}
	}
}

// TestSearch_ExactIDBoost verifies the id fast-path: querying a typed-out
// issue id surfaces that exact issue with ExactMatch=true, ranked first
// even when a fuzzy-text decoy also mentions the same token.
func TestSearch_ExactIDBoost(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsexact")

	exactID := prefix + "-1"
	testsupport.SeedIssue(t, pool, projID, owner, exactID, "first issue", "open", 2)

	res, err := svc.Search(ctx(), search.Params{
		Query:    exactID, // a full typed id — LooksLikeID ⇒ fast path
		Project:  &projID,
		Limit:    50,
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("Search exact id: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("exact-id query returned nothing")
	}
	top := res.Items[0]
	if top.ID != exactID {
		t.Fatalf("top item = %q, want exact id %q", top.ID, exactID)
	}
	if !top.ExactMatch {
		t.Errorf("top.ExactMatch = false, want true for exact id hit")
	}
}

// TestSearch_BareNumberTail covers the numeric-tail fast-path: a bare
// number query matches the issue whose id ends in -N (and not -N0).
func TestSearch_BareNumberTail(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsnum")

	want := prefix + "-7"
	testsupport.SeedIssue(t, pool, projID, owner, want, "lucky seven", "open", 2)
	testsupport.SeedIssue(t, pool, projID, owner, prefix+"-70", "seventy", "open", 2)

	res, err := svc.Search(ctx(), search.Params{
		Query:    "7",
		Project:  &projID,
		Kinds:    []search.Kind{search.KindIssue},
		Limit:    50,
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("Search bare number: %v", err)
	}
	var found bool
	for _, it := range res.Items {
		if it.ID == prefix+"-70" {
			t.Errorf("bare `7` matched %q — should anchor to -7, not -70", it.ID)
		}
		if it.ID == want {
			found = true
			if !it.ExactMatch {
				t.Errorf("numeric-tail hit %q ExactMatch=false, want true", it.ID)
			}
		}
	}
	if !found {
		t.Fatalf("bare `7` did not match %q; items=%+v", want, res.Items)
	}
}

// TestSearch_Pagination_Cursor seeds enough matching issues to span two
// pages, then walks the cursor and asserts: page 1 has `limit` items +
// HasMore + a cursor, page 2 returns the remainder, and no id repeats.
func TestSearch_Pagination_Cursor(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tspage")
	tok := token(t)

	const total = 5
	for i := 1; i <= total; i++ {
		testsupport.SeedIssue(t, pool, projID, owner, prefix+"-"+itoa(i), tok+" paginated item", "open", 2)
	}

	params := search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindIssue},
		Limit:    2,
		CallerID: owner,
	}
	page1, err := svc.Search(ctx(), params)
	if err != nil {
		t.Fatalf("Search page1: %v", err)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1.Items))
	}
	if !page1.HasMore || page1.NextCursor == nil {
		t.Fatalf("page1 HasMore=%v cursor=%v, want more pages", page1.HasMore, page1.NextCursor)
	}

	seen := map[string]bool{}
	for _, it := range page1.Items {
		seen[it.ID] = true
	}

	// Walk remaining pages.
	cursor := *page1.NextCursor
	pages := 1
	for {
		params.Cursor = cursor
		p, err := svc.Search(ctx(), params)
		if err != nil {
			t.Fatalf("Search cursor page: %v", err)
		}
		pages++
		for _, it := range p.Items {
			if seen[it.ID] {
				t.Errorf("id %q repeated across pages", it.ID)
			}
			seen[it.ID] = true
		}
		if !p.HasMore || p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
		if pages > 10 {
			t.Fatalf("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("walked %d distinct ids across %d pages, want %d", len(seen), pages, total)
	}
}

// TestSearch_NonMemberVisibility asserts the permission filter: a
// non-admin caller who is NOT a member of the seed project gets zero hits
// for a token that only this project's issue carries.
func TestSearch_NonMemberVisibility(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	outsider, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsvis")
	tok := token(t)
	testsupport.SeedIssue(t, pool, projID, owner, prefix+"-1", "secret "+tok, "open", 2)

	// Owner (member) sees it.
	res, err := svc.Search(ctx(), search.Params{
		Query: tok, Project: &projID, Kinds: []search.Kind{search.KindIssue},
		Limit: 50, CallerID: owner,
	})
	if err != nil {
		t.Fatalf("Search owner: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("member owner saw %d items, want 1", len(res.Items))
	}

	// Outsider (non-member, non-admin) sees nothing.
	res, err = svc.Search(ctx(), search.Params{
		Query: tok, Project: &projID, Kinds: []search.Kind{search.KindIssue},
		Limit: 50, CallerID: outsider,
	})
	if err != nil {
		t.Fatalf("Search outsider: %v", err)
	}
	if len(res.Items) != 0 {
		t.Errorf("non-member outsider saw %d items, want 0: %+v", len(res.Items), res.Items)
	}
}

// TestSearch_IssueStatusFilter verifies the issue-only Filters path:
// status=open narrows to the open issue, excluding the closed one, both
// sharing the query token.
func TestSearch_IssueStatusFilter(t *testing.T) {
	svc := newFTSService(t)
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tsfilt")
	tok := token(t)

	openID := prefix + "-1"
	closedID := prefix + "-2"
	testsupport.SeedIssue(t, pool, projID, owner, openID, tok+" open one", "open", 2)
	testsupport.SeedIssue(t, pool, projID, owner, closedID, tok+" closed one", "closed", 2)

	res, err := svc.Search(ctx(), search.Params{
		Query:    tok,
		Project:  &projID,
		Kinds:    []search.Kind{search.KindIssue},
		Limit:    50,
		CallerID: owner,
		Filters:  search.Filters{Statuses: []string{"open"}},
	})
	if err != nil {
		t.Fatalf("Search status filter: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != openID {
		t.Fatalf("status=open filter returned %+v, want only %s", res.Items, openID)
	}
}

// itoa is a tiny local int→string (avoid pulling strconv into many sites).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
