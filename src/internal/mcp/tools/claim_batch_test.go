package tools

import (
	"context"
	"reflect"
	"sync"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

func batchClaimRequest(ids ...string) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	values := make([]any, len(ids))
	for i, id := range ids {
		values[i] = id
	}
	req.Params.Arguments = map[string]any{"ids": values}
	return req
}

func TestClaimMany_SuccessPreservesInputOrder(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clbatch")
	first, second := prefix+"-1", prefix+"-2"
	seedIssue(t, pool, pid, userID, first, "first", "open", 2)
	seedIssue(t, pool, pid, userID, second, "second", "deferred", 2)

	res, err := handleClaim(pool, issues.NewStore(pool))(
		withCtxSession(t, userID, "admin", "batch-session"), batchClaimRequest(second, first))
	if err != nil || res.IsError {
		t.Fatalf("batch claim failed: err=%v body=%v", err, extractToolJSON(t, res))
	}
	// Matrix row 84 (sk-282 T023): the batch answers in the reference
	// group form now — {results, summary} — instead of a bare array, and
	// each row is an acknowledgement, not an issue card.
	rows := batchAckRows(t, res, 2)
	if rows[0]["id"] != second || rows[1]["id"] != first {
		t.Fatalf("response order changed: %#v", rows)
	}
	for i, row := range rows {
		assertAcknowledgementShape(t, row)
		if row["status"] != "in_progress" {
			t.Errorf("results[%d].status = %v, want in_progress", i, row["status"])
		}
		if row["lockedBy"] != "batch-session" {
			t.Errorf("results[%d].lockedBy = %v, want batch-session — the claim stamp is "+
				"what proves the write applied", i, row["lockedBy"])
		}
		if _, present := row["startedAt"]; !present {
			t.Errorf("results[%d] has no startedAt", i)
		}
	}
	for _, id := range []string{first, second} {
		var status, lockedBy string
		if err := pool.QueryRow(context.Background(), `SELECT status, locked_by FROM issues WHERE id=$1`, id).Scan(&status, &lockedBy); err != nil {
			t.Fatal(err)
		}
		if status != "in_progress" || lockedBy != "batch-session" {
			t.Fatalf("%s status=%s lockedBy=%s", id, status, lockedBy)
		}
	}
}

// TestClaimMany_LockHolderNameOnEveryIssue is sk-421.1 test-plan row 14,
// batch half: ClaimMany goes through claimInTx per issue and reads back via
// getIssuesByIDs, and every claimed issue names the user who performed the
// batch claim.
func TestClaimMany_LockHolderNameOnEveryIssue(t *testing.T) {
	pool := newTestPool(t)
	u, uName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, u, "clbhn")
	first, second := prefix+"-1", prefix+"-2"
	seedIssue(t, pool, pid, u, first, "first", "open", 2)
	seedIssue(t, pool, pid, u, second, "second", "deferred", 2)
	store := issues.NewStore(pool)

	res, err := handleClaim(pool, store)(withCtxSession(t, u, "user", "batch-holder"), batchClaimRequest(first, second))
	if err != nil || res.IsError {
		t.Fatalf("batch claim failed: err=%v body=%v", err, extractToolJSON(t, res))
	}
	for _, id := range []string{first, second} {
		assertLockHolder(t, store, u, id, uName, "batch claim of "+id)
	}
}

