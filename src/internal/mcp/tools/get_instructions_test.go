package tools

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/mcp/instructions"
)

// TestGetInstructions_AlwaysLoadMetaSet verifies the registered tool
// carries `anthropic/alwaysLoad: true`. skein_get_instructions is step 3
// of the mandatory bootstrap the server `instructions` prescribe — if it
// ever falls back to deferred loading, every session pays a Tool Search
// round-trip before the agent can read the rules it must follow first.
func TestGetInstructions_AlwaysLoadMetaSet(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	// Register* only builds the tool + closes over the deps, so nil
	// stores are fine for a registration-surface assertion.
	RegisterGetInstructions(s, nil, nil)

	tool := lookupRegisteredTool(t, s, "skein_get_instructions")
	if tool.Meta == nil {
		t.Fatalf("_meta missing on skein_get_instructions")
	}
	v, ok := tool.Meta.AdditionalFields["anthropic/alwaysLoad"]
	if !ok {
		t.Fatalf("_meta missing anthropic/alwaysLoad")
	}
	if got, _ := v.(bool); !got {
		t.Fatalf("anthropic/alwaysLoad = %v, want true", v)
	}
}

// TestGetInstructions_SearchHintSet verifies the tool goes through the
// addTool funnel, i.e. that alwaysLoad and the hint coexist rather than
// one overwriting the other's _meta map.
func TestGetInstructions_SearchHintSet(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterGetInstructions(s, nil, nil)

	tool := lookupRegisteredTool(t, s, "skein_get_instructions")
	if tool.Meta == nil {
		t.Fatalf("_meta missing on skein_get_instructions")
	}
	hint, _ := tool.Meta.AdditionalFields[metaSearchHint].(string)
	if hint == "" {
		t.Fatalf("%s missing or empty", metaSearchHint)
	}
}

