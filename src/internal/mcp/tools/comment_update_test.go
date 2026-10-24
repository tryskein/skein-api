package tools

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"skein-api/internal/issues"
)

// TestMapCommentWriteError_SerializationRetry pins the sk-303.17 coderev
// fix at the MCP boundary: UpdateComment/DeleteComment exhausting their
// commentRowMaxAttempts retry budget now returns issues.ErrSerializationRetry
// from the store, and mapCommentWriteError (shared by both
// skein_comment_update and skein_comment_delete) must turn that into
// `serialization_retry` — NOT fall through to the ErrCodeInternalError
// default, which is what happened before this fix (the sentinel didn't
// exist at this call site's branch list at all).
//
// Checked BEFORE ErrVersionConflict in the same spirit as
// TestMapClaimError_SerializationRetryIsNotAlreadyClaimed: a lost
// serialization race and a genuine version conflict are different events
// with different correct client behaviour (retry verbatim vs re-read
// first), so confusing the codes is its own regression class.
//
// Red on the pre-fix mapCommentWriteError (no issues.ErrSerializationRetry
// branch): every case below would report ErrCodeInternalError.
func TestMapCommentWriteError_Codes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "serialization retry",
			err:  issues.ErrSerializationRetry,
			want: ErrCodeSerializationRetry,
		},
		{
			// Store may wrap the sentinel with fmt.Errorf("...: %w", ...);
			// the mapping must see through it via errors.Is.
			name: "wrapped serialization retry",
			err:  fmt.Errorf("updating comment: %w", issues.ErrSerializationRetry),
			want: ErrCodeSerializationRetry,
		},
		{
			name: "version conflict",
			err:  &issues.ErrVersionConflict{CurrentVersion: 5},
			want: ErrCodeVersionConflict,
		},
		{
			name: "comment not found",
			err:  issues.ErrCommentNotFound,
			want: ErrCodeIssueNotFound,
		},
		{
			name: "issue not found",
			err:  issues.ErrNotFound,
			want: ErrCodeIssueNotFound,
		},
		{
			name: "validation error",
			err:  &issues.ValidationError{Code: "comment_deleted", Message: "cannot edit a deleted comment"},
			want: ErrCodeValidationError,
		},
		{
			name: "unknown error is internal",
			err:  errors.New("connection reset"),
			want: ErrCodeInternalError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mapCommentWriteError(context.Background(), tc.err, "skein_comment_update", "user-1", "comment-1")
			if res == nil {
				t.Fatalf("mapCommentWriteError returned nil")
			}
			if !res.IsError {
				t.Fatalf("mapCommentWriteError(%v) produced a non-error result", tc.err)
			}
			body := extractToolJSON(t, res)
			if got := body["error"]; got != tc.want {
				t.Fatalf("error = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestMapCommentWriteError_SerializationRetryIsNotVersionConflict states
// the sk-303.17 invariant as its own assertion, mirroring
// TestMapClaimError_SerializationRetryIsNotAlreadyClaimed: mapping an
// exhausted retry to version_conflict would tell the caller to re-read
// and retry on "the live currentVersion" — but the comment's version
// never moved (the whole transaction rolled back on every attempt), so a
// compliant re-read-and-retry would just observe the SAME version it
// already had and learn nothing.
func TestMapCommentWriteError_SerializationRetryIsNotVersionConflict(t *testing.T) {
	res := mapCommentWriteError(context.Background(), issues.ErrSerializationRetry,
		"skein_comment_update", "user-1", "comment-1")
	body := extractToolJSON(t, res)
	if got := body["error"]; got == ErrCodeVersionConflict {
		t.Fatalf("a lost serialization race reported as %s — the comment's version did not move", ErrCodeVersionConflict)
	}
	if _, has := body["currentVersion"]; has {
		t.Fatalf("serialization_retry response carries currentVersion %v — there is no version to report", body["currentVersion"])
	}
}

// TestMapAddCommentError_Codes covers skein_comment (AddComment)'s error
// mapping, extracted from handleComment specifically so it can be pinned
// here without a database — see mapAddCommentError's doc comment.
func TestMapAddCommentError_Codes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "serialization retry",
			err:  issues.ErrSerializationRetry,
			want: ErrCodeSerializationRetry,
		},
		{
			name: "wrapped serialization retry",
			err:  fmt.Errorf("adding comment: %w", issues.ErrSerializationRetry),
			want: ErrCodeSerializationRetry,
		},
		{
			name: "issue not found",
			err:  issues.ErrNotFound,
			want: ErrCodeIssueNotFound,
		},
		{
			name: "validation error",
			err:  &issues.ValidationError{Code: "invalid_body", Message: "comment body is required"},
			want: ErrCodeValidationError,
		},
		{
			name: "unknown error is internal",
			err:  errors.New("connection reset"),
			want: ErrCodeInternalError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mapAddCommentError(context.Background(), tc.err, "user-1", "sk-1")
			if res == nil {
				t.Fatalf("mapAddCommentError returned nil")
			}
			if !res.IsError {
				t.Fatalf("mapAddCommentError(%v) produced a non-error result", tc.err)
			}
			body := extractToolJSON(t, res)
			if got := body["error"]; got != tc.want {
				t.Fatalf("error = %v, want %s", got, tc.want)
			}
		})
	}
}
