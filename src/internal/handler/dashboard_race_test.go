// dashboard_race_test — integration tests for sk-29.1's
// GET /api/v1/dashboard/race endpoint. Mirrors the test conventions in
// dashboard_test.go: live TEST_DATABASE_URL pool, per-test seed +
// cleanup, deterministic actor-userid assignment. All time math runs in
// UTC; seed timestamps are anchored relative to "now" so window
// arithmetic in the handler matches.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// raceRouter mounts ONLY the race endpoint — keeps the test surface
// narrow (the rest of the dashboard handler is exercised in
// dashboard_test.go).
func raceRouter(h *DashboardHandler) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/v1/dashboard/race", h.Race)
	return r
}

func callRace(t *testing.T, h *DashboardHandler, target string, userID uuid.UUID, role string) (*http.Response, []byte) {
	t.Helper()
	r := raceRouter(h)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if userID != uuid.Nil {
		ctx := auth.WithUserID(req.Context(), userID)
		ctx = auth.WithUserRole(ctx, role)
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, rec.Body.Bytes()
}

// seedRaceClosedEvent inserts an `issue.closed` event row at a precise
// timestamp. Used by every race test that needs to assert per-day or
// per-user counts.
func seedRaceClosedEvent(
	t *testing.T,
	pool *pgxpool.Pool,
	issueID string,
	projectID uuid.UUID,
	actor string,
	actorUserID uuid.UUID,
	occurredAt time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'issue.closed', $2, $3, 'human', 'issue', $4, $5, '{}'::jsonb)`,
		occurredAt, actor, actorUserID, issueID, projectID); err != nil {
		t.Fatalf("seed closed event %s: %v", issueID, err)
	}
}

// seedRaceCreatedEvent inserts an `issue.created` event row.
func seedRaceCreatedEvent(
	t *testing.T,
	pool *pgxpool.Pool,
	issueID string,
	projectID uuid.UUID,
	actor string,
	actorUserID uuid.UUID,
	occurredAt time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'issue.created', $2, $3, 'human', 'issue', $4, $5, '{}'::jsonb)`,
		occurredAt, actor, actorUserID, issueID, projectID); err != nil {
		t.Fatalf("seed created event %s: %v", issueID, err)
	}
}

// seedRaceCommentEvent inserts a `comment.created` event row. The
// entity_id is the comment uuid (string form). entity_type='comment'
// per the events table CHECK and the existing comment.created emit
// path.
func seedRaceCommentEvent(
	t *testing.T,
	pool *pgxpool.Pool,
	commentUUID uuid.UUID,
	projectID uuid.UUID,
	actor string,
	actorUserID uuid.UUID,
	occurredAt time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'comment.created', $2, $3, 'human', 'comment', $4, $5, '{}'::jsonb)`,
		occurredAt, actor, actorUserID, commentUUID.String(), projectID); err != nil {
		t.Fatalf("seed comment.created event %s: %v", commentUUID, err)
	}
}

// seedRaceIssueCommit attaches one row in issue_commits. Used by the
// metric=commits path; linked_at controls day bucketing.
func seedRaceIssueCommit(
	t *testing.T,
	pool *pgxpool.Pool,
	issueID string,
	linkedBy uuid.UUID,
	linkedAt time.Time,
) {
	t.Helper()
	repoID := uuid.Must(uuid.NewRandom())
	hash := uuid.Must(uuid.NewRandom()).String()[:12]
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_commits (issue_id, repo_id, hash, linked_by, linked_by_kind, linked_at)
VALUES ($1, $2, $3, $4, 'human', $5)`,
		issueID, repoID, hash, linkedBy, linkedAt); err != nil {
		t.Fatalf("seed issue_commit: %v", err)
	}
}

// raceResp mirrors dashboard.RaceResult on the wire so tests can
// decode + assert without importing the store struct (which would
// pull pgx types into the test scope).
type raceResp struct {
	Metric         string     `json:"metric"`
	Window         string     `json:"window"`
	FirstEventInDB *time.Time `json:"firstEventInDb"`
	EffectiveStart time.Time  `json:"effectiveStart"`
	EffectiveEnd   time.Time  `json:"effectiveEnd"`
	IsColdStart    bool       `json:"isColdStart"`
	Users          []struct {
		UserID   *string `json:"userId"`
		Username string  `json:"username"`
		Total    int64   `json:"total"`
		Points   []struct {
			Day        time.Time `json:"day"`
			Cumulative int64     `json:"cumulative"`
		} `json:"points"`
	} `json:"users"`
	Others struct {
		UserCount int   `json:"userCount"`
		Total     int64 `json:"total"`
		Points    []struct {
			Day        time.Time `json:"day"`
			Cumulative int64     `json:"cumulative"`
		} `json:"points"`
	} `json:"others"`
}

// decodeRace decodes the JSON body or t.Fatals on error.
func decodeRace(t *testing.T, body []byte) raceResp {
	t.Helper()
	var got raceResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode race body: %v\nbody=%s", err, string(body))
	}
	return got
}

