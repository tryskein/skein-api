// Package eventbus owns the cross-package primitives that R3.2b uses to
// turn a generic PATCH on a domain entity into a parent + N children pair
// of rows in the events table.
//
// FieldDef captures one patch-able column of an entity together with the
// closure that decides whether the in-flight patch actually changed it.
// Per-entity registries (issues.IssuePatchFields, projects.ProjectPatchFields,
// etc.) collect the columns the matching PATCH method recognises.
//
// FieldChange is the runtime value. Each FieldDef.Diff that returns
// changed=true contributes one FieldChange to the registry walk, which
// the caller then hands to WriteHybridUpdateEvents. The combo is purely
// pure-Go — no DB / network / time dependencies — so each registry can be
// unit-tested without spinning up Postgres.
package eventbus

// FieldDef declares one patch-able field on an entity. GoName / JSONName /
// Kebab carry the three different casings the rest of the system needs:
// GoName for log lines, JSONName for events.field (which propagates into
// payload.changes[*].field on the parent row), and Kebab for the NATS
// subject builder (skein.events.project.<pid>.<entity>.field.<kebab>.<eid>
// in outbox.BuildSubject).
//
// Diff is a typed closure: each entity's registry casts oldRow / newPatch
// from interface{} to its own concrete *xxxRow / *PatchInput pair and
// returns the old/new tuple plus a changed bit. Returning changed=false
// short-circuits the field — no FieldChange is emitted, no child event
// row is written, no NATS subject is published. Returning changed=true
// commits the registry to writing one child event for the field, even if
// the old/new values happen to be JSON-equal (the closure is the source
// of truth for "did this caller mutate this column").
type FieldDef struct {
	GoName   string
	JSONName string
	Kebab    string
	Diff     func(oldRow, newPatch any) (changed bool, oldVal, newVal any)
}

// FieldChange is the runtime output of FieldDef.Diff for a field that
// actually changed. The triple flows into events.field / events.old_data /
// events.new_data on the child row plus payload.changes[] on the parent.
//
// OldValue / NewValue are interface{} so registries can return any
// json.Marshal-able shape — strings for plain fields, []ComponentPath for
// the components.paths slice, *time.Time for nullable timestamps. The
// hybrid writer marshals each to JSON exactly once when emitting the
// child row.
type FieldChange struct {
	Field    string `json:"field"`
	OldValue any    `json:"oldValue"`
	NewValue any    `json:"newValue"`
}

// DiffRegistry walks each FieldDef in registry, calling its Diff closure
// against the same (oldRow, newPatch) pair, and returns the slice of
// FieldChange values for fields that actually changed.
//
// Order is preserved from the registry slice — callers therefore control
// the serialization order of payload.changes (handy for golden-file
// tests). An empty registry or a no-op patch returns a nil slice; the
// hybrid writer treats that as "no events" and skips the INSERTs.
func DiffRegistry(registry []FieldDef, oldRow, newPatch any) []FieldChange {
	if len(registry) == 0 {
		return nil
	}
	changes := make([]FieldChange, 0, len(registry))
	for _, def := range registry {
		changed, oldVal, newVal := def.Diff(oldRow, newPatch)
		if !changed {
			continue
		}
		changes = append(changes, FieldChange{
			Field:    def.JSONName,
			OldValue: oldVal,
			NewValue: newVal,
		})
	}
	// Preserve nil for no-op patches (doc contract + empty-registry tests).
	if len(changes) == 0 {
		return nil
	}
	return changes
}