// fullGuideBudgetBytes is the size budget for the agent guide
// (instructions/full.md), in bytes. It is NOT the truncation limit —
// that is getInstructionsMaxResultChars, asserted separately by
// TestGetInstructions_PayloadFitsCap and set two and a half times higher.
// The two numbers guard different things and must not be conflated: the
// cap protects correctness (a truncated rulebook), this protects cost.
//
// Raising it is a decision, not a formality. The guide is not a document
// that is read once and dropped: every session is instructed to fetch it
// at startup, and the tool_result it arrives in stays in the transcript
// for the rest of that session — so it is re-sent to the model on every
// subsequent request of every session, by every agent. Growth here is
// paid continuously and by everyone, which is the cost sk-255 exists to
// hold down; the whole slice moved prose OUT of tool schemas and INTO
// this file, so the file is exactly where the pressure now lands.
//
// So: before bumping this, first try to make room. Something in full.md
// is usually restating an error message the agent will meet anyway, or
// re-explaining a rule the schemas already carry. Bump it only when the
// added text is genuinely load-bearing at read time, and say so in the
// commit.
//
// Current: 64426 bytes (sk-282 T027, corrected in a later fix round
// and again in the next one, where the epic-closing rule was given the
// counter that answers it), up from 57943 before the run. Raised from 60000 to 66000 deliberately,
// with the additions named:
//
//   - the thread obligation rewritten (SC-017). The old wording told the
//     agent to re-fetch the whole thread; that route no longer exists, and
//     a rule pointing at a removed route is worse than a stale one — it is
//     followed and fails. The replacement has to carry the new trigger, the
//     new route and the index cap, so it is longer than what it replaced;
//   - the write-tool contract rewritten. Eleven tools stopped returning the
//     issue card; the paragraph that described the old lean card would have
//     taught agents to look for prose and for a `bodyOmitted` marker that
//     issue writes no longer emit;
//   - one new section, «Default answers, and the cheap route to more» —
//     the per-tool default and its named escape in a table, plus the two
//     navigation routes. US-10's acceptance is that an agent reading only
//     this can walk them, which is not satisfiable by prose scattered
//     across the tool entries.
//
// That size is what the guard below PRINTS on every run, so the number in
// this comment can be checked against reality instead of trusted: an
// earlier revision of this paragraph said 63328 while the file weighed
// 63335, which is a small error about the one quantity this constant
// exists to hold down.
//
// The margin is now ~2%, deliberately TIGHTER than the ~10%
// this constant used to carry. That run found retired-contract text in the
// guide four times; the file is where such text hides, and a budget that
// leaves room for a section is a budget that lets the next one in without
// an argument. Room for a paragraph is the intended amount.
//
// ⚠ RAISED AGAIN, 66000 → 68800 (sk-303.14 / T022). The number above —
// «Current: 64426» — describes the file BEFORE that wave and is left in
// place as history, not as a measurement of today's file; the guard below
// prints the real one on every run.
//
// What was added, named as this comment requires: ONE section, «Review
// flag — the author's private bookkeeping» (~1.4 KB). Wave sk-303.14 gave
// every issue a review-required flag and closed issues an author's review
// mark, and the guide said nothing about either: `reviewRequired` existed
// only in one tool's schema, so it was visible only to an agent who had
// already decided to send it. Two failures follow from that silence and
// both are read-time, which is the bar this constant sets for a raise:
//
//   - the built-in default of the personal default is «review required», so
//     a user asking «don't nudge me about this one» needs an EXPLICIT
//     `reviewRequired: false`. An agent that does not know this writes the
//     request into the description, where it does nothing;
//   - the flag does not travel the ordinary update path: a call carrying
//     it together with any other field is rejected WHOLE and writes
//     nothing. An agent that does not know this reads one rejection and
//     assumes the rest of its edit landed.
//
// Nothing was cut to make room — the attempt was made and every candidate
// was a live contract, not a restatement. The section was compressed
// instead: its first draft was 2.1 KB.
//
// The margin is again ~2% (1392 bytes at 67408), the same discipline as
// the previous raise, not a looser one.
//
// ⚠ RAISED AGAIN, 68800 → 71000 (sk-421.1 / T004). The guide was 68632
// bytes before the wave — 168 bytes of margin — and the wave changed two
// things agents read on the issue card, both of which the contract table
// requires the guide to describe in the same change. Named additions
// (~975 bytes after compression; the first draft was ~2.1 KB):
//
//   - a paragraph «Blockers you cannot see» next to the other
//     unavailability rules: a blocker outside the reader's horizon is now a
//     redacted entry with its `id` and NO `title`/`status` keys. An agent
//     that does not know this branches on a missing status as if it were a
//     value, or reads the entry as broken;
//   - one Identity-model bullet on `lockedByUsername`: who PERFORMED the
//     claim, absent without a lock or claim record, not carried by write
//     acknowledgements. Without it an agent looks for the name on the
//     acknowledgement, or takes the assignee for the lock holder;
//   - the post-claim example's `blockers:` line shows the redacted form.
//
// Cutting was tried first: the neighbouring sections are live contracts
// pinned by their own guide tests, not restatements. The margin is 1393
// bytes at 69607 — ~2%, the same discipline as the raises above.
//
// ⚠ RAISED AGAIN, 71000 → 73400 (sk-489 / T019). The guide was 70063
// bytes before the change — 937 bytes of margin — and US-9 makes the
// guide, not only the tool schemas, responsible for naming the close
// outcome: "The guide and the tool descriptions name the outcome, the values,
// the default on transition, the target rule, the outcome edit and the effect on
// progress". Named additions (~1830 bytes NET, after the compression
// below):
//
//   - a «Close outcome — how the issue ENDED» section: the four values,
//     the default on a transition (and the archive exception), that only
//     `done` counts toward progress, the `closeTarget` rule with
//     `closeLink`, editing a closed issue's outcome, which tools take
//     the field, and one sentence on the rollout window — an older
//     server OMITS the key rather than answering `done`, so absence must
//     not be read as an outcome (US-10);
//   - the epic-close bullet now reads the FIELD instead of guessing from
//     the reason text, and adds the one check the machine value makes
//     possible: a child closed as `duplicate` / `superseded` should
//     point at an issue that is itself closed, or the work only moved;
//   - the ownerless-close exception names the outcome to give such an
//     issue, and says the «closed with no assignee» warning is not
//     raised when the outcome is not `done`.
//
// Cutting was tried first and DID recover ~470 bytes, both of them
// restatement rather than contract: the ✗ BAD code block under «Do NOT
// write dependency refs in free-text» re-stated in code the sentence
// directly above it, and one sentence of Claim discipline («the tracker
// shows a ghost task no one owns») repeated the clause before it. The
// rest of the neighbourhood is live contract pinned by its own guide
// tests. The margin was 1504 bytes at 71896 — ~2%, the same discipline
// as every raise above, not a looser one.
//
// Spent since, WITHOUT a raise (sk-489, review follow-up A/5): 219 bytes
// on two corrections the section could not do without — the echo sentence
// now excludes `skein_update_many`, whose row genuinely carries no outcome,
// and the archive default names its standing-issue exception. The margin
// is 1285 bytes at 72115 — ~1.75%.
// The next addition that does not fit ASKS FOR A RAISE with its own
// justification; it does not quietly eat the remainder.
//
// Spent since, WITHOUT a raise (sk-394): 1144 bytes on the arrival of
// skein_commits_detach — its lifecycle-table row and the paragraph under
// «Commit refs» that says a wrong hash is now removable. Not a raise
// because cutting came first and worked: the paragraph was drafted at
// 1273 bytes and went in at 755, the half that was restatement of the
// tool description dropped. What stayed is the part the description
// cannot carry — WHY detach matches on the hash alone (attach's repo
// resolution can itself fail, and that failure is one of the ways a
// wrong row gets written), and that `notFound` is the only thing
// telling a typo from a real removal. The margin is 141 bytes at 73259
// — ~0.2%, so the NEXT addition is the one that asks for the raise, and
// it inherits a sharper argument for it: this section is now the
// recovery procedure for a data defect, not a reference table.
//
// ⚠ RAISED AGAIN, 73400 → 76000 (main merged into dev, sk-538.3). In main
// the budget grew separately — 68800 → 70750, for the sk-499 paragraph on a batch call
// that returned no answer: the rows already applied stay applied, and
// an agent that thinks otherwise either loses the write or applies it twice.
// That paragraph is a live contract (codes batch_deadline_exceeded /
// request_cancelled and the retry order), and in main it has already been
// compressed twofold, ~1100 → ~600 bytes. Here it landed on top of the 141-byte
// headroom left after sk-394 — exactly the case the paragraph above
// warns about: the next addition asks for a raise. The sk-303.55 cleanup
// from the same branch returned some bytes (references to internal issues in
// the lifecycle table). The headroom is again ~2% (1471 bytes at 74529).
//
// ⚠ LOWERED, 76000 → 75500 (sk-540.6). sk-540.1/.2/.4 grew the «Close
// outcome» section to 75816 bytes, 184 bytes under the budget. sk-540.6
// rewrote that section with every rule kept (required on a transition,
// refusal codes and where they fall, the REST/archive defaults, progress,
// the target rule and its exceptions, outcome edits, the echo exception,
// the rollout window) and dropped only restatement, bringing the guide to
// 74935. The budget follows it down so the recovered bytes are not quietly
// re-spent: margin 565 bytes, ~0.75%.
//
// ⚠ RAISED, 75500 → 77000 (sk-565, T018 — load-test text US-10). The
// MCP create paths now assign the caller by default, and US-10 makes the
// guide say so: the «Assignee on create» section (the default and its
// personal setting, the someone-else's-parent rule, `unassigned: true`,
// removal by clearing, REST never doing it), the Claim discipline
// paragraph and the canonical SKEIN-USAGE block no longer claiming that
// an unclaimed issue has no assignee as a general rule, the
// auto-subscription exception and the move kit's `unassigned`. Measured
// on the file, not estimated: T018 took full.md from 75295 to 76491 bytes
// (+1196); cutting came first — the first draft measured 76664, and
// compressing the new section took 173 bytes back. The 205-byte margin
// left by sk-540.6 could not hold that. Guide 76491 bytes, margin 509,
// ~0.7%.
//
// Spent since, WITHOUT a raise (found in review of sk-565): 448
// bytes on corrections the reviews named — clearing the assignee also
// drops the acceptance, `unassigned` is per epic and per child, the
// pre-close check is the claim (lock / `in_progress`) rather than the
// assignee, «ancestor» rather than «parent», the subscription exception
// holds at create only, and the canonical SKEIN-USAGE block names the
// someone-else's-ancestor rule (SC-007). Guide 76939 bytes, margin 61,
// ~0.08% — the next addition asks for a raise with its own reason.
//
// Spent since, WITHOUT a raise (sk-607): 8 bytes. The two batch stop codes
// now also mark a close_many row whose transaction the deadline cut before
// it committed, so «never ran» became «did not apply» in both places.
// Guide 76947 bytes, margin 53. Review of sk-607: «did not apply»
// → «reported no commit» (a context cut during COMMIT leaves the outcome
// unknown). Guide 76952 bytes, margin 48. Next: the acknowledgement
// paragraphs name the one marker that exists, `cardUnavailable`, and were
// compressed to pay for it. Guide 76948 bytes, margin 52.
//
// Spent since, WITHOUT a raise (sk-647.2 review): the guide had shrunk to
// 76414 by then. The batch-stop paragraph no longer says every stopped row
// «reported no commit, resend with the same expectedVersion»: a
// skein_update_many row cut inside its chain may have committed earlier
// steps, so the recovery is split per tool (never started / dep edge /
// update chain / close). Guide 76920 bytes, margin 80.
const fullGuideBudgetBytes = 77000

