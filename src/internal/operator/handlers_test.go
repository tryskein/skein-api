package operator_test

// handlers_test.go — sk-674.18, the test delivery of rows 181 and 183
// (T042, US-11), through the operator's router over the real users store.
// The references, written before the code's output:
//
//   - US-11: blocking is the deactivation — sign-in refused, sessions and
//     keys stop working, memberships and assignments stay, the spaces keep
//     working for the others; unblocking lets the account sign in again
//     (K-16: the session epoch is not lowered, so a session issued before
//     the block stays refused, and the revoked keys stay revoked);
//     deleting carries US-10's limits — an owner of a space with other
//     people is refused and the spaces are named.
//   - The contract was accepted for T042 (operator/contract.go): the
//     account by email, case-insensitively; account_not_found for an email
//     no account carries — a deleted account's included; 400
//     invalid_request without an email.
//   - Row 183: skein_operator_requests_total{operation,result} moves its
//     own pair on a success and on a refusal.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/operator"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

const accountPassword = "operator test password 7"

// opRig is the operator's router over the real store, and the doors an
// account goes through: password login, a browser session, an API key.
type opRig struct {
	pool     *pgxpool.Pool
	router   http.Handler
	login    *handler.AuthHandler
	sessions http.Handler
}

func newOpRig(t *testing.T) *opRig {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	login, err := handler.NewAuthHandler(pool, auth.MinBcryptCost, "local", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := chi.NewRouter()
	s.Use(handler.ParseUserHeaders, handler.RequireSessionEpoch(pool))
	s.Get("/probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return &opRig{pool: pool, router: operator.NewRouter(testKey, operator.NewAccounts(pool, store)), login: login, sessions: s}
}

// account is a live account with a real password and an API key.
type account struct {
	id         uuid.UUID
	email      string
	username   string
	key        string
	firstEpoch int64
}

func (r *opRig) seedAccount(t *testing.T) account {
	t.Helper()
	ctx := context.Background()
	id, username := testsupport.SeedUser(t, r.pool, "user")
	hash, err := auth.Hash(accountPassword, auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	a := account{id: id, username: username}
	if err := r.pool.QueryRow(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1 RETURNING email, session_epoch`, id, hash).
		Scan(&a.email, &a.firstEpoch); err != nil {
		t.Fatal(err)
	}
	plain, prefix, keyHash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := apikeys.New(r.pool).Create(ctx, id, "op-test", prefix, keyHash); err != nil {
		t.Fatal(err)
	}
	a.key = plain
	t.Cleanup(func() {
		c := context.Background()
		_, _ = r.pool.Exec(c, `DELETE FROM events WHERE actor_user_id = $1 OR (entity_type = 'user' AND entity_id = $1::text)`, id)
		_, _ = r.pool.Exec(c, `
DELETE FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, a.email, username)
	})
	return a
}

// operate calls the operator's API with the key.
func (r *opRig) operate(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set(operator.KeyHeader, testKey)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (r *opRig) signIn(t *testing.T, a account) int {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"email": a.email, "password": accountPassword})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.login.Login(rec, req)
	return rec.Code
}

func (r *opRig) session(t *testing.T, a account, epoch int64) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(auth.HeaderUserID, a.id.String())
	req.Header.Set(auth.HeaderUserRole, "user")
	req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
	rec := httptest.NewRecorder()
	r.sessions.ServeHTTP(rec, req)
	return rec.Code
}

func (r *opRig) keyWorks(t *testing.T, a account) int {
	t.Helper()
	chain := auth.RequireAPIKey(r.pool, auth.MinBcryptCost, "")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set(auth.HeaderAPIKey, a.key)
	req.Header.Set(auth.HeaderSurface, "internal")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	return rec.Code
}

