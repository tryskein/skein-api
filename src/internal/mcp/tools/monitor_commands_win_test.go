package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"skein-api/internal/monitorassets"
	"skein-api/internal/monitorregistry"
)

// The Windows shells of row #88, each with the reader of its quoting rules
// (get_monitor_script_win_integ_test.go#parseGitBash, #parsePowerShell).
var winShells = []struct {
	shell string
	parse func(*testing.T, string) startCommand
}{
	{monitorregistry.WindowsShellGitBash, parseGitBash},
	{monitorregistry.WindowsShellPwsh, parsePowerShell},
	{monitorregistry.WindowsShellPowerShell, parsePowerShell},
}

// winTokenPath is the token path the mint side builds from gmsWinTokenDir.
var winTokenPath = windowsTokenFilePath(gmsWinTokenDir, "sk-1", "0011223344556677")

func winCommandInput(publicURL, loaderSHA256 string) clientCommandInput {
	return clientCommandInput{
		publicURL: publicURL, issueID: "sk-1", excludeSession: "s-1", tokenFilePath: winTokenPath,
		runnerSHA256: gmsTestRunnerSHA256, loaderSHA256: loaderSHA256, client: "win-agent", platform: "windows",
	}
}

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

// prepareMinPython is the lowest Python the Windows prepare runs on
// (assets/monitor_prepare_windows.py).
var prepareMinPython = [3]int{3, 12, 4}

// prepareVersionFixture is Python run before the prepare code that makes it
// see the lowest version it accepts.
var prepareVersionFixture = fmt.Sprintf("import sys\nsys.version_info = (%d, %d, %d, 'final', 0)\n",
	prepareMinPython[0], prepareMinPython[1], prepareMinPython[2])

// needPrepareVersion skips a case that runs prepare on the real interpreter
// without a version fixture (the Git Bash form takes no code before it)
// when that interpreter is below the prepare minimum.
func needPrepareVersion(t *testing.T) {
	t.Helper()
	needPython(t)
	out, err := exec.Command("python3", "-c",
		fmt.Sprintf("import sys; print(sys.version_info >= (%d, %d, %d))",
			prepareMinPython[0], prepareMinPython[1], prepareMinPython[2])).Output()
	if err != nil || strings.TrimSpace(string(out)) != "True" {
		t.Skipf("python3 below %d.%d.%d: the Git Bash prepare runs on the real interpreter",
			prepareMinPython[0], prepareMinPython[1], prepareMinPython[2])
	}
}

