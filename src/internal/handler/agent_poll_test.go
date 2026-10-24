package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/auth"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/testsupport"
)

// agent_poll_test — HTTP-handler tests for AgentPollHandler.Poll.
//
// sk-164 cutover (T082): the monitor poll reads the primary `events` audit log
// scoped to the token, NOT the personal `notifications` inbox. Every DB fixture
// here is therefore EVENT-SEEDED — apSeedIssueEvent inserts an events row and
// returns its events.id (the id/cursor space the envelope now carries). The
// store-level scoping/cursor/floor correctness lives in
// internal/notifications/poll_for_monitor_test.go; this suite exercises the
// HTTP seam: claims plumbing, query-param parsing/validation, the limit clamp,
// the JSON envelope over events.id, the server-owned floor, the fail-closed
// visibility recheck (403), and the exclude_session rebind plumbing.

// pollEnvelope decodes the wire shape of GET /agent/v1/notifications/poll.
type pollEnvelope struct {
	Notifications []struct {
		ID             int64     `json:"id"`
		Type           string    `json:"type"`
		CreatedAt      time.Time `json:"createdAt"`
		ActorSessionID *string   `json:"actorSessionId,omitempty"`
	} `json:"notifications"`
	Cursor struct {
		TS time.Time `json:"ts"`
		ID int64     `json:"id"`
	} `json:"cursor"`
}

// doPoll drives Poll directly with the supplied claims stashed in context and
// the given raw query string. No middleware: the claims are injected exactly
// as RequireMonitorToken would, isolating the handler.
func doPoll(t *testing.T, h *AgentPollHandler, claims monitorcreds.Claims, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll?"+rawQuery, nil)
	req = req.WithContext(auth.WithMonitorClaims(req.Context(), claims))
	rec := httptest.NewRecorder()
	h.Poll(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

// --- Pure-unit tests (no DB): every case short-circuits before the store. ---

// TestAgentPoll_MissingClaims proves the defensive fail-closed: a request that
// reaches Poll without monitor claims in context (route mounted without the
// middleware) is 401, never served unscoped.
func TestAgentPoll_MissingClaims(t *testing.T) {
	h := NewAgentPollHandler(nil, 5*time.Second) // store never touched on this path
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil)
	rec := httptest.NewRecorder()
	h.Poll(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec); got != "unauthenticated" {
		t.Fatalf("error = %q, want unauthenticated", got)
	}
}

// TestAgentPoll_BadRequests covers every 400 path. All run without a DB: the
// handler validates query params before it ever calls the store OR the
// visibility recheck, so a nil store/pool is safe here (the "400 paths need no
// store" contract).
func TestAgentPoll_BadRequests(t *testing.T) {
	h := NewAgentPollHandler(nil, 5*time.Second)
	claims := monitorcreds.Claims{
		UserID:       uuid.New(),
		ProjectID:    uuid.New(),
		ScopeIssueID: "ap-1",
		EventTypes:   []string{"comment.created"},
	}

	cases := []struct {
		name      string
		query     string
		wantCode  int
		wantError string
	}{
		// since_ts without since_id (and vice versa) → both must be supplied
		// together.
		{"lone since_ts", "since_ts=2026-01-01T00:00:00Z", http.StatusBadRequest, "invalid_cursor"},
		{"lone since_id", "since_id=42", http.StatusBadRequest, "invalid_cursor"},
		// both present but since_ts is not RFC3339.
		{"malformed since_ts", "since_ts=not-a-time&since_id=1", http.StatusBadRequest, "invalid_cursor"},
		// both present but since_id is not an integer.
		{"non-int since_id", "since_ts=2026-01-01T00:00:00Z&since_id=abc", http.StatusBadRequest, "invalid_cursor"},
		// limit must be a positive integer.
		{"limit zero", "limit=0", http.StatusBadRequest, "invalid_limit"},
		{"limit non-int", "limit=abc", http.StatusBadRequest, "invalid_limit"},
		{"limit negative", "limit=-3", http.StatusBadRequest, "invalid_limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPoll(t, h, claims, tc.query)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := decodeError(t, rec); got != tc.wantError {
				t.Fatalf("error = %q, want %q", got, tc.wantError)
			}
		})
	}
}

// --- DB-integration tests: skip silently without TEST_DATABASE_URL. ---

// apSeedIssueEvent inserts an events row (entity_type='issue' on issueID) at the
// given createdAt SQL expression and returns its events.id — the id the poll
// envelope now carries (sk-164). session is written verbatim to
// actor_session_id (nil → SQL NULL). No notification row is seeded: the monitor
// reads events directly.
func apSeedIssueEvent(t *testing.T, pool *pgxpool.Pool, actor, projectID uuid.UUID,
	issueID, ntype, createdAt string, session *string) int64 {
	t.Helper()
	var eid int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_session_id, actor_type, payload)
