package handler

// accounts_reset_test.go — sk-674.18, test matrix rows 156, 156a, 157,
// 157a, 158, 159 (US-9, SC-009, INV-26, INV-25, K-11).
//
// Reference: the contract accounts/contract.go#PasswordResetRequest /
// #PasswordResetCompleteRequest and the confirm-email route (T012, written
// before T039), and the spec US-9: the reset request answers the same for
// an existing and a missing email (status, body, headers), and queues the
// email only for an account that logs in with a password (hybrid: an
// external one or the break-glass admin); the rate-limit refusal is the
// same for both too; a completed reset ends the other sessions, answers
// with the new epoch and leaves the agent keys working; a token is used up
// only by an action that succeeds; with mail off the routes answer
// email_disabled; the email is in the language of the request.
//
// The routes run through the real handler on the real stores; the raw
// token is issued the way the mail worker gets it (lease the queued email,
// tokens.IssueForEmail).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
	"skein-api/internal/users"
)

const resetSecret = "reset-test-internal-secret"

type resetOpts struct {
	mode       string // "" = local
	mailOff    bool
	breakGlass string
}

type resetEnv struct {
	pool   *pgxpool.Pool
	router http.Handler
	h      *AccountsHandler
}

func newResetEnv(t *testing.T, o resetOpts) *resetEnv {
	t.Helper()
	if o.mode == "" {
		o.mode = "local"
	}
	pool := testsupport.NewTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), o.mode, o.breakGlass, nil)
	svc := accounts.NewService(accounts.ServiceConfig{
		EmailEnabled: !o.mailOff, EmailDeliveryTTL: time.Hour, AuthMode: o.mode, BreakGlassAdmin: o.breakGlass,
	}, pool, store, accounts.NewDirectory(pool), accounts.NewTurnstile("", accounts.TurnstileVerifyURL), accounts.NewRateLimiter())
	h := NewAccountsHandler(svc)
	r := chi.NewRouter()
	r.With(RequireInternalSecret(resetSecret, "")).Post("/api/v1/auth/password-reset", h.PasswordReset)
	r.With(RequireInternalSecret(resetSecret, "")).Post("/api/v1/auth/password-reset/{token}", h.PasswordResetComplete)
	r.With(RequireInternalSecret(resetSecret, "")).Post("/api/v1/auth/confirm-email/{token}", h.ConfirmEmail)
	return &resetEnv{pool: pool, router: r, h: h}
}

type httpAnswer struct {
	status int
	header http.Header
	raw    []byte
	body   map[string]any
}

func (a httpAnswer) code() string {
	s, _ := a.body["error"].(string)
	return s
}

func (e *resetEnv) post(t *testing.T, path string, body any, ip string) httpAnswer {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set(auth.HeaderInternalSecret, resetSecret)
	if ip != "" {
		req.Header.Set(auth.HeaderClientIP, ip)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	a := httpAnswer{status: rec.Code, header: rec.Header(), raw: rec.Body.Bytes()}
	_ = json.Unmarshal(a.raw, &a.body)
	return a
}

// meConfirmation calls POST /me/email/confirmation as user.
func (e *resetEnv) meConfirmation(t *testing.T, user uuid.UUID, lang, ip string) httpAnswer {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"language": lang})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/email/confirmation", bytes.NewReader(raw))
	if ip != "" {
		req.Header.Set(auth.HeaderClientIP, ip)
	}
	req = req.WithContext(auth.WithUserID(req.Context(), user))
	rec := httptest.NewRecorder()
	e.h.RequestConfirmationEmail(rec, req)
	a := httpAnswer{status: rec.Code, header: rec.Header(), raw: rec.Body.Bytes()}
	_ = json.Unmarshal(a.raw, &a.body)
	return a
}

