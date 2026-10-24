package review

// defaults.go — sk-303.14. Factory values of the review workflow
// that do not belong to the schedule (the schedule lives in schedule.go).
//
// Why a built-in default MUST exist as a CONSTANT IN CODE, not only as a column
// DEFAULT. Rows in user_notification_prefs are created lazily: a user who has
// never saved notification settings has no row at all, and every upsert that
// creates one names its columns explicitly. So a schema-DEFAULT is unreachable
// in practice, and the "built-in default" is the magnitude the reader substitutes
// when the row is missing. The same trap already cost the first release a drift between
// the settings form and the fan-out (see notifications.Store.GetPrefs, sk-343):
// there it is written "all three must move together".
//
// Hence the rule: the column DEFAULT in migration 000044 and the constants
// below are ONE decision in two places, and they change together.

const (
	// DefaultMechanicEnabled — built-in default of the review workflow's master
	// switch (INV-6): the mechanic is ON.
	//
	// The direction is chosen by the same reasoning as the flag, but a
	// mistake here is more expensive: the switch removes the mechanic for a
	// person WHOLESALE — the set, its display, and the reminders on it. A
	// default of "off" would mean INV-6 running in reverse: everyone who has
	// never touched settings (that is, everyone) has no mechanic, and none of
	// the wave's remaining tasks is observable. Turning it off is a conscious
	// action of the person, not the starting state.
	//
	// A reader with no settings row substitutes THIS constant; the
	// review_mechanic_enabled column DEFAULT in 000044 is its second instance
	// for rows created by a foreign upsert that does not name the column. The
	// instances must agree, and their agreement is checked by the T011 gate,
	// not by attentiveness.
	DefaultMechanicEnabled = true

	// DefaultReviewRequired — built-in default of the personal default for the
	// review_required flag, applied on every issue-creation path where the
	// flag is not passed explicitly (US-3).
	//
	// "Required", not "not required": the mechanic exists for the author, who
	// otherwise loses closed results from view, and with it off by default it
	// would work for nobody except those who already knew about it. Historical
	// issues do NOT get this value — the migration leaves them "not required"
	// (SC-015), otherwise on ship day the entire archive of closed delegated
	// issues would flood the counters and reminders at once.
	DefaultReviewRequired = true
)