// TestRace_Canonical30d seeds three users closing tasks, all within a
// single project, and scopes the request to that project (`&project=`)
// so the response is exactly what this test seeded — no shared-DB noise
// to filter out. Asserts:
//   - users[] holds EXACTLY our three seeded users, ordered by total DESC
//   - cumulative is monotonic non-decreasing per user
//   - the final cumulative equals total
//
// sk-347 — before this, the request carried no `&project=` filter, so
// on a non-empty database (other packages' events, or a prior test's
// leftovers) the assertions read the caller/bob/eve rows out of a
// `byName` map keyed off the GLOBAL top-N list. The map lookup could
// find them regardless of what else was in `got.Users`, but nothing
// stopped foreign rows from being interleaved among them or from
// crowding one of the three out of the default topN=8 entirely — a
// silent false negative the test never exercised because the harness
// always starts from `down -v`. Scoping to `proj` and asserting the
// EXACT user count makes the fixture's own isolation the thing under
// test, not the harness's.
//
// sk-347 (found in review) — scoping the REQUEST
// to `proj` is not the same as proving `ProjectFilter` reaches the
// query: on a CLEAN, isolated database (no cross-package noise) this
// fixture used to seed events into `proj` alone, so zeroing
// `RaceArgs.ProjectFilter` in the handler left every assertion below
// green — there was no OTHER project's data for a dropped filter to
// leak in. `otherProj` + `noise` below exist for exactly that: a second
// project owned by the same caller, with a fourth user's closes that
// belong ONLY to `otherProj`. With the filter working, `?project=proj`
// makes those events invisible and every assertion below is unchanged.
// With the filter dropped, `noise` surfaces as a 4th row and the very
// first assertion (`len(got.Users) != 3`) goes red — on a clean DB, no
// shared-DB luck required. Mirrors the two-project shape
// TestRace_CommentsMetric already uses for metric=comments.
func TestRace_Canonical30d(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	bob, bobUname := seedDashboardUser(t, pool, "user")
	eve, eveUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)

	// otherProj + noise: a second project, invisible to the `?project=`
	// filter below by construction. If ProjectFilter stops reaching the
	// query, these 4 closes surface as a 4th user and the exact-count
	// assertions immediately below go red — see the round-2 doc note
	// above.
	otherProj, _ := seedDashboardProject(t, pool, caller)
	noiseUser, noiseUname := seedDashboardUser(t, pool, "user")

	now := time.Now().UTC()

	for i := 0; i < 4; i++ {
		issueID := fmt.Sprintf("rc-noise-%d-%s", i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, otherProj, caller, issueID, "human")
		occurred := utcDay(now).AddDate(0, 0, -(6 - i)).Add(2 * time.Hour)
		seedRaceClosedEvent(t, pool, issueID, otherProj, noiseUname, noiseUser, occurred)
	}

	// caller: 5 closes spread across the last 10 days
	// bob:    3 closes
	// eve:    1 close
	type seedCfg struct {
		user      uuid.UUID
		uname     string
		nCloses   int
		dayOffset int // first close at now - dayOffset days
	}
	cfgs := []seedCfg{
		{caller, callerUname, 5, 10},
		{bob, bobUname, 3, 7},
		{eve, eveUname, 1, 2},
	}
	for _, cfg := range cfgs {
		for i := 0; i < cfg.nCloses; i++ {
			issueID := fmt.Sprintf("rc-%s-%d-%s", cfg.uname, i, uuid.Must(uuid.NewRandom()).String()[:6])
			seedDashboardIssue(t, pool, proj, caller, issueID, "human")
			// sk-205: anchor to UTC midnight, then offset — otherwise the
			// +2h rolls the point into the next calendar day for late-in-
			// the-day runs and "mid-day, N days ago" stops being true.
			occurred := utcDay(now).AddDate(0, 0, -(cfg.dayOffset - i)).Add(2 * time.Hour) // mid-day
			seedRaceClosedEvent(t, pool, issueID, proj, cfg.uname, cfg.user, occurred)
		}
	}

	target := fmt.Sprintf("/api/v1/dashboard/race?metric=closed&window=30d&project=%s", proj.String())
	res, body := callRace(t, h, target, caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)

	if got.Metric != "closed" || got.Window != "30d" {
		t.Fatalf("wrong echo metric/window: %+v", got)
	}

	// `?project=` scopes the response to exactly what this test seeded —
	// no shared-DB noise can appear here, so the count itself is a real
	// assertion: it goes red the moment ProjectFilter stops reaching the
	// query, not just when a value happens to differ.
	if len(got.Users) != 3 {
		t.Fatalf("expected exactly 3 users (project-scoped), got %d: %+v", len(got.Users), got.Users)
	}
	byName := make(map[string]int64, 3)
	for _, u := range got.Users {
		byName[u.Username] = u.Total
	}
	if byName[callerUname] != 5 {
		t.Fatalf("expected caller total=5, got %d (users=%+v)", byName[callerUname], got.Users)
	}
	if byName[bobUname] != 3 {
		t.Fatalf("expected bob total=3, got %d", byName[bobUname])
	}
	if byName[eveUname] != 1 {
		t.Fatalf("expected eve total=1, got %d", byName[eveUname])
	}

	// Ordering: caller > bob > eve — the FULL users[] list now, not a
	// filtered subset, since project-scoping means there is nothing else
	// in it.
	if got.Users[0].Username != callerUname || got.Users[1].Username != bobUname || got.Users[2].Username != eveUname {
		t.Fatalf("expected order [caller, bob, eve], got [%s, %s, %s]",
			got.Users[0].Username, got.Users[1].Username, got.Users[2].Username)
	}

	// Cumulative monotonicity + final-equals-total per row.
	for _, u := range got.Users {
		var prev int64
		for i, p := range u.Points {
			if p.Cumulative < prev {
				t.Fatalf("non-monotonic cumulative for %s at index %d: %d < %d",
					u.Username, i, p.Cumulative, prev)
			}
			prev = p.Cumulative
		}
		if len(u.Points) == 0 {
			t.Fatalf("user %s has empty points", u.Username)
		}
		final := u.Points[len(u.Points)-1].Cumulative
		if final != u.Total {
			t.Fatalf("user %s: final cumulative %d != total %d", u.Username, final, u.Total)
		}
	}
}

