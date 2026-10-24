package notifications

import "sort"

// reasons.go — T001 (sk-303.13): the CLOSED registry of notification reasons
// and the "event × recipient → reason" classifier.
//
// Why a registry at all. Until this wave the user-facing settings spoke the
// language of raw event types, and five of the ten toggles in the form mapped
// to events with no delivery at all (sk-87). The spec's answer is a single
// server-side registry: it names every reason, what events project into it,
// which of the two channels (feed / push) is applicable, WHOSE inbox each
// channel reaches, and what the narrow default is. The client renders what the
// server returns (R-1); nothing downstream re-derives this table.
//
// Two properties of the registry are load-bearing and easy to lose:
//
//   - Each channel carries its OWN audience (R-12 / INV-10). The feed goes to
//     subscribers, the push is addressed to a role — these are different sets
//     under one name, and merging them would either flood devices or gut
//     watching. So Audience lives per channel, never per reason.
//
//   - A reason is either ACTIVE (has a non-empty event mapping, is shown in
//     the form, and delivery for it exists) or RESERVED (mapping does not
//     exist yet, not shown in the form, pinned to the wave that must activate
//     it). There is no third state: an orphan reason with neither delivery nor
//     a wave is a defect, and it is told apart from a legitimate reserve
//     exactly by that wave pin.
//
// The default set is deliberately narrow: EIGHT reasons are enabled by default
// in STORAGE. The first seven were enabled and only five of them were shown in the form,
// because the two reminder reasons were still RESERVED (Q10). sk-303.14 / T005
// ACTIVATES both — "Reminder categories … need to be
// moved to the active state", and the pin that held "Awaiting review" for
// a later wave is removed by the maintainers' decision — so all
// seven became visible. sk-489 adds the eighth, "Closed issue outcome
// changed", enabled on both channels by the maintainers' decision (US-3).
// The two numbers still answer different questions and are asserted
// separately: eight in storage, eight in the form only because the registry
// currently reserves nothing.
//
// Scope note. This file owns the registry, the subscription-event catalogue
// and the classifier. It deliberately does NOT decide whether a row is
// materialised: that is "preference enabled AND recipient in the channel's
// audience", composed in the fan-out (T007). The audience half of that
// composition is exported here as ChannelAudienceMatches.

// ReasonKey is the stable identifier of a reason. It is what gets persisted in
// the per-user preference rows and sent on the wire; labels are display-only.
type ReasonKey string

// The closed set of reason keys. The names are prefixed ReasonKey* rather than
// Reason* because this package already uses Reason* for WATCHER subscription
// reasons (creator / assignee / commenter / …), which are a different concept:
// those say why a subscription row exists, these say what a notification is
// about.
const (
	// ReasonKeyAssignment — an issue was assigned: an edit that changed the
	// assignee, or creating an issue already carrying one.
	ReasonKeyAssignment ReasonKey = "assignment"
	// ReasonKeyAccepted — the assignee accepted the issue with the EXPLICIT
	// acceptance tool. Automatic acceptance does not get a reason.
	ReasonKeyAccepted ReasonKey = "accepted"
	// ReasonKeyDelegatedStatus — an issue status change. The feed is any
	// status change on a watched issue; the push is only to the creator and
	// only when the issue is really delegated.
	ReasonKeyDelegatedStatus ReasonKey = "delegated_status"
	// ReasonKeyReminder — a personal reminder rang. The feed channel is not
	// applicable: the live ring is driven by reminder state, not a feed row.
	ReasonKeyReminder ReasonKey = "reminder"
	// ReasonKeyMention — a mention via @ in a comment or in the issue text.
	ReasonKeyMention ReasonKey = "mention"
	// ReasonKeyIssueCreated — an issue was created (to subscribers).
	ReasonKeyIssueCreated ReasonKey = "issue_created"
	// ReasonKeyComment — a comment was created (to subscribers).
	ReasonKeyComment ReasonKey = "comment"
	// ReasonKeyAttachment — an attachment was added (to subscribers).
	ReasonKeyAttachment ReasonKey = "attachment"
	// ReasonKeyReparent — an issue was moved in the tree (to subscribers).
	ReasonKeyReparent ReasonKey = "reparent"
	// ReasonKeyAutoUnclaim — a lock was released by the stale-lock reaper.
	// Default is off and there is no push channel: the silence is deliberate
	// (the spec).
	ReasonKeyAutoUnclaim ReasonKey = "auto_unclaim"
	// ReasonKeyOtherEdits — other issue edits. Today this is exactly a
	// priority change: the only field besides assignee that has delivery.
	ReasonKeyOtherEdits ReasonKey = "other_edits"
	// ReasonKeyUnacceptedReminder — a reminder about an unaccepted issue
	// (category review.CategoryUnaccepted). Activated by sk-303.14.
	ReasonKeyUnacceptedReminder ReasonKey = "unaccepted_reminder"
	// ReasonKeyReviewReminder — a reminder about an issue awaiting review
	// (category review.CategoryUnreviewed). It was first pinned to a later
	// wave; moving it forward was confirmed by the owner.
	ReasonKeyReviewReminder ReasonKey = "review_reminder"
	// ReasonKeyOutcomeChanged — the outcome of a closed issue changed: was X,
	// became Y (sk-489, US-3). There is one addressee — the issue AUTHOR; the channels are the feed and
	// push, on by default.
	//
	// Why an author who changed the outcome THEMSELVES does not get the reason, although
	// the audience of both channels is AudienceCreator without reservations: they are cut off by
	// the own-action exclusion in the recipient SELECTION
	// (selectFanOutCandidateSQL), not by the registry. The registry answers the question "to whom
	// is this reason addressed", the selection — "who is a candidate at all"; mixing them
	// here would create a second place for one rule.
	ReasonKeyOutcomeChanged ReasonKey = "outcome_changed"
	// ReasonKeySpaceInvitation — an invitation to a space (sk-674.18 T040,
	// US-5): a push to the invited person when it is sent (and sent again),
	// and to whoever sent it when it is accepted. No feed: the invitation's
	// Inbox card is the pending invitation itself (GET /me/invitations), and
	// a feed row would be a second copy of it. The rows are written by the
	// invitations package in the transaction of the invitation, not by the
	// event fan-out (invitations/notify.go).
	ReasonKeySpaceInvitation ReasonKey = "space_invitation"
)

