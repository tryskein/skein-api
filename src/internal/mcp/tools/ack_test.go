package tools

// ack_test.go — sk-282 T007, test-plan rows 9, 10 and 11.
//
//	row  9  US-5/INV-7: the ack helper turns any IssueDetail into an
//	            acknowledgement carrying the identifier, the authoritative
//	            post-state and the version — and not one free-text field
//	row 10  INV-4: verbose=true through the same helper returns the
//	            previous full IssueDetail
//	row 11  US-5: the batchResultRow / batchSummary form of the group
//	            close and update has not changed by a single byte — the
//	            FP-1 reference is not "fixed"
//
// Row 89 ("some rows in version conflict") is T020's and is not here.
// The wiring of the ack into the write-tools is group G-A4's, and is not
// here either — this file tests the CONTRACT of the helper, which is what
// the plan's gate for T007 says in as many words.
//
// Pure unit: no pool, no database.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// ackFixtureIssue is an IssueDetail with EVERY free-text field populated
// and every post-state stamp set. Populating the prose is the point: an
// acknowledgement built from an issue whose description is empty proves
// nothing about whether the description would have been echoed.
func ackFixtureIssue() *issues.IssueDetail {
	assignee := "alice"
	lockedBy := "mcp-session-ack"
	lockedAt := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	acceptedAt := time.Date(2026, 8, 20, 0, 30, 0, 0, time.UTC)
	closedReason := "SENTINEL closed reason the caller supplied"
	sourceContext := "SENTINEL source context the caller supplied"
	externalRef := "SENTINEL-123"

	return &issues.IssueDetail{
		ID:                 "sk-282.18.9",
		ProjectID:          uuid.MustParse("01900000-0000-7000-8000-0000000000a1"),
		Version:            7,
		Title:              "SENTINEL title",
		Description:        "SENTINEL description the caller supplied",
		Type:               "task",
		Priority:           1,
		Status:             "closed",
		Source:             "agent_directed",
		SourceContext:      &sourceContext,
		Assignee:           &assignee,
		LockedBy:           &lockedBy,
		LockedAt:           &lockedAt,
		Design:             "SENTINEL design the caller supplied",
		AcceptanceCriteria: "SENTINEL acceptance criteria the caller supplied",
		Notes:              "SENTINEL notes the caller supplied",
		StartedAt:          &startedAt,
		ClosedAt:           &closedAt,
		ClosedReason:       &closedReason,
		ExternalRef:        &externalRef,
		AcceptedAt:         &acceptedAt,
		CreatedAt:          time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC),
		Metadata:           map[string]any{},
		ComponentIDs:       []uuid.UUID{},
		Labels:             []string{},
		CommitRefs:         []issues.CommitRef{},
	}
}

// row 9 — the acknowledgement carries the identifier, the authoritative
// post-state and the version, and no free text.
func TestAck_CarriesPostStateAndNoFreeText(t *testing.T) {
	d := ackFixtureIssue()
	encoded := marshalAck(t, mustAck(t, d))

	var got map[string]any
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}

	t.Run("identifier and version are present", func(t *testing.T) {
		if got["id"] != d.ID {
			t.Errorf("id = %v, want %q", got["id"], d.ID)
		}
		// INV-7: the version is what continues the optimistic cycle
		// without an extra read, so it must be present and correct — not
		// merely non-zero.
		v, ok := got["version"].(float64)
		if !ok || int(v) != d.Version {
			t.Errorf("version = %#v, want %d — without it the caller must re-read the "+
				"issue before its next conditional update", got["version"], d.Version)
		}
	})

	t.Run("authoritative post-state is present", func(t *testing.T) {
		// US-5's required column, item by item: status, version (above),
		// the lock and claim stamps.
		if got["status"] != d.Status {
			t.Errorf("status = %v, want %q — the caller cannot derive the applied state "+
				"from its own request", got["status"], d.Status)
		}
		for _, key := range []string{"lockedBy", "lockedAt", "startedAt", "closedAt"} {
			if _, ok := got[key]; !ok {
				t.Errorf("%s is missing — lock and claim stamps are the proof the "+
					"server applied the write", key)
			}
		}
		// The ACCEPTANCE stamp is deliberately NOT on the common row
		// (sk-282 T021): US-5's required column names the lock and claim
		// stamps and not this one, and on a realistic close its 42
		// characters put the response over SC-006's budget. It is
		// carried by skein_accept alone — see ackFromAcceptedIssue and
		// the sibling assertion below.
		if _, present := got["acceptedAt"]; present {
			t.Errorf("the common acknowledgement carries acceptedAt = %v — it belongs to "+
				"skein_accept only", got["acceptedAt"])
		}
	})

	t.Run("no free-text field survives", func(t *testing.T) {
		for _, key := range ackForbiddenTextFields {
			if v, ok := got[key]; ok {
				t.Errorf("acknowledgement carries %q = %#v — US-5 forbids echoing text the "+
					"caller is already holding; it lands in the agent's context twice and is "+
					"re-sent on every later turn", key, v)
			}
		}
		// Belt and braces: no SENTINEL text may appear anywhere in the
		// payload, whatever key it hides under. This catches a prose
		// field that gets echoed under a NAME the forbidden list does not
		// know — which is precisely the failure a name-based check
		// cannot see.
		if strings.Contains(encoded, "SENTINEL") {
			t.Errorf("acknowledgement contains caller-supplied text:\n%s", encoded)
		}
	})

	t.Run("the acknowledgement is small enough to be worth it", func(t *testing.T) {
		// Not an SC gate (SC-006 is measured on the whole close response
		// by T021), but a floor: a "narrow" acknowledgement that came out
		// the size of a card would mean the allowlist leaked.
		if n := len(encoded); n > 400 {
			t.Errorf("acknowledgement is %d bytes:\n%s", n, encoded)
		}
		t.Logf("acknowledgement is %d bytes: %s", len(encoded), encoded)
	})
}

