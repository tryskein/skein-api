package tools

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestClose_LockHolderNameGoesWithTheLock is sk-421.1 test-plan row 15
// (US-15, SC-005), close half: the holder closes its claimed issue and the
// lock holder's name disappears with the lock. The name is asserted before
// the close, so the absence afterwards is not vacuous.
func TestClose_LockHolderNameGoesWithTheLock(t *testing.T) {
	pool := newTestPool(t)
	u, uName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, u, "clslhn")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, u, issueID, "held then closed", "open", 2)
	store := issues.NewStore(pool)
	ctx := withCtxSession(t, u, "user", "sess-U")

	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := handleClaim(pool, store)(ctx, cl); err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}
	assertLockHolder(t, store, u, issueID, uName, "before close")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{"id": issueID, "expectedVersion": 2, "closeOutcome": "done", "reason": "done"}
	if r, err := handleClose(pool, store)(ctx, clo); err != nil || r.IsError {
		t.Fatalf("close failed: %v %+v", err, r)
	}
	assertLockHolder(t, store, u, issueID, "", "after the holder closed")
}

// TestClose_HappyPathLockHolder: lock holder closes → status=closed,
// locked_by cleared, closed_reason set, version+1, two events
// (issue.closed + issue.unclaimed).
func TestClose_HappyPathLockHolder(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clshp")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "closing", "open", 2)

	store := issues.NewStore(pool)
	claimHandler := handleClaim(pool, store)
	closeHandler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-mine")

	// Claim first → version goes 1→2.
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctx, cl); err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"closeOutcome":    "done",
		"reason":          "done",
	}
	res, err := closeHandler(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if body["status"] != "closed" {
		t.Fatalf("status = %v, want closed", body["status"])
	}
	if body["lockedBy"] != nil {
		t.Fatalf("lockedBy = %v, want nil after close", body["lockedBy"])
	}
	// sk-282 T021: the close acknowledges — the reason the caller just
	// sent is NOT echoed back. It is still in the system; that is
	// asserted below by reading the row.
	if _, echoed := body["closedReason"]; echoed {
		t.Fatalf("closedReason echoed = %v — US-5 forbids it", body["closedReason"])
	}
	if v, _ := body["version"].(float64); int(v) != 3 {
		t.Fatalf("version = %v, want 3", body["version"])
	}
	var storedReason string
	if err := pool.QueryRow(context.Background(),
		`SELECT closed_reason FROM issues WHERE id = $1`, issueID).Scan(&storedReason); err != nil {
		t.Fatalf("read back closed_reason: %v", err)
	}
	if storedReason != "done" {
		t.Fatalf("stored closed_reason = %q, want \"done\" (INV-3: the cut is on the wire)",
			storedReason)
	}

	// Two events: issue.closed AND issue.unclaimed (since the issue was locked).
	var nClosed, nUnclaimed int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.closed'`,
		issueID).Scan(&nClosed); err != nil {
		t.Fatalf("query closed events: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.unclaimed'`,
		issueID).Scan(&nUnclaimed); err != nil {
		t.Fatalf("query unclaimed events: %v", err)
	}
	if nClosed != 1 {
		t.Fatalf("expected 1 issue.closed event, got %d", nClosed)
	}
	if nUnclaimed != 1 {
		t.Fatalf("expected 1 issue.unclaimed event, got %d", nUnclaimed)
	}
}

// TestClose_UnlockedIssue: close an issue that was never locked →
// only issue.closed event (no issue.unclaimed).
func TestClose_UnlockedIssue(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clsuc")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "free-close", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-anon")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "wontfix",
	}
	res, err := handler(ctx, clo)
	if err != nil || res.IsError {
		t.Fatalf("close failed: err=%v body=%+v", err,
			func() any {
				if res == nil {
					return nil
				}
				return extractToolJSON(t, res)
			}())
	}

	var nClosed, nUnclaimed int
	_ = pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.closed'`,
		issueID).Scan(&nClosed)
	_ = pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.unclaimed'`,
		issueID).Scan(&nUnclaimed)
	if nClosed != 1 {
		t.Fatalf("expected 1 issue.closed event, got %d", nClosed)
	}
	if nUnclaimed != 0 {
		t.Fatalf("expected 0 issue.unclaimed events on unlocked close, got %d", nUnclaimed)
	}
}

