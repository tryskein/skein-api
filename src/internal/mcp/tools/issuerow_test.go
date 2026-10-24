package tools

// issuerow_test.go — sk-282 T005 (rows 3–7) and T006 (row 8).
//
//	row 3  US-2: the default named key set is EXACTLY the spec's list,
//	            not one key more
//	row 4  SC-002: the row's overhead on the reference issue is ≤300
//	            characters (the budget shifts by the title's and, in
//	            cross-project mode, the project identifier's length)
//	row 5  US-2: an explicit `fields` yields only the selected fields plus
//	            the identifier; an empty set yields the identifier alone
//	row 6  edge: an unknown name in `fields` is REJECTED, and the error
//	            names it
//	row 7  INV-1 "not provided": a per-carrier key set disappears
//	            entirely, while a genuinely-loaded empty set stays `[]`
//
//	row 8  D-9 / the US-2 compensation: the drift-guard over both
//	            carriers' full keysets — see the T006 section at the
//	            bottom of this file
//
// The expectations are transcribed from US-2's normative list, not read
// back from issuerow.go: wantDefaultRowKeys below is the spec's
// enumeration spelled out again in test-local literals, so a change to the
// production list cannot quietly bless itself.
//
// Pure unit: no pool, no database.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// wantDefaultRowKeys is US-2's "Included" list, transcribed. Written as
// literals rather than as the IssueRowField* constants on purpose: this
// test's job is to hold the production list to the SPEC, and referring to
// the same constants the code uses would make a renamed key pass.
var wantDefaultRowKeys = []string{
	"id",              // identifier
	"title",           // title
	"status",          // status
	"type",            // type
	"priority",        // priority
	"assignee",        // assignee
	"labels",          // labels
	"isBlocked",       // blocked flag
	"parentIdCurrent", // parent identifier
	"version",         // version
	"updatedAt",       // last-modified stamp
	"childrenCount",   // child counters (US-3)
	"unclosedChildrenCount",
}

// wantExcludedRowKeys is US-2's "Not included" list, transcribed. Asserting
// it separately from "the set equals the included list" is not redundant:
// the two lists are what the spec actually says, and a future field that
// lands in neither is caught by T006 rather than by an equality that
// silently absorbed it.
var wantExcludedRowKeys = []string{
	"projectId",         // project identifier in project mode
	"componentIds",      // component identifiers
	"createdBy",         // creator identifier
	"createdByUsername", // creator name
	"assigneeUserId",    // assignee user identifier
	"createdAt",         // created stamp
	"acceptedAt",        // accepted stamp
	"iAssigned",         // derived "assigned to me" flag
	"epicProgress",      // epic progress
}

// referenceIssueTitleRunes is the reference issue's title length. SC-002
// excludes the title from the budget and shifts the threshold by exactly
// the difference when a title is longer, so the number has to be named to
// compute the shift.
const referenceIssueTitleRunes = 60

// sc002BudgetChars is SC-002's threshold for the row's overhead.
const sc002BudgetChars = 300

// referenceIssueSummary builds US-2's reference issue: "60-character
// title, one assignee, two labels, one component, has a parent,
// no blocks."
func referenceIssueSummary(t *testing.T) issues.IssueSummary {
	t.Helper()
	assignee := "measure_user"
	parent := "sk-282.18"
	title := strings.Repeat("x", referenceIssueTitleRunes)
	if len([]rune(title)) != referenceIssueTitleRunes {
		t.Fatalf("fixture title is %d runes, want %d", len([]rune(title)), referenceIssueTitleRunes)
	}
	accepted := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	return issues.IssueSummary{
		ID:                "sk-282.18.7",
		ProjectID:         uuid.MustParse("01900000-0000-7000-8000-0000000000a1"),
		ParentID:          &parent,
		Version:           3,
		Title:             title,
		Type:              "task",
		Priority:          1,
		Status:            "open",
		Pinned:            false,
		Assignee:          &assignee,
		AssigneeUserID:    &[]uuid.UUID{uuid.MustParse("3cc3d111-a0cc-4532-946d-c16484b0e802")}[0],
		CreatedBy:         uuid.MustParse("3cc3d111-a0cc-4532-946d-c16484b0e802"),
		CreatedByUsername: &assignee,
		ComponentIDs:      []uuid.UUID{uuid.MustParse("01900000-0000-7000-8000-0000000000c1")},
		Labels:            []string{"backend", "p1"},
		AcceptedAt:        &accepted,
		UpdatedAt:         time.Date(2026, 8, 20, 2, 37, 37, 123456000, time.UTC),
		CreatedAt:         time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC),
		IsBlocked:         false,
	}
}

