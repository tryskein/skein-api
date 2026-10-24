package notifications

import (
	"testing"
)

// reasons_test.go — coverage-matrix row 1 of sk-303.13 (plan task T001):
// the unit gate of the closed reason registry.
//
// The tests are written from the spec, not from the code: the expectation
// table below is copied verbatim from the section "Reason registry — closed
// list" (reason → applicability and audience of each channel → default). If
// the registry drifts from the spec, it will drift here, not in a user
// review.

// expectedReason is one row of the spec's registry table.
type expectedReason struct {
	key   ReasonKey
	state ReasonState
	wave  int

	feedApplicable bool
	feedAudience   Audience
	feedDefault    bool

	pushApplicable bool
	pushAudience   Audience
	pushDefault    bool
}

// specRegistry is the registry as the spec states it, IN DISPLAY ORDER.
// Order is load-bearing: the form shows reasons in this order.
var specRegistry = []expectedReason{
	{
		key: ReasonKeyAssignment, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: true,
		pushApplicable: true, pushAudience: AudienceNewAssignee, pushDefault: true,
	},
	{
		key: ReasonKeyAccepted, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: true,
		pushApplicable: true, pushAudience: AudienceCreator, pushDefault: true,
	},
	{
		key: ReasonKeyDelegatedStatus, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: true,
		// The channel audiences are deliberately different: the push is narrower than the feed.
		pushApplicable: true, pushAudience: AudienceDelegatingCreator, pushDefault: true,
	},
	{
		key: ReasonKeyReminder, state: ReasonStateActive,
		// The reminder's feed channel is inapplicable (SC-022).
		feedApplicable: false,
		pushApplicable: true, pushAudience: AudienceReminderOwner, pushDefault: true,
	},
	{
		key: ReasonKeyMention, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceMentioned, feedDefault: true,
		pushApplicable: true, pushAudience: AudienceMentioned, pushDefault: true,
	},
	{
		key: ReasonKeyIssueCreated, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: false,
		pushApplicable: false,
	},
	{
		key: ReasonKeyComment, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: false,
		pushApplicable: false,
	},
	{
		key: ReasonKeyAttachment, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: false,
		pushApplicable: false,
	},
	{
		key: ReasonKeyReparent, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: false,
		pushApplicable: false,
	},
	{
		key: ReasonKeyAutoUnclaim, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: false,
		pushApplicable: false,
	},
	{
		key: ReasonKeyOtherEdits, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceSubscribers, feedDefault: false,
		pushApplicable: false,
	},
	// sk-303.14 / T005: both reminder categories are now active,
	// wave pins are removed, the push audience is "digest recipient", not a
	// relation to one issue: a digest names several issues at once, and the
	// recipient's relation to any of them does not decide whether this digest
	// is theirs.
	{
		key: ReasonKeyUnacceptedReminder, state: ReasonStateActive,
		feedApplicable: false,
		pushApplicable: true, pushAudience: AudienceDigestRecipient, pushDefault: true,
	},
	{
		key: ReasonKeyReviewReminder, state: ReasonStateActive,
		feedApplicable: false,
		pushApplicable: true, pushAudience: AudienceDigestRecipient, pushDefault: true,
	},
	// sk-489 / US-3: "closed issue outcome changed". Both audiences are the issue author
	// (AudienceCreator), both channels are on by default (the owner's
	// decision). This is the first reason whose channel audiences coincide
	// ON PURPOSE: the addressee is the same person, and narrowing one channel
	// relative to the other would be a claim the spec does not make.
	{
		key: ReasonKeyOutcomeChanged, state: ReasonStateActive,
		feedApplicable: true, feedAudience: AudienceCreator, feedDefault: true,
		pushApplicable: true, pushAudience: AudienceCreator, pushDefault: true,
	},
	// sk-674.18 T040 / US-5 (decision 2026-10-14): "an email, push and
	// Inbox card" to the existing invitation addressee and "push … to the inviter"
	// on acceptance. No feed — the invitation card is not a feed row but the
	// pending invitation itself; push is on by default, like other addressed
	// reasons.
	{
		key: ReasonKeySpaceInvitation, state: ReasonStateActive,
		feedApplicable: false,
		pushApplicable: true, pushAudience: AudienceInvitationParty, pushDefault: true,
	},
}