// Event types this wave adds. They are named here, beside the registry, for
// the same reason the registry is here at all: the classifier is the one place
// that must know EVERY type — the producers (T007, the nudge worker) come
// later and read these constants instead of re-typing string literals.
//
// Two groups, and they are opposites:
//
//   - the PRIVATE pair — the bookkeeping of "the creator's review". They
//     are journal records and nothing else (INV-2): no notification, no feed,
//     no reason. They are declared AUDIT-ONLY below, which is what makes their
//     silence a decision rather than an unmapped hole. Excluding them from the
//     bus publication is a separate obligation and belongs to T008; the
//     registry only classifies.
//
//   - the DIGEST pair — one per reminder category. Each projects onto its OWN
//     reason (R-4: "categories are not merged"), and only for the person the
//     digest is addressed to.
const (
	// EventTypeReviewMarked — the author marked a closed result as reviewed.
	EventTypeReviewMarked = "review.marked"
	// EventTypeReviewFlagChanged — the author turned the "review required"
	// flag on or off.
	EventTypeReviewFlagChanged = "review.flag_changed"
	// EventTypeUnacceptedDigest — an unaccepted-issue digest was built
	// (INV-10: the event is named after the checkable fact — "was built").
	EventTypeUnacceptedDigest = "nudge.unaccepted_digest"
	// EventTypeUnreviewedDigest — an unreviewed-results digest was built.
	EventTypeUnreviewedDigest = "nudge.unreviewed_digest"

	// EventTypeOutcomeChanged — the outcome of a closed issue changed (sk-489,
	// US-3). Written by both close writers on a repeated close with a
	// different outcome and on an outcome edit of a closed issue; field = "closeOutcome",
	// old/new — the outcome strings.
	//
	// The type is ORDINARY, PUBLIC: unlike the private review.* pair above it is
	// not journal-only — it has a reason (ReasonKeyOutcomeChanged) and a fan-out to
	// the issue author, so it is not in auditOnlyEventTypes and is not registered as
	// private.
	EventTypeOutcomeChanged = "issue.outcome_changed"

	// EventTypeSpaceInvitationSent / EventTypeSpaceInvitationAccepted — a
	// space invitation was sent (created or sent again) / accepted (sk-674.18
	// T040). Written by the invitations package, already published, beside
	// the push row of ReasonKeySpaceInvitation it addresses (DirectWriter):
	// entity_type 'space', entity_id the space, payload {invitationId,
	// spaceId}. The fan-out never reads them, so the classifier does not map
	// them.
	EventTypeSpaceInvitationSent     = "space.invitation_sent"
	EventTypeSpaceInvitationAccepted = "space.invitation_accepted"
)

// ReasonState is the two-valued lifecycle of a registry entry. There is no
// third state by contract.
type ReasonState string

const (
	// ReasonStateActive — mapping exists, delivery exists, shown in the form.
	ReasonStateActive ReasonState = "active"
	// ReasonStateReserved — no mapping yet, not shown in the form, pinned to
	// the wave that must activate it.
	ReasonStateReserved ReasonState = "reserved"
)

// Audience names WHO a channel of a reason reaches. It is recorded per channel
// precisely because the two channels of one reason legitimately differ.
type Audience string

const (
	// AudienceNone — the channel is not applicable to this reason. The form
	// shows a dash with an explanation, never a switched-off toggle.
	AudienceNone Audience = ""
	// AudienceSubscribers — everyone subscribed to the issue (directly, via
	// subtree or via the project).
	AudienceSubscribers Audience = "subscribers"
	// AudienceNewAssignee — addressed to the issue's new assignee.
	AudienceNewAssignee Audience = "new_assignee"
	// AudienceCreator — addressed to the issue's creator.
	AudienceCreator Audience = "creator"
	// AudienceDelegatingCreator — addressed to the creator, and ONLY when the
	// task is really delegated: the creator is neither the assignee nor the
	// actor of this action.
	AudienceDelegatingCreator Audience = "delegating_creator"
	// AudienceMentioned — addressed to the mentioned user.
	AudienceMentioned Audience = "mentioned"
	// AudienceReminderOwner — addressed to the reminder's owner.
	AudienceReminderOwner Audience = "reminder_owner"
	// AudienceDigestRecipient — addressed to the person a reminder DIGEST was
	// built for.
	//
	// It is a relation of its own and not a re-use of new_assignee / creator,
	// even though the two reminder categories are aimed at exactly those
	// people. A digest is not about one issue: it names several, and the
	// recipient's relation to any single one of them is not what put the
	// digest in their queue. Answering the audience question with
	// "is the recipient the new assignee (of which issue?)" would be a
	// statement the delivery tract cannot honestly evaluate — the tract holds
	// a row addressed to a person, not an event about one task.
	AudienceDigestRecipient Audience = "digest_recipient"
	// AudienceInvitationParty — addressed to a party of a space invitation:
	// the invited person when it is sent, its sender when it is accepted
	// (sk-674.18 T040). A relation of its own: an invitation is about a
	// space, not an issue, so none of the issue relations describes it.
	AudienceInvitationParty Audience = "invitation_party"
)

// Channel is one of the two independent delivery channels of a reason.
type Channel string

const (
	// ChannelFeed — the in-app feed (feed, counters, live stream).
	ChannelFeed Channel = "feed"
	// ChannelPush — web push to the user's devices.
	ChannelPush Channel = "push"
)

// ChannelSpec is one channel of one reason: whether it exists at all, whom it
// reaches, and whether the narrow default leaves it on.
//
// Applicable=false is NOT the same as DefaultOn=false. "Not applicable" means
// the channel does not exist for this reason and cannot be switched on by the
// user; "off by default" means it exists and is switched off until the user
// says otherwise.
type ChannelSpec struct {
	Applicable bool     `json:"applicable"`
	Audience   Audience `json:"audience,omitempty"`
	DefaultOn  bool     `json:"defaultOn"`
}

