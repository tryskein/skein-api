package spaces

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Row 49 (K-7, the retry): the transaction wrapper retries a serialisation
// failure (40001) or a deadlock (40P01) — two failures and a success end in
// success; three failures end in ErrConcurrentChange; any other error is
// not retried. Reference: plan K-7 ("bounded retry (3 attempts)…
// after exhaustion — 422 concurrent_change").
func TestRetry_BoundedAndClassified(t *testing.T) {
	serialization := &pgconn.PgError{Code: "40001"}
	deadlock := &pgconn.PgError{Code: "40P01"}
	other := errors.New("not a serialisation failure")
	unique := &pgconn.PgError{Code: "23505"}

	cases := []struct {
		name         string
		results      []error
		wantAttempts int
		wantConflict bool
		wantErr      error
	}{
		{"success at once", []error{nil}, 1, false, nil},
		{"two failures then success", []error{serialization, deadlock, nil}, 3, false, nil},
		{"three serialisation failures", []error{serialization, serialization, serialization}, 3, true, nil},
		{"three deadlocks", []error{deadlock, deadlock, deadlock}, 3, true, nil},
		{"other error is not retried", []error{other}, 1, false, other},
		{"a unique violation is not retried", []error{unique}, 1, false, unique},
		{"other error after a failure", []error{serialization, other}, 2, false, other},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			attempts := 0
			err := retry(func() error {
				if attempts >= len(c.results) {
					t.Fatalf("attempt %d beyond the scripted %d", attempts+1, len(c.results))
				}
				r := c.results[attempts]
				attempts++
				return r
			})
			if attempts != c.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, c.wantAttempts)
			}
			if got := errors.Is(err, ErrConcurrentChange); got != c.wantConflict {
				t.Errorf("ErrConcurrentChange = %v (err %v), want %v", got, err, c.wantConflict)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Errorf("err = %v, want %v", err, c.wantErr)
			}
			if !c.wantConflict && c.wantErr == nil && err != nil {
				t.Errorf("err = %v, want success", err)
			}
		})
	}
}