VALUES (`+createdAt+`, $1, 'issue', $2, $3, 'tester', $4, $5, 'human', '{}'::jsonb)
RETURNING id`, ntype, issueID, projectID, actor, session).Scan(&eid); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return eid
}

// apRegisterRowCleanup tears down events (and any notifications) seeded for the
// given users. Registered AFTER seedDashboardUser so it runs (LIFO) before the
// users are deleted — events.actor_user_id is ON DELETE RESTRICT.
func apRegisterRowCleanup(t *testing.T, pool *pgxpool.Pool, users ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM notifications WHERE user_id = ANY($1)`, users)
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE actor_user_id = ANY($1)`, users)
	})
}

// TestAgentPoll_HappyEnvelope is the integration happy path: a forged epoch
// cursor with no floor surfaces the in-scope event backlog as a well-formed
// JSON envelope over events.id, the cursor equals the last emitted
// (occurred_at,id) tuple, and re-polling with that cursor round-trips to an
// empty page.
// apMintClaims mints a real credential for c (sk-579 T012: Poll records its
// outcome on the credential row, by Claims.ID) and returns c with that id.
// The api key's cleanup cascades the credential away before the user goes.
func apMintClaims(t *testing.T, pool *pgxpool.Pool, c monitorcreds.Claims) monitorcreds.Claims {
	t.Helper()
	key := seedScriptAPIKey(t, pool, c.UserID)
	_, cred, err := monitorcreds.New(pool, time.Hour).Mint(context.Background(), monitorcreds.MintParams{
		UserID:           c.UserID,
		MintedByAPIKeyID: key,
		Audience:         "notifications:poll",
		Capability:       "read",
		ProjectID:        c.ProjectID,
		ScopeIssueID:     c.ScopeIssueID,
		ScopeSubtree:     c.ScopeSubtree,
		EventTypes:       c.EventTypes,
	})
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	c.ID = cred.ID
	return c
}

func TestAgentPoll_HappyEnvelope(t *testing.T) {
	pool := newDashboardTestPool(t)
	caller, _ := seedDashboardUser(t, pool, "user")
	actor, _ := seedDashboardUser(t, pool, "user")
	apRegisterRowCleanup(t, pool, caller, actor)
	projectID, _ := seedDashboardProject(t, pool, caller)

	const issueID = "appoll1"
	e1 := apSeedIssueEvent(t, pool, actor, projectID, issueID,
		"issue.status_changed", "now() - interval '2 minutes'", nil)
	e2 := apSeedIssueEvent(t, pool, actor, projectID, issueID,
		"issue.status_changed", "now() - interval '1 minute'", nil)

	h := NewAgentPollHandler(pool, 5*time.Second)
	claims := apMintClaims(t, pool, monitorcreds.Claims{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		// CreatedAt zero → floor disabled, so the forged epoch cursor surfaces
		// the backlog (the floor has its own dedicated test below).
	})

	rec := doPoll(t, h, claims, "since_ts=1970-01-01T00:00:00Z&since_id=0&limit=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env pollEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", rec.Body.String(), err)
	}
	got := map[int64]bool{}
	for _, n := range env.Notifications {
		got[n.ID] = true
	}
	if !got[e1] || !got[e2] {
		t.Fatalf("expected both %d and %d in envelope, got %+v", e1, e2, env.Notifications)
	}
	// Cursor = last emitted tuple (ascending (occurred_at,id) order → e2 last).
	last := env.Notifications[len(env.Notifications)-1]
	if env.Cursor.ID != last.ID {
		t.Fatalf("cursor id = %d, want last row id %d", env.Cursor.ID, last.ID)
	}
	if env.Cursor.ID != e2 {
		t.Fatalf("cursor id = %d, want %d (newest row)", env.Cursor.ID, e2)
	}

	// Round-trip: feed the returned cursor back → the already-seen rows must
	// not reappear (empty page; cursor unchanged so the runner still advances).
	q := "since_ts=" + env.Cursor.TS.UTC().Format(time.RFC3339Nano) +
		"&since_id=" + strconv.FormatInt(env.Cursor.ID, 10)
	rec2 := doPoll(t, h, claims, q)
	if rec2.Code != http.StatusOK {
		t.Fatalf("round-trip status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}
	var env2 pollEnvelope
	if err := json.Unmarshal(rec2.Body.Bytes(), &env2); err != nil {
		t.Fatalf("decode round-trip envelope: %v", err)
	}
	if len(env2.Notifications) != 0 {
		t.Fatalf("round-trip page should be empty (cursor excludes seen rows), got %d", len(env2.Notifications))
	}
}

// TestAgentPoll_LimitClamped seeds 101 eligible events and polls with limit=500.
// The handler clamps limit to 100, so the page must hold at most 100 rows.
func TestAgentPoll_LimitClamped(t *testing.T) {
	pool := newDashboardTestPool(t)
	caller, _ := seedDashboardUser(t, pool, "user")
	actor, _ := seedDashboardUser(t, pool, "user")
	apRegisterRowCleanup(t, pool, caller, actor)
	projectID, _ := seedDashboardProject(t, pool, caller)

	const issueID = "apclamp1"
	// Bulk-seed 101 issue events aged ~5 minutes (past the lag), all in scope
	// and of the whitelisted type.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
SELECT now() - interval '5 minutes', 'issue.status_changed', 'issue', $3,
       $2, 'tester', $1, 'human', '{}'::jsonb
FROM generate_series(1, 101) g`, actor, projectID, issueID); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}

	h := NewAgentPollHandler(pool, 5*time.Second)
	claims := apMintClaims(t, pool, monitorcreds.Claims{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
	})
	rec := doPoll(t, h, claims, "since_ts=1970-01-01T00:00:00Z&since_id=0&limit=500")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env pollEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Notifications) > 100 {
		t.Fatalf("limit=500 not clamped: page has %d rows, want <= 100", len(env.Notifications))
	}
	if len(env.Notifications) != 100 {
		t.Fatalf("expected exactly 100 rows (clamp applied over 101 eligible), got %d", len(env.Notifications))
	}
}

// TestAgentPoll_FloorHonored proves the handler passes claims.CreatedAt to the
// store as the server-owned not-before floor: an event committed BEFORE the
// credential mint time stays invisible even with a forged epoch cursor, while a
// post-mint event surfaces.
func TestAgentPoll_FloorHonored(t *testing.T) {
	pool := newDashboardTestPool(t)
	caller, _ := seedDashboardUser(t, pool, "user")
	actor, _ := seedDashboardUser(t, pool, "user")
	apRegisterRowCleanup(t, pool, caller, actor)
	projectID, _ := seedDashboardProject(t, pool, caller)

	const issueID = "apfloor1"
	// Backlog committed before the floor.
	eOld := apSeedIssueEvent(t, pool, actor, projectID, issueID,
		"issue.status_changed", "now() - interval '1 hour'", nil)

	// Floor = the credential mint time, read off the DB clock so it shares the
	// now() the store reads (no app/DB skew).
	var floor time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&floor); err != nil {
		t.Fatalf("db now(): %v", err)
	}

	// A legitimately post-mint event.
	eNew := apSeedIssueEvent(t, pool, actor, projectID, issueID,
		"issue.status_changed", "now()", nil)

	h := NewAgentPollHandler(pool, 0) // 0 lag → isolate the floor from the watermark
	claims := apMintClaims(t, pool, monitorcreds.Claims{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		CreatedAt:    floor,
	})
	rec := doPoll(t, h, claims, "since_ts=1970-01-01T00:00:00Z&since_id=0&limit=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env pollEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[int64]bool{}
	for _, n := range env.Notifications {
		got[n.ID] = true
	}
	if got[eOld] {
		t.Errorf("pre-mint backlog %d must NOT surface via a forged old cursor (floor=claims.CreatedAt)", eOld)
	}
	if !got[eNew] {
		t.Errorf("post-mint event %d must surface", eNew)
	}
}

