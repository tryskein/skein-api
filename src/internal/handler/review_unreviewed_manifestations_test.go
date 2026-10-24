package handler

// review_unreviewed_manifestations_test.go — sk-303.14, task T014.
// Matrix rows 49, 52, 53, 54, 57, 58, 59.
//
// # What is proven here
//
// INV-3: "the unreviewed set is defined once; the second number, the
// filter contents and the reminder recipients are derived from it and
// must agree". The third manifestation — reminder-selection recipients —
// does not yet exist in this group, and matching against it belongs to
// T020. Here the TWO that exist are matched: the second number and the
// filter contents, and the filter is on BOTH paths of the delegated
// surface.
//
// # Why one fixture for every assertion
//
// A divergence of manifestations is not "one of them was wrong on a
// concrete scenario", it is "two readers answered differently on ONE
// data state". So the fixture is built once and covers all nine ways to
// enter or not enter the set, and each assertion asks its reader the
// same state. Splitting it into nine independent tests would mean
// checking each reader nine times separately and never — their
// agreement.
//
// # Mutations this file is written for
//
//   - remove the lift of the "no closed" default in ListByUser → the
//     cross-project filter reddens (empty list) and so does the match
//     with the number; the project one stays green — exactly the split
//     row 54 requires;
//   - replace the fragment in the counter with "created by me and
//     closed" → assertions about a cleared flag, a mark, a dismissal
//     and an archived project redden;
//   - inherit in the filter the breadth of the delegated lens (the
//     previous-assignee chain) → row 52 reddens;
//   - return the second number as zero instead of null with the
//     mechanic off → row 59 reddens.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/nudges"
	"skein-api/internal/projects"
	"skein-api/internal/review"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// unreviewedEnv is one data state and three readers over it.
type unreviewedEnv struct {
	pool *pgxpool.Pool

	meH        *MeHandler
	crossH     *IssuesMeHandler
	listH      *IssuesHandler
	caller     uuid.UUID
	callerName string
	project    uuid.UUID
	prefix     string

	// wantSet are the ids that must make up the caller's unreviewed set
	// in this data state.
	wantSet []string
}

