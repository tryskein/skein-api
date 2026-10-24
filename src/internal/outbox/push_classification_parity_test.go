package outbox

// push_classification_parity_test.go — coverage-matrix row 18
// (sk-303.14 / T005).
//
// WHY A SEPARATE FILE. Event classification exists in THREE implementations:
// the reason registry in code (internal/notifications), the function in the
// migration (000041 → 000045) and the REASON PROJECTION IN THE DELIVERY
// TRACT — the switch in buildPushPayload. The first two are checked against
// each other on a shared corpus in migrations/migration_000041_test.go, and
// their agreement says nothing about the third: it answers a different
// question ("reason → what the person will see") and degrades in silence
// rather than failing. A reason it does not know gives the device the title
// "Skein" — honest for a row from a NEWER build and a silent lie for a
// reason this build's own registry already serves: the person gets a push
// that says nothing.
//
// So the assertion here is separate, and the input is THE SAME corpus:
// checking the tract against a reason list re-typed by hand in this file
// would be comparing a copy with a copy.

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"skein-api/internal/notifications"
)

// corpusPath is the shared corpus, the same file the runtime classifier and
// the SQL-projection check read.
const corpusPath = "../notifications/testdata/reason_cases.json"

type tractCorpusCase struct {
	ID        string `json:"id"`
	Recipient struct {
		Subscriber      bool `json:"subscriber"`
		NewAssignee     bool `json:"newAssignee"`
		Assignee        bool `json:"assignee"`
		Creator         bool `json:"creator"`
		Actor           bool `json:"actor"`
		Mentioned       bool `json:"mentioned"`
		ReminderOwner   bool `json:"reminderOwner"`
		DigestRecipient bool `json:"digestRecipient"`
	} `json:"recipient"`
	Expect struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
		Feed    bool   `json:"feed"`
		Push    bool   `json:"push"`
	} `json:"expect"`
}

