package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// seedProjectWithRepos seeds a project with a populated git_repos[]
// jsonb array and a project_members owner row. Returns project id,
// id_prefix, and the slice of repo ids in input order.
func seedProjectWithRepos(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, namePrefix string, repoCount int) (uuid.UUID, string, []uuid.UUID) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := namePrefix + "_" + suffix
	idPrefix := namePrefix + suffix[:6]

	type repoRow struct {
		ID            uuid.UUID `json:"id"`
		Name          string    `json:"name"`
		URL           string    `json:"url"`
		DefaultBranch string    `json:"defaultBranch,omitempty"`
	}
	repos := make([]repoRow, repoCount)
	repoIDs := make([]uuid.UUID, repoCount)
	for i := 0; i < repoCount; i++ {
		rid := uuid.Must(uuid.NewV7())
		repoIDs[i] = rid
		repos[i] = repoRow{
			ID:   rid,
			Name: fmt.Sprintf("repo%d", i+1),
			URL:  fmt.Sprintf("https://example.com/%s/repo%d.git", idPrefix, i+1),
		}
	}
	repoBytes, err := json.Marshal(repos)
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}

	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err = pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', $4::jsonb, NULL, $5, now(), now(), '{}'::jsonb, $6)`,
		id, name, idPrefix, string(repoBytes), owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project with repos: %v", err)
	}
	_, err = pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner)
	if err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed project counter: %v", err)
	}
	t.Cleanup(func() {
		// issues.project_id is ON DELETE RESTRICT (migration 000004), so
		// delete issues leaf-first (children before parents) before the
		// project, else the project DELETE silently fails and rows leak.
		for range 64 {
			ct, err := pool.Exec(context.Background(), `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = $1 AND parent_id IS NOT NULL)`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, idPrefix, repoIDs
}

// TestClose_WithCommit_SingleRepo: project has exactly one repo, no
// repoUrl/repoId on the commit ref → resolver picks the only repo,
// commit is persisted, returned IssueDetail.commitRefs reflects it.
func TestClose_WithCommit_SingleRepo(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, repoIDs := seedProjectWithRepos(t, pool, userID, "clcs", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "ship feature", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcs")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "implemented",
		"commits": []any{
			map[string]any{
				"hash":    "abc1234",
				"message": "feat: ship feature",
				"author":  "alice@example.com",
			},
		},
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	// sk-282 T021: the close acknowledges, so commitRefs — card data, not
	// post-state — is read back rather than echoed. The link itself is
	// what this test is about, and reading it proves more than an echo.
	showCard := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
		map[string]any{"id": issueID, "includeComments": false, "includeDeps": false})
	shown, ok := showCard["issue"].(map[string]any)
	if !ok {
		t.Fatalf("skein_show issue type %T", showCard["issue"])
	}
	commitRefs, _ := shown["commitRefs"].([]any)
	if len(commitRefs) != 1 {
		t.Fatalf("commitRefs len = %d, want 1; issue=%+v", len(commitRefs), shown)
	}
	first, _ := commitRefs[0].(map[string]any)
	if first["hash"] != "abc1234" {
		t.Fatalf("commitRefs[0].hash = %v, want abc1234", first["hash"])
	}
	if first["repoId"] != repoIDs[0].String() {
		t.Fatalf("commitRefs[0].repoId = %v, want %s", first["repoId"], repoIDs[0])
	}
	if first["repoUrl"] == "" {
		t.Fatalf("commitRefs[0].repoUrl missing")
	}
	if first["linkedByKind"] != "agent" {
		t.Fatalf("commitRefs[0].linkedByKind = %v, want agent", first["linkedByKind"])
	}

	// DB row exists.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_commits WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("query commits: %v", err)
	}
	if n != 1 {
		t.Fatalf("issue_commits rows = %d, want 1", n)
	}
}

// TestClose_WithCommit_RepoURLLookup: multi-repo project, caller supplies
// repoUrl → resolver picks the matching entry.
func TestClose_WithCommit_RepoURLLookup(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, repoIDs := seedProjectWithRepos(t, pool, userID, "clcurl", 3)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "url-pick", "open", 2)

	// We'll pick the second repo by URL.
	targetURL := fmt.Sprintf("https://example.com/%s/repo2.git", prefix)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcurl")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "implemented in repo2",
		"commits": []any{
			map[string]any{
				"hash":    "deadbeef",
				"repoUrl": targetURL,
			},
		},
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}

	var repoIDStr string
	if err := pool.QueryRow(context.Background(),
		`SELECT repo_id::text FROM issue_commits WHERE issue_id = $1`, issueID).Scan(&repoIDStr); err != nil {
		t.Fatalf("query commit repo_id: %v", err)
	}
	if repoIDStr != repoIDs[1].String() {
		t.Fatalf("repo_id = %s, want %s (repo2)", repoIDStr, repoIDs[1])
	}
}

