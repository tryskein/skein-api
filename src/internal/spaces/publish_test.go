package spaces_test

// publish_test.go — sk-674.18 T018, matrix row 51 (D-10, ADR-3). Reference —
// the contract of `skein.user.{uid}.access_changed` (signals/contract.go,
// written before the primitives): published only after the final commit of
// the change, only to the users whose access changed, with exact sets; a
// rolled back change publishes nothing. The pattern is
// signals/batch_test.go#TestBeginTxFunc_RollbackPublishesNothing; what it
// does not give — the content of the sets each primitive publishes — is
// asserted here:
//
//   - joining a space: the joiner gains its `space` projects, nobody else
//     hears anything;
//   - leaving it: the leaver loses the space and the projects he saw in it —
//     one event, the two contributions merged — and not his project of
//     another space;
//   - members → space: every participant without a row gains the project;
//     space → members: whoever had it only from the space loses it, an
//     owner by appointment (source "manually") does not;
//   - a primitive rolled back with its transaction: nothing.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/projects"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// The sink is delete_test.go#startSink (same package): access_changed and
// personal signals per user, with settle() waiting for a sentinel signal.

// take returns and forgets the events of user.
func (s *accessSink) take(user uuid.UUID) []signals.AccessChanged {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := s.access[user]
	delete(s.access, user)
	return ev
}

func sortedIDs(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	slices.Sort(out)
	return out
}

// expectOne asserts exactly one event for user with these exact sets.
func expectOne(t *testing.T, s *accessSink, user uuid.UUID, who string, lost, gained []uuid.UUID, lostSpaces []string) {
	t.Helper()
	got := s.take(user)
	if len(got) != 1 {
		t.Errorf("%s: %d access_changed events, want exactly 1: %+v", who, len(got), got)
		return
	}
	ev := got[0]
	if ev.LostProjectIDs == nil || ev.GainedProjectIDs == nil || ev.LostSpaceIDs == nil {
		t.Errorf("%s: an array is absent (null), the contract says [] when empty: %+v", who, ev)
	}
	if !slices.Equal(sortedIDs(ev.LostProjectIDs), sortedIDs(lost)) {
		t.Errorf("%s: lostProjectIds %v, want %v", who, sortedIDs(ev.LostProjectIDs), sortedIDs(lost))
	}
	if !slices.Equal(sortedIDs(ev.GainedProjectIDs), sortedIDs(gained)) {
		t.Errorf("%s: gainedProjectIds %v, want %v", who, sortedIDs(ev.GainedProjectIDs), sortedIDs(gained))
	}
	gotSpaces := slices.Clone(ev.LostSpaceIDs)
	slices.Sort(gotSpaces)
	wantSpaces := slices.Clone(lostSpaces)
	slices.Sort(wantSpaces)
	if !slices.Equal(gotSpaces, wantSpaces) && !(len(gotSpaces) == 0 && len(wantSpaces) == 0) {
		t.Errorf("%s: lostSpaceIds %v, want %v", who, gotSpaces, wantSpaces)
	}
}

func expectNone(t *testing.T, s *accessSink, user uuid.UUID, who string) {
	t.Helper()
	if got := s.take(user); len(got) != 0 {
		t.Errorf("%s: %d access_changed events, want none: %+v", who, len(got), got)
	}
}

