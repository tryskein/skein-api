package notifications

// reason_form_coverage_test.go — coverage-matrix row 20
// (sk-303.14 / T005): "no new type appears in the settings form twice
// and none is left undisplayed".
//
// What counts as "the settings form" here. The server gives the client TWO
// server-driven surfaces, and both are the form:
//
//   - the reason registry (ActiveReasons) — toggles "what to bother me about";
//   - the subscription-filter catalogue (SubscriptionEventTypes) — "which
//     events of issues I watch".
//
// The axes multiply (R-4/Q1), so a subscription type is legitimately visible
// on both: its reason in the first, itself in the second. An ADDRESSED type
// must NOT be in the second: it arrives regardless of subscription (INV-8),
// and a filter on it would be a second toggle over the same signal — exactly
// "appeared in the form twice".
//
// "Undisplayed" is the inverse error: the type is delivered, but no toggle
// governs it. The user gets a notification they cannot switch off.

import "testing"

// waveTwoEventTypes — the four private and digest types sk-303.14 introduces. The list is
// handwritten on purpose: the claim is about THESE specifically, and deriving
// it from the code would be asking the subject what it added. Overall
// completeness (every production type) is checked by the second test below,
// where the universe is taken from productionEventTypes.
var waveTwoEventTypes = []string{
	EventTypeReviewMarked,
	EventTypeReviewFlagChanged,
	EventTypeUnacceptedDigest,
	EventTypeUnreviewedDigest,
}

// formReasonCounts — how many times each reason occurs in the form. A value
// greater than one IS "appeared in the form twice".
func formReasonCounts() map[ReasonKey]int {
	out := map[ReasonKey]int{}
	for _, r := range ActiveReasons() {
		out[r.Key]++
	}
	return out
}

// reasonsProducedByType — the set of reasons the type projects into for at
// least one recipient. Taken from the SHARED corpus plus the "recipient in
// every relation at once" probe: the corpus names real pairs, the probe
// catches a reason no case has been written for yet.
func reasonsProducedByType(t *testing.T, eventType string) map[ReasonKey]bool {
	t.Helper()
	out := map[ReasonKey]bool{}
	for _, c := range loadCorpus(t).Cases {
		if c.Event.Type != eventType || c.Expect.Outcome != string(OutcomeReason) {
			continue
		}
		out[ReasonKey(c.Expect.Reason)] = true
	}
	probe := Classify(EventFacts{
		Type:             eventType,
		ChangedFields:    []string{"assignee", "priority"},
		ExplicitAccept:   true,
		StaleLockRelease: true,
	}, allRelations())
	if probe.Outcome == OutcomeReason {
		out[probe.Reason] = true
	}
	return out
}

// TestWaveTwoTypesShownOnceOrDeclaredSilent — row 20 for the four private
// and digest types.
func TestWaveTwoTypesShownOnceOrDeclaredSilent(t *testing.T) {
	counts := formReasonCounts()

	for _, et := range waveTwoEventTypes {
		t.Run(et, func(t *testing.T) {
			// No new type is a subscription filter: the two digests are
			// addressed (INV-8), the two private ones are not delivered at all.
			if IsSubscriptionEventType(et) {
				t.Errorf("%s is offered as a subscription filter — a second switch over the "+
					"same signal, which is exactly \"appeared in the form twice\"", et)
			}

			produced := reasonsProducedByType(t, et)

			if !HasDeliveryPath(et) {
				// The private half: silence must be DECLARED. An unknown type
				// also shows nothing in the form, but for a different reason —
				// it is a configuration error, and it must not be confused with
				// privacy.
				if !IsAuditOnlyEventType(et) {
					t.Fatalf("%s neither delivers nor is declared audit-only — its silence is "+
						"an unmapped hole, not a decision", et)
				}
				if got := Classify(EventFacts{Type: et}, allRelations()).Outcome; got != OutcomeAuditOnly {
					t.Errorf("%s: classifier answers %q for a recipient in every relation, want audit_only", et, got)
				}
				if len(produced) != 0 {
					t.Errorf("%s is audit-only yet projects into %v", et, produced)
				}
				return
			}

			// The delivered half: exactly one reason, and it is in the form
			// exactly once.
			if len(produced) != 1 {
				t.Fatalf("%s projects into %d reason(s) %v — a digest carries exactly one, "+
					"otherwise one signal would need two switches", et, len(produced), produced)
			}
			for key := range produced {
				r, ok := LookupReason(key)
				if !ok {
					t.Fatalf("%s projects into reason %s which the registry does not know", et, key)
				}
				if r.State != ReasonStateActive {
					t.Errorf("%s projects into %s, whose state is %q — a delivered type governed by "+
						"no visible switch is \"undisplayed\"", et, key, r.State)
				}
				switch counts[key] {
				case 1:
					// as it should be
				case 0:
					t.Errorf("%s delivers as %s, but the form shows no row for it", et, key)
				default:
					t.Errorf("%s delivers as %s, which the form shows %d times", et, key, counts[key])
				}
			}
		})
	}
}

// TestEveryDeliveredTypeIsGovernedByExactlyOneFormRow — the same row 20, but
// now for the WHOLE universe of production types, not the four new ones: a
// type added later and forgotten in waveTwoEventTypes will be caught here.
func TestEveryDeliveredTypeIsGovernedByExactlyOneFormRow(t *testing.T) {
	counts := formReasonCounts()
	for key, n := range counts {
		if n > 1 {
			t.Errorf("reason %s occurs %d times in the form — one switch per reason", key, n)
		}
	}

	checked := 0
	for _, et := range productionEventTypes {
		if !HasDeliveryPath(et) {
			continue
		}
		produced := reasonsProducedByType(t, et)
		if len(produced) == 0 {
			t.Errorf("%s has a delivery path but projects into no reason at all — "+
				"it would arrive with nothing to switch it off", et)
			continue
		}
		checked++
		for key := range produced {
			r, ok := LookupReason(key)
			if !ok {
				t.Errorf("%s projects into unknown reason %s", et, key)
				continue
			}
			if r.State != ReasonStateActive || counts[key] != 1 {
				t.Errorf("%s delivers as %s (state %q), which the form shows %d time(s) — "+
					"a delivered type must be governed by exactly one visible row",
					et, key, r.State, counts[key])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no production type with a delivery path was examined — the assertion is inert")
	}
}
