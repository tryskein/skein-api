package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// Search must select the existing Delegated set, including the historical
// branch and its dismissal rules, identically to the three ordinary lists.
func TestSearchScope_DelegatedParity(t *testing.T) {
	for _, hybrid := range []bool{false, true} {
		name := "fts"
		if hybrid {
			name = "hybrid"
		}
		t.Run(name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			caller, username := testsupport.SeedUser(t, pool, "user")
			creator, _ := testsupport.SeedUser(t, pool, "user")
			delegate, delegateName := testsupport.SeedUser(t, pool, "user")
			pid, prefix := testsupport.SeedProject(t, pool, caller, "scdelegate")
			testsupport.AddProjectMember(t, pool, pid, creator, "member")
			testsupport.AddProjectMember(t, pool, pid, delegate, "member")
			var email string
			if err := pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id=$1`, caller).Scan(&email); err != nil {
				t.Fatal(err)
			}
			term := "delegated" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			handle := func(value string) *string { return &value }
			var want []string
			for i, row := range []struct {
				creator             uuid.UUID
				assignee            *string
				previous, dismissed bool
				included            bool
			}{
				{caller, handle(delegateName), false, false, true},
				{creator, handle(delegateName), true, false, true},
				{creator, handle(delegateName), true, true, false},
				{caller, handle(username), false, false, false},
				{caller, handle(strings.ToUpper(email)), false, false, false},
				{caller, handle(strings.ToUpper(caller.String())), false, false, false},
				{caller, nil, false, false, false},
				{caller, handle(delegateName), false, true, true},
				{creator, handle(delegateName), false, false, false},
				{creator, handle(username), true, false, false},
				{creator, nil, true, false, false},
				{creator, handle("external docs team"), true, false, true},
			} {
				id := testsupport.SeedIssue(t, pool, pid, row.creator, prefix+"-"+strconv.Itoa(i+1), term+" task", "open", 2)
				previous, dismissed := []uuid.UUID{}, []uuid.UUID{}
				if row.previous {
					previous = []uuid.UUID{caller}
				}
				if row.dismissed {
					dismissed = []uuid.UUID{caller}
				}
				if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,previous_assignees=$3::uuid[],dismissed_by=$4::uuid[] WHERE id=$1`, id, row.assignee, previous, dismissed); err != nil {
					t.Fatal(err)
				}
				if row.included {
					want = append(want, id)
				}
			}
			slices.Sort(want)
			vector := make([]float32, 1024)
			vector[0] = 1
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET embedding=$2::halfvec WHERE project_id=$1`, pid, pgvector.NewHalfVector(vector)); err != nil {
				t.Fatal(err)
			}
			store := issues.NewStore(pool)
			ctx := auth.WithUsername(auth.WithUserRole(auth.WithUserID(context.Background(), caller), "user"), username)
			queries := []struct {
				name string
				read func() (*issues.ListResult, error)
			}{
				{"project", func() (*issues.ListResult, error) {
					return store.ListIssues(ctx, issues.ListIssuesArgs{ProjectID: pid, Caller: &caller, CallerUsername: username, CreatedBy: &caller, Statuses: []string{"open"}, Text: &term})
				}},
				{"all-projects", func() (*issues.ListResult, error) {
					return store.ListAllIssues(ctx, issues.ListAllIssuesArgs{Caller: caller, Scope: authz.ScopeFor(ctx, caller), CallerUsername: username, CreatedBy: &caller, Status: "open", Q: term})
				}},
				{"per-user", func() (*issues.ListResult, error) {
					return store.ListByUser(ctx, issues.ListByUserArgs{UserID: caller, Scope: authz.ScopeFor(ctx, caller), CallerUsername: username, Mode: issues.ModeCreatedBy, Statuses: []string{"open"}})
				}},
			}
			for _, query := range queries {
				t.Run(query.name, func(t *testing.T) {
					result, err := query.read()
					if err != nil {
						t.Fatal(err)
					}
					ids := make([]string, 0, len(result.Items))
					for _, row := range result.Items {
						ids = append(ids, row.ID)
					}
					slices.Sort(ids)
					if !slices.Equal(ids, want) {
						t.Fatalf("%s list=%v, want %v", query.name, ids, want)
					}
				})
			}
			svc := scopeSearchService(t, pool, hybrid)
			q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "createdBy": {"me"}, "limit": {"100"}}
			status, body := scopeSearch(t, svc, caller, username, q)
			if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), want) {
				t.Fatalf("search=%d %v; want existing Delegated %v", status, body, want)
			}
			// Missing principal username retains the ordinary list's strict
			// fallback; it must not share the full-lens pool's cache identity.
			strict := []string{prefix + "-1", prefix + "-4", prefix + "-5", prefix + "-6", prefix + "-7", prefix + "-8"}
			slices.Sort(strict)
			status, body = scopeSearch(t, svc, caller, "", q)
			if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), strict) {
				t.Fatalf("missing-name fallback=%d %v; want %v", status, body, strict)
			}
			q.Set("limit", "1")
			status, body = scopeSearch(t, svc, caller, username, q)
			cursor, ok := body["nextCursor"].(string)
			if status != http.StatusOK || !ok || cursor == "" {
				t.Fatalf("need a personal cursor: %d %v", status, body)
			}
			q.Set("cursor", cursor)
			for _, principal := range []struct {
				id   uuid.UUID
				name string
			}{{caller, ""}, {delegate, delegateName}} {
				status, body = scopeSearch(t, svc, principal.id, principal.name, q)
				if status != http.StatusBadRequest || body["error"] != "invalid_cursor" {
					t.Fatalf("personal cursor crossed caller/identity: %d %v", status, body)
				}
			}
			q.Del("cursor")
			status, body = scopeSearch(t, svc, delegate, delegateName, q)
			if status != http.StatusOK || len(scopeSearchIDs(t, body)) != 0 {
				t.Fatalf("another caller inherited the personal pool: %d %v", status, body)
			}
		})
	}
}

func TestSearchScope_RefreshAfterMembershipChange(t *testing.T) {
	for _, selector := range []string{"delegated", "not-creator", "assignee", "acceptedAt", "blocked", "component", "origin"} {
		t.Run(selector, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			caller, username := testsupport.SeedUser(t, pool, "user")
			other, otherName := testsupport.SeedUser(t, pool, "user")
			pid, prefix := testsupport.SeedProject(t, pool, caller, "scfresh")
			testsupport.AddProjectMember(t, pool, pid, other, "member")
			term := "fresh" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			id := testsupport.SeedIssue(t, pool, pid, other, prefix+"-1", term+" selected", "open", 2)
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,accepted_at=now(),accepted_by=$3,previous_assignees=$4::uuid[] WHERE id=$1`, id, otherName, other, []uuid.UUID{caller}); err != nil {
				t.Fatal(err)
			}
			blocker := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-2", "blocking target", "open", 2)
			testsupport.SeedDep(t, pool, id, blocker, "blocks", caller)
			component := testsupport.SeedComponent(t, pool, pid, caller, "fresh-component", "")
			if _, err := pool.Exec(context.Background(), `INSERT INTO issue_components(issue_id,component_id) VALUES($1,$2)`, id, component); err != nil {
				t.Fatal(err)
			}
			q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}}
			var change string
			var args []any
			switch selector {
			case "delegated":
				q.Set("createdBy", "me")
				change, args = `UPDATE issues SET previous_assignees='{}' WHERE id=$1`, []any{id}
			case "not-creator":
				q.Set("createdBy", "not-me")
				change, args = `UPDATE issues SET created_by=$2 WHERE id=$1`, []any{id, caller}
			case "assignee":
				q.Set("assignee", otherName)
				change, args = `UPDATE issues SET assignee=$2,accepted_at=NULL,accepted_by=NULL WHERE id=$1`, []any{id, username}
			case "acceptedAt":
				q.Set("acceptedAt", "not-null")
				change, args = `UPDATE issues SET accepted_at=NULL,accepted_by=NULL WHERE id=$1`, []any{id}
			case "blocked":
				q.Set("isBlocked", "true")
				change, args = `DELETE FROM dependencies WHERE from_id=$1 AND to_id=$2`, []any{id, blocker}
			case "component":
				q.Set("componentId", component.String())
				change, args = `DELETE FROM issue_components WHERE issue_id=$1 AND component_id=$2`, []any{id, component}
			case "origin":
				q.Set("origin", "human-driven")
				change, args = `UPDATE issues SET source='agent_autonomous' WHERE id=$1`, []any{id}
			}
			svc := scopeSearchService(t, pool, false)
			status, body := scopeSearch(t, svc, caller, username, q)
			if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), []string{id}) {
				t.Fatalf("fixture is not initially selected: %d %v", status, body)
			}
			if _, err := pool.Exec(context.Background(), change, args...); err != nil {
				t.Fatal(err)
			}
			status, body = scopeSearch(t, svc, caller, username, q)
			if status != http.StatusOK || len(scopeSearchIDs(t, body)) != 0 {
				t.Fatalf("cursorless scope returned stale membership after mutation: %d %v", status, body)
			}
		})
	}
}