// row 9, second half — the acknowledgement's shape does not depend on
// which write produced it. A claim has no close stamp and a close has no
// acceptance; neither may emit an explicit null to say so.
func TestAck_OmitsStampsThatDoNotApply(t *testing.T) {
	assignee := "alice"
	lockedBy := "mcp-session-ack"
	lockedAt := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	// sk-421.1 T002 (test-plan row 22): the detail now carries the lock
	// holder's name, and the acknowledgement must NOT grow it — US-15
	// Unchanged, "the brief confirmations of MCP writes are not extended".
	holder := "bob"
	claimed := &issues.IssueDetail{
		ID:               "sk-282.18.9",
		Version:          2,
		Status:           "in_progress",
		Assignee:         &assignee,
		LockedBy:         &lockedBy,
		LockedAt:         &lockedAt,
		LockedByUsername: &holder,
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(marshalAck(t, mustAck(t, claimed))), &got); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	if v, ok := got["lockedByUsername"]; ok {
		t.Errorf("a claim acknowledgement carries lockedByUsername = %v — the acknowledgement "+
			"is not a serialisation of the detail and does not widen (US-15 Unchanged)", v)
	}
	for _, key := range []string{"closedAt", "acceptedAt", "startedAt", "error"} {
		if _, ok := got[key]; ok {
			t.Errorf("a claim acknowledgement carries %q — an explicit null spends bytes on "+
				"every call to say 'not applicable'", key)
		}
	}
	for _, key := range []string{"id", "ok", "version", "status", "lockedBy", "lockedAt"} {
		if _, ok := got[key]; !ok {
			t.Errorf("a claim acknowledgement is missing %q", key)
		}
	}
}

// row 9, third half — a missing post-state is an ERROR, on every one of
// the three entry points.
//
// The wiring tasks call these helpers from six surfaces, and the way each
// of them can go wrong is the same: forgetting the post-write load. If
// that produced a zero row instead of an error, the caller would receive
// `{"id":"","ok":false,"version":0}` as a SUCCESS — no identifier, no
// post-state, no version, i.e. none of the three things INV-7 names, and
// an `ok:false` with no `error` beside it, which is a failure with no
// reason. The mistake has to reach the caller as a tool error instead.
func TestAck_NilIssueIsAnError(t *testing.T) {
	t.Run("ackFromIssue", func(t *testing.T) {
		row, err := ackFromIssue(nil)
		if !errors.Is(err, ErrAckNilIssue) {
			t.Fatalf("ackFromIssue(nil) error = %v, want ErrAckNilIssue — a zero row here is a "+
				"fake acknowledgement that the caller cannot tell from a real one", err)
		}
		if row != (ackRow{}) {
			t.Errorf("ackFromIssue(nil) = %+v, want no row alongside the error", row)
		}
	})

	t.Run("ackPayload, default mode", func(t *testing.T) {
		payload, err := ackPayload(nil, false)
		if !errors.Is(err, ErrAckNilIssue) {
			t.Fatalf("ackPayload(nil, false) error = %v, want ErrAckNilIssue", err)
		}
		if payload != nil {
			t.Errorf("ackPayload(nil, false) payload = %#v, want nothing to serialise", payload)
		}
	})

	t.Run("ackPayload, verbose mode", func(t *testing.T) {
		// Serialising a nil detail as the "full" response yields the JSON
		// literal `null` — the same fake success, merely louder.
		payload, err := ackPayload(nil, true)
		if !errors.Is(err, ErrAckNilIssue) {
			t.Fatalf("ackPayload(nil, true) error = %v, want ErrAckNilIssue", err)
		}
		if payload != nil {
			t.Errorf("ackPayload(nil, true) payload = %#v, want nothing to serialise", payload)
		}
	})

	t.Run("the failure reaches the tool as an error result", func(t *testing.T) {
		// The wiring contract: writeIssueAck must not answer a missing
		// post-state with a 200-shaped acknowledgement.
		res := mustCall(t, func() (*mcpgo.CallToolResult, error) {
			return writeIssueAck(context.Background(), "skein_close", ackTestUser, "sk-1", nil, false)
		})
		if !res.IsError {
			t.Fatalf("writeIssueAck(nil issue) = %#v, want a tool error result", res)
		}
	})
}