// TestAgentPoll_FailClosedVisibility is the SC-006 class gate on the HTTP seam.
// The handler re-derives the owner's CURRENT visibility of the token's project
// on every poll (INV-8, parity with EnsureProjectVisible):
//   - (a) owner removed from project_members and NOT an org-admin → 403
//     visibility_lost, no events served;
//   - (b) project archived but the owner is still a member → 200 (archival does
//     NOT gate);
//   - (c) org-admin removed from members but still sees via admin-bypass → 200.
func TestAgentPoll_FailClosedVisibility(t *testing.T) {
	pool := newDashboardTestPool(t)
	ctx := context.Background()
	const issueID = "apvis1"
	const old = "now() - interval '1 minute'"

	// A permissive cursor + no floor so the seeded backlog is eligible whenever
	// visibility passes.
	query := "since_ts=1970-01-01T00:00:00Z&since_id=0&limit=50"

	t.Run("owner removed, not admin → 403", func(t *testing.T) {
		caller, _ := seedDashboardUser(t, pool, "user")
		actor, _ := seedDashboardUser(t, pool, "user")
		apRegisterRowCleanup(t, pool, caller, actor)
		projectID, _ := seedDashboardProject(t, pool, caller)
		apSeedIssueEvent(t, pool, actor, projectID, issueID, "issue.status_changed", old, nil)

		// Remove the owner from the project's members → loses visibility.
		if _, err := pool.Exec(ctx,
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, projectID, caller); err != nil {
			t.Fatalf("remove member: %v", err)
		}

		h := NewAgentPollHandler(pool, 5*time.Second)
		claims := apMintClaims(t, pool, monitorcreds.Claims{UserID: caller, ProjectID: projectID, ScopeIssueID: issueID, EventTypes: []string{"issue.status_changed"}})
		rec := doPoll(t, h, claims, query)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
		if got := decodeError(t, rec); got != "visibility_lost" {
			t.Fatalf("error = %q, want visibility_lost", got)
		}
	})

	t.Run("archived project, still member → 200", func(t *testing.T) {
		caller, _ := seedDashboardUser(t, pool, "user")
		actor, _ := seedDashboardUser(t, pool, "user")
		apRegisterRowCleanup(t, pool, caller, actor)
		projectID, _ := seedDashboardProject(t, pool, caller)
		eid := apSeedIssueEvent(t, pool, actor, projectID, issueID, "issue.status_changed", old, nil)

		// Archive the project; the owner keeps membership → archival must NOT gate.
		if _, err := pool.Exec(ctx, `UPDATE projects SET archived_at = now() WHERE id = $1`, projectID); err != nil {
			t.Fatalf("archive project: %v", err)
		}

		h := NewAgentPollHandler(pool, 5*time.Second)
		claims := apMintClaims(t, pool, monitorcreds.Claims{UserID: caller, ProjectID: projectID, ScopeIssueID: issueID, EventTypes: []string{"issue.status_changed"}})
		rec := doPoll(t, h, claims, query)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 on an archived-but-member project (body=%s)", rec.Code, rec.Body.String())
		}
		var env pollEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		found := false
		for _, n := range env.Notifications {
			if n.ID == eid {
				found = true
			}
		}
		if !found {
			t.Errorf("event %d must still be delivered on an archived-but-member project (archival does not gate, INV-8)", eid)
		}
	})

	t.Run("org-admin removed from members → 200 via admin-bypass", func(t *testing.T) {
		// The token owner is an org admin; the project is owned by someone else
		// and the admin is NOT a member — admin-bypass keeps visibility.
		projOwner, _ := seedDashboardUser(t, pool, "user")
		admin, _ := seedDashboardUser(t, pool, "admin")
		actor, _ := seedDashboardUser(t, pool, "user")
		apRegisterRowCleanup(t, pool, projOwner, admin, actor)
		projectID, _ := seedDashboardProject(t, pool, projOwner)
		eid := apSeedIssueEvent(t, pool, actor, projectID, issueID, "issue.status_changed", old, nil)

		// The admin is not (and never was) a member of this project.
		h := NewAgentPollHandler(pool, 5*time.Second)
		claims := apMintClaims(t, pool, monitorcreds.Claims{UserID: admin, ProjectID: projectID, ScopeIssueID: issueID, EventTypes: []string{"issue.status_changed"}})
		rec := doPoll(t, h, claims, query)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for an org-admin via admin-bypass (body=%s)", rec.Code, rec.Body.String())
		}
		var env pollEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		found := false
		for _, n := range env.Notifications {
			if n.ID == eid {
				found = true
			}
		}
		if !found {
			t.Errorf("event %d must be delivered to an org-admin who sees via bypass (INV-8 admin-bypass)", eid)
		}
	})
}