func (r *opRig) epoch(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	var e int64
	if err := r.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id = $1`, id).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestOperator_BlockUnblockDelete — row 181.
func TestOperator_BlockUnblockDelete(t *testing.T) {
	r := newOpRig(t)
	ctx := context.Background()

	t.Run("block is the deactivation, unblock lets in without reviving", func(t *testing.T) {
		a := r.seedAccount(t)
		owner, _ := testsupport.SeedUser(t, r.pool, "user")
		space := testsupport.SeedSpace(t, r.pool, owner)
		testsupport.AddSpaceMember(t, r.pool, space, a.id)
		project, prefix := testsupport.SeedProjectInSpace(t, r.pool, owner, space, "opb")
		testsupport.AddProjectMember(t, r.pool, project, a.id, "member")
		issue := testsupport.SeedIssue(t, r.pool, project, owner, prefix+"-1", "on him", "open", 2)
		testsupport.SetAssignee(t, r.pool, issue, a.username)
		if r.signIn(t, a) != http.StatusOK || r.session(t, a, a.firstEpoch) != http.StatusOK || r.keyWorks(t, a) != http.StatusOK {
			t.Fatal("the account does not work before the block — the checks below would prove nothing")
		}

		if code, body := r.operate(t, operator.PathBlock, map[string]string{"email": strings.ToUpper(a.email)}); code != http.StatusNoContent {
			t.Fatalf("block = %d %v, want 204", code, body)
		}
		if code := r.signIn(t, a); code == http.StatusOK {
			t.Error("a blocked account signs in")
		}
		if code := r.session(t, a, a.firstEpoch); code != http.StatusUnauthorized {
			t.Errorf("the session after the block = %d, want 401", code)
		}
		if code := r.keyWorks(t, a); code != http.StatusUnauthorized {
			t.Errorf("the key after the block = %d, want 401", code)
		}
		if n := count(t, r.pool, `SELECT count(*) FROM space_members WHERE space_id = $1 AND user_id = $2`, space, a.id); n != 1 {
			t.Error("the block took the space membership away")
		}
		if n := count(t, r.pool, `SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`, project, a.id); n != 1 {
			t.Error("the block took the project membership away")
		}
		var assignee *string
		if err := r.pool.QueryRow(ctx, `SELECT assignee FROM issues WHERE id = $1`, issue).Scan(&assignee); err != nil {
			t.Fatal(err)
		}
		if assignee == nil || *assignee != a.username {
			t.Errorf("assignee after the block = %v, want kept %q", assignee, a.username)
		}
		if n := count(t, r.pool, `SELECT count(*) FROM space_members WHERE space_id = $1 AND user_id = $2`, space, owner); n != 1 {
			t.Error("the space's owner lost the space")
		}
		if code, _ := r.operate(t, operator.PathBlock, map[string]string{"email": a.email}); code != http.StatusNoContent {
			t.Errorf("blocking again = %d, want 204", code)
		}

		if code, body := r.operate(t, operator.PathUnblock, map[string]string{"email": a.email}); code != http.StatusNoContent {
			t.Fatalf("unblock = %d %v, want 204", code, body)
		}
		if code := r.signIn(t, a); code != http.StatusOK {
			t.Errorf("sign-in after the unblock = %d, want 200", code)
		}
		if code := r.session(t, a, a.firstEpoch); code != http.StatusUnauthorized {
			t.Errorf("the session issued before the block, after the unblock = %d, want still 401", code)
		}
		if code := r.session(t, a, r.epoch(t, a.id)); code != http.StatusOK {
			t.Errorf("a session issued after the unblock = %d, want 200", code)
		}
		if code := r.keyWorks(t, a); code != http.StatusUnauthorized {
			t.Errorf("the key revoked by the block, after the unblock = %d, want still 401", code)
		}
	})

	t.Run("delete with US-10's limits", func(t *testing.T) {
		owner := r.seedAccount(t)
		other, _ := testsupport.SeedUser(t, r.pool, "user")
		shared := testsupport.SeedSpace(t, r.pool, owner.id)
		testsupport.AddSpaceMember(t, r.pool, shared, other)
		var short string
		if err := r.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, shared).Scan(&short); err != nil {
			t.Fatal(err)
		}
		code, body := r.operate(t, operator.PathDelete, map[string]string{"email": owner.email})
		if code != http.StatusUnprocessableEntity || body["error"] != "owns_spaces_with_members" {
			t.Fatalf("deleting an owner of a space with people = %d %v, want 422 owns_spaces_with_members", code, body)
		}
		named, _ := body["spaces"].([]any)
		if len(named) != 1 || named[0].(map[string]any)["id"] != short {
			t.Errorf("named spaces = %v, want [%s]", body["spaces"], short)
		}
		if n := count(t, r.pool, `SELECT count(*) FROM users WHERE id = $1 AND deleted_at IS NULL`, owner.id); n != 1 {
			t.Error("the refused owner was deleted")
		}

		plain := r.seedAccount(t)
		if code, body := r.operate(t, operator.PathDelete, map[string]string{"email": plain.email}); code != http.StatusNoContent {
			t.Fatalf("delete = %d %v, want 204", code, body)
		}
		if n := count(t, r.pool, `SELECT count(*) FROM users WHERE id = $1 AND deleted_at IS NOT NULL AND email <> $2`, plain.id, plain.email); n != 1 {
			t.Error("the account is not deleted and anonymized")
		}
		if code := r.keyWorks(t, plain); code != http.StatusUnauthorized {
			t.Errorf("the deleted account's key = %d, want 401", code)
		}
		code, body = r.operate(t, operator.PathBlock, map[string]string{"email": plain.email})
		if code != http.StatusUnprocessableEntity || body["error"] != operator.CodeAccountNotFound {
			t.Errorf("blocking a deleted account by its old email = %d %v, want 422 account_not_found", code, body)
		}
	})

	t.Run("an unknown email and an empty body", func(t *testing.T) {
		for _, path := range []string{operator.PathBlock, operator.PathUnblock, operator.PathDelete} {
			code, body := r.operate(t, path, map[string]string{"email": "nobody-" + uuid.NewString()[:8] + "@nowhere.test"})
			if code != http.StatusUnprocessableEntity || body["error"] != operator.CodeAccountNotFound {
				t.Errorf("%s of an unknown email = %d %v, want 422 account_not_found", path, code, body)
			}
			code, body = r.operate(t, path, map[string]string{})
			if code != http.StatusBadRequest || body["error"] != operator.CodeInvalidRequest {
				t.Errorf("%s without an email = %d %v, want 400 invalid_request", path, code, body)
			}
		}
	})
}

// TestOperator_RequestsMetric — row 183. Each request moves exactly its own
// pair of skein_operator_requests_total: a success and a refusal of each
// operation, and a request without the key (operation none).
func TestOperator_RequestsMetric(t *testing.T) {
	r := newOpRig(t)
	pair := func(op, result string) float64 {
		return testutil.ToFloat64(operator.OperatorRequestsTotal.WithLabelValues(op, result))
	}
	unknown := map[string]string{"email": "nobody-" + uuid.NewString()[:8] + "@nowhere.test"}
	a := r.seedAccount(t)
	owner := r.seedAccount(t)
	other, _ := testsupport.SeedUser(t, r.pool, "user")
	testsupport.AddSpaceMember(t, r.pool, testsupport.SeedSpace(t, r.pool, owner.id), other)

	cases := []struct {
		name, op, result, path string
		body                   any
	}{
		{"block succeeds", "block", "success", operator.PathBlock, map[string]string{"email": a.email}},
		{"unblock succeeds", "unblock", "success", operator.PathUnblock, map[string]string{"email": a.email}},
		{"block refused", "block", "account_not_found", operator.PathBlock, unknown},
		{"unblock refused", "unblock", "account_not_found", operator.PathUnblock, unknown},
		{"delete refused", "delete", "owns_spaces_with_members", operator.PathDelete, map[string]string{"email": owner.email}},
		{"delete without an email", "delete", "invalid_request", operator.PathDelete, map[string]string{}},
		{"delete succeeds", "delete", "success", operator.PathDelete, map[string]string{"email": a.email}},
	}
	for _, c := range cases {
		before := pair(c.op, c.result)
		r.operate(t, c.path, c.body)
		if got := pair(c.op, c.result) - before; got != 1 {
			t.Errorf("%s: {%s,%s} moved by %v, want 1", c.name, c.op, c.result, got)
		}
	}

	before := pair("none", "unauthorized")
	req := httptest.NewRequest(http.MethodPost, operator.PathBlock, bytes.NewReader([]byte(`{"email":"x@y.z"}`)))
	r.router.ServeHTTP(httptest.NewRecorder(), req)
	if got := pair("none", "unauthorized") - before; got != 1 {
		t.Errorf("a request without the key moved {none,unauthorized} by %v, want 1", got)
	}
}

// TestOperator_ConcurrentDeleteOfOneAccount — found in review. Two
// deletions of one email both find the account, then meet on its row (held
// by the test until both wait on it): one deletes (204), the other finds
// it deleted and answers 422 account_not_found — never a 500.
func TestOperator_ConcurrentDeleteOfOneAccount(t *testing.T) {
	r := newOpRig(t)
	ctx := context.Background()
	a := r.seedAccount(t)
	holder, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	var pid int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, a.id); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code int
		body map[string]any
	}
	answers := make(chan answer, 2)
	for range 2 {
		go func() {
			code, body := r.operate(t, operator.PathDelete, map[string]string{"email": a.email})
			answers <- answer{code, body}
		}()
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := r.pool.QueryRow(ctx, `
WITH RECURSIVE waiters(pid) AS (
    SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
    UNION
    SELECT s.pid FROM pg_stat_activity s JOIN waiters w ON w.pid = ANY(pg_blocking_pids(s.pid)))
SELECT count(*) FROM waiters`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of the two deletions wait on the account's row", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for range 2 {
		x := <-answers
		code, _ := x.body["error"].(string)
		got[strconv.Itoa(x.code)+" "+code]++
	}
	if got["204 "] != 1 || got["422 "+operator.CodeAccountNotFound] != 1 {
		t.Errorf("two concurrent deletions answered %v, want one 204 and one 422 account_not_found", got)
	}
}
