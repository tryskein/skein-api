package handler

// issues_review_test.go — sk-303.14 T007, matrix rows 25–31, 37, 39, 45
// (REST half; the MCP half of rows 25, 31 and the whole of row 120 live in
// mcp/tools/review_flag_test.go).
//
// Shared scenario for almost every row: the author created an issue,
// delegated it to someone else, the issue is closed, the flag is on. Then
// the behaviour of two operations is checked — the mark and the flag toggle.

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// reviewFixture is the shared setup: author, non-author administrator,
// project, and a closed delegated issue with the flag on.
type reviewFixture struct {
	pool       *pgxpool.Pool
	h          *IssuesHandler
	author     uuid.UUID
	admin      uuid.UUID
	adminName  string
	projectID  uuid.UUID
	issueID    string
	projPrefix string
}

func newReviewFixture(t *testing.T, namePrefix string) *reviewFixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	author, _ := testsupport.SeedUser(t, pool, "user")
	admin, adminName := testsupport.SeedUser(t, pool, "admin")
	pid, prefix := testsupport.SeedProject(t, pool, author, namePrefix)
	testsupport.AddProjectMember(t, pool, pid, admin, "member")

	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, author, issueID, "result", "closed", 2)
	testsupport.SetAssignee(t, pool, issueID, adminName)
	setReviewFlagDirect(t, pool, issueID, true)

	return &reviewFixture{
		pool: pool, h: newXPIssuesHandler(t, pool),
		author: author, admin: admin, adminName: adminName,
		projectID: pid, issueID: issueID, projPrefix: prefix,
	}
}

func setReviewFlagDirect(t *testing.T, pool *pgxpool.Pool, issueID string, required bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET review_required = $2 WHERE id = $1`, issueID, required); err != nil {
		t.Fatalf("set review flag: %v", err)
	}
}

// reviewRowState is the observable row state almost every assertion below
// judges by. One fetch instead of five scattered ones.
type reviewRowState struct {
	Version    int
	UpdatedAt  time.Time
	Status     string
	Required   bool
	ReviewedAt *time.Time
	ReviewedBy *uuid.UUID
}

func readReviewRow(t *testing.T, pool *pgxpool.Pool, issueID string) reviewRowState {
	t.Helper()
	var s reviewRowState
	err := pool.QueryRow(context.Background(), `
SELECT version, updated_at, status, review_required, reviewed_at, reviewed_by
  FROM issues WHERE id = $1`, issueID).Scan(
		&s.Version, &s.UpdatedAt, &s.Status, &s.Required, &s.ReviewedAt, &s.ReviewedBy)
	if err != nil {
		t.Fatalf("read review row: %v", err)
	}
	return s
}

// callMark hits POST /issues/{id}/review.
func (f *reviewFixture) callMark(t *testing.T, caller uuid.UUID, role string, body any) xpResponse {
	t.Helper()
	req := xpRequest(t, http.MethodPost, "", body, caller, role,
		"projectID", f.projectID.String(), "issueID", f.issueID)
	return xpCallFull(t, f.h.MarkReviewed, req)
}

// callPatch hits PATCH /issues/{id} with an arbitrary body.
func (f *reviewFixture) callPatch(t *testing.T, caller uuid.UUID, role string, version int, body any) xpResponse {
	t.Helper()
	req := xpRequest(t, http.MethodPatch, strconv.Itoa(version), body, caller, role,
		"projectID", f.projectID.String(), "issueID", f.issueID)
	return xpCallFull(t, f.h.Patch, req)
}

// countEvents counts journal rows of one type for an issue.
//
// The private rows of the review workflow do NOT sit on the "issue"
// entity — they sit on the author, with an empty project and the issue
// id in the payload (see issues.WritePrivateReviewEvent). So the counter
// looks them up by payload->>'issueId', not by entity_id: a query by
// entity_id would return zero and report the absence of audit as its
// presence.
func countEvents(t *testing.T, pool *pgxpool.Pool, issueID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type = 'user'
   AND payload->>'issueId' = $1
   AND type = $2`,
		issueID, eventType).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// TestReview_OnlyAuthorMayActREST — row 25.
