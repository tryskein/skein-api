package issues

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVersionAwareAbort_WithoutVersionEvidenceDoesNotInventConflict(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://localhost:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	// Closing an unused pool makes reads fail before any network operation.
	pool.Close()
	store := NewStore(pool)
	version := 1
	for _, sqlstate := range []string{"40001", "40P01"} {
		t.Run(sqlstate, func(t *testing.T) {
			got := store.degradeSerializationAbort(context.Background(), &pgconn.PgError{Code: sqlstate}, "close", "demo-1", &version)
			if !errors.Is(got, ErrSerializationRetry) {
				t.Fatalf("error = %v, want retryable abort without a fabricated currentVersion", got)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := store.degradeSerializationAbort(ctx, &pgconn.PgError{Code: "40001"}, "close", "demo-1", &version); !errors.Is(got, context.Canceled) {
		t.Fatalf("cancelled read = %v, want context.Canceled", got)
	}
	other := errors.New("unrelated failure")
	if got := store.degradeSerializationAbort(context.Background(), other, "close", "demo-1", &version); !errors.Is(got, other) {
		t.Fatalf("unrelated error changed: %v", got)
	}
}