// seedReviewIssue inserts an issues row with ALL mechanic fields set
// explicitly.
//
// A direct write, not a product path: the fixture needs states the
// product does not create in one call (an issue in an archived project,
// an author's dismissal, a previous-assignee chain), and mixing two
// ways inside one fixture is worse than picking one. The semantics of
// the mechanic operations are checked by their own classes (T009, T010);
// here the READ is checked.
func seedReviewIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID,
	idStr, status, assignee string, reviewRequired bool, reviewedBy *uuid.UUID,
	previousAssignees, dismissedBy []uuid.UUID,
) {
	t.Helper()
	var assigneeArg any
	if assignee != "" {
		assigneeArg = assignee
	}
	var closedAtArg any
	if status == "closed" {
		closedAtArg = time.Now().UTC()
	}
	var reviewedAtArg, reviewedByArg any
	if reviewedBy != nil {
		reviewedAtArg = time.Now().UTC()
		reviewedByArg = *reviewedBy
	}
	if previousAssignees == nil {
		previousAssignees = []uuid.UUID{}
	}
	if dismissedBy == nil {
		dismissedBy = []uuid.UUID{}
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata,
                    previous_assignees, dismissed_by,
                    review_required, reviewed_at, reviewed_by)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, $3, false, 'human', NULL, $4::varchar,
        '', '', '', NULL, NULL, NULL, NULL, $5::timestamptz, NULL,
        NULL, $6, now(), now(), NULL, NULL, '{}'::jsonb,
        $7::uuid[], $8::uuid[],
        $9::boolean, $10::timestamptz, $11::uuid)`,
		idStr, projectID, status, assigneeArg, closedAtArg, creator,
		previousAssignees, dismissedBy,
		reviewRequired, reviewedAtArg, reviewedByArg); err != nil {
		t.Fatalf("seed review issue %s: %v", idStr, err)
	}
}

// newUnreviewedEnv builds one data state covering EVERY point of the
// set contract — both membership and every way not to enter.
func newUnreviewedEnv(t *testing.T) unreviewedEnv {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	pool := newIMTestPool(t)

	caller, callerName := seedIMUser(t, pool, "user")
	worker, workerName := seedIMUser(t, pool, "user")
	other, _ := seedIMUser(t, pool, "user")

	pid, prefix := seedIMProject(t, pool, caller, worker, other)
	archivedPID, archivedPrefix := seedIMProject(t, pool, caller, worker)
	if _, err := pool.Exec(t.Context(),
		`UPDATE projects SET archived_at = now() WHERE id = $1`, archivedPID); err != nil {
		t.Fatalf("archiving the project: %v", err)
	}

	// ── IN THE SET ─────────────────────────────────────────────────────────
	// A — the reference: closed, created by the caller, delegated, flag on,
	//     no mark.
	seedReviewIssue(t, pool, pid, caller, prefix+"-1", "closed", workerName, true, nil, nil, nil)
	// I — "archived" by a bulk action: the system has no separate archived
	//     state of an ISSUE, the bulk action turns it into an ordinary
	//     close. So it does NOT leave the set (row 57).
	seedReviewIssue(t, pool, pid, caller, prefix+"-9", "closed", workerName, true, nil, nil, nil)
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET closed_reason = 'archived' WHERE id = $1`, prefix+"-9"); err != nil {
		t.Fatalf("bulk-archiving the issue: %v", err)
	}

	// ── OUTSIDE THE SET, each row is its own contract point ─────────────────
	// B — the flag is cleared (INV-5, SC-005).
	seedReviewIssue(t, pool, pid, caller, prefix+"-2", "closed", workerName, false, nil, nil, nil)
	// C — the reviewed mark is already there.
	seedReviewIssue(t, pool, pid, caller, prefix+"-3", "closed", workerName, true, &caller, nil, nil)
	// D — the assignee is the caller themselves: there was no delegation.
	seedReviewIssue(t, pool, pid, caller, prefix+"-4", "closed", callerName, true, nil, nil, nil)
	// E — not closed: there is no result yet.
	seedReviewIssue(t, pool, pid, caller, prefix+"-5", "open", workerName, true, nil, nil, nil)
	// F — the caller is only a PREVIOUS assignee, the author is someone else
	//     (INV-13, SC-018). The existing delegated lens shows this issue;
	//     the unreviewed filter does not inherit its breadth.
	seedReviewIssue(t, pool, pid, other, prefix+"-6", "closed", workerName, true, nil,
		[]uuid.UUID{caller}, nil)
	// G — the caller explicitly dismissed the issue (SC-026).
	seedReviewIssue(t, pool, pid, caller, prefix+"-7", "closed", workerName, true, nil, nil,
		[]uuid.UUID{caller})
	// H — the project is archived (SC-025).
	seedReviewIssue(t, pool, archivedPID, caller, archivedPrefix+"-8", "closed", workerName, true, nil, nil, nil)

	return unreviewedEnv{
		pool:       pool,
		meH:        NewMeHandler(pool, nil, nil),
		crossH:     NewIssuesMeHandler(pool, issues.NewStore(pool)),
		listH:      NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool)),
		caller:     caller,
		callerName: callerName,
		project:    pid,
		prefix:     prefix,
		wantSet:    []string{prefix + "-1", prefix + "-9"},
	}
}

// ---------------------------------------------------------------------------
// Readers
// ---------------------------------------------------------------------------

// crossPathIDs is the cross-project path of the delegated surface.
//
// The request is built here, not through the shared callMe: that one does
// not put the caller's name in the context, and the WIDENED delegated
// lens (the previous-assignee chain, sk-34) without a name degenerates
// into strict authorship. Without the name the row-52 precondition would
// become unreachable, and the narrowing self-fulfilling: the filter "did
// not return someone else's issue" because the wide list did not return
// it either. Both transports set the name, so this also more accurately
// reproduces the product.
func (e unreviewedEnv) crossPathIDs(t *testing.T, query string) []string {
	t.Helper()
	url := "/api/v1/me/created-by"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := auth.WithUserID(req.Context(), e.caller)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithUsername(ctx, e.callerName)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	e.crossH.CreatedBy(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("cross-project path (%s): status %d, body %v", query, res.StatusCode, body)
	}
	return sortedIDs(t, body)
}

