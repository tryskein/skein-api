package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/monitorcreds"
	"skein-api/internal/monitorregistry"
	"skein-api/internal/notifications"
)

// Records of gmsWinIntegRegistry, one per Windows shell the commands are
// written for: Git Bash, pwsh, and a record that names no shell, which runs
// Windows PowerShell (US-11).
const (
	gmsWinGitBashClient    = "integ-gitbash"
	gmsWinPwshClient       = "integ-pwsh"
	gmsWinPowerShellClient = "integ-powershell"
)

// gmsWinTokenDir is a tokenDir with a space, Cyrillic, an apostrophe and a
// typographic apostrophe, which PowerShell also takes as a quote.
const gmsWinTokenDir = `C:\Users\Иван O'Brien’s\AppData\Local`

func gmsWinIntegRegistry(t *testing.T) *monitorregistry.Registry {
	t.Helper()
	record := func(id, shell string) string {
		field := ""
		if shell != "" {
			field = `"windowsShell": "` + shell + `", `
		}
		return `{"id": "` + id + `", "about": "integ fixture agent", "source": "integ fixture", ` + field +
			`"process": {"executables": ["` + id + `"]}, "methods": []}`
	}
	reg, err := monitorregistry.Load([]byte(`{
  "schemaVersion": 1,
  "general": {"about": "general", "source": "integ fixture", "warnings": ["not found"]},
  "agents": [` + record(gmsWinGitBashClient, "git-bash") + `, ` + record(gmsWinPwshClient, "pwsh") + `, ` +
		record(gmsWinPowerShellClient, "") + `],
  "channels": [],
  "shells": {"source": "integ fixture", "names": ["sh"]},
  "zavodEngines": {}
}`))
	if err != nil {
		t.Fatalf("fixture registry: %v", err)
	}
	return reg
}

// newGMSWinIntegFixture is newGMSIntegFixture on gmsWinIntegRegistry.
func newGMSWinIntegFixture(t *testing.T) *gmsIntegFixture {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gmw")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "monitor me", "open", 2)
	keyID := seedAPIKeyForTool(t, pool, userID)
	h := handleGetMonitorScript(poolIssueResolver{pool: pool}, notifications.NewStore(pool),
		monitorcreds.New(pool, 6*time.Hour), gmsWinIntegRegistry(t), gmsTestPublicURL, gmsTestRunnerSHA256, gmsTestLoaderSHA256)
	ctx := withCtxKey(userID, "user", keyID)
	return &gmsIntegFixture{
		pool: pool, userID: userID, issueID: issueID, ctx: ctx,
		handle: func(args map[string]any) *mcpgo.CallToolResult {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = args
			res, err := h(ctx, req)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			return res
		},
	}
}

// startCommand is what a start command hands Python: the variables it sets
// and the arguments after the interpreter, plus the text piped into it.
type startCommand struct {
	vars  map[string]string
	args  []string
	stdin string
}

// fakePy is a `py` launcher that runs nothing: it prints its arguments and
// the SKEIN_* variables it inherited, NUL-separated.
const fakePy = `#!/bin/sh
for a in "$@"; do printf 'ARG=%s\0' "$a"; done
for v in $(env | sed -n 's/^\(SKEIN_[A-Z0-9_]*\)=.*/\1/p'); do
	eval "printf '%s=%s\0' \"\$v\" \"\${$v}\""
done
`

// parseGitBash runs a Git Bash command with a POSIX sh, whose quoting rules
// Git Bash shares, and a fake py first on PATH.
func parseGitBash(t *testing.T, command string) startCommand {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Fatal("sh is needed to read a Git Bash command")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "py"), []byte(fakePy), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("command failed: %v\n%s", err, command)
	}
	got := startCommand{vars: map[string]string{}}
	for _, item := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if a, ok := strings.CutPrefix(item, "ARG="); ok {
			got.args = append(got.args, a)
			continue
		}
		k, v, _ := strings.Cut(item, "=")
		got.vars[k] = v
	}
	return got
}

// psQuotes are the characters PowerShell takes as the quote of a
// single-quoted string (about_Quoting_Rules: the apostrophe and the
// typographic single quotes U+2018-U+201B).
func psQuote(r rune) bool { return r == '\'' || (r >= '\u2018' && r <= '\u201b') }

// psSingleQuoted reads the single-quoted string that starts at s[0]: inside
// it a quote followed by another quote stands for one quote, any other
// quote ends it. It returns the value and the rest after the closing quote.
func psSingleQuoted(t *testing.T, s []rune) (string, []rune) {
	t.Helper()
	if len(s) == 0 || !psQuote(s[0]) {
		t.Fatalf("no single-quoted string at %q", string(s))
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if !psQuote(s[i]) {
			b.WriteRune(s[i])
			continue
		}
		if i+1 < len(s) && psQuote(s[i+1]) {
			b.WriteRune(s[i])
			i++
			continue
		}
		return b.String(), s[i+1:]
	}
	t.Fatalf("unterminated single-quoted string %q", string(s))
	return "", nil
}

