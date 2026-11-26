package tools

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/monitorcreds"
	"skein-api/internal/monitorregistry"
)

// The three sides of the monitor tool, faked: each counts its calls so a
// test can tell whether the handler reached it.
type fakeMonitorIssues struct{ calls int }

func (f *fakeMonitorIssues) ResolveVisibleIssueProject(context.Context, string, string, uuid.UUID) (uuid.UUID, *mcpgo.CallToolResult) {
	f.calls++
	return uuid.New(), nil
}

type fakeMonitorWatcher struct{ calls int }

func (f *fakeMonitorWatcher) AddIssueWatch(context.Context, string, uuid.UUID, bool) error {
	f.calls++
	return nil
}

type fakeMonitorMinter struct{ calls int }

func (f *fakeMonitorMinter) Mint(context.Context, monitorcreds.MintParams) (string, monitorcreds.Credential, error) {
	f.calls++
	return monitorcreds.TokenTag + strings.Repeat("a", 43), monitorcreds.Credential{ID: uuid.New()}, nil
}

// gmsUnitRegistry has one agent record with a warning, and a general
// record with a warning of its own; the values describe no real agent.
func gmsUnitRegistry(t *testing.T) *monitorregistry.Registry {
	t.Helper()
	reg, err := monitorregistry.Load([]byte(`{
  "schemaVersion": 1,
  "general": {"about": "general", "source": "unit fixture", "warnings": ["unit-general-warning-7f3a"]},
  "agents": [{
    "id": "unit-agent", "about": "unit fixture agent", "source": "unit fixture",
    "process": {"executables": ["unit-agent"]},
    "methods": [{"kind": "stream", "instruction": "unit-stream-instruction", "conditions": {}}],
    "warnings": ["unit-agent-warning-2c9d"]
  }],
  "channels": [],
  "shells": {"source": "unit fixture", "names": ["sh"]},
  "zavodEngines": {}
}`))
	if err != nil {
		t.Fatalf("fixture registry: %v", err)
	}
	return reg
}

type gmsUnit struct {
	issues  *fakeMonitorIssues
	watcher *fakeMonitorWatcher
	minter  *fakeMonitorMinter
	handler server.ToolHandlerFunc
	ctx     context.Context
}

func newGMSUnit(reg *monitorregistry.Registry) *gmsUnit {
	u := &gmsUnit{issues: &fakeMonitorIssues{}, watcher: &fakeMonitorWatcher{}, minter: &fakeMonitorMinter{}}
	u.handler = handleGetMonitorScript(u.issues, u.watcher, u.minter, reg, gmsTestPublicURL, gmsTestRunnerSHA256, gmsTestLoaderSHA256)
	u.ctx = withCtxKey(uuid.New(), "user", uuid.New())
	return u
}

