package handler

// accounts_confirm_test.go — sk-674.18, test matrix rows 160, 161, 161a
// (E-26, US-5).
//
// Reference: the spec — E-26 "the rate is limited; the button is inactive for a minute;
// the latest token works"; US-5 "after the email is confirmed the open
// tab gets a personal inbox signal"; the contracts of
// POST /api/v1/me/email/confirmation (spaces/contract.go) and
// POST /api/v1/auth/confirm-email/{token} (accounts/contract.go).
//
// The confirmation request is a logged-in route keyed on the session's user
// alone: the client address is outside the key. The signal is observed on
// skein.user.*.signal through an in-process NATS server (prefs_signal_test.go
// has the sink). The invitation-accept right that a confirmed email opens
// (row 161a, T040) is observed through POST /me/invitations/{id}/accept with
// mail on.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// newConfirmEnv is the reset environment with the limiter on a clock the
// test drives.
func newConfirmEnv(t *testing.T, now func() time.Time) *resetEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	svc := accounts.NewService(accounts.ServiceConfig{
		EmailEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local",
	}, pool, store, accounts.NewDirectory(pool), accounts.NewTurnstile("", accounts.TurnstileVerifyURL),
		accounts.NewRateLimiterWithClock(now))
	h := NewAccountsHandler(svc)
	r := chi.NewRouter()
	r.With(RequireInternalSecret(resetSecret, "")).Post("/api/v1/auth/confirm-email/{token}", h.ConfirmEmail)
	return &resetEnv{pool: pool, router: r, h: h}
}