// TestGetInstructions_PayloadFitsCap is the TRUNCATION guard: whatever
// the tool can return must fit the anthropic/maxResultSizeChars it
// advertises.
//
// Overrunning the cap does not fail loudly — the client truncates the
// result, so the agent reads the first N KB of the rulebook and acts as
// though the rest was never written. That is exactly the failure this
// test exists to prevent: the cap was set at 50000 when the guide was
// ~6 KB, and sk-255.2's additions took the guide from 49.5 KB to 54 KB,
// silently past it.
//
// The measured payload is the guide PLUS the worst-case
// "Project: <name>\n\n" header handleGetInstructions prepends when
// `project` is supplied — the guide alone is not what a client receives.
// Sizes are byte counts, which is the conservative reading of the cap's
// unit; see getInstructionsMaxResultChars for why.
//
// No headroom is asserted beyond the payload itself. Slack under this
// cap is not a budget anyone should be spending against — it is a
// client-side ceiling, unused margin costs nothing, and the number is
// raiseable on sight. The invariant that guide growth has to answer to
// is fullGuideBudgetBytes, below.
func TestGetInstructions_PayloadFitsCap(t *testing.T) {
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterGetInstructions(s, nil, nil)

	tool := lookupRegisteredTool(t, s, "skein_get_instructions")
	if tool.Meta == nil {
		t.Fatalf("_meta missing on skein_get_instructions")
	}
	// mcp-go round-trips _meta through JSON, so the int const arrives as
	// a float64 — read it the way a client would rather than asserting
	// the Go type.
	raw, ok := tool.Meta.AdditionalFields["anthropic/maxResultSizeChars"]
	if !ok {
		t.Fatalf("_meta missing anthropic/maxResultSizeChars")
	}
	var advertisedCap int
	switch v := raw.(type) {
	case float64:
		advertisedCap = int(v)
	case int:
		advertisedCap = v
	default:
		t.Fatalf("anthropic/maxResultSizeChars = %v (%T), want a number", raw, raw)
	}

	guideBytes := len(instructions.FullGuide)
	payloadBytes := guideBytes + getInstructionsPrefixMaxBytes
	if payloadBytes > advertisedCap {
		t.Fatalf("worst-case skein_get_instructions payload is %d bytes "+
			"(full.md %d + project header %d) but the tool advertises "+
			"maxResultSizeChars=%d — the client would truncate the guide; "+
			"raise getInstructionsMaxResultChars",
			payloadBytes, guideBytes, getInstructionsPrefixMaxBytes, advertisedCap)
	}
}

