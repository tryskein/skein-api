// Package handler — dashboard surface (sk-17).
//
// Two endpoints power the new /activity surface in the SPA:
//
//   - GET /api/v1/dashboard/scoring — throughput matrix + leaderboards,
//     a 3-axis (human / agent_directed / agent_autonomous) × 3-window
//     (24h / 7d / 30d) grid. Visibility-filtered to the caller's
//     project_members horizon (admins see all). Optional project=<uuid>
//     and user=<username> filters narrow the scope.
//
//   - GET /api/v1/dashboard/feed — paginated activity feed across every
//     visible project. Returns the same ActivityEntry shape used by the
//     existing /me/activity endpoint so the frontend can render rows with
//     the same components.
//
// The live-updating "ticker" zone is fed by the existing SignalR
// firehose published by skein-web NatsBridge — every project-scoped
// event lands on the connected user's hub via the per-project
// visibility check already enforced by MembershipCache. The frontend
// subscribes to the existing `event` SignalR channel and buffers
// incoming envelopes into the ticker; no new NATS subject is required.
//
// This architectural choice is deliberate: a parallel firehose group
// (`firehose-{userId}`) was the spec's first draft, but the existing
// NatsBridge already does per-user visibility filtering on every event
// and a second SignalR group would duplicate that work. Reusing the
// existing channel keeps the audit trail single-source and removes
// the need to mirror the visibility logic in a second place.
package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/dashboard"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/redact"
)

// feedRedactLevel maps a war-room caller to the redaction level applied
// to rows whose project the caller cannot see (sk-163.18). It is the
// single source of the unrestricted/external/internal decision shared by
// the Feed and InProgress handlers:
//
//   - unrestricted (declared internal work, authz) → redact.None;
//   - external → redact.Hard (content AND every stable id);
//   - any other caller → redact.Soft (content only — unchanged internal
//     war-room). An admin is such a caller: its reach is the access
//     relation's (sk-674.18 T010), so a project the relation shows it is
//     never redacted, and nothing here reads the role.
//
// The value is passed explicitly to the store (no zero-value default);
// the store's redaction helpers are fail-closed on any unhandled level.
func feedRedactLevel(unrestricted, isExternal bool) redact.Level {
	switch {
	case unrestricted:
		return redact.None
	case isExternal:
		return redact.Hard
	default:
		return redact.Soft
	}
}

// hotZoneReach resolves the caller's HOT ZONE reach (US-13, sk-674.18
// T030) from the access relation — the visible projects (shown in full)
// and the contribution of his own non-personal spaces (counted, rows
// anonymized, INV-28) — narrowed by the page filter `space=<shortId>`, which
// never widens it. A space outside the caller's reach yields an empty
// reach: every surface answers with nothing.
func (h *DashboardHandler) hotZoneReach(r *http.Request) (authz.HotZoneReach, error) {
	ctx := r.Context()
	return authz.ReachFor(ctx, h.pool, authz.FromContext(ctx), strings.TrimSpace(r.URL.Query().Get("space")))
}

// DashboardHandler wires the dashboard scoring + feed endpoints. The
// store is sk-17-specific; the caller's visible set comes from authz;
// issuesStore is reused for the activity feed reads (same shape as the
// /me/activity endpoint).
type DashboardHandler struct {
	pool        *pgxpool.Pool
	store       *dashboard.Store
	issuesStore *issues.Store
}

// NewDashboardHandler constructs the handler. All deps are required.
func NewDashboardHandler(
	pool *pgxpool.Pool,
	store *dashboard.Store,
	issuesStore *issues.Store,
) *DashboardHandler {
	return &DashboardHandler{
		pool:        pool,
		store:       store,
		issuesStore: issuesStore,
	}
}