// defaultRowOptions are the options a project-mode consumer passes for the
// reference issue: the default set, both counters supplied.
func defaultRowOptions() IssueRowOptions {
	return IssueRowOptions{ChildCounts: &IssueRowChildCounts{Children: 0, Unclosed: 0}}
}

// row 3 — the default set is exactly the spec's list.
func TestProjectIssueRow_DefaultKeySetIsExactlyTheSpecList(t *testing.T) {
	row, err := ProjectIssueRow(referenceIssueSummary(t), defaultRowOptions())
	if err != nil {
		t.Fatalf("ProjectIssueRow: %v", err)
	}

	for _, key := range wantDefaultRowKeys {
		if _, ok := row[key]; !ok {
			t.Errorf("default row is missing %q — US-2 names it in the default set", key)
		}
	}
	for _, key := range wantExcludedRowKeys {
		if _, ok := row[key]; ok {
			t.Errorf("default row carries %q — US-2 names it in \"Not included\"; it is part of the "+
				"measured 45%% the change removes", key)
		}
	}
	if len(row) != len(wantDefaultRowKeys) {
		t.Errorf("default row has %d keys, want exactly %d — no extra keys. Got: %v",
			len(row), len(wantDefaultRowKeys), sortedRowKeys(row))
	}

	// sk-489, matrix row 3: the default set does NOT contain the close outcome,
	// and this is checked on a row that HAS an outcome.
	//
	// The fixture above is open, its outcome is empty, and on it the absence of the key would be
	// a tautology: it would not arrive because of omitempty either. So here is its own
	// closed row — then "no key" means the projection's decision, not
	// an empty value.
	closed := referenceIssueSummary(t)
	closed.Status = "closed"
	closed.CloseOutcome = &[]string{"wont_do"}[0]

	closedRow, err := ProjectIssueRow(closed, defaultRowOptions())
	if err != nil {
		t.Fatalf("ProjectIssueRow (closed row): %v", err)
	}
	if got, ok := closedRow["closeOutcome"]; ok {
		t.Errorf("the default row carries closeOutcome = %#v — the outcome is classified "+
			"\"selectable, not default\" (rowFieldsIntentionallyOut): it comes only "+
			"with an explicit `fields`, otherwise every result page pays for it", got)
	}
	if len(closedRow) != len(wantDefaultRowKeys) {
		t.Errorf("the default row of a closed issue carries %d keys instead of %d: %v",
			len(closedRow), len(wantDefaultRowKeys), sortedRowKeys(closedRow))
	}
}

// row 3, second half — the counters are emitted at zero. US-3 requires
// them present even when there is nothing to count, otherwise "none" and
// "not counted" become the same absence.
func TestProjectIssueRow_ZeroChildCountsAreStillEmitted(t *testing.T) {
	row, err := ProjectIssueRow(referenceIssueSummary(t), defaultRowOptions())
	if err != nil {
		t.Fatalf("ProjectIssueRow: %v", err)
	}
	for _, key := range []string{"childrenCount", "unclosedChildrenCount"} {
		v, ok := row[key]
		if !ok {
			t.Fatalf("%s absent on a row with zero children — a zero must read as "+
				"'counted, and it is none', not as 'not counted'", key)
		}
		if n, isInt := v.(int); !isInt || n != 0 {
			t.Errorf("%s = %#v, want int 0", key, v)
		}
	}
}