// TestGetInstructions_GuideWithinBudget is the COST guard: the agent
// guide must stay inside the size budget it is allowed to occupy.
//
// Separate from the truncation guard on purpose. Merging them is what
// the old single assertion did, and it left this invariant unenforced:
// with a 150000 cap and a 10000 slack floor, the guide could grow from
// 54 KB to ~140 KB — nearly triple — without a single test going red.
// A budget expressed as "distance below an unrelated, freely raiseable
// ceiling" is not a budget.
func TestGetInstructions_GuideWithinBudget(t *testing.T) {
	guideBytes := len(instructions.FullGuide)
	// Printed, not only compared: this is the fixed per-session cost the
	// guide adds to every agent that fetches it, and it is the number the
	// comment on fullGuideBudgetBytes quotes. A quoted number nobody can
	// see drifts from the file it describes.
	t.Logf("MEASURE_GUIDE full.md bytes=%d budget=%d margin=%d",
		guideBytes, fullGuideBudgetBytes, fullGuideBudgetBytes-guideBytes)
	if guideBytes > fullGuideBudgetBytes {
		t.Errorf("full.md is %d bytes, over the %d-byte budget by %d — "+
			"the guide is re-sent on every request of every session that "+
			"fetched it, so cut something before raising "+
			"fullGuideBudgetBytes (and justify the raise in the commit)",
			guideBytes, fullGuideBudgetBytes, guideBytes-fullGuideBudgetBytes)
	}
}

