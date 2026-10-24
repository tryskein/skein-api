package tools

// monitor_stop_test.go — sk-579 matrix rows 26 and 27 (US-6, SC-010, INV-11):
// skein_watchers(action="monitor_stop") on the tool, observed through the
// real monitor door (RequireMonitorToken + Poll), and the stop counter.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/monitorcreds"
)

// stopFixture is one user with two api keys, an issue, and a store.
type stopFixture struct {
	pool     *pgxpool.Pool
	store    *monitorcreds.Store
	userID   uuid.UUID
	keyA     uuid.UUID
	keyB     uuid.UUID
	project  uuid.UUID
	issueID  string
	pollDoor http.Handler
}

func newStopFixture(t *testing.T) stopFixture {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "mst")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "stop me", "open", 2)
	store := monitorcreds.New(pool, 6*time.Hour)
	return stopFixture{
		pool:    pool,
		store:   store,
		userID:  userID,
		keyA:    seedAPIKeyForTool(t, pool, userID),
		keyB:    seedAPIKeyForTool(t, pool, userID),
		project: pid,
		issueID: issueID,
		pollDoor: auth.RequireMonitorToken(store, "notifications:poll")(
			http.HandlerFunc(handler.NewAgentPollHandler(pool, time.Second).Poll)),
	}
}

// mint issues a monitor on the fixture issue through the given key.
func (f stopFixture) mint(t *testing.T, key uuid.UUID) (string, uuid.UUID) {
	t.Helper()
	token, cred, err := f.store.Mint(context.Background(), monitorcreds.MintParams{
		UserID:           f.userID,
		MintedByAPIKeyID: key,
		Audience:         "notifications:poll",
		Capability:       "read",
		ProjectID:        f.project,
		ScopeIssueID:     f.issueID,
		EventTypes:       []string{"comment.created", "issue.closed"},
		IssuedSessionID:  "sess-stop",
		TokenFilePath:    "/tmp/skein-mon-stop.token",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return token, cred.ID
}

// poll runs one poll through the real monitor door and returns its status.
func (f stopFixture) poll(t *testing.T, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil)
	req.Header.Set(auth.HeaderAPIKey, token)
	rec := httptest.NewRecorder()
	f.pollDoor.ServeHTTP(rec, req)
	return rec.Code
}

// stop calls monitor_stop as ctx with the given monitorId.
func stop(t *testing.T, store *monitorcreds.Store, ctx context.Context, monitorID string) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"action": "monitor_stop", "monitorId": monitorID}
	res, err := handleMonitorStop(store)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return res
}

func resultText(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want one content block, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("content is %T, want TextContent", res.Content[0])
	}
	return tc.Text
}

// Row 26: the monitor named by monitorId — and only it — is stopped, by its
// owning user from any key; a foreign and an unknown id get one identical
// refusal.
func TestMonitorStop_StopsExactlyThatMonitor(t *testing.T) {
	f := newStopFixture(t)
	tokenA, idA := f.mint(t, f.keyA)
	tokenB, _ := f.mint(t, f.keyA)
	_, idC := f.mint(t, f.keyA)
	ctxA := withCtxKey(f.userID, "user", f.keyA)

	if f.poll(t, tokenA) != http.StatusOK || f.poll(t, tokenB) != http.StatusOK {
		t.Fatalf("both monitors must poll before the stop")
	}

	res := stop(t, f.store, ctxA, idA.String())
	if res.IsError {
		t.Fatalf("monitor_stop of an own monitor failed: %s", resultText(t, res))
	}
	if body := extractToolJSON(t, res); body["monitorId"] != idA.String() || body["stopped"] != true || body["alreadyStopped"] != false {
		t.Fatalf("monitor_stop answer = %v", body)
	}
	if got := f.poll(t, tokenA); got != http.StatusUnauthorized {
		t.Errorf("poll with the stopped monitor's token = %d, want 401", got)
	}
	if got := f.poll(t, tokenB); got != http.StatusOK {
		t.Errorf("poll of the other monitor on the same issue = %d, want 200 (only the named monitor stops)", got)
	}

	if res := stop(t, f.store, ctxA, idA.String()); res.IsError {
		t.Errorf("repeating the stop failed: %s", resultText(t, res))
	} else if body := extractToolJSON(t, res); body["alreadyStopped"] != true {
		t.Errorf("repeat monitor_stop answer = %v, want alreadyStopped=true", body)
	}

	// The same user, another of its keys.
	if res := stop(t, f.store, withCtxKey(f.userID, "user", f.keyB), idC.String()); res.IsError {
		t.Errorf("stopping an own monitor from another key failed: %s", resultText(t, res))
	}

	// Another user, and an id nobody has: one refusal, byte for byte.
	strangerID, _ := seedUser(t, f.pool, "user")
	strangerKey := seedAPIKeyForTool(t, f.pool, strangerID)
	strangerCtx := withCtxKey(strangerID, "user", strangerKey)
	_, idB := f.mint(t, f.keyA)
	foreign := stop(t, f.store, strangerCtx, idB.String())
	unknown := stop(t, f.store, strangerCtx, uuid.NewString())
	for name, r := range map[string]*mcpgo.CallToolResult{"foreign": foreign, "unknown": unknown} {
		if !r.IsError {
			t.Errorf("%s id: want isError, got %s", name, resultText(t, r))
			continue
		}
		if code := extractToolJSON(t, r)["error"]; code != ErrCodeMonitorNotFound {
			t.Errorf("%s id: error code = %v, want %s", name, code, ErrCodeMonitorNotFound)
		}
	}
	if a, b := resultText(t, foreign), resultText(t, unknown); a != b {
		t.Errorf("foreign and unknown refusals differ:\n%s\n%s", a, b)
	}

	bad := stop(t, f.store, ctxA, "not-a-uuid")
	if !bad.IsError || extractToolJSON(t, bad)["error"] != ErrCodeValidationError {
		t.Errorf("a non-UUID monitorId: got %s, want validation_error", resultText(t, bad))
	}
}

// Row 27: the counter follows the fact of revocation — one stop +1, a repeat
// on the already stopped monitor +0, a refusal +0.
func TestMonitorStop_CountsActualStops(t *testing.T) {
	f := newStopFixture(t)
	_, id := f.mint(t, f.keyA)
	ctx := withCtxKey(f.userID, "user", f.keyA)
	before := testutil.ToFloat64(monitorStopsTotal)

	if res := stop(t, f.store, ctx, id.String()); res.IsError {
		t.Fatalf("stop: %s", resultText(t, res))
	}
	if got := testutil.ToFloat64(monitorStopsTotal) - before; got != 1 {
		t.Fatalf("after one stop the counter moved by %v, want 1", got)
	}
	if res := stop(t, f.store, ctx, id.String()); res.IsError {
		t.Fatalf("repeat stop: %s", resultText(t, res))
	}
	if got := testutil.ToFloat64(monitorStopsTotal) - before; got != 1 {
		t.Fatalf("a repeat stop moved the counter: total delta %v, want 1", got)
	}
	if res := stop(t, f.store, ctx, uuid.NewString()); !res.IsError {
		t.Fatalf("an unknown id was not refused")
	}
	if got := testutil.ToFloat64(monitorStopsTotal) - before; got != 1 {
		t.Fatalf("a refusal moved the counter: total delta %v, want 1", got)
	}
}
