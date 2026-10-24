package outbox

// private.go — sk-303.14, task T008; fixed by repair A/8.
//
// Here the outbox dispatcher asks whether a log row may be published. The
// SET of private types itself does not live here — see
// internal/notifications/private.go.
//
// # Why excluding from the selection is not enough
//
// The queue selects rows by "the publication stamp is empty", WITH NO
// filter on type, and the lag metric is counted by the same condition. A
// private row struck from the selection alone would stay in the queue
// forever: it would occupy the head of the batch, the lag metric would
// grow forever and the alert would never go quiet. So exclusion from
// publication must be accompanied by THE SAME terminal stamp published
// rows receive — see dispatcher.tickEvents.
//
// # What the row's shape closes, and what the registry closes
//
// A private row is written in the shape "entity — user, project empty,
// the issue (or a list of them) in the payload": issues.WritePrivateReviewEvent
// for a review mark and the flag, nudges.writeDigestEvent for a reminder
// digest. The shape itself takes the row out of the project subject and
// out of every in-API log reader. But the user subject
// `skein.user.<recipient>.*` it does NOT close: the live-feed bridge
// fans that namespace out to the recipient AND TO EVERY ADMINISTRATOR,
// so the author's private bookkeeping would go to every administrator.
// Exactly this channel, and only this one, is closed by the registry
// (INV-2, SC-003).
//
// # Why there is no list of our own here any more (CORRECTED MISTAKE)
//
// There was one: a hand-written map of two types with a comment promising
// that the guard was an integration assertion selecting private rows "BY
// SHAPE, not by type". The promise was false twice. The selection in that
// assertion went by `payload->>'issueId'` — in the SINGULAR — and a
// reminder digest writes `issueIds` in the plural and never matched; and
// neither digest type was in the map. Result: rows "user X has N
// unreviewed issues, here are their identifiers" were published onto
// `skein.user.<recipient>.*` and reached every connected administrator.
//
// Adding two values to the map would have handed the same mine to the
// next wave: a hand-written list is silent on a miss. So the set is owned
// by internal/notifications and owned in a way that a miss is IMPOSSIBLE
// — writers of the private shape accept an opaque
// notifications.PrivateEventType, which can only be obtained by
// registering in the set. What remains here is one question to the
// registry.

import "skein-api/internal/notifications"

// isPrivateEventType reports that a row of this type must not be published.
//
// A thin redirect to the registry, not a copy of the set: a copy is a
// second source of truth that drifts in silence.
func isPrivateEventType(eventType string) bool {
	return notifications.IsPrivateEventType(eventType)
}
