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
	for _, sqlstate := range []string{"40001", "40P01"} {
		if got := store.degradeSerializationAbort(ctx, &pgconn.PgError{Code: sqlstate}, "close", "demo-1", &version); !errors.Is(got, context.Canceled) {
			t.Fatalf("cancelled %s = %v, want context.Canceled", sqlstate, got)
		}
	}
	other := errors.New("unrelated failure")
	if got := store.degradeSerializationAbort(context.Background(), other, "close", "demo-1", &version); !errors.Is(got, other) {
		t.Fatalf("unrelated error changed: %v", got)
	}
}

func TestVersionAwareAbort_ClassifiesTheObservedVersion(t *testing.T) {
	e := newWritePathEnv(t)
	issue := e.newIssue(t, nil)
	if _, err := e.pool.Exec(t.Context(), "UPDATE issues SET version=2 WHERE id=$1", issue.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		expected   int
		wantsRetry bool
	}{
		{"genuine change", 1, false},
		{"unchanged version", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := e.store.degradeSerializationAbort(t.Context(), &pgconn.PgError{Code: "40001"}, "close", issue.ID, &tc.expected)
			if tc.wantsRetry {
				if !errors.Is(got, ErrSerializationRetry) {
					t.Fatalf("unchanged version error = %v, want ErrSerializationRetry", got)
				}
				return
			}
			var mismatch *ErrVersionConflict
			if !errors.As(got, &mismatch) || mismatch.CurrentVersion != 2 || errors.Is(got, ErrSerializationRetry) {
				t.Fatalf("genuine version change error = %v, want ErrVersionConflict with currentVersion 2", got)
			}
		})
	}
}
