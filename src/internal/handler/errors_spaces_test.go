package handler

// errors_spaces_test.go — sk-674.18 T018/T024, matrix row 50 (K-7, SC-012),
// the REST half. The MCP half — the same primitive error, the same code — is
// internal/mcp/tools/errors_spaces_test.go (by design:
// the mappers of each channel are reached from its own package).
//
// Reference: plan K-7, written before the code — a membership operation that
// lost to a concurrent one after its bounded retries is a business refusal,
// 422 with the machine code `concurrent_change`, the same in REST and MCP —
// never 500 or 409. The error is the public primitive's: spaces.WithRetry
// over a transaction that fails with a serialisation error on every attempt
// (row 49's exhaustion), not a hand-made sentinel.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/spaces"
)

// failingTx is a transaction whose body is never reached by the database:
// the attempt fails before commit, and rollback succeeds.
type failingTx struct{ pgx.Tx }

func (failingTx) Rollback(context.Context) error { return nil }
func (failingTx) Commit(context.Context) error   { return nil }

// failingBeginner opens failingTx and counts the attempts.
type failingBeginner struct{ attempts int }

func (b *failingBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	b.attempts++
	return failingTx{}, nil
}

// exhaustedRetries is the error of the public primitive after every attempt
// lost a serialisation race.
func exhaustedRetries(t *testing.T) error {
	t.Helper()
	b := &failingBeginner{}
	err := spaces.WithRetry(context.Background(), b, func(pgx.Tx) error {
		return &pgconn.PgError{Code: "40001"}
	})
	if b.attempts < 2 || !errors.Is(err, spaces.ErrConcurrentChange) {
		t.Fatalf("the primitive after %d attempts = %v, want the exhausted retries", b.attempts, err)
	}
	return err
}

func TestConcurrentChange_RESTIs422WithTheCode(t *testing.T) {
	err := exhaustedRetries(t)
	channels := []struct {
		name  string
		write func(w http.ResponseWriter) bool
	}{
		{"project create and update (mapSpaceWriteErr)", func(w http.ResponseWriter) bool {
			return mapSpaceWriteErr(w, err, "")
		}},
		{"users (mapStoreErr)", func(w http.ResponseWriter) bool {
			return (&UsersHandler{}).mapStoreErr(w, err)
		}},
	}
	for _, c := range channels {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !c.write(rec) {
				t.Fatalf("the error is not mapped — it would be a 500")
			}
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status %d, want 422", rec.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s: %v", rec.Body.String(), err)
			}
			if body["error"] != "concurrent_change" {
				t.Errorf("error = %v, want concurrent_change (K-7)", body["error"])
			}
		})
	}
}
