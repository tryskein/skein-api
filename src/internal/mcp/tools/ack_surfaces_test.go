package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// ack_surfaces_test.go — matrix row 75 (sk-282 T020).
//
// "US-5: update, accept, unclaim, commit attach, reparent and
// delegation answer with a versioned acknowledgement and without a
// single free-text field; `verbose=true` — the previous full
// response."
//
// The six files named by T020 carry seven registered ACTIONS (delegation
// is two tools behind one handler), and they are asserted as a CLASS
// rather than one by one. A per-tool assertion would pass while a
// seventh surface quietly kept the card — which is exactly the partial
// application FP-2 / FP-9 warn about, and the reason this group is one
// task in one head.

// ackWireKeys is the set of JSON keys an acknowledgement may carry.
//
// It is DERIVED from ackRow's struct tags, never transcribed. A
// hand-written list is the defect this run has already paid for three
// times: it agrees with the code on the day it is written and lies
// silently afterwards. Ratcheting it to the type means a field added to
// ackRow is legal here the moment it is legal on the wire, and a field
// that leaks onto the wire from ANYWHERE ELSE — the issue card, a
// wrapper, a future "just this one extra key" — fails the test with the
// key's own name.
func ackWireKeys(t *testing.T) map[string]bool {
	t.Helper()
	rt := reflect.TypeOf(ackRow{})
	out := make(map[string]bool, rt.NumField())
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	if len(out) == 0 {
		t.Fatal("ackRow exposes no JSON keys — the ratchet would pass on anything")
	}
	return out
}

// assertAcknowledgementShape checks one write response against the
// acknowledgement contract: nothing on the wire that ackRow does not
// declare, plus the three things INV-7 says a confirmation must carry
// (identifier, authoritative post-state, version).
func assertAcknowledgementShape(t *testing.T, out map[string]any) {
	t.Helper()
	allowed := ackWireKeys(t)
	for k, v := range out {
		if !allowed[k] {
			t.Errorf("response carries %q = %#v — not a key of the acknowledgement; "+
				"the write-tool is still emitting the issue card", k, v)
		}
	}
	// Named explicitly as well as covered by the allowlist: a failure on
	// "closedReason" should read as the US-5 violation it is, not as an
	// anonymous extra key.
	for _, forbidden := range ackForbiddenTextFields {
		if v, present := out[forbidden]; present {
			t.Errorf("response echoes free text %q = %#v — US-5 forbids it", forbidden, v)
		}
	}
	if id, _ := out["id"].(string); id == "" {
		t.Errorf("acknowledgement has no id: %#v", out)
	}
	if out["ok"] != true {
		t.Errorf("ok = %#v, want true", out["ok"])
	}
	if v, ok := out["version"].(float64); !ok || v < 1 {
		t.Errorf("version = %#v — INV-7: the caller continues its optimistic cycle "+
			"from this number, so a missing or zero version forces an extra read",
			out["version"])
	}
	if s, _ := out["status"].(string); s == "" {
		t.Errorf("status = %#v — the authoritative post-state is what proves the "+
			"server applied the write", out["status"])
	}
}

// ackProse is the hostile fixture: every free-text field of the issue
// carries a distinct, greppable sentinel.
//
// Hostile, not convenient: the values are long enough to be visible in a
// size check, distinct per field so a leak names itself, and they are
// planted in fields the caller did NOT send on the call under test —
// so a response that "helpfully" returns the card fails even though the
// arguments of that particular call were innocent.
var ackProse = map[string]string{
	"description":         "SENTINEL-DESCRIPTION — the caller is already holding this text",
	"design":              "SENTINEL-DESIGN — echoing it costs context on every later turn",
	"acceptance_criteria": "SENTINEL-ACCEPTANCE — US-5 forbids returning it",
	"notes":               "SENTINEL-NOTES — read it back with skein_show instead",
	"source_context":      "SENTINEL-SOURCECONTEXT — supplied by the caller on create",
	"title":               "SENTINEL-TITLE of the probed issue",
}

