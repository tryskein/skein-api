package handler

// issue_card_rest_contract_test.go — sk-282 T012, test-plan row 51.
//
// "INV-2: REST issue card — NOT A SINGLE new key at all (not only
// counters), previous JSON types and representative values. The wording
// \"new counter keys\" was already a weakening of the invariant: any other
// additive key would have slipped past it."
//
// The row's wording is the point. T012 adds childrenCount /
// unclosedChildrenCount to the MCP card, and the cheap way to do that is
// to hang them on issues.IssueDetail — which is the struct the REST card
// serialises. A test that only looked for those two names would pass while
// some other additive key slipped in, so this one is an ALLOWLIST: every
// key on the REST card is enumerated, and anything else fails.
//
// Types are asserted alongside presence, because INV-2 covers "the field
// set, their types and values" — a field that turned from a number into a
// string breaks the same contract as a missing one.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// restIssueCardKeys is the REST card's field set, enumerated. Sourced from
// issues.IssueDetail's json tags — but written out HERE, because a test
// that derived the expectation from the struct it guards would agree with
// any change to it.
var restIssueCardKeys = map[string]string{
	"id": "string", "projectId": "string", "version": "number",
	"title": "string", "description": "string", "type": "string",
	"priority": "number", "status": "string", "pinned": "bool",
	"source": "string", "design": "string", "acceptanceCriteria": "string",
	"notes": "string", "createdBy": "string", "createdAt": "string",
	"updatedAt": "string", "metadata": "object", "componentIds": "array",
	"labels": "array", "commentsCount": "number", "commitRefs": "array",
	"isBlocked": "bool", "blockers": "array", "mentions": "array",
	// sk-423: a deliberate additive REST change, allowed by the sk-423
	// decision (variant B) against sk-282 INV-2 — the number of blocks /
	// waits-for links to issues outside the reader's horizon, whatever
	// their status. Always serialised (0 when none), so it is mandatory.
	"unavailableBlockingLinks": "number",
}