// Reason is one registry entry.
type Reason struct {
	Key   ReasonKey `json:"key"`
	Label string    `json:"label"`
	// Hint is the sub-caption the form renders under the label. For reasons
	// whose two channels have different audiences it MUST say so — the form is
	// not allowed to promise "delegated only" for a feed channel that is
	// broader (Q7).
	Hint  string      `json:"hint,omitempty"`
	State ReasonState `json:"state"`
	// Wave is the wave that must activate a reserved reason. Zero for active
	// reasons; a reserved reason with Wave == 0 is the orphan the contract
	// forbids, and registryInvariants() refuses to build such a registry.
	Wave int `json:"wave,omitempty"`
	// Priority breaks ties when one event gives a recipient several candidate
	// reasons: the more specific reason wins. Higher is more specific.
	Priority int         `json:"-"`
	Feed     ChannelSpec `json:"feed"`
	Push     ChannelSpec `json:"push"`
	// DirectWriter marks a reason whose notification rows are written by the
	// producer of the action itself, in the action's own transaction, and
	// never by the event fan-out (sk-674.18 T040: a space invitation — the
	// invitations package writes the push row beside the invitation, under
	// the space's lock, for a recipient it has just resolved by email).
	//
	// Such a reason does not pass the classifier, by construction: its event
	// is written already published, the fan-out never reads it, and no
	// recipient relation of an ISSUE event describes it. The classifier
	// (Classify, its SQL twin notif_classify and their shared corpus) knows
	// only the reasons the fan-out produces; mapping a direct reason there
	// would be a branch no event ever reaches. Its delivery is checked by the
	// producer's own tests and by the push tract's test of its payload. The
	// invariant the other way — no Classify branch yields a direct reason —
	// is held by reason_corpus_test.go#TestDirectWriterReasonsNeverClassified.
	DirectWriter bool `json:"-"`
}

// Reserved-reason wave pins USED to live here (waveUnacceptedReminder = 2,
// waveReviewReminder = 3). Both are gone with sk-303.14: the registry now
// reserves nothing, and a pin left behind on an active reason is refused by
// registryInvariants() rather than ignored. The next reserve declares its own
// pin when it appears.

// Priority scale. Only relative order matters; the gaps leave room for later
// reasons without renumbering. The one ordering the spec states outright:
// "Issue assignment" is more specific than "Other issue edits" and than
// "New issue".
const (
	priorityAddressed  = 100 // personally addressed: mention, reminder
	priorityAssignment = 90  // assignment — older than other edits and a new issue
	priorityAccepted   = 80
	// priorityOutcome — outcome change of a closed issue. Older than a status change and
	// younger than acceptance: "how it ended" is more specific than "the status changed", but
	// does not override the targeted "the issue was accepted". A tie with delegated_status on
	// one event cannot happen (the types differ), so today the value
	// matters not for resolving ties but for keeping the scale readable.
	// The scale has a copy in notif_classify (SQL revision 3, migration
	// 000051); change both together.
	priorityOutcome     = 75
	priorityStatus      = 70
	priorityAutoUnclaim = 60
	priorityEntity      = 40 // comment / attachment / reparent
	priorityOtherEdits  = 20
	priorityIssueBirth  = 10 // "New issue" — the most general reason
)