// row 3, third half — the cross-project mode adds the project identifier
// and nothing else.
func TestProjectIssueRow_CrossProjectAddsOnlyTheProjectIdentifier(t *testing.T) {
	opts := defaultRowOptions()
	opts.CrossProject = true
	row, err := ProjectIssueRow(referenceIssueSummary(t), opts)
	if err != nil {
		t.Fatalf("ProjectIssueRow: %v", err)
	}
	if _, ok := row["projectId"]; !ok {
		t.Error("cross-project row has no projectId — US-2: without it the listing is unreadable")
	}
	if len(row) != len(wantDefaultRowKeys)+1 {
		t.Errorf("cross-project row has %d keys, want %d (default set + projectId). Got: %v",
			len(row), len(wantDefaultRowKeys)+1, sortedRowKeys(row))
	}
}

// row 4 — SC-002. The budget is on the row's OVERHEAD: the title is user
// text of up to 1000 characters and is not truncatable, so a threshold
// that included it could neither be met nor honestly failed. Same rule for
// the project identifier in cross-project mode.
func TestProjectIssueRow_ReferenceRowFitsSC002(t *testing.T) {
	ref := referenceIssueSummary(t)

	t.Run("project mode", func(t *testing.T) {
		row, err := ProjectIssueRow(ref, defaultRowOptions())
		if err != nil {
			t.Fatalf("ProjectIssueRow: %v", err)
		}
		encoded := marshalRow(t, row)
		overhead := len([]rune(encoded)) - referenceIssueTitleRunes
		t.Logf("SC-002 project-mode row: %d chars total, %d overhead (budget %d)",
			len([]rune(encoded)), overhead, sc002BudgetChars)
		if overhead > sc002BudgetChars {
			t.Errorf("row overhead is %d characters, SC-002 allows %d:\n%s",
				overhead, sc002BudgetChars, encoded)
		}
	})

	t.Run("cross-project mode shifts by the project identifier", func(t *testing.T) {
		opts := defaultRowOptions()
		opts.CrossProject = true
		row, err := ProjectIssueRow(ref, opts)
		if err != nil {
			t.Fatalf("ProjectIssueRow: %v", err)
		}
		encoded := marshalRow(t, row)
		// The shift is the serialised project identifier itself, quotes
		// included — the same rule SC-002 applies to the title.
		projectIDChars := len([]rune(ref.ProjectID.String())) + 2
		overhead := len([]rune(encoded)) - referenceIssueTitleRunes - projectIDChars
		t.Logf("SC-002 cross-project row: %d chars total, %d overhead (budget %d)",
			len([]rune(encoded)), overhead, sc002BudgetChars)
		if overhead > sc002BudgetChars {
			t.Errorf("cross-project row overhead is %d characters, SC-002 allows %d:\n%s",
				overhead, sc002BudgetChars, encoded)
		}
	})
}

