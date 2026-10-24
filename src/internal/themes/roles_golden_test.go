package themes

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// goldenRetiredRolesPath is the rendered form of RetiredRoles. One line
// per retired name, tab-separated: name, successor ("" when none), in
// name order.
const goldenRetiredRolesPath = "testdata/retired_roles.tsv"

// updateRetiredGoldenEnv rewrites the golden instead of asserting it.
// Same shape and same reasoning as updateGoldenEnv: regeneration must be
// a sentence someone typed on purpose.
const updateRetiredGoldenEnv = "SKEIN_UPDATE_RETIRED_ROLES"

// renderRetiredRoles renders RetiredRoles in the golden's format.
// Sorted by name rather than left in the declaration order of roles.go,
// because the golden's job is to be DIFFED — against a previous version
// of itself and, at the workspace root, against a table written on the
// other side of the repository boundary in a different order.
func renderRetiredRoles() string {
	names := make([]string, 0, len(RetiredRoles))
	for name := range RetiredRoles {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s\t%s\n", name, RetiredRoles[name])
	}
	return b.String()
}

// TestRetiredRolesGolden pins the retired-role table byte for byte, and
// exists for a reason the in-repo guards cannot cover.
//
// What no in-repo check can see is whether `--bg-hover`'s successor
// is `--elev` rather than `--surface`: both are live roles, both satisfy
// every check this repository can make, and only one of them is what the
// frontend's `LEGACY_TO_NEW` actually says. Get it wrong and the warning
// this table exists to produce sends an agent at the wrong role —
// confidently, which is worse than saying nothing.
//
// So the table is rendered to a file that `scripts/check-theme-mirror.sh`
// at the workspace root compares against the frontend's own declaration,
// exactly as it already does for the token catalogue. This test is what
// makes that substitution honest: without it the script would compare
// the frontend against a FILE, and a stale file would let a real drift
// pass. The script runs this test first, for that reason.
//
// Regenerate deliberately, then read the diff:
//
//	SKEIN_UPDATE_RETIRED_ROLES=1 go test ./internal/themes/ -run TestRetiredRolesGolden
func TestRetiredRolesGolden(t *testing.T) {
	got := renderRetiredRoles()

	if os.Getenv(updateRetiredGoldenEnv) == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenRetiredRolesPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenRetiredRolesPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("%s rewritten (%d roles) — review the diff before committing",
			goldenRetiredRolesPath, len(RetiredRoles))
		return
	}

	raw, err := os.ReadFile(goldenRetiredRolesPath)
	if err != nil {
		t.Fatalf("read %s: %v\nregenerate with: %s=1 go test ./internal/themes/ -run TestRetiredRolesGolden",
			goldenRetiredRolesPath, err, updateRetiredGoldenEnv)
	}
	want := string(raw)
	// Vacuity guard, as on the catalogue golden: an empty file compares
	// as "everything was removed", which reads as a catastrophe rather
	// than as a truncated guard. Say which it is.
	if strings.TrimSpace(want) == "" {
		t.Fatalf("%s is empty — the guard pins nothing; regenerate with: %s=1 go test ./internal/themes/ -run TestRetiredRolesGolden",
			goldenRetiredRolesPath, updateRetiredGoldenEnv)
	}
	if got == want {
		return
	}

	// Per-line diff rather than a byte complaint: the failure has to
	// name the role and the direction, or the next reader re-derives it.
	wantRows := parseRetiredGolden(want)
	gotRows := parseRetiredGolden(got)
	for name, wantSucc := range wantRows {
		gotSucc, ok := gotRows[name]
		if !ok {
			t.Errorf("%s: role %q is in the golden but not in RetiredRoles", goldenRetiredRolesPath, name)
			continue
		}
		if gotSucc != wantSucc {
			t.Errorf("%s: successor of %q moved %q → %q", goldenRetiredRolesPath, name, wantSucc, gotSucc)
		}
	}
	for name := range gotRows {
		if _, ok := wantRows[name]; !ok {
			t.Errorf("%s: RetiredRoles gained %q, which the golden does not carry", goldenRetiredRolesPath, name)
		}
	}
	t.Errorf("regenerate with: %s=1 go test ./internal/themes/ -run TestRetiredRolesGolden",
		updateRetiredGoldenEnv)
}

func parseRetiredGolden(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			out[parts[0]] = ""
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}
