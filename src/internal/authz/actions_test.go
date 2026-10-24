package authz

import "testing"

// TestAllows — the action table by level, written from the US-6 table
// of the spec as changed by the owner 2026-10-17 (sk-741): an instance
// admin has the owner's rights in every project he sees, with a member row
// (admin_member) or without one (admin); a plain member writes and assigns
// but neither manages, re-roles, switches visibility, force-unlocks nor
// deletes another author's comment; the owner (the implicit owner of the
// space included — his level is owner) and declared internal work
// (internal) may do everything.
func TestAllows(t *testing.T) {
	const (
		y = true
		n = false
	)
	table := []struct {
		action                                      Action
		internal, owner, member, adminMember, admin bool
	}{
		{ActionView, y, y, y, y, y},
		{ActionWrite, y, y, y, y, y},
		{ActionAssign, y, y, y, y, y},
		{ActionManageMembers, y, y, n, y, y},
		{ActionChangeVisibility, y, y, n, y, y},
		{ActionManageProject, y, y, n, y, y},
		{ActionForceUnlock, y, y, n, y, y},
		{ActionDeleteAnyComment, y, y, n, y, y},
		// Project creation concerns no existing project: no level grants
		// it through Can (CanCreateProject answers it).
		{ActionCreateProject, n, n, n, n, n},
	}
	for _, row := range table {
		for level, want := range map[Level]bool{
			LevelInternal: row.internal, LevelOwner: row.owner, LevelMember: row.member,
			LevelAdminMember: row.adminMember, LevelAdmin: row.admin,
		} {
			if got := Allows(level, row.action); got != want {
				t.Errorf("Allows(%s, %s) = %v, want %v", level, row.action, got, want)
			}
		}
		// No level (the project is out of reach): nothing (row 15, "none").
		if Allows("", row.action) {
			t.Errorf("Allows(no level, %s) = true, want false", row.action)
		}
	}
	if Allows(LevelAdmin, "drop_everything") {
		t.Error("an unknown action must permit nothing")
	}
}