// projectPathIDs is the project path of the delegated surface.
func (e unreviewedEnv) projectPathIDs(t *testing.T, query string) []string {
	t.Helper()
	url := "/api/v1/projects/" + e.project.String() + "/issues?" + query
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", e.project.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, e.caller)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithUsername(ctx, e.callerName)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	e.listH.List(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("project path (%s): status %d, body %v", query, res.StatusCode, body)
	}
	return sortedIDs(t, body)
}

// secondNumber is the second number and the mechanic-enabled flag.
// The flag is returned separately, the number as a pointer: null and 0
// must stay distinguishable on the whole path from SQL to the wire.
func (e unreviewedEnv) secondNumber(t *testing.T) (mechanicEnabled bool, count *int) {
	t.Helper()
	res, body := callCounts(t, e.meH, e.caller)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("counters: status %d, body %v", res.StatusCode, body)
	}
	rawFlag, ok := body["reviewMechanicEnabled"]
	if !ok {
		t.Fatalf("the response has no reviewMechanicEnabled key — the only distinguisher "+
			"of \"there is no number\" from \"zero\" (SC-016): %v", body)
	}
	flag, ok := rawFlag.(bool)
	if !ok {
		t.Fatalf("reviewMechanicEnabled is not boolean: %v (%T)", rawFlag, rawFlag)
	}
	rawCount, ok := body["delegatedUnreviewedCount"]
	if !ok {
		t.Fatalf("the delegatedUnreviewedCount key is absent; the contract requires it "+
			"always present (null with the mechanic off): %v", body)
	}
	if rawCount == nil {
		return flag, nil
	}
	f, ok := rawCount.(float64)
	if !ok {
		t.Fatalf("delegatedUnreviewedCount is not numeric: %v (%T)", rawCount, rawCount)
	}
	n := int(f)
	return flag, &n
}

