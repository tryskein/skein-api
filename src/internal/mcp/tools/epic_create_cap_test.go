package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
)

// epic_create_cap_test.go — the INV-5 guard on skein_epic_create's declared
// result ceiling.
//
// "The declared ceiling must not be below a legally possible answer": a
// declaration under what the tool can emit sends legitimate answers down
// the client's persist-to-disk path, where the agent gets a file reference
// instead of the result it asked for (see result_size.go for the documented
// behaviour and for MCPMaxDeclarableResultChars).
//
// The tool's heaviest legal answer is `verbose=true` on a full batch: the
// epic plus epicChildTemplateMaxItems freshly-created children, each a
// complete IssueDetail. This file measures that answer by SERIALISING it
// rather than by estimating it, and checks the declaration against it under
// the one rule the platform leaves available:
//
//   - measured ≤ the platform ceiling → the declaration must cover the
//     measured response;
//   - measured > the platform ceiling → the declaration must BE the
//     ceiling, because nothing larger can be stated and anything smaller
//     gives up inline responses the client would otherwise have kept.
//
// What the fixture is sized from (measured 2026-08-20, not estimated):
// 35 real cards read out of a live instance of this tracker
// through skein_show with comments / deps / events off, so the object
// measured is exactly the IssueDetail this tool returns.
// The implementation tasks of this very wave came to 6 208 … 9 978 bytes
// (mean 7 307); the plan-level tasks of the same epic to 1 433 … 3 568
// (mean 2 244). epicCreateReferenceCardChars rounds the heaviest of those
// up to the next thousand.
//
// The residual, stated here because the number cannot state it: there is NO
// upper bound on a card at all. issues.ValidateTitle caps the title at 1 000
// characters and ValidateLabel a label at 100 — description, design,
// acceptanceCriteria, notes and sourceContext have no length limit anywhere
// in the system (internal/issues/validate.go), and neither has the number of
// labels, components or commit refs. So no finite cap is a PROOF against
// truncation; it can only cover the heaviest response the surface has
// actually been observed to produce. That is what this guard checks, and it
// is why raising the number is a measurement, never a guess.

// capFixtureCard builds one card of the verbose response, padded to `target`
// serialised bytes.
//
// Padding goes into `description` because that is where a real card's weight
// sits (of the 9 978-byte maximum measured above, 5 911 bytes were the four
// prose fields). The bounded fields are set to their own maxima — the title
// at the 1 000-character limit ValidateTitle enforces — so the part of the
// card that CAN be bounded is measured at its ceiling and the rest is the
// padding.
func capFixtureCard(t *testing.T, id string, target int) *issues.IssueDetail {
	t.Helper()
	now := time.Date(2026, 8, 20, 9, 13, 43, 114566000, time.UTC)
	uid := uuid.MustParse("3cc3d111-a0cc-4532-946d-c16484b0e802")
	assignee := uid.String() // the longest handle the schema accepts verbatim
	parent := "sk-282"
	card := &issues.IssueDetail{
		ID:                 id,
		ProjectID:          uid,
		ParentID:           &parent,
		Version:            7,
		Title:              strings.Repeat("t", 1000),
		Type:               "task",
		Priority:           3,
		Status:             "in_progress",
		Source:             "agent_directed",
		Assignee:           &assignee,
		AssigneeUserID:     &uid,
		CreatedBy:          uid,
		CreatedAt:          now,
		UpdatedAt:          now,
		Metadata:           map[string]any{},
		ComponentIDs:       []uuid.UUID{uid},
		Labels:             []string{"batch-4"},
		CommitRefs:         []issues.CommitRef{},
		Blockers:           []issues.BlockerRef{},
		Mentions:           []issues.MentionRef{},
		Design:             "",
		AcceptanceCriteria: "",
		Notes:              "",
	}
	base, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal fixture card: %v", err)
	}
	pad := target - len(base)
	if pad < 0 {
		t.Fatalf("fixture card is already %d bytes, over the %d target", len(base), target)
	}
	// Plain ASCII: json.Marshal escapes nothing here, so one byte of prose
	// is one byte on the wire and the target is hit exactly.
	card.Description = strings.Repeat("d", pad)
	return card
}