func setVisibilityRaw(t *testing.T, pool *pgxpool.Pool, project uuid.UUID, vis string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE projects SET visibility = $2 WHERE id = $1`, project, vis); err != nil {
		t.Fatalf("set visibility: %v", err)
	}
}

func shortIDOf(t *testing.T, pool *pgxpool.Pool, space uuid.UUID) string {
	t.Helper()
	var short string
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&short); err != nil {
		t.Fatalf("short id: %v", err)
	}
	return short
}

func TestAccessChanged_PublishedAfterCommitWithExactSets(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	sink := startSink(t)
	store := spaces.NewStore(pool)

	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	u, _ := testsupport.SeedUser(t, pool, "user")
	bystander, _ := testsupport.SeedUser(t, pool, "user")
	actor := spaces.Actor{UserID: owner, Username: ownerName, Type: "human"}

	space := testsupport.SeedSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, space, bystander)
	pSpace1, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "pub1")
	pSpace2, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "pub2")
	pMembers, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "pubm")
	setVisibilityRaw(t, pool, pSpace1, spaces.VisibilitySpace)
	setVisibilityRaw(t, pool, pSpace2, spaces.VisibilitySpace)
	setVisibilityRaw(t, pool, pMembers, spaces.VisibilityMembers)
	// u is a member of a project of ANOTHER space, which no operation here
	// may name.
	otherOwner, _ := testsupport.SeedUser(t, pool, "user")
	otherSpace := testsupport.SeedSpace(t, pool, otherOwner)
	testsupport.AddSpaceMember(t, pool, otherSpace, u)
	pOther, _ := testsupport.SeedProjectInSpace(t, pool, otherOwner, otherSpace, "pubo")
	testsupport.AddProjectMember(t, pool, pOther, u, "member")
	sink.settle(t, pool)
	for _, x := range []uuid.UUID{owner, u, bystander, otherOwner} {
		sink.take(x)
	}

	t.Run("rolled back join publishes nothing", func(t *testing.T) {
		refused := errors.New("the path refuses after the primitive")
		err := signals.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
			if err := spaces.JoinInTx(ctx, tx, space, u, actor); err != nil {
				return err
			}
			return refused
		})
		if !errors.Is(err, refused) {
			t.Fatalf("BeginTxFunc = %v, want the path's own error", err)
		}
		sink.settle(t, pool)
		expectNone(t, sink, u, "the would-be joiner")
	})

	t.Run("join", func(t *testing.T) {
		if err := store.Join(ctx, space, u, actor); err != nil {
			t.Fatalf("Join: %v", err)
		}
		sink.settle(t, pool)
		expectOne(t, sink, u, "the joiner", nil, []uuid.UUID{pSpace1, pSpace2}, nil)
		expectNone(t, sink, bystander, "another participant")
		expectNone(t, sink, owner, "the owner")
	})

	t.Run("members → space, then space → members", func(t *testing.T) {
		if err := store.SetVisibility(ctx, pMembers, spaces.VisibilitySpace, actor); err != nil {
			t.Fatalf("SetVisibility space: %v", err)
		}
		sink.settle(t, pool)
		expectOne(t, sink, u, "a participant without a row", nil, []uuid.UUID{pMembers}, nil)
		expectOne(t, sink, bystander, "another participant without a row", nil, []uuid.UUID{pMembers}, nil)
		expectNone(t, sink, owner, "the owner (implicit, already sees it)")

		// bystander becomes an owner by appointment: the source "manually".
		if _, err := pool.Exec(ctx, `
UPDATE project_members SET role = 'owner', via_manual = true WHERE project_id = $1 AND user_id = $2`, pMembers, bystander); err != nil {
			t.Fatal(err)
		}
		if err := store.SetVisibility(ctx, pMembers, spaces.VisibilityMembers, actor); err != nil {
			t.Fatalf("SetVisibility members: %v", err)
		}
		sink.settle(t, pool)
		expectOne(t, sink, u, "who had it only from the space", []uuid.UUID{pMembers}, nil, nil)
		expectNone(t, sink, bystander, "the owner by appointment (E-28)")
		expectNone(t, sink, owner, "the space owner")
	})

	t.Run("leave", func(t *testing.T) {
		if err := store.Leave(ctx, space, u, "left_space", false, actor); err != nil {
			t.Fatalf("Leave: %v", err)
		}
		sink.settle(t, pool)
		expectOne(t, sink, u, "the leaver", []uuid.UUID{pSpace1, pSpace2}, nil, []string{shortIDOf(t, pool, space)})
		expectNone(t, sink, bystander, "another participant")
		expectNone(t, sink, otherOwner, "the owner of the other space")
	})
}

// the sets follow the
// actual visibility before and after the operation, by the relation — not
// the rows the operation wrote. Someone who already saw a project gains
// nothing; someone who still sees a space after leaving it loses nothing.
func TestAccessChanged_SetsFollowVisibility(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	sink := startSink(t)
	store := spaces.NewStore(pool)
	var flagBefore bool
	if err := pool.QueryRow(ctx, `SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&flagBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE instance_settings SET admin_enabled = true WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE instance_settings SET admin_enabled = $1 WHERE id = 1`, flagBefore)
	})

	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	admin, _ := testsupport.SeedUser(t, pool, "admin")
	plain, _ := testsupport.SeedUser(t, pool, "user")
	returning, _ := testsupport.SeedUser(t, pool, "user")
	actor := spaces.Actor{UserID: owner, Username: ownerName, Type: "human"}
	space := testsupport.SeedSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, space, admin)
	testsupport.AddSpaceMember(t, pool, space, plain)
	pSpace, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "vis1")
	setVisibilityRaw(t, pool, pSpace, spaces.VisibilitySpace)
	// Created by plain, so the space owner sees it only as its implicit owner.
	pMembers, _ := testsupport.SeedProjectInSpace(t, pool, plain, space, "vism")
	setVisibilityRaw(t, pool, pMembers, spaces.VisibilityMembers)
	// returning holds a manual row on pMembers but is not a participant
	// (inserted directly: testsupport.AddProjectMember would enrol him).
	if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
VALUES ($1, $2, 'member', now(), $3, true, false)`, pMembers, returning, plain); err != nil {
		t.Fatalf("seed the manual row: %v", err)
	}
	sink.settle(t, pool)
	for _, u := range []uuid.UUID{owner, admin, plain, returning} {
		sink.take(u)
	}

	t.Run("join: the manual project becomes visible too", func(t *testing.T) {
		if err := store.Join(ctx, space, returning, actor); err != nil {
			t.Fatalf("Join: %v", err)
		}
		sink.settle(t, pool)
		expectOne(t, sink, returning, "the returning participant", nil, []uuid.UUID{pSpace, pMembers}, nil)
	})

	t.Run("members→space: who already saw it gains nothing", func(t *testing.T) {
		pOther, _ := testsupport.SeedProjectInSpace(t, pool, plain, space, "viso")
		setVisibilityRaw(t, pool, pOther, spaces.VisibilityMembers)
		sink.settle(t, pool)
		for _, u := range []uuid.UUID{owner, admin, plain, returning} {
			sink.take(u)
		}
		if err := store.SetVisibility(ctx, pOther, spaces.VisibilitySpace, actor); err != nil {
			t.Fatalf("SetVisibility: %v", err)
		}
		sink.settle(t, pool)
		expectNone(t, sink, admin, "an admin participant (sees it by INV-1 g")
		expectNone(t, sink, owner, "the space owner (its implicit owner)")
		expectNone(t, sink, plain, "the project's creator and owner")
		expectOne(t, sink, returning, "a participant without a row", nil, []uuid.UUID{pOther}, nil)
	})

	t.Run("explicit add of an admin who already sees it: nothing", func(t *testing.T) {
		if _, err := projects.NewStore(pool).UpsertMember(ctx, pMembers, admin, "member", plain,
			projects.Actor{UserID: plain, Username: "plain", Role: "human"}); err != nil {
			t.Fatalf("UpsertMember: %v", err)
		}
		sink.settle(t, pool)
		expectNone(t, sink, admin, "the admin added to a project he sees")
	})

	t.Run("an admin who leaves still sees the space: nothing lost", func(t *testing.T) {
		if err := store.Leave(ctx, space, admin, "left_space", false, actor); err != nil {
			t.Fatalf("Leave: %v", err)
		}
		sink.settle(t, pool)
		expectNone(t, sink, admin, "the admin who left (adminAccess keeps the space)")
	})
}
