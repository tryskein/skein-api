package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterPrime wires the skein_prime tool. Composite session-priming
// read: caller identity, in-progress + ready + blocked issue lists, and
// recent activity events. Designed to be called once at the start of a
// working session.
//
// Annotations:
//
//   - readOnlyHint: true
//   - destructiveHint: false
//   - idempotentHint: true
//   - openWorldHint: false
//
// _meta:
//
//   - anthropic/alwaysLoad: true — needed every time the agent boots a
//     session; deferring through Tool Search would force an extra
//     round-trip on every conversation.
//
//   - anthropic/maxResultSizeChars: 100000 — kept, and now with a number
//     behind it: three issue lists at the schema ceiling of 25 rows each,
//     every row carrying a 1000-character title and ten labels, plus the
//     activity feed, serialise to 32 280 bytes (measured 2026-08-20,
//     throwaway harness on the integration database). Roughly a third of
//     the declaration.
//
//     Not lowered to fit that: the label count per row has no ceiling, so
//     the bound is open-ended above the measurement, and an over-budget
//     result on the one tool that loads in EVERY session is the worst place
//     to discover a file reference instead of an answer (result_size.go).
func RegisterPrime(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store, depsStore *dependencies.Store, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_prime",
		mcpgo.WithDescription(
			"Session priming: returns caller identity (user + agent), the "+
				"caller's in-progress issues, top ready (unblocked) issues "+
				"assigned to or available for the caller, currently blocked "+
				"issues, and recent activity events the caller is involved in. "+
				"Call this ONCE at the start of a working session before any "+
				"other Skein tool — the response is the agent's orientation "+
				"snapshot. By default the three issue lists carry the LEAN "+
				"row described below and never the issue card, and the scan "+
				"spans every project you can see; optional `project` narrows "+
				"every section to one.\n\n"+
				"What the rows are, because this tool loads on every session "+
				"and it is worth knowing before you build on it: all three "+
				"issue lists carry the same LEAN row skein_list returns, "+
				"here in cross-project form — "+
				IssueRowDefaultFieldsSentence(true)+
				" — and never "+
				"the issue card. Keys outside that named set are absent on all "+
				"three lists, `componentIds` among them. There is no `fields` "+
				"here and no verbose mode: read one issue whole with "+
				"skein_show, or take the wider row from skein_list, whose "+
				"`fields` can name the columns this tool never carries.\n\n"+
				"One documented exception on top of that, and it is an "+
				"ABSENCE rather than an empty value: rows in `readyForMe` "+
				"carry NO `labels` key at all. That list comes from the "+
				"readiness query, which does not load labels (the join would "+
				"make it N+1). Returning an empty array would assert the issue "+
				"HAS no labels — a different and usually false statement — so "+
				"the key is absent instead, and «not supplied here» stays "+
				"distinguishable from «none». Read labels with skein_show on "+
				"the issue, or with skein_list, which does load them. "+
				"Everything else on that row is present, `isBlocked` included: "+
				"it is always false there, because unblocked is what the query "+
				"selected on.",
		),
		mcpgo.WithString("project",
			mcpgo.Description("Optional project handle (alias|name|UUID) to scope every section to one project. Default: all visible projects."),
		),
		mcpgo.WithString("component",
			mcpgo.Description("Optional component UUID to scope myInProgress/readyForMe/blocked to issues attached to this component. Must belong to a visible project."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Maximum rows per section, 1..25. Default: 10."),
			mcpgo.Min(1),
			mcpgo.Max(25),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/alwaysLoad":         true,
		"anthropic/maxResultSizeChars": 100000,
	})

	addTool(s, tool, handlePrime(pool, issuesStore, depsStore, projectsStore))
}

// primeResponse is the wire shape skein_prime returns. Stable contract —
// fields are additive only across releases. Slice fields are always
// non-nil so the agent can iterate without nil-checks.
type primeResponse struct {
	User  primeUser   `json:"user"`
	Agent *primeAgent `json:"agent,omitempty"`
	Scope primeScope  `json:"scope"`
	// The three issue lists are PROJECTED rows (prime_rows.go), not the
	// carrier structs: US-12 puts all three under the US-2 named field
	// set, with the «ready to take» carrier's unloaded fields removed as
	// keys. RecentActivity is a different shape and US-12 declares it
	// Unchanged, so it passes through untouched.
	MyInProgress   []map[string]any      `json:"myInProgress"`
	ReadyForMe     []map[string]any      `json:"readyForMe"`
	Blocked        []map[string]any      `json:"blocked"`
	RecentActivity []issues.UserEventDTO `json:"recentActivity"`
	Summary        string                `json:"summary"`
}

type primeUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	IsAdmin  bool   `json:"isAdmin"`
}

// primeAgent carries the agent identity echo. SessionID is the
// cross-system join key — also written to events.actor_session_id for
// every action this agent performs, so an external system holding the
// conversation log can join it to skein audit by string-matching.
type primeAgent struct {
	SessionID string `json:"sessionId"`
}

type primeScope struct {
	Project   *primeScopeProject   `json:"project"`
	Component *primeScopeComponent `json:"component"`
}

type primeScopeProject struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IDPrefix string `json:"idPrefix"`
}

type primeScopeComponent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func handlePrime(pool *pgxpool.Pool, issuesStore *issues.Store, depsStore *dependencies.Store, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		// Section limit. Spec says default 10, max 25.
		limit := req.GetInt("limit", 10)
		if limit < 1 {
			limit = 10
		}
		if limit > 25 {
			limit = 25
		}

		// 1) Caller identity. Pull username + email + role from users in
		//    one short SELECT — primary-key lookup. Failure here is fatal:
		//    without a username the primeUser block would render useless
		//    to the agent. We re-read role from DB rather than trusting
		//    ctx so a config drift in the api-key middleware can't hide
		//    the discrepancy.
		var puser primeUser
		puser.ID = userID.String()
		var dbRole string
		var dbIsExternal bool
		err := pool.QueryRow(ctx, `
SELECT username, email, role, is_external
  FROM users
 WHERE id = $1`, userID).Scan(&puser.Username, &puser.Email, &dbRole, &dbIsExternal)
		if errors.Is(err, pgx.ErrNoRows) {
			return skmcp.ToolError(ErrCodeUnauthenticated,
				"user resolved from the API key no longer exists", nil), nil
		}
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_prime",
				"reason", "user lookup failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load user", nil), nil
		}
		// Admin-ceiling derivation (sk-163 S2): re-read is_external from the
		// same row as role so a tampered/stale ctx can't matter — an external
		// user is never admin regardless of the stored role.
		puser.IsAdmin = auth.IsAdminRole(dbRole, dbIsExternal)

		// 2) Agent identity. SessionID may be empty on the very first
		//    request of a session (spec — initialize hasn't returned the
		//    id yet). When empty, we elide the agent block entirely
		//    (mirrors whoami's pointer-with-omitempty policy) so agents
		//    don't faithfully echo an empty string into chat per the
		//    initialize.txt instructions and pollute the string-equality
		//    join key. We deliberately omit api-key
		//    context here to match whoami's policy: the agent already
		//    has the plain key locally, and a server-side echo would add
		//    noise plus a small info-leak surface. SessionID is the
		//    cross-system join key.
		var pagent *primeAgent
		if sid := auth.SessionIDFrom(ctx); sid != "" {
			pagent = &primeAgent{SessionID: sid}
		}

		// 3) Resolve optional project arg.
		scope := primeScope{}
		var scopeProjectID *uuid.UUID
		if rawProject := strings.TrimSpace(req.GetString("project", "")); rawProject != "" {
			pid, perr := ResolveProjectHandle(ctx, pool, projectsStore, rawProject, userID)
			if perr != nil {
				return projectErrorResult(perr)
			}
			scopeProjectID = &pid
			// Pull a slim {id, name, idPrefix} summary for the scope
			// echo. Failures are non-fatal — log + nil-out the block.
			var sp primeScopeProject
			sp.ID = pid.String()
			if err := pool.QueryRow(ctx, `
SELECT name, id_prefix
  FROM projects
 WHERE id = $1`, pid).Scan(&sp.Name, &sp.IDPrefix); err != nil {
				slog.WarnContext(ctx, "skein_prime: scope project lookup failed",
					"userId", userID.String(), "projectId", pid.String(),
					"error", err.Error())
			} else {
				scope.Project = &sp
			}
		}

		// 4) Resolve optional component arg. Must be a UUID; visibility
		//    requires the parent project to be visible to the caller.
		var scopeComponentID *uuid.UUID
		if rawComp := strings.TrimSpace(req.GetString("component", "")); rawComp != "" {
			cid, parseErr := uuid.Parse(rawComp)
			if parseErr != nil {
				return skmcp.ToolError(ErrCodeValidationError,
					"component must be a UUID",
					map[string]any{"component": rawComp}), nil
			}
			// Look up the component to find its project, then check
			// visibility. We don't reach for projectsStore.GetComponent
			// because we don't yet have the project id in hand.
			var compProject uuid.UUID
			var compName string
			err := pool.QueryRow(ctx, `
SELECT project_id, name
  FROM components
 WHERE id = $1
   AND archived_at IS NULL`, cid).Scan(&compProject, &compName)
			if errors.Is(err, pgx.ErrNoRows) {
				return skmcp.ToolError(ErrCodeForbidden,
					"component does not exist or is not visible",
					map[string]any{"component": rawComp}), nil
			}
			if err != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_prime",
					"reason", "component lookup failed",
					"userId", userID.String(), "componentId", cid.String(), "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to load component", nil), nil
			}
			if _, vErr := projects.EnsureProjectVisible(ctx, pool, compProject, userID); vErr != nil {
				return skmcp.ToolError(ErrCodeForbidden,
					"component does not exist or is not visible",
					map[string]any{"component": rawComp}), nil
			}
			// If both project and component args were given, enforce
			// they agree — otherwise the agent's mental model is
			// inconsistent and we'd silently apply the wrong scope.
			if scopeProjectID != nil && *scopeProjectID != compProject {
				return skmcp.ToolError(ErrCodeValidationError,
					"component does not belong to the supplied project",
					map[string]any{"component": rawComp}), nil
			}
			scopeComponentID = &cid
			scope.Component = &primeScopeComponent{ID: cid.String(), Name: compName}
		}

		// 5) The caller's horizon (authz) for the cross-project reads, and
		//    the projects the per-project section loaders walk: every
		//    project of the visible set that holds a non-closed task
		//    assigned to the caller (the three sections are all assignee
		//    sections), whatever the size of the set.
		horizon := authz.FromContext(ctx)
		fanoutProjectIDs, err := resolveFanoutProjects(ctx, pool, horizon, scopeProjectID, puser.Username)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_prime",
				"reason", "project enumeration failed",
				"userId", userID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to enumerate projects", nil), nil
		}

		// 6) Run the three issue queries + recent events in parallel.
		//    Each section degrades to empty + a logged warning on its
		//    own error so an outage on one path doesn't kill priming.
		var (
			wg             sync.WaitGroup
			myInProgress   []issues.IssueSummary
			readyForMe     []dependencies.IssueSummary
			blocked        []issues.IssueSummary
			recentActivity []issues.UserEventDTO
		)

		// Username for assignee match — issues.assignee is stored as a
		// free-form string of the username, so we filter by puser.Username.
		assigneeUsername := puser.Username

		wg.Add(4)
		go func() {
			defer wg.Done()
			items, err := loadAssignedIssues(ctx, issuesStore, fanoutProjectIDs, assigneeUsername, scopeComponentID, []string{issues.StatusInProgress}, nil, limit)
			if err != nil {
				slog.WarnContext(ctx, "skein_prime: in-progress section failed",
					"userId", userID.String(), "error", err.Error())
				return
			}
			myInProgress = items
		}()

		go func() {
			defer wg.Done()
			items, err := loadReadyForMe(ctx, depsStore, fanoutProjectIDs, assigneeUsername, scopeComponentID, limit)
			if err != nil {
				slog.WarnContext(ctx, "skein_prime: ready section failed",
					"userId", userID.String(), "error", err.Error())
				return
			}
			readyForMe = items
		}()

		go func() {
			defer wg.Done()
			// sk-31: blocked is no longer a status — it's the EXISTS
			// predicate on outgoing blocks/waits-for edges. Same fan-out
			// helper as in-progress; we pass isBlocked=true and
			// constrain status to non-closed so we don't surface
			// blocked-then-closed rows.
			isBlocked := true
			items, err := loadAssignedIssues(ctx, issuesStore, fanoutProjectIDs, assigneeUsername, scopeComponentID,
				[]string{issues.StatusOpen, issues.StatusInProgress, issues.StatusDeferred}, &isBlocked, limit)
			if err != nil {
				slog.WarnContext(ctx, "skein_prime: blocked section failed",
					"userId", userID.String(), "error", err.Error())
				return
			}
			blocked = items
		}()

		go func() {
			defer wg.Done()
			items, err := issuesStore.RecentEventsForUser(ctx, userID, horizon, scopeProjectID, limit)
			if err != nil {
				slog.WarnContext(ctx, "skein_prime: recent events section failed",
					"userId", userID.String(), "error", err.Error())
				return
			}
			recentActivity = items
		}()
		wg.Wait()

		// Always emit non-nil slices.
		if myInProgress == nil {
			myInProgress = []issues.IssueSummary{}
		}
		if readyForMe == nil {
			readyForMe = []dependencies.IssueSummary{}
		}
		if blocked == nil {
			blocked = []issues.IssueSummary{}
		}
		if recentActivity == nil {
			recentActivity = []issues.UserEventDTO{}
		}

		// sk-282 T018: all three issue lists go through the US-2 row
		// projection. The counters come from the same fragment every other
		// surface uses, so «how many children» reads the same here as in
		// skein_list and on the card.
		inProgressRows, perr := projectPrimeIssueRows(ctx, pool, myInProgress, horizon)
		if perr != nil {
			return primeProjectionError(ctx, userID.String(), perr), nil
		}
		blockedRows, perr := projectPrimeIssueRows(ctx, pool, blocked, horizon)
		if perr != nil {
			return primeProjectionError(ctx, userID.String(), perr), nil
		}
		readyRows, perr := projectPrimeReadyRows(ctx, pool, readyForMe, horizon)
		if perr != nil {
			return primeProjectionError(ctx, userID.String(), perr), nil
		}

		resp := primeResponse{
			User:           puser,
			Agent:          pagent,
			Scope:          scope,
			MyInProgress:   inProgressRows,
			ReadyForMe:     readyRows,
			Blocked:        blockedRows,
			RecentActivity: recentActivity,
			// The summary is rendered from the CARRIERS, not the projected
			// rows: it counts sections, and counting a map's keys instead
			// would couple prose to the field set.
			Summary: renderPrimeSummary(myInProgress, readyForMe, blocked, recentActivity),
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_prime",
			"userId", userID.String(),
			"inProgress", len(myInProgress),
			"ready", len(readyForMe),
			"blocked", len(blocked),
			"events", len(recentActivity),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(resp)
	}
}

