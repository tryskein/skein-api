package issues

// commits_detach_integration_test.go — sk-394.
//
// A regression for a defect that had no subject: unlinking
// did not exist. The hash is accepted by FORM (lowercase hex, 7..64) — the server has no clone
// of the repository and nobody to ask — so an abbreviation typed
// by hand passes the check and is recorded. It happened in
// sk-285.7.2.88: next to three real commits lay an invention, and
// there was nothing to remove it with — no MCP tool, no REST route, only
// editing issue_commits in the database. "Taken on faith" plus "forever"
// turned one typo into a permanent lie in the evidence part of
// the tracker.
//
// The fixture reaches the DISTINGUISHING case, not just the first one at hand:
// the project has TWO repositories, the same hash is linked to both, and next to it
// lies a second, real hash. An implementation that resolves the repository
// (as attach must) would fail on this fixture with
// repo_ambiguous; an implementation that wipes all of the issue's links would take away the
// real one. A fixture with one repository and one link would distinguish
// neither of them.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const (
	// detachHashBogus — the hash that plays the invention in this story:
	// its rows must disappear from BOTH repositories.
	detachHashBogus = "5cff4e28a1f9c0e1a5f2c8b0d3e4f5a6b7c8d9e0"
	// detachHashReal — the neighbouring real link: it must survive.
	detachHashReal = "b250cd9"
)

// TestDetachCommits_RemovesNamedHashAcrossRepos_LeavesTheRest — a guard:
// unlinking removes exactly the named hash, in all repositories, and does not
// touch the neighbouring link or the issue itself.
func TestDetachCommits_RemovesNamedHashAcrossRepos_LeavesTheRest(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	repo1 := testsupport.SeedRepo(t, pool, pid, "repo1", "https://example.com/repo1.git")
	repo2 := testsupport.SeedRepo(t, pool, pid, "repo2", "https://example.com/repo2.git")
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	id := prefix + "-1"
	seedIssue(t, pool, pid, owner, id, "ship it")

	store := NewStore(pool)

	// The same hash in TWO repositories + a real link next to it.
	if _, err := store.AttachCommits(ctx, AttachCommitsInput{
		ID:    id,
		Actor: actor,
		Commits: []CloseCommitInput{
			{Hash: detachHashBogus, RepoURL: "https://example.com/repo1.git"},
			{Hash: detachHashBogus, RepoURL: "https://example.com/repo2.git"},
			{Hash: detachHashReal, RepoURL: "https://example.com/repo1.git"},
		},
	}); err != nil {
		t.Fatalf("AttachCommits (fixture): %v", err)
	}
	if got := countCommitRows(t, pool, id, detachHashBogus); got != 2 {
		t.Fatalf("the fixture did not reach the distinguishing case: rows of the invented hash %d, expected 2 "+
			"(one per repository) — without them the test does not check the walk over both repositories", got)
	}

	before := issueVersion(t, pool, id)

	res, err := store.DetachCommits(ctx, DetachCommitsInput{
		ID:     id,
		Hashes: []string{detachHashBogus},
		Actor:  actor,
	})
	if err != nil {
		t.Fatalf("DetachCommits: %v", err)
	}

	// 1. No rows of the invented hash are left — in any repository.
	if got := countCommitRows(t, pool, id, detachHashBogus); got != 0 {
		t.Errorf("after unlinking %d rows of the invented hash remain, expected 0", got)
	}
	// 2. The neighbouring real link is in place.
	if got := countCommitRows(t, pool, id, detachHashReal); got != 1 {
		t.Errorf("real link %s: rows %d, expected 1 — unlinking took someone else's", detachHashReal, got)
	}
	// 3. The response names BOTH removed rows together with the repository the
	//    caller did not name, and counts nothing as not found.
	if len(res.Detached) != 2 {
		t.Fatalf("res.Detached = %+v, expected 2 rows", res.Detached)
	}
	gotRepos := map[uuid.UUID]bool{}
	for _, ref := range res.Detached {
		if ref.Hash != detachHashBogus {
			t.Errorf("res.Detached contains someone else's hash %q", ref.Hash)
		}
		gotRepos[ref.RepoID] = true
	}
	if !gotRepos[repo1] || !gotRepos[repo2] {
		t.Errorf("res.Detached named repositories %v, expected both: %s and %s", gotRepos, repo1, repo2)
	}
	if len(res.NotFound) != 0 {
		t.Errorf("res.NotFound = %v, expected empty — the hash was linked", res.NotFound)
	}
	// 4. The issue card in the response sees the same as the database.
	for _, ref := range res.Issue.CommitRefs {
		if ref.Hash == detachHashBogus {
			t.Errorf("CommitRefs post-state still contains the unlinked hash %q", ref.Hash)
		}
	}
	// 5. Nothing but the links moved.
	if after := issueVersion(t, pool, id); after != before {
		t.Errorf("the issue version moved %d -> %d — unlinking must touch only the links", before, after)
	}

	// 6. Exactly one event, and it carries ONE hash, not one per repository:
	//    the event answers the question "which commits stopped being linked".
	if n := testsupport.CountEventsForProject(t, pool, pid, EventIssueCommitsDetached); n != 1 {
		t.Fatalf("events %s: %d, expected 1", EventIssueCommitsDetached, n)
	}
	if got := detachedEventHashes(t, pool, pid); len(got) != 1 || got[0] != detachHashBogus {
		t.Errorf("event payload = %v, expected [%s]", got, detachHashBogus)
	}

	// 7. sk-540.9: next to `hashes` the event names EACH removed row
	//    as a (repoId, hash) pair — otherwise the journal of a multi-repo issue
	//    does not show what exactly was removed. Both pairs, nothing beyond them.
	gotRefs := detachedEventRefs(t, pool, pid)
	wantRefs := map[detachedEventRef]bool{
		{RepoID: repo1.String(), Hash: detachHashBogus}: true,
		{RepoID: repo2.String(), Hash: detachHashBogus}: true,
	}
	if len(gotRefs) != len(wantRefs) {
		t.Fatalf("payload.refs = %+v, expected exactly %d pairs", gotRefs, len(wantRefs))
	}
	for _, r := range gotRefs {
		if !wantRefs[r] {
			t.Errorf("payload.refs contains the pair %+v, which was not removed", r)
		}
		delete(wantRefs, r)
	}
	if len(wantRefs) != 0 {
		t.Errorf("payload.refs did not name the pairs %+v", wantRefs)
	}
}

