package projects

import "skein-api/internal/eventbus"

// ProjectPatchFields is the canonical, ordered registry of patch-able
// columns recognised by Store.UpdateProject. Each FieldDef.Diff downcasts
// oldRow / newPatch to (*ProjectDTO, *UpdateProjectArgs) and returns the
// (changed, oldVal, newVal) triple.
//
// Description / Name / DefaultIDMode are simple string fields; GitlabGroup
// is a *string in the DTO so the closure normalises nil → "" before the
// equality check (mirrors COALESCE semantics in the SQL UPDATE).
var ProjectPatchFields = []eventbus.FieldDef{
	{
		GoName: "Name", JSONName: "name", Kebab: "name",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*ProjectDTO)
			in := newPatch.(*UpdateProjectArgs)
			if in.Name == nil || *in.Name == old.Name {
				return false, nil, nil
			}
			return true, old.Name, *in.Name
		},
	},
	{
		GoName: "Description", JSONName: "description", Kebab: "description",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*ProjectDTO)
			in := newPatch.(*UpdateProjectArgs)
			if in.Description == nil || *in.Description == old.Description {
				return false, nil, nil
			}
			return true, old.Description, *in.Description
		},
	},
	{
		GoName: "DefaultIDMode", JSONName: "defaultIdMode", Kebab: "default-id-mode",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*ProjectDTO)
			in := newPatch.(*UpdateProjectArgs)
			if in.DefaultIDMode == nil || *in.DefaultIDMode == old.DefaultIDMode {
				return false, nil, nil
			}
			return true, old.DefaultIDMode, *in.DefaultIDMode
		},
	},
	{
		GoName: "GitlabGroup", JSONName: "gitlabGroup", Kebab: "gitlab-group",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*ProjectDTO)
			in := newPatch.(*UpdateProjectArgs)
			if in.GitlabGroup == nil {
				return false, nil, nil
			}
			oldGroup := ""
			if old.GitlabGroup != nil {
				oldGroup = *old.GitlabGroup
			}
			if oldGroup == *in.GitlabGroup {
				return false, nil, nil
			}
			return true, oldGroup, *in.GitlabGroup
		},
	},
}