// TestRegistryMatchesSpec — composition, order, states, applicability,
// audiences and defaults of both channels match the spec (INV-10, R-12).
func TestRegistryMatchesSpec(t *testing.T) {
	got := AllReasons()
	if len(got) != len(specRegistry) {
		t.Fatalf("registry has %d reasons, spec has %d", len(got), len(specRegistry))
	}
	for i, want := range specRegistry {
		r := got[i]
		if r.Key != want.key {
			t.Fatalf("reason #%d: key = %q, want %q (display order is part of the contract)",
				i, r.Key, want.key)
		}
		if r.State != want.state {
			t.Errorf("%s: state = %q, want %q", r.Key, r.State, want.state)
		}
		if r.Wave != want.wave {
			t.Errorf("%s: wave = %d, want %d", r.Key, r.Wave, want.wave)
		}
		if r.Label == "" {
			t.Errorf("%s: label is empty — the form renders what the server returns", r.Key)
		}
		// A hint is required of EVERY entry, not of one (sk-489, rework after
		// review A/5). Before this line a non-empty Hint was checked only for
		// delegated_status — in the test about diverging audiences — and every
		// new reason arrived without a guard: `Hint: ""` on outcome_changed
		// survived both the unit gate and the REST form. An empty hint is an
		// empty explanation under the toggle (R-1: the client draws what the
		// server returned), that is, a toggle without an explanation of who it sends to and where.
		if r.Hint == "" {
			t.Errorf("%s: hint is empty — there will be no explanation under the toggle in the form, "+
				"of whom and for which issues this reason arrives", r.Key)
		}

		if r.Feed.Applicable != want.feedApplicable {
			t.Errorf("%s: feed.applicable = %v, want %v", r.Key, r.Feed.Applicable, want.feedApplicable)
		}
		if r.Feed.Audience != want.feedAudience {
			t.Errorf("%s: feed.audience = %q, want %q", r.Key, r.Feed.Audience, want.feedAudience)
		}
		if r.Feed.DefaultOn != want.feedDefault {
			t.Errorf("%s: feed.defaultOn = %v, want %v", r.Key, r.Feed.DefaultOn, want.feedDefault)
		}

		if r.Push.Applicable != want.pushApplicable {
			t.Errorf("%s: push.applicable = %v, want %v", r.Key, r.Push.Applicable, want.pushApplicable)
		}
		if r.Push.Audience != want.pushAudience {
			t.Errorf("%s: push.audience = %q, want %q", r.Key, r.Push.Audience, want.pushAudience)
		}
		if r.Push.DefaultOn != want.pushDefault {
			t.Errorf("%s: push.defaultOn = %v, want %v", r.Key, r.Push.DefaultOn, want.pushDefault)
		}
	}
}

// TestChannelAudiencesRecordedPerChannel — INV-10 in its sharpest form: for
// a reason whose two channel audiences differ, they also differ in the
// registry. Aligning them down would gut watching; aligning them up would
// flood devices with pushes across the whole tree.
func TestChannelAudiencesRecordedPerChannel(t *testing.T) {
	r, ok := LookupReason(ReasonKeyDelegatedStatus)
	if !ok {
		t.Fatal("delegated_status missing from the registry")
	}
	if r.Feed.Audience == r.Push.Audience {
		t.Fatalf("delegated_status: both channels claim audience %q — the spec says the feed is "+
			"broader (any watched status change) and the push is narrower (delegating creator only)",
			r.Feed.Audience)
	}
	if r.Feed.Audience != AudienceSubscribers {
		t.Errorf("delegated_status feed audience = %q, want subscribers", r.Feed.Audience)
	}
	if r.Push.Audience != AudienceDelegatingCreator {
		t.Errorf("delegated_status push audience = %q, want delegating_creator", r.Push.Audience)
	}
	// The reason's hint must reflect the audience split and must not promise
	// "delegated only" for the feed channel (Q7).
	if r.Hint == "" {
		t.Error("delegated_status: hint is empty — the form must explain the two audiences")
	}
}

// TestInapplicableChannelIsNotAnOffToggle — "inapplicable" and "off" are
// different things: an inapplicable channel cannot be switched on either by
// default or by the user, and it has no audience.
func TestInapplicableChannelIsNotAnOffToggle(t *testing.T) {
	for _, r := range AllReasons() {
		for _, ch := range []Channel{ChannelFeed, ChannelPush} {
			spec := r.ChannelSpecOf(ch)
			if spec.Applicable {
				if spec.Audience == AudienceNone {
					t.Errorf("%s/%s: applicable channel without an audience", r.Key, ch)
				}
				continue
			}
			if spec.Audience != AudienceNone {
				t.Errorf("%s/%s: inapplicable channel carries audience %q", r.Key, ch, spec.Audience)
			}
			if spec.DefaultOn {
				t.Errorf("%s/%s: inapplicable channel is defaulted ON", r.Key, ch)
			}
			// And no recipient falls into it. The relation universe is built
			// by reflection (allRelations), not a literal: with a literal a
			// relation added later would stay false, and "no recipient" would
			// quietly mean "none of the old ones".
			if ChannelAudienceMatches(r.Key, ch, allRelations()) {
				t.Errorf("%s/%s: inapplicable channel matched a recipient", r.Key, ch)
			}
		}
	}
}

