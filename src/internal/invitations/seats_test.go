package invitations_test

// seats_test.go — sk-674.18 test matrix rows 164, 165, 165a (T040; E-1,
// INV-13, K14, US-14, "Restrictions").
//
// References, written before the code:
//   - INV-13 / E-1: participants plus pending invitations ≤
//     SKEIN_SPACE_MEMBER_LIMIT at the moment an invitation is created; the
//     check is atomic with the creation under the space's lock — of two
//     invitations for the last seat one is refused no_seats (with
//     memberCount, pendingInvitations, memberLimit); a pending invitation
//     holds a seat; an invitation to another space does not wait; accepting
//     a pending invitation does not check the limit again (K14).
//   - US-14: "day" of SKEIN_SPACE_INVITES_PER_DAY are the sliding 24 hours
//     per sender, one counter across every space: sends older than 24 hours
//     do not count; another sender is not affected; the same sender in
//     another space is refused too.
//   - "Restrictions" and the decision of 2026-10-14: with Turnstile
//     configured, a sender who has sent more than ten invitations in the
//     last 10 minutes passes the captcha — no token: captcha_required, a
//     verified one: created, an unverified one: captcha_failed; the key is
//     the sender (another space does not lift it, another sender does not
//     get it); without the Turnstile key there is no requirement.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

func intp(n int) *int { return &n }

// serviceWith is the service with a seat limit, a daily limit and a captcha.
func serviceWith(pool *pgxpool.Pool, memberLimit, perDay *int, captcha accounts.Captcha) *invitations.Service {
	return invitations.NewService(pool, spaces.NewService(pool, spaces.Options{MemberLimit: memberLimit}), captcha,
		invitations.Options{SignupEnabled: true, EmailEnabled: true, InviteTTL: 24 * time.Hour,
			InvitesPerDay: perDay, DeliveryTTL: time.Hour})
}

func noCaptcha() accounts.Captcha { return accounts.NewTurnstile("", accounts.TurnstileVerifyURL) }

func newEmail() string { return "seat-" + uuid.NewString()[:8] + "@invite.test" }

// createWith is create with a Turnstile token; the refusal as a *Refusal.
func createWith(svc *invitations.Service, c spaces.Caller, short, email string, token *string) (spaces.CreateInvitationResponse, *spaces.Refusal, error) {
	resp, err := svc.Create(context.Background(), c, short, spaces.CreateInvitationRequest{Email: &email, Language: "en", TurnstileToken: token}, "")
	var ref *spaces.Refusal
	if errors.As(err, &ref) {
		return resp, ref, nil
	}
	return resp, nil, err
}