// row 5 — an explicit selection returns only what was asked for, plus the
// identifier; the empty selection returns the identifier alone.
func TestProjectIssueRow_ExplicitFieldSelection(t *testing.T) {
	ref := referenceIssueSummary(t)

	t.Run("only the selected fields plus the identifier", func(t *testing.T) {
		row, err := ProjectIssueRow(ref, IssueRowOptions{Fields: []string{"status", "priority"}})
		if err != nil {
			t.Fatalf("ProjectIssueRow: %v", err)
		}
		want := map[string]bool{"id": true, "status": true, "priority": true}
		for key := range row {
			if !want[key] {
				t.Errorf("row carries %q, which was not selected", key)
			}
		}
		for key := range want {
			if _, ok := row[key]; !ok {
				t.Errorf("row is missing selected key %q", key)
			}
		}
	})

	t.Run("the identifier comes back even when not asked for", func(t *testing.T) {
		row, err := ProjectIssueRow(ref, IssueRowOptions{Fields: []string{"title"}})
		if err != nil {
			t.Fatalf("ProjectIssueRow: %v", err)
		}
		if _, ok := row["id"]; !ok {
			t.Error("row has no id — US-2: the issue identifier is always returned")
		}
	})

	t.Run("empty selection yields the identifier alone", func(t *testing.T) {
		row, err := ProjectIssueRow(ref, IssueRowOptions{Fields: []string{}})
		if err != nil {
			t.Fatalf("empty selection was rejected: %v — the edge-case table says it returns "+
				"the identifier, the minimal recognisable minimum", err)
		}
		if len(row) != 1 {
			t.Errorf("empty selection returned %d keys, want exactly the identifier. Got: %v",
				len(row), sortedRowKeys(row))
		}
		if _, ok := row["id"]; !ok {
			t.Errorf("empty selection returned %v, want the identifier", sortedRowKeys(row))
		}
	})

	// sk-489, matrix row 3: the close outcome is "selectable, not
	// default". Two halves, and the second is the one the case was written for:
	// an OPEN row has no key even on an explicit request, because the outcome
	// exists if and only if the issue is closed (INV-1). Without it
	// "returned on explicit fields" would pass for a projection that emits
	// `closeOutcome: null` on every unclosed row.
	t.Run("sk-489: closeOutcome is returned on explicit fields — and only for a closed one", func(t *testing.T) {
		closed := referenceIssueSummary(t)
		closed.Status = "closed"
		closed.CloseOutcome = &[]string{"wont_do"}[0]

		row, err := ProjectIssueRow(closed, IssueRowOptions{Fields: []string{"closeOutcome"}})
		if err != nil {
			t.Fatalf("closeOutcome rejected: %v — it is outside the default set, not outside "+
				"existence, and `fields` is the named path to it", err)
		}
		if got := row["closeOutcome"]; got != "wont_do" {
			t.Errorf("the explicitly selected closeOutcome = %#v, expected \"wont_do\"", got)
		}

		open := referenceIssueSummary(t) // Status = "open", no outcome
		row, err = ProjectIssueRow(open, IssueRowOptions{Fields: []string{"closeOutcome"}})
		if err != nil {
			t.Fatalf("ProjectIssueRow on an open row: %v", err)
		}
		if _, present := row["closeOutcome"]; present {
			t.Errorf("the open row carries the closeOutcome key (%#v) — INV-1: the outcome exists exactly "+
				"on a closed one, and \"no key\" is an answer, not a gap", row["closeOutcome"])
		}
	})

	t.Run("a selectable field outside the default set is still servable", func(t *testing.T) {
		// INV-4: every narrowing has a named way out. Refusing a field
		// merely because the DEFAULT omits it would close that door.
		row, err := ProjectIssueRow(ref, IssueRowOptions{Fields: []string{"componentIds"}})
		if err != nil {
			t.Fatalf("componentIds was refused: %v — it is out of the default set, not out of "+
				"existence, and `fields` is the INV-4 way to reach it", err)
		}
		if _, ok := row["componentIds"]; !ok {
			t.Error("componentIds was selected but not returned")
		}
	})
}

// row 6 — an unknown name is rejected, and the rejection names it.
func TestProjectIssueRow_UnknownFieldIsRejectedByName(t *testing.T) {
	ref := referenceIssueSummary(t)
	const typo = "prioirty" // a plausible transposition of "priority"

	row, err := ProjectIssueRow(ref, IssueRowOptions{Fields: []string{"status", typo}})
	if err == nil {
		t.Fatalf("unknown field %q was accepted, row = %v. Silently ignoring it is "+
			"indistinguishable from \"the field is present but empty\"; silently returning everything "+
			"turns a typo into the fat response this change removes", typo, sortedRowKeys(row))
	}
	if row != nil {
		t.Errorf("a rejected selection still produced a row (%v) — the call must return nothing",
			sortedRowKeys(row))
	}

	var unknown *UnknownIssueRowFieldError
	if !errors.As(err, &unknown) {
		t.Fatalf("error is %T (%v), want *UnknownIssueRowFieldError so a tool can turn it into "+
			"a validation_error naming the field", err, err)
	}
	if unknown.Field != typo {
		t.Errorf("error names %q, want the offending name %q", unknown.Field, typo)
	}
	if !strings.Contains(err.Error(), typo) {
		t.Errorf("error text %q does not contain the unknown name %q — the caller cannot see "+
			"which name was wrong", err.Error(), typo)
	}
}

