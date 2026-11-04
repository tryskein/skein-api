package handler

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"skein-api/internal/auth"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

func scopeSearchService(t *testing.T, pool *pgxpool.Pool, hybrid bool) *search.Service {
	t.Helper()
	var embedder *search.Embedder
	if hybrid {
		boundary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			vector := make([]float32, 1024)
			vector[0] = 1
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vector}}); err != nil {
				t.Errorf("embedding response: %v", err)
			}
		}))
		t.Cleanup(boundary.Close)
		var err error
		embedder, err = search.NewEmbedder(search.EmbedConfig{BaseURL: boundary.URL, Model: "fixture", Dim: 1024})
		if err != nil {
			t.Fatal(err)
		}
	}
	svc, err := search.NewService(search.NewStore(pool), embedder, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestSearchScope_FiltersBeforeCandidateLimit(t *testing.T) {
	for _, hybrid := range []bool{false, true} {
		name := "fts"
		if hybrid {
			name = "vector-only"
		}
		t.Run(name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			caller, callerName := testsupport.SeedUser(t, pool, "user")
			other, otherName := testsupport.SeedUser(t, pool, "user")
			pid, prefix := testsupport.SeedProject(t, pool, caller, "sclimit")
			testsupport.AddProjectMember(t, pool, pid, other, "member")
			term := "narrow" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			decoyTitle, wantedTitle := strings.Repeat(term+" ", 3), term
			if hybrid {
				// No lexical match: only the real vector CTE can retrieve the target.
				decoyTitle, wantedTitle = "closer semantic decoy", "more distant semantic target"
			}
			for i := 0; i < 120; i++ {
				testsupport.SeedIssue(t, pool, pid, caller, prefix+"-"+strconv.Itoa(100+i), decoyTitle, "open", 2)
			}
			wanted := prefix + "-999"
			testsupport.SeedIssue(t, pool, pid, other, wanted, wantedTitle, "open", 2)
			vector := make([]float32, 1024)
			vector[0] = 1
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,source='agent_autonomous',embedding=$3::halfvec WHERE project_id=$1`, pid, callerName, pgvector.NewHalfVector(vector)); err != nil {
				t.Fatal(err)
			}
			vector[1] = 1
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,source='agent_directed',accepted_at=now(),accepted_by=$3,embedding=$4::halfvec,previous_assignees=$5::uuid[] WHERE id=$1`, wanted, otherName, other, pgvector.NewHalfVector(vector), []uuid.UUID{caller}); err != nil {
				t.Fatal(err)
			}
			blocker := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-1000", "blocking target", "open", 2)
			testsupport.SeedDep(t, pool, wanted, blocker, "waits-for", caller)
			component := testsupport.SeedComponent(t, pool, pid, caller, "narrow-component", "")
			if _, err := pool.Exec(context.Background(), `INSERT INTO issue_components(issue_id,component_id) VALUES($1,$2)`, wanted, component); err != nil {
				t.Fatal(err)
			}
			svc := scopeSearchService(t, pool, hybrid)
			base := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "limit": {"100"}}
			status, body := scopeSearch(t, svc, caller, callerName, base)
			if status != http.StatusOK {
				t.Fatalf("unfiltered status=%d: %v", status, body)
			}
			unfiltered := scopeSearchIDs(t, body)
			if len(unfiltered) == 0 || slices.Contains(unfiltered, wanted) {
				t.Fatal("fixture must put the only qualifying issue behind the unfiltered candidate cap")
			}
			for _, selector := range []struct{ key, value string }{
				{"assignee", otherName}, {"acceptedAt", "not-null"}, {"isBlocked", "true"},
				{"componentId", component.String()}, {"origin", "human-driven"}, {"createdBy", "not-me"},
				{"createdBy", "me"},
			} {
				t.Run(selector.key, func(t *testing.T) {
					q := maps.Clone(base)
					q.Set(selector.key, selector.value)
					status, body := scopeSearch(t, svc, caller, callerName, q)
					if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), []string{wanted}) {
						t.Fatalf("filtered status=%d body=%v; want only %s beyond the old cap", status, body, wanted)
					}
					if body["degraded"] != !hybrid {
						t.Fatalf("degraded=%v; hybrid=%v", body["degraded"], hybrid)
					}
				})
			}
		})
	}
}

