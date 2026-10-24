package tools

// monitor_stalled_middleware_test.go — sk-579 test matrix rows 28, 29 and
// 31–36 (US-5, INV-7, INV-8, INV-9, SC-008, SC-009, NFR): the monitorStalled
// response layer registered with (*MCPServer).Use.
//
// Rows 28 and 29 are unit tests on an mcp-go server with a fake source; the
// rest run the layer over the real store and real tools on a database. Calls
// go through (*MCPServer).HandleMessage, so the layer is exercised in the
// position the production server gives it. Time is moved by shifting stamps
// in the database, or by the synctest bubble clock (row 29) — never by
// waiting.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/agents"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// hintCtx is the auth context the production /mcp door builds: user, role,
// api key and — when session is not empty — the Mcp-Session-Id.
func hintCtx(user, key uuid.UUID, session string) context.Context {
	ctx := context.Background()
	ctx = auth.WithUserID(ctx, user)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithAPIKeyID(ctx, key)
	if session != "" {
		ctx = auth.WithSessionID(ctx, session)
	}
	return ctx
}

// rawCall sends one tools/call through the server and returns the JSON-RPC
// response exactly as the transport would encode it.
func rawCall(t *testing.T, s *server.MCPServer, ctx context.Context, tool string, args map[string]any) []byte {
	t.Helper()
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := json.Marshal(s.HandleMessage(ctx, msg))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return out
}

// callResponse is the decoded tools/call response.
type callResponse struct {
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error any `json:"error"`
}

func (r callResponse) texts() []string {
	var out []string
	if r.Result != nil {
		for _, c := range r.Result.Content {
			out = append(out, c.Text)
		}
	}
	return out
}

func decodeCall(t *testing.T, raw []byte) callResponse {
	t.Helper()
	var r callResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode response: %v\n%s", err, raw)
	}
	return r
}

func call(t *testing.T, s *server.MCPServer, ctx context.Context, tool string, args map[string]any) callResponse {
	t.Helper()
	r := decodeCall(t, rawCall(t, s, ctx, tool, args))
	if r.Result == nil {
		t.Fatalf("%s: no result: %v", tool, r.Error)
	}
	return r
}

// stalledHint collects the monitorStalled items and the omitted count from
// every text block of a response that is a JSON object carrying the key.
type stalledHint struct {
	present bool
	items   []map[string]any
	omitted float64
}

func hintOf(t *testing.T, r callResponse) stalledHint {
	t.Helper()
	var h stalledHint
	for _, text := range r.texts() {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &obj) != nil {
			continue
		}
		raw, ok := obj[MonitorStalledKey]
		if !ok {
			continue
		}
		h.present = true
		var items []map[string]any
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatalf("monitorStalled is not an array of objects: %s", raw)
		}
		h.items = append(h.items, items...)
		if om, ok := obj[MonitorStalledOmittedKey]; ok {
			var n float64
			if err := json.Unmarshal(om, &n); err != nil {
				t.Fatalf("monitorStalledOmitted: %s", om)
			}
			h.omitted += n
		}
	}
	return h
}