// TestClose_VersionConflict: stale expectedVersion → version_conflict
// with currentVersion echoed.
func TestClose_VersionConflict(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clsvc")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "vc", "open", 2)
	// Bump version once via direct SQL — easier than threading a second
	// tool call.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET version=2, updated_at=now() WHERE id=$1`, issueID); err != nil {
		t.Fatalf("bump: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-vc")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1, // stale
		"closeOutcome":    "done",
		"reason":          "ok",
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on stale version")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeVersionConflict {
		t.Fatalf("error = %v, want %s", got, ErrCodeVersionConflict)
	}
	if cv, _ := body["currentVersion"].(float64); int(cv) != 2 {
		t.Fatalf("currentVersion = %v, want 2", body["currentVersion"])
	}
}

// TestClose_LockedByOther: locked by another session → locked_by_other
// (without force).
func TestClose_LockedByOther(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clslbo")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "owned", "open", 2)

	store := issues.NewStore(pool)
	claimHandler := handleClaim(pool, store)
	closeHandler := handleClose(pool, store)

	ctxA := withCtxSession(t, userID, "admin", "sess-A")
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctxA, cl); err != nil || r.IsError {
		t.Fatalf("seed claim: %v %+v", err, r)
	}

	ctxB := withCtxSession(t, userID, "admin", "sess-B")
	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"closeOutcome":    "done",
		"reason":          "trying",
	}
	res, err := closeHandler(ctxB, clo)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for locked-by-other close")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeLockedByOther {
		t.Fatalf("error = %v, want %s", got, ErrCodeLockedByOther)
	}
}

// TestClose_PinnedNoForce: pinned issue without force → pinned_no_force.
func TestClose_PinnedNoForce(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clspnf")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "pinned", "open", 2)
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET pinned=true WHERE id=$1`, issueID); err != nil {
		t.Fatalf("pin: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-pin")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "trying",
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for pinned without force")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodePinnedNoForce {
		t.Fatalf("error = %v, want %s", got, ErrCodePinnedNoForce)
	}
}

// TestClose_PinnedAdminForce: pinned issue + admin force=true → success.
func TestClose_PinnedAdminForce(t *testing.T) {
	pool := newTestPool(t)
	admin, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, admin, "clspaf")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, admin, issueID, "pin-admin", "open", 2)
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET pinned=true WHERE id=$1`, issueID); err != nil {
		t.Fatalf("pin: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, admin, "admin", "sess-admin")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "force-close-pinned",
		"force":           true,
	}
	res, err := handler(ctx, clo)
	if err != nil || res.IsError {
		t.Fatalf("admin force close on pinned failed: %v %+v", err, res)
	}
}