// sortedIDs takes ids from items[] and sorts them — order is not
// significant in these assertions, contents are.
func sortedIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["items"]
	if !ok {
		t.Fatalf("the response has no items: %v", body)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("items is not an array: %T", raw)
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("an items element is not an object: %T", it)
		}
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// setMechanic sets the caller's master mechanic switch.
func setMechanic(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO user_notification_prefs (user_id, review_mechanic_enabled, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET review_mechanic_enabled = EXCLUDED.review_mechanic_enabled`,
		userID, enabled); err != nil {
		t.Fatalf("mechanic switch: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_notification_prefs WHERE user_id = $1`, userID)
	})
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// TestUnreviewed_ManifestationsAgree — rows 49, 52, 53, 57, 58.
//
// One data state, three readers, one answer.
func TestUnreviewed_ManifestationsAgree(t *testing.T) {
	e := newUnreviewedEnv(t)

	cross := e.crossPathIDs(t, "unreviewed=true")
	project := e.projectPathIDs(t, "createdBy=me&unreviewed=true")
	mechanicOn, count := e.secondNumber(t)

	// Row 49: the reference issue is visible in the filter and counted.
	// Checked first: if it is missing, every agreement below may be an
	// agreement of zeros.
	if !hasID(cross, e.prefix+"-1") {
		t.Errorf("row 49: a delegated closed issue with the flag on is not visible "+
			"in the cross-project filter; got %v", cross)
	}
	if !hasID(project, e.prefix+"-1") {
		t.Errorf("row 49: the same issue is not visible in the project filter; got %v", project)
	}

	// Row 57: an issue "archived" by a bulk action does NOT leave the
	// set — an issue has no separate archived state.
	if !hasID(cross, e.prefix+"-9") {
		t.Errorf("row 57: an issue \"archived\" by a bulk action left the set. "+
			"Bulk archive is an ordinary close, an issue has no separate archived state; "+
			"the \"take it off my eyes\" scenario is closed by clearing the flag. Got %v", cross)
	}

	// Row 52 (SC-018, INV-13): an issue where the caller is only a previous
	// assignee does not enter the filter. It does enter the WIDE delegated
	// lens — that is checked by the precondition below, otherwise the
	// assertion would be proven by the issue not existing.
	widened := e.crossPathIDs(t, "includeClosed=true")
	if !hasID(widened, e.prefix+"-6") {
		t.Fatalf("row 52 precondition: an issue where the caller is only a previous assignee "+
			"is not visible even in the wide delegated lens — there is nothing to check the "+
			"narrowing on; %v", widened)
	}
	if hasID(cross, e.prefix+"-6") {
		t.Errorf("row 52: the filter inherited the breadth of the delegated lens and returned an issue " +
			"where the caller is only a previous assignee. They may not mark it — the list, " +
			"the number and the availability of the action would diverge (INV-13, SC-018)")
	}

	// Row 53 (C3): the filter contents equal the set on BOTH paths. The
	// project path sees only its project, so it is compared with the set
	// narrowed to it; the cross-project one — with the full set. In this
	// fixture they coincide: the only issue outside the project is in an
	// archived one, and it is not in the set.
	if !equalIDs(cross, e.wantSet) {
		t.Errorf("row 53: cross-project filter = %v, want set %v", cross, e.wantSet)
	}
	if !equalIDs(project, e.wantSet) {
		t.Errorf("row 53: project filter = %v, want set %v", project, e.wantSet)
	}

	// Row 58 (INV-3, SC-001): the second number equals the size of the set
	// and the number of filter rows. The comparison is with BOTH paths
	// separately — agreement with one says nothing about the second.
	if !mechanicOn {
		t.Fatalf("row 58 precondition: the mechanic is off for the caller, the second number is not counted")
	}
	if count == nil {
		t.Fatalf("row 58: the review workflow is on, and there is no second number (null)")
	}
	if *count != len(e.wantSet) {
		t.Errorf("row 58: second number %d, set size %d (%v)", *count, len(e.wantSet), e.wantSet)
	}
	if *count != len(cross) {
		t.Errorf("row 58: second number %d, rows in the cross-project filter %d — "+
			"two manifestations of one set diverged (INV-3)", *count, len(cross))
	}
	if *count != len(project) {
		t.Errorf("row 58: second number %d, rows in the project filter %d — "+
			"two manifestations of one set diverged (INV-3)", *count, len(project))
	}
}

// TestUnreviewed_ClosedDefaultLiftedOnlyWhereItExists — row 54.
//
// The "no closed" default exists only on the cross-project path. The
// filter lifts it — otherwise a set made of closed issues would be cut
// out entirely and the filter would silently return empty. The project
// path has no such default, and there is nothing to lift there; that is
// proven by it returning closed issues even WITHOUT the filter.
func TestUnreviewed_ClosedDefaultLiftedOnlyWhereItExists(t *testing.T) {
	e := newUnreviewedEnv(t)

	// Cross-project path WITHOUT the filter: the default applies, no closed.
	plainCross := e.crossPathIDs(t, "")
	for _, id := range e.wantSet {
		if hasID(plainCross, id) {
			t.Fatalf("precondition: the cross-project path without the filter returned closed issue %s — "+
				"it has no \"no closed\" default, and there is nothing to lift; %v", id, plainCross)
		}
	}

	// With the filter — the default is lifted, the set is visible.
	withFilter := e.crossPathIDs(t, "unreviewed=true")
	if !equalIDs(withFilter, e.wantSet) {
		t.Errorf("row 54: the filter did not lift the \"no closed\" default on the cross-project path: "+
			"got %v, want %v", withFilter, e.wantSet)
	}

	// Project path WITHOUT the filter: there is no default — closed are already here.
	plainProject := e.projectPathIDs(t, "createdBy=me")
	if !hasID(plainProject, e.prefix+"-1") {
		t.Errorf("row 54, second half: the project path without the filter did not return a closed "+
			"issue — so it does have a \"no closed\" default after all, and it must be "+
			"lifted too; got %v", plainProject)
	}

	// An explicitly passed status set still wins over the filter: a person
	// who asked for open unreviewed gets empty — that is their own
	// request, not a system default.
	explicitOpen := e.crossPathIDs(t, "unreviewed=true&status=open")
	if len(explicitOpen) != 0 {
		t.Errorf("explicit status=open together with the filter gave %v; want empty — "+
			"the set is made of closed issues", explicitOpen)
	}
}