// seedAckProbe seeds one issue whose every free-text field is a
// sentinel, and returns its id.
func seedAckProbe(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, owner uuid.UUID, id string) string {
	t.Helper()
	seedIssue(t, pool, pid, owner, id, ackProse["title"], "open", 2)
	if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET description = $2, design = $3, acceptance_criteria = $4,
       notes = $5, source_context = $6
 WHERE id = $1`,
		id, ackProse["description"], ackProse["design"],
		ackProse["acceptance_criteria"], ackProse["notes"], ackProse["source_context"]); err != nil {
		t.Fatalf("seed ack probe prose: %v", err)
	}
	return id
}

// assertNoSentinelLeak scans the RAW serialised response.
//
// A key-name check cannot see prose that comes back under a name the
// forbidden list does not know (a wrapper, a renamed field, a nested
// copy). The sentinels are unique strings, so their absence from the
// bytes is the statement that actually matters.
func assertNoSentinelLeak(t *testing.T, raw string) {
	t.Helper()
	if strings.Contains(raw, "SENTINEL") {
		t.Errorf("response carries caller-held free text:\n%s", raw)
	}
}

// assertVerboseIsTheFullCard is the INV-4 half: the named way out
// returns the PREVIOUS full response, prose included. A "full" mode that
// is itself narrowed would not be a way out.
func assertVerboseIsTheFullCard(t *testing.T, out map[string]any, raw string) {
	t.Helper()
	for _, field := range []string{"description", "design", "acceptanceCriteria", "notes"} {
		if _, present := out[field]; !present {
			t.Errorf("verbose response is missing %q — it must be the full IssueDetail", field)
		}
	}
	for _, want := range []string{
		ackProse["description"], ackProse["design"],
		ackProse["acceptance_criteria"], ackProse["notes"], ackProse["title"],
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("verbose response lost %q:\n%s", want, raw)
		}
	}
}

// boolPtr is the one-liner the dismissedFlag exception needs: a nil
// pointer means "must not carry the key", a non-nil one states the value.
func boolPtr(v bool) *bool { return &v }

// ackSurface is one registered write action of the T020 set.
type ackSurface struct {
	name string
	// carriesAcceptanceStamp marks the ONE surface whose acknowledgement
	// legitimately includes `acceptedAt` (sk-282 T021). The tag-derived
	// allowlist cannot express this — `acceptedAt` is a declared ackRow
	// key — so the exception is stated here, and every other surface is
	// asserted NOT to carry it. Without that leg, putting the stamp back
	// on the common row would go unnoticed and SC-006 would start
	// failing in a different test file.
	carriesAcceptanceStamp bool
	// dismissedFlag marks the TWO surfaces whose acknowledgement
	// legitimately includes `dismissed` (the delegation pair), and states
	// the value each of them must report: true for the drop, false for the
	// resume. Same shape of exception as carriesAcceptanceStamp, and here
	// for the stronger reason — the delegation tools move nothing else the
	// row carries, so without this key their two answers are identical
	// bytes and neither proves the write applied (found on a live database
	// in review). nil = the surface must NOT carry the key.
	dismissedFlag *bool
	// run performs the action against a freshly-seeded probe issue.
	run func(t *testing.T, pool *pgxpool.Pool, store *issues.Store,
		ctx context.Context, userID uuid.UUID, pid uuid.UUID, prefix, issueID string,
		verbose bool) *mcpgo.CallToolResult
}

func ackSurfaces() []ackSurface {
	call := func(h server.ToolHandlerFunc, ctx context.Context, args map[string]any) func(*testing.T) *mcpgo.CallToolResult {
		return func(t *testing.T) *mcpgo.CallToolResult {
			t.Helper()
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = args
			res, err := h(ctx, req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
			}
			return res
		}
	}
	withVerbose := func(args map[string]any, verbose bool) map[string]any {
		if verbose {
			args["verbose"] = true
		}
		return args
	}

	return []ackSurface{
		{
			name: "skein_update",
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
				_ uuid.UUID, _ uuid.UUID, _, issueID string, verbose bool) *mcpgo.CallToolResult {
				// Patch a field the caller did NOT send prose for: the
				// sentinels must stay out of the answer anyway.
				return call(handleUpdate(pool, store), ctx, withVerbose(map[string]any{
					"id":              issueID,
					"expectedVersion": issueVersion(t, pool, issueID),
					"priority":        3,
				}, verbose))(t)
			},
		},
		{
			name:                   "skein_accept",
			carriesAcceptanceStamp: true,
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
				userID uuid.UUID, _ uuid.UUID, _, issueID string, verbose bool) *mcpgo.CallToolResult {
				if _, err := pool.Exec(context.Background(),
					`UPDATE issues SET assignee=(SELECT username FROM users WHERE id=$2) WHERE id=$1`,
					issueID, userID); err != nil {
					t.Fatalf("seed assignee: %v", err)
				}
				return call(handleAccept(pool, store), ctx, withVerbose(map[string]any{
					"id": issueID,
				}, verbose))(t)
			},
		},
		{
			name: "skein_unclaim",
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
				_ uuid.UUID, _ uuid.UUID, _, issueID string, verbose bool) *mcpgo.CallToolResult {
				claim := call(handleClaim(pool, store), ctx, map[string]any{"id": issueID})(t)
				_ = claim
				return call(handleUnclaim(pool, store), ctx, withVerbose(map[string]any{
					"id":         issueID,
					"nextStatus": "open",
					"reason":     "SENTINEL-UNCLAIM-REASON must not come back",
				}, verbose))(t)
			},
		},
		{
			name: "skein_commits_attach",
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
				_ uuid.UUID, _ uuid.UUID, _, issueID string, verbose bool) *mcpgo.CallToolResult {
				return call(handleCommitsAttach(pool, store), ctx, withVerbose(map[string]any{
					"issueId": issueID,
					"commits": []any{map[string]any{
						"hash":    "0ddba11",
						"message": "SENTINEL-COMMIT-MESSAGE",
					}},
				}, verbose))(t)
			},
		},
		{
			name: "skein_reparent",
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
				userID uuid.UUID, pid uuid.UUID, prefix, issueID string, verbose bool) *mcpgo.CallToolResult {
				parent := seedIssue(t, pool, pid, userID, prefix+"-900", "new parent", "open", 2)
				return call(handleReparent(pool, dependencies.NewStore(pool), store), ctx,
					withVerbose(map[string]any{
						"id":              issueID,
						"newParentId":     parent,
						"expectedVersion": issueVersion(t, pool, issueID),
					}, verbose))(t)
			},
		},
		{
			name:          "skein_drop_delegated",
			dismissedFlag: boolPtr(true),
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
				_ uuid.UUID, _ uuid.UUID, _, issueID string, verbose bool) *mcpgo.CallToolResult {
				return call(handleDelegationAction(pool, store, true), ctx, withVerbose(map[string]any{
					"id": issueID,
				}, verbose))(t)
			},
		},
		{
			name:          "skein_resume_delegated",
			dismissedFlag: boolPtr(false),
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
				_ uuid.UUID, _ uuid.UUID, _, issueID string, verbose bool) *mcpgo.CallToolResult {
				drop := call(handleDelegationAction(pool, store, true), ctx,
					map[string]any{"id": issueID})(t)
				_ = drop
				return call(handleDelegationAction(pool, store, false), ctx,
					withVerbose(map[string]any{"id": issueID}, verbose))(t)
			},
		},
	}
}

// TestAckSurfaces_CommonWritePath_AcknowledgeAndOptOut is matrix row 75.
func TestAckSurfaces_CommonWritePath_AcknowledgeAndOptOut(t *testing.T) {
	surfaces := ackSurfaces()
	// The count is asserted so that deleting a case to make the suite
	// green is a visible edit rather than a silent one.
	if len(surfaces) != 7 {
		t.Fatalf("expected the 7 registered actions of T020, got %d", len(surfaces))
	}

	for _, s := range surfaces {
		for _, verbose := range []bool{false, true} {
			mode := "acknowledgement"
			if verbose {
				mode = "verbose"
			}
			t.Run(s.name+"/"+mode, func(t *testing.T) {
				pool := newTestPool(t)
				userID, _ := seedUser(t, pool, "admin")
				pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "acksurf", 1)
				issueID := seedAckProbe(t, pool, pid, userID, prefix+"-1")

				store := issues.NewStore(pool)
				ctx := withCtxSession(t, userID, "admin", "sess-ack-"+s.name)

				res := s.run(t, pool, store, ctx, userID, pid, prefix, issueID, verbose)
				raw := toolText(t, res)
				out := extractToolJSON(t, res)

				if verbose {
					assertVerboseIsTheFullCard(t, out, raw)
					return
				}
				assertAcknowledgementShape(t, out)
				stamp, hasStamp := out["acceptedAt"]
				if s.carriesAcceptanceStamp {
					if !hasStamp || stamp == nil {
						t.Errorf("%s must carry acceptedAt — it is the only proof this "+
							"write applied", s.name)
					}
				} else if hasStamp {
					t.Errorf("%s carries acceptedAt = %v — the acceptance stamp belongs to "+
						"skein_accept alone (SC-006 pays 42 characters for it)", s.name, stamp)
				}
				flag, hasFlag := out["dismissed"]
				switch {
				case s.dismissedFlag == nil && hasFlag:
					t.Errorf("%s carries dismissed = %v — the delegation flag belongs to "+
						"skein_drop_delegated / skein_resume_delegated alone", s.name, flag)
				case s.dismissedFlag != nil && !hasFlag:
					t.Errorf("%s must carry dismissed — nothing else in its acknowledgement "+
						"moves, so without it the response proves no write applied", s.name)
				case s.dismissedFlag != nil && flag != *s.dismissedFlag:
					t.Errorf("%s dismissed = %v, want %v — the flag is the caller's own "+
						"post-state in dismissed_by", s.name, flag, *s.dismissedFlag)
				}
				assertNoSentinelLeak(t, raw)
				t.Logf("%s acknowledgement is %d bytes: %s", s.name, len(raw), raw)

				// INV-3: the free text is still in the system and still
				// reachable by a reading tool — the cut is on the wire,
				// not in the data.
				show := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
					map[string]any{"id": issueID, "includeComments": false, "includeDeps": false})
				shown, ok := show["issue"].(map[string]any)
				if !ok {
					t.Fatalf("skein_show issue type %T", show["issue"])
				}
				for wireName, field := range map[string]string{
					"description":        "description",
					"design":             "design",
					"acceptanceCriteria": "acceptance_criteria",
					"notes":              "notes",
					"sourceContext":      "source_context",
				} {
					if shown[wireName] != ackProse[field] {
						t.Errorf("skein_show %s = %#v, want the stored sentinel %q — "+
							"INV-3: the cut is on the wire, the text stays in the system",
							wireName, shown[wireName], ackProse[field])
					}
				}
			})
		}
	}
}

// TestAckWireKeys_MatchesTheDeclaredStruct is the ratchet's own guard:
// if ackRow ever loses its json tags (or gains an untagged exported
// field), the allowlist would silently widen to Go field names and stop
// catching card leaks.
func TestAckWireKeys_MatchesTheDeclaredStruct(t *testing.T) {
	keys := ackWireKeys(t)
	row, err := ackFromIssue(&issues.IssueDetail{ID: "sk-1", Version: 3, Status: "open"})
	if err != nil {
		t.Fatalf("ackFromIssue: %v", err)
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal ack row: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal ack row: %v", err)
	}
	for k := range m {
		if !keys[k] {
			t.Errorf("serialised acknowledgement carries %q, which the tag-derived "+
				"allowlist does not know", k)
		}
	}
	for _, must := range []string{"id", "ok", "version", "status"} {
		if !keys[must] {
			t.Errorf("ackRow no longer declares %q — INV-7 requires it", must)
		}
	}
}

// jsonTagNames returns a struct's declared JSON key names.
func jsonTagNames(t *testing.T, rt reflect.Type) map[string]bool {
	t.Helper()
	out := make(map[string]bool, rt.NumField())
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// assertBatchWireFormUnchanged is matrix row 89 (sk-282 T020).
//
// "Edge 'group close or update, some rows in version conflict':
// the call succeeds, one row per input issue plus a summary — the
// shape did not change."
//
// US-5 names the group close/update response THE reference form and
// forbids repairing it. This checks the WIRE, which is the thing US-5
// actually promises and the thing a wrapper, an envelope rename, or an
// accidental extra key would break without touching a struct at all.
func assertBatchWireFormUnchanged(t *testing.T, body map[string]any, wantRows int) {
	t.Helper()
	assertBatchWireFormAgainst(t, body, wantRows,
		reflect.TypeOf(batchResponse{}), reflect.TypeOf(batchResultRow{}),
		"the reference row")
}

// assertCloseManyWireForm is skein_close_many's OWN wire guard
// (sk-489, US-9, matrix row 29).
//
// It replaces assertBatchWireFormUnchanged on that ONE tool, and the
// replacement is the deliberate part. US-9 requires the batch close to
// report each row's outcome and, when the row named a target, the link
// state; the reference row declares neither, so the previous guard — «a
// row is a subset of batchResultRow» — now fails by design rather than by
// regression. Turning it off for close_many would have removed the guard;
// pointing it at close_many's own pair keeps every property it had:
// no extra envelope key, no undeclared row key, a version on every
// success, a structured error on every failure.
//
// The reference pair keeps its guard, and skein_update_many keeps calling
// it (update_many_test.go). That is the whole point of splitting the
// types: the extension is one tool's, and the form US-5 froze stays
// frozen for the tools it still describes.
func assertCloseManyWireForm(t *testing.T, body map[string]any, wantRows int) {
	t.Helper()
	assertBatchWireFormAgainst(t, body, wantRows,
		reflect.TypeOf(closeManyResponse{}), reflect.TypeOf(closeManyResultRow{}),
		"skein_close_many's own row")
}

// assertBatchWireFormAgainst is the shared body of the two guards above,
// parameterised by the envelope and row types the surface declares.
//
// One body rather than two copies so that a property added to one guard
// cannot be missing from the other — the failure mode of a copied guard is
// that it keeps passing while asserting less.
func assertBatchWireFormAgainst(
	t *testing.T,
	body map[string]any,
	wantRows int,
	envelopeType, rowType reflect.Type,
	rowLabel string,
) {
	t.Helper()

	envelope := jsonTagNames(t, envelopeType)
	for k := range body {
		if !envelope[k] {
			t.Errorf("group response grew a top-level key %q — US-5 says this form does "+
				"not change", k)
		}
	}
	results, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("results missing or not an array: %+v", body)
	}
	if len(results) != wantRows {
		t.Fatalf("results len = %d, want one row per input issue (%d)", len(results), wantRows)
	}

	rowKeys := jsonTagNames(t, rowType)
	errKeys := jsonTagNames(t, reflect.TypeOf(batchErrorDTO{}))
	for i, raw := range results {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("results[%d] is %T, want an object", i, raw)
		}
		for k := range row {
			if !rowKeys[k] {
				t.Errorf("results[%d] carries %q, which %s does not declare", i, k, rowLabel)
			}
		}
		if _, present := row["id"]; !present {
			t.Errorf("results[%d] has no id", i)
		}
		okFlag, isBool := row["ok"].(bool)
		if !isBool {
			t.Errorf("results[%d].ok = %#v, want a bool", i, row["ok"])
			continue
		}
		if okFlag {
			// A successful row must carry the version — that is what
			// makes the group response an acknowledgement already.
			if v, isNum := row["version"].(float64); !isNum || v < 1 {
				t.Errorf("results[%d].version = %#v on a successful row", i, row["version"])
			}
			if _, present := row["error"]; present {
				t.Errorf("results[%d] carries both ok=true and an error", i)
			}
			continue
		}
		errObj, isObj := row["error"].(map[string]any)
		if !isObj {
			t.Errorf("results[%d].ok = false with no structured error: %+v", i, row)
			continue
		}
		for k := range errObj {
			if !errKeys[k] {
				t.Errorf("results[%d].error carries %q, which the reference error does not "+
					"declare", i, k)
			}
		}
		if code, _ := errObj["code"].(string); code == "" {
			t.Errorf("results[%d].error has no code", i)
		}
	}

	summary, ok := body["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing or not an object: %+v", body)
	}
	summaryKeys := jsonTagNames(t, reflect.TypeOf(batchSummary{}))
	for k := range summary {
		if !summaryKeys[k] {
			t.Errorf("summary carries %q, which the reference summary does not declare", k)
		}
	}
	for _, must := range []string{"requested", "succeeded", "failed"} {
		if _, present := summary[must]; !present {
			t.Errorf("summary has no %q", must)
		}
	}
}

// TestAckBypasses_VerboseReturnsTheFullObject is matrix row 75a.
//
// "INV-4 on four serialisation-point bypasses (FP-2/FP-9):
// `verbose=true` returns the previous full object for create, single
// claim, group claim and group epic create (for the last — the epic
// AND the children). T007's helper does not prove this: it is not
// wired to any of those tools, and a bypass could wrap the full
// object in a different envelope."
//
// The four bypasses are asserted together, in the file that owns the
// class, rather than one per tool file. That is deliberate: the defect
// this row exists to catch is one of the four keeping its own
// serialisation path, and four separate tests in four files is exactly
// the arrangement in which the fourth one goes unwritten (FP-2 / FP-9).
// Each case knows how to reach the full object through ITS OWN envelope,
// which is the wrapper half of the row.
func TestAckBypasses_VerboseReturnsTheFullObject(t *testing.T) {
	const prose = "SENTINEL-VERBOSE description that only the full object carries"

	t.Run("skein_create", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, _ := seedProject(t, pool, userID, "v75create")
		ctx := withCtx(t, userID, "admin")

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project":     pid.String(),
			"title":       "verbose create probe",
			"type":        "task",
			"kind":        "directed",
			"description": prose,
			"verbose":     true,
		}
		res, err := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool),
			projects.NewStore(pool))(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		body := extractToolJSON(t, res)
		issue, ok := body["issue"].(map[string]any)
		if !ok {
			t.Fatalf("issue missing from the create envelope: %+v", body)
		}
		assertFullObject(t, issue, prose)
	})

	t.Run("skein_claim single", func(t *testing.T) {
		pool, ctx, store, ids := seedVerboseProbeIssues(t, "v75claim", prose, 1)
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"id": ids[0], "verbose": true}
		res, err := handleClaim(pool, store)(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		assertFullObject(t, extractToolJSON(t, res), prose)
	})

	t.Run("skein_claim batch", func(t *testing.T) {
		pool, ctx, store, ids := seedVerboseProbeIssues(t, "v75batch", prose, 2)
		values := make([]any, len(ids))
		for i, id := range ids {
			values[i] = id
		}
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"ids": values, "verbose": true}
		res, err := handleClaim(pool, store)(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		body := extractToolJSON(t, res)
		// The wrapper half of the row: the group envelope is the same in
		// both modes, and the FULL object sits in the acknowledgement's
		// place inside it.
		rows, ok := body["results"].([]any)
		if !ok || len(rows) != len(ids) {
			t.Fatalf("results = %#v, want %d full rows", body["results"], len(ids))
		}
		for i, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("results[%d] is %T, want an object", i, raw)
			}
			assertFullObject(t, row, prose)
		}
		if _, present := body["summary"]; !present {
			t.Errorf("verbose group response dropped the summary: %+v", body)
		}
	})

	t.Run("skein_epic_create", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, _ := seedProject(t, pool, userID, "v75epic")
		ctx := withCtx(t, userID, "admin")

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project":     pid.String(),
			"title":       "verbose epic probe",
			"kind":        "directed",
			"description": prose,
			"childTemplates": []any{
				map[string]any{"title": "first child", "description": prose},
				map[string]any{"title": "second child", "description": prose},
			},
			"verbose": true,
		}
		res, err := handleEpicCreate(pool, issues.NewStore(pool), projects.NewStore(pool))(ctx, req)
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
		}
		body := extractToolJSON(t, res)
		epic, ok := body["epic"].(map[string]any)
		if !ok {
			t.Fatalf("epic missing: %+v", body)
		}
		assertFullObject(t, epic, prose)
		// "for the last — the epic AND the children": the epic alone passing would be
		// the half-application this row is written against.
		children, ok := body["children"].([]any)
		if !ok || len(children) != 2 {
			t.Fatalf("children = %#v, want 2 full rows", body["children"])
		}
		for i, raw := range children {
			child, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("children[%d] is %T, want an object", i, raw)
			}
			assertFullObject(t, child, prose)
		}
	})
}

// assertFullObject checks that a wire object is the untouched
// IssueDetail: the prose is there with the value the caller stored, and
// none of the acknowledgement's narrowing has been applied.
func assertFullObject(t *testing.T, obj map[string]any, wantDescription string) {
	t.Helper()
	if obj["description"] != wantDescription {
		t.Errorf("description = %#v, want %q — verbose must return the full object",
			obj["description"], wantDescription)
	}
	for _, key := range []string{"design", "acceptanceCriteria", "notes", "title", "type",
		"projectId", "createdAt"} {
		if _, present := obj[key]; !present {
			t.Errorf("verbose object is missing %q — it is not the full IssueDetail", key)
		}
	}
	if _, present := obj["ok"]; present {
		t.Errorf("verbose object carries the acknowledgement's `ok` — the two shapes got "+
			"mixed: %v", obj["ok"])
	}
}

// seedVerboseProbeIssues seeds n open issues carrying `prose` in every
// text field, and returns everything the claim handler needs.
func seedVerboseProbeIssues(t *testing.T, prefixName, prose string, n int) (
	*pgxpool.Pool, context.Context, *issues.Store, []string,
) {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, prefixName)
	ids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		seedIssue(t, pool, pid, userID, id, "verbose probe", "open", 2)
		if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET description = $2, design = $2, acceptance_criteria = $2, notes = $2
 WHERE id = $1`, id, prose); err != nil {
			t.Fatalf("seed prose on %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	return pool, withCtxSession(t, userID, "admin", "sess-verbose-"+prefixName),
		issues.NewStore(pool), ids
}

// ---------------- sk-489: the outcome in the acknowledgements of THREE surfaces ----------------

// closedAckSurface — one write surface, run on a CLOSED probe,
// with an explicit expectation: does its acknowledgement carry `closeOutcome`.
//
// # Why a separate list and not a field on ackSurface
//
// The shared loop above seeds an OPEN probe (seedAckProbe), and on an open issue
// `closeOutcome` is NULL by INV-1 — and the key disappears by omitempty in
// ANY implementation. The negative leg on such a probe is empty: it is green both
// when only three surfaces put the outcome in and when the shared
// `ackFromIssue` puts it in for all eleven. The discriminating case is a closed
// issue, and the fixture must reach it.
//
// The limit, named so that it is not "fixed" in the wrong direction: for
// `dismissedFlag` there is no such duplicate, and rightly so — it is read only by the shared
// loop over open probes, where it does discriminate.
type closedAckSurface struct {
	name string
	// wantOutcome — the acknowledgement must carry `closeOutcome`.
	wantOutcome bool
	// run performs the action on an ALREADY CLOSED probe.
	run func(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
		issueID string) *mcpgo.CallToolResult
}

// closedAckSurfaces — an explicit list with an expectation on every row.
//
// It is NOT complete by construction and need not be: the US-9 statement is
// "the outcome is on three surfaces and on no other", and it is proved by
// the pair "present on a closing one / absent on a non-closing one". The control
// non-closing surface here is `skein_commits_attach`: it works on a closed
// issue as a matter of course (that is its headline use case), so its acknowledgement is
// the only place where "the outcome leaked into the shared ackFromIssue" is visible
// directly.
//
// The surface counter of the shared loop (len(surfaces) != 7) is not affected by this
// list: that is a different set and a different question.
func closedAckSurfaces() []closedAckSurface {
	callTool := func(h server.ToolHandlerFunc, ctx context.Context, args map[string]any) func(*testing.T) *mcpgo.CallToolResult {
		return func(t *testing.T) *mcpgo.CallToolResult {
			t.Helper()
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = args
			res, err := h(ctx, req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
			}
			return res
		}
	}
	return []closedAckSurface{
		{
			name:        "skein_close",
			wantOutcome: true,
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store,
				ctx context.Context, issueID string) *mcpgo.CallToolResult {
				return callTool(handleClose(pool, store), ctx, map[string]any{
					"id":              issueID,
					"expectedVersion": issueVersion(t, pool, issueID),
					"reason":          "SENTINEL-CLOSE-REASON must not come back",
					"closeOutcome":    "wont_do",
				})(t)
			},
		},
		{
			name:        "skein_close_many",
			wantOutcome: true,
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store,
				ctx context.Context, issueID string) *mcpgo.CallToolResult {
				return callTool(handleCloseMany(pool, store), ctx, map[string]any{
					"items": []any{map[string]any{
						"id":              issueID,
						"expectedVersion": float64(issueVersion(t, pool, issueID)),
						"reason":          "SENTINEL-CLOSE-REASON must not come back",
						"closeOutcome":    "wont_do",
					}},
				})(t)
			},
		},
		{
			// sk-489 T009 (matrix row 34, last clause): the third closing
			// surface. It does not close HERE — the probe is already
			// closed and the call carries no status — it EDITS the
			// outcome, which is the second of its two routes and the one
			// that reaches Store.PatchIssueWithResult.
			name:        "skein_update",
			wantOutcome: true,
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store,
				ctx context.Context, issueID string) *mcpgo.CallToolResult {
				return callTool(handleUpdate(pool, store), ctx, map[string]any{
					"id":              issueID,
					"expectedVersion": issueVersion(t, pool, issueID),
					"closeOutcome":    "wont_do",
				})(t)
			},
		},
		{
			name:        "skein_commits_attach",
			wantOutcome: false,
			run: func(t *testing.T, pool *pgxpool.Pool, store *issues.Store,
				ctx context.Context, issueID string) *mcpgo.CallToolResult {
				return callTool(handleCommitsAttach(pool, store), ctx, map[string]any{
					"issueId": issueID,
					"commits": []any{map[string]any{"hash": "0ddba11"}},
				})(t)
			},
		},
	}
}

