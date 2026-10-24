package issues_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sk-118.11 (F3) — the cross-cutting standing-invariant coverage map.
//
// This file is the LIVING 1:1 INDEX with the 16 standing invariants of the
// integration-test architecture (the authoritative list).
// Each of the 16
// invariants maps to the named Go test(s) that actually cover it. Most of those
// tests live in their F2* implementation slice (close to the code they guard);
// F3 adds the two genuine cross-cutting gaps (rows 3-leg-A and 3-leg-B):
//
//   - TestInbox_StandingExcludedFromUnaccepted (internal/handler) — §6.3 inbox
//   - TestListReady_StandingIssueNotListed     (internal/dependencies) — §6.3 ready
//
// TestStandingInvariants_AllSixteenCovered is NOT a doc comment that drifts: it
// resolves the src/ root, scans EVERY *_test.go in the tree for the
// `func <TestName>(` declarations, and FAILS if any invariant row maps to a
// test that does not exist (renamed, deleted, or never written). That makes the
// table a real guard — you cannot remove a covering test without this index
// going red, and you cannot add a §6 invariant row here without a real test
// behind it. F-AUDIT remains the final human-reviewed backstop.

// invariant is one row of the §6 → test mapping.
type invariant struct {
	num   int      // §6 invariant number (1..16)
	desc  string   // short description (must match §6 intent)
	tests []string // covering Go test func names; ALL must exist
}

// standingInvariants is the canonical 16-row map. Order = §6 order. Each `tests`
// entry is a representative covering test (or set) for that invariant; the
// assertion below requires every listed name to exist in the source tree. Where
// an invariant has many sibling tests, the most load-bearing one(s) are listed —
// enough to prove the invariant is genuinely exercised, not merely declared.
var standingInvariants = []invariant{
	{1, "Counts exclude standing (project + component pills) + standing aggregate", []string{
		"TestProjectStats_StandingExcludedFromOpenInProgressBlockedDeferred",
		"TestProjectStats_StandingCountReported",
		"TestComponentStats_StandingExcludedAndBucketed",
	}},
	{2, "Cross-project lenses (On me / Delegated / outbox / cross-project default) exclude standing", []string{
		"TestOnMeAndDelegated_ExcludeStanding",
		"TestCrossProjectDefault_ExcludesStanding",
	}},
	{3, "Ready excludes standing issues + inbox-needs-acceptance excludes standing", []string{
		"TestListReady_StandingIssueNotListed",     // F3 NEW — ready leg
		"TestInbox_StandingExcludedFromUnaccepted", // F3 NEW — inbox leg
		"TestListReady_NotBlockedByStandingTarget", // standing-blocker non-gating (cross-ref §6.4)
	}},
	{4, "standing-as-blocker rejected / non-gating", []string{
		"TestDepAdd_BlocksTargetingStanding_Rejected",
		"TestDepAdd_WaitsForTargetingStanding_Rejected",
		"TestIsBlocked_StandingTargetNotBlocking",
	}},
	{5, "Epic with standing child: rollup + eligible + summary-total exclude standing", []string{
		"TestEpicRollup_StandingChildExcludedFromTotal",
		"TestEpicRollup_EpicWithOnlyStandingChild_NotEligibleButCloseable",
		"TestEligibleEpics_StandingOnlyEpic_NotCounted",
		"TestEpicProgress_StandingChildExcludedFromTotal",
		// Load-bearing POSITIVE legs (found in review: under-indexed): a
		// standing child must NOT strand an otherwise-closeable epic, and the
		// epic must still be counted eligible. Indexing these guards them from
		// silent deletion.
		"TestEpicRollup_EpicWithStandingPlusAllClosed_Eligible",
		"TestEpicRollup_EpicOpenChildren_OmitsStanding",
		"TestEligibleEpics_ClosedPlusStandingChild_Counted",
	}},
	{6, "Claim rejected (MCP + REST)", []string{
		"TestClaim_StandingRejected",
		"TestClaim_StandingReturnsCannotClaimStanding",
		"TestClaim_StandingRejected_REST",
	}},
	{7, "Retire-block on close with any non-closed child (both child reps)", []string{
		"TestClose_StandingWithOpenChild_Blocked",
		"TestClose_StandingChild_ViaParentId_AndViaEdge_BothBlock",
		"TestClose_StandingAllChildrenClosed_Succeeds",
		"TestClose_NonStandingParentNotBlocked",
	}},
	{8, "Nested-standing forbidden (create / reparent / promote, both reps)", []string{
		"TestDepAdd_StandingChildOfStanding_Rejected",
		"TestReparent_StandingUnderStanding_Rejected",
		"TestCreate_StandingUnderStandingParent_Rejected",
		"TestPromote_ToStanding_WithStandingParent_Rejected_ParentIDColumn",
		"TestPromote_ToStanding_WithStandingParent_Rejected_DepEdge",
		"TestPromote_ToStanding_WithStandingChild_Rejected_ParentIDColumn",
		"TestPromote_ToStanding_WithStandingChild_Rejected_DepEdge",
	}},
	{9, "Transition matrix (allowed + rejected edges)", []string{
		"TestTransition_OpenToStanding_Allowed",
		"TestTransition_DeferredToStanding_Allowed",
		"TestTransition_StandingToOpen_Allowed",
		"TestTransition_InProgressLockedToStanding_Rejected",
		"TestTransition_StandingToInProgress_Rejected",
		"TestTransition_StandingToDeferred_Rejected",
		"TestUnclaim_NextStatusStanding_Rejected",
	}},
	{10, "Create-as-standing: status=standing, no started_at / lock / accepted_at / ownerless-warning", []string{
		"TestCreate_Standing_StatusStandingNoStampsNoLock",
		"TestCreate_StandingFlag_NoOwnerlessWarning",
	}},
	{11, "Predicate centralization (byte-stable helpers)", []string{
		"TestPredicates_ByteStable",
	}},
	{12, "Enum-acceptance: update / list / search accept status=standing", []string{
		"TestUpdate_StatusStandingAcceptedByEnum",
		"TestList_StatusesStandingFilter",
		"TestSearch_StatusesStanding",
	}},
	{13, "Down-migration data-guard (000029 up→down round-trip with a standing row)", []string{
		"TestMigration000029_UpDownRoundTrip",
	}},
	{14, "closed→standing reopen clears closed_at / closed_reason", []string{
		"TestTransition_ClosedToStanding_ClearsClosedAt",
	}},
	{15, "Deferred child blocks retire (not only open/in_progress)", []string{
		"TestClose_StandingWithDeferredChild_Blocked",
	}},
	{16, "Demote round-trip: standing→open is claimable again", []string{
		"TestTransition_StandingToOpen_Allowed",
		"TestDemote_StandingToOpen_Claimable",
	}},
}

