package handler

// bulk_close_outcome_test.go — sk-489, T007, matrix row 26.
//
// # Subject
//
//	POST /projects/{pid}/issues/bulk  → handler/bulk.go#Bulk + #runOne
//
// The batch is the THIRD surface for writing the outcome, and it has its own refusal contract,
// different from the two single ones:
//
//   - the outcome and the target here are properties of the REQUEST (one outcome and one target for the whole
//     batch), so their refusal rejects the WHOLE call BEFORE the loop and applies
//     nothing;
//   - an item that equals the target is a property of the ROW, so its refusal
//     leaves the other items done.
//
// This is exactly the OPPOSITE of the MCP batch (matrix row 65), where a target refusal
// is an item refusal. The difference is deliberate: there the outcome and the target come PER ITEM,
// here - per request. Both rows are checked, and together they forbid "reducing the
// surfaces to one rule".
//
// # What each assertion discriminates
//
//   - "a target in another visible project is accepted" goes red on an implementation
//     that checked the target via issues.Store.IssueBelongsToProject: that one is scoped
//     to the batch's project and would reject a cross-project target allowed by US-2;
//   - "400/422 of the WHOLE batch, nothing applied" goes red on an implementation
//     that put the target check inside #runOne: it would refuse every
//     item - or, worse, refuse one while the others are done;
//   - "item = target is an ITEM refusal" goes red on an implementation that moved this
//     check before the loop too: a single self-target would sink the whole batch.
//
// The main caller is not an admin for the same reason as in
// issues_close_outcome_test.go: an admin's horizon does not tell a visible target from an
// invisible one. The reverse blindness is there too: swapping the SYSTEM role for Actor.Role
// on a non-admin is invisible (both non-admin, membership is counted the same), and
// only an admin with a target outside his membership tells them apart. For the batch this is
// #TestBulkCloseOutcome_TargetVisibilityUsesTheSystemRole below: it has its own
// role-carrier line - handler/bulk.go#checkBulkCloseOutcome, separate from
// #closeTargetCaller of the single surfaces.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// bcoBulk runs one batch with an arbitrary body - including the outcome and target fields
// that callXPBulk (the sk-194.6.50 guard) does not know and need not know.
func bcoBulk(t *testing.T, env *coEnv, body map[string]any) (xpResponse, bulkResponse) {
	t.Helper()
	return bcoBulkAs(t, env, env.user, "user", body)
}