func (u *gmsUnit) call(t *testing.T, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := u.handler(u.ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return res
}

// TestGetMonitorScriptClient_RefusedBeforeSideEffects — API table, SC-008
// (row #12): every invalid client, platform or tokenDir is a
// validation_error with its code, and the handler reached neither the issue
// lookup, nor the watch, nor the mint.
func TestGetMonitorScriptClient_RefusedBeforeSideEffects(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantCode string
	}{
		{"client of the wrong type", map[string]any{"client": []any{"x"}}, monitorCodeClientInvalid},
		{"empty client", map[string]any{"client": ""}, monitorCodeClientInvalid},
		{"client longer than 64", map[string]any{"client": strings.Repeat("a", 65)}, monitorCodeClientInvalid},
		{"client with an upper-case letter", map[string]any{"client": "Codex"}, monitorCodeClientInvalid},
		{"client with an underscore", map[string]any{"client": "my_agent"}, monitorCodeClientInvalid},
		{"platform of the wrong type", map[string]any{"client": "x", "platform": 1}, monitorCodePlatformInvalid},
		{"empty platform", map[string]any{"client": "x", "platform": ""}, monitorCodePlatformInvalid},
		{"platform outside posix and windows", map[string]any{"client": "x", "platform": "linux"}, monitorCodePlatformInvalid},
		{"platform without client", map[string]any{"platform": "windows"}, monitorCodePlatformWithoutClient},
		{"tokenDir without client", map[string]any{"tokenDir": `C:\Users\me`}, monitorCodeTokenDirWithoutClient},
		{"tokenDir without platform=windows", map[string]any{"client": "x", "platform": "posix", "tokenDir": `C:\Users\me`}, monitorCodeTokenDirNotWindows},
		{"windows without tokenDir", map[string]any{"client": "x", "platform": "windows"}, monitorCodeTokenDirRequired},
		{"tokenDir of the wrong type", map[string]any{"client": "x", "platform": "windows", "tokenDir": false}, monitorCodeTokenDirInvalid},
		{"empty tokenDir", map[string]any{"client": "x", "platform": "windows", "tokenDir": ""}, monitorCodeTokenDirInvalid},
		{"relative tokenDir", map[string]any{"client": "x", "platform": "windows", "tokenDir": `AppData\Local`}, monitorCodeTokenDirInvalid},
		{"UNC tokenDir", map[string]any{"client": "x", "platform": "windows", "tokenDir": `\\host\share\dir`}, monitorCodeTokenDirInvalid},
		{"tokenDir with ..", map[string]any{"client": "x", "platform": "windows", "tokenDir": `C:\Users\me\..\..\Windows`}, monitorCodeTokenDirInvalid},
		{"tokenDir with a control character", map[string]any{"client": "x", "platform": "windows", "tokenDir": "C:\\Users\\me\ttab"}, monitorCodeTokenDirInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newGMSUnit(gmsUnitRegistry(t))
			args := map[string]any{"id": "sk-42"}
			for k, v := range tc.args {
				args[k] = v
			}
			res := u.call(t, args)
			if !res.IsError {
				t.Fatalf("call succeeded, want validation_error %s", tc.wantCode)
			}
			body := extractToolJSON(t, res)
			if body["error"] != ErrCodeValidationError || body["code"] != tc.wantCode {
				t.Fatalf("error = %v code = %v, want %s %s", body["error"], body["code"], ErrCodeValidationError, tc.wantCode)
			}
			if u.issues.calls+u.watcher.calls+u.minter.calls != 0 {
				t.Fatalf("refusal reached the sides: issue lookup %d, watch %d, mint %d",
					u.issues.calls, u.watcher.calls, u.minter.calls)
			}
		})
	}
}