func (h stalledHint) item(id uuid.UUID) (map[string]any, bool) {
	for _, it := range h.items {
		if it["monitorId"] == id.String() {
			return it, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Rows 28, 29 — unit, fake source
// ---------------------------------------------------------------------------

// fakeStalledSource is a MonitorStalledSource with scripted answers. Every
// method honours a cancelled context; the block* switches make it wait for
// the cancellation.
type fakeStalledSource struct {
	monitors   []monitorcreds.StalledMonitor
	total      int // Total of the selection; 0 = len(monitors)
	stalledErr error
	claimErr   error
	claimOnly  int    // >0: the claim wins only the first claimOnly ids
	block      string // "stalled", "claim", "touch" or ""
	mu         sync.Mutex
	selects    int // Stalled calls
	claims     int // ClaimStallNotices calls
}

func (f *fakeStalledSource) wait(ctx context.Context, what string) error {
	if f.block == what {
		<-ctx.Done()
	}
	return ctx.Err()
}

func (f *fakeStalledSource) Stalled(ctx context.Context, q monitorcreds.StalledQuery) (monitorcreds.StalledResult, error) {
	f.mu.Lock()
	f.selects++
	f.mu.Unlock()
	if err := f.wait(ctx, "stalled"); err != nil {
		return monitorcreds.StalledResult{}, err
	}
	if f.stalledErr != nil {
		return monitorcreds.StalledResult{}, f.stalledErr
	}
	total := f.total
	if total == 0 {
		total = len(f.monitors)
	}
	return monitorcreds.StalledResult{Monitors: f.monitors, Total: total}, nil
}

func (f *fakeStalledSource) ClaimStallNotices(ctx context.Context, _ string, ids []uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	f.claims++
	f.mu.Unlock()
	if err := f.wait(ctx, "claim"); err != nil {
		return nil, err
	}
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.claimOnly > 0 && f.claimOnly < len(ids) {
		return ids[:f.claimOnly], nil
	}
	return ids, nil
}

func (f *fakeStalledSource) TouchSessionless(ctx context.Context, _ uuid.UUID, _ time.Duration) error {
	return f.wait(ctx, "touch")
}

var fakeMonitorID = uuid.MustParse("6f1c2b8e-1d1a-4c39-9a7e-0b5c3e0f2a11")

func oneFakeMonitor() []monitorcreds.StalledMonitor {
	return []monitorcreds.StalledMonitor{{
		ID: fakeMonitorID, IssueID: "zz-1", IssuedSessionID: "s-unit",
		TokenFilePath: "/tmp/skein-mon-zz-1-ab.token", Mine: true,
	}}
}

// probeServer is an mcp-go server with one tool "probe" answering what
// answer returns; mw, when not nil, is registered with Use.
func probeServer(answer func() (*mcpgo.CallToolResult, error), mw server.ToolHandlerMiddleware) *server.MCPServer {
	s := server.NewMCPServer("probe", "1", server.WithToolCapabilities(false))
	s.AddTool(mcpgo.NewTool("probe"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return answer()
	})
	if mw != nil {
		s.Use(mw)
	}
	return s
}

func textAnswer(text string, isError bool) func() (*mcpgo.CallToolResult, error) {
	return func() (*mcpgo.CallToolResult, error) {
		r := mcpgo.NewToolResultText(text)
		r.IsError = isError
		return r, nil
	}
}

// probeBoth returns the raw response of the bare server and of the same
// server with the middleware over src.
func probeBoth(t *testing.T, answer func() (*mcpgo.CallToolResult, error), src MonitorStalledSource, ctx context.Context) (bare, hinted []byte) {
	t.Helper()
	bare = rawCall(t, probeServer(answer, nil), ctx, "probe", nil)
	mw := MonitorStalledMiddleware(MonitorStalledConfig{Source: src, Threshold: 3 * time.Minute, AgentPublicURL: gmsTestPublicURL})
	hinted = rawCall(t, probeServer(answer, mw), ctx, "probe", nil)
	return bare, hinted
}

func unitCtx(session string) context.Context {
	return hintCtx(uuid.MustParse("0b3c1e2d-7f4a-4e6b-8c1d-2a3b4c5d6e7f"),
		uuid.MustParse("9a8b7c6d-5e4f-4a3b-9c2d-1e0f2a3b4c5d"), session)
}

// Row 28 (INV-9): where the hint goes, and that nothing else moves.
func TestMonitorStalledMiddleware_AttachmentForms(t *testing.T) {
	ctx := unitCtx("s-unit")

	t.Run("JSON object: keys inserted before the last brace, other bytes intact", func(t *testing.T) {
		for _, text := range []string{
			`{"b":1,"a":[2]}`,
			"{\n  \"b\": 1,\n  \"a\": [2]\n}\n", // re-encoding would lose the layout
		} {
			_, hinted := probeBoth(t, textAnswer(text, false), &fakeStalledSource{monitors: oneFakeMonitor()}, ctx)
			r := decodeCall(t, hinted)
			if got := r.texts(); len(got) != 1 {
				t.Fatalf("%q: want one block, got %q", text, got)
			}
			out := r.texts()[0]
			lb := strings.LastIndex(text, "}")
			head, tail := text[:lb], text[lb:]
			if !strings.HasPrefix(out, head) || !strings.HasSuffix(out, tail) || len(out) <= len(text) {
				t.Fatalf("original bytes moved:\nin:  %q\nout: %q", text, out)
			}
			inserted := out[len(head) : len(out)-len(tail)]
			if !strings.HasPrefix(inserted, `,"monitorStalled":`) {
				t.Fatalf("inserted segment = %q, want it to start with ,\"monitorStalled\":", inserted)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(out), &obj); err != nil {
				t.Fatalf("result is not JSON: %v\n%s", err, out)
			}
			if obj["b"] != float64(1) {
				t.Errorf("existing key b lost: %v", obj)
			}
			if _, ok := hintOf(t, r).item(fakeMonitorID); !ok {
				t.Errorf("hint item missing: %s", out)
			}
		}
	})

	t.Run("not an object: original block intact, a second block carries the hint", func(t *testing.T) {
		for _, text := range []string{"plain answer", `[1,2]`} {
			_, hinted := probeBoth(t, textAnswer(text, false), &fakeStalledSource{monitors: oneFakeMonitor()}, ctx)
			r := decodeCall(t, hinted)
			got := r.texts()
			if len(got) != 2 || got[0] != text {
				t.Fatalf("%q: blocks = %q, want the original then the hint", text, got)
			}
			if _, ok := hintOf(t, r).item(fakeMonitorID); !ok {
				t.Errorf("%q: second block carries no hint item: %q", text, got[1])
			}
		}
	})

	t.Run("error with a body: hint present, isError kept", func(t *testing.T) {
		_, hinted := probeBoth(t, textAnswer(`{"error":"validation_error","message":"x"}`, true),
			&fakeStalledSource{monitors: oneFakeMonitor()}, ctx)
		r := decodeCall(t, hinted)
		if !r.Result.IsError {
			t.Errorf("isError lost")
		}
		if _, ok := hintOf(t, r).item(fakeMonitorID); !ok {
			t.Errorf("hint missing on an error with a body: %q", r.texts())
		}
	})

	t.Run("Go error and nil result pass through untouched", func(t *testing.T) {
		for name, answer := range map[string]func() (*mcpgo.CallToolResult, error){
			"go error":   func() (*mcpgo.CallToolResult, error) { return nil, errors.New("handler broke") },
			"nil result": func() (*mcpgo.CallToolResult, error) { return nil, nil },
		} {
			bare, hinted := probeBoth(t, answer, &fakeStalledSource{monitors: oneFakeMonitor()}, ctx)
			if string(bare) != string(hinted) {
				t.Errorf("%s: response changed\nbare:   %s\nhinted: %s", name, bare, hinted)
			}
		}
	})

	t.Run("result without content: untouched, nothing claimed", func(t *testing.T) {
		for _, isError := range []bool{false, true} {
			for name, content := range map[string][]mcpgo.Content{"nil": nil, "empty": {}} {
				answer := func() (*mcpgo.CallToolResult, error) {
					return &mcpgo.CallToolResult{Content: content, IsError: isError}, nil
				}
				src := &fakeStalledSource{monitors: oneFakeMonitor()}
				bare, hinted := probeBoth(t, answer, src, ctx)
				if string(bare) != string(hinted) {
					t.Errorf("%s content, isError=%v: response changed\nbare:   %s\nhinted: %s", name, isError, bare, hinted)
				}
				if src.claims != 0 {
					t.Errorf("%s content, isError=%v: %d show marks claimed for an answer without a body", name, isError, src.claims)
				}
			}
		}
	})

	t.Run("empty list: no field, response byte-identical", func(t *testing.T) {
		for _, text := range []string{`{"b":1,"a":[2]}`, "plain answer"} {
			bare, hinted := probeBoth(t, textAnswer(text, false), &fakeStalledSource{}, ctx)
			if string(bare) != string(hinted) {
				t.Errorf("%q: response changed\nbare:   %s\nhinted: %s", text, bare, hinted)
			}
		}
	})
}

// monitorStalledOmitted counts only candidates cut by the selection's LIMIT
// (US-5 "on truncation — the number not shown"): a candidate within the cap
// whose claim a concurrent call of the same session won is throttled, it will
// not «come next», so it is not counted as omitted.
func TestMonitorStalledMiddleware_LostClaimIsNotOmitted(t *testing.T) {
	three := []monitorcreds.StalledMonitor{
		{ID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), IssueID: "zz-1", IssuedSessionID: "s-unit", Mine: true},
		{ID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), IssueID: "zz-1", IssuedSessionID: "s-unit", Mine: true},
		{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), IssueID: "zz-1", IssuedSessionID: "s-unit", Mine: true},
	}
	t.Run("no truncation, two claims lost", func(t *testing.T) {
		_, hinted := probeBoth(t, textAnswer(`{"ok":true}`, false),
			&fakeStalledSource{monitors: three, claimOnly: 1}, unitCtx("s-unit"))
		out := decodeCall(t, hinted).texts()[0]
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			t.Fatalf("result is not JSON: %v\n%s", err, out)
		}
		if _, ok := obj[MonitorStalledOmittedKey]; ok {
			t.Errorf("lost claims reported as omitted: %s", out)
		}
		if h := hintOf(t, decodeCall(t, hinted)); len(h.items) != 1 {
			t.Errorf("want the one claimed item, got %v", h.items)
		}
	})
	t.Run("truncation counts only the cut", func(t *testing.T) {
		_, hinted := probeBoth(t, textAnswer(`{"ok":true}`, false),
			&fakeStalledSource{monitors: three, total: 5, claimOnly: 1}, unitCtx("s-unit"))
		if h := hintOf(t, decodeCall(t, hinted)); h.omitted != 2 {
			t.Errorf("Total 5, selection 3: omitted = %v, want 2 (the lost claims do not count)", h.omitted)
		}
	})
}

