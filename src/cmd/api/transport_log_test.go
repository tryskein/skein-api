package main

// transport_log_test.go — a refused claim is logged under the transport it
// came through: "public API operation rejected" for a request of the public
// API, "mcp tool rejected" for an MCP call, as before the operations were
// shared. The reference is the defect report: REST claims were logged as
// "mcp tool rejected", so an operator read them as MCP traffic.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// messageRecorder keeps the message of every record of the default logger.
type messageRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (h *messageRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *messageRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *messageRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *messageRecorder) WithGroup(string) slog.Handler      { return h }

// take returns the messages recorded so far and forgets them.
func (h *messageRecorder) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.msgs
	h.msgs = nil
	return out
}

func recordMessages(t *testing.T) *messageRecorder {
	t.Helper()
	rec := &messageRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// wantPrefix asserts that msgs hold want and no message of the other
// transport's prefix.
func wantPrefix(t *testing.T, msgs []string, want, foreign string) {
	t.Helper()
	found := false
	for _, m := range msgs {
		if m == want {
			found = true
		}
		if strings.HasPrefix(m, foreign) {
			t.Errorf("logged %q, a line of the other transport", m)
		}
	}
	if !found {
		t.Errorf("logged %q, want %q among them", msgs, want)
	}
}

func TestTransportLog_RefusedClaim(t *testing.T) {
	t.Run("public API", func(t *testing.T) {
		w := newClassWorld(t)
		key := w.key(w.owner, levelKey(apikeys.LevelWrite))
		id := w.freshIssue(w.p)
		claim := opCall{http.MethodPost, "/api/v1/issues/" + id + "/claim", nil, nil}
		if a := w.do(key, claim, "", map[string]string{"X-Skein-Worker-Id": "w1"}); a.status != http.StatusOK {
			t.Fatalf("first claim: %d %s", a.status, a.body)
		}
		msgs := recordMessages(t)
		a := w.do(key, claim, "", map[string]string{"X-Skein-Worker-Id": "w2"})
		if a.status != http.StatusUnprocessableEntity || a.code() != "already_claimed" {
			t.Fatalf("second claim: %d %s, want 422 already_claimed", a.status, a.body)
		}
		wantPrefix(t, msgs.take(), "public API operation rejected", "mcp tool")
	})

	t.Run("MCP", func(t *testing.T) {
		s := newParityStack(t)
		a := testsupport.ActorProjectOwner
		p := s.f.AMembers
		id := testsupport.SeedIssue(t, s.pool, p.ID, s.f.User(t, a).ID, p.IDPrefix+"-971", "claimed twice", "open", 2)
		if r := s.tool(a, s.session(a), "skein_claim", map[string]any{"id": id}); r.isError {
			t.Fatalf("first claim: %s", r.text)
		}
		sid := s.session(a)
		msgs := recordMessages(t)
		r := s.tool(a, sid, "skein_claim", map[string]any{"id": id})
		if !r.isError || !strings.Contains(r.text, "already_claimed") {
			t.Fatalf("second claim: %s, want already_claimed", r.text)
		}
		wantPrefix(t, msgs.take(), "mcp tool rejected", "public API")
	})
}
