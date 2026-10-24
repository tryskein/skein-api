package migrations

// migration_000072_spaces_constraints_test.go — sk-674.18 T016, matrix row
// 42 (the tightening after the transition). Reference — the spec, written
// before the migration:
//
//   - a project name is unique within a space, regardless of case, and only
//     there: same-named projects in two spaces are allowed, in one space the
//     second is refused; the global idx_projects_name_lower is gone;
//   - a theme name is unique among one author's themes only (US-21):
//     same-named themes of two authors are allowed, a second one of the same
//     author is refused;
//   - users_auth_method lets a deleted account have no way to sign in
//     (US-10: password and identities are erased).
//
// projects.space_id NOT NULL and themes.author_id NOT NULL are not this
// row any more: the lead's decision on the T016 fork (2026-10-13, recorded
// in sk-674.18) moved them to T024 and T035, each with its write path.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/testsupport"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func TestMigration000072_SpacesConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := m59Migrator(t)
	m59Up(t, m, "head")

	owner, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	spaceA := testsupport.SeedSpace(t, pool, owner)
	spaceB := testsupport.SeedSpace(t, pool, owner)

	t.Run("project names are unique per space, regardless of case", func(t *testing.T) {
		pA, _ := testsupport.SeedProjectInSpace(t, pool, owner, spaceA, "m72")
		var name string
		if err := pool.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, pA).Scan(&name); err != nil {
			t.Fatal(err)
		}
		insert := func(space uuid.UUID, projectName string) error {
			id := uuid.New()
			prefix := "m72" + strings.ReplaceAll(id.String()[:8], "-", "")
			_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos, created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, $4, now(), now(), '{}'::jsonb, $5)`, id, projectName, prefix, owner, space)
			if err == nil {
				t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id) })
			}
			return err
		}
		if err := insert(spaceB, name); err != nil {
			t.Errorf("the same name in another space: %v, want allowed", err)
		}
		if err := insert(spaceA, strings.ToUpper(name)); !isUniqueViolation(err) {
			t.Errorf("the same name (another case) in the same space: err %v, want a unique violation", err)
		}
		if n := m59Count(t, pool, "global index",
			`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_projects_name_lower'`); n != 0 {
			t.Error("the global idx_projects_name_lower is still there")
		}
	})

	t.Run("theme names are unique per author", func(t *testing.T) {
		name := "m72 theme " + uuid.NewString()[:8]
		theme := func(author uuid.UUID, themeName string) error {
			_, err := pool.Exec(ctx, `
INSERT INTO themes (name, base, variables, created_by, updated_by, author_id)
VALUES ($1, 'dark', '{}'::jsonb, $2, $2, $2)`, themeName, author)
			return err
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM themes WHERE author_id = ANY($1)`, []uuid.UUID{owner, other})
		})
		if err := theme(owner, name); err != nil {
			t.Fatalf("the first theme: %v", err)
		}
		if err := theme(other, name); err != nil {
			t.Errorf("the same name by another author: %v, want allowed", err)
		}
		if err := theme(owner, strings.ToUpper(name)); !isUniqueViolation(err) {
			t.Errorf("the same name (another case) by the same author: err %v, want a unique violation", err)
		}
	})

	t.Run("a deleted account may have no way to sign in", func(t *testing.T) {
		deleted, _ := testsupport.SeedUser(t, pool, "user")
		if _, err := pool.Exec(ctx, `
UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8),
                 password_hash = NULL, keycloak_sub = NULL
 WHERE id = $1`, deleted); err != nil {
			t.Errorf("a deleted account without password and identity: %v, want allowed", err)
		}
		live, _ := testsupport.SeedUser(t, pool, "user")
		var pgErr *pgconn.PgError
		_, err := pool.Exec(ctx, `UPDATE users SET password_hash = NULL, keycloak_sub = NULL WHERE id = $1`, live)
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "users_auth_method" {
			t.Errorf("a live account without a way to sign in: err %v, want users_auth_method", err)
		}
	})
}
