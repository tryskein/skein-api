package invitations_test

// matrix_test.go — sk-674.18 test matrix rows 162 and 163 (T040; US-5,
// SC-009, SC-021).
//
// Reference, written from the sources before the code: the table
// "What the inviter sees after sending" and the
// invitation scenarios, with the codes of spaces/contract.go
// (CreateInvitationRequest / CreateInvitationResponse):
//
//   - registration open (mail on — the only allowed pair, INV-21): always the
//     neutral «sent», account or not; an email already invited — the same
//     invitation, nothing new;
//   - registration closed, an existing user: «sent»; mail off — the card in
//     his Inbox and the push, no email;
//   - registration closed, no account: mail on → no_such_user, mail off →
//     email_disabled; nothing created;
//   - already a member: already_member, nothing created (not an error);
//   - already invited, registration closed: already_pending with that
//     invitation;
//   - external: external_user, nothing created;
//   - a deleted account's email: as no account ("sent" with
//     registration open — spec US-5, the accepted residual);
//   - a deactivated account: the invitation waits in his Inbox; no push (the
//     decision of 2026-10-14: a blocked person is no addressee of a push);
//   - the inviter: the owner and the instance admin alike; a member who does
//     not manage the space — 403 forbidden, nothing created.
//
// What is observed per cell: the answer (status, code or outcome), the
// invitation row, the email in the outgoing queue, the push row (notification
// of type space_invitation) and the Inbox card (GET /me/invitations of the
// addressee's account).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// flagsOf is SKEIN_SIGNUP_ENABLED × SKEIN_EMAIL_ENABLED.
type flagsOf struct{ signup, mail bool }

func (f flagsOf) String() string { return fmt.Sprintf("signup=%v,mail=%v", f.signup, f.mail) }

func newService(pool *pgxpool.Pool, f flagsOf) *invitations.Service {
	return invitations.NewService(pool, spaces.NewService(pool, spaces.Options{}),
		accounts.NewTurnstile("", accounts.TurnstileVerifyURL),
		invitations.Options{SignupEnabled: f.signup, EmailEnabled: f.mail, InviteTTL: 24 * time.Hour, DeliveryTTL: time.Hour})
}

// world is one space with its owner, a member who does not manage it, and
// the instance admin; everything an invitation writes is removed after the
// test.
type world struct {
	t      *testing.T
	pool   *pgxpool.Pool
	space  uuid.UUID
	short  string
	owner  spaces.Caller
	member spaces.Caller
	admin  spaces.Caller
}

func caller(t *testing.T, pool *pgxpool.Pool, role string) spaces.Caller {
	t.Helper()
	id, name := testsupport.SeedUser(t, pool, role)
	return spaces.Caller{UserID: id, Username: name}
}

func newWorld(t *testing.T, pool *pgxpool.Pool) *world {
	t.Helper()
	w := &world{t: t, pool: pool}
	w.owner = caller(t, pool, "user")
	w.member = caller(t, pool, "user")
	w.admin = caller(t, pool, "admin")
	w.space = testsupport.SeedSpace(t, pool, w.owner.UserID)
	testsupport.AddSpaceMember(t, pool, w.space, w.member.UserID)
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, w.space).Scan(&w.short); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanSpace(pool, w.space) })
	return w
}

// cleanSpace removes what the invitations of space wrote.
func cleanSpace(pool *pgxpool.Pool, space uuid.UUID) {
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM outgoing_emails WHERE invitation_id IN (SELECT id FROM space_invitations WHERE space_id = $1)`, space)
	_, _ = pool.Exec(ctx, `DELETE FROM auth_tokens WHERE invitation_id IN (SELECT id FROM space_invitations WHERE space_id = $1)`, space)
	_, _ = pool.Exec(ctx, `DELETE FROM events WHERE entity_type = 'space' AND entity_id = $1`, space.String())
	_, _ = pool.Exec(ctx, `DELETE FROM space_invitation_sends WHERE space_id = $1`, space)
	_, _ = pool.Exec(ctx, `DELETE FROM space_invitations WHERE space_id = $1`, space)
}

// addressee kinds of row 162.
const (
	adExists      = "existing user"
	adNone        = "no account"
	adExternal    = "external user"
	adMember      = "already a member"
	adPending     = "already invited"
	adDeleted     = "deleted account"
	adDeactivated = "deactivated account"
)

// seedAddressee makes the addressee of kind for the space and returns its
// email and account (uuid.Nil when there is none).
func (w *world) seedAddressee(kind string) (string, uuid.UUID) {
	t := w.t
	ctx := context.Background()
	if kind == adNone {
		return "nobody-" + uuid.NewString()[:8] + "@invite.test", uuid.Nil
	}
	id, name := testsupport.SeedUser(t, w.pool, "user")
	email := name + "@test.local"
	var err error
	switch kind {
	case adExternal:
		_, err = w.pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, id)
	case adMember:
		testsupport.AddSpaceMember(t, w.pool, w.space, id)
	case adPending:
		_, err = w.pool.Exec(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day')`, w.space, email, w.owner.UserID)
	case adDeleted:
		_, err = w.pool.Exec(ctx, `UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(random()::text), 1, 8) WHERE id = $1`, id)
	case adDeactivated:
		_, err = w.pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, id)
	}
	if err != nil {
		t.Fatalf("seeding %s: %v", kind, err)
	}
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = $1`, id) })
	return email, id
}

// effects is what one cell observes besides the answer.
type effects struct {
	invitations int  // invitation rows of the email in the space
	email       bool // an invitation email queued
	push        bool // a push row to the addressee's account
	card        bool // the invitation in the account's GET /me/invitations
}

func (w *world) effects(svc *invitations.Service, email string, account uuid.UUID) effects {
	t := w.t
	ctx := context.Background()
	var e effects
	if err := w.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM space_invitations WHERE space_id = $1 AND email = $2),
       EXISTS (SELECT 1 FROM outgoing_emails o JOIN space_invitations i ON i.id = o.invitation_id
                WHERE i.space_id = $1 AND i.email = $2),
       EXISTS (SELECT 1 FROM notifications WHERE user_id = $3 AND type = 'space_invitation')`,
		w.space, email, account).Scan(&e.invitations, &e.email, &e.push); err != nil {
		t.Fatalf("reading the effects: %v", err)
	}
	if account != uuid.Nil {
		mine, err := svc.Mine(ctx, spaces.Caller{UserID: account})
		if err != nil {
			t.Fatalf("GET /me/invitations of the addressee: %v", err)
		}
		for _, m := range mine.Items {
			e.card = e.card || m.SpaceID == w.short
		}
	}
	return e
}

