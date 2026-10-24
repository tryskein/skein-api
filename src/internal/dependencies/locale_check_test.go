package dependencies

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestClusterReadsUnicodeLetters_CorrectLocale pins the "everything is
// fine" side of sk-303.12's acceptance: on a cluster whose ctype classifies
// non-ASCII letters (the harness's own TEST_DATABASE_URL — see
// docker-compose.test.yml / infra/postgres, no locale override, so it
// takes the base image's default, which the existing sk-228 duplicate-
// detection integration tests already depend on being unicode-aware),
// ClusterReadsUnicodeLetters must report true and LocaleWarning must
// report "" (nothing to warn about).
func TestClusterReadsUnicodeLetters_CorrectLocale(t *testing.T) {
	pool := newTestPool(t)

	ok, err := ClusterReadsUnicodeLetters(context.Background(), pool)
	if err != nil {
		t.Fatalf("ClusterReadsUnicodeLetters: %v", err)
	}
	if !ok {
		t.Fatal("ClusterReadsUnicodeLetters = false on the test cluster — " +
			"either the harness's Postgres image regressed to a C-flavoured " +
			"locale, or the probe is broken; either way every duplicate-check " +
			"integration test elsewhere in this package would also be silently " +
			"degraded, which they are not observed to be")
	}

	msg, err := LocaleWarning(context.Background(), pool)
	if err != nil {
		t.Fatalf("LocaleWarning: %v", err)
	}
	if msg != "" {
		t.Fatalf("LocaleWarning on a correctly-configured cluster = %q, want \"\"", msg)
	}
}

