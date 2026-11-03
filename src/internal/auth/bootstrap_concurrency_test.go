package auth

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"skein-api/internal/storage"
)

type bootstrapCountKey struct{}

// Observe the real database boundary: hold each first in-transaction count
// after both SERIALIZABLE snapshots have seen the empty users table.
type bootstrapSnapshotBarrier struct {
	once    sync.Once
	ready   chan<- struct{}
	release <-chan struct{}
}

func (b *bootstrapSnapshotBarrier) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, bootstrapCountKey{}, conn.PgConn().TxStatus() == 'T' && strings.Contains(data.SQL, "CountUsers"))
}

func (b *bootstrapSnapshotBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	count, _ := ctx.Value(bootstrapCountKey{}).(bool)
	if !count || data.Err != nil {
		return
	}
	b.once.Do(func() {
		b.ready <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
}

func TestBootstrap_ConcurrentReplicasCreateOneCompleteAdministrator(t *testing.T) {
	admin := newOIDCTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	database := "bootstrap_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER builtin BUILTIN_LOCALE 'C.UTF-8' LC_COLLATE 'C.UTF-8' LC_CTYPE 'C.UTF-8'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated bootstrap database: %v", err)
		}
	})
	databaseURL, err := url.Parse(os.Getenv(testDatabaseURL))
	if err != nil {
		t.Fatal(err)
	}
	databaseURL.Path = "/" + database
	cfg, err := pgxpool.ParseConfig(databaseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "CREATE EXTENSION pg_trgm; CREATE EXTENSION vector"); err != nil {
		t.Fatal(err)
	}
	if err := storage.RunMigrations(cfg.ConnString(), filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	results := make(chan error, 2)
	bootstrap := BootstrapConfig{
		AdminEmail: "admin@example.com", AdminPassword: "test-bootstrap-password",
		AdminUsername: "bootstrap-admin", BcryptCost: bcrypt.MinCost,
	}
	for range 2 {
		replicaCfg := cfg.Copy()
		replicaCfg.ConnConfig.Tracer = &bootstrapSnapshotBarrier{ready: ready, release: release}
		replica, err := pgxpool.NewWithConfig(ctx, replicaCfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(replica.Close)
		go func() { results <- Bootstrap(ctx, replica, bootstrap) }()
	}
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("both bootstrap transactions did not reach their empty snapshots")
		}
	}
	unblock()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("bootstrap replica failed: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("bootstrap replicas did not finish")
		}
	}
	var users, spaces int
	var role, hash string
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT role, password_hash FROM users WHERE email = $1", bootstrap.AdminEmail).Scan(&role, &hash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM spaces WHERE kind = 'personal'").Scan(&spaces); err != nil {
		t.Fatal(err)
	}
	if users != 1 || spaces != 1 || role != "admin" {
		t.Fatalf("bootstrap result: users=%d personal spaces=%d role=%q, want 1/1/admin", users, spaces, role)
	}
	if err := Verify(hash, bootstrap.AdminPassword); err != nil {
		t.Fatalf("administrator cannot authenticate: %v", err)
	}
	if err := Bootstrap(ctx, pool, bootstrap); err != nil {
		t.Fatalf("subsequent startup is not idempotent: %v", err)
	}
}
