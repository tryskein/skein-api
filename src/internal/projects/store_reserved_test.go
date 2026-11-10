package projects_test

// store_reserved_test.go — row 81 (sk-878.3.1, «Addressing»): a prefix and an
// alias name a project in the same place of a public API path (UUID →
// prefix → alias) and a static segment wins over both, so a NEW prefix or
// alias equal to a reserved word (resolve) or to another project's alias or
// prefix is refused on every path that creates one — the create of a
// project, its composite patch (MCP skein_project_update, REST
// updateProject) and the single alias insert of the UI. A value that
// already exists is left alone, and its project stays reachable by UUID.
// The store is the one writer of all entries, so the refusal is asserted
// here; the codes each entry answers are asserted by the parity tests of
// the entries.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

func reservedCreate(t *testing.T, pool *pgxpool.Pool, store *projects.Store, owner, space uuid.UUID, name, prefix string, aliases ...string) (*projects.ProjectDTO, error) {
	t.Helper()
	p, err := store.CreateProject(context.Background(), projects.CreateProjectArgs{
		Name: name, IDPrefix: prefix, DefaultIDMode: "counter",
		CreatorID: owner, CreatorRole: "user", SpaceID: space, Visibility: "members",
		Aliases: aliases, AliasAddedByKind: "agent",
	})
	if err == nil {
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM events WHERE project_id = $1`, p.ID)
			_, _ = pool.Exec(c, `DELETE FROM projects WHERE id = $1`, p.ID)
		})
	}
	return p, err
}

func wantAliasTaken(t *testing.T, what string, err error) {
	t.Helper()
	var taken *projects.ErrAliasTaken
	if !errors.As(err, &taken) {
		t.Errorf("%s: %v, want the alias refused as taken", what, err)
	}
}

func wantCreateAliasRefused(t *testing.T, what string, err error) {
	t.Helper()
	var conflict *projects.ErrConflict
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Constraint, "project_aliases") {
		t.Errorf("%s: %v, want the unique refusal of project_aliases", what, err)
	}
}

func aliasCount(t *testing.T, pool *pgxpool.Pool, alias string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM project_aliases WHERE alias = $1`, alias).Scan(&n); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	return n
}

// TestStore_NewHandleRules — row 81. Red when a collision with a reserved
// word or with the other kind of handle is let through.
func TestStore_NewHandleRules(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := projects.NewStore(pool)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	space := testsupport.SeedSpace(t, pool, owner)
	sfx := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	actor := projects.HumanActor(owner, "owner")

	held, err := reservedCreate(t, pool, store, owner, space, "held "+sfx, "hd"+sfx, "ha-"+sfx)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	target, err := reservedCreate(t, pool, store, owner, space, "target "+sfx, "tg"+sfx)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	t.Run("a new prefix may not repeat an alias or a reserved word", func(t *testing.T) {
		for _, prefix := range []string{"ha-" + sfx, "resolve"} {
			if _, err := reservedCreate(t, pool, store, owner, space, "p "+prefix, prefix); !errors.Is(err, projects.ErrPrefixTaken) {
				t.Errorf("create with prefix %q: %v, want ErrPrefixTaken", prefix, err)
			}
		}
		taken, err := store.PrefixTaken(ctx, "ha-"+sfx)
		if err != nil || !taken {
			t.Errorf("PrefixTaken of another project's alias: %v, %v; want taken", taken, err)
		}
		taken, err = store.PrefixTaken(ctx, "resolve")
		if err != nil || !taken {
			t.Errorf("PrefixTaken of the reserved word: %v, %v; want taken", taken, err)
		}
	})

	t.Run("a new alias may not repeat another prefix or a reserved word", func(t *testing.T) {
		for _, alias := range []string{"hd" + sfx, "resolve"} {
			_, err := reservedCreate(t, pool, store, owner, space, "a "+alias, "ac"+sfx+strings.ReplaceAll(alias, "-", "")[:2], alias)
			wantCreateAliasRefused(t, "create with alias "+alias, err)

			_, err = store.PatchProject(ctx, target.ID, projects.PatchProjectArgs{AliasesAdd: []string{alias}}, owner, "user", actor)
			wantAliasTaken(t, "patch adding alias "+alias, err)

			_, err = store.AddAlias(ctx, target.ID, owner, alias, actor)
			wantAliasTaken(t, "add alias "+alias, err)
		}
		if n := aliasCount(t, pool, "hd"+sfx); n != 0 {
			t.Errorf("%d aliases equal to another project's prefix were written", n)
		}
	})

	t.Run("a project's own prefix may be its alias", func(t *testing.T) {
		if _, err := store.PatchProject(ctx, target.ID, projects.PatchProjectArgs{AliasesAdd: []string{"tg" + sfx}}, owner, "user", actor); err != nil {
			t.Errorf("patch adding the project's own prefix as its alias: %v", err)
		}
	})

	t.Run("an existing value is left alone and reached by UUID", func(t *testing.T) {
		// Written before the rule: an alias equal to the reserved word.
		if _, err := pool.Exec(ctx, `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind) VALUES ('resolve', $1, $2, 'human')`,
			held.ID, owner); err != nil {
			t.Fatalf("seed an existing reserved alias: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM project_aliases WHERE alias = 'resolve'`) })

		// A patch of anything else keeps it.
		desc := "still here"
		if _, err := store.PatchProject(ctx, held.ID, projects.PatchProjectArgs{Description: &desc}, owner, "user", actor); err != nil {
			t.Fatalf("patch the description: %v", err)
		}
		if n := aliasCount(t, pool, "resolve"); n != 1 {
			t.Fatalf("the existing reserved alias is gone (%d rows)", n)
		}
		id, err := store.ResolvePathHandle(ctx, held.ID.String(), owner)
		if err != nil || id != held.ID {
			t.Errorf("the project of an existing reserved alias by UUID: %s, %v", id, err)
		}
		// And it can still be removed.
		if _, err := store.PatchProject(ctx, held.ID, projects.PatchProjectArgs{AliasesRemove: []string{"resolve"}}, owner, "user", actor); err != nil {
			t.Errorf("remove the existing reserved alias: %v", err)
		}
	})
}
