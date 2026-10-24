package handler

// invitations_token_test.go — sk-674.18 test matrix rows 172, 173, 174, 174a
// (T040; US-5, US-8, E-5, E-7, K-11).
//
// References, written before the code:
//   - 172 (spec US-5 "Order of link states", screens 2.5; E-5): the
//     logged-in view of /invitations/{token} answers, in this order, space
//     deleted → already a member ("earlier \"invalid\"") → invalid
//     (expired, revoked, used) → another address → the accept screen; under
//     another account nothing is accepted and the invited address is not in
//     the answer.
//   - 173 (US-5): opening the link (GET) as the invited user confirms his
//     email and accepts nothing; only POST …/accept accepts.
//   - 174 (US-8, E-7, accounts/contract.go#InvitationSignupRequest): signup
//     by invitation creates in one transaction the account with its email
//     confirmed, its "Personal", the membership of the invitation's space, and
//     answers with sessionEpoch; a link that expired while the form was
//     filled in is invitation_invalid and creates no account; the email of a
//     deleted account is email_taken.
//   - 174a (K-11): an acceptance or a signup refused with a business refusal
//     does not spend the link; a successful one does.
//
// Driven through the routes as cmd/api mounts them: the logged-in routes
// behind ParseUserHeaders + RequireUser, the anonymous ones behind the
// service secret.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
	"skein-api/internal/invitations"
	"skein-api/internal/pwned"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
	"skein-api/internal/users"
)

const invTokenSecret = "invitation-token-test-secret"

type invTokenEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router chi.Router
	svc    *invitations.Service
	space  uuid.UUID
	short  string
	owner  spUser
}

// newInvTokenEnv mounts the link routes over a fresh space of a fresh owner;
// registration open, mail on.
func newInvTokenEnv(t *testing.T) *invTokenEnv {
	t.Helper()
	return newInvTokenEnvWith(t, nil)
}

// newInvTokenEnvWith is newInvTokenEnv whose users store checks new
// passwords against the breach corpus through pwnedChecker (nil: off).
func newInvTokenEnvWith(t *testing.T, pwnedChecker users.PwnedChecker) *invTokenEnv {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	sp := spaces.NewService(pool, spaces.Options{})
	turnstile := accounts.NewTurnstile("", accounts.TurnstileVerifyURL)
	svc := invitations.NewService(pool, sp, turnstile, invitations.Options{
		SignupEnabled: true, EmailEnabled: true, InviteTTL: 24 * time.Hour, DeliveryTTL: time.Hour})
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", pwnedChecker)
	acc := accounts.NewService(accounts.ServiceConfig{SignupEnabled: true, EmailEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local"},
		pool, store, accounts.NewDirectory(pool), turnstile, accounts.NewRateLimiter())
	acc.UseInvitations(svc)
	ah, ih := NewAccountsHandler(acc), NewInvitationsHandler(svc)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.With(RequireInternalSecret(invTokenSecret, "")).Get("/api/v1/auth/invitations/{token}", ah.InvitationPreview)
	r.With(RequireInternalSecret(invTokenSecret, "")).Post("/api/v1/auth/invitations/{token}/signup", ah.InvitationSignup)
	r.Route("/api/v1/invitations/{token}", func(r chi.Router) {
		r.Use(ParseUserHeaders, RequireUser)
		r.Get("/", ih.View)
		r.Post("/accept", ih.AcceptByToken)
		r.Post("/decline", ih.DeclineByToken)
	})

	e := &invTokenEnv{t: t, pool: pool, router: r, svc: svc, owner: seedSpUser(t, pool, "user")}
	e.space = testsupport.SeedSpace(t, pool, e.owner.id)
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, e.space).Scan(&e.short); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM outgoing_emails WHERE invitation_id IN (SELECT id FROM space_invitations WHERE space_id = $1 OR invited_by = $2)`,
			`DELETE FROM auth_tokens WHERE invitation_id IN (SELECT id FROM space_invitations WHERE space_id = $1 OR invited_by = $2)`,
			`DELETE FROM space_invitations WHERE space_id = $1 OR invited_by = $2`,
			`DELETE FROM events WHERE (entity_type = 'space' AND entity_id = $1::text) OR actor_user_id = $2`,
		} {
			_, _ = pool.Exec(ctx, q, e.space, e.owner.id)
		}
	})
	return e
}

// invite sends an invitation to email by the owner and returns its live link.
func (e *invTokenEnv) invite(email string) (uuid.UUID, string) {
	e.t.Helper()
	caller := spaces.Caller{UserID: e.owner.id, Username: e.owner.name}
	if _, err := e.svc.Create(context.Background(), caller, e.short,
		spaces.CreateInvitationRequest{Email: &email, Language: "en"}, ""); err != nil {
		e.t.Fatalf("inviting %s: %v", email, err)
	}
	var inv uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM space_invitations WHERE space_id = $1 AND email = $2 AND status = 'pending'`, e.space, email).Scan(&inv); err != nil {
		e.t.Fatal(err)
	}
	return inv, invitationLink(e.t, e.pool, inv)
}

