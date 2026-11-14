// protocol_versions_test.go locks which MCP protocol revisions the /mcp
// transport offers.
//
// mcp-go v1 also serves the stateless 2026-07-28 revision, which has no
// initialize handshake and no Mcp-Session-Id. Agent identity, the agent
// registry and the monitor all key on that header, so Setup restricts the
// transport to the session-oriented revisions. Dropping that restriction
// compiles and serves fine; only these requests notice.
//
// The expected answers come from the mcp-go v1.5.0 transport contract
// (server/streamable_http_modern.go validateModernRequest, mcp/errors.go
// UnsupportedProtocolVersionError): a modern request for a revision the
// transport does not serve is refused with HTTP 400 and JSON-RPC error -32022,
// whose data lists the revisions it does serve.
//
// DB-free (nil pool, no authenticated context), so it runs in the unit job.

package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const (
	statelessProtocolVersion    = "2026-07-28"
	latestLegacyProtocolVersion = "2025-11-25"
	// unsupportedProtocolVersionCode is UNSUPPORTED_PROTOCOL_VERSION of the
	// 2026-07-28 specification.
	unsupportedProtocolVersionCode = -32022
)

func TestSetup_StatelessProtocolRevisionNotOffered(t *testing.T) {
	handler, _ := Setup(nil, 0, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for _, method := range []string{"server/discover", "tools/list"} {
		t.Run(method+" with 2026-07-28 is refused", func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":{"_meta":{` +
				`"io.modelcontextprotocol/protocolVersion":"` + statelessProtocolVersion + `",` +
				`"io.modelcontextprotocol/clientInfo":{"name":"protocol-test","version":"1"},` +
				`"io.modelcontextprotocol/clientCapabilities":{}}}}`
			req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
			if err != nil {
				t.Fatalf("building request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", statelessProtocolVersion)

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("POST %s: %v", method, err)
			}
			defer resp.Body.Close()

			var got struct {
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code int `json:"code"`
					Data struct {
						Supported []string `json:"supported"`
						Requested string   `json:"requested"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decoding %s response (status %d): %v", method, resp.StatusCode, err)
			}
			if resp.StatusCode != http.StatusBadRequest || got.Error == nil {
				t.Fatalf("%s with protocol %s: status %d, result %s; want 400 with a JSON-RPC error",
					method, statelessProtocolVersion, resp.StatusCode, got.Result)
			}
			if got.Error.Code != unsupportedProtocolVersionCode {
				t.Errorf("error code = %d, want %d", got.Error.Code, unsupportedProtocolVersionCode)
			}
			if got.Error.Data.Requested != statelessProtocolVersion {
				t.Errorf("error data requested = %q, want %q", got.Error.Data.Requested, statelessProtocolVersion)
			}
			supported := got.Error.Data.Supported
			if slices.Contains(supported, statelessProtocolVersion) {
				t.Errorf("supported versions %v offer %s", supported, statelessProtocolVersion)
			}
			for _, want := range []string{latestLegacyProtocolVersion, testProtocolVersion} {
				if !slices.Contains(supported, want) {
					t.Errorf("supported versions %v lack session-oriented %s", supported, want)
				}
			}
		})
	}

	t.Run("legacy initialize mints a session", func(t *testing.T) {
		// mcpInitialize fails the test unless initialize answers 200 with an
		// Mcp-Session-Id header.
		sessionID, initializedStatus := mcpInitialize(t, srv)
		if sessionID == "" {
			t.Fatal("initialize minted no Mcp-Session-Id")
		}
		if initializedStatus != http.StatusAccepted {
			t.Errorf("notifications/initialized status = %d, want 202", initializedStatus)
		}
	})
}