func TestSearchScope_FiltersBeforeIDLimit(t *testing.T) {
	for _, numeric := range []bool{false, true} {
		name := "prefix"
		if numeric {
			name = "numeric"
		}
		t.Run(name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			caller, username := testsupport.SeedUser(t, pool, "user")
			other, _ := testsupport.SeedUser(t, pool, "user")
			pid, prefix := testsupport.SeedProject(t, pool, caller, "scid")
			testsupport.AddProjectMember(t, pool, pid, other, "member")
			qtext := prefix + "-"
			if numeric {
				qtext = "7"
			}
			ids := make([]string, 0, 6)
			for i := 1; i <= 6; i++ {
				id := prefix + "-" + strconv.Itoa(i)
				if numeric {
					id += ".7"
				}
				ids = append(ids, testsupport.SeedIssue(t, pool, pid, caller, id, "unrelated title", "closed", 4))
			}
			wanted := ids[5]
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET source='agent_autonomous' WHERE project_id=$1`, pid); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET status='open',closed_at=NULL,closed_reason=NULL,close_outcome=NULL,priority=1,type='bug',source='agent_directed',assignee=$2,accepted_at=now(),accepted_by=$3 WHERE id=$1`, wanted, username, caller); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET created_by=$2 WHERE id=$1`, wanted, other); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), `INSERT INTO issue_labels(issue_id,label) VALUES($1,'narrow-label')`, wanted); err != nil {
				t.Fatal(err)
			}
			component := testsupport.SeedComponent(t, pool, pid, caller, "id-component", "")
			if _, err := pool.Exec(context.Background(), `INSERT INTO issue_components(issue_id,component_id) VALUES($1,$2)`, wanted, component); err != nil {
				t.Fatal(err)
			}
			blockerPID, blockerPrefix := testsupport.SeedProject(t, pool, caller, "scidblock")
			blocker := testsupport.SeedIssue(t, pool, blockerPID, caller, blockerPrefix+"-1", "blocking target", "open", 2)
			testsupport.SeedDep(t, pool, wanted, blocker, "blocks", caller)
			svc := scopeSearchService(t, pool, false)
			for _, selector := range []struct{ key, value string }{
				{"status", "open"}, {"priority", "1"}, {"type", "bug"}, {"label", "narrow-label"},
				{"source", "agent_directed"}, {"origin", "human-driven"}, {"assignee", "me"},
				{"acceptedAt", "not-null"}, {"componentId", component.String()}, {"isBlocked", "true"},
				{"createdBy", "not-me"},
			} {
				t.Run(selector.key, func(t *testing.T) {
					q := url.Values{"q": {qtext}, "project": {pid.String()}, "kind": {"issue"}, selector.key: {selector.value}}
					status, body := scopeSearch(t, svc, caller, username, q)
					if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), []string{wanted}) {
						t.Fatalf("filtered ID status=%d body=%v; want only sixth match %s", status, body, wanted)
					}
				})
			}
			// A previous assignee also reaches a qualifying sixth ID hit.
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,previous_assignees=$3::uuid[] WHERE id=$1`, wanted, "external docs team", []uuid.UUID{caller}); err != nil {
				t.Fatal(err)
			}
			q := url.Values{"q": {qtext}, "project": {pid.String()}, "kind": {"issue"}, "createdBy": {"me"}}
			status, body := scopeSearch(t, svc, caller, username, q)
			if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), []string{wanted}) {
				t.Fatalf("Delegated sixth ID hit: status=%d body=%v", status, body)
			}
			// The separate direct-jump endpoint still ignores column filters.
			req := httptest.NewRequest(http.MethodGet, "/api/v1/search/lookup-id?q="+url.QueryEscape(qtext)+"&project="+pid.String()+"&status=open&componentId="+component.String(), nil)
			ctx := auth.WithUserRole(auth.WithUserID(req.Context(), caller), "user")
			rec := httptest.NewRecorder()
			NewSearchHandler(svc).LookupID(rec, req.WithContext(ctx))
			body = nil
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || len(scopeSearchIDs(t, body)) != 5 || slices.Contains(scopeSearchIDs(t, body), wanted) {
				t.Fatalf("direct lookup changed its filter-agnostic first-five selection: %d %v", rec.Code, body)
			}
		})
	}
}

