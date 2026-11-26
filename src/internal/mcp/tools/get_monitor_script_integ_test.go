package tools

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/monitorcreds"
	"skein-api/internal/monitorregistry"
	"skein-api/internal/notifications"
)

// gmsIntegKnownClient is the agent record of gmsIntegRegistry.
const gmsIntegKnownClient = "integ-agent"

// gmsIntegRegistry is a registry with one agent record, so a known client
// name can be called; its values describe no real agent.
func gmsIntegRegistry(t *testing.T) *monitorregistry.Registry {
	t.Helper()
	reg, err := monitorregistry.Load([]byte(`{
  "schemaVersion": 1,
  "general": {"about": "general", "source": "integ fixture", "warnings": ["not found"]},
  "agents": [{
    "id": "` + gmsIntegKnownClient + `", "about": "integ fixture agent", "source": "integ fixture",
    "process": {"executables": ["integ-agent"]},
    "methods": [{"kind": "stream", "instruction": "run it with the stream tool", "conditions": {}}]
  }],
  "channels": [],
  "shells": {"source": "integ fixture", "names": ["sh"]},
  "zavodEngines": {}
}`))
	if err != nil {
		t.Fatalf("fixture registry: %v", err)
	}
	return reg
}

// gmsIntegFixture is a user with a visible issue and an API key, and the
// handler under test on the real stores.
type gmsIntegFixture struct {
	pool    *pgxpool.Pool
	userID  uuid.UUID
	issueID string
	ctx     context.Context
	handle  func(args map[string]any) *mcpgo.CallToolResult
}

func newGMSIntegFixture(t *testing.T) *gmsIntegFixture {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gmi")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "monitor me", "open", 2)
	keyID := seedAPIKeyForTool(t, pool, userID)
	h := handleGetMonitorScript(poolIssueResolver{pool: pool}, notifications.NewStore(pool),
		monitorcreds.New(pool, 6*time.Hour), gmsIntegRegistry(t), gmsTestPublicURL, gmsTestRunnerSHA256)
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

// watchRows counts the watch rows of the fixture's user on its issue.
func (f *gmsIntegFixture) watchRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		f.issueID, f.userID).Scan(&n); err != nil {
		t.Fatalf("count watch rows: %v", err)
	}
	return n
}

// credentialRows counts every monitor credential of the fixture's user.
func (f *gmsIntegFixture) credentialRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM monitor_credentials WHERE user_id = $1`, f.userID).Scan(&n); err != nil {
		t.Fatalf("count monitor credentials: %v", err)
	}
	return n
}

// tokenFilePathOf reads monitor_credentials.token_file_path of a token.
func (f *gmsIntegFixture) tokenFilePathOf(t *testing.T, plaintext string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	var p string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT token_file_path FROM monitor_credentials WHERE token_digest = $1`, digest[:]).Scan(&p); err != nil {
		t.Fatalf("read token_file_path: %v", err)
	}
	return p
}

// TestGetMonitorScriptInteg_RefusalLeavesNoTrace — SC-008: every invalid
// client, platform or tokenDir is a validation_error with its code, and the
// call leaves neither a watch row nor a monitor credential.
func TestGetMonitorScriptInteg_RefusalLeavesNoTrace(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantCode string
	}{
		{"client of the wrong type", map[string]any{"client": 7}, monitorCodeClientInvalid},
		{"empty client", map[string]any{"client": ""}, monitorCodeClientInvalid},
		{"platform of the wrong type", map[string]any{"client": "x", "platform": true}, monitorCodePlatformInvalid},
		{"empty platform", map[string]any{"client": "x", "platform": ""}, monitorCodePlatformInvalid},
		{"platform without client", map[string]any{"platform": "posix"}, monitorCodePlatformWithoutClient},
		{"tokenDir without client", map[string]any{"tokenDir": `C:\Users\me`}, monitorCodeTokenDirWithoutClient},
		{"tokenDir with platform posix", map[string]any{"client": "x", "platform": "posix", "tokenDir": `C:\Users\me`}, monitorCodeTokenDirNotWindows},
		{"tokenDir with the default platform", map[string]any{"client": "x", "tokenDir": `C:\Users\me`}, monitorCodeTokenDirNotWindows},
		{"windows without tokenDir", map[string]any{"client": "x", "platform": "windows"}, monitorCodeTokenDirRequired},
		{"tokenDir of the wrong type", map[string]any{"client": "x", "platform": "windows", "tokenDir": 5}, monitorCodeTokenDirInvalid},
		{"empty tokenDir", map[string]any{"client": "x", "platform": "windows", "tokenDir": ""}, monitorCodeTokenDirInvalid},
		{"relative tokenDir", map[string]any{"client": "x", "platform": "windows", "tokenDir": `Users\me\AppData`}, monitorCodeTokenDirInvalid},
		{"UNC tokenDir", map[string]any{"client": "x", "platform": "windows", "tokenDir": `\\server\share\me`}, monitorCodeTokenDirInvalid},
		{"tokenDir with ..", map[string]any{"client": "x", "platform": "windows", "tokenDir": `C:\Users\me\..\Public`}, monitorCodeTokenDirInvalid},
		{"tokenDir with a control character", map[string]any{"client": "x", "platform": "windows", "tokenDir": "C:\\Users\\me\x01dir"}, monitorCodeTokenDirInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGMSIntegFixture(t)
			args := map[string]any{"id": f.issueID}
			for k, v := range tc.args {
				args[k] = v
			}
			res := f.handle(args)
			if !res.IsError {
				t.Fatalf("call succeeded, want validation_error %s", tc.wantCode)
			}
			body := extractToolJSON(t, res)
			if body["error"] != ErrCodeValidationError || body["code"] != tc.wantCode {
				t.Fatalf("error = %v code = %v, want %s %s", body["error"], body["code"], ErrCodeValidationError, tc.wantCode)
			}
			if n := f.watchRows(t); n != 0 {
				t.Fatalf("refused call left %d watch rows", n)
			}
			if n := f.credentialRows(t); n != 0 {
				t.Fatalf("refused call left %d monitor credentials", n)
			}
		})
	}
}

