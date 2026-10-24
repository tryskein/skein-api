package projects

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"skein-api/internal/testsupport"
)

// TestReadJITOff — the statements of the project list run with JIT off
// inside their own read-only transaction, and only there (sk-674.18 T009:
// off for the whole pool, JIT's absence slowed other hot paths). Observed
// on the live connection of the transaction; outside it the pool keeps the
// server's setting.
func TestReadJITOff(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	s := NewStore(pool)
	ctx := context.Background()

	var serverJIT string
	if err := pool.QueryRow(ctx, `SELECT reset_val FROM pg_settings WHERE name = 'jit'`).Scan(&serverJIT); err != nil {
		t.Fatalf("reading the server's jit: %v", err)
	}

	var jit, readOnly string
	if err := s.readJITOff(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SHOW jit`).Scan(&jit); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly)
	}); err != nil {
		t.Fatalf("readJITOff: %v", err)
	}
	if jit != "off" || readOnly != "on" {
		t.Errorf("inside the list transaction: jit = %q, read only = %q; want off, on", jit, readOnly)
	}

	var after string
	if err := pool.QueryRow(ctx, `SHOW jit`).Scan(&after); err != nil {
		t.Fatalf("SHOW jit: %v", err)
	}
	if after != serverJIT {
		t.Errorf("jit = %q on a pool connection after the list, want the server's %q", after, serverJIT)
	}
}
