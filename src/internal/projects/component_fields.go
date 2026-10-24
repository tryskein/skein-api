package projects

import (
	"skein-api/internal/eventbus"
)

// ComponentPatchFields is the canonical, ordered registry of patch-able
// columns recognised by Store.UpdateComponent. Paths is a JSONB-backed
// slice; per the R3.2b plan we emit a single FieldChange with the full
// old/new slice rather than deep-diffing entries inside the array.
var ComponentPatchFields = []eventbus.FieldDef{
	{
		GoName: "Name", JSONName: "name", Kebab: "name",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*ComponentDTO)
			in := newPatch.(*UpdateComponentArgs)
			if in.Name == nil || *in.Name == old.Name {
				return false, nil, nil
			}
			return true, old.Name, *in.Name
		},
	},
	{
		GoName: "Description", JSONName: "description", Kebab: "description",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*ComponentDTO)
			in := newPatch.(*UpdateComponentArgs)
			if in.Description == nil || *in.Description == old.Description {
				return false, nil, nil
			}
			return true, old.Description, *in.Description
		},
	},
	{
		GoName: "Paths", JSONName: "paths", Kebab: "paths",
		// Manual len+loop compare — ComponentPath is comparable (uuid +
		// string). Order matters for both the UPDATE (whole jsonb blob)
		// and the event payload (subscribers may render order-aware).
		// Nil vs empty is treated unequal, matching reflect.DeepEqual.
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*ComponentDTO)
			in := newPatch.(*UpdateComponentArgs)
			if in.Paths == nil {
				return false, nil, nil
			}
			if componentPathsEqual(old.Paths, *in.Paths) {
				return false, nil, nil
			}
			return true, old.Paths, *in.Paths
		},
	},
}

// componentPathsEqual reports whether a and b are equal element-wise.
// Matches reflect.DeepEqual on []ComponentPath (including nil ≠ empty).
func componentPathsEqual(a, b []ComponentPath) bool {
	if len(a) != len(b) {
		return false
	}
	if (a == nil) != (b == nil) {
		return false
	}
	for i := range a {
		if a[i].RepoID != b[i].RepoID || a[i].Glob != b[i].Glob {
			return false
		}
	}
	return true
}