// TestRace_ColdStartClamp seeds only ~3 days of data, all within a
// single project, and requests a 30d window scoped to that project
// (`&project=`). The effectiveStart should clamp to the oldest event;
// isColdStart should be true; the day range should reflect ~3 days
// rather than 30.
//
// sk-347 — the users[] assertion below is project-scoped and asserts an
// EXACT count (1: just the caller) so a leak of foreign users back into
// the response fails loudly instead of merely widening a `found` search.
// firstEventInDb / effectiveStart / isColdStart stay UNSCOPED on
// purpose — Store.Race resolves firstEventInDb as a database-wide
// `MIN(occurred_at)` regardless of ProjectFilter (documented in
// dashboard/store.go: "simpler than per-metric MIN per spec"), so no
// amount of project scoping makes those three deterministic against a
// non-empty database. They keep the self-consistency check they already
// had (effectiveStart == max(firstEventInDb day, window-start day)) —
// that's not a self-sufficiency gap, it's the documented global
// behaviour of a different field.
//
// sk-347 (found in review) — same gap as
// TestRace_Canonical30d: on a clean, isolated DB there was no OTHER
// project's data for a dropped ProjectFilter to leak, so the exact
// `len(got.Users) != 1` check below stayed green even with the filter
// zeroed. `otherProj` + `noise` give it something to leak: a second
// project, owned by the same caller, holding a second user's closes.
// Its timestamps are irrelevant to the (deliberately unscoped, see
// above) firstEventInDb/effectiveStart/isColdStart self-consistency
// check — that check derives its expectation FROM got.FirstEventInDB,
// whatever the DB actually returns, so adding older or younger rows
// elsewhere cannot break it either way.
func TestRace_ColdStartClamp(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)

	// otherProj + noise: invisible to `?project=proj` by construction.
	// If ProjectFilter stops reaching the query, this second user
	// surfaces alongside caller and `len(got.Users) != 1` goes red — see
	// the round-2 doc note above.
	otherProj, _ := seedDashboardProject(t, pool, caller)
	noiseUser, noiseUname := seedDashboardUser(t, pool, "user")

	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		issueID := fmt.Sprintf("cs-noise-%d-%s", i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, otherProj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, otherProj, noiseUname, noiseUser, now.Add(-time.Duration(i+1)*24*time.Hour))
	}

	// Three closes: 2d ago, 1d ago, 0d ago.
	// To exercise the cold-start clamp we need our events to be the
	// OLDEST in the table. Against a shared TEST_DATABASE_URL with
	// historical data that's not guaranteed. We assert the soft
	// invariants instead: effectiveStart >= firstEventInDb day and
	// effectiveStart <= our oldest seeded day. The cold-start flag
	// is asserted via the day-arithmetic relationship (window minus
	// firstEvent).
	for i := 0; i < 3; i++ {
		issueID := fmt.Sprintf("cs-%s-%d-%s", callerUname, i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, proj, callerUname, caller, now.Add(-time.Duration(2-i)*24*time.Hour))
	}

	target := fmt.Sprintf("/api/v1/dashboard/race?metric=closed&window=30d&project=%s", proj.String())
	res, body := callRace(t, h, target, caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)

	if got.FirstEventInDB == nil {
		t.Fatalf("expected firstEventInDb to be set")
	}
	// effectiveStart must equal the day of firstEventInDb (when the
	// clamp fires) — but on a shared DB with older rows from prior
	// tests, the firstEventInDb may be more than 30 days old or
	// younger. We just assert internal consistency:
	expectedWindowStart := now.Add(-30 * 24 * time.Hour)
	firstDay := time.Date(got.FirstEventInDB.Year(), got.FirstEventInDB.Month(), got.FirstEventInDB.Day(), 0, 0, 0, 0, time.UTC)
	winStartDay := time.Date(expectedWindowStart.Year(), expectedWindowStart.Month(), expectedWindowStart.Day(), 0, 0, 0, 0, time.UTC)
	// effectiveStart should equal MAX(firstDay, winStartDay).
	var wantStart time.Time
	if firstDay.After(winStartDay) {
		wantStart = firstDay
	} else {
		wantStart = winStartDay
	}
	if !got.EffectiveStart.Equal(wantStart) {
		t.Fatalf("effectiveStart=%s want %s (first=%s winStart=%s)",
			got.EffectiveStart.Format(time.RFC3339),
			wantStart.Format(time.RFC3339),
			firstDay.Format(time.RFC3339),
			winStartDay.Format(time.RFC3339))
	}
	// isColdStart MUST be true iff firstDay > winStartDay AND window != all.
	wantCold := firstDay.After(winStartDay) || firstDay.Equal(winStartDay)
	// Strictly: cold-start is when first-event day is NOT BEFORE the
	// nominal window-start day. The store uses `!before(...)`, so
	// equality counts as cold-start too.
	if got.IsColdStart != wantCold {
		t.Fatalf("isColdStart=%v want %v (first=%s winStart=%s)",
			got.IsColdStart, wantCold,
			firstDay.Format(time.RFC3339),
			winStartDay.Format(time.RFC3339))
	}

	// `?project=` scopes users[] to exactly what this test seeded: the
	// caller and no one else. Assert the exact count, not just presence —
	// a leaked foreign user (ProjectFilter not reaching this query path)
	// would otherwise pass silently as long as the caller's own row was
	// still findable somewhere in the list.
	if len(got.Users) != 1 {
		t.Fatalf("expected exactly 1 user (project-scoped), got %d: %+v", len(got.Users), got.Users)
	}
	if got.Users[0].Username != callerUname {
		t.Fatalf("expected caller as sole user, got %q", got.Users[0].Username)
	}
	if got.Users[0].Total != 3 {
		t.Fatalf("caller total=%d want 3", got.Users[0].Total)
	}
}