// TestActiveReasonHasDelivery — SC-017: the form has no reason without
// actual delivery. An active reason must have at least one applicable
// channel; a reason without delivery and without a wave pin is "dead" and
// forbidden.
func TestActiveReasonHasDelivery(t *testing.T) {
	for _, r := range AllReasons() {
		switch r.State {
		case ReasonStateActive:
			if !r.Feed.Applicable && !r.Push.Applicable {
				t.Errorf("%s: active reason without any applicable channel (sk-87 defect class)", r.Key)
			}
			if r.Wave != 0 {
				t.Errorf("%s: active reason pinned to wave %d", r.Key, r.Wave)
			}
		case ReasonStateReserved:
			if r.Wave == 0 {
				t.Errorf("%s: reserved reason without a wave pin — an orphan, not a reserve", r.Key)
			}
		default:
			t.Errorf("%s: state %q — the contract has exactly two states", r.Key, r.State)
		}
	}
}

// TestFormProjectionExcludesReserved — Q10: a reserved reason does not get
// a toggle in the form.
//
// sk-303.14 activated both reminder categories, so the registry currently
// reserves NOTHING, and the assertion is taken from the registry, not from
// two names written by hand: the rule "a reserve does not enter the form"
// stays in force for the next reserve, and today's empty reserve is
// asserted explicitly — otherwise the test would look green because "there
// is nothing to check", and nobody would know.
func TestFormProjectionExcludesReserved(t *testing.T) {
	active := ActiveReasons()
	for _, r := range active {
		if r.State != ReasonStateActive {
			t.Errorf("form projection contains a %s reason: %s", r.State, r.Key)
		}
	}
	inForm := map[ReasonKey]bool{}
	for _, r := range active {
		inForm[r.Key] = true
	}
	reserved := 0
	for _, r := range AllReasons() {
		if r.State != ReasonStateReserved {
			continue
		}
		reserved++
		if inForm[r.Key] {
			t.Errorf("reserved reason %s leaked into the form projection", r.Key)
		}
	}
	if reserved != 0 {
		t.Logf("the registry currently reserves %d reason(s) — the exclusion above is live", reserved)
	}

	// The two reminder categories: they are in the form, they are no
	// longer a reserve, they carry no wave pin. That is exactly the
	// "moved into active" state that was required; checking by name is
	// appropriate here because the claim is about these two reasons.
	for _, activated := range []ReasonKey{ReasonKeyUnacceptedReminder, ReasonKeyReviewReminder} {
		r, ok := LookupReason(activated)
		if !ok {
			t.Errorf("reason %s is missing from the registry entirely", activated)
			continue
		}
		if r.State != ReasonStateActive {
			t.Errorf("%s: state = %q, it is activated", activated, r.State)
		}
		if r.Wave != 0 {
			t.Errorf("%s: still pinned to wave %d — the pin is removed on activation", activated, r.Wave)
		}
		if !inForm[activated] {
			t.Errorf("%s is active but the form projection does not show it", activated)
		}
	}
}

