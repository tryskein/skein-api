package attachments

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// BuildS3Key composes the deterministic S3 key for an attachment.
//
// Layout: "{projectID}/{issueID}/{attachmentID}/{filename}".
//
// Why include the attachmentID even when filename is unique-looking?
// Because two users uploading "screenshot.png" to the same issue must not
// collide — the attachment row is the source of truth, the filename is
// metadata. Including the attachment UUID in the path makes the key
// 1-to-1 with the row (UNIQUE (s3_bucket, s3_key) backs this up at the
// DB level too).
//
// The cleanup goroutine relies on this scheme: it lists every object
// under the bucket, derives the attachmentID from the second-to-last
// path component, and reconciles against the attachments table.
//
// The returned key is sanitised to keep S3-safe characters only — non-
// ASCII filenames are kept (S3 allows them), but slashes/backslashes/NUL
// are stripped or replaced.
func BuildS3Key(projectID uuid.UUID, issueID string, attachmentID uuid.UUID, filename string) string {
	safeIssue := sanitizeIssueID(issueID)
	safeFile := sanitizeFilename(filename)
	return fmt.Sprintf("%s/%s/%s/%s", projectID.String(), safeIssue, attachmentID.String(), safeFile)
}

// AttachmentIDFromKey extracts the attachment UUID from a key produced by
// BuildS3Key. Returns false if the key does not match the expected
// shape — the caller treats unrecognised keys as out-of-band uploads
// and skips them in cleanup.
func AttachmentIDFromKey(key string) (uuid.UUID, bool) {
	parts := strings.Split(key, "/")
	// Must be at least: project/issue/attachment/filename.
	if len(parts) < 4 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(parts[len(parts)-2])
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// sanitizeIssueID strips path separators from the issue id. issue IDs may
// contain dots and dashes (demo-42.1.3) — both are S3-safe. Slash and
// backslash are not allowed in issues (validated upstream) but defence in
// depth here is cheap.
func sanitizeIssueID(id string) string {
	r := strings.NewReplacer("/", "_", "\\", "_")
	return r.Replace(id)
}

// sanitizeFilename strips path separators, control chars, and NUL.
// Preserves Unicode and most punctuation — S3 keys are byte sequences
// without further interpretation.
func sanitizeFilename(name string) string {
	// Use base() so a malicious "../../etc/passwd" reduces to "passwd".
	name = filepath.Base(name)
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r < 0x20 || r == '/' || r == '\\' || r == 0x7f {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "file"
	}
	return out
}
