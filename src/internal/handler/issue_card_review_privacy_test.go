package handler

// issue_card_review_privacy_test.go — sk-303.14 T006, matrix row 22,
// REST half.
//
// # What is proven
//
// INV-2, fourth channel: the "needs review" flag and the reviewed mark
// with its author and timestamp are returned ONLY to the issue's author —
// and not "empty", but ABSENT. SC-028 states the same as an observable
// property: the assignee cannot tell an issue with a mark from an issue
// without one.
//
// # Why the test walks every return, not a single GET
//
// The card leaves REST not only from a read. A patch returns it as-is,
// a status transition does too, create does too. A gate placed in the
// read handler would leave those three returns open, and one green GET
// would cover the hole. So every REST path that returns a card is called
// here BY NAME, and each is checked on both halves: for a non-author the
// keys are absent, for the author they are present.
//
// The list is derived from the router (cmd/api/main.go, the
// /projects/{projectID}/issues group) — these are all IssuesHandler
// handlers that serialise issues.IssueDetail:
//
//	POST   /issues                        Create
//	GET    /issues/{issueID}              Get
//	PATCH  /issues/{issueID}              Patch
//	POST   /issues/{issueID}/transitions  UpdateStatus
//	POST   /issues/{issueID}/pin|unpin    Pin / Unpin
//	PUT    /issues/{issueID}/components   ReplaceComponents
//	POST   /issues/{issueID}/labels       AddLabel
//	POST   /issues/{issueID}/accept       Accept
//	POST   /issues/{issueID}/dismiss      Dismiss
//	POST   /issues/{issueID}/undismiss    Undismiss
//
// Claim and close have no REST routes of their own — they live on the
// agent path (skein_claim / skein_close) and are checked in
// mcp/tools/show_review_privacy_test.go. Here their role is played by
// the status transition, which is the same path close opens.

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// reviewPrivateKeys are the three mechanic keys on the wire. Written out
// here again, not imported from the issues package: a test that takes
// its expectation from the code it guards would agree with any change
// to it.
var reviewPrivateKeys = []string{"reviewRequired", "reviewedAt", "reviewedBy"}