// TestRace_TopNAndOthers seeds 12 users with a distinct close count
// each (12, 11, ..., 1), all within a single project, and scopes the
// request to that project (`&project=`) so topN=8 splits EXACTLY along
// the seeded counts:
//   - users[] holds exactly our top 8 (caller=12 down to the 8th user=5), in
//     that order;
//   - others aggregates exactly the remaining 4 (the 9th to 12th users,
//     counts 4+3+2+1=10).
//
// sk-347 — before this, the request carried no `&project=` filter and
// the assertions only checked "our caller is somewhere in users[]" plus
// "others is non-zero" — properties a shared, non-empty database could
// satisfy by accident (foreign rows filling out the top-8, or
// contributing to `others`) even if ProjectFilter never reached the
// query. Scoping to `proj` and asserting the exact split makes the
// numbers themselves the proof.
//
// sk-347 (found in review) — the fixture used
// "a single project membership for the caller" (see the comment further
// down) precisely so ProjectFilter had nothing to do on a clean,
// isolated DB: with only ONE project visible to the caller, zeroing
// RaceArgs.ProjectFilter changes nothing to select from. `otherProj` +
// `noise` fix that: a second project the caller also owns, holding a
// 13th user's closes — 13, one more than caller's own 12, so a leak
// does not just widen `others`, it takes the #1 slot in `users[]` and
// every ordering/value assertion below goes red immediately.
func TestRace_TopNAndOthers(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)

	// 12 users. Caller is one of them, each gets exactly one close.
	// To break the topN tie deterministically by total + username
	// alphabetically we seed each user with a different total: the
	// caller gets 12 closes, the 2nd user gets 11, ..., the 12th gets 1. That
	// way the top-8 are caller .. the 8th user and others = the 9th to 12th (4
	// users). Use a unique prefix so we can filter out
	// shared-DB noise.
	type seeded struct {
		uid   uuid.UUID
		uname string
		count int
	}
	seeds := make([]seeded, 0, 12)
	seeds = append(seeds, seeded{caller, callerUname, 12})
	for i := 2; i <= 12; i++ {
		uid, uname := seedDashboardUser(t, pool, "user")
		seeds = append(seeds, seeded{uid, uname, 13 - i})
	}

	// Use a single project membership for the caller — events on
	// other projects would still fan into our visible set because the
	// caller is owner of `proj` and we use `proj` for every event.
	now := time.Now().UTC()
	for _, s := range seeds {
		for i := 0; i < s.count; i++ {
			issueID := fmt.Sprintf("tn-%s-%d-%s", s.uname, i, uuid.Must(uuid.NewRandom()).String()[:6])
			seedDashboardIssue(t, pool, proj, caller, issueID, "human")
			seedRaceClosedEvent(t, pool, issueID, proj, s.uname, s.uid, now.Add(-time.Duration(i+1)*time.Hour))
		}
	}

	// otherProj + noise: a second project the caller ALSO owns (so it's
	// in the caller's HOT ZONE reach — every project of the reach would
	// fan into an unfiltered response, sk-674.18 T030 removed
	// globalScope), invisible to `?project=proj` by construction. See the round-2 doc note above
	// for why 13 (one more than caller's 12) makes a leak unmistakable.
	otherProj, _ := seedDashboardProject(t, pool, caller)
	noiseUser, noiseUname := seedDashboardUser(t, pool, "user")
	const noiseCount = 13
	for i := 0; i < noiseCount; i++ {
		issueID := fmt.Sprintf("tn-noise-%d-%s", i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, otherProj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, otherProj, noiseUname, noiseUser, now.Add(-time.Duration(i+1)*time.Minute))
	}

	target := fmt.Sprintf("/api/v1/dashboard/race?metric=closed&window=30d&topN=8&project=%s", proj.String())
	res, body := callRace(t, h, target, caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)

	// `?project=` scopes both users[] and others to exactly our 12
	// seeds — top8 = caller(12)..8th(5), others = 9th(4)..12th(1).
	if len(got.Users) != 8 {
		t.Fatalf("expected exactly 8 users in topN=8 response, got %d: %+v", len(got.Users), got.Users)
	}
	for i, want := range seeds[:8] {
		if got.Users[i].Username != want.uname {
			t.Fatalf("users[%d] = %q, want %q (order caller..8th user by descending count)",
				i, got.Users[i].Username, want.uname)
		}
		if got.Users[i].Total != int64(want.count) {
			t.Fatalf("users[%d] (%s) total=%d, want %d", i, want.uname, got.Users[i].Total, want.count)
		}
	}

	const wantOthersCount = 4             // the 9th to 12th users
	const wantOthersTotal = 4 + 3 + 2 + 1 // = 10
	if got.Others.UserCount != wantOthersCount {
		t.Fatalf("others.userCount=%d want %d", got.Others.UserCount, wantOthersCount)
	}
	if got.Others.Total != wantOthersTotal {
		t.Fatalf("others.total=%d want %d", got.Others.Total, wantOthersTotal)
	}
}