// TestDetachCommits_UnlinkedHashIsReportedNotRemoved — the CONTROL for
// the guard above. "One event and no rows left" is worth nothing without
// proof that on an unlinked hash the counter does NOT move: otherwise
// the same green would be given by an implementation that writes an event on every call and
// wipes whatever it likes.
func TestDetachCommits_UnlinkedHashIsReportedNotRemoved(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	testsupport.SeedRepo(t, pool, pid, "repo1", "https://example.com/repo1.git")
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	id := prefix + "-1"
	seedIssue(t, pool, pid, owner, id, "ship it")

	store := NewStore(pool)
	if _, err := store.AttachCommits(ctx, AttachCommitsInput{
		ID:      id,
		Actor:   actor,
		Commits: []CloseCommitInput{{Hash: detachHashReal}},
	}); err != nil {
		t.Fatalf("AttachCommits (fixture): %v", err)
	}

	const neverLinked = "0123456789abcdef0123456789abcdef01234567"
	res, err := store.DetachCommits(ctx, DetachCommitsInput{
		ID:     id,
		Hashes: []string{neverLinked},
		Actor:  actor,
	})
	if err != nil {
		t.Fatalf("DetachCommits on an unlinked hash returned an error: %v", err)
	}
	if len(res.Detached) != 0 {
		t.Errorf("res.Detached = %+v, expected empty — this hash was never linked", res.Detached)
	}
	if len(res.NotFound) != 1 || res.NotFound[0] != neverLinked {
		t.Errorf("res.NotFound = %v, expected [%s] — otherwise the caller cannot tell a typo from a removal",
			res.NotFound, neverLinked)
	}
	if got := countCommitRows(t, pool, id, detachHashReal); got != 1 {
		t.Errorf("the linked reference suffered from unlinking someone else's hash: rows %d, expected 1", got)
	}
	if n := testsupport.CountEventsForProject(t, pool, pid, EventIssueCommitsDetached); n != 0 {
		t.Errorf("events %s: %d, expected 0 — an event about a removal that did not happen "+
			"is read by the next auditor as \"somebody unlinked a commit\"", EventIssueCommitsDetached, n)
	}

	// Unlinking an already removed hash again — the same response, and that is exactly
	// what the tool's idempotentHint promises.
	if _, err := store.DetachCommits(ctx, DetachCommitsInput{
		ID:     id,
		Hashes: []string{detachHashReal},
		Actor:  actor,
	}); err != nil {
		t.Fatalf("first unlinking of the real hash: %v", err)
	}
	again, err := store.DetachCommits(ctx, DetachCommitsInput{
		ID:     id,
		Hashes: []string{detachHashReal},
		Actor:  actor,
	})
	if err != nil {
		t.Fatalf("repeated unlinking: %v", err)
	}
	if len(again.Detached) != 0 || len(again.NotFound) != 1 {
		t.Errorf("repeated unlinking: detached=%+v notFound=%v, expected nothing to remove and one notFound",
			again.Detached, again.NotFound)
	}
	if n := testsupport.CountEventsForProject(t, pool, pid, EventIssueCommitsDetached); n != 1 {
		t.Errorf("events after two unlinkings: %d, expected 1 — the second removed nothing", n)
	}
}

