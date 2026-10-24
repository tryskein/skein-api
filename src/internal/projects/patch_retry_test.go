package projects

// patch_retry_test.go — sk-674.18. Reference —
// the contract of PatchProject (patch_composite.go): it answers «the set of
// effects that materialised» — the repos, aliases and archive flip that are
// in the database after the call. Since T024 the patch runs under
// spaces.WithRetry: an attempt that loses a serialization race is rolled
// back and run again, and only the committed attempt materialised.
//
// The race is staged at the database boundary: a trigger of this test on
// events fails the FIRST project.repo_added event of this project with
// SQLSTATE 40001 — after the attempt has already added the alias and the
// repo — and lets every later one through (a sequence counts, being
// non-transactional; the rollback does not reset it).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPatchProject_AnswersOnlyTheCommittedAttempt(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUserForRepoTest(t, pool)
	uniq := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	pid := seedNamedProject(t, pool, owner, "patch-retry-"+uniq, "pr"+uniq)

	tag := "patch_retry_" + uniq
	for _, ddl := range []string{
		`CREATE SEQUENCE ` + tag,
		`CREATE FUNCTION ` + tag + `() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF nextval('` + tag + `') = 1 THEN
    RAISE EXCEPTION 'injected serialization failure' USING ERRCODE = '40001';
  END IF;
  RETURN NEW;
END $$`,
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON events FOR EACH ROW
WHEN (NEW.project_id = '%s'::uuid AND NEW.type = 'project.repo_added')
EXECUTE FUNCTION %s()`, tag, pid, tag),
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("staging the race: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DROP TRIGGER IF EXISTS `+tag+` ON events`)
		_, _ = pool.Exec(c, `DROP FUNCTION IF EXISTS `+tag+`()`)
		_, _ = pool.Exec(c, `DROP SEQUENCE IF EXISTS `+tag)
		_, _ = pool.Exec(c, `DELETE FROM events WHERE project_id = $1`, pid)
		_, _ = pool.Exec(c, `DELETE FROM project_aliases WHERE project_id = $1`, pid)
	})

	alias := "pra" + uniq
	archived := true
	res, err := NewStore(pool).PatchProject(ctx, pid, PatchProjectArgs{
		Archived:    &archived,
		AliasesAdd:  []string{alias},
		GitReposAdd: []AddRepoArgs{{Name: "r", URL: "https://git.example/" + uniq + ".git"}},
	}, owner, "user", HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	var attempts int64
	if err := pool.QueryRow(ctx, `SELECT last_value FROM `+tag).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Fatalf("the race was not staged: %d repo events", attempts)
	}

	// What the database holds after the call.
	var repoIDs []string
	if err := pool.QueryRow(ctx, `SELECT coalesce(array_agg(r->>'id'), '{}') FROM projects, jsonb_array_elements(git_repos) r WHERE id = $1`, pid).Scan(&repoIDs); err != nil {
		t.Fatal(err)
	}
	if len(res.ReposAdded) != 1 || len(repoIDs) != 1 || res.ReposAdded[0].ID.String() != repoIDs[0] {
		t.Errorf("reposAdded %v, the project holds %v — the answer names a repo of the rolled-back attempt", res.ReposAdded, repoIDs)
	}
	if len(res.AliasesAdded) != 1 || res.AliasesAdded[0].Alias != alias {
		t.Errorf("aliasesAdded %v, want the one alias once", res.AliasesAdded)
	}
	if res.ArchivedFlip == nil || !*res.ArchivedFlip {
		t.Errorf("archivedFlip %v, want the committed archive", res.ArchivedFlip)
	}
}