// TestRace_MetricSwitch — same caller, request metric=created vs
// metric=closed, assert the per-user totals track the seeded counts
// (created differs from closed), proving the metric filter selects the
// right event-type predicate.
//
// Isolation note: /dashboard/race is a top-N leaderboard over the caller's
// HOT ZONE reach (sk-674.18 T030: no globalScope; no project/username
// filter here, topN capped at 20). A low-volume seeded user can be crowded
// out of the returned top-N by other events of the reach in the same
// window — when that happens the
// caller is folded into the un-itemised `others` aggregate and its own
// total is unreadable (this is exactly how the test failed on a clean DB
// when TestRace_TopNAndOthers ran first). To make the assertion
// deterministic regardless of ambient rows we (a) seed the caller with
// counts large enough to guarantee a top-N slot — the highest single-user
// count anywhere else in the suite is ~12, so 30/29 dominates with margin
// — (b) request the maximum topN=20, and (c) still assert the EXACT
// seeded counts (30 created vs 29 closed), which differ between the two
// metrics and so prove the switch.
func TestRace_MetricSwitch(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)

	const (
		wantCreated = 30
		wantClosed  = 29
	)

	now := time.Now().UTC()
	for i := 0; i < wantCreated; i++ {
		issueID := fmt.Sprintf("ms-cre-%d-%s", i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceCreatedEvent(t, pool, issueID, proj, callerUname, caller, now.Add(-time.Duration(i+1)*time.Minute))
	}
	for i := 0; i < wantClosed; i++ {
		issueID := fmt.Sprintf("ms-cls-%d-%s", i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, proj, callerUname, caller, now.Add(-time.Duration(i+1)*time.Minute))
	}

	callerTotal := func(target string) int64 {
		t.Helper()
		res, body := callRace(t, h, target, caller, "user")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s expected 200, got %d body=%s", target, res.StatusCode, string(body))
		}
		got := decodeRace(t, body)
		for _, u := range got.Users {
			if u.Username == callerUname {
				return u.Total
			}
		}
		t.Fatalf("caller %q missing from top-N users[] for %s; users=%+v",
			callerUname, target, got.Users)
		return 0
	}

	if got := callerTotal("/api/v1/dashboard/race?metric=created&window=30d&topN=20"); got != wantCreated {
		t.Fatalf("metric=created caller total=%d want %d", got, wantCreated)
	}
	if got := callerTotal("/api/v1/dashboard/race?metric=closed&window=30d&topN=20"); got != wantClosed {
		t.Fatalf("metric=closed caller total=%d want %d", got, wantClosed)
	}
}

// TestRace_EmptyWindow seeds events well outside a small window and
// asserts:
//   - users[] does NOT include our caller (no events in window),
//   - the response shape is still well-formed (200 + JSON parses).
//
// We cannot assert users=[] because the shared DB has unrelated rows;
// we assert the absence of OUR seeded user instead.
func TestRace_EmptyWindow(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)

	now := time.Now().UTC()
	// Two closes 14 and 15 days ago — outside the 7d window.
	for _, daysAgo := range []int{14, 15} {
		issueID := fmt.Sprintf("ew-%d-%s", daysAgo, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, proj, callerUname, caller, now.Add(-time.Duration(daysAgo)*24*time.Hour))
	}

	res, body := callRace(t, h, "/api/v1/dashboard/race?metric=closed&window=7d", caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)
	for _, u := range got.Users {
		if u.Username == callerUname {
			t.Fatalf("caller appeared in users[] despite all events outside the 7d window: total=%d points=%+v",
				u.Total, u.Points)
		}
	}
}

// TestRace_AuthGateAndVisibility asserts two things:
//   - unauthenticated request returns 401;
//   - a non-member caller cannot see events from a project they're not
//     a member of (no leak via the race surface).
func TestRace_AuthGateAndVisibility(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	// (1) Unauthenticated.
	res, body := callRace(t, h, "/api/v1/dashboard/race?metric=closed&window=7d", uuid.Nil, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth expected 401, got %d body=%s", res.StatusCode, string(body))
	}

	// (2) Visibility — the race has no global scope any more (sk-674.18
	// T030, row 201): it counts the caller's HOT ZONE reach. callerA is in
	// project pA only; callerB owns pB in a space of his own, outside
	// callerA's reach (a project of the shared test space would be
	// callerA's contribution — his own space — and legitimately counted).
	// callerB closes 40 issues — more than anything else in the window, so
	// a leak would put him on top, never be crowded out. callerA must not
	// see him, and must see his own closes (the positive control, read
	// with ?project=pA so other projects of his reach cannot crowd it).
	callerA, callerAUname := seedDashboardUser(t, pool, "user")
	callerB, callerBUname := seedDashboardUser(t, pool, "user")
	pA, _ := seedDashboardProject(t, pool, callerA)
	pB, _ := testsupport.SeedProjectInSpace(t, pool, callerB, testsupport.SeedSpace(t, pool, callerB), "racevis")

	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		issueID := fmt.Sprintf("vis-own-%d-%s", i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, pA, callerA, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, pA, callerAUname, callerA, now.Add(-time.Duration(i+1)*time.Hour))
	}
	for i := 0; i < 40; i++ {
		issueID := fmt.Sprintf("vis-leak-%d-%s", i, uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, pB, callerB, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, pB, callerBUname, callerB, now.Add(-time.Duration(i+1)*time.Minute))
	}

	res, body = callRace(t, h, "/api/v1/dashboard/race?metric=closed&window=7d&topN=20", callerA, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authed expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)
	for _, u := range got.Users {
		if u.Username == callerBUname {
			t.Fatalf("visibility leak: callerB appeared in callerA's race view: %+v", u)
		}
	}
	res, body = callRace(t, h, "/api/v1/dashboard/race?metric=closed&window=7d&project="+pA.String(), callerA, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("own project race: %d %s", res.StatusCode, body)
	}
	sawOwn := false
	for _, u := range decodeRace(t, body).Users {
		if u.Username == callerAUname && u.Total == 2 {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Errorf("callerA's own 2 closes are not on his race (control): %s", body)
	}
}