// TestAgentPoll_ExcludeSessionPlumbing proves the HTTP layer wires the
// exclude_session query-param through to PollParams.ExcludeSession (FR-2). The
// observable effect is the store's narrow-only echo suppression:
//   - non-empty exclude_session drops that session's events (its own echo);
//   - an empty / whitespace-only value is trimmed to "" → no narrowing.
//
// Without this test the store predicate could be green while the prod HTTP
// rebind is dead (the param never reaching PollParams).
func TestAgentPoll_ExcludeSessionPlumbing(t *testing.T) {
	pool := newDashboardTestPool(t)
	caller, _ := seedDashboardUser(t, pool, "user")
	actor, _ := seedDashboardUser(t, pool, "user")
	apRegisterRowCleanup(t, pool, caller, actor)
	projectID, _ := seedDashboardProject(t, pool, caller)

	const issueID = "apxs1"
	const old = "now() - interval '1 minute'"
	eSelf := apSeedIssueEvent(t, pool, actor, projectID, issueID, "issue.status_changed", old, sptrH("sess-self"))
	ePeer := apSeedIssueEvent(t, pool, actor, projectID, issueID, "issue.status_changed", old, sptrH("sess-peer"))

	h := NewAgentPollHandler(pool, 5*time.Second)
	claims := apMintClaims(t, pool, monitorcreds.Claims{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
	})
	const cursor = "since_ts=1970-01-01T00:00:00Z&since_id=0&limit=50"

	ids := func(rec *httptest.ResponseRecorder) map[int64]bool {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var env pollEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := map[int64]bool{}
		for _, n := range env.Notifications {
			got[n.ID] = true
		}
		return got
	}

	// (1) non-empty exclude_session → its own session's event dropped, peer kept.
	got := ids(doPoll(t, h, claims, cursor+"&exclude_session=sess-self"))
	if got[eSelf] {
		t.Errorf("exclude_session=sess-self must drop event %d (plumbing dead — param never reached PollParams)", eSelf)
	}
	if !got[ePeer] {
		t.Errorf("exclude_session=sess-self must keep peer event %d", ePeer)
	}

	// (2) empty exclude_session → no narrowing, both delivered.
	got = ids(doPoll(t, h, claims, cursor+"&exclude_session="))
	if !got[eSelf] || !got[ePeer] {
		t.Errorf("empty exclude_session must not narrow: got self=%v peer=%v", got[eSelf], got[ePeer])
	}

	// (3) whitespace-only exclude_session → trimmed to "" → no narrowing.
	got = ids(doPoll(t, h, claims, cursor+"&exclude_session=%20%20%20"))
	if !got[eSelf] || !got[ePeer] {
		t.Errorf("whitespace exclude_session must be trimmed to no-narrowing: got self=%v peer=%v", got[eSelf], got[ePeer])
	}
}

