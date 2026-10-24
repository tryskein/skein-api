// heartbeat_test.go locks the sk-274.1 transport keepalive end-to-end.
//
// The bug it guards against is invisible in code review: the heartbeat only
// reaches a client that opened the standalone GET (SSE) stream, and the MCP
// TypeScript SDK opens that stream ONLY when the POST carrying
// notifications/initialized answers with exactly 202 Accepted. A future change
// that answers 200 there — or that drops the option while wiring another one —
// would leave the transport looking healthy and silently un-keepalived until
// the next gateway idle-timeout incident.
//
// DB-free on purpose (nil pool: the agents UPSERT is skipped without an
// authenticated context), so this runs in the plain unit job.

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testProtocolVersion = "2025-06-18"

// mcpInitialize performs the handshake against srv and returns the session id
// the server minted, plus the status code it answered the initialized
// notification with.
func mcpInitialize(t *testing.T, srv *httptest.Server) (sessionID string, initializedStatus int) {
	t.Helper()

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"` + testProtocolVersion + `",` +
		`"capabilities":{},"clientInfo":{"name":"heartbeat-test","version":"1"}}}`

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(initBody))
	if err != nil {
		t.Fatalf("building initialize request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}

	sessionID = resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response carries no Mcp-Session-Id header")
	}

	notifyReq, err := http.NewRequest(http.MethodPost, srv.URL,
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("building initialized request: %v", err)
	}
	notifyReq.Header.Set("Content-Type", "application/json")
	notifyReq.Header.Set("Accept", "application/json, text/event-stream")
	notifyReq.Header.Set("Mcp-Session-Id", sessionID)

	notifyResp, err := srv.Client().Do(notifyReq)
	if err != nil {
		t.Fatalf("initialized notification: %v", err)
	}
	defer notifyResp.Body.Close()

	return sessionID, notifyResp.StatusCode
}

// TestInitializedNotificationReturns202 pins the precondition for the whole
// heartbeat scheme: a client that gets anything other than 202 here never opens
// the GET stream, and the keepalive becomes dead code.
func TestInitializedNotificationReturns202(t *testing.T) {
	handler, _ := Setup(nil, 30*time.Second, nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	if _, status := mcpInitialize(t, srv); status != http.StatusAccepted {
		t.Fatalf("notifications/initialized status = %d, want 202 — "+
			"the MCP client opens its GET stream only on 202, so the heartbeat would never be seen",
			status)
	}
}

// TestHeartbeatPingsGETStream proves the keepalive actually reaches the wire:
// with a short interval the GET stream must carry JSON-RPC pings.
func TestHeartbeatPingsGETStream(t *testing.T) {
	handler, _ := Setup(nil, 50*time.Millisecond, nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	sessionID, _ := mcpInitialize(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("building GET request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("opening GET stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET stream status = %d, want 200 (405 means the server offers no SSE stream at all)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("GET stream Content-Type = %q, want text/event-stream", ct)
	}

	// Read until the first ping frame or the context deadline. The scan runs in
	// a goroutine so a stream that never emits anything fails on the timeout
	// rather than hanging the whole package. Cleanup joins the goroutine after
	// the deferred cancel/Close have unblocked it, so the reader cannot outlive
	// the test and race the httptest server's own shutdown.
	found := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(wg.Wait)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var frame struct {
				JSONRPC string `json:"jsonrpc"`
				Method  string `json:"method"`
			}
			if err := json.Unmarshal([]byte(payload), &frame); err != nil {
				continue
			}
			if frame.Method == "ping" {
				found <- payload
				return
			}
		}
	}()

	select {
	case payload := <-found:
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
			ID      any    `json:"id"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("ping frame is not valid JSON: %v", err)
		}
		// The client answers a ping *request* (the SDK has a built-in handler);
		// a malformed frame without jsonrpc/id would be dropped instead.
		if frame.JSONRPC != "2.0" {
			t.Fatalf("ping frame jsonrpc = %q, want \"2.0\"", frame.JSONRPC)
		}
		if frame.ID == nil {
			t.Fatal("ping frame carries no id — clients cannot answer it")
		}
	case <-ctx.Done():
		t.Fatal("no ping received on the GET stream within 5s — heartbeat is not reaching the client")
	}
}

// TestHeartbeatDisabledSendsNoPings pins the off switch: interval 0 must leave
// the stream silent (mcp-go's own default), so operators can turn the keepalive
// off without a code change.
func TestHeartbeatDisabledSendsNoPings(t *testing.T) {
	handler, _ := Setup(nil, 0, nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	sessionID, _ := mcpInitialize(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("building GET request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("opening GET stream: %v", err)
	}
	defer resp.Body.Close()

	pinged := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(wg.Wait)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), `"ping"`) {
				pinged <- struct{}{}
				return
			}
		}
	}()

	select {
	case <-pinged:
		t.Fatal("received a ping with the heartbeat disabled (interval 0)")
	case <-ctx.Done():
		// No ping within the window — expected.
	}
}

// TestLogMCPRequestFieldsAndPrivacy locks the diagnostic contract of the
// /mcp debug trace, which both review engines flagged as untested.
//
// Two things are pinned. First the field set: a post-mortem needs to tell a
// GET keepalive stream apart from an ordinary POST, and that judgement is made
// from `method` + `accept`. Second, and the reason this test is not optional:
// Mcp-Session-Id is the session bearer for this transport, so only its
// PRESENCE may be logged. Nothing keys the log sanitiser to this header, so a
// future "let's log the session id, it helps debugging" edit would leak
// credentials into every debug line with nothing to stop it but this test.
func TestLogMCPRequestFieldsAndPrivacy(t *testing.T) {
	const secretSessionID = "mcp-session-2f9c41d6-never-log-me"

	cases := []struct {
		name       string
		sessionID  string
		accept     string
		method     string
		wantFields []string
	}{
		{
			name:      "GET stream with a session",
			sessionID: secretSessionID,
			accept:    "text/event-stream",
			method:    http.MethodGet,
			wantFields: []string{
				`msg="mcp request"`, "method=GET", "path=/mcp",
				"hasSessionId=true", "accept=text/event-stream",
			},
		},
		{
			name:      "POST before the client has a session id",
			sessionID: "",
			accept:    "application/json",
			method:    http.MethodPost,
			wantFields: []string{
				"method=POST", "hasSessionId=false", "accept=application/json",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			var reached bool
			handler := logMCPRequest(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached = true
			}))

			req := httptest.NewRequest(tc.method, "/mcp", nil)
			req.Header.Set("Accept", tc.accept)
			if tc.sessionID != "" {
				req.Header.Set("Mcp-Session-Id", tc.sessionID)
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if !reached {
				t.Fatal("logging middleware did not call the next handler")
			}

			logged := buf.String()
			if strings.Contains(logged, secretSessionID) {
				t.Fatalf("Mcp-Session-Id value leaked into the log line: %s", logged)
			}
			for _, want := range tc.wantFields {
				if !strings.Contains(logged, want) {
					t.Fatalf("log line missing %q; got: %s", want, logged)
				}
			}
		})
	}
}