// Row 164 — E-1, INV-13, K14.
func TestSeats_LastSeatUnderTheSpaceLock(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	w := newWorld(t, pool) // owner + member: 2 participants
	other := newWorld(t, pool)
	svc := serviceWith(pool, intp(3), nil, noCaptcha())

	// Another space does not wait for this one's lock. Checked apart from
	// the race below, under a holder that only takes the lock: a send
	// committing in another space while the SERIALIZABLE first invitation
	// below is open may abort that fixture transaction (SSI's predicate
	// locks are page-wide on a small table), which says nothing about the
	// product.
	func() {
		lockOnly, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lockOnly.Rollback(context.Background()) }()
		if _, err := lockOnly.Exec(ctx, `SELECT 1 FROM spaces WHERE id = $1 FOR UPDATE`, w.space); err != nil {
			t.Fatal(err)
		}
		otherDone := make(chan error, 1)
		go func() {
			_, ref, err := createWith(svc, other.owner, other.short, newEmail(), nil)
			if ref != nil {
				err = ref
			}
			otherDone <- err
		}()
		select {
		case err := <-otherDone:
			if err != nil {
				t.Errorf("an invitation to another space: %v, want created", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("an invitation to another space waited for this space's lock")
		}
	}()

	// The first invitation for the last seat, as a concurrent one holds it
	// (a SERIALIZABLE transaction, like every invitation): the space's lock
	// taken, the seats read, the pending invitation written, not committed.
	holder, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	var pid int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM spaces WHERE id = $1 FOR UPDATE`, w.space); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `
SELECT (SELECT count(*) FROM space_members WHERE space_id = $1),
       (SELECT count(*) FROM space_invitations WHERE space_id = $1 AND status = 'pending')`, w.space); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day')`, w.space, newEmail(), w.owner.UserID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		ref *spaces.Refusal
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, ref, err := createWith(svc, w.owner, w.short, newEmail(), nil)
		done <- result{ref, err}
	}()
	waiting := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE wait_event_type = 'Lock' AND $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			waiting = true
			break
		}
	}
	if !waiting {
		t.Fatal("the second invitation did not wait for the space's lock")
	}

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the second invitation: %v", r.err)
		}
		if r.ref == nil || r.ref.Code != "no_seats" {
			t.Fatalf("the second invitation for the last seat: %+v, want no_seats", r.ref)
		}
		if r.ref.Extra["memberCount"] != 2 || r.ref.Extra["pendingInvitations"] != 1 || r.ref.Extra["memberLimit"] != 3 {
			t.Errorf("no_seats fields %v, want memberCount 2, pendingInvitations 1, memberLimit 3", r.ref.Extra)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second invitation never finished after the first committed")
	}
	if n := countPending(t, pool, w.space); n != 1 {
		t.Errorf("%d pending invitations in the space, want the first one alone", n)
	}

	// K14: the limit lowered below the current count — the pending
	// invitation of an existing user is still accepted.
	email, account := w.seedAddressee(adExists)
	var inv uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day') RETURNING id`, w.space, email, w.owner.UserID).Scan(&inv); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, account) })
	lowered := serviceWith(pool, intp(1), nil, noCaptcha())
	if _, err := lowered.AcceptMine(ctx, spaces.Caller{UserID: account}, inv); err != nil {
		t.Errorf("accepting a pending invitation after the limit was lowered: %v, want joined (K14)", err)
	}
}

func countPending(t *testing.T, pool *pgxpool.Pool, space uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM space_invitations WHERE space_id = $1 AND status = 'pending'`, space).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedSends journals n sends of sender, each age old.
func seedSends(t *testing.T, pool *pgxpool.Pool, sender uuid.UUID, n int, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO space_invitation_sends (sender_id, sent_at)
SELECT $1, now() - $3::bigint * interval '1 second' FROM generate_series(1, $2)`,
		sender, n, int64(age/time.Second)); err != nil {
		t.Fatalf("seeding sends: %v", err)
	}
}

