package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/notifications"
)

// TestNotificationsDispatch_EachActionReachesItsHandler pins the routing
// table by identity: each action is bound to a distinct sentinel and must
// land on exactly the one named. Identity is the whole point in this
// group — mark_read, mark_read_issue and mark_read_all differ only in how
// many rows they touch, so a transposed entry in the map literal is a tool
// that marks an entire inbox read when asked about one notification, and a
// test asserting only "some handler ran" would stay green through it.
func TestNotificationsDispatch_EachActionReachesItsHandler(t *testing.T) {
	var got string
	set := notificationActions.with(sentinelHandlers(notificationActions, &got))

	// Spelled out rather than derived from notificationActions.order: this
	// list is the test's independent statement of the surface, so dropping
	// an action from the table fails here instead of silently shrinking
	// what is checked.
	want := []string{"inbox", "mark_read", "mark_read_issue", "mark_read_card", "mark_read_all", "archive"}
	if len(want) != len(notificationActions.order) {
		t.Fatalf("action table has %d entries, this test knows %d — update the test",
			len(notificationActions.order), len(want))
	}
	for _, action := range want {
		t.Run(action, func(t *testing.T) {
			got = ""
			res := callGroupAction(t, "skein_notifications", set, map[string]any{"action": action})
			if res.IsError {
				t.Fatalf("action %q returned an error result: %+v", action, res)
			}
			if got != action {
				t.Errorf("action %q dispatched to handler %q", action, got)
			}
		})
	}
}

// TestNotificationsDispatch_BadAction covers the ways an agent gets the
// discriminator wrong, including the shape specific to this group: the
// pre-sk-255.3 presence-based one-of (`all: true`, a bare notificationId)
// sent without any top-level action. None of those may do anything, and
// every rejection must carry allowedActions — the list that lets an agent
// correct itself in the next turn instead of re-running Tool Search.
func TestNotificationsDispatch_BadAction(t *testing.T) {
	handlers := make(map[string]server.ToolHandlerFunc, len(notificationActions.order))
	for _, name := range notificationActions.order {
		handlers[name] = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Errorf("handler ran for an invalid action")
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	set := notificationActions.with(handlers)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"empty", map[string]any{"action": ""}},
		{"unknown", map[string]any{"action": "__nope__"}},
		{"old tool name", map[string]any{"action": "skein_mark_read"}},
		// The pre-sk-255.3 one-of: `all` is gone from the schema and
		// sending it — with or without a companion id — must not sweep
		// anything.
		{"stale all flag", map[string]any{"all": true}},
		{"stale bare id", map[string]any{"id": 7}},
		{"stale notificationId only", map[string]any{"notificationId": 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callGroupAction(t, "skein_notifications", set, tc.args)
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
			if len(allowed) != len(notificationActions.order) {
				t.Fatalf("allowedActions has %d entries, action table has %d",
					len(allowed), len(notificationActions.order))
			}
			for i, v := range allowed {
				if v != notificationActions.order[i] {
					t.Errorf("allowedActions[%d] = %v, want %q", i, v, notificationActions.order[i])
				}
			}
		})
	}
}

// TestNotificationsLabel_BoundedByAllowlist guards the `action` Prometheus
// label for this group — the one change in the consolidation that cannot
// be walked back in production, because a caller-controlled string in a
// label mints a series per typo and the cardinality never comes down.
func TestNotificationsLabel_BoundedByAllowlist(t *testing.T) {
	var got string
	set := notificationActions.with(sentinelHandlers(notificationActions, &got))

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "mark_read_all"}, "mark_read_all"},
		{map[string]any{"action": "mark_read_card"}, "mark_read_card"},
		{map[string]any{"action": " inbox "}, "inbox"}, // trimmed, same series
		{map[string]any{}, actionInvalid},
		{map[string]any{"action": "Inbox"}, actionInvalid}, // case-sensitive on purpose
		{map[string]any{"all": true}, actionInvalid},       // the old one-of shape
		{map[string]any{"action": "skein_inbox"}, actionInvalid},
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

