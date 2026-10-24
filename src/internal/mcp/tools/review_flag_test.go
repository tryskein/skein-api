package tools

// review_flag_test.go — sk-303.14 T007: the agent half of rows 25 and 31
// and the whole of row 120.
//
// The REST half of those rows and of rows 26–30, 37, 39, 45 lives in
// handler/issues_review_test.go. Here — only what is observable on the
// agent path: skein_update with the flag, and the write-acknowledgement shape.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// reviewMechanicsWireKeys — the review-mechanics keys on the wire. Written
// by hand; a list derived from the struct would agree with any edit to it.
var reviewMechanicsWireKeys = []string{"reviewRequired", "reviewedAt", "reviewedBy"}

// reviewToolFixture — author, non-author administrator, a project and a
// closed delegated issue with the flag on.
type reviewToolFixture struct {
	pool    *pgxpool.Pool
	store   *issues.Store
	author  uuid.UUID
	admin   uuid.UUID
	pid     uuid.UUID
	issueID string
	version int
}

func newReviewToolFixture(t *testing.T, namePrefix string) *reviewToolFixture {
	t.Helper()
	pool := newTestPool(t)
	author, _ := seedUser(t, pool, "user")
	admin, adminName := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, author, namePrefix)
	addProjectMember(t, pool, pid, admin)

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "result", "closed", 2)
	if _, err := pool.Exec(context.Background(), `
UPDATE issues SET assignee = $2, review_required = true WHERE id = $1`,
		issueID, adminName); err != nil {
		t.Fatalf("seed review state: %v", err)
	}

	f := &reviewToolFixture{pool: pool, store: issues.NewStore(pool),
		author: author, admin: admin, pid: pid, issueID: issueID}
	f.version = f.readVersion(t)
	return f
}

func (f *reviewToolFixture) readVersion(t *testing.T) int {
	t.Helper()
	var v int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT version FROM issues WHERE id = $1`, f.issueID).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

func (f *reviewToolFixture) readFlag(t *testing.T) bool {
	t.Helper()
	var b bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT review_required FROM issues WHERE id = $1`, f.issueID).Scan(&b); err != nil {
		t.Fatalf("read flag: %v", err)
	}
	return b
}

func (f *reviewToolFixture) callUpdate(t *testing.T, caller uuid.UUID, role string,
	args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	handler := handleUpdate(f.pool, f.store)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(withCtx(t, caller, role), req)
	if err != nil {
		t.Fatalf("skein_update: %v", err)
	}
	return res
}

