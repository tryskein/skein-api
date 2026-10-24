package notifications

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// reason_corpus_test.go — coverage-matrix rows 2 and 3 of sk-303.13 (plan
// task T002): contract checks of the "event × recipient → reason"
// classification on the SHARED corpus.
//
// The corpus lives in a separate file (testdata/reason_cases.json) not for
// beauty: two independent implementations of one rule must execute it —
// the runtime classifier and the SQL projection of the historical migration
// (T020). There cannot be one executable function here; the migration does
// not call application code, so equivalence is held by the shared set of
// regressions. By reading the corpus in code (not rewriting the cases in
// Go), this test guarantees that the runtime half is checked against exactly
// the file the migration will later take.

type corpusEvent struct {
	Type          string   `json:"type"`
	ChangedFields []string `json:"changedFields"`
	// AssigneeCleared annotates the case; the classifier does NOT read it and
	// EventFacts deliberately has no such field. Clearing the assignee gives
	// the same "Issue assignment" reason, and the absence of a push is
	// expressed on the recipient side (nobody became the new assignee), not
	// by a rule about the event. The flag stays in the corpus because the
	// file is a shared contract with the SQL projection of the historical
	// migration (T020) and must name the case, not only its outcome.
	AssigneeCleared  bool `json:"assigneeCleared"`
	ExplicitAccept   bool `json:"explicitAccept"`
	StaleLockRelease bool `json:"staleLockRelease"`
	// OutcomeChanged — the sk-489 fact: the closing event INCIDENTALLY changed the outcome.
	// Parsing is strict (DisallowUnknownFields), so a field lost here
	// will not silently turn into zero — the R-7 case would fail on parsing, not
	// turn green having checked "closing without an outcome change" under the name of a change.
	OutcomeChanged bool `json:"outcomeChanged"`
}

type corpusRecipient struct {
	Subscriber    bool `json:"subscriber"`
	NewAssignee   bool `json:"newAssignee"`
	Assignee      bool `json:"assignee"`
	Creator       bool `json:"creator"`
	Actor         bool `json:"actor"`
	Mentioned     bool `json:"mentioned"`
	ReminderOwner bool `json:"reminderOwner"`
	// DigestRecipient — the digest relation. Decoding is strict
	// (DisallowUnknownFields), so a field forgotten here will not become
	// zero silently: a digest case will fail on decode, rather than go green
	// having checked a "non-addressee" under the addressee's name.
	DigestRecipient bool `json:"digestRecipient"`
}

type corpusExpect struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Feed    bool   `json:"feed"`
	Push    bool   `json:"push"`
}

type corpusCase struct {
	ID        string          `json:"id"`
	Note      string          `json:"note"`
	Event     corpusEvent     `json:"event"`
	Recipient corpusRecipient `json:"recipient"`
	Expect    corpusExpect    `json:"expect"`
}

type reasonCorpus struct {
	Cases []corpusCase `json:"cases"`
}

