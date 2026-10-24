package issues

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestMapSerializationExhausted pins the store-side half of the sk-303.17
// coderev critical fix: AddComment / UpdateComment / DeleteComment /
// CreateEpicWithChildren all route their withSerializableRetry result
// through this one function, so its behaviour IS the exhaustion contract
// for all four. Deterministic and DB-free — the retryable/non-retryable
// split is exercised directly with synthetic errors, mirroring how
// TestIsRetryableTxError_ContextErrorsAreNotRetryable pins the predicate
// it delegates to.
//
// Red on the pre-fix code: mapSerializationExhausted did not exist (the
// four call sites did `return nil, err` / `return
// s.withSerializableRetry(...)` directly), so every case below would
// fail to compile, let alone pass.
func TestMapSerializationExhausted(t *testing.T) {
	s := &Store{}
	ctx := context.Background()

	cases := []struct {
		name string
		err  error
		// want is the error mapSerializationExhausted should return.
		// nil means "err unchanged" (checked by pointer/value equality
		// via errors.Is against the input for sentinels, or a plain
		// comparison for nil).
		wantSerializationRetry bool
	}{
		{"nil is untouched", nil, false},
		{"plain business error passes through", ErrCommentNotFound, false},
		{"validation error passes through", &ValidationError{Code: "x", Message: "x"}, false},
		{"context canceled is not retryable", context.Canceled, false},
		{"unique violation is not retryable", &pgconn.PgError{Code: "23505"}, false},
		{"serialization failure exhausts to ErrSerializationRetry",
			&pgconn.PgError{Code: pgSerializationFailure}, true},
		{"deadlock detected exhausts to ErrSerializationRetry",
			&pgconn.PgError{Code: pgDeadlockDetected}, true},
		{"wrapped serialization failure exhausts to ErrSerializationRetry",
			fmt.Errorf("inserting comment: %w", &pgconn.PgError{Code: pgSerializationFailure}), true},
		{"serialization failure wrapped in ErrEpicChildFailed exhausts too",
			&ErrEpicChildFailed{Index: 1, Err: &pgconn.PgError{Code: pgSerializationFailure}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.mapSerializationExhausted(ctx, tc.err, "testOp", 3, "targetId", "t-1")
			if tc.wantSerializationRetry {
				if !errors.Is(got, ErrSerializationRetry) {
					t.Fatalf("mapSerializationExhausted(%v) = %v, want errors.Is(_, ErrSerializationRetry)",
						tc.err, got)
				}
				return
			}
			if tc.err == nil {
				if got != nil {
					t.Fatalf("mapSerializationExhausted(nil) = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.err) && got != tc.err { //nolint:errorlint // exact passthrough check
				t.Fatalf("mapSerializationExhausted(%v) = %v, want the SAME error unchanged", tc.err, got)
			}
			if errors.Is(got, ErrSerializationRetry) {
				t.Fatalf("mapSerializationExhausted(%v) = %v, must NOT become ErrSerializationRetry "+
					"(not a retryable-abort error)", tc.err, got)
			}
		})
	}
}
