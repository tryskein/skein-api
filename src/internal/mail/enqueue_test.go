package mail_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/mail"
	"skein-api/internal/testsupport"
)

var errRollback = errors.New("the action failed")

// Row 60 (INV-25): an email queued in the action's transaction exists
// exactly when the action commits — a rolled-back action leaves no row, a
// committed one exactly one.
func TestEnqueue_AtomicWithTheAction(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	userID, uname := testsupport.SeedUser(t, pool, "")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outgoing_emails WHERE user_id = $1`, userID)
	})
	msg := mail.Message{Kind: mail.KindPasswordReset, To: uname + "@test.local", Lang: mail.LangEN, UserID: userID}

	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outgoing_emails WHERE user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	var rolledBack uuid.UUID
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		id, err := mail.Enqueue(ctx, tx, msg, time.Hour)
		if err != nil {
			return err
		}
		rolledBack = id
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("rolled-back action: err = %v", err)
	}
	if rolledBack == uuid.Nil {
		t.Fatal("Enqueue returned no id")
	}
	if n := count(); n != 0 {
		t.Fatalf("after a rolled-back action: %d emails, want 0", n)
	}

	var committed uuid.UUID
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		committed, err = mail.Enqueue(ctx, tx, msg, time.Hour)
		return err
	}); err != nil {
		t.Fatalf("committed action: %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("after a committed action: %d emails, want 1", n)
	}
	var (
		state, recipient string
		deadlineSecs     int64
	)
	if err := pool.QueryRow(ctx, `
SELECT state, recipient, extract(epoch FROM deadline_at - created_at)::bigint
  FROM outgoing_emails WHERE id = $1`, committed).Scan(&state, &recipient, &deadlineSecs); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || recipient != uname+"@test.local" || deadlineSecs != 3600 {
		t.Errorf("queued email = state %q, recipient %q, deadline in %ds; want pending, %q, 3600s",
			state, recipient, deadlineSecs, uname+"@test.local")
	}
}
