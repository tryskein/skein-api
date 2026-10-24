package migrations

// migration_000082_themes_author_not_null_test.go — sk-674.18 T035, the
// NOT NULL part of matrix row 42 for themes (moved from T016 to T035 by the
// lead's decision, sk-674.18). Reference — the lead's condition on the move
// (author_id = created_by before NOT NULL) and the transition rules of
// 000071 step 5 (US-21), written before this migration:
//
//   - a theme without an author gets its creator as the author; a name the
//     author already has (case-insensitive) gets the first free suffix
//     « 2», « 3», …, id and values unchanged;
//   - whoever selected another author's theme gets a personal copy with the
//     same name and values, and the selection moves to the copy;
//   - after the migration a theme without an author is refused by the
//     database (not_null_violation).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/testsupport"
)

const (
	m82Before = 81
	m82After  = 82
)

func TestMigration000082_ThemesAuthorNotNull(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	if err := m.Migrate(m82Before); err != nil {
		t.Fatalf("Migrate(%d): %v", m82Before, err)
	}

	a, _ := testsupport.SeedUser(t, pool, "user")
	b, _ := testsupport.SeedUser(t, pool, "user")
	c, _ := testsupport.SeedUser(t, pool, "user")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `UPDATE users SET custom_theme_id = NULL WHERE id = ANY($1)`, []uuid.UUID{a, b, c})
		if _, err := pool.Exec(ctx, `DELETE FROM themes WHERE created_by = ANY($1)`, []uuid.UUID{a, b, c}); err != nil {
			t.Errorf("cleanup themes: %v", err)
		}
	})
	insert := func(what, name string, creator uuid.UUID, author *uuid.UUID) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO themes (name, base, variables, created_by, updated_by, author_id)
VALUES ($1, 'dark', '{"--bg":"#000000"}'::jsonb, $2, $2, $3) RETURNING id`, name, creator, author).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
		return id
	}
	insert("A's authored theme", "Ocean", a, &a)
	sameName := insert("A's theme without an author, same name", "ocean", a, nil)
	foreign := insert("B's theme without an author", "Sunset", b, nil)
	m59Exec(t, pool, "C selects B's theme", `
UPDATE users SET theme_mode = 'custom', custom_theme_id = $2 WHERE id = $1`, c, foreign)

	if err := m.Migrate(m82After); err != nil {
		t.Fatalf("Migrate(%d): %v", m82After, err)
	}

	t.Run("a theme without an author gets its creator, a taken name a suffix", func(t *testing.T) {
		var author uuid.UUID
		var name string
		if err := pool.QueryRow(ctx, `SELECT author_id, name FROM themes WHERE id = $1`, sameName).Scan(&author, &name); err != nil {
			t.Fatalf("reading the theme: %v", err)
		}
		if author != a || name != "ocean 2" {
			t.Errorf("theme without an author: author %s name %q, want %s / %q", author, name, a, "ocean 2")
		}
		if err := pool.QueryRow(ctx, `SELECT author_id, name FROM themes WHERE id = $1`, foreign).Scan(&author, &name); err != nil {
			t.Fatalf("reading B's theme: %v", err)
		}
		if author != b || name != "Sunset" {
			t.Errorf("B's theme: author %s name %q, want %s / %q", author, name, b, "Sunset")
		}
	})

	t.Run("a selected foreign theme becomes a personal copy", func(t *testing.T) {
		var copyID, author uuid.UUID
		var name, base, vars string
		if err := pool.QueryRow(ctx, `
SELECT t.id, t.author_id, t.name, t.base, t.variables::text
  FROM users u JOIN themes t ON t.id = u.custom_theme_id
 WHERE u.id = $1`, c).Scan(&copyID, &author, &name, &base, &vars); err != nil {
			t.Fatalf("reading C's selection: %v", err)
		}
		if copyID == foreign || author != c {
			t.Errorf("C still selects B's theme (copy %s, author %s)", copyID, author)
		}
		if name != "Sunset" || base != "dark" || vars != `{"--bg": "#000000"}` {
			t.Errorf("copy: name %q base %q variables %s, want Sunset / dark / the original values", name, base, vars)
		}
	})

	t.Run("a theme without an author is refused", func(t *testing.T) {
		var id uuid.UUID
		err := pool.QueryRow(ctx, `
INSERT INTO themes (name, base, variables, created_by, updated_by)
VALUES ('m82 orphan', 'light', '{}'::jsonb, $1, $1) RETURNING id`, a).Scan(&id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
			t.Errorf("insert without author_id = %v, want not_null_violation (23502)", err)
		}
	})
}