// A server assembled without the stall threshold (zero) must not call every
// live monitor stalled: the layer passes responses through, as with no source.
func TestMonitorStalledMiddleware_NoThresholdPassesThrough(t *testing.T) {
	answer := textAnswer(`{"b":1,"a":[2]}`, false)
	ctx := unitCtx("")
	for _, threshold := range []time.Duration{0, -time.Minute} {
		src := &fakeStalledSource{monitors: oneFakeMonitor()}
		bare := rawCall(t, probeServer(answer, nil), ctx, "probe", nil)
		mw := MonitorStalledMiddleware(MonitorStalledConfig{Source: src, Threshold: threshold, AgentPublicURL: gmsTestPublicURL})
		hinted := rawCall(t, probeServer(answer, mw), ctx, "probe", nil)
		if string(bare) != string(hinted) {
			t.Errorf("threshold %s: response changed\nbare:   %s\nhinted: %s", threshold, bare, hinted)
		}
		if src.selects != 0 || src.claims != 0 {
			t.Errorf("threshold %s: source used (%d selections, %d claims), want a pass-through", threshold, src.selects, src.claims)
		}
	}
}

// Row 29 (INV-9 «a failing hint does not break the call»): the source fails
// or hangs; with the production timeout the bubble clock fires it, and the
// call answers exactly what it would without the layer.
func TestMonitorStalledMiddleware_FailureLeavesCallIntact(t *testing.T) {
	cases := []struct {
		name    string
		session string
		src     *fakeStalledSource
	}{
		{"selection fails", "s-unit", &fakeStalledSource{monitors: oneFakeMonitor(), stalledErr: errors.New("db down")}},
		{"selection hangs", "s-unit", &fakeStalledSource{monitors: oneFakeMonitor(), block: "stalled"}},
		{"claim fails", "s-unit", &fakeStalledSource{monitors: oneFakeMonitor(), claimErr: errors.New("db down")}},
		{"claim hangs", "s-unit", &fakeStalledSource{monitors: oneFakeMonitor(), block: "claim"}},
		{"sessionless stamp hangs", "", &fakeStalledSource{monitors: oneFakeMonitor(), block: "touch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Timeout zero = the production monitorStalledTimeout.
				bare, hinted := probeBoth(t, textAnswer(`{"b":1,"a":[2]}`, false), tc.src, unitCtx(tc.session))
				if string(bare) != string(hinted) {
					t.Errorf("response changed\nbare:   %s\nhinted: %s", bare, hinted)
				}
				if r := decodeCall(t, hinted); r.Error != nil {
					t.Errorf("the hint's failure surfaced as an error: %v", r.Error)
				}
			})
		})
	}
}