// row 7 — INV-1's "not provided" form.
//
// The two halves are the whole point: the same three bytes (`[]`) mean
// different things on the two carriers, and only the caller knows which.
func TestProjectIssueRow_NotProvidedVersusGenuinelyEmpty(t *testing.T) {
	t.Run("a declared not-provided key disappears entirely", func(t *testing.T) {
		// dependencies.IssueSummary marshals labels and componentIds as
		// `[]` although the ready query never loads them. The ready
		// surface declares that, and the keys must vanish rather than
		// claim "this issue has no labels".
		ready := dependencies.IssueSummary{
			ID:           "sk-282.18.7",
			ProjectID:    uuid.MustParse("01900000-0000-7000-8000-0000000000a1"),
			Version:      3,
			Title:        "ready row",
			Type:         "task",
			Priority:     1,
			Status:       "open",
			ComponentIDs: []uuid.UUID{},
			Labels:       []string{},
			UpdatedAt:    time.Now().UTC(),
			CreatedAt:    time.Now().UTC(),
		}
		row, err := ProjectIssueRow(ready, IssueRowOptions{
			Fields:      []string{"labels", "componentIds", "title"},
			NotProvided: []string{"labels", "componentIds"},
		})
		if err != nil {
			t.Fatalf("ProjectIssueRow: %v", err)
		}
		for _, key := range []string{"labels", "componentIds"} {
			if v, ok := row[key]; ok {
				t.Errorf("%q is present as %#v on a carrier that declared it not-provided — "+
					"an empty array here asserts \"there are no labels\", which this surface cannot know",
					key, v)
			}
		}
		if _, ok := row["title"]; !ok {
			t.Error("declaring two keys not-provided also dropped an unrelated one")
		}
	})

	t.Run("a genuinely loaded empty set stays an empty array", func(t *testing.T) {
		// The same key on a carrier that DOES load it: the issue really
		// has no labels, and `[]` is the honest answer.
		ref := referenceIssueSummary(t)
		ref.Labels = []string{}
		row, err := ProjectIssueRow(ref, defaultRowOptions())
		if err != nil {
			t.Fatalf("ProjectIssueRow: %v", err)
		}
		v, ok := row["labels"]
		if !ok {
			t.Fatal("labels vanished from a carrier that loads them — an empty but actually " +
				"loaded set stays an empty array")
		}
		arr, isSlice := v.([]any)
		if !isSlice || len(arr) != 0 {
			t.Errorf("labels = %#v, want an empty array", v)
		}
	})

	t.Run("a typo in NotProvided is rejected as loudly as one in Fields", func(t *testing.T) {
		// The asymmetry this pins: an unknown name in `fields` has always
		// failed, while a misspelled NotProvided key used to be a silent
		// no-op — and the field it was meant to remove stayed ON the wire.
		// INV-1 makes "not provided" an UNCONDITIONAL statement about this
		// surface; a declaration that can quietly miss its target states
		// nothing, and the row then claims "loaded, and empty" on behalf
		// of a surface that never loaded it.
		ready := dependencies.IssueSummary{
			ID: "sk-282.18.7", Title: "ready row", Status: "open",
			ComponentIDs: []uuid.UUID{}, Labels: []string{},
		}
		const typo = "lables"
		row, err := ProjectIssueRow(ready, IssueRowOptions{
			Fields:      []string{"labels", "title"},
			NotProvided: []string{typo},
		})
		if err == nil {
			t.Fatalf("NotProvided accepted %q silently, row = %v — the misspelled key removed "+
				"nothing and `labels` went out as `[]`", typo, sortedRowKeys(row))
		}
		if row != nil {
			t.Errorf("a rejected declaration still produced a row (%v)", sortedRowKeys(row))
		}
		var unknown *UnknownIssueRowFieldError
		if !errors.As(err, &unknown) {
			t.Fatalf("error is %T (%v), want *UnknownIssueRowFieldError — the same type the "+
				"`fields` path returns, so a tool renders both the same way", err, err)
		}
		if unknown.Field != typo {
			t.Errorf("error names %q, want the offending name %q", unknown.Field, typo)
		}
	})

	t.Run("declaring a key the carrier lacks is still legitimate", func(t *testing.T) {
		// The counterpart of the check above: NotProvided is precisely how
		// a surface says "my carrier cannot supply this default-set
		// field». dependencies.IssueSummary has no isBlocked, and the
		// ready surface declares it — that must keep working, or the
		// validation would have outlawed the mechanism it guards.
		ready := dependencies.IssueSummary{
			ID: "sk-282.18.7", Title: "ready row", Status: "open",
			ComponentIDs: []uuid.UUID{}, Labels: []string{},
		}
		row, err := ProjectIssueRow(ready, IssueRowOptions{
			ChildCounts: &IssueRowChildCounts{},
			NotProvided: []string{"isBlocked", "labels", "componentIds"},
		})
		if err != nil {
			t.Fatalf("ProjectIssueRow: %v — declaring a default-set field the carrier does not "+
				"have is the INV-1 mechanism, not a typo", err)
		}
		if _, ok := row["isBlocked"]; ok {
			t.Error("isBlocked survived a not-provided declaration")
		}
	})

	t.Run("selecting a field the carrier neither has nor declares is an error", func(t *testing.T) {
		// dependencies.IssueSummary has no isBlocked at all. Emitting the
		// row without it would silently break the default set's promise,
		// so the consumer has to say what it means.
		ready := dependencies.IssueSummary{
			ID: "sk-282.18.7", Title: "ready row", Status: "open",
			ComponentIDs: []uuid.UUID{}, Labels: []string{},
		}
		_, err := ProjectIssueRow(ready, IssueRowOptions{
			ChildCounts: &IssueRowChildCounts{},
		})
		if err == nil {
			t.Fatal("a carrier missing isBlocked produced a default row anyway — the missing " +
				"field would be indistinguishable from a deliberate omission")
		}
		var missing *MissingIssueRowValueError
		if !errors.As(err, &missing) {
			t.Fatalf("error is %T (%v), want *MissingIssueRowValueError", err, err)
		}
	})

	t.Run("selecting the counters without supplying them is an error", func(t *testing.T) {
		_, err := ProjectIssueRow(referenceIssueSummary(t), IssueRowOptions{
			Fields: []string{"childrenCount"},
		})
		if err == nil {
			t.Fatal("the counter slot was selected with nothing injected and the row came back " +
				"anyway — US-3 calls the counters mandatory, so their absence must not be silent")
		}
		var missing *MissingIssueRowValueError
		if !errors.As(err, &missing) {
			t.Fatalf("error is %T (%v), want *MissingIssueRowValueError", err, err)
		}
		if missing.Field != "childrenCount" {
			t.Errorf("error names %q, want %q", missing.Field, "childrenCount")
		}
	})
}

