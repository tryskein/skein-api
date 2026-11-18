package mail_test

// apikey_notice_test.go — the notice about a new API key (sk-977.7.49)
// through the real queue and worker.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/mail"
	"skein-api/internal/testsupport"
)

// seedKey stores an API key of userID named label and returns its id and
// creation time.
func seedKey(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, label string) (uuid.UUID, time.Time) {
	t.Helper()
	_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	id, created, err := testsupport.CreateOwnerKey(context.Background(), pool, userID, label, prefix, hash)
	if err != nil {
		t.Fatalf("seeding a key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outgoing_emails WHERE api_key_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return id, created
}

// The notifier queues the notice in the key's transaction, and the worker
// delivers it to the owner's address in the requested language with the
// key's name, its creation time and the link to the API keys page — without
// issuing a link token. Red if the notice is not queued, names something
// else, or the worker treats it as a token email.
func TestAPIKeyNotice_QueuedAndDelivered(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	resetQueue(t, pool)
	userID, to := seedUser(t, pool)
	keyID, created := seedKey(t, pool, userID, "deploy-bot")

	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return mail.APIKeyNotifier{DeliveryTTL: time.Hour}.KeyCreatedInTx(ctx, tx, userID, keyID, mail.LangRU)
	}); err != nil {
		t.Fatalf("KeyCreatedInTx: %v", err)
	}
	var (
		emailID   uuid.UUID
		kind      string
		recipient string
		lang      string
	)
	if err := pool.QueryRow(ctx, `
SELECT id, kind, recipient, lang FROM outgoing_emails WHERE api_key_id = $1`, keyID).
		Scan(&emailID, &kind, &recipient, &lang); err != nil {
		t.Fatalf("reading the queued notice: %v", err)
	}
	if kind != "api_key_created" || recipient != strings.ToLower(to) || lang != "ru" {
		t.Fatalf("queued notice = %s to %s in %s; want api_key_created to %s in ru", kind, recipient, lang, to)
	}

	r := &relay{}
	run := newRunner(t, pool, r, newClock())
	before := deliveries(t)
	tick(t, run)

	if e := readEmail(t, pool, emailID); e.state != "final" || e.outcome != mail.OutcomeSuccess {
		t.Fatalf("notice = %s/%q, want final/success", e.state, e.outcome)
	}
	assertOneDelivery(t, before, mail.KindAPIKeyCreated, mail.OutcomeSuccess)
	if len(r.Accepted()) != 1 || r.Accepted()[0].to != strings.ToLower(to) {
		t.Fatalf("relay accepted %d messages, want 1 to %s", len(r.Accepted()), to)
	}
	text := textPart(t, r.Accepted()[0].msg)
	for _, want := range []string{
		"API-ключ", // the Russian template
		"deploy-bot",
		created.UTC().Format("2006-01-02 15:04 UTC"),
		linkBase + "/settings/api-keys",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the notice does not contain %q:\n%s", want, text)
		}
	}
	var tokensIssued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_tokens WHERE email_id = $1`, emailID).Scan(&tokensIssued); err != nil {
		t.Fatal(err)
	}
	if tokensIssued != 0 {
		t.Errorf("the notice was issued %d link tokens, want none", tokensIssued)
	}
}

// A notice whose key row is gone by delivery time is cancelled, not sent.
func TestAPIKeyNotice_KeyGoneIsCancelled(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	resetQueue(t, pool)
	userID, to := seedUser(t, pool)
	keyID, _ := seedKey(t, pool, userID, "short-lived")
	id := enqueue(t, pool, mail.Message{Kind: mail.KindAPIKeyCreated, To: to, Lang: mail.LangEN, UserID: userID, APIKeyID: keyID}, time.Hour)
	if _, err := pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, keyID); err != nil {
		t.Fatalf("deleting the key: %v", err)
	}

	r := &relay{}
	run := newRunner(t, pool, r, newClock())
	before := deliveries(t)
	tick(t, run)

	if e := readEmail(t, pool, id); e.state != "final" || e.outcome != mail.OutcomeCancelled || r.Calls() != 0 {
		t.Fatalf("notice = %s/%q, relay calls %d; want final/cancelled and no call", e.state, e.outcome, r.Calls())
	}
	assertOneDelivery(t, before, mail.KindAPIKeyCreated, mail.OutcomeCancelled)
}

// Validate refuses an API key notice without its key or owner, and any other
// kind carrying a key.
func TestValidate_APIKeyNoticeSubject(t *testing.T) {
	user, key := uuid.New(), uuid.New()
	cases := []struct {
		name string
		m    mail.Message
		ok   bool
	}{
		{"owner and key", mail.Message{Kind: mail.KindAPIKeyCreated, To: "a@b.c", Lang: mail.LangEN, UserID: user, APIKeyID: key}, true},
		{"no key", mail.Message{Kind: mail.KindAPIKeyCreated, To: "a@b.c", Lang: mail.LangEN, UserID: user}, false},
		{"no owner", mail.Message{Kind: mail.KindAPIKeyCreated, To: "a@b.c", Lang: mail.LangEN, APIKeyID: key}, false},
		{"password reset with a key", mail.Message{Kind: mail.KindPasswordReset, To: "a@b.c", Lang: mail.LangEN, UserID: user, APIKeyID: key}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.m.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