// Row 165 — US-14: the sliding 24 hours per sender, through the route;
// the window is the database's, so the earlier sends are
// journal rows dated in the past.
func TestSeats_DailyLimitSlidingPerSender(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	a, b := newWorld(t, pool), newWorld(t, pool)
	r := invitationRoutes(serviceWith(pool, nil, intp(2), noCaptcha()))
	post := func(c spaces.Caller, short string) httpAnswer {
		return call(r, c, "user", http.MethodPost, "/api/v1/spaces/"+short+"/invitations", inviteBody(newEmail(), nil))
	}

	// One send 25 hours ago (outside the window), one 23 hours ago (inside):
	// one of the two allowed is left.
	seedSends(t, pool, a.owner.UserID, 1, 25*time.Hour)
	seedSends(t, pool, a.owner.UserID, 1, 23*time.Hour)
	if ans := post(a.owner, a.short); ans.status != http.StatusOK || ans.code != "sent" {
		t.Fatalf("the second send of the day: %d %q, want 200 sent — a send older than 24 hours does not count", ans.status, ans.code)
	}
	if ans := post(a.owner, a.short); ans.status != http.StatusUnprocessableEntity || ans.code != "daily_limit" {
		t.Errorf("the third send in 24 hours: %d %q, want 422 daily_limit", ans.status, ans.code)
	}
	// Outside the key: the same sender in another space of his — still
	// refused.
	second := testsupport.SeedSpace(t, pool, a.owner.UserID)
	t.Cleanup(func() { cleanSpace(pool, second) })
	var secondShort string
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, second).Scan(&secondShort); err != nil {
		t.Fatal(err)
	}
	if ans := post(a.owner, secondShort); ans.status != http.StatusUnprocessableEntity || ans.code != "daily_limit" {
		t.Errorf("the same sender in another space: %d %q, want 422 daily_limit (one counter across spaces)", ans.status, ans.code)
	}
	// The key: another sender is not affected.
	if ans := post(b.owner, b.short); ans.status != http.StatusOK {
		t.Errorf("another sender: %d %q, want 200", ans.status, ans.code)
	}
	// A revoked invitation does not give its send back (000083: the journal
	// keeps the send when the invitation goes).
	var sentID uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM space_invitations WHERE space_id = $1`, a.space).Scan(&sentID); err != nil {
		t.Fatal(err)
	}
	if ans := call(r, a.owner, "user", http.MethodDelete, "/api/v1/spaces/"+a.short+"/invitations/"+sentID.String(), nil); ans.status != http.StatusNoContent {
		t.Fatalf("revoking: %d %q", ans.status, ans.code)
	}
	if ans := post(a.owner, a.short); ans.status != http.StatusUnprocessableEntity || ans.code != "daily_limit" {
		t.Errorf("after revoking the sender's invitation: %d %q, want 422 daily_limit", ans.status, ans.code)
	}
}

// turnstileStub answers siteverify success for the token "good".
func turnstileStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]any{"success": r.PostForm.Get("response") == "good"})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Row 165a — the invitation captcha keyed on the sender, through the route:
// the token travels in the body as turnstileToken.
func TestSeats_CaptchaKeyedOnSender(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	a, b := newWorld(t, pool), newWorld(t, pool)
	withKey := invitationRoutes(serviceWith(pool, nil, nil, accounts.NewTurnstile("ts-secret", turnstileStub(t))))
	noKey := invitationRoutes(serviceWith(pool, nil, nil, noCaptcha()))
	post := func(r http.Handler, c spaces.Caller, role, short string, token *string) httpAnswer {
		return call(r, c, role, http.MethodPost, "/api/v1/spaces/"+short+"/invitations", inviteBody(newEmail(), token))
	}
	good, bad := "good", "bad"

	// Ten sends in the last minutes: the threshold is reached.
	seedSends(t, pool, a.owner.UserID, 10, time.Minute)
	if ans := post(withKey, a.owner, "user", a.short, nil); ans.status != http.StatusUnprocessableEntity || ans.code != "captcha_required" {
		t.Fatalf("over the frequency without a token: %d %q, want 422 captcha_required", ans.status, ans.code)
	}
	if ans := post(withKey, a.owner, "user", a.short, &bad); ans.status != http.StatusUnprocessableEntity || ans.code != "captcha_failed" {
		t.Errorf("with an unverified token: %d %q, want 422 captcha_failed", ans.status, ans.code)
	}
	if ans := post(withKey, a.owner, "user", a.short, &good); ans.status != http.StatusOK || ans.code != "sent" {
		t.Errorf("with a verified token: %d %q, want 200 sent", ans.status, ans.code)
	}
	// Outside the key: the same sender, only the space changes — the
	// requirement stays. The admin manages every space: he is the sender.
	seedSends(t, pool, a.admin.UserID, 10, time.Minute)
	for _, short := range []string{a.short, b.short} {
		if ans := post(withKey, a.admin, "admin", short, nil); ans.status != http.StatusUnprocessableEntity || ans.code != "captcha_required" {
			t.Errorf("the same sender in space %s: %d %q, want 422 captcha_required", short, ans.status, ans.code)
		}
	}
	// The key: another sender in the same space — no captcha.
	if ans := post(withKey, b.admin, "admin", a.short, nil); ans.status != http.StatusOK {
		t.Errorf("another sender in the same space: %d %q, want 200", ans.status, ans.code)
	}
	// Without the Turnstile key, at the same frequency — no requirement.
	if ans := post(noKey, a.owner, "user", a.short, nil); ans.status != http.StatusOK {
		t.Errorf("without the Turnstile key: %d %q, want 200", ans.status, ans.code)
	}
}

// ---------------------------------------------------------------------------
// The routes, as cmd/api mounts them
// ---------------------------------------------------------------------------

// invitationRoutes mounts the logged-in invitation routes over svc.
func invitationRoutes(svc *invitations.Service) chi.Router {
	h := handler.NewInvitationsHandler(svc)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(handler.ParseUserHeaders, handler.RequireUser)
		r.Post("/spaces/{id}/invitations", h.Create)
		r.Post("/spaces/{id}/invitations/{invId}/resend", h.Resend)
		r.Delete("/spaces/{id}/invitations/{invId}", h.Revoke)
		r.Get("/me/invitations", h.Mine)
		r.Post("/me/invitations/{invId}/accept", h.AcceptMine)
		r.Post("/me/invitations/{invId}/decline", h.DeclineMine)
		r.Get("/invitations/{token}", h.View)
		r.Post("/invitations/{token}/accept", h.AcceptByToken)
		r.Post("/invitations/{token}/decline", h.DeclineByToken)
	})
	return r
}

// httpAnswer is a route's answer: the status, the `error` code or the
// `outcome` of a 200, the body.
type httpAnswer struct {
	status int
	code   string
	body   map[string]any
}

// call sends method path as the caller with role (an API key when apiKey).
func call(r http.Handler, c spaces.Caller, role, method, path string, body any) httpAnswer {
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(auth.HeaderUserID, c.UserID.String())
	req.Header.Set(auth.HeaderUsername, c.Username)
	req.Header.Set(auth.HeaderUserRole, role)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	a := httpAnswer{status: rec.Code}
	_ = json.Unmarshal(rec.Body.Bytes(), &a.body)
	if s, ok := a.body["error"].(string); ok {
		a.code = s
	} else if s, ok := a.body["outcome"].(string); ok {
		a.code = s
	}
	return a
}

func inviteBody(email string, token *string) map[string]any {
	b := map[string]any{"email": email, "language": "en"}
	if token != nil {
		b["turnstileToken"] = *token
	}
	return b
}

// recentSends is the sender's sends in the captcha window.
func recentSends(t *testing.T, pool *pgxpool.Pool, sender uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM space_invitation_sends WHERE sender_id = $1 AND sent_at > now() - interval '10 minutes'`, sender).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The captcha threshold holds against a batch
// of parallel sends. A sender with nine sends in the window starts six
// invitations without a token, to two of his spaces, while a separate
// transaction holds his users row FOR UPDATE: every invitation takes that
// row FOR SHARE (the caller's rights) after its preflight, so once all six
// are seen waiting (pg_stat_activity, directly or queued behind one
// another) every one of them has passed the preflight with nine sends in
// the journal — the overlap the unprotected check let through. The holder
// then lets them go: at most the one that takes the tenth place goes
// through, every other is refused (captcha_required, or concurrent_change
// if its retries run out), and the journal never goes past the threshold.
func TestSeats_ParallelSendsCannotPassTheCaptchaThreshold(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	a := newWorld(t, pool)
	second := testsupport.SeedSpace(t, pool, a.owner.UserID)
	t.Cleanup(func() { cleanSpace(pool, second) })
	var secondShort string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, second).Scan(&secondShort); err != nil {
		t.Fatal(err)
	}
	r := invitationRoutes(serviceWith(pool, nil, nil, accounts.NewTurnstile("ts-secret", turnstileStub(t))))
	seedSends(t, pool, a.owner.UserID, 9, time.Minute)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	var pid int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, a.owner.UserID); err != nil {
		t.Fatal(err)
	}

	const n = 6
	answers := make([]httpAnswer, n)
	var done sync.WaitGroup
	done.Add(n)
	for i := 0; i < n; i++ {
		short := a.short
		if i%2 == 1 {
			short = secondShort
		}
		go func(i int, short string) {
			defer done.Done()
			answers[i] = call(r, a.owner, "user", http.MethodPost, "/api/v1/spaces/"+short+"/invitations", inviteBody(newEmail(), nil))
		}(i, short)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var waiting int
		if err := pool.QueryRow(ctx, `
WITH RECURSIVE waiters(pid) AS (
    SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
    UNION
    SELECT a.pid FROM pg_stat_activity a JOIN waiters w ON w.pid = ANY(pg_blocking_pids(a.pid)))
SELECT count(*) FROM waiters`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of the %d sends wait behind the held row: they did not all pass the preflight together", waiting, n)
		}
	}
	if got := recentSends(t, pool, a.owner.UserID); got != 9 {
		t.Fatalf("the journal holds %d sends while all wait, want the 9 seeded", got)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	done.Wait()

	sent, required := 0, 0
	for i, ans := range answers {
		switch {
		case ans.status == http.StatusOK && ans.code == "sent":
			sent++
		case ans.status == http.StatusUnprocessableEntity && ans.code == "captcha_required":
			required++
		case ans.status == http.StatusUnprocessableEntity && ans.code == "concurrent_change":
		default:
			t.Errorf("send %d: %d %q, want sent, captcha_required or concurrent_change", i, ans.status, ans.code)
		}
	}
	if sent > 1 {
		t.Errorf("%d sends went through past the threshold of ten, want at most one", sent)
	}
	if required == 0 {
		t.Errorf("no send was refused captcha_required: %+v", answers)
	}
	if got := recentSends(t, pool, a.owner.UserID); got > 10 {
		t.Errorf("the journal holds %d sends in the window, want at most 10", got)
	}
}