// TestNotificationsSchema_OneIdentifierName is a schema-level guard on the
// rename and the flattening this group was forced into.
//
// Rename: the folded tools called the same bigserial column two different
// things — `notificationId` in skein_mark_read, bare `id` in
// skein_archive_notification. One flat schema gets one name, and it is
// `notificationId`; a lingering `id` would be a second spelling an agent
// has to guess between.
//
// Flattening: `all` was the boolean half of a presence-based one-of. It is
// now the action `mark_read_all`, and it must not survive as a parameter —
// an agent that sends it would be describing an intent the dispatcher will
// never read.
//
// Only `action` may be required: inbox takes no arguments at all, so any
// other required field makes the read half of the tool uncallable from a
// client that validates before sending.
func TestNotificationsSchema_OneIdentifierName(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterNotifications(s, nil, nil)

	tool := registeredTool(t, s, "skein_notifications")
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != actionParam {
		t.Errorf("required = %v, want exactly [%q]", tool.InputSchema.Required, actionParam)
	}

	props := tool.InputSchema.Properties
	for _, want := range []string{"notificationId", "issueId", "projectId", "scope", "cursor", "limit"} {
		if _, ok := props[want]; !ok {
			t.Errorf("skein_notifications has no %q parameter (properties: %v)", want, keysOf(props))
		}
	}
	if _, bad := props["id"]; bad {
		t.Error("skein_notifications still exposes a bare `id` — the notification id is `notificationId`, " +
			"the name the mark_read family already used")
	}
	if _, bad := props["all"]; bad {
		t.Error("skein_notifications still exposes `all` — it was flattened into the action mark_read_all")
	}
}