func TestSearchScope_InvalidSelectors(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	svc := scopeSearchService(t, pool, false)
	for _, tc := range []struct{ key, value, code string }{
		{"createdBy", "someone-else", "invalid_created_by"},
		{"acceptedAt", "false", "invalid_accepted_at"},
		{"isBlocked", "1", "invalid_isBlocked"},
		{"componentId", "not-a-uuid", "invalid_component_id"},
		{"origin", "human", "invalid_origin"},
		{"origin", ",", "invalid_origin"},
		{"origin", "human-driven,", "invalid_origin"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			status, body := scopeSearch(t, svc, caller, username, url.Values{"q": {"probe"}, tc.key: {tc.value}})
			if status != http.StatusBadRequest || body["error"] != tc.code {
				t.Fatalf("status=%d body=%v; want 400 %s", status, body, tc.code)
			}
		})
	}
}

func TestSearchScope_CursorIdentity(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "sccursor")
	term := "cursor" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for i := 1; i <= 4; i++ {
		testsupport.SeedIssue(t, pool, pid, caller, prefix+"-"+strconv.Itoa(i), term+" result", "open", 2)
	}
	svc := scopeSearchService(t, pool, false)
	base := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "limit": {"1"}}
	status, body := scopeSearch(t, svc, caller, username, base)
	cursor, ok := body["nextCursor"].(string)
	if status != http.StatusOK || !ok || cursor == "" || body["hasMore"] != true {
		t.Fatalf("fixture needs a continuation: status=%d body=%v", status, body)
	}
	for _, warm := range []bool{true, false} {
		for _, selector := range []struct{ key, value string }{
			{"createdBy", "not-me"},
			{"createdBy", "me"},
			{"assignee", "me"}, {"acceptedAt", "not-null"}, {"acceptedAt", "null"},
			{"isBlocked", "true"}, {"isBlocked", "false"}, {"componentId", uuid.NewString()},
			{"origin", "human-driven"},
		} {
			name := "warm/"
			if !warm {
				name = "cold/"
			}
			t.Run(name+selector.key+"="+selector.value, func(t *testing.T) {
				service := svc
				if !warm {
					service = scopeSearchService(t, pool, false)
				}
				q := maps.Clone(base)
				q.Set("cursor", cursor)
				q.Set(selector.key, selector.value)
				status, body := scopeSearch(t, service, caller, username, q)
				if status != http.StatusBadRequest || body["error"] != "invalid_cursor" {
					t.Fatalf("changed filter continued old search: status=%d body=%v", status, body)
				}
			})
		}
	}
}

