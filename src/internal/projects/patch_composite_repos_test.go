package projects

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests pin the GitReposRemove contract of PatchProject, which is
// what the batched component-reference lookup must not change:
//
//   - a repo id not present in git_repos[] → *errRepoMissing;
//   - a repo still referenced by a non-archived component → *ErrRepoInUse
//     carrying every referencing component name, ordered by name;
//   - the FIRST offending repo in caller order decides which of the two
//     errors the client sees;
//   - any failure rolls the whole patch back (git_repos[] untouched).
//
// N > 1 is the interesting case: one query now resolves references for
// the entire removal batch, so a regression would show up as the wrong
// repo (or the wrong component list) being reported.

// patchReposFixture seeds a project with `n` repos and returns the store,
// project id and repo ids.
func patchReposFixture(t *testing.T, prefix string, n int) (*Store, *pgxpool.Pool, uuid.UUID, uuid.UUID, []uuid.UUID) {
	t.Helper()
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)

	repos := make([]Repo, 0, n)
	for i := 0; i < n; i++ {
		repos = append(repos, Repo{
			ID:   uuid.Must(uuid.NewV7()),
			Name: string(rune('a'+i)) + "-repo",
			URL:  "https://example.com/" + string(rune('a'+i)) + ".git",
		})
	}
	pid, repoIDs := seedProjectWithRepos(t, pool, owner, prefix, repos)
	return NewStore(pool), pool, pid, owner, repoIDs
}