// Scoring handles GET /api/v1/dashboard/scoring — the throughput matrix
// + leaderboards zone for the /activity dashboard.
//
// Query params:
//   - window:  24h | 7d | 30d | all-time (empty defaults to 24h; invalid → 400)
//   - project: optional UUID; narrows to one project (visibility-checked,
//     same gate as every other /projects/{id}/* endpoint)
//   - user:    optional username string (max 64 chars); narrows to one
//     actor — no membership cross-check, an unknown username just yields
//     zero rows
//
// Scope: the caller's HOT ZONE reach (US-13, sk-674.18 T030 —
// authz.HotZoneReach): the visible projects and the contribution of his own
// non-personal spaces; nothing of another space or another "Personal".
// `?space=` narrows the reach to one space, `?project=` to one project.
//
// Response shape: dashboard.ScoringResult (see dashboard/store.go).
//
// @Summary Throughput matrix and leaderboards
// @Description Aggregates a 3-axis (human / agent_directed / agent_autonomous) x 3-window (24h / 7d / 30d) grid of issue-lifecycle metrics (issues_created, issues_accepted, issues_closed, issues_closed_done, issues_closed_no_work, comments, and more — see dashboard.ScoringResult) plus per-actor leaderboards. Algorithm: resolves the caller's visible-project set, builds ONE project SQL predicate from (the HOT ZONE reach, unrestricted, projectFilter) via buildVisibility, and reuses that single clause across every per-metric aggregate query — so a project filter that reaches this handler reaches every metric identically. All-time (window=all-time) results are cached in-memory for 60s, keyed on (projectFilter, usernameFilter, the reach's fingerprint — a project deleted or out of reach changes the key, K-6). Scope (US-13, sk-674.18 T030): the caller's HOT ZONE reach — the projects he sees, in full, plus the projects of his own non-personal spaces he does not see, which contribute to every aggregate and appear only anonymized in row surfaces (INV-28); an external caller (no spaces) gets his own projects only; an instance admin gets every non-personal space and his own "Personal"; projects of other spaces and other people's "Personal" spaces contribute nothing — no row, no number (INV-23). `space=<shortId>` narrows the reach to one space and never widens it; `user=` is a filter, never a widening. The optional `project` filter is the opt-in narrowing to one project, gated by the same visibility check as every other /projects/{id}/* endpoint — a project the caller cannot see returns 404 with no distinction from a nonexistent id. The optional `user` filter narrows to one actor's own events with no membership cross-check (an unknown username simply yields zero rows).
// @Tags Dashboard
// @Produce json
// @Security ApiKeyAuth
// @Param window query string false "Rolling window: 24h | 7d | 30d | all-time (default 24h)"
// @Param project query string false "Optional project UUID; narrows the matrix to one project (visibility-checked — a project the caller cannot see returns 404, indistinguishable from a nonexistent one)"
// @Param user query string false "Optional username (max 64 chars); narrows to one actor's own events"
// @Param space query string false "Optional space short id; narrows the HOT ZONE reach to that space (never widens it)"
// @Success 200 {object} dashboard.ScoringResult
// @Failure 400 {object} map[string]any "invalid_query — window malformed, or project not a UUID; validation_error — user longer than 64 chars"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr; not actually reachable through EnsureProjectVisible today, but the shared mapper can write it)"
// @Failure 404 {object} map[string]any "not_found — the given project does not exist or the caller cannot see it"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/dashboard/scoring [get]
func (h *DashboardHandler) Scoring(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()
	windowStr := q.Get("window")
	if windowStr == "" {
		windowStr = "24h"
	}
	window, ok := dashboard.ParseWindow(windowStr)
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid_query",
			"window must be one of 24h, 7d, 30d, all-time", nil)
		return
	}

	var projectFilter *uuid.UUID
	if raw := q.Get("project"); raw != "" {
		pid, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_query",
				"project must be a UUID", nil)
			return
		}
		// Verify caller can see this project. Same gate as every
		// other /projects/{id}/* endpoint — non-members get 404 so
		// we don't leak existence.
		if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
			if mapAccessErr(w, err) {
				return
			}
			slog.ErrorContext(ctx, "dashboard.scoring: project visibility check failed",
				"projectId", pid.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
		projectFilter = &pid
	}

	var usernameFilter *string
	if raw := q.Get("user"); raw != "" {
		// clamp username length before binding to SQL. The
		// schema's username regex tops out at 30 chars
		// (project guidelines), but allow 64 here as a generous
		// soft cap so callers passing email-prefixes-as-handles still
		// work. Anything beyond is rejected early instead of wasting
		// bandwidth + pgx bind cycles.
		u := strings.TrimSpace(raw)
		if len(u) > 64 {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"user must be 64 characters or fewer", nil)
			return
		}
		// No projects-membership cross-check on the username filter;
		// the result is scoped by visible projects already, so a
		// nonsense username simply yields zero rows.
		if u != "" {
			usernameFilter = &u
		}
	}

	hz, err := h.hotZoneReach(r)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.scoring: load visible projects failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	visible, unrestricted := hz.Visible, hz.Unrestricted

	// sk-674.18 T030 (US-13): the aggregates count the caller's HOT ZONE
	// reach — visible projects and the contribution of his own non-personal
	// spaces — and nothing of another space or another "Personal".
	res, err := h.store.Scoring(ctx, window, visible, unrestricted, projectFilter, usernameFilter, hz.Reach)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.scoring: aggregation failed",
			"userId", c.UserID.String(), "window", windowStr, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Feed handles GET /api/v1/dashboard/feed.