//
// Only the author changes the mark and the flag; an administrator gets
// THE SAME refusal. Both properties are checked together, because "the
// refusal is the same" is a claim about a pair of answers, not about
// each one separately.
func TestReview_OnlyAuthorMayActREST(t *testing.T) {
	f := newReviewFixture(t, "revact")
	before := readReviewRow(t, f.pool, f.issueID)

	// A non-author with the administrator role.
	markAdmin := f.callMark(t, f.admin, "admin", nil)
	if markAdmin.Status != http.StatusUnprocessableEntity {
		t.Errorf("mark by administrator: %d, want 422 — INV-4 does not let a role "+
			"stand in for authorship: %s", markAdmin.Status, markAdmin.Raw)
	}
	if got := markAdmin.Body["error"]; got != "review_forbidden" {
		t.Errorf("mark refusal code = %v, want review_forbidden", got)
	}

	flagAdmin := f.callPatch(t, f.admin, "admin", before.Version,
		map[string]any{"reviewRequired": false})
	if flagAdmin.Status != http.StatusUnprocessableEntity {
		t.Errorf("flag change by administrator: %d, want 422: %s",
			flagAdmin.Status, flagAdmin.Raw)
	}
	if got := flagAdmin.Body["error"]; got != "review_forbidden" {
		t.Errorf("flag refusal code = %v, want review_forbidden", got)
	}

	// The refusal is THE SAME: the same code and the same status on both
	// operations. Different codes themselves would be an oracle — a
	// non-author would tell from them which part of the mechanic they
	// "almost" have access to.
	if markAdmin.Body["error"] != flagAdmin.Body["error"] {
		t.Errorf("refusals diverged: mark %v, flag %v — the right to the flag and "+
			"the right to the mark are the same under INV-4",
			markAdmin.Body["error"], flagAdmin.Body["error"])
	}

	// No refused operation wrote anything.
	after := readReviewRow(t, f.pool, f.issueID)
	if after != before {
		t.Errorf("refused operations changed the row: was %+v, now %+v", before, after)
	}

	// And the author may do both.
	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("mark refused to the author: %d %s", res.Status, res.Raw)
	}
	v := readReviewRow(t, f.pool, f.issueID).Version
	if res := f.callPatch(t, f.author, "user", v,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("flag change refused to the author: %d %s", res.Status, res.Raw)
	}
}

// TestReview_MarkDoesNotTouchStatus — row 26 (INV-1).
//
// The mark does not change status and gates nothing. "Gates nothing" here
// is observable: a closed issue stays closed, being marked does not
// hinder other operations on it (checked by attaching a label after the
// mark).
func TestReview_MarkDoesNotTouchStatus(t *testing.T) {
	f := newReviewFixture(t, "revstat")
	before := readReviewRow(t, f.pool, f.issueID)

	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("mark: %d %s", res.Status, res.Raw)
	}

	after := readReviewRow(t, f.pool, f.issueID)
	if after.Status != before.Status {
		t.Errorf("status changed: %s → %s", before.Status, after.Status)
	}
	if after.ReviewedAt == nil {
		t.Fatalf("the mark was not written — the other assertions would be empty")
	}

	// Being marked gates nothing: an ordinary operation on the issue
	// keeps working exactly the same.
	label := xpRequest(t, http.MethodPost, "", map[string]any{"label": "after-the-mark"},
		f.author, "user", "projectID", f.projectID.String(), "issueID", f.issueID)
	if res := xpCallFull(t, f.h.AddLabel, label); res.Status != http.StatusOK {
		t.Errorf("operation on a marked issue rejected (%d) — the mark must not "+
			"gate anything: %s", res.Status, res.Raw)
	}
}