// TestNotificationsGroup_ValidationSurvivesTheSplit is the guard on what
// skein_mark_read's presence-based one-of turned into.
//
// The old handler counted how many of notificationId / issueId / all were
// set and rejected anything but exactly one. Flattening that choice into
// three actions removes the "none set" and "wrong combination" cases from
// the common path — but NOT the check itself: an agent that picks
// mark_read_all and also sends notificationId is still expressing two
// widths at once, and on this family guessing wide is unrecoverable (there
// is no un-mark-read). Each action must therefore reject its siblings'
// targeting arguments rather than ignore them.
//
// Runs without a database on purpose: every assertion here is about a
// rejection that happens before the store is reached, so a nil store is
// the proof that it does.
func TestNotificationsGroup_ValidationSurvivesTheSplit(t *testing.T) {
	set := notificationActions.with(map[string]server.ToolHandlerFunc{
		"inbox":           handleInbox(nil),
		"mark_read":       handleMarkReadOne(nil, nil),
		"mark_read_issue": handleMarkReadIssue(nil, nil),
		"mark_read_card":  handleMarkReadCard(nil),
		"mark_read_all":   handleMarkReadAll(nil, nil),
		"archive":         handleArchiveNotification(nil, nil),
	})
	dispatch := set.dispatch("skein_notifications")
	ctx := withCtx(t, uuid.Must(uuid.NewRandom()), "user")

	cases := []struct {
		name string
		args map[string]any
		// want is a fragment the message must contain, so the test states
		// what the agent is told, not just that it was told something.
		want string
	}{
		{
			"mark_read without a target",
			map[string]any{"action": "mark_read"},
			"notificationId is required",
		},
		// JSON numbers reach a handler as float64 — the fixtures spell
		// that out so the table matches what arrives over the wire rather
		// than what a Go literal defaults to.
		{
			"mark_read with a non-numeric id",
			map[string]any{"action": "mark_read", "notificationId": "seven"},
			"must be an integer",
		},
		{
			"mark_read with a non-positive id",
			map[string]any{"action": "mark_read", "notificationId": float64(0)},
			"positive integer",
		},
		{
			"mark_read also carrying issueId",
			map[string]any{"action": "mark_read", "notificationId": float64(7), "issueId": "demo-42"},
			"mark_read_issue",
		},
		{
			"mark_read_issue without a target",
			map[string]any{"action": "mark_read_issue"},
			"issueId is required",
		},
		{
			"mark_read_issue with a bare number",
			map[string]any{"action": "mark_read_issue", "issueId": "42"},
			"hierarchical id",
		},
		{
			"mark_read_issue also carrying notificationId",
			map[string]any{"action": "mark_read_issue", "issueId": "demo-42", "notificationId": float64(7)},
			"action=\"mark_read\"",
		},
		{
			"mark_read_card without a target",
			map[string]any{"action": "mark_read_card"},
			"issueId is required",
		},
		{
			"mark_read_card with a bare number",
			map[string]any{"action": "mark_read_card", "issueId": "42"},
			"hierarchical id",
		},
		{
			"mark_read_card also carrying notificationId",
			map[string]any{"action": "mark_read_card", "issueId": "demo-42", "notificationId": float64(7)},
			"action=\"mark_read\"",
		},
		{
			"mark_read_all narrowed by notificationId",
			map[string]any{"action": "mark_read_all", "notificationId": float64(7)},
			"cannot be narrowed",
		},
		{
			"mark_read_all narrowed by issueId",
			map[string]any{"action": "mark_read_all", "issueId": "demo-42"},
			"cannot be narrowed",
		},
		{
			"mark_read_all with a malformed projectId",
			map[string]any{"action": "mark_read_all", "projectId": "not-a-uuid"},
			"projectId must be a UUID",
		},
		{
			"archive without a target",
			map[string]any{"action": "archive"},
			"notificationId is required",
		},
		{
			"archive with a non-positive id",
			map[string]any{"action": "archive", "notificationId": float64(-1)},
			"positive integer",
		},
		{
			"inbox with an unknown scope",
			map[string]any{"action": "inbox", "scope": "everything"},
			"scope must be one of",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Name = "skein_notifications"
			req.Params.Arguments = tc.args
			res, err := dispatch(ctx, req)
			if err != nil {
				t.Fatalf("dispatch: transport error %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected a rejection, got %+v", res)
			}
			body := envelope(t, res)
			if body["error"] != ErrCodeValidationError {
				t.Errorf("error = %v, want %q", body["error"], ErrCodeValidationError)
			}
			msg, _ := body["message"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

// TestNotificationsGroup_InboxMarkReadArchive is the behavioural smoke
// test the consolidation owed this group: before sk-255.3 skein_mark_read
// and skein_archive_notification had NO test at any level (skein_inbox had
// one, watch_test.go's empty-envelope check), so folding them — and
// splitting mark_read's one-of into three actions — would otherwise have
// been a refactor of code nothing watched.
//
// It walks inbox → mark_read → archive through the PRODUCTION dispatch
// table, so each assertion covers both halves of the fold: that the
// `action` string reaches the right handler, and that the handler still
// does what it did as a standalone tool. Assertions are on rows in the
// database and on what the inbox then returns, not on "no error".
func TestNotificationsGroup_InboxMarkReadArchive(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	projectID, prefix := seedProject(t, pool, userID, "notifgrp")
	issueID := seedIssue(t, pool, projectID, userID, prefix+"-1", "notified issue", "open", 2)

	first := seedNotificationOnIssue(t, pool, userID, projectID, issueID)
	second := seedNotificationOnIssue(t, pool, userID, projectID, issueID)

	call := newNotificationsCaller(t, pool, userID)

	// 1) inbox — the default scope is unread, so both rows come back.
	body := call(t, map[string]any{"action": "inbox"})
	if ids := inboxIDs(body); len(ids) != 2 {
		t.Fatalf("inbox returned %v, want both seeded notifications (%d, %d)", ids, first, second)
	}

	// 2) mark_read — exactly one row, and exactly the one named.
	body = call(t, map[string]any{"action": "mark_read", "notificationId": float64(first)})
	if body["updated"] != float64(1) {
		t.Errorf("mark_read updated = %v, want 1", body["updated"])
	}
	if !readAt(t, pool, first) {
		t.Errorf("notification %d is still unread after mark_read", first)
	}
	if readAt(t, pool, second) {
		t.Errorf("mark_read also read notification %d — it must touch one row only", second)
	}
	if ids := inboxIDs(call(t, map[string]any{"action": "inbox"})); len(ids) != 1 || ids[0] != second {
		t.Errorf("unread inbox = %v, want only %d", ids, second)
	}

	// 3) archive — moves the remaining row out of the default view, and it
	// is readable back through scope="archived". This is the action that
	// was renamed on the way in (`id` → `notificationId`); reaching the
	// store at all is the evidence the rename is wired through.
	body = call(t, map[string]any{"action": "archive", "notificationId": float64(second)})
	if body["id"] != float64(second) {
		t.Errorf("archive returned id %v, want %d", body["id"], second)
	}
	if ids := inboxIDs(call(t, map[string]any{"action": "inbox", "scope": "all"})); len(ids) != 1 || ids[0] != first {
		t.Errorf("scope=all inbox = %v, want only the non-archived %d", ids, first)
	}
	archived := inboxIDs(call(t, map[string]any{"action": "inbox", "scope": "archived"}))
	if len(archived) != 1 || archived[0] != second {
		t.Errorf("scope=archived inbox = %v, want %d", archived, second)
	}

	// Archiving stamps read_at too — the behaviour the folded tool
	// documented and the reason the archive view never shows unread bold.
	if !readAt(t, pool, second) {
		t.Errorf("notification %d is unread after archive — archive must stamp read_at as well", second)
	}
}

// TestNotificationsGroup_MarkReadThreeWidths is the test that the one-of
// split kept all three behaviours, not just the one the happy path walks.
//
// Each of the three actions gets its own fixture and its own assertion on
// which rows moved and which did not: mark_read touches one row,
// mark_read_issue touches everything tied to one issue INCLUDING comments
// on it (the special case inside MarkReadByEntity, which is the reason the
// description promises it), and mark_read_all sweeps the caller's whole
// backlog with projectId as the only narrowing.
func TestNotificationsGroup_MarkReadThreeWidths(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	projectID, prefix := seedProject(t, pool, userID, "notifwidth")
	issueA := seedIssue(t, pool, projectID, userID, prefix+"-1", "issue A", "open", 2)
	issueB := seedIssue(t, pool, projectID, userID, prefix+"-2", "issue B", "open", 2)

	call := newNotificationsCaller(t, pool, userID)

	t.Run("mark_read_issue covers the issue and its comments", func(t *testing.T) {
		onA := seedNotificationOnIssue(t, pool, userID, projectID, issueA)
		commentID := seedCommentKind(t, pool, issueA, userID, "a comment on A", "human")
		onAComment := seedNotificationOnComment(t, pool, userID, projectID, commentID)
		onB := seedNotificationOnIssue(t, pool, userID, projectID, issueB)

		body := call(t, map[string]any{"action": "mark_read_issue", "issueId": issueA})
		if body["updated"] != float64(2) {
			t.Errorf("updated = %v, want 2 (the issue row and its comment row)", body["updated"])
		}
		if !readAt(t, pool, onA) {
			t.Errorf("issue notification %d still unread", onA)
		}
		if !readAt(t, pool, onAComment) {
			t.Errorf("comment notification %d still unread — mark_read_issue promises comments too", onAComment)
		}
		if readAt(t, pool, onB) {
			t.Errorf("notification %d on another issue was marked read", onB)
		}
	})

	// sk-622.21 (US-7): a Watching card of an ancestor carries the events
	// of its descendants that the fan-out carded to it. mark_read_card reads
	// all of them and nothing else; mark_read_issue keeps its contract —
	// the issue's own events only.
	t.Run("mark_read_card reads the ancestor card with its descendants' events", func(t *testing.T) {
		child := seedIssue(t, pool, projectID, userID, issueA+".1", "child of A", "open", 2)
		onChild := seedCardedNotification(t, pool, userID, projectID, child, issueA)
		onA := seedCardedNotification(t, pool, userID, projectID, issueA, issueA)
		onB := seedCardedNotification(t, pool, userID, projectID, issueB, issueB)

		body := call(t, map[string]any{"action": "mark_read_card", "issueId": issueA})
		if body["marked"] != float64(2) {
			t.Errorf("marked = %v, want 2 (the card's own row and the descendant's)", body["marked"])
		}
		for _, id := range []int64{onChild, onA} {
			if !readAt(t, pool, id) {
				t.Errorf("notification %d on card %s still unread", id, issueA)
			}
		}
		if readAt(t, pool, onB) {
			t.Errorf("notification %d of another card was marked read", onB)
		}
		if body := call(t, map[string]any{"action": "mark_read_card", "issueId": issueA}); body["marked"] != float64(0) {
			t.Errorf("repeat marked = %v, want 0", body["marked"])
		}
	})

	t.Run("mark_read_issue still reads the issue's own events only", func(t *testing.T) {
		child := seedIssue(t, pool, projectID, userID, issueA+".2", "another child of A", "open", 2)
		onChild := seedCardedNotification(t, pool, userID, projectID, child, issueA)
		onA := seedCardedNotification(t, pool, userID, projectID, issueA, issueA)

		body := call(t, map[string]any{"action": "mark_read_issue", "issueId": issueA})
		if body["updated"] != float64(1) {
			t.Errorf("updated = %v, want 1 (the issue's own row)", body["updated"])
		}
		if !readAt(t, pool, onA) {
			t.Errorf("notification %d on the issue itself still unread", onA)
		}
		if readAt(t, pool, onChild) {
			t.Errorf("notification %d on the descendant was marked by mark_read_issue — that is mark_read_card's width", onChild)
		}
	})

	t.Run("mark_read_all sweeps everything unread", func(t *testing.T) {
		onA := seedNotificationOnIssue(t, pool, userID, projectID, issueA)
		onB := seedNotificationOnIssue(t, pool, userID, projectID, issueB)

		body := call(t, map[string]any{"action": "mark_read_all"})
		if body["updated"] != float64(2) {
			t.Errorf("updated = %v, want 2", body["updated"])
		}
		for _, id := range []int64{onA, onB} {
			if !readAt(t, pool, id) {
				t.Errorf("notification %d survived mark_read_all unread", id)
			}
		}
	})

	t.Run("mark_read_all narrows to one project", func(t *testing.T) {
		otherProject, otherPrefix := seedProject(t, pool, userID, "notifother")
		otherIssue := seedIssue(t, pool, otherProject, userID, otherPrefix+"-1", "elsewhere", "open", 2)
		here := seedNotificationOnIssue(t, pool, userID, projectID, issueA)
		elsewhere := seedNotificationOnIssue(t, pool, userID, otherProject, otherIssue)

		body := call(t, map[string]any{
			"action":    "mark_read_all",
			"projectId": projectID.String(),
		})
		if body["updated"] != float64(1) {
			t.Errorf("updated = %v, want 1 — projectId must scope the sweep", body["updated"])
		}
		if !readAt(t, pool, here) {
			t.Errorf("notification %d in the named project is still unread", here)
		}
		if readAt(t, pool, elsewhere) {
			t.Errorf("notification %d in another project was swept — projectId did not narrow anything", elsewhere)
		}
	})
}

// newNotificationsCaller wires the PRODUCTION dispatch table against a
// real store and returns a helper that calls it and decodes a successful
// payload. Going through actionSet.dispatch rather than the handlers
// directly is deliberate: it is the routing, not just the handler, that
// the consolidation introduced.
func newNotificationsCaller(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) func(*testing.T, map[string]any) map[string]any {
	t.Helper()
	store := notifications.NewStore(pool)
	set := notificationActions.with(map[string]server.ToolHandlerFunc{
		"inbox":           handleInbox(store),
		"mark_read":       handleMarkReadOne(pool, store),
		"mark_read_issue": handleMarkReadIssue(pool, store),
		"mark_read_card":  handleMarkReadCard(store),
		"mark_read_all":   handleMarkReadAll(pool, store),
		"archive":         handleArchiveNotification(pool, store),
	})
	dispatch := set.dispatch("skein_notifications")
	ctx := withCtx(t, userID, "user")

	return func(t *testing.T, args map[string]any) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "skein_notifications"
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
}

// seedNotificationOnIssue writes an events row for an issue plus one
// unread notification for the user, and returns the notification id.
func seedNotificationOnIssue(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID, issueID string) int64 {
	t.Helper()
	return seedNotificationRow(t, pool, userID, projectID, "issue.updated", "issue", issueID)
}

// seedNotificationOnComment does the same for a comment event, so a test
// can prove that marking an issue read also clears notifications about
// comments on it.
func seedNotificationOnComment(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID, commentID uuid.UUID) int64 {
	t.Helper()
	return seedNotificationRow(t, pool, userID, projectID, "comment.created", "comment", commentID.String())
}

func seedNotificationRow(
	t *testing.T,
	pool *pgxpool.Pool,
	userID, projectID uuid.UUID,
	eventType, entityType, entityID string,
) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now(), $1, $2, $3, $4, 'tester', $5, 'human', '{}'::jsonb)
RETURNING id`, eventType, entityType, entityID, projectID, userID).Scan(&eventID); err != nil {
		t.Fatalf("seed event %s on %s: %v", eventType, entityID, err)
	}
	var notificationID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, $3, $4, now(), true)
RETURNING id`, userID, eventID, eventType, projectID).Scan(&notificationID); err != nil {
		t.Fatalf("seed notification for event %d: %v", eventID, err)
	}
	// events cascade-delete their notifications; the project cleanup in
	// seedProject does not reach events, so drop them here.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eventID)
	})
	return notificationID
}