// TestGetMonitorScriptClient_WithoutClientUnchanged — US-1, SC-004, ИНВ-7
// (row #13): without client, platform and tokenDir the response has exactly
// the fields it had before them, the bootstrap keeps its inline-loader form,
// and the usage and the tool schema point an agent at `client`.
func TestGetMonitorScriptClient_WithoutClientUnchanged(t *testing.T) {
	u := newGMSUnit(gmsUnitRegistry(t))
	res := u.call(t, map[string]any{"id": "sk-42"})
	if res.IsError {
		t.Fatalf("call refused: %v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"bootstrap", "eventTypes", "issueId", "monitorId", "prepare", "subtree",
		"token", "tokenFilePath", "tokenInstruction", "usage", "watching"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("response fields = %v, want %v", keys, want)
	}
	bootstrap, _ := body["bootstrap"].(string)
	if !strings.HasPrefix(bootstrap, "SKEIN_PUBLIC_URL=") || !strings.Contains(bootstrap, " python3 -c ") {
		t.Fatalf("bootstrap lost its inline-loader form:\n%s", bootstrap)
	}
	if strings.Contains(bootstrap, "SKEIN_CLIENT=") || strings.Contains(bootstrap, "SKEIN_PLATFORM=") {
		t.Fatalf("bootstrap without client carries client variables:\n%s", bootstrap)
	}
	if usage, _ := body["usage"].(string); !strings.Contains(usage, "`client`") {
		t.Fatalf("usage does not point at client:\n%s", usage)
	}

	s := server.NewMCPServer("t", "0", server.WithToolCapabilities(false))
	RegisterGetMonitorScript(s, nil, nil, nil, gmsUnitRegistry(t), gmsTestPublicURL, gmsTestRunnerSHA256, gmsTestLoaderSHA256, 0)
	tool := s.GetTool("skein_get_monitor_script")
	if tool == nil {
		t.Fatal("tool not registered")
	}
	for _, param := range []string{"client", "platform", "tokenDir"} {
		if _, ok := tool.Tool.InputSchema.Properties[param]; !ok {
			t.Fatalf("tool schema has no %q parameter", param)
		}
	}
	if !strings.Contains(tool.Tool.Description, "`client`") || !strings.Contains(tool.Tool.Description, "unit-agent") {
		t.Fatalf("description does not name client and the registry's ids:\n%s", tool.Tool.Description)
	}
}

// TestGetMonitorScriptClient_ResolvesRecords — ИНВ-13, API surface (row #14):
// a known id answers with client equal to the id, an unknown id of the right
// form answers with the general record and its warning, and client without
// platform is posix.
func TestGetMonitorScriptClient_ResolvesRecords(t *testing.T) {
	reg := gmsUnitRegistry(t)
	ids := reg.AgentIDs()
	if len(ids) == 0 {
		t.Fatal("fixture registry has no agent record")
	}
	for _, id := range ids {
		u := newGMSUnit(reg)
		body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": id}))
		if body["client"] != id || body["platform"] != monitorPlatformPOSIX {
			t.Fatalf("client %q: response client = %v platform = %v", id, body["client"], body["platform"])
		}
		if usage, _ := body["usage"].(string); !strings.Contains(usage, "unit-agent-warning-2c9d") {
			t.Fatalf("client %q: usage lost the record's warning:\n%s", id, usage)
		}
	}

	u := newGMSUnit(reg)
	body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": "not-in-registry"}))
	if body["client"] != monitorGeneralClient || body["platform"] != monitorPlatformPOSIX {
		t.Fatalf("unknown client: response client = %v platform = %v", body["client"], body["platform"])
	}
	if usage, _ := body["usage"].(string); !strings.Contains(usage, reg.General.Warnings[0]) {
		t.Fatalf("unknown client: usage lacks the general record's warning:\n%s", usage)
	}

	t.Run("records of the embedded registry", func(t *testing.T) {
		embedded := monitorregistry.MustEmbedded()
		if len(embedded.AgentIDs()) == 0 {
			t.Skip("the embedded registry has no agent record yet")
		}
		for _, id := range embedded.AgentIDs() {
			u := newGMSUnit(embedded)
			body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": id}))
			if body["client"] != id {
				t.Fatalf("embedded record %q answered as client %v", id, body["client"])
			}
		}
	})
}

// TestGetMonitorScriptClient_TokenDirHintNamesTheShellCommand — US-11: a
// windows call without tokenDir names the command that prints
// %LOCALAPPDATA% in the record's shell: Git Bash reads the variable with
// $NAME, cmd with %NAME%, PowerShell (also for the general record and a
// record without a named shell) with $env:NAME.
func TestGetMonitorScriptClient_TokenDirHintNamesTheShellCommand(t *testing.T) {
	cases := []struct{ shell, want string }{
		{monitorregistry.WindowsShellGitBash, `echo "$LOCALAPPDATA"`},
		{monitorregistry.WindowsShellCmd, `echo %LOCALAPPDATA%`},
		{monitorregistry.WindowsShellPwsh, `$env:LOCALAPPDATA`},
		{monitorregistry.WindowsShellPowerShell, `$env:LOCALAPPDATA`},
		{"", `$env:LOCALAPPDATA`},
	}
	for _, tc := range cases {
		shellField := ""
		if tc.shell != "" {
			shellField = `"windowsShell": "` + tc.shell + `",`
		}
		reg, err := monitorregistry.Load([]byte(`{
  "schemaVersion": 1,
  "general": {"about": "general", "source": "unit fixture"},
  "agents": [{"id": "win-agent", "about": "a", "source": "unit fixture", ` + shellField + `
    "process": {"executables": ["win-agent"]}, "methods": []}],
  "channels": [], "shells": {"source": "unit fixture", "names": ["sh"]}, "zavodEngines": {}
}`))
		if err != nil {
			t.Fatalf("shell %q: fixture registry: %v", tc.shell, err)
		}
		u := newGMSUnit(reg)
		body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": "win-agent", "platform": "windows"}))
		if body["code"] != monitorCodeTokenDirRequired {
			t.Fatalf("shell %q: code = %v", tc.shell, body["code"])
		}
		if msg, _ := body["message"].(string); !strings.Contains(msg, tc.want) {
			t.Fatalf("shell %q: refusal %q does not name %s", tc.shell, msg, tc.want)
		}
	}
	u := newGMSUnit(gmsUnitRegistry(t))
	body := extractToolJSON(t, u.call(t, map[string]any{"id": "sk-42", "client": "unknown-agent", "platform": "windows"}))
	if msg, _ := body["message"].(string); !strings.Contains(msg, `$env:LOCALAPPDATA`) {
		t.Fatalf("general record: refusal %q does not name the PowerShell command", msg)
	}
}