// row 9, fourth half — a batch slot with no post-state cannot be counted
// as a success.
func TestAckBatch_NilSlotIsAnErrorAndNotASuccess(t *testing.T) {
	details := []*issues.IssueDetail{
		{ID: "sk-1", Version: 2, Status: "in_progress"},
		nil,
		{ID: "sk-3", Version: 1, Status: "in_progress"},
	}
	got, err := ackBatchFromIssues(details)
	if !errors.Is(err, ErrAckNilIssue) {
		t.Fatalf("ackBatchFromIssues with a nil slot error = %v, want ErrAckNilIssue", err)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("error %q does not say WHICH slot was empty", err.Error())
	}
	if got.Summary.Succeeded != 0 || len(got.Results) != 0 {
		t.Errorf("ackBatchFromIssues returned %+v alongside the error — a slot with no "+
			"post-state must not be counted in Succeeded, and the group tools are "+
			"all-or-nothing so there is no partial response to return", got)
	}
}

// row 9, fifth half — the top-level key slot (D-6).
//
// Three of D-6's four named bypasses put keys BESIDE the issue object
// rather than inside it; the close warning branch is the live one. G-A4
// can only bring that branch onto the common acknowledgement path if the
// helper has somewhere to put them — and only if having that slot costs
// the default response nothing.
func TestAck_TopLevelKeySlot(t *testing.T) {
	d := ackFixtureIssue()

	t.Run("no extra keys — the response is byte-identical to the plain ack", func(t *testing.T) {
		plain := toolText(t, mustCall(t, func() (*mcpgo.CallToolResult, error) {
			return writeIssueAck(context.Background(), "skein_close", ackTestUser, d.ID, d, false)
		}))
		withNone := toolText(t, mustCall(t, func() (*mcpgo.CallToolResult, error) {
			return writeIssueAckWith(context.Background(), "skein_close", ackTestUser, d.ID, d, false, nil)
		}))
		if plain != withNone {
			t.Errorf("the slot changed the default response:\n plain %s\n  slot %s\n\n"+
				"the first stage promises no change on the wire; the mechanism may only add a way to "+
				"put keys there, never a byte when nobody does", plain, withNone)
		}
		if strings.Contains(plain, "SENTINEL") {
			t.Errorf("the default acknowledgement echoed caller text: %s", plain)
		}
	})

	t.Run("extra keys land at the TOP level, beside the ack's own", func(t *testing.T) {
		res := mustCall(t, func() (*mcpgo.CallToolResult, error) {
			return writeIssueAckWith(context.Background(), "skein_close", ackTestUser, d.ID, d, false,
				map[string]any{
					"warning":       "closed with no assignee",
					"openChildList": []string{"sk-282.18.9.1"},
				})
		})
		got := extractToolJSON(t, res)
		// Flat, not nested: close.go's agents read `warning` at the top
		// level today, in both modes.
		if got["warning"] != "closed with no assignee" {
			t.Errorf("warning = %#v, want it at the top level — nesting it is a wire change",
				got["warning"])
		}
		if _, ok := got["openChildList"]; !ok {
			t.Error("openChildList did not survive")
		}
		// And the acknowledgement itself is still there, unwrapped.
		if got["id"] != d.ID || got["version"] == nil || got["status"] != d.Status {
			t.Errorf("the acknowledgement is not intact beside the extra keys: %#v", got)
		}
	})

	t.Run("verbose mode keeps the same flat shape", func(t *testing.T) {
		got := extractToolJSON(t, mustCall(t, func() (*mcpgo.CallToolResult, error) {
			return writeIssueAckWith(context.Background(), "skein_close", ackTestUser, d.ID, d, true,
				map[string]any{"warning": "closed with no assignee"})
		}))
		if got["warning"] != "closed with no assignee" {
			t.Errorf("warning = %#v in verbose mode — the two modes must agree on where the "+
				"key lives, which is what close.go's two branches already promise",
				got["warning"])
		}
		if got["description"] != d.Description {
			t.Error("verbose mode stopped being the full response")
		}
	})

	t.Run("a key that collides with the ack's own is refused", func(t *testing.T) {
		res := mustCall(t, func() (*mcpgo.CallToolResult, error) {
			return writeIssueAckWith(context.Background(), "skein_close", ackTestUser, d.ID, d, false,
				map[string]any{"version": 999})
		})
		if res == nil || !res.IsError {
			t.Fatalf("overwriting %q was accepted (%s) — an acknowledgement whose post-state "+
				"can be overwritten from the outside is not authoritative", "version",
				toolText(t, res))
		}
	})
}

