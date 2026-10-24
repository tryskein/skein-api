package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
)

// AgentPollHandler serves the /agent/v1 data plane for the agent task
// monitor (sk-142.4). It is mounted under RequireInternalSecret +
// RequireMonitorToken, so by the time a request reaches Poll the monitor
// claims (scope issue, subtree flag, event-type whitelist, owner user) are
// already validated and stashed in the request context — the handler reads
// scope from the token, never from the query string.
type AgentPollHandler struct {
	pool      *pgxpool.Pool
	store     *notifications.Store
	safetyLag time.Duration
}

// NewAgentPollHandler builds the handler with a notifications.Store over the
// shared pool. safetyLag is the config-driven watermark fence
// (MonitorPollSafetyLag) passed through to every poll. The pool itself is
// retained (not just wrapped in the store) so Poll can run the fail-closed
// visibility recheck (owner user-lookup + projects.EnsureProjectVisible,
// INV-8) against the same shared pool the store uses.
func NewAgentPollHandler(pool *pgxpool.Pool, safetyLag time.Duration) *AgentPollHandler {
	return &AgentPollHandler{
		pool:      pool,
		store:     notifications.NewStore(pool),
		safetyLag: safetyLag,
	}
}

// agentPollCursor is the watermark cursor echoed on every poll response. The
// runner passes ts/id straight back as ?since_ts=&since_id= on the next poll.
type agentPollCursor struct {
	TS time.Time `json:"ts"`
	ID int64     `json:"id"`
}

// agentPollResponse is the wire shape of GET /agent/v1/notifications/poll.
type agentPollResponse struct {
	// Notifications are the new in-scope events, ascending (occurred_at, id).
	Notifications []notifications.NotificationDTO `json:"notifications"`
	// Cursor is where the next poll resumes.
	Cursor agentPollCursor `json:"cursor"`
	// RootClosed is true once the server has recorded that this monitor is
	// finished because its root issue is closed and every notification owed
	// before that close has been delivered (sk-579, INV-12). A runner that
	// sees true stops with root_closed. Once recorded it stays true — a later
	// reopen of the root does not revive the monitor. Older runners ignore
	// the field.
	RootClosed bool `json:"rootClosed"`
}