// TestEpicCreate_DeclaredCapCoversTheHeaviestVerboseResponse is the guard.
func TestEpicCreate_DeclaredCapCoversTheHeaviestVerboseResponse(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterEpicCreate(s, nil, nil, nil)

	tool := lookupRegisteredTool(t, s, "skein_epic_create")
	if tool.Meta == nil {
		t.Fatalf("_meta missing on skein_epic_create")
	}
	raw, ok := tool.Meta.AdditionalFields["anthropic/maxResultSizeChars"]
	if !ok {
		t.Fatalf("_meta missing anthropic/maxResultSizeChars")
	}
	// mcp-go round-trips _meta through JSON, so an int const arrives as a
	// float64 — read it the way a client would.
	var declared int
	switch v := raw.(type) {
	case float64:
		declared = int(v)
	case int:
		declared = v
	default:
		t.Fatalf("anthropic/maxResultSizeChars = %v (%T), want a number", raw, raw)
	}
	// SC-011 / INV-5's other half: what the schema announces is what the
	// code decided, not a literal that drifted away from it.
	if declared != epicCreateMaxResultChars {
		t.Errorf("declared maxResultSizeChars = %d, epicCreateMaxResultChars = %d — "+
			"the announced ceiling and the reasoned one have diverged",
			declared, epicCreateMaxResultChars)
	}

	// The heaviest legal answer: verbose, a full batch of children, every
	// card at the reference size.
	epic := capFixtureCard(t, "sk-282", epicCreateReferenceCardChars)
	children := make([]any, 0, epicChildTemplateMaxItems)
	for i := 1; i <= epicChildTemplateMaxItems; i++ {
		children = append(children,
			capFixtureCard(t, fmt.Sprintf("sk-282.%d", i), epicCreateReferenceCardChars))
	}
	payload, err := json.Marshal(epicCreateResponse{Epic: epic, Children: children})
	if err != nil {
		t.Fatalf("marshal verbose response: %v", err)
	}
	measured := len(payload)
	t.Logf("verbose response: %d children + epic at %d bytes each = %d bytes "+
		"(declared ceiling %d)",
		epicChildTemplateMaxItems, epicCreateReferenceCardChars, measured, declared)
	if declared > MCPMaxDeclarableResultChars {
		t.Errorf("declared maxResultSizeChars = %d is above the client's hard ceiling "+
			"of %d — the excess is not honoured, so the declaration says less than it "+
			"appears to", declared, MCPMaxDeclarableResultChars)
	}
	switch {
	case measured > MCPMaxDeclarableResultChars:
		// Nothing bigger can be declared; the honest declaration is the
		// ceiling itself, and the shortfall is a platform limit.
		if declared < MCPMaxDeclarableResultChars {
			t.Errorf("the heaviest measured verbose response is %d bytes — above the "+
				"client's hard ceiling of %d — so the declaration must BE the ceiling; "+
				"it is %d, which sends %d bytes of legitimate response to the "+
				"persist-to-disk path (INV-5)",
				measured, MCPMaxDeclarableResultChars, declared,
				MCPMaxDeclarableResultChars-declared)
		}
		t.Logf("residual: %d bytes of the heaviest response sit above the client's "+
			"hard ceiling of %d and cannot be covered by any declaration",
			measured-MCPMaxDeclarableResultChars, MCPMaxDeclarableResultChars)
	case measured > declared:
		t.Errorf("the heaviest measured verbose response is %d bytes but the tool "+
			"advertises maxResultSizeChars=%d — the client would hand the agent a file "+
			"reference instead of the result (INV-5); raise epicCreateMaxResultChars",
			measured, declared)
	}

	// The DEFAULT answer, for contrast rather than as an assertion: the same
	// batch as acknowledgements. It is the number the cap must NOT be sized
	// on — a ceiling fitted to the acknowledgement truncates the opt-in full
	// response, which is the trap the registry names for skein_show.
	details := make([]*issues.IssueDetail, 0, epicChildTemplateMaxItems)
	for i := 1; i <= epicChildTemplateMaxItems; i++ {
		details = append(details,
			capFixtureCard(t, fmt.Sprintf("sk-282.%d", i), epicCreateReferenceCardChars))
	}
	batch, err := ackBatchFromIssues(details)
	if err != nil {
		t.Fatalf("ackBatchFromIssues: %v", err)
	}
	ackEpic, err := ackFromIssue(epic)
	if err != nil {
		t.Fatalf("ackFromIssue: %v", err)
	}
	ackRows := make([]any, 0, len(batch.Results))
	for _, row := range batch.Results {
		ackRows = append(ackRows, row)
	}
	ackPayloadBytes, err := json.Marshal(epicCreateResponse{Epic: ackEpic, Children: ackRows})
	if err != nil {
		t.Fatalf("marshal acknowledgement response: %v", err)
	}
	t.Logf("acknowledgement response for the same batch: %d bytes (%.1f%% of the "+
		"verbose one)", len(ackPayloadBytes),
		100*float64(len(ackPayloadBytes))/float64(measured))
}