// loadAssignedIssues runs ListIssues with assignee=username and the given
// statuses across the supplied projectIDs (one query per project). The
// project set is resolved upstream by resolveFanoutProjects.
//
// componentID (when set) is pushed into ListIssuesArgs as an EXISTS
// filter so the SQL itself returns only matching rows — no post-filter,
// no silent truncation when many issues at the front of the order don't
// match the component.
//
// isBlocked (when non-nil) narrows by the derived blocked-ness flag
// (sk-31) — pass &true for the prime "blocked" section, nil for the
// status-only path used by the in-progress section. Consolidates the
// previously-duplicated loadBlockedAssignedIssues helper.
func loadAssignedIssues(
	ctx context.Context,
	store *issues.Store,
	projectIDs []uuid.UUID,
	assignee string,
	componentID *uuid.UUID,
	statuses []string,
	isBlocked *bool,
	limit int,
) ([]issues.IssueSummary, error) {
	if assignee == "" {
		return nil, nil
	}
	if len(projectIDs) == 0 {
		return []issues.IssueSummary{}, nil
	}

	// Fan out one query per project to honour the existing single-
	// project ListIssues contract; the loop is bounded by the supplied
	// projectIDs and we early-exit when we have `limit` items.
	out := make([]issues.IssueSummary, 0, limit)
	for _, pid := range projectIDs {
		if len(out) >= limit {
			break
		}
		args := issues.ListIssuesArgs{
			ProjectID:   pid,
			Statuses:    statuses,
			Assignee:    &assignee,
			ComponentID: componentID,
			IsBlocked:   isBlocked,
			Limit:       limit,
		}
		res, err := store.ListIssues(ctx, args)
		if err != nil {
			return nil, fmt.Errorf("list issues for project %s: %w", pid, err)
		}
		for _, item := range res.Items {
			out = append(out, item)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// resolveFanoutProjects produces the project-id set the section loaders
// iterate over:
//
//   - scope set → exactly one project ([scope]);
//   - otherwise → every project of the caller's visible set (authz, the
//     request's Scope.Bind) holding a non-closed task assigned to the
//     caller, most recently touched first.
//
// The three task sections (myInProgress, readyForMe, blocked) all filter
// on the assignee, so no project outside this set can contribute a row,
// and none inside it is left out: the sections cover the whole visible
// set, however wide, while the fan-out is bounded by the
// projects the caller actually has work in. One query on the assignee
// index; no cut by the number of visible projects, no branch by role.
func resolveFanoutProjects(
	ctx context.Context,
	pool *pgxpool.Pool,
	horizon authz.Scope,
	scope *uuid.UUID,
	assignee string,
) ([]uuid.UUID, error) {
	if scope != nil {
		return []uuid.UUID{*scope}, nil
	}
	if assignee == "" {
		return nil, nil
	}
	unrestricted, visible, err := horizon.Bind(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("binding the visible set for the fan-out: %w", err)
	}
	rows, err := pool.Query(ctx, `
SELECT i.project_id
  FROM issues i
 WHERE i.assignee = $1
   AND i.status <> 'closed'
   AND `+authz.ProjectVisibleExpr("i", "$2", "$3")+`
 GROUP BY i.project_id
 ORDER BY max(i.updated_at) DESC, i.project_id`, assignee, unrestricted, visible)
	if err != nil {
		return nil, fmt.Errorf("listing projects for the fan-out: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var pid uuid.UUID
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("scanning a fan-out project: %w", err)
		}
		out = append(out, pid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading fan-out projects: %w", err)
	}
	return out, nil
}

// primeProjectionError logs and renders the one failure mode the row
// projection has: a carrier that cannot supply a field of the default set
// without declaring it not-provided. That is a wiring bug, not a caller
// error, so it surfaces as internal_error rather than silently dropping
// the field.
func primeProjectionError(ctx context.Context, userID string, err error) *mcpgo.CallToolResult {
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_prime",
		"reason", "row projection failed",
		"userId", userID, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil)
}

// loadReadyForMe fans out ListReady across the supplied projects,
// scoped to the assignee. componentID (when set) is pushed into
// ReadyFilters as an EXISTS filter so the SQL itself returns only
// matching rows.
func loadReadyForMe(
	ctx context.Context,
	depsStore *dependencies.Store,
	projectIDs []uuid.UUID,
	assignee string,
	componentID *uuid.UUID,
	limit int,
) ([]dependencies.IssueSummary, error) {
	if len(projectIDs) == 0 {
		return []dependencies.IssueSummary{}, nil
	}

	filters := dependencies.ReadyFilters{}
	if assignee != "" {
		filters.Assignee = &assignee
	}
	if componentID != nil {
		filters.ComponentID = componentID
	}

	out := make([]dependencies.IssueSummary, 0, limit)
	for _, pid := range projectIDs {
		if len(out) >= limit {
			break
		}
		items, err := depsStore.ListReady(ctx, pid, filters, limit)
		if err != nil {
			return nil, fmt.Errorf("list ready for project %s: %w", pid, err)
		}
		for _, item := range items {
			out = append(out, item)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// renderPrimeSummary builds the deterministic one-paragraph natural-
// language summary. Same input → same string. We deliberately do NOT
// include "now" — only the latest event timestamp leaks into the text
// (and even that is sourced directly from the rendered event, not
// from time.Now()).
func renderPrimeSummary(
	inProgress []issues.IssueSummary,
	ready []dependencies.IssueSummary,
	blocked []issues.IssueSummary,
	events []issues.UserEventDTO,
) string {
	var b strings.Builder
	b.Grow(128)
	fmt.Fprintf(&b, "You have %d in-progress, %d ready, %d blocked.",
		len(inProgress), len(ready), len(blocked))
	if len(events) > 0 {
		latest := events[0]
		fmt.Fprintf(&b, " Latest activity: %s on %s",
			latest.Type, latest.IssueID)
		if latest.Title != "" {
			fmt.Fprintf(&b, " (%s)", latest.Title)
		}
		fmt.Fprintf(&b, " by %s at %s.",
			latest.Actor, latest.OccurredAt.UTC().Format(time.RFC3339))
	} else {
		b.WriteString(" No recent activity.")
	}
	return b.String()
}
