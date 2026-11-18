package redact

import "time"

// CommentDeletedBody is the text a feed row (activity, inbox, Watching)
// carries in place of the body of a comment that has been deleted or whose
// row is gone. A task-monitor row carries no body at all: its text travels
// only as the bounded bodyPreview, absent for a deleted comment. The row also carries "deleted": true, so a
// client tells the marker from a comment that happens to say the same words
// (sk-977.7.3, sk-1024).
const CommentDeletedBody = "comment deleted"

// CurrentCommentBody replaces the "body" key of a comment event's decoded
// old/new value with the comment's current text: body is the live
// comments.body, deletedAt its deleted_at. A comment event keeps a copy of
// the body as it was when the event was written; feeds never serve that
// copy. A deleted comment, or one whose row no longer exists (body nil),
// gets CommentDeletedBody and "deleted": true. A value without a "body" key
// is returned unchanged unless always is set (the new value of
// comment.created, which shows the comment's text whether or not the event
// kept a copy); then a missing object is started empty.
func CurrentCommentBody(v any, body *string, deletedAt *time.Time, always bool) any {
	m, ok := v.(map[string]any)
	if !ok {
		if !always || v != nil {
			return v
		}
		m = map[string]any{}
	}
	if _, has := m["body"]; !has && !always {
		return v
	}
	if body == nil || deletedAt != nil {
		m["body"] = CommentDeletedBody
		m["deleted"] = true
		return m
	}
	m["body"] = *body
	return m
}