func loadCorpus(t *testing.T) reasonCorpus {
	t.Helper()
	raw, err := os.ReadFile("testdata/reason_cases.json")
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	// The corpus carries a documentation header ("about") beside the cases,
	// so the outer object is decoded permissively and each case strictly.
	var shell struct {
		About []string          `json:"about"`
		Cases []json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	var c reasonCorpus
	for i, rawCase := range shell.Cases {
		var cc corpusCase
		// An unknown field in a case is a corpus typo ("reciepient"), and a
		// typo that silently becomes the zero value would make the case
		// assert something nobody wrote. Fail loudly instead.
		d := json.NewDecoder(bytes.NewReader(rawCase))
		d.DisallowUnknownFields()
		if err := d.Decode(&cc); err != nil {
			t.Fatalf("parsing corpus case #%d: %v", i, err)
		}
		c.Cases = append(c.Cases, cc)
	}
	if len(c.Cases) == 0 {
		t.Fatal("corpus is empty")
	}
	return c
}

func (c corpusCase) facts() EventFacts {
	return EventFacts{
		Type:             c.Event.Type,
		ChangedFields:    c.Event.ChangedFields,
		ExplicitAccept:   c.Event.ExplicitAccept,
		StaleLockRelease: c.Event.StaleLockRelease,
		OutcomeChanged:   c.Event.OutcomeChanged,
	}
}

func (c corpusCase) relation() RecipientRelation {
	return RecipientRelation{
		IsSubscriber:      c.Recipient.Subscriber,
		IsNewAssignee:     c.Recipient.NewAssignee,
		IsAssignee:        c.Recipient.Assignee,
		IsCreator:         c.Recipient.Creator,
		IsActor:           c.Recipient.Actor,
		IsMentioned:       c.Recipient.Mentioned,
		IsReminderOwner:   c.Recipient.ReminderOwner,
		IsDigestRecipient: c.Recipient.DigestRecipient,
	}
}

// allRelations is a recipient who is in EVERY relation at once. It is needed
// by checks of the form "this type is journal-only regardless of who the
// recipient is", and it is built by REFLECTION, not a literal.
//
// The difference is load-bearing and cost the previous wave a defect. A
// literal is a handwritten list: a field added to RecipientRelation later
// stays false in it, and the check "no recipient gets anything" quietly
// stops covering the new recipient while staying green. Reflection here
// builds a UNIVERSE, not an allowlist, so it must be complete automatically
// — the opposite of allowlists derived from a struct, which are not guards.
func allRelations() RecipientRelation {
	var rc RecipientRelation
	v := reflect.ValueOf(&rc).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Bool {
			panic("notifications: RecipientRelation gained a non-bool field " +
				v.Type().Field(i).Name + " — allRelations() must be taught what «all relations» means for it")
		}
		f.SetBool(true)
	}
	return rc
}

// TestEveryRelationProbeIsComplete — the production probe "does this event
// deliver to ANYBODY" (fanout_compose.everyRelation) is written as a
// literal, and a relation forgotten in it silently narrows the probe: an
// event that delivers only to the new relation would answer
// no_reason_for_recipient — neither of the two negative answers the probe
// exists for — and the fan-out would skip recipient selection. Comparing
// with the reflective universe catches this on the next added field.
func TestEveryRelationProbeIsComplete(t *testing.T) {
	if everyRelation != allRelations() {
		t.Errorf("fanout_compose.everyRelation = %+v, every relation set = %+v — "+
			"a relation was added to RecipientRelation and not to the probe",
			everyRelation, allRelations())
	}
}

// TestReasonCorpus runs every corpus case through the runtime classifier
// and through the audience half of the channel decision.
func TestReasonCorpus(t *testing.T) {
	corpus := loadCorpus(t)
	seen := map[string]bool{}
	for _, c := range corpus.Cases {
		if seen[c.ID] {
			t.Fatalf("duplicate corpus case id %q", c.ID)
		}
		seen[c.ID] = true

		t.Run(c.ID, func(t *testing.T) {
			got := Classify(c.facts(), c.relation())
			if string(got.Outcome) != c.Expect.Outcome {
				t.Fatalf("outcome = %q, want %q (%s)", got.Outcome, c.Expect.Outcome, c.Note)
			}
			if c.Expect.Outcome != string(OutcomeReason) {
				if got.Reason != "" {
					t.Errorf("outcome %q carries reason %q — only a mapped pair names a reason",
						got.Outcome, got.Reason)
				}
				return
			}
			if string(got.Reason) != c.Expect.Reason {
				t.Fatalf("reason = %q, want %q (%s)", got.Reason, c.Expect.Reason, c.Note)
			}
			if in := ChannelAudienceMatches(got.Reason, ChannelFeed, c.relation()); in != c.Expect.Feed {
				t.Errorf("feed audience match = %v, want %v (%s)", in, c.Expect.Feed, c.Note)
			}
			if in := ChannelAudienceMatches(got.Reason, ChannelPush, c.relation()); in != c.Expect.Push {
				t.Errorf("push audience match = %v, want %v (%s)", in, c.Expect.Push, c.Note)
			}
		})
	}
}