func emailConfirmedAt(t *testing.T, env *resetEnv, user uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := env.pool.QueryRow(context.Background(), `SELECT email_confirmed_at FROM users WHERE id = $1`, user).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// Row 160 — E-26: one confirmation request per user per window; the IP is
// outside the key, the user is the key; the latest link works, the previous
// one does not.
func TestConfirmationRequest_PerUserWindowLatestLink(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	env := newConfirmEnv(t, func() time.Time { return now })
	a, _ := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
	b, _ := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
	const ip1, ip2 = "10.160.0.1", "10.160.0.2"

	if r := env.meConfirmation(t, a, "en", ip1); r.status != http.StatusNoContent {
		t.Fatalf("first request: %d %q, want 204", r.status, r.code())
	}
	first := issueLink(t, env.pool, a, "email_confirm")

	if r := env.meConfirmation(t, a, "en", ip1); r.status != http.StatusTooManyRequests || r.code() != "rate_limited" {
		t.Errorf("repeat in the window: %d %q, want 429 rate_limited", r.status, r.code())
	}
	if r := env.meConfirmation(t, a, "en", ip2); r.status != http.StatusTooManyRequests || r.code() != "rate_limited" {
		t.Errorf("same user from another IP in the window: %d %q, want 429 — the IP is not the key", r.status, r.code())
	}
	if r := env.meConfirmation(t, b, "en", ip1); r.status != http.StatusNoContent {
		t.Errorf("another user from the same IP in the window: %d %q, want 204 — the user is the key", r.status, r.code())
	}

	now = now.Add(accounts.ActionConfirmationEmail.Window + time.Second)
	if r := env.meConfirmation(t, a, "en", ip1); r.status != http.StatusNoContent {
		t.Fatalf("after the window: %d %q, want 204", r.status, r.code())
	}
	latest := issueLink(t, env.pool, a, "email_confirm")

	if r := env.post(t, "/api/v1/auth/confirm-email/"+first, nil, ""); r.code() != "token_invalid" {
		t.Errorf("the earlier link: %d %q, want token_invalid", r.status, r.code())
	}
	if emailConfirmedAt(t, env, a) != nil {
		t.Fatal("the earlier link confirmed the email")
	}
	if r := env.post(t, "/api/v1/auth/confirm-email/"+latest, nil, ""); r.status != http.StatusNoContent {
		t.Errorf("the latest link: %d %q, want 204", r.status, r.code())
	}
}

// Row 161 — US-5: confirming the email publishes the personal signal
// `inbox` to the user.
func TestConfirmEmail_SignalsInbox(t *testing.T) {
	sink := startPrefsSigSink(t)
	env := newConfirmEnv(t, time.Now)
	u, _ := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
	if r := env.meConfirmation(t, u, "en", ""); r.status != http.StatusNoContent {
		t.Fatalf("confirmation request: %d %q", r.status, r.code())
	}
	link := issueLink(t, env.pool, u, "email_confirm")

	from := sink.mark()
	if r := env.post(t, "/api/v1/auth/confirm-email/"+link, nil, ""); r.status != http.StatusNoContent {
		t.Fatalf("confirm: %d %q, want 204", r.status, r.code())
	}
	got := sink.settle(t, env.pool, from)
	hasInbox := false
	for _, a := range got[u] {
		if a == "inbox" {
			hasInbox = true
		}
	}
	if !hasInbox {
		t.Errorf("signals after confirmation = %v, want inbox for the user", got)
	}
	for other := range got {
		if other != u {
			t.Errorf("signal to %s, want only the confirming user", other)
		}
	}
}

// inboxInvitation seeds a pending invitation of user's email to a fresh
// regular space (T040) and returns the call accepting it from the user's
// Inbox with mail on (POST /me/invitations/{id}/accept). Whatever the
// acceptance writes is removed after the test.
func inboxInvitation(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, email string) func() httpAnswer {
	t.Helper()
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, owner)
	var inv uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, lower($2), $3, 'en', now() + interval '1 day') RETURNING id`, space, email, owner).Scan(&inv); err != nil {
		t.Fatalf("seeding the invitation: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, inv)
		_, _ = pool.Exec(c, `DELETE FROM events WHERE actor_user_id = ANY($1)`, []uuid.UUID{user, owner})
	})
	h := NewInvitationsHandler(invitations.NewService(pool, spaces.NewService(pool, spaces.Options{}),
		accounts.NewTurnstile("", accounts.TurnstileVerifyURL),
		invitations.Options{EmailEnabled: true, InviteTTL: time.Hour, DeliveryTTL: time.Hour}))
	r := chi.NewRouter()
	r.Post("/api/v1/me/invitations/{invId}/accept", h.AcceptMine)
	return func() httpAnswer {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/me/invitations/"+inv.String()+"/accept", nil)
		req = req.WithContext(auth.WithUserID(req.Context(), user))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		a := httpAnswer{status: rec.Code, header: rec.Header(), raw: rec.Body.Bytes()}
		_ = json.Unmarshal(a.raw, &a.body)
		return a
	}
}

// Row 161a — US-5: a successful confirmation stamps email_confirmed_at and
// opens accepting an invitation from the Inbox (refused email_unconfirmed
// before it, with mail on); a repeated link and an unknown token change
// nothing.
func TestConfirmEmail_StampsOnceInvalidChangesNothing(t *testing.T) {
	env := newConfirmEnv(t, time.Now)
	u, uEmail := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
	other, _ := seedResetAccount(t, env.pool, "user", false, signupGoodPassword)
	accept := inboxInvitation(t, env.pool, u, uEmail)
	if r := accept(); r.status != http.StatusUnprocessableEntity || r.code() != "email_unconfirmed" {
		t.Errorf("accepting from the Inbox before the confirmation: %d %q, want 422 email_unconfirmed", r.status, r.code())
	}
	if r := env.meConfirmation(t, u, "en", ""); r.status != http.StatusNoContent {
		t.Fatalf("confirmation request: %d %q", r.status, r.code())
	}
	link := issueLink(t, env.pool, u, "email_confirm")

	if r := env.post(t, "/api/v1/auth/confirm-email/not-a-token", nil, ""); r.code() != "token_invalid" {
		t.Errorf("unknown token: %d %q, want token_invalid", r.status, r.code())
	}
	if emailConfirmedAt(t, env, u) != nil || emailConfirmedAt(t, env, other) != nil {
		t.Fatal("an unknown token confirmed an email")
	}

	if r := env.post(t, "/api/v1/auth/confirm-email/"+link, nil, ""); r.status != http.StatusNoContent {
		t.Fatalf("confirm: %d %q, want 204", r.status, r.code())
	}
	at := emailConfirmedAt(t, env, u)
	if at == nil {
		t.Fatal("email_confirmed_at is NULL after a successful confirmation")
	}
	if emailConfirmedAt(t, env, other) != nil {
		t.Error("another account's email became confirmed")
	}
	if r := accept(); r.status != http.StatusOK {
		t.Errorf("accepting from the Inbox after the confirmation: %d %q, want 200", r.status, r.code())
	}

	if r := env.post(t, "/api/v1/auth/confirm-email/"+link, nil, ""); r.code() != "token_invalid" {
		t.Errorf("the link again: %d %q, want token_invalid", r.status, r.code())
	}
	if again := emailConfirmedAt(t, env, u); again == nil || !again.Equal(*at) {
		t.Errorf("email_confirmed_at changed on a repeat: %v → %v", at, again)
	}
}
