package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"skein-api/internal/issues"
	"skein-api/internal/notifications"
)

// TestNotifications_CommentBodyComesFromTheComment — sk-1024: the inbox
// (GET /me/notifications, MCP skein_notifications) and the Watching card
// timelines show a comment event's text from the comment itself, as the
// activity feed does (sk-977.7.3): live — the current text, edited — the new
// text, deleted — "comment deleted" with deleted=true; the copy the event
// kept is never served. (The task monitor, whose rows carry no body at all,
// is covered by notifications.TestHydrate_CommentBody.)
func TestNotifications_CommentBodyComesFromTheComment(t *testing.T) {
	pool := newAuthTestPool(t)
	ctx := context.Background()
	watcher, _ := seedActivityUser(t, pool, "user")
	author, authorName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, watcher)
	if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by) VALUES ($1, $2, 'member', now(), $3)`,
		pid, author, watcher); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role)
SELECT space_id, $2, 'member' FROM projects WHERE id = $1 ON CONFLICT DO NOTHING`, pid, author); err != nil {
		t.Fatalf("add space member: %v", err)
	}
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, watcher, issueID, "inbox comment body")
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since) VALUES ($1, $2, ARRAY['manual']::text[], now())`,
		issueID, watcher); err != nil {
		t.Fatalf("watch: %v", err)
	}
	// The comment reason is off by default; the watcher turns its feed on.
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'comment', true, false)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = true`, watcher); err != nil {
		t.Fatalf("enable comment reason: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_reason_prefs WHERE user_id = $1`, watcher)
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = $1`, watcher)
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_watchers WHERE issue_id = $1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE issue_id = $1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, pid, author)
	})

	store := issues.NewStore(pool)
	actor := issues.HumanActor(author, authorName)
	add := func(body string) *issues.CommentDTO {
		t.Helper()
		c, err := store.AddComment(ctx, issueID, body, author, authorName,
			issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman, actor)
		if err != nil {
			t.Fatalf("add comment: %v", err)
		}
		return c
	}
	const (
		liveText    = "live inbox text"
		original    = "original inbox secret"
		edited      = "edited inbox text"
		deletedText = "deleted inbox secret"
	)
	live := add(liveText)
	ed := add(original)
	if _, err := store.UpdateComment(ctx, ed.ID, edited, ed.Version, actor); err != nil {
		t.Fatalf("edit: %v", err)
	}
	del := add(deletedText)
	if err := store.DeleteComment(ctx, del.ID, "", actor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	type want struct {
		body    string
		deleted bool
	}
	wants := map[string]want{
		live.ID.String(): {body: liveText},
		ed.ID.String():   {body: edited},
		del.ID.String():  {body: "comment deleted", deleted: true},
	}
	check := func(t *testing.T, surface string, items []notifications.NotificationDTO) {
		t.Helper()
		seen := map[string]bool{}
		for _, it := range items {
			if it.Type != "comment.created" {
				continue
			}
			nv, _ := it.NewValue.(map[string]any)
			cid, _ := nv["id"].(string)
			w, ours := wants[cid]
			if !ours {
				continue
			}
			seen[cid] = true
			if nv["body"] != w.body {
				t.Errorf("%s: comment %s body = %v, want %q", surface, cid, nv["body"], w.body)
			}
			if d, _ := nv["deleted"].(bool); d != w.deleted {
				t.Errorf("%s: comment %s deleted = %v, want %v", surface, cid, nv["deleted"], w.deleted)
			}
		}
		for cid := range wants {
			if !seen[cid] {
				t.Errorf("%s: no comment.created row of comment %s", surface, cid)
			}
		}
		raw, _ := json.Marshal(items)
		for _, secret := range []string{original, deletedText} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s leaks %q", surface, secret)
			}
		}
	}

	nstore := notifications.NewStore(pool)
	inbox, err := nstore.ListInbox(ctx, notifications.ListInboxArgs{UserID: watcher, Scope: "all", Limit: 100})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	check(t, "inbox", inbox.Items)

	recent, err := nstore.HydrateWatchingRecentEvents(ctx, watcher, []string{issueID})
	if err != nil {
		t.Fatalf("HydrateWatchingRecentEvents: %v", err)
	}
	check(t, "watching", recent[issueID])
}
