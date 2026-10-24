package spaces_test

// signals_test.go — sk-674.18 T023, matrix row 89 (US-19, screens 1.15).
// Reference — the addressees of the personal signal `spaces` per reason, as
// signals/contract.go#SpaceReason and screens 1.15 name them, written before
// the code:
//
//   - added — the user the admin added;
//   - removed — the user removed;
//   - owner_assigned — the owner of a space the admin created for him, and
//     the new owner of an owner change;
//   - owner_changed — the previous owner and the other participants (not
//     the new owner);
//   - deleted — the participants of the deleted space;
//   - renamed — the participants, with the new name.
//
// Every entry carries the space's short id. Nobody outside the addressees
// gets an entry of that reason — the admin who acted included.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// spaceEntries returns and forgets the `spaces` entries user received.
func (s *accessSink) spaceEntries(user uuid.UUID) []signals.SpaceEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []signals.SpaceEvent
	for _, sig := range s.signals[user] {
		out = append(out, sig.Spaces...)
	}
	delete(s.signals, user)
	return out
}

type sigUser struct {
	id   uuid.UUID
	name string
}

func (u sigUser) caller() spaces.Caller {
	return spaces.Caller{UserID: u.id, Username: u.name,
		Actor: spaces.Actor{UserID: u.id, Username: u.name, Type: "human"}}
}

func seedSigUser(t *testing.T, role string) sigUser {
	t.Helper()
	id, name := testsupport.SeedUser(t, testsupport.NewTestPool(t), role)
	return sigUser{id: id, name: name}
}