const ackTestUser = "00000000-0000-0000-0000-000000000000"

// mustCall runs a helper that returns a tool result, failing on the
// transport error a tool handler is never supposed to produce.
func mustCall(t *testing.T, f func() (*mcpgo.CallToolResult, error)) *mcpgo.CallToolResult {
	t.Helper()
	res, err := f()
	if err != nil {
		t.Fatalf("tool helper returned a transport error: %v", err)
	}
	if res == nil {
		t.Fatal("tool helper returned no result")
	}
	return res
}

// toolText returns the raw JSON body of a tool result.
func toolText(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want TextContent", res.Content[0])
	}
	return tc.Text
}

// row 10 — INV-4: verbose=true returns the previous full IssueDetail.
func TestAck_VerboseReturnsTheFullIssueDetail(t *testing.T) {
	d := ackFixtureIssue()
	// sk-421.1 T002 (test-plan row 22): verbose is the detail, so it carries
	// the lock holder's name as part of it.
	holder := "verbose-holder-name"
	d.LockedByUsername = &holder

	payload, err := ackPayload(d, true)
	if err != nil {
		t.Fatalf("ackPayload(verbose): %v", err)
	}

	// Identity, not equivalence: INV-4 requires a named path to the FULL
	// data, and a "full" mode that returned a re-projection would be a
	// second narrowing wearing the escape hatch's name.
	same, ok := payload.(*issues.IssueDetail)
	if !ok {
		t.Fatalf("verbose payload is %T, want *issues.IssueDetail — the escape hatch must "+
			"return the previous response, not another projection", payload)
	}
	if same != d {
		t.Errorf("verbose payload is a different object than the one passed in")
	}

	// And the prose really is there: this is the path that makes the
	// default narrowing legitimate at all.
	verboseJSON := marshalAck(t, payload)
	if !strings.Contains(verboseJSON, `"lockedByUsername":"`+holder+`"`) {
		t.Errorf("verbose response is missing lockedByUsername — it is the full detail:\n%s", verboseJSON)
	}
	if leanPayload, err := ackPayload(d, false); err != nil {
		t.Fatalf("ackPayload(default) for the holder check: %v", err)
	} else if strings.Contains(marshalAck(t, leanPayload), "lockedByUsername") {
		t.Errorf("the default acknowledgement over the same issue carries lockedByUsername")
	}
	for _, want := range []string{
		d.Description, d.Design, d.AcceptanceCriteria, d.Notes, *d.ClosedReason,
	} {
		if !strings.Contains(verboseJSON, want) {
			t.Errorf("verbose response is missing %q — it is supposed to be the FULL response", want)
		}
	}

	// The default path over the same issue must differ: if both modes
	// returned the same thing, one of them is broken and the test above
	// would still pass.
	defaultPayload, err := ackPayload(d, false)
	if err != nil {
		t.Fatalf("ackPayload(default): %v", err)
	}
	if len(marshalAck(t, defaultPayload)) >= len(verboseJSON) {
		t.Errorf("the default acknowledgement is not smaller than the verbose response — "+
			"default %d bytes, verbose %d", len(marshalAck(t, defaultPayload)), len(verboseJSON))
	}
}