// seedResetAccount creates an account with a real password hash.
func seedResetAccount(t *testing.T, pool *pgxpool.Pool, role string, external bool, password string) (uuid.UUID, string) {
	t.Helper()
	hash, err := auth.Hash(password, auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	u := "rs" + uniq()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $3, $4, $5, $6, now(), '{}'::jsonb)`, id, u+"@reset.test", u, role, hash, external); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM auth_tokens WHERE user_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM outgoing_emails WHERE user_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE user_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	})
	return id, u + "@reset.test"
}

// queued counts the user's unfinished emails of kind and returns the
// language of the newest.
func queued(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, kind string) (int, string) {
	t.Helper()
	var (
		n    int
		lang *string
	)
	if err := pool.QueryRow(context.Background(), `
SELECT count(*),
       (SELECT lang FROM outgoing_emails WHERE user_id = $1 AND kind = $2 AND state <> 'final'
         ORDER BY created_at DESC, id DESC LIMIT 1)
  FROM outgoing_emails WHERE user_id = $1 AND kind = $2 AND state <> 'final'`, user, kind).Scan(&n, &lang); err != nil {
		t.Fatalf("read queued emails: %v", err)
	}
	if lang == nil {
		return n, ""
	}
	return n, *lang
}

// issueLink plays the mail worker: leases the user's pending email of kind
// and issues its link token.
func issueLink(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, kind string) string {
	t.Helper()
	ctx := context.Background()
	var (
		emailID uuid.UUID
		gen     int
	)
	if err := pool.QueryRow(ctx, `
UPDATE outgoing_emails
   SET state = 'leased', attempts = attempts + 1, leased_until = now() + interval '2 minutes'
 WHERE id = (SELECT id FROM outgoing_emails WHERE user_id = $1 AND kind = $2 AND state = 'pending'
             ORDER BY created_at DESC LIMIT 1)
RETURNING id, attempts`, user, kind).Scan(&emailID, &gen); err != nil {
		t.Fatalf("lease the %s email: %v", kind, err)
	}
	raw, err := tokens.IssueForEmail(ctx, pool, emailID, gen)
	if err != nil {
		t.Fatalf("IssueForEmail: %v", err)
	}
	return raw
}

// Row 156 — SC-009: the reset request answers identically (status, body,
// headers) for an existing and a missing email; the email is queued only
// for an account that logs in with a password.
func TestPasswordReset_NeutralAnswer(t *testing.T) {
	type subject struct {
		name      string
		email     string
		user      uuid.UUID
		wantEmail bool
	}
	run := func(t *testing.T, env *resetEnv, subjects []subject) {
		var first *httpAnswer
		for i, s := range subjects {
			a := env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": s.email, "language": "en"}, "10.156.0."+strconv.Itoa(i+1))
			if a.status != http.StatusNoContent {
				t.Fatalf("%s: got %d, want 204; body=%s", s.name, a.status, a.raw)
			}
			if first == nil {
				first = &a
			} else if a.status != first.status || !bytes.Equal(a.raw, first.raw) || !reflect.DeepEqual(a.header, first.header) {
				t.Errorf("%s: answer %d %v %q differs from %s's %d %v %q", s.name, a.status, a.header, a.raw,
					subjects[0].name, first.status, first.header, first.raw)
			}
			if s.user != uuid.Nil {
				n, _ := queued(t, env.pool, s.user, "password_reset")
				if (n == 1) != s.wantEmail || n > 1 {
					t.Errorf("%s: %d reset emails queued, want email=%v", s.name, n, s.wantEmail)
				}
			}
		}
	}

	t.Run("local", func(t *testing.T) {
		env := newResetEnv(t, resetOpts{})
		u, email := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
		run(t, env, []subject{
			{"existing account", email, u, true},
			{"missing email", "nobody" + uniq() + "@reset.test", uuid.Nil, false},
		})
	})

	t.Run("hybrid", func(t *testing.T) {
		admin, adminEmail := seedResetAccount(t, testsupport.NewTestPool(t), "admin", false, signupGoodPassword)
		env := newResetEnv(t, resetOpts{mode: "hybrid", breakGlass: adminEmail})
		internal, internalEmail := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
		external, externalEmail := seedResetAccount(t, env.pool, "user", true, signupGoodPassword)
		run(t, env, []subject{
			{"internal account (logs in at the SSO provider)", internalEmail, internal, false},
			{"external account", externalEmail, external, true},
			{"break-glass admin", adminEmail, admin, true},
			{"missing email", "nobody" + uniq() + "@reset.test", uuid.Nil, false},
		})
	})
}

// Row 156a — SC-009: the rate-limit refusal of the reset request is the
// same for an existing and a missing email.
func TestPasswordReset_RateLimitRefusalIsNeutral(t *testing.T) {
	env := newResetEnv(t, resetOpts{})
	_, existing := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
	missing := "nobody" + uniq() + "@reset.test"
	var answers []httpAnswer
	for i, email := range []string{existing, missing} {
		ip := "10.157.0." + strconv.Itoa(i+1)
		for k := 0; k < accounts.ActionPasswordReset.PerEmail; k++ {
			if a := env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": email, "language": "en"}, ip); a.status != http.StatusNoContent {
				t.Fatalf("attempt %d for %s: %d, want 204", k+1, email, a.status)
			}
		}
		a := env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": email, "language": "en"}, ip)
		if a.status != http.StatusTooManyRequests || a.code() != "rate_limited" {
			t.Fatalf("over the limit for %s: %d %q, want 429 rate_limited", email, a.status, a.code())
		}
		answers = append(answers, a)
	}
	if !bytes.Equal(answers[0].raw, answers[1].raw) || !reflect.DeepEqual(answers[0].header, answers[1].header) {
		t.Errorf("refusals differ: existing %v %q, missing %v %q", answers[0].header, answers[0].raw, answers[1].header, answers[1].raw)
	}
}

// Row 157 — INV-26: a completed reset ends the other sessions (401
// session_revoked on the previous epoch), answers with the new epoch, and
// the agent keys keep working.
func TestPasswordReset_EndsSessionsKeepsKeys(t *testing.T) {
	ctx := context.Background()
	env := newResetEnv(t, resetOpts{})
	u, email := seedResetAccount(t, env.pool, "user", false, "old password 123")
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := apikeys.New(env.pool).Create(ctx, u, "reset-test", prefix, hash); err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyChain := auth.RequireAPIKey(env.pool, auth.MinBcryptCost, "")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	keyRequest := func() int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(auth.HeaderAPIKey, plain)
		req.Header.Set(auth.HeaderSurface, "internal")
		rec := httptest.NewRecorder()
		keyChain.ServeHTTP(rec, req)
		return rec.Code
	}
	sessions := sessionRouter(t)
	if code, _ := sessionRequest(t, sessions, u, 0); code != http.StatusOK {
		t.Fatalf("session before the reset = %d, want 200", code)
	}
	if code := keyRequest(); code != http.StatusOK {
		t.Fatalf("key before the reset = %d, want 200", code)
	}

	if a := env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": email, "language": "en"}, ""); a.status != http.StatusNoContent {
		t.Fatalf("reset request: %d", a.status)
	}
	raw := issueLink(t, env.pool, u, "password_reset")
	a := env.post(t, "/api/v1/auth/password-reset/"+raw, map[string]string{"password": signupGoodPassword}, "")
	if a.status != http.StatusOK {
		t.Fatalf("reset: %d %q; body=%s", a.status, a.code(), a.raw)
	}
	var dbEpoch int64
	if err := env.pool.QueryRow(ctx, `SELECT session_epoch FROM users WHERE id = $1`, u).Scan(&dbEpoch); err != nil {
		t.Fatal(err)
	}
	got, ok := a.body["sessionEpoch"].(float64)
	if !ok || int64(got) != dbEpoch || dbEpoch == 0 {
		t.Fatalf("sessionEpoch = %v, want the raised epoch %d", a.body["sessionEpoch"], dbEpoch)
	}
	if code, errCode := sessionRequest(t, sessions, u, 0); code != http.StatusUnauthorized || errCode != auth.CodeSessionRevoked {
		t.Errorf("session with the previous epoch = %d %q, want 401 session_revoked", code, errCode)
	}
	if code, _ := sessionRequest(t, sessions, u, dbEpoch); code != http.StatusOK {
		t.Errorf("session re-issued with the returned epoch = %d, want 200", code)
	}
	if code := keyRequest(); code != http.StatusOK {
		t.Errorf("agent key after the reset = %d, want 200 — a reset revokes no key", code)
	}
}

// Row 157a — K-11, INV-25: a reset refused — before the token is used (a
// weak password) or after it, inside the action's transaction (the account
// no longer eligible) — does not spend it, and a refusal after Use rolls the
// password and the epoch back too; a successful reset spends the link; a
// successful confirmation spends its link too (that route has no refusal
// after Use).
func TestTokens_UsedOnlyBySuccess(t *testing.T) {
	env := newResetEnv(t, resetOpts{})
	u, email := seedResetAccount(t, env.pool, "user", false, "old password 123")

	if a := env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": email, "language": "en"}, ""); a.status != http.StatusNoContent {
		t.Fatalf("reset request: %d", a.status)
	}
	raw := issueLink(t, env.pool, u, "password_reset")
	if a := env.post(t, "/api/v1/auth/password-reset/"+raw, map[string]string{"password": "short"}, ""); a.code() != "password_weak" {
		t.Fatalf("weak password: %d %q, want password_weak", a.status, a.code())
	}
	if a := env.post(t, "/api/v1/auth/password-reset/"+raw, map[string]string{"password": signupGoodPassword}, ""); a.status != http.StatusOK {
		t.Fatalf("the same link after a refusal: %d %q, want 200 — a refusal must not spend the token", a.status, a.code())
	}
	if a := env.post(t, "/api/v1/auth/password-reset/"+raw, map[string]string{"password": signupGoodPassword + "!"}, ""); a.code() != "token_invalid" {
		t.Errorf("the link after a successful reset: %d %q, want token_invalid", a.status, a.code())
	}

	// A refusal AFTER tokens.Use: a link sent while the installation was
	// local, presented after it became hybrid, where this internal account
	// no longer logs in with a password. The refusal comes inside the
	// transaction that used the token — the token, the password and the
	// epoch must all roll back; the same link then works where the account
	// is eligible.
	w, wEmail := seedResetAccount(t, env.pool, "user", false, "old password 123")
	if a := env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": wEmail, "language": "en"}, ""); a.status != http.StatusNoContent {
		t.Fatalf("reset request (local): %d", a.status)
	}
	wLink := issueLink(t, env.pool, w, "password_reset")
	var hashBefore string
	var epochBefore int64
	if err := env.pool.QueryRow(context.Background(), `SELECT password_hash, session_epoch FROM users WHERE id = $1`, w).Scan(&hashBefore, &epochBefore); err != nil {
		t.Fatal(err)
	}
	hybrid := newResetEnv(t, resetOpts{mode: "hybrid"})
	if a := hybrid.post(t, "/api/v1/auth/password-reset/"+wLink, map[string]string{"password": signupGoodPassword}, ""); a.code() != "token_invalid" {
		t.Fatalf("reset of an account no longer password-eligible: %d %q, want token_invalid", a.status, a.code())
	}
	var hashAfter string
	var epochAfter int64
	if err := env.pool.QueryRow(context.Background(), `SELECT password_hash, session_epoch FROM users WHERE id = $1`, w).Scan(&hashAfter, &epochAfter); err != nil {
		t.Fatal(err)
	}
	if hashAfter != hashBefore || epochAfter != epochBefore {
		t.Errorf("a reset refused after Use changed the account: hash changed %v, epoch %d → %d", hashAfter != hashBefore, epochBefore, epochAfter)
	}
	if a := env.post(t, "/api/v1/auth/password-reset/"+wLink, map[string]string{"password": signupGoodPassword}, ""); a.status != http.StatusOK {
		t.Errorf("the same link where the account is eligible: %d %q, want 200 — a refusal after Use must not spend the token", a.status, a.code())
	}

	if a := env.meConfirmation(t, u, "en", ""); a.status != http.StatusNoContent {
		t.Fatalf("confirmation request: %d %q", a.status, a.code())
	}
	link := issueLink(t, env.pool, u, "email_confirm")
	if a := env.post(t, "/api/v1/auth/confirm-email/"+link, nil, ""); a.status != http.StatusNoContent {
		t.Fatalf("confirm: %d %q, want 204", a.status, a.code())
	}
	if a := env.post(t, "/api/v1/auth/confirm-email/"+link, nil, ""); a.code() != "token_invalid" {
		t.Errorf("the confirmation link used twice: %d %q, want token_invalid", a.status, a.code())
	}
}

// Row 158 — with mail off, the reset and confirmation routes answer
// email_disabled.
func TestAccountMailRoutes_MailOff(t *testing.T) {
	env := newResetEnv(t, resetOpts{mailOff: true})
	u, email := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
	cases := []struct {
		name string
		do   func() httpAnswer
	}{
		{"reset request", func() httpAnswer {
			return env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": email, "language": "en"}, "")
		}},
		{"reset by link", func() httpAnswer {
			return env.post(t, "/api/v1/auth/password-reset/some-token", map[string]string{"password": signupGoodPassword}, "")
		}},
		{"confirm by link", func() httpAnswer { return env.post(t, "/api/v1/auth/confirm-email/some-token", nil, "") }},
		{"confirmation request", func() httpAnswer { return env.meConfirmation(t, u, "en", "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if a := tc.do(); a.status != http.StatusUnprocessableEntity || a.code() != "email_disabled" {
				t.Errorf("got %d %q, want 422 email_disabled", a.status, a.code())
			}
		})
	}
	if n, _ := queued(t, env.pool, u, "password_reset"); n != 0 {
		t.Errorf("%d reset emails queued with mail off, want 0", n)
	}
}

// Row 159 — the email is in the language of the request.
func TestAccountMail_LanguageOfTheRequest(t *testing.T) {
	env := newResetEnv(t, resetOpts{})
	for _, lang := range []string{"ru", "en"} {
		u, email := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
		if a := env.post(t, "/api/v1/auth/password-reset", map[string]string{"email": email, "language": lang}, ""); a.status != http.StatusNoContent {
			t.Fatalf("reset request: %d", a.status)
		}
		if _, got := queued(t, env.pool, u, "password_reset"); got != lang {
			t.Errorf("reset email language = %q, want %q", got, lang)
		}
		if a := env.meConfirmation(t, u, lang, ""); a.status != http.StatusNoContent {
			t.Fatalf("confirmation request: %d %q", a.status, a.code())
		}
		if _, got := queued(t, env.pool, u, "email_confirm"); got != lang {
			t.Errorf("confirmation email language = %q, want %q", got, lang)
		}
	}
}
