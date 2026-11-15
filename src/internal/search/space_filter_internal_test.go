package search

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

// TestSpaceIDByShort_OnlySpacesTheCallerSees asserts that the space= filter
// of search resolves a short id only among the spaces the caller sees: a
// member gets the space, while a caller outside it gets the same answer as
// for a short id no space has — so the filter cannot tell an existing space
// from a missing one.
func TestSpaceIDByShort_OnlySpacesTheCallerSees(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	member, _ := testsupport.SeedUser(t, pool, "user")
	outsider, _ := testsupport.SeedUser(t, pool, "user")
	spaceID := testsupport.SeedSpace(t, pool, member)
	var short string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, spaceID).Scan(&short); err != nil {
		t.Fatalf("read short id: %v", err)
	}
	store := NewStore(pool)

	for _, tc := range []struct {
		name   string
		caller uuid.UUID
		short  string
		want   uuid.UUID
	}{
		{"member", member, short, spaceID},
		{"outsider", outsider, short, uuid.Nil},
		{"missing space", member, "nosuchs1", uuid.Nil}, // '1' is outside the short-id alphabet
	} {
		got, err := store.spaceIDByShort(ctx, tc.caller, tc.short)
		if err != nil {
			t.Fatalf("%s: spaceIDByShort(%q): %v", tc.name, tc.short, err)
		}
		if got != tc.want {
			t.Errorf("%s: spaceIDByShort(%q) = %s, want %s", tc.name, tc.short, got, tc.want)
		}
	}
}