// TestGetInstructions_BudgetIsNotSlackDressedAsABudget is matrix row 101:
// "the guide's byte budget was raised deliberately — the new limit is a
// named number; automatic raising is impossible."
//
// Intent cannot be asserted. What CAN be asserted is that the number stays
// a budget rather than becoming permission: a ceiling raised far above the
// file it bounds constrains nothing, and the cheapest way to make the
// guard above green is to type a bigger number. So the slack itself is
// bounded — a raise has to be justified by growth that already happened,
// which is what makes bumping it in advance impossible rather than merely
// discouraged.
//
// The ratio, not a constant: 10% of the guide is «room for a paragraph or
// two» at any size the guide reaches, whereas a fixed slack becomes
// generous as the file shrinks and cramped as it grows.
func TestGetInstructions_BudgetIsNotSlackDressedAsABudget(t *testing.T) {
	guideBytes := len(instructions.FullGuide)
	if guideBytes == 0 {
		t.Fatal("the guide is empty — every budget assertion would be vacuous")
	}
	slack := fullGuideBudgetBytes - guideBytes
	if slack < 0 {
		return // the over-budget case is TestGetInstructions_GuideWithinBudget's
	}
	maxSlack := guideBytes / 10
	if slack > maxSlack {
		t.Errorf("fullGuideBudgetBytes is %d and the guide is %d — %d bytes of slack, "+
			"more than the %d this allows (10%% of the file). "+
			"A budget that far above the thing it bounds is not a budget, it is "+
			"permission. Raise it WITH the text that needs the room, in the same "+
			"commit, and say what the text is.",
			fullGuideBudgetBytes, guideBytes, slack, maxSlack)
	}
	t.Logf("guide %d bytes, budget %d, slack %d (ceiling %d)",
		guideBytes, fullGuideBudgetBytes, slack, maxSlack)
}

// lookupRegisteredTool drives an in-process MCP client through
// initialize + tools/list and returns the named tool as the wire
// actually carries it — the only way to see the _meta a client would
// receive, rather than the struct the Register* call happened to build.
func lookupRegisteredTool(t *testing.T, s *server.MCPServer, name string) *mcpgo.Tool {
	t.Helper()

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for i := range listRes.Tools {
		if listRes.Tools[i].Name == name {
			return &listRes.Tools[i]
		}
	}
	t.Fatalf("%s not registered", name)
	return nil
}
