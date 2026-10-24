package users

import (
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/eventbus"
)

func ptrStr(s string) *string { return &s }

func findChange(changes []eventbus.FieldChange, field string) *eventbus.FieldChange {
	for i := range changes {
		if changes[i].Field == field {
			return &changes[i]
		}
	}
	return nil
}

func baseUserDTO() *UserDTO {
	return &UserDTO{
		ID:       uuid.Must(uuid.NewRandom()),
		Email:    "old@example.com",
		Username: "oldname",
		Name:     "Old Name",
		Role:     "user",
	}
}

// TestUserPatchFields_NoOp — empty input yields zero changes; password
// rotation is NOT in the registry (lifecycle path) so a Password-only
// patch must not emit any hybrid event.
func TestUserPatchFields_NoOp(t *testing.T) {
	old := baseUserDTO()
	in := &UpdateUserInput{}
	changes := eventbus.DiffRegistry(UserPatchFields, old, in)
	if len(changes) != 0 {
		t.Fatalf("got %d changes, want 0", len(changes))
	}
}

func TestUserPatchFields_PasswordOnlyNoChange(t *testing.T) {
	old := baseUserDTO()
	in := &UpdateUserInput{Password: ptrStr("newPa$$1234")}
	changes := eventbus.DiffRegistry(UserPatchFields, old, in)
	if len(changes) != 0 {
		t.Fatalf("password-only patch produced %d changes; password is lifecycle-only", len(changes))
	}
}

// TestUserPatchFields_NameAndRole — both fields change, both should
// appear in the result.
func TestUserPatchFields_NameAndRole(t *testing.T) {
	old := baseUserDTO()
	in := &UpdateUserInput{Name: ptrStr("New Name"), Role: ptrStr("admin")}
	changes := eventbus.DiffRegistry(UserPatchFields, old, in)
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(changes))
	}
	if findChange(changes, "name") == nil {
		t.Fatal("missing name change")
	}
	if findChange(changes, "role") == nil {
		t.Fatal("missing role change")
	}
}

// TestUserPatchFields_NameUnchanged — same value, no change.
func TestUserPatchFields_NameUnchanged(t *testing.T) {
	old := baseUserDTO()
	in := &UpdateUserInput{Name: ptrStr(old.Name)}
	changes := eventbus.DiffRegistry(UserPatchFields, old, in)
	if findChange(changes, "name") != nil {
		t.Fatal("name should not appear when value matches old")
	}
}

// TestUserPatchFields_NameClearedToEmpty — setting the field to "" is
// still a change when old was non-empty (mirrors the explicit-clear
// semantics tested in issues/projects).
func TestUserPatchFields_NameClearedToEmpty(t *testing.T) {
	old := baseUserDTO()
	in := &UpdateUserInput{Name: ptrStr("")}
	changes := eventbus.DiffRegistry(UserPatchFields, old, in)
	c := findChange(changes, "name")
	if c == nil {
		t.Fatal("missing name change for clear")
	}
	if c.OldValue != "Old Name" || c.NewValue != "" {
		t.Fatalf("got %+v, want old=\"Old Name\" new=\"\"", c)
	}
}