// TestUnreviewed_MechanicSwitchIsObservable — row 59 (SC-016, INV-6).
//
// Three states, and all three are distinguishable:
//   - mechanic on, unreviewed results exist → flag true, number > 0;
//   - mechanic on, no unreviewed results    → flag true, number 0;
//   - mechanic off                          → flag false, there is NO number.
//
// The distinguisher is the flag alone. Neither an absent field nor a zero
// is a state: the counters object on the client is filled with defaults, and
// "the field is missing" and "the field is zero" are indistinguishable on it.
func TestUnreviewed_MechanicSwitchIsObservable(t *testing.T) {
	e := newUnreviewedEnv(t)

	// 1. On, unreviewed results exist.
	on, count := e.secondNumber(t)
	if !on || count == nil || *count == 0 {
		t.Fatalf("precondition: mechanic=%v, number=%v; want on and greater than zero", on, count)
	}
	nonZero := *count

	// 2. Off — there is no number at all, and the flag says so.
	setMechanic(t, e.pool, e.caller, false)
	off, offCount := e.secondNumber(t)
	if off {
		t.Errorf("row 59: the mechanic switch did not reach the counters flag (INV-6)")
	}
	if offCount != nil {
		t.Errorf("row 59: with the mechanic off the second number is %d; "+
			"it must not be there at all — absence, distinguishable from zero (SC-016)", *offCount)
	}
	// And the filter is empty on both paths with the mechanic off — INV-6
	// removes the mechanic WHOLE, not just its number.
	if got := e.crossPathIDs(t, "unreviewed=true"); len(got) != 0 {
		t.Errorf("row 59: with the mechanic off the cross-project filter returned %v", got)
	}
	if got := e.projectPathIDs(t, "createdBy=me&unreviewed=true"); len(got) != 0 {
		t.Errorf("row 59: with the mechanic off the project filter returned %v", got)
	}

	// 3. On again, but the set is emptied — number zero, flag true.
	// States 2 and 3 MUST be observably different.
	setMechanic(t, e.pool, e.caller, true)
	if _, err := e.pool.Exec(t.Context(),
		`UPDATE issues SET review_required = false WHERE project_id = $1`, e.project); err != nil {
		t.Fatalf("emptying the set: %v", err)
	}
	backOn, zeroCount := e.secondNumber(t)
	if !backOn {
		t.Fatalf("the mechanic did not come back on")
	}
	if zeroCount == nil {
		t.Fatalf("row 59: with the mechanic on and an empty set the second number is missing; " +
			"want an observable zero — otherwise this state is indistinguishable from the mechanic off")
	}
	if *zeroCount != 0 {
		t.Errorf("row 59: the set is emptied, but the number is %d (was %d)", *zeroCount, nonZero)
	}
}