// remainingRepoIDs reads the project's git_repos[] as a set, straight
// from the column — deliberately not through the store, so the assertion
// does not depend on the code under test.
func remainingRepoIDs(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID) map[uuid.UUID]struct{} {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT git_repos FROM projects WHERE id = $1`, pid).Scan(&raw); err != nil {
		t.Fatalf("read git_repos: %v", err)
	}
	var repos []Repo
	if err := json.Unmarshal(raw, &repos); err != nil {
		t.Fatalf("decode git_repos: %v", err)
	}
	out := make(map[uuid.UUID]struct{}, len(repos))
	for _, r := range repos {
		out[r.ID] = struct{}{}
	}
	return out
}

// TestPatchProject_ReposRemove_BatchUnreferenced: N=3 unreferenced repos
// removed in one call — all three gone, nothing else touched.
func TestPatchProject_ReposRemove_BatchUnreferenced(t *testing.T) {
	s, pool, pid, owner, repoIDs := patchReposFixture(t, "prrb", 4)
	ctx := context.Background()

	res, err := s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: []uuid.UUID{repoIDs[0], repoIDs[1], repoIDs[2]},
	}, owner, "user", HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("PatchProject: %v", err)
	}
	if len(res.ReposRemoved) != 3 {
		t.Fatalf("ReposRemoved = %v, want 3 entries", res.ReposRemoved)
	}
	for i, got := range res.ReposRemoved {
		if got != repoIDs[i] {
			t.Errorf("ReposRemoved[%d] = %s, want %s", i, got, repoIDs[i])
		}
	}
	left := remainingRepoIDs(t, pool, pid)
	if len(left) != 1 {
		t.Fatalf("git_repos[] has %d entries, want 1", len(left))
	}
	if _, ok := left[repoIDs[3]]; !ok {
		t.Errorf("surviving repo = %v, want %s", left, repoIDs[3])
	}
}

// TestPatchProject_ReposRemove_SingleUnreferenced is the N=1 boundary.
func TestPatchProject_ReposRemove_SingleUnreferenced(t *testing.T) {
	s, pool, pid, owner, repoIDs := patchReposFixture(t, "prr1", 2)
	ctx := context.Background()

	res, err := s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: []uuid.UUID{repoIDs[1]},
	}, owner, "user", HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("PatchProject: %v", err)
	}
	if len(res.ReposRemoved) != 1 || res.ReposRemoved[0] != repoIDs[1] {
		t.Fatalf("ReposRemoved = %v, want [%s]", res.ReposRemoved, repoIDs[1])
	}
	left := remainingRepoIDs(t, pool, pid)
	if _, gone := left[repoIDs[1]]; gone {
		t.Errorf("repo %s still in git_repos[]", repoIDs[1])
	}
	if _, kept := left[repoIDs[0]]; !kept {
		t.Errorf("repo %s must survive", repoIDs[0])
	}
}

// TestPatchProject_ReposRemove_Empty is the N=0 boundary: an empty
// removal list must not touch git_repos[] (and must not error).
func TestPatchProject_ReposRemove_Empty(t *testing.T) {
	s, pool, pid, owner, repoIDs := patchReposFixture(t, "prr0", 2)
	ctx := context.Background()

	res, err := s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: nil,
	}, owner, "user", HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("PatchProject: %v", err)
	}
	if len(res.ReposRemoved) != 0 {
		t.Fatalf("ReposRemoved = %v, want empty", res.ReposRemoved)
	}
	left := remainingRepoIDs(t, pool, pid)
	for _, id := range repoIDs {
		if _, ok := left[id]; !ok {
			t.Errorf("repo %s must survive an empty removal batch", id)
		}
	}
}

// TestPatchProject_ReposRemove_InUseInBatch: the second repo of the batch
// is referenced by two components → *ErrRepoInUse with BOTH component
// names ordered by name, and the first (removable) repo is rolled back.
func TestPatchProject_ReposRemove_InUseInBatch(t *testing.T) {
	s, pool, pid, owner, repoIDs := patchReposFixture(t, "prriu", 3)
	ctx := context.Background()
	act := HumanActor(owner, "owner")

	// "zeta" created first, "alpha" second — the error must still list
	// them alphabetically, which is what ORDER BY name guarantees.
	for _, name := range []string{"zeta", "alpha"} {
		if _, err := s.CreateComponent(ctx, pid, owner, CreateComponentArgs{
			Name:  name,
			Paths: []ComponentPath{{RepoID: repoIDs[1], Glob: "src/**"}},
		}, act); err != nil {
			t.Fatalf("CreateComponent %s: %v", name, err)
		}
	}
	// A component on repo[2] as well, to prove the batch query attributes
	// references to the right repo instead of merging them.
	if _, err := s.CreateComponent(ctx, pid, owner, CreateComponentArgs{
		Name:  "other",
		Paths: []ComponentPath{{RepoID: repoIDs[2], Glob: "ops/**"}},
	}, act); err != nil {
		t.Fatalf("CreateComponent other: %v", err)
	}

	_, err := s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: []uuid.UUID{repoIDs[0], repoIDs[1]},
	}, owner, "user", act)

	var inUse *ErrRepoInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("err = %v, want *ErrRepoInUse", err)
	}
	want := []string{"alpha", "zeta"}
	if len(inUse.Components) != len(want) {
		t.Fatalf("Components = %v, want %v", inUse.Components, want)
	}
	for i := range want {
		if inUse.Components[i] != want[i] {
			t.Errorf("Components[%d] = %q, want %q", i, inUse.Components[i], want[i])
		}
	}

	// Whole patch rolled back — repo[0] must still be there.
	left := remainingRepoIDs(t, pool, pid)
	if len(left) != 3 {
		t.Fatalf("git_repos[] has %d entries, want 3 (rollback)", len(left))
	}
}

// TestPatchProject_ReposRemove_FirstOffenderWins: repo[0] is referenced,
// repo[1] is missing from git_repos[]. Caller order decides which error
// surfaces — repo[0] comes first, so *ErrRepoInUse wins over
// *errRepoMissing.
func TestPatchProject_ReposRemove_FirstOffenderWins(t *testing.T) {
	s, pool, pid, owner, repoIDs := patchReposFixture(t, "prrfo", 2)
	ctx := context.Background()
	act := HumanActor(owner, "owner")

	if _, err := s.CreateComponent(ctx, pid, owner, CreateComponentArgs{
		Name:  "holder",
		Paths: []ComponentPath{{RepoID: repoIDs[0], Glob: "cmd/**"}},
	}, act); err != nil {
		t.Fatalf("CreateComponent: %v", err)
	}
	bogus := uuid.Must(uuid.NewV7())

	// referenced repo first → in-use error.
	_, err := s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: []uuid.UUID{repoIDs[0], bogus},
	}, owner, "user", act)
	var inUse *ErrRepoInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("err = %v, want *ErrRepoInUse (referenced repo listed first)", err)
	}

	// missing repo first → missing error, even though a referenced repo
	// is also in the batch.
	_, err = s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: []uuid.UUID{bogus, repoIDs[0]},
	}, owner, "user", act)
	gotID, ok := IsRepoMissing(err)
	if !ok {
		t.Fatalf("err = %v, want errRepoMissing (missing repo listed first)", err)
	}
	if gotID != bogus {
		t.Errorf("missing repo id = %s, want %s", gotID, bogus)
	}
	if left := remainingRepoIDs(t, pool, pid); len(left) != 2 {
		t.Errorf("git_repos[] has %d entries, want 2 (both attempts rolled back)", len(left))
	}
}

// TestPatchProject_ReposRemove_DuplicateIDInBatch: the same repo listed
// twice — the second occurrence is no longer in the (in-memory) list, so
// it must report *errRepoMissing and roll the whole patch back.
func TestPatchProject_ReposRemove_DuplicateIDInBatch(t *testing.T) {
	s, pool, pid, owner, repoIDs := patchReposFixture(t, "prrdup", 2)
	ctx := context.Background()

	_, err := s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: []uuid.UUID{repoIDs[0], repoIDs[0]},
	}, owner, "user", HumanActor(owner, "owner"))
	gotID, ok := IsRepoMissing(err)
	if !ok {
		t.Fatalf("err = %v, want errRepoMissing on the duplicate", err)
	}
	if gotID != repoIDs[0] {
		t.Errorf("missing repo id = %s, want %s", gotID, repoIDs[0])
	}
	if len(remainingRepoIDs(t, pool, pid)) != 2 {
		t.Errorf("git_repos[] must be intact after rollback")
	}
}

// TestPatchProject_ReposRemove_ArchivedComponentIgnored: an archived
// component's reference does not block removal — the batched query keeps
// the `archived_at IS NULL` predicate.
func TestPatchProject_ReposRemove_ArchivedComponentIgnored(t *testing.T) {
	s, pool, pid, owner, repoIDs := patchReposFixture(t, "prrarch", 2)
	ctx := context.Background()
	act := HumanActor(owner, "owner")

	dto, err := s.CreateComponent(ctx, pid, owner, CreateComponentArgs{
		Name:  "archived-holder",
		Paths: []ComponentPath{{RepoID: repoIDs[0], Glob: "cmd/**"}},
	}, act)
	if err != nil {
		t.Fatalf("CreateComponent: %v", err)
	}
	if err := s.ArchiveComponent(ctx, pid, dto.ID, act); err != nil {
		t.Fatalf("ArchiveComponent: %v", err)
	}

	res, err := s.PatchProject(ctx, pid, PatchProjectArgs{
		GitReposRemove: []uuid.UUID{repoIDs[0]},
	}, owner, "user", act)
	if err != nil {
		t.Fatalf("PatchProject: %v", err)
	}
	if len(res.ReposRemoved) != 1 {
		t.Fatalf("ReposRemoved = %v, want 1", res.ReposRemoved)
	}
	if _, still := remainingRepoIDs(t, pool, pid)[repoIDs[0]]; still {
		t.Errorf("repo %s must be gone from git_repos[]", repoIDs[0])
	}
}
