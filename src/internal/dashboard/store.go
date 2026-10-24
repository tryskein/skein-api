// Package dashboard provides aggregation queries for the sk-17 activity
// dashboard surface: a 3-axis (human / agent_directed / agent_autonomous)
// × 3-window (24h / 7d / 30d) throughput matrix plus two leaderboards
// (closed_human_driven, closed_with_commits).
//
// The store deliberately separates two concepts the events table conflates:
//
//   - actor_type (events.actor_type) is TRANSPORT: how the row landed
//     (human via cookie-session, agent via MCP api-key, system via
//     migrations). NOT the intent dimension.
//   - intent (issues.source, comments.author_kind) is INTENT: did a human
//     author or instruct, or did an agent act autonomously?
//
// For issue.* and comment.* events we JOIN to the parent issue/comment
// row to read the intent axis. For dep_add / status_changed / claim /
// etc. there is no intent column in the schema — those metrics fall
// outside the axis split (we don't expose them in the v1 matrix; see
// metrics list below).
//
// Metric keys (matches the wire contract in dashboard.go and the
// frontend ActivityDashboard):
//
//	issues_created          — events.type='issue.created'
//	                          intent = issues.source
//	issues_closed           — events.type='issue.closed'
//	                          intent = issues.source
//	issues_closed_done      — sk-494: the subset of issues_closed whose
//	                          CLOSE OUTCOME is "done". This is
//	                          the "work actually finished" leg — see
//	                          closeOutcomeExpr for why the outcome is read
//	                          off the EVENT and not off the issue row.
//	issues_closed_no_work   — sk-494: the complementary subset — closes
//	                          whose outcome is "won't do" / "duplicate"
//	                          / "superseded". Per axis, and by construction,
//	                          issues_closed = issues_closed_done +
//	                          issues_closed_no_work.
//	issues_closed_with_commits
//	                        — events.type='issue.closed' (in window) AND the
//	                          closing issue has at least one issue_commits
//	                          link ever (EXISTS on issue_commits.issue_id =
//	                          events.entity_id; NOT bounded by linked_at —
//	                          sk-191, so a pre-window commit still counts)
//	                          intent = issues.source
//	comments_authored       — events.type='comment.created'
//	                          intent = comments.author_kind
//	comments_on_others_issues
//	                        — comments_authored where the author is NOT
//	                          the assignee of the issue at the time of
//	                          the comment (best-effort: we check
//	                          assignee on the current row; same-actor
//	                          self-comments are filtered out)
//	mentions_sent           — events.type IN ('issue.mention',
//	                          'comment.mention'). Intent of a mention is
//	                          modelled as the actor's transport axis
//	                          (mentions don't carry author_kind), so we
//	                          map actor_type='human' → human, agent →
//	                          agent_autonomous (the agent firehose; an
//	                          agent_directed mention is rare and would
//	                          show up here too — accepted limitation).
//	dep_edges_added         — events.type='dependency.created'.
//	                          intent is mapped from actor_type the same
//	                          way as mentions.
package dashboard

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/assignee"
	"skein-api/internal/redact"
)

// Window enumerates the rolling time windows the dashboard exposes.
type Window string

const (
	Window24h     Window = "24h"
	Window7d      Window = "7d"
	Window30d     Window = "30d"
	WindowAllTime Window = "all-time"
)

// Duration translates the wire string into a Go duration. Unknown values
// fall back to 24h — the handler is expected to validate before calling
// the store; this is defence in depth.
//
// WindowAllTime returns 0 as a sentinel — callers MUST branch on the
// WindowAllTime case before calling Duration (Scoring does this), so the
// zero is never used in time arithmetic on the happy path.
func (w Window) Duration() time.Duration {
	switch w {
	case Window24h:
		return 24 * time.Hour
	case Window7d:
		return 7 * 24 * time.Hour
	case Window30d:
		return 30 * 24 * time.Hour
	case WindowAllTime:
		return 0
	default:
		return 24 * time.Hour
	}
}

// ParseWindow validates a wire string and returns it as a Window. Returns
// ok=false for unknown inputs so the handler can emit a 400.
func ParseWindow(s string) (Window, bool) {
	switch Window(s) {
	case Window24h, Window7d, Window30d, WindowAllTime:
		return Window(s), true
	}
	return "", false
}

// RaceWindow enumerates the windows the race endpoint exposes — a
// parallel parser to ParseWindow because race takes 7d / 30d / 90d /
// all rather than 24h / 7d / 30d. Strings on the wire are echoed back
// verbatim in the response.
type RaceWindow string

const (
	RaceWindow7d  RaceWindow = "7d"
	RaceWindow30d RaceWindow = "30d"
	RaceWindow90d RaceWindow = "90d"
	RaceWindowAll RaceWindow = "all"
)

// Duration translates the race window into a Go duration. "all" has no
// duration — callers handle it specially via the IsAll() guard below.
func (w RaceWindow) Duration() time.Duration {
	switch w {
	case RaceWindow7d:
		return 7 * 24 * time.Hour
	case RaceWindow30d:
		return 30 * 24 * time.Hour
	case RaceWindow90d:
		return 90 * 24 * time.Hour
	default:
		// "all" or unknown — caller should pre-check IsAll() and not
		// call Duration() at all. Fallback keeps the code defensive.
		return 30 * 24 * time.Hour
	}
}

// IsAll returns true when the window means "since first event".
func (w RaceWindow) IsAll() bool { return w == RaceWindowAll }

// ParseRaceWindow validates the wire string and returns ok=false on
// unknown inputs so the handler can emit a 400.
func ParseRaceWindow(s string) (RaceWindow, bool) {
	switch RaceWindow(s) {
	case RaceWindow7d, RaceWindow30d, RaceWindow90d, RaceWindowAll:
		return RaceWindow(s), true
	}
	return "", false
}

// RaceMetric enumerates the four metrics the race endpoint supports.
type RaceMetric string

const (
	RaceMetricClosed RaceMetric = "closed"
	// RaceMetricClosedDone (sk-494) ranks by closes whose outcome is
	// "done". A SEPARATE metric rather than a narrowing of
	// RaceMetricClosed: the race echoes the metric name back in the
	// response and the chart labels the series with it, so «closed» has to
	// keep meaning closes.
	RaceMetricClosedDone RaceMetric = "closed_done"
	RaceMetricCommits    RaceMetric = "commits"
	RaceMetricCreated    RaceMetric = "created"
	RaceMetricComments   RaceMetric = "comments"
)

// ParseRaceMetric validates the wire string. ok=false on unknown.
func ParseRaceMetric(s string) (RaceMetric, bool) {
	switch RaceMetric(s) {
	case RaceMetricClosed, RaceMetricClosedDone,
		RaceMetricCommits, RaceMetricCreated, RaceMetricComments:
		return RaceMetric(s), true
	}
	return "", false
}

// MetricRow is one row of the throughput matrix — three counts split by
// intent axis. The frontend renders one row per Key.
//
// Key-specific notes (wire/OpenAPI consumers):
//   - comments_on_others_issues: "others" uses the issue's *current*
//     assignee (schema has no assignee snapshot at comment time).
//   - mentions_sent / dep_edges_added: actor_type='agent' is mapped
//     entirely to agent_autonomous (agent_directed folded in; see
//     aggregateActorIntent package rule).
type MetricRow struct {
	Key             string `json:"key"`
	Human           int64  `json:"human"`
	AgentDirected   int64  `json:"agent_directed"`
	AgentAutonomous int64  `json:"agent_autonomous"`
}

// LeaderboardEntry is one row in a top-N list — username + count.
type LeaderboardEntry struct {
	Username string `json:"username"`
	Count    int64  `json:"count"`
}

// MetricScope echoes the scope used to compute the response so the
// frontend can render the active filters back to the user without
// re-deriving them from the URL.
type MetricScope struct {
	ProjectID *uuid.UUID `json:"projectId,omitempty"`
	Username  *string    `json:"username,omitempty"`
}

// ScoringResult is the full response shape for GET /dashboard/scoring.
type ScoringResult struct {
	Window       string                        `json:"window"`
	Scope        MetricScope                   `json:"scope"`
	Metrics      []MetricRow                   `json:"metrics"`
	Leaderboards map[string][]LeaderboardEntry `json:"leaderboards"`
	// MatrixUsers carries the per-user breakdown driving the bottom
	// "metrics matrix" zone of the dashboard. One row per active user
	// in the window, sorted by closed_human_driven DESC. The frontend
	// can re-sort client-side without an extra round trip.
	MatrixUsers []MatrixUserRow `json:"matrixUsers"`
}

// MatrixUserRow is one row of the per-user matrix. Columns hold the same
// 3-axis split as MetricRow; the frontend renders columns according to
// what is sorted in.
type MatrixUserRow struct {
	Username                string `json:"username"`
	IssuesCreatedHuman      int64  `json:"issuesCreatedHuman"`
	IssuesCreatedDirected   int64  `json:"issuesCreatedDirected"`
	IssuesCreatedAutonomous int64  `json:"issuesCreatedAutonomous"`
	IssuesClosedHuman       int64  `json:"issuesClosedHuman"`
	IssuesClosedDirected    int64  `json:"issuesClosedDirected"`
	IssuesClosedAutonomous  int64  `json:"issuesClosedAutonomous"`
	// IssuesClosedDone* (sk-494) is the "done" subset of the three
	// IssuesClosed* cells on the SAME row — the closes that finished the
	// work, by the rule sk-489 normalised. The IssuesClosed* cells keep
	// counting every close at any outcome, so per axis
	// `IssuesClosedDone* <= IssuesClosed*`, and the difference is the
	// "closed without work" remainder the matrix row does not spell out
	// (the matrix has one cell per axis per metric; a fourth triple for
	// the remainder would add a column that is exactly a subtraction).
	IssuesClosedDoneHuman      int64 `json:"issuesClosedDoneHuman"`
	IssuesClosedDoneDirected   int64 `json:"issuesClosedDoneDirected"`
	IssuesClosedDoneAutonomous int64 `json:"issuesClosedDoneAutonomous"`
	ClosedWithCommitsHuman     int64 `json:"closedWithCommitsHuman"`
	ClosedWithCommitsDir       int64 `json:"closedWithCommitsDir"`
	ClosedWithCommitsAuto      int64 `json:"closedWithCommitsAuto"`
	CommentsHuman              int64 `json:"commentsHuman"`
	CommentsDirected           int64 `json:"commentsDirected"`
	CommentsAutonomous         int64 `json:"commentsAutonomous"`
	// TotalScore is human + agent_directed across issues_closed —
	// the default sort axis on the podium. Stored for the leaderboard
	// pickers without re-summing on the frontend.
	ClosedHumanDriven int64 `json:"closedHumanDriven"`
	// ClosedDoneHumanDriven (sk-494) is the same sum over the "done"
	// subset — the sort axis a surface picks when it wants to rank by work
	// finished rather than by tasks closed. Supplied for the same reason
	// ClosedHumanDriven is: so no consumer re-sums two cells by hand.
	//
	// The store's OWN ordering does NOT change — rows still come back
	// sorted by ClosedHumanDriven DESC. Re-sorting server-side would move
	// which 50 users survive the LIMIT, i.e. silently change an existing
	// answer; which axis the table sorts by is the surface's call, and it
	// already re-sorts client-side.
	ClosedDoneHumanDriven int64 `json:"closedDoneHumanDriven"`
}