// callMine — a raw call of one of the three /me/* routes: status code and body.
//
// Separate from crossPathIDs: that one needs success, and here the subject
// of the check is the status code itself, so failing on a non-200 is not
// allowed.
func (e unreviewedEnv) callMine(t *testing.T, route http.HandlerFunc, path, query string) (int, map[string]any) {
	t.Helper()
	url := "/api/v1/me/" + path
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := auth.WithUserID(req.Context(), e.caller)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithUsername(ctx, e.callerName)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	route(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestUnreviewed_FilterLivesOnlyOnTheDelegatedSurface — INV-3, the
// transport boundary of the filter.
//
// # What is proven
//
// The unreviewed filter is accepted on BOTH paths of the delegated
// surface and refused on "assigned to me".
//
// # Why a refusal, not an empty list
//
// The set is defined once (review.UnreviewedPredicate) and requires that
// an assignee is set and is NOT the caller. The /me/assigned-to route
// adds exactly the opposite — "the assignee is the caller". The
// intersection is identically empty, on any data: the parameter would
// not narrow the selection, it would zero it. An accepted parameter would
// pass a call error off as a correct "nothing found" and would introduce
// a third transport consumer of the set with a leftover condition on top
// — exactly the multiplication of definitions INV-3 forbids.
//
// # Mutation this test is written for
//
// Make parsing of `unreviewed` common to all three modes (drop the
// mode == ModeAssignedTo check in runMine) → the route answers 200 with
// an empty list again, and this assertion reddens. The control "the
// route without the parameter answers 200" keeps it from degenerating
// into "it is always 400 here".
func TestUnreviewed_FilterLivesOnlyOnTheDelegatedSurface(t *testing.T) {
	e := newUnreviewedEnv(t)

	// Control: the route itself works, it is specifically the parameter.
	if code, body := e.callMine(t, e.crossH.AssignedTo, "assigned-to", ""); code != http.StatusOK {
		t.Fatalf("precondition: /me/assigned-to without the filter answered %d, body %v", code, body)
	}

	code, body := e.callMine(t, e.crossH.AssignedTo, "assigned-to", "unreviewed=true")
	if code != http.StatusBadRequest {
		t.Errorf("INV-3: /me/assigned-to?unreviewed=true answered %d (body %v); "+
			"want a request-shape refusal — on this route the filter is identically empty, "+
			"and an accepted parameter masks a call error as a correct empty answer",
			code, body)
	}
	if got, _ := body["error"].(string); got != "validation_error" {
		t.Errorf("INV-3: error code %q, want validation_error (body %v)", got, body)
	}

	// And both paths of the delegated surface still accept the filter —
	// otherwise "refusal everywhere" would also be green.
	for _, path := range []struct {
		name  string
		route http.HandlerFunc
		url   string
	}{
		{"assigned-by", e.crossH.AssignedBy, "assigned-by"},
		{"created-by", e.crossH.CreatedBy, "created-by"},
	} {
		if code, body := e.callMine(t, path.route, path.url, "unreviewed=true"); code != http.StatusOK {
			t.Errorf("/me/%s?unreviewed=true answered %d, body %v; "+
				"this is a path of the delegated surface, the filter works here",
				path.name, code, body)
		}
	}
}

func hasID(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	return strings.Join(g, ",") == strings.Join(w, ",")
}

// ---------------------------------------------------------------------------
// Third manifestation of the set — reminder selection (T020)
// ---------------------------------------------------------------------------

// nudgeSetIDs — the THIRD manifestation of the set: issues the background
// reminder worker treats as unreviewed for the caller.
//
// The reader is real: the worker's selection (nudges.Store.SelectDue), not
// a query assembled here. The walk is batched and keyset, so the caller
// may not be in the first batch — the database is shared, and foreign
// addressees legally sit in front of them.
//
// An empty result means "there is no addressee at all": the worker has no
// row "user with an empty set", grouping does not produce one. That is
// exactly what is required — leaving the set must drop the addressee, not
// leave them with an empty list.
//
// # Why the moment is SHIFTED FORWARD, not "now"
//
// The worker's selection is the set AND READINESS: besides membership it
// asks whether the addressee could have become due at all (repair A/8
// moved the lower bound of the first mark into SQL — otherwise an
// addressee whose mark has not arrived would take a budget slot EVERY
// tick and starve the walk's tail). This class's fixture closes issues
// "right now", so by INV-11 a reminder on them is still lawfully not
// due — and at the moment "now" the selection would return empty for a
// reason that has nothing to do with INV-3.
//
// Shifting the moment strips readiness out of the answer and leaves
// membership: the bound is the only thing the moment affects in the
// composition when there is no rate-limit row. The assertions below are
// about the agreement of THREE MANIFESTATIONS OF THE SET, not about the
// schedule; the schedule is checked by the worker's own class.
func (e unreviewedEnv) nudgeSetIDs(t *testing.T) []string {
	t.Helper()
	store := nudges.NewStore(e.pool)
	bound, err := review.EarliestFirstMark(review.CategoryUnreviewed)
	if err != nil {
		t.Fatalf("first-mark bound: %v", err)
	}
	now := time.Now().UTC().Add(bound + time.Minute)
	after := uuid.Nil
	for range 100 {
		batch, err := store.SelectDue(t.Context(), review.CategoryUnreviewed, now, 50, after)
		if err != nil {
			t.Fatalf("reminder selection: %v", err)
		}
		if len(batch) == 0 {
			return []string{}
		}
		for _, c := range batch {
			if c.UserID == e.caller {
				out := append([]string(nil), c.IssueIDs...)
				sort.Strings(out)
				return out
			}
		}
		after = batch[len(batch)-1].UserID
	}
	t.Fatalf("reminder selection: walk did not converge in 100 batches")
	return nil
}

// TestUnreviewed_ThirdManifestationAgrees — row 85 (INV-3, SC-001).
//
// Moved here from class T014 on purpose: the third consumer did not yet
// exist there, and "three manifestations" were checked with two. Now
// there really are three, and all three read one state.
//
// Compared with EACH of the two earlier manifestations separately:
// matching one says nothing about the other — that is exactly how the
// earlier manifestations diverged.
func TestUnreviewed_ThirdManifestationAgrees(t *testing.T) {
	e := newUnreviewedEnv(t)

	cross := e.crossPathIDs(t, "unreviewed=true")
	project := e.projectPathIDs(t, "createdBy=me&unreviewed=true")
	mechanicOn, count := e.secondNumber(t)
	nudge := e.nudgeSetIDs(t)

	if !mechanicOn || count == nil {
		t.Fatalf("precondition of row 85: mechanic off, there is no second number")
	}
	if len(nudge) == 0 {
		t.Fatalf("row 85: the caller has unreviewed results (%v), but "+
			"they did not become a reminder addressee — the third manifestation does not exist", e.wantSet)
	}
	if !equalIDs(nudge, e.wantSet) {
		t.Errorf("row 85: reminder-selection addressees see %v, the set = %v",
			nudge, e.wantSet)
	}
	if !equalIDs(nudge, cross) {
		t.Errorf("row 85: reminder selection = %v, cross-project filter = %v — "+
			"two manifestations of one set diverged (INV-3)", nudge, cross)
	}
	if !equalIDs(nudge, project) {
		t.Errorf("row 85: reminder selection = %v, project filter = %v — "+
			"two manifestations of one set diverged (INV-3)", nudge, project)
	}
	if len(nudge) != *count {
		t.Errorf("row 85: reminder selection has %d issues, the second number is %d — "+
			"the number and the addressees are derived from different sets (INV-3)", len(nudge), *count)
	}
}

// TestUnreviewed_LeavingTheSetShowsInAllThree — rows 48, 50, 51, 55, 56, 111.
//
// Every way of leaving the set is checked ON THE SAME reference
// issue and in ALL THREE manifestations at once, including the
// reminder selection. Then the change is undone, and the issue must
// come back — this simultaneously proves that the data was not
// deleted (row 48) and that restoring the flag brings the issue back
// (row 111).
//
// The "before" check is mandatory in every subcase: without it the
// assertion "after the change the issue is gone" would be proved by
// it not having been there earlier either.
//
// What is NOT here: row 50 also says "does not change status" — that
// is about the mark action itself, and it lives in the operation
// class (T007). Here only the consequence for the three
// manifestations is checked.
func TestUnreviewed_LeavingTheSetShowsInAllThree(t *testing.T) {
	for _, tc := range []struct {
		row      string
		leave    func(t *testing.T, e unreviewedEnv)
		comeBack func(t *testing.T, e unreviewedEnv)
		// wholeSet — leaving takes EVERYTHING out of the set, not one issue.
		wholeSet bool
	}{
		{
			row: "50 (reviewed mark)",
			leave: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE issues SET reviewed_at = now(), reviewed_by = $2
                                  WHERE id = $1`, e.prefix+"-1", e.caller)
			},
			comeBack: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE issues SET reviewed_at = NULL, reviewed_by = NULL
                                  WHERE id = $1`, e.prefix+"-1")
			},
		},
		{
			row: "51 and 111 (flag cleared and restored)",
			leave: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE issues SET review_required = false WHERE id = $1`,
					e.prefix+"-1")
			},
			comeBack: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE issues SET review_required = true WHERE id = $1`,
					e.prefix+"-1")
			},
		},
		{
			row: "56 (author's explicit dismissal)",
			leave: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE issues SET dismissed_by = ARRAY[$2::uuid] WHERE id = $1`,
					e.prefix+"-1", e.caller)
			},
			comeBack: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE issues SET dismissed_by = '{}'::uuid[] WHERE id = $1`,
					e.prefix+"-1")
			},
		},
		{
			row: "55 (project archived)",
			leave: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE projects SET archived_at = now() WHERE id = $1`, e.project)
			},
			comeBack: func(t *testing.T, e unreviewedEnv) {
				exec(t, e.pool, `UPDATE projects SET archived_at = NULL WHERE id = $1`, e.project)
			},
			wholeSet: true,
		},
		{
			row: "48 (mechanic off)",
			leave: func(t *testing.T, e unreviewedEnv) {
				setMechanic(t, e.pool, e.caller, false)
			},
			comeBack: func(t *testing.T, e unreviewedEnv) {
				setMechanic(t, e.pool, e.caller, true)
			},
			wholeSet: true,
		},
	} {
		t.Run(tc.row, func(t *testing.T) {
			e := newUnreviewedEnv(t)

			if !hasID(e.crossPathIDs(t, "unreviewed=true"), e.prefix+"-1") ||
				!hasID(e.nudgeSetIDs(t), e.prefix+"-1") {
				t.Fatalf("precondition of row %s: the reference issue is not in the set — "+
					"there is nothing to check leaving on", tc.row)
			}

			tc.leave(t, e)

			cross := e.crossPathIDs(t, "unreviewed=true")
			project := e.projectPathIDs(t, "createdBy=me&unreviewed=true")
			nudge := e.nudgeSetIDs(t)
			_, count := e.secondNumber(t)

			if hasID(cross, e.prefix+"-1") {
				t.Errorf("row %s: the issue stayed in the cross-project filter: %v", tc.row, cross)
			}
			if hasID(project, e.prefix+"-1") {
				t.Errorf("row %s: the issue stayed in the project filter: %v", tc.row, project)
			}
			if hasID(nudge, e.prefix+"-1") {
				t.Errorf("row %s: the issue stayed in the REMINDER SELECTION: %v — "+
					"the third manifestation diverged from the first two", tc.row, nudge)
			}
			if tc.wholeSet {
				if len(nudge) != 0 {
					t.Errorf("row %s: the set must empty entirely, leftover %v",
						tc.row, nudge)
				}
			}
			if count != nil && *count == len(e.wantSet) {
				t.Errorf("row %s: the second number did not change (%d) — the counter reads a "+
					"different set", tc.row, *count)
			}

			tc.comeBack(t, e)

			if !hasID(e.crossPathIDs(t, "unreviewed=true"), e.prefix+"-1") {
				t.Errorf("row %s: after undoing the change the issue did not return to the filter — "+
					"the data was lost, not hidden", tc.row)
			}
			if !hasID(e.nudgeSetIDs(t), e.prefix+"-1") {
				t.Errorf("row %s: after undoing the change the issue did not return to the reminder "+
					"selection", tc.row)
			}
			// The number is printed as a VALUE, not a pointer: a message with
			// a pointer address tells the failure reader nothing.
			if _, back := e.secondNumber(t); back == nil {
				t.Errorf("row %s: after undoing the change there is no second number (null), "+
					"want %d", tc.row, len(e.wantSet))
			} else if *back != len(e.wantSet) {
				t.Errorf("row %s: after undoing the change the second number is %d, want %d",
					tc.row, *back, len(e.wantSet))
			}
		})
	}
}

// exec — a short form of a direct change of the fixture state.
func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("changing the fixture state: %v", err)
	}
}
