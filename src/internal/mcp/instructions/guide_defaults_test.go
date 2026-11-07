package instructions

import (
	"strings"
	"testing"
)

// guide_defaults_test.go — sk-282 T027, matrix rows 99 and 100; reduced
// in sk-908 when the per-tool defaults table left the guide (the tool
// descriptions carry those defaults; the guide keeps only the reading
// rules an agent needs before any call).
//
// Row 100: "US-10: the guide no longer describes the old defaults (list
// page, 'the whole thread as a dump', search page default, project list
// default)".
//
// The guide is embedded in the binary and shipped with the code, so a
// sentence here that teaches a retired contract is not documentation drift
// — it is a wrong instruction delivered by the server, to every agent, at
// session start, with the authority of the tool that serves it. This run
// found such a sentence four times in a row; these tests are what stops
// the fifth.

// retiredGuideClaims are statements the guide is forbidden to make, each
// paired with what the wire actually does now.
//
// Written as literals rather than derived from anything: a guard that
// imports the text it checks passes by construction.
var retiredGuideClaims = []struct {
	literal string
	why     string
}{
	{
		literal: "to fetch the full filtered thread",
		why: "`commentsLimit: 0` returns the thread INDEX plus the last three in " +
			"full (US-4). An agent following this would expect every body and " +
			"plan around having them.",
	},
	{
		literal: "full thread show",
		why: "same route, other wording — there is no call that downloads a whole " +
			"thread any more.",
	},
	{
		literal: "return the issue post-state WITHOUT `description`",
		why: "the eleven US-5 write-tools stopped returning the card at all; the " +
			"acknowledgement is not \"the card minus four fields\".",
	},
	{
		literal: "flagged by `bodyOmitted: true`",
		why: "issue writes emit no such marker now. It survives on COMMENT writes, " +
			"and the guide says so there.",
	},
	{
		literal: "Everything else survives",
		why: "it does not: title, labels, componentIds, commitRefs and the rest of " +
			"the card are gone from a write response too.",
	},
	{
		literal: "default page size is 50",
		why:     "skein_list defaults to 25 and skein_search to 25 (SC-003 / T014, T015).",
	},
	{
		literal: "up to 500 rows",
		why: "the list ceiling is 100 and the applied default 25; the 500-row page " +
			"was the defect T014 closed.",
	},
	{
		literal: "stop and ask the user",
		why: "since sk-908 the agent decides a duplicate match itself (force / " +
			"duplicateOf); the mandatory ask-the-user rubric is retired.",
	},
}

// TestFullGuide_TeachesNoRetiredDefault is matrix row 100.
func TestFullGuide_TeachesNoRetiredDefault(t *testing.T) {
	if len(FullGuide) == 0 {
		t.Fatal("the guide is empty — this guard would pass on anything")
	}
	for _, retired := range retiredGuideClaims {
		if strings.Contains(FullGuide, retired.literal) {
			t.Errorf("full.md still says %q.\n%s", retired.literal, retired.why)
		}
	}
}

// TestFullGuide_StatesTheCurrentReadingRules is the positive half of row
// 100, narrowed in sk-908 to the rules that stayed in the guide.
//
// Forbidding the old sentences is only half the job: a guide that simply
// deleted them would pass the test above and still leave an agent with no
// idea what it gets back. Each fact below is one an agent needs BEFORE it
// calls anything, and each is checked as a fact, not as a phrase.
func TestFullGuide_StatesTheCurrentReadingRules(t *testing.T) {
	facts := []struct {
		what  string
		needs []string
	}{
		{
			what:  "skein_show default is counts, not edges",
			needs: []string{"depsCounts", "no edges"},
		},
		{
			what:  "commentsLimit:0 is the index plus the last three",
			needs: []string{"commentsLimit: 0", "index", "last 3"},
		},
		{
			what:  "the write-tools answer with an acknowledgement",
			needs: []string{"acknowledgement", "`version`"},
		},
		{
			what:  "absent keys are not empty values; cut values carry a marker",
			needs: []string{"absent", "valuesTruncated", "descriptionTruncated"},
		},
	}
	for _, fact := range facts {
		for _, need := range fact.needs {
			if !strings.Contains(FullGuide, need) {
				t.Errorf("full.md never states %q — missing %q. An agent reading only "+
					"the guide has to be able to predict what a call returns before "+
					"making it (US-10).", fact.what, need)
			}
		}
	}
}

// TestFullGuide_ObligationTriggersOnTheIncompletenessField is matrix row
// 99: "the trigger is the incompleteness field; the raw comment
// count does not participate in the rule".
//
// This pins the obligation's TRIGGER, and specifically the negative half — because the
// wrong trigger is the one that costs something: `commentsCount` is
// unfiltered, so an agent using it re-reads on every deliberate
// `commentsSince` window, forever, and nothing ever fails.
func TestFullGuide_ObligationTriggersOnTheIncompletenessField(t *testing.T) {
	if !strings.Contains(FullGuide, "commentsTruncated=true") {
		t.Error("full.md does not name commentsTruncated as the trigger")
	}
	// The rule must say what the flag MEANS, or "truncated" reads as
	// "some bodies were withheld" — a different claim entirely.
	if !strings.Contains(FullGuide, "fewer entries were listed than the") {
		t.Error("full.md does not define the incompleteness field (fewer entries " +
			"listed than the population holds) — without the definition an agent " +
			"cannot tell what the flag is asserting")
	}
	if !strings.Contains(FullGuide, "ONLY trigger") {
		t.Error("full.md does not state that the incompleteness field is the only " +
			"trigger — leaving room for the raw count to creep back in")
	}
	// And the raw count must appear ONLY inside its rejection.
	const rawCount = "issue.commentsCount"
	occurrences := strings.Count(FullGuide, rawCount)
	if occurrences == 0 {
		t.Fatalf("full.md never mentions %s — the rejection it needs to carry is gone",
			rawCount)
	}
	if occurrences > 1 {
		t.Errorf("full.md mentions %s %d times. The rule is that this count does NOT "+
			"participate; every mention beyond the one rejection is a place it can be "+
			"read as participating.", rawCount, occurrences)
	}
}
