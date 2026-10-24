package issues

// close_outcome_test.go — sk-489, matrix row 4: INV-2 on the server side.
//
// "The outcome is one of four values; the server rejects anything else on input." There is
// nothing to check here except the boundary of the set — and the boundary is the subject:
// a validator that accepts a fifth value lets it into the column under CHECK
// (a database refusal instead of a clear error code) and into the progress numbers, where it has
// nothing to be compared with.
//
// Pure unit: no pool, no database.

import (
	"errors"
	"testing"
)

// TestValidateCloseOutcome_AcceptsExactlyTheFourValues — the positive
// half of the set, listed as LITERALS rather than package constants.
//
// Literals on purpose: the test's job is to keep the code tied to the SPEC, while checking a constant
// against itself would pass even after the value is renamed, that is after
// changing what actually lands in the column and goes on the wire.
func TestValidateCloseOutcome_AcceptsExactlyTheFourValues(t *testing.T) {
	for _, want := range []string{"done", "wont_do", "duplicate", "superseded"} {
		got, err := ValidateCloseOutcome(want)
		if err != nil {
			t.Errorf("ValidateCloseOutcome(%q) rejected: %v — the spec names this value "+
				"as one of the four", want, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateCloseOutcome(%q) returned %q — the canonical form must match "+
				"what lies in the column and goes on the wire", want, got)
		}
	}

	// The flip side of the same claim: the set is exactly four-valued.
	// Without it "accepts four" is green even for a validator that accepts five.
	if got := ValidCloseOutcomes(); len(got) != 4 {
		t.Errorf("ValidCloseOutcomes() returned %d values (%v), the spec names exactly four",
			len(got), got)
	}
}

// TestValidateCloseOutcome_RejectsAnythingElse — the negative half, and
// each case negates its own thing.
//
//   - ""         — "no outcome" is expressed by the caller having NO value,
//     not by an empty string; allow it and the emptiness would reach
//     the column under CHECK;
//   - "Done"     — case is not normalised: SQL, the frontend and the agent all compare
//     the value as a string, and none of them
//     knows about "Done == done";
//   - "bogus"    — a value outside the set, the most direct case of INV-2.
//
// SEPARATELY, AND THIS IS NOT A REJECTION: "  done  " is ACCEPTED by the validator, returning "done" —
// the spaces are trimmed, as in ValidateStatus next to it. The claim is standalone
// (the wire carries the string as is, and a space in the column would break any
// comparison), but its sign is opposite to the list above, so it lives in
// its own branch of the test, not in the rejection loop. The review of wave sk-489/A/1 (critic
// fable) caught exactly this: the earlier edition of the doc block counted " done " among the
// negative cases, although its body accepted it.
func TestValidateCloseOutcome_RejectsAnythingElse(t *testing.T) {
	for _, bad := range []string{"", "Done", "DONE", "bogus", "won't do", "wontdo", "closed"} {
		got, err := ValidateCloseOutcome(bad)
		if err == nil {
			t.Errorf("ValidateCloseOutcome(%q) accepted as %q — INV-2: the server rejects "+
				"a value outside the four", bad, got)
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("ValidateCloseOutcome(%q) returned %T, not *ValidationError — the handler "+
				"has nothing to branch on", bad, err)
			continue
		}
		if ve.Code != "invalid_close_outcome" {
			t.Errorf("ValidateCloseOutcome(%q) gave code %q, the contract names "+
				"invalid_close_outcome — the code is machine-readable and not subject to localisation", bad, ve.Code)
		}
		if got != "" {
			t.Errorf("ValidateCloseOutcome(%q) returned the value %q along with a rejection — "+
				"a rejection must leave the caller nothing usable for a write", bad, got)
		}
	}

	// Spaces are trimmed — separately, because this is an ACCEPTANCE, not a rejection.
	if got, err := ValidateCloseOutcome("  done  "); err != nil || got != "done" {
		t.Errorf("ValidateCloseOutcome(\"  done  \") = (%q, %v), expected (\"done\", nil): "+
			"spaces are trimmed, as in ValidateStatus next to it", got, err)
	}
}

// TestIsNoWorkOutcome_TrueForExactlyTheThreeNonDone — the progress denominator
// predicate (INV-5).
//
// It is true for exactly the three non-`done` values and false for `done`. Both
// halves are needed: a predicate true for everything would throw the done work out of the denominator,
// and one false for everything would bring back today's falsehood, for which
// the feature is being made.
//
// Separately — what the predicate does NOT count as dropped out: an unknown string and the
// empty one. Both answer "I do not claim it dropped out": truth here would silently
// move the progress numbers on a value the server does not let in, and
// an unclosed issue has no outcome at all.
func TestIsNoWorkOutcome_TrueForExactlyTheThreeNonDone(t *testing.T) {
	for _, noWork := range []string{"wont_do", "duplicate", "superseded"} {
		if !IsNoWorkOutcome(noWork) {
			t.Errorf("IsNoWorkOutcome(%q) = false — an issue with this outcome is closed WITHOUT work "+
				"and must drop out of the progress denominator", noWork)
		}
	}

	if IsNoWorkOutcome("done") {
		t.Error("IsNoWorkOutcome(\"done\") = true — \"done\" is the work itself; truth here " +
			"would throw out of the denominator exactly what progress counts")
	}

	// The counting half: the predicate is true for THREE values of the set, no more.
	// Without it "true for the three named" would pass even for a predicate true
	// for everything.
	var noWork int
	for _, o := range ValidCloseOutcomes() {
		if IsNoWorkOutcome(o) {
			noWork++
		}
	}
	if noWork != 3 {
		t.Errorf("the \"no work\" predicate is true for %d values of the set, expected 3", noWork)
	}

	for _, unknown := range []string{"", "bogus", "Done", "wontdo"} {
		if IsNoWorkOutcome(unknown) {
			t.Errorf("IsNoWorkOutcome(%q) = true — an unknown value (and no outcome)"+
				" has no right to move the progress numbers: INV-2 leaves the count to the server, and "+
				"only a known non-done outcome is declared dropped out", unknown)
		}
	}
}
