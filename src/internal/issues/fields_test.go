package issues

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/eventbus"
)

// findChange returns the FieldChange with the matching field name, or
// nil. Tests use this as a tiny lookup helper instead of asserting on
// slice index (registry order may evolve).
func findChange(changes []eventbus.FieldChange, field string) *eventbus.FieldChange {
	for i := range changes {
		if changes[i].Field == field {
			return &changes[i]
		}
	}
	return nil
}

// baseRow builds an *issueRow with every field populated so individual
// tests can mutate one column at a time.
func baseRow() *issueRow {
	zero := 0
	pri := 3
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := "alice"
	ext := "EXT-1"
	src := "ctx-old"
	_ = zero
	return &issueRow{
		ID:                 "test-1",
		ProjectID:          uuid.Must(uuid.NewRandom()),
		Version:            1,
		Title:              "Old title",
		Description:        "Old desc",
		Type:               "story",
		Priority:           pri,
		Status:             "open",
		Pinned:             false,
		Assignee:           &a,
		Design:             "old design",
		AcceptanceCriteria: "old ac",
		Notes:              "old notes",
		EstimatedMinutes:   nil,
		DueAt:              &t1,
		DeferUntil:         nil,
		ExternalRef:        &ext,
		SourceContext:      &src,
	}
}

func ptrInt(v int) *int       { return &v }
func ptrStr(v string) *string { return &v }
func ptrTime(v time.Time) *time.Time {
	return &v
}

// TestIssuePatchFields_NoOp covers all 13 fields when no patch is set.
// DiffRegistry must return an empty slice — no events, no fanout.
func TestIssuePatchFields_NoOp(t *testing.T) {
	old := baseRow()
	in := &PatchIssueInput{}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	if len(changes) != 0 {
		t.Fatalf("got %d changes, want 0; %+v", len(changes), changes)
	}
}

// TestIssuePatchFields_AllSet exercises the registry hitting every field.
// We validate that exactly 13 changes come back when every patch pointer
// differs from the old row.
func TestIssuePatchFields_AllSet(t *testing.T) {
	old := baseRow()
	in := &PatchIssueInput{
		Title:              ptrStr("New title"),
		Description:        ptrStr("New desc"),
		Type:               ptrStr("bug"),
		Priority:           ptrInt(5),
		Assignee:           ptrStr("bob"),
		Design:             ptrStr("new design"),
		AcceptanceCriteria: ptrStr("new ac"),
		Notes:              ptrStr("new notes"),
		EstimatedMinutes:   ptrInt(60),
		DueAt:              ptrTime(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)),
		DeferUntil:         ptrTime(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)),
		ExternalRef:        ptrStr("EXT-9"),
		SourceContext:      ptrStr("ctx-new"),
	}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	if len(changes) != 13 {
		t.Fatalf("got %d changes, want 13; fields=%v", len(changes), fieldNames(changes))
	}
}

func fieldNames(changes []eventbus.FieldChange) []string {
	names := make([]string, 0, len(changes))
	for _, c := range changes {
		names = append(names, c.Field)
	}
	return names
}

// TestIssuePatchFields_ClearStringField verifies that patching a string
// field to "" emits a change when the old value is non-empty (R3.2b
// "explicit clear" semantics — distinct from "leave alone" which uses a
// nil pointer).
func TestIssuePatchFields_ClearStringField(t *testing.T) {
	old := baseRow()
	old.Title = "Original"
	in := &PatchIssueInput{Title: ptrStr("")}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	if c := findChange(changes, "title"); c == nil || c.OldValue != "Original" || c.NewValue != "" {
		t.Fatalf("got %+v, want field=title, old=Original, new=\"\"", c)
	}
}

// TestIssuePatchFields_AssigneeFromNilToValue covers the (nil → "alice")
// transition on the *string field. After W4 (uniform null-vs-zero shape)
// the old value is the raw *string pointer — nil for "was unset" — so
// json.Marshal serialises the change as `oldValue:null,newValue:"alice"`
// (matching the EstimatedMinutes / DueAt / DeferUntil shape).
func TestIssuePatchFields_AssigneeFromNilToValue(t *testing.T) {
	old := baseRow()
	old.Assignee = nil
	in := &PatchIssueInput{Assignee: ptrStr("alice")}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	c := findChange(changes, "assignee")
	if c == nil {
		t.Fatal("missing assignee change")
	}
	if c.OldValue != (*string)(nil) {
		t.Fatalf("OldValue = %v (%T), want (*string)(nil)", c.OldValue, c.OldValue)
	}
	if c.NewValue != "alice" {
		t.Fatalf("NewValue = %v, want alice", c.NewValue)
	}
}