// TestClose_EmptyReason: missing or whitespace-only reason → validation_error.
func TestClose_EmptyReason(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clser")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "no-reason", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-er")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "   ",
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for empty reason")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestClose_AdminForceOverridesVersionConflict: admin force=true ignores
// version conflict and writes audit event with reason="force-overridden".
func TestClose_AdminForceOverridesVersionConflict(t *testing.T) {
	pool := newTestPool(t)
	admin, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, admin, "clsfvc")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, admin, issueID, "stale", "open", 2)
	// Bump version so caller's expected=1 is stale.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET version=2, updated_at=now() WHERE id=$1`, issueID); err != nil {
		t.Fatalf("bump: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleClose(pool, store)
	ctx := withCtxSession(t, admin, "admin", "sess-force")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1, // stale, but force=true
		"closeOutcome":    "done",
		"reason":          "stale-but-forced",
		"force":           true,
	}
	res, err := handler(ctx, clo)
	if err != nil || res.IsError {
		t.Fatalf("admin force vc-bypass failed: %v %+v", err, res)
	}

	var reason string
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(reason, '') FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.closed'
 ORDER BY id DESC LIMIT 1`,
		issueID).Scan(&reason); err != nil {
		t.Fatalf("query event reason: %v", err)
	}
	// The event reason should mention "force-overridden" — we appended it
	// to the user-provided reason.
	if !strings.Contains(reason, "force-overridden") {
		t.Fatalf("event reason = %q, want substring \"force-overridden\"", reason)
	}
}

// TestClose_OwnerForceAllowed — row 187 (sk-674.18, US-16): the project
// owner, who is no instance admin, now holds force_unlock and closes past
// another holder's claim.
func TestClose_OwnerForceAllowed(t *testing.T) {
	f := newForceFixture(t, "clsown")
	handler := handleClose(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.owner, "user", "sess-owner")
	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id": f.issueID, "expectedVersion": 1, "closeOutcome": "done", "reason": "x", "force": true,
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("the project owner's forced close was refused: %+v", extractToolJSON(t, res))
	}
	f.assertReleased(t)
}

// TestClose_MemberForceForbidden — closing past another holder's claim
// needs force_unlock (US-16, K-28): a member gets force_unlock_forbidden.
func TestClose_MemberForceForbidden(t *testing.T) {
	f := newForceFixture(t, "clsnaf")
	handler := handleClose(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.member, "user", "sess-naf")

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              f.issueID,
		"expectedVersion": 1,
		"closeOutcome":    "done",
		"reason":          "x",
		"force":           true,
	}
	res, err := handler(ctx, clo)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for a member's force")
	}
	f.assertForceRefused(t, extractToolJSON(t, res)["error"])
}

// sc006HostileProjectPrefix is the project id_prefix the SC-006 fixture
// runs on: the LONGEST one the schema accepts.
//
// The DB CHECK and projects.ValidateIDPrefix agree on
// `^[a-z0-9][a-z0-9_-]{0,49}$` — fifty characters — and seedProject appends
// six random characters of its own, so the literal is 44. Every issue id in
// such a project carries all fifty. This is not a pathological value
// invented for the test; it is the ceiling the product sells, and an
// acceptance number that only holds for short prefixes is a number about
// the fixture rather than about the tool.
const sc006HostileProjectPrefix = "sc006-a-deliberately-long-but-legal-project-"

// sc006BudgetChars is SC-006's threshold, and it is measured on the
// acknowledgement's SERVICE PART — everything except the issue id and the
// assignee handle.
//
// # The shift rule, and why it is not a concession
//
// This is the rule the spec already states for SC-002, applied to the
// criterion that forgot it: "The title is not in the budget: when the
// title is longer than the reference 60 characters the threshold shifts
// by exactly the difference. The wording is that way because a title is
// allowed up to 1000 characters and is not truncatable — a threshold
// that depends on user text can be neither met nor honestly failed". internal/mcp/tools/
// issuerow_test.go implements exactly that for the list row, and extends
// it to the project identifier in cross-project mode.
//
// The issue id and the assignee handle are the same kind of text. Neither
// is truncatable — an acknowledgement that shortened the id it confirms
// would be worthless — and neither length is ours: a project prefix is
// legal up to 50 characters, ids nest arbitrarily deep, and `assignee` is
// documented as «username, email, or user UUID — stored verbatim». On the
// hostile fixture below those two fields alone are 98 of the 200, so a
// threshold that counted them would be decided by the fixture's project
// name rather than by the response's composition.
//
// What the budget therefore guards is what the tool actually chooses: the
// key names, the stamps, and the fields US-5's required column names. A
// regression that puts a stamp or a prose field back on the row moves this
// number; a customer with a long project prefix does not.
//
// # sk-489 (US-9): two new keys, and why the number did NOT move
//
// The close acknowledgement now also carries `closeOutcome` and, when the
// call named a target, `closeLink`. The plan authorised raising the budget
// by exactly their worst-case cost — `,"closeOutcome":"superseded"` (28
// characters) plus `,"closeLink":"existed"` (22) — "deliberately, with a
// comment", if they did not fit.
//
// They fit, so the number stays at 200 and this paragraph records the
// MEASUREMENT rather than a decision. The fixture below is hostile on both
// new axes at once (the LONGEST outcome value AND a target whose edge
// already exists, so the longer of the two link states is reported), and
// its service part came out at 198 — logged by the test on every run.
// Raising a budget that is not exceeded would retire the gate quietly: at
// the authorised 250 even a re-added timestamp stamp (42 characters) would
// land inside the slack, which is the one regression this gate exists for.
//
// ⚠ THE MARGIN IS NOW TWO CHARACTERS, and the consequence is named here
// rather than discovered later. One axis of the response is NOT controlled
// by the fixture: the two timestamps render RFC3339 in the DATABASE
// SESSION's time zone, and `…+03:00` costs 5 characters more per stamp than
// `…Z` — 10 for the pair. Under the previous 31-character margin that
// variance was covered; under 2 it is not, so this gate ASSUMES a UTC
// database session.
//
// An assumption a test depends on is asserted by the test, not left in
// prose (sk-507, decided in review; the budget stays 200). The
// assertion is #assertSC006StampsAreUTC below, and it fires BEFORE the
// budget comparison. Without it a run against a non-UTC server reads ~208
// and fails as "budget exceeded" — sending the next reader to hunt for
// extra characters in the acknowledgement instead of fixing the session's
// time zone. With it the failure names the environment. In neither case is
// the answer to raise the budget: raised, the number stops describing the
// response.
//
// If a genuine composition change does exceed it, the authorised move is
// the one above — prior value plus the two literals' lengths — and it is
// recorded here, not in the assertion.
const sc006BudgetChars = 200

// TestClose_SC006_AcknowledgementFitsTheBudget is matrix row 76.
//
// "SC-006: a close without warnings is under 200 characters and does
// not contain the submitted reason."
//
// # The fixture is hostile on EVERY axis, not just the convenient ones
//
// The first version of this test was hostile only about free text: long
// reason, every prose field populated, claimed first so the lock / claim
// stamps are present. It passed at 176–187 characters — and the review
// pointed out that the two axes it left at their MINIMA are the two
// that decide the number. This is the third fixture of this run caught the
// same way (SC-005 was the second), so the fixture now takes
// every axis of the acknowledgement to the largest value the schema
// actually allows:
//
//   - `id` — a project prefix at the 50-character maximum plus a
//     four-level hierarchical suffix, the shape this very change's own ids
//     have (sk-282.18.4.12). 62 characters against the 13 of the old
//     fixture.
//   - `assignee` — a user UUID. skein_create documents the field as
//     «username, email, or user UUID — stored verbatim, resolved on read»,
//     the column has no CHECK, and a UUID is 36 characters against the 18
//     of a generated test username. (An e-mail handle is longer still and
//     equally legal — this axis has no true maximum.)
//   - `version` — three digits. Nothing caps it, and a much-edited issue
//     reaches them; two characters more than the old fixture's `3`.
//   - the prose axes, kept from the original fixture: long close reason,
//     every free-text field populated with a sentinel.
//   - `closeOutcome` (sk-489) — `superseded`, the LONGEST of the four
//     values. The default `done` is the shortest, and a fixture left at
//     the default would measure the cheapest possible answer on the axis
//     the change just added.
//   - `closeLink` (sk-489) — present, and reporting `existed`, which is
//     one character longer than `created`. Present costs more than absent,
//     and the edge is pre-seeded so the longer state is the one reported:
//     both halves of that axis are at their maximum, not just the first.
//
// This is the fourth fixture of this programme caught leaving a new axis
// at its minimum (SC-005, the id and assignee axes here). The rule that keeps being relearned: a budget measured on a fixture
// that does not exercise the newest axis is a budget about the old
// response.
//
// One axis CANNOT be maximised here, and the reason is worth stating so
// the next reader does not "fix" it: `status` on this path is always
// `closed` (6 characters). The longest status in the system is
// `in_progress` (11), but an acknowledgement that reported it would mean
// the close had not applied.
//
// The remaining axis is environmental rather than fixture-controlled: the
// two timestamps render as RFC3339 in the DATABASE SESSION's time zone, so
// `…+03:00` costs 5 characters more per stamp than `…Z`. Both numbers are
// logged below; a run on a UTC server reads 10 characters lower. With a
// two-character margin that is no longer slack, so the UTC assumption is
// ASSERTED (#assertSC006StampsAreUTC) rather than assumed — see
// sc006BudgetChars.
//
// # What the hostile fixture is measured AGAINST
//
// The budget is on the SERVICE PART — the response minus the actual
// lengths of the id and the assignee handle — under the shift rule SC-002
// already states for the title (see sc006BudgetChars). The fixture stays
// hostile precisely because of that: with the identifiers subtracted, a
// long id can neither hide a regression in the composition nor fail the
// gate on its own. The raw total is logged beside the service part so a
// reader can still see what an agent receives.
func TestClose_SC006_AcknowledgementFitsTheBudget(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, sc006HostileProjectPrefix)
	// The project prefix is already at its 50-character ceiling; the
	// suffix is the four-level hierarchy this repo's own decomposition
	// produces.
	issueID := prefix + "-282.18.4.12"
	seedIssue(t, pool, pid, userID, issueID, "close budget probe", "open", 2)
	if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET description = $2, design = $2, acceptance_criteria = $2,
       notes = $2, source_context = $2, version = 997
 WHERE id = $1`, issueID,
		"SENTINEL-PROSE that the caller is already holding, repeated to make any echo "+
			"impossible to miss in a 200-character budget"); err != nil {
		t.Fatalf("seed prose: %v", err)
	}

	// A realistic long close reason — the field US-5 names first among
	// the forbidden echoes, and the one measured at 2 220 characters in
	// the spec's own sample.
	const reason = "Done: rewrote the token-rotation handler in middleware/auth.go, added a " +
		"regression test for the concurrent-refresh race, verified against the staging " +
		"cluster, and updated the runbook. Follow-up filed as a separate issue."

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-sc006")

	claim := mcpgo.CallToolRequest{}
	claim.Params.Arguments = map[string]any{"id": issueID}
	if r, err := handleClaim(pool, store)(ctx, claim); err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}
	// The claim writes the caller's USERNAME into assignee; replace it with
	// the UUID handle, which is what skein_update(assignee=<uuid>) leaves
	// on the row. Order is incidental since sk-294 (skein_claim resolves
	// the handle to a users.id, so a UUID-assigned issue is claimable by
	// its assignee); the UPDATE stays here because the fixture only needs
	// the longest legal handle on the row at CLOSE time.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET assignee = $2 WHERE id = $1`, issueID, userID.String()); err != nil {
		t.Fatalf("seed uuid assignee: %v", err)
	}

	// sk-489 — the close-target axis. The target lives in the SAME project
	// (its id length does not enter the budget: only the ISSUE's id and the
	// assignee handle are shifted out, and closeTarget appears nowhere in
	// the answer), and the edge is created up front so this close reports
	// `existed` rather than `created`.
	targetID := prefix + "-282.18.4.13"
	seedIssue(t, pool, pid, userID, targetID, "close budget target", "open", 2)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, created_by, created_at)