// answer is the observable answer of a create.
type answer struct {
	status  int
	code    string // refusal code, or the outcome of a 200
	pending bool   // the 200 carries the pending invitation
}

func create(t *testing.T, svc *invitations.Service, c spaces.Caller, short, email string) answer {
	t.Helper()
	resp, err := svc.Create(context.Background(), c, short, spaces.CreateInvitationRequest{Email: &email, Language: "en"}, "")
	var ref *spaces.Refusal
	switch {
	case err == nil:
		return answer{status: http.StatusOK, code: resp.Outcome, pending: resp.Invitation != nil}
	case errors.As(err, &ref):
		return answer{status: ref.Status, code: ref.Code}
	}
	t.Fatalf("create: %v", err)
	return answer{}
}

// TestInvitationMatrix is row 162: (flags) × (addressee) × (inviter).
func TestInvitationMatrix(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	open, closedMail, closedNoMail := flagsOf{true, true}, flagsOf{false, true}, flagsOf{false, false}
	sent := answer{http.StatusOK, "sent", false}
	cells := []struct {
		flags flagsOf
		kind  string
		want  answer
		eff   effects // for the owner and the admin
	}{
		{open, adExists, sent, effects{1, true, true, true}},
		{open, adNone, sent, effects{1, true, false, false}},
		{open, adExternal, answer{422, "external_user", false}, effects{0, false, false, false}},
		{open, adMember, answer{200, "already_member", false}, effects{0, false, false, false}},
		{open, adPending, sent, effects{1, false, false, true}},
		{open, adDeleted, sent, effects{1, true, false, false}},
		{open, adDeactivated, sent, effects{1, true, false, true}},

		{closedMail, adExists, sent, effects{1, true, true, true}},
		{closedMail, adNone, answer{422, "no_such_user", false}, effects{0, false, false, false}},
		{closedMail, adExternal, answer{422, "external_user", false}, effects{0, false, false, false}},
		{closedMail, adMember, answer{200, "already_member", false}, effects{0, false, false, false}},
		{closedMail, adPending, answer{200, "already_pending", true}, effects{1, false, false, true}},
		{closedMail, adDeleted, answer{422, "no_such_user", false}, effects{0, false, false, false}},
		{closedMail, adDeactivated, sent, effects{1, true, false, true}},

		{closedNoMail, adExists, sent, effects{1, false, true, true}},
		{closedNoMail, adNone, answer{422, "email_disabled", false}, effects{0, false, false, false}},
		{closedNoMail, adExternal, answer{422, "external_user", false}, effects{0, false, false, false}},
		{closedNoMail, adMember, answer{200, "already_member", false}, effects{0, false, false, false}},
		{closedNoMail, adPending, answer{200, "already_pending", true}, effects{1, false, false, true}},
		{closedNoMail, adDeleted, answer{422, "email_disabled", false}, effects{0, false, false, false}},
		{closedNoMail, adDeactivated, sent, effects{1, false, false, true}},
	}
	for _, c := range cells {
		for _, role := range []string{"owner", "admin", "member"} {
			t.Run(c.flags.String()+"/"+c.kind+"/"+role, func(t *testing.T) {
				w := newWorld(t, pool)
				svc := newService(pool, c.flags)
				email, account := w.seedAddressee(c.kind)
				before := w.effects(svc, email, account)
				inviter := map[string]spaces.Caller{"owner": w.owner, "admin": w.admin, "member": w.member}[role]
				got := create(t, svc, inviter, w.short, email)
				want, eff := c.want, c.eff
				if role == "member" {
					// A member who does not manage the space: refused, nothing changes.
					want, eff = answer{http.StatusForbidden, "forbidden", false}, before
				}
				if got != want {
					t.Errorf("answer %+v, want %+v", got, want)
				}
				if after := w.effects(svc, email, account); after != eff {
					t.Errorf("effects %+v, want %+v (before: %+v)", after, eff, before)
				}
			})
		}
	}
}

