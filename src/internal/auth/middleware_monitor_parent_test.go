package auth_test

// middleware_monitor_parent_test.go — matrix row 36 (INV-11, SC-019, US-4
// "derived tokens"). Reference — the spec, written before the code: a skm_
// monitor token minted by a key stops serving on the next /agent/v1 poll when
// the parent key expired, was revoked, lost the read permission, its owner
// was deactivated or the issue left the key's scope — the poll answers 401,
// as for a revoked monitor; widening the key or its scope afterwards does not
// restore the token. A monitor can be minted only for an issue inside the
// key's scope; outside it the mint answers as for a nonexistent issue.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

const parentTestAudience = "notifications:poll"

type parentFixture struct {
	pool    *pgxpool.Pool
	store   *monitorcreds.Store
	user    uuid.UUID
	project uuid.UUID
	other   uuid.UUID
	oPrefix string
	issue   string
	key     uuid.UUID
}

func newParentFixture(t *testing.T) parentFixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	u, _ := testsupport.SeedUser(t, pool, "user")
	p, prefix := testsupport.SeedProject(t, pool, u, "mparent")
	other, otherPrefix := testsupport.SeedProject(t, pool, u, "mother")
	issue := testsupport.SeedIssue(t, pool, p, u, prefix+"-1", "monitored", "open", 2)
	_, keyPrefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := testsupport.CreateOwnerKey(context.Background(), pool, u, "monitor parent test", keyPrefix, hash)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, key) })
	return parentFixture{pool: pool, store: monitorcreds.New(pool, 6*time.Hour), user: u, project: p, other: other, oPrefix: otherPrefix, issue: issue, key: key}
}

func (f parentFixture) mint(t *testing.T) (string, uuid.UUID) {
	t.Helper()
	plain, cred, err := f.store.Mint(context.Background(), monitorcreds.MintParams{
		UserID: f.user, MintedByAPIKeyID: f.key, Audience: parentTestAudience, Capability: "read",
		ProjectID: f.project, ScopeIssueID: f.issue, ScopeSubtree: true, EventTypes: []string{"comment.created"},
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return plain, cred.ID
}

func (f parentFixture) poll(t *testing.T, plain string) int {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil)
	req.Header.Set(auth.HeaderAPIKey, plain)
	rec := httptest.NewRecorder()
	auth.RequireMonitorToken(f.store, parentTestAudience)(ok).ServeHTTP(rec, req)
	return rec.Code
}

func (f parentFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestRequireMonitorToken_ParentKeyCheckedOnEveryPoll(t *testing.T) {
	for _, c := range []struct {
		name    string
		fail    func(t *testing.T, f parentFixture)
		restore func(t *testing.T, f parentFixture)
	}{
		{"parent key expired",
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, f.key)
			},
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE api_keys SET expires_at = now() + interval '30 days' WHERE id = $1`, f.key)
			}},
		{"parent key revoked",
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, f.key)
			},
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE api_keys SET revoked_at = NULL WHERE id = $1`, f.key)
			}},
		{"parent key lost the read permission",
			func(t *testing.T, f parentFixture) {
				// The admin flag alone grants user operations only, no read.
				f.exec(t, `UPDATE api_keys SET level = NULL, is_admin = true WHERE id = $1`, f.key)
			},
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE api_keys SET level = 'manage', is_admin = false WHERE id = $1`, f.key)
			}},
		{"owner deactivated",
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE users SET deactivated_at = now() WHERE id = $1`, f.user)
			},
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE users SET deactivated_at = NULL WHERE id = $1`, f.user)
			}},
		{"issue left the key's scope",
			func(t *testing.T, f parentFixture) {
				f.exec(t, `UPDATE api_keys SET scope_mode = 'projects' WHERE id = $1`, f.key)
				f.exec(t, `INSERT INTO api_key_scope_projects (api_key_id, project_id) VALUES ($1, $2)`, f.key, f.other)
			},
			func(t *testing.T, f parentFixture) {
				f.exec(t, `DELETE FROM api_key_scope_projects WHERE api_key_id = $1`, f.key)
				f.exec(t, `UPDATE api_keys SET scope_mode = 'all' WHERE id = $1`, f.key)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newParentFixture(t)
			plain, credID := f.mint(t)
			if code := f.poll(t, plain); code != http.StatusOK {
				t.Fatalf("poll before the change = %d, want 200", code)
			}
			c.fail(t, f)
			if code := f.poll(t, plain); code != http.StatusUnauthorized {
				t.Fatalf("poll after the change = %d, want 401", code)
			}
			var revoked bool
			if err := f.pool.QueryRow(context.Background(),
				`SELECT revoked_at IS NOT NULL FROM monitor_credentials WHERE id = $1`, credID).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			if !revoked {
				t.Errorf("the token was not revoked when its parent failed")
			}
			// Widening back: the parent is usable again, the token is not.
			c.restore(t, f)
			if code := f.poll(t, plain); code != http.StatusUnauthorized {
				t.Errorf("poll after widening the parent back = %d, want 401 — the token came back", code)
			}
			fresh, _ := f.mint(t)
			if code := f.poll(t, fresh); code != http.StatusOK {
				t.Errorf("a fresh token of the restored parent = %d, want 200 — the restore did not restore", code)
			}
		})
	}
}

// TestGetMonitorScript_OutsideTheKeyScopeIsNonexistent — row 36, the mint:
// a key with scope "project P" mints no monitor for an issue of another
// project the owner sees; the answer is the nonexistent issue's.
func TestGetMonitorScript_OutsideTheKeyScopeIsNonexistent(t *testing.T) {
	f := newParentFixture(t)
	outside := testsupport.SeedIssue(t, f.pool, f.other, f.user, f.oPrefix+"-1", "outside the scope", "open", 2)
	s := server.NewMCPServer("monitor-parent", "1", server.WithToolCapabilities(true))
	tools.RegisterGetMonitorScript(s, f.pool, notifications.NewStore(f.pool), f.store, "http://skein-monitor.test", 6*time.Hour)
	ctx := auth.WithUserID(context.Background(), f.user)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, "monitor-parent")
	ctx = auth.WithAPIKeyID(ctx, f.key)
	ctx = auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelRead,
		Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{f.project}}})
	code := func(issueID string) (bool, string) {
		t.Helper()
		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "skein_get_monitor_script", "arguments": map[string]any{"id": issueID}}})
		raw, _ := json.Marshal(s.HandleMessage(ctx, msg))
		var r struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || len(r.Result.Content) == 0 {
			t.Fatalf("decode %s: %v", raw, err)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(r.Result.Content[0].Text), &body)
		c, _ := body["error"].(string)
		return r.Result.IsError, c
	}
	gotErr, got := code(outside)
	_, want := code("nope-" + uuid.NewString()[:8] + "-1")
	if !gotErr || got != want || got == "" {
		t.Errorf("mint for an issue outside the scope = isError %v %q, want the nonexistent answer %q", gotErr, got, want)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM monitor_credentials WHERE scope_issue_id = $1`, outside).Scan(&n); err != nil || n != 0 {
		t.Errorf("credentials minted for the outside issue: %d (%v)", n, err)
	}
	if okErr, _ := code(f.issue); okErr {
		t.Errorf("mint for the issue inside the scope refused — the comparison above proves nothing")
	}
}
