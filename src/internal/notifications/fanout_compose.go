package notifications

// fanout_compose.go — T007 (sk-303.13): the PURE half of the write-side
// filter. It answers, for one (event, recipient) pair, whether a notification
// row has to exist at all and which of the two channels it belongs to.
//
// It is deliberately separate from the SQL in fanout.go. The composition rule
// is the load-bearing part of US-2 and it must be provable without a database:
// the four states of a channel pair, the fact that the two channels of one
// reason have DIFFERENT audiences (R-12), and the fact that a recipient found
// by both the subscription and the role path gets ONE row belonging to both
// channels — none of that needs a row in Postgres to be true or false.
//
// The rule itself, verbatim from US-2:
//
//	A row belongs to a channel if and only if (a) the recipient has this
//	channel of the reason enabled and (b) the recipient themselves falls
//	into the audience of exactly that channel.
//
// Both halves are required. A switched-on toggle alone is not enough — that is
// what keeps "Delegated issue status" from pushing to every watcher when
// its push audience is the delegating creator only.

// ChannelMembership is the composed answer for one candidate recipient: what
// the notification is about, and which channels the single row belongs to.
//
// A row is materialised if and only if at least one of Feed / Push is true
// (INV-1). Feed and Push are INDEPENDENT: a row can be feed-only, push-only or
// both, and the push half never implies visibility in any reading surface.
type ChannelMembership struct {
	// Outcome is the classifier's verdict for the pair. Only OutcomeReason
	// can produce a row; the two negative outcomes are kept apart on purpose
	// (see the Outcome docs) because OutcomeUnmapped is a configuration error
	// that has to be observable, while OutcomeAuditOnly is a decision.
	Outcome Outcome
	// Reason is set only when Outcome == OutcomeReason.
	Reason ReasonKey
	// Feed reports that the row belongs to the feed channel — it is visible in
	// every reading surface (feeds, counters, live stream, the agent reply).
	Feed bool
	// Push reports that the row is eligible for push delivery.
	Push bool
}

// Materialises reports whether this pair produces a notification row.
func (m ChannelMembership) Materialises() bool { return m.Feed || m.Push }

// Compose applies the composition rule to one (event, recipient) pair.
//
// prefs answers "what are this recipient's two channel states for this
// reason". It is a function rather than a map so the caller can supply the
// registry default for a reason the storage has no row for: an ABSENT row
// means "the narrow default is enough", never "everything is on" — see
// Store.GetReasonStates, which resolves exactly the same way.
//
// Compose does NOT decide whether the recipient is allowed to see the issue at
// all: membership in the project, sticky unwatch and self-action exclusion are
// recipient-selection gates and live in the fan-out query, because they decide
// who is a CANDIDATE. What is composed here is what a candidate gets.
func Compose(ev EventFacts, rc RecipientRelation, prefs func(ReasonKey) ReasonChannelState) ChannelMembership {
	cl := Classify(ev, rc)
	if cl.Outcome != OutcomeReason {
		return ChannelMembership{Outcome: cl.Outcome}
	}
	st := prefs(cl.Reason)
	return ChannelMembership{
		Outcome: cl.Outcome,
		Reason:  cl.Reason,
		// ChannelAudienceMatches already answers false for a channel the
		// reason does not have, so an inapplicable channel can never be
		// switched on here even if a stale storage row claims it is.
		Feed: st.Feed && ChannelAudienceMatches(cl.Reason, ChannelFeed, rc),
		Push: st.Push && ChannelAudienceMatches(cl.Reason, ChannelPush, rc),
	}
}

// everyRelation is the probe relation used to ask «does this event deliver to
// ANYBODY?» before the fan-out spends a query on recipient selection. Every
// flag is true, so the classifier answers with the event's most specific
// reason if the event delivers at all, and with OutcomeAuditOnly /
// OutcomeUnmapped if it does not.
//
// ⚠ "Every flag is true" no longer means "the widest recipient" since
// sk-489: for issue.status_changed with an outcome change the classifier branch gives
// the author NOTHING (R-7), and this probe includes the author, so on such a pair of
// facts the probe answers OutcomeNoReasonForRecipient even though the event
// is delivered to subscribers. The answer is handled in EventDeliveryOutcome below: it is not
// negative in the sense of this gate, and the fan-out must follow the selection.
//
// Using the classifier itself as the gate is the point: the previous
// implementation kept a SECOND table of notifiable event types beside the
// registry, and the two drifted — the fan-out materialised rows for claim,
// close, archival and dependencies, which the registry calls audit-only.
//
// ⚠ EVERY field must be true, and this literal is handwritten. A relation
// added to RecipientRelation and forgotten here silently narrows the probe:
// an event that delivers ONLY to that new relation would answer
// no_reason_for_recipient — neither of the two negative answers this gate is
// documented to produce — and the fan-out would skip a pool it should have
// selected. That is not left to review: allRelations() in
// reason_corpus_test.go rebuilds the same value by reflection and
// TestEveryRelationProbeIsComplete compares the two.
var everyRelation = RecipientRelation{
	IsSubscriber:      true,
	IsNewAssignee:     true,
	IsAssignee:        true,
	IsCreator:         true,
	IsActor:           true,
	IsMentioned:       true,
	IsReminderOwner:   true,
	IsDigestRecipient: true,
	IsInvitationParty: true,
}

// EventDeliveryOutcome reports what the registry thinks of an event type
// BEFORE any recipient is known:
//
//   - OutcomeReason — the event delivers to at least one kind of recipient;
//   - OutcomeAuditOnly — journal-only by decision, no signal is due;
//   - OutcomeUnmapped — the type claims a delivery path the registry cannot
//     name. Callers on a delivery path must make this observable;
//   - OutcomeNoReasonForRecipient — the probe itself fell into a rule that
//     SUPPRESSES a recipient (sk-489: the author of an issue whose closing
//     also changed the outcome). It says nothing about the event's delivery
//     as a whole, so it is NOT one of the two negative answers and the
//     fan-out must proceed to real recipient selection on it.
//
// It exists so the fan-out can skip recipient selection entirely for the two
// negative answers instead of selecting a pool and then discarding it.
func EventDeliveryOutcome(ev EventFacts) Outcome {
	return Classify(ev, everyRelation).Outcome
}