// levelRecorder is an slog.Handler that keeps the level of every record.
type levelRecorder struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.levels = append(h.levels, r.Level)
	h.mu.Unlock()
	return nil
}
func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }

func (h *levelRecorder) errors() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.levels {
		if l >= slog.LevelError {
			n++
		}
	}
	return n
}

func recordLevels(t *testing.T) *levelRecorder {
	t.Helper()
	rec := &levelRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// The hint failing because the request context was cancelled (the client
// went away after the tool ran) is a normal path, not an Error record — an
// Error line is an alert signal. Its own 2s budget running out and a database
// error stay Error.
func TestMonitorStalledMiddleware_FailureLogLevel(t *testing.T) {
	answer := textAnswer(`{"b":1,"a":[2]}`, false)
	mwFor := func(src MonitorStalledSource) server.ToolHandlerMiddleware {
		return MonitorStalledMiddleware(MonitorStalledConfig{Source: src, Threshold: 3 * time.Minute, AgentPublicURL: gmsTestPublicURL})
	}

	t.Run("request cancelled: not an error", func(t *testing.T) {
		rec := recordLevels(t)
		src := &fakeStalledSource{monitors: oneFakeMonitor()}
		ctx, cancel := context.WithCancel(unitCtx("s-unit"))
		cancel()
		rawCall(t, probeServer(answer, mwFor(src)), ctx, "probe", nil)
		if src.selects != 1 {
			t.Fatalf("the hint did not run (%d selections)", src.selects)
		}
		if n := rec.errors(); n != 0 {
			t.Errorf("a cancelled request logged %d Error records, want 0", n)
		}
	})

	t.Run("database error: error", func(t *testing.T) {
		rec := recordLevels(t)
		src := &fakeStalledSource{monitors: oneFakeMonitor(), stalledErr: errors.New("db down")}
		rawCall(t, probeServer(answer, mwFor(src)), unitCtx("s-unit"), "probe", nil)
		if n := rec.errors(); n != 1 {
			t.Errorf("a database error logged %d Error records, want 1", n)
		}
	})

	t.Run("hint budget exceeded: error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rec := recordLevels(t)
			src := &fakeStalledSource{monitors: oneFakeMonitor(), block: "stalled"}
			rawCall(t, probeServer(answer, mwFor(src)), unitCtx("s-unit"), "probe", nil)
			if n := rec.errors(); n != 1 {
				t.Errorf("the 2s budget running out logged %d Error records, want 1", n)
			}
		})
	})
}

// ---------------------------------------------------------------------------
// Rows 31–36 — integration, real store and tools
// ---------------------------------------------------------------------------

const hintThreshold = 3 * time.Minute

type hintFixture struct {
	pool    *pgxpool.Pool
	store   *monitorcreds.Store
	user    uuid.UUID
	project uuid.UUID
	prefix  string
	root    string
	key     uuid.UUID
	srv     *server.MCPServer
}