// seedCardedNotification seeds an issue.updated notification on issueID
// whose Watching card is card — the row the fan-out writes for a recipient
// watching card (an ancestor of issueID with its subtree, or issueID itself).
func seedCardedNotification(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID, issueID, card string) int64 {
	t.Helper()
	id := seedNotificationOnIssue(t, pool, userID, projectID, issueID)
	if _, err := pool.Exec(context.Background(),
		`UPDATE notifications SET card_issue_id = $2, card_resolved = true WHERE id = $1`, id, card); err != nil {
		t.Fatalf("card notification %d: %v", id, err)
	}
	return id
}

// readAt reports whether a notification row has been marked read.
func readAt(t *testing.T, pool *pgxpool.Pool, notificationID int64) bool {
	t.Helper()
	var read bool
	if err := pool.QueryRow(context.Background(),
		`SELECT read_at IS NOT NULL FROM notifications WHERE id = $1`, notificationID).Scan(&read); err != nil {
		t.Fatalf("read state of notification %d: %v", notificationID, err)
	}
	return read
}

// inboxIDs pulls the notification ids out of a decoded inbox payload, in
// the order the tool returned them.
func inboxIDs(body map[string]any) []int64 {
	items, _ := body["items"].([]any)
	out := make([]int64, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if f, ok := item["id"].(float64); ok {
			out = append(out, int64(f))
		}
	}
	return out
}

