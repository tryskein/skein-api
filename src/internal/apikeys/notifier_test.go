package apikeys_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/apikeys"
)

// failingNotifier refuses every notice, as a broken mail queue would.
type failingNotifier struct{ calls int }

var errNoticeRefused = errors.New("notice refused")

func (n *failingNotifier) KeyCreatedInTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, string) error {
	n.calls++
	return errNoticeRefused
}

// sk-977.7.49: the owner's notice is part of the key's transaction — when
// it cannot be queued, neither Create nor Reissue leaves a key behind. Red
// if the notice runs outside the transaction (a key without its notice).
func TestCreatedNotifier_FailureLeavesNoKey(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUserDirect(t, pool, false)
	all := apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
	base := createKey(t, apikeys.New(pool), owner, "base", all, testPolicy)

	n := &failingNotifier{}
	store := apikeys.New(pool).WithCreatedNotifier(n)
	m := newKeyMaterial(t)
	if _, err := store.Create(ctx, apikeys.NewKey{UserID: owner, Label: "noticed", Prefix: m.prefix, Hash: m.hash, Grant: all, Lang: "en"}, testPolicy); !errors.Is(err, errNoticeRefused) {
		t.Fatalf("Create with a refused notice: err = %v, want the notifier's error", err)
	}
	m = newKeyMaterial(t)
	if _, err := store.Reissue(ctx, owner, base.ID, m.prefix, m.hash, testPolicy, "en"); !errors.Is(err, errNoticeRefused) {
		t.Fatalf("Reissue with a refused notice: err = %v, want the notifier's error", err)
	}
	if n.calls != 2 {
		t.Errorf("notifier called %d times, want 2", n.calls)
	}
	var keys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id = $1`, owner).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 1 {
		t.Errorf("%d keys of the owner, want only the base key", keys)
	}
}
