package instructions

import "testing"

// TestFullGuide_StandingWorkingModel asserts the guide still documents the
// standing-task feature (the section heading is what standingGuideRef in
// the tool schemas points at) and carries the working model an agent needs
// BEFORE acting: the spawn-a-child model and the create/promote verbs. The
// refusal codes are deliberately left to the error messages (sk-118 F4,
// trimmed sk-132, table dropped sk-908). Each string is a contiguous
// substring of the section.
func TestFullGuide_StandingWorkingModel(t *testing.T) {
	requireSectionFacts(t, "## Standing tasks", []string{
		"skein_create(parentId=",        // spawn-a-child working model
		"skein_create(standing:",        // create-as-standing flag
		"skein_update(status=standing)", // promote
	})
}