// TestReviewFlagTool_OnlyAuthorMayFlip — row 25, agent half.
func TestReviewFlagTool_OnlyAuthorMayFlip(t *testing.T) {
	f := newReviewToolFixture(t, "revtool")

	res := f.callUpdate(t, f.admin, "admin", map[string]any{
		"id": f.issueID, "expectedVersion": f.version, "reviewRequired": false,
	})
	if !res.IsError {
		t.Fatalf("administrator flipped someone else's flag — INV-4 does not let a role "+
			"stand in for authorship: %+v", res)
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeReviewForbidden {
		t.Errorf("refusal code = %v, want %s", got, ErrCodeReviewForbidden)
	}
	if !f.readFlag(t) {
		t.Errorf("the refused call still cleared the flag")
	}

	ok := f.callUpdate(t, f.author, "user", map[string]any{
		"id": f.issueID, "expectedVersion": f.version, "reviewRequired": false,
	})
	if ok.IsError {
		t.Fatalf("author was refused: %+v", ok)
	}
	if f.readFlag(t) {
		t.Errorf("the flag was not cleared for the author")
	}
}

// TestReviewFlagTool_MixedCallRejected — row 31, agent half.
//
// A mixed call is refused with a FORM code and writes NOTHING: neither
// the flag nor the field it was mixed with. Several different "second
// fields" are checked because "refused" is a claim about the whole class
// of mixed calls, and one example would prove only one path.
func TestReviewFlagTool_MixedCallRejected(t *testing.T) {
	f := newReviewToolFixture(t, "revmixtool")

	mixes := map[string]map[string]any{
		"with an ordinary field": {"notes": "a note"},
		"with status":            {"status": "open"},
		"with a label":           {"labelsAdd": []any{"label"}},
		"with components":        {"componentIds": []any{}},
		"with a reason":          {"reason": "for some reason"},
		"with a title":           {"title": "new title"},
		"with assignee":          {"assignee": ""},
		"with priority":          {"priority": 0},
		"with description":       {"description": "text"},
		"with external ref":      {"externalRef": "ext-1"},
	}

	for name, extra := range mixes {
		t.Run(name, func(t *testing.T) {
			beforeFlag := f.readFlag(t)
			beforeVersion := f.readVersion(t)

			args := map[string]any{
				"id": f.issueID, "expectedVersion": beforeVersion, "reviewRequired": !beforeFlag,
			}
			for k, v := range extra {
				args[k] = v
			}

			res := f.callUpdate(t, f.author, "user", args)
			if !res.IsError {
				t.Fatalf("mixed call succeeded: %+v", res)
			}
			body := extractToolJSON(t, res)
			if got := body["error"]; got != ErrCodeValidationError {
				t.Errorf("refusal code = %v, want %s — this is a request FORM constraint, "+
					"not a business rule", got, ErrCodeValidationError)
			}

			if f.readFlag(t) != beforeFlag {
				t.Errorf("the flag was applied despite the refusal")
			}
			if f.readVersion(t) != beforeVersion {
				t.Errorf("version changed despite the refusal — so the second field " +
					"applied: the request must be refused WHOLE")
			}
		})
	}
}

// TestReviewFlagTool_FlipDoesNotMoveVersion — part of row 27 on the agent
// path. The flag is cleared, the version stays the same, and the write
// acknowledgement carries that version — so the expectedVersion cycle
// continues without a re-read.
func TestReviewFlagTool_FlipDoesNotMoveVersion(t *testing.T) {
	f := newReviewToolFixture(t, "revverstool")

	res := f.callUpdate(t, f.author, "user", map[string]any{
		"id": f.issueID, "expectedVersion": f.version, "reviewRequired": false,
	})
	if res.IsError {
		t.Fatalf("flag flip: %+v", res)
	}
	if got := f.readVersion(t); got != f.version {
		t.Errorf("version on the row = %d, was %d — the flag must not move it",
			got, f.version)
	}
	ack := extractToolJSON(t, res)
	if got, ok := ack["version"].(float64); !ok || int(got) != f.version {
		t.Errorf("acknowledgement carries version = %v, want the previous %d",
			ack["version"], f.version)
	}
}

// TestReviewAck_CarriesNoMechanicsKeys — row 120.
//
// # Why this claim is separate, not a consequence of the existing guards
//
// The package already has two guards around the write acknowledgement,
// and NEITHER of them proves this:
//
//   - the surface guard derives the allowed key list by REFLECTION from
//     the ackRow struct. A field added to the struct enters the list
//     automatically and is accepted silently — the guard will agree with
//     any edit to it;
//   - the neighbouring drift guard only requires that every
//     acknowledgement key also exist on the card. A field that landed in
//     BOTH shapes passes it too.
//
// So what is asserted here is not the struct but the wire itself: the
// acknowledgement response body carries none of the mechanics keys. It
// catches the case both guards miss: someone added the field to both
// ackRow and IssueDetail.
func TestReviewAck_CarriesNoMechanicsKeys(t *testing.T) {
	f := newReviewToolFixture(t, "revack")
	// The mark is set directly so that ALL THREE keys are live on the
	// author's card: a test about keys being absent is meaningless if they
	// are never on the card either.
	if _, err := f.pool.Exec(context.Background(), `
UPDATE issues SET reviewed_at = now(), reviewed_by = $2 WHERE id = $1`,
		f.issueID, f.author); err != nil {
		t.Fatalf("seed mark: %v", err)
	}
	depsStore := dependencies.NewStore(f.pool)
	projectsStore := projects.NewStore(f.pool)

	// Every agent-path write acknowledgement that can be obtained on this
	// issue by its AUTHOR. By the author — on purpose: a non-author does
	// not have the fields on the card either, so their absence from the
	// acknowledgement would prove nothing. It is the author's card that
	// carries them, and it is here that you can see the acknowledgement
	// is not the card.
	acks := map[string]*mcpgo.CallToolResult{}

	acks["skein_update (flag flip)"] = f.callUpdate(t, f.author, "user", map[string]any{
		"id": f.issueID, "expectedVersion": f.version, "reviewRequired": false,
	})
	acks["skein_update (ordinary patch)"] = f.callUpdate(t, f.author, "user", map[string]any{
		"id": f.issueID, "expectedVersion": f.readVersion(t), "notes": "a note",
	})

	// Claim and close — on an OPEN issue of the same author: a closed one
	// cannot be claimed. The flag is on, so the author's card carries it,
	// and the key's absence from the acknowledgement is a substantive claim.
	openID := f.issueID + ".1"
	seedIssue(t, f.pool, f.pid, f.author, openID, "in progress", "open", 2)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE issues SET review_required = true WHERE id = $1`, openID); err != nil {
		t.Fatalf("seed flag on open issue: %v", err)
	}

	claimReq := mcpgo.CallToolRequest{}
	claimReq.Params.Arguments = map[string]any{"id": openID}
	claimRes, err := handleClaim(f.pool, f.store)(withCtx(t, f.author, "user"), claimReq)
	if err != nil {
		t.Fatalf("skein_claim: %v", err)
	}
	acks["skein_claim"] = claimRes

	var openVersion int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT version FROM issues WHERE id = $1`, openID).Scan(&openVersion); err != nil {
		t.Fatalf("read version: %v", err)
	}
	closeReq := mcpgo.CallToolRequest{}
	closeReq.Params.Arguments = map[string]any{
		"id": openID, "expectedVersion": openVersion, "closeOutcome": "done", "reason": "done",
	}
	closeRes, err := handleClose(f.pool, f.store)(withCtx(t, f.author, "user"), closeReq)
	if err != nil {
		t.Fatalf("skein_close: %v", err)
	}
	acks["skein_close"] = closeRes

	createReq := mcpgo.CallToolRequest{}
	createReq.Params.Arguments = map[string]any{
		"project": f.pid.String(), "title": "new issue", "type": "task", "kind": "autonomous",
	}
	createRes, err := handleCreate(f.pool, f.store, depsStore, projectsStore)(
		withCtx(t, f.author, "user"), createReq)
	if err != nil {
		t.Fatalf("skein_create: %v", err)
	}
	acks["skein_create"] = createRes

	for surface, res := range acks {
		if res == nil || res.IsError {
			t.Fatalf("%s returned an error — on an error response the absence of keys "+
				"holds trivially and proves nothing: %+v", surface, res)
		}
		raw := firstTextContent(t, res)
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("%s: parse response: %v", surface, err)
		}
		for _, key := range reviewMechanicsWireKeys {
			if v, ok := body[key]; ok {
				t.Errorf("%s: write acknowledgement carries key %q = %#v. The acknowledgement "+
					"is not the card: SC-028 requires that the mechanics not be visible "+
					"through it either, and the existing guards do not catch this (one "+
					"derives the list by reflection from the struct, the other only "+
					"requires the key to exist on the card)", surface, key, v)
			}
		}
	}
}