func TestSpaceSignals_AddresseesPerReason(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	svc := spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll})

	// want: user → the one entry (reason, space, name if any) he must get
	// for this reason; everyone else in `all` must get none of it.
	type want struct {
		reason signals.SpaceReason
		space  string
		name   string
	}
	check := func(t *testing.T, sink *accessSink, all []sigUser, wants map[uuid.UUID]want, reason signals.SpaceReason) {
		t.Helper()
		sink.settle(t, pool)
		for _, u := range all {
			var got []signals.SpaceEvent
			for _, e := range sink.spaceEntries(u.id) {
				if e.Reason == reason {
					got = append(got, e)
				}
			}
			w, addressee := wants[u.id]
			if !addressee {
				if len(got) != 0 {
					t.Errorf("%s is not an addressee of %q, got %+v", u.name, reason, got)
				}
				continue
			}
			if len(got) != 1 || got[0].SpaceID != w.space || (w.name != "" && got[0].SpaceName != w.name) {
				t.Errorf("%s: %q entries %+v, want one for space %s %q", u.name, reason, got, w.space, w.name)
			}
		}
	}

	t.Run("added", func(t *testing.T) {
		owner, member, admin, x := seedSigUser(t, "user"), seedSigUser(t, "user"), seedSigUser(t, "admin"), seedSigUser(t, "user")
		space := testsupport.SeedSpace(t, pool, owner.id)
		testsupport.AddSpaceMember(t, pool, space, member.id)
		short := shortIDOf(t, pool, space)
		sink := startSink(t)
		if _, err := svc.AddMember(ctx, admin.caller(), short, x.id); err != nil {
			t.Fatalf("add: %v", err)
		}
		check(t, sink, []sigUser{owner, member, admin, x},
			map[uuid.UUID]want{x.id: {signals.SpaceAdded, short, ""}}, signals.SpaceAdded)
	})

	t.Run("removed", func(t *testing.T) {
		owner, member, other := seedSigUser(t, "user"), seedSigUser(t, "user"), seedSigUser(t, "user")
		space := testsupport.SeedSpace(t, pool, owner.id)
		testsupport.AddSpaceMember(t, pool, space, member.id)
		testsupport.AddSpaceMember(t, pool, space, other.id)
		short := shortIDOf(t, pool, space)
		sink := startSink(t)
		if err := svc.RemoveMember(ctx, owner.caller(), short, member.id); err != nil {
			t.Fatalf("remove: %v", err)
		}
		check(t, sink, []sigUser{owner, member, other},
			map[uuid.UUID]want{member.id: {signals.SpaceRemoved, short, ""}}, signals.SpaceRemoved)
	})

	t.Run("owner_assigned on creation", func(t *testing.T) {
		admin, owner := seedSigUser(t, "admin"), seedSigUser(t, "user")
		sink := startSink(t)
		resp, err := svc.Create(ctx, admin.caller(), "Signalled", &owner.id)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE short_id = $1)`, resp.ID)
			_, _ = pool.Exec(c, `DELETE FROM spaces WHERE short_id = $1`, resp.ID)
		})
		check(t, sink, []sigUser{admin, owner},
			map[uuid.UUID]want{owner.id: {signals.SpaceOwnerAssigned, resp.ID, "Signalled"}}, signals.SpaceOwnerAssigned)
	})

	t.Run("owner change: owner_assigned and owner_changed", func(t *testing.T) {
		owner, next, member, admin, outsider := seedSigUser(t, "user"), seedSigUser(t, "user"), seedSigUser(t, "user"),
			seedSigUser(t, "admin"), seedSigUser(t, "user")
		space := testsupport.SeedSpace(t, pool, owner.id)
		testsupport.AddSpaceMember(t, pool, space, next.id)
		testsupport.AddSpaceMember(t, pool, space, member.id)
		short := shortIDOf(t, pool, space)
		sink := startSink(t)
		if _, err := svc.ChangeOwner(ctx, admin.caller(), short, next.id); err != nil {
			t.Fatalf("change the owner: %v", err)
		}
		sink.settle(t, pool)
		all := []sigUser{owner, next, member, admin, outsider}
		// Read every user's entries once and judge both reasons on them.
		got := map[uuid.UUID][]signals.SpaceEvent{}
		for _, u := range all {
			got[u.id] = sink.spaceEntries(u.id)
		}
		count := func(u sigUser, r signals.SpaceReason) int {
			n := 0
			for _, e := range got[u.id] {
				if e.Reason == r && e.SpaceID == short {
					n++
				}
			}
			return n
		}
		for _, c := range []struct {
			u             sigUser
			assigned, chg int
		}{
			{owner, 0, 1}, {next, 1, 0}, {member, 0, 1}, {admin, 0, 0}, {outsider, 0, 0},
		} {
			if a, ch := count(c.u, signals.SpaceOwnerAssigned), count(c.u, signals.SpaceOwnerChanged); a != c.assigned || ch != c.chg {
				t.Errorf("%s: owner_assigned %d owner_changed %d, want %d %d", c.u.name, a, ch, c.assigned, c.chg)
			}
		}
	})

	t.Run("deleted", func(t *testing.T) {
		owner, member, outsider := seedSigUser(t, "user"), seedSigUser(t, "user"), seedSigUser(t, "user")
		space := testsupport.SeedSpace(t, pool, owner.id)
		testsupport.AddSpaceMember(t, pool, space, member.id)
		short := shortIDOf(t, pool, space)
		var name string
		if err := pool.QueryRow(ctx, `SELECT name FROM spaces WHERE id = $1`, space).Scan(&name); err != nil {
			t.Fatal(err)
		}
		sink := startSink(t)
		if err := svc.Delete(ctx, owner.caller(), short, name); err != nil {
			t.Fatalf("delete: %v", err)
		}
		check(t, sink, []sigUser{owner, member, outsider}, map[uuid.UUID]want{
			owner.id:  {signals.SpaceDeleted, short, name},
			member.id: {signals.SpaceDeleted, short, name},
		}, signals.SpaceDeleted)
	})

	t.Run("renamed", func(t *testing.T) {
		owner, member, outsider := seedSigUser(t, "user"), seedSigUser(t, "user"), seedSigUser(t, "user")
		space := testsupport.SeedSpace(t, pool, owner.id)
		testsupport.AddSpaceMember(t, pool, space, member.id)
		short := shortIDOf(t, pool, space)
		sink := startSink(t)
		if _, err := svc.Rename(ctx, owner.caller(), short, "New Name"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		check(t, sink, []sigUser{owner, member, outsider}, map[uuid.UUID]want{
			owner.id:  {signals.SpaceRenamed, short, "New Name"},
			member.id: {signals.SpaceRenamed, short, "New Name"},
		}, signals.SpaceRenamed)
	})
}
