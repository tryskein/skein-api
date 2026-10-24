package projects

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

func baseProject() *ProjectDTO {
	g := "old-group"
	return &ProjectDTO{
		ID:            uuid.Must(uuid.NewRandom()),
		Name:          "Old Name",
		IDPrefix:      "old",
		DefaultIDMode: "counter",
		Description:   "Old description",
		GitlabGroup:   &g,
	}
}

// TestProjectPatchFields_NoOp — empty patch yields zero changes.
func TestProjectPatchFields_NoOp(t *testing.T) {
	old := baseProject()
	in := &UpdateProjectArgs{}
	changes := eventbus.DiffRegistry(ProjectPatchFields, old, in)
	if len(changes) != 0 {
		t.Fatalf("got %d changes, want 0", len(changes))
	}
}

// TestProjectPatchFields_AllSet — every field different, expect 4
// changes in registry order.
func TestProjectPatchFields_AllSet(t *testing.T) {
	old := baseProject()
	in := &UpdateProjectArgs{
		Name:          ptrStr("New Name"),
		Description:   ptrStr("New description"),
		DefaultIDMode: ptrStr("uuid"),
		GitlabGroup:   ptrStr("new-group"),
	}
	changes := eventbus.DiffRegistry(ProjectPatchFields, old, in)
	if len(changes) != 4 {
		t.Fatalf("got %d changes, want 4", len(changes))
	}
}

// TestProjectPatchFields_GitlabGroupNilToValue covers the *string → ""
// normalisation that mirrors COALESCE semantics in the SQL UPDATE.
func TestProjectPatchFields_GitlabGroupNilToValue(t *testing.T) {
	old := baseProject()
	old.GitlabGroup = nil
	in := &UpdateProjectArgs{GitlabGroup: ptrStr("acme")}
	changes := eventbus.DiffRegistry(ProjectPatchFields, old, in)
	c := findChange(changes, "gitlabGroup")
	if c == nil {
		t.Fatal("missing gitlabGroup change")
	}
	if c.OldValue != "" || c.NewValue != "acme" {
		t.Fatalf("got %+v, want old=\"\" new=acme", c)
	}
}

// TestProjectPatchFields_NameUnchanged — same value, no change.
func TestProjectPatchFields_NameUnchanged(t *testing.T) {
	old := baseProject()
	in := &UpdateProjectArgs{Name: ptrStr(old.Name)}
	changes := eventbus.DiffRegistry(ProjectPatchFields, old, in)
	if findChange(changes, "name") != nil {
		t.Fatal("expected no change when patching to current value")
	}
}

func baseComponent() *ComponentDTO {
	return &ComponentDTO{
		ID:          uuid.Must(uuid.NewRandom()),
		ProjectID:   uuid.Must(uuid.NewRandom()),
		Name:        "ApiServer",
		Description: "Old description",
		Paths: []ComponentPath{
			{RepoID: uuid.Must(uuid.NewRandom()), Glob: "src/api/**"},
		},
	}
}

// TestComponentPatchFields_NoOp — empty patch yields zero changes.
func TestComponentPatchFields_NoOp(t *testing.T) {
	old := baseComponent()
	in := &UpdateComponentArgs{}
	changes := eventbus.DiffRegistry(ComponentPatchFields, old, in)
	if len(changes) != 0 {
		t.Fatalf("got %d changes, want 0", len(changes))
	}
}

// TestComponentPatchFields_PathsReplaced — the slice is emitted as a
// single FieldChange with the full old/new values rather than per-entry
// deltas.
func TestComponentPatchFields_PathsReplaced(t *testing.T) {
	old := baseComponent()
	newPaths := []ComponentPath{
		{RepoID: uuid.Must(uuid.NewRandom()), Glob: "lib/**"},
		{RepoID: uuid.Must(uuid.NewRandom()), Glob: "cmd/**"},
	}
	in := &UpdateComponentArgs{Paths: &newPaths}
	changes := eventbus.DiffRegistry(ComponentPatchFields, old, in)
	c := findChange(changes, "paths")
	if c == nil {
		t.Fatal("missing paths change")
	}
	got, ok := c.NewValue.([]ComponentPath)
	if !ok {
		t.Fatalf("NewValue type = %T, want []ComponentPath", c.NewValue)
	}
	if len(got) != 2 {
		t.Fatalf("new paths len = %d, want 2", len(got))
	}
}

// TestComponentPatchFields_PathsSameContent — DeepEqual catches
// equivalent slices and skips the change.
func TestComponentPatchFields_PathsSameContent(t *testing.T) {
	old := baseComponent()
	cp := append([]ComponentPath(nil), old.Paths...)
	in := &UpdateComponentArgs{Paths: &cp}
	changes := eventbus.DiffRegistry(ComponentPatchFields, old, in)
	if findChange(changes, "paths") != nil {
		t.Fatal("expected no change when paths slice is structurally equal")
	}
}