// TestClosedAckSurfaces_OutcomeOnlyWhereDeclared — matrix, row 31.
func TestClosedAckSurfaces_OutcomeOnlyWhereDeclared(t *testing.T) {
	for _, s := range closedAckSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "admin")
			pid, prefix, _ := seedProjectWithRepos(t, pool, userID, "ackclosed", 1)
			issueID := seedAckProbe(t, pool, pid, userID, prefix+"-1")
			// CLOSED, and closed with an outcome other than the default: on
			// an open probe the key is absent in any implementation, and on one
			// closed with `done` it would be easier to confuse the "surface's outcome" and the "issue's
			// outcome".
			if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET status = 'closed', closed_at = now(), closed_reason = 'seeded',
       close_outcome = 'duplicate', version = version + 1
 WHERE id = $1`, issueID); err != nil {
				t.Fatalf("seed closed probe: %v", err)
			}

			store := issues.NewStore(pool)
			ctx := withCtxSession(t, userID, "admin", "sess-ackclosed-"+s.name)
			res := s.run(t, pool, store, ctx, issueID)
			raw := toolText(t, res)
			out := extractToolJSON(t, res)

			// The batch answers with rows; the key is looked up in the row, not in the
			// envelope — otherwise the test would say nothing about the batch at all.
			probe := out
			if rows, isBatch := out["results"].([]any); isBatch {
				if len(rows) != 1 {
					t.Fatalf("%s: expected one row, got %d", s.name, len(rows))
				}
				row, isObj := rows[0].(map[string]any)
				if !isObj {
					t.Fatalf("%s: results[0] is %T", s.name, rows[0])
				}
				probe = row
			}

			outcome, present := probe["closeOutcome"]
			switch {
			case s.wantOutcome && !present:
				t.Errorf("%s: the acknowledgement does not carry closeOutcome — US-9 requires the outcome in "+
					"the post-state of the three closing surfaces, otherwise the caller "+
					"must re-read the issue after every close", s.name)
			case s.wantOutcome && outcome == nil:
				t.Errorf("%s: closeOutcome = null on a CLOSED issue — INV-1 does not allow such "+
					"a state", s.name)
			case !s.wantOutcome && present:
				t.Errorf("%s: the acknowledgement carries closeOutcome = %v. The outcome is filled in by "+
					"ackFromClosedIssue and only by it; the key here means that the outcome "+
					"leaked into the shared ackFromIssue and will arrive on all eleven "+
					"tools", s.name, outcome)
			}
			assertNoSentinelLeak(t, raw)
		})
	}
}