func loadTractCorpus(t *testing.T) []tractCorpusCase {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("reading corpus %s: %v", corpusPath, err)
	}
	var shell struct {
		Cases []json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	out := make([]tractCorpusCase, 0, len(shell.Cases))
	for i, rawCase := range shell.Cases {
		var c tractCorpusCase
		// Strict decode: a typo in a field name ("reciepient") would become
		// a zero value, and the case would assert something other than what
		// is written. This file does not need the event fields, so only
		// recipient and expect are decoded — and to keep strictness from
		// tripping over event, the nested objects are decoded separately.
		d := json.NewDecoder(bytes.NewReader(rawCase))
		var probe struct {
			ID        string          `json:"id"`
			Note      string          `json:"note"`
			Event     json.RawMessage `json:"event"`
			Recipient json.RawMessage `json:"recipient"`
			Expect    json.RawMessage `json:"expect"`
		}
		d.DisallowUnknownFields()
		if err := d.Decode(&probe); err != nil {
			t.Fatalf("parsing corpus case #%d: %v", i, err)
		}
		c.ID = probe.ID
		for name, raw := range map[string]json.RawMessage{
			"recipient": probe.Recipient, "expect": probe.Expect,
		} {
			if len(raw) == 0 {
				continue
			}
			dd := json.NewDecoder(bytes.NewReader(raw))
			dd.DisallowUnknownFields()
			var err error
			if name == "recipient" {
				err = dd.Decode(&c.Recipient)
			} else {
				err = dd.Decode(&c.Expect)
			}
			if err != nil {
				t.Fatalf("parsing %s of corpus case %s: %v", name, probe.ID, err)
			}
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatal("corpus is empty — the regression would prove nothing")
	}
	return out
}

// samplePushEvent is the issue projection the tract holds when it renders a
// payload. Values are arbitrary but non-empty: an empty title or id would make
// "title is not empty" true for the wrong reason.
func samplePushEvent() eventIssue {
	return eventIssue{
		issueID:   "sk-1",
		title:     "Issue",
		newData:   []byte(`"in_progress"`),
		actor:     "",
		actorType: "human",
	}
}

// TestTractProjectionNamesEveryReasonTheCorpusPushes — parity of the third
// implementation on the SHARED corpus.
//
// The assertion is narrow and checkable: if the corpus says the pair
// "event × recipient" yields a reason AND the recipient is in the PUSH
// audience, the tract must name that reason with its own title, not the
// generic fallback. Reasons without an applicable push channel never reach
// the tract at all and are outside the assertion — otherwise it would
// demand a title for something that does not go to a device.
func TestTractProjectionNamesEveryReasonTheCorpusPushes(t *testing.T) {
	checked := map[notifications.ReasonKey]bool{}
	for _, c := range loadTractCorpus(t) {
		if c.Expect.Outcome != string(notifications.OutcomeReason) || !c.Expect.Push {
			continue
		}
		key := notifications.ReasonKey(c.Expect.Reason)
		checked[key] = true

		got := buildPushPayload(string(key), samplePushEvent(), "")
		if got.Title == pushHeadUnknownReason {
			t.Errorf("%s: reason %q reaches a device, but the delivery tract renders the "+
				"unknown-reason head %q — the push would say nothing about what happened",
				c.ID, key, pushHeadUnknownReason)
		}
		if got.Category != string(key) {
			t.Errorf("%s: payload category = %q, want the reason key %q", c.ID, got.Category, key)
		}
	}
	if len(checked) == 0 {
		t.Fatal("no corpus case produces a push — the parity assertion is inert")
	}

	// The other direction: the registry can introduce a reason with a push
	// channel for which the corpus has no delivery case. Then the loop
	// above would not see it, and green would mean "we did not check", not
	// "it agrees". Such a reason is named out loud.
	//
	// The one exception is a direct-writer reason (Reason.DirectWriter,
	// sk-674.18 T040): its rows are written by the producer of the action,
	// never by the fan-out, so the classifier's corpus cannot contain it by
	// construction (notifications#TestDirectWriterReasonsNeverClassified).
	// Its payload is asserted by its own test (push_invitation_test.go); it
	// is named here so the skip is visible, not silent.
	for _, r := range notifications.ActiveReasons() {
		if !r.Push.Applicable || checked[r.Key] {
			continue
		}
		if r.DirectWriter {
			t.Logf("reason %s is a direct writer: outside the classifier corpus, its payload is checked by push_invitation_test.go", r.Key)
			continue
		}
		t.Errorf("reason %s has an applicable push channel but no corpus case delivers it — "+
			"the tract projection for it is unchecked", r.Key)
	}
}

// TestTractProjectionFallsBackOnlyForUnknownReasons — a control shot against
// tautology: the assertion above would only mean something if the fallback
// is reachable at all. A reason the registry does not know must get it —
// that is how the tract treats a row written by a newer build.
func TestTractProjectionFallsBackOnlyForUnknownReasons(t *testing.T) {
	got := buildPushPayload("reason_from_the_future", samplePushEvent(), "")
	if got.Title != pushHeadUnknownReason {
		t.Errorf("unknown reason rendered head %q, want the fallback %q — "+
			"the parity assertion above cannot fail if nothing reaches the fallback",
			got.Title, pushHeadUnknownReason)
	}
	// And the fallback does not invent content: the body is still about the issue.
	if !strings.Contains(got.Body, "sk-1") {
		t.Errorf("fallback payload body = %q, want it to still anchor on the issue id", got.Body)
	}
}

// TestTractProjectionNamesBothDigests — the two reminder-digest categories do not
// merge (R-4) in the tract either: each has its own title, and they are
// distinct. The shared check above would catch a missing title, but would
// NOT catch both digests rendering as the same text.
func TestTractProjectionNamesBothDigests(t *testing.T) {
	unaccepted := buildPushPayload(string(notifications.ReasonKeyUnacceptedReminder), samplePushEvent(), "")
	unreviewed := buildPushPayload(string(notifications.ReasonKeyReviewReminder), samplePushEvent(), "")

	if unaccepted.Title == unreviewed.Title {
		t.Errorf("both reminder digests render the same title %q — the two categories are "+
			"different notifications with different destinations, not one text with two sections",
			unaccepted.Title)
	}

	// Distinctness is NOT ENOUGH. Swapping the two titles still leaves them
	// distinct, and that mutation survived this test: the acceptance
	// category would tell the person to review, the review category to
	// accept. So each title is pinned to its category by name.
	if !strings.Contains(unaccepted.Title, "acceptance") {
		t.Errorf("unaccepted digest title = %q — must name ACCEPTANCE, not something else", unaccepted.Title)
	}
	if !strings.Contains(unreviewed.Title, "review") {
		t.Errorf("unreviewed digest title = %q — must name REVIEW, not something else", unreviewed.Title)
	}
	for _, p := range []pushPayload{unaccepted, unreviewed} {
		if p.Title == pushHeadUnknownReason || p.Title == "" {
			t.Errorf("digest payload title = %q — the reason is active in this build's registry", p.Title)
		}
	}
}