// row 11, second half — ackBatchFromIssues reports what it was given and
// never invents a partial success. The claim batch and epic_create are
// all-or-nothing; a summary saying "9 of 10" would describe a state the
// database never reaches.
func TestAckBatch_ReportsEveryInputRowAndNoPartialSuccess(t *testing.T) {
	details := []*issues.IssueDetail{
		{ID: "sk-1", Version: 2, Status: "in_progress"},
		{ID: "sk-2", Version: 3, Status: "in_progress"},
		{ID: "sk-3", Version: 1, Status: "in_progress"},
	}
	got, err := ackBatchFromIssues(details)
	if err != nil {
		t.Fatalf("ackBatchFromIssues: %v", err)
	}

	if len(got.Results) != len(details) {
		t.Fatalf("results = %d rows, want %d (one per input issue)", len(got.Results), len(details))
	}
	for i, row := range got.Results {
		if row.ID != details[i].ID {
			t.Errorf("results[%d].id = %q, want %q — input order is the contract", i, row.ID, details[i].ID)
		}
		if row.Version != details[i].Version {
			t.Errorf("results[%d].version = %d, want %d", i, row.Version, details[i].Version)
		}
		if !row.OK {
			t.Errorf("results[%d].ok = false on a success path", i)
		}
	}
	want := batchSummary{Requested: len(details), Succeeded: len(details), Failed: 0}
	if got.Summary != want {
		t.Errorf("summary = %+v, want %+v — a group write that reached this helper succeeded "+
			"in full; a non-zero Failed here would describe a partial batch that the "+
			"all-or-nothing group tools never produce", got.Summary, want)
	}
}

// mustAck builds an acknowledgement or fails the test — the success path
// always has a post-state, so an error here is the test's bug.
func mustAck(t *testing.T, d *issues.IssueDetail) ackRow {
	t.Helper()
	row, err := ackFromIssue(d)
	if err != nil {
		t.Fatalf("ackFromIssue: %v", err)
	}
	return row
}

// marshalAck serialises a payload the way a tool would.
func marshalAck(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestAck_AcceptanceStampIsTheAcceptSurfaceOnly pins both halves of the
// sk-282 T021 split: the common row has no acceptance stamp, and the
// acceptance row does — otherwise skein_accept would acknowledge a write
// with nothing that proves it applied (it moves no status, and a repeat
// call bumps no version).
func TestAck_AcceptanceStampIsTheAcceptSurfaceOnly(t *testing.T) {
	d := ackFixtureIssue()

	common := marshalAck(t, mustAck(t, d))
	if strings.Contains(common, "acceptedAt") {
		t.Errorf("the common acknowledgement carries acceptedAt:\n%s", common)
	}

	accepted, err := ackFromAcceptedIssue(d)
	if err != nil {
		t.Fatalf("ackFromAcceptedIssue: %v", err)
	}
	encoded := marshalAck(t, accepted)
	var got map[string]any
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["acceptedAt"]; !present {
		t.Errorf("the acceptance acknowledgement has no acceptedAt:\n%s", encoded)
	}
	// Everything else must be identical to the common row — the split is
	// one key, not a second shape.
	delete(got, "acceptedAt")
	var base map[string]any
	if err := json.Unmarshal([]byte(common), &base); err != nil {
		t.Fatalf("unmarshal common: %v", err)
	}
	if !reflect.DeepEqual(got, base) {
		t.Errorf("acceptance acknowledgement differs from the common row by more than the "+
			"stamp.\n got %v\nwant %v", got, base)
	}

	// And it must still carry no free text.
	for _, key := range ackForbiddenTextFields {
		if v, present := got[key]; present {
			t.Errorf("acceptance acknowledgement carries %q = %#v", key, v)
		}
	}
	if strings.Contains(encoded, "SENTINEL") {
		t.Errorf("acceptance acknowledgement contains caller-supplied text:\n%s", encoded)
	}
}

// TestAck_NilIssueIsAnErrorOnTheAcceptancePathToo — the acceptance
// builder must not turn a missing post-state into a fake success either.
func TestAck_NilIssueIsAnErrorOnTheAcceptancePath(t *testing.T) {
	if _, err := ackFromAcceptedIssue(nil); !errors.Is(err, ErrAckNilIssue) {
		t.Errorf("ackFromAcceptedIssue(nil) error = %v, want ErrAckNilIssue", err)
	}
}