// restIssueCardOptionalKeys may or may not appear depending on the row —
// they carry `omitempty`. Listed so the allowlist does not fail on a
// fixture that happens to populate one.
var restIssueCardOptionalKeys = map[string]string{
	"parentIdCurrent": "string", "sourceContext": "string", "assignee": "string",
	"lockedBy": "string", "lockedAt": "string", "estimatedMinutes": "number",
	"dueAt": "string", "deferUntil": "string", "startedAt": "string",
	"closedAt": "string", "closedReason": "string", "externalRef": "string",
	"createdByUsername": "string", "assigneeUserId": "string",
	"acceptedAt": "string", "acceptedBy": "string", "acceptedByUsername": "string",
	"epic": "object", "inheritedComponentIds": "array",
	"previousAssignees": "array", "dismissedBy": "array",
	"previousAssigneeUsernames": "array", "dismissedByUsernames": "array",

	// sk-303.14 T006 (contract C1) — extending the list as a DELIBERATE ACT,
	// not as a repair of a reddened test.
	//
	// Three review-mechanic fields are added to the detail card on
	// purpose: US-1 requires the author to see the flag and the stamp
	// exactly there, and the review-flag work is the very spec the INV-2 guard above
	// demands ("if this is a deliberate REST change — it belongs in a spec").
	//
	// All three sit in the OPTIONAL list, and that is a substantive
	// claim, not a consequence of the tag: INV-2 declares them the author's
	// private bookkeeping, so for a non-author the keys are not in the
	// object at all. A key mandatory for everyone would break the invariant
	// by the mere fact of being present. That the absence actually happens
	// is proven by issue_card_review_privacy_test.go; here only the
	// contract's shape is pinned.
	"reviewRequired": "bool", "reviewedAt": "string", "reviewedBy": "string",

	// sk-421.1 T002 (US-15, R-3, INV-3) — extending the list as a DELIBERATE
	// ACT following the sk-303.14 precedent above, not a repair of a test gone red.
	//
	// The name of the lock holder is added to the detail card on purpose:
	// the main consumer of the name is the web card, and it reads REST, so
	// the sk-282 route "only in the MCP projection" does not apply to it (the spec,
	// "Rejected alternatives"). This is exactly one new top-level
	// key, which the sk-421.1 spec allows against sk-282 INV-2.
	//
	// The key is OPTIONAL by meaning, not only by tag: it is present only
	// while the issue is locked and the lock fact record is found, and it disappears
	// together with the lock. The former lockedBy (the session) changes neither name, nor
	// type, nor meaning, and the session is not repeated in the new key (INV-4).
	"lockedByUsername": "string",

	// sk-489 T001 (US-1, INV-1, INV-3) — extending the list as a DELIBERATE
	// ACT following the sk-303.14 and sk-421.1 precedents above, not a repair of a
	// test gone red.
	//
	// The close outcome is added to the card on purpose: US-7 requires that the
	// outcome be visible on the issue card, and US-12 - that it can be edited
	// right there; the edit control decides whether to show itself by the presence
	// of exactly this key (US-10, "new frontend on an old api").
	//
	// The key is OPTIONAL by meaning, not only by tag: INV-1 says that the
	// outcome exists if and only if the issue is closed - for an unclosed one
	// it is absent from the object altogether. A mandatory key (even with the value
	// null) would violate the invariant by the very fact of its presence.
	"closeOutcome": "string",

	// sk-622.21 T026 (US-15, K16) — extending the list as a DELIBERATE ACT
	// following the precedents above: the card gets the freshness order of its row,
	// by which the phase 2 frontend decides whether to substitute the row from an event
	// into an open card (plan, T026: "rowSeq in all REST issue rows and
	// the card"). Optional by tag (omitempty, as in the row — the T009
	// contract); for an issue card it is always present — the column is NOT NULL since
	// migration 000064.
	"rowSeq": "number",

	// sk-674.18 T032 (US-3, US-13) — extending the list as a DELIBERATE ACT:
	// the short id of the space of the issue's project (not of the id prefix). Optional by
	// tag (omitempty); for an issue from issues it is always present — projects.space_id
	// is NOT NULL since migration 000080.
	"spaceId": "string",

	// sk-674.18 T034 (US-16, K-28) — extending the list as a DELIBERATE ACT:
	// the reader may release someone else's lock. Optional: it is set only by
	// reading the card and the unlock action, on other surfaces the key is absent.
	"canForceUnlock": "bool",
}

// jsonWireTags splits a struct's json tags into the fields that are
// always serialised and the ones carrying `omitempty`. Fields tagged "-"
// never reach the wire and are skipped.
//
// It exists for the cross-check in dependencies_rest_contract_test.go, NOT to
// build expectations: the enumerations stay hand-written, and this only asks
// whether they still describe the struct.
func jsonWireTags(typ reflect.Type) (mandatory, optional map[string]bool) {
	mandatory, optional = map[string]bool{}, map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" { // unexported
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		if strings.Contains(opts, "omitempty") {
			optional[name] = true
			continue
		}
		mandatory[name] = true
	}
	return mandatory, optional
}

