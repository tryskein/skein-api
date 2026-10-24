package eventbus

import "testing"

// fakeOldRow / fakePatch are sufficient for exercising DiffRegistry's
// orchestration (does it call each Diff exactly once, does it skip
// changed=false, does it preserve order). Per-entity behavioural tests
// for IssuePatchFields / ProjectPatchFields / ComponentPatchFields /
// UserPatchFields live in their respective packages so each registry
// owns its semantics-as-code.
type fakeOldRow struct {
	A, B, C string
}

type fakePatch struct {
	A, B, C *string
}

// fakeRegistry mirrors the per-entity FieldDef pattern but stays in this
// package so eventbus has zero outside dependencies in its tests.
var fakeRegistry = []FieldDef{
	{
		GoName: "A", JSONName: "a", Kebab: "a",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*fakeOldRow)
			in := newPatch.(*fakePatch)
			if in.A == nil || *in.A == old.A {
				return false, nil, nil
			}
			return true, old.A, *in.A
		},
	},
	{
		GoName: "B", JSONName: "b", Kebab: "b",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*fakeOldRow)
			in := newPatch.(*fakePatch)
			if in.B == nil || *in.B == old.B {
				return false, nil, nil
			}
			return true, old.B, *in.B
		},
	},
	{
		GoName: "C", JSONName: "c", Kebab: "c",
		Diff: func(oldRow, newPatch any) (bool, any, any) {
			old := oldRow.(*fakeOldRow)
			in := newPatch.(*fakePatch)
			if in.C == nil || *in.C == old.C {
				return false, nil, nil
			}
			return true, old.C, *in.C
		},
	},
}

func ptrTo(s string) *string { return &s }

func TestDiffRegistry_NoChanges(t *testing.T) {
	old := &fakeOldRow{A: "x", B: "y", C: "z"}
	in := &fakePatch{} // all nil
	changes := DiffRegistry(fakeRegistry, old, in)
	if len(changes) != 0 {
		t.Fatalf("got %d changes, want 0", len(changes))
	}
	// Nil, not an empty slice: the doc contract is "no-op patch returns a
	// nil slice" and the hybrid writer branches on it to skip the INSERTs.
	// A len-only assertion would pass for the empty-but-non-nil slice that
	// the pre-allocated `make(..., 0, len(registry))` produces, so assert
	// nil explicitly (audit sk-194, findings rev-A20-016 / opt-A20-013).
	if changes != nil {
		t.Fatalf("no-op patch returned a non-nil empty slice; want nil")
	}
}

func TestDiffRegistry_PointerSetSameValue_NoChange(t *testing.T) {
	old := &fakeOldRow{A: "x", B: "y", C: "z"}
	in := &fakePatch{A: ptrTo("x"), B: ptrTo("y")}
	changes := DiffRegistry(fakeRegistry, old, in)
	if len(changes) != 0 {
		t.Fatalf("expected zero changes when values match, got %d", len(changes))
	}
}

func TestDiffRegistry_OrderPreserved(t *testing.T) {
	old := &fakeOldRow{A: "x", B: "y", C: "z"}
	in := &fakePatch{C: ptrTo("z2"), A: ptrTo("a2"), B: ptrTo("b2")}
	changes := DiffRegistry(fakeRegistry, old, in)
	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3", len(changes))
	}
	want := []string{"a", "b", "c"}
	for i, fc := range changes {
		if fc.Field != want[i] {
			t.Fatalf("changes[%d].Field = %q, want %q (order should mirror registry, not patch)", i, fc.Field, want[i])
		}
	}
}

func TestDiffRegistry_OldNewValuesPropagated(t *testing.T) {
	old := &fakeOldRow{A: "x", B: "y", C: "z"}
	in := &fakePatch{B: ptrTo("y2")}
	changes := DiffRegistry(fakeRegistry, old, in)
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	c := changes[0]
	if c.Field != "b" || c.OldValue != "y" || c.NewValue != "y2" {
		t.Fatalf("got %+v, want {Field:b OldValue:y NewValue:y2}", c)
	}
}

func TestDiffRegistry_EmptyRegistry(t *testing.T) {
	changes := DiffRegistry(nil, &fakeOldRow{}, &fakePatch{})
	if changes != nil {
		t.Fatalf("got %v, want nil", changes)
	}
}
