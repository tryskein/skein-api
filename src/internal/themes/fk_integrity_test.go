package themes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/testsupport"
)

// sk-105.5 (S4) — themes authorship FK integrity (migration 000023).
//
// 000023 declares two distinct FK behaviors that the schema comment calls
// out explicitly:
//
//   themes.created_by / updated_by → users(id)  ON DELETE RESTRICT
//       "we never silently drop authored content when a user is removed"
//   users.custom_theme_id          → themes(id) ON DELETE SET NULL
//       "deleting a saved theme cleanly degrades any user pointing at it
//        back to the builtin default"
//
// These tests assert BOTH arms against the live schema (read from the
// migration, not assumed). RESTRICT manifests as SQLSTATE 23503
// (foreign_key_violation) on the author DELETE; SET NULL manifests as the
// referencing user's custom_theme_id going NULL after the theme DELETE.

// pgRestrictViolation (23001) is what Postgres raises when an explicit
// ON DELETE RESTRICT edge is tripped — distinct from 23503
// (foreign_key_violation), which a NO ACTION / deferred check raises.
// themes.created_by is declared ON DELETE RESTRICT (000023), so the
// blocked author-delete surfaces as 23001. We accept either code so the
// test pins the BEHAVIOR (delete blocked) rather than the exact SQLSTATE
// flavor, but 23001 is the one actually observed.
const (
	pgRestrictViolation   = "23001"
	pgForeignKeyViolation = "23503"
)

// TestThemes_DeleteAuthor_RestrictBlocks asserts the ON DELETE RESTRICT on
// themes.created_by: while a theme authored by a user exists, deleting
// that user is rejected with a foreign_key_violation. The author is NOT
// reaped, so the authored theme is never silently orphaned.
func TestThemes_DeleteAuthor_RestrictBlocks(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	author, _ := testsupport.SeedUser(t, pool, "user")
	themeID := testsupport.SeedTheme(t, pool, author)

	// The author's t.Cleanup DELETE would itself trip the RESTRICT, so
	// register a theme-delete BEFORE that cleanup runs (LIFO order: this
	// registers last, runs first) to unblock the author cleanup.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM themes WHERE id = $1`, themeID)
	})

	_, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, author)
	if err == nil {
		t.Fatalf("delete author %s succeeded; want RESTRICT foreign_key_violation (000023 themes.created_by)", author)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != pgRestrictViolation && pgErr.Code != pgForeignKeyViolation) {
		t.Fatalf("delete author error = %v, want SQLSTATE %s (restrict_violation) or %s (foreign_key_violation)", err, pgRestrictViolation, pgForeignKeyViolation)
	}

	// The author row must still be present — RESTRICT blocked the delete.
	if got := testsupport.CountRows(t, pool, "users", "id", author); got != 1 {
		t.Fatalf("author rows after blocked delete = %d, want 1 (delete must have rolled back)", got)
	}
}

// TestThemes_DeleteTheme_SetsNullOnUserPointer asserts the ON DELETE SET
// NULL on users.custom_theme_id: a user whose active custom theme is the
// deleted theme has their custom_theme_id cleared (degraded to builtin)
// rather than blocking the theme delete.
func TestThemes_DeleteTheme_SetsNullOnUserPointer(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	author, _ := testsupport.SeedUser(t, pool, "user")
	themeID := testsupport.SeedTheme(t, pool, author)

	// Point the author's active preference at the theme (theme_mode=custom
	// + custom_theme_id). This is the row the SET NULL must degrade.
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET theme_mode = 'custom', custom_theme_id = $2 WHERE id = $1`,
		author, themeID); err != nil {
		t.Fatalf("point user at theme: %v", err)
	}

	// Sanity: the pointer is set before the delete.
	var before *string
	if err := pool.QueryRow(context.Background(),
		`SELECT custom_theme_id::text FROM users WHERE id = $1`, author).Scan(&before); err != nil {
		t.Fatalf("read custom_theme_id pre-delete: %v", err)
	}
	if before == nil {
		t.Fatalf("custom_theme_id is NULL pre-delete; test wiring did not point the user at the theme")
	}

	// Delete the theme — must succeed (SET NULL, not RESTRICT) and clear
	// the pointer.
	tag, err := pool.Exec(context.Background(), `DELETE FROM themes WHERE id = $1`, themeID)
	if err != nil {
		t.Fatalf("delete theme %s: %v (SET NULL must allow the delete)", themeID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete theme %s: %d rows affected, want 1", themeID, tag.RowsAffected())
	}

	var after *string
	if err := pool.QueryRow(context.Background(),
		`SELECT custom_theme_id::text FROM users WHERE id = $1`, author).Scan(&after); err != nil {
		t.Fatalf("read custom_theme_id post-delete: %v", err)
	}
	if after != nil {
		t.Fatalf("custom_theme_id = %q post-delete, want NULL (000023 SET NULL must degrade the pointer)", *after)
	}
}