// invitationLink plays the mail worker: it leases the newest unsent email of
// invitation inv and issues its token (tokens.IssueForEmail).
func invitationLink(t *testing.T, pool *pgxpool.Pool, inv uuid.UUID) string {
	t.Helper()
	var emailID uuid.UUID
	var gen int
	if err := pool.QueryRow(context.Background(), `
UPDATE outgoing_emails
   SET state = 'leased', attempts = attempts + 1, leased_until = now() + interval '2 minutes'
 WHERE id = (SELECT id FROM outgoing_emails WHERE invitation_id = $1 AND state = 'pending'
             ORDER BY created_at DESC LIMIT 1)
RETURNING id, attempts`, inv).Scan(&emailID, &gen); err != nil {
		t.Fatalf("leasing the invitation email: %v", err)
	}
	raw, err := tokens.IssueForEmail(context.Background(), pool, emailID, gen)
	if err != nil {
		t.Fatalf("IssueForEmail: %v", err)
	}
	return raw
}

// as calls a logged-in route as u.
func (e *invTokenEnv) as(u spUser, method, path string) httpAnswer {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(auth.HeaderUserID, u.id.String())
	req.Header.Set(auth.HeaderUsername, u.name)
	req.Header.Set(auth.HeaderUserRole, u.role)
	if u.external {
		req.Header.Set(auth.HeaderUserIsExternal, "true")
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	a := httpAnswer{status: rec.Code, header: rec.Header(), raw: rec.Body.Bytes()}
	_ = json.Unmarshal(a.raw, &a.body)
	return a
}

// anon calls an anonymous route with the service secret.
func (e *invTokenEnv) anon(method, path string, body any) httpAnswer {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set(auth.HeaderInternalSecret, invTokenSecret)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	a := httpAnswer{status: rec.Code, header: rec.Header(), raw: rec.Body.Bytes()}
	_ = json.Unmarshal(a.raw, &a.body)
	return a
}

// addressee is an account whose email the invitation names; its email is
// unconfirmed.
func (e *invTokenEnv) addressee() (spUser, string) {
	e.t.Helper()
	u := seedSpUser(e.t, e.pool, "user")
	e.t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, u.id)
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM space_members WHERE user_id = $1`, u.id)
	})
	return u, u.name + "@test.local"
}

func (e *invTokenEnv) member(u uuid.UUID) bool {
	e.t.Helper()
	var ok bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`, e.space, u).Scan(&ok); err != nil {
		e.t.Fatal(err)
	}
	return ok
}