//
// Query params:
//   - limit:  1..100 (default 100 for dashboard rendering)
//   - cursor: opaque cursor returned by a previous response
//
// Scope: the caller's HOT ZONE reach (US-13, sk-674.18 T030). Unlike
// /me/activity (which CUTS rows to the visible projects), this surface also
// returns the rows of the projects of the caller's own non-personal spaces
// he does not see, REDACTED (redact.Soft: content stripped, identity kept —
// INV-28 (2)); an external caller has no such projects; nothing of another
// space or another "Personal" is returned. `?space=` narrows the reach.
//
// The frontend blurs the row and disables the link for redacted rows. The
// frontend renders the initial page and then prepends SignalR-pushed
// envelopes into the same ticker.
func (h *DashboardHandler) Feed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	hz, err := h.hotZoneReach(r)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.feed: load visible projects failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	visible, unrestricted := hz.Visible, hz.Unrestricted

	limit, cursor, err := parseActivityArgs(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_query", err.Error(), nil)
		return
	}
	if limit == 50 {
		// parseActivityArgs defaults to 50, but the dashboard ticker
		// wants 100 — bump only when the caller didn't pass an
		// explicit limit.
		if r.URL.Query().Get("limit") == "" {
			limit = 100
		}
	}

	// sk-674.18 T030 (US-13): rows of the caller's HOT ZONE reach; rows of
	// a contributing project (in the reach, not visible) are redacted
	// server-side at `level`.
	//
	// sk-163.18 — an EXTERNAL caller now sees the global feed too, but every
	// inaccessible-project row is HARD-redacted (id/prefix/projectId dropped,
	// not just the title). Internal non-admins keep the unchanged SOFT
	// war-room; admins see everything unredacted.
	level := feedRedactLevel(unrestricted, c.IsExternal)
	items, next, err := h.issuesStore.ListUserActivity(
		ctx, c.UserID, visible, unrestricted, nil, cursor, limit, hz.Reach, level)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.feed: list activity failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeActivityResult(w, items, next)
}

