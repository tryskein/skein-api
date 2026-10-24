package main

// session_epoch_route_test.go — matrix row 57 on the production router.
// Reference — the spec (INV-26, K-16): after
// the user's session epoch rises (a password reset), a request with the
// previous epoch is refused with 401 session_revoked, the new one passes,
// another user's sessions are untouched, and the same user's agent key
// keeps working. The router is the one main builds, so a missing mount of
// RequireSessionEpoch on /api/v1 turns this red
// (handler/middleware_session_test.go checks the middleware itself).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

func TestSessionEpoch_ProductionRouter(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	router := watchingWindowRouter(t, pool)
	u, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")

	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := apikeys.New(pool).Create(ctx, u, "epoch-route", prefix, hash); err != nil {
		t.Fatalf("create key: %v", err)
	}

	session := func(user uuid.UUID, epoch int64) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me/notification-counts", nil)
		req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		req.Header.Set(auth.HeaderUserID, user.String())
		req.Header.Set(auth.HeaderUserRole, "user")
		req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		code, _ := body["error"].(string)
		return rec.Code, code
	}
	// key opens an MCP session with u's agent key and returns why it did
	// not succeed ("" when it did): a working key is a completed
	// initialize — 200, a JSON-RPC result and no error, a session id — not
	// merely the absence of 401/403.
	key := func() string {
		raw, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
				"clientInfo": map[string]any{"name": "epoch-route", "version": "1"},
			},
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		req.Header.Set(auth.HeaderAPIKey, plain)
		req.Header.Set(auth.HeaderSurface, "internal")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return fmt.Sprintf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			return fmt.Sprintf("response is not JSON: %v: %s", err, rec.Body.String())
		}
		if out["error"] != nil {
			return fmt.Sprintf("JSON-RPC error: %v", out["error"])
		}
		if result, _ := out["result"].(map[string]any); len(result) == 0 {
			return fmt.Sprintf("no JSON-RPC result: %v", out)
		}
		if rec.Header().Get("Mcp-Session-Id") == "" {
			return "no Mcp-Session-Id: the session was not opened"
		}
		return ""
	}

	// Positive control.
	if code, errCode := session(u, 0); code != http.StatusOK {
		t.Fatalf("u's session before the raise = %d %s, want 200", code, errCode)
	}
	if why := key(); why != "" {
		t.Fatalf("u's key before the raise: initialize failed: %s", why)
	}

	var raised int64
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		raised, err = users.BumpSessionEpochInTx(ctx, tx, u)
		return err
	}); err != nil {
		t.Fatalf("BumpSessionEpochInTx: %v", err)
	}

	if code, errCode := session(u, 0); code != http.StatusUnauthorized || errCode != auth.CodeSessionRevoked {
		t.Errorf("u's session with the previous epoch = %d %q, want 401 %q", code, errCode, auth.CodeSessionRevoked)
	}
	if code, errCode := session(u, raised); code != http.StatusOK {
		t.Errorf("u's session with the new epoch = %d %s, want 200", code, errCode)
	}
	if code, errCode := session(other, 0); code != http.StatusOK {
		t.Errorf("another user's session = %d %s, want 200 — the epoch is per user", code, errCode)
	}
	if why := key(); why != "" {
		t.Errorf("u's agent key after the raise: initialize failed: %s — the raise revokes no key", why)
	}
}