// TestCorpusOneReasonPerPair — storage keeps one row per (recipient, event)
// pair, so the classifier must be a function: the same pair is the same
// single reason, with no dependence on evaluation order.
func TestCorpusOneReasonPerPair(t *testing.T) {
	corpus := loadCorpus(t)
	for _, c := range corpus.Cases {
		first := Classify(c.facts(), c.relation())
		for i := 0; i < 5; i++ {
			again := Classify(c.facts(), c.relation())
			if again != first {
				t.Fatalf("%s: classification is not deterministic: %+v vs %+v", c.ID, first, again)
			}
		}
	}
}

// TestCorpusCoversEveryDeliveryPath — completeness: every event type with a
// delivery path is represented in the corpus. The check applies ONLY to
// events with delivery; journal events are not under it, and their absence
// from the corpus is not a failure.
func TestCorpusCoversEveryDeliveryPath(t *testing.T) {
	corpus := loadCorpus(t)
	covered := map[string]bool{}
	for _, c := range corpus.Cases {
		covered[c.Event.Type] = true
	}
	for _, et := range productionEventTypes {
		if !HasDeliveryPath(et) {
			continue
		}
		if !covered[et] {
			t.Errorf("event type %s has a delivery path but no corpus case", et)
		}
	}
}

// TestEveryProductionEventTypeIsMappedOrExcluded — matrix row 3 at the
// classifier: every production event type is either mapped onto a reason or
// EXPLICITLY declared journal-only. A type in neither answers unmapped — an
// observable signal, not a silent loss of a notification (metric and log
// record — T012).
func TestEveryProductionEventTypeIsMappedOrExcluded(t *testing.T) {
	everyone := allRelations()
	for _, et := range productionEventTypes {
		mapped := HasDeliveryPath(et)
		excluded := IsAuditOnlyEventType(et)
		if mapped == excluded {
			t.Errorf("%s: mapped=%v excluded=%v — a type must be exactly one of the two",
				et, mapped, excluded)
			continue
		}
		// Facts are chosen so the type has a chance to produce a reason: for
		// issue.updated a deliverable changed field is needed, for acceptance —
		// the explicit tool, for unclaim — a system lock release.
		facts := EventFacts{
			Type:             et,
			ChangedFields:    []string{"assignee"},
			ExplicitAccept:   true,
			StaleLockRelease: true,
		}
		c := Classify(facts, everyone)
		if c.Outcome == OutcomeUnmapped {
			t.Errorf("%s: classifier answers unmapped — a production type must be mapped or audit-only", et)
		}
		if mapped && c.Outcome != OutcomeReason {
			t.Errorf("%s: has a delivery path but classification gave %q", et, c.Outcome)
		}
		if excluded && c.Outcome != OutcomeAuditOnly {
			t.Errorf("%s: declared audit-only but classification gave %q", et, c.Outcome)
		}
	}
}

// TestDirectWriterReasonsNeverClassified — the invariant of
// Reason.DirectWriter (sk-674.18 T040, decision of the owner 2026-10-14): a
// reason whose rows its producer writes itself is yielded by NO branch of the
// classifier and by no corpus case — otherwise the fan-out would write a
// second row for the same fact beside the producer's. Every production type
// is probed with a recipient in every relation, with each deliverable
// changed field.
func TestDirectWriterReasonsNeverClassified(t *testing.T) {
	direct := map[ReasonKey]bool{}
	for _, r := range AllReasons() {
		if r.DirectWriter {
			direct[r.Key] = true
		}
	}
	if len(direct) == 0 {
		t.Fatal("the registry declares no direct-writer reason — space_invitation (T040) is one; the invariant would be inert")
	}
	everyone := allRelations()
	for _, et := range productionEventTypes {
		for _, fields := range [][]string{nil, {"assignee"}, {"priority"}} {
			c := Classify(EventFacts{Type: et, ChangedFields: fields, ExplicitAccept: true, StaleLockRelease: true}, everyone)
			if c.Outcome == OutcomeReason && direct[c.Reason] {
				t.Errorf("%s %v classifies into direct-writer reason %s — the fan-out would duplicate the producer's row",
					et, fields, c.Reason)
			}
		}
	}
	for _, c := range loadCorpus(t).Cases {
		if direct[ReasonKey(c.Expect.Reason)] {
			t.Errorf("corpus case %s expects direct-writer reason %s — the classifier never yields it", c.ID, c.Expect.Reason)
		}
	}
}