VALUES ($1, $2, 'supersedes', $3, now())`, targetID, issueID, userID); err != nil {
		t.Fatalf("seed existing close-target edge: %v", err)
	}

	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": issueVersion(t, pool, issueID),
		"reason":          reason,
		"closeOutcome":    issues.CloseOutcomeSuperseded,
		"closeTarget":     targetID,
	}
	res, err := handleClose(pool, store)(ctx, clo)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	raw := toolText(t, res)
	body := extractToolJSON(t, res)

	// This is the no-warning path — if a warning fired, the budget below
	// would be measuring the wrong response and SC-006 would be
	// vacuously passed or unfairly failed.
	if _, has := body["warning"]; has {
		t.Fatalf("this fixture must produce no warning; got %v", body["warning"])
	}
	assertAcknowledgementShape(t, body)
	// The two new axes are asserted PRESENT before the budget is measured.
	// Without this the fixture could silently stop exercising them — a
	// rejected argument, a renamed key — and the budget would go green by
	// measuring the cheap answer again.
	if got, _ := body["closeOutcome"].(string); got != issues.CloseOutcomeSuperseded {
		t.Fatalf("fixture is not hostile on the outcome axis: closeOutcome = %q, "+
			"want the longest value %q", got, issues.CloseOutcomeSuperseded)
	}
	if got, _ := body["closeLink"].(string); got != string(issues.CloseLinkExisted) {
		t.Fatalf("fixture is not hostile on the link axis: closeLink = %q, want %q "+
			"(the longer of the two states, which needs the edge pre-seeded)",
			got, issues.CloseLinkExisted)
	}

	// The budget's one uncontrolled axis, asserted before it is measured.
	assertSC006StampsAreUTC(t, raw)

	if strings.Contains(raw, "SENTINEL") {
		t.Errorf("close response echoes issue prose:\n%s", raw)
	}
	// Not just "the whole reason is absent" — any distinctive slice of it
	// would mean a truncated echo, which is still an echo.
	for _, fragment := range []string{reason, "token-rotation handler", "runbook"} {
		if strings.Contains(raw, fragment) {
			t.Errorf("close response echoes the caller's reason (%q):\n%s", fragment, raw)
		}
	}
	// SC-006, on the service part. sc006BudgetChars is defined in this
	// file rather than imported from the production code on purpose: a
	// gate that compares against the very constant it guards moves
	// whenever that code moves and stops being a gate.
	//
	// The composition of the acknowledgement is NOT trimmed to fit the
	// number: US-5's required column (identifier, status, version, lock
	// and claim stamps) is what makes the answer worth anything, and
	// cutting it to go green would be the same trade as measuring on a
	// convenient fixture.
	total := utf8.RuneCountInString(raw)
	idChars := utf8.RuneCountInString(issueID)
	assigneeChars := utf8.RuneCountInString(userID.String())
	service := total - idChars - assigneeChars
	t.Logf("SC-006 on the hostile fixture: %d characters total, %d service part "+
		"(id %d + assignee %d shifted out, budget %d): %s",
		total, service, idChars, assigneeChars, sc006BudgetChars, raw)
	if service >= sc006BudgetChars {
		t.Errorf("close acknowledgement's service part is %d characters, SC-006 budget "+
			"is < %d (total %d, id %d, assignee %d):\n%s",
			service, sc006BudgetChars, total, idChars, assigneeChars, raw)
	}

	// INV-3: the reason is still in the system and reachable by a
	// reading tool — the cut is on the wire, not in the data.
	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT closed_reason FROM issues WHERE id = $1`, issueID).Scan(&stored); err != nil {
		t.Fatalf("read back closed_reason: %v", err)
	}
	if stored != reason {
		t.Errorf("stored closed_reason = %q, want the reason as sent", stored)
	}
}