// InProgress handles GET /api/v1/dashboard/in-progress — snapshot of
// currently-active work for the "now working" zone on /activity.
// Scope: the caller's HOT ZONE reach (US-13, sk-674.18 T030): the
// in-progress issues of the projects he sees, in full, and of the projects
// of his own non-personal spaces he does not see, with the title redacted
// (redacted=true, identity kept — INV-28 (2)); nothing of another space or
// another "Personal". Optional space=<shortId> narrows the reach, project=<uuid>
// to one project (visibility-checked at the handler).
//
// Query params:
//   - project: optional UUID; narrows to one project (visibility-checked
//     — a project the caller cannot see returns 404 project_not_found,
//     distinct from Scoring/Race's shared `not_found` code)
//   - limit:   1..200 (default 50; values above 200 are clamped, not
//     rejected)
//
// Refresh model is dual: the frontend issues an initial fetch on
// mount + a refetch on a 30s interval as a fallback, AND subscribes
// to the existing SignalR `event` channel and invalidates the query
// on issue.claimed / issue.unclaimed / issue.status_changed /
// issue.accepted / issue.closed. The intersection of those event
// types covers every transition that changes the in-progress set.
// SignalR delivers the trigger; this endpoint serves the truth.
//
// @Summary In-progress work snapshot
// @Description Lists issues currently in progress (status='in_progress' OR locked_by IS NOT NULL, excluding closed) across every project, ordered by the most recent of locked_at/started_at/accepted_at/updated_at, most recent first. Algorithm: resolves the caller's visible-project set, builds ONE visibility predicate on issues.project_id (project filter pins to one project; otherwise the caller's HOT ZONE reach — rows of contributing projects are content-redacted after the fact), then a single query joins issues to projects and to users (case-insensitive assignee lookup). Scope (US-13, sk-674.18 T030): the caller's HOT ZONE reach — the projects he sees, in full, plus the projects of his own non-personal spaces he does not see, which contribute to every aggregate and appear only anonymized in row surfaces (INV-28); an external caller (no spaces) gets his own projects only; an instance admin gets every non-personal space and his own "Personal"; projects of other spaces and other people's "Personal" spaces contribute nothing — no row, no number (INV-23). `space=<shortId>` narrows the reach to one space and never widens it; `user=` is a filter, never a widening. A row of a project the caller cannot see (a contributing project) has its title redacted (redacted=true, identity fields kept). The optional `project` filter is the opt-in narrowing to one project, gated by the same visibility check as every other /projects/{id}/* endpoint — a project the caller cannot see returns 404 with no distinction from a nonexistent id.
// @Tags Dashboard
// @Produce json
// @Security ApiKeyAuth
// @Param project query string false "Optional project UUID; narrows the snapshot to one project (visibility-checked — a project the caller cannot see returns 404, indistinguishable from a nonexistent one)"
// @Param limit query int false "Max rows to return, 1..200 (default 50; values above 200 are clamped rather than rejected)"
// @Param space query string false "Optional space short id; narrows the HOT ZONE reach to that space (never widens it)"
// @Success 200 {object} dashboard.InProgressResult
// @Failure 400 {object} map[string]any "validation_error — project not a UUID, or limit not a positive integer"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 404 {object} map[string]any "project_not_found — the given project does not exist or the caller cannot see it"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/dashboard/in-progress [get]
func (h *DashboardHandler) InProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()
	var projectFilter *uuid.UUID
	if raw := q.Get("project"); raw != "" {
		pid, perr := uuid.Parse(raw)
		if perr != nil {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"project must be a uuid", nil)
			return
		}
		// Project visibility check — same pattern as Scoring. Admin
		// pass-through is handled inside EnsureProjectVisible.
		if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
			if errors.Is(err, projects.ErrProjectNotVisible) {
				writeProblem(w, http.StatusNotFound, "project_not_found", "", nil)
				return
			}
			slog.ErrorContext(ctx, "dashboard.in_progress: visibility check failed",
				"userId", c.UserID.String(), "projectId", pid.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
		projectFilter = &pid
	}

	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n <= 0 {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"limit must be a positive integer", nil)
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}

	hz, err := h.hotZoneReach(r)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.in_progress: load visible projects failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	visible, unrestricted := hz.Visible, hz.Unrestricted

	// sk-674.18 T030 (US-13): the snapshot covers the caller's HOT ZONE
	// reach; rows of a contributing project are redacted at `level`.
	//
	// sk-163.18 — an EXTERNAL caller now sees the global snapshot too, but
	// every inaccessible-project row is HARD-redacted (issue id, project
	// prefix, projectId and lockedBySession dropped; assignee/status/timing
	// kept). Internal non-admins keep the unchanged SOFT war-room; admins
	// see everything unredacted.
	level := feedRedactLevel(unrestricted, c.IsExternal)
	res, err := h.store.InProgress(ctx, visible, unrestricted, projectFilter, limit, hz.Reach, level)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.in_progress: query failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Race handles GET /api/v1/dashboard/race — per-user cumulative