func TestSearchScope_SnapshotCursorBindsEveryQueryField(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "scallfields")
	term := "allfields" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for i := 1; i <= 3; i++ {
		testsupport.SeedIssue(t, pool, pid, caller, prefix+"-"+strconv.Itoa(i), term+" task", "open", 2)
	}
	svc := scopeSearchService(t, pool, false)
	base := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "isBlocked": {"false"}, "limit": {"1"}}
	status, first := scopeSearch(t, svc, caller, username, base)
	cursor, ok := first["nextCursor"].(string)
	if status != http.StatusOK || !ok || cursor == "" {
		t.Fatalf("need a scoped snapshot: %d %v", status, first)
	}
	for _, field := range []struct{ key, value string }{
		{"q", "another query"}, {"project", uuid.NewString()}, {"space", "other-space"},
		{"kind", "issue,comment"}, {"status", "open"}, {"priority", "2"}, {"type", "task"},
		{"label", "release"}, {"source", "human"}, {"commentAuthorKind", "human"},
		{"assignee", "me"}, {"assignee", "another-user"}, {"createdBy", "me"}, {"createdBy", "not-me"},
		{"acceptedAt", "null"}, {"acceptedAt", "not-null"}, {"isBlocked", "true"},
		{"componentId", uuid.NewString()}, {"origin", "human-driven"},
	} {
		t.Run(field.key+"="+field.value, func(t *testing.T) {
			q := maps.Clone(base)
			q.Set("cursor", cursor)
			q.Set(field.key, field.value)
			status, body := scopeSearch(t, svc, caller, username, q)
			if status != http.StatusBadRequest || body["error"] != "invalid_cursor" {
				t.Fatalf("snapshot crossed query field %s: %d %v", field.key, status, body)
			}
		})
	}
}

func TestSearchScope_BlockedVisibilityAndCache(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	other, otherName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "schorizon")
	hiddenPID, hiddenPrefix := testsupport.SeedProject(t, pool, other, "schidden")
	testsupport.AddProjectMember(t, pool, pid, other, "member")
	testsupport.AddProjectMember(t, pool, hiddenPID, caller, "member")
	term := "horizon" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	issue := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-1", term+" selected issue", "open", 2)
	blocker := testsupport.SeedIssue(t, pool, hiddenPID, other, hiddenPrefix+"-1", "hidden blocking target", "open", 2)
	testsupport.SeedDep(t, pool, issue, blocker, "blocks", caller)
	svc := scopeSearchService(t, pool, false)
	q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "isBlocked": {"true"}}
	status, body := scopeSearch(t, svc, caller, username, q)
	if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), []string{issue}) {
		t.Fatalf("visible blocker: status=%d body=%v", status, body)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, hiddenPID, caller); err != nil {
		t.Fatal(err)
	}
	status, body = scopeSearch(t, svc, caller, username, q)
	if status != http.StatusOK || len(scopeSearchIDs(t, body)) != 0 {
		t.Fatalf("hidden blocker was counted or old cache answered: status=%d body=%v", status, body)
	}
	q.Set("isBlocked", "false")
	status, body = scopeSearch(t, svc, caller, username, q)
	if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), []string{issue}) {
		t.Fatalf("unblocked reader view: status=%d body=%v", status, body)
	}
	q.Set("isBlocked", "true")
	status, body = scopeSearch(t, svc, other, otherName, q)
	if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), []string{issue}) {
		t.Fatalf("reader who sees both projects: status=%d body=%v", status, body)
	}
	q.Set("project", hiddenPID.String())
	q.Del("isBlocked")
	q.Set("q", "hidden")
	status, body = scopeSearch(t, svc, caller, username, q)
	if status != http.StatusOK || len(scopeSearchIDs(t, body)) != 0 {
		t.Fatalf("project narrowing bypassed caller access: status=%d body=%v", status, body)
	}
}

func TestSearchScope_IssueOnlySelectors(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "sckinds")
	term := "kinds" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	issue := testsupport.SeedIssue(t, pool, pid, caller, prefix+"-1", term+" issue", "open", 2)
	comment := testsupport.SeedComment(t, pool, issue, caller, term+" comment")
	component := testsupport.SeedComponent(t, pool, pid, caller, term, term+" component")
	if _, err := pool.Exec(context.Background(), `UPDATE projects SET description=$2 WHERE id=$1`, pid, term); err != nil {
		t.Fatal(err)
	}
	svc := scopeSearchService(t, pool, false)
	wantOtherKinds := []string{pid.String(), comment.String(), component.String()}
	slices.Sort(wantOtherKinds)
	for _, selector := range []struct{ key, value string }{
		{"assignee", "unmatched-user"}, {"acceptedAt", "not-null"}, {"isBlocked", "true"},
		{"componentId", uuid.NewString()}, {"origin", "agent-autonomous"}, {"createdBy", "not-me"},
	} {
		t.Run(selector.key, func(t *testing.T) {
			q := url.Values{"q": {term}, "project": {pid.String()}, selector.key: {selector.value}}
			status, body := scopeSearch(t, svc, caller, username, q)
			if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), wantOtherKinds) {
				t.Fatalf("issue filter changed other kinds: status=%d body=%v want=%v", status, body, wantOtherKinds)
			}
		})
	}
}