// Poll handles GET /agent/v1/notifications/poll.
//
// @Summary      Poll for new scoped notifications (agent monitor)
// @Description  Returns the calling monitor token's owner's fresh,
// @Description  in-scope, in-type events in ascending (occurred_at, id)
// @Description  order, plus a cursor to resume from. Scope (issue +
// @Description  subtree) and the event-type whitelist are taken from the
// @Description  validated monitor token — NOT from the query — so a runner
// @Description  cannot widen its own scope.
// @Description
// @Description  Source (sk-164): rows come from the primary `events` audit
// @Description  log scoped to the token, NOT from the personal
// @Description  `notifications` inbox — so delivery no longer depends on a
// @Description  per-recipient watcher row and works between any two distinct
// @Description  sessions of the same owner. Consequently `id` and `cursor.id`
// @Description  are `events.id` (were `notifications.id`) and the timestamps
// @Description  are `events.occurred_at`; `readAt`/`archivedAt` are absent on
// @Description  this path (no inbox row). The JSON shape is otherwise
// @Description  unchanged, but the id/cursor namespace differs, so cursor
// @Description  continuity across a deploy is not guaranteed.
// @Description
// @Description  Each returned item carries `actorSessionId` (omitempty):
// @Description  present only when the originating event had a non-empty
// @Description  actor_session_id, absent otherwise. The monitor runner/agent
// @Description  uses it for reader-side self-skip — ignore an item whose
// @Description  actorSessionId equals the agent's own current MCP session
// @Description  (echo suppression, INV-2).
// @Description
// @Description  Comment-entity events (sk-242.1) additionally carry, omitempty:
// @Description  `commentId` (event comment UUID when parseable), `bodyPreview`
// @Description  (live comments.body snapshot, rune-truncated ≤1024; present
// @Description  empty string allowed), and `truncated` (true only when the
// @Description  live body exceeded the cap). Soft-deleted comments omit
// @Description  bodyPreview/truncated; full body is never on this stream.
// @Description  These fields are monitor-poll only — human inbox DTOs do not
// @Description  include them.
// @Description
// @Description  Query params: since_ts (RFC3339) + since_id (int) are the
// @Description  watermark cursor and must be supplied together; omit both
// @Description  for a live start (the server anchors the cursor at DB-side
// @Description  now() and returns no historical backlog). limit (default
// @Description  50) is clamped to [1,100].
// @Description
// @Description  Only events at least MonitorPollSafetyLag old are returned,
// @Description  bounding the bigserial commit-order skip race.
// @Description
// @Description  Visibility is re-checked fail-closed on every poll: if the
// @Description  token owner has lost visibility of the scoped project
// @Description  (removed from members and not an org-admin; archival does NOT
// @Description  gate), the poll returns 403 visibility_lost — a terminal
// @Description  signal the runner treats as end-of-channel, distinct from the
// @Description  401 credential-revocation path.
// @Description
// @Description  `rootClosed` (sk-579): true once the monitor is recorded as
// @Description  finished because its root issue is closed and everything it
// @Description  owed before the close has been delivered; the runner then
// @Description  stops. It stays true after a reopen of the root. Additive —
// @Description  older runners ignore it.
// @Description
// @Description  A 200 also records this monitor's successful poll
// @Description  (last_poll_succeeded_at, sk-579 INV-5) and decides the
// @Description  root-closed rule in one extra statement; if that statement
// @Description  fails the answer is 500 and nothing is recorded. A 400, 403
// @Description  or 500 never records a successful poll.
// @Tags         agent-monitor
// @Produce      json
// @Security     ApiKeyAuth
// @Param        since_ts  query  string  false  "Watermark cursor timestamp (RFC3339); requires since_id"
// @Param        since_id  query  int     false  "Watermark cursor id; requires since_ts"
// @Param        limit     query  int     false  "Max rows to return, 1..100 (default 50)"
// @Param        exclude_session  query  string  false  "Agent's current MCP session id for server best-effort echo suppression (rebind, FR-2). Narrow-only: events from this session are dropped from the body; empty/omitted disables narrowing. Never widens the result."
// @Success      200  {object}  agentPollResponse
// @Failure      400  {object}  map[string]any  "invalid_cursor | invalid_limit"
// @Failure      401  {object}  map[string]any  "unauthenticated"
// @Failure      403  {object}  map[string]any  "visibility_lost"
// @Failure      500  {object}  map[string]any  "internal_error"
// @Router       /agent/v1/notifications/poll [get]
func (h *AgentPollHandler) Poll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Defensive: RequireMonitorToken guarantees claims are present. A
	// missing claim here means the route was mounted without the middleware
	// — fail closed rather than serve unscoped data.
	claims, ok := auth.MonitorClaimsFrom(ctx)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	q := r.URL.Query()
	rawTS := strings.TrimSpace(q.Get("since_ts"))
	rawID := strings.TrimSpace(q.Get("since_id"))

	var sinceTS *time.Time
	var sinceID *int64
	switch {
	case rawTS == "" && rawID == "":
		// live-start — store anchors at DB-side now().
	case rawTS != "" && rawID != "":
		ts, err := time.Parse(time.RFC3339Nano, rawTS)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_cursor", "message": "since_ts must be an RFC3339 timestamp",
			})
			return
		}
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_cursor", "message": "since_id must be an integer",
			})
			return
		}
		sinceTS = &ts
		sinceID = &id
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_cursor", "message": "since_ts and since_id must be supplied together",
		})
		return
	}

	limit := 50
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_limit", "message": "limit must be a positive integer",
			})
			return
		}
		if n > 100 {
			n = 100
		}
		limit = n
	}

	// exclude_session (sk-164 FR-2 / INV-9) — the agent's re-declared current
	// MCP session for server best-effort echo suppression (rebind). Trimmed;
	// empty means "do not narrow" (the default — reader-side self-skip stays
	// authoritative). This can only ever NARROW the owner's own output: it does
	// not touch project/issue/subtree/type/floor/visibility, so any value
	// (valid, empty, forged, foreign) yields a subset of the no-rebind result.
	excludeSession := strings.TrimSpace(q.Get("exclude_session"))

	// Fail-closed visibility recheck (INV-8 / SC-006). Runs AFTER the cheap,
	// DB-free query-shape validation above (so a malformed request still
	// short-circuits to 400 without a DB round-trip — the documented "400
	// paths need no store" contract) but BEFORE any event is read: no data is
	// ever served without passing this gate.
	//
	// This gate is point-in-time PER POLL, deliberately NOT transactionally
	// atomic with the event read below. Spec INV-8 commits to exact parity with
	// the fan-out's per-event membership recheck — "no new gates beyond it"
	// (no gate stronger than the fan-out ever had). The residual TOCTOU window:
	// a membership/clamp change committing in the sub-ms gap between this check
	// and PollForMonitor's query snapshot can let ONE already-safety-lagged page
	// through, after which every subsequent poll 403s. Accepted by spec — it is
	// a strict improvement over the baseline `notifications` path, which had NO
	// per-poll recheck at all. Folding visibility INTO the event query would
	// close the window but duplicate EnsureProjectVisible's role/external-clamp
	// logic in SQL (two derivation points) for a stronger guarantee than INV-8
	// asks for — a worse trade than the single-derivation-point reuse below.
	//
	// The monitor tail reads the primary `events` audit log directly; unlike
	// the personal `notifications` inbox that path carries NO per-recipient
	// membership filter, so the fan-out membership recheck is no longer
	// implicit. Re-derive the owner's CURRENT visibility of the token's project
	// on every poll: an owner removed from the project (or role/clamp changed)
	// after the token was minted must stop receiving its events, even though
	// the token itself is still live. The predicate is exact parity with
	// projects.EnsureProjectVisible — membership OR internal-admin bypass with
	// the external-admin clamp; project archival does NOT gate. The monitor
	// claims carry neither role nor is_external, so both are resolved fresh
	// from `users`. This is distinct from the 401 revocation path
	// (ValidateAndRenew): there the credential is dead; here it is valid but
	// the owner simply lost sight of the project → 403.
	var ownerRole string
	var ownerIsExternal bool
	if err := h.pool.QueryRow(ctx,
		`SELECT role, is_external FROM users WHERE id = $1`, claims.UserID,
	).Scan(&ownerRole, &ownerIsExternal); err != nil {
		slog.ErrorContext(ctx, "monitor poll owner lookup failed",
			"userId", claims.UserID.String(),
			"scopeIssueId", claims.ScopeIssueID,
			"error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	// Stash is_external so EnsureProjectVisible's admin-ceiling clamp reads the
	// same flag the rest of the codebase does (single derivation point).
	ctx = auth.WithIsExternal(ctx, ownerIsExternal)
	if _, err := projects.EnsureProjectVisible(ctx, h.pool, claims.ProjectID, claims.UserID); err != nil {
		if errors.Is(err, projects.ErrProjectNotVisible) {
			slog.WarnContext(ctx, "monitor poll visibility lost",
				"userId", claims.UserID.String(),
				"scopeIssueId", claims.ScopeIssueID)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "visibility_lost"})
			return
		}
		slog.ErrorContext(ctx, "monitor poll visibility check failed",
			"userId", claims.UserID.String(),
			"scopeIssueId", claims.ScopeIssueID,
			"error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	pollParams := notifications.PollParams{
		UserID:         claims.UserID,
		ProjectID:      claims.ProjectID,
		ScopeIssueID:   claims.ScopeIssueID,
		ScopeSubtree:   claims.ScopeSubtree,
		EventTypes:     claims.EventTypes,
		SinceTS:        sinceTS,
		SinceID:        sinceID,
		Floor:          claims.CreatedAt,
		Limit:          limit,
		SafetyLag:      h.safetyLag,
		ExcludeSession: excludeSession,
	}
	res, err := h.store.PollForMonitor(ctx, pollParams)
	if err != nil {
		slog.ErrorContext(ctx, "monitor poll failed",
			"userId", claims.UserID.String(),
			"scopeIssueId", claims.ScopeIssueID,
			"error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	// sk-579 T012 — the one extra statement of a successful poll: record the
	// success (INV-5) and decide the root-closed rule (INV-12) over the same
	// admission predicate and the cursor this response returns. If it fails
	// the poll is not a success: 500, nothing recorded, the runner retries
	// with the same cursor.
	finish, err := h.store.FinishMonitorPoll(ctx, pollParams, claims.ID, res.CursorTS, res.CursorID)
	if errors.Is(err, notifications.ErrMonitorCredentialGone) {
		// The credential was purged or stopped after the door let this request
		// in — an expected race. Not a success; the next poll gets 401.
		slog.WarnContext(ctx, "monitor poll finish: credential gone or revoked",
			"userId", claims.UserID.String(),
			"monitorId", claims.ID.String(),
			"scopeIssueId", claims.ScopeIssueID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "monitor poll finish failed",
			"userId", claims.UserID.String(),
			"monitorId", claims.ID.String(),
			"scopeIssueId", claims.ScopeIssueID,
			"error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	if finish.NewlyClosed {
		monitorcreds.RootClosedTotal.Inc()
		slog.InfoContext(ctx, "monitor finished: root issue closed",
			"userId", claims.UserID.String(),
			"monitorId", claims.ID.String(),
			"scopeIssueId", claims.ScopeIssueID)
	}

	writeJSON(w, http.StatusOK, agentPollResponse{
		Notifications: res.Items,
		Cursor:        agentPollCursor{TS: res.CursorTS, ID: res.CursorID},
		RootClosed:    finish.RootClosed,
	})
}