// TestNarrowDefaultSevenInStorageSevenInForm — the default: exactly NINE reasons
// are enabled in STORAGE (the ninth is "Space invitation", sk-674.18 T040, push
// only). Before sk-303.14 seven were enabled, two of them
// were reserved, and the form showed five; sk-303.14 activated both, and the numbers
// matched; sk-489 added the eighth — "Closed issue outcome changed".
// The match of the storage and form numbers is a property of the current registry, not
// an identity, so both figures are still counted separately and from different
// sources: the storage one from DefaultEnabledReasons(), the form one from
// ActiveReasons().
//
// The test name is kept on purpose: the coverage matrix
// refers to the test by it, and renaming would break the link. The numbers in the body are current.
func TestNarrowDefaultSevenInStorageSevenInForm(t *testing.T) {
	const wantDefaultEnabled = 9

	enabled := DefaultEnabledReasons()
	if len(enabled) != wantDefaultEnabled {
		t.Errorf("default-enabled reasons in storage = %d (%v), want %d",
			len(enabled), enabled, wantDefaultEnabled)
	}

	wantEnabled := map[ReasonKey]bool{
		ReasonKeyAssignment:         true,
		ReasonKeyDelegatedStatus:    true,
		ReasonKeyAccepted:           true,
		ReasonKeyReminder:           true,
		ReasonKeyMention:            true,
		ReasonKeyUnacceptedReminder: true,
		ReasonKeyReviewReminder:     true,
		ReasonKeyOutcomeChanged:     true,
		ReasonKeySpaceInvitation:    true,
	}
	got := map[ReasonKey]bool{}
	for _, k := range enabled {
		got[k] = true
	}
	for k := range wantEnabled {
		if !got[k] {
			t.Errorf("%s must be enabled by default", k)
		}
	}
	for k := range got {
		if !wantEnabled[k] {
			t.Errorf("%s is enabled by default but the narrow default does not include it", k)
		}
	}

	// How many of the nine are visible in the form: there is no reserve in the registry now — all
	// nine. The number is derived from the state of the reasons, not written as a constant
	// twice: "enabled in storage" minus "enabled but reserved".
	reservedEnabled := 0
	for _, r := range AllReasons() {
		if r.State == ReasonStateReserved && (r.Feed.DefaultOn || r.Push.DefaultOn) {
			reservedEnabled++
		}
	}
	visible := 0
	for _, r := range ActiveReasons() {
		if r.Feed.DefaultOn || r.Push.DefaultOn {
			visible++
		}
	}
	if want := wantDefaultEnabled - reservedEnabled; visible != want {
		t.Errorf("default-enabled reasons visible in the form = %d, want %d", visible, want)
	}
	if visible != wantDefaultEnabled {
		t.Errorf("the registry currently reserves nothing, so all %d default-enabled reasons "+
			"must be visible in the form; got %d", wantDefaultEnabled, visible)
	}

	// "Everything on" is left for nobody: at least one reason is off.
	allOn := true
	for _, d := range DefaultChannelStates() {
		if !d.Feed || !d.Push {
			allOn = false
			break
		}
	}
	if allOn {
		t.Error("the default set leaves every channel of every reason on — that is the state the wave removes")
	}
}

// TestDefaultChannelStatesCoverRegistry — a default is set for every reason
// in the registry, and an inapplicable channel in it is always off.
func TestDefaultChannelStatesCoverRegistry(t *testing.T) {
	defaults := DefaultChannelStates()
	if len(defaults) != len(AllReasons()) {
		t.Fatalf("defaults cover %d reasons, registry has %d", len(defaults), len(AllReasons()))
	}
	for _, d := range defaults {
		r, ok := LookupReason(d.Key)
		if !ok {
			t.Errorf("default for unknown reason %s", d.Key)
			continue
		}
		if !r.Feed.Applicable && d.Feed {
			t.Errorf("%s: feed default on for an inapplicable channel", d.Key)
		}
		if !r.Push.Applicable && d.Push {
			t.Errorf("%s: push default on for an inapplicable channel", d.Key)
		}
	}
}

// TestReasonPriorityOrder — the priority rule: the more specific reason
// beats the more general one. The spec names two pairs outright.
func TestReasonPriorityOrder(t *testing.T) {
	prio := func(k ReasonKey) int {
		r, ok := LookupReason(k)
		if !ok {
			t.Fatalf("unknown reason %s", k)
		}
		return r.Priority
	}
	if prio(ReasonKeyAssignment) <= prio(ReasonKeyOtherEdits) {
		t.Error("\"Issue assignment\" must outrank \"Other issue edits\"")
	}
	if prio(ReasonKeyAssignment) <= prio(ReasonKeyIssueCreated) {
		t.Error("\"Issue assignment\" must outrank \"New issue\"")
	}
}

// TestExcludedEventsHaveNoReason — claim, close, archival and dependencies
// get no toggle: they exist only as journal records, and no reason is
// mapped onto them for any recipient.
func TestExcludedEventsHaveNoReason(t *testing.T) {
	excluded := []string{
		"issue.claimed", "issue.closed", "issue.archived",
		"dependency.created", "dependency.deleted",
		// INV-2: private review bookkeeping lives only in the
		// journal — no reason, no notification, for any recipient.
		EventTypeReviewMarked, EventTypeReviewFlagChanged,
	}
	everyone := allRelations()
	for _, et := range excluded {
		if !IsAuditOnlyEventType(et) {
			t.Errorf("%s must be audit-only", et)
		}
		if HasDeliveryPath(et) {
			t.Errorf("%s must have no delivery path", et)
		}
		if c := Classify(EventFacts{Type: et}, everyone); c.Outcome != OutcomeAuditOnly {
			t.Errorf("Classify(%s) outcome = %q, want audit_only", et, c.Outcome)
		}
	}
}

