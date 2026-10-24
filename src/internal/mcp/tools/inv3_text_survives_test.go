package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// inv3_text_survives_test.go — matrix row 94 (sk-282, T025).
//
// "INV-3 (roll-up): for every slice of free text (close reason,
// source context, comment body, notification values, project
// description) the text STAYS IN THE SYSTEM and is read by a reading
// tool — the check is done through a read, not through the action's response."
//
// The row is a roll-up and it is written as ONE test with
// one subtest per cut rather than five assertions scattered across five
// files. The statement it makes is about the run as a whole: every cut
// this run introduced is a cut on the WIRE and none of them touched the
// data. Five separate assertions can each pass while nobody can say
// whether the set is complete; one place with one subtest per cut can be
// read against the spec's own list in a single pass.
//
// The oracle is always a READ — a reading tool, or the store call the
// REST reader uses where the MCP surface has no detail read. Never the
// body of the write that performed the cut: that body is exactly what
// the cut removed, so asserting on it could only be satisfied by putting
// the echo back.

func TestINV3_EveryTextCutSurvivesInTheSystem(t *testing.T) {
	t.Run("closed reason (skein_close)", func(t *testing.T) {
		pool := newTestPool(t)
		userID, uname := seedUser(t, pool, "admin")
		pid, prefix := seedProject(t, pool, userID, "inv3a")
		issueID := prefix + "-1"
		seedIssue(t, pool, pid, userID, issueID, "closing", "open", 2)
		assignIssue(t, pool, issueID, uname)

		const reason = "SENTINEL closed reason: the full why, which the caller supplied " +
			"and the acknowledgement no longer returns"
		store := issues.NewStore(pool)
		ctx := withCtxSession(t, userID, "admin", "sess-inv3a")

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"id": issueID, "expectedVersion": 1, "closeOutcome": "done", "reason": reason,
		}
		res, err := handleClose(pool, store)(ctx, req)
		if err != nil || res.IsError {
			t.Fatalf("close failed: %v %+v", err, res)
		}
		if strings.Contains(toolText(t, res), "SENTINEL") {
			t.Fatalf("the close response still echoes the reason — this subtest would then "+
				"be proving nothing:\n%s", toolText(t, res))
		}
		card := showCard(t, pool, store, ctx, issueID)
		if card["closedReason"] != reason {
			t.Errorf("skein_show closedReason = %#v, want the reason as sent", card["closedReason"])
		}
	})

	t.Run("source context (skein_create)", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, _ := seedProject(t, pool, userID, "inv3b")
		store := issues.NewStore(pool)
		ctx := withCtx(t, userID, "admin")

		const sourceContext = "SENTINEL source context: while implementing sk-282.18.4.12"
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project":       pid.String(),
			"title":         "an issue with a source context",
			"type":          "task",
			"kind":          "autonomous",
			"sourceContext": sourceContext,
		}
		res, err := handleCreate(pool, store, dependencies.NewStore(pool),
			projects.NewStore(pool))(ctx, req)
		if err != nil || res.IsError {
			t.Fatalf("create failed: %v %+v", err, res)
		}
		if strings.Contains(toolText(t, res), "SENTINEL") {
			t.Fatalf("the create response still echoes the source context:\n%s", toolText(t, res))
		}
		body := extractToolJSON(t, res)
		issueObj, _ := body["issue"].(map[string]any)
		issueID, _ := issueObj["id"].(string)

		card := showCard(t, pool, store, ctx, issueID)
		if card["sourceContext"] != sourceContext {
			t.Errorf("skein_show sourceContext = %#v, want the value as sent", card["sourceContext"])
		}
	})

	t.Run("comment body (skein_comment)", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, prefix := seedProject(t, pool, userID, "inv3c")
		issueID := prefix + "-1"
		seedIssue(t, pool, pid, userID, issueID, "with a comment", "open", 2)
		store := issues.NewStore(pool)
		ctx := withCtx(t, userID, "admin")

		const body = "SENTINEL comment body: the text the caller wrote and the write-tool " +
			"deliberately does not read back to it"
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"issueId": issueID, "body": body, "kind": "autonomous",
		}
		res, err := handleComment(pool, store)(ctx, req)
		if err != nil || res.IsError {
			t.Fatalf("comment failed: %v %+v", err, res)
		}
		if strings.Contains(toolText(t, res), "SENTINEL") {
			t.Fatalf("the comment response still echoes the body:\n%s", toolText(t, res))
		}
		created := extractToolJSON(t, res)
		commentObj, _ := created["comment"].(map[string]any)
		commentID, _ := commentObj["id"].(string)
		if commentID == "" {
			t.Fatalf("comment response carries no id: %+v", created)
		}

		// The addressed read — the tool US-4 hands the re-read to.
		getReq := mcpgo.CallToolRequest{}
		getReq.Params.Arguments = map[string]any{"commentId": commentID}
		getRes, err := handleCommentGet(pool, store)(ctx, getReq)
		if err != nil || getRes.IsError {
			t.Fatalf("skein_comment_get failed: %v %+v", err, getRes)
		}
		got := extractToolJSON(t, getRes)
		comment, _ := got["comment"].(map[string]any)
		if comment["body"] != body {
			t.Errorf("skein_comment_get body = %#v, want the body as sent", comment["body"])
		}
	})

	t.Run("notification values (skein_notifications inbox)", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "user")
		pid, prefix := seedProject(t, pool, userID, "inv3d")
		issueID := seedIssue(t, pool, pid, userID, prefix+"-1", "edited issue", "open", 2)

		// The notification's `newValue` IS the issue's new description —
		// which is what makes the issue card the right place to read the
		// untruncated text back from.
		newValue := "SENTINEL-DESCRIPTION " + strings.Repeat("x", 5*1024)
		if _, err := pool.Exec(context.Background(),
			`UPDATE issues SET description = $2 WHERE id = $1`, issueID, newValue); err != nil {
			t.Fatalf("seed description: %v", err)
		}
		seedNotificationWithPayload(t, pool, userID, pid,
			"issue.updated", "issue", issueID, "description", "", newValue)

		call := newNotificationsCaller(t, pool, userID)
		rows := inboxRows(t, call(t, map[string]any{"action": "inbox"}))
		if len(rows) != 1 {
			t.Fatalf("inbox returned %d rows, want 1", len(rows))
		}
		row := rows[0]
		if row["valuesTruncated"] != true {
			t.Fatalf("the inbox row did not truncate — this subtest would be proving "+
				"nothing: %#v", row)
		}
		if shown, _ := row["newValue"].(string); shown == newValue {
			t.Fatalf("the inbox row carries the whole value; nothing was cut")
		}

		card := showCard(t, pool, issues.NewStore(pool), withCtx(t, userID, "user"), issueID)
		if card["description"] != newValue {
			t.Errorf("skein_show description is not the untruncated value — the cut reached " +
				"the data")
		}
	})

	t.Run("project description (skein_project_list)", func(t *testing.T) {
		pool := newTestPool(t)
		userID, _ := seedUser(t, pool, "admin")
		pid, _ := seedProject(t, pool, userID, "inv3e")
		description := "SENTINEL project description " +
			strings.Repeat("d", ProjectDescriptionPreviewMaxRunes*2)
		if _, err := pool.Exec(context.Background(),
			`UPDATE projects SET description = $2 WHERE id = $1`, pid, description); err != nil {
			t.Fatalf("seed project description: %v", err)
		}

		pStore := projects.NewStore(pool)
		ctx := withCtx(t, userID, "admin")
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{}
		res, err := handleProjectList(pStore, nil)(ctx, req)
		if err != nil || res.IsError {
			t.Fatalf("project list failed: %v %+v", err, res)
		}
		body := extractToolJSON(t, res)
		items, _ := body["items"].([]any)
		var found map[string]any
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item["id"] == pid.String() {
				found = item
			}
		}
		if found == nil {
			t.Fatalf("seeded project missing from the list: %+v", body)
		}
		if found["descriptionTruncated"] != true {
			t.Fatalf("the row did not truncate — this subtest would be proving nothing: %#v",
				found)
		}
		if found["description"] == description {
			t.Fatalf("the row carries the whole description; nothing was cut")
		}

		// skein_project_list is the only MCP surface that renders a
		// project's description, and skein_project_resolve returns
		// identity fields only — so the read that proves the data is
		// intact is the store call the REST project reader serves. Stated
		// rather than glossed: this is the one cut in the run whose
		// full text has no MCP reader today.
		full, err := pStore.GetProject(context.Background(), pid)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if full.Description != description {
			t.Errorf("stored project description = %q, want the value as seeded",
				full.Description)
		}
	})
}

// showCard reads an issue through skein_show and returns its card.
func showCard(t *testing.T, pool *pgxpool.Pool, store *issues.Store, ctx context.Context,
	issueID string) map[string]any {
	t.Helper()
	body := callShow(t, handleShow(pool, store, dependencies.NewStore(pool)), ctx,
		map[string]any{"id": issueID, "includeComments": false, "includeDeps": false})
	card, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("skein_show issue type %T", body["issue"])
	}
	return card
}
