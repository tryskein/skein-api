package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// sampleIssueDetail builds an IssueDetail with every prose field
// populated plus a representative spread of scalars/aggregates, so a
// projection test can tell "dropped the prose" from "dropped half the
// struct".
func sampleIssueDetail() *issues.IssueDetail {
	parent := "sk-1"
	assignee := "alice"
	locked := "mcp-session-abc"
	return &issues.IssueDetail{
		ID:                 "sk-1.2",
		ProjectID:          uuid.MustParse("01900000-0000-7000-8000-0000000000a1"),
		ParentID:           &parent,
		Version:            7,
		Title:              "a title",
		Description:        "LONG DESCRIPTION BODY",
		Type:               "feature",
		Priority:           2,
		Status:             "in_progress",
		Source:             "agent_directed",
		Assignee:           &assignee,
		LockedBy:           &locked,
		Design:             "LONG DESIGN BODY",
		AcceptanceCriteria: "LONG ACCEPTANCE BODY",
		Notes:              "LONG NOTES BODY",
		CreatedBy:          uuid.MustParse("3cc3d111-a0cc-4532-946d-c16484b0e802"),
		Metadata:           map[string]any{},
		ComponentIDs:       []uuid.UUID{uuid.MustParse("01900000-0000-7000-8000-0000000000c1")},
		Labels:             []string{"infra"},
		CommentsCount:      3,
		IsBlocked:          true,
	}
}

// TestIssueWithoutDescription_NullsDescriptionKeepsTheRest is the
// sk-244.1 contract, the twin of TestLeanIssue_DropsProseKeepsEverythingElse:
// `description` is PRESENT and null (so "not requested" stays
// distinguishable from "empty"), the three sibling prose fields are
// returned untouched, and bodyOmitted — which belongs to the write-tools
// and means "all four bodies cut" — is NOT set.
func TestIssueWithoutDescription_NullsDescriptionKeepsTheRest(t *testing.T) {
	m, err := issueWithoutDescription(sampleIssueDetail())
	if err != nil {
		t.Fatalf("issueWithoutDescription: %v", err)
	}

	v, present := m["description"]
	if !present {
		t.Error("description key must be PRESENT (rendered null), not absent")
	}
	if v != nil {
		t.Errorf("description = %v, want nil", v)
	}

	// Sibling prose survives — this flag elides the description only.
	for field, want := range map[string]any{
		"design":             "LONG DESIGN BODY",
		"acceptanceCriteria": "LONG ACCEPTANCE BODY",
		"notes":              "LONG NOTES BODY",
	} {
		if m[field] != want {
			t.Errorf("%s = %v, want %v", field, m[field], want)
		}
	}
	if _, present := m["bodyOmitted"]; present {
		t.Errorf("bodyOmitted must not be set by skein_show, got %v", m["bodyOmitted"])
	}
	// version drives the next expectedVersion — it is the whole point of
	// the cheap read and must never be cut.
	if got, _ := m["version"].(float64); int(got) != 7 {
		t.Errorf("version = %v, want 7", m["version"])
	}
}

// TestIssueWithoutDescription_DiffersFromFullByExactlyDescription is the
// drift guard, mirroring TestLeanIssue_DiffersFromFullByExactlyKnownKeys.
// The projection rewrites one key of the marshalled IssueDetail rather
// than rebuilding a parallel struct precisely so new fields flow through
// automatically; this pins that the only difference between full and
// elided is `description` itself.
func TestIssueWithoutDescription_DiffersFromFullByExactlyDescription(t *testing.T) {
	d := sampleIssueDetail()

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}

	elided, err := issueWithoutDescription(d)
	if err != nil {
		t.Fatalf("issueWithoutDescription: %v", err)
	}

	if full["description"] != "LONG DESCRIPTION BODY" {
		t.Fatalf("test fixture is stale: full description = %v", full["description"])
	}
	if elided["description"] != nil {
		t.Fatalf("elided description = %v, want nil", elided["description"])
	}
	delete(full, "description")
	delete(elided, "description")

	if !reflect.DeepEqual(full, elided) {
		t.Errorf("elided projection differs from full by more than `description`.\n full keys: %v\n elided keys: %v",
			sortedKeys(full), sortedKeys(elided))
	}
}