// TestSubscriptionCatalogOnlyDeliverableTypes — types with no delivery
// have been removed from the subscription-filter catalogue; personally
// addressed reasons (mention, reminder) are not in the catalogue — they
// arrive regardless of subscription, and a filter on them would be a dead
// toggle.
func TestSubscriptionCatalogOnlyDeliverableTypes(t *testing.T) {
	catalog := SubscriptionEventTypes()
	if len(catalog) == 0 {
		t.Fatal("subscription catalogue is empty")
	}
	for _, e := range catalog {
		if !HasDeliveryPath(e.Type) {
			t.Errorf("catalogue lists %s which delivers nothing", e.Type)
		}
		if !IsSubscriptionEventType(e.Type) {
			t.Errorf("catalogue entry %s fails its own membership check", e.Type)
		}
		if len(e.Reasons) == 0 {
			t.Errorf("catalogue entry %s names no reason", e.Type)
		}
		for _, key := range e.Reasons {
			if _, ok := LookupReason(key); !ok {
				t.Errorf("catalogue entry %s points at unknown reason %s", e.Type, key)
			}
		}
		if e.Label == "" {
			t.Errorf("catalogue entry %s has no label", e.Type)
		}
	}
	for _, dead := range []string{"issue.claimed", "issue.closed", "issue.archived",
		"dependency.created", "dependency.deleted", "issue.label_added"} {
		if IsSubscriptionEventType(dead) {
			t.Errorf("undeliverable type %s is offered as a subscription filter", dead)
		}
	}
	for _, addressed := range []string{"comment.mention", "issue.mention", "reminder.rang",
		EventTypeUnacceptedDigest, EventTypeUnreviewedDigest} {
		if IsSubscriptionEventType(addressed) {
			t.Errorf("personally addressed type %s must not be a subscription filter (INV-8)", addressed)
		}
	}
}

// TestSubscriptionCatalogReasonsMatchClassifier — the catalogue must name
// ALL reasons the type projects into for a subscriber, and only those.
//
// The catalogue field is not decoration: T032 groups filters by it, and a
// single declared reason on a type the classifier splits by edit contents
// would tuck "Other issue edits" under "Assignment". Unchecking assignment
// would silently turn off priority changes — a filter nobody touched. The
// truth is taken not from a restatement of the rule but from the shared
// corpus: the set of reasons a pure subscriber gets for each type.
func TestSubscriptionCatalogReasonsMatchClassifier(t *testing.T) {
	corpus := loadCorpus(t)

	fromCorpus := map[string]map[ReasonKey]bool{}
	for _, c := range corpus.Cases {
		r := c.Recipient
		// A pure subscriber: neither assignee, nor creator, nor mentioned —
		// otherwise the set would pick up a reason the recipient earned by
		// another role (a new assignee on issue.created reads "Assignment",
		// not "New issue").
		if !r.Subscriber || r.NewAssignee || r.Assignee || r.Creator || r.Actor ||
			r.Mentioned || r.ReminderOwner {
			continue
		}
		if c.Expect.Outcome != string(OutcomeReason) {
			continue
		}
		if fromCorpus[c.Event.Type] == nil {
			fromCorpus[c.Event.Type] = map[ReasonKey]bool{}
		}
		fromCorpus[c.Event.Type][ReasonKey(c.Expect.Reason)] = true
	}

	for _, e := range SubscriptionEventTypes() {
		observed := fromCorpus[e.Type]
		if len(observed) == 0 {
			t.Errorf("%s: corpus has no pure-subscriber case — the catalogue claim %v is unchecked",
				e.Type, e.Reasons)
			continue
		}
		declared := map[ReasonKey]bool{}
		for _, key := range e.Reasons {
			declared[key] = true
			if !observed[key] {
				t.Errorf("%s: catalogue names reason %s which no subscriber case produces",
					e.Type, key)
			}
		}
		for key := range observed {
			if !declared[key] {
				t.Errorf("%s: a subscriber receives this type as %s, but the catalogue does not "+
					"list that reason — the filter would be grouped under a switch that does not govern it",
					e.Type, key)
			}
		}
	}
}
