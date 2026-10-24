package push

// Push categories — the closed set of reasons Skein turns a bell notification
// into a web-push delivery. A producer stamps exactly one of these onto the
// recipient's notifications.push_category row (see migration 000028); the
// outbox dispatcher reads it back and, unless the user has opted the category
// out, fans the push to that user's registered devices.
//
// The category is the unit the user toggles in their push prefs — NOT the raw
// event type. Several event types can map to one category (and one type, like
// issue.updated, splits across "assigned" and "not pushed at all"), so the
// category is the stable, user-facing knob.
//
// Keep this registry extensible: a new push reason = a new const + a Category
// entry in Categories. Nothing else in the delivery path hardcodes the values.
const (
	// CategoryAssigned — the issue's assignee changed (or a new issue was
	// created already assigned) and the new assignee is not the actor.
	CategoryAssigned = "assigned"
	// CategoryReminder — a personal reminder (alarm) came due and rang.
	CategoryReminder = "reminder"
	// CategoryAccepted — the issue's creator is told their delegate accepted
	// (acknowledged) the task.
	CategoryAccepted = "accepted"
	// CategoryDelegatedStatus — the issue's creator is told that a task they
	// delegated (created_by = creator, assignee ≠ creator) changed status
	// (claimed → in_progress, closed, reopened, deferred, ...). Stamped on the
	// creator's issue.status_changed notification row.
	CategoryDelegatedStatus = "delegated_status"
)

// Category is one push-category registry entry: the stable key persisted in
// the DB / sent on the wire, plus a human-readable label for the prefs
// UI.
type Category struct {
	// Key is the stable identifier persisted in notifications.push_category
	// and user_notification_prefs.push_disabled_categories.
	Key string `json:"key"`
	// Label is the human-readable name shown in the push-prefs UI.
	Label string `json:"label"`
}

// Categories is the ordered registry of every known push category. Order is
// the display order for the prefs UI. Append new categories here.
var Categories = []Category{
	{Key: CategoryAssigned, Label: "Issue assigned"},
	{Key: CategoryReminder, Label: "Reminder"},
	{Key: CategoryAccepted, Label: "Issue accepted by assignee"},
	{Key: CategoryDelegatedStatus, Label: "Delegated issue status"},
}

// knownCategories indexes Categories by key for O(1) validation.
var knownCategories = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Categories))
	for _, c := range Categories {
		m[c.Key] = struct{}{}
	}
	return m
}()

// IsKnownCategory reports whether key is a registered push category. Used by
// the prefs PUT handler to reject unknown values with a 422.
func IsKnownCategory(key string) bool {
	_, ok := knownCategories[key]
	return ok
}