// assertSC006StampsAreUTC checks the assumption sc006BudgetChars depends on:
// the acknowledgement's timestamps arrived in UTC, rendered with the `Z`
// suffix rather than a numeric offset.
//
// # Why a test asserts its own environment here, and only here
//
// Normally it should not — the environment is the harness's job. This gate
// is the exception because its margin is two characters and the offset form
// costs ten: `2026-09-20T10:00:00+03:00` is ten characters longer than
// `2026-09-20T10:00:00Z` across the pair of stamps. A non-UTC session
// therefore fails the budget by five times the margin, and it fails it with
// the message «the service part is 208, budget is < 200» — which is true,
// unhelpful, and sends the reader looking for a regression in the response
// composition that is not there.
//
// So the assumption is checked first and named. A failure here says: fix the
// database session's time zone (the local harness and CI both run UTC), not
// the budget.
//
// The check is on the RAW text rather than on parsed time.Time values on
// purpose: what the budget counts is characters on the wire, and
// time.Time.UTC() would normalise away exactly the difference being guarded.
func assertSC006StampsAreUTC(t *testing.T, raw string) {
	t.Helper()
	// RFC3339 instants as they appear inside JSON string values. The class
	// after the seconds captures whatever terminates the stamp — `Z` for
	// UTC, `+`/`-` for an offset.
	stamps := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(Z|[+-]\d{2}:\d{2})`).
		FindAllStringSubmatch(raw, -1)
	if len(stamps) == 0 {
		t.Fatalf("no RFC3339 timestamp found in the acknowledgement — the fixture "+
			"claims lock and claim stamps are present, and the budget is measured "+
			"on a response that carries them:\n%s", raw)
	}
	for _, m := range stamps {
		if m[1] != "Z" {
			t.Fatalf("timestamp %q is not UTC (offset %q). SC-006's margin under the "+
				"budget of %d is two characters, and a numeric offset costs five per "+
				"stamp, so this run would fail the budget on its ENVIRONMENT rather "+
				"than on the response. Fix the database session's time zone — do NOT "+
				"raise sc006BudgetChars:\n%s", m[0], m[1], sc006BudgetChars, raw)
		}
	}
}
