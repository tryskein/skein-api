package notifications

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sk-1024: hydrate never serves the body copy a comment event kept. On a
// feed row (inbox, Watching) the body is the comment's current text, or the
// "comment deleted" marker with deleted=true; on a task-monitor row
// (previewOnly) there is no body at all — its text travels only as the
// bounded bodyPreview.
func TestHydrate_CommentBody(t *testing.T) {
	copyData := []byte(`{"id":"c1","issueId":"sk-1","author":"a","body":"copy kept by the event"}`)
	current := "current text"
	deletedAt := time.Now().UTC()
	cases := []struct {
		name     string
		raw      notifRaw
		wantBody any // nil: no body key
		wantGone bool
	}{
		{"feed, live", notifRaw{entityType: "comment", eventType: "comment.created", newData: copyData, commentBody: &current}, "current text", false},
		{"feed, deleted", notifRaw{entityType: "comment", eventType: "comment.created", newData: copyData, commentBody: &current, commentDeletedAt: &deletedAt}, "comment deleted", true},
		{"feed, row gone", notifRaw{entityType: "comment", eventType: "comment.created", newData: copyData}, "comment deleted", true},
		{"feed, event without copy", notifRaw{entityType: "comment", eventType: "comment.created", commentBody: &current}, "current text", false},
		{"monitor", notifRaw{entityType: "comment", eventType: "comment.created", newData: copyData, commentBody: &current, previewOnly: true}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n NotificationDTO
			tc.raw.hydrate(&n)
			raw, _ := json.Marshal(n)
			if strings.Contains(string(raw), "copy kept by the event") {
				t.Fatalf("the event's copy reached the row: %s", raw)
			}
			nv, _ := n.NewValue.(map[string]any)
			if tc.wantBody == nil {
				if _, has := nv["body"]; has {
					t.Fatalf("monitor row carries a body: %v", nv)
				}
				if nv["id"] != "c1" {
					t.Fatalf("monitor row lost the other keys: %v", nv)
				}
				return
			}
			if nv["body"] != tc.wantBody {
				t.Fatalf("body = %v, want %v", nv["body"], tc.wantBody)
			}
			if d, _ := nv["deleted"].(bool); d != tc.wantGone {
				t.Fatalf("deleted = %v, want %v", nv["deleted"], tc.wantGone)
			}
		})
	}
}