// TestIssuePatchFields_AssigneeUnchanged covers (alice → alice) — same
// value should NOT emit a change.
func TestIssuePatchFields_AssigneeUnchanged(t *testing.T) {
	old := baseRow()
	a := "alice"
	old.Assignee = &a
	in := &PatchIssueInput{Assignee: ptrStr("alice")}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	if findChange(changes, "assignee") != nil {
		t.Fatal("assignee should not be in changes when value unchanged")
	}
}

// TestIssuePatchFields_EstimatedMinutesFromNilToValue covers *int
// nullable: nil old + non-nil patch = change.
func TestIssuePatchFields_EstimatedMinutesFromNilToValue(t *testing.T) {
	old := baseRow()
	old.EstimatedMinutes = nil
	in := &PatchIssueInput{EstimatedMinutes: ptrInt(45)}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	c := findChange(changes, "estimatedMinutes")
	if c == nil {
		t.Fatal("missing estimatedMinutes change")
	}
	if c.NewValue != 45 {
		t.Fatalf("new = %v, want 45", c.NewValue)
	}
}

// TestIssuePatchFields_DueAtSameInstantNoChange covers Time.Equal —
// equivalent instants in different *time.Time pointers should not emit.
func TestIssuePatchFields_DueAtSameInstantNoChange(t *testing.T) {
	old := baseRow()
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	old.DueAt = &t1
	t2 := t1.In(time.FixedZone("X", 3*3600)) // same instant, different zone
	in := &PatchIssueInput{DueAt: &t2}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	if findChange(changes, "dueAt") != nil {
		t.Fatal("dueAt should not change when both pointers reference the same instant")
	}
}

// TestIssuePatchFields_ExternalRefFromNilToValue mirrors the Assignee
// case for ExternalRef: nil old → JSON null on the wire (raw *string
// pointer in OldValue). Pinned by W4 — uniform null-vs-zero shape.
func TestIssuePatchFields_ExternalRefFromNilToValue(t *testing.T) {
	old := baseRow()
	old.ExternalRef = nil
	in := &PatchIssueInput{ExternalRef: ptrStr("EXT-2")}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	c := findChange(changes, "externalRef")
	if c == nil {
		t.Fatal("missing externalRef change")
	}
	if c.OldValue != (*string)(nil) {
		t.Fatalf("OldValue = %v (%T), want (*string)(nil)", c.OldValue, c.OldValue)
	}
	if c.NewValue != "EXT-2" {
		t.Fatalf("NewValue = %v, want EXT-2", c.NewValue)
	}
}

// TestIssuePatchFields_SourceContextFromNilToValue mirrors the Assignee
// case for SourceContext.
func TestIssuePatchFields_SourceContextFromNilToValue(t *testing.T) {
	old := baseRow()
	old.SourceContext = nil
	in := &PatchIssueInput{SourceContext: ptrStr("ctx-new")}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	c := findChange(changes, "sourceContext")
	if c == nil {
		t.Fatal("missing sourceContext change")
	}
	if c.OldValue != (*string)(nil) {
		t.Fatalf("OldValue = %v (%T), want (*string)(nil)", c.OldValue, c.OldValue)
	}
	if c.NewValue != "ctx-new" {
		t.Fatalf("NewValue = %v, want ctx-new", c.NewValue)
	}
}

// TestIssuePatchFields_PriorityChange covers a numeric scalar change —
// most-trafficked path post-R3.2b (priority used to have its own event
// type).
func TestIssuePatchFields_PriorityChange(t *testing.T) {
	old := baseRow()
	old.Priority = 3
	in := &PatchIssueInput{Priority: ptrInt(5)}
	changes := eventbus.DiffRegistry(IssuePatchFields, old, in)
	c := findChange(changes, "priority")
	if c == nil {
		t.Fatal("missing priority change")
	}
	if c.OldValue != 3 || c.NewValue != 5 {
		t.Fatalf("got %+v, want old=3 new=5", c)
	}
}