func newHintFixture(t *testing.T) *hintFixture {
	t.Helper()
	pool := newTestPool(t)
	user, _ := seedUser(t, pool, "user")
	project, prefix := seedProject(t, pool, user, "msh")
	root := seedIssue(t, pool, project, user, prefix+"-1", "stalled hint", "open", 2)
	key := seedAPIKeyForTool(t, pool, user)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE user_id = $1`, user)
	})
	f := &hintFixture{pool: pool, store: monitorcreds.New(pool, 6*time.Hour), user: user,
		project: project, prefix: prefix, root: root, key: key}
	f.srv = f.server(pool)
	return f
}

// server builds an MCP server with the real tools the rows call, a stub
// "probe" tool answering {"ok":true}, and the layer over a store on pool.
func (f *hintFixture) server(pool *pgxpool.Pool) *server.MCPServer {
	s := server.NewMCPServer("hint-test", "1", server.WithToolCapabilities(false))
	store := monitorcreds.New(pool, 6*time.Hour)
	notifStore := notifications.NewStore(f.pool)
	issuesStore := issues.NewStore(f.pool)
	RegisterGetMonitorScript(s, f.pool, notifStore, store, gmsTestPublicURL, 6*time.Hour)
	RegisterWatchers(s, f.pool, projects.NewStore(f.pool), notifStore, store)
	RegisterClose(s, f.pool, issuesStore)
	RegisterShow(s, f.pool, issuesStore, dependencies.NewStore(f.pool))
	s.AddTool(mcpgo.NewTool("probe"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(`{"ok":true}`), nil
	})
	s.Use(MonitorStalledMiddleware(MonitorStalledConfig{
		Source: store, Threshold: hintThreshold, AgentPublicURL: gmsTestPublicURL,
	}))
	return s
}

func (f *hintFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// mint issues a monitor on the root through key for the issuing session and
// makes it stalled: minted 10 minutes ago, never polled.
func (f *hintFixture) mint(t *testing.T, key uuid.UUID, session string) uuid.UUID {
	t.Helper()
	_, cred, err := f.store.Mint(context.Background(), monitorcreds.MintParams{
		UserID: f.user, MintedByAPIKeyID: key, Audience: "notifications:poll", Capability: "read",
		ProjectID: f.project, ScopeIssueID: f.root, EventTypes: []string{"comment.created"},
		IssuedSessionID: session, TokenFilePath: "/tmp/skein-mon-hint.token",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	f.exec(t, `UPDATE monitor_credentials SET created_at = now() - interval '10 minutes' WHERE id = $1`, cred.ID)
	return cred.ID
}

// seen records session as active now; quiet backdates it past the threshold.
func (f *hintFixture) seen(t *testing.T, session string) {
	t.Helper()
	if err := agents.Upsert(context.Background(), f.pool, f.user, session, agents.LastSeenGranularity); err != nil {
		t.Fatalf("agents.Upsert: %v", err)
	}
}

func (f *hintFixture) quiet(t *testing.T, session string) {
	t.Helper()
	f.seen(t, session)
	f.exec(t, `UPDATE agents SET last_seen_at = now() - interval '10 minutes' WHERE user_id = $1 AND session_id = $2`, f.user, session)
}

// rewindNotice moves the show mark of (monitor, recipient) 6 minutes back,
// past the 5-minute throttle.
func (f *hintFixture) rewindNotice(t *testing.T, monitor uuid.UUID, recipient string) {
	t.Helper()
	f.exec(t, `UPDATE monitor_stall_notices SET shown_at = shown_at - interval '6 minutes'
	            WHERE monitor_id = $1 AND recipient_session_id = $2`, monitor, recipient)
}

func (f *hintFixture) probe(t *testing.T, key uuid.UUID, session string) stalledHint {
	t.Helper()
	return hintOf(t, call(t, f.srv, hintCtx(f.user, key, session), "probe", nil))
}

// Row 31 (US-5 item, SC-008, R10, INV-8): the item's fields, its bootstrap
// rebuilt with the ISSUING session, mine and action per recipient, and no
// token anywhere.
func TestMonitorStalledMiddleware_Item(t *testing.T) {
	f := newHintFixture(t)
	const s1, s2 = "sess-issuing-31", "sess-other-31"
	ctx1 := hintCtx(f.user, f.key, s1)

	type minted struct {
		MonitorID string `json:"monitorId"`
		Token     string `json:"token"`
		Bootstrap string `json:"bootstrap"`
	}
	mintViaTool := func() minted {
		t.Helper()
		r := call(t, f.srv, ctx1, "skein_get_monitor_script", map[string]any{"id": f.root})
		if r.Result.IsError {
			t.Fatalf("mint failed: %q", r.texts())
		}
		var m minted
		if err := json.Unmarshal([]byte(r.texts()[0]), &m); err != nil {
			t.Fatalf("mint answer: %v", err)
		}
		return m
	}
	never, polled := mintViaTool(), mintViaTool()
	neverID, polledID := uuid.MustParse(never.MonitorID), uuid.MustParse(polled.MonitorID)
	f.exec(t, `UPDATE monitor_credentials SET created_at = now() - interval '20 minutes' WHERE id = ANY($1)`,
		[]uuid.UUID{neverID, polledID})
	f.exec(t, `UPDATE monitor_credentials SET last_poll_succeeded_at = now() - interval '10 minutes' WHERE id = $1`, polledID)
	var polledAt time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT last_poll_succeeded_at FROM monitor_credentials WHERE id = $1`, polledID).Scan(&polledAt); err != nil {
		t.Fatalf("read last_poll_succeeded_at: %v", err)
	}
	f.seen(t, s1)

	// The issuing session's own call.
	r1 := call(t, f.srv, ctx1, "probe", nil)
	h1 := hintOf(t, r1)
	itNever, ok1 := h1.item(neverID)
	itPolled, ok2 := h1.item(polledID)
	if !ok1 || !ok2 {
		t.Fatalf("issuing session: want both monitors, got %v", h1.items)
	}
	for _, it := range []map[string]any{itNever, itPolled} {
		if it["mine"] != true {
			t.Errorf("issuing session's item mine = %v, want true", it["mine"])
		}
		if it["issueId"] != f.root {
			t.Errorf("issueId = %v, want %s", it["issueId"], f.root)
		}
	}
	if v, present := itNever["lastPollSucceededAt"]; !present || v != nil {
		t.Errorf("never-polled monitor: lastPollSucceededAt = %v (present %v), want null", v, present)
	}
	if s, _ := itPolled["lastPollSucceededAt"].(string); s == "" {
		t.Errorf("polled monitor: lastPollSucceededAt = %v, want a timestamp", itPolled["lastPollSucceededAt"])
	} else if got, err := time.Parse(time.RFC3339Nano, s); err != nil || !got.Equal(polledAt) {
		t.Errorf("lastPollSucceededAt = %q (%v), want %s", s, err, polledAt.UTC().Format(time.RFC3339Nano))
	}
	if itNever["bootstrap"] != never.Bootstrap || itPolled["bootstrap"] != polled.Bootstrap {
		t.Errorf("bootstrap differs from the mint answer:\nitem: %v\nmint: %s", itNever["bootstrap"], never.Bootstrap)
	}
	if itNever["action"] != itPolled["action"] {
		t.Errorf("two mine=true items carry different actions")
	}
	for _, text := range r1.texts() {
		if strings.Contains(text, never.Token) || strings.Contains(text, polled.Token) {
			t.Errorf("the hint carries a plaintext token")
		}
	}

	// While the issuing session is live, another session sees nothing.
	if _, ok := hintOf(t, call(t, f.srv, hintCtx(f.user, f.key, s2), "probe", nil)).item(neverID); ok {
		t.Fatalf("another session saw the monitor while its issuing session is live")
	}
	f.quiet(t, s1)
	h2 := hintOf(t, call(t, f.srv, hintCtx(f.user, f.key, s2), "probe", nil))
	other, ok := h2.item(neverID)
	if !ok {
		t.Fatalf("after the issuing session went quiet another session sees nothing: %v", h2.items)
	}
	if other["mine"] != false {
		t.Errorf("other session's item mine = %v, want false", other["mine"])
	}
	if other["bootstrap"] != never.Bootstrap {
		t.Errorf("other session's bootstrap is not the issuing one:\nitem: %v\nmint: %s", other["bootstrap"], never.Bootstrap)
	}
	if other["action"] == itNever["action"] {
		t.Errorf("action does not depend on mine: %v", other["action"])
	}
	if otherPolled, ok := h2.item(polledID); !ok || otherPolled["action"] != other["action"] {
		t.Errorf("two mine=false items carry different actions")
	}
}

