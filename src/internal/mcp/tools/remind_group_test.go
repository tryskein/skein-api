package tools

import (
	"context"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

// TestRemindDispatch_EachActionReachesItsHandler pins the routing table by
// identity: each action is bound to a distinct sentinel and must land on
// exactly the one named. Identity matters in this group because two of the
// four actions are near-opposites in effect — snooze keeps the reminder
// alive, dismiss deletes it outright with no undo — so a transposed entry
// in the map literal would silently destroy rows an agent asked to
// postpone, and a test that only asserted "no error" would stay green
// through it.
func TestRemindDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	set := remindActions.with(sentinelHandlers(remindActions, &got))

	// Spelled out rather than derived from remindActions.order: this list
	// is the test's independent statement of the surface, so dropping an
	// action from the table fails here instead of silently shrinking what
	// is checked.
	want := []string{"set", "list", "snooze", "dismiss"}
	if len(want) != len(remindActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(remindActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callGroupAction(t, "skein_remind", set, map[string]any{"action": action})
			if res.IsError {
				t.Fatalf("action %q returned an error result: %+v", action, res)
			}
			if got != action {
				t.Errorf("action %q dispatched to handler %q", action, got)
			}
		})
	}
}

// TestRemindDispatch_BadAction covers the ways an agent gets the
// discriminator wrong, including calling the folded tool by its old name.
// Every one must come back as a structured validation_error carrying
// allowedActions — that list is what lets an agent correct itself in the
// next turn instead of re-running Tool Search.
func TestRemindDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(remindActions.order))
	for _, name := range remindActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := remindActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"empty", map[string]any{"action": ""}},
		{"unknown", map[string]any{"action": "__nope__"}},
		// The pre-sk-255.3 tool names. An agent working from a stale
		// prompt will reach for these first, and the rejection has to hand
		// it the real vocabulary.
		{"old tool name", map[string]any{"action": "skein_remind_set"}},
		{"old bare name", map[string]any{"action": "remind_snooze"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGroupAction(t, "skein_remind", set, tc.args)
			if !res.IsError {
				t.Fatalf("expected an error result, got %+v", res)
			}
			body := envelope(t, res)
			if body["error"] != ErrCodeValidationError {
				t.Errorf("error = %v, want %q", body["error"], ErrCodeValidationError)
			}
			allowed, ok := body["allowedActions"].([]any)
			if !ok {
				t.Fatalf("allowedActions missing or not a list: %#v", body["allowedActions"])
			}
			if len(allowed) != len(remindActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(remindActions.order))
			}
			for i, v := range allowed {
				if v != remindActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, remindActions.order[i])
				}
			}
		})
	}
}