// funcDeclRe matches a top-level Go test-function declaration.
var funcDeclRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

// collectTestFuncs walks the src/ tree (this test's ../.. ) and returns the set
// of all declared `func Test*` names across every *_test.go file, mapped to the
// repo-relative path where each is declared (first occurrence wins — names are
// unique per package by Go's own rules, and globally unique here in practice).
func collectTestFuncs(t *testing.T) map[string]string {
	t.Helper()
	// cwd when this package's tests run is internal/issues; src/ root is ../..
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving src/ root: %v", err)
	}
	found := map[string]string{}
	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			// Skip vendored / generated trees if any appear later.
			if info.Name() == "vendor" || info.Name() == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		for _, m := range funcDeclRe.FindAllStringSubmatch(string(data), -1) {
			name := m[1]
			if _, ok := found[name]; !ok {
				found[name] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking src/: %v", err)
	}
	return found
}

// TestStandingInvariants_AllSixteenCovered asserts the 16-invariant → test map
// is complete and that every mapped test actually exists in the source tree.
// It fails — loudly, per-invariant — if:
//
//   - the table does not contain exactly the 16 invariants numbered 1..16, or
//   - any invariant row lists no covering test, or
//   - any listed covering test does not exist as a `func Test...` in the tree.
//
// This is the cross-cutting net F3 owns: a 1:1 living index with §6 that breaks
// the build the instant standing coverage regresses.
func TestStandingInvariants_AllSixteenCovered(t *testing.T) {
	declared := collectTestFuncs(t)

	// 1. Exactly 16 invariants, numbered 1..16, no gaps or dupes.
	if len(standingInvariants) != 16 {
		t.Fatalf("standingInvariants has %d rows, want exactly 16 (1:1 with §6)", len(standingInvariants))
	}
	seenNum := map[int]bool{}
	for _, inv := range standingInvariants {
		if inv.num < 1 || inv.num > 16 {
			t.Errorf("invariant number %d out of range 1..16 (%q)", inv.num, inv.desc)
		}
		if seenNum[inv.num] {
			t.Errorf("invariant number %d duplicated", inv.num)
		}
		seenNum[inv.num] = true
		if strings.TrimSpace(inv.desc) == "" {
			t.Errorf("invariant %d has empty description", inv.num)
		}
		if len(inv.tests) == 0 {
			t.Errorf("invariant %d (%q) has NO mapped covering test", inv.num, inv.desc)
		}
	}
	for n := 1; n <= 16; n++ {
		if !seenNum[n] {
			t.Errorf("invariant number %d missing from the table", n)
		}
	}

	// 2. Every mapped covering test must exist in the source tree.
	var missing []string
	for _, inv := range standingInvariants {
		for _, name := range inv.tests {
			if _, ok := declared[name]; !ok {
				missing = append(missing,
					"invariant "+itoa16(inv.num)+" ("+inv.desc+"): mapped test "+name+" NOT FOUND in any *_test.go")
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("standing invariant coverage incomplete — %d mapped test(s) missing:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}

	// 3. Emit the resolved map for human eyeballing in -v runs (provenance:
	//    invariant → test → file).
	for _, inv := range standingInvariants {
		for _, name := range inv.tests {
			t.Logf("§6.%-2d %-12s %s  (%s)", inv.num, name, "✓", declared[name])
		}
	}
}

func itoa16(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}
