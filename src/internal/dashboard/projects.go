// sk-183.1 — per-project activity leaderboard for the HOT ZONE.
//
// GET /api/v1/dashboard/projects groups activity BY PROJECT rather than
// by user (the axis every other dashboard surface — Scoring, Race,
// Agents — uses). Four metrics are aggregated per (project, window):
//
//	events        — every row in the events table for the project
//	commits       — issue_commits linked in the window, JOINed to issues
//	                for project_id; contributor axis is issue_commits
//	                .linked_by (a real users.id FK), NOT the free-text
//	                git `author` column
//	issuesOpened  — issues.created_at in the window
//	issuesClosed  — derived from events (type='issue.closed'); the
//	                issues table has no closed_by column, only
//	                closed_at, so the actor comes from the closing
//	                event row
//
// activeAgents  — distinct MCP sessions (events.actor_session_id) that
//
//	produced at least one agent event in the project. This
//	IS a fifth query: the `events` aggregate above is
//	COUNT(*) grouped by (project, actor), which has already
//	thrown away session granularity, so COUNT(DISTINCT
//	session) cannot be derived from it (sk-214).
//
// The unit is the MCP session, matching the agents zone. It is NOT the
// count of distinct PEOPLE: before sk-214 this metric grouped by
// events.actor (a username), so it could never exceed the number of
// users in the installation — which on a small team is two orders of
// magnitude below the real session count over a 30-day window. Caveat inherent to the unit: Mcp-Session-Id rotates
// on every reconnect (mcp-go mints a fresh uuid per initialize), so one
// physical agent that reconnects five times counts five. There is no
// stable physical-agent id to dedupe by — X-Skein-Agent-Label was
// removed 2026-05-06 — and over-counting reconnects is the accepted
// trade against under-counting by ~200x. The UI footnote says so.
//
// topActor is the single highest-count contributor from the `events`
// metric per project — a simplification, not a separately weighted
// score (documented once here rather than at each call site).
//
// Redaction is a THIRD, narrower shape distinct from redact.Soft/Hard
// (see internal/redact):
// only project identity (name / id_prefix / id) is stripped from a row
// the caller cannot see. Every count and every username — including
// contributors[] and topActor — stays visible to every caller,
// regardless of project membership. This is an explicit product
// decision (see the issue description for sk-183.1), not an oversight;
// the existing redact.Level ladder is deliberately NOT reused because
// it strips fields (like lockedBySession) that don't exist on this
// surface and its semantics don't match what's needed here.
package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/person"
)

// ProjectsWindow enumerates the rolling windows /dashboard/projects
// exposes. Deliberately a distinct 4-value enum from Window ("all-time"
// string, no 24h in some callers' minds but it does have 24h — the
// difference is the "all" spelling) and from RaceWindow (7d/30d/90d/all,
// no 24h) — reusing either would force an incompatible wire string.
type ProjectsWindow string

const (
	ProjectsWindow24h ProjectsWindow = "24h"
	ProjectsWindow7d  ProjectsWindow = "7d"
	ProjectsWindow30d ProjectsWindow = "30d"
	ProjectsWindowAll ProjectsWindow = "all"
)

// IsAll returns true when the window means "since the first row ever".
func (w ProjectsWindow) IsAll() bool { return w == ProjectsWindowAll }

