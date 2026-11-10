package handler

// source_api_filter_test.go — test matrix row 21 (the exact filter on
// /ui/v1). Reference — the spec, written before the code: source=api and the
// comment author kind api are accepted by the search parameters (no 400
// invalid_source / invalid_comment_author_kind) and return only api records.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

func TestSearchREST_SourceAPIFilters(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, caller, "rsapi")
	svc := newRESTSearchService(t, pool)
	tok := "tok" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	ctx := context.Background()

	for i, src := range []string{issues.SourceHuman, issues.SourceAPI} {
		id := prefix + "-" + string(rune('1'+i))
		testsupport.SeedIssue(t, pool, projID, caller, id, "issue "+tok+" "+src, "open", 2)
		if _, err := pool.Exec(ctx, `UPDATE issues SET source = $2 WHERE id = $1`, id, src); err != nil {
			t.Fatal(err)
		}
	}
	host := prefix + "-9"
	testsupport.SeedIssue(t, pool, projID, caller, host, "comment host", "open", 2)
	for _, kind := range []string{issues.CommentAuthorKindHuman, issues.CommentAuthorKindAPI} {
		id := testsupport.SeedComment(t, pool, host, caller, "note "+tok+" "+kind)
		if _, err := pool.Exec(ctx, `UPDATE comments SET author_kind = $2 WHERE id = $1`, id, kind); err != nil {
			t.Fatal(err)
		}
	}

	items := func(t *testing.T, query string) []map[string]any {
		t.Helper()
		status, body := callRESTSearchStatus(t, svc, caller, "user", "q="+tok+"&project="+projID.String()+"&"+query)
		if status != http.StatusOK {
			t.Fatalf("GET /search?%s = %d, want 200: %v", query, status, body)
		}
		raw, _ := body["items"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, r := range raw {
			out = append(out, r.(map[string]any))
		}
		return out
	}

	t.Run("source=api returns only the api issue", func(t *testing.T) {
		got := items(t, "kind=issue&source=api")
		if len(got) != 1 || got[0]["id"] != prefix+"-2" {
			t.Errorf("items %v, want only %s-2", got, prefix)
		}
	})
	t.Run("commentAuthorKind=api returns only the api comment", func(t *testing.T) {
		got := items(t, "kind=comment&commentAuthorKind=api")
		if len(got) != 1 {
			t.Fatalf("items %v, want one api comment", got)
		}
		var kind string
		if err := pool.QueryRow(ctx, `SELECT author_kind FROM comments WHERE id = $1`, got[0]["id"]).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != issues.CommentAuthorKindAPI {
			t.Errorf("returned comment author_kind %q, want api", kind)
		}
	})
}