// TestDetachCommits_MalformedHashRejected — the hash form is checked on
// the way out as well. Without this the garbage that attach rejects, detach would accept and
// return as `notFound` — "no such link here", indistinguishable from a correctly
// written but unlinked hash, and the caller would not learn about their
// typo.
func TestDetachCommits_MalformedHashRejected(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	testsupport.SeedRepo(t, pool, pid, "repo1", "https://example.com/repo1.git")
	uname := testsupport.Username(t, pool, owner)
	actor := Actor{UserID: owner, Username: uname, Role: "human"}

	id := prefix + "-1"
	seedIssue(t, pool, pid, owner, id, "ship it")

	store := NewStore(pool)
	if _, err := store.AttachCommits(ctx, AttachCommitsInput{
		ID:      id,
		Actor:   actor,
		Commits: []CloseCommitInput{{Hash: detachHashReal}},
	}); err != nil {
		t.Fatalf("AttachCommits (fixture): %v", err)
	}

	for _, bad := range []string{
		"B250CD9", // upper case — attach rejects this
		"b250c",   // shorter than seven
		"zzzzzzz", // not hex
		"",        // empty
	} {
		res, err := store.DetachCommits(ctx, DetachCommitsInput{
			ID:     id,
			Hashes: []string{bad},
			Actor:  actor,
		})
		if err == nil {
			t.Errorf("DetachCommits(%q) accepted, expected a ValidationError (response: %+v)", bad, res)
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("DetachCommits(%q) returned %T (%v), expected *ValidationError", bad, err, err)
		}
	}

	// No rejection touched the linked reference.
	if got := countCommitRows(t, pool, id, detachHashReal); got != 1 {
		t.Errorf("after the rejections rows of the real link: %d, expected 1", got)
	}
	if n := testsupport.CountEventsForProject(t, pool, pid, EventIssueCommitsDetached); n != 0 {
		t.Errorf("the rejected request wrote %d events %s, expected 0", n, EventIssueCommitsDetached)
	}
}

// ---------------- helpers ----------------

func countCommitRows(t *testing.T, pool *pgxpool.Pool, issueID, hash string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_commits WHERE issue_id = $1 AND hash = $2`,
		issueID, hash).Scan(&n); err != nil {
		t.Fatalf("count issue_commits: %v", err)
	}
	return n
}

// detachedEventHashes reads old_data.hashes off the single
// issue.commits_detached event of the project.
func detachedEventHashes(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) []string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT old_data FROM events WHERE project_id = $1 AND type = $2`,
		projectID, EventIssueCommitsDetached).Scan(&raw); err != nil {
		t.Fatalf("read issue.commits_detached payload: %v", err)
	}
	var payload struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode issue.commits_detached payload %q: %v", raw, err)
	}
	return payload.Hashes
}

// detachedEventRef is one entry of old_data.refs of issue.commits_detached.
type detachedEventRef struct {
	RepoID string `json:"repoId"`
	Hash   string `json:"hash"`
}

// detachedEventRefs reads old_data.refs off the single
// issue.commits_detached event of the project.
func detachedEventRefs(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) []detachedEventRef {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT old_data FROM events WHERE project_id = $1 AND type = $2`,
		projectID, EventIssueCommitsDetached).Scan(&raw); err != nil {
		t.Fatalf("read issue.commits_detached payload: %v", err)
	}
	var payload struct {
		Refs []detachedEventRef `json:"refs"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode issue.commits_detached payload %q: %v", raw, err)
	}
	return payload.Refs
}

// issueVersion reads the issue's optimistic-concurrency counter straight
// from the table — the point of the assertion is that DetachCommits does
// not move it, so reading it through the store's own post-state would
// prove less.
func issueVersion(t *testing.T, pool *pgxpool.Pool, issueID string) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		`SELECT version FROM issues WHERE id = $1`, issueID).Scan(&v); err != nil {
		t.Fatalf("read issues.version: %v", err)
	}
	return v
}
