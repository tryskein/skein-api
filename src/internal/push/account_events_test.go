package push_test

// account_events_test.go — account revocation of push subscriptions
// through the real entries of the account events (integration,
// TEST_DATABASE_URL). The reference is the specification: US-7, US-8
// (server half), R-8, INV-4, INV-5, SC-003, Edge 2 and Observability
// («removal of subscriptions by an account event: user id, reason,
// number»), written before this code.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/push"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
	"skein-api/internal/users"
)

const resetPassword = "Push-events-reset-2026!"

func usersStoreOn(pool *pgxpool.Pool) *users.Store {
	return users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
}

func resetServiceOn(pool *pgxpool.Pool) *accounts.Service {
	return accounts.NewService(accounts.ServiceConfig{
		EmailEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local",
	}, pool, usersStoreOn(pool), accounts.NewDirectory(pool),
		accounts.NewTurnstile("", accounts.TurnstileVerifyURL), accounts.NewRateLimiter())
}

var operator = users.Actor{Username: "operator", Role: "system"}

// eventPath is one entry of an account event. prepare sets the user up for
// the event and returns the entry, run over the pool it is given.
type eventPath struct {
	name   string
	reason string
	// raisesEpoch — the event raises the session epoch.
	raisesEpoch bool
	prepare     func(t *testing.T, w txWorld, user uuid.UUID) func(pool *pgxpool.Pool) error
	// reenable switches the user on again after the event (nil: the
	// event is final).
	reenable func(t *testing.T, w txWorld, user uuid.UUID)
}

func reactivate(t *testing.T, w txWorld, user uuid.UUID) {
	t.Helper()
	if err := usersStoreOn(w.pool).Reactivate(context.Background(), user, operator); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
}