// bcoBulkAs is the same batch from an arbitrary caller. Needed by the probe of
// the system role: it is the only one that goes as "admin".
func bcoBulkAs(t *testing.T, env *coEnv, userID uuid.UUID, role string,
	body map[string]any) (xpResponse, bulkResponse) {
	t.Helper()
	bh := NewBulkHandler(env.pool, env.issues.issuesStore)
	req := xpRequest(t, http.MethodPost, "", body, userID, role,
		"projectID", env.pid.String())
	rec := httptest.NewRecorder()
	bh.Bulk(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	full := xpResponse{Status: res.StatusCode, ContentType: res.Header.Get("Content-Type")}
	raw := rec.Body.Bytes()
	full.Raw = string(raw)
	_ = json.Unmarshal(raw, &full.Body)
	var parsed bulkResponse
	_ = json.Unmarshal(raw, &parsed)
	return full, parsed
}

// bcoStates snapshots the state of several issues at once - "nothing applied"
// cannot be asserted from a single row.
func bcoStates(t *testing.T, env *coEnv, ids ...string) map[string]coState {
	t.Helper()
	out := make(map[string]coState, len(ids))
	for _, id := range ids {
		out[id] = coRead(t, env, id)
	}
	return out
}

func bcoSameStates(a, b map[string]coState) bool {
	if len(a) != len(b) {
		return false
	}
	for id, sa := range a {
		if sb, ok := b[id]; !ok || sa != sb {
			return false
		}
	}
	return true
}

// bcoClose closes an issue by a transition - preparing an "already closed" item.
func bcoClose(t *testing.T, env *coEnv, id string, outcome string) {
	t.Helper()
	body := map[string]any{"status": "closed"}
	if outcome != "" {
		body["closeOutcome"] = outcome
	}
	// sk-540.2: duplicate / superseded name a target on a transition.
	if outcome == "duplicate" || outcome == "superseded" {
		body["closeTarget"] = coSeed(t, env, env.pid, env.prefix,
			"target-"+strings.TrimPrefix(id, env.prefix+"-"))
	}
	if res := coTransition(t, env, id, coRead(t, env, id).Version, body); res.Status != http.StatusOK {
		t.Fatalf("preparation: closing %s = %d (body=%s)", id, res.Status, res.Raw)
	}
}

// bcoStanding switches an issue to "standing" directly in the database: for a standing
// issue the archive default does NOT apply (US-1), and it is a separate cell.
func bcoStanding(t *testing.T, env *coEnv, id string) {
	t.Helper()
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE issues SET status = 'standing' WHERE id = $1`, id); err != nil {
		t.Fatalf("switching %s to standing: %v", id, err)
	}
}

// ---------------- archive default ----------------

// TestBulkCloseOutcome_ArchiveMatrix — sk-540.4, the acceptance matrix
// {no outcome, outcome, outcome+target} × {open, closed, standing, mixed}.
//
// Reference: the task's acceptance (owner decision 2026-09-29), not today's
// output:
//   - no outcome + an open row in the batch → the WHOLE request is refused,
//     422 close_outcome_required, nothing written;
//   - no outcome on closed/standing only → archived: closed keeps its
//     outcome, standing gets done;
//   - an outcome (and target) apply to open rows only; closed keep their
//     outcome, standing get done, and neither gets an edge.
//
// The mixed column puts all three row kinds into ONE call: the rule depends
// on the ROW, so an implementation deciding once per batch is green on every
// single-row cell and red only here.
func TestBulkCloseOutcome_ArchiveMatrix(t *testing.T) {
	env := coSetup(t)
	target := coSeed(t, env, env.pid, env.prefix, "b-am-target")

	type rowKind string
	const (
		kOpen     rowKind = "open"
		kClosed   rowKind = "closed"
		kStanding rowKind = "standing"
	)
	// The outcome a closed row carries before the archive. superseded, not
	// done: «keeps its outcome» must differ from every outcome the batch or
	// a default could write.
	const closedBefore = "superseded"

	columns := []struct {
		name  string
		kinds []rowKind
	}{
		{"open", []rowKind{kOpen}},
		{"closed", []rowKind{kClosed}},
		{"standing", []rowKind{kStanding}},
		{"mixed", []rowKind{kOpen, kClosed, kStanding}},
	}
	rowsOf := []struct {
		name    string
		outcome string // "" = not sent
		target  bool
		// wantOpen is the outcome an open row ends with; "" = the request
		// is refused as a whole.
		wantOpen string
	}{
		{name: "no-outcome", wantOpen: ""},
		{name: "outcome", outcome: "wont_do", wantOpen: "wont_do"},
		{name: "outcome+target", outcome: "duplicate", target: true, wantOpen: "duplicate"},
	}

	for ri, r := range rowsOf {
		for ci, col := range columns {
			t.Run(r.name+"/"+col.name, func(t *testing.T) {
				ids := make([]string, 0, len(col.kinds))
				kindOf := map[string]rowKind{}
				for ki, k := range col.kinds {
					id := coSeed(t, env, env.pid, env.prefix,
						"b-am-"+strconv.Itoa(ri)+strconv.Itoa(ci)+strconv.Itoa(ki))
					switch k {
					case kClosed:
						bcoClose(t, env, id, closedBefore)
					case kStanding:
						bcoStanding(t, env, id)
					}
					ids = append(ids, id)
					kindOf[id] = k
				}
				before := bcoStates(t, env, ids...)
				edgesBefore := coEdges(t, env, ids...)

				body := map[string]any{"ids": ids, "action": "archive"}
				if r.outcome != "" {
					body["closeOutcome"] = r.outcome
				}
				if r.target {
					body["closeTarget"] = target
				}
				res, parsed := bcoBulk(t, env, body)

				hasOpen := slices.Contains(col.kinds, kOpen)
				if r.wantOpen == "" && hasOpen {
					if res.Status != http.StatusUnprocessableEntity || coErrCode(res) != "close_outcome_required" {
						t.Fatalf("archive without outcome over an open row = %d/%s, want 422/close_outcome_required (body=%s)",
							res.Status, coErrCode(res), res.Raw)
					}
					if len(parsed.Succeeded) != 0 {
						t.Errorf("refused request reported succeeded rows: %v", parsed.Succeeded)
					}
					var openIDs []string
					for _, id := range ids {
						if kindOf[id] == kOpen {
							openIDs = append(openIDs, id)
						}
					}
					if got := bcoProblemIDs(res); !slices.Equal(got, openIDs) {
						t.Errorf("refusal ids = %v, want the open rows %v", got, openIDs)
					}
					if after := bcoStates(t, env, ids...); !bcoSameStates(before, after) {
						t.Errorf("refused request changed rows:\n  before %+v\n  after  %+v", before, after)
					}
					return
				}

				if res.Status != http.StatusOK || len(parsed.Failed) != 0 {
					t.Fatalf("archive = %d (failed %+v), want 200 (body=%s)", res.Status, parsed.Failed, res.Raw)
				}
				for _, id := range ids {
					want := map[rowKind]string{
						kOpen: r.wantOpen, kClosed: closedBefore, kStanding: "done",
					}[kindOf[id]]
					got := coRead(t, env, id)
					if got.Status != "closed" || got.Outcome != want {
						t.Errorf("%s row %s after archive: %+v, want closed/%s", kindOf[id], id, got, want)
					}
					if reason := bcoClosedReason(t, env, id); reason != "archived" {
						t.Errorf("%s row %s closed_reason = %q, want archived", kindOf[id], id, reason)
					}
				}
				// Edges: only open rows with a target get one; closed and
				// standing rows keep exactly the edges they had.
				var wantEdges []string
				wantEdges = append(wantEdges, edgesBefore...)
				if r.target {
					for _, id := range ids {
						if kindOf[id] == kOpen {
							wantEdges = append(wantEdges, id+"|duplicates|"+target)
						}
					}
				}
				sort.Strings(wantEdges)
				if got := coEdges(t, env, ids...); !slices.Equal(got, wantEdges) {
					t.Errorf("edges after archive = %v, want %v", got, wantEdges)
				}
			})
		}
	}
}

// TestBulkCloseOutcome_ArchiveClosedOnlyIgnoresDuplicateWithoutTarget — a
// batch of only closed/standing rows with duplicate and no target passes:
// the outcome is not applied to them, so the sk-540.2 target rule has
// nothing to refuse, and the closed rows keep their outcome.
func TestBulkCloseOutcome_ArchiveClosedOnlyIgnoresDuplicateWithoutTarget(t *testing.T) {
	env := coSetup(t)
	done := coSeed(t, env, env.pid, env.prefix, "b-acd-done")
	bcoClose(t, env, done, "done")
	wontDo := coSeed(t, env, env.pid, env.prefix, "b-acd-wontdo")
	bcoClose(t, env, wontDo, "wont_do")
	standing := coSeed(t, env, env.pid, env.prefix, "b-acd-standing")
	bcoStanding(t, env, standing)

	res, body := bcoBulk(t, env, map[string]any{
		"ids": []string{done, wontDo, standing}, "action": "archive", "closeOutcome": "duplicate"})
	if res.Status != http.StatusOK || len(body.Failed) != 0 {
		t.Fatalf("archive = %d (failed %+v), want 200 (body=%s)", res.Status, body.Failed, res.Raw)
	}
	for id, want := range map[string]string{done: "done", wontDo: "wont_do", standing: "done"} {
		if got := coRead(t, env, id); got.Status != "closed" || got.Outcome != want {
			t.Errorf("%s after archive: %+v, want closed/%s", id, got, want)
		}
	}
	if got := coEdges(t, env, done, wontDo, standing); len(got) != 0 {
		t.Errorf("archive created edges %v, want none", got)
	}
}

// TestBulkCloseOutcome_ArchiveOpenDuplicateWithoutTargetFailsPerRow — with an
// open row in the batch, duplicate without a target is refused on THAT row
// only (sk-540.2), and the closed row still archives with its outcome kept.
func TestBulkCloseOutcome_ArchiveOpenDuplicateWithoutTargetFailsPerRow(t *testing.T) {
	env := coSetup(t)
	open := coSeed(t, env, env.pid, env.prefix, "b-aod-open")
	closed := coSeed(t, env, env.pid, env.prefix, "b-aod-closed")
	bcoClose(t, env, closed, "done")
	before := bcoStates(t, env, open)

	res, body := bcoBulk(t, env, map[string]any{
		"ids": []string{open, closed}, "action": "archive", "closeOutcome": "duplicate"})
	if res.Status != http.StatusMultiStatus {
		t.Fatalf("archive = %d, want 207 (body=%s)", res.Status, res.Raw)
	}
	if len(body.Failed) != 1 || body.Failed[0].ID != open || body.Failed[0].Code != "close_target_required" {
		t.Fatalf("failed = %+v, want exactly %s/close_target_required", body.Failed, open)
	}
	if after := bcoStates(t, env, open); !bcoSameStates(before, after) {
		t.Errorf("refused open row changed: before %+v, after %+v", before, after)
	}
	if got := coRead(t, env, closed); got.Status != "closed" || got.Outcome != "done" {
		t.Errorf("closed row after archive: %+v, want closed/done", got)
	}
}

// TestBulkCloseOutcome_ArchivePreflightIgnoresIdsOutsideTheProject — the
// preflight reads only rows of the path project: an unknown id neither
// triggers the whole-request refusal nor is reported by it; it fails per row
// as not_found, exactly as before.
func TestBulkCloseOutcome_ArchivePreflightIgnoresIdsOutsideTheProject(t *testing.T) {
	env := coSetup(t)
	closed := coSeed(t, env, env.pid, env.prefix, "b-apf-closed")
	bcoClose(t, env, closed, "done")
	foreignOpen := coSeed(t, env, env.pidOther, env.prefixOther, "b-apf-foreign")
	missing := env.prefix + "-99999"

	res, body := bcoBulk(t, env, map[string]any{
		"ids": []string{closed, foreignOpen, missing}, "action": "archive"})
	if res.Status != http.StatusMultiStatus {
		t.Fatalf("archive = %d, want 207 (body=%s)", res.Status, res.Raw)
	}
	codes := map[string]string{}
	for _, f := range body.Failed {
		codes[f.ID] = f.Code
	}
	if codes[foreignOpen] != "not_found" || codes[missing] != "not_found" {
		t.Fatalf("failed = %+v, want not_found for %s and %s", body.Failed, foreignOpen, missing)
	}
	if got := coRead(t, env, foreignOpen); got.Status != "open" {
		t.Errorf("foreign issue changed: %+v", got)
	}
}

// bcoProblemIDs reads the `ids` list of a whole-request refusal.
func bcoProblemIDs(res xpResponse) []string {
	raw, _ := res.Body["ids"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// bcoClosedReason reads closed_reason directly: «other archive effects as
// today» includes the archived reason.
func bcoClosedReason(t *testing.T, env *coEnv, id string) string {
	t.Helper()
	var reason *string
	if err := env.pool.QueryRow(t.Context(),
		`SELECT closed_reason FROM issues WHERE id = $1`, id).Scan(&reason); err != nil {
		t.Fatalf("read closed_reason %s: %v", id, err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

// TestBulkCloseOutcome_CloseWithoutOutcomeStaysDone — sk-540.1 made the
// outcome mandatory on MCP close transitions ONLY. The REST batch
// `action=close` keeps the old default: a transition without an outcome
// (standing included) gets `done`.
func TestBulkCloseOutcome_CloseWithoutOutcomeStaysDone(t *testing.T) {
	env := coSetup(t)

	open := coSeed(t, env, env.pid, env.prefix, "b-close-noout-open")
	standing := coSeed(t, env, env.pid, env.prefix, "b-close-noout-standing")
	bcoStanding(t, env, standing)

	res, body := bcoBulk(t, env, map[string]any{
		"ids": []string{open, standing}, "action": "close"})
	if res.Status != http.StatusOK {
		t.Fatalf("bulk close = %d, want 200 (body=%s)", res.Status, res.Raw)
	}
	if len(body.Failed) != 0 {
		t.Fatalf("bulk close refused rows: %+v", body.Failed)
	}
	for _, id := range []string{open, standing} {
		if got := coRead(t, env, id); got.Status != "closed" || got.Outcome != "done" {
			t.Errorf("%s after bulk close: %+v, want closed/done", id, got)
		}
	}
}

// TestBulkCloseOutcome_ExplicitOutcome — an explicit outcome in close. (archive is
// covered by TestBulkCloseOutcome_ArchiveMatrix: since sk-540.4 it does not
// apply the outcome to an already closed row.)
func TestBulkCloseOutcome_ExplicitOutcome(t *testing.T) {
	env := coSetup(t)

	for _, action := range []string{"close"} {
		t.Run(action, func(t *testing.T) {
			open := coSeed(t, env, env.pid, env.prefix, "b-expl-open-"+action)
			closed := coSeed(t, env, env.pid, env.prefix, "b-expl-closed-"+action)
			bcoClose(t, env, closed, "done")

			res, body := bcoBulk(t, env, map[string]any{
				"ids": []string{open, closed}, "action": action, "closeOutcome": "wont_do"})
			if res.Status != http.StatusOK {
				t.Fatalf("batch = %d, want 200 (body=%s)", res.Status, res.Raw)
			}
			if len(body.Failed) != 0 {
				t.Fatalf("the batch refused items: %+v", body.Failed)
			}
			// For the unclosed one the explicit outcome took the place of the default (for archive
			// that is wont_do too, so the discriminating one here is the second
			// item); for the already closed one it is a repeated close with a CHANGE.
			if got := coRead(t, env, open); got.Outcome != "wont_do" {
				t.Errorf("unclosed item: outcome %q, want wont_do", got.Outcome)
			}
			if got := coRead(t, env, closed); got.Outcome != "wont_do" {
				t.Errorf("already closed item: outcome %q, want wont_do (changed from done)", got.Outcome)
			}
		})
	}
}

// TestBulkCloseOutcome_BogusValue — a value outside the four sinks the whole batch.
func TestBulkCloseOutcome_BogusValue(t *testing.T) {
	env := coSetup(t)
	a := coSeed(t, env, env.pid, env.prefix, "b-bogus-a")
	b := coSeed(t, env, env.pid, env.prefix, "b-bogus-b")
	before := bcoStates(t, env, a, b)

	res, _ := bcoBulk(t, env, map[string]any{
		"ids": []string{a, b}, "action": "close", "closeOutcome": "bogus"})
	if res.Status != http.StatusBadRequest || coErrCode(res) != "invalid_close_outcome" {
		t.Fatalf("batch = %d/%s, want 400/invalid_close_outcome (body=%s)",
			res.Status, coErrCode(res), res.Raw)
	}
	if after := bcoStates(t, env, a, b); !bcoSameStates(before, after) {
		t.Errorf("the rejected batch applied something:\n  was  %+v\n  now %+v", before, after)
	}
}

// TestBulkCloseOutcome_NonClosingActionsRefuseTheWholeBatch — an outcome or target
// with reopen / pin / unpin.
//
// The items here are CLOSED so that reopen has something to do: on open ones it would be
// almost a no-op, and "nothing applied" would be proven by the absence of work, not
// by the refusal.
func TestBulkCloseOutcome_NonClosingActionsRefuseTheWholeBatch(t *testing.T) {
	env := coSetup(t)
	target := coSeed(t, env, env.pid, env.prefix, "b-nonclose-target")

	for i, action := range []string{"reopen", "pin", "unpin"} {
		for j, extra := range []map[string]any{
			{"closeOutcome": "done"},
			{"closeOutcome": "duplicate", "closeTarget": target},
		} {
			t.Run(action+"/"+strconv.Itoa(j), func(t *testing.T) {
				id := coSeed(t, env, env.pid, env.prefix,
					"b-nonclose"+strconv.Itoa(i)+strconv.Itoa(j))
				bcoClose(t, env, id, "done")
				before := bcoStates(t, env, id)

				body := map[string]any{"ids": []string{id}, "action": action}
				for k, v := range extra {
					body[k] = v
				}
				res, _ := bcoBulk(t, env, body)
				if res.Status != http.StatusBadRequest ||
					coErrCode(res) != "close_outcome_not_a_close" {
					t.Fatalf("%s with an outcome = %d/%s, want 400/close_outcome_not_a_close (body=%s)",
						action, res.Status, coErrCode(res), res.Raw)
				}
				if after := bcoStates(t, env, id); !bcoSameStates(before, after) {
					t.Errorf("the rejected batch applied something:\n  was  %+v\n  now %+v",
						before, after)
				}
				if edges := coEdges(t, env, id, target); len(edges) != 0 {
					t.Errorf("the rejected batch created edges: %v", edges)
				}
			})
		}
	}
}

// ---------------- an edge on every item ----------------

// TestBulkCloseOutcome_CrossProjectTargetAcceptedOnEveryPosition — a target in
// another visible project is accepted, and the edge appears on EVERY item.
func TestBulkCloseOutcome_CrossProjectTargetAcceptedOnEveryPosition(t *testing.T) {
	env := coSetup(t)
	a := coSeed(t, env, env.pid, env.prefix, "b-xp-a")
	b := coSeed(t, env, env.pid, env.prefix, "b-xp-b")
	target := coSeed(t, env, env.pidOther, env.prefixOther, "b-xp-target")

	want := []string{a + "|duplicates|" + target, b + "|duplicates|" + target}
	sort.Strings(want)

	res, body := bcoBulk(t, env, map[string]any{
		"ids": []string{a, b}, "action": "close",
		"closeOutcome": "duplicate", "closeTarget": target})
	if res.Status != http.StatusOK {
		t.Fatalf("a batch with a cross-project target = %d, want 200 (body=%s). "+
			"A refusal here is a sign of the target check via IssueBelongsToProject, "+
			"which is scoped to the batch's project and forbids US-2", res.Status, res.Raw)
	}
	if len(body.Failed) != 0 {
		t.Fatalf("the batch refused items: %+v", body.Failed)
	}
	if got := coEdges(t, env, a, b, target); len(got) != 2 ||
		got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("edges = %v, want exactly %v", got, want)
	}

	// A repeat of the same batch: still one link per item.
	res, body = bcoBulk(t, env, map[string]any{
		"ids": []string{a, b}, "action": "close",
		"closeOutcome": "duplicate", "closeTarget": target})
	if res.Status != http.StatusOK || len(body.Failed) != 0 {
		t.Fatalf("batch repeat = %d (failures %+v)", res.Status, body.Failed)
	}
	if got := coEdges(t, env, a, b, target); len(got) != 2 ||
		got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("edges after the REPEAT = %v, want still exactly %v", got, want)
	}
}

// TestBulkCloseOutcome_TargetVisibilityUsesTheSystemRole — the target visibility
// key of the REST batch is the SYSTEM role, not Actor.Role.
//
// It has its own place: the batch checks the target BEFORE the loop, in its own line
// handler/bulk.go#checkBulkCloseOutcome, not via #closeTargetCaller. One
// line is one chance to take the wrong one, and the proofs of the single surfaces
// do not carry over to it.
//
// The discriminating case and the reference are the same as in
// issues_close_outcome_test.go#TestCloseOutcome_TargetVisibilityUsesTheSystemRole:
// an administrator, a target in a project where he is not a member, the reference is taken by running a manual
// link by the same caller.
func TestBulkCloseOutcome_TargetVisibilityUsesTheSystemRole(t *testing.T) {
	env := coSetup(t)
	target := coSeed(t, env, env.pidHidden, env.prefixHidden, "b-adm-target")

	coAdminWritesInPid(t, env)
	scratch := coSeed(t, env, env.pid, env.prefix, "b-adm-scratch")
	if manual := coDepCreateAs(t, env, env.admin, "admin", scratch, target); manual.Status != http.StatusCreated &&
		manual.Status != http.StatusOK {
		t.Fatalf("the fixture does not reach the discriminating case: the admin's manual link "+
			"to a target outside his membership = %d/%s (body=%s)",
			manual.Status, coErrCode(manual), manual.Raw)
	}

	a := coSeed(t, env, env.pid, env.prefix, "b-adm-a")
	res, body := bcoBulkAs(t, env, env.admin, "admin", map[string]any{
		"ids": []string{a}, "action": "close",
		"closeOutcome": "duplicate", "closeTarget": target})
	if res.Status != http.StatusOK || len(body.Failed) != 0 {
		t.Fatalf("the admin's batch is rejected (%d, failures %+v) on a target that the manual "+
			"link of the same caller ACCEPTED (body=%s).\nUS-2 requires the same refusal contract "+
			"of the close path as POST …/dependencies. The target visibility "+
			"is read not by the SYSTEM role (checkBulkCloseOutcome) but by "+
			"Actor.Role — on a non-admin these two keys are indistinguishable, here they diverge",
			res.Status, body.Failed, res.Raw)
	}
	want := a + "|duplicates|" + target
	if got := coEdges(t, env, a); len(got) != 1 || got[0] != want {
		t.Fatalf("edges of the closed issue = %v, want exactly [%s]", got, want)
	}
}

// ---------------- target refusals: the whole batch ----------------

// TestBulkCloseOutcome_TargetRefusalsAreBatchWide — the same target cells as in
// the single surfaces, but with the batch contract: a refusal of the WHOLE call before
// execution. In every case both items stay untouched.
func TestBulkCloseOutcome_TargetRefusalsAreBatchWide(t *testing.T) {
	env := coSetup(t)
	invisible := coSeed(t, env, env.pidHidden, env.prefixHidden, "b-invisible")
	missing := env.prefix + "-99999"
	scratch := coSeed(t, env, env.pid, env.prefix, "b-scratch")

	// The cell "target = the issue itself" from the common table is ABSENT here
	// on purpose: for the batch it is not batch-wide but per item, and the
	// next test is devoted to it. The other five are the same.
	for i, c := range coTargetCases("", invisible, missing) {
		if c.wantCode == "close_target_self" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			a := coSeed(t, env, env.pid, env.prefix, "b-refuse-a"+strconv.Itoa(i))
			b := coSeed(t, env, env.pid, env.prefix, "b-refuse-b"+strconv.Itoa(i))
			before := bcoStates(t, env, a, b)

			body := map[string]any{"ids": []string{a, b}, "action": "close",
				"closeTarget": c.target}
			if c.outcome != nil {
				body["closeOutcome"] = *c.outcome
			}
			res, parsed := bcoBulk(t, env, body)

			if res.Status != c.wantStatus || coErrCode(res) != c.wantCode {
				t.Fatalf("batch = %d/%s, want %d/%s (body=%s)",
					res.Status, coErrCode(res), c.wantStatus, c.wantCode, res.Raw)
			}
			// A refusal of the REQUEST, not of the items: the body is a problem, not a
			// bulkResponse with a full failed[].
			if len(parsed.Succeeded) != 0 {
				t.Errorf("the rejected batch reported successful items: %v", parsed.Succeeded)
			}
			if after := bcoStates(t, env, a, b); !bcoSameStates(before, after) {
				t.Errorf("the rejected batch applied something:\n  was  %+v\n  now %+v",
					before, after)
			}
			if edges := coEdges(t, env, a, b, c.target); len(edges) != 0 {
				t.Errorf("the rejected batch created edges: %v", edges)
			}
		})
	}

	for _, target := range []string{invisible, missing} {
		t.Run("the same code as POST …/dependencies on "+target, func(t *testing.T) {
			manual := coDepCreate(t, env, scratch, target)
			a := coSeed(t, env, env.pid, env.prefix, "b-oracle-"+target)
			batch, _ := bcoBulk(t, env, map[string]any{
				"ids": []string{a}, "action": "close",
				"closeOutcome": "duplicate", "closeTarget": target})
			if manual.Status != batch.Status || coErrCode(manual) != coErrCode(batch) {
				t.Fatalf("the batch path tightened or loosened the refusal contract of its surface:\n"+
					"  POST …/dependencies = %d/%s\n  batch with target = %d/%s",
					manual.Status, coErrCode(manual), batch.Status, coErrCode(batch))
			}
		})
	}
}

// TestBulkCloseOutcome_SelfTargetFailsOnlyThatPosition — the only target refusal
// that is PER ITEM in the batch.
//
// The target lives in the same project as the batch: otherwise the target item would fall off
// earlier on the object check (IssueBelongsToProject -> not_found) and the cell
// would stay unreached.
func TestBulkCloseOutcome_SelfTargetFailsOnlyThatPosition(t *testing.T) {
	env := coSetup(t)
	target := coSeed(t, env, env.pid, env.prefix, "b-self-target")
	other := coSeed(t, env, env.pid, env.prefix, "b-self-other")

	res, body := bcoBulk(t, env, map[string]any{
		"ids": []string{other, target}, "action": "close",
		"closeOutcome": "superseded", "closeTarget": target})

	if res.Status != http.StatusMultiStatus {
		t.Fatalf("batch = %d, want 207: the self-target refusal must be an ITEM refusal, "+
			"not of the whole call (body=%s)", res.Status, res.Raw)
	}
	if len(body.Succeeded) != 1 || body.Succeeded[0] != other {
		t.Fatalf("succeeded items = %v, want [%s]: the refusal of one item has no "+
			"right to cancel the others", body.Succeeded, other)
	}
	if len(body.Failed) != 1 || body.Failed[0].ID != target ||
		body.Failed[0].Code != "close_target_self" {
		t.Fatalf("failed items = %+v, want exactly one: %s / close_target_self",
			body.Failed, target)
	}
	if got := coRead(t, env, target); got.Status != "open" || got.Outcome != "" {
		t.Errorf("the target item changed: %+v, want open with no outcome", got)
	}
	want := target + "|supersedes|" + other
	if got := coEdges(t, env, other, target); len(got) != 1 || got[0] != want {
		t.Fatalf("edges = %v, want exactly [%s]", got, want)
	}
}

// TestBulkCloseOutcome_TargetRequiredIsPerRow — sk-540.2 on the REST batch.
//
// "duplicate/superseded need closeTarget" has two exceptions that depend on
// the ROW, so the preflight must not refuse the whole batch; each row is
// judged by the writer:
//   - already closed (done) WITH a `duplicates` edge from it → (a), passes;
//   - historical duplicate without any edge, re-closed with the same
//     outcome → (b), passes;
//   - already closed (done) WITHOUT an edge → refused, that row only;
//   - open → a transition, refused, that row only.
func TestBulkCloseOutcome_TargetRequiredIsPerRow(t *testing.T) {
	env := coSetup(t)
	original := coSeed(t, env, env.pid, env.prefix, "b-treq-original")

	withEdge := coSeed(t, env, env.pid, env.prefix, "b-treq-with-edge")
	bcoClose(t, env, withEdge, "done")
	if _, err := env.pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'duplicates', '{}'::jsonb, 1, $3, now())`, withEdge, original, env.user); err != nil {
		t.Fatalf("seed duplicates edge: %v", err)
	}

	historical := coSeed(t, env, env.pid, env.prefix, "b-treq-historical")
	bcoClose(t, env, historical, "done")
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE issues SET close_outcome = 'duplicate' WHERE id = $1`, historical); err != nil {
		t.Fatalf("seed historical duplicate: %v", err)
	}

	noEdge := coSeed(t, env, env.pid, env.prefix, "b-treq-no-edge")
	bcoClose(t, env, noEdge, "done")
	open := coSeed(t, env, env.pid, env.prefix, "b-treq-open")

	before := bcoStates(t, env, noEdge, open)
	res, body := bcoBulk(t, env, map[string]any{
		"ids":    []string{withEdge, historical, noEdge, open},
		"action": "close", "closeOutcome": "duplicate"})

	if res.Status != http.StatusMultiStatus {
		t.Fatalf("batch = %d, want 207 (body=%s)", res.Status, res.Raw)
	}
	succeeded := append([]string(nil), body.Succeeded...)
	sort.Strings(succeeded)
	wantOK := []string{withEdge, historical}
	sort.Strings(wantOK)
	if len(succeeded) != 2 || succeeded[0] != wantOK[0] || succeeded[1] != wantOK[1] {
		t.Fatalf("succeeded = %v, want %v", body.Succeeded, wantOK)
	}
	failed := map[string]string{}
	for _, f := range body.Failed {
		failed[f.ID] = f.Code
	}
	for _, id := range []string{noEdge, open} {
		if failed[id] != "close_target_required" {
			t.Errorf("row %s failure code = %q, want close_target_required (failed=%+v)", id, failed[id], body.Failed)
		}
	}
	if got := coRead(t, env, withEdge); got.Outcome != "duplicate" {
		t.Errorf("row with edge: outcome %q, want duplicate", got.Outcome)
	}
	if got := coRead(t, env, historical); got.Outcome != "duplicate" {
		t.Errorf("historical row: outcome %q, want duplicate", got.Outcome)
	}
	if after := bcoStates(t, env, noEdge, open); !bcoSameStates(before, after) {
		t.Errorf("refused rows changed: before %+v, after %+v", before, after)
	}
	if got := coEdges(t, env, withEdge, original); len(got) != 1 {
		t.Errorf("edges between %s and %s = %v, want exactly the seeded one", withEdge, original, got)
	}
}