func (e *invTokenEnv) invStatus(inv uuid.UUID) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM space_invitations WHERE id = $1`, inv).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func statusOf(a httpAnswer) string { s, _ := a.body["status"].(string); return s }

// Row 172 — the states of the link in the order of screens 2.5, each case
// built so that the state checked earlier wins over a later one that also
// holds.
func TestInvitationLink_StatesInOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("space deleted, before everything", func(t *testing.T) {
		e := newInvTokenEnv(t)
		u, email := e.addressee()
		_, link := e.invite(email)
		if err := spaces.NewStore(e.pool).Delete(ctx, e.space, spaces.Actor{UserID: e.owner.id, Username: e.owner.name, Type: "human"}); err != nil {
			t.Fatal(err)
		}
		if a := e.as(u, http.MethodGet, "/api/v1/invitations/"+link); a.status != 200 || statusOf(a) != "space_deleted" {
			t.Errorf("deleted space: %d %s, want space_deleted", a.status, a.raw)
		}
	})

	t.Run("already a member, before invalid", func(t *testing.T) {
		e := newInvTokenEnv(t)
		u, email := e.addressee()
		_, link := e.invite(email)
		if _, err := e.pool.Exec(ctx, `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, u.id); err != nil {
			t.Fatal(err)
		}
		if a := e.as(u, http.MethodPost, "/api/v1/invitations/"+link+"/accept"); a.status != 200 {
			t.Fatalf("accepting: %d %s", a.status, a.raw)
		}
		// The link is used now — invalid — and the caller is a member.
		a := e.as(u, http.MethodGet, "/api/v1/invitations/"+link)
		space, _ := a.body["space"].(map[string]any)
		if a.status != 200 || statusOf(a) != "already_member" || space["id"] != e.short {
			t.Errorf("a member opening a used link: %d %s, want already_member naming the space", a.status, a.raw)
		}
	})

	t.Run("invalid, before another address", func(t *testing.T) {
		e := newInvTokenEnv(t)
		_, email := e.addressee()
		stranger := seedSpUser(t, e.pool, "user")
		inv, link := e.invite(email)
		if err := e.svc.Revoke(ctx, spaces.Caller{UserID: e.owner.id, Username: e.owner.name}, e.short, inv); err != nil {
			t.Fatal(err)
		}
		if a := e.as(stranger, http.MethodGet, "/api/v1/invitations/"+link); a.status != 200 || statusOf(a) != "invalid" {
			t.Errorf("a revoked link under another account: %d %s, want invalid", a.status, a.raw)
		}
	})

	t.Run("another address: no acceptance, no address", func(t *testing.T) {
		e := newInvTokenEnv(t)
		_, email := e.addressee()
		stranger := seedSpUser(t, e.pool, "user")
		inv, link := e.invite(email)
		a := e.as(stranger, http.MethodGet, "/api/v1/invitations/"+link)
		if a.status != 200 || statusOf(a) != "other_email" {
			t.Errorf("another account: %d %s, want other_email", a.status, a.raw)
		}
		if bytes.Contains(a.raw, []byte(email)) {
			t.Errorf("the answer to another account names the invited address: %s", a.raw)
		}
		if p := e.as(stranger, http.MethodPost, "/api/v1/invitations/"+link+"/accept"); p.code() != "invitation_other_email" {
			t.Errorf("another account accepting: %d %s, want invitation_other_email", p.status, p.raw)
		}
		if e.member(stranger.id) || e.invStatus(inv) != "pending" {
			t.Error("another account's acceptance changed something")
		}
	})

	t.Run("valid: the accept screen", func(t *testing.T) {
		e := newInvTokenEnv(t)
		u, email := e.addressee()
		_, link := e.invite(email)
		a := e.as(u, http.MethodGet, "/api/v1/invitations/"+link)
		space, _ := a.body["space"].(map[string]any)
		if a.status != 200 || statusOf(a) != "valid" || space["id"] != e.short ||
			a.body["inviterUsername"] != e.owner.name || a.body["expiresAt"] == nil {
			t.Errorf("the addressee: %d %s, want valid with the space, the inviter and the expiry", a.status, a.raw)
		}
	})
}