// TestClaimMany_VerboseDetailsCarryRedactionAndHolderName is sk-421.1
// test-plan row 9 (b): the batch path (Store.ClaimMany → getIssuesByIDs)
// hydrates every detail through the same assembly as the single path. A has
// a blocker from Q (for this reader only an unavailable link, sk-423), B
// only one from P (full);
// isBlocked matches the list on both, and both name the performer.
func TestClaimMany_VerboseDetailsCarryRedactionAndHolderName(t *testing.T) {
	f := newVerboseDetailFixture(t)
	req := batchClaimRequest(f.a, f.b)
	req.Params.Arguments.(map[string]any)["verbose"] = true
	res, err := handleClaim(f.pool, f.store)(withCtxSession(t, f.reader, "user", "sess-row9b"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("verbose batch claim failed: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	results, ok := body["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results = %#v, want two full details", body["results"])
	}
	for _, item := range results {
		detail, _ := item.(map[string]any)
		switch detail["id"] {
		case f.a:
			f.assertVerboseDetail(t, detail, f.aPBlocker, f.aQBlocker)
		case f.b:
			f.assertVerboseDetail(t, detail, f.bPBlocker, "")
		default:
			t.Errorf("unexpected result row %v", detail["id"])
		}
	}
}

func TestClaimMany_ConflictRollsBackEntireBatch(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clrollback")
	free, taken := prefix+"-1", prefix+"-2"
	seedIssue(t, pool, pid, userID, free, "free", "open", 2)
	seedIssue(t, pool, pid, userID, taken, "taken", "open", 2)
	handler := handleClaim(pool, issues.NewStore(pool))
	if res, err := handler(withCtxSession(t, userID, "admin", "owner"), singleClaimRequest(taken)); err != nil || res.IsError {
		t.Fatalf("seed claim failed: %v", err)
	}

	res, err := handler(withCtxSession(t, userID, "admin", "contender"), batchClaimRequest(free, taken))
	if err != nil || !res.IsError {
		t.Fatalf("want conflict: err=%v res=%v", err, res)
	}
	if body := extractToolJSON(t, res); body["error"] != ErrCodeAlreadyClaimed || body["id"] != taken || body["lockedBy"] != "owner" {
		t.Fatalf("wrong conflict detail: %#v", body)
	}
	var status string
	var lockedBy *string
	if err := pool.QueryRow(context.Background(), `SELECT status, locked_by FROM issues WHERE id=$1`, free).Scan(&status, &lockedBy); err != nil {
		t.Fatal(err)
	}
	if status != "open" || lockedBy != nil {
		t.Fatalf("free member partially mutated: status=%s lock=%v", status, lockedBy)
	}
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE entity_id=$1 AND type='issue.claimed'`, free).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("rolled-back member has %d claim events", events)
	}
}

func TestClaimMany_TerminalMemberRollsBackEntireBatch(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		wantError string
	}{
		{name: "closed", status: "closed", wantError: ErrCodeCannotClaimClosed},
		{name: "standing", status: "standing", wantError: ErrCodeCannotClaimStanding},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "admin")
			pid, prefix := seedProject(t, pool, userID, "clterminal"+tt.name)
			free, rejected := prefix+"-1", prefix+"-2"
			seedIssue(t, pool, pid, userID, free, "free", "open", 2)
			seedIssue(t, pool, pid, userID, rejected, "rejected", tt.status, 2)

			res, err := handleClaim(pool, issues.NewStore(pool))(
				withCtxSession(t, userID, "admin", "terminal-session"), batchClaimRequest(free, rejected))
			if err != nil || !res.IsError {
				t.Fatalf("want rejection: err=%v res=%v", err, res)
			}
			body := extractToolJSON(t, res)
			if body["error"] != tt.wantError || body["id"] != rejected {
				t.Fatalf("wrong rejection: %#v", body)
			}
			var status string
			var lockedBy *string
			if err := pool.QueryRow(context.Background(), `SELECT status, locked_by FROM issues WHERE id=$1`, free).Scan(&status, &lockedBy); err != nil {
				t.Fatal(err)
			}
			if status != "open" || lockedBy != nil {
				t.Fatalf("free member partially mutated: status=%s lock=%v", status, lockedBy)
			}
		})
	}
}

func TestClaimMany_StoreRejectsEmptyIDs(t *testing.T) {
	pool := newTestPool(t)
	userID, username := seedUser(t, pool, "admin")
	_, err := issues.NewStore(pool).ClaimMany(t.Context(), issues.ClaimManyInput{
		SessionID: "empty-batch",
		Actor:     issues.Actor{UserID: userID, Username: username, Role: "admin"},
	})
	if vErr, ok := issues.IsValidationError(err); !ok || vErr.Code != "empty_claim_batch" {
		t.Fatalf("error=%v, want empty_claim_batch validation", err)
	}
}

func TestClaimMany_StoreRejectsDuplicateIDsWithoutMutation(t *testing.T) {
	pool := newTestPool(t)
	userID, username := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clstoredup")
	id := prefix + "-1"
	seedIssue(t, pool, pid, userID, id, "duplicate", "open", 2)

	got, err := issues.NewStore(pool).ClaimMany(t.Context(), issues.ClaimManyInput{
		IDs:       []string{id, id},
		SessionID: "duplicate-batch",
		Actor:     issues.Actor{UserID: userID, Username: username, Role: "admin"},
	})
	if got != nil {
		t.Fatalf("result=%#v, want nil (no alias DTOs)", got)
	}
	if vErr, ok := issues.IsValidationError(err); !ok || vErr.Code != "duplicate_claim_id" {
		t.Fatalf("error=%v, want duplicate_claim_id validation", err)
	}

	var status string
	var lockedBy *string
	var version int
	if err := pool.QueryRow(t.Context(), `SELECT status, locked_by, version FROM issues WHERE id=$1`, id).Scan(&status, &lockedBy, &version); err != nil {
		t.Fatal(err)
	}
	if status != "open" || lockedBy != nil || version != 1 {
		t.Fatalf("duplicate input mutated row: status=%s lockedBy=%v version=%d", status, lockedBy, version)
	}
	var events int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE entity_id=$1 AND type='issue.claimed'`, id).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("duplicate input wrote %d claim events", events)
	}
}