// TestUnmappedIsDistinctFromAuditOnly — the two negative answers are not
// merged: journal silence is correct, and an unknown type with a delivery
// path must be distinguishable so it can be seen in the metric.
func TestUnmappedIsDistinctFromAuditOnly(t *testing.T) {
	rc := RecipientRelation{IsSubscriber: true}
	if got := Classify(EventFacts{Type: "issue.claimed"}, rc).Outcome; got != OutcomeAuditOnly {
		t.Errorf("audit-only type classified as %q", got)
	}
	if got := Classify(EventFacts{Type: "issue.made_up"}, rc).Outcome; got != OutcomeUnmapped {
		t.Errorf("unknown type classified as %q, want unmapped", got)
	}
	if OutcomeAuditOnly == OutcomeUnmapped {
		t.Fatal("the two negative outcomes must be distinguishable")
	}
}

// productionEventTypes is the complete list of event types production
// writes (taken from constants and literals in the codebase). It is the
// "universe" of the completeness check: every element must be either
// mapped or explicitly journal-only.
//
// ⚠ The list is HAND-WRITTEN, and this is its weak spot: a type not entered here
// is simply not seen by the completeness checks — they stay green without saying anything about it.
// Therefore a new event type is entered here by the same commit as its
// declaration; the four private and digest types were entered by
// task T005 and are listed alphabetically with the rest.
//
// issue.outcome_changed (sk-489) was entered NOT by the commit of its declaration, and this is
// a recorded deviation, not an omission: the constant declaration arrived
// separately (T001), but the list requires every element of it to be either
// mapped to a reason or declared journal-only — and the mapping arrives together
// with the registry entry, that is, by this commit (T010). Entering it earlier would have turned
// #TestEveryProductionEventTypeIsMappedOrExcluded red on a type that has
// nowhere to be mapped yet.
var productionEventTypes = []string{
	"attachment.added", "attachment.removed",
	"comment.created", "comment.deleted", "comment.edited", "comment.mention",
	"component.archived", "component.created", "component.field_changed",
	"component.unarchived", "component.updated",
	"dependency.created", "dependency.deleted",
	"issue.accepted", "issue.archived", "issue.assigned", "issue.claimed",
	"issue.closed", "issue.commits_attached", "issue.components_changed",
	"issue.created", "issue.delegation_dropped", "issue.delegation_resumed",
	"issue.field_changed", "issue.label_added", "issue.label_removed",
	"issue.mention", "issue.outcome_changed", "issue.pinned",
	"issue.priority_changed",
	"issue.reparented", "issue.status_changed", "issue.unclaimed",
	"issue.unpinned", "issue.updated",
	"nudge.unaccepted_digest", "nudge.unreviewed_digest",
	"project.alias_added", "project.alias_removed", "project.archived",
	"project.created", "project.field_changed", "project.member_added",
	"project.member_removed", "project.member_role_changed",
	"project.repo_added", "project.repo_removed", "project.repo_updated",
	"project.unarchived", "project.updated",
	"reminder.dismissed", "reminder.rang", "reminder.set", "reminder.snoozed",
	"review.flag_changed", "review.marked",
	"user.created", "user.deactivated", "user.field_changed",
	"user.password_changed", "user.reactivated", "user.updated",
}