// TestReviewAck_VerboseDoesNotWidenTheFlagBranch — fix from wave A/3 review.
//
// Row 120 requires literally: an agent-path write acknowledgement does NOT
// carry the mechanics keys. Previously this held only with a caveat: the
// shared verbose projection returned the card as-is, and only the author
// can perform the operation — i.e. exactly the person whose card carries
// those keys. A caveat in a privacy claim is later read as permission, so
// the branch is narrowed: `verbose:true` does not widen it.
//
// Checked on the AUTHOR — a non-author is refused the operation entirely,
// and the absence of keys there would hold trivially. Plus a control half:
// ordinary-patch verbose still returns the card, so it is this branch that
// is narrowed, not the mode as a whole.
func TestReviewAck_VerboseDoesNotWidenTheFlagBranch(t *testing.T) {
	f := newReviewToolFixture(t, "revackverb")
	// The mark is live — so all three keys on the author's card are non-empty.
	if _, err := f.pool.Exec(context.Background(), `
UPDATE issues SET reviewed_at = now(), reviewed_by = $2 WHERE id = $1`,
		f.issueID, f.author); err != nil {
		t.Fatalf("seed mark: %v", err)
	}

	res := f.callUpdate(t, f.author, "user", map[string]any{
		"id": f.issueID, "expectedVersion": f.version,
		"reviewRequired": false, "verbose": true,
	})
	if res.IsError {
		t.Fatalf("verbose call: %+v", res)
	}
	body := extractToolJSON(t, res)
	for _, key := range reviewMechanicsWireKeys {
		if v, ok := body[key]; ok {
			t.Errorf("flag-branch acknowledgement under verbose:true carries %q = %#v. "+
				"Row 120 has no exceptions: a write acknowledgement carries no "+
				"mechanics keys in any response mode (SC-028)", key, v)
		}
	}
	// The acknowledgement stayed an acknowledgement and did not collapse
	// into emptiness: "no keys" on an empty object holds trivially.
	if got, _ := body["id"].(string); got != f.issueID {
		t.Errorf("acknowledgement carries no identifier: %v", body)
	}

	// Control: ordinary-patch verbose is still the card. The branch is
	// narrowed, not the mode; without this half "narrowing" cannot be told
	// apart from verbose being broken entirely.
	plain := f.callUpdate(t, f.author, "user", map[string]any{
		"id": f.issueID, "expectedVersion": f.readVersion(t),
		"notes": "a note", "verbose": true,
	})
	if plain.IsError {
		t.Fatalf("ordinary-patch verbose: %+v", plain)
	}
	plainBody := extractToolJSON(t, plain)
	if _, ok := plainBody["createdBy"]; !ok {
		t.Errorf("ordinary-patch verbose stopped returning the card: %v. The narrowing "+
			"applies ONLY to the flag branch", plainBody)
	}
}

