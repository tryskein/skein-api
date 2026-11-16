package redact

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// The registry of reference fields of event data (sk-1007 US-2, D2, K4,
// K5): the machine fields of an event row that name ANOTHER task or a
// project — an end of a link, the parent at creation, the parents of a
// move — by event type. It is closed: a type outside it, and every field
// outside it, is never touched. User-entered text (titles, descriptions,
// payload.changes of issue.updated) is not a reference field, even when it
// contains a string equal to a hidden id (R-3).
//
// The registry was checked against every writer of the types the activity
// feeds show (issues.DefaultActivityEventTypes) and of the notifiable types
// (notifications/reasons.go#Classify):
//
//   - dependency.created / dependency.deleted —
//     dependencies/events.go#WriteDependencyEvent: entity_id is the
//     composite "from::to::type"; the data object (new_data on created,
//     old_data on deleted) carries fromId, toId and the ends' projects
//     fromProjectId, toProjectId. A close target (duplicate / superseded)
//     is written as such an edge, so it is covered here.
//   - issue.created — the snapshot in issues/store.go#CreateIssue:
//     new_data.parentId (the parent named at creation, possibly of another
//     project). new_data.projectId is the row's own project, the one the
//     reader sees for the row to be given at all; it is not a reference.
//   - issue.reparented — dependencies/events.go#WriteReparentEvent:
//     old_data.parentIds (array), new_data.parentId,
//     payload.oldParentIds (array), payload.newParentId.
//
// No other writer puts the id of another task or another project into an
// event: issue.updated / issue.field_changed.* carry the patch fields of
// issues/fields.go#IssuePatchFields (text, people, dates — no issue id);
// issue.status_changed, issue.closed and issue.outcome_changed carry the
// outcome payload of issues/close_outcome.go#closeOutcomePayload (strings
// only); issue.claimed / unclaimed / accepted carry the actor and the
// session; comment.created, the mentions, attachment.added and
// reminder.rang name only their own issue, comment, attachment or
// reminder. eventbus/hybrid.go#WriteHybridUpdateEvents writes the same
// patch changes for issues, projects and components.
//
// A reference field with an unexpected shape (a number, an object, a list
// where one id is expected) is not a known id: it is treated as a hidden
// one — replaced by the marker (task) or null (project) — so a writer that
// drifts can never leak an identifier through this path (fail-closed; the
// spec does not name this case).

// EventRow is the raw part of an event row the registry reads and edits:
// the event type and the columns before any hydration.
type EventRow struct {
	Type     string
	EntityID string
	OldData  []byte
	NewData  []byte
	Payload  []byte
}

// EventRefs is the set of references collected from event rows: task ids
// and project ids, each once.
type EventRefs struct {
	issues   map[string]struct{}
	projects map[uuid.UUID]struct{}
}

// NewEventRefs returns an empty set.
func NewEventRefs() *EventRefs {
	return &EventRefs{issues: map[string]struct{}{}, projects: map[uuid.UUID]struct{}{}}
}

// IssueIDs returns the collected task ids.
func (r *EventRefs) IssueIDs() []string {
	out := make([]string, 0, len(r.issues))
	for id := range r.issues {
		out = append(out, id)
	}
	return out
}

// ProjectIDs returns the collected project ids.
func (r *EventRefs) ProjectIDs() []uuid.UUID {
	out := make([]uuid.UUID, 0, len(r.projects))
	for id := range r.projects {
		out = append(out, id)
	}
	return out
}

// Empty reports whether nothing was collected.
func (r *EventRefs) Empty() bool { return len(r.issues) == 0 && len(r.projects) == 0 }

func (r *EventRefs) addIssue(id string) {
	if id != "" {
		r.issues[id] = struct{}{}
	}
}

func (r *EventRefs) addProject(raw string) {
	if id, err := uuid.Parse(raw); err == nil {
		r.projects[id] = struct{}{}
	}
}

// EventVisibility is what the reader sees now among the collected
// references: the visible task ids (P-VIS) and the visible project ids
// (with the key's scope). A reference absent from a set is hidden —
// whatever the reason (no access, outside the key, deleted; INV-4).
type EventVisibility struct {
	Issues   map[string]struct{}
	Projects map[uuid.UUID]struct{}
}

func (v EventVisibility) issue(id string) bool {
	_, ok := v.Issues[id]
	return ok
}

func (v EventVisibility) project(raw string) bool {
	id, err := uuid.Parse(raw)
	if err != nil {
		return false
	}
	_, ok := v.Projects[id]
	return ok
}

// depEnds are the reference keys of a dependency event's data object: the
// two ends and their projects.
var depEnds = [2]struct{ id, project string }{
	{"fromId", "fromProjectId"},
	{"toId", "toProjectId"},
}

// CollectEventRefs adds the references of row's registry fields to refs.
// Nothing outside the registry is read.
func CollectEventRefs(row EventRow, refs *EventRefs) {
	switch row.Type {
	case "dependency.created", "dependency.deleted":
		if parts, ok := linkParts(row.EntityID); ok {
			refs.addIssue(parts[0])
			refs.addIssue(parts[1])
		}
		for _, data := range [][]byte{row.OldData, row.NewData} {
			obj := decodeObject(data)
			for _, end := range depEnds {
				collectString(obj[end.id], refs.addIssue)
				collectString(obj[end.project], refs.addProject)
			}
		}
	case "issue.created":
		collectString(decodeObject(row.NewData)["parentId"], refs.addIssue)
	case "issue.reparented":
		collectStrings(decodeObject(row.OldData)["parentIds"], refs.addIssue)
		collectString(decodeObject(row.NewData)["parentId"], refs.addIssue)
		payload := decodeObject(row.Payload)
		collectStrings(payload["oldParentIds"], refs.addIssue)
		collectString(payload["newParentId"], refs.addIssue)
	}
}