// TestClose_WithCommit_AmbiguousRepo: multi-repo project, no
// repoUrl/repoId → top-level error=repo_ambiguous and the issue
// stays open (the SERIALIZABLE tx rolls back).
//
// sk-303.36: the body must ALSO carry the project's declared repos so
// the caller can retry immediately with a valid repoId/repoUrl instead
// of a separate skein_project_resolve-style lookup round-trip.
func TestClose_WithCommit_AmbiguousRepo(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, repoIDs := seedProjectWithRepos(t, pool, userID, "clcamb", 2)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "ambiguous", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcamb")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "ambiguous",
		"commits": []any{
			map[string]any{"hash": "abc1234"},
		},
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on ambiguous repo")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeRepoAmbiguous {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeRepoAmbiguous)
	}
	if total, _ := body["reposTotal"].(float64); total != 2 {
		t.Fatalf("reposTotal = %v, want 2", body["reposTotal"])
	}
	repos, ok := body["repos"].([]any)
	if !ok || len(repos) != 2 {
		t.Fatalf("repos = %v, want a 2-entry list", body["repos"])
	}
	gotIDs := map[string]bool{}
	for _, r := range repos {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("repos entry = %v, want an object", r)
		}
		if row["url"] == "" || row["url"] == nil {
			t.Fatalf("repos entry missing url: %v", row)
		}
		gotIDs[row["id"].(string)] = true
	}
	for _, rid := range repoIDs {
		if !gotIDs[rid.String()] {
			t.Errorf("repos list missing seeded repo id %s: %v", rid, repos)
		}
	}
	if body["reposTruncated"] != nil {
		t.Errorf("reposTruncated = %v, want absent for a 2-repo project", body["reposTruncated"])
	}

	// Issue did NOT close — tx rolled back.
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM issues WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "open" {
		t.Fatalf("issue status = %s, want open (tx must roll back)", status)
	}
}

// TestClose_WithCommit_AmbiguousRepo_ZeroRepos: sk-303.36's "tell them apart" —
// a project with NO declared repos hits the same repo_ambiguous code as
// the multi-repo case, but "specify repoUrl or repoId" is impossible
// advice when there is nothing to specify. The message must say so
// instead, and reposTotal must read 0 rather than omitting the field.
func TestClose_WithCommit_AmbiguousRepo_ZeroRepos(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "clcnorepo", 0)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "no repos declared", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcnorepo")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "no repos declared",
		"commits": []any{
			map[string]any{"hash": "abc1234"},
		},
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on zero-repo project")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeRepoAmbiguous {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeRepoAmbiguous)
	}
	if total, _ := body["reposTotal"].(float64); total != 0 {
		t.Fatalf("reposTotal = %v, want 0", body["reposTotal"])
	}
	msg, _ := body["message"].(string)
	if strings.Contains(msg, "specify repoUrl or repoId") {
		t.Errorf("message = %q, should not ask to specify a repo when none exist", msg)
	}
	if !strings.Contains(msg, "no repos declared") {
		t.Errorf("message = %q, want it to say no repos are declared", msg)
	}
}

// TestClose_WithCommit_RepoNotFound: repoUrl that doesn't match any
// project repo → top-level error=repo_not_found.
//
// sk-303.36: same treatment as repo_ambiguous — the body carries the
// project's actual repos so the caller sees what WOULD have matched.
func TestClose_WithCommit_RepoNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, repoIDs := seedProjectWithRepos(t, pool, userID, "clcrnf", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "wrong-repo", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcrnf")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "wrong repo",
		"commits": []any{
			map[string]any{
				"hash":    "abc1234",
				"repoUrl": "https://nope.example.com/x.git",
			},
		},
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on unknown repoUrl")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeRepoNotFound {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeRepoNotFound)
	}
	if total, _ := body["reposTotal"].(float64); total != 1 {
		t.Fatalf("reposTotal = %v, want 1", body["reposTotal"])
	}
	repos, ok := body["repos"].([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("repos = %v, want a 1-entry list", body["repos"])
	}
	row, ok := repos[0].(map[string]any)
	if !ok || row["id"] != repoIDs[0].String() {
		t.Fatalf("repos[0] = %v, want the project's declared repo %s", repos[0], repoIDs[0])
	}
}