// TestReview_NeitherOperationMovesVersionOrUpdatedAt — row 27 (INV-2,
// side channels; SC-028).
//
// This is the very assertion for which both operations live outside the
// general patch path. An assignee with the card open must not see that
// "something happened" without seeing the fields themselves.
func TestReview_NeitherOperationMovesVersionOrUpdatedAt(t *testing.T) {
	f := newReviewFixture(t, "revver")
	before := readReviewRow(t, f.pool, f.issueID)

	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("mark: %d %s", res.Status, res.Raw)
	}
	afterMark := readReviewRow(t, f.pool, f.issueID)
	if afterMark.Version != before.Version {
		t.Errorf("the mark bumped version %d → %d", before.Version, afterMark.Version)
	}
	if !afterMark.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the mark moved updated_at %s → %s", before.UpdatedAt, afterMark.UpdatedAt)
	}
	if afterMark.ReviewedAt == nil {
		t.Fatalf("the mark was not written — the version-stillness check would be empty")
	}

	if res := f.callPatch(t, f.author, "user", before.Version,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("flag change: %d %s", res.Status, res.Raw)
	}
	afterFlag := readReviewRow(t, f.pool, f.issueID)
	if afterFlag.Version != before.Version {
		t.Errorf("the flag change bumped version %d → %d", before.Version, afterFlag.Version)
	}
	if !afterFlag.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the flag change moved updated_at %s → %s",
			before.UpdatedAt, afterFlag.UpdatedAt)
	}
	if afterFlag.Required {
		t.Fatalf("the flag was not cleared — the version-stillness check would be empty")
	}
}

// TestReview_ConcurrentMarksLeaveOneResult — row 28.
//
// Two concurrent marks: one result, no second event, the author and
// timestamp of the first do not change. The loser of the serialisation
// race answers with a version conflict — the same code as any lost race
// in this store — and that is an acceptable outcome; a second event or
// a rewritten mark is not.
func TestReview_ConcurrentMarksLeaveOneResult(t *testing.T) {
	f := newReviewFixture(t, "revrace")

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = f.callMark(t, f.author, "user", nil).Status
		}(i)
	}
	wg.Wait()

	okCount := 0
	for _, st := range statuses {
		if st == http.StatusOK {
			okCount++
		}
	}
	if okCount == 0 {
		t.Fatalf("neither of two concurrent marks succeeded: %v", statuses)
	}

	first := readReviewRow(t, f.pool, f.issueID)
	if first.ReviewedAt == nil || first.ReviewedBy == nil {
		t.Fatalf("after the race there is no mark at all: %+v", first)
	}
	if *first.ReviewedBy != f.author {
		t.Errorf("mark author = %v, want %v", *first.ReviewedBy, f.author)
	}
	if n := countEvents(t, f.pool, f.issueID, issues.EventReviewMarked); n != 1 {
		t.Errorf("review.marked rows = %d, want exactly one: one mark — "+
			"one journal row, however many calls asked for it", n)
	}

	// A repeat after the race is a quiet success, overwrites nothing.
	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("repeat mark: %d %s", res.Status, res.Raw)
	}
	after := readReviewRow(t, f.pool, f.issueID)
	if !after.ReviewedAt.Equal(*first.ReviewedAt) || *after.ReviewedBy != *first.ReviewedBy {
		t.Errorf("the repeat rewrote the mark: was %v/%v, now %v/%v",
			first.ReviewedAt, first.ReviewedBy, after.ReviewedAt, after.ReviewedBy)
	}
	if n := countEvents(t, f.pool, f.issueID, issues.EventReviewMarked); n != 1 {
		t.Errorf("the repeat created a second event: %d rows", n)
	}
}