func TestSearchScope_FreshFirstPageKeepsIssuedCursorPool(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	other, otherName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "scsnapshot")
	testsupport.AddProjectMember(t, pool, pid, other, "member")
	term := "snapshot" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	var original []string
	for i := 1; i <= 3; i++ {
		id := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-"+strconv.Itoa(i), term+" task", "open", 2)
		if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2 WHERE id=$1`, id, otherName); err != nil {
			t.Fatal(err)
		}
		original = append(original, id)
	}
	svc := scopeSearchService(t, pool, false)
	q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "createdBy": {"me"}, "limit": {"1"}}
	status, first := scopeSearch(t, svc, caller, username, q)
	firstIDs := scopeSearchIDs(t, first)
	cursor, ok := first["nextCursor"].(string)
	if status != http.StatusOK || len(firstIDs) != 1 || !ok || cursor == "" {
		t.Fatalf("need first page and continuation: %d %v", status, first)
	}
	var removed string
	var oldRemainder []string
	for _, id := range original {
		if id != firstIDs[0] {
			oldRemainder = append(oldRemainder, id)
			removed = id
		}
	}
	if removed == "" || len(oldRemainder) != 2 {
		t.Fatal("fixture needs two remaining original hits")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,accepted_at=now(),accepted_by=$3 WHERE id=$1`, removed, username, caller); err != nil {
		t.Fatal(err)
	}
	q.Set("limit", "100")
	status, fresh := scopeSearch(t, svc, caller, username, q)
	wantFresh := make([]string, 0, 2)
	for _, id := range original {
		if id != removed {
			wantFresh = append(wantFresh, id)
		}
	}
	slices.Sort(wantFresh)
	if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, fresh), wantFresh) {
		t.Fatalf("new first page did not reflect current membership: %d %v", status, fresh)
	}
	q.Set("cursor", cursor)
	status, continued := scopeSearch(t, svc, caller, username, q)
	slices.Sort(oldRemainder)
	if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, continued), oldRemainder) {
		t.Fatalf("new first page replaced an already-issued cursor pool: %d %v, want %v", status, continued, oldRemainder)
	}
	status, cold := scopeSearch(t, scopeSearchService(t, pool, false), caller, username, q)
	if status != http.StatusBadRequest || cold["error"] != "invalid_cursor" {
		t.Fatalf("missing scoped generation rebuilt an offset: %d %v", status, cold)
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ key, value string }{
		{"s", uuid.NewString()}, {"s", "not-a-uuid"}, {"s", uuid.Nil.String()},
		{"m", "hybrid"}, {"s", ""},
	} {
		t.Run("tamper-"+change.key+"="+change.value, func(t *testing.T) {
			forged := maps.Clone(payload)
			forged[change.key] = change.value
			bytes, err := json.Marshal(forged)
			if err != nil {
				t.Fatal(err)
			}
			query := maps.Clone(q)
			query.Set("cursor", base64.RawURLEncoding.EncodeToString(bytes))
			status, body := scopeSearch(t, svc, caller, username, query)
			if status != http.StatusBadRequest || body["error"] != "invalid_cursor" {
				t.Fatalf("tampered generation/ranking was accepted: %d %v", status, body)
			}
		})
	}
	q.Set("q", "another query")
	status, changedQuery := scopeSearch(t, svc, caller, username, q)
	if status != http.StatusBadRequest || changedQuery["error"] != "invalid_cursor" {
		t.Fatalf("scoped cursor crossed query: %d %v", status, changedQuery)
	}
	q.Set("q", term)
	testsupport.SeedProject(t, pool, caller, "scnewvisibility")
	status, changedVisibility := scopeSearch(t, svc, caller, username, q)
	if status != http.StatusBadRequest || changedVisibility["error"] != "invalid_cursor" {
		t.Fatalf("scoped cursor crossed visibility: %d %v", status, changedVisibility)
	}
}