// ---------------------------------------------------------------------
// sk-282 T019 — the inbox row (test-plan rows 71, 72, 73). Row 74 is the
// REST side and lives with the REST handler.
// ---------------------------------------------------------------------

// seedNotificationWithPayload writes an event carrying field/old/new and
// one unread notification for the user, so the inbox row has values to
// project.
func seedNotificationWithPayload(
	t *testing.T,
	pool *pgxpool.Pool,
	userID, projectID uuid.UUID,
	eventType, entityType, entityID, field string,
	oldValue, newValue any,
) int64 {
	t.Helper()
	ctx := context.Background()
	// The DTO reads the dedicated columns (e.field / e.old_data /
	// e.new_data), not the payload blob — writing the values into the
	// payload would leave oldValue/newValue nil and the test would assert
	// nothing.
	oldRaw, err := json.Marshal(oldValue)
	if err != nil {
		t.Fatalf("marshal old: %v", err)
	}
	newRaw, err := json.Marshal(newValue)
	if err != nil {
		t.Fatalf("marshal new: %v", err)
	}

	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload,
                    field, old_data, new_data)
VALUES (now(), $1, $2, $3, $4, 'tester', $5, 'human', '{}'::jsonb,
        $6, $7::jsonb, $8::jsonb)
RETURNING id`, eventType, entityType, entityID, projectID, userID,
		field, oldRaw, newRaw).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var notificationID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, $3, $4, now(), true)
RETURNING id`, userID, eventID, eventType, projectID).Scan(&notificationID); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eventID)
	})
	return notificationID
}

