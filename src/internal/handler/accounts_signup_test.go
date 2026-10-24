package handler

// accounts_signup_test.go — sk-674.18, test matrix rows 150, 151, 152, 155
// (US-8, INV-18, E-25).
//
// Reference: the contract accounts/contract.go#SignupRequest and
// #UsernameSuggestionsResponse (T012, written before T038) and the spec
// US-8 — every refusal code of screens 2.6 in its own case; the email and
// username of a deleted account answer as taken; a successful signup
// creates the user, "Personal", the consent stamped by the server, a queued
// confirmation email and an authentication response with sessionEpoch; of
// two concurrent signups of one email exactly one succeeds (E-25).
//
// The routes are driven through the real handler wrapped in
// RequireInternalSecret, on the real users store and database. Outside
// stubs: Turnstile siteverify (an HTTP server checking the secret) and the
// Pwned Passwords corpus (one known-breached password).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

const (
	signupSecret         = "signup-test-internal-secret"
	signupTurnstileKey   = "turnstile-test-secret"
	signupGoodCaptcha    = "captcha-ok"
	signupBreachedPasswd = "breached password 123"
	signupGoodPassword   = "correct horse battery staple"
)

// breachedOnly is a users.PwnedChecker that knows one breached password.
type breachedOnly struct{}

func (breachedOnly) Check(_ context.Context, p string) (bool, error) {
	return p == signupBreachedPasswd, nil
}

// turnstileStub is siteverify: success only for the configured secret and
// the good token.
func turnstileStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ok := r.PostForm.Get("secret") == signupTurnstileKey && r.PostForm.Get("response") == signupGoodCaptcha
		_ = json.NewEncoder(w).Encode(map[string]any{"success": ok})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type signupEnv struct {
	pool   *pgxpool.Pool
	router http.Handler
}

type signupOpts struct {
	disabled bool
	captcha  bool
}

func newSignupEnv(t *testing.T, o signupOpts) *signupEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", breachedOnly{})
	secret := ""
	if o.captcha {
		secret = signupTurnstileKey
	}
	env := &signupEnv{pool: pool}
	svc := accounts.NewService(accounts.ServiceConfig{
		SignupEnabled: !o.disabled, EmailEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local",
	}, pool, store, accounts.NewDirectory(pool), accounts.NewTurnstile(secret, turnstileStub(t)), accounts.NewRateLimiter())
	h := NewAccountsHandler(svc)
	r := chi.NewRouter()
	// As buildRouter: a panic is a 500, not a crashed process.
	r.Use(middleware.Recoverer)
	r.With(RequireInternalSecret(signupSecret, "")).Post("/api/v1/auth/signup", h.Signup)
	r.With(RequireInternalSecret(signupSecret, "")).Get("/api/v1/auth/username-suggestions", h.UsernameSuggestions)
	env.router = r
	return env
}

type signupAnswer struct {
	status int
	body   map[string]any
	raw    []byte
}

func (a signupAnswer) code() string {
	s, _ := a.body["error"].(string)
	return s
}