// Row 32 (INV-7, SC-008, US-5 truncation): one show per pair across
// replicas, per recipient, again after the interval; the cap of 10 with the
// omitted count, and the omitted ones are not throttled.
func TestMonitorStalledMiddleware_ThrottleAndTruncation(t *testing.T) {
	t.Run("one show per pair across two replicas", func(t *testing.T) {
		f := newHintFixture(t)
		const s1, s3 = "sess-issuing-32", "sess-other-32"
		id := f.mint(t, f.key, s1)
		f.seen(t, s1)
		replicas := []*server.MCPServer{f.server(f.pool), f.server(f.pool)}

		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "probe"}})
		answers := make([]any, 20)
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				answers[i] = replicas[i%2].HandleMessage(hintCtx(f.user, f.key, s1), msg)
			}()
		}
		wg.Wait()
		shown := 0
		for _, a := range answers {
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatalf("marshal answer: %v", err)
			}
			if _, ok := hintOf(t, decodeCall(t, raw)).item(id); ok {
				shown++
			}
		}
		if shown != 1 {
			t.Fatalf("20 concurrent calls of one session: %d responses carry the item, want exactly 1", shown)
		}

		f.quiet(t, s1)
		if _, ok := f.probe(t, f.key, s3).item(id); !ok {
			t.Errorf("another session did not get its own show")
		}
		if _, ok := f.probe(t, f.key, s1).item(id); ok {
			t.Errorf("issuing session shown again within the interval")
		}
		f.rewindNotice(t, id, s1)
		if _, ok := f.probe(t, f.key, s1).item(id); !ok {
			t.Errorf("issuing session not shown again after the interval")
		}
	})

	t.Run("cap of 10, omitted count, the rest come next", func(t *testing.T) {
		f := newHintFixture(t)
		const s1 = "sess-issuing-32b"
		all := map[uuid.UUID]bool{}
		for range 12 {
			all[f.mint(t, f.key, s1)] = true
		}
		first := f.probe(t, f.key, s1)
		if len(first.items) != 10 || first.omitted != 2 {
			t.Fatalf("12 stalled: %d items, omitted %v; want 10 and 2", len(first.items), first.omitted)
		}
		second := f.probe(t, f.key, s1)
		if len(second.items) != 2 || second.omitted != 0 {
			t.Fatalf("next call: %d items, omitted %v; want the 2 left out and 0", len(second.items), second.omitted)
		}
		seen := map[string]bool{}
		for _, it := range append(first.items, second.items...) {
			id, _ := it["monitorId"].(string)
			if seen[id] || !all[uuid.MustParse(id)] {
				t.Errorf("item %s repeated or unknown", id)
			}
			seen[id] = true
		}
	})
}

// Row 33 (US-5 «after the call's own action», SC-009): monitor_stop and the
// root close are reflected in their own responses.
func TestMonitorStalledMiddleware_AfterOwnAction(t *testing.T) {
	const s1 = "sess-issuing-33"
	setup := func(t *testing.T) (*hintFixture, uuid.UUID, uuid.UUID) {
		f := newHintFixture(t)
		a, b := f.mint(t, f.key, s1), f.mint(t, f.key, s1)
		f.seen(t, s1)
		return f, a, b
	}
	ctxFor := func(f *hintFixture) context.Context { return hintCtx(f.user, f.key, s1) }

	t.Run("monitor_stop then close", func(t *testing.T) {
		f, a, b := setup(t)
		h := hintOf(t, call(t, f.srv, ctxFor(f), "skein_watchers",
			map[string]any{"action": "monitor_stop", "monitorId": a.String()}))
		if _, ok := h.item(a); ok {
			t.Errorf("monitor_stop(A) answer still carries A")
		}
		if _, ok := h.item(b); !ok {
			t.Fatalf("monitor_stop(A) answer does not carry B: %v", h.items)
		}
		f.rewindNotice(t, b, s1)
		r := call(t, f.srv, ctxFor(f), "skein_close", map[string]any{
			"id": f.root, "expectedVersion": 1, "closeOutcome": "done", "reason": "row 33",
		})
		if r.Result.IsError {
			t.Fatalf("close failed: %q", r.texts())
		}
		if _, ok := hintOf(t, r).item(b); ok {
			t.Errorf("the close answer carries B of the closed root")
		}
	})

	t.Run("control: show instead of close", func(t *testing.T) {
		f, a, b := setup(t)
		call(t, f.srv, ctxFor(f), "skein_watchers", map[string]any{"action": "monitor_stop", "monitorId": a.String()})
		f.rewindNotice(t, b, s1)
		if _, ok := hintOf(t, call(t, f.srv, ctxFor(f), "skein_show", map[string]any{"id": f.root})).item(b); !ok {
			t.Errorf("skein_show answer does not carry B with the rewound mark")
		}
	})
}

