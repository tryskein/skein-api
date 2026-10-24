// dashboard_test — integration tests for the sk-17 dashboard handler.
//
// Focus is on the wire shape + visibility filter + error paths around
// the store; the store's per-metric correctness is covered exhaustively
// in internal/dashboard/store_test.go. Test seeds run against the live
// TEST_DATABASE_URL pool and clean up via the same chain as the rest of
// internal/handler/*_test.go.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dashboard"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// newDashboardTestPool returns a pool against TEST_DATABASE_URL or
// skips when the env var is unset.
func newDashboardTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedDashboardUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "dh" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'unused', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, role); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

func seedDashboardProject(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	prefix := "dhp" + strings.ReplaceAll(id.String()[:6], "-", "")
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      created_by, created_at, updated_at, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, $4, now(), now(), $5)`,
		id, prefix, prefix, ownerID, seedSpaceID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, ownerID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0) ON CONFLICT (project_id) DO NOTHING`, id); err != nil {
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
	return id, prefix
}

func seedDashboardIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, source string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, $4, NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $5, now(), now(), '{}'::jsonb)`,
		idStr, projectID, idStr, source, creator); err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

func seedDashboardEvent(t *testing.T, pool *pgxpool.Pool, eventType, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, actorType string, occurredAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, $2, $3, $4, $5, 'issue', $6, $7, '{}'::jsonb)`,
		occurredAt, eventType, actor, actorUserID, actorType, issueID, projectID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

func dashboardRouter(h *DashboardHandler) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/v1/dashboard/scoring", h.Scoring)
	r.Get("/api/v1/dashboard/feed", h.Feed)
	r.Get("/api/v1/dashboard/projects", h.Projects)
	r.Get("/api/v1/dashboard/in-progress", h.InProgress)
	return r
}

func callDashboard(t *testing.T, h *DashboardHandler, target string, userID uuid.UUID, role string) (*http.Response, []byte) {
	t.Helper()
	r := dashboardRouter(h)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, rec.Body.Bytes()
}

