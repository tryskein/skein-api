package issues

// commit_refs_test.go — the checks of a commit ref being written
// (sk-977.7.69): url an absolute http or https URL of at most 2048
// characters, author at most 256, message at most 1000, refused as a
// validation error before any transaction opens.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestValidateCommitRefs_URLAuthorMessage — the accepted and refused
// shapes of the free fields; lengths are code points.
func TestValidateCommitRefs_URLAuthorMessage(t *testing.T) {
	const hash = "abcdef1"
	longURL := func(n int) string {
		prefix := "https://example.com/"
		return prefix + strings.Repeat("a", n-len(prefix))
	}
	for _, tc := range []struct {
		name     string
		ref      CloseCommitInput
		wantCode string // "" — accepted
	}{
		{"https url", CloseCommitInput{Hash: hash, URL: strPtr("https://example.com/org/repo/commit/abcdef1")}, ""},
		{"http url", CloseCommitInput{Hash: hash, URL: strPtr("http://example.com/c/abcdef1")}, ""},
		{"upper-case scheme", CloseCommitInput{Hash: hash, URL: strPtr("HTTPS://example.com/c")}, ""},
		{"url of exactly 2048 characters", CloseCommitInput{Hash: hash, URL: strPtr(longURL(2048))}, ""},
		{"no url, author, message", CloseCommitInput{Hash: hash}, ""},
		{"javascript url", CloseCommitInput{Hash: hash, URL: strPtr("javascript:alert(1)")}, "invalid_commit_url"},
		{"data url", CloseCommitInput{Hash: hash, URL: strPtr("data:text/html,<script>alert(1)</script>")}, "invalid_commit_url"},
		{"file url", CloseCommitInput{Hash: hash, URL: strPtr("file:///etc/passwd")}, "invalid_commit_url"},
		{"relative reference", CloseCommitInput{Hash: hash, URL: strPtr("/org/repo/commit/abcdef1")}, "invalid_commit_url"},
		{"scheme-relative reference", CloseCommitInput{Hash: hash, URL: strPtr("//example.com/c")}, "invalid_commit_url"},
		{"opaque http", CloseCommitInput{Hash: hash, URL: strPtr("http:example.com")}, "invalid_commit_url"},
		{"http without a host", CloseCommitInput{Hash: hash, URL: strPtr("http://:80/c")}, "invalid_commit_url"},
		{"control character", CloseCommitInput{Hash: hash, URL: strPtr("https://example.com/\x7f")}, "invalid_commit_url"},
		{"url of 2049 characters", CloseCommitInput{Hash: hash, URL: strPtr(longURL(2049))}, "invalid_commit_url"},
		{"author of 256 characters", CloseCommitInput{Hash: hash, Author: strPtr(strings.Repeat("я", 256))}, ""},
		{"author of 257 characters", CloseCommitInput{Hash: hash, Author: strPtr(strings.Repeat("a", 257))}, "invalid_commit_author"},
		{"message of 1000 characters", CloseCommitInput{Hash: hash, Message: strPtr(strings.Repeat("я", 1000))}, ""},
		{"message of 1001 characters", CloseCommitInput{Hash: hash, Message: strPtr(strings.Repeat("a", 1001))}, "invalid_commit_message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommitRefs([]CloseCommitInput{tc.ref})
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a ValidationError %s", err, tc.wantCode)
			}
			if ve.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (%s)", ve.Code, tc.wantCode, ve.Message)
			}
		})
	}
}

// TestCommitWrites_RefuseJavascriptURL — both writes of commit refs, a close
// and an attach, refuse a javascript: URL as a validation error before the
// database is reached (the store here has no pool).
func TestCommitWrites_RefuseJavascriptURL(t *testing.T) {
	s := NewStore(nil)
	commits := []CloseCommitInput{{Hash: "abcdef1", URL: strPtr("javascript:alert(document.cookie)")}}
	for name, write := range map[string]func() error{
		"close": func() error {
			_, err := s.Close(context.Background(), CloseInput{ID: "demo-1", ExpectedVersion: 1, Reason: "done", Commits: commits})
			return err
		},
		"attach": func() error {
			_, err := s.AttachCommits(context.Background(), AttachCommitsInput{ID: "demo-1", Commits: commits})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var ve *ValidationError
			if err := write(); !errors.As(err, &ve) || ve.Code != "invalid_commit_url" {
				t.Fatalf("err = %v, want ValidationError invalid_commit_url", err)
			}
		})
	}
}
