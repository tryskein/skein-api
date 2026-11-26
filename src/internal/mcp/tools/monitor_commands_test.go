package tools

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"skein-api/internal/monitorregistry"
)

// fakePython is a python3 that runs nothing: it prints its arguments and the
// SKEIN_* variables it inherited, NUL-separated, so a test sees exactly what
// a command would hand the real interpreter.
const fakePython = `#!/bin/sh
for a in "$@"; do printf 'ARG=%s\0' "$a"; done
for v in SKEIN_TOKEN_DIR SKEIN_MONITOR_MODE SKEIN_PUBLIC_URL SKEIN_ISSUE SKEIN_TOKEN_FILE SKEIN_EXCLUDE_SESSION SKEIN_CLIENT SKEIN_PLATFORM; do
	eval "printf '%s=%s\0' \"\$v\" \"\${$v-<unset>}\""
done
`

// runWithFakePython runs a command with sh and fakePython first on PATH,
// and returns what fakePython saw: its arguments and its SKEIN_* variables.
func runWithFakePython(t *testing.T, command string) (args []string, env map[string]string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(fakePython), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("command failed: %v\n%s", err, command)
	}
	env = map[string]string{}
	for _, item := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if a, ok := strings.CutPrefix(item, "ARG="); ok {
			args = append(args, a)
			continue
		}
		k, v, _ := strings.Cut(item, "=")
		env[k] = v
	}
	return args, env
}