// TestIssueWithoutDescription_EmptyStillNulls: the helper does not
// special-case an empty body. The empty-vs-null distinction is made by
// the CALLER (handleShow only projects when includeDescription=false),
// so a project of an empty description is still null, never "".
func TestIssueWithoutDescription_EmptyStillNulls(t *testing.T) {
	d := sampleIssueDetail()
	d.Description = ""

	m, err := issueWithoutDescription(d)
	if err != nil {
		t.Fatalf("issueWithoutDescription: %v", err)
	}
	if v, present := m["description"]; !present || v != nil {
		t.Errorf("description = %v (present=%v), want present and nil", v, present)
	}
}

// TestIssueWithoutDescription_Nil guards the nil path — handleShow never
// passes nil today (the issue load short-circuits on not-found), but the
// helper is shared-shaped with leanComment and must not panic.
func TestIssueWithoutDescription_Nil(t *testing.T) {
	m, err := issueWithoutDescription(nil)
	if err != nil {
		t.Fatalf("issueWithoutDescription(nil): %v", err)
	}
	if m != nil {
		t.Errorf("issueWithoutDescription(nil) = %v, want nil", m)
	}
}

// sampleCommentDTO mirrors sampleIssueDetail: every field populated so a
// projection test can tell "dropped the body" from "dropped half the
// struct".
func sampleCommentDTO() *issues.CommentDTO {
	author := uuid.MustParse("3cc3d111-a0cc-4532-946d-c16484b0e802")
	edited := time.Date(2026, 7, 31, 15, 42, 52, 0, time.UTC)
	return &issues.CommentDTO{
		ID:           uuid.MustParse("01900000-0000-7000-8000-0000000000c1"),
		IssueID:      "sk-248",
		Author:       "alice",
		AuthorUserID: &author,
		AuthorType:   "agent",
		AuthorKind:   "agent_directed",
		Body:         "LONG COMMENT BODY",
		CreatedAt:    time.Date(2026, 7, 31, 15, 0, 0, 0, time.UTC),
		UpdatedAt:    edited,
		Version:      3,
		EditedAt:     &edited,
		Mentions:     []issues.MentionRef{},
	}
}

// TestLeanComment_DropsBodyKeepsEverythingElse is the sk-248 contract:
// `body` is gone, the marker is set, and everything the agent needs to
// continue — `id` for a follow-up edit/delete, `version` for the next
// expectedVersion — survives untouched.
func TestLeanComment_DropsBodyKeepsEverythingElse(t *testing.T) {
	m, err := leanComment(sampleCommentDTO())
	if err != nil {
		t.Fatalf("leanComment: %v", err)
	}

	if _, present := m["body"]; present {
		t.Errorf("body must be absent from the lean projection, got %v", m["body"])
	}
	if m["bodyOmitted"] != true {
		t.Errorf("bodyOmitted = %v, want true", m["bodyOmitted"])
	}
	if v, _ := m["version"].(float64); int(v) != 3 {
		t.Errorf("version = %v, want 3", m["version"])
	}
	for field, want := range map[string]any{
		"id":         "01900000-0000-7000-8000-0000000000c1",
		"issueId":    "sk-248",
		"author":     "alice",
		"authorType": "agent",
		"authorKind": "agent_directed",
	} {
		if m[field] != want {
			t.Errorf("%s = %v, want %v", field, m[field], want)
		}
	}
	if _, present := m["createdAt"]; !present {
		t.Error("createdAt must survive the cut")
	}
	if _, present := m["mentions"]; !present {
		t.Error("mentions must survive the cut")
	}
}