// Row 173 — GET confirms, only POST accepts.
func TestInvitationLink_GetConfirmsPostAccepts(t *testing.T) {
	e := newInvTokenEnv(t)
	u, email := e.addressee()
	inv, link := e.invite(email)
	confirmed := func() bool {
		var ok bool
		if err := e.pool.QueryRow(context.Background(), `SELECT email_confirmed_at IS NOT NULL FROM users WHERE id = $1`, u.id).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if confirmed() {
		t.Fatal("the fixture's email is confirmed already")
	}
	if a := e.as(u, http.MethodGet, "/api/v1/invitations/"+link); statusOf(a) != "valid" {
		t.Fatalf("opening the link: %d %s", a.status, a.raw)
	}
	if !confirmed() {
		t.Error("opening the link as the addressee did not confirm the email")
	}
	if e.member(u.id) || e.invStatus(inv) != "pending" {
		t.Error("opening the link accepted the invitation")
	}
	if a := e.as(u, http.MethodPost, "/api/v1/invitations/"+link+"/accept"); a.status != 200 {
		t.Fatalf("POST accept: %d %s", a.status, a.raw)
	}
	if !e.member(u.id) || e.invStatus(inv) != "accepted" {
		t.Error("POST accept did not accept")
	}
}

// Row 174 — signup by invitation.
func TestInvitationSignup_OneTransaction(t *testing.T) {
	ctx := context.Background()
	signup := func(e *invTokenEnv, link, username string) httpAnswer {
		return e.anon(http.MethodPost, "/api/v1/auth/invitations/"+link+"/signup", map[string]any{
			"username": username, "password": "correct horse battery 174", "termsAccepted": true})
	}
	username := func() string { return "iv" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") }

	t.Run("success", func(t *testing.T) {
		e := newInvTokenEnv(t)
		email := "new-" + uuid.NewString()[:8] + "@invite.test"
		inv, link := e.invite(email)
		name := username()
		a := signup(e, link, name)
		if a.status != 200 {
			t.Fatalf("signup: %d %s", a.status, a.raw)
		}
		id, err := uuid.Parse(a.body["id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { removeAccount(t, e.pool, id) })
		if _, ok := a.body["sessionEpoch"]; !ok {
			t.Error("the answer has no sessionEpoch")
		}
		space, _ := a.body["space"].(map[string]any)
		if space["id"] != e.short {
			t.Errorf("the answer names space %v, want %s", space["id"], e.short)
		}
		var confirmed, personal, mailed bool
		if err := e.pool.QueryRow(ctx, `
SELECT u.email_confirmed_at IS NOT NULL AND u.terms_accepted_at IS NOT NULL AND lower(u.email) = $2,
       EXISTS (SELECT 1 FROM spaces s WHERE s.owner_id = u.id AND s.kind = 'personal'),
       EXISTS (SELECT 1 FROM outgoing_emails o WHERE o.user_id = u.id)
  FROM users u WHERE u.id = $1`, id, email).Scan(&confirmed, &personal, &mailed); err != nil {
			t.Fatal(err)
		}
		if !confirmed || !personal || mailed || !e.member(id) || e.invStatus(inv) != "accepted" {
			t.Errorf("after signup: email confirmed with consent %v, Personal %v, confirmation email %v (want none), member %v, invitation %s",
				confirmed, personal, mailed, e.member(id), e.invStatus(inv))
		}
	})

	t.Run("expired while the form was filled in", func(t *testing.T) {
		e := newInvTokenEnv(t)
		email := "late-" + uuid.NewString()[:8] + "@invite.test"
		inv, link := e.invite(email)
		if _, err := e.pool.Exec(ctx, `UPDATE space_invitations SET expires_at = now() - interval '1 second' WHERE id = $1`, inv); err != nil {
			t.Fatal(err)
		}
		if a := signup(e, link, username()); a.status != 422 || a.code() != "invitation_invalid" {
			t.Errorf("signup on an expired link: %d %s, want 422 invitation_invalid", a.status, a.raw)
		}
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email) = $1`, email).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Error("an account was created on an expired link")
		}
	})

	t.Run("the email of a deleted account", func(t *testing.T) {
		e := newInvTokenEnv(t)
		gone, email := e.addressee()
		_, link := e.invite(email)
		if _, err := e.pool.Exec(ctx, `UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(random()::text), 1, 8) WHERE id = $1`, gone.id); err != nil {
			t.Fatal(err)
		}
		if a := signup(e, link, username()); a.status != 422 || a.code() != "email_taken" {
			t.Errorf("signup on a deleted account's email: %d %s, want 422 email_taken", a.status, a.raw)
		}
	})
}

// Row 174a — a refused answer does not spend the link, a successful one does.
func TestInvitationLink_RefusalKeepsTheToken(t *testing.T) {
	ctx := context.Background()

	t.Run("acceptance refused after the token was used in its transaction", func(t *testing.T) {
		e := newInvTokenEnv(t)
		u, email := e.addressee()
		_, link := e.invite(email)
		// An external account with the invited email: tokens.Use accepts the
		// link, the joining refuses — the whole answer rolls back.
		if _, err := e.pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, u.id); err != nil {
			t.Fatal(err)
		}
		ext := u
		ext.external = true
		if a := e.as(ext, http.MethodPost, "/api/v1/invitations/"+link+"/accept"); a.code() != "external_user" {
			t.Fatalf("an external addressee accepting: %d %s, want external_user", a.status, a.raw)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE users SET is_external = false WHERE id = $1`, u.id); err != nil {
			t.Fatal(err)
		}
		if a := e.as(u, http.MethodPost, "/api/v1/invitations/"+link+"/accept"); a.status != 200 {
			t.Fatalf("the same link after the refusal: %d %s, want accepted — the refusal must not spend it", a.status, a.raw)
		}
		if a := e.as(u, http.MethodPost, "/api/v1/invitations/"+link+"/decline"); a.code() != "invitation_invalid" {
			t.Errorf("the link after a successful acceptance: %d %s, want invitation_invalid", a.status, a.raw)
		}
	})

	t.Run("signup refused with email_taken", func(t *testing.T) {
		e := newInvTokenEnv(t)
		blocker, email := e.addressee()
		_, link := e.invite(email)
		if _, err := e.pool.Exec(ctx, `UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(random()::text), 1, 8) WHERE id = $1`, blocker.id); err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"username": "iv" + strings.ReplaceAll(uuid.NewString()[:12], "-", ""),
			"password": "correct horse battery 174a", "termsAccepted": true}
		if a := e.anon(http.MethodPost, "/api/v1/auth/invitations/"+link+"/signup", body); a.code() != "email_taken" {
			t.Fatalf("signup on a taken email: %d %s, want email_taken", a.status, a.raw)
		}
		if p := e.anon(http.MethodGet, "/api/v1/auth/invitations/"+link, nil); statusOf(p) != "valid" {
			t.Errorf("the link after the refused signup: %d %s, want valid", p.status, p.raw)
		}
		// The address is freed: the same link now registers, and is spent.
		if _, err := e.pool.Exec(ctx, `UPDATE users SET email = 'freed-' || id || '@gone.test' WHERE id = $1`, blocker.id); err != nil {
			t.Fatal(err)
		}
		a := e.anon(http.MethodPost, "/api/v1/auth/invitations/"+link+"/signup", body)
		if a.status != 200 {
			t.Fatalf("signup on the same link: %d %s", a.status, a.raw)
		}
		id, _ := uuid.Parse(a.body["id"].(string))
		t.Cleanup(func() { removeAccount(t, e.pool, id) })
		if p := e.anon(http.MethodGet, "/api/v1/auth/invitations/"+link, nil); statusOf(p) != "invalid" {
			t.Errorf("the link after a successful signup: %d %s, want invalid", p.status, p.raw)
		}
	})
}