// ApplyEventRefs rewrites row's registry fields by v and reports whether
// anything changed (R-1, R-2, K5):
//
//   - a hidden task id becomes HiddenMarker, also as a part of the
//     composite "from::to::type" of a dependency event;
//   - a project field becomes JSON null — the key stays — when its project
//     is hidden or when the end it belongs to is hidden;
//   - arrays keep their length, each hidden element a marker.
//
// A column without a hidden reference keeps its exact bytes; a rewritten
// column keeps every other key and value (only the object's key order may
// change).
func ApplyEventRefs(row *EventRow, v EventVisibility) bool {
	changed := false
	switch row.Type {
	case "dependency.created", "dependency.deleted":
		if parts, ok := linkParts(row.EntityID); ok {
			p0, c0 := hideID(parts[0], v)
			p1, c1 := hideID(parts[1], v)
			if c0 || c1 {
				row.EntityID = p0 + "::" + p1 + "::" + parts[2]
				changed = true
			}
		} else if row.EntityID != HiddenMarker {
			// Not the composite "from::to::type": no part of it is a known
			// id, so none can be shown as visible (fail-closed).
			row.EntityID = HiddenMarker
			changed = true
		}
		for _, data := range []*[]byte{&row.OldData, &row.NewData} {
			changed = editObject(data, func(obj map[string]json.RawMessage) bool {
				c := false
				for _, end := range depEnds {
					raw, has := obj[end.id]
					endHidden := false
					if has {
						out, hidden, ch := applyString(raw, v)
						endHidden = hidden
						if ch {
							obj[end.id] = out
							c = true
						}
					}
					if praw, ok := obj[end.project]; ok && !isNull(praw) {
						if endHidden || !projectVisible(praw, v) {
							obj[end.project] = json.RawMessage("null")
							c = true
						}
					}
				}
				return c
			}) || changed
		}
	case "issue.created":
		changed = editObject(&row.NewData, func(obj map[string]json.RawMessage) bool {
			return applyKey(obj, "parentId", applyString, v)
		})
	case "issue.reparented":
		c1 := editObject(&row.OldData, func(obj map[string]json.RawMessage) bool {
			return applyKey(obj, "parentIds", applyStrings, v)
		})
		c2 := editObject(&row.NewData, func(obj map[string]json.RawMessage) bool {
			return applyKey(obj, "parentId", applyString, v)
		})
		c3 := editObject(&row.Payload, func(obj map[string]json.RawMessage) bool {
			a := applyKey(obj, "oldParentIds", applyStrings, v)
			b := applyKey(obj, "newParentId", applyString, v)
			return a || b
		})
		changed = c1 || c2 || c3
	}
	return changed
}

// linkParts splits the composite "from::to::type" of a dependency event;
// ok is false for any other shape (fewer or more parts).
func linkParts(entityID string) ([]string, bool) {
	parts := strings.Split(entityID, "::")
	return parts, len(parts) == 3
}

// hideID returns the marker for a hidden non-empty id.
func hideID(id string, v EventVisibility) (string, bool) {
	if id == "" || v.issue(id) {
		return id, false
	}
	return HiddenMarker, true
}

var markerJSON = mustMarshal(HiddenMarker)

func mustMarshal(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return b
}

// applyString edits one id value: null and "" stay; a visible id stays; a
// hidden id — or a value that is not a string — becomes the marker.
// hidden reports whether the value named a hidden task.
func applyString(raw json.RawMessage, v EventVisibility) (out json.RawMessage, hidden, changed bool) {
	if isNull(raw) {
		return raw, false, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return markerJSON, true, true
	}
	if _, c := hideID(s, v); c {
		return markerJSON, true, true
	}
	return raw, false, false
}

// applyStrings edits a list of ids element by element, keeping its length;
// a value that is not a list is edited as one id.
func applyStrings(raw json.RawMessage, v EventVisibility) (json.RawMessage, bool, bool) {
	if isNull(raw) {
		return raw, false, false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return applyString(raw, v)
	}
	changed, anyHidden := false, false
	for i, e := range elems {
		out, hidden, c := applyString(e, v)
		if c {
			elems[i] = out
			changed = true
		}
		anyHidden = anyHidden || hidden
	}
	if !changed {
		return raw, anyHidden, false
	}
	b, err := json.Marshal(elems)
	if err != nil {
		return markerJSON, true, true
	}
	return b, anyHidden, true
}

func applyKey(obj map[string]json.RawMessage, key string,
	f func(json.RawMessage, EventVisibility) (json.RawMessage, bool, bool), v EventVisibility) bool {
	raw, ok := obj[key]
	if !ok {
		return false
	}
	out, _, c := f(raw, v)
	if c {
		obj[key] = out
	}
	return c
}

func projectVisible(raw json.RawMessage, v EventVisibility) bool {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	return v.project(s)
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// decodeObject reads a JSON object column; anything else reads as empty.
func decodeObject(data []byte) map[string]json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil
	}
	return obj
}

// editObject applies edit to the object in *data and re-encodes it only
// when edit changed something; otherwise *data keeps its bytes.
func editObject(data *[]byte, edit func(map[string]json.RawMessage) bool) bool {
	obj := decodeObject(*data)
	if obj == nil || !edit(obj) {
		return false
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return false
	}
	*data = b
	return true
}

func collectString(raw json.RawMessage, add func(string)) {
	if isNull(raw) {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		add(s)
	}
}

func collectStrings(raw json.RawMessage, add func(string)) {
	if isNull(raw) {
		return
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		collectString(raw, add)
		return
	}
	for _, e := range list {
		collectString(e, add)
	}
}