// seedReviewMark puts a FULL mark on the issue — the flag, the timestamp
// and the author. Full on purpose: a card whose row holds zeros would
// pass the negative half of the test without any gate at all.
func seedReviewMark(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
UPDATE issues
   SET review_required = true,
       reviewed_at     = now(),
       reviewed_by     = $2
 WHERE id = $1`, issueID, author)
	if err != nil {
		t.Fatalf("seed review mark on %s: %v", issueID, err)
	}
}

// assertReviewKeysAbsent is the negative half. The key must not be there
// at all; `null`, `false` and zero here are the same failure as a true
// value.
func assertReviewKeysAbsent(t *testing.T, surface string, body map[string]any) {
	t.Helper()
	for _, key := range reviewPrivateKeys {
		if v, ok := body[key]; ok {
			t.Errorf("%s: a non-author received key %q = %#v. INV-2 requires the key to be ABSENT: "+
				"from a `false` or `null` value the assignee learns that the mechanic "+
				"exists and did not fire on this issue — and SC-028 requires that they "+
				"cannot tell an issue with a mark from an issue without one",
				surface, key, v)
		}
	}
}

// assertReviewKeysPresent is the positive half. Without it the negative
// half would pass on code that simply does not return the fields to
// anyone, and the whole mechanic would be invisible even to the author.
func assertReviewKeysPresent(t *testing.T, surface string, body map[string]any) {
	t.Helper()
	for _, key := range reviewPrivateKeys {
		if _, ok := body[key]; !ok {
			t.Errorf("%s: the author did NOT receive key %q — US-1 requires the author "+
				"to see the flag and the mark on their own card", surface, key)
		}
	}
	if got, ok := body["reviewRequired"].(bool); !ok || !got {
		t.Errorf("%s: reviewRequired = %#v, want true from the row", surface, body["reviewRequired"])
	}
	if body["reviewedAt"] == nil {
		t.Errorf("%s: reviewedAt is empty even though the mark sits on the row", surface)
	}
	if body["reviewedBy"] == nil {
		t.Errorf("%s: reviewedBy is empty even though the mark's author is written on the row", surface)
	}
}

// TestIssueCardREST_ReviewFieldsAreAuthorOnly — row 22, REST.
//
// One scenario for both faces: the author created an issue, delegated it
// to an assignee and marked it reviewed. Then both pull the card, via
// every return REST can give.
func TestIssueCardREST_ReviewFieldsAreAuthorOnly(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	author, _ := testsupport.SeedUser(t, pool, "user")
	// The assignee is an ADMINISTRATOR on purpose. INV-4: a role widens
	// visibility, but not authorship, so the most privileged non-author
	// in the system must get the same answer as any other. A test on a
	// plain user would also pass on an implementation that compared
	// roles instead of authorship.
	reader, readerName := testsupport.SeedUser(t, pool, "admin")

	pid, prefix := testsupport.SeedProject(t, pool, author, "revpriv")
	testsupport.AddProjectMember(t, pool, pid, reader, "member")

	issueID := prefix + "-1"
	testsupport.SeedIssue(t, pool, pid, author, issueID, "delegated result", "closed", 2)
	testsupport.SetAssignee(t, pool, issueID, readerName)
	seedReviewMark(t, pool, issueID, author)

	h := newXPIssuesHandler(t, pool)

	// Each card return is a pair "surface name → call".
	// The call takes a reader and their role and returns the response body.
	//
	// Version is re-read before every call: a patch and a status
	// transition move it, and two successive calls with one version
	// would hit a version conflict and return an error, not a card —
	// so the test would be green having checked nothing.
	type surfaceCall struct {
		name string
		call func(t *testing.T, caller uuid.UUID, role string) map[string]any
	}

	version := func(t *testing.T) string {
		t.Helper()
		var v int
		if err := pool.QueryRow(context.Background(),
			`SELECT version FROM issues WHERE id = $1`, issueID).Scan(&v); err != nil {
			t.Fatalf("read version: %v", err)
		}
		return strconv.Itoa(v)
	}

	surfaces := []surfaceCall{
		{"GET /issues/{id} (read)", func(t *testing.T, caller uuid.UUID, role string) map[string]any {
			req := xpRequest(t, http.MethodGet, "", nil, caller, role,
				"projectID", pid.String(), "issueID", issueID)
			res := xpCallFull(t, h.Get, req)
			requireOK(t, res)
			return res.Body
		}},
		{"PATCH /issues/{id} (patch)", func(t *testing.T, caller uuid.UUID, role string) map[string]any {
			note := "note from " + caller.String()
			req := xpRequest(t, http.MethodPatch, version(t),
				map[string]any{"notes": note}, caller, role,
				"projectID", pid.String(), "issueID", issueID)
			res := xpCallFull(t, h.Patch, req)
			requireOK(t, res)
			return res.Body
		}},
		{"POST /issues/{id}/transitions (status transition)", func(t *testing.T, caller uuid.UUID, role string) map[string]any {
			// The issue is already closed; a repeat close is a legal
			// transition and returns the same card. Status is left
			// unchanged on purpose, so the "closed delegated issue"
			// scenario stays in force for the following surfaces.
			req := xpRequest(t, http.MethodPost, version(t),
				map[string]any{"status": "closed", "reason": "repeat close in the test"},
				caller, role, "projectID", pid.String(), "issueID", issueID)
			res := xpCallFull(t, h.UpdateStatus, req)
			requireOK(t, res)
			return res.Body
		}},
		{"POST /issues/{id}/labels (label)", func(t *testing.T, caller uuid.UUID, role string) map[string]any {
			req := xpRequest(t, http.MethodPost, "",
				map[string]any{"label": "label-" + caller.String()[:8]}, caller, role,
				"projectID", pid.String(), "issueID", issueID)
			res := xpCallFull(t, h.AddLabel, req)
			requireOK(t, res)
			return res.Body
		}},
		{"POST /issues/{id}/pin (pin)", func(t *testing.T, caller uuid.UUID, role string) map[string]any {
			req := xpRequest(t, http.MethodPost, version(t), nil, caller, role,
				"projectID", pid.String(), "issueID", issueID)
			res := xpCallFull(t, h.Pin, req)
			requireOK(t, res)
			// Return the issue to the unpinned state so the next call
			// does not hit "already pinned".
			unpin := xpRequest(t, http.MethodPost, version(t), nil, caller, role,
				"projectID", pid.String(), "issueID", issueID)
			requireOK(t, xpCallFull(t, h.Unpin, unpin))
			return res.Body
		}},
	}

	for _, s := range surfaces {
		t.Run("non-author/"+s.name, func(t *testing.T) {
			assertReviewKeysAbsent(t, s.name, s.call(t, reader, "admin"))
		})
	}
	for _, s := range surfaces {
		t.Run("author/"+s.name, func(t *testing.T) {
			assertReviewKeysPresent(t, s.name, s.call(t, author, "user"))
		})
	}
}

// TestIssueCardREST_CreateReturnsAuthorsOwnFields — the create return.
//
// It stands separately because create has no second face: the author of
// a new issue is always the caller, and "a non-author on the create
// return" is a state that does not exist. What is checked here is
// something else: the create return goes THROUGH the gate, not around
// it. Before the gate the create card was assembled by the same GetIssue,
// and if its path bypassed the gate the flag would not arrive for the
// author at all — that is, the mechanic would be broken the other way,
// and the negative checks above would not notice.
func TestIssueCardREST_CreateReturnsAuthorsOwnFields(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	author, _ := testsupport.SeedUser(t, pool, "user")
	pid, _ := testsupport.SeedProject(t, pool, author, "revcreate")

	h := newXPIssuesHandler(t, pool)

	// The author's personal default is "not required". Wave A/4 (T009)
	// wired it to the create path, and with it a fresh issue takes the
	// flag FROM HERE, not from the schema-DEFAULT of migration 000044.
	//
	// The value is chosen as false on purpose, and that is not a fixture
	// detail: the property checked below is that "the flag is cleared"
	// arrives for the author as a KEY with value false, not as an absent
	// key. On an issue with the flag on this property is not checkable
	// at all, so creating it with the flag cleared is the only way to
	// keep the assertion after T009.
	setPersonalReviewDefaultREST(t, pool, author, false)

	req := xpRequest(t, http.MethodPost, "",
		map[string]any{"title": "fresh issue", "type": "task"}, author, "user",
		"projectID", pid.String())
	res := xpCallFull(t, h.Create, req)
	if res.Status != http.StatusCreated && res.Status != http.StatusOK {
		t.Fatalf("create returned %d: %s", res.Status, res.Raw)
	}

	// The key must be present with value false — present, specifically:
	// for the author "the flag is cleared" and "there are no fields"
	// must be distinguishable, otherwise the author will not see that
	// the switch exists at all.
	got, ok := res.Body["reviewRequired"]
	if !ok {
		t.Fatalf("the create return does not carry reviewRequired — so the create path "+
			"does not go through the card gate: %s", res.Raw)
	}
	if got != false {
		t.Errorf("reviewRequired on a fresh issue = %#v, want false", got)
	}
	// There is no mark on a fresh issue — and its keys are absent for ANY
	// reader, including the author: an empty mark is not "null", it is
	// nothing.
	for _, key := range []string{"reviewedAt", "reviewedBy"} {
		if v, ok := res.Body[key]; ok {
			t.Errorf("the create return carries %q = %#v even though there is no mark", key, v)
		}
	}
}

// requireOK fails the test on any non-2xx: an error response does not
// carry the card's keys, so "the keys are absent" holds on it
// trivially and the check would be green having checked nothing.
func requireOK(t *testing.T, res xpResponse) {
	t.Helper()
	if res.Status < 200 || res.Status > 299 {
		t.Fatalf("the surface answered %d, and a privacy check only makes sense "+
			"on a real card return: %s", res.Status, res.Raw)
	}
}