// TestWindowsCommands_OneLiteralPath — US-11 (row #88): what each shell's
// rules make of prepare and preflight is the token directory and the token
// path built from tokenDir, preflight runs the starter.py of that directory,
// every command sets its own SKEIN_* variables, and the Git Bash commands
// turn off the MSYS path conversion.
func TestWindowsCommands_OneLiteralPath(t *testing.T) {
	dir := winTokenPath[:strings.LastIndex(winTokenPath, `\`)]
	for _, sh := range winShells {
		t.Run(sh.shell, func(t *testing.T) {
			cmds := buildWindowsClientCommands(winCommandInput(gmsTestPublicURL, gmsTestLoaderSHA256), sh.shell)
			p := sh.parse(t, cmds.prepare)
			if p.vars["SKEIN_TOKEN_DIR"] != dir || p.vars["SKEIN_PUBLIC_URL"] != gmsTestPublicURL ||
				p.vars["SKEIN_LOADER_SHA256"] != gmsTestLoaderSHA256 {
				t.Fatalf("prepare sets %v", p.vars)
			}
			f := sh.parse(t, cmds.preflight)
			want := map[string]string{
				"SKEIN_MONITOR_MODE": "preflight", "SKEIN_PUBLIC_URL": gmsTestPublicURL, "SKEIN_ISSUE": "sk-1",
				"SKEIN_TOKEN_FILE": winTokenPath, "SKEIN_EXCLUDE_SESSION": "s-1",
				"SKEIN_RUNNER_SHA256": gmsTestRunnerSHA256, "SKEIN_LOADER_SHA256": gmsTestLoaderSHA256,
				"SKEIN_CLIENT": "win-agent", "SKEIN_PLATFORM": "windows", "SKEIN_SUBTREE": "", "SKEIN_EVENT_TYPES": "",
			}
			for k, v := range want {
				if got, ok := f.vars[k]; !ok || got != v {
					t.Fatalf("preflight: %s = %q (set %v), want %q", k, got, ok, v)
				}
			}
			if strings.Join(f.args, "\x00") != strings.Join([]string{"-3", dir + `\starter.py`}, "\x00") {
				t.Fatalf("preflight runs py %q", f.args)
			}
		})
	}
	for _, c := range []string{
		buildWindowsClientCommands(winCommandInput(gmsTestPublicURL, gmsTestLoaderSHA256), monitorregistry.WindowsShellGitBash).prepare,
		buildWindowsClientCommands(winCommandInput(gmsTestPublicURL, gmsTestLoaderSHA256), monitorregistry.WindowsShellGitBash).preflight,
	} {
		got := parseGitBashEnv(t, c)
		if got["MSYS_NO_PATHCONV"] != "1" || got["MSYS2_ARG_CONV_EXCL"] != "*" {
			t.Fatalf("a Git Bash command does not turn off the MSYS conversion: %v", got)
		}
	}
}

// parseGitBashEnv runs a Git Bash command with a POSIX sh and a fake py
// that prints its whole environment, NUL-separated.
func parseGitBashEnv(t *testing.T, command string) map[string]string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "py"), []byte("#!/bin/sh\nenv -0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	env := map[string]string{}
	for _, item := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		k, v, _ := strings.Cut(item, "=")
		env[k] = v
	}
	return env
}

// runPrepare runs a Windows prepare as its shell would hand it to Python,
// with a real python3: Git Bash through a POSIX sh and a py that drops -3,
// PowerShell by the text and variables its rules give, on the standard
// input of python3. pre is Python run before the prepare code (a fake
// version). It returns the stdout of Python.
func runPrepare(t *testing.T, shell, command string, extraEnv []string, pre string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "py"), []byte("#!/bin/sh\nshift\nexec python3 \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	if shell == monitorregistry.WindowsShellGitBash {
		cmd = exec.Command("sh", "-c", command)
		cmd.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH")}, extraEnv...)
		if pre != "" {
			t.Fatal("a fake version is given only through the standard input")
		}
	} else {
		parsed := parsePowerShell(t, command)
		if strings.Join(parsed.args, " ") != "-3 -" {
			t.Fatalf("PowerShell prepare does not pipe into py -3 -: %v", parsed.args)
		}
		cmd = exec.Command("python3", "-")
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, extraEnv...)
		for k, v := range parsed.vars {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdin = strings.NewReader(pre + parsed.stdin)
	}
	cmd.Dir = t.TempDir()
	out, _ := cmd.Output()
	return string(out)
}

// TestWindowsCommands_PrepareHandsTheStarterOn — US-11, R-10 (row #88):
// the Python code that each shell's rules make of prepare checks the
// version first and then runs the starter in prepare mode, which downloads
// the loader and hands it the text of starter.py: byte for byte the starter
// asset, with SKEIN_MONITOR_MODE=prepare. A Python below 3.12.4 gets
// prepare_failed with config_invalid before any download.
func TestWindowsCommands_PrepareHandsTheStarterOn(t *testing.T) {
	needPython(t)
	// The fake loader writes down what prepare handed it and prints the
	// ready line, as the loader's prepare mode does after its checks.
	loader := "import os\n" +
		"open(os.environ['OUT'], 'w').write(os.environ['SKEIN_MONITOR_MODE'] + '\\n' + os.environ['SKEIN_PREPARE_STARTER'])\n" +
		"print('{\"event\":\"prepare_ready\"}')\n"
	sum := sha256.Sum256([]byte(loader))
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/monitor-loader" {
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		_, _ = w.Write([]byte(loader))
	}))
	defer srv.Close()

	for _, sh := range winShells {
		t.Run(sh.shell, func(t *testing.T) {
			cmds := buildWindowsClientCommands(winCommandInput(srv.URL, hex.EncodeToString(sum[:])), sh.shell)
			outFile := filepath.Join(t.TempDir(), "handed")
			// The lowest accepted version: by a fixture on the standard
			// input, or the real interpreter for Git Bash.
			pre := prepareVersionFixture
			if sh.shell == monitorregistry.WindowsShellGitBash {
				needPrepareVersion(t)
				pre = ""
			}
			stdout := runPrepare(t, sh.shell, cmds.prepare, []string{"OUT=" + outFile}, pre)
			if strings.TrimSpace(stdout) != `{"event":"prepare_ready"}` {
				t.Fatalf("prepare printed %q", stdout)
			}
			handed, err := os.ReadFile(outFile)
			if err != nil {
				t.Fatalf("the loader got nothing from prepare: %v", err)
			}
			mode, text, _ := strings.Cut(string(handed), "\n")
			if mode != "prepare" {
				t.Fatalf("SKEIN_MONITOR_MODE in the loader = %q, want prepare", mode)
			}
			if text != monitorStarter(true) {
				t.Fatalf("the starter text prepare hands on differs from the starter asset")
			}
		})
	}

	for _, shell := range []string{monitorregistry.WindowsShellPwsh, monitorregistry.WindowsShellPowerShell} {
		t.Run("old python "+shell, func(t *testing.T) {
			before := downloads.Load()
			cmds := buildWindowsClientCommands(winCommandInput(srv.URL, hex.EncodeToString(sum[:])), shell)
			outFile := filepath.Join(t.TempDir(), "handed")
			stdout := runPrepare(t, shell, cmds.prepare, []string{"OUT=" + outFile},
				"import sys\nsys.version_info = (3, 12, 3, 'final', 0)\n")
			var line map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &line); err != nil {
				t.Fatalf("prepare printed %q", stdout)
			}
			if line["event"] != "prepare_failed" || line["reason"] != "config_invalid" {
				t.Fatalf("prepare on Python 3.12.3 printed %v", line)
			}
			if downloads.Load() != before {
				t.Fatalf("prepare downloaded the loader before refusing the version")
			}
			if _, err := os.Stat(outFile); err == nil {
				t.Fatalf("the loader ran on a refused version")
			}
		})
	}
}

// TestWindowsCommands_WorkingCommand — US-11 (row #88): the working command
// the runner prints on Windows, read by each shell's rules, runs the same
// starter.py with SKEIN_MONITOR_MODE=run, the number of the preflight
// result and every SKEIN_* variable the runner reads set explicitly, so a
// SKEIN_MONITOR_MODE=preflight left in a PowerShell session does not turn it
// into a preflight.
func TestWindowsCommands_WorkingCommand(t *testing.T) {
	needPython(t)
	src := filepath.Join(t.TempDir(), "runner_parts.py")
	if err := os.WriteFile(src, []byte(mustAssembleAssets(monitorassets.LoaderClient, monitorassets.LoaderClientDir,
		"parts.txt")), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"SKEIN_PUBLIC_URL": gmsTestPublicURL, "SKEIN_ISSUE": "sk-1", "SKEIN_TOKEN_FILE": winTokenPath,
		"SKEIN_EXCLUDE_SESSION": "s-1", "SKEIN_RUNNER_SHA256": gmsTestRunnerSHA256,
		"SKEIN_LOADER_SHA256": gmsTestLoaderSHA256, "SKEIN_CLIENT": "win-agent", "SKEIN_PLATFORM": "windows",
		"SKEIN_SUBTREE": "1", "SKEIN_EVENT_TYPES": "issue.closed",
	}
	envJSON, _ := json.Marshal(env)
	const script = `import json, sys
g = {"__name__": "working_command_under_test"}
exec(compile(open(sys.argv[1]).read(), "parts", "exec"), g)
agent = {"id": "win-agent"}
if sys.argv[2]:
    agent["windowsShell"] = sys.argv[2]
settings = g["Settings"](record=g["Record"]("win-agent", agent, None, []), platform="windows")
env = json.loads(sys.argv[3])
sys.stdout.write(g["working_command"](env, env["SKEIN_TOKEN_FILE"], 7, settings))
`
	dir := winTokenPath[:strings.LastIndex(winTokenPath, `\`)]
	for _, sh := range winShells {
		t.Run(sh.shell, func(t *testing.T) {
			named := sh.shell
			if named == monitorregistry.WindowsShellPowerShell {
				named = "" // a record without a named shell runs Windows PowerShell
			}
			out, err := exec.Command("python3", "-c", script, src, named, string(envJSON)).Output()
			if err != nil {
				t.Fatalf("working_command: %v", err)
			}
			got := sh.parse(t, string(out))
			want := map[string]string{"SKEIN_MONITOR_MODE": "run", "SKEIN_MONITOR_PREFLIGHT": "7",
				"SKEIN_MONITOR_CHANNEL": "", "SKEIN_MONITOR_BLIND": ""}
			for k, v := range env {
				want[k] = v
			}
			for k, v := range want {
				if g, ok := got.vars[k]; !ok || g != v {
					t.Fatalf("working command: %s = %q (set %v), want %q\n%s", k, g, ok, v, out)
				}
			}
			if strings.Join(got.args, "\x00") != strings.Join([]string{"-3", dir + `\starter.py`}, "\x00") {
				t.Fatalf("working command runs py %q", got.args)
			}
		})
	}
}

// TestWindowsCommands_TokenInstruction — US-11 (row #88, the rule of row
// #16): on Windows the Write of the token comes only after prepare's ready
// line, and a failed prepare has no Write step.
func TestWindowsCommands_TokenInstruction(t *testing.T) {
	u := newGMSUnit(gmsUnitRegistry(t))
	body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": "unit-agent", "platform": "windows",
		"tokenDir": gmsWinTokenDir}))
	instr, _ := body["tokenInstruction"].(string)
	ready, write, failed := strings.Index(instr, "prepare_ready"), strings.Index(instr, "Write"), strings.Index(instr, "prepare_failed")
	if ready < 0 || write < ready || failed < 0 {
		t.Fatalf("Windows tokenInstruction does not put the Write after the ready line:\n%s", instr)
	}
	if strings.Contains(strings.ToLower(instr[failed:]), "write") {
		t.Fatalf("the failed-prepare branch has a Write step:\n%s", instr)
	}
}
