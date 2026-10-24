package migrations

// migration_000069_spaces_schema_test.go — sk-674.18 T015, matrix rows 35
// and 36. Reference — the spec, written before the migration:
//
//   - row 35 (T015, INV-6): the spaces schema goes down and up again on a
//     seeded database without losing the seed; a membership row without a
//     source (via_manual = false ∧ via_space = false) is refused by the
//     database — "a row without sources does not exist";
//   - row 36 (INV-16): one pending invitation per (space, email regardless
//     of case) — a second pending one is refused by the database, a case
//     variant of the email cannot stand next to it either; a pending one
//     next to accepted / revoked ones of the same pair is allowed (the
//     uniqueness is partial).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/testsupport"
)

const m69Before = 68 // the version before the spaces schema

func TestMigration000069_SpacesSchema(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")

	owner, _ := testsupport.SeedUser(t, pool, "user")
	member, _ := testsupport.SeedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, owner)
	project, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "m69")
	testsupport.AddProjectMember(t, pool, project, member, "member")

	t.Run("row 35: a membership row without a source is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
UPDATE project_members SET via_manual = false, via_space = false WHERE project_id = $1 AND user_id = $2`, project, member)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "project_members_has_source" {
			t.Errorf("a row without a source: err %v, want check violation of project_members_has_source", err)
		}
	})

	t.Run("row 36: one pending invitation per space and email", func(t *testing.T) {
		invite := func(email, status string) error {
			var resolved *time.Time
			if status != "pending" {
				now := time.Now().UTC()
				resolved = &now
			}
			_, err := pool.Exec(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, status, lang, expires_at, resolved_at)
VALUES ($1, $2, $3, $4, 'ru', now() + interval '7 days', $5)`, space, email, owner, status, resolved)
			return err
		}
		email := "inv-" + uuid.NewString()[:8] + "@m69.test"
		for _, st := range []string{"accepted", "revoked"} {
			if err := invite(email, st); err != nil {
				t.Fatalf("an %s invitation: %v", st, err)
			}
		}
		if err := invite(email, "pending"); err != nil {
			t.Fatalf("a pending invitation next to accepted and revoked ones: %v — the uniqueness must be partial", err)
		}
		var pgErr *pgconn.PgError
		if err := invite(email, "pending"); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Errorf("a second pending invitation: err %v, want a unique violation", err)
		}
		// A case variant cannot stand next to the pending one: the email is
		// stored normalised (CHECK) and the index is on lower(email).
		upper := "INV-" + email[4:]
		if err := invite(upper, "pending"); !errors.As(err, &pgErr) || (pgErr.Code != "23505" && pgErr.Code != "23514") {
			t.Errorf("a pending invitation for %q next to %q: err %v, want it refused", upper, email, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM space_invitations WHERE space_id = $1`, space); err != nil {
			t.Errorf("cleanup invitations: %v", err)
		}
	})

	t.Run("row 35: down and up again on the seeded database", func(t *testing.T) {
		if err := m.Migrate(m69Before); err != nil {
			t.Fatalf("Migrate(%d): %v", m69Before, err)
		}
		if n := m59Count(t, pool, "seed below the spaces schema",
			`SELECT count(*) FROM project_members WHERE project_id = $1`, project); n != 2 {
			t.Errorf("below the spaces schema the project has %d members, want 2 (owner, member)", n)
		}
		m59Up(t, m, "up again")
		if n := m59Count(t, pool, "seed after up",
			`SELECT count(*) FROM project_members WHERE project_id = $1 AND (via_manual OR via_space)`, project); n != 2 {
			t.Errorf("after up again the project has %d members with a source, want 2", n)
		}
	})
}