// marshalRow serialises a projected row the way a tool would.
func marshalRow(t *testing.T, row map[string]any) string {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	return string(b)
}

// sortedRowKeys renders a row's keys for a failure message.
func sortedRowKeys(row map[string]any) []string {
	out := make([]string, 0, len(row))
	for k := range row {
		out = append(out, k)
	}
	set := make(map[string]bool, len(out))
	for _, k := range out {
		set[k] = true
	}
	return sortedFieldNames(set)
}

// ---------------------------------------------------------------------
// sk-282 T006 — the drift-guard (test-plan row 8, plan D-9).
//
// This is not an extra assertion; it is the CONDITION under which US-2's
// named field list is allowed to exist at all. The spec's reasoning:
// R-2 rejects a parallel MCP struct because it silently loses every field
// later added to the shared type, and the loss looks exactly like a
// deliberate omission. A named list has the SAME flaw — so the spec
// permits it only together with a compensation, and requires that
// compensation to be "a mechanism that breaks on divergence, not a
// document". Without it the named list is forbidden and a denylist applies
// instead.
//
// The mechanism: enumerate the FULL JSON keyset of both row carriers and
// require every key to appear in exactly one of two explicit lists — "in
// the default" or "intentionally out". A field added to issues.IssueSummary lands
// in neither and turns this test red, which is the forced decision the
// spec asks for.
//
// Precedent in this repo: lean_test.go's
// TestLeanIssue_DiffersFromFullByExactlyKnownKeys does the same for the
// write-response projection. That test covers a different projection and
// is re-aimed by T028, not here.
// ---------------------------------------------------------------------