// A resend is a send: it is counted in the same journal
// and passes the same captcha rule (spaces/contract.go, resend: an optional
// body with turnstileToken; captcha_required, captcha_failed). The key is
// the sender of the resend: an admin resending another user's invitation is
// judged by his own sends.
func TestSeats_ResendPassesTheCaptchaRule(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	w := newWorld(t, pool)
	r := invitationRoutes(serviceWith(pool, nil, nil, accounts.NewTurnstile("ts-secret", turnstileStub(t))))
	inv := w.invite(serviceWith(pool, nil, nil, noCaptcha()), w.owner, newEmail())
	resend := func(c spaces.Caller, role string, token *string) httpAnswer {
		var body any
		if token != nil {
			body = map[string]any{"turnstileToken": *token}
		}
		return call(r, c, role, http.MethodPost, "/api/v1/spaces/"+w.short+"/invitations/"+inv.String()+"/resend", body)
	}
	good, bad := "good", "bad"

	seedSends(t, pool, w.owner.UserID, 10, time.Minute)
	if a := resend(w.owner, "user", nil); a.status != http.StatusUnprocessableEntity || a.code != "captcha_required" {
		t.Errorf("resend over the frequency without a token: %d %q, want 422 captcha_required", a.status, a.code)
	}
	if a := resend(w.owner, "user", &bad); a.status != http.StatusUnprocessableEntity || a.code != "captcha_failed" {
		t.Errorf("resend with an unverified token: %d %q, want 422 captcha_failed", a.status, a.code)
	}
	if a := resend(w.owner, "user", &good); a.status != http.StatusOK {
		t.Errorf("resend with a verified token: %d %q, want 200", a.status, a.code)
	}
	// The admin resending the owner's invitation: his own sends decide.
	if a := resend(w.admin, "admin", nil); a.status != http.StatusOK {
		t.Errorf("the admin, below the threshold, resending another's invitation: %d %q, want 200", a.status, a.code)
	}
	seedSends(t, pool, w.admin.UserID, 10, time.Minute)
	if a := resend(w.admin, "admin", nil); a.status != http.StatusUnprocessableEntity || a.code != "captcha_required" {
		t.Errorf("the admin over the frequency: %d %q, want 422 captcha_required", a.status, a.code)
	}
}
