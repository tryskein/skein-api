package notifications

// private.go — sk-303.14, repair A/8.
//
// REGISTRY OF PRIVATE EVENT TYPES: journal rows that must leave the queue
// unpublished, never having entered the bus (INV-2, SC-003).
//
// # What was broken and why a list is not enough
//
// Until this fix the set lived as a handwritten map in internal/outbox/private.go,
// and the forgetfulness guard was a test that supposedly selected private
// rows "by shape". The promise was false twice:
//
//   - that test selected on `payload->>'issueId'` — SINGULAR — while a
//     reminder digest writes `issueIds` in the plural and never matched;
//   - the types nudge.unaccepted_digest / nudge.unreviewed_digest were not
//     in the map, and the dispatcher published them into the namespace
//     skein.user.<addressee>.*, which the live-feed bridge (skein-web,
//     NatsBridge.ForwardUserEvent) hands to the addressee AND EVERY
//     ADMINISTRATOR. The creator's private bookkeeping — "user X has
//     unreviewed issues …" — went to every connected administrator.
//
// Adding two values to the map would have planted the same mine for the next
// wave: a handwritten list is silent on a miss.
//
// # How forgetfulness is made impossible
//
// PrivateEventType is OPAQUE and cannot be assembled from outside: its only
// constructor is the unexported registerPrivate, which at the same moment
// inserts the name into the set. Writers of the private row FORM
// (issues.WritePrivateReviewEvent for the mark and the flag,
// nudges.writeDigestEvent for the digest) accept PrivateEventType, not
// string. Hence:
//
//	introducing a private type and forgetting to put it in the set is
//	IMPOSSIBLE: there is nowhere else to take a value the writer will
//	accept, and obtaining the value IS inserting it into the set.
//
// A mutation "a third private type past the set" never reaches a test run —
// it does not compile, and that is the earliest red possible.
//
// # Why the set is here, not in outbox
//
// The type names are declared in this package file (reasons.go), and a second
// literal in a foreign package would be a second source of truth. The
// decision "do not publish" is taken by the queue dispatcher, but the
// KNOWLEDGE "this type is private" belongs to whoever declared the type.
// outbox asks the registry (IsPrivateEventType); it does not keep a copy.

import "sort"

// PrivateEventType is an event type whose row is NEVER published onto the bus.
//
// An opaque wrapper, not a string alias: outside the package a value of this
// type cannot be assembled — neither by a literal nor by a conversion (the
// field is unexported) — so the only path to one is through registerPrivate.
type PrivateEventType struct{ name string }

// String returns the type name for writing into the events.type column.
func (t PrivateEventType) String() string { return t.name }

// IsZero reports that the value was not obtained by registration (zero).
// Writers of the private form must refuse such a value: zero arrives only
// from `var t notifications.PrivateEventType` — i.e. from a broken call site.
func (t PrivateEventType) IsZero() bool { return t.name == "" }

// privateEventTypes is the closed set. Filled ONLY by registerPrivate,
// never edited by hand, and therefore cannot drift from the writers.
var privateEventTypes = map[string]struct{}{}

// registerPrivate inserts the type into the set and issues a pass to writers
// of the private form. Unexported on purpose: the right to declare a type
// private belongs to the file where the type is declared, not to any caller.
func registerPrivate(name string) PrivateEventType {
	if name == "" {
		panic("notifications: private event type must have a name")
	}
	if _, dup := privateEventTypes[name]; dup {
		panic("notifications: private event type registered twice: " + name)
	}
	privateEventTypes[name] = struct{}{}
	return PrivateEventType{name: name}
}

// Passes for the private writers. Declaration order does not matter:
// the init dependency runs from each to privateEventTypes, so the map is
// filled before the first read.
var (
	// PrivateReviewMarked — the author marked a closed result as reviewed.
	PrivateReviewMarked = registerPrivate(EventTypeReviewMarked)
	// PrivateReviewFlagChanged — the author turned the "review required"
	// flag on or off.
	PrivateReviewFlagChanged = registerPrivate(EventTypeReviewFlagChanged)
	// PrivateUnacceptedDigest — an unaccepted-issue digest was built.
	// Private for the same reason as the pair above: addressed to a PERSON,
	// written in the user form, and the user namespace is also handed out
	// to every administrator by the bridge.
	PrivateUnacceptedDigest = registerPrivate(EventTypeUnacceptedDigest)
	// PrivateUnreviewedDigest — an unreviewed-results digest was built.
	// This is the one whose body lists the creator's issues: leaking this
	// row IS leaking the private bookkeeping.
	PrivateUnreviewedDigest = registerPrivate(EventTypeUnreviewedDigest)
)

// IsPrivateEventType reports that a row of this type must not be published.
// A pure function with no state — safe from any goroutine (the set is
// read-only after package init).
func IsPrivateEventType(eventType string) bool {
	_, ok := privateEventTypes[eventType]
	return ok
}

// PrivateEventTypes returns the whole set, sorted, as a copy.
//
// It exists for the guards: the queue class iterates THIS set, not its own
// list, so a new private type falls under the assertion "is not published
// and receives a terminal stamp" at the same moment it is registered.
func PrivateEventTypes() []string {
	out := make([]string, 0, len(privateEventTypes))
	for t := range privateEventTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
