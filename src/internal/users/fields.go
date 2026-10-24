package users

import "skein-api/internal/eventbus"

// userFieldDiff is the private typed Diff signature for UserPatchFields.
type userFieldDiff func(old *UserDTO, in *UpdateUserInput) (changed bool, oldVal, newVal any)

// userDiff adapts a typed userFieldDiff to eventbus.FieldDef.Diff.
// Wrong concrete types yield changed=false (defensive).
func userDiff(f userFieldDiff) func(oldRow, newPatch any) (bool, any, any) {
	return func(oldRow, newPatch any) (bool, any, any) {
		old, ok1 := oldRow.(*UserDTO)
		in, ok2 := newPatch.(*UpdateUserInput)
		if !ok1 || !ok2 {
			return false, nil, nil
		}
		return f(old, in)
	}
}

// UserPatchFields is the registry of patch-able columns recognised by
// Store.UpdateUser for the **non-password** path. Password rotation
// continues to emit user.password_changed lifecycle events directly, so
// it is intentionally NOT in this registry — when a single PATCH carries
// both Name/Role and Password the store emits the hybrid pair AND the
// password_changed lifecycle event in the same transaction (R3.2b D-1
// "coexist").
var UserPatchFields = []eventbus.FieldDef{
	{
		GoName: "Name", JSONName: "name", Kebab: "name",
		Diff: userDiff(func(old *UserDTO, in *UpdateUserInput) (bool, any, any) {
			if in.Name == nil || *in.Name == old.Name {
				return false, nil, nil
			}
			return true, old.Name, *in.Name
		}),
	},
	{
		GoName: "Role", JSONName: "role", Kebab: "role",
		Diff: userDiff(func(old *UserDTO, in *UpdateUserInput) (bool, any, any) {
			if in.Role == nil || *in.Role == old.Role {
				return false, nil, nil
			}
			return true, old.Role, *in.Role
		}),
	},
}
