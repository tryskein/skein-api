package migrations

// migration_000080_projects_space_not_null_test.go — sk-674.18 T024, the
// NOT NULL part of matrix row 42 (moved from T016 to T024 by decision,
// 2026-10-13). Reference — the condition on the move and
// the transition rule of 000071 (step 2), written before this migration:
//
//   - a project left without a space by the write path before T024 moves
//     into the common space with visibility members, its owner a participant
//     of the common space;
//   - after the migration a project without a space is refused by the
//     database (not_null_violation).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/testsupport"
)

const (
	m80Before = 79
	m80After  = 80
)

func TestMigration000080_ProjectsSpaceNotNull(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := m59Migrator(t) // returns the suite to head on cleanup
	m59Up(t, m, "head")
	if err := m.Migrate(m80Before); err != nil {
		t.Fatalf("Migrate(%d): %v", m80Before, err)
	}

	// An active admin, in case the database has no common space yet: the
	// rule makes the earliest active admin its owner.
	testsupport.SeedUser(t, pool, "admin")
	owner, _ := testsupport.SeedUser(t, pool, "user")
	project := uuid.New()
	prefix := "m80" + strings.ReplaceAll(project.String()[:8], "-", "")
	m59Exec(t, pool, "seed a project without a space", `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      created_by, created_at, updated_at, metadata, visibility)
VALUES ($1, $2::text, $2::text, 'counter', '', '[]'::jsonb, $3, now(), now(), '{}'::jsonb, 'space')`,
		project, prefix, owner)
	m59Exec(t, pool, "seed the owner member", `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by) VALUES ($1, $2, 'owner', now(), $2)`,
		project, owner)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM project_members WHERE project_id = $1`,
			`DELETE FROM projects WHERE id = $1`,
		} {
			if _, err := pool.Exec(context.Background(), q, project); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	if err := m.Migrate(m80After); err != nil {
		t.Fatalf("Migrate(%d): %v", m80After, err)
	}

	t.Run("a project without a space moves into the common space as members", func(t *testing.T) {
		var kind, visibility string
		var ownerIn bool
		if err := pool.QueryRow(ctx, `
SELECT s.kind, p.visibility,
       EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = s.id AND sm.user_id = $2)
  FROM projects p JOIN spaces s ON s.id = p.space_id
 WHERE p.id = $1`, project, owner).Scan(&kind, &visibility, &ownerIn); err != nil {
			t.Fatalf("reading the moved project: %v", err)
		}
		if kind != "common" || visibility != "members" {
			t.Errorf("moved into a %q space with visibility %q, want common / members", kind, visibility)
		}
		if !ownerIn {
			t.Errorf("the project owner is not a participant of the common space")
		}
	})

	t.Run("a project without a space is refused", func(t *testing.T) {
		id := uuid.New()
		p := "m80" + strings.ReplaceAll(id.String()[:8], "-", "")
		_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      created_by, created_at, updated_at, metadata)
VALUES ($1, $2::text, $2::text, 'counter', '', '[]'::jsonb, $3, now(), now(), '{}'::jsonb)`, id, p, owner)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
			if err == nil {
				_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
			}
			t.Errorf("insert without space_id = %v, want not_null_violation (23502)", err)
		}
	})
}

// TestMigration000080_ExistingCommonKeepsItsExclusions — found in review.
// Reference — the transition rule of 000071: «all non-external
// users are participants» describes the common space this rule CREATES. An
// existing common space has its own membership, decided since (an admin
// unticked a user, T037; an account created with spaceIds: []); moving a
// project into it adds the people of that project — they had its access,
// and INV-5 wants a project member to be a participant — and nobody else.
func TestMigration000080_ExistingCommonKeepsItsExclusions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := m59Migrator(t)
	m59Up(t, m, "head")
	if err := m.Migrate(m80Before); err != nil {
		t.Fatalf("Migrate(%d): %v", m80Before, err)
	}

	admin, _ := testsupport.SeedUser(t, pool, "admin")
	var common uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM spaces WHERE kind = 'common'`).Scan(&common); err != nil {
		// No common space at this version: one exists before 000080 in the
		// scenario under test.
		short := testsupport.NewShortID(t)
		if err := pool.QueryRow(ctx, `INSERT INTO spaces (short_id, kind, owner_id, name)
VALUES ($1, 'common', $2, 'Common') RETURNING id`, short, admin).Scan(&common); err != nil {
			t.Fatalf("seeding the common space: %v", err)
		}
		m59Exec(t, pool, "the owner row", `INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'owner')`, common, admin)
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM space_members WHERE space_id = $1`, common)
			_, _ = pool.Exec(c, `DELETE FROM spaces WHERE id = $1`, common)
		})
	}

	excluded, _ := testsupport.SeedUser(t, pool, "user") // not a participant of the common space
	owner, _ := testsupport.SeedUser(t, pool, "user")
	member, _ := testsupport.SeedUser(t, pool, "user")
	project := uuid.New()
	prefix := "m8x" + strings.ReplaceAll(project.String()[:8], "-", "")
	m59Exec(t, pool, "seed a project without a space", `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      created_by, created_at, updated_at, metadata)
VALUES ($1, $2::text, $2::text, 'counter', '', '[]'::jsonb, $3, now(), now(), '{}'::jsonb)`,
		project, prefix, owner)
	m59Exec(t, pool, "seed the members", `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2), ($1, $3, 'member', now(), $2)`, project, owner, member)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM project_members WHERE project_id = $1`, project)
		_, _ = pool.Exec(c, `DELETE FROM projects WHERE id = $1`, project)
		_, _ = pool.Exec(c, `DELETE FROM space_members WHERE user_id IN ($1, $2, $3) AND space_id = $4`, excluded, owner, member, common)
	})

	if err := m.Migrate(m80After); err != nil {
		t.Fatalf("Migrate(%d): %v", m80After, err)
	}
	in := func(u uuid.UUID) bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`, common, u).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !in(owner) || !in(member) {
		t.Errorf("the moved project's people are not participants: owner %v, member %v", in(owner), in(member))
	}
	if in(excluded) {
		t.Error("a user outside the existing common space was added to it by the move")
	}
}
