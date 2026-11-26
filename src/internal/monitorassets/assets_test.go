package monitorassets

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// runnerTemplateSHA256 is the SHA-256 of the runner template (the parts
// joined in manifest order, placeholders in place) as it was assembled from
// internal/handler/assets/runner before the parts moved here.
const runnerTemplateSHA256 = "fbcce9fd9dbb6f047a9f5c12c18484546c3eecb762e50c07dcdd2a3ccd9894b6"

// TestRunnerTemplateUnchangedByTheMove: the embedded parts join into the very
// bytes the server served before the move, so the body and its digest are
// the same. The join is the one of handler.assembleRunner: the listed parts
// in order, each ending with a newline.
func TestRunnerTemplateUnchangedByTheMove(t *testing.T) {
	manifest, err := fs.ReadFile(Runner, path.Join(RunnerDir, "parts.txt"))
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}
	var body bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(manifest))
	parts := 0
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		part, err := fs.ReadFile(Runner, path.Join(RunnerDir, name))
		if err != nil {
			t.Fatalf("reading part %q: %v", name, err)
		}
		body.Write(part)
		if len(part) > 0 && part[len(part)-1] != '\n' {
			body.WriteByte('\n')
		}
		parts++
	}
	if parts == 0 {
		t.Fatal("the manifest lists no part")
	}
	sum := sha256.Sum256(body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != runnerTemplateSHA256 {
		t.Fatalf("runner template SHA-256 = %s, want %s", got, runnerTemplateSHA256)
	}
}
