package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// runServedPrepare runs the prepare command exactly as a response with
// `client` serves it (buildClientCommands) under sh with the real python3,
// for a token file in tokenDir; it returns the exit code and the JSON lines
// prepare printed.
func runServedPrepare(t *testing.T, tokenDir string) (int, []map[string]any) {
	t.Helper()
	cmds := buildClientCommands(clientCommandInput{
		publicURL: gmsTestPublicURL, issueID: "sk-1", excludeSession: "s",
		tokenFilePath: filepath.Join(tokenDir, "token"), runnerSHA256: gmsTestRunnerSHA256,
		client: "some-agent", platform: monitorPlatformPOSIX,
	})
	cmd := exec.Command("sh", "-c", cmds.prepare)
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run prepare: %v", err)
		}
		code = exitErr.ExitCode()
	}
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("prepare printed a line that is not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	return code, lines
}

func prepareEvents(lines []map[string]any) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i], _ = l["event"].(string)
	}
	return out
}

// loaderModeOf asks python3 what the loader file assigns to
// _SKEIN_LOADER_MODE (by its syntax tree, without running it).
func loaderModeOf(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("python3", "-c", `import ast, sys
tree = ast.parse(open(sys.argv[1]).read())
for node in tree.body:
    if isinstance(node, ast.Assign) and any(getattr(x, "id", "") == "_SKEIN_LOADER_MODE" for x in node.targets):
        print(ast.literal_eval(node.value))
`, path).Output()
	if err != nil {
		t.Fatalf("read the loader mode: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func requirePython(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"sh", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

// TestServedPrepare_POSIX — П-3 (row #18): the prepare command the server
// serves creates the token directory new with mode 0700, owned by the
// caller, writes the client-mode loader into it and prints the ready line;
// an existing directory, private or not, is refused without the ready line
// and left as it was.
func TestServedPrepare_POSIX(t *testing.T) {
	requirePython(t)

	t.Run("new directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skein-mon-sk-1-0123456789abcdef")
		code, lines := runServedPrepare(t, dir)
		if code != 0 || strings.Join(prepareEvents(lines), ",") != "prepare_ready" {
			t.Fatalf("prepare exit %d, lines %v; want 0 and only prepare_ready", code, lines)
		}
		st, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("token directory: %v", err)
		}
		if !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("token directory mode = %v, want a 0700 directory", st.Mode())
		}
		if uid := st.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Getuid() {
			t.Fatalf("token directory owned by %d, want %d", uid, os.Getuid())
		}
		loader := filepath.Join(dir, "loader.py")
		if lines[0]["loader"] != loader {
			t.Fatalf("ready line names loader %v, want %s", lines[0]["loader"], loader)
		}
		if mode := loaderModeOf(t, loader); mode != "client" {
			t.Fatalf("the written loader sets mode %q, want client", mode)
		}
	})

	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{{"existing private directory", 0o700}, {"existing open directory", 0o755}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "skein-mon-sk-1-taken")
			if err := os.Mkdir(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			code, lines := runServedPrepare(t, dir)
			if code != 1 || strings.Join(prepareEvents(lines), ",") != "prepare_failed" {
				t.Fatalf("prepare exit %d, lines %v; want 1 and only prepare_failed", code, lines)
			}
			if lines[0]["reason"] != "token_dir_exists" {
				t.Fatalf("refusal reason = %v, want token_dir_exists", lines[0]["reason"])
			}
			st, err := os.Lstat(dir)
			if err != nil || st.Mode().Perm() != tc.mode {
				t.Fatalf("existing directory changed: %v %v", st, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("prepare wrote into an existing directory: %v", entries)
			}
		})
	}
}
