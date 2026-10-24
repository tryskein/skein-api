package issues

import (
	"strings"
	"time"

	"skein-api/internal/eventbus"
)

// issueFieldDiff is the private typed Diff signature for IssuePatchFields.
// Each entry casts once at the eventbus boundary via issueDiff; the
// closures themselves never touch any.
type issueFieldDiff func(old *issueRow, in *PatchIssueInput) (changed bool, oldVal, newVal any)

// issueDiff adapts a typed issueFieldDiff to eventbus.FieldDef.Diff
// (func(oldRow, newPatch any)). Wrong concrete types yield changed=false
// (no field event) — defensive; production callers always pass
// *issueRow / *PatchIssueInput.
func issueDiff(f issueFieldDiff) func(oldRow, newPatch any) (bool, any, any) {
	return func(oldRow, newPatch any) (bool, any, any) {
		old, ok1 := oldRow.(*issueRow)
		in, ok2 := newPatch.(*PatchIssueInput)
		if !ok1 || !ok2 {
			return false, nil, nil
		}
		return f(old, in)
	}
}

// IssuePatchFields is the canonical, ordered list of patch-able columns
// PatchIssue recognises (mirrors PatchIssueInput in types.go). Each
// FieldDef.Diff is a typed closure that downcasts oldRow / newPatch to
// (*issueRow, *PatchIssueInput) and returns the (changed, oldVal, newVal)
// triple for that column.
//
// The "newPatch's pointer is non-nil AND the value differs from oldRow"
// rule matches the legacy buildPatchDiff semantics: a caller that omits
// the field (nil pointer) leaves the column alone, and a caller that
// explicitly writes the same value the row already holds writes no event.
//
// NULL/UNSET SHAPE (uniform across all 13 fields):
//
//	OldValue is the raw underlying value — for nullable columns
//	(Assignee / ExternalRef / SourceContext / EstimatedMinutes / DueAt /
//	DeferUntil) it is a typed Go pointer that serialises to JSON `null`
//	when the previous row had no value. Subscribers MUST treat `null`
//	as "was unset"; they MUST NOT also branch on the empty string ""
//	for that semantic. Non-nullable string columns (Title / Description
//	/ Type / Priority / Design / AcceptanceCriteria / Notes) carry the
//	plain string value and a deliberately empty old value serialises as
//	"" (the column itself is NOT NULLABLE in the schema, so "" is the
//	natural zero — distinct from null).
//
// Order is significant only for golden-file determinism — payload.changes
// on the parent event preserves insertion order.
//
// JSONName / Kebab keep camelCase / kebab-case mappings explicit:
//
//	acceptanceCriteria → acceptance-criteria  (NATS subject infix)
//	estimatedMinutes   → estimated-minutes
//	dueAt              → due-at
//	deferUntil         → defer-until
//	externalRef        → external-ref
//	sourceContext      → source-context
//
// outbox.BuildSubject re-derives the kebab form from JSONName at runtime,
// so the Kebab column here is documentation rather than a load-bearing
// override; we still record it so a registry consumer that wants the
// kebab form does not need a second helper.
var IssuePatchFields = []eventbus.FieldDef{
	{
		GoName: "Title", JSONName: "title", Kebab: "title",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.Title == nil || *in.Title == old.Title {
				return false, nil, nil
			}
			return true, old.Title, *in.Title
		}),
	},
	{
		GoName: "Description", JSONName: "description", Kebab: "description",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.Description == nil || *in.Description == old.Description {
				return false, nil, nil
			}
			return true, old.Description, *in.Description
		}),
	},
	{
		GoName: "Type", JSONName: "type", Kebab: "type",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.Type == nil || *in.Type == old.Type {
				return false, nil, nil
			}
			return true, old.Type, *in.Type
		}),
	},
	{
		GoName: "Priority", JSONName: "priority", Kebab: "priority",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.Priority == nil || *in.Priority == old.Priority {
				return false, nil, nil
			}
			return true, old.Priority, *in.Priority
		}),
	},
	{
		GoName: "Assignee", JSONName: "assignee", Kebab: "assignee",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.Assignee == nil {
				return false, nil, nil
			}
			// Equality on a nullable string: nil-old ≠ "" by design — a
			// transition from "no assignee" to "" is a real change.
			//
			// Compare case-insensitively so a patch that only changes
			// the case of the assignee string ("Alice" → "alice") does
			// NOT emit an issue.field_changed audit row. The username
			// resolver in notifications.ResolveAssigneeUserID is
			// case-insensitive (LOWER=LOWER on users.username), so the
			// two strings address the same user and the audit/state-
			// reset path in PatchIssue treats them as equal — keeping
			// the audit log consistent with the acceptance-state gate.
			oldNil := old.Assignee == nil
			if !oldNil && strings.EqualFold(*old.Assignee, *in.Assignee) {
				return false, nil, nil
			}
			// Emit raw pointer so JSON null is preserved on unset old
			// (uniform with EstimatedMinutes / DueAt / DeferUntil).
			return true, old.Assignee, *in.Assignee
		}),
	},
	{
		GoName: "Design", JSONName: "design", Kebab: "design",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.Design == nil || *in.Design == old.Design {
				return false, nil, nil
			}
			return true, old.Design, *in.Design
		}),
	},
	{
		GoName: "AcceptanceCriteria", JSONName: "acceptanceCriteria", Kebab: "acceptance-criteria",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.AcceptanceCriteria == nil || *in.AcceptanceCriteria == old.AcceptanceCriteria {
				return false, nil, nil
			}
			return true, old.AcceptanceCriteria, *in.AcceptanceCriteria
		}),
	},
	{
		GoName: "Notes", JSONName: "notes", Kebab: "notes",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.Notes == nil || *in.Notes == old.Notes {
				return false, nil, nil
			}
			return true, old.Notes, *in.Notes
		}),
	},
	{
		GoName: "EstimatedMinutes", JSONName: "estimatedMinutes", Kebab: "estimated-minutes",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.EstimatedMinutes == nil {
				return false, nil, nil
			}
			if intPtrEq(in.EstimatedMinutes, old.EstimatedMinutes) {
				return false, nil, nil
			}
			return true, old.EstimatedMinutes, *in.EstimatedMinutes
		}),
	},
	{
		GoName: "DueAt", JSONName: "dueAt", Kebab: "due-at",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.DueAt == nil {
				return false, nil, nil
			}
			if timePtrEq(in.DueAt, old.DueAt) {
				return false, nil, nil
			}
			return true, old.DueAt, *in.DueAt
		}),
	},
	{
		GoName: "DeferUntil", JSONName: "deferUntil", Kebab: "defer-until",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.DeferUntil == nil {
				return false, nil, nil
			}
			if timePtrEq(in.DeferUntil, old.DeferUntil) {
				return false, nil, nil
			}
			return true, old.DeferUntil, *in.DeferUntil
		}),
	},
	{
		GoName: "ExternalRef", JSONName: "externalRef", Kebab: "external-ref",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.ExternalRef == nil {
				return false, nil, nil
			}
			oldNil := old.ExternalRef == nil
			if !oldNil && *old.ExternalRef == *in.ExternalRef {
				return false, nil, nil
			}
			// Emit raw pointer — JSON null on unset old.
			return true, old.ExternalRef, *in.ExternalRef
		}),
	},
	{
		GoName: "SourceContext", JSONName: "sourceContext", Kebab: "source-context",
		Diff: issueDiff(func(old *issueRow, in *PatchIssueInput) (bool, any, any) {
			if in.SourceContext == nil {
				return false, nil, nil
			}
			oldNil := old.SourceContext == nil
			if !oldNil && *old.SourceContext == *in.SourceContext {
				return false, nil, nil
			}
			// Emit raw pointer — JSON null on unset old.
			return true, old.SourceContext, *in.SourceContext
		}),
	},
}

// intPtrEq reports whether a non-nil patch pointer is equal to an
// (optionally nil) old pointer. Both nil counts as equal; nil-vs-value
// counts as a change.
func intPtrEq(newPtr, old *int) bool {
	if newPtr == nil && old == nil {
		return true
	}
	if newPtr == nil || old == nil {
		return false
	}
	return *newPtr == *old
}

// timePtrEq compares two *time.Time using Equal so timezone differences
// on otherwise-equal instants don't generate spurious events.
func timePtrEq(newPtr, old *time.Time) bool {
	if newPtr == nil && old == nil {
		return true
	}
	if newPtr == nil || old == nil {
		return false
	}
	return newPtr.Equal(*old)
}