// TestInvitationAnswerNeutral is row 163 (SC-021, SC-009): with registration
// open the answer «sent» for an email with an account and for one without
// is the same in status, body and headers; the invitation row and the email
// in the queue exist in both cases.
func TestInvitationAnswerNeutral(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	w := newWorld(t, pool)
	svc := newService(pool, flagsOf{signup: true, mail: true})
	h := handler.NewInvitationsHandler(svc)
	r := chi.NewRouter()
	r.With(handler.ParseUserHeaders, handler.RequireUser).Post("/api/v1/spaces/{id}/invitations", h.Create)

	post := func(email string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"email": email, "language": "ru"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/spaces/"+w.short+"/invitations", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.HeaderUserID, w.owner.UserID.String())
		req.Header.Set(auth.HeaderUsername, w.owner.Username)
		req.Header.Set(auth.HeaderUserRole, "user")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	existing, account := w.seedAddressee(adExists)
	missing, _ := w.seedAddressee(adNone)
	a, b := post(existing), post(missing)
	if a.Code != http.StatusOK {
		t.Fatalf("existing addressee: %d %s", a.Code, a.Body)
	}
	if a.Code != b.Code || a.Body.String() != b.Body.String() || !reflect.DeepEqual(a.Header(), b.Header()) {
		t.Errorf("answers differ:\n existing %d %v %q\n missing  %d %v %q", a.Code, a.Header(), a.Body, b.Code, b.Header(), b.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(a.Body.Bytes(), &body); err != nil || body["outcome"] != "sent" || body["invitation"] != nil {
		t.Errorf("body %s, want {\"outcome\":\"sent\",\"invitation\":null}", a.Body)
	}
	for _, c := range []struct {
		email   string
		account uuid.UUID
	}{{existing, account}, {missing, uuid.Nil}} {
		e := w.effects(svc, c.email, c.account)
		if e.invitations != 1 || !e.email {
			t.Errorf("%s: invitation rows %d, email queued %v; want 1 and true", c.email, e.invitations, e.email)
		}
	}
}

// INV-17 through the routes: a caller authenticated by an
// API key — an agent — is refused invitation_agent_forbidden on every route
// a person must use: sending, resending, accepting and declining from the
// Inbox and by the link; nothing changes and the link stays live. Reading
// his invitations stays open to him.
func TestInvitationAgentForbiddenThroughRoutes(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	w := newWorld(t, pool)
	svc := newService(pool, flagsOf{signup: true, mail: true})
	email, addressee := w.account(true)
	inv := w.invite(svc, w.owner, email)
	link := issueLink(t, pool, inv)
	sendsBefore := recentSends(t, pool, w.owner.UserID)

	routes := invitationRoutes(svc)
	agent := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		routes.ServeHTTP(rw, r.WithContext(auth.WithAPIKeyID(r.Context(), uuid.New())))
	})
	for _, c := range []struct {
		name   string
		as     spaces.Caller
		method string
		path   string
		body   any
	}{
		{"create", w.owner, http.MethodPost, "/api/v1/spaces/" + w.short + "/invitations", inviteBody(newEmail(), nil)},
		{"resend", w.owner, http.MethodPost, "/api/v1/spaces/" + w.short + "/invitations/" + inv.String() + "/resend", nil},
		{"accept from the Inbox", addressee, http.MethodPost, "/api/v1/me/invitations/" + inv.String() + "/accept", nil},
		{"decline from the Inbox", addressee, http.MethodPost, "/api/v1/me/invitations/" + inv.String() + "/decline", nil},
		{"accept by the link", addressee, http.MethodPost, "/api/v1/invitations/" + link + "/accept", nil},
		{"decline by the link", addressee, http.MethodPost, "/api/v1/invitations/" + link + "/decline", nil},
	} {
		if a := call(agent, c.as, "user", c.method, c.path, c.body); a.status != http.StatusUnprocessableEntity || a.code != "invitation_agent_forbidden" {
			t.Errorf("an agent, %s: %d %q, want 422 invitation_agent_forbidden", c.name, a.status, a.code)
		}
	}
	if a := call(agent, addressee, "user", http.MethodGet, "/api/v1/me/invitations", nil); a.status != http.StatusOK {
		t.Errorf("an agent reading his invitations: %d %q, want 200", a.status, a.code)
	}
	if w.status(inv) != "pending" || w.joined(addressee.UserID) || recentSends(t, pool, w.owner.UserID) != sendsBefore {
		t.Error("an agent's refused request changed the invitation, the space or the journal")
	}
	if p, err := svc.Preview(context.Background(), link); err != nil || p.Status != "valid" {
		t.Errorf("the link after the agent's refusals: %+v %v, want valid", p, err)
	}
}