// TestClusterReadsUnicodeLetters_CLocale pins the "warn, don't crash" side
// of sk-303.12's acceptance against a REAL C-locale database — not a
// mocked SQL parameter (that style is already exercised by
// TestTrigramDuplicates_CtypeGate and friends in duplicates_test.go,
// which pin trigramCandidateSQL's $6 to false to reproduce the ctype=C
// WHERE-clause branch on a unicode-aware CI database). This test instead
// creates a SIBLING database in the same cluster with LC_CTYPE='C' and
// connects to IT, so ClusterReadsUnicodeLetters runs its real query
// against a real ctype=C `pg_database` row — the same probe main.go runs
// at startup, unmocked end to end.
//
// Requires the TEST_DATABASE_URL role to have CREATEDB. The harness's
// POSTGRES_USER (docker-compose.test.yml) is the cluster's initdb
// bootstrap role, which Postgres always creates as a superuser —
// superuser implies CREATEDB — so this is expected to hold in the
// canonical harness. If it does not (a deployment wired a least-privilege
// TEST_DATABASE_URL role), the test skips LOUDLY with the exact error
// rather than reporting a false green: sk-303.12's acceptance criterion is
// "tested against a real C-locale database", and a skip must say so, not
// hide behind a passing assertion that never ran.
func TestClusterReadsUnicodeLetters_CLocale(t *testing.T) {
	adminPool := newTestPool(t)

	dbName := "sk303_12_clocale_" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String(), "-", "")[:16]

	createCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// CREATE DATABASE cannot run inside a transaction block; pgxpool.Exec
	// on a single statement is autocommit, so a plain Exec is correct here.
	//
	// LOCALE_PROVIDER libc is explicit and deliberate, not incidental: the
	// admin connection's own cluster now defaults to LOCALE_PROVIDER
	// builtin (docker-compose.test.yml / DEPLOYMENT-NOTES.md §1, sk-303.12
	// wiring), and CREATE DATABASE's provider defaults from the CLUSTER,
	// not from the literal string 'C' — an unqualified `LC_COLLATE 'C'
	// LC_CTYPE 'C'` on a builtin-default cluster creates a BUILTIN 'C'
	// locale, not a libc one, and builtin 'C.UTF-8'-adjacent handling is
	// not what this test needs to reproduce. `libc` C/POSIX is also the
	// more representative real-world failure: it is what an unconfigured
	// `initdb` falls back to on the base images DEPLOYMENT-NOTES.md names
	// (Alpine-flavoured Postgres), not the opt-in `builtin` provider.
	_, err := adminPool.Exec(createCtx,
		`CREATE DATABASE `+dbName+` TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LC_COLLATE 'C' LC_CTYPE 'C'`)
	if err != nil {
		if strings.Contains(err.Error(), "permission denied") {
			t.Skipf("TEST_DATABASE_URL role lacks CREATEDB, cannot create a real "+
				"C-locale sibling database to test against — sk-303.12's "+
				"ClusterReadsUnicodeLetters classification function itself is "+
				"still covered by TestClusterReadsUnicodeLetters_CorrectLocale "+
				"and by measureLegibility's use of the identical SQL constant in "+
				"duplicates_test.go, but the ctype=C path specifically is NOT "+
				"exercised in this run. Original error: %v", err)
		}
		t.Fatalf("CREATE DATABASE %s (LC_CTYPE=C): %v", dbName, err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// WITH (FORCE) (PG13+; this harness runs pg18) terminates any
		// lingering connections to dbName instead of racing this cleanup
		// against cPool.Close()'s own connection teardown below.
		if _, err := adminPool.Exec(dropCtx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`); err != nil {
			t.Errorf("cleanup: DROP DATABASE %s: %v", dbName, err)
		}
	})

	cURL := swapDatabaseName(t, cLocaleTestURL(t), dbName)
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 10*time.Second)
	cPool, err := pgxpool.New(connectCtx, cURL)
	cancelConnect()
	if err != nil {
		t.Fatalf("connect to C-locale database %s: %v", dbName, err)
	}
	defer cPool.Close()
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
	if err := cPool.Ping(pingCtx); err != nil {
		cancelPing()
		t.Fatalf("ping C-locale database %s: %v", dbName, err)
	}
	cancelPing()

	// pg_trgm is a per-database extension (CREATE EXTENSION), not
	// cluster-wide — the fresh database does not have it just because
	// `skein` does. ClusterReadsUnicodeLetters's SQL calls show_trgm(),
	// so the extension has to exist here for the probe to run at all.
	extCtx, cancelExt := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := cPool.Exec(extCtx, `CREATE EXTENSION IF NOT EXISTS pg_trgm`); err != nil {
		cancelExt()
		t.Fatalf("CREATE EXTENSION pg_trgm on %s: %v", dbName, err)
	}
	cancelExt()

	// Sanity check the fixture itself before trusting the assertions
	// below: confirm this database really did land on ctype=C, the same
	// way DEPLOYMENT-NOTES.md's "how to check an existing install" query
	// does. A silently-ignored LC_CTYPE would make this test tautological.
	var datctype string
	sanityCtx, cancelSanity := context.WithTimeout(context.Background(), 10*time.Second)
	if err := cPool.QueryRow(sanityCtx,
		`SELECT datctype FROM pg_database WHERE datname = current_database()`).
		Scan(&datctype); err != nil {
		cancelSanity()
		t.Fatalf("reading datctype of fixture database: %v", err)
	}
	cancelSanity()
	if datctype != "C" {
		t.Fatalf("fixture database datctype = %q, want \"C\" — CREATE DATABASE's "+
			"LC_CTYPE='C' did not take, so this test would not be testing what it "+
			"claims to", datctype)
	}

	ok, err := ClusterReadsUnicodeLetters(context.Background(), cPool)
	if err != nil {
		t.Fatalf("ClusterReadsUnicodeLetters on C-locale database: %v", err)
	}
	if ok {
		t.Fatal("ClusterReadsUnicodeLetters = true on a confirmed ctype=C database — " +
			"the probe (duplicateCtypeProbe / trigramLegibilitySQL) is not detecting " +
			"the condition it exists to detect")
	}

	msg, err := LocaleWarning(context.Background(), cPool)
	if err != nil {
		t.Fatalf("LocaleWarning on C-locale database: %v", err)
	}
	if msg == "" {
		t.Fatal("LocaleWarning on a confirmed ctype=C database = \"\", want a non-empty warning")
	}
	for _, want := range []string{"pg_trgm", "non-ASCII", "DEPLOYMENT-NOTES.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("LocaleWarning message %q does not mention %q — the warning must "+
				"name the CONSEQUENCE, not just the fact of degradation", msg, want)
		}
	}
}

// cLocaleTestURL returns TEST_DATABASE_URL, failing the test (not
// skipping — newTestPool already skipped if it were unset) if it is
// somehow empty at this point.
func cLocaleTestURL(t *testing.T) string {
	t.Helper()
	// newTestPool already validated TEST_DATABASE_URL is set and reachable;
	// re-read it here rather than threading it through as a return value,
	// so this helper reads the same way at every call site.
	raw := os.Getenv(testDatabaseURL)
	if raw == "" {
		t.Fatal("TEST_DATABASE_URL unset after newTestPool succeeded — impossible")
	}
	return raw
}

// swapDatabaseName rewrites a postgres:// URL's path (database name) to
// dbName, keeping host/port/credentials/query-string untouched.
func swapDatabaseName(t *testing.T, rawURL, dbName string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + dbName
	return u.String()
}
