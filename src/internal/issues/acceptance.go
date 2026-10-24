package issues

// Acceptance is the single decision about what a write does with the
// acceptance stamp (sk-303.13 / T021).
//
// Before this wave FOUR paths set the `accepted_at` stamp, and exactly one
// wrote the `issue.accepted` event — the explicit accept tool. The other
// three (claim, self-assign on create, self-assign on patch) moved the
// stamp silently, and the journal kept no trace: "who took this issue and
// when" was only knowable indirectly, and sometimes not at all. INV-5
// closes that hole with "the acceptance mark does not go from empty to
// filled without an acceptance event — on ANY path".
//
// The same place is where overshooting becomes a risk. An event is audit;
// a notification is UX (R-11); writing the event MUST NOT drag delivery
// along: claiming an unaccepted issue already notifies the creator of the
// status change, and a second "assignee accepted" would be a second
// notification for one user action — exactly what INV-6 forbids.
//
// So the decision is taken once, as a pure function, and both halves are
// checked without a database.

// AcceptanceOutcome is what a write must do with the acceptance stamp.
//
// Two independent bits, and neither is derived from the other: the event
// is written on EVERY transition (INV-5), and a delivery reason exists
// only for explicit accept (registry: "acceptance stamped automatically …
// there is no reason — the event is journal-only").
type AcceptanceOutcome struct {
	// WriteEvent — the "empty → filled" transition happened; write
	// `issue.accepted` to the journal.
	WriteEvent bool
	// Deliver — this event has a delivery reason, so fan-out must be
	// called. False means "journal only", not "we forgot".
	Deliver bool
}

// ClassifyAcceptance decides what one write does with the acceptance stamp.
//
//	hadStamp     — the row ALREADY had the stamp before this write;
//	stampsNow    — this write sets the stamp;
//	explicitTool — the write is the explicit accept tool (not claim,
//	               create-on-self, or self-assign on patch).
//
// Rules, and each is a separately observable case:
//
//   - there was no stamp and one appears → event. The explicit tool
//     additionally gives a delivery reason; automatic acceptance does not;
//   - the stamp was already set → no transition, no event. This also covers
//     a repeated accept: it is idempotent, the original acceptance moment
//     stays the truth, and a second event for the same fact does not appear;
//   - the write does not set the stamp → acceptance is not involved.
//
// The reverse transition is intentionally not considered here: the stamp
// is legally cleared when the issue is reassigned to another assignee, and
// the historical event about the previous acceptance stays (US-11). INV-5
// is one-directional.
func ClassifyAcceptance(hadStamp, stampsNow, explicitTool bool) AcceptanceOutcome {
	if hadStamp || !stampsNow {
		return AcceptanceOutcome{}
	}
	return AcceptanceOutcome{WriteEvent: true, Deliver: explicitTool}
}

// acceptanceReasonAuto lands in events.reason on an `issue.accepted` written
// by an automatic path. It is what tells an audit reader "the stamp was set
// by taking the task, not by pressing Accept" — the two are the same
// transition in the issues table and would otherwise be indistinguishable in
// the journal. The explicit tool writes no reason, matching every other
// event it emits.
const acceptanceReasonAuto = "auto-accept"
