package attachments

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBuildS3Key(t *testing.T) {
	pid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	aid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	got := BuildS3Key(pid, "demo-42", aid, "screenshot.png")
	want := "11111111-1111-1111-1111-111111111111/demo-42/22222222-2222-2222-2222-222222222222/screenshot.png"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildS3KeySanitizes(t *testing.T) {
	pid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	aid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	// Path-traversal in the filename gets reduced to the basename.
	got := BuildS3Key(pid, "demo-42", aid, "../../../etc/passwd")
	if !strings.HasSuffix(got, "/passwd") {
		t.Fatalf("expected sanitization to drop path components: %q", got)
	}
	// Issue ids with slashes (defensive — issues package rejects these
	// upstream) are flattened to underscores.
	got2 := BuildS3Key(pid, "demo/42", aid, "x.png")
	if strings.Contains(got2, "/demo/42/") {
		t.Fatalf("expected slash sanitisation in issueID: %q", got2)
	}
}

func TestAttachmentIDFromKey(t *testing.T) {
	aid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	key := "11111111-1111-1111-1111-111111111111/demo-42/22222222-2222-2222-2222-222222222222/photo.png"
	got, ok := AttachmentIDFromKey(key)
	if !ok {
		t.Fatalf("AttachmentIDFromKey unexpectedly failed: %q", key)
	}
	if got != aid {
		t.Fatalf("got %s, want %s", got, aid)
	}

	// Wrong shape returns false instead of crashing.
	if _, ok := AttachmentIDFromKey("rogue/object/path"); ok {
		t.Fatalf("expected ok=false for malformed key")
	}
	if _, ok := AttachmentIDFromKey("a/b/not-a-uuid/file"); ok {
		t.Fatalf("expected ok=false when UUID slot isn't a UUID")
	}
}