// diffKeySets reports what one side has that the other does not.
func diffKeySets(got map[string]bool, want map[string]string) (missing, extra []string) {
	for name := range got {
		if _, ok := want[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range want {
		if !got[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func callIssueGet(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID, role string, projectID uuid.UUID, issueID string) map[string]any {
	t.Helper()
	h := NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/"+projectID.String()+"/issues/"+issueID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID.String())
	rctx.URLParams.Add("issueID", issueID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, caller)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.Get(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		var problem map[string]any
		_ = json.NewDecoder(res.Body).Decode(&problem)
		t.Fatalf("GET issue = %d, want 200: %v", res.StatusCode, problem)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// assertJSONKind checks one wire value against its hand-written type
// declaration, and is the ONLY place the six REST contract files decide
// whether a value's type is acceptable.
//
// Every one of them used to spell the check `got != want && got != "null"`
// — a JSON null passed on every key, whatever the enumeration declared.
// That tolerance sat exactly where INV-2 is worth the most: a field that
// starts arriving null is a break every consumer feels (`row.labels.map`
// throws, a `string` becomes nil), and it costs no key, so the key-set
// half of these guards cannot see it either. Null is accepted here only
// where the declaration says so, by spelling the type "<kind>|null", and
// nowhere else — which also forces the author of a genuinely nullable
// field to state that in the enumeration.
//
// The declaration "any" is for a value whose JSON kind is legitimately not
// fixed — oldValue / newValue carry whatever the changed field held: a
// string, a number, a bool. It is a statement about the field, not a way
// out of the check: "any" still refuses null unless spelled "any|null".
func assertJSONKind(t *testing.T, where, key string, value any, declared string) {
	t.Helper()
	want, nullable := strings.CutSuffix(declared, "|null")
	got := jsonKind(value)
	switch {
	case got == "null":
		if !nullable {
			t.Errorf("%s: key %q arrived null, and the enumeration declares it %q — a null "+
				"where %s is promised breaks the consumer without removing a key, so it is "+
				"accepted only where the declaration spells «%s|null»",
				where, key, declared, want, want)
		}
	case want == "any":
		// The field's kind is genuinely unconstrained — see above.
	case got != want:
		t.Errorf("%s: key %q is a %s, want %s", where, key, got, want)
	}
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// TestIssueCardREST_NoNewKeys is the allowlist.
func TestIssueCardREST_NoNewKeys(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restcard")
	parent := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "head", "open", 2)
	// Children, so the MCP-side counters would have something to report —
	// the REST card must still not report it.
	for i, st := range []string{"open", "closed", "standing"} {
		child := testsupport.SeedIssue(t, pool, pid, owner,
			prefix+"-1."+string(rune('1'+i)), "child", st, 2)
		testsupport.SeedDep(t, pool, child, parent, dependencies.DepParentChild, owner)
	}

	// sk-421.1 T003 (test-plan rows 11, 13): the card must actually CARRY
	// what the enumerations below describe, or they guard nothing.
	//   - a visible blocker (same project) and a hidden one (a project the
	//     reader is not a member of): the visible one is the blockers[]
	//     entry whose form is checked below; the hidden one must NOT be an
	//     entry (sk-423) and is counted in unavailableBlockingLinks;
	//   - a real claim through Store.Claim with a session, so the
	//     issue.claimed record exists and lockedByUsername is on the wire —
	//     a claim seeded by SQL would leave the key absent and the optional
	//     enumeration would never check it.
	outsider, _ := testsupport.SeedUser(t, pool, "user")
	hiddenPID, hiddenPrefix := testsupport.SeedProject(t, pool, outsider, "restcardq")
	visibleBlocker := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-2", "visible blocker", "open", 2)
	hiddenBlocker := testsupport.SeedIssue(t, pool, hiddenPID, outsider, hiddenPrefix+"-1", "hidden blocker", "open", 2)
	testsupport.SeedDep(t, pool, parent, visibleBlocker, dependencies.DepBlocks, owner)
	testsupport.SeedDep(t, pool, parent, hiddenBlocker, dependencies.DepBlocks, owner)
	if _, err := issues.NewStore(pool).Claim(context.Background(), issues.ClaimInput{
		ID:        parent,
		SessionID: "mcp-session-restcard",
		Actor:     issues.HumanActor(owner, ownerName),
	}); err != nil {
		t.Fatalf("claim the card's issue: %v", err)
	}

	card := callIssueGet(t, pool, owner, "user", pid, parent)

	// Presence first: without it the allowlist below would pass on a card
	// that simply never carried the key.
	if v, ok := card["lockedByUsername"]; !ok {
		t.Fatalf("the claimed card carries no lockedByUsername — the fixture no longer "+
			"exercises the optional key T002 added; card keys: %v", sortedCardKeys(card))
	} else if _, isString := v.(string); !isString {
		t.Errorf("lockedByUsername is %T, want a string", v)
	}
	if _, isArray := card["blockers"].([]any); !isArray {
		t.Fatalf("blockers is %T, want a (mandatory) array", card["blockers"])
	}

	for key, value := range card {
		wantKind, known := restIssueCardKeys[key]
		if !known {
			wantKind, known = restIssueCardOptionalKeys[key]
		}
		if !known {
			t.Errorf("REST issue card carries an unenumerated key %q = %#v.\n"+
				"INV-2 covers additive changes: \"no REST response changes its field set "+
				"… including additive key insertion\". If this key is a deliberate REST "+
				"change it belongs in a spec, not in a projection task; if it leaked from the "+
				"MCP projection, it must live there instead.", key, value)
			continue
		}
		assertJSONKind(t, "REST issue card", key, value, wantKind)
	}

	// The two names T012 introduces, called out by name as well: the
	// allowlist above already covers them, but a failure that says WHICH
	// key leaked is worth more than one that says "unenumerated".
	for _, key := range []string{"childrenCount", "unclosedChildrenCount"} {
		if v, ok := card[key]; ok {
			t.Errorf("the REST card carries %q = %v — the US-3 counters live in the MCP "+
				"projection only (D-2). Hanging them on issues.IssueDetail is the cheap "+
				"implementation and the one INV-2 forbids.", key, v)
		}
	}

	// …and the other half of "field set": every key the enumeration
	// calls mandatory must actually BE on the card. Without this the
	// allowlist is only an upper bound — it catches a key that appeared
	// and misses a key that vanished, while INV-2 says the REST response
	// does not CHANGE its field set, not that it does not grow.
	for key := range restIssueCardKeys {
		if _, ok := card[key]; !ok {
			t.Errorf("the REST issue card lost %q. INV-2 covers removals as well as additions: "+
				"\"no REST response changes its field set\". A consumer reading this key gets "+
				"undefined instead of a value, and nothing else in this suite would notice.", key)
		}
	}

	// sk-421.1 T003 (row 11), as sk-423 left it: down into blockers[]. Every
	// entry has exactly the visible form, and the hidden blocker of the
	// fixture is not an entry but a count.
	entries := card["blockers"].([]any)
	for i, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("blockers[%d] is %T, want an object", i, raw)
			continue
		}
		assertBlockerEntryForm(t, i, entry)
	}
	if len(entries) != 1 || card["unavailableBlockingLinks"] != float64(1) {
		t.Errorf("blockers = %v, unavailableBlockingLinks = %v; want exactly the visible blocker "+
			"and one unavailable link — the fixture must exercise both halves or the descent "+
			"checks nothing", entries, card["unavailableBlockingLinks"])
	}
}

func sortedCardKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ─────────────── sk-421.1 T003: inside the blocker record blockers[] ───────────────
//
// Extending the guard as a DELIBERATE ACT, following the sk-303.14 precedent above and
// the pattern of descending into the epic summary sk-421.2 — not a repair of a test gone red.
//
// Before sk-421.1 the guard knew `blockers` only as a value of kind "array" and did not
// descend into it. sk-421.1 introduced a second, hidden record form ({id,
// redacted:true}); sk-423 removed it: the very presence of such a record said that the
// inaccessible issue is not closed. Now the record has ONE form — the former visible
// {id, title, status}, — and a link to an issue outside the reader's horizon is counted in
// the card's `unavailableBlockingLinks`. The guard holds exactly this: a record of exactly
// one form, without `redacted`.

// restBlockerVisibleKeys — the only form of a blocker record.
var restBlockerVisibleKeys = map[string]string{
	"id": "string", "title": "string", "status": "string",
}

// assertBlockerEntryForm checks one blockers[] entry against the one form.
func assertBlockerEntryForm(t *testing.T, i int, entry map[string]any) {
	t.Helper()
	where := "REST issue card blockers[" + strconv.Itoa(i) + "]"
	same := len(entry) == len(restBlockerVisibleKeys)
	for key := range entry {
		if _, ok := restBlockerVisibleKeys[key]; !ok {
			same = false
		}
	}
	if !same {
		t.Errorf("%s has keys %v, which do not match the blocker form %v.\n"+
			"A key added to a blockers[] entry CHANGES the REST card: enumerate it here in the same "+
			"commit, or keep it off the wire. (sk-423 removed the redacted form on purpose.)",
			where, sortedCardKeys(entry), sortedCardKeys(toAnyMap(restBlockerVisibleKeys)))
		return
	}
	for key, value := range entry {
		assertJSONKind(t, where, key, value, restBlockerVisibleKeys[key])
	}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ─────────────────────── sk-421.2 T002: inside the epic summary ───────────────────────
//
// Before sk-421.2 the REST card composition guard knew `epic` ONLY as a value
// of kind "object" (see restIssueCardOptionalKeys) and did not descend into it, and its
// fixture seeded an issue of type 'task' — it has no `epic` due to omitempty.
// So adding a field to the epic summary failed no check here.
// Below: an epic fixture and the key list of the nested object — following the pattern
// restIssueCardKeys.

// seedEpicIssue seeds an issue of type 'epic'. testsupport.SeedIssue nails
// the type to 'task', so the epic has to be seeded here.
func seedEpicIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title string) string {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'epic', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		idStr, projectID, title, creator); err != nil {
		t.Fatalf("seed epic %s: %v", idStr, err)
	}
	return idStr
}

// restEpicRollupKeys — the composition of the nested `epic` object, written out HERE for
// the same reason as restIssueCardKeys: a list derived from the
// structure it guards would agree with any change to it.
var restEpicRollupKeys = map[string]string{
	"totalChildren": "number", "openChildren": "number",
	"inProgressChildren": "number", "closedChildren": "number",
	"deferredChildren": "number", "standingChildren": "number",
	"unavailableChildren": "number", "blockedChildren": "number",
	"eligibleForClose": "bool",

	// sk-489 T001 (US-5, INV-3, INV-5) — extending the list as a DELIBERATE
	// ACT: the epic summary gets three progress numbers BY OUTCOME next to
	// the former per-status breakdowns, whose meaning does not change.
	//
	// All three are MANDATORY, and this is a substantive claim: zero must
	// read as "counted, and there are none". A client that does not see the key
	// must conclude that an OLD api is answering, and draw the former picture
	// from the former fields (US-10) — omitempty on them would merge these two states
	// on the very first epic with no dropped children.
	"doneChildren": "number", "noWorkChildren": "number",
	"countedChildren": "number",
}

// epicJSONKind — the kind of a JSON value after encoding/json.
func epicJSONKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// TestIssueCardREST_EpicRollupKeysAreEnumerated — allowlist INSIDE the summary.
func TestIssueCardREST_EpicRollupKeysAreEnumerated(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restepic")
	epicID := seedEpicIssue(t, pool, pid, owner, prefix+"-1", "the epic")
	for i, st := range []string{"open", "deferred", "closed"} {
		child := testsupport.SeedIssue(t, pool, pid, owner,
			prefix+"-1."+string(rune('1'+i)), "child", st, 2)
		testsupport.SeedDep(t, pool, child, epicID, dependencies.DepParentChild, owner)
	}

	card := callIssueGet(t, pool, owner, "user", pid, epicID)

	raw, present := card["epic"]
	if !present {
		t.Fatalf("the epic card does not carry the `epic` object; card: %#v", card)
	}
	rollup, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("`epic` has kind %T, want an object", raw)
	}

	for key, value := range rollup {
		wantKind, known := restEpicRollupKeys[key]
		if !known {
			t.Errorf("the nested `epic` object carries an unlisted key %q = %#v.\n"+
				"Adding a field to the epic summary CHANGES the REST card: list it here "+
				"in the same commit or remove it from the surface.", key, value)
			continue
		}
		if got := epicJSONKind(value); got != wantKind {
			t.Errorf("epic.%s has kind %s, the list asserts %s", key, got, wantKind)
		}
	}
	for key := range restEpicRollupKeys {
		if _, ok := rollup[key]; !ok {
			t.Errorf("the list names epic.%s, but there is no such key on the wire", key)
		}
	}
}