// Row 34 (US-5, edge "client without Mcp-Session-Id", NFR "once per 60 s per
// key»): the sessionless activity clock.
func TestMonitorStalledMiddleware_SessionlessActivity(t *testing.T) {
	f := newHintFixture(t)
	k1, k2 := f.key, seedAPIKeyForTool(t, f.pool, f.user)
	seenAt := func(key uuid.UUID) *time.Time {
		t.Helper()
		var at *time.Time
		if err := f.pool.QueryRow(context.Background(),
			`SELECT sessionless_seen_at FROM api_keys WHERE id = $1`, key).Scan(&at); err != nil {
			t.Fatalf("read sessionless_seen_at: %v", err)
		}
		return at
	}
	setAgo := func(key uuid.UUID, ago string) *time.Time {
		t.Helper()
		f.exec(t, `UPDATE api_keys SET sessionless_seen_at = now() - $2::interval WHERE id = $1`, key, ago)
		return seenAt(key)
	}
	same := func(a, b *time.Time) bool { return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b)) }

	if seenAt(k1) != nil {
		t.Fatalf("fresh key already has sessionless_seen_at")
	}
	f.probe(t, k1, "")
	if seenAt(k1) == nil {
		t.Fatalf("a tool call without a session did not stamp its key")
	}

	before := setAgo(k1, "30 seconds")
	f.probe(t, k1, "")
	if !same(before, seenAt(k1)) {
		t.Errorf("stamp 30s old was rewritten (throttle is 60s)")
	}
	before = setAgo(k1, "90 seconds")
	f.probe(t, k1, "")
	if after := seenAt(k1); same(before, after) {
		t.Errorf("stamp 90s old was not rewritten")
	}

	before = setAgo(k1, "90 seconds")
	f.probe(t, k1, "sess-34")
	initMsg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t", "version": "1"},
	}})
	f.srv.HandleMessage(hintCtx(f.user, k1, ""), initMsg)
	if !same(before, seenAt(k1)) {
		t.Errorf("a call with a session or a sessionless initialize touched the stamp")
	}

	// Two keys of one user: the throttle is per key.
	setAgo(k1, "90 seconds")
	k2Before := setAgo(k2, "90 seconds")
	k1Before := seenAt(k1)
	f.probe(t, k1, "")
	if same(k1Before, seenAt(k1)) {
		t.Fatalf("K1 not stamped")
	}
	if !same(k2Before, seenAt(k2)) {
		t.Errorf("a sessionless call on K1 stamped K2")
	}
	f.probe(t, k2, "")
	if same(k2Before, seenAt(k2)) {
		t.Errorf("K2 not stamped right after K1 — the throttle is not per key")
	}

	// An empty issuing session on K2, quiet by K2's clock, is shown to a
	// session of K2 although K1 has just been active.
	empty := f.mint(t, k2, "")
	f.probe(t, k1, "")
	setAgo(k2, "10 minutes")
	if _, ok := f.probe(t, k2, "sess-34-k2").item(empty); !ok {
		t.Errorf("empty issuing session quiet on K2 not shown to a K2 session")
	}
}

// countingTracer records the statements one pool runs.
type countingTracer struct {
	mu       sync.Mutex
	reads    int
	writes   int
	activity int64 // rows changed by the sessionless stamp
	other    []string
}

type tracerSQLKey struct{}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, tracerSQLKey{}, d.SQL)
}

func (c *countingTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(tracerSQLKey{}).(string)
	verb := strings.ToUpper(strings.Fields(sql + " x")[0])
	c.mu.Lock()
	defer c.mu.Unlock()
	switch verb {
	case "SELECT", "WITH":
		c.reads++
	case "INSERT", "UPDATE", "DELETE":
		c.writes++
		if strings.Contains(sql, "sessionless_seen_at") {
			c.activity += d.CommandTag.RowsAffected()
		}
	default:
		c.other = append(c.other, sql)
	}
}

func (c *countingTracer) take() (reads, writes int, activity int64, other []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reads, writes, activity, other = c.reads, c.writes, c.activity, c.other
	c.reads, c.writes, c.activity, c.other = 0, 0, 0, nil
	return
}

// Row 35 (NFR hint budget): statements the layer runs per call, counted by a
// pgx.QueryTracer on the pool its store uses; the tool itself is a stub.
func TestMonitorStalledMiddleware_Budget(t *testing.T) {
	f := newHintFixture(t)
	cfg, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	tracer := &countingTracer{}
	cfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	srv := f.server(traced)
	probe := func(session string) {
		t.Helper()
		call(t, srv, hintCtx(f.user, f.key, session), "probe", nil)
	}
	if err := traced.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	tracer.take()

	probe("sess-35")
	if r, w, _, other := tracer.take(); r != 1 || w != 0 || len(other) != 0 {
		t.Errorf("empty list, with a session: %d reads, %d writes, other %q; want 1 and 0", r, w, other)
	}

	for range 3 {
		f.mint(t, f.key, "sess-35")
	}
	probe("sess-35")
	if r, w, _, other := tracer.take(); r != 1 || w != 1 || len(other) != 0 {
		t.Errorf("three stalled, with a session: %d reads, %d writes, other %q; want 1 and 1", r, w, other)
	}

	f.exec(t, `UPDATE api_keys SET sessionless_seen_at = NULL WHERE id = $1`, f.key)
	f.exec(t, `DELETE FROM monitor_credentials WHERE minted_by_api_key_id = $1`, f.key)
	for range 3 {
		probe("")
	}
	if _, _, activity, _ := tracer.take(); activity != 1 {
		t.Errorf("three sessionless calls in a row changed the activity stamp %d times, want 1", activity)
	}
}