func TestSearchScope_OnMe(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, username := testsupport.SeedUser(t, pool, "user")
	other, otherName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, caller, "sconme")
	testsupport.AddProjectMember(t, pool, pid, other, "member")
	var email string
	if err := pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id=$1`, caller).Scan(&email); err != nil {
		t.Fatal(err)
	}
	term := "onme" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	handle := func(value string) *string { return &value }
	var want []string
	for i, row := range []struct {
		creator  uuid.UUID
		assignee *string
		accepted bool
	}{
		{other, handle(username), true},
		{other, handle(strings.ToUpper(email)), true},
		{other, handle(strings.ToUpper(caller.String())), true},
		{other, handle(username), false},
		{caller, handle(username), true},
		{other, handle(otherName), true},
		{other, nil, false},
	} {
		id := testsupport.SeedIssue(t, pool, pid, row.creator, prefix+"-"+strconv.Itoa(i+1), term+" task", "open", 2)
		var acceptedBy any
		if row.accepted {
			acceptedBy = caller
			if row.assignee != nil && *row.assignee == otherName {
				acceptedBy = other
			}
		}
		if _, err := pool.Exec(context.Background(), `UPDATE issues SET assignee=$2,accepted_at=CASE WHEN $3::uuid IS NOT NULL THEN now() ELSE NULL END,accepted_by=$3 WHERE id=$1`, id, row.assignee, acceptedBy); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			want = append(want, id)
		}
	}
	svc := scopeSearchService(t, pool, false)
	q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "assignee": {"me"}, "createdBy": {"not-me"}, "acceptedAt": {"not-null"}}
	status, body := scopeSearch(t, svc, caller, username, q)
	slices.Sort(want)
	if status != http.StatusOK || !slices.Equal(scopeSearchIDs(t, body), want) {
		t.Fatalf("On me: status=%d body=%v, want %v", status, body, want)
	}
}

func scopeSearch(t *testing.T, svc *search.Service, caller uuid.UUID, username string, query url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?"+query.Encode(), nil)
	ctx := auth.WithUsername(auth.WithUserRole(auth.WithUserID(req.Context(), caller), "user"), username)
	rec := httptest.NewRecorder()
	NewSearchHandler(svc).Search(rec, req.WithContext(ctx))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rec.Code, body
}

func scopeSearchIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items is not an array: %v", body)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("invalid item: %v", item)
		}
		id, ok := row["id"].(string)
		if !ok {
			t.Fatalf("invalid item identifier: %v", row)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// The task-column controls select a set before candidate ranking and LIMIT.
// Exercise the public REST boundary with real PostgreSQL on both retrievers.
func TestSearchScope_Controls(t *testing.T) {
	for _, hybrid := range []bool{false, true} {
		name := "fts"
		if hybrid {
			name = "hybrid"
		}
		t.Run(name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			caller, callerName := testsupport.SeedUser(t, pool, "user")
			other, otherName := testsupport.SeedUser(t, pool, "user")
			pid, prefix := testsupport.SeedProject(t, pool, caller, "scscope")
			testsupport.AddProjectMember(t, pool, pid, other, "member")
			term := "scope" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			a, b, c, blocker := prefix+"-1", prefix+"-2", prefix+"-3", prefix+"-4"
			testsupport.SeedIssue(t, pool, pid, caller, a, term+" first", "open", 2)
			testsupport.SeedIssue(t, pool, pid, other, b, term+" second", "open", 2)
			testsupport.SeedIssue(t, pool, pid, other, c, term+" third", "open", 2)
			testsupport.SeedIssue(t, pool, pid, caller, blocker, "visible blocking target", "open", 2)
			testsupport.SeedDep(t, pool, a, blocker, "blocks", caller)
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET
				assignee=CASE WHEN id=$2 THEN $4 ELSE $5 END,
 source=CASE WHEN id=$1 THEN 'human' WHEN id=$2 THEN 'agent_directed' ELSE 'agent_autonomous' END,
 accepted_at=CASE WHEN id=$1 THEN now() ELSE NULL END,
 accepted_by=CASE WHEN id=$1 THEN $6::uuid ELSE NULL END
				WHERE id IN ($1,$2,$3)`, a, b, c, callerName, otherName, other); err != nil {
				t.Fatal(err)
			}
			component := testsupport.SeedComponent(t, pool, pid, caller, "scope-component", "")
			if _, err := pool.Exec(context.Background(), `INSERT INTO issue_components(issue_id,component_id) VALUES($1,$2)`, a, component); err != nil {
				t.Fatal(err)
			}
			vector := make([]float32, 1024)
			vector[0] = 1
			if _, err := pool.Exec(context.Background(), `UPDATE issues SET embedding=$2::halfvec WHERE id=ANY($1::text[])`, []string{a, b, c}, pgvector.NewHalfVector(vector)); err != nil {
				t.Fatal(err)
			}
			svc := scopeSearchService(t, pool, hybrid)
			for _, tc := range []struct {
				name string
				args map[string]string
				want []string
			}{
				{"none", nil, []string{a, b, c}},
				{"delegated", map[string]string{"createdBy": "me"}, []string{a}},
				{"assignee", map[string]string{"assignee": strings.ToUpper(otherName)}, []string{a, c}},
				{"assignee-me", map[string]string{"assignee": "me"}, []string{b}},
				{"accepted", map[string]string{"acceptedAt": "not-null"}, []string{a}},
				{"unaccepted", map[string]string{"acceptedAt": "null"}, []string{b, c}},
				{"blocked", map[string]string{"isBlocked": "true"}, []string{a}},
				{"unblocked", map[string]string{"isBlocked": "false"}, []string{b, c}},
				{"component", map[string]string{"componentId": component.String()}, []string{a}},
				{"human-driven", map[string]string{"origin": "human-driven"}, []string{a, b}},
				{"origin-source-intersection", map[string]string{"origin": "human-driven", "source": "agent_directed"}, []string{b}},
				{"origin-source-disjoint", map[string]string{"origin": "human-driven", "source": "agent_autonomous"}, nil},
				{"autonomous", map[string]string{"origin": "agent-autonomous"}, []string{c}},
				{"not-created-by-me", map[string]string{"createdBy": "not-me"}, []string{b, c}},
				{"combined", map[string]string{"assignee": otherName, "acceptedAt": "not-null", "isBlocked": "true", "componentId": component.String(), "origin": "human-driven"}, []string{a}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					q := url.Values{"q": {term}, "project": {pid.String()}, "kind": {"issue"}, "limit": {"100"}}
					for key, value := range tc.args {
						q.Set(key, value)
					}
					status, body := scopeSearch(t, svc, caller, callerName, q)
					if status != http.StatusOK {
						t.Fatalf("status=%d: %v", status, body)
					}
					got, want := scopeSearchIDs(t, body), slices.Clone(tc.want)
					slices.Sort(want)
					if !slices.Equal(got, want) {
						t.Fatalf("issue IDs=%v, want %v", got, want)
					}
					if body["degraded"] != !hybrid {
						t.Fatalf("degraded=%v; hybrid=%v", body["degraded"], hybrid)
					}
				})
			}
		})
	}
}
