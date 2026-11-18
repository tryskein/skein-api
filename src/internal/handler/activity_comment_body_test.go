package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"skein-api/internal/issues"
)

// TestActivityFeeds_CommentBodyComesFromTheComment pins the rule of the
// activity feeds for comment rows: the text is the comment's current text,
// never the copy the comment.created event kept. An edited comment shows its
// new text (the original must not reach the wire), a deleted one shows the
// "comment deleted" marker with deleted=true, a live one its text. Every feed
// that reads activityListSQL is checked: the project feed, the issue feed,
// /me/activity and the dashboard feed.
func TestActivityFeeds_CommentBodyComesFromTheComment(t *testing.T) {
	pool := newAuthTestPool(t)
	owner, ownerName := seedActivityUser(t, pool, "user")
	pid, prefix := seedActivityProject(t, pool, owner)
	issueID := prefix + "-1"
	seedActivityIssue(t, pool, pid, owner, issueID, "comment body subject")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE issue_id = $1`, issueID)
	})

	ctx := context.Background()
	store := issues.NewStore(pool)
	actor := issues.HumanActor(owner, ownerName)
	add := func(body string) *issues.CommentDTO {
		t.Helper()
		c, err := store.AddComment(ctx, issueID, body, owner, ownerName,
			issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman, actor)
		if err != nil {
			t.Fatalf("add comment %q: %v", body, err)
		}
		return c
	}

	const (
		liveText     = "live comment text"
		originalText = "original text with a secret"
		editedText   = "edited text"
		deletedText  = "deleted text with a secret"
	)
	live := add(liveText)
	edited := add(originalText)
	if _, err := store.UpdateComment(ctx, edited.ID, editedText, edited.Version, actor); err != nil {
		t.Fatalf("edit comment: %v", err)
	}
	deleted := add(deletedText)
	if err := store.DeleteComment(ctx, deleted.ID, "", actor); err != nil {
		t.Fatalf("delete comment: %v", err)
	}

	activityH := NewActivityHandler(pool, store)
	dashH := buildDashHandlerX(t, pool)
	feeds := []struct {
		name string
		call func() (*http.Response, []byte)
	}{
		{"project", func() (*http.Response, []byte) {
			return callActivity(t, activityH, http.MethodGet,
				fmt.Sprintf("/api/v1/projects/%s/activity?limit=100", pid), owner, "user")
		}},
		{"issue", func() (*http.Response, []byte) {
			return callIssueActivity(t, activityH,
				fmt.Sprintf("/api/v1/projects/%s/issues/%s/activity?limit=100", pid, issueID), owner, "user")
		}},
		{"me", func() (*http.Response, []byte) {
			return callActivity(t, activityH, http.MethodGet, "/api/v1/me/activity?limit=100", owner, "user")
		}},
		{"dashboard", func() (*http.Response, []byte) {
			return callDashEx(t, dashH, "/api/v1/dashboard/feed?limit=100", owner, false)
		}},
	}

	type want struct {
		body    string
		deleted bool
	}
	wants := map[string]want{
		live.ID.String():    {body: liveText},
		edited.ID.String():  {body: editedText},
		deleted.ID.String(): {body: issues.CommentDeletedBody, deleted: true},
	}

	for _, f := range feeds {
		t.Run(f.name, func(t *testing.T) {
			res, raw := f.call()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", res.StatusCode, raw)
			}
			items, _ := decodeActivityResponse(t, raw)
			seen := map[string]bool{}
			for _, it := range items {
				if it.Type != issues.EventCommentCreated {
					continue
				}
				nv, ok := it.NewValue.(map[string]any)
				if !ok {
					t.Fatalf("comment.created newValue is %T, want an object", it.NewValue)
				}
				cid, _ := nv["id"].(string)
				w, ours := wants[cid]
				if !ours {
					continue
				}
				seen[cid] = true
				if got := nv["body"]; got != w.body {
					t.Errorf("comment %s: newValue.body = %v, want %q", cid, got, w.body)
				}
				gotDeleted, _ := nv["deleted"].(bool)
				if gotDeleted != w.deleted {
					t.Errorf("comment %s: newValue.deleted = %v, want %v", cid, nv["deleted"], w.deleted)
				}
			}
			for cid := range wants {
				if !seen[cid] {
					t.Errorf("comment.created row of comment %s missing from the %s feed", cid, f.name)
				}
			}
			for _, secret := range []string{originalText, deletedText} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Errorf("%s feed leaks %q: %s", f.name, secret, raw)
				}
			}
		})
	}
}