// TestRace_CommentsMetric seeds three comment.created events for caller
// and one for bob inside the target project `proj`, PLUS five more
// caller comments and two more bob comments in a second, unrelated
// project `otherProj` the same caller also owns. metric=comments with
// `&project=<proj>` must report caller total=3 (not 3+5=8) and, with
// topN=1 pushing bob into the "others" bucket, others.Total=1 (not
// 1+2=3).
//
// sk-292 (found in review): the cross-project noise is deliberate, not
// incidental: it is what makes this test PROVE `ProjectFilter` reaches
// the query. The original version of this test scoped by
// `&project=<proj>` purely to dodge Race's global leaderboard picking up
// ambient comment.created rows seeded by unrelated packages sharing the
// same DB (the thread-index corpus in internal/mcp/tools, etc) — a real
// isolation problem, but seeding data ONLY inside `proj` meant the
// assertion (caller total=3) passed identically whether or not the
// filter reached the query: nothing in the fixture ever depended on
// filtering actually happening, so it stayed green through a silent
// `ProjectFilter` regression. Seeding caller's and bob's extra comments
// in a second project the caller can also see closes that gap: without
// `ProjectFilter` reaching every branch of Store.Race, caller's total
// comes back 8 and others' comes back 3 — this test fails, in a single
// `go test ./internal/handler` run, independent of package ordering or
// of what else shares the database.
//
// Mutation-tested: temporarily forcing dashboard.RaceArgs.ProjectFilter
// to nil in DashboardHandler.Race (simulating the filter failing to
// reach the store) turns this test red — caller total=8, others
// total=3 — and reverting the change turns it back green. //
// topN=1 additionally exercises BOTH per-request query paths that carry
// their own copy of the visibility predicate: Store.Race computes
// users[]'s cumulative Points via a query separate from the one that
// produces Total (dailySQL vs totalsSQL), and computes "others" via a
// THIRD, separate query (othersSQL) — each renders its own copy of
// visClause. A filter that reached totalsSQL but not dailySQL/othersSQL
// would still pass a totals-only assertion (Total right, day buckets
// wrong); the final-cumulative-equals-total checks below catch that,
// because leaked foreign-project rows would inflate the day buckets
// above the (correctly filtered) total.
func TestRace_CommentsMetric(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	bob, bobUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)
	otherProj, _ := seedDashboardProject(t, pool, caller)

	now := time.Now().UTC()

	// In-scope: 3 caller comments + 1 bob comment, in `proj`.
	for i := 0; i < 3; i++ {
		cid := uuid.Must(uuid.NewRandom())
		seedRaceCommentEvent(t, pool, cid, proj, callerUname, caller, now.Add(-time.Duration(i+1)*time.Hour))
	}
	{
		cid := uuid.Must(uuid.NewRandom())
		seedRaceCommentEvent(t, pool, cid, proj, bobUname, bob, now.Add(-2*time.Hour))
	}
	// Out-of-scope noise: 5 more caller comments + 2 more bob comments,
	// in `otherProj`. If ProjectFilter fails to reach any query branch,
	// these leak into the `proj`-scoped response.
	for i := 0; i < 5; i++ {
		cid := uuid.Must(uuid.NewRandom())
		seedRaceCommentEvent(t, pool, cid, otherProj, callerUname, caller, now.Add(-time.Duration(i+10)*time.Hour))
	}
	for i := 0; i < 2; i++ {
		cid := uuid.Must(uuid.NewRandom())
		seedRaceCommentEvent(t, pool, cid, otherProj, bobUname, bob, now.Add(-time.Duration(i+20)*time.Hour))
	}

	// topN=1 forces bob out of users[] and into "others", so the
	// assertions below cover both the top-user daily branch (caller) and
	// the others branch (bob) in one request.
	target := fmt.Sprintf("/api/v1/dashboard/race?metric=comments&window=30d&topN=1&project=%s", proj.String())
	res, body := callRace(t, h, target, caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)

	if len(got.Users) != 1 {
		t.Fatalf("expected exactly 1 top user (topN=1), got %d: %+v", len(got.Users), got.Users)
	}
	callerRow := got.Users[0]
	if callerRow.Username != callerUname {
		t.Fatalf("expected caller as sole top user, got %q", callerRow.Username)
	}
	if callerRow.Total != 3 {
		t.Fatalf("metric=comments caller total=%d want 3 (proj only, not proj+otherProj=8)", callerRow.Total)
	}
	callerLastCum := int64(-1)
	if n := len(callerRow.Points); n > 0 {
		callerLastCum = callerRow.Points[n-1].Cumulative
	}
	if callerLastCum != callerRow.Total {
		t.Fatalf("caller final cumulative=%d want %d (daily branch must also honor the project filter)",
			callerLastCum, callerRow.Total)
	}

	if got.Others.UserCount != 1 {
		t.Fatalf("expected exactly 1 'others' user (bob), got %d", got.Others.UserCount)
	}
	if got.Others.Total != 1 {
		t.Fatalf("metric=comments others(bob) total=%d want 1 (proj only, not proj+otherProj=3)", got.Others.Total)
	}
	othersLastCum := int64(-1)
	if n := len(got.Others.Points); n > 0 {
		othersLastCum = got.Others.Points[n-1].Cumulative
	}
	if othersLastCum != got.Others.Total {
		t.Fatalf("others final cumulative=%d want %d (others branch must also honor the project filter)",
			othersLastCum, got.Others.Total)
	}
}