// TestAgentPoll_ScopeFromClaimsNotQuery is the INV-4 verification: scope
// (issue/subtree/type) is taken from the validated claims, never from the
// query string. An event on a DIFFERENT issue than the claims scope is never
// delivered, and the request carries no scope query-param that could change it.
func TestAgentPoll_ScopeFromClaimsNotQuery(t *testing.T) {
	pool := newDashboardTestPool(t)
	caller, _ := seedDashboardUser(t, pool, "user")
	actor, _ := seedDashboardUser(t, pool, "user")
	apRegisterRowCleanup(t, pool, caller, actor)
	projectID, _ := seedDashboardProject(t, pool, caller)

	const scopedIssue = "apscope1"
	const otherIssue = "apscope2"
	const old = "now() - interval '1 minute'"
	eScoped := apSeedIssueEvent(t, pool, actor, projectID, scopedIssue, "issue.status_changed", old, nil)
	eOther := apSeedIssueEvent(t, pool, actor, projectID, otherIssue, "issue.status_changed", old, nil)

	h := NewAgentPollHandler(pool, 5*time.Second)
	claims := apMintClaims(t, pool, monitorcreds.Claims{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: scopedIssue,
		EventTypes:   []string{"issue.status_changed"},
	})
	// A query that TRIES to widen scope to the other issue is ignored — the
	// handler reads scope only from claims.
	rec := doPoll(t, h, claims, "since_ts=1970-01-01T00:00:00Z&since_id=0&limit=50&scope_issue_id="+otherIssue+"&subtree=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env pollEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[int64]bool{}
	for _, n := range env.Notifications {
		got[n.ID] = true
	}
	if !got[eScoped] {
		t.Errorf("scoped-issue event %d must be delivered", eScoped)
	}
	if got[eOther] {
		t.Errorf("other-issue event %d must NOT be delivered — a query scope_issue_id must not widen the claims scope (INV-4)", eOther)
	}
}

// sptrH returns a pointer to s (handler-package local helper for actor session).
func sptrH(s string) *string { return &s }

// ---------------------------------------------------------------------------
// sk-579 T012 — successful-poll record and rootClosed (matrix rows 18–21).
// Time is moved by shifting closed_at and occurred_at of the fixture project
// by the same amount; the handler's SafetyLag is 5s.
// ---------------------------------------------------------------------------

const rcLag = 5 * time.Second

// rcEnvelope is the poll answer with the rootClosed flag.
type rcEnvelope struct {
	Notifications []struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"notifications"`
	Cursor struct {
		TS time.Time `json:"ts"`
		ID int64     `json:"id"`
	} `json:"cursor"`
	RootClosed bool `json:"rootClosed"`
}

func (e rcEnvelope) ids() []int64 {
	out := make([]int64, 0, len(e.Notifications))
	for _, n := range e.Notifications {
		out = append(out, n.ID)
	}
	return out
}

func (e rcEnvelope) next() string {
	return "since_ts=" + e.Cursor.TS.UTC().Format(time.RFC3339Nano) + "&since_id=" + strconv.FormatInt(e.Cursor.ID, 10)
}

const rcEpoch = "since_ts=1970-01-01T00:00:00Z&since_id=0"

type rcFixture struct {
	pool      *pgxpool.Pool
	owner     uuid.UUID
	ownerAct  issues.Actor
	other     uuid.UUID
	otherAct  issues.Actor
	project   uuid.UUID
	prefix    string
	issues    *issues.Store
	h         *AgentPollHandler
	otherProj uuid.UUID
}

func newRCFixture(t *testing.T) *rcFixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	other, otherName := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "rcl")
	testsupport.AddProjectMember(t, pool, project, other, "member")
	otherProj, _ := testsupport.SeedProject(t, pool, owner, "rco")
	// Registered last → runs first: events (RESTRICT on users) and comments
	// go before the issues, projects and users.
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE project_id = ANY($1)`, []uuid.UUID{project, otherProj})
		_, _ = pool.Exec(bg, `DELETE FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, project)
	})
	return &rcFixture{
		pool: pool, owner: owner, other: other, project: project, prefix: prefix, otherProj: otherProj,
		ownerAct: issues.HumanActor(owner, ownerName),
		otherAct: issues.Actor{UserID: other, Username: otherName, Role: "agent", SessionID: "sess-other"},
		issues:   issues.NewStore(pool),
		h:        NewAgentPollHandler(pool, rcLag),
	}
}

// issue creates an issue through the product path (optionally under parent).
func (f *rcFixture) issue(t *testing.T, parent string) string {
	t.Helper()
	in := issues.CreateIssueInput{Title: "rc", Type: "task", Priority: 2}
	if parent != "" {
		in.ParentID = &parent
	}
	dto, err := f.issues.CreateIssue(context.Background(), f.project, f.prefix, "counter", in, f.owner, f.ownerAct)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	return dto.ID
}

// close closes id through the product path (closed_at and issue.closed are
// written by one transaction).
func (f *rcFixture) close(t *testing.T, id string, actor issues.Actor) {
	t.Helper()
	var version int
	if err := f.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, id).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if _, err := f.issues.Close(context.Background(), issues.CloseInput{
		ID: id, ExpectedVersion: version, Reason: "done", Actor: actor, SessionID: actor.SessionID, Force: true,
	}); err != nil {
		t.Fatalf("Close %s: %v", id, err)
	}
}

func (f *rcFixture) reopen(t *testing.T, id string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE issues SET status = 'open', closed_at = NULL, closed_reason = NULL, close_outcome = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

// event inserts one event at occurredAt (SQL expression) and returns its id.
func (f *rcFixture) event(t *testing.T, project uuid.UUID, typ, entityType, entityID, occurredAt string, session *string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_session_id, actor_type, payload)
VALUES (`+occurredAt+`, $1, $2, $3, $4, 'tester', $5, $6, 'agent', '{}'::jsonb)
RETURNING id`, typ, entityType, entityID, project, f.other, session).Scan(&id); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

// comment adds a comment row on issueID and its comment.created event.
func (f *rcFixture) comment(t *testing.T, issueID, occurredAt string, session *string) int64 {
	t.Helper()
	cid := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, created_at, updated_at, version)
VALUES ($1, $2, 'tester', $3, 'agent', 'rc comment', now(), now(), 1)`, cid, issueID, f.other); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	return f.event(t, f.project, "comment.created", "comment", cid.String(), occurredAt, session)
}

// shift moves every closed_at and occurred_at of the fixture projects into
// the past by d.
func (f *rcFixture) shift(t *testing.T, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	projects := []uuid.UUID{f.project, f.otherProj}
	if _, err := f.pool.Exec(ctx, `UPDATE issues SET closed_at = closed_at - $2::interval WHERE project_id = ANY($1) AND closed_at IS NOT NULL`, projects, d); err != nil {
		t.Fatalf("shift issues: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE events SET occurred_at = occurred_at - $2::interval WHERE project_id = ANY($1)`, projects, d); err != nil {
		t.Fatalf("shift events: %v", err)
	}
}

// monitor mints a real credential on root and returns its claims (floor off).
func (f *rcFixture) monitor(t *testing.T, root string, subtree bool, types ...string) monitorcreds.Claims {
	t.Helper()
	return apMintClaims(t, f.pool, monitorcreds.Claims{
		UserID: f.owner, ProjectID: f.project, ScopeIssueID: root, ScopeSubtree: subtree, EventTypes: types,
	})
}