func inboxRows(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("inbox items missing or not an array: %#v", body["items"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

// row 71 — SC-005: a five-kilobyte value gives a row under 1500
// characters, carrying the truncation marker.
//
// Three inputs, not one. Written against a single narrow fixture — a
// 25-character title and an empty oldValue — this test passed at 1 467
// characters purely because the per-value ceiling of 1 024 runes happened
// to leave 33 characters of slack. Two ordinary inputs blew straight
// through it: a 100-character title took the row to 1 544, and the
// non-empty oldValue that virtually every real description edit carries
// took it to 2 493. Both are rows an agent gets on a normal day, so a
// criterion that holds only on the narrow one is a criterion checked
// against its fixture. All three run here, and the row budget is what
// makes the answer independent of which one it is.
func TestInbox_ValuePairFitsSC005OnEveryInput(t *testing.T) {
	// 5 KB of text, which is what a real long comment or description edit
	// puts on the wire — twice, when the old value is carried too.
	const body = 5 * 1024
	const shortTitle = "an issue with a long edit"

	cases := []struct {
		hint     string
		name     string
		title    string
		oldValue string
		// wantCeilingCut asserts the PER-VALUE ceiling is what bound the
		// newValue — true only where the row budget left more room than
		// the ceiling, which is the original fixture and nothing else.
		wantCeilingCut bool
	}{
		{
			hint:           "nfsc5a",
			name:           "short title, empty old value (the original fixture)",
			title:          shortTitle,
			oldValue:       "",
			wantCeilingCut: true,
		},
		{
			hint:     "nfsc5b",
			name:     "a hundred-character title",
			title:    strings.Repeat("t", 100),
			oldValue: "",
		},
		{
			hint:     "nfsc5c",
			name:     "both halves of an ordinary description edit",
			title:    shortTitle,
			oldValue: strings.Repeat("o", body),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "user")
			pid, prefix := seedProject(t, pool, userID, tc.hint)
			issueID := seedIssue(t, pool, pid, userID, prefix+"-1", tc.title, "open", 2)

			seedNotificationWithPayload(t, pool, userID, pid,
				"issue.updated", "issue", issueID, "description",
				tc.oldValue, strings.Repeat("x", body))

			call := newNotificationsCaller(t, pool, userID)
			rows := inboxRows(t, call(t, map[string]any{"action": "inbox"}))
			if len(rows) != 1 {
				t.Fatalf("inbox returned %d rows, want 1", len(rows))
			}
			row := rows[0]

			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatalf("marshal row: %v", err)
			}
			const sc005Budget = 1500
			size := len([]rune(string(encoded)))
			t.Logf("SC-005 inbox row: title %d chars, oldValue %d, newValue %d → row %d chars "+
				"(budget %d)", len([]rune(tc.title)), len([]rune(tc.oldValue)), body,
				size, sc005Budget)
			if size >= sc005Budget {
				t.Errorf("inbox row is %d characters, SC-005 allows fewer than %d:\n%s",
					size, sc005Budget, encoded)
			}
			if row["valuesTruncated"] != true {
				t.Errorf("valuesTruncated = %v, want true — the row carries less than exists "+
					"and has to say so (INV-1)", row["valuesTruncated"])
			}

			newValue, _ := row["newValue"].(string)
			gotNew := len([]rune(newValue))
			if gotNew == 0 {
				t.Error("newValue came back empty — fitting the budget by deleting the value " +
					"is not a cut, it is a different answer to the question")
			}
			if gotNew > InboxValuePreviewMaxRunes {
				t.Errorf("newValue is %d runes, above the per-value ceiling of %d",
					gotNew, InboxValuePreviewMaxRunes)
			}
			if tc.wantCeilingCut && gotNew != InboxValuePreviewMaxRunes {
				t.Errorf("newValue is %d runes, want the per-value ceiling of %d — on this "+
					"input the row budget is the roomier of the two limits and must not bind",
					gotNew, InboxValuePreviewMaxRunes)
			}
			if tc.oldValue != "" {
				if n := len([]rune(row["oldValue"].(string))); n == 0 {
					t.Error("oldValue came back empty while newValue survived — the budget is " +
						"shared between the pair, not spent on whichever key is walked first")
				}
			}
		})
	}
}