// TestRemindLabel_BoundedByAllowlist guards the `action` Prometheus label
// for this group — the one change in the consolidation that cannot be
// walked back in production, because a caller-controlled string in a label
// mints a series per typo and the cardinality never comes down.
func TestRemindLabel_BoundedByAllowlist(t *testing.T) {
	var got string
	set := remindActions.with(sentinelHandlers(remindActions, &got))

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "dismiss"}, "dismiss"},
		{map[string]any{"action": " snooze "}, "snooze"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": "Set"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"action": "skein_remind_list"}, actionInvalid},
		{map[string]any{"action": 42}, actionInvalid}, // wrong JSON type
	}
	for _, tc := range cases {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = tc.args
		if got := set.label(req); got != tc.want {
			t.Errorf("label(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestRemindSchema_OnlyActionIsRequired pins the schema decision this
// group was forced into. Nothing but the discriminator can be required:
// `issueId` and `remindAt` are mandatory for set and meaningless for list,
// and a JSON Schema `required` array cannot say "only when action=set"
// (Anthropic rejects a top-level oneOf/allOf/anyOf outright). Marking
// either of them required would make list uncallable from any client that
// validates the schema before sending.
//
// It also pins the two parameters this group deliberately did NOT merge.
// `note` is one parameter covering set and dismiss — same role, free text
// attached to the action — while `reason` stays separate on snooze; a
// later cleanup that "tidies" either decision has to fail here first.
func TestRemindSchema_OnlyActionIsRequired(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterRemind(s, nil, nil, nil, nil)

	tool := registeredTool(t, s, "skein_remind")
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != actionParam {
		t.Errorf("required = %v, want exactly [%q]", tool.InputSchema.Required, actionParam)
	}

	props := tool.InputSchema.Properties
	for _, want := range []string{"issueId", "remindAt", "reminderId", "by", "until", "reason", "note", "state", "project"} {
		if _, ok := props[want]; !ok {
			t.Errorf("skein_remind has no %q parameter (properties: %v)", want, keysOf(props))
		}
	}
	// The folded per-action names must not linger as parameters — an agent
	// that sends one gets nothing, and the dispatcher will never honour it.
	for _, gone := range []string{"op", "mode", "id"} {
		if _, bad := props[gone]; bad {
			t.Errorf("skein_remind exposes %q — reminders are targeted by reminderId / issueId", gone)
		}
	}
}

// TestRemindGroup_SetListSnoozeDismiss is the behavioural smoke test the
// consolidation owed this group: before sk-255.3 the four reminder tools
// had NO test of their own at any level, so folding them would otherwise
// have been a refactor of code nothing watched.
//
// It walks the whole life of one reminder — set → list → snooze → dismiss
// — through the PRODUCTION dispatch table (remindActions bound to the real
// handlers), so every assertion covers both halves of the fold: that the
// `action` string reaches the right handler, and that the handler still
// does what it did as a standalone tool. The assertions are on effects,
// not on "no error": the reminder appears in list with the note that was
// set, snooze moves remind_at and bumps snoozeCount, dismiss removes the
// row from the database.
func TestRemindGroup_SetListSnoozeDismiss(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	projectID, prefix := seedProject(t, pool, userID, "remindgrp")
	issueID := seedIssue(t, pool, projectID, userID, prefix+"-1", "reminder target", "open", 2)

	set := remindActions.with(map[string]server.ToolHandlerFunc{
		"set":     handleRemindSet(pool, reminders.NewStore(pool), issues.NewStore(pool)),
		"list":    handleRemindList(pool, reminders.NewStore(pool), projects.NewStore(pool)),
		"snooze":  handleRemindSnooze(pool, reminders.NewStore(pool)),
		"dismiss": handleRemindDismiss(pool, reminders.NewStore(pool)),
	})
	dispatch := set.dispatch("skein_remind")
	ctx := withCtx(t, userID, "user")

	call := func(t *testing.T, args map[string]any) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "skein_remind"
		req.Params.Arguments = args
		res, err := dispatch(ctx, req)
		if err != nil {
			t.Fatalf("dispatch(%v): transport error %v", args["action"], err)
		}
		if res.IsError {
			t.Fatalf("action %v returned an error: %v", args["action"], extractToolJSON(t, res))
		}
		return extractToolJSON(t, res)
	}

	// 1) set — arms a reminder two hours out, carrying a note.
	remindAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	created := call(t, map[string]any{
		"action":   "set",
		"issueId":  issueID,
		"remindAt": remindAt.Format(time.RFC3339),
		"note":     "smoke note",
	})
	reminderID, _ := created["id"].(string)
	if reminderID == "" {
		t.Fatalf("set returned no reminder id: %v", created)
	}
	if created["issueId"] != issueID {
		t.Errorf("set returned issueId %v, want %q", created["issueId"], issueID)
	}
	if created["state"] != string(reminders.StateArmed) {
		t.Errorf("state = %v, want %q (remindAt is in the future)", created["state"], reminders.StateArmed)
	}
	// The shared `note` parameter has to reach the reminder's own field —
	// this is the assertion that the set/dismiss `note` merge did not
	// quietly drop the value on one of the two sides.
	if created["lastReason"] != "smoke note" {
		t.Errorf("lastReason = %v, want \"smoke note\" — the `note` argument did not reach the row",
			created["lastReason"])
	}

	// 2) list — the reminder is visible to its owner, with the badge count.
	listed := call(t, map[string]any{"action": "list"})
	rows, _ := listed["reminders"].([]any)
	found := findReminderRow(rows, reminderID)
	if found == nil {
		t.Fatalf("list did not return the reminder just set: %v", listed)
	}
	if found["issueTitle"] != "reminder target" {
		t.Errorf("list issueTitle = %v, want \"reminder target\"", found["issueTitle"])
	}
	if found["state"] != string(reminders.StateArmed) {
		t.Errorf("list state = %v, want %q", found["state"], reminders.StateArmed)
	}
	if _, ok := listed["ringingTaskCount"]; !ok {
		t.Errorf("list dropped the ringingTaskCount badge: %v", listed)
	}

	// 3) snooze — by reminderId, relative offset. remind_at must move
	// forward and snoozeCount must go up; both are what distinguishes a
	// real snooze from a no-op the dispatcher merely routed.
	snoozed := call(t, map[string]any{
		"action":     "snooze",
		"reminderId": reminderID,
		"by":         "1d",
		"reason":     "smoke snooze",
	})
	if got, want := snoozed["snoozeCount"], float64(1); got != want {
		t.Errorf("snoozeCount = %v, want %v", got, want)
	}
	newRemindAt := parseRFC3339Field(t, snoozed, "remindAt")
	if !newRemindAt.After(remindAt) {
		t.Errorf("remindAt = %s, want later than the original %s", newRemindAt, remindAt)
	}
	// `reason` is the snooze-only parameter that was deliberately NOT
	// merged into `note`; it must still land in last_reason.
	if snoozed["lastReason"] != "smoke snooze" {
		t.Errorf("lastReason = %v, want \"smoke snooze\"", snoozed["lastReason"])
	}

	// 4) dismiss — by issueId this time, exercising the shared targeting
	// helper (exactly one active reminder on the issue) rather than the
	// explicit id path used above.
	dismissed := call(t, map[string]any{
		"action":  "dismiss",
		"issueId": issueID,
		"note":    "smoke dismiss",
	})
	if dismissed["dismissed"] != true {
		t.Errorf("dismiss returned %v, want dismissed=true", dismissed)
	}
	if dismissed["reminderId"] != reminderID {
		t.Errorf("dismiss targeted %v, want the reminder set on this issue (%s)",
			dismissed["reminderId"], reminderID)
	}

	// Dismiss is a hard delete — assert against the table, not the
	// response, because "no soft-delete" is the reason the whole
	// consolidated tool declares destructiveHint=true.
	var remaining int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reminders WHERE user_id = $1`, userID).Scan(&remaining); err != nil {
		t.Fatalf("count reminders: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d reminder rows survived dismiss, want 0", remaining)
	}

	// And the list lens agrees with the table.
	afterList := call(t, map[string]any{"action": "list"})
	afterRows, _ := afterList["reminders"].([]any)
	if findReminderRow(afterRows, reminderID) != nil {
		t.Errorf("list still returns the dismissed reminder: %v", afterList)
	}
}

// TestRemindGroup_TargetingErrorsSurvive pins the per-action validation
// the fold could have swallowed: snooze and dismiss share
// resolveReminderTarget, whose errors are the only thing standing between
// "I meant that reminder" and acting on the wrong row.
func TestRemindGroup_TargetingErrorsSurvive(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	projectID, prefix := seedProject(t, pool, userID, "remindtgt")
	issueID := seedIssue(t, pool, projectID, userID, prefix+"-1", "two alarms", "open", 2)

	store := reminders.NewStore(pool)
	set := remindActions.with(map[string]server.ToolHandlerFunc{
		"set":     handleRemindSet(pool, store, issues.NewStore(pool)),
		"list":    handleRemindList(pool, store, projects.NewStore(pool)),
		"snooze":  handleRemindSnooze(pool, store),
		"dismiss": handleRemindDismiss(pool, store),
	})
	dispatch := set.dispatch("skein_remind")
	ctx := withCtx(t, userID, "user")

	call := func(args map[string]any) *mcpgo.CallToolResult {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "skein_remind"
		req.Params.Arguments = args
		res, err := dispatch(ctx, req)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		return res
	}

	// Two reminders on one issue → issueId targeting is ambiguous and must
	// say so with the candidate list, not pick one.
	for _, offset := range []time.Duration{2 * time.Hour, 4 * time.Hour} {
		res := call(map[string]any{
			"action":   "set",
			"issueId":  issueID,
			"remindAt": time.Now().UTC().Add(offset).Format(time.RFC3339),
		})
		if res.IsError {
			t.Fatalf("set failed: %v", extractToolJSON(t, res))
		}
	}

	res := call(map[string]any{"action": "dismiss", "issueId": issueID})
	if !res.IsError {
		t.Fatalf("dismiss with two candidate reminders was accepted: %v", extractToolJSON(t, res))
	}
	body := envelope(t, res)
	if body["error"] != ErrCodeReminderTargetAmbiguous {
		t.Errorf("error = %v, want %q", body["error"], ErrCodeReminderTargetAmbiguous)
	}
	if refs, ok := body["reminders"].([]any); !ok || len(refs) != 2 {
		t.Errorf("ambiguity error carries %v, want the two candidate reminders", body["reminders"])
	}

	// Neither target supplied → validation_error, not a silent no-op.
	res = call(map[string]any{"action": "snooze", "by": "1d"})
	if !res.IsError {
		t.Fatalf("snooze without a target was accepted: %v", extractToolJSON(t, res))
	}
	if code := envelope(t, res)["error"]; code != ErrCodeValidationError {
		t.Errorf("error = %v, want %q", code, ErrCodeValidationError)
	}

	// set against an issue that does not exist → issue_not_found, the
	// visibility gate the handler runs before touching the store.
	res = call(map[string]any{
		"action":   "set",
		"issueId":  prefix + "-999",
		"remindAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	})
	if !res.IsError {
		t.Fatalf("set on a missing issue was accepted: %v", extractToolJSON(t, res))
	}
	if code := envelope(t, res)["error"]; code != ErrCodeIssueNotFound {
		t.Errorf("error = %v, want %q", code, ErrCodeIssueNotFound)
	}
}

// findReminderRow picks one row out of a decoded remind list by id.
func findReminderRow(rows []any, reminderID string) map[string]any {
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if row["reminderId"] == reminderID || row["id"] == reminderID {
			return row
		}
	}
	return nil
}

// parseRFC3339Field reads a timestamp field out of a decoded tool payload.
func parseRFC3339Field(t *testing.T, body map[string]any, field string) time.Time {
	t.Helper()
	raw, _ := body[field].(string)
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("%s = %q, not an RFC3339 timestamp: %v", field, raw, err)
	}
	return ts.UTC()
}