// TestRace_ProjectFilter_CommitsMetric proves `ProjectFilter` reaches
// metric=commits, which is architecturally separate from every other
// race metric: Store.Race sources it from `issue_commits JOIN issues`
// (not `events`) and builds its visibility predicate via
// buildVisibilityForColumn(..., "i.project_id") instead of the shared
// buildVisibility(...) helper the events-sourced metrics use (see
// Store.Race's `commitsMetric` branch, dashboard/store.go). A regression
// that filters events-sourced metrics correctly (covered by
// TestRace_CommentsMetric above) but drops the filter specifically on
// this branch would still leak here.
func TestRace_ProjectFilter_CommitsMetric(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)
	otherProj, _ := seedDashboardProject(t, pool, caller)

	now := time.Now().UTC()

	// In-scope: 3 commits on an issue in `proj`.
	{
		issueID := "pf-commits-in-" + uuid.Must(uuid.NewRandom()).String()[:8]
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		for i := 0; i < 3; i++ {
			seedRaceIssueCommit(t, pool, issueID, caller, now.Add(-time.Duration(i+1)*time.Hour))
		}
	}
	// Out-of-scope noise: 5 more commits on an issue in `otherProj`.
	{
		issueID := "pf-commits-out-" + uuid.Must(uuid.NewRandom()).String()[:8]
		seedDashboardIssue(t, pool, otherProj, caller, issueID, "human")
		for i := 0; i < 5; i++ {
			seedRaceIssueCommit(t, pool, issueID, caller, now.Add(-time.Duration(i+10)*time.Hour))
		}
	}

	target := fmt.Sprintf("/api/v1/dashboard/race?metric=commits&window=30d&project=%s", proj.String())
	res, body := callRace(t, h, target, caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)
	if len(got.Users) != 1 {
		t.Fatalf("expected exactly 1 user, got %d: %+v", len(got.Users), got.Users)
	}
	if got.Users[0].Username != callerUname {
		t.Fatalf("expected caller, got %q", got.Users[0].Username)
	}
	if got.Users[0].Total != 3 {
		t.Fatalf("metric=commits caller total=%d want 3 (proj only, not proj+otherProj=8)", got.Users[0].Total)
	}
	lastCum := int64(-1)
	if n := len(got.Users[0].Points); n > 0 {
		lastCum = got.Users[0].Points[n-1].Cumulative
	}
	if lastCum != got.Users[0].Total {
		t.Fatalf("caller final cumulative=%d want %d (issue_commits JOIN issues daily branch must also honor the project filter)",
			lastCum, got.Users[0].Total)
	}
}