// TestLeanComment_DiffersFromFullByExactlyBody is the drift guard, twin
// of TestLeanIssue_DiffersFromFullByExactlyKnownKeys: the projection
// deletes one key from the marshalled DTO rather than rebuilding a
// parallel struct, so a field later added to CommentDTO must flow into
// the write-tool response automatically.
func TestLeanComment_DiffersFromFullByExactlyBody(t *testing.T) {
	d := sampleCommentDTO()

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}

	lean, err := leanComment(d)
	if err != nil {
		t.Fatalf("leanComment: %v", err)
	}

	if full["body"] != "LONG COMMENT BODY" {
		t.Fatalf("test fixture is stale: full body = %v", full["body"])
	}
	delete(full, "body")
	delete(lean, "bodyOmitted")

	if !reflect.DeepEqual(full, lean) {
		t.Errorf("lean comment projection differs from full by more than `body`.\n full keys: %v\n lean keys: %v",
			sortedKeys(full), sortedKeys(lean))
	}
}

// TestLeanComment_EmptyBodyStillOmitted: the cut is unconditional, so
// the contract is uniform — a caller can never read "body absent" as
// "body empty", and `bodyOmitted` always means elided.
func TestLeanComment_EmptyBodyStillOmitted(t *testing.T) {
	d := sampleCommentDTO()
	d.Body = ""

	m, err := leanComment(d)
	if err != nil {
		t.Fatalf("leanComment: %v", err)
	}
	if _, present := m["body"]; present {
		t.Errorf("body key must be absent even when empty, got %v", m["body"])
	}
	if m["bodyOmitted"] != true {
		t.Errorf("bodyOmitted = %v, want true", m["bodyOmitted"])
	}
}

// TestLeanComment_Nil guards the nil path — the write-tools never pass
// nil today (a store error short-circuits first), but the helper is
// shared-shaped with issueWithoutDescription and must not panic.
func TestLeanComment_Nil(t *testing.T) {
	m, err := leanComment(nil)
	if err != nil {
		t.Fatalf("leanComment(nil): %v", err)
	}
	if m != nil {
		t.Errorf("leanComment(nil) = %v, want nil", m)
	}
}

// TestCommentWrites_OmitBodyOnTheWireNotInTheRow exercises both comment
// write-tools end-to-end against real rows (sk-248).
//
// The DB assertions are the point of the test, not decoration: they pin
// that the elision is a wire-format decision and nothing more. A
// regression that "saved context" by not persisting the body would pass
// a response-only assertion and fail here.
//
// There is deliberately no verbose counterpart to the claim test above —
// the comment tools have no verbose flag, because a body the caller just
// wrote has no post-write use. skein_comment_get is the read path.
func TestCommentWrites_OmitBodyOnTheWireNotInTheRow(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "lcm")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "comment lean probe", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtx(t, userID, "admin")

	// --- skein_comment ---
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId": issueID,
		"body":    "ORIGINAL COMMENT BODY",
		"kind":    "directed",
	}
	res, err := handleComment(pool, store)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	created, ok := extractToolJSON(t, res)["comment"].(map[string]any)
	if !ok {
		t.Fatalf("comment missing from skein_comment response")
	}
	if _, present := created["body"]; present {
		t.Errorf("skein_comment must not echo body, got %v", created["body"])
	}
	if created["bodyOmitted"] != true {
		t.Errorf("bodyOmitted = %v, want true", created["bodyOmitted"])
	}
	commentID, _ := created["id"].(string)
	if commentID == "" {
		t.Fatalf("skein_comment response carries no id — a follow-up edit would be impossible")
	}
	if v, _ := created["version"].(float64); int(v) != 1 {
		t.Errorf("version = %v, want 1", created["version"])
	}

	assertStoredBody(t, pool, commentID, "ORIGINAL COMMENT BODY")

	// --- skein_comment_update, chaining off the returned version ---
	reqU := mcpgo.CallToolRequest{}
	reqU.Params.Arguments = map[string]any{
		"commentId":       commentID,
		"body":            "EDITED COMMENT BODY",
		"expectedVersion": 1,
		"kind":            "directed",
	}
	resU, err := handleCommentUpdate(pool, store)(ctx, reqU)
	if err != nil {
		t.Fatalf("update handler error: %v", err)
	}
	if resU.IsError {
		t.Fatalf("unexpected isError=true on update: %+v", extractToolJSON(t, resU))
	}
	edited, ok := extractToolJSON(t, resU)["comment"].(map[string]any)
	if !ok {
		t.Fatalf("comment missing from skein_comment_update response")
	}
	if _, present := edited["body"]; present {
		t.Errorf("skein_comment_update must not echo body, got %v", edited["body"])
	}
	if edited["bodyOmitted"] != true {
		t.Errorf("bodyOmitted = %v, want true", edited["bodyOmitted"])
	}
	// The bumped version has to come back, or a second edit needs an
	// extra read just to learn it.
	if v, _ := edited["version"].(float64); int(v) != 2 {
		t.Errorf("version after edit = %v, want 2", edited["version"])
	}

	assertStoredBody(t, pool, commentID, "EDITED COMMENT BODY")

	// The body stays reachable through the read path — omission is not
	// loss.
	reqG := mcpgo.CallToolRequest{}
	reqG.Params.Arguments = map[string]any{"commentId": commentID}
	resG, err := handleCommentGet(pool, store)(ctx, reqG)
	if err != nil {
		t.Fatalf("get handler error: %v", err)
	}
	if resG.IsError {
		t.Fatalf("unexpected isError=true on get: %+v", extractToolJSON(t, resG))
	}
	got, _ := extractToolJSON(t, resG)["comment"].(map[string]any)
	if got["body"] != "EDITED COMMENT BODY" {
		t.Errorf("skein_comment_get body = %v, want the edited body", got["body"])
	}
}