func TestSearchScope_SinglePageRefreshDoesNotEvictLegacyPool(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "scsingle")
	term := "single" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for i := 1; i <= 3; i++ {
		testsupport.SeedIssue(t, pool, pid, caller, prefix+"-"+strconv.Itoa(i), term+" task", "open", 2)
	}
	component := testsupport.SeedComponent(t, pool, pid, caller, "single-component", "")
	if _, err := pool.Exec(context.Background(), `INSERT INTO issue_components(issue_id,component_id) VALUES($1,$2)`, prefix+"-1", component); err != nil {
		t.Fatal(err)
	}
	svc, err := search.NewService(search.NewStore(pool), nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "limit": {"1"}}
	status, first := scopeSearch(t, svc, caller, username, q)
	cursor, ok := first["nextCursor"].(string)
	if status != http.StatusOK || !ok || cursor == "" {
		t.Fatalf("need legacy-compatible warm pool: %d %v", status, first)
	}
	key, offset, err := search.DecodeCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := search.EncodeCursor(key, offset)
	if err != nil {
		t.Fatal(err)
	}
	single := maps.Clone(q)
	single.Set("componentId", component.String())
	single.Set("limit", "100")
	// The policy permits 256 pools. Completed single-page refreshes must
	// not retain unreachable generations and evict a live continuation.
	for i := 0; i < 257; i++ {
		status, body := scopeSearch(t, svc, caller, username, single)
		if status != http.StatusOK || len(scopeSearchIDs(t, body)) != 1 || body["hasMore"] != false {
			t.Fatalf("single-page refresh: %d %v", status, body)
		}
	}
	q.Set("cursor", legacy)
	q.Set("limit", "100")
	status, continued := scopeSearch(t, svc, caller, username, q)
	if status != http.StatusOK || len(scopeSearchIDs(t, continued)) != 2 {
		t.Fatalf("single-page orphan generations evicted the legacy pool: %d %v", status, continued)
	}
}