// TestRace_ProjectFilter_Errors covers the two failure branches
// `?project=` added to Race (sk-292): a malformed UUID is a client
// error caught before any DB round-trip (400 invalid_query), and a real
// project the caller cannot see returns 404 not_found — the same
// wire-indistinguishable-from-nonexistent shape every other
// EnsureProjectVisible call site uses, so existence is never leaked to
// a non-member.
func TestRace_ProjectFilter_Errors(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, _ := seedDashboardUser(t, pool, "user")
	owner, _ := seedDashboardUser(t, pool, "user")
	notMember, _ := seedDashboardProject(t, pool, owner) // caller is NOT a member

	t.Run("invalid_uuid", func(t *testing.T) {
		res, body := callRace(t, h, "/api/v1/dashboard/race?metric=closed&window=7d&project=not-a-uuid", caller, "user")
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
		target := fmt.Sprintf("/api/v1/dashboard/race?metric=closed&window=7d&project=%s", notMember.String())
		res, body := callRace(t, h, target, caller, "user")
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

// utcDay truncates t to UTC midnight of its own calendar day — the same
// bucket key the race endpoint groups points by.
func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// commitsFixtureTimes are the four seed timestamps TestRace_CommitsMetric
// plants. Each lands on a distinct calendar day; the per-day cumulative
// assertions depend on that separation holding.
type commitsFixtureTimes struct {
	dayA         time.Time // 5 days ago, 10:00 UTC
	dayB         time.Time // 2 days ago, 14:00 UTC
	lateLink     time.Time // 1 day ago, 08:00 UTC
	outsideClose time.Time // 40 days ago, 00:00 UTC — outside the 30d window
}

// commitsFixture derives TestRace_CommitsMetric's seed timestamps from now.
//
// Every point is anchored to UTC midnight of now's calendar day and only then
// offset — "5 days ago at 10:00" is exactly that, at any run hour. Anchoring
// to now instead (the pre-sk-205 form, `now.Add(-2*24*time.Hour).Add(14*
// time.Hour)`) let the intraday offset carry the point across a day boundary:
// dayB moved to D-1 once now.Hour() >= 10 and lateLink stayed on D-1 while
// now.Hour() < 16, so for runs in [10:00, 16:00) UTC the two collapsed onto
// one calendar day, merged their buckets, and made dayB's cumulative read 6
// instead of 5. Midnight anchoring makes the day of each point a function of
// now's date alone — see TestCommitsFixture_StableAcrossRunHour.
func commitsFixture(now time.Time) commitsFixtureTimes {
	midnight := utcDay(now)
	return commitsFixtureTimes{
		dayA:         midnight.AddDate(0, 0, -5).Add(10 * time.Hour),
		dayB:         midnight.AddDate(0, 0, -2).Add(14 * time.Hour),
		lateLink:     midnight.AddDate(0, 0, -1).Add(8 * time.Hour),
		outsideClose: midnight.AddDate(0, 0, -40),
	}
}

// TestCommitsFixture_StableAcrossRunHour is the sk-205 regression guard.
//
// The fixture's calendar days must be a pure function of now's *date*, never
// of the hour the suite happens to run at. Pure arithmetic — no DB, so it
// runs in every environment and pins the property TestRace_CommitsMetric
// silently depended on.
func TestCommitsFixture_StableAcrossRunHour(t *testing.T) {
	t.Parallel()

	for _, hour := range []int{0, 9, 10, 12, 15, 16, 23} {
		t.Run(fmt.Sprintf("hour=%02d", hour), func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 7, 16, hour, 40, 0, 0, time.UTC)
			today := utcDay(now)
			f := commitsFixture(now)

			cases := []struct {
				name     string
				got      time.Time
				daysAgo  int
				wantHour int
			}{
				{"dayA", f.dayA, 5, 10},
				{"dayB", f.dayB, 2, 14},
				{"lateLink", f.lateLink, 1, 8},
				{"outsideClose", f.outsideClose, 40, 0},
			}

			seen := make(map[time.Time]string, len(cases))
			for _, c := range cases {
				gotDay := utcDay(c.got)
				wantDay := today.AddDate(0, 0, -c.daysAgo)
				if !gotDay.Equal(wantDay) {
					t.Errorf("%s: day = %s, want %s (fixture claims %d days ago)",
						c.name, gotDay.Format("2006-01-02"), wantDay.Format("2006-01-02"), c.daysAgo)
				}
				if c.got.UTC().Hour() != c.wantHour {
					t.Errorf("%s: hour = %d, want %d", c.name, c.got.UTC().Hour(), c.wantHour)
				}
				if c.got.After(now) {
					t.Errorf("%s: %s is in the future relative to now = %s",
						c.name, c.got.Format(time.RFC3339), now.Format(time.RFC3339))
				}
				if other, dup := seen[gotDay]; dup {
					t.Errorf("%s collides with %s on day %s — per-day buckets would merge",
						c.name, other, gotDay.Format("2006-01-02"))
				}
				seen[gotDay] = c.name
			}
		})
	}
}

// TestRace_CommitsMetric — metric=commits counts issue_commits rows
// (one per SHA), attributed to linked_by and bucketed by linked_at.
// Old semantics counted commit-backed closes (one per close regardless
// of SHA count); this test locks the row-count semantics:
//
//	(a) one close with three SHAs → 3;
//	(b) late attach: close outside window, link inside → 1;
//	(c) close without commits → 0;
//	plus per-day bucketing across two link days.
func TestRace_CommitsMetric(t *testing.T) {
	pool := newDashboardTestPool(t)
	defer pool.Close()
	store := dashboard.New(pool)
	issuesStore := issues.NewStore(pool)
	h := NewDashboardHandler(pool, store, issuesStore)

	caller, callerUname := seedDashboardUser(t, pool, "user")
	proj, _ := seedDashboardProject(t, pool, caller)

	// sk-205: fixture points are anchored to UTC midnight, so their
	// calendar days — and therefore the per-day buckets asserted below —
	// do not depend on the hour of day the suite runs at.
	f := commitsFixture(time.Now())
	dayA := f.dayA                 // 5 days ago, 10:00 UTC
	dayB := f.dayB                 // 2 days ago, 14:00 UTC
	lateLink := f.lateLink         // 1 day ago, 08:00 UTC
	outsideClose := f.outsideClose // 40 days ago — outside 30d window

	// (a) One close with three commits on dayA — old logic would yield 1.
	{
		issueID := fmt.Sprintf("cm-multi-%s", uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, proj, callerUname, caller, dayA)
		for i := 0; i < 3; i++ {
			seedRaceIssueCommit(t, pool, issueID, caller, dayA.Add(time.Duration(i)*time.Minute))
		}
	}

	// (b) Late attach: closed 40d ago, commit linked 1d ago — old logic 0.
	{
		issueID := fmt.Sprintf("cm-late-%s", uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, proj, callerUname, caller, outsideClose)
		seedRaceIssueCommit(t, pool, issueID, caller, lateLink)
	}

	// (c) Close without commits — contributes 0.
	{
		issueID := fmt.Sprintf("cm-none-%s", uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, proj, callerUname, caller, dayB)
	}

	// Per-day bucketing: two commits on dayB (distinct from dayA's three).
	{
		issueID := fmt.Sprintf("cm-dayb-%s", uuid.Must(uuid.NewRandom()).String()[:6])
		seedDashboardIssue(t, pool, proj, caller, issueID, "human")
		seedRaceClosedEvent(t, pool, issueID, proj, callerUname, caller, dayB)
		for i := 0; i < 2; i++ {
			seedRaceIssueCommit(t, pool, issueID, caller, dayB.Add(time.Duration(i+10)*time.Minute))
		}
	}

	// Expected: 3 (multi) + 1 (late) + 0 (none) + 2 (dayB) = 6 SHA rows.
	// Old close-count logic would yield 3 (multi + late-close-outside? no +
	// none + dayB) = 3 closes in window with commits... actually:
	// multi=1, late close outside=0, none=0, dayB=1 → old total 2.
	const wantCommits = int64(6)

	res, body := callRace(t, h, "/api/v1/dashboard/race?metric=commits&window=30d&topN=20", caller, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.StatusCode, string(body))
	}
	got := decodeRace(t, body)

	var callerUser *struct {
		UserID   *string `json:"userId"`
		Username string  `json:"username"`
		Total    int64   `json:"total"`
		Points   []struct {
			Day        time.Time `json:"day"`
			Cumulative int64     `json:"cumulative"`
		} `json:"points"`
	}
	for i := range got.Users {
		if got.Users[i].Username == callerUname {
			callerUser = &got.Users[i]
			break
		}
	}
	if callerUser == nil {
		t.Fatalf("caller %q missing from users[]: %+v", callerUname, got.Users)
	}
	if callerUser.Total != wantCommits {
		t.Fatalf("metric=commits caller total=%d want %d (counts issue_commits rows, not closes)",
			callerUser.Total, wantCommits)
	}

	// Per-day bucketing: dayA (+3), dayB (+2), late-link day (+1).
	dayAStr := utcDay(dayA)
	dayBStr := utcDay(dayB)
	lateStr := utcDay(lateLink)

	cumOn := func(day time.Time) (int64, bool) {
		for _, p := range callerUser.Points {
			pd := utcDay(p.Day)
			if pd.Equal(day) {
				return p.Cumulative, true
			}
		}
		return 0, false
	}
	if cum, ok := cumOn(dayAStr); !ok || cum != 3 {
		t.Fatalf("dayA cumulative=%d want 3 (ok=%v)", cum, ok)
	}
	if cum, ok := cumOn(dayBStr); !ok || cum != 5 {
		t.Fatalf("dayB cumulative=%d want 5 (ok=%v)", cum, ok)
	}
	if cum, ok := cumOn(lateStr); !ok || cum != 6 {
		t.Fatalf("late-link day cumulative=%d want 6 (ok=%v)", cum, ok)
	}
}
