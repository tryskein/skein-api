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

// preparedInput is the input of the prepare runs below: every value a
// response with `client` carries, so the ready line can be checked for not
// repeating any of them.
func preparedInput(tokenDir string) clientCommandInput {
	return clientCommandInput{
		publicURL: gmsTestPublicURL, issueID: "sk-1", excludeSession: "prepare-session",
		tokenFilePath: filepath.Join(tokenDir, "token"), runnerSHA256: gmsTestRunnerSHA256,
		loaderSHA256: gmsTestLoaderSHA256, client: "some-agent", platform: monitorPlatformPOSIX,
	}
}

// runServedPrepare runs the prepare command exactly as a response with
// `client` serves it (buildClientCommands) under sh with the real python3,
// for a token file in tokenDir; it returns the exit code and the JSON lines
// prepare printed. extraEnv is added to the environment of sh.
func runServedPrepare(t *testing.T, tokenDir string, extraEnv ...string) (int, []map[string]any) {
	code, out := runServedPrepareRaw(t, tokenDir, extraEnv...)
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
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

// runServedPrepareRaw is runServedPrepare with the raw stdout.
func runServedPrepareRaw(t *testing.T, tokenDir string, extraEnv ...string) (int, string) {
	t.Helper()
	cmds := buildClientCommands(preparedInput(tokenDir))
	cmd := exec.Command("sh", "-c", cmds.prepare)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.Output()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run prepare: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return code, string(out)
}

func prepareEvents(lines []map[string]any) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i], _ = l["event"].(string)
	}
	return out
}

func requirePython(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"sh", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

// TestServedPrepare_POSIX — П-3, R-9, R-10 (rows #18, #140): the prepare
// command the server serves checks the Python version first (an old one
// gets prepare_failed with config_invalid and nothing is created), creates
// the token directory new with mode 0700, owned by the caller, writes into
// it only starter.py, byte for byte the starter of a call with `client`, and
// prints one short ready line that repeats nothing of the response; an
// existing directory, private or not, is refused without the ready line and
// left as it was.
func TestServedPrepare_POSIX(t *testing.T) {
	requirePython(t)

	t.Run("Python below the minimum", func(t *testing.T) {
		// sitecustomize runs before the -c script: the interpreter reports
		// 3.5, the version the minimum (3.6) refuses.
		site := t.TempDir()
		if err := os.WriteFile(filepath.Join(site, "sitecustomize.py"),
			[]byte("import sys\nsys.version_info = (3, 5, 9, 'final', 0)\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "skein-mon-sk-1-0123456789abcdef")
		code, lines := runServedPrepare(t, dir, "PYTHONPATH="+site)
		if code != 1 || strings.Join(prepareEvents(lines), ",") != "prepare_failed" {
			t.Fatalf("prepare exit %d, lines %v; want 1 and only prepare_failed", code, lines)
		}
		if lines[0]["reason"] != "config_invalid" {
			t.Fatalf("refusal reason = %v, want config_invalid", lines[0]["reason"])
		}
		if action, _ := lines[0]["action"].(string); !strings.Contains(action, "3.6") {
			t.Fatalf("the action does not name the minimum version: %q", action)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("an old Python still created the token directory (lstat: %v)", err)
		}
	})

	t.Run("new directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skein-mon-sk-1-0123456789abcdef")
		code, raw := runServedPrepareRaw(t, dir)
		if code != 0 {
			t.Fatalf("prepare exit %d, stdout %q; want 0", code, raw)
		}
		// One line of at most 128 bytes (SC-012) that repeats no value of
		// the response (INV-17).
		ready := strings.TrimSuffix(raw, "\n")
		if strings.Contains(ready, "\n") || len(ready) > 128 {
			t.Fatalf("ready output = %q (%d bytes); want one line of at most 128 bytes", raw, len(ready))
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(ready), &line); err != nil || line["event"] != "prepare_ready" {
			t.Fatalf("ready line = %q; want the prepare_ready event", ready)
		}
		in := preparedInput(dir)
		for _, v := range []string{dir, in.tokenFilePath, in.publicURL, in.issueID, in.excludeSession,
			in.runnerSHA256, in.loaderSHA256, in.client, "starter.py"} {
			if strings.Contains(ready, v) {
				t.Fatalf("ready line %q repeats %q", ready, v)
			}
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
		// Only starter.py: no loader file, no temporary file left behind.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read the token directory: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != "starter.py" {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("token directory holds %v, want only starter.py", names)
		}
		got, err := os.ReadFile(filepath.Join(dir, "starter.py"))
		if err != nil {
			t.Fatalf("read starter.py: %v", err)
		}
		if string(got) != monitorStarter(true) {
			t.Fatalf("starter.py (%d bytes) is not the starter of a call with client (%d bytes)",
				len(got), len(monitorStarter(true)))
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