// TestDashboard_Scoring_VisibilityFilter — a non-admin caller in
// project A should NOT see events from project B.
// TestDashboard_Scoring_GlobalWarRoom — /dashboard/scoring is a global
// war-room surface: a non-admin caller's leaderboard/metrics count
// activity across EVERY project, including ones they're not a member of
// (counts carry only usernames + numbers, no titles, so nothing leaks).
func TestDashboard_Scoring_GlobalWarRoom(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	other, otherUname := seedDashboardUser(t, pool, "user")
	mine, _ := seedDashboardProject(t, pool, caller)
	theirs, _ := seedDashboardProject(t, pool, other)

	// The /dashboard/scoring window=24h surface counts issue.created
	// events GLOBALLY (across every project) — that's the war-room
	// semantics this test asserts. A bare `== 2` only holds on an empty
	// DB; against the shared/throwaway-but-shared-within-run pool other
	// tests' issue.created events in the same 24h window inflate the
	// count. So we measure the DELTA: read the global count BEFORE
	// seeding, seed exactly two cross-project issue.created events, then
	// assert the count rose by exactly 2. Deterministic regardless of
	// ambient rows, and still proves both projects (own + other) feed the
	// global counter.
	readCreated := func() int64 {
		t.Helper()
		res, body := callDashboard(t, h, "/api/v1/dashboard/scoring?window=24h", caller, "user")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Metrics []struct {
				Key   string `json:"key"`
				Human int64  `json:"human"`
			} `json:"metrics"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, m := range got.Metrics {
			if m.Key == "issues_created" {
				return m.Human
			}
		}
		t.Fatalf("issues_created metric missing from scoring response")
		return 0
	}

	before := readCreated()

	now := time.Now().UTC()
	mineID := "vis-mine-" + uuid.Must(uuid.NewRandom()).String()[:8]
	theirsID := "vis-other-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, mine, caller, mineID, "human")
	seedDashboardEvent(t, pool, "issue.created", mineID, mine, callerUname, caller, "human", now.Add(-30*time.Minute))
	seedDashboardIssue(t, pool, theirs, other, theirsID, "human")
	seedDashboardEvent(t, pool, "issue.created", theirsID, theirs, otherUname, other, "human", now.Add(-30*time.Minute))

	after := readCreated()

	// >= (not ==): /dashboard/scoring is a GLOBAL counter shared across
	// every test package, so a concurrent package inserting a human
	// issue.created in the same 24h window can only INCREASE the delta.
	// Our two cross-project events must contribute, so the delta is at
	// least 2; a delta < 2 means they were NOT counted — the regression
	// this test guards. Robust under SKEIN_TEST_PARALLEL_PKGS without
	// re-introducing the ambient-data flake a bare `== 2` had.
	if delta := after - before; delta < 2 {
		t.Fatalf("expected human issues_created to rise by >=2 (global war-room: own + other project), got delta=%d (before=%d after=%d)",
			delta, before, after)
	}
}

// TestDashboard_Scoring_InvalidWindow — non-recognised window string
// returns 400 invalid_query.
func TestDashboard_Scoring_InvalidWindow(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, _ := seedDashboardUser(t, pool, "user")

	res, _ := callDashboard(t, h, "/api/v1/dashboard/scoring?window=1y", caller, "user")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.StatusCode)
	}
}

// TestDashboard_Scoring_ProjectFilter — explicit project=<uuid> narrows
// the result. Self-sufficient: the caller also posts an identical
// issue.created event to a SECOND project (pB) it owns, so an unfiltered
// result would read human=2, not 1 — the assertion fails the moment
// ProjectFilter stops reaching Store.Scoring's query, on any database,
// not just a package run that happens to leave it alone.
//
// Mutation-verified (sk-350.1): temporarily forcing projectFilter to nil
// in DashboardHandler.Scoring turns this red (human=2, want 1);
// reverting turns it back green.
func TestDashboard_Scoring_ProjectFilter(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	pA, _ := seedDashboardProject(t, pool, caller)
	pB, _ := seedDashboardProject(t, pool, caller)

	now := time.Now().UTC()
	aID := "pf-a-" + uuid.Must(uuid.NewRandom()).String()[:8]
	bID := "pf-b-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, pA, caller, aID, "human")
	seedDashboardEvent(t, pool, "issue.created", aID, pA, callerUname, caller, "human", now.Add(-1*time.Hour))
	seedDashboardIssue(t, pool, pB, caller, bID, "human")
	seedDashboardEvent(t, pool, "issue.created", bID, pB, callerUname, caller, "human", now.Add(-1*time.Hour))

	target := fmt.Sprintf("/api/v1/dashboard/scoring?window=24h&project=%s", pA.String())
	res, body := callDashboard(t, h, target, caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	var got struct {
		Metrics []struct {
			Key   string `json:"key"`
			Human int64  `json:"human"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var created int64 = -1
	for _, m := range got.Metrics {
		if m.Key == "issues_created" {
			created = m.Human
		}
	}
	if created != 1 {
		t.Fatalf("expected human=1 (project A only), got %d", created)
	}
}

// TestDashboard_Scoring_ProjectFilter_Errors covers the two failure
// branches `?project=` carries on Scoring, mirroring
// TestRace_ProjectFilter_Errors: a malformed UUID is a client error
// caught before any DB round-trip (400 invalid_query), and a real
// project the caller cannot see returns 404 not_found — the same
// wire-indistinguishable-from-nonexistent shape every other
// EnsureProjectVisible call site uses.
func TestDashboard_Scoring_ProjectFilter_Errors(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, _ := seedDashboardUser(t, pool, "user")
	owner, _ := seedDashboardUser(t, pool, "user")
	notMember, _ := seedDashboardProject(t, pool, owner) // caller is NOT a member

	t.Run("invalid_uuid", func(t *testing.T) {
		res, body := callDashboard(t, h, "/api/v1/dashboard/scoring?window=24h&project=not-a-uuid", caller, "user")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Error != "invalid_query" {
			t.Fatalf("expected error=invalid_query, got %q", got.Error)
		}
	})

	t.Run("project_not_visible", func(t *testing.T) {
		target := fmt.Sprintf("/api/v1/dashboard/scoring?window=24h&project=%s", notMember.String())
		res, body := callDashboard(t, h, target, caller, "user")
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Error != "not_found" {
			t.Fatalf("expected error=not_found, got %q", got.Error)
		}
	})
}

// TestDashboard_InProgress_ProjectFilter — explicit project=<uuid>
// narrows the snapshot to one project. Self-sufficient: the caller seeds
// an in-progress issue in EACH of two projects it owns, so an
// unfiltered result would return both — the assertion (exactly 1 item,
// the pA issue) fails the moment ProjectFilter stops reaching
// Store.InProgress's query, on any database.
//
// Mutation-verified (sk-350.1): temporarily forcing projectFilter to nil
// in DashboardHandler.InProgress turns this red (2 items, both projects);
// reverting turns it back green.
func TestDashboard_InProgress_ProjectFilter(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, _ := seedDashboardUser(t, pool, "user")
	pA, _ := seedDashboardProject(t, pool, caller)
	pB, _ := seedDashboardProject(t, pool, caller)

	aID := "ipf-a-" + uuid.Must(uuid.NewRandom()).String()[:8]
	bID := "ipf-b-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, pA, caller, aID, "human")
	setIssueInProgress(t, pool, aID)
	seedDashboardIssue(t, pool, pB, caller, bID, "human")
	setIssueInProgress(t, pool, bID)

	target := fmt.Sprintf("/api/v1/dashboard/in-progress?project=%s", pA.String())
	res, body := callDashboard(t, h, target, caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	var got struct {
		Items []struct {
			IssueID string `json:"issueId"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("expected exactly 1 item (project A only), got %d: %+v", len(got.Items), got.Items)
	}
	if got.Items[0].IssueID != aID {
		t.Fatalf("expected issue %q, got %q", aID, got.Items[0].IssueID)
	}
}

// TestDashboard_InProgress_ProjectFilter_Errors covers the two failure
// branches `?project=` carries on InProgress: a malformed UUID is a
// client error caught before any DB round-trip (400 validation_error —
// InProgress's own code, distinct from Scoring/Race's invalid_query),
// and a real project the caller cannot see returns 404 project_not_found
// (also InProgress's own code, distinct from the shared not_found
// mapAccessErr uses) — the same wire-indistinguishable-from-nonexistent
// shape every other EnsureProjectVisible call site uses.
func TestDashboard_InProgress_ProjectFilter_Errors(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, _ := seedDashboardUser(t, pool, "user")
	owner, _ := seedDashboardUser(t, pool, "user")
	notMember, _ := seedDashboardProject(t, pool, owner) // caller is NOT a member

	t.Run("invalid_uuid", func(t *testing.T) {
		res, body := callDashboard(t, h, "/api/v1/dashboard/in-progress?project=not-a-uuid", caller, "user")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Error != "validation_error" {
			t.Fatalf("expected error=validation_error, got %q", got.Error)
		}
	})

	t.Run("project_not_visible", func(t *testing.T) {
		target := fmt.Sprintf("/api/v1/dashboard/in-progress?project=%s", notMember.String())
		res, body := callDashboard(t, h, target, caller, "user")
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d body=%s", res.StatusCode, string(body))
		}
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Error != "project_not_found" {
			t.Fatalf("expected error=project_not_found, got %q", got.Error)
		}
	})
}

// TestDashboard_Feed_RedactsInvisible — the war-room feed returns
// activity from EVERY project (global) but REDACTS the content of rows
// whose project the caller can't see: the foreign row is present with its
// id + project prefix, but title/summary are stripped and redacted=true.
// The caller's own-project row comes through full.
func TestDashboard_Feed_RedactsInvisible(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	other, otherUname := seedDashboardUser(t, pool, "user")
	mine, _ := seedDashboardProject(t, pool, caller)
	theirs, _ := seedDashboardProject(t, pool, other)

	now := time.Now().UTC()
	mineFV := "fv-mine-" + uuid.Must(uuid.NewRandom()).String()[:8]
	theirsFV := "fv-other-" + uuid.Must(uuid.NewRandom()).String()[:8]
	seedDashboardIssue(t, pool, mine, caller, mineFV, "human")
	seedDashboardEvent(t, pool, "issue.created", mineFV, mine, callerUname, caller, "human", now.Add(-1*time.Hour))
	seedDashboardIssue(t, pool, theirs, other, theirsFV, "human")
	seedDashboardEvent(t, pool, "issue.created", theirsFV, theirs, otherUname, other, "human", now.Add(-1*time.Hour))

	res, body := callDashboard(t, h, "/api/v1/dashboard/feed?limit=50", caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	var got struct {
		Items []struct {
			Type     string `json:"type"`
			Summary  string `json:"summary"`
			Redacted bool   `json:"redacted"`
			Entity   struct {
				ID              string `json:"id"`
				Title           string `json:"title"`
				ProjectIDPrefix string `json:"projectIdPrefix"`
			} `json:"entity"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var foundMine, foundTheirs bool
	for _, it := range got.Items {
		switch it.Entity.ID {
		case mineFV:
			foundMine = true
			if it.Redacted {
				t.Fatalf("own-project row must not be redacted: %+v", it)
			}
			if it.Entity.Title == "" {
				t.Fatalf("own-project row must keep its title: %+v", it)
			}
		case theirsFV:
			foundTheirs = true
			// Present but content-redacted: id + project prefix survive,
			// title + summary stripped, redacted flag set.
			if !it.Redacted {
				t.Fatalf("foreign-project row must be redacted: %+v", it)
			}
			if it.Entity.Title != "" || it.Summary != "" {
				t.Fatalf("foreign-project row must have title+summary stripped: %+v", it)
			}
			if it.Entity.ProjectIDPrefix == "" {
				t.Fatalf("foreign-project row must keep its project prefix: %+v", it)
			}
		}
	}
	if !foundMine {
		t.Fatalf("expected own-project issue %s in feed", mineFV)
	}
	if !foundTheirs {
		t.Fatalf("expected foreign-project issue %s present (redacted) in feed", theirsFV)
	}
}