func (f *rcFixture) poll(t *testing.T, claims monitorcreds.Claims, query string) rcEnvelope {
	t.Helper()
	rec := doPoll(t, f.h, claims, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env rcEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return env
}

func rcHas(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func sp(s string) *string { return &s }

// TestAgentPoll_FinishSkipsRevokedCredential — regression (review of wave
// sk-579/skein-api/4): a monitor stopped after the door accepted the request
// must not record the poll nor decide root_closed; the answer is not 200.
func TestAgentPoll_FinishSkipsRevokedCredential(t *testing.T) {
	f := newRCFixture(t)
	root := f.issue(t, "")
	f.close(t, root, f.otherAct)
	f.shift(t, time.Minute)
	m := f.monitor(t, root, false, "comment.created")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE monitor_credentials SET revoked_at = now() WHERE id = $1`, m.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec := doPoll(t, f.h, m, rcEpoch)
	if rec.Code == http.StatusOK {
		t.Fatalf("poll of a credential revoked after the door = 200, want a non-success")
	}
	var succeeded, rootClosed *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT last_poll_succeeded_at, root_closed_at FROM monitor_credentials WHERE id = $1`, m.ID).
		Scan(&succeeded, &rootClosed); err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if succeeded != nil || rootClosed != nil {
		t.Fatalf("revoked credential got last_poll_succeeded_at=%v root_closed_at=%v, want both NULL", succeeded, rootClosed)
	}
}

// TestAgentPoll_RootClosedRule — matrix row 19 (INV-12, SC-007 (a)–(e)).
func TestAgentPoll_RootClosedRule(t *testing.T) {
	const minute = time.Minute

	t.Run("a: inclusive bound — page 1 the comment, page 2 issue.closed and true", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		c := f.comment(t, root, "now()", nil)
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		m := f.monitor(t, root, false, "comment.created", "issue.closed")
		p1 := f.poll(t, m, rcEpoch+"&limit=1")
		if !rcHas(p1.ids(), c) || p1.RootClosed {
			t.Fatalf("page 1 = %v rootClosed=%v, want the comment and false", p1.ids(), p1.RootClosed)
		}
		p2 := f.poll(t, m, p1.next()+"&limit=1")
		if len(p2.Notifications) != 1 || p2.Notifications[0].Type != "issue.closed" || !p2.RootClosed {
			t.Fatalf("page 2 = %+v rootClosed=%v, want issue.closed and true", p2.Notifications, p2.RootClosed)
		}
	})

	t.Run("a: closed by the issuing session behind exclude_session — true without delivering it", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		f.close(t, root, issues.Actor{UserID: f.owner, Username: f.ownerAct.Username, Role: "agent", SessionID: "sess-mint"})
		f.shift(t, minute)
		m := f.monitor(t, root, false, "comment.created", "issue.closed")
		p := f.poll(t, m, rcEpoch+"&exclude_session=sess-mint")
		if len(p.Notifications) != 0 || !p.RootClosed {
			t.Fatalf("got %+v rootClosed=%v, want nothing delivered and true", p.Notifications, p.RootClosed)
		}
	})

	t.Run("b: a comment of the non-empty issuing session does not hold the exit", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		own := f.comment(t, root, "now() - interval '1 second'", sp("sess-mint"))
		f.close(t, root, f.otherAct)
		f.shift(t, 10*time.Second)
		// Only comment.created: nothing is delivered, so the returned cursor
		// stays at the epoch and the own comment lies inside the debt window —
		// a debt that ignored exclude_session would hold the exit (false).
		m := f.monitor(t, root, false, "comment.created")
		p := f.poll(t, m, rcEpoch+"&exclude_session=sess-mint")
		if rcHas(p.ids(), own) || !p.RootClosed {
			t.Fatalf("got %v rootClosed=%v, want the own comment absent and true", p.ids(), p.RootClosed)
		}
	})

	t.Run("b: an undelivered comment inside the lag holds false until it is delivered", func(t *testing.T) {
		for name, session := range map[string]*string{"another session": sp("sess-peer"), "session-less (empty issuing session)": nil} {
			t.Run(name, func(t *testing.T) {
				f := newRCFixture(t)
				root := f.issue(t, "")
				c := f.comment(t, root, "now() - interval '1 second'", session)
				f.close(t, root, f.otherAct)
				m := f.monitor(t, root, false, "comment.created", "issue.closed")
				// The first false is already held by the conditional SafetyLag
				// (closed_at is fresh and later than the epoch cursor); this
				// subtest guards «delivered, then true», not the debt content.
				p1 := f.poll(t, m, rcEpoch)
				if rcHas(p1.ids(), c) || p1.RootClosed {
					t.Fatalf("inside the lag: %v rootClosed=%v, want nothing yet and false", p1.ids(), p1.RootClosed)
				}
				f.shift(t, 10*time.Second)
				p2 := f.poll(t, m, p1.next())
				if !rcHas(p2.ids(), c) || !p2.RootClosed {
					t.Fatalf("after the lag: %v rootClosed=%v, want the comment and true", p2.ids(), p2.RootClosed)
				}
			})
		}
	})

	t.Run("c: a debt longer than a page holds false until the last page", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		for i := 0; i < 3; i++ {
			f.comment(t, root, "now()", nil)
		}
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		m := f.monitor(t, root, false, "comment.created", "issue.closed")
		q := rcEpoch
		for page := 1; page <= 4; page++ {
			p := f.poll(t, m, q+"&limit=1")
			if want := page == 4; p.RootClosed != want {
				t.Fatalf("page %d rootClosed=%v, want %v", page, p.RootClosed, want)
			}
			q = p.next()
		}
	})

	t.Run("same occurred_at as the cursor and a larger id is still owed", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		at := "'2026-01-01T00:00:00Z'::timestamptz"
		c1 := f.comment(t, root, at, nil)
		c2 := f.comment(t, root, at, nil)
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		m := f.monitor(t, root, false, "comment.created")
		p1 := f.poll(t, m, rcEpoch+"&limit=1")
		if !rcHas(p1.ids(), c1) || p1.RootClosed {
			t.Fatalf("page 1 = %v rootClosed=%v, want %d and false (%d has the same occurred_at, larger id)", p1.ids(), p1.RootClosed, c1, c2)
		}
		p2 := f.poll(t, m, p1.next()+"&limit=1")
		if !rcHas(p2.ids(), c2) || !p2.RootClosed {
			t.Fatalf("page 2 = %v rootClosed=%v, want %d and true", p2.ids(), p2.RootClosed, c2)
		}
	})

	t.Run("d: cursor not before closed_at — true at once; a break event is not delivered", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		brk := f.comment(t, root, "now() - interval '1 minute'", nil)
		f.close(t, root, f.otherAct) // fresh: younger than the lag
		m := f.monitor(t, root, false, "comment.created", "issue.closed")
		p := f.poll(t, m, "") // live-start: the cursor is now() ≥ closed_at
		if rcHas(p.ids(), brk) || !p.RootClosed {
			t.Fatalf("live-start after the close: %v rootClosed=%v, want the break event absent and true", p.ids(), p.RootClosed)
		}
	})

	t.Run("cursor before a fresh closed_at with an empty debt — false until the lag passes", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		f.close(t, root, f.otherAct)
		m := f.monitor(t, root, false, "comment.created")
		p1 := f.poll(t, m, rcEpoch)
		if p1.RootClosed {
			t.Fatalf("closed_at younger than the lag after the cursor: rootClosed=true, want false")
		}
		f.shift(t, 10*time.Second)
		if p2 := f.poll(t, m, p1.next()); !p2.RootClosed {
			t.Fatalf("after the lag: rootClosed=false, want true")
		}
	})

	t.Run("e: eventTypes without issue.closed", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		m := f.monitor(t, root, false, "comment.created")
		if p := f.poll(t, m, rcEpoch); !p.RootClosed {
			t.Fatalf("rootClosed=false, want true")
		}
	})

	t.Run("sticky: reopened after the record, still true", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		m := f.monitor(t, root, false, "comment.created")
		p1 := f.poll(t, m, rcEpoch)
		if !p1.RootClosed {
			t.Fatalf("first poll rootClosed=false, want true")
		}
		f.reopen(t, root)
		if p2 := f.poll(t, m, p1.next()); !p2.RootClosed {
			t.Fatalf("after reopen rootClosed=false, want true (the record is sticky)")
		}
	})

	t.Run("f: closed and reopened before the poll — false", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		f.reopen(t, root)
		m := f.monitor(t, root, false, "comment.created", "issue.closed")
		if p := f.poll(t, m, rcEpoch); p.RootClosed {
			t.Fatalf("rootClosed=true for a reopened root, want false")
		}
	})

	t.Run("a closed descendant or ancestor is not the root", func(t *testing.T) {
		f := newRCFixture(t)
		parent := f.issue(t, "")
		child := f.issue(t, parent)
		f.close(t, child, f.otherAct)
		f.shift(t, minute)
		if p := f.poll(t, f.monitor(t, parent, true, "comment.created"), rcEpoch); p.RootClosed {
			t.Fatalf("subtree monitor on %s: a closed child gave rootClosed=true", parent)
		}
		f2 := newRCFixture(t)
		p2 := f2.issue(t, "")
		c2 := f2.issue(t, p2)
		f2.close(t, p2, f2.otherAct)
		f2.shift(t, minute)
		if p := f2.poll(t, f2.monitor(t, c2, false, "comment.created"), rcEpoch); p.RootClosed {
			t.Fatalf("monitor on %s: its closed parent gave rootClosed=true", c2)
		}
	})

	t.Run("events the poll would not deliver are not debt", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		other := f.issue(t, "")
		child := f.issue(t, root)
		f.event(t, f.project, "issue.mention", "issue", root, "now()", nil)
		cm := f.comment(t, root, "now()", nil)
		if _, err := f.pool.Exec(context.Background(), `UPDATE events SET type = 'comment.mention' WHERE id = $1`, cm); err != nil {
			t.Fatalf("mention: %v", err)
		}
		f.event(t, f.project, "issue.status_changed", "issue", other, "now()", nil)
		f.event(t, f.otherProj, "issue.status_changed", "issue", root, "now()", nil)
		f.comment(t, child, "now()", nil)
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		late := f.comment(t, root, "now()", nil) // after closed_at, inside the lag
		m := f.monitor(t, root, false, "comment.created", "comment.mention", "issue.mention", "issue.status_changed")
		p := f.poll(t, m, rcEpoch)
		if !p.RootClosed {
			t.Fatalf("rootClosed=false: an event the poll never delivers was counted as debt (body %v)", p.ids())
		}
		if rcHas(p.ids(), late) {
			t.Fatalf("the late comment inside the lag was delivered")
		}
	})

	t.Run("a descendant comment is debt under subtree", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		child := f.issue(t, root)
		f.comment(t, child, "now()", nil)
		f.comment(t, child, "now()", nil)
		f.close(t, root, f.otherAct)
		f.shift(t, minute)
		m := f.monitor(t, root, true, "comment.created")
		p1 := f.poll(t, m, rcEpoch+"&limit=1")
		if p1.RootClosed {
			t.Fatalf("page 1: rootClosed=true with an undelivered descendant comment")
		}
		if p2 := f.poll(t, m, p1.next()+"&limit=1"); !p2.RootClosed {
			t.Fatalf("page 2: rootClosed=false after the last descendant comment")
		}
	})
}

