package publicapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/errcode"
	"skein-api/internal/publicapi"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestOperationDeadline_RollsBackTheTransaction — row 57 (OST-2-6). The
// context of an operation carries the deadline of SKEIN_PUBLIC_API_WRITE_TIMEOUT
// (the router's OperationTimeout): a test route that inserts a user and then
// keeps its transaction open past the deadline leaves no row — the deadline
// reached the database and the transaction was rolled back. The same route
// without the wait commits its row, so the absence is the deadline's doing.
// What is asserted is the absence of the effect, not a duration.
func TestOperationDeadline_RollsBackTheTransaction(t *testing.T) {
	pool := newTestPool(t)

	// route inserts a user with id in one transaction; with hold it then
	// waits in the database longer than any deadline below before
	// committing.
	route := func(id uuid.UUID, hold bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			tx, err := pool.Begin(ctx)
			if err != nil {
				publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.InternalError, Origin: publicapi.OriginDomain})
				return
			}
			defer func() {
				if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Errorf("rollback: %v", err)
				}
			}()
			uname := "dl" + strings.ReplaceAll(id.String()[:18], "-", "")
			if _, err := tx.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $3, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
				id, uname+"@test.local", uname); err != nil {
				publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.InternalError, Origin: publicapi.OriginDomain})
				return
			}
			if hold {
				if _, err := tx.Exec(ctx, `SELECT pg_sleep(30)`); err != nil {
					publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.InternalError, Origin: publicapi.OriginDomain})
					return
				}
			}
			if err := tx.Commit(ctx); err != nil {
				publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.InternalError, Origin: publicapi.OriginDomain})
				return
			}
			w.WriteHeader(http.StatusCreated)
		})
	}

	userExists := func(id uuid.UUID) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count users: %v", err)
		}
		return n == 1
	}

	serve := func(id uuid.UUID, hold bool) int {
		h := publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 500 * time.Millisecond}, []publicapi.Route{{
			OperationID: "testInsertUser", Method: http.MethodPost, Path: "/api/v1/users-under-deadline",
			Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightWrite},
			Handler: route(id, hold),
		}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/users-under-deadline", nil))
		return rec.Code
	}

	t.Run("past the deadline: nothing written", func(t *testing.T) {
		id := uuid.New()
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
		if status := serve(id, true); status == http.StatusCreated {
			t.Fatalf("status %d: the operation committed past its deadline", status)
		}
		if userExists(id) {
			t.Error("the row written before the deadline survived it: the transaction was not rolled back")
		}
	})

	t.Run("within the deadline: written", func(t *testing.T) {
		id := uuid.New()
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
		if status := serve(id, false); status != http.StatusCreated {
			t.Fatalf("status %d, want 201", status)
		}
		if !userExists(id) {
			t.Error("the row of an operation that finished within its deadline is missing")
		}
	})
}