// row 72 — a short field change is untouched and unmarked.
func TestInbox_ShortFieldChangeIsNotTruncated(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "nfshort")
	issueID := seedIssue(t, pool, pid, userID, prefix+"-1", "status change", "open", 2)

	seedNotificationWithPayload(t, pool, userID, pid,
		"issue.status_changed", "issue", issueID, "status", "open", "in_progress")

	call := newNotificationsCaller(t, pool, userID)
	rows := inboxRows(t, call(t, map[string]any{"action": "inbox"}))
	if len(rows) != 1 {
		t.Fatalf("inbox returned %d rows, want 1", len(rows))
	}
	row := rows[0]

	if row["oldValue"] != "open" || row["newValue"] != "in_progress" {
		t.Errorf("values = %v → %v, want them untouched", row["oldValue"], row["newValue"])
	}
	if v, ok := row["valuesTruncated"]; ok {
		t.Errorf("valuesTruncated = %v on a status change — on short text the marker is not "+
			"set, otherwise the marker lies", v)
	}
}

// row 73 — the summary stops repeating what the row already carries.
func TestInbox_SummaryDoesNotRepeatIssueIDAndTitle(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "nfsum")
	const title = "a distinctive issue title for the summary check"
	issueID := seedIssue(t, pool, pid, userID, prefix+"-1", title, "open", 2)

	seedNotificationOnIssue(t, pool, userID, pid, issueID)

	call := newNotificationsCaller(t, pool, userID)
	rows := inboxRows(t, call(t, map[string]any{"action": "inbox"}))
	if len(rows) != 1 {
		t.Fatalf("inbox returned %d rows, want 1", len(rows))
	}
	row := rows[0]

	summary, _ := row["summary"].(string)
	if summary == "" {
		t.Fatal("the row has no summary — the assertion below would hold vacuously")
	}
	if strings.Contains(summary, issueID) {
		t.Errorf("summary repeats the issue identifier %q, which the row already carries under "+
			"entity.id: %q", issueID, summary)
	}
	if strings.Contains(summary, title) {
		t.Errorf("summary repeats the issue title, which the row already carries under "+
			"entity.title: %q", summary)
	}
	// …and the structured fields are still there, or the de-duplication
	// removed the information instead of the duplication.
	entity, ok := row["entity"].(map[string]any)
	if !ok {
		t.Fatalf("entity missing: %#v", row["entity"])
	}
	if entity["id"] != issueID {
		t.Errorf("entity.id = %v, want %q", entity["id"], issueID)
	}
	if entity["title"] != title {
		t.Errorf("entity.title = %v, want the full title", entity["title"])
	}
}