func TestClaimMany_SameSessionIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clidbatch")
	ids := []string{prefix + "-1", prefix + "-2"}
	for _, id := range ids {
		seedIssue(t, pool, pid, userID, id, id, "open", 2)
	}
	handler := handleClaim(pool, issues.NewStore(pool))
	ctx := withCtxSession(t, userID, "admin", "same-session")
	for range 2 {
		if res, err := handler(ctx, batchClaimRequest(ids...)); err != nil || res.IsError {
			t.Fatalf("claim failed: %v", err)
		}
	}
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE entity_id=ANY($1) AND type='issue.claimed'`, ids).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("claim events=%d, want 2", events)
	}
}

func TestClaimMany_OverlappingBatchesAreAtomic(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clracebatch")
	a, b, c := prefix+"-1", prefix+"-2", prefix+"-3"
	for _, id := range []string{a, b, c} {
		seedIssue(t, pool, pid, userID, id, id, "open", 2)
	}
	handler := handleClaim(pool, issues.NewStore(pool))
	type outcome struct {
		failed bool
		err    error
	}
	out := make(chan outcome, 2)
	var start sync.WaitGroup
	start.Add(1)
	for i, ids := range [][]string{{a, b}, {c, b}} {
		go func(i int, ids []string) {
			start.Wait()
			res, err := handler(withCtxSession(t, userID, "admin", []string{"race-a", "race-b"}[i]), batchClaimRequest(ids...))
			out <- outcome{failed: res != nil && res.IsError, err: err}
		}(i, ids)
	}
	start.Done()
	first, second := <-out, <-out
	if first.err != nil || second.err != nil {
		t.Fatalf("handler errors: %v %v", first.err, second.err)
	}
	successes := 0
	for _, got := range []outcome{first, second} {
		if !got.failed {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes=%d outcomes=%#v %#v", successes, first, second)
	}
	var claimed int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM issues WHERE id=ANY($1) AND locked_by IS NOT NULL`, []string{a, b, c}).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if claimed != 2 {
		t.Fatalf("claimed rows=%d, want exactly one complete batch (2)", claimed)
	}
}

func TestParseClaimIDs_XORValidation(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "neither", args: map[string]any{}},
		{name: "both", args: map[string]any{"id": "sk-1", "ids": []any{"sk-2"}}},
		{name: "empty batch", args: map[string]any{"ids": []any{}}},
		{name: "duplicate", args: map[string]any{"ids": []any{"sk-1", "sk-1"}}},
		{name: "invalid member", args: map[string]any{"ids": []any{"42"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, errResult := parseClaimIDs(tt.args); errResult == nil || !errResult.IsError {
				t.Fatal("expected validation tool error")
			}
		})
	}

	ids, batch, errResult := parseClaimIDs(map[string]any{"ids": []any{"sk-2", "sk-1"}})
	if errResult != nil || !batch || len(ids) != 2 || ids[0] != "sk-2" || ids[1] != "sk-1" {
		t.Fatalf("valid batch parsed incorrectly: ids=%v batch=%v error=%v", ids, batch, errResult)
	}
}

func singleClaimRequest(id string) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": id}
	return req
}