func TestSearchScope_CommentOnlyKeepsLegacyContinuation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "sccomments")
	id := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-1", "carrier", "open", 2)
	term := "comment" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for i := 0; i < 2; i++ {
		testsupport.SeedComment(t, pool, id, caller, term+" body")
	}
	q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"comment"}, "componentId": {uuid.NewString()}, "limit": {"1"}}
	status, first := scopeSearch(t, scopeSearchService(t, pool, false), caller, username, q)
	cursor, ok := first["nextCursor"].(string)
	if status != http.StatusOK || !ok || cursor == "" {
		t.Fatalf("need comment continuation: %d %v", status, first)
	}
	q.Set("cursor", cursor)
	status, next := scopeSearch(t, scopeSearchService(t, pool, false), caller, username, q)
	if status != http.StatusOK || len(scopeSearchIDs(t, next)) != 1 {
		t.Fatalf("irrelevant issue filter changed comment-only cold continuation: %d %v", status, next)
	}
}

func TestSearchScope_PagedGenerationsDoNotEvictLegacyPool(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	other, otherName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "scpaged")
	testsupport.AddProjectMember(t, pool, pid, other, "member")
	term := "paged" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for i := 1; i <= 3; i++ {
		id := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-"+strconv.Itoa(i), term+" task", "open", 2)
		if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2 WHERE id=$1`, id, otherName); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := search.NewService(search.NewStore(pool), nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "limit": {"1"}}
	status, first := scopeSearch(t, svc, caller, username, base)
	cursor, ok := first["nextCursor"].(string)
	if status != http.StatusOK || !ok || cursor == "" {
		t.Fatalf("need legacy warm pool: %d %v", status, first)
	}
	key, offset, err := search.DecodeCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := search.EncodeCursor(key, offset)
	if err != nil {
		t.Fatal(err)
	}
	scoped := maps.Clone(base)
	scoped.Set("createdBy", "me")
	var oldest string
	for i := 0; i < 257; i++ {
		status, body := scopeSearch(t, svc, caller, username, scoped)
		current, ok := body["nextCursor"].(string)
		if status != http.StatusOK || !ok || current == "" {
			t.Fatalf("paged scoped refresh: %d %v", status, body)
		}
		if i == 0 {
			oldest = current
		}
	}
	base.Set("cursor", legacy)
	base.Set("limit", "100")
	status, continued := scopeSearch(t, svc, caller, username, base)
	if status != http.StatusOK || len(scopeSearchIDs(t, continued)) != 2 {
		t.Fatalf("scoped pressure evicted the legacy namespace: %d %v", status, continued)
	}
	scoped.Set("cursor", oldest)
	status, evicted := scopeSearch(t, svc, caller, username, scoped)
	if status != http.StatusBadRequest || evicted["error"] != "invalid_cursor" {
		t.Fatalf("scoped namespace did not evict its oldest bounded generation: %d %v", status, evicted)
	}
}

func TestSearchScope_CursorCannotCrossDelimiterEquivalentFilters(t *testing.T) {
	for _, pair := range []string{"status-assignee", "label-acceptance"} {
		t.Run(pair, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			caller, username := testsupport.SeedUser(t, pool, "user")
			other, otherName := testsupport.SeedUser(t, pool, "user")
			pid, prefix := testsupport.SeedProject(t, pool, caller, "scbinding")
			testsupport.AddProjectMember(t, pool, pid, other, "member")
			term := "binding" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			for i := 1; i <= 3; i++ {
				id := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-"+strconv.Itoa(i), term+" task", "open", 2)
				if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,accepted_at=now(),accepted_by=$3 WHERE id=$1`, id, otherName, other); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(context.Background(), `INSERT INTO issue_labels(issue_id,label) VALUES($1,'release')`, id); err != nil {
					t.Fatal(err)
				}
			}
			svc := scopeSearchService(t, pool, false)
			valid := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "isBlocked": {"false"}, "limit": {"1"}}
			if pair == "status-assignee" {
				valid.Set("status", "open")
				valid.Set("assignee", otherName)
			} else {
				valid.Set("label", "release")
				valid.Set("acceptedAt", "not-null")
			}
			status, body := scopeSearch(t, svc, caller, username, valid)
			cursor, ok := body["nextCursor"].(string)
			if status != http.StatusOK || !ok || cursor == "" {
				t.Fatalf("fixture needs a scoped continuation: %d %v", status, body)
			}
			otherQuery := maps.Clone(valid)
			if pair == "status-assignee" {
				otherQuery.Del("assignee")
				otherQuery.Set("status", "open|a="+strconv.Quote(otherName))
			} else {
				otherQuery.Del("acceptedAt")
				otherQuery.Set("label", "release|accepted=not-null")
			}
			status, empty := scopeSearch(t, svc, caller, username, otherQuery)
			if status != http.StatusOK || len(scopeSearchIDs(t, empty)) != 0 {
				t.Fatalf("different selection must match nothing: %d %v", status, empty)
			}
			otherQuery.Set("cursor", cursor)
			status, replayed := scopeSearch(t, svc, caller, username, otherQuery)
			if status != http.StatusBadRequest || replayed["error"] != "invalid_cursor" {
				t.Fatalf("cursor replay crossed selections: %d %v", status, replayed)
			}
		})
	}
}