// reasonRegistry is the ordered closed registry. Order is the display order of
// the settings form. It is unexported and handed out only as copies so no
// consumer can mutate the single source of truth.
var reasonRegistry = []Reason{
	{
		Key:      ReasonKeyAssignment,
		Label:    "Issue assignment",
		Hint:     "feed: issues I watch; push: when I become the assignee",
		State:    ReasonStateActive,
		Priority: priorityAssignment,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: true},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceNewAssignee, DefaultOn: true},
	},
	{
		Key:      ReasonKeyAccepted,
		Label:    "Issue accepted by assignee",
		Hint:     "feed: issues I watch; push: issues I created",
		State:    ReasonStateActive,
		Priority: priorityAccepted,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: true},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceCreator, DefaultOn: true},
	},
	{
		Key:   ReasonKeyDelegatedStatus,
		Label: "Delegated issue status",
		// The channel audiences are deliberately different: the feed is
		// broader (any status change on a watched issue), the push is
		// narrower (only a delegated issue, only to the creator).
		Hint:     "feed: any status change on issues I watch; push: only issues I delegated",
		State:    ReasonStateActive,
		Priority: priorityStatus,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: true},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceDelegatingCreator, DefaultOn: true},
	},
	{
		Key:   ReasonKeyReminder,
		Label: "Reminder",
		Hint:  "not shown in the feed: ringing follows the reminder's own state",
		State: ReasonStateActive,
		// The feed channel is inapplicable — old rings in the feed would
		// depict behaviour that new rings no longer produce (SC-022).
		Priority: priorityAddressed,
		Feed:     ChannelSpec{Applicable: false},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceReminderOwner, DefaultOn: true},
	},
	{
		Key:      ReasonKeyMention,
		Label:    "Mention via @",
		Hint:     "arrives regardless of subscriptions: both feed and push go to the person mentioned",
		State:    ReasonStateActive,
		Priority: priorityAddressed,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceMentioned, DefaultOn: true},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceMentioned, DefaultOn: true},
	},
	{
		Key:      ReasonKeyIssueCreated,
		Label:    "New issue",
		Hint:     "feed: issues and projects I watch",
		State:    ReasonStateActive,
		Priority: priorityIssueBirth,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: false},
		Push:     ChannelSpec{Applicable: false},
	},
	{
		Key:      ReasonKeyComment,
		Label:    "Comment",
		Hint:     "feed: issues I watch",
		State:    ReasonStateActive,
		Priority: priorityEntity,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: false},
		Push:     ChannelSpec{Applicable: false},
	},
	{
		Key:      ReasonKeyAttachment,
		Label:    "Attachment",
		Hint:     "feed: issues I watch",
		State:    ReasonStateActive,
		Priority: priorityEntity,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: false},
		Push:     ChannelSpec{Applicable: false},
	},
	{
		Key:      ReasonKeyReparent,
		Label:    "Moved in the tree",
		Hint:     "feed: issues I watch",
		State:    ReasonStateActive,
		Priority: priorityEntity,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: false},
		Push:     ChannelSpec{Applicable: false},
	},
	{
		Key:      ReasonKeyAutoUnclaim,
		Label:    "Automatic unclaim",
		Hint:     "feed: issues I watch; this reason has no push",
		State:    ReasonStateActive,
		Priority: priorityAutoUnclaim,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: false},
		Push:     ChannelSpec{Applicable: false},
	},
	{
		Key:   ReasonKeyOtherEdits,
		Label: "Other issue edits",
		// A toggle is required to affect observable behaviour; today only a
		// priority change produces delivery, and extending that set is a
		// separate decision, not an inheritance from the incumbent.
		Hint:     "currently only a priority change — the only edit besides the assignee that is delivered",
		State:    ReasonStateActive,
		Priority: priorityOtherEdits,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceSubscribers, DefaultOn: false},
		Push:     ChannelSpec{Applicable: false},
	},
	{
		Key:   ReasonKeyUnacceptedReminder,
		Label: "Unaccepted issue — reminder",
		Hint:  "not shown in the feed: a follow-up about an assignment is a push, not a second feed row; push: one digest per interval",
		State: ReasonStateActive,
		// The feed channel is inapplicable, not switched off: the digest is
		// deliberately not shown in the feed (the notification row is marked
		// non-visible), otherwise the same fact would sit in the feed once
		// per interval the person did not react.
		Priority: priorityAddressed,
		Feed:     ChannelSpec{Applicable: false},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceDigestRecipient, DefaultOn: true},
	},
	{
		Key:      ReasonKeyReviewReminder,
		Label:    "Awaiting review — reminder",
		Hint:     "not shown in the feed: as with unaccepted issues, the follow-up is push only; push: one digest per interval",
		State:    ReasonStateActive,
		Priority: priorityAddressed,
		Feed:     ChannelSpec{Applicable: false},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceDigestRecipient, DefaultOn: true},
	},
	// sk-489 / US-3. Both audiences are the issue author, and this is the only reason
	// where they coincide not by chance: the outcome change is addressed exactly to the
	// requester, because the closed result is theirs, and they are the one who could
	// mark it reviewed (R-7). A subscriber is told about the same action by
	// the "Delegated issue status" row, not by a second row about the outcome.
	{
		Key:      ReasonKeyOutcomeChanged,
		Label:    "Closed issue outcome changed",
		Hint:     "feed and push: issues I created whose outcome changed from \"done\" to something else, or the other way round",
		State:    ReasonStateActive,
		Priority: priorityOutcome,
		Feed:     ChannelSpec{Applicable: true, Audience: AudienceCreator, DefaultOn: true},
		Push:     ChannelSpec{Applicable: true, Audience: AudienceCreator, DefaultOn: true},
	},
	// sk-674.18 T040 / US-5: "an email, push and Inbox card" to the existing
	// addressee; "Acceptance — push and an accepted signal to the inviter". The feed is not
	// applicable: the invitation card is the pending invitation itself
	// (GET /me/invitations), a feed row would repeat it.
	{
		Key:          ReasonKeySpaceInvitation,
		Label:        "Space invitation",
		Hint:         "push only: an invitation to a space for me, and the acceptance of an invitation I sent",
		State:        ReasonStateActive,
		Priority:     priorityAddressed,
		Feed:         ChannelSpec{Applicable: false},
		Push:         ChannelSpec{Applicable: true, Audience: AudienceInvitationParty, DefaultOn: true},
		DirectWriter: true,
	},
}

// reasonByKey indexes the registry for O(1) lookups and, on the way, asserts
// the registry's own invariants. A violation panics at package init: a broken
// registry is a build-time defect, not a runtime condition to branch on — every
// downstream surface (form, defaults, migration, classifier) reads this table
// as ground truth.
var reasonByKey = func() map[ReasonKey]Reason {
	m := make(map[ReasonKey]Reason, len(reasonRegistry))
	for _, r := range reasonRegistry {
		if r.Key == "" {
			panic("notifications: reason with empty key in registry")
		}
		if _, dup := m[r.Key]; dup {
			panic("notifications: duplicate reason key in registry: " + string(r.Key))
		}
		switch r.State {
		case ReasonStateActive:
			if r.Wave != 0 {
				panic("notifications: active reason pinned to a wave: " + string(r.Key))
			}
			if !r.Feed.Applicable && !r.Push.Applicable {
				// An active reason with no applicable channel is a toggle
				// without delivery — exactly the sk-87 defect.
				panic("notifications: active reason without any applicable channel: " + string(r.Key))
			}
		case ReasonStateReserved:
			if r.Wave == 0 {
				// The orphan the contract forbids: no delivery AND no wave.
				panic("notifications: reserved reason without a wave pin: " + string(r.Key))
			}
		default:
			panic("notifications: reason with unknown state: " + string(r.Key))
		}
		if r.Feed.Applicable == (r.Feed.Audience == AudienceNone) {
			panic("notifications: feed channel applicability and audience disagree: " + string(r.Key))
		}
		if r.Push.Applicable == (r.Push.Audience == AudienceNone) {
			panic("notifications: push channel applicability and audience disagree: " + string(r.Key))
		}
		if !r.Feed.Applicable && r.Feed.DefaultOn {
			panic("notifications: inapplicable feed channel defaulted on: " + string(r.Key))
		}
		if !r.Push.Applicable && r.Push.DefaultOn {
			panic("notifications: inapplicable push channel defaulted on: " + string(r.Key))
		}
		m[r.Key] = r
	}
	return m
}()

// AllReasons returns a copy of the whole registry in display order, reserved
// entries included. Storage-level consumers (defaults backfill, migration)
// want this one; the form does not.
func AllReasons() []Reason {
	out := make([]Reason, len(reasonRegistry))
	copy(out, reasonRegistry)
	return out
}

// ActiveReasons returns a copy of the ACTIVE registry entries in display order
// — the form projection. Reserved reasons are not part of it: they have no
// event mapping yet, and a toggle without effect is the sk-87 defect (Q10).
func ActiveReasons() []Reason {
	out := make([]Reason, 0, len(reasonRegistry))
	for _, r := range reasonRegistry {
		if r.State == ReasonStateActive {
			out = append(out, r)
		}
	}
	return out
}

// LookupReason returns the registry entry for key.
func LookupReason(key ReasonKey) (Reason, bool) {
	r, ok := reasonByKey[key]
	return r, ok
}