// rcSuccess reads the successful-poll stamp and the root-closed record.
func rcSuccess(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (success, rootClosed *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT last_poll_succeeded_at, root_closed_at FROM monitor_credentials WHERE id = $1`, id).
		Scan(&success, &rootClosed); err != nil {
		t.Fatalf("read credential: %v", err)
	}
	return success, rootClosed
}

// TestAgentPoll_SuccessfulPollRecord — matrix row 18 (INV-5 "successful poll",
// R5): only a 200 records the successful poll.
func TestAgentPoll_SuccessfulPollRecord(t *testing.T) {
	t.Run("a 200 records it; 400 and 403 do not", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		m := f.monitor(t, root, false, "comment.created")
		if rec := doPoll(t, f.h, m, "since_ts=garbage&since_id=1"); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad cursor status = %d", rec.Code)
		}
		if s, _ := rcSuccess(t, f.pool, m.ID); s != nil {
			t.Fatalf("a 400 recorded a successful poll")
		}
		stranger := apMintClaims(t, f.pool, monitorcreds.Claims{UserID: f.other, ProjectID: f.project, ScopeIssueID: root, EventTypes: []string{"comment.created"}})
		if _, err := f.pool.Exec(context.Background(), `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.project, f.other); err != nil {
			t.Fatalf("remove member: %v", err)
		}
		if rec := doPoll(t, f.h, stranger, rcEpoch); rec.Code != http.StatusForbidden {
			t.Fatalf("visibility-lost status = %d, want 403", rec.Code)
		}
		if s, _ := rcSuccess(t, f.pool, stranger.ID); s != nil {
			t.Fatalf("a 403 recorded a successful poll")
		}
		f.poll(t, m, rcEpoch)
		if s, _ := rcSuccess(t, f.pool, m.ID); s == nil {
			t.Fatalf("a 200 did not record the successful poll")
		}
	})

	t.Run("a failing finish statement is 500 with nothing recorded", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		f.close(t, root, f.otherAct)
		f.shift(t, time.Minute)
		m := f.monitor(t, root, false, "comment.created")

		cfg, err := pgxpool.ParseConfig(os.Getenv(testsupport.TestDatabaseURL))
		if err != nil {
			t.Fatalf("parse url: %v", err)
		}
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = "200ms"
		lockedPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer lockedPool.Close()

		tx, err := f.pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(context.Background(), `SELECT 1 FROM monitor_credentials WHERE id = $1 FOR UPDATE`, m.ID); err != nil {
			t.Fatalf("lock credential: %v", err)
		}
		rec := doPoll(t, NewAgentPollHandler(lockedPool, rcLag), m, rcEpoch)
		_ = tx.Rollback(context.Background())
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status with the credential row locked = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
		}
		if s, rc := rcSuccess(t, f.pool, m.ID); s != nil || rc != nil {
			t.Fatalf("a failed finish recorded success=%v rootClosed=%v", s, rc)
		}
	})

	t.Run("downloading the runner body is not a successful poll", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		key := seedScriptAPIKey(t, f.pool, f.owner)
		store := monitorcreds.New(f.pool, time.Hour)
		token, cred, err := store.Mint(context.Background(), monitorcreds.MintParams{
			UserID: f.owner, MintedByAPIKeyID: key, Audience: "notifications:poll", Capability: "read",
			ProjectID: f.project, ScopeIssueID: root, EventTypes: []string{"comment.created"},
		})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		h := auth.RequireMonitorToken(store, "notifications:poll")(http.HandlerFunc(
			NewAgentScriptHandler(2, 10*time.Second, 15*time.Second, 3*time.Minute).Script))
		req := httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil)
		req.Header.Set(auth.HeaderAPIKey, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("script status = %d", rec.Code)
		}
		var lastUsed *time.Time
		if err := f.pool.QueryRow(context.Background(), `SELECT last_used_at FROM monitor_credentials WHERE id = $1`, cred.ID).Scan(&lastUsed); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		if lastUsed == nil {
			t.Fatalf("the download did not touch last_used_at")
		}
		if s, _ := rcSuccess(t, f.pool, cred.ID); s != nil {
			t.Fatalf("downloading the runner recorded a successful poll")
		}
	})

	t.Run("a pre-rollout credential is recorded and keeps NULL session and path", func(t *testing.T) {
		f := newRCFixture(t)
		root := f.issue(t, "")
		m := f.monitor(t, root, false, "comment.created")
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE monitor_credentials SET issued_session_id = NULL, token_file_path = NULL WHERE id = $1`, m.ID); err != nil {
			t.Fatalf("make pre-rollout: %v", err)
		}
		f.poll(t, m, rcEpoch)
		var session, path *string
		var success *time.Time
		if err := f.pool.QueryRow(context.Background(),
			`SELECT issued_session_id, token_file_path, last_poll_succeeded_at FROM monitor_credentials WHERE id = $1`, m.ID).
			Scan(&session, &path, &success); err != nil {
			t.Fatalf("read: %v", err)
		}
		if success == nil || session != nil || path != nil {
			t.Fatalf("success=%v session=%v path=%v, want success set and both NULL", success, session, path)
		}
	})
}

// rcTracer counts the statements a pool sends.
type rcTracer struct{ n atomic.Int64 }

func (r *rcTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	r.n.Add(1)
	return ctx
}

func (r *rcTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestAgentPoll_OneExtraStatement — matrix row 20 (NFR: a poll adds at most one
// statement). N₀ was measured with this tracer on the tree before T012
// (commit 2bdb0737): 3 statements for a poll with a cursor (owner lookup,
// project access, page) and 4 for a live-start poll (plus SELECT now()).
func TestAgentPoll_OneExtraStatement(t *testing.T) {
	const n0Cursor, n0LiveStart = 3, 4
	f := newRCFixture(t)
	root := f.issue(t, "")
	f.close(t, root, f.otherAct)
	f.shift(t, time.Minute)
	m := f.monitor(t, root, false, "comment.created")

	cfg, err := pgxpool.ParseConfig(os.Getenv(testsupport.TestDatabaseURL))
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	tracer := &rcTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	h := NewAgentPollHandler(pool, rcLag)

	for _, tc := range []struct {
		name  string
		query string
		want  int64
	}{
		{"cursor", rcEpoch, n0Cursor + 1},
		{"live-start", "", n0LiveStart + 1},
	} {
		tracer.n.Store(0)
		if rec := doPoll(t, h, m, tc.query); rec.Code != http.StatusOK {
			t.Fatalf("%s poll status = %d", tc.name, rec.Code)
		}
		if got := tracer.n.Load(); got != tc.want {
			t.Errorf("%s poll sent %d statements, want %d (N₀ + 1)", tc.name, got, tc.want)
		}
	}
}

// TestAgentPoll_RootClosedCounter — matrix row 21: +1 on the poll that
// records the root-closed state, +0 on later polls and on rootClosed=false.
func TestAgentPoll_RootClosedCounter(t *testing.T) {
	f := newRCFixture(t)
	closedRoot := f.issue(t, "")
	openRoot := f.issue(t, "")
	f.close(t, closedRoot, f.otherAct)
	f.shift(t, time.Minute)
	closedMon := f.monitor(t, closedRoot, false, "comment.created")
	openMon := f.monitor(t, openRoot, false, "comment.created")
	before := testutil.ToFloat64(monitorcreds.RootClosedTotal)

	if p := f.poll(t, openMon, rcEpoch); p.RootClosed {
		t.Fatalf("open root reported closed")
	}
	if d := testutil.ToFloat64(monitorcreds.RootClosedTotal) - before; d != 0 {
		t.Fatalf("rootClosed=false moved the counter by %v", d)
	}
	p := f.poll(t, closedMon, rcEpoch)
	if !p.RootClosed {
		t.Fatalf("closed root not reported")
	}
	if d := testutil.ToFloat64(monitorcreds.RootClosedTotal) - before; d != 1 {
		t.Fatalf("the recording poll moved the counter by %v, want 1", d)
	}
	f.poll(t, closedMon, p.next())
	if d := testutil.ToFloat64(monitorcreds.RootClosedTotal) - before; d != 1 {
		t.Fatalf("a later poll of a finished monitor moved the counter: total %v, want 1", d)
	}
}