// TestGetMonitorScriptInteg_WindowsTokenDirAccepted — SC-008, US-11, R-7: a
// Windows tokenDir with spaces, Cyrillic and an apostrophe is accepted; the
// token file lies in it under a directory with the mint discriminator, and
// the credential row records the same path.
func TestGetMonitorScriptInteg_WindowsTokenDirAccepted(t *testing.T) {
	f := newGMSIntegFixture(t)
	tokenDir := `C:\Users\Иван O'Brien\AppData\Local`
	res := f.handle(map[string]any{"id": f.issueID, "client": gmsIntegKnownClient, "platform": "windows", "tokenDir": tokenDir})
	if res.IsError {
		t.Fatalf("call refused: %v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	tokenFilePath, _ := body["tokenFilePath"].(string)
	prefix := tokenDir + `\skein-mon-` + f.issueID + "-"
	if !strings.HasPrefix(tokenFilePath, prefix) || !strings.HasSuffix(tokenFilePath, `\token`) {
		t.Fatalf("tokenFilePath = %q, want %s<discriminator>\\token", tokenFilePath, prefix)
	}
	if disc := strings.TrimSuffix(strings.TrimPrefix(tokenFilePath, prefix), `\token`); disc == "" || strings.ContainsAny(disc, `\/`) {
		t.Fatalf("tokenFilePath %q has no single discriminator directory", tokenFilePath)
	}
	token, _ := body["token"].(string)
	if got := f.tokenFilePathOf(t, token); got != tokenFilePath {
		t.Fatalf("monitor_credentials.token_file_path = %q, response tokenFilePath = %q", got, tokenFilePath)
	}
	if body["client"] != gmsIntegKnownClient || body["platform"] != "windows" {
		t.Fatalf("client = %v platform = %v, want %s windows", body["client"], body["platform"], gmsIntegKnownClient)
	}
}

// TestGetMonitorScriptInteg_ClientOnPOSIX — SC-008: a valid client on POSIX,
// known or not, watches and mints like a call without client, and the
// response carries client (the record id, or "general"), platform and
// preflight.
func TestGetMonitorScriptInteg_ClientOnPOSIX(t *testing.T) {
	cases := []struct {
		name, client, wantClient string
	}{
		{"known client", gmsIntegKnownClient, gmsIntegKnownClient},
		{"unknown client", "some-other-agent", monitorGeneralClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGMSIntegFixture(t)
			res := f.handle(map[string]any{"id": f.issueID, "client": tc.client})
			if res.IsError {
				t.Fatalf("call refused: %v", extractToolJSON(t, res))
			}
			body := extractToolJSON(t, res)
			if n := f.watchRows(t); n != 1 {
				t.Fatalf("watch rows = %d, want 1", n)
			}
			if n := f.credentialRows(t); n != 1 {
				t.Fatalf("monitor credentials = %d, want 1", n)
			}
			if body["client"] != tc.wantClient || body["platform"] != monitorPlatformPOSIX {
				t.Fatalf("client = %v platform = %v, want %s posix", body["client"], body["platform"], tc.wantClient)
			}
			if p, _ := body["preflight"].(string); p == "" {
				t.Fatal("response with client has no preflight")
			}
		})
	}
}