// TestDelegationTools_AcknowledgeByDefault closes the sk-233 gap the
// sk-248 audit surfaced: skein_drop_delegated / skein_resume_delegated
// predate the lean contract and used to return the untouched
// IssueDetail. Their argument list is a bare id, so the prose in that
// response was pure unrequested weight. sk-282 T020 takes the same two
// tools one step further: the default is now the acknowledgement, not
// the card minus prose.
//
// Both tools are covered in both modes — the full drop/resume ×
// default/verbose matrix. They share `writeIssueAck` today, so half the
// matrix would look sufficient; the point is that it stops being
// sufficient the moment someone gives one of the two its own projection
// or response envelope, and a half-matrix test would stay green through
// exactly that regression (found in review).
func TestDelegationTools_AcknowledgeByDefault(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dlg")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "delegation lean probe", "open", 2)

	if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET description = $2, design = $3, acceptance_criteria = $4, notes = $5
 WHERE id = $1`,
		issueID, "DESCRIPTION BODY", "DESIGN BODY", "ACCEPTANCE BODY", "NOTES BODY"); err != nil {
		t.Fatalf("seed prose: %v", err)
	}

	store := issues.NewStore(pool)
	ctx := withCtx(t, userID, "admin")

	// Both tools are idempotent no-ops when already in the target state,
	// and both return the same post-state either way — so the four cases
	// can run in sequence against one issue.
	cases := []struct {
		name    string
		drop    bool
		verbose bool
	}{
		{"drop ack default", true, false},
		{"resume ack default", false, false},
		{"drop verbose", true, true},
		{"resume verbose", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"id": issueID}
			if tc.verbose {
				args["verbose"] = true
			}
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = args

			res, err := handleDelegationAction(pool, store, tc.drop)(ctx, req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
			}
			out := extractToolJSON(t, res)

			if out["id"] != issueID {
				t.Errorf("id = %v, want %v", out["id"], issueID)
			}
			if tc.verbose {
				for field, want := range map[string]any{
					"description":        "DESCRIPTION BODY",
					"design":             "DESIGN BODY",
					"acceptanceCriteria": "ACCEPTANCE BODY",
					"notes":              "NOTES BODY",
				} {
					if out[field] != want {
						t.Errorf("verbose %s = %v, want %v", field, out[field], want)
					}
				}
				if _, present := out["bodyOmitted"]; present {
					t.Errorf("verbose response must not carry bodyOmitted, got %v", out["bodyOmitted"])
				}
				return
			}
			assertAcknowledgementShape(t, out)
			// The delegation pair's own post-state: nothing else in the
			// acknowledgement moves on these two calls, so `dismissed` is
			// what makes the confirmation mean anything (found in review;
			// gate in delegation_ack_test.go).
			if want := tc.drop; out["dismissed"] != want {
				t.Errorf("dismissed = %#v, want %v", out["dismissed"], want)
			}
			for _, sentinel := range []string{
				"DESCRIPTION BODY", "DESIGN BODY", "ACCEPTANCE BODY", "NOTES BODY",
			} {
				if strings.Contains(toolText(t, res), sentinel) {
					t.Errorf("default response leaks %q:\n%s", sentinel, toolText(t, res))
				}
			}
		})
	}
}

func assertStoredBody(t *testing.T, pool *pgxpool.Pool, commentID, want string) {
	t.Helper()
	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT body FROM comments WHERE id = $1`, commentID).Scan(&stored); err != nil {
		t.Fatalf("read stored body: %v", err)
	}
	if stored != want {
		t.Errorf("stored body = %q, want %q", stored, want)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// verboseDetailFixture is the sk-421.1 test-plan row 9 fixture: a reader who
// is a member of P and not of Q; issue A in P is blocked by one issue from P
// and one from Q, issue B in P only by an issue from P.
type verboseDetailFixture struct {
	pool          *pgxpool.Pool
	store         *issues.Store
	reader        uuid.UUID
	readerName    string
	a, b          string
	aPBlocker     string
	aQBlocker     string
	bPBlocker     string
	qBlockerTitle string
}

func newVerboseDetailFixture(t *testing.T) verboseDetailFixture {
	t.Helper()
	pool := newTestPool(t)
	reader, readerName := seedUser(t, pool, "user")
	qOwner, _ := seedUser(t, pool, "user")
	pid, pPrefix := seedProject(t, pool, reader, "vdp")
	qid, qPrefix := seedProject(t, pool, qOwner, "vdq")

	f := verboseDetailFixture{
		pool: pool, store: issues.NewStore(pool), reader: reader, readerName: readerName,
		a: pPrefix + "-1", b: pPrefix + "-2",
		aPBlocker: pPrefix + "-3", bPBlocker: pPrefix + "-4",
		aQBlocker: qPrefix + "-1", qBlockerTitle: "q secret blocker for row 9",
	}
	seedIssue(t, pool, pid, reader, f.a, "issue A", "open", 2)
	seedIssue(t, pool, pid, reader, f.b, "issue B", "open", 2)
	seedIssue(t, pool, pid, reader, f.aPBlocker, "p blocker of A", "open", 2)
	seedIssue(t, pool, pid, reader, f.bPBlocker, "p blocker of B", "in_progress", 2)
	seedIssue(t, pool, qid, qOwner, f.aQBlocker, f.qBlockerTitle, "deferred", 2)
	for _, edge := range [][2]string{{f.a, f.aPBlocker}, {f.a, f.aQBlocker}, {f.b, f.bPBlocker}} {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())`, edge[0], edge[1], reader); err != nil {
			t.Fatalf("seed blocks %s -> %s: %v", edge[0], edge[1], err)
		}
	}
	return f
}

// assertVerboseDetail checks one full detail from a verbose MCP write
// response: blockers per horizon, isBlocked ⇔ non-empty list, and the lock
// holder's name.
func (f verboseDetailFixture) assertVerboseDetail(t *testing.T, detail map[string]any, pBlocker, qBlocker string) {
	t.Helper()
	id := detail["id"]
	list, ok := detail["blockers"].([]any)
	if !ok {
		t.Fatalf("%v: blockers missing: %v", id, detail)
	}
	if detail["isBlocked"] != (len(list) > 0) {
		t.Errorf("%v: isBlocked = %v with %d blockers — flag and list disagree", id, detail["isBlocked"], len(list))
	}
	// sk-423: the Q blocker is outside the reader's horizon — not an entry,
	// only a count; the P blocker is the one entry.
	wantLinks := 0
	if qBlocker != "" {
		wantLinks = 1
	}
	if detail["unavailableBlockingLinks"] != float64(wantLinks) {
		t.Errorf("%v: unavailableBlockingLinks = %v, want %d", id, detail["unavailableBlockingLinks"], wantLinks)
	}
	if len(list) != 1 {
		t.Fatalf("%v: %d blockers, want 1 (the P blocker): %v", id, len(list), list)
	}
	for _, item := range list {
		entry, _ := item.(map[string]any)
		keys := sortedKeys(entry)
		sort.Strings(keys)
		got := strings.Join(keys, ",")
		if entry["id"] != pBlocker || got != "id,status,title" {
			t.Errorf("%v: blocker entry = %v (keys %s), want the P blocker {id, status, title}", id, entry, got)
		}
	}
	if detail["lockedByUsername"] != f.readerName {
		t.Errorf("%v: lockedByUsername = %v, want %q — both detail changes come through one assembly",
			id, detail["lockedByUsername"], f.readerName)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), f.qBlockerTitle) {
		t.Errorf("%v: the Q blocker's title reached the verbose response: %s", id, raw)
	}
}

// TestClaim_VerboseDetailCarriesRedactionAndHolderName is sk-421.1 test-plan
// row 9 (a): the single verbose claim (Store.Claim → GetIssue) returns the
// full detail with the Q blocker as an unavailable link (sk-423) AND the
// lock holder's name — both
// T001 and T002 arrive through the one detail assembly.
func TestClaim_VerboseDetailCarriesRedactionAndHolderName(t *testing.T) {
	f := newVerboseDetailFixture(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": f.a, "verbose": true}
	res, err := handleClaim(f.pool, f.store)(withCtxSession(t, f.reader, "user", "sess-row9a"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("verbose claim failed: %+v", extractToolJSON(t, res))
	}
	f.assertVerboseDetail(t, extractToolJSON(t, res), f.aPBlocker, f.aQBlocker)
}

// TestClaim_AcknowledgesByDefault_VerboseOptsOut exercises the full tool
// path end-to-end against a real row: the default response is the
// acknowledgement, verbose=true is the full card with all the prose, and
// both agree on version. (sk-233 made it lean; sk-282 T023 made it an
// acknowledgement.)
func TestClaim_AcknowledgesByDefault_VerboseOptsOut(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "lean")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "lean probe", "open", 2)

	// seedIssue writes empty prose; put real bodies in so "absent"
	// can't be confused with "empty".
	if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET description = $2, design = $3, acceptance_criteria = $4, notes = $5
 WHERE id = $1`,
		issueID, "DESCRIPTION BODY", "DESIGN BODY", "ACCEPTANCE BODY", "NOTES BODY"); err != nil {
		t.Fatalf("seed prose: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-lean")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	lean := extractToolJSON(t, res)

	assertAcknowledgementShape(t, lean)
	// The post-action summary and the expectedVersion cycle both need
	// these — they must survive the cut.
	if v, _ := lean["version"].(float64); int(v) != 2 {
		t.Errorf("version = %v, want 2", lean["version"])
	}
	if lean["status"] != "in_progress" {
		t.Errorf("status = %v, want in_progress", lean["status"])
	}
	if lean["lockedBy"] != "sess-lean" {
		t.Errorf("lockedBy = %v, want sess-lean", lean["lockedBy"])
	}

	// verbose=true on an already-claimed issue: claim is idempotent for
	// the same session, so this returns the same post-state with prose.
	reqV := mcpgo.CallToolRequest{}
	reqV.Params.Arguments = map[string]any{"id": issueID, "verbose": true}
	resV, err := handler(ctx, reqV)
	if err != nil {
		t.Fatalf("verbose handler error: %v", err)
	}
	if resV.IsError {
		t.Fatalf("unexpected isError=true on verbose: %+v", extractToolJSON(t, resV))
	}
	full := extractToolJSON(t, resV)

	if full["description"] != "DESCRIPTION BODY" {
		t.Errorf("verbose description = %v, want DESCRIPTION BODY", full["description"])
	}
	if full["design"] != "DESIGN BODY" {
		t.Errorf("verbose design = %v, want DESIGN BODY", full["design"])
	}
	if full["acceptanceCriteria"] != "ACCEPTANCE BODY" {
		t.Errorf("verbose acceptanceCriteria = %v, want ACCEPTANCE BODY", full["acceptanceCriteria"])
	}
	if full["notes"] != "NOTES BODY" {
		t.Errorf("verbose notes = %v, want NOTES BODY", full["notes"])
	}
	if _, present := full["bodyOmitted"]; present {
		t.Errorf("verbose response must not carry bodyOmitted, got %v", full["bodyOmitted"])
	}
}