// IsKnownReason reports whether key is in the registry (active or reserved).
// The preferences handler uses it to answer an unknown reason with 422 rather
// than storing junk.
func IsKnownReason(key ReasonKey) bool {
	_, ok := reasonByKey[key]
	return ok
}

// ChannelSpecOf returns the spec of one channel of one reason.
func (r Reason) ChannelSpecOf(ch Channel) ChannelSpec {
	if ch == ChannelPush {
		return r.Push
	}
	return r.Feed
}

// ReasonDefault is one row of the narrow default: the storage state a fresh
// (or migrated) account starts with.
type ReasonDefault struct {
	Key ReasonKey `json:"key"`
	// Feed / Push are the default enabled states. An inapplicable channel is
	// always false — the registry invariants refuse the opposite.
	Feed bool `json:"feed"`
	Push bool `json:"push"`
}

// DefaultChannelStates returns the narrow default for every reason in the
// registry, in display order. This is what the migration writes for every
// account (R-6 / Q5: prior explicit settings are NOT carried over) and what a
// newly created account gets.
//
// Exactly EIGHT entries have at least one channel on (seven original ones plus
// «Closed issue outcome changed», sk-489). Until sk-303.14 two of the seven were
// reserved and the form therefore showed five enabled rows; every entry is
// active now, so the two numbers coincide — which is a coincidence of this
// wave, not an identity. Reading the storage default off the form response is
// the mistake this comment exists to prevent, and it stays wrong the moment
// the next reserve is added.
func DefaultChannelStates() []ReasonDefault {
	out := make([]ReasonDefault, 0, len(reasonRegistry))
	for _, r := range reasonRegistry {
		out = append(out, ReasonDefault{Key: r.Key, Feed: r.Feed.DefaultOn, Push: r.Push.DefaultOn})
	}
	return out
}