// TestReview_MarkRefusedOnUnclosedIssue — row 29.
func TestReview_MarkRefusedOnUnclosedIssue(t *testing.T) {
	f := newReviewFixture(t, "revopen")
	testsupport.SetStatus(t, f.pool, f.issueID, "open")

	res := f.callMark(t, f.author, "user", nil)
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("mark on an unclosed issue: %d, want 422: %s", res.Status, res.Raw)
	}
	if got := res.Body["error"]; got != "review_issue_not_closed" {
		t.Errorf("refusal code = %v, want review_issue_not_closed", got)
	}
	if s := readReviewRow(t, f.pool, f.issueID); s.ReviewedAt != nil {
		t.Errorf("the refused mark was written anyway: %v", s.ReviewedAt)
	}
}

// TestReview_MarkRefusedWhenFlagCleared — row 30.
//
// A cleared flag is a declared "I am not interested": the mark is
// meaningless, and a silent no-effect would hide from the client that
// the action did not apply.
func TestReview_MarkRefusedWhenFlagCleared(t *testing.T) {
	f := newReviewFixture(t, "revnoflag")
	setReviewFlagDirect(t, f.pool, f.issueID, false)

	res := f.callMark(t, f.author, "user", nil)
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("mark with the flag cleared: %d, want 422: %s", res.Status, res.Raw)
	}
	if got := res.Body["error"]; got != "review_flag_cleared" {
		t.Errorf("refusal code = %v, want review_flag_cleared", got)
	}
	if s := readReviewRow(t, f.pool, f.issueID); s.ReviewedAt != nil {
		t.Errorf("the refused mark was written anyway: %v", s.ReviewedAt)
	}
}