// callUpdateMany calls skein_update_many with a prepared list of rows.
func (f *reviewToolFixture) callUpdateMany(t *testing.T, caller uuid.UUID, role string,
	items []any) map[string]any {
	t.Helper()
	handler := handleUpdateMany(f.pool, f.store)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"items": items}
	res, err := handler(withCtx(t, caller, role), req)
	if err != nil {
		t.Fatalf("skein_update_many: %v", err)
	}
	if res.IsError {
		t.Fatalf("skein_update_many returned an envelope error: %+v", res)
	}
	return extractToolJSON(t, res)
}

// TestReviewFlagTool_BatchRowRejected — a critical finding of the A/3 review.
//
// The batch path goes through its OWN row parser (buildUpdateOneInput), and
// before this fix it did not read the flag key at all: the row returned
// `ok:true` having written nothing, and a mixed one applied halfway because
// the mixing refusal lives on the single-issue path.
//
// BOTH cases are checked as one class: a lone flag and a mixed row. Three
// claims per case — the refusal, its code, and UNCHANGED row state:
// "refused" without the last would only mean the response was painted red.
func TestReviewFlagTool_BatchRowRejected(t *testing.T) {
	// The companion in the mixed row is `notes`: an ordinary field the
	// batch path is known to apply and that cannot be refused on its own.
	// A companion refused for its own reason would make the case falsely
	// green: the row would return a refusal, and the flag would still go
	// unread (checked by mutation — with an invalid companion the case
	// survived the refusal being dropped).
	cases := map[string]map[string]any{
		"lone flag": {"reviewRequired": false},
		"mixed row": {"reviewRequired": false, "notes": "a note"},
	}

	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReviewToolFixture(t, "revbatch")
			before := f.readIssueSnapshot(t)

			item := map[string]any{
				"id":              f.issueID,
				"expectedVersion": float64(f.version),
			}
			for k, v := range extra {
				item[k] = v
			}

			body := f.callUpdateMany(t, f.author, "user", []any{item})

			results, _ := body["results"].([]any)
			if len(results) != 1 {
				t.Fatalf("rows in the response %d, want one: %v", len(results), body)
			}
			row, _ := results[0].(map[string]any)
			if ok, _ := row["ok"].(bool); ok {
				t.Fatalf("a row with the flag was confirmed as applied: %v. "+
					"The flag does not travel the batch path — an acknowledgement without a write "+
					"is a lost write under a green response", row)
			}
			errObj, _ := row["error"].(map[string]any)
			if code, _ := errObj["code"].(string); code != ErrCodeValidationError {
				t.Errorf("refusal code = %q, want %q (a request-form constraint, "+
					"not a domain rule)", code, ErrCodeValidationError)
			}
			// The refusal must name the CAUSE. Without this claim the case
			// would pass on any refusal — including a refusal caused by the
			// companion, with the flag completely unread.
			details, _ := errObj["details"].(map[string]any)
			if got, _ := details["field"].(string); got != "reviewRequired" {
				t.Errorf("refusal does not name the flag as the cause: details.field = %q, "+
					"response %v. A refusal for someone else's reason leaves the flag "+
					"unread — i.e. the defect is still there", got, errObj)
			}

			after := f.readIssueSnapshot(t)
			if after != before {
				t.Errorf("the refused row still wrote something: was %+v, "+
					"became %+v. The refusal must be BEFORE any write — otherwise "+
					"a mixed row applies halfway", before, after)
			}
		})
	}
}

// issueSnapshot — the observable issue state used to judge "nothing was
// written". The flag and an ordinary field together: a mixed row must
// apply neither — "refused, not applied halfway".
type issueSnapshot struct {
	Version int
	Flag    bool
	Notes   string
}

func (f *reviewToolFixture) readIssueSnapshot(t *testing.T) issueSnapshot {
	t.Helper()
	var s issueSnapshot
	if err := f.pool.QueryRow(context.Background(), `
SELECT version, review_required, COALESCE(notes, '') FROM issues WHERE id = $1`,
		f.issueID).Scan(&s.Version, &s.Flag, &s.Notes); err != nil {
		t.Fatalf("read issue snapshot: %v", err)
	}
	return s
}