// Duration translates the wire string into a Go duration. ProjectsWindowAll
// has no meaningful duration — callers branch on IsAll() before calling
// Duration(); the "24h" fallback here is defence in depth only, mirroring
// Window.Duration's documented contract.
func (w ProjectsWindow) Duration() time.Duration {
	switch w {
	case ProjectsWindow24h:
		return 24 * time.Hour
	case ProjectsWindow7d:
		return 7 * 24 * time.Hour
	case ProjectsWindow30d:
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// ParseProjectsWindow validates a wire string and returns it as a
// ProjectsWindow. Returns ok=false for unknown inputs; the handler for
// this endpoint defaults to ProjectsWindow24h on both empty AND invalid
// input (unlike Scoring, which 400s on an invalid window) — see the
// handler doc comment on DashboardHandler.Projects for the rationale.
func ParseProjectsWindow(s string) (ProjectsWindow, bool) {
	switch ProjectsWindow(s) {
	case ProjectsWindow24h, ProjectsWindow7d, ProjectsWindow30d, ProjectsWindowAll:
		return ProjectsWindow(s), true
	}
	return "", false
}

// topContributorsCap bounds how many per-metric contributors are
// returned inline on each row; OtherContributorsCount signals how many
// more exist beyond the cap so the payload never silently truncates.
// Distinct from scoringLeaderboardLimit (10) on the Scoring podium —
// different surfaces, deliberately different caps (see store.go).
const topContributorsCap = 8

// ContributorDTO is one entry in a MetricValue's per-contributor
// breakdown — username + their raw count for that metric. UserID is
// nil for legacy/system rows that never resolved to a users.id.
type ContributorDTO struct {
	Username string     `json:"username"`
	UserID   *uuid.UUID `json:"userId,omitempty"`
	Count    int64      `json:"count"`
	// The person marks (sk-674.18 T030, spaces/contract.go): on a project
	// row — departed from that project's space, or deleted with the
	// pseudonym; on the `all` row — the deletion marks only.
	person.Status
}

// MetricValue is one metric cell on a ProjectActivityRow (or on the All
// summary row). Count is the metric total — including for ActiveAgents,
// whose contributor counts are per-user session counts, so their sum is
// the project's distinct-session total (a session belongs to exactly one
// user, so no double-count is possible on a project row).
// Contributors is capped at topContributorsCap, ranked by Count DESC;
// OtherContributorsCount is the number of additional distinct
// contributors beyond the cap (0 when everyone fit).
type MetricValue struct {
	Count                  int64            `json:"count"`
	Contributors           []ContributorDTO `json:"contributors"`
	OtherContributorsCount int64            `json:"otherContributorsCount,omitempty"`
}

// ProjectActivityRow is one project's activity leaderboard entry.
// ProjectID/ProjectName/ProjectPrefix are omitted (ProjectID nil) and
// Redacted is set true when the row belongs to a project the caller
// cannot see — see the package doc comment for why this is narrower
// than redact.Hard. Every metric and every contributor username stays
// populated regardless of Redacted.
type ProjectActivityRow struct {
	ProjectID     *uuid.UUID `json:"projectId,omitempty"`
	ProjectName   string     `json:"projectName,omitempty"`
	ProjectPrefix string     `json:"projectPrefix,omitempty"`
	// SpaceID is the short id of the space of the row's project (sk-674.18
	// T030). Kept on an anonymized row: such a row is a project of the
	// caller's own space (INV-28 (2)). Absent on the `all` row.
	SpaceID      string          `json:"spaceId,omitempty"`
	Redacted     bool            `json:"redacted"`
	Events       MetricValue     `json:"events"`
	Commits      MetricValue     `json:"commits"`
	IssuesOpened MetricValue     `json:"issuesOpened"`
	IssuesClosed MetricValue     `json:"issuesClosed"`
	ActiveAgents MetricValue     `json:"activeAgents"`
	TopActor     *ContributorDTO `json:"topActor,omitempty"`
}

// ProjectsResult is the wire shape returned by GET /dashboard/projects.
// All is a global aggregate across EVERY project regardless of caller
// visibility (same reasoning as the org-wide counts on Scoring/Race/
// Agents — see external-activity-blur.md §3a) — never redacted, and
// carries no project-identity fields at all (they're zero/omitted, not
// stripped from a populated value). Rows is sorted by Events.Count DESC.
type ProjectsResult struct {
	Window ProjectsWindow       `json:"window"`
	All    ProjectActivityRow   `json:"all"`
	Rows   []ProjectActivityRow `json:"rows"`
}

// projectContributorRow is the flat shape scanned out of each of the
// per-metric queries: one row per (project, contributor) pair, already
// summed by the query's GROUP BY.
type projectContributorRow struct {
	ProjectID uuid.UUID
	UserID    *uuid.UUID
	Name      string
	Count     int64
}

// projectAgentSessionRow is one DISTINCT (project, contributor, session)
// triple scanned out of projectsQueryAgentSessions. Deliberately NOT
// pre-aggregated in SQL: the same session may appear under several
// projects, and the All summary row must count it once, so the dedupe
// has to happen over the raw triples in Go rather than by summing the
// per-project rows (sk-214).
type projectAgentSessionRow struct {
	ProjectID uuid.UUID
	UserID    *uuid.UUID
	Name      string
	SessionID string
}

// contributorAgg is the Go-side accumulator used both per-project (for
// each row's MetricValue) and globally (for the All summary row).
type contributorAgg struct {
	UserID *uuid.UUID
	Name   string
	Count  int64
}

// contributorKey is the identity a contributor is aggregated under:
// UserID when the row resolved to a real users.id, else the lowercased
// display name. Mirrors the identity-matching approach Race already
// uses for the same user-id-may-be-nil situation.
type contributorKey struct {
	hasID bool
	id    uuid.UUID
	name  string
}

func keyOf(userID *uuid.UUID, name string) contributorKey {
	if userID != nil {
		return contributorKey{hasID: true, id: *userID}
	}
	return contributorKey{name: strings.ToLower(name)}
}

// Projects runs the sk-183.1 per-project activity aggregation for one
// window and redacts project identity on rows the caller cannot see.
//
// Implementation runs exactly FOUR round trips — one per metric family
// (events, commits, issuesOpened, issuesClosed) — mirroring the "narrow
// queries over one wide CTE" choice already made in Scoring: the join
// shapes diverge enough (events has no join at all; commits joins
// issue_commits→issues; issuesOpened/issuesClosed each anchor on a
// different table) that unifying them would either be unreadable or
// force the planner into a join it can't index well. activeAgents is
// derived from the events rows already fetched, not a fifth query.
//
// Ranking, capping at topContributorsCap, and the OtherContributorsCount
// signal are all computed in Go after scanning flat (project,
// contributor, count) rows — simpler to read and audit than a
// ROW_NUMBER()/json_agg pipeline for four small result sets, and this
// codebase already prefers that shape (see Race's per-day cumulative
// fold, also computed in Go over a small row set).
func (s *Store) Projects(
	ctx context.Context,
	window ProjectsWindow,
	visibleProjects map[uuid.UUID]struct{},
	unrestricted bool,
	// reach — the caller's HOT ZONE reach (sk-674.18 T030): only its
	// projects make rows and the `all` summary; rows outside
	// visibleProjects are anonymized (INV-28 (2)). nil — the visible
	// projects only.
	reach map[uuid.UUID]struct{},
) (*ProjectsResult, error) {
	var since time.Time
	if !window.IsAll() {
		since = s.now().UTC().Add(-window.Duration())
	}
	// window.IsAll(): since stays the Go zero time (year 0001 UTC), which
	// trivially satisfies every `>= $1` predicate below — same sentinel
	// trick Scoring's WindowAllTime uses.

	eventsRows, err := s.projectsQueryEvents(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("projects events: %w", err)
	}
	commitsRows, err := s.projectsQueryCommits(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("projects commits: %w", err)
	}
	issuesOpenedRows, err := s.projectsQueryIssuesOpened(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("projects issues_opened: %w", err)
	}
	issuesClosedRows, err := s.projectsQueryIssuesClosed(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("projects issues_closed: %w", err)
	}

	agentSessionRows, err := s.projectsQueryAgentSessions(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("projects agent_sessions: %w", err)
	}

	// sk-674.18 T030 (US-13, INV-23): nothing of a project outside the
	// reach — no row, no number, no contribution to `all`.
	if !unrestricted {
		set := filterSetOf(visibleProjects, reach)
		eventsRows = inReach(eventsRows, set)
		commitsRows = inReach(commitsRows, set)
		issuesOpenedRows = inReach(issuesOpenedRows, set)
		issuesClosedRows = inReach(issuesClosedRows, set)
		agentSessionRows = sessionsInReach(agentSessionRows, set)
	}

	eventsByProject := groupByProject(eventsRows)
	commitsByProject := groupByProject(commitsRows)
	openedByProject := groupByProject(issuesOpenedRows)
	closedByProject := groupByProject(issuesClosedRows)
	agentsByProject := groupByProject(countAgentSessionsPerProject(agentSessionRows))

	// Union of every project id that appears in ANY metric — a project
	// with zero rows across all four metrics in this window has no
	// activity to report and is simply absent from the leaderboard.
	projectIDs := make(map[uuid.UUID]struct{})
	for pid := range eventsByProject {
		projectIDs[pid] = struct{}{}
	}
	for pid := range commitsByProject {
		projectIDs[pid] = struct{}{}
	}
	for pid := range openedByProject {
		projectIDs[pid] = struct{}{}
	}
	for pid := range closedByProject {
		projectIDs[pid] = struct{}{}
	}

	marks, err := s.projectsLoadMarks(ctx, eventsRows, commitsRows, issuesOpenedRows, issuesClosedRows, agentSessionRows)
	if err != nil {
		return nil, fmt.Errorf("projects load_marks: %w", err)
	}

	names, err := s.projectsLoadNames(ctx, projectIDs)
	if err != nil {
		return nil, fmt.Errorf("projects load_names: %w", err)
	}

	rows := make([]ProjectActivityRow, 0, len(projectIDs))
	for pid := range projectIDs {
		name, ok := names[pid]
		if !ok {
			// Project row no longer exists (deleted or never had one —
			// events.project_id carries no FK by design, see db-schema.md
			// §events). Nothing to attach a name/prefix to and no way to
			// resolve visibility, so the row is dropped rather than
			// emitted with empty identity fields that would look like a
			// redacted row without actually being one.
			continue
		}
		row := ProjectActivityRow{
			ProjectID:     &pid,
			ProjectName:   name.name,
			ProjectPrefix: name.prefix,
			SpaceID:       name.space,
			Events:        buildMetricValue(eventsByProject[pid], marks.inProject(pid)),
			Commits:       buildMetricValue(commitsByProject[pid], marks.inProject(pid)),
			IssuesOpened:  buildMetricValue(openedByProject[pid], marks.inProject(pid)),
			IssuesClosed:  buildMetricValue(closedByProject[pid], marks.inProject(pid)),
			ActiveAgents:  buildMetricValue(agentsByProject[pid], marks.inProject(pid)),
			TopActor:      topContributor(eventsByProject[pid], marks.inProject(pid)),
		}
		if !unrestricted {
			if _, visible := visibleProjects[pid]; !visible {
				row.ProjectID = nil
				row.ProjectName = ""
				row.ProjectPrefix = ""
				row.Redacted = true
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Events.Count > rows[j].Events.Count
	})

	// All: the same underlying flat rows re-aggregated GLOBALLY (ignoring
	// project_id) rather than another set of queries — the per-project
	// grouping above already scanned every project's rows, so summing
	// across projects per contributor is pure Go-side work.
	//
	// ActiveAgents is the exception: it re-derives from the raw session
	// triples instead of groupGlobal'ing the per-project counts, because
	// summing those would count a session once per project it touched.
	// Hence All.ActiveAgents <= sum(rows[].ActiveAgents), by design.
	all := ProjectActivityRow{
		Events:       buildMetricValue(groupGlobal(eventsRows), marks.deletedOnly),
		Commits:      buildMetricValue(groupGlobal(commitsRows), marks.deletedOnly),
		IssuesOpened: buildMetricValue(groupGlobal(issuesOpenedRows), marks.deletedOnly),
		IssuesClosed: buildMetricValue(groupGlobal(issuesClosedRows), marks.deletedOnly),
		ActiveAgents: buildMetricValue(countAgentSessionsGlobal(agentSessionRows), marks.deletedOnly),
		TopActor:     topContributor(groupGlobal(eventsRows), marks.deletedOnly),
	}

	return &ProjectsResult{
		Window: window,
		All:    all,
		Rows:   rows,
	}, nil
}

// projectsQueryEvents counts every events row per (project, actor) in
// the window, tagging each with actor_type so the caller can carve out
// the agent subset for activeAgents without a second query.
func (s *Store) projectsQueryEvents(ctx context.Context, since time.Time) ([]projectContributorRow, error) {
	rows, err := s.pool.Query(ctx, `
SELECT project_id, actor_user_id, actor, COUNT(*) AS cnt
  FROM events
 WHERE occurred_at >= $1
   AND project_id IS NOT NULL
 GROUP BY project_id, actor_user_id, actor`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregating projects.events: %w", err)
	}
	defer rows.Close()
	out := make([]projectContributorRow, 0, 64)
	for rows.Next() {
		var r projectContributorRow
		if err := rows.Scan(&r.ProjectID, &r.UserID, &r.Name, &r.Count); err != nil {
			return nil, fmt.Errorf("scanning projects.events row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// projectsQueryAgentSessions returns every DISTINCT (project, actor,
// session) triple with at least one agent event in the window — the
// input to both the per-project activeAgents cell and the All row.
//
// Rows are NOT aggregated in SQL because the two consumers need
// different dedupe scopes (per-project vs org-wide over the same
// session), and the raw triple count is small — measured in thousands
// of rows for the unbounded `all` window against tens of thousands of
// agent events in 30 days, because a session touches ~1 project on
// average. Aggregating twice in Go over that is
// cheaper than a second round trip.
//
// actor_session_id IS NOT NULL filters out pre-session-tracking rows
// (zero of them among agent events today) — they carry no session to
// count and would otherwise collapse into one phantom NULL "session".
func (s *Store) projectsQueryAgentSessions(ctx context.Context, since time.Time) ([]projectAgentSessionRow, error) {
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT project_id, actor_user_id, actor, actor_session_id
  FROM events
 WHERE occurred_at >= $1
   AND actor_type = 'agent'
   AND project_id IS NOT NULL
   AND actor_session_id IS NOT NULL`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregating projects.activeAgents: %w", err)
	}
	defer rows.Close()
	out := make([]projectAgentSessionRow, 0, 64)
	for rows.Next() {
		var r projectAgentSessionRow
		if err := rows.Scan(&r.ProjectID, &r.UserID, &r.Name, &r.SessionID); err != nil {
			return nil, fmt.Errorf("scanning projects.activeAgents row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// projectsQueryCommits counts issue_commits linked in the window per
// (project, linked_by). Contributor axis is linked_by (a real users.id
// FK) — NOT the free-text git `author` column, which is an unmapped
// string with no corresponding user. linked_by is nullable
// (system-link, e.g. an automated webhook); those rows are attributed
// to a synthetic "system" contributor (UserID nil) rather than dropped,
// so the metric's total count still reflects every commit link.
func (s *Store) projectsQueryCommits(ctx context.Context, since time.Time) ([]projectContributorRow, error) {
	rows, err := s.pool.Query(ctx, `
SELECT i.project_id, ic.linked_by, COALESCE(u.username, 'system') AS name, COUNT(*) AS cnt
  FROM issue_commits ic
  JOIN issues i ON i.id = ic.issue_id
  LEFT JOIN users u ON u.id = ic.linked_by
 WHERE ic.linked_at >= $1
 GROUP BY i.project_id, ic.linked_by, COALESCE(u.username, 'system')`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregating projects.commits: %w", err)
	}
	defer rows.Close()
	out := make([]projectContributorRow, 0, 64)
	for rows.Next() {
		var r projectContributorRow
		if err := rows.Scan(&r.ProjectID, &r.UserID, &r.Name, &r.Count); err != nil {
			return nil, fmt.Errorf("scanning projects.commits row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// projectsQueryIssuesOpened counts issues.created_at in the window per
// (project, created_by). created_by is NOT NULL (FK required at issue
// creation), so a plain JOIN is safe.
func (s *Store) projectsQueryIssuesOpened(ctx context.Context, since time.Time) ([]projectContributorRow, error) {
	rows, err := s.pool.Query(ctx, `
SELECT i.project_id, i.created_by, u.username, COUNT(*) AS cnt
  FROM issues i
  JOIN users u ON u.id = i.created_by
 WHERE i.created_at >= $1
 GROUP BY i.project_id, i.created_by, u.username`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregating projects.issuesOpened: %w", err)
	}
	defer rows.Close()
	out := make([]projectContributorRow, 0, 64)
	for rows.Next() {
		var r projectContributorRow
		if err := rows.Scan(&r.ProjectID, &r.UserID, &r.Name, &r.Count); err != nil {
			return nil, fmt.Errorf("scanning projects.issuesOpened row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// projectsQueryIssuesClosed derives close counts from the events table
// rather than issues — the issues table has no closed_by column, only
// closed_at, so the actor of a
// close comes from the events row the close handler already writes.
func (s *Store) projectsQueryIssuesClosed(ctx context.Context, since time.Time) ([]projectContributorRow, error) {
	rows, err := s.pool.Query(ctx, `
SELECT project_id, actor_user_id, actor, COUNT(*) AS cnt
  FROM events
 WHERE type = 'issue.closed'
   AND entity_type = 'issue'
   AND occurred_at >= $1
   AND project_id IS NOT NULL
 GROUP BY project_id, actor_user_id, actor`, since)
	if err != nil {
		return nil, fmt.Errorf("aggregating projects.issuesClosed: %w", err)
	}
	defer rows.Close()
	out := make([]projectContributorRow, 0, 64)
	for rows.Next() {
		var r projectContributorRow
		if err := rows.Scan(&r.ProjectID, &r.UserID, &r.Name, &r.Count); err != nil {
			return nil, fmt.Errorf("scanning projects.issuesClosed row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// projectNameRow is the {name, id_prefix} pair used to hydrate a
// ProjectActivityRow's identity fields.
type projectNameRow struct {
	name   string
	prefix string
	space  string
}

// projectsLoadNames resolves name/id_prefix for exactly the project ids
// that appeared in at least one metric this call. Empty input returns
// an empty map without a round trip.
func (s *Store) projectsLoadNames(ctx context.Context, ids map[uuid.UUID]struct{}) (map[uuid.UUID]projectNameRow, error) {
	out := make(map[uuid.UUID]projectNameRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	list := make([]uuid.UUID, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	rows, err := s.pool.Query(ctx, `
SELECT p.id, p.name, p.id_prefix, sp.short_id
  FROM projects p JOIN spaces sp ON sp.id = p.space_id
 WHERE p.id = ANY($1)`, list)
	if err != nil {
		return nil, fmt.Errorf("loading project names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n projectNameRow
		if err := rows.Scan(&id, &n.name, &n.prefix, &n.space); err != nil {
			return nil, fmt.Errorf("scanning project name row: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// groupByProject buckets flat (project, contributor, count) rows by
// project id, sorting each project's contributor list by Count DESC
// (ties broken by Name ASC for deterministic output).
func groupByProject(rows []projectContributorRow) map[uuid.UUID][]contributorAgg {
	// Size hint: unique projects ≤ len(rows) (one row per project×contributor).
	out := make(map[uuid.UUID][]contributorAgg, len(rows))
	for _, r := range rows {
		out[r.ProjectID] = append(out[r.ProjectID], contributorAgg{
			UserID: r.UserID,
			Name:   r.Name,
			Count:  r.Count,
		})
	}
	for pid := range out {
		sortContributors(out[pid])
	}
	return out
}

// groupGlobal re-aggregates flat rows across EVERY project, summing a
// contributor's count wherever they appear (a user active in three
// projects gets one global row with the sum of all three). Identity key
// is UserID when present, else the lowercased display name — mirrors
// the identity-matching approach Race already uses for the same
// user-id-may-be-nil situation.
func groupGlobal(rows []projectContributorRow) []contributorAgg {
	agg := make(map[contributorKey]*contributorAgg)
	order := make([]contributorKey, 0, len(rows))
	for _, r := range rows {
		k := keyOf(r.UserID, r.Name)
		e, ok := agg[k]
		if !ok {
			e = &contributorAgg{UserID: r.UserID, Name: r.Name}
			agg[k] = e
			order = append(order, k)
		}
		e.Count += r.Count
	}
	out := make([]contributorAgg, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	sortContributors(out)
	return out
}

// sortContributors orders a contributor list by Count DESC, Name ASC —
// shared by both the per-project and the global aggregation paths so
// ranking is identical regardless of which one produced the slice.
func sortContributors(c []contributorAgg) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Count != c[j].Count {
			return c[i].Count > c[j].Count
		}
		return c[i].Name < c[j].Name
	})
}

// buildMetricValue caps an already-sorted contributor list at
// topContributorsCap and sums every contributor's count into the
// metric's headline total.
// marksFn returns the person marks of a contributor (nil user id: none).
type marksFn func(userID *uuid.UUID) person.Status

func noMarks(*uuid.UUID) person.Status { return person.Status{} }

func buildMetricValue(contributors []contributorAgg, marks marksFn) MetricValue {
	n := len(contributors)
	top := contributors
	var other int64
	if n > topContributorsCap {
		top = contributors[:topContributorsCap]
		other = int64(n - topContributorsCap)
	}
	dtos := make([]ContributorDTO, 0, len(top))
	var sum int64
	for _, c := range contributors {
		sum += c.Count
	}
	for _, c := range top {
		dtos = append(dtos, ContributorDTO{Username: c.Name, UserID: c.UserID, Count: c.Count, Status: marks(c.UserID)})
	}
	return MetricValue{Count: sum, Contributors: dtos, OtherContributorsCount: other}
}

// countAgentSessionsPerProject collapses raw (project, contributor,
// session) triples into one row per (project, contributor) whose Count
// is that contributor's DISTINCT session count in that project. Summing
// those rows per project (buildMetricValue) therefore yields the
// project's distinct-session total: a session has exactly one actor, so
// two contributors can never share one.
func countAgentSessionsPerProject(rows []projectAgentSessionRow) []projectContributorRow {
	type projectContributor struct {
		project uuid.UUID
		who     contributorKey
	}
	sessions := make(map[projectContributor]map[string]struct{})
	meta := make(map[projectContributor]projectAgentSessionRow)
	order := make([]projectContributor, 0, len(rows))

	for _, r := range rows {
		k := projectContributor{project: r.ProjectID, who: keyOf(r.UserID, r.Name)}
		if _, ok := sessions[k]; !ok {
			sessions[k] = make(map[string]struct{})
			meta[k] = r
			order = append(order, k)
		}
		sessions[k][r.SessionID] = struct{}{}
	}

	out := make([]projectContributorRow, 0, len(order))
	for _, k := range order {
		out = append(out, projectContributorRow{
			ProjectID: k.project,
			UserID:    meta[k].UserID,
			Name:      meta[k].Name,
			Count:     int64(len(sessions[k])),
		})
	}
	return out
}

// countAgentSessionsGlobal is the All-row counterpart: it dedupes each
// contributor's sessions across EVERY project before counting, so a
// session that touched three projects contributes 1, not 3. This is why
// the All row cannot be groupGlobal'd out of the per-project rows.
func countAgentSessionsGlobal(rows []projectAgentSessionRow) []contributorAgg {
	sessions := make(map[contributorKey]map[string]struct{})
	meta := make(map[contributorKey]projectAgentSessionRow)
	order := make([]contributorKey, 0, len(rows))

	for _, r := range rows {
		k := keyOf(r.UserID, r.Name)
		if _, ok := sessions[k]; !ok {
			sessions[k] = make(map[string]struct{})
			meta[k] = r
			order = append(order, k)
		}
		sessions[k][r.SessionID] = struct{}{}
	}

	out := make([]contributorAgg, 0, len(order))
	for _, k := range order {
		out = append(out, contributorAgg{
			UserID: meta[k].UserID,
			Name:   meta[k].Name,
			Count:  int64(len(sessions[k])),
		})
	}
	sortContributors(out)
	return out
}

// topContributor returns the #1 entry of an already-sorted contributor
// list (nil for an empty list) — the events-metric contributor with the
// highest count, i.e. TopActor.
func topContributor(contributors []contributorAgg, marks marksFn) *ContributorDTO {
	if len(contributors) == 0 {
		return nil
	}
	top := contributors[0]
	return &ContributorDTO{Username: top.Name, UserID: top.UserID, Count: top.Count, Status: marks(top.UserID)}
}

// inReach keeps the contributor rows of the projects in set.
func inReach(rows []projectContributorRow, set map[uuid.UUID]struct{}) []projectContributorRow {
	out := rows[:0:0]
	for _, r := range rows {
		if _, ok := set[r.ProjectID]; ok {
			out = append(out, r)
		}
	}
	return out
}

// sessionsInReach keeps the agent-session rows of the projects in set.
func sessionsInReach(rows []projectAgentSessionRow, set map[uuid.UUID]struct{}) []projectAgentSessionRow {
	out := rows[:0:0]
	for _, r := range rows {
		if _, ok := set[r.ProjectID]; ok {
			out = append(out, r)
		}
	}
	return out
}

// contributorMarks are the person marks of the contributors of one
// Projects read (sk-674.18 T030): per (project, user) pair in that
// project's context, and the deletion marks per user.
type contributorMarks struct {
	pair    map[[2]uuid.UUID]person.Status
	deleted map[uuid.UUID]person.Status
}

// inProject is the marks function of one project row.
func (m contributorMarks) inProject(pid uuid.UUID) marksFn {
	return func(uid *uuid.UUID) person.Status {
		if uid == nil {
			return person.Status{}
		}
		return m.pair[[2]uuid.UUID{pid, *uid}]
	}
}

// deletedOnly is the marks function of the `all` row, which has no single
// project to be departed from.
func (m contributorMarks) deletedOnly(uid *uuid.UUID) person.Status {
	if uid == nil {
		return person.Status{}
	}
	return m.deleted[*uid]
}

// projectsLoadMarks reads the marks of every (project, user) pair of the
// read's rows in one statement: person.StatusInProjectExpr per pair.
func (s *Store) projectsLoadMarks(
	ctx context.Context,
	events, commits, opened, closed []projectContributorRow,
	sessions []projectAgentSessionRow,
) (contributorMarks, error) {
	out := contributorMarks{pair: map[[2]uuid.UUID]person.Status{}, deleted: map[uuid.UUID]person.Status{}}
	seen := map[[2]uuid.UUID]struct{}{}
	var pids, uids []uuid.UUID
	add := func(pid uuid.UUID, uid *uuid.UUID) {
		if uid == nil {
			return
		}
		k := [2]uuid.UUID{pid, *uid}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		pids = append(pids, pid)
		uids = append(uids, *uid)
	}
	for _, set := range [][]projectContributorRow{events, commits, opened, closed} {
		for _, r := range set {
			add(r.ProjectID, r.UserID)
		}
	}
	for _, r := range sessions {
		add(r.ProjectID, r.UserID)
	}
	if len(pids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT pr.pid, pr.uid, `+person.StatusInProjectExpr("u", "pr.pid")+`
  FROM unnest($1::uuid[], $2::uuid[]) AS pr(pid, uid)
  JOIN users u ON u.id = pr.uid`, pids, uids)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid, uid uuid.UUID
		var st *person.Status
		if err := rows.Scan(&pid, &uid, &st); err != nil {
			return out, err
		}
		if st == nil {
			continue
		}
		out.pair[[2]uuid.UUID{pid, uid}] = *st
		if st.Deleted {
			out.deleted[uid] = *st
		}
	}
	return out, rows.Err()
}