// Row 36: skein_monitor_stalled_items_total counts items actually shown, by
// mine.
func TestMonitorStalledMiddleware_ItemsCounter(t *testing.T) {
	f := newHintFixture(t)
	const s1 = "sess-issuing-36"
	mineID := f.mint(t, f.key, s1)
	f.seen(t, s1)
	otherID := f.mint(t, f.key, "sess-gone-36")
	f.quiet(t, "sess-gone-36")
	mineC := monitorStalledItemsTotal.WithLabelValues("true")
	otherC := monitorStalledItemsTotal.WithLabelValues("false")

	m0, o0 := testutil.ToFloat64(mineC), testutil.ToFloat64(otherC)
	h := f.probe(t, f.key, s1) // mineID as mine=true, otherID as mine=false
	if _, ok := h.item(mineID); !ok {
		t.Fatalf("mine item not shown")
	}
	if it, ok := h.item(otherID); !ok || it["mine"] != false {
		t.Fatalf("other item not shown as mine=false: %v", h.items)
	}
	if d := testutil.ToFloat64(mineC) - m0; d != 1 {
		t.Errorf("mine=true moved by %v, want 1", d)
	}
	if d := testutil.ToFloat64(otherC) - o0; d != 1 {
		t.Errorf("mine=false moved by %v, want 1", d)
	}

	f.probe(t, f.key, s1) // within the throttle: nothing shown
	if d := testutil.ToFloat64(mineC) - m0; d != 1 {
		t.Errorf("a throttled repeat moved mine=true: total delta %v, want 1", d)
	}
	if d := testutil.ToFloat64(otherC) - o0; d != 1 {
		t.Errorf("a throttled repeat moved mine=false: total delta %v, want 1", d)
	}
}

// TestMonitorStalledMiddleware_OnlyVisibleProjects — sk-674.18 matrix row
// 28b (US-1, «stalled monitors», the MCP layer). The assembled layer over
// the real store, on the T003 corpus: every actor owns one stalled monitor
// rooted at the marker issue of every fixture project, minted through its
// own key by its own session, and calls a tool from that session (identity
// from the API key, the user and the session). The answer carries exactly
// the monitors of the projects the actor sees (testsupport.TenancyVisible):
// an unavailable one is neither attached nor counted in
// monitorStalledOmitted; its own are attached. The deactivated actor gets
// none. The internal admin's monitors on the "Personal spaces" of others are minted
// too and must be neither attached nor counted (asserted since T017). The
// store half (row 28a) does not prove the layer.
func TestMonitorStalledMiddleware_OnlyVisibleProjects(t *testing.T) {
	pool := newTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	store := monitorcreds.New(pool, 6*time.Hour)
	srv := probeServer(textAnswer(`{"ok":true}`, false), MonitorStalledMiddleware(MonitorStalledConfig{
		Source: store, Threshold: hintThreshold, AgentPublicURL: gmsTestPublicURL,
	}))
	ctx := context.Background()

	for _, a := range testsupport.TenancyActors {
		t.Run(string(a), func(t *testing.T) {
			u := f.User(t, a)
			key := seedAPIKeyForTool(t, pool, u.ID)
			session := "sess-tn-" + string(a)
			want := map[string]string{} // monitor id → project key
			for _, p := range f.Projects() {
				_, cred, err := store.Mint(ctx, monitorcreds.MintParams{
					UserID: u.ID, MintedByAPIKeyID: key, Audience: "notifications:poll", Capability: "read",
					ProjectID: p.ID, ScopeIssueID: p.IssueID, EventTypes: []string{"comment.created"},
					IssuedSessionID: session, TokenFilePath: "/tmp/skein-mon-tenancy.token",
				})
				if err != nil {
					t.Fatalf("%s: Mint: %v", p.Key(), err)
				}
				if _, err := pool.Exec(ctx, `UPDATE monitor_credentials SET created_at = now() - interval '1 hour' WHERE id = $1`, cred.ID); err != nil {
					t.Fatalf("%s: backdate: %v", p.Key(), err)
				}
				if f.Sees(a, p) {
					want[cred.ID.String()] = p.Key()
				}
			}

			h := hintOf(t, call(t, srv, hintCtx(u.ID, key, session), "probe", nil))
			got := map[string]bool{}
			for _, it := range h.items {
				id, _ := it["monitorId"].(string)
				got[id] = true
				if _, ok := want[id]; !ok {
					t.Errorf("monitor %s of an unavailable project attached", id)
				}
			}
			for id, key := range want {
				if !got[id] {
					t.Errorf("own monitor on visible %s not attached", key)
				}
			}
			if h.omitted != 0 {
				t.Errorf("monitorStalledOmitted = %v, want 0 — at most 5 visible monitors, under the cap", h.omitted)
			}
		})
	}
}