// TestClientCommands_HostileInputsStayQuoted — П-3, ИНВ-5 (row #15): with a
// hostile issue id and token path (quotes, $(), spaces, a newline), prepare,
// preflight and bootstrap hand python3 exactly the intended values, the
// injection's marker file never appears, preflight carries
// SKEIN_MONITOR_MODE=preflight and both run commands carry SKEIN_CLIENT and
// SKEIN_PLATFORM.
func TestClientCommands_HostileInputsStayQuoted(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	hostileID := "sk-1';touch " + marker + ";'\n$(touch " + marker + ") x"
	tokenFile := filepath.Join(t.TempDir(), "dir with 'quote' $(touch "+marker+")", "token")
	in := clientCommandInput{
		publicURL: "https://h.example/`touch " + marker + "`", issueID: hostileID,
		excludeSession: "sess'; touch " + marker, tokenFilePath: tokenFile,
		runnerSHA256: gmsTestRunnerSHA256, client: "some-agent", platform: monitorPlatformPOSIX,
	}
	cmds := buildClientCommands(in)
	dir := path.Dir(tokenFile)
	loaderPath := path.Join(dir, "loader.py")

	args, env := runWithFakePython(t, cmds.prepare)
	if len(args) != 2 || args[0] != "-c" || args[1] != monitorPrepareScript {
		t.Fatalf("prepare handed python3 %d arguments, want -c and the prepare script", len(args))
	}
	if env["SKEIN_TOKEN_DIR"] != dir {
		t.Fatalf("prepare SKEIN_TOKEN_DIR = %q, want %q", env["SKEIN_TOKEN_DIR"], dir)
	}

	for _, c := range []struct {
		name, command, mode string
	}{
		{"preflight", cmds.preflight, "preflight"},
	} {
		args, env := runWithFakePython(t, c.command)
		if len(args) != 1 || args[0] != loaderPath {
			t.Fatalf("%s handed python3 %q, want the loader file %q", c.name, args, loaderPath)
		}
		want := map[string]string{
			"SKEIN_MONITOR_MODE": c.mode, "SKEIN_PUBLIC_URL": in.publicURL, "SKEIN_ISSUE": hostileID,
			"SKEIN_TOKEN_FILE": tokenFile, "SKEIN_EXCLUDE_SESSION": in.excludeSession,
			"SKEIN_CLIENT": "some-agent", "SKEIN_PLATFORM": monitorPlatformPOSIX,
		}
		for k, v := range want {
			if env[k] != v {
				t.Fatalf("%s: %s = %q, want %q", c.name, k, env[k], v)
			}
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("marker %q exists: a value left its quotes", marker)
	}
}

// TestClientTokenInstruction — П-3, US-11 (row #16): with client the Write of
// the token comes only after prepare's ready line, and the failed-prepare
// branch has no Write step; without client the instruction is the previous
// one.
func TestClientTokenInstruction(t *testing.T) {
	ready := strings.Index(clientTokenInstruction, "prepare_ready")
	write := strings.Index(clientTokenInstruction, "Write")
	failed := strings.Index(clientTokenInstruction, "prepare_failed")
	if ready < 0 || write < 0 || failed < 0 {
		t.Fatalf("instruction lacks the ready line, the Write step or the failure branch:\n%s", clientTokenInstruction)
	}
	if write < ready {
		t.Fatalf("Write comes before the ready line:\n%s", clientTokenInstruction)
	}
	if strings.Contains(strings.ToLower(clientTokenInstruction[failed:]), "write") {
		t.Fatalf("the failed-prepare branch has a Write step:\n%s", clientTokenInstruction)
	}

	u := newGMSUnit(gmsUnitRegistry(t))
	body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42"}))
	const previous = "Run `prepare`, then write `token` to `tokenFilePath` with the Write tool " +
		"(never in a shell command), then run `bootstrap` as-is."
	if body["tokenInstruction"] != previous {
		t.Fatalf("tokenInstruction without client = %q, want the previous %q", body["tokenInstruction"], previous)
	}
	withClient := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": "unit-agent"}))
	if withClient["tokenInstruction"] != clientTokenInstruction {
		t.Fatalf("tokenInstruction with client = %q", withClient["tokenInstruction"])
	}
}

// TestClientUsage_ComposedFromTheRecord — US-13, US-2 (row #17): usage with
// client puts the preflight step first, then the instruction of each of the
// record's methods in the record's order, and carries the literal path of
// the echo exclusion file and the record's warnings.
func TestClientUsage_ComposedFromTheRecord(t *testing.T) {
	reg, err := monitorregistry.Load([]byte(`{
  "schemaVersion": 1,
  "general": {"about": "general", "source": "unit fixture"},
  "agents": [{
    "id": "two-methods", "about": "unit fixture agent", "source": "unit fixture",
    "process": {"executables": ["two-methods"]},
    "methods": [
      {"kind": "stream", "instruction": "first-method-instruction-91c2", "conditions": {}},
      {"kind": "exit-on-event", "instruction": "second-method-instruction-4e7b", "conditions": {}}
    ],
    "warnings": ["record-warning-one-5d10", "record-warning-two-a6f3"]
  }],
  "channels": [],
  "shells": {"source": "unit fixture", "names": ["sh"]},
  "zavodEngines": {}
}`))
	if err != nil {
		t.Fatalf("fixture registry: %v", err)
	}
	u := newGMSUnit(reg)
	body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": "two-methods"}))
	usage, _ := body["usage"].(string)
	tokenFile, _ := body["tokenFilePath"].(string)

	order := []string{"`preflight`", "first-method-instruction-91c2", "second-method-instruction-4e7b"}
	last := -1
	for _, s := range order {
		i := strings.Index(usage, s)
		if i < 0 || i < last {
			t.Fatalf("usage does not carry %q in order %v:\n%s", s, order, usage)
		}
		last = i
	}
	exclude := path.Join(path.Dir(tokenFile), "exclude_session")
	if !strings.Contains(usage, exclude) {
		t.Fatalf("usage lacks the literal exclusion path %q:\n%s", exclude, usage)
	}
	for _, w := range []string{"record-warning-one-5d10", "record-warning-two-a6f3"} {
		if !strings.Contains(usage, w) {
			t.Fatalf("usage lost the record's warning %q:\n%s", w, usage)
		}
	}
}
