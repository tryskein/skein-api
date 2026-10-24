package instructions

import (
	"strings"
	"testing"
)

// guide_defaults_test.go — sk-282 T027, matrix rows 99 and 100.
//
// Row 100: "US-10: the guide no longer describes the old defaults (list
// page, 'the whole thread as a dump', search page default, project list
// default)".
//
// The guide is embedded in the binary and shipped with the code, so a
// sentence here that teaches a retired contract is not documentation drift
// — it is a wrong instruction delivered by the server, to every agent, at
// session start, with the authority of the tool that serves it. This run
// found such a sentence four times in a row; these two tests are what
// stops the fifth.

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

// TestFullGuide_StatesTheCurrentDefaults is the positive half of row 100.
//
// Forbidding the old sentences is only half the job: a guide that simply
// deleted them would pass the test above and still leave an agent with no
// idea what it gets by default. Each fact below is one an agent needs
// BEFORE it calls anything, and each is checked as a fact, not as a phrase
// — the numbers are what matter.
func TestFullGuide_StatesTheCurrentDefaults(t *testing.T) {
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
			what:  "list and search page defaults",
			needs: []string{"25 rows", "25 hits"},
		},
		{
			what:  "project list page default and its cut description",
			needs: []string{"50 rows", "descriptionTruncated"},
		},
		{
			what:  "the write-tools answer with an acknowledgement",
			needs: []string{"acknowledgement", "`version`"},
		},
		{
			what:  "absent keys are not empty values",
			needs: []string{"absent", "valuesTruncated"},
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

// TestFullGuide_ListHopForAnAbsentKeyNamesTheSpelling guards the OTHER
// half of a narrowing: not the inventory of what is missing, but the
// route back.
//
// An earlier fix corrected the inventory first — the guide stopped claiming
// `componentIds` rides on a default row — and left the route alone, so it
// still sent an agent to a bare `skein_list` for a key that row does not
// carry either. That is the same defect from the other side: the agent
// walks the named route, meets a second absence with no explanation, and
// reads it as "this issue has no components".
//
// So: any "way to more" cell that names `componentIds` AND `skein_list`
// must also name `fields`, because that is the only spelling of the hop
// that answers. `labels` needs no such qualifier — it IS in the default
// row — which is why the rule keys on the field, not on the tool.
func TestFullGuide_ListHopForAnAbsentKeyNamesTheSpelling(t *testing.T) {
	checked := 0
	for _, line := range strings.Split(FullGuide, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "| `skein_") {
			continue
		}
		if !strings.Contains(line, "componentIds") || !strings.Contains(line, "skein_list") {
			continue
		}
		checked++
		if !strings.Contains(line, "fields") {
			t.Errorf("this defaults-table row sends the reader to skein_list for "+
				"componentIds without naming `fields`, and the default row does not "+
				"carry that key either — the route ends on a second absence:\n%s", line)
		}
	}
	if checked == 0 {
		t.Error("no defaults-table row mentions componentIds and skein_list together — " +
			"either the table lost the rows this guards, or the guard's anchor rotted; " +
			"a guard that checks nothing is worse than no guard")
	}
}