// Signup by invitation is one transaction even when it
// fails after the account was written: the link passes the view, the
// signup goes on to the breach-corpus check (an external dependency, here a
// stub that holds the request), the invitation is revoked meanwhile, and
// tokens.Use in the account's transaction refuses — invitation_invalid,
// and nothing of the account survives: no user, "Personal", membership,
// confirmation email or signal; the link's token is not spent.
func TestInvitationSignup_RollsBackAfterTheAccountIsWritten(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	corpus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(reached) })
		<-release
		w.WriteHeader(http.StatusOK) // no suffix: the password is clean
	}))
	t.Cleanup(corpus.Close)
	sink := startPrefsSigSink(t)
	e := newInvTokenEnvWith(t, pwned.NewClient(corpus.URL, 30*time.Second))
	ctx := context.Background()
	email := "late-" + uuid.NewString()[:8] + "@invite.test"
	inv, link := e.invite(email)
	username := "rb" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")

	answer := make(chan httpAnswer, 1)
	go func() {
		answer <- e.anon(http.MethodPost, "/api/v1/auth/invitations/"+link+"/signup", map[string]any{
			"username": username, "password": "correct horse battery rb", "termsAccepted": true})
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the signup never reached the breach-corpus check: it was refused before the account's transaction")
	}
	if err := e.svc.Revoke(ctx, spaces.Caller{UserID: e.owner.id, Username: e.owner.name}, e.short, inv); err != nil {
		close(release)
		t.Fatal(err)
	}
	from := sink.mark()
	close(release)
	a := <-answer
	if a.status != http.StatusUnprocessableEntity || a.code() != "invitation_invalid" {
		t.Fatalf("signup on a link revoked during it: %d %s, want 422 invitation_invalid", a.status, a.raw)
	}

	var users, personal, mails, used int
	if err := e.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM users WHERE lower(email) = $1 OR username = $2),
       (SELECT count(*) FROM spaces s JOIN users u ON u.id = s.owner_id WHERE u.username = $2),
       (SELECT count(*) FROM outgoing_emails WHERE kind = 'email_confirm' AND recipient = $1),
       (SELECT count(*) FROM auth_tokens WHERE invitation_id = $3 AND used_at IS NOT NULL)`,
		email, username, inv).Scan(&users, &personal, &mails, &used); err != nil {
		t.Fatal(err)
	}
	if users != 0 || personal != 0 || mails != 0 {
		t.Errorf("after the rolled-back signup: %d accounts, %d personal spaces, %d confirmation emails; want none", users, personal, mails)
	}
	if used != 0 {
		t.Errorf("the link's token was spent by a rolled-back signup")
	}
	if got := sink.settle(t, e.pool, from); len(got) != 0 {
		t.Errorf("signals of a rolled-back signup: %v", got)
	}
}

// A successful signup by invitation with the common space
// on joins exactly the invitation's space beside its "Personal" (INV-22: a
// self-registered account does not join the common space).
func TestInvitationSignup_CommonSpaceOnJoinsOnlyTheInvitation(t *testing.T) {
	e := newInvTokenEnv(t)
	ctx := context.Background()
	var was bool
	if err := e.pool.QueryRow(ctx, `SELECT common_space_enabled FROM instance_settings WHERE id = 1`).Scan(&was); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE instance_settings SET common_space_enabled = true WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `UPDATE instance_settings SET common_space_enabled = $1 WHERE id = 1`, was)
	})
	sw := &spWorld{t: t, pool: e.pool}
	sw.asCommon(seedSpUser(t, e.pool, "user"))
	email := "common-" + uuid.NewString()[:8] + "@invite.test"
	_, link := e.invite(email)
	a := e.anon(http.MethodPost, "/api/v1/auth/invitations/"+link+"/signup", map[string]any{
		"username": "cm" + strings.ReplaceAll(uuid.NewString()[:12], "-", ""), "password": "correct horse battery cm", "termsAccepted": true})
	if a.status != http.StatusOK {
		t.Fatalf("signup: %d %s", a.status, a.raw)
	}
	id, _ := uuid.Parse(a.body["id"].(string))
	t.Cleanup(func() { removeAccount(t, e.pool, id) })
	rows, err := e.pool.Query(ctx, `SELECT s.kind, s.id = $2 FROM space_members m JOIN spaces s ON s.id = m.space_id WHERE m.user_id = $1`, id, e.space)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		var invited bool
		if err := rows.Scan(&kind, &invited); err != nil {
			t.Fatal(err)
		}
		if invited {
			kind = "invited"
		}
		kinds = append(kinds, kind)
	}
	if len(kinds) != 2 || !slices.Contains(kinds, "personal") || !slices.Contains(kinds, "invited") {
		t.Errorf("the new account's spaces: %v, want its personal space and the invitation's alone", kinds)
	}
}