// TestReview_MixedPatchRejectedREST — row 31, REST half.
//
// A mixed request is rejected with a SHAPE code (400), the flag is not
// applied, the version did not change. Separately it is checked that an
// ordinary field from the same request was not applied either:
// "rejected" means as a whole, not "the flag was ignored, the rest was
// done".
func TestReview_MixedPatchRejectedREST(t *testing.T) {
	f := newReviewFixture(t, "revmix")
	before := readReviewRow(t, f.pool, f.issueID)

	res := f.callPatch(t, f.author, "user", before.Version, map[string]any{
		"reviewRequired": false,
		"notes":          "note from a mixed request",
	})
	if res.Status != http.StatusBadRequest {
		t.Fatalf("mixed request: %d, want 400 — this is a request-shape "+
			"constraint, not a business rule: %s", res.Status, res.Raw)
	}
	if got := res.Body["error"]; got != "review_flag_not_patchable" {
		t.Errorf("refusal code = %v, want review_flag_not_patchable", got)
	}

	after := readReviewRow(t, f.pool, f.issueID)
	if after.Required != before.Required {
		t.Errorf("the flag was applied despite the refusal: %v → %v", before.Required, after.Required)
	}
	if after.Version != before.Version {
		t.Errorf("version changed despite the refusal: %d → %d", before.Version, after.Version)
	}
	var notes string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT notes FROM issues WHERE id = $1`, f.issueID).Scan(&notes); err != nil {
		t.Fatalf("read notes: %v", err)
	}
	if notes != "" {
		t.Errorf("an ordinary field from the mixed request was applied (notes = %q) — "+
			"the request is rejected as a WHOLE, not halfway", notes)
	}
}

// TestReview_NoNotificationsAreBorn — row 37 (INV-2, "notification feed"
// channel).
//
// Neither the mark nor a flag change produces a row in the notification
// feed — not for the assignee, not for the author, not for watchers.
// Checked by store state, not by the absence of a call: a call may be
// absent because the path never reached the write.
func TestReview_NoNotificationsAreBorn(t *testing.T) {
	f := newReviewFixture(t, "revnotif")

	countNotifications := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(context.Background(), `
SELECT count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE (e.entity_type = 'issue' AND e.entity_id = $1)
    OR (e.entity_type = 'user'  AND e.payload->>'issueId' = $1)`, f.issueID).Scan(&n); err != nil {
			t.Fatalf("count notifications: %v", err)
		}
		return n
	}
	before := countNotifications()

	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("mark: %d %s", res.Status, res.Raw)
	}
	v := readReviewRow(t, f.pool, f.issueID).Version
	if res := f.callPatch(t, f.author, "user", v,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("flag change: %d %s", res.Status, res.Raw)
	}

	if got := countNotifications(); got != before {
		t.Errorf("the mechanic produced %d notification-feed rows — INV-2 requires "+
			"this channel to stay silent", got-before)
	}
}

// TestReview_BothOperationsLeaveAudit — row 39 (INV-10).
//
// Every state change leaves an audit row. Silence of the feeds is not
// silence of the journal: the mechanic's privacy means others do not
// learn about it, not that its actions disappear from the system's
// history.
func TestReview_BothOperationsLeaveAudit(t *testing.T) {
	f := newReviewFixture(t, "revaudit")

	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("mark: %d %s", res.Status, res.Raw)
	}
	if n := countEvents(t, f.pool, f.issueID, issues.EventReviewMarked); n != 1 {
		t.Errorf("%s rows = %d, want one", issues.EventReviewMarked, n)
	}

	v := readReviewRow(t, f.pool, f.issueID).Version
	if res := f.callPatch(t, f.author, "user", v,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("flag change: %d %s", res.Status, res.Raw)
	}
	if n := countEvents(t, f.pool, f.issueID, issues.EventReviewFlagChanged); n != 1 {
		t.Errorf("%s rows = %d, want one", issues.EventReviewFlagChanged, n)
	}

	// Actor and contents of the row: a journal without "who and what"
	// does not satisfy invariant INV-10 — it requires a record of the
	// FACT, not a stub row.
	var actorUserID *uuid.UUID
	var field *string
	var entityType, entityID string
	var projectID *uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `
SELECT actor_user_id, field, entity_type, entity_id, project_id FROM events
 WHERE entity_type = 'user' AND payload->>'issueId' = $1 AND type = $2`,
		f.issueID, issues.EventReviewMarked).Scan(
		&actorUserID, &field, &entityType, &entityID, &projectID); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	// The row's shape is what closes the observation channels. Checked
	// here, where the fact of the audit itself is checked: a row that
	// lost its shape stops being private while remaining a row.
	if entityType != "user" || entityID != f.author.String() {
		t.Errorf("row entity = %s/%s, want user/%s — a private row "+
			"sits on the AUTHOR, and that is exactly what takes it out of the issue feed",
			entityType, entityID, f.author)
	}
	if projectID != nil {
		t.Errorf("row project_id = %v, want empty — a non-empty project "+
			"returns the row to the live-feed project subject and to dashboard "+
			"aggregates", projectID)
	}
	if actorUserID == nil || *actorUserID != f.author {
		t.Errorf("mark-row actor = %v, want author %v", actorUserID, f.author)
	}
	if field == nil || *field != "reviewed_at" {
		t.Errorf("mark-row field = %v, want reviewed_at", field)
	}

	// Re-applying THE SAME flag value does not write a second row:
	// state did not change, so there was no state change.
	v = readReviewRow(t, f.pool, f.issueID).Version
	if res := f.callPatch(t, f.author, "user", v,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("repeat flag change: %d %s", res.Status, res.Raw)
	}
	if n := countEvents(t, f.pool, f.issueID, issues.EventReviewFlagChanged); n != 1 {
		t.Errorf("repeating the same value created a second row: %d", n)
	}
}

// TestReview_ClearingFlagIsNotAMark — row 45 (INV-12, SC-006).
//
// Clearing the flag takes the issue out of the set and does NOT create a
// mark. Membership in the set is defined through the flag being on, so
// the observable half of "takes out of the set" here is the cleared
// flag; the other half is the untouched mark. Restoring the flag, the
// author returns the issue together with the previous (empty) mark
// state.
func TestReview_ClearingFlagIsNotAMark(t *testing.T) {
	f := newReviewFixture(t, "revclear")

	v := readReviewRow(t, f.pool, f.issueID).Version
	if res := f.callPatch(t, f.author, "user", v,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("clearing the flag: %d %s", res.Status, res.Raw)
	}
	cleared := readReviewRow(t, f.pool, f.issueID)
	if cleared.Required {
		t.Fatalf("the flag was not cleared")
	}
	if cleared.ReviewedAt != nil || cleared.ReviewedBy != nil {
		t.Errorf("clearing the flag created a mark (%v / %v) — INV-12 says "+
			"the opposite: the issue leaves the set because the flag is cleared, not "+
			"because it was reviewed", cleared.ReviewedAt, cleared.ReviewedBy)
	}
	if n := countEvents(t, f.pool, f.issueID, issues.EventReviewMarked); n != 0 {
		t.Errorf("clearing the flag wrote %d mark events", n)
	}

	// Restoring the flag returns the issue — still without a mark.
	if res := f.callPatch(t, f.author, "user", cleared.Version,
		map[string]any{"reviewRequired": true}); res.Status != http.StatusOK {
		t.Fatalf("restoring the flag: %d %s", res.Status, res.Raw)
	}
	restored := readReviewRow(t, f.pool, f.issueID)
	if !restored.Required {
		t.Errorf("the flag did not come back")
	}
	if restored.ReviewedAt != nil {
		t.Errorf("restoring the flag created a mark: %v", restored.ReviewedAt)
	}
}

// TestReview_AuthorshipRefusalComesFirst — refusal order, a repair from
// the wave A/3 review warning.
//
// review_ops.go records: the authorship check runs FIRST, otherwise a
// non-author asking for a mark on someone else's issue would learn its
// status and flag state from the refusal code — that is, the refusal
// itself would become an observation channel, exactly the one INV-2
// closes. The claim was stated but not checked: a reorder mutation
// survived it, because the case "a non-author on an issue that also has
// other objections" was reproduced by no test of the class.
//
// Here that case is reproduced — all three refusal conditions hold at
// once, and the answer MUST name ONLY authorship. Move any check above
// the authorship check — the test reddens.
func TestReview_AuthorshipRefusalComesFirst(t *testing.T) {
	// Mark: non-author, the issue is NOT closed, the flag is CLEARED.
	// Each of the three conditions on its own yields its own code; a
	// non-author may only be returned the first.
	t.Run("mark", func(t *testing.T) {
		f := newReviewFixture(t, "revordmark")
		testsupport.SetStatus(t, f.pool, f.issueID, "open")
		setReviewFlagDirect(t, f.pool, f.issueID, false)

		res := f.callMark(t, f.admin, "admin", nil)
		if res.Status != http.StatusUnprocessableEntity {
			t.Fatalf("mark by a non-author: %d, want 422: %s", res.Status, res.Raw)
		}
		if got := res.Body["error"]; got != "review_forbidden" {
			t.Errorf("refusal code = %v, want review_forbidden. Any other code "+
				"tells a non-author the state of someone else's issue (status or flag) — "+
				"that is, the refusal itself becomes an observation channel (INV-2)", got)
		}
	})

	// Flag: a non-author with a DELIBERATELY diverging version. A version
	// check placed above authorship would answer version_conflict and
	// with it hand over the live version of someone else's issue.
	t.Run("flag", func(t *testing.T) {
		f := newReviewFixture(t, "revordflag")
		live := readReviewRow(t, f.pool, f.issueID)

		res := f.callPatch(t, f.admin, "admin", live.Version+41,
			map[string]any{"reviewRequired": false})
		if res.Status != http.StatusUnprocessableEntity {
			t.Fatalf("flag change by a non-author: %d, want 422: %s", res.Status, res.Raw)
		}
		if got := res.Body["error"]; got != "review_forbidden" {
			t.Errorf("refusal code = %v, want review_forbidden. version_conflict "+
				"here is not only a different code — it carries currentVersion of someone "+
				"else's issue to a person the issue does not belong to", got)
		}
		if _, leaked := res.Body["currentVersion"]; leaked {
			t.Errorf("a non-author refusal carries currentVersion: %v", res.Body)
		}
	})
}
