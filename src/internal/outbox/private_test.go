package outbox

// private_test.go — sk-303.14 T008, the unit gate of the closed set of
// private types. Coverage-matrix rows 32–34 are proven by integration
// (dispatcher_private_test.go); here the set itself is checked — what can
// be checked without a database and without the bus.

import (
	"testing"

	"skein-api/internal/notifications"
)

// TestPrivateEventTypes_NeverReachTheFeed — a private type does not produce
// a FEED notification entry for any recipient.
//
// ⚠ CORRECTED MISTAKE. Until repair A/8 this asserted something stronger
// and WRONG: "a private type has no delivery path at all" (HasDeliveryPath).
// The claim was true while the set held only the review.* pair, and became
// false the moment the set was joined by reminder digests: a digest IS
// DELIVERED — by push — and that is exactly why it exists. "Private" here
// means "not published onto the live-feed BUS", not "delivered to nobody".
//
// What remains load-bearing and why the assertion is not tautological: it
// joins two independent decisions — the set of types unpublished onto the
// bus (this package) and the applicability of the FEED channel (the reason
// registry). Both halves of the mechanic's privacy say the same thing:
// neither the project's live feed nor the notification feed shows these
// rows (INV-2, INV-7). A type whose feed channel is declared applicable
// will go red here — and that is exactly the gate INV-7 did not have: the
// feed_channel value of a delivery row is derived from the registry, and
// the registry must keep saying "not applicable".
//
// DIFFERENT recipient relations are walked, not one: the classifier picks
// a reason by relation, and an assertion on a single relation would say
// nothing about the others.
func TestPrivateEventTypes_NeverReachTheFeed(t *testing.T) {
	types := notifications.PrivateEventTypes()
	if len(types) == 0 {
		t.Fatal("the set of private types is empty — then unpublished does nothing, " +
			"and the assertions below check emptiness, not a decision")
	}

	all := notifications.RecipientRelation{
		IsSubscriber: true, IsNewAssignee: true, IsAssignee: true,
		IsCreator: true, IsActor: true, IsMentioned: true,
		IsReminderOwner: true, IsDigestRecipient: true,
	}
	relations := []struct {
		name string
		rc   notifications.RecipientRelation
	}{
		{"stranger", notifications.RecipientRelation{}},
		{"watcher", notifications.RecipientRelation{IsSubscriber: true}},
		{"author", notifications.RecipientRelation{IsCreator: true}},
		{"assignee", notifications.RecipientRelation{IsAssignee: true, IsNewAssignee: true}},
		{"mentioned", notifications.RecipientRelation{IsMentioned: true}},
		{"digest recipient", notifications.RecipientRelation{IsDigestRecipient: true}},
		{"every relation at once", all},
	}

	for _, eventType := range types {
		for _, rel := range relations {
			cl := notifications.Classify(
				notifications.EventFacts{Type: eventType}, rel.rc)
			switch cl.Outcome {
			case notifications.OutcomeAuditOnly, notifications.OutcomeNoReasonForRecipient:
				// Silence is declared — that is privacy.
			case notifications.OutcomeReason:
				spec, ok := notifications.LookupReason(cl.Reason)
				if !ok {
					t.Errorf("type %q (%s) mapped onto reason %q, which is not in the registry",
						eventType, rel.name, cl.Reason)
					continue
				}
				if spec.Feed.Applicable {
					t.Errorf("type %q (%s) mapped onto reason %q with an APPLICABLE feed "+
						"channel. A row unpublished onto the live feed but sitting in the "+
						"notification feed is the same private bookkeeping, just by another "+
						"channel (INV-2); for reminders it is also a direct violation of "+
						"INV-7 \"reminders do not create notification-feed entries\"",
						eventType, rel.name, cl.Reason)
				}
			case notifications.OutcomeUnmapped:
				t.Errorf("type %q (%s) is unknown to the reason registry: private silence "+
					"would look like a configuration error, not a decision",
					eventType, rel.name)
			}
		}
	}
}

// TestIsPrivateEventType_DoesNotOverreach — the predicate cuts off exactly
// the declared types and nothing beyond.
//
// This assertion is why a separate test exists: an implementation via a
// prefix ("everything that starts with review.") or a substring would pass
// any check on the two known types and still silently cut a future public
// type with the same root out of the live feed. Ordinary types that must
// stay publishable are listed here.
func TestIsPrivateEventType_DoesNotOverreach(t *testing.T) {
	for _, eventType := range []string{
		"issue.created",
		"issue.updated",
		"issue.closed",
		"issue.field_changed",
		"comment.created",
		"user.updated",
		// sk-489: a close-outcome change is an ORDINARY public type. It
		// resembles the private review.* pair in shape (an alias of a constant
		// from notifications, a marker field on a closed issue), and that is exactly
		// why it is named here explicitly: a predicate that classed it as private
		// would cut from the live feed an event that the card, the rows
		// and the parent's summary subscribe to.
		notifications.EventTypeOutcomeChanged,
		// Neighbours by root and by spelling: if the predicate starts answering
		// "private" for any of them, the live feed would lose a public
		// event, and the test will show it.
		"review",
		"review.",
		"review.requested",
		"issue.review_marked",
		"",
	} {
		if isPrivateEventType(eventType) {
			t.Errorf("isPrivateEventType(%q) = true — the predicate is wider than the declared set; "+
				"that is how a public event silently disappears from the live feed", eventType)
		}
	}

	// The other half: the predicate really does catch the declared types.
	// Without it "nothing is private" would pass the test above wholesale.
	// The list is taken FROM THE REGISTRY, not typed here: a typed list
	// would repeat the same mistake that leaked reminder digests to
	// administrators — it would be silent about a type that was forgotten.
	registered := notifications.PrivateEventTypes()
	if len(registered) == 0 {
		t.Fatal("the registry of private types is empty — the other half would check emptiness")
	}
	for _, eventType := range registered {
		if !isPrivateEventType(eventType) {
			t.Errorf("isPrivateEventType(%q) = false — the author's private bookkeeping "+
				"will go onto the live-feed user subject, which the bridge fans out "+
				"to the recipient and every administrator (INV-2, SC-003)", eventType)
		}
	}
}