func (e *signupEnv) signup(t *testing.T, body map[string]any, ip string) signupAnswer {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/signup", bytes.NewReader(raw))
	req.Header.Set(auth.HeaderInternalSecret, signupSecret)
	if ip != "" {
		req.Header.Set(auth.HeaderClientIP, ip)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	ans := signupAnswer{status: rec.Code, raw: rec.Body.Bytes()}
	_ = json.Unmarshal(ans.raw, &ans.body)
	if ans.status == http.StatusOK {
		if id, err := uuid.Parse(fmt.Sprint(ans.body["id"])); err == nil {
			t.Cleanup(func() { removeAccount(t, e.pool, id) })
		}
	}
	return ans
}

// removeAccount deletes an account a test created through signup, with
// what the signup made for it (§4a: the test cleans up after itself): its
// emails and tokens, its events, its "Personal", its memberships.
func removeAccount(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM auth_tokens WHERE user_id = $1`,
		`DELETE FROM outgoing_emails WHERE user_id = $1`,
		`DELETE FROM events WHERE actor_user_id = $1`,
		`DELETE FROM space_members WHERE user_id = $1
		    OR space_id IN (SELECT id FROM spaces WHERE owner_id = $1 AND kind = 'personal')`,
		`DELETE FROM spaces WHERE owner_id = $1 AND kind = 'personal'`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Errorf("cleanup of account %s: %v", id, err)
			return
		}
	}
}

func (e *signupEnv) suggestions(t *testing.T, email string) signupAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/username-suggestions?email="+url.QueryEscape(email), nil)
	req.Header.Set(auth.HeaderInternalSecret, signupSecret)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	ans := signupAnswer{status: rec.Code, raw: rec.Body.Bytes()}
	_ = json.Unmarshal(ans.raw, &ans.body)
	return ans
}

// uniq is a fresh lower-case token for names of this test run.
func uniq() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

// form is a valid signup body for a fresh email and username.
func form() map[string]any {
	u := "su" + uniq()
	return map[string]any{
		"email": u + "@signup.test", "username": u, "password": signupGoodPassword,
		"termsAccepted": true, "language": "en",
	}
}

func with(base map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// seedAccount creates an ordinary account and returns its email and
// username.
func seedAccount(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	u := "ex" + uniq()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES (gen_random_uuid(), $1, $2, $2, 'user', 'x', now(), '{}'::jsonb)`, u+"@signup.test", u); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE username = $1`, u)
	})
	return u + "@signup.test", u
}

// reserve writes a deleted account's reserved identity (K-21).
func reserve(t *testing.T, pool *pgxpool.Pool, kind, value string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO reserved_identities (kind, value_hash) VALUES ($1, sha256(convert_to($2, 'UTF8')))`, kind, value); err != nil {
		t.Fatalf("reserve %s %s: %v", kind, value, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM reserved_identities WHERE kind = $1 AND value_hash = sha256(convert_to($2, 'UTF8'))`, kind, value)
	})
}

func accountCount(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE lower(email) = lower($1)`, email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertFreeUsernames checks that every suggestion obeys the username
// rule, is not of the pseudonym shape, and belongs to no account and to no
// reservation.
func assertFreeUsernames(t *testing.T, pool *pgxpool.Pool, raw any, notThese ...string) {
	t.Helper()
	list, ok := raw.([]any)
	if !ok || len(list) < 2 || len(list) > 3 {
		t.Fatalf("suggestions = %v, want 2–3 usernames", raw)
	}
	for _, v := range list {
		s, _ := v.(string)
		if !accounts.UsernameRegex.MatchString(s) || strings.HasPrefix(s, "deleted-") {
			t.Errorf("suggestion %q breaks the username rule", s)
		}
		for _, n := range notThese {
			if s == n {
				t.Errorf("suggestion %q is the taken or reserved name", s)
			}
		}
		var taken bool
		if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM users WHERE lower(username) = $1)
    OR EXISTS (SELECT 1 FROM reserved_identities WHERE kind = 'username' AND value_hash = sha256(convert_to($1, 'UTF8')))`, s).Scan(&taken); err != nil {
			t.Fatal(err)
		}
		if taken {
			t.Errorf("suggestion %q is taken", s)
		}
	}
}

// Row 150 — every refusal code of screens 2.6 in its own case, and only
// there: each case breaks exactly one rule of an otherwise valid form.
func TestSignup_EachRefusalInItsOwnCase(t *testing.T) {
	env := newSignupEnv(t, signupOpts{captcha: true})
	pool := env.pool
	takenEmail, takenUser := seedAccount(t, pool)
	deletedEmail := "gone" + uniq() + "@signup.test"
	reserve(t, pool, "email", deletedEmail)
	deletedUser := "gone" + uniq()
	reserve(t, pool, "username", deletedUser)

	valid := func() map[string]any { return with(form(), "turnstileToken", signupGoodCaptcha) }
	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"email of an account", with(valid(), "email", strings.ToUpper(takenEmail)), 422, "email_taken"},
		{"email of a deleted account", with(valid(), "email", deletedEmail), 422, "email_taken"},
		{"username of an account", with(valid(), "username", takenUser), 422, "username_taken"},
		{"username of a deleted account", with(valid(), "username", deletedUser), 422, "username_taken"},
		{"username breaking the rule", with(valid(), "username", "-x"), 422, "username_invalid"},
		{"username of the pseudonym shape", with(valid(), "username", "deleted-0a1b2c3d"), 422, "username_invalid"},
		{"short password", with(valid(), "password", "short"), 422, "password_weak"},
		{"breached password", with(valid(), "password", signupBreachedPasswd), 422, "password_pwned"},
		{"terms not accepted", with(valid(), "termsAccepted", false), 422, "terms_required"},
		{"captcha rejected", with(valid(), "turnstileToken", "captcha-bad"), 422, "captcha_failed"},
		{"captcha missing", with(valid(), "turnstileToken", ""), 422, "captcha_failed"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A client address per case keeps the IP bucket out of it.
			ans := env.signup(t, tc.body, "10.150.0."+strconv.Itoa(i+1))
			if ans.status != tc.status || ans.code() != tc.code {
				t.Fatalf("got %d %q, want %d %q; body=%s", ans.status, ans.code(), tc.status, tc.code, ans.raw)
			}
			if tc.code == "username_taken" {
				assertFreeUsernames(t, pool, ans.body["suggestions"], takenUser, deletedUser)
			}
			if email, _ := tc.body["email"].(string); tc.code != "email_taken" && accountCount(t, pool, email) != 0 {
				t.Errorf("a refused signup created an account for %s", email)
			}
		})
	}

	t.Run("rate limited", func(t *testing.T) {
		email := "rl" + uniq() + "@signup.test"
		// Attempts of one email count whatever their outcome.
		for i := 0; i < accounts.ActionSignup.PerEmail; i++ {
			ans := env.signup(t, with(valid(), "email", email, "termsAccepted", false), "10.150.1.1")
			if ans.code() != "terms_required" {
				t.Fatalf("attempt %d: got %d %q, want terms_required", i+1, ans.status, ans.code())
			}
		}
		ans := env.signup(t, with(valid(), "email", email), "10.150.1.1")
		if ans.status != http.StatusTooManyRequests || ans.code() != "rate_limited" {
			t.Fatalf("got %d %q, want 429 rate_limited; body=%s", ans.status, ans.code(), ans.raw)
		}
		if accountCount(t, pool, email) != 0 {
			t.Error("a rate-limited signup created an account")
		}
	})

	t.Run("a valid form is admitted", func(t *testing.T) {
		body := valid()
		ans := env.signup(t, body, "10.150.2.1")
		if ans.status != http.StatusOK {
			t.Fatalf("got %d %q, want 200; body=%s", ans.status, ans.code(), ans.raw)
		}
	})
}

// Row 151 — a successful signup creates the user with "Personal", the
// consent with the server's time and a queued confirmation email, and
// answers with sessionEpoch; with registration closed the route refuses.
func TestSignup_SuccessAndClosed(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		env := newSignupEnv(t, signupOpts{})
		pool := env.pool
		body := with(form(), "language", "ru")
		var before time.Time
		if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		ans := env.signup(t, body, "10.151.0.1")
		if ans.status != http.StatusOK {
			t.Fatalf("got %d, want 200; body=%s", ans.status, ans.raw)
		}
		epoch, ok := ans.body["sessionEpoch"]
		if !ok {
			t.Fatalf("response has no sessionEpoch: %s", ans.raw)
		}
		id := uuid.MustParse(ans.body["id"].(string))
		var (
			dbEpoch   int64
			termsAt   *time.Time
			confirmed bool
			after     time.Time
			personal  int
			emails    int
		)
		if err := pool.QueryRow(context.Background(), `
SELECT session_epoch, terms_accepted_at, email_confirmed_at IS NOT NULL, now(),
       (SELECT count(*) FROM spaces s JOIN space_members m ON m.space_id = s.id AND m.user_id = u.id AND m.role = 'owner'
         WHERE s.kind = 'personal' AND s.owner_id = u.id),
       (SELECT count(*) FROM outgoing_emails e
         WHERE e.user_id = u.id AND e.kind = 'email_confirm' AND e.state = 'pending' AND e.lang = 'ru'
           AND e.recipient = lower($2))
  FROM users u WHERE u.id = $1`, id, body["email"]).Scan(&dbEpoch, &termsAt, &confirmed, &after, &personal, &emails); err != nil {
			t.Fatalf("read the new account: %v", err)
		}
		if got, _ := epoch.(float64); int64(got) != dbEpoch {
			t.Errorf("sessionEpoch = %v, want the account's %d", epoch, dbEpoch)
		}
		if termsAt == nil || termsAt.Before(before) || termsAt.After(after) {
			t.Errorf("terms_accepted_at = %v, want the server's time within [%v, %v]", termsAt, before, after)
		}
		if confirmed {
			t.Error("the email of a signup is confirmed, want not")
		}
		if personal != 1 {
			t.Errorf("personal spaces = %d, want 1", personal)
		}
		if emails != 1 {
			t.Errorf("queued confirmation emails in ru = %d, want 1", emails)
		}
		if ans.body["username"] != body["username"] || ans.body["email"] != body["email"] {
			t.Errorf("response names %v / %v, want %v / %v", ans.body["username"], ans.body["email"], body["username"], body["email"])
		}
	})

	t.Run("registration closed", func(t *testing.T) {
		env := newSignupEnv(t, signupOpts{disabled: true})
		body := form()
		ans := env.signup(t, body, "10.151.1.1")
		if ans.status != http.StatusUnprocessableEntity || ans.code() != "signup_disabled" {
			t.Fatalf("got %d %q, want 422 signup_disabled", ans.status, ans.code())
		}
		if accountCount(t, env.pool, body["email"].(string)) != 0 {
			t.Error("a closed registration created an account")
		}
	})
}

// Row 152 — E-25: two concurrent signups of one email give one account;
// the other is email_taken, never a 500.
func TestSignup_ConcurrentSameEmail(t *testing.T) {
	env := newSignupEnv(t, signupOpts{})
	// Many rounds: the losing side of the race may exhaust its retries under
	// SERIALIZABLE (the T038 fix), which a few rounds rarely
	// reach.
	for round := 0; round < 40; round++ {
		email := "race" + uniq() + "@signup.test"
		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			answers [2]signupAnswer
		)
		for i := range answers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				answers[i] = env.signup(t, with(form(), "email", email), "10.152.0."+strconv.Itoa(i+1+2*round))
			}(i)
		}
		close(start)
		wg.Wait()
		ok, taken := 0, 0
		for _, a := range answers {
			switch {
			case a.status == http.StatusOK:
				ok++
			case a.status == http.StatusUnprocessableEntity && a.code() == "email_taken":
				taken++
			default:
				t.Errorf("round %d: answer %d %q, want 200 or 422 email_taken; body=%s", round, a.status, a.code(), a.raw)
			}
		}
		if ok != 1 || taken != 1 {
			t.Errorf("round %d: %d successes and %d email_taken, want 1 and 1", round, ok, taken)
		}
		if n := accountCount(t, env.pool, email); n != 1 {
			t.Errorf("round %d: %d accounts with the email, want 1", round, n)
		}
	}
}

// Row 155 — username suggestions are free usernames: none taken by an
// account, none reserved by a deleted one.
func TestSignup_UsernameSuggestionsAreFree(t *testing.T) {
	env := newSignupEnv(t, signupOpts{})
	pool := env.pool
	stem := "sg" + uniq()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES (gen_random_uuid(), $1, $2, $2, 'user', 'x', now(), '{}'::jsonb)`, stem+"@elsewhere.test", stem); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE username = $1`, stem) })
	reserve(t, pool, "username", stem+"2")

	ans := env.suggestions(t, stem+"@corp.test")
	if ans.status != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", ans.status, ans.raw)
	}
	assertFreeUsernames(t, pool, ans.body["suggestions"], stem, stem+"2")

	if bad := env.suggestions(t, "not-an-email"); bad.status != http.StatusBadRequest || bad.code() != "invalid_request" {
		t.Errorf("malformed email: got %d %q, want 400 invalid_request", bad.status, bad.code())
	}
	closed := newSignupEnv(t, signupOpts{disabled: true})
	if c := closed.suggestions(t, stem+"@corp.test"); c.status != http.StatusUnprocessableEntity || c.code() != "signup_disabled" {
		t.Errorf("registration closed: got %d %q, want 422 signup_disabled", c.status, c.code())
	}
}

// An email whose first domain label is
// longer than a whole username (a valid DNS label of up to 63 characters)
// still gets 2–3 valid free usernames: from the suggestions route, and in
// the username_taken refusal of a signup — never a 500.
func TestSignup_LongDomainLabelStillSuggests(t *testing.T) {
	env := newSignupEnv(t, signupOpts{})
	_, takenUser := seedAccount(t, env.pool)
	email := "dev" + uniq() + "@" + strings.Repeat("a", 40) + ".test"

	ans := env.suggestions(t, email)
	if ans.status != http.StatusOK {
		t.Fatalf("suggestions: got %d, want 200; body=%s", ans.status, ans.raw)
	}
	assertFreeUsernames(t, env.pool, ans.body["suggestions"])

	ans = env.signup(t, with(form(), "email", email, "username", takenUser), "10.150.9.1")
	if ans.status != http.StatusUnprocessableEntity || ans.code() != "username_taken" {
		t.Fatalf("signup with a taken username: got %d %q, want 422 username_taken; body=%s", ans.status, ans.code(), ans.raw)
	}
	assertFreeUsernames(t, env.pool, ans.body["suggestions"], takenUser)
}
