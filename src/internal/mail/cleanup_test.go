package mail_test

// cleanup_test.go — the retention sweep of outgoing_emails (sk-674.18,
// T022; matrix row 78). Model — outbox/cleanup_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/mail"
	"skein-api/internal/testsupport"
)

// seedEmailRow writes one email in the given state directly — the sweep
// reads only the row, not how it got there.
func seedEmailRow(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, to, state string, finalizedAt *time.Time, now time.Time) uuid.UUID {
	t.Helper()
	var outcome *string
	if state == "final" {
		o := mail.OutcomeSuccess
		outcome = &o
	}
	var leasedUntil *time.Time
	if state == "leased" {
		lu := now.Add(time.Minute)
		leasedUntil = &lu
	}
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
INSERT INTO outgoing_emails (kind, recipient, lang, user_id, state, outcome, finalized_at,
                             leased_until, attempts, deadline_at, created_at)
VALUES ('password_reset', $2, 'en', $1, $3, $4, $5, $6, 1, $7, $8)
RETURNING id`, userID, to, state, outcome, finalizedAt, leasedUntil,
		now.Add(24*time.Hour), now.Add(-40*24*time.Hour)).Scan(&id); err != nil {
		t.Fatalf("seeding a %s email: %v", state, err)
	}
	return id
}

// Row 78 (K-9): final emails whose outcome is older than 30 days are
// deleted; a final email younger than that, and every unfinished one
// (pending, leased) however old, stay. Red if the sweep keeps what has
// aged out or removes an email still owed an outcome.
func TestCleanup_DeletesFinalEmailsPastRetention(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	ctx := context.Background()
	userID, to := seedUser(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }

	aged := seedEmailRow(t, pool, userID, to, "final", at(31*24*time.Hour), now)
	young := seedEmailRow(t, pool, userID, to, "final", at(29*24*time.Hour), now)
	pending := seedEmailRow(t, pool, userID, to, "pending", nil, now)
	leased := seedEmailRow(t, pool, userID, to, "leased", nil, now)

	deleted, err := mail.NewCleanupRunner(pool, mail.CleanupOptions{Now: func() time.Time { return now }}).RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	exists := func(id uuid.UUID) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outgoing_emails WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if exists(aged) {
		t.Error("a final email 31 days past its outcome was kept")
	}
	for name, id := range map[string]uuid.UUID{"final, 29 days": young, "pending": pending, "leased": leased} {
		if !exists(id) {
			t.Errorf("the %s email was deleted", name)
		}
	}
	if deleted != 1 {
		t.Errorf("RunOnce reported %d deleted, want 1", deleted)
	}
}

// INV-25, K-9: with mail switched off only the
// cleanup runs. An email a stopped worker left leased — its lease and its
// deadline both past — is returned, ends as expired, and is then deleted
// when its retention is over. A lease still running is left to its holder
// even though its deadline has passed. Red if a leftover lease stays
// without an outcome (and with the recipient's address) forever.
func TestCleanup_MailOffFinishesALeftoverLease(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	ctx := context.Background()
	userID, to := seedUser(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	leased := func(leasedUntil time.Time) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO outgoing_emails (kind, recipient, lang, user_id, state, leased_until, attempts, deadline_at, created_at)
VALUES ('password_reset', $2, 'en', $1, 'leased', $3, 1, $4, $5)
RETURNING id`, userID, to, leasedUntil, now.Add(-time.Minute), now.Add(-2*24*time.Hour)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	leftover := leased(now.Add(-time.Minute))
	active := leased(now.Add(time.Minute))
	before := deliveries(t)

	if _, err := mail.NewCleanupRunner(pool, mail.CleanupOptions{Now: func() time.Time { return now }}).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if e := readEmail(t, pool, leftover); e.state != "final" || e.outcome != mail.OutcomeExpired {
		t.Fatalf("leftover lease = %s/%q after the cleanup, want final/expired", e.state, e.outcome)
	}
	if e := readEmail(t, pool, active); e.state != "leased" {
		t.Errorf("a running lease was taken from its holder: %s/%q", e.state, e.outcome)
	}
	assertOneDelivery(t, before, mail.KindPasswordReset, mail.OutcomeExpired)

	later := now.Add(mail.Retention + time.Hour)
	if _, err := mail.NewCleanupRunner(pool, mail.CleanupOptions{Now: func() time.Time { return later }}).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce after the retention: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outgoing_emails WHERE id = $1`, leftover).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the expired email was kept past its retention")
	}
}

// K-9 "and on account deletion", K-21): an
// invitation email is addressed by invitation, not by user, so the step by
// user_id never reaches it. Once the account of that address is deleted —
// its email is reserved as sha256(lower(email)) in reserved_identities
// (K-21, T041) — its final invitation emails go on the next pass, young or
// not; an invitation email to an address of nobody deleted stays until its
// retention. Red if the address of a deleted account outlives it in the
// queue.
func TestCleanup_DeletesFinalInvitationEmailsOfDeletedAccounts(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	invite := func(to string, inv uuid.UUID) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO outgoing_emails (kind, recipient, lang, invitation_id, state, outcome, finalized_at,
                             attempts, deadline_at, created_at)
VALUES ('space_invite', $1, 'en', $2, 'final', 'success', $3, 1, $4, $5)
RETURNING id`, to, inv, now.Add(-24*time.Hour), now.Add(24*time.Hour), now.Add(-2*24*time.Hour)).Scan(&id); err != nil {
			t.Fatalf("seeding an invitation email: %v", err)
		}
		return id
	}
	gone, goneTo, _, _ := seedInvitation(t, pool)
	kept, keptTo, _, _ := seedInvitation(t, pool)
	toGone := invite(goneTo, gone)
	toKept := invite(keptTo, kept)
	if _, err := pool.Exec(ctx, `
INSERT INTO reserved_identities (kind, value_hash) VALUES ('email', sha256(convert_to(lower($1::text), 'UTF8')))`, goneTo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM reserved_identities WHERE kind = 'email' AND value_hash = sha256(convert_to(lower($1::text), 'UTF8'))`, goneTo)
	})

	if _, err := mail.NewCleanupRunner(pool, mail.CleanupOptions{Now: func() time.Time { return now }}).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	count := func(id uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outgoing_emails WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(toGone) != 0 {
		t.Error("the final invitation email to a deleted account's address was kept")
	}
	if count(toKept) != 1 {
		t.Error("an invitation email to an address of nobody deleted was deleted before its retention")
	}
}