// TestClose_InvalidHash: hash that doesn't match the lowercase-hex
// 7..64 regex → validation_error before the tx opens. Issue stays open.
func TestClose_InvalidHash(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "clcih", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "bad-hash", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcih")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "bad hash",
		"commits": []any{
			map[string]any{"hash": "ZZZ"}, // not lowercase hex
		},
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on invalid hash")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}

	// Issue did NOT close.
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM issues WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "open" {
		t.Fatalf("issue status = %s, want open (validation must short-circuit)", status)
	}
}

// TestClose_UppercaseHashRejected: the hash regex is lowercase-hex
// only. A 7-char uppercase-hex string matches the length window but
// must still be rejected so callers can't sneak past the contract by
// sending hashes that postgres would happily store but other tooling
// (Loki search, gitlab links) won't normalise.
func TestClose_UppercaseHashRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "clcuh", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "uc-hash", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcuh")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "uppercase hash",
		"commits": []any{
			map[string]any{"hash": "ABC1234"}, // valid length, uppercase hex
		},
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on uppercase hash")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}

	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM issues WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "open" {
		t.Fatalf("issue status = %s, want open", status)
	}
}

// TestClose_EmptyMessageNormalisedToNull: agents that pass `message: ""`
// must land NULL in issue_commits.message, not the empty string.
// Mirrors the trim+empty-as-nil pattern used for `author`, `url`,
// `repoUrl`. Locks in the contract so future drift produces a failing
// test rather than a silent wire-format change.
func TestClose_EmptyMessageNormalisedToNull(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "clcem", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "empty-msg", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcem")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "empty message",
		"commits": []any{
			map[string]any{
				"hash":    "abc1234",
				"message": "   ", // whitespace only — must collapse to NULL
				"author":  "",    // empty — must collapse to NULL too
			},
		},
	}
	res, err := handler(ctx, clo)
	if err != nil || res.IsError {
		t.Fatalf("close failed: %v %+v", err, res)
	}

	var msg, author *string
	if err := pool.QueryRow(context.Background(),
		`SELECT message, author FROM issue_commits WHERE issue_id = $1`, issueID).Scan(&msg, &author); err != nil {
		t.Fatalf("query commit fields: %v", err)
	}
	if msg != nil {
		t.Fatalf("message = %q, want NULL (empty string must normalise to nil)", *msg)
	}
	if author != nil {
		t.Fatalf("author = %q, want NULL", *author)
	}
}

// TestMaxCommitsConstantsMatch is a compile-time + runtime guard that
// the MCP-layer cap and the store-layer cap are the same value.
// The MCP constant is now `const maxCommitsPerCloseCall = issues.MaxCommitsPerClose`
// so drift is impossible — but if a future refactor ever de-aliases
// them, this test fails the moment the values diverge.
func TestMaxCommitsConstantsMatch(t *testing.T) {
	if maxCommitsPerCloseCall != issues.MaxCommitsPerClose {
		t.Fatalf("maxCommitsPerCloseCall = %d, issues.MaxCommitsPerClose = %d — must match",
			maxCommitsPerCloseCall, issues.MaxCommitsPerClose)
	}
}

// TestClose_IdempotentRecloseSameCommit: closing already-closed issue
// is a no-op for status (force=false fails with version_conflict).
// Instead we test idempotency at the commit-link level: re-close with
// force=true and the same commit hash → ON CONFLICT DO NOTHING, only
// one issue_commits row.
func TestClose_IdempotentRecloseSameCommit(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "clcic", 1)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "idempotent", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcic")

	commitArgs := []any{
		map[string]any{"hash": "feedface", "message": "first attempt"},
	}

	// First close.
	first := mcpgo.CallToolRequest{}
	first.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "first close",
		"commits":         commitArgs,
	}
	r1, err := handler(ctx, first)
	if err != nil || r1.IsError {
		t.Fatalf("first close failed: %v %+v", err, r1)
	}

	// Re-close with force=true to bypass the version + closed checks,
	// same commit hash. ON CONFLICT DO NOTHING means the row count
	// stays at 1.
	second := mcpgo.CallToolRequest{}
	second.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"closeOutcome":    "done",
		"reason":          "second close, same commit",
		"force":           true,
		"commits":         commitArgs,
	}
	r2, err := handler(ctx, second)
	if err != nil || r2.IsError {
		t.Fatalf("second close failed: %v %+v", err, r2)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_commits WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("query commits: %v", err)
	}
	if n != 1 {
		t.Fatalf("issue_commits rows = %d, want 1 (ON CONFLICT DO NOTHING)", n)
	}
}