// DefaultEnabledReasons returns the keys of reasons with at least one channel
// on by default — the original "seven" plus the outcome reason of
// sk-489, in display order.
func DefaultEnabledReasons() []ReasonKey {
	out := make([]ReasonKey, 0, len(reasonRegistry))
	for _, r := range reasonRegistry {
		if r.Feed.DefaultOn || r.Push.DefaultOn {
			out = append(out, r.Key)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Classification: "event × recipient → reason"
// ---------------------------------------------------------------------------
//
// The mapping is a function of the PAIR, not of the event alone. Storage keeps
// one row per (recipient, event), so one event must give each recipient
// exactly one reason — and different recipients of the same event legitimately
// land in different reasons. That is how the personal and the subscription
// meanings of one action are told apart: the person who just became the
// assignee reads "Issue assignment", while the watchers of a freshly created
// issue read "New issue".

// HOW TO LAND A NEW EVENT TYPE (FOR-PLAN 7).
// Classification exists in THREE implementations, and "added a type" is six
// steps, not one. Skipping any of them does not break the build and does not
// redden existing tests by itself, so the list is written here, next to the
// first implementation:
//
//  1. declare the type constant (see the EventType* block above);
//  2. decide its fate in THIS file: either auditOnlyEventTypes — silence as
//     a decision, — or a branch in Classify plus a row in HasDeliveryPath. Neither
//     one nor the other = unmapped, that is, a delivery claim that the registry cannot
//     name;
//  3. enter the type into productionEventTypes (reason_corpus_test.go). This is the
//     UNIVERSE of the completeness checks, and it is hand-written: a type forgotten there is seen by no
//     completeness check, and they all stay green;
//  4. add cases to the shared corpus testdata/reason_cases.json — at least
//     a positive and a negative one;
//  5. repeat the decision of step 2 in the SQL projection by the NEXT migration (the last
//     revision is 3, migration 000051). Applied migrations are not edited;
//     equality of the copies is held by migrations/migration_000041_test.go on the same
//     corpus;
//  6. if the type has a push path — teach the delivery path projection
//     (internal/outbox/push.go, buildPushPayload) to name its reason. It
//     degrades silently: an unfamiliar reason gives the title "Skein".
//     The guard is internal/outbox/push_classification_parity_test.go.
//
// EventFacts is everything about an event the classifier needs. It is
// deliberately a plain struct of already-extracted facts rather than the raw
// events row: the classifier stays pure and testable on a JSON corpus (T002),
// and the callers own the parsing of their own payloads.
//
// "Everything the classifier needs" is meant literally — a fact the classifier
// does not branch on has no place here. Clearing the assignee, for instance,
// is NOT such a fact: the reason stays "Issue assignment", and the absence of
// a push follows from the recipient side (nobody is the new assignee, so the
// push audience matches nobody), not from a rule about the event. Carrying it
// here as an unread field would invite the next author to write a rule for it
// that does not exist.
type EventFacts struct {
	// Type is the events.type value, e.g. "issue.updated".
	Type string
	// ChangedFields lists payload.changes[*].field for issue.updated. Ignored
	// for every other type.
	ChangedFields []string
	// ExplicitAccept marks an issue.accepted event written by the explicit
	// acceptance tool. Acceptance stamped automatically (claim, self-assign,
	// creating an issue on yourself) carries no reason at all: it is written
	// to the journal only. Without this flag the creator and the watchers
	// would start getting "Issue accepted by assignee" on every claim —
	// delivery would grow exactly where the spec decided not to change it.
	ExplicitAccept bool
	// StaleLockRelease marks an issue.unclaimed emitted by the stale-lock
	// reaper (actor is the system). A human unclaim carries no reason
	// of its own: the sibling issue.status_changed already delivers it, and
	// INV-6 forbids two notifications per action per reason.
	StaleLockRelease bool
	// OutcomeChanged marks a CLOSING event (issue.status_changed /
	// issue.closed) that ALSO changed the close outcome — a repeated close
	// with a different outcome (sk-489, US-11). There is one source of the fact, named in T003:
	// the presence of the previousCloseOutcome key in the event payload. There is no
	// second way to derive it — not from ChangedFields (the outcome is not an edit field), nor from
	// new_data (that holds the status).
	//
	// Exactly one branch reads it — issue.status_changed — and reads it ON THE
	// RECIPIENT SIDE: the issue author gets a separate
	// issue.outcome_changed notification, so the status row would be a second
	// notification about one action (R-7). A subscriber who is not the author gets it
	// as usual: for them it is a status change, which is what they subscribed to.
	OutcomeChanged bool
}

// RecipientRelation is the recipient's relation to the issue at the moment of
// the event. Every field is "is this recipient …?", so the zero value is the
// stranger who gets nothing.
type RecipientRelation struct {
	// IsSubscriber — the recipient watches the issue (directly, through a
	// subtree subscription or through the project).
	IsSubscriber bool
	// IsNewAssignee — the recipient is the assignee this event installed.
	IsNewAssignee bool
	// IsAssignee — the recipient is the issue's assignee (before or after the
	// event); used by the delegated-push audience predicate.
	IsAssignee bool
	// IsCreator — the recipient created the issue.
	IsCreator bool
	// IsActor — the recipient is the one who performed the action. Self-action
	// suppression itself belongs to fan-out; the delegated-push audience needs
	// it because a creator acting on their own task has not been delegated
	// anything.
	IsActor bool
	// IsMentioned — the recipient was mentioned by this event.
	IsMentioned bool
	// IsReminderOwner — the recipient owns the reminder that rang.
	IsReminderOwner bool
	// IsDigestRecipient — the recipient is the person this reminder DIGEST was
	// built for. It is the only relation a digest event carries: a digest
	// names several issues, so no per-issue relation of the recipient decides
	// whether the digest is theirs (see AudienceDigestRecipient).
	IsDigestRecipient bool
	// IsInvitationParty — the recipient is the invited person of a space
	// invitation being sent, or the sender of one being accepted (sk-674.18
	// T040; see AudienceInvitationParty).
	IsInvitationParty bool
}

// Outcome is the four-valued answer of the classifier. The two negative
// answers are deliberately different: an audit-only event has no delivery path
// at all and its absence of a reason is correct, whereas an unmapped event
// claims a delivery path the registry cannot name — a configuration error that
// must be visible in metrics and logs rather than silently swallowed.
type Outcome string

const (
	// OutcomeReason — the pair maps to exactly one reason.
	OutcomeReason Outcome = "reason"
	// OutcomeAuditOnly — the event lives only as a journal record; no
	// recipient gets anything and that is not a failure.
	OutcomeAuditOnly Outcome = "audit_only"
	// OutcomeNoReasonForRecipient — the event does deliver, but not to THIS
	// recipient: their relation puts them in no audience of it (a non-owner of
	// a rung reminder, a user who was not the one mentioned).
	OutcomeNoReasonForRecipient Outcome = "no_reason_for_recipient"
	// OutcomeUnmapped — the event type is unknown to the registry. Callers on
	// a delivery path must emit the observable unmapped signal (T012) instead
	// of dropping the notification quietly.
	OutcomeUnmapped Outcome = "unmapped"
)

// Classification is the classifier's answer.
type Classification struct {
	Outcome Outcome
	// Reason is set only when Outcome == OutcomeReason.
	Reason ReasonKey
}

// auditOnlyEventTypes is the closed set of event types that exist ONLY as
// journal records. Two groups live here:
//
//   - the four the spec excludes from the registry by name — claiming an
//     issue, closing, archival, creating and deleting a dependency. Closing
//     and claiming still reach people, but as a change of status: their sibling
//     issue.status_changed carries "Delegated issue status", so giving
//     them a second reason would notify twice for one action (INV-6);
//
//   - the administrative / bookkeeping types that never had a delivery path:
//     labels, pins, components, commits, project, user and component
//     lifecycle, comment edits and deletions, attachment removal, and the
//     reminder lifecycle other than the ring.
//
// Being on this list is a decision, not an omission: the classifier answers
// OutcomeAuditOnly for these and no unmapped signal is raised.
var auditOnlyEventTypes = map[string]struct{}{
	// Named exclusions from the registry.
	"issue.claimed":      {},
	"issue.closed":       {},
	"issue.archived":     {},
	"dependency.created": {},
	"dependency.deleted": {},
	// Bookkeeping on the issue itself.
	"issue.assigned":           {}, // legacy sibling of issue.updated
	"issue.priority_changed":   {}, // legacy sibling of issue.updated
	"issue.field_changed":      {},
	"issue.label_added":        {},
	"issue.label_removed":      {},
	"issue.components_changed": {},
	"issue.commits_attached":   {},
	"issue.pinned":             {},
	"issue.unpinned":           {},
	"issue.delegation_dropped": {},
	"issue.delegation_resumed": {},
	// Private review bookkeeping (INV-2). Marking and flipping the
	// flag do NOT produce a notification for any recipient, including the
	// author: these are journal records and nothing else. Being listed here
	// is required — otherwise the classifier would answer unmapped, i.e. a
	// "configuration error", and privacy would look like a breakage. A
	// separate obligation is not to publish these rows onto the bus (T008);
	// the type list for that lives there.
	"review.marked":       {},
	"review.flag_changed": {},
	// Comments and attachments beyond the two notifiable actions.
	"comment.edited":     {},
	"comment.deleted":    {},
	"attachment.removed": {},
	// Reminder lifecycle other than the ring.
	"reminder.set":       {},
	"reminder.snoozed":   {},
	"reminder.dismissed": {},
	// Project / component / user administration.
	"project.created":             {},
	"project.updated":             {},
	"project.archived":            {},
	"project.unarchived":          {},
	"project.field_changed":       {},
	"project.alias_added":         {},
	"project.alias_removed":       {},
	"project.repo_added":          {},
	"project.repo_removed":        {},
	"project.repo_updated":        {},
	"project.member_added":        {},
	"project.member_removed":      {},
	"project.member_role_changed": {},
	"component.created":           {},
	"component.updated":           {},
	"component.archived":          {},
	"component.unarchived":        {},
	"component.field_changed":     {},
	"user.created":                {},
	"user.updated":                {},
	"user.field_changed":          {},
	"user.deactivated":            {},
	"user.reactivated":            {},
	"user.password_changed":       {},
}

// IsAuditOnlyEventType reports whether the event type is journal-only by
// decision. Used by the unmapped-signal path to tell a legitimate silence from
// a configuration error.
func IsAuditOnlyEventType(eventType string) bool {
	_, ok := auditOnlyEventTypes[eventType]
	return ok
}

// AuditOnlyEventTypes returns the closed journal-only set, sorted, as a copy.
//
// It exists for ONE caller: the regression that proves the historical
// migration's SQL projection (T020) carries the same set as this package. The
// migration cannot call Go, so the set lives there as a second copy, and a
// second copy is only safe while something compares the two. Comparing them by
// enumeration is the point — a test that re-typed the list by hand would prove
// the list matches itself.
func AuditOnlyEventTypes() []string {
	out := make([]string, 0, len(auditOnlyEventTypes))
	for t := range auditOnlyEventTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// fieldAssignee / fieldPriority are the two issue.updated fields that carry a
// delivery path. Every other edited field (title, description, type, due date,
// notes) delivers nothing — extending that set is a separate decision, not an
// inheritance from the incumbent.
const (
	fieldAssignee = "assignee"
	fieldPriority = "priority"
)

// Classify maps one (event, recipient) pair onto at most one reason.
//
// When several candidate reasons apply to the same recipient, the most
// specific one wins: "Issue assignment" beats "Other issue edits" and
// "New issue", so an edit that changed both the assignee and the priority
// produces one notification about the assignment, not two rows and not a
// demand that the user enable two reasons to see one change.
func Classify(ev EventFacts, rc RecipientRelation) Classification {
	if _, ok := auditOnlyEventTypes[ev.Type]; ok {
		return Classification{Outcome: OutcomeAuditOnly}
	}

	var candidates []ReasonKey

	switch ev.Type {
	case "issue.created":
		if rc.IsNewAssignee {
			candidates = append(candidates, ReasonKeyAssignment)
		}
		if rc.IsSubscriber {
			candidates = append(candidates, ReasonKeyIssueCreated)
		}

	case "issue.updated":
		assignee, priority := false, false
		for _, f := range ev.ChangedFields {
			switch f {
			case fieldAssignee:
				assignee = true
			case fieldPriority:
				priority = true
			}
		}
		if !assignee && !priority {
			// Edits of other fields produce no delivery: the event lives in
			// the journal, and that is not a failure.
			return Classification{Outcome: OutcomeAuditOnly}
		}
		if assignee && (rc.IsNewAssignee || rc.IsSubscriber) {
			candidates = append(candidates, ReasonKeyAssignment)
		}
		if priority && rc.IsSubscriber {
			candidates = append(candidates, ReasonKeyOtherEdits)
		}

	case "issue.status_changed":
		// R-7: "one close or one outcome change — one notification to the author".
		// When a close INCIDENTALLY changed the outcome, the author is addressed with the reason
		// outcome_changed (the branch below), and a status row for them is a second one
		// about one action. So IsCreator WINS over IsSubscriber here:
		// an author subscribed to their own issue would otherwise get it
		// exactly as a subscriber. All other recipients do not notice the branch.
		switch {
		case ev.OutcomeChanged && rc.IsCreator:
			// no candidates — no_reason_for_recipient
		case rc.IsSubscriber || rc.IsCreator:
			candidates = append(candidates, ReasonKeyDelegatedStatus)
		}

	case EventTypeOutcomeChanged:
		// There is one addressee — the issue author. A subscriber does not get a separate row about
		// an outcome change: for the same action they are told by the status row
		// (on a repeated close) or by nothing (on an outcome edit),
		// and this is the maintainers' decision, not a consequence of the audiences.
		if rc.IsCreator {
			candidates = append(candidates, ReasonKeyOutcomeChanged)
		}

	case "issue.accepted":
		if !ev.ExplicitAccept {
			return Classification{Outcome: OutcomeAuditOnly}
		}
		if rc.IsSubscriber || rc.IsCreator {
			candidates = append(candidates, ReasonKeyAccepted)
		}

	case "issue.unclaimed":
		if !ev.StaleLockRelease {
			return Classification{Outcome: OutcomeAuditOnly}
		}
		if rc.IsSubscriber {
			candidates = append(candidates, ReasonKeyAutoUnclaim)
		}

	case "issue.reparented":
		if rc.IsSubscriber {
			candidates = append(candidates, ReasonKeyReparent)
		}

	case "comment.created":
		if rc.IsSubscriber {
			candidates = append(candidates, ReasonKeyComment)
		}

	case "attachment.added":
		if rc.IsSubscriber {
			candidates = append(candidates, ReasonKeyAttachment)
		}

	case "comment.mention", "issue.mention":
		if rc.IsMentioned {
			candidates = append(candidates, ReasonKeyMention)
		}

	case "reminder.rang":
		if rc.IsReminderOwner {
			candidates = append(candidates, ReasonKeyReminder)
		}

	case EventTypeUnacceptedDigest:
		// Categories are not merged (R-4): each digest carries its OWN
		// reason, and unsubscribing from one does not silence the other.
		if rc.IsDigestRecipient {
			candidates = append(candidates, ReasonKeyUnacceptedReminder)
		}

	case EventTypeUnreviewedDigest:
		if rc.IsDigestRecipient {
			candidates = append(candidates, ReasonKeyReviewReminder)
		}

	default:
		return Classification{Outcome: OutcomeUnmapped}
	}

	if len(candidates) == 0 {
		return Classification{Outcome: OutcomeNoReasonForRecipient}
	}

	best := candidates[0]
	for _, k := range candidates[1:] {
		if reasonByKey[k].Priority > reasonByKey[best].Priority {
			best = k
		}
	}
	return Classification{Outcome: OutcomeReason, Reason: best}
}

// HasDeliveryPath reports whether the event type projects onto a reason for at
// least one kind of recipient. It is the predicate behind "an event with a
// delivery path but no reason is a configuration error": a type that answers
// false here is either audit-only by decision or unknown to the registry.
func HasDeliveryPath(eventType string) bool {
	if _, ok := auditOnlyEventTypes[eventType]; ok {
		return false
	}
	switch eventType {
	case "issue.created", "issue.updated", "issue.status_changed",
		"issue.accepted", "issue.unclaimed", "issue.reparented",
		"comment.created", "attachment.added",
		"comment.mention", "issue.mention", "reminder.rang",
		EventTypeUnacceptedDigest, EventTypeUnreviewedDigest,
		EventTypeOutcomeChanged:
		return true
	}
	return false
}

// ChannelAudienceMatches reports whether the recipient falls into the audience
// of one channel of one reason. This is the AUDIENCE half of the fan-out's
// decision; the preference half ("is this channel enabled for this user") is
// composed with it in T007. An inapplicable channel never matches.
func ChannelAudienceMatches(key ReasonKey, ch Channel, rc RecipientRelation) bool {
	r, ok := reasonByKey[key]
	if !ok {
		return false
	}
	spec := r.ChannelSpecOf(ch)
	if !spec.Applicable {
		return false
	}
	switch spec.Audience {
	case AudienceSubscribers:
		return rc.IsSubscriber
	case AudienceNewAssignee:
		return rc.IsNewAssignee
	case AudienceCreator:
		return rc.IsCreator
	case AudienceDelegatingCreator:
		// A really delegated issue: the creator is neither the assignee nor
		// the actor of this action.
		return rc.IsCreator && !rc.IsAssignee && !rc.IsActor
	case AudienceMentioned:
		return rc.IsMentioned
	case AudienceReminderOwner:
		return rc.IsReminderOwner
	case AudienceDigestRecipient:
		return rc.IsDigestRecipient
	case AudienceInvitationParty:
		return rc.IsInvitationParty
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Subscription-event catalogue
// ---------------------------------------------------------------------------
//
// Subscription filters (the event-type whitelist on a project subscription and
// on custom subscriptions) survive this wave and keep speaking the language of
// event types: they are a property of the SUBSCRIPTION, not of the channel,
// and the two axes multiply — a row is created when the reason is enabled AND
// the subscription filter lets the event through (R-4 / Q1).
//
// What does change is the catalogue they offer. A type with no delivery is a
// dead toggle on whatever surface it appears, so the catalogue lists exactly
// the types that reach a subscriber through the watcher pool. Personally
// addressed reasons — mention, reminder and, since sk-303.14, the two reminder
// DIGESTS — are NOT in it: they reach their recipient regardless of any
// subscription (INV-8), so offering them as subscription filters would be
// another toggle without effect. The private review types are not in it
// either, for the stronger reason that they reach nobody at all.

// SubscriptionEventType is one catalogue entry.
type SubscriptionEventType struct {
	// Type is the events.type value stored in the subscription filter.
	Type string `json:"type"`
	// Label is the human-readable name for the filter UI.
	Label string `json:"label"`
	// Reasons lists EVERY reason this event type can project into for a
	// subscriber. It lets the UI group the filter by the same vocabulary the
	// settings form uses.
	//
	// It is a list and not a single key because the classifier splits some
	// types by payload: issue.updated reaches a subscriber as "Issue
	// assignment" when the assignee changed and as "Other issue edits" when
	// only the priority did. Naming one of the two would make the field a
	// lie for the other half and would group a filter under a reason whose
	// switch does not govern it — turning the assignment reason off would
	// silently take the priority edits with it.
	Reasons []ReasonKey `json:"reasons"`
}

// subscriptionCatalog is the ordered catalogue of subscription-filterable
// event types. Order is display order.
var subscriptionCatalog = []SubscriptionEventType{
	{Type: "issue.created", Label: "New issue", Reasons: []ReasonKey{ReasonKeyIssueCreated}},
	{Type: "issue.updated", Label: "Issue edit", Reasons: []ReasonKey{ReasonKeyAssignment, ReasonKeyOtherEdits}},
	{Type: "issue.status_changed", Label: "Status change", Reasons: []ReasonKey{ReasonKeyDelegatedStatus}},
	{Type: "issue.accepted", Label: "Issue accepted by assignee", Reasons: []ReasonKey{ReasonKeyAccepted}},
	{Type: "issue.unclaimed", Label: "Automatic unclaim", Reasons: []ReasonKey{ReasonKeyAutoUnclaim}},
	{Type: "issue.reparented", Label: "Moved in the tree", Reasons: []ReasonKey{ReasonKeyReparent}},
	{Type: "comment.created", Label: "Comment", Reasons: []ReasonKey{ReasonKeyComment}},
	{Type: "attachment.added", Label: "Attachment", Reasons: []ReasonKey{ReasonKeyAttachment}},
}

// subscriptionCatalogIndex indexes the catalogue and asserts, at package init,
// that every entry really has a delivery path and a known reason. The check is
// what keeps "the catalogue lists only deliverable types" true by construction
// rather than by review.
var subscriptionCatalogIndex = func() map[string]SubscriptionEventType {
	m := make(map[string]SubscriptionEventType, len(subscriptionCatalog))
	for _, e := range subscriptionCatalog {
		if !HasDeliveryPath(e.Type) {
			panic("notifications: subscription catalogue lists an undeliverable type: " + e.Type)
		}
		if len(e.Reasons) == 0 {
			panic("notifications: subscription catalogue entry without a reason: " + e.Type)
		}
		for _, key := range e.Reasons {
			if _, ok := reasonByKey[key]; !ok {
				panic("notifications: subscription catalogue entry with unknown reason: " + e.Type)
			}
		}
		if _, dup := m[e.Type]; dup {
			panic("notifications: duplicate type in subscription catalogue: " + e.Type)
		}
		m[e.Type] = e
	}
	return m
}()

// SubscriptionEventTypes returns a copy of the catalogue in display order.
func SubscriptionEventTypes() []SubscriptionEventType {
	out := make([]SubscriptionEventType, 0, len(subscriptionCatalog))
	for _, e := range subscriptionCatalog {
		// The reason slice is copied too: a caller appending to the returned
		// entry would otherwise write into the package-level catalogue.
		reasons := make([]ReasonKey, len(e.Reasons))
		copy(reasons, e.Reasons)
		e.Reasons = reasons
		out = append(out, e)
	}
	return out
}

// IsSubscriptionEventType reports whether the event type may be stored in a
// subscription filter. A value that fails this check is a BUSINESS error, not
// a syntax error: the request was well-formed and the caller named a type that
// either does not exist or delivers nothing, so the answer is 422, while a
// malformed body (not an array, not strings, broken JSON) stays 400.
func IsSubscriptionEventType(eventType string) bool {
	_, ok := subscriptionCatalogIndex[eventType]
	return ok
}