// parsePowerShell reads a PowerShell start command of the forms the server
// writes: statements `$env:NAME='value';` and then either
// `py -3 'file'` or `'text' | py -3 -`. Any other text fails the test, so
// a command outside these forms is not read loosely.
func parsePowerShell(t *testing.T, command string) startCommand {
	t.Helper()
	got := startCommand{vars: map[string]string{}}
	rest := []rune(command)
	skip := func(lit string) bool {
		if strings.HasPrefix(string(rest), lit) {
			rest = rest[len([]rune(lit)):]
			return true
		}
		return false
	}
	for skip("$env:") {
		eq := strings.IndexRune(string(rest), '=')
		if eq < 0 {
			t.Fatalf("no = after $env: in %q", command)
		}
		name := string(rest)[:eq]
		rest = rest[len([]rune(name))+1:]
		var value string
		value, rest = psSingleQuoted(t, rest)
		got.vars[name] = value
		if !skip("; ") {
			t.Fatalf("no ; after $env:%s in %q", name, command)
		}
	}
	switch {
	case skip("py -3 "):
		var file string
		file, rest = psSingleQuoted(t, rest)
		got.args = []string{"-3", file}
	case len(rest) > 0 && psQuote(rest[0]):
		got.stdin, rest = psSingleQuoted(t, rest)
		if !skip(" | py -3 -") {
			t.Fatalf("the piped text does not go to py -3 - in %q", command)
		}
		got.args = []string{"-3", "-"}
	default:
		t.Fatalf("unknown command form %q", command)
	}
	if len(rest) != 0 {
		t.Fatalf("text after the command: %q", string(rest))
	}
	return got
}

// TestGetMonitorScriptWinInteg_OneLiteralPathInEveryShell — R-7, US-11
// (row #131): one real mint per Windows shell with a tokenDir holding a
// space, Cyrillic and apostrophes. The response's tokenFilePath equals the
// token path the credential row records and the literal path that the
// shell's own rules make of the prepare and preflight commands; the
// starter path is the same directory; there is no bootstrap key.
func TestGetMonitorScriptWinInteg_OneLiteralPathInEveryShell(t *testing.T) {
	cases := []struct {
		client string
		parse  func(*testing.T, string) startCommand
	}{
		{gmsWinGitBashClient, parseGitBash},
		{gmsWinPwshClient, parsePowerShell},
		{gmsWinPowerShellClient, parsePowerShell},
	}
	for _, tc := range cases {
		t.Run(tc.client, func(t *testing.T) {
			f := newGMSWinIntegFixture(t)
			res := f.handle(map[string]any{"id": f.issueID, "client": tc.client, "platform": "windows",
				"tokenDir": gmsWinTokenDir})
			if res.IsError {
				t.Fatalf("call refused: %v", extractToolJSON(t, res))
			}
			body := extractToolJSON(t, res)
			tokenFilePath, _ := body["tokenFilePath"].(string)
			if !strings.HasPrefix(tokenFilePath, gmsWinTokenDir+`\skein-mon-`) {
				t.Fatalf("tokenFilePath %q is not inside tokenDir", tokenFilePath)
			}
			token, _ := body["token"].(string)
			if got := f.tokenFilePathOf(t, token); got != tokenFilePath {
				t.Fatalf("monitor_credentials.token_file_path = %q, response tokenFilePath = %q", got, tokenFilePath)
			}
			if _, ok := body["bootstrap"]; ok {
				t.Fatalf("a call with client carries bootstrap")
			}
			tokenDir := tokenFilePath[:strings.LastIndex(tokenFilePath, `\`)]

			prepare, _ := body["prepare"].(string)
			pc := tc.parse(t, prepare)
			if pc.vars["SKEIN_TOKEN_DIR"] != tokenDir {
				t.Fatalf("prepare: SKEIN_TOKEN_DIR = %q, want the directory of tokenFilePath %q",
					pc.vars["SKEIN_TOKEN_DIR"], tokenDir)
			}

			preflight, _ := body["preflight"].(string)
			fc := tc.parse(t, preflight)
			if fc.vars["SKEIN_TOKEN_FILE"] != tokenFilePath {
				t.Fatalf("preflight: SKEIN_TOKEN_FILE = %q, want tokenFilePath %q", fc.vars["SKEIN_TOKEN_FILE"], tokenFilePath)
			}
			if fc.vars["SKEIN_MONITOR_MODE"] != "preflight" || fc.vars["SKEIN_PLATFORM"] != "windows" {
				t.Fatalf("preflight: mode %q platform %q", fc.vars["SKEIN_MONITOR_MODE"], fc.vars["SKEIN_PLATFORM"])
			}
			if want := []string{"-3", tokenDir + `\starter.py`}; strings.Join(fc.args, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("preflight runs %q, want py %q", fc.args, want)
			}
		})
	}
}