// TestClose_NoCommitsBackCompat: omitting `commits` works exactly as
// before — close succeeds, no issue_commits rows, commitRefs is `[]`.
// Regression test for callers that don't know about commits[].
func TestClose_NoCommitsBackCompat(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clcnc")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "no-commits", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clcnc")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "no commits",
	}
	res, err := handler(ctx, clo)
	if err != nil || res.IsError {
		t.Fatalf("close failed: %v %+v", err, res)
	}
	body := extractToolJSON(t, res)
	commitRefs, _ := body["commitRefs"].([]any)
	if len(commitRefs) != 0 {
		t.Fatalf("commitRefs = %v, want []", commitRefs)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_commits WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("query commits: %v", err)
	}
	if n != 0 {
		t.Fatalf("issue_commits rows = %d, want 0", n)
	}
}

// TestCloseMany_MixedRepoLookups: batch with three rows — one resolves
// the only-repo case, one resolves via repoUrl, one fails repo_not_found.
// Verifies per-row commit linking + partial-success error mapping.
func TestCloseMany_MixedRepoLookups(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")

	// Single-repo project for row 0 + 2.
	pidA, prefA, repoA := seedProjectWithRepos(t, pool, userID, "clmra", 1)
	issA := prefA + "-1"
	seedIssue(t, pool, pidA, userID, issA, "row-A", "open", 2)
	issA2 := prefA + "-2"
	seedIssue(t, pool, pidA, userID, issA2, "row-A2", "open", 2)

	// Multi-repo project for row 1 (uses repoUrl to disambiguate).
	pidB, prefB, _ := seedProjectWithRepos(t, pool, userID, "clmrb", 2)
	issB := prefB + "-1"
	seedIssue(t, pool, pidB, userID, issB, "row-B", "open", 2)
	repoBURL := fmt.Sprintf("https://example.com/%s/repo2.git", prefB)

	store := issues.NewStore(pool)
	handler := handleCloseMany(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-clmr")

	items := []any{
		// Row 0 — single-repo, no hint.
		map[string]any{
			"id":              issA,
			"expectedVersion": float64(1),
			"closeOutcome":    "done",
			"reason":          "fix in repo A",
			"commits": []any{
				map[string]any{"hash": "abc1234"},
			},
		},
		// Row 1 — multi-repo, repoUrl-disambiguated.
		map[string]any{
			"id":              issB,
			"expectedVersion": float64(1),
			"closeOutcome":    "done",
			"reason":          "fix in repo B/repo2",
			"commits": []any{
				map[string]any{"hash": "deadbeef", "repoUrl": repoBURL},
			},
		},
		// Row 2 — single-repo project, but caller supplies an unknown URL → repo_not_found.
		map[string]any{
			"id":              issA2,
			"expectedVersion": float64(1),
			"closeOutcome":    "done",
			"reason":          "wrong url",
			"commits": []any{
				map[string]any{"hash": "abc1234", "repoUrl": "https://nope.example.com/x.git"},
			},
		},
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	results, _ := body["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results len = %d, want 3", len(results))
	}

	// Row 0: ok.
	row0, _ := results[0].(map[string]any)
	if ok, _ := row0["ok"].(bool); !ok {
		t.Fatalf("row 0 ok = false, want true; row=%+v", row0)
	}
	// Row 1: ok.
	row1, _ := results[1].(map[string]any)
	if ok, _ := row1["ok"].(bool); !ok {
		t.Fatalf("row 1 ok = false, want true; row=%+v", row1)
	}
	// Row 2: repo_not_found.
	row2, _ := results[2].(map[string]any)
	if ok, _ := row2["ok"].(bool); ok {
		t.Fatalf("row 2 ok = true, want false")
	}
	errObj, _ := row2["error"].(map[string]any)
	if errObj["code"] != ErrCodeRepoNotFound {
		t.Fatalf("row 2 error.code = %v, want %s", errObj["code"], ErrCodeRepoNotFound)
	}

	// Verify commits persisted for rows 0 & 1, none for row 2.
	var n0, n1, n2 int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_commits WHERE issue_id = $1`, issA).Scan(&n0)
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_commits WHERE issue_id = $1`, issB).Scan(&n1)
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_commits WHERE issue_id = $1`, issA2).Scan(&n2)
	if n0 != 1 || n1 != 1 || n2 != 0 {
		t.Fatalf("issue_commits counts: A=%d (want 1), B=%d (want 1), A2=%d (want 0)", n0, n1, n2)
	}

	// repoA[0] used for row 0 (sanity check).
	var got string
	_ = pool.QueryRow(context.Background(), `SELECT repo_id::text FROM issue_commits WHERE issue_id = $1`, issA).Scan(&got)
	if got != repoA[0].String() {
		t.Fatalf("issA repo_id = %s, want %s", got, repoA[0])
	}
}