// batchAckRows decodes a group acknowledgement and checks the envelope
// US-5 calls the reference form, returning the rows in caller order.
//
// The envelope's key set is derived from the same structs the group close
// and update serialise, so «the same form as skein_close_many» is
// asserted rather than described.
func batchAckRows(t *testing.T, res *mcpgo.CallToolResult, wantRows int) []map[string]any {
	t.Helper()
	body := extractToolJSON(t, res)

	envelope := jsonTagNames(t, reflect.TypeOf(ackBatchResponse{}))
	for k := range body {
		if !envelope[k] {
			t.Errorf("group acknowledgement carries an undeclared top-level key %q", k)
		}
	}
	raw, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("results missing or not an array: %+v", body)
	}
	if len(raw) != wantRows {
		t.Fatalf("results len = %d, want one row per input id (%d)", len(raw), wantRows)
	}
	rows := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("results[%d] is %T, want an object", i, item)
		}
		rows = append(rows, row)
	}

	summary, ok := body["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing or not an object: %+v", body)
	}
	for key, want := range map[string]int{
		"requested": wantRows,
		"succeeded": wantRows,
		// FP-10: a group claim is all-or-nothing, so a SUCCESSFUL
		// response can never report a failure. A non-zero `failed` here
		// would mean the batch had started reporting partial success —
		// the exact reading US-5 forbids.
		"failed": 0,
	} {
		got, isNum := summary[key].(float64)
		if !isNum || int(got) != want {
			t.Errorf("summary.%s = %#v, want %d", key, summary[key], want)
		}
	}
	return rows
}

// TestClaimMany_BlockedMemberProducesNoSummaryOfZeros is matrix row 85.
//
// "FP-10 / Edge: when an issue locked by another session is in the
// group, none is claimed; a per-row summary of zeros does not appear."
//
// The second clause is the one this run makes newly falsifiable, and it
// is the reason the test exists. Giving the batch the {results, summary}
// envelope makes «0 succeeded, N failed» a shape the code could now
// express — and expressing it would silently convert a group claim from
// all-or-nothing into partial success. So the assertion is not merely
// «an error came back»: it is that the failure response is NOT the group
// envelope at all.
func TestClaimMany_BlockedMemberProducesNoSummaryOfZeros(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "fp10")
	free, taken := prefix+"-1", prefix+"-2"
	seedIssue(t, pool, pid, userID, free, "free", "open", 2)
	seedIssue(t, pool, pid, userID, taken, "taken by another session", "open", 2)

	handler := handleClaim(pool, issues.NewStore(pool))
	if res, err := handler(withCtxSession(t, userID, "admin", "other-session"),
		singleClaimRequest(taken)); err != nil || res.IsError {
		t.Fatalf("seed claim failed: err=%v res=%+v", err, res)
	}

	// The blocked issue is deliberately NOT first: a batch that gave up
	// on the first row would leave the same database state, and the test
	// must fail if the rollback of an already-applied row stops working.
	res, err := handler(withCtxSession(t, userID, "admin", "contender"),
		batchClaimRequest(free, taken))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("batch with a blocked member succeeded: %s", toolText(t, res))
	}

	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeAlreadyClaimed {
		t.Errorf("error = %v, want %s — the previous failure contract is unchanged",
			body["error"], ErrCodeAlreadyClaimed)
	}
	if body["id"] != taken {
		t.Errorf("error names %v, want the blocked issue %s", body["id"], taken)
	}
	// No group envelope, and therefore no all-zero summary.
	for _, key := range []string{"results", "summary"} {
		if v, present := body[key]; present {
			t.Errorf("the failure response carries %q = %v — a group claim that fails is a "+
				"tool error with nothing written, not a per-row report of zeros", key, v)
		}
	}

	// And nothing was claimed: the free issue is untouched, the taken one
	// still belongs to its original holder.
	var status string
	var lockedBy *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, locked_by FROM issues WHERE id = $1`, free).Scan(&status, &lockedBy); err != nil {
		t.Fatalf("read %s: %v", free, err)
	}
	if status != "open" || lockedBy != nil {
		t.Errorf("%s status=%s lockedBy=%v — the batch left a lock behind", free, status, lockedBy)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(locked_by, '') FROM issues WHERE id = $1`, taken).Scan(&status); err != nil {
		t.Fatalf("read %s: %v", taken, err)
	}
	if status != "other-session" {
		t.Errorf("%s lockedBy = %q, want the original holder", taken, status)
	}
}