// InProgressRow is one row of the "now working" zone — a snapshot
// of work the team is actively touching right now. Surfaced on the
// activity dashboard between the podium and the live ticker so a
// viewer can answer "what's happening this moment" without scanning
// the event stream.
//
// `since` is the most-recent claim/accept/start timestamp on the
// issue — whichever is freshest. `staleLock` is true when the agent
// lock is older than 25 min, signalling that the 30-min stale-lock
// reaper will release it soon (the assignee's agent likely crashed
// or disconnected). `lockedBySession` is non-nil only when an agent
// currently holds the lock.
type InProgressRow struct {
	// IssueID / ProjectPrefix / ProjectID are omitempty (and ProjectID is
	// a pointer) so a Hard-redacted row drops them from the wire entirely
	// — a zeroed uuid.UUID would still serialize "00000000-…". LockedBySession
	// is likewise dropped on Hard (raw Mcp-Session-Id is a precise
	// cross-project correlator with no UX value).
	IssueID       string     `json:"issueId,omitempty"`
	ProjectID     *uuid.UUID `json:"projectId,omitempty"`
	ProjectPrefix string     `json:"projectPrefix,omitempty"`
	// SpaceID is the short id of the space of the row's project (sk-674.18
	// T030); dropped with the other identifiers by the hard redaction.
	SpaceID         string     `json:"spaceId,omitempty"`
	Title           string     `json:"title"`
	Type            string     `json:"type"`
	Priority        int        `json:"priority"`
	Assignee        *string    `json:"assignee,omitempty"`
	AssigneeUserID  *uuid.UUID `json:"assigneeUserId,omitempty"`
	Status          string     `json:"status"`
	LockedBySession *string    `json:"lockedBySession,omitempty"`
	LockedAt        *time.Time `json:"lockedAt,omitempty"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	AcceptedAt      *time.Time `json:"acceptedAt,omitempty"`
	Since           time.Time  `json:"since"`
	StaleLock       bool       `json:"staleLock"`
	// Redacted is true when this row belongs to a project the caller
	// cannot see (war-room cross-project view). Under Soft redaction the
	// Title is stripped but every identity field survives; under Hard
	// redaction the issue id, project prefix, projectId and
	// lockedBySession are dropped too. The kept "people + shape" fields
	// (assignee, type, priority, status, locks, since) let the row stay
	// visible while its identity is hidden.
	Redacted bool `json:"redacted,omitempty"`
}

// InProgressResult is the wire shape returned by /dashboard/in-progress.
type InProgressResult struct {
	Items []InProgressRow `json:"items"`
}

// AgentsRow is one row of the agents zone — per-user agent-session
// counts across four sliding windows: live (15 min) + 24h + 7d + 30d,
// plus an all-time total. The "session" here is an MCP `Mcp-Session-Id`
// (`agents.session_id`), which rotates on reconnect — so the counts
// answer "how many MCP sessions has this user run", not "how many
// physical Claude Code instances they have". Live > 0 is the
// operational signal; AllTime spans every session ever recorded for
// the user, including those older than the 30-day visibility window.
type AgentsRow struct {
	Username string `json:"username"`
	Live     int64  `json:"live"`    // last_seen_at > now - 15m
	H24      int64  `json:"h24"`     // last_seen_at > now - 24h
	D7       int64  `json:"d7"`      // last_seen_at > now - 7d
	D30      int64  `json:"d30"`     // last_seen_at > now - 30d
	AllTime  int64  `json:"allTime"` // every session ever (no time bound)
}

// AgentsResult is the wire shape returned by /dashboard/agents.
type AgentsResult struct {
	Items []AgentsRow `json:"items"`
}

// scoringCacheKey identifies one Scoring(...) invocation for cache
// lookup purposes. Only used for window=all-time (other windows are
// cheap on the (occurred_at) index and stay un-cached).
//
//   - projectFilter — zero UUID when caller passed nil.
//   - usernameFilter — empty string when caller passed nil or empty.
//   - noVisibilityFilter — true when no per-project visibility predicate
//     was applied (declared internal work OR globalScope/war-room is on).
//     Such callers all see the same global aggregate and must share one
//     cache slot, distinct from scoped callers who only see their own
//     visible projects.
//   - visibilityHash — FNV-1a 64 over the lexicographically-sorted
//     uuid.UUID keys of visibleProjects (16 bytes each). 0 when no
//     visibility filter applies so all no-filter callers consistently
//     key against the same slot regardless of what visibleProjects
//     contains.
type scoringCacheKey struct {
	projectFilter      uuid.UUID
	usernameFilter     string
	noVisibilityFilter bool
	visibilityHash     uint64
}

// scoringCacheEntry holds one cached ScoringResult plus its TTL deadline.
type scoringCacheEntry struct {
	result    *ScoringResult
	expiresAt time.Time
}

// scoringCacheTTL — how long an all-time scoring result is considered
// fresh. 60s matches the frontend's 30s refetch cadence so a refetch is
// roughly guaranteed to hit cache while still bounding staleness.
const scoringCacheTTL = 60 * time.Second

// scoringLeaderboardLimit is the top-N cap for Scoring podium queries.
// Distinct from projects.topContributorsCap (per-project contributor
// breakdown) — different surfaces, deliberately different numbers.
const scoringLeaderboardLimit = 10

// Store wraps the pool. The all-time scoring cache is the only mutable
// state — every other query rebinds parameters fresh.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time // injectable clock; defaults to time.Now

	scoringCacheMu sync.Mutex
	scoringCache   map[scoringCacheKey]scoringCacheEntry
}

// New builds the store. Caller owns the pool's lifetime.
func New(pool *pgxpool.Pool) *Store {
	return NewWithClock(pool, time.Now)
}

// NewWithClock builds the store with a caller-supplied clock. Tests use
// this to advance time deterministically when exercising the all-time
// scoring cache without sleeping past the TTL.
func NewWithClock(pool *pgxpool.Pool, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		pool:         pool,
		now:          now,
		scoringCache: make(map[scoringCacheKey]scoringCacheEntry),
	}
}

// hashVisibility computes the cache-key fingerprint of the caller's
// visible project set. Sorts the UUID slice lexicographically before
// hashing so map iteration order doesn't perturb the result. Returns 0
// when the input is empty so callers with no visible projects share one
// slot (they all see zero rows anyway).
func hashVisibility(visibleProjects map[uuid.UUID]struct{}) uint64 {
	if len(visibleProjects) == 0 {
		return 0
	}
	ids := make([]uuid.UUID, 0, len(visibleProjects))
	for pid := range visibleProjects {
		ids = append(ids, pid)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i].String() < ids[j].String()
	})
	h := fnv.New64a()
	for _, id := range ids {
		bytes := id // uuid.UUID is [16]byte
		_, _ = h.Write(bytes[:])
	}
	return h.Sum64()
}

// cloneScoringResult returns a deep copy of the supplied result so a
// caller mutating the returned value can't poison subsequent cache hits.
// Every slice + map is freshly allocated; scalars and structs (which
// don't contain references) are copied by value.
func cloneScoringResult(src *ScoringResult) *ScoringResult {
	if src == nil {
		return nil
	}
	out := &ScoringResult{
		Window: src.Window,
		Scope:  MetricScope{},
	}
	if src.Scope.ProjectID != nil {
		pid := *src.Scope.ProjectID
		out.Scope.ProjectID = &pid
	}
	if src.Scope.Username != nil {
		un := *src.Scope.Username
		out.Scope.Username = &un
	}
	if src.Metrics != nil {
		out.Metrics = make([]MetricRow, len(src.Metrics))
		copy(out.Metrics, src.Metrics)
	}
	if src.Leaderboards != nil {
		out.Leaderboards = make(map[string][]LeaderboardEntry, len(src.Leaderboards))
		for k, v := range src.Leaderboards {
			cp := make([]LeaderboardEntry, len(v))
			copy(cp, v)
			out.Leaderboards[k] = cp
		}
	}
	if src.MatrixUsers != nil {
		out.MatrixUsers = make([]MatrixUserRow, len(src.MatrixUsers))
		copy(out.MatrixUsers, src.MatrixUsers)
	}
	return out
}

// Scoring runs the aggregation pipeline for one window. visibleProjects
// is the set the caller can see (an admin's is the relation's, authz);
// unrestricted marks declared internal work (no visibility filter).
// projectFilter narrows further to one project; usernameFilter narrows
// to one actor. Both filters are optional.
//
// globalScope is the war-room flag: when true (and no projectFilter is
// set), the aggregation spans EVERY project for every caller — the
// leaderboard/metrics carry only usernames + counts (no titles), so
// global counts leak nothing. All no-filter callers (internal work +
// globalScope callers) share one all-time cache slot.
//
// The implementation runs ONE round-trip per metric family — six total
// queries, each cheap because the events table is indexed on
// (project_id, id) and occurred_at. We deliberately do NOT try to fold
// them into one CTE: the JOIN shapes diverge enough between metrics
// (issue intent vs comment intent vs actor-type fallback) that a unified
// CTE would either be unreadable or force the planner into a wide join
// it can't index. Six narrow queries are easier to maintain and to
// reason about for cardinality.
func (s *Store) Scoring(
	ctx context.Context,
	window Window,
	visibleProjects map[uuid.UUID]struct{},
	unrestricted bool,
	projectFilter *uuid.UUID,
	usernameFilter *string,
	// reach is the caller's HOT ZONE reach (authz.HotZoneReach.Reach,
	// sk-674.18 T030): the aggregates count exactly these projects — the
	// visible ones and the contribution of his own non-personal spaces.
	// nil counts the visible projects.
	reach map[uuid.UUID]struct{},
) (*ScoringResult, error) {
	// Compute `since` first — WindowAllTime uses the Go zero time
	// (year 0001 UTC) as a sentinel. The existing
	// `e.occurred_at >= $1::timestamptz` predicate trivially covers
	// every row at year 0001. The planner is cost-based and may pick
	// either an index scan on idx_events_occurred or a sequential scan
	// depending on row-count statistics; the 60s in-memory cache below
	// absorbs the cost of one heavy scan per minute per cache slot, so
	// the chosen plan is not load-bearing here.
	var since time.Time
	if window == WindowAllTime {
		since = time.Time{}
	} else {
		since = s.now().UTC().Add(-window.Duration())
	}

	// noVisibilityFilter is true when no per-project predicate is applied
	// (declared internal work). Both the cache discriminator and the
	// visibility hash key off this so every no-filter caller shares one
	// all-time cache slot.
	noVisibilityFilter := unrestricted
	filterSet := filterSetOf(visibleProjects, reach)

	// All-time cache lookup. We key on (projectFilter, usernameFilter,
	// noVisibilityFilter, visibilityHash) so two callers with the same
	// scope share the slot. Cache TTL is 60s; expired entries get pruned
	// on each lookup (lazy eviction — see top of function below the cache
	// hit path).
	var cacheKey scoringCacheKey
	if window == WindowAllTime {
		cacheKey = scoringCacheKey{
			projectFilter:      zeroOrUUID(projectFilter),
			usernameFilter:     strOrEmpty(usernameFilter),
			noVisibilityFilter: noVisibilityFilter,
			visibilityHash:     visibilityHashForCache(filterSet, noVisibilityFilter),
		}
		if cached, ok := s.lookupScoringCache(cacheKey); ok {
			return cached, nil
		}
	}

	// Build the project visibility predicate once and reuse across
	// every query.
	visibilityClause, visibilityArgs := buildVisibility(filterSet, unrestricted, projectFilter)

	// Username filter applies to events.actor (we filter by actor name
	// because we want "events caused BY this user", not "events ON
	// issues authored by this user").
	usernameClause := ""
	var usernameArg any
	if usernameFilter != nil && *usernameFilter != "" {
		usernameClause = " AND e.actor = $%d "
		usernameArg = *usernameFilter
	}

	// ---------- aggregate (matrix top row sums) ----------
	metrics := make([]MetricRow, 0, 9)

	// 1. issues_created — by issues.source
	row, err := s.aggregateIssueIntent(ctx, "issues_created", "issue.created",
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring issues_created: %w", err)
	}
	metrics = append(metrics, row)

	// 2. issues_closed
	row, err = s.aggregateIssueIntent(ctx, "issues_closed", "issue.closed",
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring issues_closed: %w", err)
	}
	metrics = append(metrics, row)

	// 2a / 2b. issues_closed_done + issues_closed_no_work (sk-494).
	// They stand RIGHT after issues_closed, because they are read only together with
	// it: two halves of one number, and the order of the matrix rows is what
	// shows this on screen without a caption.
	closedDone, closedNoWork, err := s.aggregateClosedByOutcome(ctx,
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring issues_closed by outcome: %w", err)
	}
	metrics = append(metrics, closedDone, closedNoWork)

	// 3. issues_closed_with_commits
	row, err = s.aggregateClosedWithCommits(ctx,
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring issues_closed_with_commits: %w", err)
	}
	metrics = append(metrics, row)

	// 4. comments_authored
	row, err = s.aggregateCommentIntent(ctx, "comments_authored", false,
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring comments_authored: %w", err)
	}
	metrics = append(metrics, row)

	// 5. comments_on_others_issues
	row, err = s.aggregateCommentIntent(ctx, "comments_on_others_issues", true,
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring comments_on_others_issues: %w", err)
	}
	metrics = append(metrics, row)

	// 6. mentions_sent — actor-type fallback (no intent on mention events)
	row, err = s.aggregateActorIntent(ctx, "mentions_sent",
		[]string{"issue.mention", "comment.mention"},
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring mentions_sent: %w", err)
	}
	metrics = append(metrics, row)

	// 7. dep_edges_added
	row, err = s.aggregateActorIntent(ctx, "dep_edges_added",
		[]string{"dependency.created"},
		since, visibilityClause, visibilityArgs, usernameClause, usernameArg)
	if err != nil {
		return nil, fmt.Errorf("scoring dep_edges_added: %w", err)
	}
	metrics = append(metrics, row)

	// ---------- leaderboards ----------
	closedHumanDriven, err := s.leaderboardClosedHumanDriven(ctx,
		since, visibilityClause, visibilityArgs)
	if err != nil {
		return nil, fmt.Errorf("scoring leaderboard_closed_human_driven: %w", err)
	}
	closedDoneHumanDriven, err := s.leaderboardClosedDoneHumanDriven(ctx,
		since, visibilityClause, visibilityArgs)
	if err != nil {
		return nil, fmt.Errorf("scoring leaderboard_closed_done_human_driven: %w", err)
	}
	closedWithCommits, err := s.leaderboardClosedWithCommits(ctx,
		since, visibilityClause, visibilityArgs)
	if err != nil {
		return nil, fmt.Errorf("scoring leaderboard_closed_with_commits: %w", err)
	}

	// ---------- matrix per-user ----------
	matrixUsers, err := s.matrixPerUser(ctx,
		since, visibilityClause, visibilityArgs)
	if err != nil {
		return nil, fmt.Errorf("scoring matrix_per_user: %w", err)
	}

	scope := MetricScope{}
	if projectFilter != nil {
		pid := *projectFilter
		scope.ProjectID = &pid
	}
	if usernameFilter != nil && *usernameFilter != "" {
		un := *usernameFilter
		scope.Username = &un
	}

	result := &ScoringResult{
		Window:  string(window),
		Scope:   scope,
		Metrics: metrics,
		Leaderboards: map[string][]LeaderboardEntry{
			"closed_human_driven":      closedHumanDriven,
			"closed_done_human_driven": closedDoneHumanDriven,
			"closed_with_commits":      closedWithCommits,
		},
		MatrixUsers: matrixUsers,
	}

	if window == WindowAllTime {
		s.storeScoringCache(cacheKey, result)
		// Return a fresh copy so the caller mutating the result can't
		// corrupt the cached copy. lookupScoringCache also returns a
		// deep copy on hit, so both paths are symmetric.
		return cloneScoringResult(result), nil
	}
	return result, nil
}

// lookupScoringCache returns a deep copy of any unexpired cached
// result for the supplied key. On the miss path it lazily evicts
// expired entries so the map stays bounded without a background ticker.
func (s *Store) lookupScoringCache(key scoringCacheKey) (*ScoringResult, bool) {
	s.scoringCacheMu.Lock()
	defer s.scoringCacheMu.Unlock()
	now := s.now()
	// Lazy eviction — scan-and-delete on every lookup. The cardinality
	// here is `{projects × users}` for callers actually hitting the
	// all-time window, which in practice is a handful per server.
	for k, e := range s.scoringCache {
		if !e.expiresAt.After(now) {
			delete(s.scoringCache, k)
		}
	}
	entry, ok := s.scoringCache[key]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.After(now) {
		delete(s.scoringCache, key)
		return nil, false
	}
	return cloneScoringResult(entry.result), true
}

// storeScoringCache writes a result + its TTL deadline under key. We
// also opportunistically prune expired entries here so the map stays
// bounded even when callers only ever hit (no misses to drive eviction).
func (s *Store) storeScoringCache(key scoringCacheKey, result *ScoringResult) {
	s.scoringCacheMu.Lock()
	defer s.scoringCacheMu.Unlock()
	now := s.now()
	for k, e := range s.scoringCache {
		if !e.expiresAt.After(now) {
			delete(s.scoringCache, k)
		}
	}
	s.scoringCache[key] = scoringCacheEntry{
		result:    cloneScoringResult(result),
		expiresAt: now.Add(scoringCacheTTL),
	}
}

// zeroOrUUID returns the dereferenced uuid or the zero uuid when nil.
// Lets the cache key be a comparable struct of value types.
func zeroOrUUID(u *uuid.UUID) uuid.UUID {
	if u == nil {
		return uuid.UUID{}
	}
	return *u
}

// strOrEmpty returns the dereferenced string or "" when nil.
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// visibilityHashForCache returns 0 when no visibility filter applies
// (declared internal work skips visibility filtering entirely), otherwise
// the FNV-1a hash of the sorted project UUIDs of the filter set — the
// caller's HOT ZONE reach (K-6, sk-674.18 T030): a project deleted or out of
// reach changes the set, so a warm cache entry is never served for it.
func visibilityHashForCache(filterSet map[uuid.UUID]struct{}, noVisibilityFilter bool) uint64 {
	if noVisibilityFilter {
		return 0
	}
	return hashVisibility(filterSet)
}

// filterSetOf is the project set a HOT ZONE read is filtered on: the reach
// when the caller gave one (sk-674.18 T030, authz.HotZoneReach — visible
// plus contributing), otherwise the visible set.
func filterSetOf(visibleProjects, reach map[uuid.UUID]struct{}) map[uuid.UUID]struct{} {
	if reach != nil {
		return reach
	}
	return visibleProjects
}

// buildVisibilityForColumn is the parameterized sibling of buildVisibility.
// `col` is the project-id column to filter on (e.g. "e.project_id" for
// events queries, "i.project_id" for issue_commits JOIN issues).
func buildVisibilityForColumn(
	filterSet map[uuid.UUID]struct{},
	unrestricted bool,
	projectFilter *uuid.UUID,
	col string,
) (string, []any) {
	// A single project filter pins us to one row regardless of the
	// unrestricted flag. The handler already verified the caller can see this
	// project before calling Scoring.
	if projectFilter != nil {
		return fmt.Sprintf(" AND %s = $%%d ", col), []any{*projectFilter}
	}
	if unrestricted {
		return "", nil
	}
	if len(filterSet) == 0 {
		// Nothing in the caller's reach. Force a no-match predicate so
		// every aggregate returns zero without a special-case in every
		// caller path.
		return " AND FALSE ", nil
	}
	ids := make([]uuid.UUID, 0, len(filterSet))
	for pid := range filterSet {
		ids = append(ids, pid)
	}
	return fmt.Sprintf(" AND %s = ANY($%%d) ", col), []any{ids}
}

// buildVisibility returns ("", nil) when no project filtering applies —
// declared internal work (authz) with no per-project filter. Otherwise it
// emits the proper IN-list clause over filterSet (the HOT ZONE reach or the
// visible set — filterSetOf) and the arg list ready to be appended to a
// query's $-args.
//
// The returned clause uses $%d placeholders (one per arg, in order); the
// caller renders them with the live argument index in their own query
// builder. This keeps the visibility clause composable across the six
// query shapes without duplicating the filter logic six times.
func buildVisibility(
	filterSet map[uuid.UUID]struct{},
	unrestricted bool,
	projectFilter *uuid.UUID,
) (string, []any) {
	return buildVisibilityForColumn(filterSet, unrestricted, projectFilter, "e.project_id")
}

// reachPlanned returns the arguments of a query that binds the HOT ZONE
// reach (or the visible set) as an array of project ids, prefixed with
// pgx.QueryExecModeDescribeExec: the statement is prepared unnamed and
// planned with that array on every execution.
//
// The default mode caches a named prepared statement per connection, and
// after five executions Postgres may switch it to a generic plan, which
// cannot see how many projects the array holds. For the counts over events
// that plan was ~40× slower than the custom one on the 10k corpus
// (≈60 ms against ≈1.5 ms on a reach of 2500 projects — sk-674.18
// EXPLAIN gate, dashboard-scoring): every call after the fifth on a pooled
// connection paid it. Before T030 these reads had no array (the dashboard
// ran unfiltered, «war room»), so they had nothing to switch on. The price
// of the custom plan is its planning, a few milliseconds a statement.
func reachPlanned(args []any) []any {
	return append([]any{pgx.QueryExecModeDescribeExec}, args...)
}

// renderPlaceholders walks the clause template and replaces every $%d
// occurrence with $<n> where n starts at startIdx+1 (1-indexed pgx
// parameter numbers). Returns the rendered clause and the new running
// index.
func renderPlaceholders(template string, startIdx int) (string, int) {
	if template == "" {
		return "", startIdx
	}
	var b strings.Builder
	i := 0
	for i < len(template) {
		if i+1 < len(template) && template[i] == '$' && template[i+1] == '%' && i+2 < len(template) && template[i+2] == 'd' {
			startIdx++
			fmt.Fprintf(&b, "$%d", startIdx)
			i += 3
			continue
		}
		b.WriteByte(template[i])
		i++
	}
	return b.String(), startIdx
}

// aggregateIssueIntent counts events of one type, joined to issues to
// read the intent axis (issues.source).
func (s *Store) aggregateIssueIntent(
	ctx context.Context,
	key, eventType string,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
	usernameClause string, usernameArg any,
) (MetricRow, error) {
	args := []any{eventType, since}
	args = append(args, visibilityArgs...)
	visClause, idx := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	unClause := ""
	if usernameClause != "" {
		args = append(args, usernameArg)
		idx++
		unClause = fmt.Sprintf(usernameClause, idx)
	}
	sql := fmt.Sprintf(`
SELECT
    COUNT(*) FILTER (WHERE i.source = 'human')             AS human,
    COUNT(*) FILTER (WHERE i.source = 'agent_directed')    AS directed,
    COUNT(*) FILTER (WHERE i.source = 'agent_autonomous')  AS autonomous
  FROM events e
  JOIN issues i ON i.id = e.entity_id
 WHERE e.type = $1
   AND e.occurred_at >= $2
   AND e.entity_type = 'issue'
   %s
   %s`, visClause, unClause)

	var human, directed, autonomous int64
	if err := s.pool.QueryRow(ctx, sql, reachPlanned(args)...).Scan(&human, &directed, &autonomous); err != nil {
		return MetricRow{}, fmt.Errorf("aggregating %s: %w", key, err)
	}
	return MetricRow{Key: key, Human: human, AgentDirected: directed, AgentAutonomous: autonomous}, nil
}

// ---------------------------------------------------------------------------
// sk-494 - the close outcome on the dashboard surfaces: ONE place of the rule
// ---------------------------------------------------------------------------

// closeOutcomeExpr is the ONE spelling of "which outcome THIS
// close ended with" for every dashboard query. It reads the outcome the closing
// event carried, defaulting to "done" when the event predates sk-489.
//
// ⚠ WHY THE EVENT, NOT THE ISSUE ROW. The neighbouring legs of this package
// do join `issues` and read the current state (`i.source`,
// EXISTS over `issue_commits`), and the first impulse was to add here
// `i.close_outcome` - that is, the issue's CURRENT outcome. That would open a hole
// the event does not have: a REOPENED issue has no outcome at all (INV-1,
// migration 000050 clears it on any unclosed status), and its past
// close would land in `issues_closed` and in NEITHER of the two halves -
// the identity "closed = done + no_work" would break at the very first
// reopening. For the same reason an outcome edit (US-3) would rewrite
// the past window retroactively: the same yesterday would be counted
// differently today than it was counted yesterday.
//
// The event has no such hole: sk-489 puts the RESOLVED outcome in the payload
// of BOTH close events of BOTH writers (`issues/store.go#Close`,
// `issues/store.go#UpdateStatus` - both call
// `issues/close_outcome.go#closeOutcomePayload`), so every
// `issue.closed` written after the rollout has the key.
//
// COALESCE closes exactly one class of rows - events BEFORE sk-489, whose
// payload is empty. The `done` default here is not a choice but the same history
// record that is already made in two places: the seed of migration 000050 ("all
// previously closed ones - done", US-8) and its trigger normalizer
// ("closed without an outcome -> done"). Writing something different here would mean
// introducing a THIRD rule about the same history.
const closeOutcomeExpr = `COALESCE(e.payload->>'closeOutcome', 'done')`

// closeDonePredicate / closeNoWorkPredicate - the two halves of the partition,
// written out as MUTUALLY COMPLEMENTARY conditions over one expression.
//
// They are intentionally not independent: `<>` is the negation of `=` over the same
// COALESCE expression, which never returns NULL, so every `issue.closed`
// row lands in exactly one half, and the identity
// "issues_closed = done + no_work" holds BY CONSTRUCTION, not by the coincidence
// of two wordings. The same reason why
// `issues/store.go#epicSummaryNoWorkExpr` is written as the closed leg plus a
// predicate.
//
// The blacklist ("everything that is not done") is the second place in the tree with the same rule
// record as INV-5; the whitelist of three values lives in
// `issues/close_outcome.go#IsNoWorkOutcome`. If you add a fifth value -
// edit both halves, as said there.
const (
	closeDonePredicate   = ` AND ` + closeOutcomeExpr + ` = 'done' `
	closeNoWorkPredicate = ` AND ` + closeOutcomeExpr + ` <> 'done' `
)

// aggregateClosedByOutcome returns the two sk-494 matrix rows —
// issues_closed_done and issues_closed_no_work — from ONE query.
//
// One query, not two, for the reason the identity above exists: both
// legs are FILTER counts over the SAME row set with the SAME window,
// visibility and username predicates, so no future edit can narrow one
// and forget the other. The caller appends both rows next to
// issues_closed, whose own count is produced by aggregateIssueIntent
// over exactly this row set.
func (s *Store) aggregateClosedByOutcome(
	ctx context.Context,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
	usernameClause string, usernameArg any,
) (done MetricRow, noWork MetricRow, err error) {
	args := []any{since}
	args = append(args, visibilityArgs...)
	visClause, idx := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	unClause := ""
	if usernameClause != "" {
		args = append(args, usernameArg)
		idx++
		unClause = fmt.Sprintf(usernameClause, idx)
	}
	sql := fmt.Sprintf(`
SELECT
    COUNT(*) FILTER (WHERE i.source = 'human'
                       AND %[1]s = 'done')              AS done_human,
    COUNT(*) FILTER (WHERE i.source = 'agent_directed'
                       AND %[1]s = 'done')              AS done_directed,
    COUNT(*) FILTER (WHERE i.source = 'agent_autonomous'
                       AND %[1]s = 'done')              AS done_autonomous,
    COUNT(*) FILTER (WHERE i.source = 'human'
                       AND %[1]s <> 'done')             AS nowork_human,
    COUNT(*) FILTER (WHERE i.source = 'agent_directed'
                       AND %[1]s <> 'done')             AS nowork_directed,
    COUNT(*) FILTER (WHERE i.source = 'agent_autonomous'
                       AND %[1]s <> 'done')             AS nowork_autonomous
  FROM events e
  JOIN issues i ON i.id = e.entity_id
 WHERE e.type = 'issue.closed'
   AND e.occurred_at >= $1
   AND e.entity_type = 'issue'
   %[2]s
   %[3]s`, closeOutcomeExpr, visClause, unClause)

	var dh, dd, da, nh, nd, na int64
	if scanErr := s.pool.QueryRow(ctx, sql, reachPlanned(args)...).
		Scan(&dh, &dd, &da, &nh, &nd, &na); scanErr != nil {
		return MetricRow{}, MetricRow{},
			fmt.Errorf("aggregating issues_closed_done / issues_closed_no_work: %w", scanErr)
	}
	return MetricRow{Key: "issues_closed_done", Human: dh, AgentDirected: dd, AgentAutonomous: da},
		MetricRow{Key: "issues_closed_no_work", Human: nh, AgentDirected: nd, AgentAutonomous: na},
		nil
}

// aggregateClosedWithCommits is a specialisation of
// aggregateIssueIntent that requires the closing issue to have at least
// one issue_commits row (a commit was ever linked to it).
//
// The EXISTS subquery on issue_commits.issue_id = e.entity_id matches on
// the issue id (hierarchical text key). We deliberately do NOT bound the
// commit by linked_at: the metric answers "did this in-window close ship
// any code?", so a close whose commits were linked before the window
// start (or attached slightly later via skein_commits_attach) must still
// count. Re-bounding by linked_at silently dropped a legitimately commit-
// backed close from finite-window counts — the divergence-vs-HOT-ZONE bug
// spotted on /activity (sk-65 follow-up).
func (s *Store) aggregateClosedWithCommits(
	ctx context.Context,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
	usernameClause string, usernameArg any,
) (MetricRow, error) {
	args := []any{since}
	args = append(args, visibilityArgs...)
	visClause, idx := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	unClause := ""
	if usernameClause != "" {
		args = append(args, usernameArg)
		idx++
		unClause = fmt.Sprintf(usernameClause, idx)
	}
	sql := fmt.Sprintf(`
SELECT
    COUNT(*) FILTER (WHERE i.source = 'human')             AS human,
    COUNT(*) FILTER (WHERE i.source = 'agent_directed')    AS directed,
    COUNT(*) FILTER (WHERE i.source = 'agent_autonomous')  AS autonomous
  FROM events e
  JOIN issues i ON i.id = e.entity_id
 WHERE e.type = 'issue.closed'
   AND e.occurred_at >= $1
   AND e.entity_type = 'issue'
   AND EXISTS (
       SELECT 1 FROM issue_commits ic
        WHERE ic.issue_id = e.entity_id
   )
   %s
   %s`, visClause, unClause)

	var human, directed, autonomous int64
	if err := s.pool.QueryRow(ctx, sql, reachPlanned(args)...).Scan(&human, &directed, &autonomous); err != nil {
		return MetricRow{}, fmt.Errorf("aggregating issues_closed_with_commits: %w", err)
	}
	return MetricRow{Key: "issues_closed_with_commits", Human: human, AgentDirected: directed, AgentAutonomous: autonomous}, nil
}

// aggregateCommentIntent counts comment.created events, JOINed to the
// comments table to read author_kind. When onOthersOnly is true we
// further filter to comments where the author username differs from
// the parent issue's current assignee (best-effort heuristic).
func (s *Store) aggregateCommentIntent(
	ctx context.Context,
	key string,
	onOthersOnly bool,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
	usernameClause string, usernameArg any,
) (MetricRow, error) {
	args := []any{since}
	args = append(args, visibilityArgs...)
	visClause, idx := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	unClause := ""
	if usernameClause != "" {
		args = append(args, usernameArg)
		idx++
		unClause = fmt.Sprintf(usernameClause, idx)
	}
	othersClause := ""
	if onOthersOnly {
		// Comments where the actor IS NOT the current assignee (the
		// "on others' issues" signal). Wire/OpenAPI note: we use the
		// *current* assignee because the schema doesn't snapshot
		// assignee at comment time — accepted approximation (a reassign
		// after the comment can reclassify the row).
		othersClause = " AND (i.assignee IS NULL OR i.assignee <> e.actor) "
	}
	sql := fmt.Sprintf(`
SELECT
    COUNT(*) FILTER (WHERE c.author_kind = 'human')             AS human,
    COUNT(*) FILTER (WHERE c.author_kind = 'agent_directed')    AS directed,
    COUNT(*) FILTER (WHERE c.author_kind = 'agent_autonomous')  AS autonomous
  FROM events e
  -- review C1 — wrap the uuid cast in a CASE gated on entity_type so
  -- Postgres only evaluates it on the comment branch. Mirrors the
  -- pattern used in internal/handler/me.go, internal/issues/store.go
  -- (activityListSQL), and internal/notifications/store.go — a bare
  -- e.entity_id::uuid lets the planner hoist the cast and aborts on
  -- non-comment rows whose entity_id is a hierarchical issue id.
  JOIN comments c ON c.id = (CASE WHEN e.entity_type = 'comment'
                                   THEN e.entity_id::uuid
                                   ELSE NULL END)
  JOIN issues i ON i.id = c.issue_id
 WHERE e.type = 'comment.created'
   AND e.occurred_at >= $1
   AND e.entity_type = 'comment'
   %s
   %s
   %s`, visClause, unClause, othersClause)

	var human, directed, autonomous int64
	if err := s.pool.QueryRow(ctx, sql, reachPlanned(args)...).Scan(&human, &directed, &autonomous); err != nil {
		return MetricRow{}, fmt.Errorf("aggregating %s: %w", key, err)
	}
	return MetricRow{Key: key, Human: human, AgentDirected: directed, AgentAutonomous: autonomous}, nil
}

// aggregateActorIntent counts events of the given types, classified by
// actor_type rather than per-entity intent (used for mentions /
// dep_edges where there's no intent column on the source rows).
//
// Mapping rule (documented on the package comment):
//
//	actor_type = 'human'  → human
//	actor_type = 'agent'  → agent_autonomous (agent firehose; the
//	                        agent_directed case is rare for these
//	                        event types and is folded in)
//	actor_type = 'system' → ignored (system rows don't get attributed
//	                        to a person/agent intent for these axes)
func (s *Store) aggregateActorIntent(
	ctx context.Context,
	key string,
	eventTypes []string,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
	usernameClause string, usernameArg any,
) (MetricRow, error) {
	args := []any{eventTypes, since}
	args = append(args, visibilityArgs...)
	visClause, idx := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	unClause := ""
	if usernameClause != "" {
		args = append(args, usernameArg)
		idx++
		unClause = fmt.Sprintf(usernameClause, idx)
	}
	sql := fmt.Sprintf(`
SELECT
    COUNT(*) FILTER (WHERE actor_type = 'human') AS human,
    0::bigint                                    AS directed,
    COUNT(*) FILTER (WHERE actor_type = 'agent') AS autonomous
  FROM events e
 WHERE e.type = ANY($1)
   AND e.occurred_at >= $2
   %s
   %s`, visClause, unClause)

	var human, directed, autonomous int64
	if err := s.pool.QueryRow(ctx, sql, reachPlanned(args)...).Scan(&human, &directed, &autonomous); err != nil {
		return MetricRow{}, fmt.Errorf("aggregating %s: %w", key, err)
	}
	return MetricRow{Key: key, Human: human, AgentDirected: directed, AgentAutonomous: autonomous}, nil
}

// leaderboardClosedHumanDriven returns the top-10 actors by
// issue.closed events whose intent is human or agent_directed (the
// "human-driven" definition from the design comment podium).
func (s *Store) leaderboardClosedHumanDriven(
	ctx context.Context,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
) ([]LeaderboardEntry, error) {
	args := []any{since}
	args = append(args, visibilityArgs...)
	visClause, _ := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	sql := fmt.Sprintf(`
SELECT e.actor, COUNT(*) AS n
  FROM events e
  JOIN issues i ON i.id = e.entity_id
 WHERE e.type = 'issue.closed'
   AND e.occurred_at >= $1
   AND e.entity_type = 'issue'
   AND i.source IN ('human', 'agent_directed')
   %s
 GROUP BY e.actor
 ORDER BY n DESC, e.actor ASC
 LIMIT %d`, visClause, scoringLeaderboardLimit)
	return s.scanLeaderboard(ctx, sql, args)
}

// leaderboardClosedDoneHumanDriven is the sk-494 twin of
// leaderboardClosedHumanDriven: the same top-10, narrowed to closes
// whose outcome is "done".
//
// ⚠ THIS IS A SECOND PODIUM, NOT A CORRECTION OF THE FIRST. `closed_human_driven`
// stays exactly what it was - the ranking of CLOSES, and the caption the frontend
// prints ("Top 5 · closed") does not start to lie. Narrowing the
// existing key would change the meaning of a field already handed out
// (the sk-489 INV-3 rule, carried over here) and would quietly rewrite all
// earlier rankings; an ADDED key gives the surface a choice and touches
// no old number.
//
// `closed_with_commits` intentionally has NO such twin: the presence of a commit
// already evidences work, and a fourth ranking on the same screen
// is an interface decision nobody ordered.
func (s *Store) leaderboardClosedDoneHumanDriven(
	ctx context.Context,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
) ([]LeaderboardEntry, error) {
	args := []any{since}
	args = append(args, visibilityArgs...)
	visClause, _ := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	sql := fmt.Sprintf(`
SELECT e.actor, COUNT(*) AS n
  FROM events e
  JOIN issues i ON i.id = e.entity_id
 WHERE e.type = 'issue.closed'
   AND e.occurred_at >= $1
   AND e.entity_type = 'issue'
   AND i.source IN ('human', 'agent_directed')
   %s
   %s
 GROUP BY e.actor
 ORDER BY n DESC, e.actor ASC
 LIMIT %d`, closeDonePredicate, visClause, scoringLeaderboardLimit)
	return s.scanLeaderboard(ctx, sql, args)
}

// leaderboardClosedWithCommits returns top-10 actors by close-with-
// commits events in the window.
func (s *Store) leaderboardClosedWithCommits(
	ctx context.Context,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
) ([]LeaderboardEntry, error) {
	args := []any{since}
	args = append(args, visibilityArgs...)
	visClause, _ := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	sql := fmt.Sprintf(`
SELECT e.actor, COUNT(*) AS n
  FROM events e
 WHERE e.type = 'issue.closed'
   AND e.occurred_at >= $1
   AND e.entity_type = 'issue'
   AND EXISTS (
       SELECT 1 FROM issue_commits ic
        WHERE ic.issue_id = e.entity_id
   )
   %s
 GROUP BY e.actor
 ORDER BY n DESC, e.actor ASC
 LIMIT %d`, visClause, scoringLeaderboardLimit)
	return s.scanLeaderboard(ctx, sql, args)
}

func (s *Store) scanLeaderboard(ctx context.Context, sql string, args []any) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, sql, reachPlanned(args)...)
	if err != nil {
		return nil, fmt.Errorf("running leaderboard query: %w", err)
	}
	defer rows.Close()
	out := make([]LeaderboardEntry, 0, 10)
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.Username, &e.Count); err != nil {
			return nil, fmt.Errorf("scanning leaderboard row: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// matrixPerUser returns the per-user breakdown used by the bottom
// "metrics matrix" zone. One row per active user (≥1 event in the
// window across the metric set), sorted by ClosedHumanDriven DESC.
//
// One query, one round-trip — the per-user grouping makes the planner
// pick a hash aggregate over the events index scan which is what we
// want even on busy projects.
func (s *Store) matrixPerUser(
	ctx context.Context,
	since time.Time,
	visibilityClause string, visibilityArgs []any,
) ([]MatrixUserRow, error) {
	args := []any{since}
	args = append(args, visibilityArgs...)
	visClause, _ := renderPlaceholders(visibilityClause, len(args)-len(visibilityArgs))
	// We use one query with FILTER clauses per metric × axis cell.
	// The events table is joined LEFT to issues / comments so a row
	// for a non-matching event_type passes through with NULL i/c
	// columns and counts as 0 in the corresponding FILTER.
	sql := fmt.Sprintf(`
WITH issue_rows AS (
    -- sk-494: close_outcome next to intent - the outcome of THIS close, from the
    -- event payload (closeOutcomeExpr). For issue.created rows it is
    -- meaningless and not read: the closed_done branch below selects by
    -- type = 'issue.closed'.
    SELECT e.actor, i.source AS intent, e.type,
           %[2]s AS close_outcome
      FROM events e
      JOIN issues i ON i.id = e.entity_id
     WHERE e.type IN ('issue.created', 'issue.closed')
       AND e.occurred_at >= $1
       AND e.entity_type = 'issue'
       %[1]s
),
issue_closed_commits AS (
    SELECT e.actor, i.source AS intent
      FROM events e
      JOIN issues i ON i.id = e.entity_id
     WHERE e.type = 'issue.closed'
       AND e.occurred_at >= $1
       AND e.entity_type = 'issue'
       AND EXISTS (
           SELECT 1 FROM issue_commits ic
            WHERE ic.issue_id = e.entity_id
       )
       %[1]s
),
comment_rows AS (
    -- review C1 — same CASE-wrapped cast as in aggregateCommentIntent;
    -- bare e.entity_id::uuid risks an "invalid input syntax for type
    -- uuid" abort on non-comment rows under different planner choices.
    SELECT e.actor, c.author_kind AS intent
      FROM events e
      JOIN comments c ON c.id = (CASE WHEN e.entity_type = 'comment'
                                       THEN e.entity_id::uuid
                                       ELSE NULL END)
     WHERE e.type = 'comment.created'
       AND e.occurred_at >= $1
       AND e.entity_type = 'comment'
       %[1]s
),
created AS (
    SELECT actor,
           COUNT(*) FILTER (WHERE intent = 'human')            AS h,
           COUNT(*) FILTER (WHERE intent = 'agent_directed')   AS d,
           COUNT(*) FILTER (WHERE intent = 'agent_autonomous') AS a
      FROM issue_rows
     WHERE type = 'issue.created'
     GROUP BY actor
),
closed AS (
    SELECT actor,
           COUNT(*) FILTER (WHERE intent = 'human')            AS h,
           COUNT(*) FILTER (WHERE intent = 'agent_directed')   AS d,
           COUNT(*) FILTER (WHERE intent = 'agent_autonomous') AS a
      FROM issue_rows
     WHERE type = 'issue.closed'
     GROUP BY actor
),
closed_done AS (
    -- sk-494: the same selection and the same grouping as closed above, plus
    -- one predicate on the outcome. A separate CTE, not a FILTER inside closed,
    -- only because the three cells are read as separate columns; the rows
    -- are still the same, so closed_done ⊆ closed by construction.
    SELECT actor,
           COUNT(*) FILTER (WHERE intent = 'human')            AS h,
           COUNT(*) FILTER (WHERE intent = 'agent_directed')   AS d,
           COUNT(*) FILTER (WHERE intent = 'agent_autonomous') AS a
      FROM issue_rows
     WHERE type = 'issue.closed' AND close_outcome = 'done'
     GROUP BY actor
),
closed_commits AS (
    SELECT actor,
           COUNT(*) FILTER (WHERE intent = 'human')            AS h,
           COUNT(*) FILTER (WHERE intent = 'agent_directed')   AS d,
           COUNT(*) FILTER (WHERE intent = 'agent_autonomous') AS a
      FROM issue_closed_commits
     GROUP BY actor
),
comments AS (
    SELECT actor,
           COUNT(*) FILTER (WHERE intent = 'human')            AS h,
           COUNT(*) FILTER (WHERE intent = 'agent_directed')   AS d,
           COUNT(*) FILTER (WHERE intent = 'agent_autonomous') AS a
      FROM comment_rows
     GROUP BY actor
),
all_actors AS (
    -- closed_done is NOT in the union on purpose: it is a subset of closed by
    -- construction (the same selection plus a predicate), so an actor who
    -- is not in closed cannot be in it, and an extra UNION branch would give the
    -- reader a false signal that one can.
    SELECT actor FROM created
    UNION SELECT actor FROM closed
    UNION SELECT actor FROM closed_commits
    UNION SELECT actor FROM comments
)
SELECT a.actor,
       COALESCE(cr.h, 0), COALESCE(cr.d, 0), COALESCE(cr.a, 0),
       COALESCE(cl.h, 0), COALESCE(cl.d, 0), COALESCE(cl.a, 0),
       COALESCE(cd.h, 0), COALESCE(cd.d, 0), COALESCE(cd.a, 0),
       COALESCE(cc.h, 0), COALESCE(cc.d, 0), COALESCE(cc.a, 0),
       COALESCE(cm.h, 0), COALESCE(cm.d, 0), COALESCE(cm.a, 0),
       (COALESCE(cl.h, 0) + COALESCE(cl.d, 0)) AS closed_hd,
       (COALESCE(cd.h, 0) + COALESCE(cd.d, 0)) AS closed_done_hd
  FROM all_actors a
  LEFT JOIN created        cr USING (actor)
  LEFT JOIN closed         cl USING (actor)
  LEFT JOIN closed_done    cd USING (actor)
  LEFT JOIN closed_commits cc USING (actor)
  LEFT JOIN comments       cm USING (actor)
 ORDER BY closed_hd DESC, a.actor ASC
 LIMIT 50`, visClause, closeOutcomeExpr)

	rows, err := s.pool.Query(ctx, sql, reachPlanned(args)...)
	if err != nil {
		return nil, fmt.Errorf("running matrix query: %w", err)
	}
	defer rows.Close()
	out := make([]MatrixUserRow, 0, 50)
	for rows.Next() {
		var r MatrixUserRow
		if err := rows.Scan(
			&r.Username,
			&r.IssuesCreatedHuman, &r.IssuesCreatedDirected, &r.IssuesCreatedAutonomous,
			&r.IssuesClosedHuman, &r.IssuesClosedDirected, &r.IssuesClosedAutonomous,
			&r.IssuesClosedDoneHuman, &r.IssuesClosedDoneDirected, &r.IssuesClosedDoneAutonomous,
			&r.ClosedWithCommitsHuman, &r.ClosedWithCommitsDir, &r.ClosedWithCommitsAuto,
			&r.CommentsHuman, &r.CommentsDirected, &r.CommentsAutonomous,
			&r.ClosedHumanDriven, &r.ClosedDoneHumanDriven,
		); err != nil {
			return nil, fmt.Errorf("scanning matrix row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InProgress returns up to `limit` issues currently being worked on.
//
// Scope and redaction are TWO INDEPENDENT axes (mirroring
// issues.ListUserActivity):
//
//   - globalScope selects the row set. globalScope == false CUTS rows to
//     the caller's visible set (the relation's, an admin's included; a
//     caller with no visible projects sees none). globalScope == true (the war-room
//     /dashboard/in-progress contract) returns ALL in-progress issues
//     when no projectFilter is set. A projectFilter pins to one project
//     in either mode.
//   - level selects how invisible rows are redacted for a scoped
//     caller: redact.Soft strips the Title but keeps identity;
//     redact.Hard strips the issue id, project prefix, projectId and
//     lockedBySession too; redact.None leaves rows untouched. Declared
//     internal work is never redacted.
//
// "In progress" = status='in_progress' OR
// locked_by IS NOT NULL — captures both the formal status transition
// (assignee explicitly moved the issue) and the agent-claim signal
// (an agent grabbed the lock without flipping status). Both axes are
// useful for the "what's happening right now" surface; a viewer wants
// to see both committed work and active agent runs.
//
// Ordering: most-recent activity first via COALESCE(locked_at,
// started_at, accepted_at). Falls back to updated_at as a last
// resort so rows always have a deterministic position.
//
// `staleLock` is computed in Go from `locked_at` (we want a
// uniform 25-min threshold visible to the frontend; the 30-min stale
// reaper in lockreap runs server-side and we surface the warning
// 5 min ahead). Cheaper than a CASE expression in SQL.
//
// Schema reuses the existing partial indexes:
//   - idx_issues_locked  ON issues (locked_at) WHERE locked_by IS NOT NULL  (000015)
//   - idx_issues_status  on (status, ...)                                    (000001)
//
// Bounded by `limit` (handler clamps to 50) and by the OR-condition
// cardinality (an org rarely has >50 simultaneously-active issues).
func (s *Store) InProgress(
	ctx context.Context,
	visibleProjects map[uuid.UUID]struct{},
	unrestricted bool,
	projectFilter *uuid.UUID,
	limit int,
	// reach — the caller's HOT ZONE reach (sk-674.18 T030): rows of these
	// projects; those outside visibleProjects are redacted at level. nil —
	// the visible projects only.
	reach map[uuid.UUID]struct{},
	level redact.Level,
) (*InProgressResult, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// Visibility predicate operates on issues.project_id (NOT
	// events.project_id), so we inline rather than reuse the events-
	// flavoured buildVisibility helper above.
	//
	// War-room (globalScope=true) with no projectFilter applies NO
	// visibility clause — every in-progress issue is returned globally;
	// rows of invisible projects are content-redacted after scanning.
	// A projectFilter still pins to one project regardless.
	args := []any{}
	visClause := ""
	switch {
	case projectFilter != nil:
		args = append(args, *projectFilter)
		visClause = fmt.Sprintf(" AND i.project_id = $%d ", len(args))
	case unrestricted:
		visClause = ""
	case len(filterSetOf(visibleProjects, reach)) == 0:
		visClause = " AND FALSE "
	default:
		set := filterSetOf(visibleProjects, reach)
		ids := make([]uuid.UUID, 0, len(set))
		for pid := range set {
			ids = append(ids, pid)
		}
		args = append(args, ids)
		visClause = fmt.Sprintf(" AND i.project_id = ANY($%d) ", len(args))
	}
	args = append(args, limit)
	limitIdx := len(args)

	// issues.assignee is the denormalised username string; users.id is
	// resolved via a case-insensitive lookup (the same pattern as
	// notifications.ResolveAssigneeUserID and issues.list_by_user.go).
	// LEFT JOIN keeps unassigned rows (assignee IS NULL) in the result.
	sql := fmt.Sprintf(`
SELECT i.id, i.project_id, p.id_prefix,
       (SELECT sp.short_id FROM spaces sp WHERE sp.id = p.space_id),
       i.title, i.type, i.priority,
       i.assignee, u.id AS assignee_user_id, i.status,
       i.locked_by, i.locked_at, i.started_at, i.accepted_at,
       COALESCE(i.locked_at, i.started_at, i.accepted_at, i.updated_at) AS since
  FROM issues i
  JOIN projects p ON p.id = i.project_id
  `+assignee.LeftJoin("u", "i")+`
 WHERE (i.status = 'in_progress' OR i.locked_by IS NOT NULL)
   AND i.status <> 'closed'
   %s
 ORDER BY since DESC, i.id ASC
 LIMIT $%d`, visClause, limitIdx)

	rows, err := s.pool.Query(ctx, sql, reachPlanned(args)...)
	if err != nil {
		return nil, fmt.Errorf("listing in-progress issues: %w", err)
	}
	defer rows.Close()

	staleThreshold := s.now().Add(-25 * time.Minute)
	out := make([]InProgressRow, 0, limit)
	for rows.Next() {
		var r InProgressRow
		// Scan project_id into a local non-nil uuid.UUID so the
		// visibility comparison below runs on the real value FIRST; only
		// after that decision does the pointer field (which redaction may
		// nil) get set. i.project_id is NOT NULL.
		var projectID uuid.UUID
		var assignee *string
		var assigneeUserID *uuid.UUID
		var lockedBy *string
		var lockedAt, startedAt, acceptedAt *time.Time
		if err := rows.Scan(
			&r.IssueID, &projectID, &r.ProjectPrefix, &r.SpaceID, &r.Title, &r.Type, &r.Priority,
			&assignee, &assigneeUserID, &r.Status,
			&lockedBy, &lockedAt, &startedAt, &acceptedAt,
			&r.Since,
		); err != nil {
			return nil, fmt.Errorf("scanning in-progress row: %w", err)
		}
		r.ProjectID = &projectID
		r.Assignee = assignee
		r.AssigneeUserID = assigneeUserID
		r.LockedBySession = lockedBy
		r.LockedAt = lockedAt
		r.StartedAt = startedAt
		r.AcceptedAt = acceptedAt
		if lockedAt != nil && lockedAt.Before(staleThreshold) {
			r.StaleLock = true
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating in-progress rows: %w", err)
	}
	// War-room redaction: strip any row whose project is not in the
	// caller's visible set, at `level`. redact.None (the scoped path)
	// skips the loop — there are no invisible rows anyway. Declared
	// internal work is never redacted.
	if level != redact.None && !unrestricted {
		for i := range out {
			pid := out[i].ProjectID
			visibleRow := pid != nil
			if visibleRow {
				_, visibleRow = visibleProjects[*pid]
			}
			if !visibleRow {
				redactInProgressRow(&out[i], level)
			}
		}
	}
	return &InProgressResult{Items: out}, nil
}

// redactInProgressRow strips a "now working" row whose project the caller
// cannot see. The base strip (Title) runs for every level; a fail-closed
// switch then decides the identity strip. Only redact.Soft keeps identity
// (issue id, project prefix, projectId, lock session). redact.Hard — and
// ANY other/unknown level (fail closed: over-redact rather than leak) —
// additionally drops the issue id, project prefix, projectId and the raw
// Mcp-Session-Id lock holder. Kept: assignee, type, priority, status,
// timestamps, staleLock (the accepted "people + shape" exposure).
func redactInProgressRow(r *InProgressRow, level redact.Level) {
	r.Title = ""
	r.Redacted = true

	switch level {
	case redact.Soft:
		return
	default:
		r.IssueID = ""
		r.ProjectPrefix = ""
		r.SpaceID = ""
		r.ProjectID = nil
		r.LockedBySession = nil
	}
}

// Agents returns per-user MCP-session counts across four windows
// (live 15m / 24h / 7d / 30d) and all time. One SQL: four COUNT(*) FILTER
// expressions slice the same sessions for the windows. Result is
// ordered by live DESC then d30 DESC so active-right-now users float to
// the top, with all-time-busy users next when nobody is currently online.
//
// Cap at 50 rows — the dashboard zone is meant for an at-a-glance
// snapshot, not a full directory. Org-wide agent populations larger
// than that warrant a dedicated page.
//
// sk-674.18 T030 (US-13: "agents by scope"), reworded by sk-755 (owner's
// decision 2026-10-18): the reach picks the people, not the sessions. The
// people of the reach are the participants of the space of a reach project
// and the members of a reach project — the caller among them. Every session
// of such a person counts by its last appearance anywhere (agents.last_seen_at,
// any protocol traffic, reads and waits included), wherever it worked: a
// another user's agent busy in a space the caller is not in adds to the number
// (INV-28 allows the agent count; no project name is given). A person outside
// the people of the reach adds nothing. Unrestricted (declared internal work)
// counts every agent the same way, with no people filter.
//
// sk-740 counted a session by its last event in the reach, which dropped
// sessions that only read or waited (6 live instead of 31 on prod); sk-755
// retires that rule. The partial index idx_events_actor_session_seen (000085)
// served the sk-740 grouping and is no longer read by this query.
func (s *Store) Agents(ctx context.Context, reach map[uuid.UUID]struct{}, unrestricted bool) (*AgentsResult, error) {
	// seen — the session's last appearance anywhere, for every caller.
	sessions := `SELECT a.user_id, a.last_seen_at AS seen FROM agents a`
	args := []any{}
	if !unrestricted {
		ids := make([]uuid.UUID, 0, len(reach))
		for pid := range reach {
			ids = append(ids, pid)
		}
		args = append(args, ids)
		sessions += `
 WHERE a.user_id IN (SELECT sm.user_id FROM space_members sm
                      JOIN projects p ON p.space_id = sm.space_id
                     WHERE p.id = ANY($1::uuid[])
                     UNION
                     SELECT pm.user_id FROM project_members pm
                      WHERE pm.project_id = ANY($1::uuid[]))`
	}
	// The all-time column (COUNT(*) with no time predicate) requires the
	// scan to span every session, so the 30-day cut lives in HAVING, not
	// WHERE: only users with at least one session seen in the last 30 days
	// appear, while all_time counts sessions older than that window.
	rows, err := s.pool.Query(ctx, `WITH s AS (`+sessions+`)
SELECT u.username,
       COUNT(*) FILTER (WHERE s.seen > now() - interval '15 minutes') AS live,
       COUNT(*) FILTER (WHERE s.seen > now() - interval '24 hours')   AS h24,
       COUNT(*) FILTER (WHERE s.seen > now() - interval '7 days')     AS d7,
       COUNT(*) FILTER (WHERE s.seen > now() - interval '30 days')    AS d30,
       COUNT(*)                                                       AS all_time
  FROM s
  JOIN users u ON u.id = s.user_id
 WHERE s.seen IS NOT NULL
 GROUP BY u.username
HAVING COUNT(*) FILTER (WHERE s.seen > now() - interval '30 days') > 0
 ORDER BY live DESC, d30 DESC, all_time DESC, u.username ASC
 LIMIT 50`, reachPlanned(args)...)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	defer rows.Close()

	out := make([]AgentsRow, 0, 16)
	for rows.Next() {
		var r AgentsRow
		if err := rows.Scan(&r.Username, &r.Live, &r.H24, &r.D7, &r.D30, &r.AllTime); err != nil {
			return nil, fmt.Errorf("scanning agents row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating agents rows: %w", err)
	}
	return &AgentsResult{Items: out}, nil
}

// ---------- sk-29 — race chart ----------

// RacePoint is one (day, cumulative) datum on a user's race line.
// `Day` is a UTC date stamped to midnight; `Cumulative` is the running
// count of matching events authored by the user from effectiveStart up
// to and including this day.
type RacePoint struct {
	Day        time.Time `json:"day"`
	Cumulative int64     `json:"cumulative"`
}

// RaceUser is one user's line on the race chart. UserID may be nil for
// rows whose actor_user_id never got backfilled (legacy events); in
// that case the username is used as the stable identity key on the
// frontend.
type RaceUser struct {
	UserID   *uuid.UUID  `json:"userId,omitempty"`
	Username string      `json:"username"`
	Total    int64       `json:"total"`
	Points   []RacePoint `json:"points"`
}

// RaceOthers is the single "shadow line" lumping all users outside the
// top-N into one cumulative series. `UserCount` is the cardinality of
// contributing users (so the frontend can tooltip "+12 others").
type RaceOthers struct {
	UserCount int         `json:"userCount"`
	Total     int64       `json:"total"`
	Points    []RacePoint `json:"points"`
}

// RaceResult is the wire shape returned by GET /dashboard/race.
type RaceResult struct {
	Metric         string     `json:"metric"`
	Window         string     `json:"window"`
	FirstEventInDB *time.Time `json:"firstEventInDb,omitempty"`
	EffectiveStart time.Time  `json:"effectiveStart"`
	EffectiveEnd   time.Time  `json:"effectiveEnd"`
	IsColdStart    bool       `json:"isColdStart"`
	Users          []RaceUser `json:"users"`
	Others         RaceOthers `json:"others"`
}

// RaceArgs is the input bundle to Store.Race. Visibility is plumbed
// the same shape as Scoring: Unrestricted=true OR GlobalScope=true skips the
// visibility predicate (war-room: the chart carries only usernames +
// counts, so global aggregation leaks nothing); otherwise the set
// restricts events to those project ids.
//
// ProjectFilter narrows the leaderboard to one project, same as
// Scoring's — it takes precedence over Unrestricted/GlobalScope (see
// buildVisibilityForColumn: "a single project filter pins us to one
// row regardless of the unrestricted flag"). nil means the default org-wide
// leaderboard.
type RaceArgs struct {
	Metric          RaceMetric
	Window          RaceWindow
	TopN            int
	VisibleProjects map[uuid.UUID]struct{}
	Unrestricted    bool
	// Reach is the caller's HOT ZONE reach (sk-674.18 T030); nil — the
	// visible projects.
	Reach         map[uuid.UUID]struct{}
	ProjectFilter *uuid.UUID
}

// Race aggregates per-user cumulative counts of one metric over the
// window and returns the top-N users plus a single "others" line.
//
// Algorithm:
//
//  1. Resolve `firstEventInDb` (global MIN(occurred_at)) and clamp the
//     effective start: `effectiveStart = max(now - window, firstEventInDb)`
//     for finite windows, or `firstEventInDb` for window=all. The
//     cold-start flag is `effectiveStart == firstEventInDb` AND
//     `window != all` — the frontend renders «since X» when set.
//
//  2. Query per-user totals over [effectiveStart, now] filtered by
//     metric-specific event-type predicate + visibility. Pick the top
//     N by total DESC, then lump the rest into a single per-day series.
//
//  3. Query per-day per-user counts for the union of top + others,
//     generate the dense day series via generate_series, and compute
//     cumulative totals in Go (one pass per user). On days with no
//     activity the cumulative carries the previous day's value so the
//     polyline is evenly spaced and monotonic non-decreasing.
//
// We deliberately do per-day cumulative in Go rather than a SQL window
// function: the row counts are tiny (≤9 users × ≤90 days = ≤810 cells)
// and Go-side iteration is easier to read + audit. Pure SQL with a
// generate_series CROSS JOIN per user would be the alternative but
// adds another moving part for no measurable win.
func (s *Store) Race(ctx context.Context, args RaceArgs) (*RaceResult, error) {
	topN := args.TopN
	if topN <= 0 {
		topN = 8
	}
	if topN > 20 {
		topN = 20
	}

	// effectiveEnd is "now" rounded to the day so the chart always
	// extends through "today" — partial-day counts land on the right
	// edge.
	now := s.now().UTC()
	effectiveEnd := truncDayUTC(now)

	// firstEventInDb — the first event of the race's own reach (the same
	// projects and the same project filter as the race, every metric):
	// simpler than per-metric MIN per spec. Read inside the reach, not over
	// the whole table — an event of a project outside the HOT ZONE would
	// otherwise move the window and the cold-start flag and show its
	// timestamp (US-13, INV-23). NULL when the reach has
	// no events (a fresh DB, an empty reach); we return an empty result in
	// that case rather than constructing a malformed window.
	var firstEvent *time.Time
	{
		reachClause, reachArgs := buildVisibility(filterSetOf(args.VisibleProjects, args.Reach), args.Unrestricted, args.ProjectFilter)
		rendered, _ := renderPlaceholders(reachClause, 0)
		var t *time.Time
		if err := s.pool.QueryRow(ctx, `SELECT MIN(e.occurred_at) FROM events e WHERE TRUE`+rendered,
			reachPlanned(reachArgs)...).Scan(&t); err != nil {
			return nil, fmt.Errorf("loading first-event timestamp: %w", err)
		}
		firstEvent = t
	}

	// Clamp the start. For "all" → effectiveStart = firstEventInDb.
	// For finite → effectiveStart = max(now - window, firstEventInDb).
	var effectiveStart time.Time
	switch {
	case firstEvent == nil:
		// No events at all. Pick a degenerate but well-defined start
		// so the dense-day loop is trivially empty. Use effectiveEnd
		// to avoid emitting any day points.
		effectiveStart = effectiveEnd
	case args.Window.IsAll():
		effectiveStart = truncDayUTC(*firstEvent)
	default:
		windowStart := truncDayUTC(now.Add(-args.Window.Duration()))
		firstDay := truncDayUTC(*firstEvent)
		if firstDay.After(windowStart) {
			effectiveStart = firstDay
		} else {
			effectiveStart = windowStart
		}
	}

	isColdStart := false
	if firstEvent != nil && !args.Window.IsAll() {
		isColdStart = !truncDayUTC(*firstEvent).Before(truncDayUTC(now.Add(-args.Window.Duration())))
	}

	// Build the visibility predicate. Re-use buildVisibility (events-
	// flavoured). By default race is cross-project across the caller's
	// horizon (per spec); args.ProjectFilter is the opt-in narrowing —
	// same knob Scoring exposes via `?project=` — and it takes
	// precedence over Unrestricted/GlobalScope. metric=commits sources
	// issue_commits JOIN issues, so visibility filters on i.project_id
	// instead of e.project_id.
	commitsMetric := args.Metric == RaceMetricCommits
	var visClause string
	var visArgs []any
	if commitsMetric {
		visClause, visArgs = buildVisibilityForColumn(filterSetOf(args.VisibleProjects, args.Reach), args.Unrestricted, args.ProjectFilter, "i.project_id")
	} else {
		visClause, visArgs = buildVisibility(filterSetOf(args.VisibleProjects, args.Reach), args.Unrestricted, args.ProjectFilter)
	}

	// Build the metric-specific event-type WHERE clause for events-
	// sourced metrics. metric=commits bypasses this and queries
	// issue_commits directly (see totalsSQL / dailySQL branches).
	metricClause := ""
	if !commitsMetric {
		metricClause = s.raceMetricClause(args.Metric)
	}

	// Step 1 — per-user totals over the window. We need the username
	// AND the user id (when present) to keep identity stable for the
	// frontend. actor_user_id is the comment author for comment.created
	// rows (the events writer sets it from the caller), the closer for
	// issue.closed, and the creator for issue.created. The legacy
	// fallback path (actor_user_id IS NULL) uses LOWER(username)
	// equality against users.username so we can still attribute by
	// actor string. metric=commits uses issue_commits.linked_by /
	// linked_at instead (mirrors projectsQueryCommits).
	args1 := []any{effectiveStart, effectiveEnd}
	args1 = append(args1, visArgs...)
	visClauseRendered, _ := renderPlaceholders(visClause, len(args1)-len(visArgs))

	var totalsSQL string
	if commitsMetric {
		totalsSQL = fmt.Sprintf(`
WITH commit_actors AS (
    SELECT
        ic.linked_by                    AS user_id,
        COALESCE(u.username, 'system')  AS username
      FROM issue_commits ic
      JOIN issues i ON i.id = ic.issue_id
      LEFT JOIN users u ON u.id = ic.linked_by
     WHERE ic.linked_at >= $1::timestamptz
       AND ic.linked_at <  ($2::timestamptz + interval '1 day')
       %s
)
SELECT user_id, COALESCE(username, 'system') AS username, COUNT(*) AS total
  FROM commit_actors
 GROUP BY 1, 2
 ORDER BY total DESC, username ASC`, visClauseRendered)
	} else {
		totalsSQL = fmt.Sprintf(`
WITH actors AS (
    SELECT
        e.actor_user_id AS user_id,
        e.actor         AS actor_handle,
        e.entity_id     AS entity_id,
        e.entity_type   AS entity_type
      FROM events e
     WHERE e.occurred_at >= $1::timestamptz
       AND e.occurred_at <  ($2::timestamptz + interval '1 day')
       %s
       %s
)
SELECT
    a.user_id,
    COALESCE(u.username, a.actor_handle) AS username,
    COUNT(*) AS total
  FROM actors a
  LEFT JOIN users u
    ON u.id = a.user_id
       OR (a.user_id IS NULL AND LOWER(u.username) = LOWER(a.actor_handle))
 GROUP BY 1, 2
 ORDER BY total DESC, username ASC`, metricClause, visClauseRendered)
	}

	// sk-29 review fix Go W1 — `defer rows.Close()` immediately after
	// the err check matches the pattern used elsewhere in this package
	// (scanLeaderboard, matrixPerUser, InProgress, Agents). Double-close
	// is a documented no-op in pgx v5, so any future early-return
	// inside the loop is automatically covered.
	rows, err := s.pool.Query(ctx, totalsSQL, reachPlanned(args1)...)
	if err != nil {
		return nil, fmt.Errorf("loading race totals: %w", err)
	}
	defer rows.Close()
	type totalRow struct {
		UserID   *uuid.UUID
		Username string
		Total    int64
	}
	totals := make([]totalRow, 0, 64)
	for rows.Next() {
		var t totalRow
		if err := rows.Scan(&t.UserID, &t.Username, &t.Total); err != nil {
			return nil, fmt.Errorf("scanning race totals: %w", err)
		}
		totals = append(totals, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating race totals: %w", err)
	}
	rows.Close()

	// Build the dense day series.
	days := denseDays(effectiveStart, effectiveEnd)

	// Empty case: no totals → return empty users + empty others.
	if len(totals) == 0 {
		return &RaceResult{
			Metric:         string(args.Metric),
			Window:         string(args.Window),
			FirstEventInDB: firstEvent,
			EffectiveStart: effectiveStart,
			EffectiveEnd:   effectiveEnd,
			IsColdStart:    isColdStart,
			Users:          []RaceUser{},
			Others:         RaceOthers{UserCount: 0, Total: 0, Points: emptyPoints(days)},
		}, nil
	}

	// Split into top-N + others.
	topRows := totals
	var othersRows []totalRow
	if len(totals) > topN {
		topRows = totals[:topN]
		othersRows = totals[topN:]
	}

	// Step 2 — per-day per-user counts for the top users, plus a
	// single aggregate query for "others" lumped together. We use one
	// query for the top and one for the lump so the SQL stays small
	// and per-user grouping doesn't need a CASE.

	// Collect the top users' identity keys for the per-day query. We
	// match on user_id when present, falling back to a case-
	// insensitive actor-handle match for legacy rows. The dual-key
	// approach keeps a user's history intact across the actor_user_id
	// backfill boundary.
	topUserIDs := make([]uuid.UUID, 0, len(topRows))
	topHandles := make([]string, 0, len(topRows))
	for _, t := range topRows {
		if t.UserID != nil {
			topUserIDs = append(topUserIDs, *t.UserID)
		}
		topHandles = append(topHandles, strings.ToLower(t.Username))
	}

	// Per-day counts for the top users.
	args2 := []any{effectiveStart, effectiveEnd, topUserIDs, topHandles}
	args2 = append(args2, visArgs...)
	visClauseRendered2, _ := renderPlaceholders(visClause, len(args2)-len(visArgs))

	var dailySQL string
	if commitsMetric {
		dailySQL = fmt.Sprintf(`
WITH commit_actors AS (
    SELECT
        date_trunc('day', ic.linked_at AT TIME ZONE 'UTC')::date AS day,
        ic.linked_by                    AS user_id,
        COALESCE(u.username, 'system')  AS username
      FROM issue_commits ic
      JOIN issues i ON i.id = ic.issue_id
      LEFT JOIN users u ON u.id = ic.linked_by
     WHERE ic.linked_at >= $1::timestamptz
       AND ic.linked_at <  ($2::timestamptz + interval '1 day')
       %s
       AND (
           ($3::uuid[] IS NOT NULL AND ic.linked_by = ANY($3))
           OR
           ($4::text[] IS NOT NULL AND ic.linked_by IS NULL AND 'system' = ANY($4))
       )
)
SELECT day, user_id, username, COUNT(*) AS n
  FROM commit_actors
 GROUP BY 1, 2, 3`, visClauseRendered2)
	} else {
		dailySQL = fmt.Sprintf(`
WITH actors AS (
    SELECT
        date_trunc('day', e.occurred_at AT TIME ZONE 'UTC')::date AS day,
        e.actor_user_id AS user_id,
        LOWER(e.actor)  AS actor_handle
      FROM events e
     WHERE e.occurred_at >= $1::timestamptz
       AND e.occurred_at <  ($2::timestamptz + interval '1 day')
       %s
       %s
       AND (
           ($3::uuid[] IS NOT NULL AND e.actor_user_id = ANY($3))
           OR
           ($4::text[] IS NOT NULL AND e.actor_user_id IS NULL AND LOWER(e.actor) = ANY($4))
       )
)
SELECT
    a.day,
    a.user_id,
    COALESCE(u.username, a.actor_handle) AS username,
    COUNT(*) AS n
  FROM actors a
  LEFT JOIN users u
    ON u.id = a.user_id
       OR (a.user_id IS NULL AND LOWER(u.username) = a.actor_handle)
 GROUP BY 1, 2, 3`, metricClause, visClauseRendered2)
	}

	dayRows, err := s.pool.Query(ctx, dailySQL, reachPlanned(args2)...)
	if err != nil {
		return nil, fmt.Errorf("loading per-day top race counts: %w", err)
	}
	defer dayRows.Close()
	type cell struct {
		Day      time.Time
		UserID   *uuid.UUID
		Username string
		N        int64
	}
	var cells []cell
	for dayRows.Next() {
		var c cell
		if err := dayRows.Scan(&c.Day, &c.UserID, &c.Username, &c.N); err != nil {
			return nil, fmt.Errorf("scanning per-day cell: %w", err)
		}
		cells = append(cells, c)
	}
	if err := dayRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating per-day cells: %w", err)
	}
	dayRows.Close()

	// Index cells by (identity, day) — identity is the same key used
	// in topRows so we can match unambiguously. We use uuid VALUE (not
	// pointer) + lowercase handle as the composite key so two pointers
	// to the same uuid value collapse to the same map entry (Go maps
	// compare *T by pointer identity, which would otherwise split
	// every row into its own bucket).
	type identity struct {
		UserID    uuid.UUID
		HasUserID bool
		Handle    string // lowercase
	}
	keyOf := func(uid *uuid.UUID, name string) identity {
		if uid != nil {
			return identity{UserID: *uid, HasUserID: true, Handle: strings.ToLower(name)}
		}
		return identity{HasUserID: false, Handle: strings.ToLower(name)}
	}
	dayCounts := map[identity]map[time.Time]int64{}
	for _, c := range cells {
		k := keyOf(c.UserID, c.Username)
		if _, ok := dayCounts[k]; !ok {
			dayCounts[k] = make(map[time.Time]int64, len(days))
		}
		dayCounts[k][truncDayUTC(c.Day)] += c.N
	}

	// Build the users[] output preserving topRows order.
	users := make([]RaceUser, 0, len(topRows))
	for _, t := range topRows {
		k := keyOf(t.UserID, t.Username)
		pts := buildCumulative(days, dayCounts[k])
		users = append(users, RaceUser{
			UserID:   t.UserID,
			Username: t.Username,
			Total:    t.Total,
			Points:   pts,
		})
	}

	// Step 3 — others. Two paths: (a) zero remaining users → empty
	// shadow line; (b) ≥1 remaining → a single query summing per-day
	// counts across that set.
	others := RaceOthers{UserCount: 0, Total: 0, Points: emptyPoints(days)}
	if len(othersRows) > 0 {
		othersUserIDs := make([]uuid.UUID, 0, len(othersRows))
		othersHandles := make([]string, 0, len(othersRows))
		var othersTotal int64
		for _, t := range othersRows {
			othersTotal += t.Total
			if t.UserID != nil {
				othersUserIDs = append(othersUserIDs, *t.UserID)
			}
			othersHandles = append(othersHandles, strings.ToLower(t.Username))
		}

		args3 := []any{effectiveStart, effectiveEnd, othersUserIDs, othersHandles}
		args3 = append(args3, visArgs...)
		visClauseRendered3, _ := renderPlaceholders(visClause, len(args3)-len(visArgs))

		var othersSQL string
		if commitsMetric {
			othersSQL = fmt.Sprintf(`
WITH commit_actors AS (
    SELECT
        date_trunc('day', ic.linked_at AT TIME ZONE 'UTC')::date AS day
      FROM issue_commits ic
      JOIN issues i ON i.id = ic.issue_id
     WHERE ic.linked_at >= $1::timestamptz
       AND ic.linked_at <  ($2::timestamptz + interval '1 day')
       %s
       AND (
           ($3::uuid[] IS NOT NULL AND ic.linked_by = ANY($3))
           OR
           ($4::text[] IS NOT NULL AND ic.linked_by IS NULL AND 'system' = ANY($4))
       )
)
SELECT day, COUNT(*) AS n
  FROM commit_actors
 GROUP BY 1`, visClauseRendered3)
		} else {
			othersSQL = fmt.Sprintf(`
SELECT
    date_trunc('day', e.occurred_at AT TIME ZONE 'UTC')::date AS day,
    COUNT(*) AS n
  FROM events e
 WHERE e.occurred_at >= $1::timestamptz
   AND e.occurred_at <  ($2::timestamptz + interval '1 day')
   %s
   %s
   AND (
       ($3::uuid[] IS NOT NULL AND e.actor_user_id = ANY($3))
       OR
       ($4::text[] IS NOT NULL AND e.actor_user_id IS NULL AND LOWER(e.actor) = ANY($4))
   )
 GROUP BY 1`, metricClause, visClauseRendered3)
		}

		oRows, err := s.pool.Query(ctx, othersSQL, reachPlanned(args3)...)
		if err != nil {
			return nil, fmt.Errorf("loading others per-day counts: %w", err)
		}
		defer oRows.Close()
		othersDayCounts := make(map[time.Time]int64, len(days))
		for oRows.Next() {
			var d time.Time
			var n int64
			if err := oRows.Scan(&d, &n); err != nil {
				return nil, fmt.Errorf("scanning others row: %w", err)
			}
			othersDayCounts[truncDayUTC(d)] += n
		}
		if err := oRows.Err(); err != nil {
			return nil, fmt.Errorf("iterating others rows: %w", err)
		}
		oRows.Close()

		others = RaceOthers{
			UserCount: len(othersRows),
			Total:     othersTotal,
			Points:    buildCumulative(days, othersDayCounts),
		}
	}

	return &RaceResult{
		Metric:         string(args.Metric),
		Window:         string(args.Window),
		FirstEventInDB: firstEvent,
		EffectiveStart: effectiveStart,
		EffectiveEnd:   effectiveEnd,
		IsColdStart:    isColdStart,
		Users:          users,
		Others:         others,
	}, nil
}

// raceMetricClause returns the event-type WHERE fragment for events-
// sourced race metrics. The fragment starts with " AND " so it composes
// onto the other predicates without a leading WHERE.
//
// `closed`      → e.type='issue.closed'    AND e.entity_type='issue'
// `closed_done` → the same, narrowed to closes whose outcome is "done"
//
//	(sk-494, closeDonePredicate)
//
// `created`  → e.type='issue.created'  AND e.entity_type='issue'
// `comments` → e.type='comment.created' AND e.entity_type='comment'
//
// `commits` is NOT handled here — Store.Race branches to issue_commits
// queries keyed on linked_by / linked_at (one row per SHA).
func (s *Store) raceMetricClause(m RaceMetric) string {
	switch m {
	case RaceMetricCreated:
		return " AND e.type = 'issue.created' AND e.entity_type = 'issue' "
	case RaceMetricComments:
		return " AND e.type = 'comment.created' AND e.entity_type = 'comment' "
	case RaceMetricClosedDone:
		// Built from the "closed" clause plus the common outcome predicate, not
		// written out in full: it can no longer diverge from `closed` in event type or in
		// entity_type.
		return " AND e.type = 'issue.closed' AND e.entity_type = 'issue' " + closeDonePredicate
	default: // RaceMetricClosed and fallback
		return " AND e.type = 'issue.closed' AND e.entity_type = 'issue' "
	}
}

// denseDays returns the inclusive list of UTC midnights from
// effectiveStart through effectiveEnd. Both ends are clamped to day
// boundaries by the caller; if start > end, returns empty.
func denseDays(start, end time.Time) []time.Time {
	if start.After(end) {
		return nil
	}
	out := make([]time.Time, 0, int(end.Sub(start).Hours()/24)+1)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out
}

// emptyPoints returns one zero-cumulative RacePoint per day. Used for
// the empty-result fall-through so the frontend can render an empty
// shadow line without special-casing nil.
func emptyPoints(days []time.Time) []RacePoint {
	if len(days) == 0 {
		return []RacePoint{}
	}
	out := make([]RacePoint, 0, len(days))
	for _, d := range days {
		out = append(out, RacePoint{Day: d, Cumulative: 0})
	}
	return out
}

// buildCumulative folds per-day raw counts into a monotone-non-
// decreasing cumulative series across the dense day list. Missing
// days inherit the previous day's cumulative (no resets).
func buildCumulative(days []time.Time, byDay map[time.Time]int64) []RacePoint {
	if len(days) == 0 {
		return []RacePoint{}
	}
	out := make([]RacePoint, 0, len(days))
	var run int64
	for _, d := range days {
		run += byDay[d]
		out = append(out, RacePoint{Day: d, Cumulative: run})
	}
	return out
}

// truncDayUTC strips the time-of-day component (UTC) returning the
// midnight of the same day. We compute the day stamp on the Go side
// rather than relying on pg's date_trunc-then-cast pipeline to avoid
// pgtype surprises with the date/timestamp dance.
func truncDayUTC(t time.Time) time.Time {
	tu := t.UTC()
	return time.Date(tu.Year(), tu.Month(), tu.Day(), 0, 0, 0, 0, time.UTC)
}
