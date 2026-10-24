package instructions

import (
	"strings"
	"testing"
)

// TestFullGuide_HasStandingSection asserts the agent operational guide
// (returned by skein_get_instructions) still documents the standing-task
// feature. The section is deliberately terse (sk-118 F4, trimmed sk-132): the
// startup guide carries only what an agent must know BEFORE acting — the rest
// surfaces as self-documenting error codes on contact. This test just guards
// that the section is not dropped entirely.
func TestFullGuide_HasStandingSection(t *testing.T) {
	if !strings.Contains(FullGuide, "## Standing tasks") {
		t.Fatal("full.md is missing the \"## Standing tasks\" section")
	}
}

// TestFullGuide_StandingWorkingModel asserts the guide carries the load-bearing
// operational facts an agent needs proactively (not the full code list, which
// is intentionally left to the error messages): that standing isn't claimable,
// the spawn-a-child working model, the create/promote verbs, and the aggregate
// pill. These are the behaviours that would otherwise be silently lost in an
// edit — each string is a contiguous substring of the terse section.
func TestFullGuide_StandingWorkingModel(t *testing.T) {
	wants := []string{
		"cannot_claim_standing",         // not claimable
		"skein_create(parentId=",        // spawn-a-child working model
		"skein_create(standing:",        // create-as-standing flag
		"skein_update(status=standing)", // promote
		"standingCount",                 // the aggregate pill
	}
	for _, w := range wants {
		if !strings.Contains(FullGuide, w) {
			t.Errorf("full.md standing section is missing operational fact %q", w)
		}
	}
}