// leaderboard for one metric over a rolling time window. Powers the
// "race chart" zone at the top of /activity.
//
// Query params:
//   - metric:  closed | closed_done | commits | created | comments
//     (default "closed"; sk-494 — closed_done narrows to closes whose
//     outcome is "done", closed keeps counting every close)
//   - window:  7d | 30d | 90d | all (default "30d")
//   - topN:    1..20 (default 8)
//   - project: optional UUID; narrows to one project (visibility-checked,
//     same gate as Scoring's `?project=`)
//
// Scope: the caller's HOT ZONE reach (US-13, sk-674.18 T030): the chart
// aggregates per-user cumulative counts over the visible projects and the
// contribution of his own non-personal spaces — nothing of another space or
// another "Personal". `?space=` and `?project=` narrow it.
//
// Cold-start clamp: when `now() - window` is older than the first event
// in the DB, the effective start sticks at firstEventInDb and
// isColdStart=true so the frontend can label «since X».
//
// Response shape: dashboard.RaceResult.
//
// @Summary Per-user cumulative leaderboard ("race chart")
// @Description Aggregates one metric (issue closes, issue commits, issue creations, or comments) per user into a cumulative day-by-day series for the "race chart" zone at the top of /activity. Algorithm: resolves firstEventInDb (global MIN(occurred_at)) and clamps effectiveStart to max(now-window, firstEventInDb) — or to firstEventInDb itself for window=all — then computes per-user totals over [effectiveStart, effectiveEnd] via a metric-specific query: closed/closed_done/created/comments read the events table filtered by type (closed_done additionally requires the close event's payload outcome to be done — sk-494, "closed" is not "done"); commits reads issue_commits JOIN issues, counting one row per linked SHA (not per issue close, and via a visibility predicate keyed on the issue's project rather than an event's). Totals are split into the top `topN` users plus a single "others" aggregate; per-day cumulative counts for both are then computed in Go over a dense day series, carrying forward the previous day's value on inactive days so every polyline is monotonic non-decreasing. Cold-start clamp: when now()-window predates the first event in the DB, effectiveStart sticks at firstEventInDb and isColdStart=true so the frontend can label "since X". Scope (US-13, sk-674.18 T030): the caller's HOT ZONE reach — the projects he sees, in full, plus the projects of his own non-personal spaces he does not see, which contribute to every aggregate and appear only anonymized in row surfaces (INV-28); an external caller (no spaces) gets his own projects only; an instance admin gets every non-personal space and his own "Personal"; projects of other spaces and other people's "Personal" spaces contribute nothing — no row, no number (INV-23). `space=<shortId>` narrows the reach to one space and never widens it; `user=` is a filter, never a widening. The optional `project` filter is the opt-in narrowing to one project, gated by the same visibility check as every other /projects/{id}/* endpoint — a project the caller cannot see returns 404 with no distinction from a nonexistent id.
// @Tags Dashboard
// @Produce json
// @Security ApiKeyAuth
// @Param metric query string false "Metric to rank by: closed | closed_done | commits | created | comments (default closed). closed_done counts only closes whose close outcome is done (sk-494); closed counts every close at any outcome."
// @Param window query string false "Rolling window: 7d | 30d | 90d | all (default 30d)"
// @Param topN query int false "Number of top users broken out individually, 1..20 (default 8); the remainder is aggregated into a single 'others' line"
// @Param project query string false "Optional project UUID; narrows the leaderboard to one project (visibility-checked — a project the caller cannot see returns 404, indistinguishable from a nonexistent one)"
// @Param space query string false "Optional space short id; narrows the HOT ZONE reach to that space (never widens it)"
// @Success 200 {object} dashboard.RaceResult
// @Failure 400 {object} map[string]any "invalid_query — metric, window, topN, or project malformed or out of range"
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 403 {object} map[string]any "forbidden — the project is visible but the caller may not act on it (mapAccessErr; not actually reachable through EnsureProjectVisible today, but the shared mapper can write it)"
// @Failure 404 {object} map[string]any "not_found — the given project does not exist or the caller cannot see it"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/dashboard/race [get]
func (h *DashboardHandler) Race(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	q := r.URL.Query()

	metricStr := q.Get("metric")
	if metricStr == "" {
		metricStr = "closed"
	}
	metric, ok := dashboard.ParseRaceMetric(metricStr)
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid_query",
			"metric must be one of closed, closed_done, commits, created, comments", nil)
		return
	}

	windowStr := q.Get("window")
	if windowStr == "" {
		windowStr = "30d"
	}
	window, ok := dashboard.ParseRaceWindow(windowStr)
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid_query",
			"window must be one of 7d, 30d, 90d, all", nil)
		return
	}

	topN := 8
	if raw := q.Get("topN"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 || n > 20 {
			writeProblem(w, http.StatusBadRequest, "invalid_query",
				"topN must be an integer between 1 and 20", nil)
			return
		}
		topN = n
	}

	var projectFilter *uuid.UUID
	if raw := q.Get("project"); raw != "" {
		pid, perr := uuid.Parse(raw)
		if perr != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_query",
				"project must be a UUID", nil)
			return
		}
		// Verify caller can see this project. Same gate as Scoring's
		// `?project=` and every other /projects/{id}/* endpoint —
		// non-members get 404 so we don't leak existence.
		if _, err := projects.EnsureProjectVisible(ctx, h.pool, pid, c.UserID); err != nil {
			if mapAccessErr(w, err) {
				return
			}
			slog.ErrorContext(ctx, "dashboard.race: project visibility check failed",
				"projectId", pid.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
		projectFilter = &pid
	}

	hz, err := h.hotZoneReach(r)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.race: load visible projects failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	visible, unrestricted := hz.Visible, hz.Unrestricted

	res, err := h.store.Race(ctx, dashboard.RaceArgs{
		Metric:          metric,
		Window:          window,
		TopN:            topN,
		VisibleProjects: visible,
		Unrestricted:    unrestricted,
		// sk-674.18 T030 (US-13): the caller's HOT ZONE reach, unless
		// ProjectFilter narrows it to one project.
		Reach:         hz.Reach,
		ProjectFilter: projectFilter,
	})
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.race: aggregation failed",
			"userId", c.UserID.String(),
			"metric", metricStr, "window", windowStr,
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Projects handles GET /api/v1/dashboard/projects — per-project activity
// leaderboard for the HOT ZONE.
//
// Query params:
//   - window: 24h | 7d | 30d | all (default "24h" on BOTH empty AND
//     invalid input — unlike Scoring/Race/InProgress, which 400 on an
//     invalid `window`, this endpoint silently falls back to 24h per the
//     approved sk-183.1 plan; ParseProjectsWindow's ok=false path is
//     folded into the default rather than surfaced as a client error).
//
// Redaction is project-identity-only (sk-183.1) — a THIRD shape distinct
// from redact.Soft/Hard: a row whose project the caller cannot see has
// its projectId/projectName/projectPrefix stripped and redacted=true,
// but every count and every username (contributors[], topActor) stays
// populated exactly as an admin would see it. This is a THIRD, narrower redaction
// shape, distinct from the existing org-wide counts+usernames (zero
// redaction) and the Hard-redaction field table precedents. The `all` summary row is a global aggregate across every visible-or-not
// project and is never redacted, carrying no project-identity fields at
// all (omitted/zero, not stripped from a populated value).
//
// Response shape: dashboard.ProjectsResult.
//
// @Summary Per-project activity leaderboard
// @Description Aggregates four metrics (events, commits, issuesOpened, issuesClosed) per project over the requested window, plus a derived activeAgents count and a topActor pick — each with a per-contributor username+count breakdown capped at 8 entries (otherContributorsCount signals more exist). Algorithm: four narrow queries group raw rows by (project, contributor) — events groups every events-table row; commits groups issue_commits by linked_by (the real users.id FK, not the free-text git author string), joined to issues for project_id; issuesOpened groups issues by created_by; issuesClosed derives from events(type='issue.closed') since issues has no closed_by column. activeAgents is carved out of the already-fetched events rows (actor_type='agent'), not a fifth query. Ranking/capping/otherContributorsCount are computed in Go over the small per-metric result sets. Rows are sorted by events.count DESC. Redaction: rows of a contributing project (in the reach, not visible to the caller) have projectId/projectName/projectPrefix stripped and redacted=true — every count and every username (including contributors[] and topActor) remains; spaceId (the caller's own space) remains. The `all` field aggregates the caller's reach, never redacted, with no project-identity fields at all. Scope (US-13, sk-674.18 T030): the caller's HOT ZONE reach — the projects he sees, in full, plus the projects of his own non-personal spaces he does not see, which contribute to every aggregate and appear only anonymized in row surfaces (INV-28); an external caller (no spaces) gets his own projects only; an instance admin gets every non-personal space and his own "Personal"; projects of other spaces and other people's "Personal" spaces contribute nothing — no row, no number (INV-23). `space=<shortId>` narrows the reach to one space and never widens it; `user=` is a filter, never a widening.
// @Tags Dashboard
// @Produce json
// @Security ApiKeyAuth
// @Param window query string false "Rolling window: 24h | 7d | 30d | all (default 24h; invalid values also fall back to 24h rather than erroring)"
// @Param space query string false "Optional space short id; narrows the HOT ZONE reach to that space (never widens it)"
// @Success 200 {object} dashboard.ProjectsResult
// @Failure 401 {object} map[string]any "unauthenticated"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/dashboard/projects [get]
func (h *DashboardHandler) Projects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	windowStr := r.URL.Query().Get("window")
	window, ok := dashboard.ParseProjectsWindow(windowStr)
	if !ok {
		// Deliberate: default rather than 400 on an invalid/empty window —
		// see the doc comment above.
		window = dashboard.ProjectsWindow24h
	}

	hz, err := h.hotZoneReach(r)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.projects: load visible projects failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	visible, unrestricted := hz.Visible, hz.Unrestricted

	res, err := h.store.Projects(ctx, window, visible, unrestricted, hz.Reach)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.projects: aggregation failed",
			"userId", c.UserID.String(), "window", windowStr, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Agents handles GET /api/v1/dashboard/agents — per-user MCP-session
// counts across the four windows (live 15m + 24h + 7d + 30d) for the
// /activity dashboard's `agents` zone. Polled every 30s on the
// frontend; the underlying agents.Upsert is throttled at 60s so the
// effective freshness is ~60-90s (one MCP request per agent per
// minute keeps last_seen_at green, polling catches it within 30s).
//
// No SignalR push integration — the agents table has no event
// emission today (Upsert is silent), so the polling path is the
// canonical refresh.
func (h *DashboardHandler) Agents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Authentication is still required; the caller's external flag no
	// longer gates this surface (see comment below).
	if _, ok := caller(r); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	// The agents zone is a per-username MCP-session leaderboard with NO
	// project dimension and NO titles/ids. sk-674.18 T030 (US-13): it
	// counts the agents of the people of the caller's HOT ZONE reach only —
	// no longer every agent of the installation. sk-755: every session of
	// such a person counts by its last appearance anywhere, not by its work
	// in the reach.
	hz, err := h.hotZoneReach(r)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.agents: load the reach failed", "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	res, err := h.store.Agents(ctx, hz.Reach, hz.Unrestricted)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard.agents: query failed",
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