// cleanAccountTraces removes what the events leave outside the user's row
// cascade: events, tokens, emails, reserved identities of a deletion.
func cleanAccountTraces(t *testing.T, w txWorld, user uuid.UUID) {
	t.Helper()
	var email, username string
	if err := w.pool.QueryRow(context.Background(), `SELECT email, username FROM users WHERE id = $1`, user).Scan(&email, &username); err != nil {
		t.Fatalf("read account: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = w.pool.Exec(ctx, `DELETE FROM events WHERE actor_user_id = $1 OR (entity_type = 'user' AND entity_id = $1::text)`, user)
		_, _ = w.pool.Exec(ctx, `DELETE FROM auth_tokens WHERE user_id = $1`, user)
		_, _ = w.pool.Exec(ctx, `DELETE FROM outgoing_emails WHERE user_id = $1`, user)
		_, _ = w.pool.Exec(ctx, `
DELETE FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to(lower(btrim($1)), 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, email, username)
	})
}

// resetLink requests a password reset for user and plays the mail worker:
// leases the queued email and issues its link token.
func resetLink(t *testing.T, w txWorld, user uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	var email string
	if err := w.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, user).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if err := resetServiceOn(w.pool).RequestPasswordReset(ctx,
		accounts.PasswordResetRequest{Email: email, Language: accounts.LanguageEN}, "10.20.0.1"); err != nil {
		t.Fatalf("request the reset: %v", err)
	}
	var (
		emailID uuid.UUID
		gen     int
	)
	if err := w.pool.QueryRow(ctx, `
UPDATE outgoing_emails
   SET state = 'leased', attempts = attempts + 1, leased_until = now() + interval '2 minutes'
 WHERE id = (SELECT id FROM outgoing_emails WHERE user_id = $1 AND kind = 'password_reset' AND state = 'pending'
             ORDER BY created_at DESC LIMIT 1)
RETURNING id, attempts`, user).Scan(&emailID, &gen); err != nil {
		t.Fatalf("lease the reset email: %v", err)
	}
	raw, err := tokens.IssueForEmail(ctx, w.pool, emailID, gen)
	if err != nil {
		t.Fatalf("IssueForEmail: %v", err)
	}
	return raw
}

func eventPaths() []eventPath {
	return []eventPath{
		{
			name: "deactivation", reason: push.ReasonDeactivation, raisesEpoch: true,
			prepare: func(t *testing.T, w txWorld, user uuid.UUID) func(*pgxpool.Pool) error {
				cleanAccountTraces(t, w, user)
				return func(pool *pgxpool.Pool) error {
					return usersStoreOn(pool).Deactivate(context.Background(), user, operator)
				}
			},
			reenable: reactivate,
		},
		{
			name: "password reset by link", reason: push.ReasonPasswordReset, raisesEpoch: true,
			prepare: func(t *testing.T, w txWorld, user uuid.UUID) func(*pgxpool.Pool) error {
				cleanAccountTraces(t, w, user)
				raw := resetLink(t, w, user)
				return func(pool *pgxpool.Pool) error {
					_, err := resetServiceOn(pool).CompletePasswordReset(context.Background(), raw,
						accounts.PasswordResetCompleteRequest{Password: resetPassword}, "10.20.0.2")
					return err
				}
			},
		},
		{
			name: "external user removed from the last project", reason: push.ReasonExternalOrphaned,
			prepare: func(t *testing.T, w txWorld, user uuid.UUID) func(*pgxpool.Pool) error {
				cleanAccountTraces(t, w, user)
				w.exec(t, `UPDATE users SET is_external = true WHERE id = $1`, user)
				owner, ownerName := testsupport.SeedUser(t, w.pool, "user")
				project, _ := testsupport.SeedProject(t, w.pool, owner, "pev")
				testsupport.AddProjectMember(t, w.pool, project, user, "member")
				return func(pool *pgxpool.Pool) error {
					return projects.NewStore(pool).RemoveMember(context.Background(), project, user,
						projects.Actor{UserID: owner, Username: ownerName, Role: "human"})
				}
			},
			reenable: reactivate,
		},
		{
			name: "account deletion", reason: push.ReasonAccountDeleted, raisesEpoch: true,
			prepare: func(t *testing.T, w txWorld, user uuid.UUID) func(*pgxpool.Pool) error {
				cleanAccountTraces(t, w, user)
				return func(pool *pgxpool.Pool) error {
					return usersStoreOn(pool).DeleteAccount(context.Background(), user, "", operator)
				}
			},
		},
	}
}

// accountLogs keeps the attributes of every slog record.
type accountLogs struct {
	mu      sync.Mutex
	records []map[string]string
}

func (c *accountLogs) Enabled(context.Context, slog.Level) bool { return true }

func (c *accountLogs) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = fmt.Sprint(a.Value.Any())
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, attrs)
	c.mu.Unlock()
	return nil
}

func (c *accountLogs) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *accountLogs) WithGroup(string) slog.Handler      { return c }

// removals returns the records of user that carry a reason.
func (c *accountLogs) removals(user uuid.UUID) []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]string
	for _, r := range c.records {
		if _, ok := r["reason"]; ok && r["userId"] == user.String() {
			out = append(out, r)
		}
	}
	return out
}

func captureLogs(t *testing.T) *accountLogs {
	t.Helper()
	logs := &accountLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// failAfterPushDeletion makes the event of user fail at its first write of
// a user.* event — written after the users row and after
// DeleteAllForUserInTx on every entry — with a trigger of the test
// database, dropped in cleanup.
func failAfterPushDeletion(t *testing.T, w txWorld, user uuid.UUID) {
	t.Helper()
	name := "push_test_late_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	w.exec(t, fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'late failure of the account event';
END $$`, name))
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	w.exec(t, fmt.Sprintf(`
CREATE TRIGGER %s BEFORE INSERT ON events FOR EACH ROW
WHEN (NEW.entity_type = 'user' AND NEW.entity_id = '%s' AND NEW.type LIKE 'user.%%')
EXECUTE FUNCTION %s()`, name, user, name))
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON events`, name))
	})
}

// accountRow renders what each event changes in the users row.
func accountRow(t *testing.T, w txWorld, user uuid.UUID) string {
	t.Helper()
	var s string
	if err := w.pool.QueryRow(context.Background(), `
SELECT concat_ws(',', deactivated_at IS NULL, deleted_at IS NULL, session_epoch, password_hash, email)
  FROM users WHERE id = $1`, user).Scan(&s); err != nil {
		t.Fatalf("read account row: %v", err)
	}
	return s
}

func subscriptionsOf(t *testing.T, w txWorld, user uuid.UUID) int {
	t.Helper()
	return w.count(t, `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`, user)
}

// Row 20 — US-7, SC-003 (first half), INV-5, R-8, Observability: each
// event entry deletes every subscription of the user with the event; a late
// failure of the event rolls the users row and the subscriptions back
// together.
func TestAccountEvents_DeleteSubscriptionsWithTheEvent(t *testing.T) {
	w := newTxWorld(t)
	logs := captureLogs(t)
	for _, path := range eventPaths() {
		t.Run(path.name, func(t *testing.T) {
			user, _ := w.user(t)
			other, _ := w.user(t)
			label, revokedLabel := uuid.New(), uuid.New()
			w.seedSub(t, user, newEndpoint(), &label)
			w.seedSub(t, user, newEndpoint(), nil)
			testsupport.SeedPushDeviceRevocation(t, w.pool, user, revokedLabel)
			othersSub := w.seedSub(t, other, newEndpoint(), &label)
			event := path.prepare(t, w, user)

			if err := event(w.pool); err != nil {
				t.Fatalf("event: %v", err)
			}
			if n := subscriptionsOf(t, w, user); n != 0 {
				t.Errorf("subscriptions of the user after the event = %d, want 0", n)
			}
			if !w.exists(t, othersSub) {
				t.Error("another user's subscription was deleted")
			}
			got := logs.removals(user)
			if len(got) != 1 || got[0]["reason"] != path.reason || got[0]["removed"] != "2" {
				t.Errorf("removal records = %v, want one with reason %s and removed 2", got, path.reason)
			}
			wantRevoked := path.reason != push.ReasonAccountDeleted
			if got := w.revoked(t, user, revokedLabel); got != wantRevoked {
				t.Errorf("device revocation present after the event = %v, want %v", got, wantRevoked)
			}
		})
		t.Run(path.name+": late failure rolls everything back", func(t *testing.T) {
			user, _ := w.user(t)
			w.seedSub(t, user, newEndpoint(), nil)
			w.seedSub(t, user, newEndpoint(), labelPtr(uuid.New()))
			event := path.prepare(t, w, user)
			failAfterPushDeletion(t, w, user)
			before := accountRow(t, w, user)

			if err := event(w.pool); err == nil {
				t.Fatal("the event did not fail")
			}
			if after := accountRow(t, w, user); after != before {
				t.Errorf("the failed event changed the users row: before %s, after %s", before, after)
			}
			if n := subscriptionsOf(t, w, user); n != 2 {
				t.Errorf("subscriptions after the failed event = %d, want 2", n)
			}
			if got := logs.removals(user); len(got) != 0 {
				t.Errorf("removal records after the failed event: %v", got)
			}
		})
	}
}

// Row 21 — SC-003 (second half), US-7: an ordinary password change and a
// password set by an administrator leave the subscriptions as they are.
func TestAccountEvents_PasswordChangesKeepSubscriptions(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	user, _ := w.user(t)
	cleanAccountTraces(t, w, user)
	const current = "Push-events-current-2026!"
	hash, err := auth.Hash(current, auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	w.exec(t, `UPDATE users SET password_hash = $2 WHERE id = $1`, user, hash)
	w.seedSub(t, user, newEndpoint(), labelPtr(uuid.New()))
	w.seedSub(t, user, newEndpoint(), nil)
	store := usersStoreOn(w.pool)

	if err := store.ChangePassword(ctx, user, current, "Push-events-changed-2026!"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if n := subscriptionsOf(t, w, user); n != 2 {
		t.Errorf("subscriptions after a password change = %d, want 2", n)
	}
	set := "Push-events-admin-set-2026!"
	if _, err := store.UpdateUser(ctx, user, users.UpdateUserInput{Password: &set}, users.Actor{Username: "admin", Role: "human"}); err != nil {
		t.Fatalf("UpdateUser with a password: %v", err)
	}
	if n := subscriptionsOf(t, w, user); n != 2 {
		t.Errorf("subscriptions after an administrator set the password = %d, want 2", n)
	}
}

// eventGate is the commit gate of a test pool for an event: it records the
// backend pid of every statement and, when holding, holds the first COMMIT
// after a statement containing marker ("" — any) until opened.
type eventGate struct {
	marker   string
	hold     bool
	seen     atomic.Bool
	done     atomic.Bool
	pid      atomic.Int32
	held     chan struct{}
	release  chan struct{}
	released sync.Once
}

func (g *eventGate) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	g.pid.Store(int32(conn.PgConn().PID()))
	if !g.hold || g.done.Load() {
		return ctx
	}
	if g.marker == "" || strings.Contains(data.SQL, g.marker) {
		g.seen.Store(true)
	}
	if data.SQL == "commit" && g.seen.Load() && g.done.CompareAndSwap(false, true) {
		close(g.held)
		<-g.release
	}
	return ctx
}

func (g *eventGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (g *eventGate) open() { g.released.Do(func() { close(g.release) }) }

func gatedPool(t *testing.T, marker string, hold bool) (*pgxpool.Pool, *eventGate) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv(testsupport.TestDatabaseURL))
	if err != nil {
		t.Fatalf("parse %s: %v", testsupport.TestDatabaseURL, err)
	}
	g := &eventGate{marker: marker, hold: hold, held: make(chan struct{}), release: make(chan struct{})}
	cfg.ConnConfig.Tracer = g
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(g.open)
	return pool, g
}

// overlapping runs first and second as two product transactions that
// overlap: first is held at its commit; second starts and the test waits
// until the pid of first is among pg_blocking_pids of second; then first
// commits.
func overlapping(t *testing.T, w txWorld, first func() error, fg *eventGate, second func() error, sg *eventGate) (error, error) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	firstDone := make(chan error, 1)
	go func() { firstDone <- first() }()
	select {
	case <-fg.held:
	case err := <-firstDone:
		t.Fatalf("first transaction ended before its commit was held: %v", err)
	case <-deadline:
		t.Fatal("first transaction did not reach its commit")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- second() }()
	holder := fg.pid.Load()
	for {
		select {
		case err := <-secondDone:
			t.Fatalf("second transaction ended without waiting for the first: %v", err)
		case <-deadline:
			t.Fatal("second transaction never waited for the first")
		default:
		}
		if waiter := sg.pid.Load(); waiter != 0 {
			var blocked bool
			if err := w.pool.QueryRow(context.Background(),
				`SELECT $1::int = ANY(pg_blocking_pids($2::int))`, holder, waiter).Scan(&blocked); err != nil {
				t.Fatalf("read blocking pids: %v", err)
			}
			if blocked {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	fg.open()
	return <-firstDone, <-secondDone
}

func registerOn(pool *pgxpool.Pool, user uuid.UUID, epoch int64, label uuid.UUID) error {
	_, err := push.NewStore(pool).Register(context.Background(), push.RegisterInput{
		UserID: user, SessionEpoch: epoch, Endpoint: newEndpoint(),
		P256dh: "p", Auth: "a", DeviceLabel: &label, Mode: push.ModeUpsert,
	})
	return err
}

// Row 22 — INV-4, Edge 2, US-7: a registration overlapping an account event
// leaves no subscription after the event committed; the exception of INV-4
// by path.
func TestAccountEvents_OverlappingRegistrationLeavesNoSubscription(t *testing.T) {
	w := newTxWorld(t)
	for _, path := range eventPaths() {
		t.Run(path.name+": event, then registration", func(t *testing.T) {
			user, epoch := w.user(t)
			event := path.prepare(t, w, user)
			eventPool, eventGate := gatedPool(t, "push_subscriptions", true)
			regPool, regGate := gatedPool(t, "", false)
			eventErr, regErr := overlapping(t, w,
				func() error { return event(eventPool) }, eventGate,
				func() error { return registerOn(regPool, user, epoch, uuid.New()) }, regGate)
			if eventErr != nil {
				t.Errorf("event: %v, want nil", eventErr)
			}
			if !errors.Is(regErr, push.ErrSessionStale) {
				t.Errorf("registration: %v, want ErrSessionStale", regErr)
			}
			if n := subscriptionsOf(t, w, user); n != 0 {
				t.Errorf("subscriptions after both = %d, want 0", n)
			}
		})
		t.Run(path.name+": registration, then event", func(t *testing.T) {
			user, epoch := w.user(t)
			event := path.prepare(t, w, user)
			regPool, regGate := gatedPool(t, "", true)
			eventPool, eventGate := gatedPool(t, "", false)
			regErr, eventErr := overlapping(t, w,
				func() error { return registerOn(regPool, user, epoch, uuid.New()) }, regGate,
				func() error { return event(eventPool) }, eventGate)
			if regErr != nil || eventErr != nil {
				t.Errorf("registration %v, event %v; want both nil", regErr, eventErr)
			}
			if n := subscriptionsOf(t, w, user); n != 0 {
				t.Errorf("subscriptions after both = %d, want 0", n)
			}
		})
		if path.reason == push.ReasonAccountDeleted {
			continue
		}
		t.Run(path.name+": exception of INV-4", func(t *testing.T) {
			user, epoch := w.user(t)
			event := path.prepare(t, w, user)
			if err := event(w.pool); err != nil {
				t.Fatalf("event: %v", err)
			}
			if !path.raisesEpoch {
				// The epoch stays: while deactivated the session is refused.
				if err := registerOn(w.pool, user, epoch, uuid.New()); !errors.Is(err, push.ErrSessionStale) {
					t.Errorf("registration while deactivated: %v, want ErrSessionStale", err)
				}
			}
			if path.reenable != nil {
				path.reenable(t, w, user)
			}
			if path.raisesEpoch {
				if err := registerOn(w.pool, user, epoch, uuid.New()); !errors.Is(err, push.ErrSessionStale) {
					t.Errorf("registration with the former epoch: %v, want ErrSessionStale", err)
				}
			}
			current := w.epoch(t, user)
			if err := registerOn(w.pool, user, current, uuid.New()); err != nil {
				t.Errorf("registration of a valid session after the event: %v, want nil", err)
			}
			if n := subscriptionsOf(t, w, user); n != 1 {
				t.Errorf("subscriptions = %d, want 1", n)
			}
		})
	}
}

// Row 23 — R-8 «without revoking devices», US-8 (server half): after a
// password reset and after deactivation and reactivation the user
// registers again with a former label that was not revoked; a label revoked
// before the event stays refused.
func TestAccountEvents_LabelsSurviveTheEvent(t *testing.T) {
	w := newTxWorld(t)
	ctx := context.Background()
	for _, path := range eventPaths() {
		if path.reason != push.ReasonPasswordReset && path.reason != push.ReasonDeactivation {
			continue
		}
		t.Run(path.name, func(t *testing.T) {
			user, epoch := w.user(t)
			kept, revoked := uuid.New(), uuid.New()
			w.seedSub(t, user, newEndpoint(), &kept)
			if _, err := w.store.RevokeDevice(ctx, user, epoch, []uuid.UUID{revoked}, ""); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			event := path.prepare(t, w, user)
			if err := event(w.pool); err != nil {
				t.Fatalf("event: %v", err)
			}
			if path.reenable != nil {
				path.reenable(t, w, user)
			}
			current := w.epoch(t, user)
			if err := registerOn(w.pool, user, current, kept); err != nil {
				t.Errorf("registration with the former label: %v, want nil", err)
			}
			if err := registerOn(w.pool, user, current, revoked); !errors.Is(err, push.ErrDeviceRevoked) {
				t.Errorf("registration with the label revoked before the event: %v, want ErrDeviceRevoked", err)
			}
		})
	}
}
