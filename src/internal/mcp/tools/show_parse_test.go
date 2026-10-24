package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

func TestParseOptionalWholeInt(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		def     int
		want    int
		wantErr bool
	}{
		{"absent", map[string]any{}, 3, 3, false},
		{"nil", map[string]any{"commentsLimit": nil}, 3, 3, false},
		{"int", map[string]any{"commentsLimit": 5}, 3, 5, false},
		{"zero", map[string]any{"commentsLimit": 0}, 3, 0, false},
		{"float whole", map[string]any{"commentsLimit": float64(7)}, 3, 7, false},
		{"float non-int", map[string]any{"commentsLimit": 3.7}, 3, 0, true},
		{"neg float", map[string]any{"commentsLimit": float64(-1)}, 3, -1, false},
		{"string int", map[string]any{"commentsLimit": "10"}, 3, 10, false},
		{"string float", map[string]any{"commentsLimit": "3.7"}, 3, 0, true},
		{"empty string", map[string]any{"commentsLimit": ""}, 3, 0, true},
		{"whitespace string", map[string]any{"commentsLimit": "  "}, 3, 0, true},
		{"bool", map[string]any{"commentsLimit": true}, 3, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOptionalWholeInt(tc.args, "commentsLimit", tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseShowCommentsOpts_Validation(t *testing.T) {
	// Non-integer limit → validation_error result.
	_, res := parseShowCommentsOpts(map[string]any{"commentsLimit": 3.7})
	if res == nil {
		t.Fatal("expected validation error for 3.7")
	}
	// Negative limit.
	_, res = parseShowCommentsOpts(map[string]any{"commentsLimit": -1})
	if res == nil {
		t.Fatal("expected validation error for -1")
	}
	// Invalid since.
	_, res = parseShowCommentsOpts(map[string]any{"commentsSince": "not-a-date"})
	if res == nil {
		t.Fatal("expected validation error for bad since")
	}
	// Happy default.
	opts, res := parseShowCommentsOpts(map[string]any{})
	if res != nil {
		t.Fatalf("unexpected err: %+v", res)
	}
	if opts.Limit != 3 {
		t.Fatalf("default limit = %d, want 3", opts.Limit)
	}
	if opts.Since != nil {
		t.Fatalf("default since should be nil")
	}
	// limit 0 + valid since.
	opts, res = parseShowCommentsOpts(map[string]any{
		"commentsLimit": 0,
		"commentsSince": "2026-07-01T12:00:00Z",
	})
	if res != nil {
		t.Fatalf("unexpected err: %+v", res)
	}
	if opts.Limit != 0 {
		t.Fatalf("limit = %d, want 0", opts.Limit)
	}
	if opts.Since == nil {
		t.Fatal("since should be set")
	}
}

// ---------------------------------------------------------------------
// sk-282 T009 — the US-1 input binding (test-plan row 20).
//
// "All 9 combinations (`includeDeps` ∈ {absent, true, false} × `depsTypes` ∈
// {absent, empty, non-empty}) land in exactly one rule."
//
// The table is transcribed from the plan's "Wire form", not read off
// resolveDepsRequest: this test's job is to hold the code to the plan, and
// deriving the expectations from the code would make any rule renumbering
// agree with itself.
//
// Two of these nine are the ones that already cost rounds of review, and
// they are called out by name below: the DEFAULT call must not fall into
// the refusal, and a non-empty type list without includeDeps must not fall
// into the counters-only default.
// ---------------------------------------------------------------------

// TestResolveDepsRequest_NineCombinations walks the whole argument space.
func TestResolveDepsRequest_NineCombinations(t *testing.T) {
	// The three values of each axis, spelled as they arrive in the raw
	// argument map. «absent» is the absence of the key — which is exactly
	// the distinction req.GetBool cannot make and the binding turns on.
	const absent = "<<absent>>"

	includeDepsAxis := []struct {
		name string
		val  any
	}{
		{"includeDeps absent", absent},
		{"includeDeps true", true},
		{"includeDeps false", false},
	}
	depsTypesAxis := []struct {
		name string
		val  any
	}{
		{"depsTypes absent", absent},
		{"depsTypes empty", []any{}},
		{"depsTypes non-empty", []any{"parent-child"}},
	}

	// want[includeDeps][depsTypes] — transcribed from the plan's table of
	// nine combinations.
	want := [3][3]depsRule{
		// includeDeps absent: absent → 5, empty → 2, non-empty → 3
		{depsRuleCountsOnly, depsRuleEmptyTypes, depsRuleTypedEdges},
		// includeDeps true:   absent → 4, empty → 2, non-empty → 3
		{depsRuleNoTypes, depsRuleEmptyTypes, depsRuleTypedEdges},
		// includeDeps false:  every column → 1
		{depsRuleOff, depsRuleOff, depsRuleOff},
	}
	// Which of the nine are refusals rather than answers.
	wantRefusal := [3][3]bool{
		{false, true, false},
		{true, true, false},
		{false, false, false},
	}

	for i, inc := range includeDepsAxis {
		for j, types := range depsTypesAxis {
			t.Run(inc.name+" / "+types.name, func(t *testing.T) {
				args := map[string]any{"id": "sk-1"}
				if inc.val != any(absent) {
					args["includeDeps"] = inc.val
				}
				if types.val != any(absent) {
					args["depsTypes"] = types.val
				}

				got, err := resolveDepsRequest(args)

				if wantRefusal[i][j] {
					if err == nil {
						t.Fatalf("combination was accepted (rule %d, counters=%v, types=%v); "+
							"the plan's table refuses it under rule %d",
							got.Rule, got.Counters, got.Types, want[i][j])
					}
					if err.Rule != want[i][j] {
						t.Errorf("refused under rule %d, want rule %d", err.Rule, want[i][j])
					}
					if err.Field == "" {
						t.Error("the refusal names no parameter — a refusal with a clear error " +
							"means the caller can tell WHICH parameter to fix")
					}
					return
				}

				if err != nil {
					t.Fatalf("combination was refused (%v); the plan's table answers it "+
						"under rule %d", err, want[i][j])
				}
				if got.Rule != want[i][j] {
					t.Errorf("matched rule %d, want rule %d", got.Rule, want[i][j])
				}
			})
		}
	}
}

// TestResolveDepsRequest_TheTwoTrapsFromReview states the two readings
// that pass a casual look and destroy a feature each, as their own named
// cases. They are covered by the table above; they are repeated here
// because a table failure says «rule 4, want 5» while these say what broke.
func TestResolveDepsRequest_TheTwoTrapsFromReview(t *testing.T) {
	t.Run("the default call is not a refusal", func(t *testing.T) {
		// includeDeps declares a default of true. Reading «includeDeps:true
		// without types → refuse» off the effective VALUE rather than the
		// key's presence turns every parameterless read into an error —
		// and SC-001 is stated on exactly that read.
		got, err := resolveDepsRequest(map[string]any{"id": "sk-1"})
		if err != nil {
			t.Fatalf("a call with no dependency parameters was refused (%v). The refusal is "+
				"about the KEY being present, not about includeDeps being effectively "+
				"true — otherwise the default read, which SC-001 measures, always errors", err)
		}
		if got.Rule != depsRuleCountsOnly || !got.Counters || len(got.Types) != 0 {
			t.Errorf("default = rule %d counters=%v types=%v, want rule 5 with counters and "+
				"no edges", got.Rule, got.Counters, got.Types)
		}
	})

	t.Run("a type list without includeDeps still returns edges", func(t *testing.T) {
		// US-10 teaches agents not to re-send default flags, so the real
		// «counters → edges of the type I need» call carries depsTypes and
		// nothing else. A rule «no includeDeps key → no edges» applied
		// without priority swallows it, and the advertised route never
		// works.
		got, err := resolveDepsRequest(map[string]any{
			"id":        "sk-1",
			"depsTypes": []any{"parent-child"},
		})
		if err != nil {
			t.Fatalf("depsTypes without includeDeps was refused (%v) — that is the US-10 "+
				"route, not a malformed call", err)
		}
		if got.Rule != depsRuleTypedEdges {
			t.Errorf("matched rule %d, want rule 3", got.Rule)
		}
		if len(got.Types) != 1 || got.Types[0] != "parent-child" {
			t.Errorf("types = %v, want [parent-child]", got.Types)
		}
		if !got.Counters {
			t.Error("a typed edge request dropped the counters — an agent that asked for one " +
				"type still has to see that the others exist, or the typed request is a " +
				"narrower lie than the default")
		}
	})

	t.Run("an explicit switch-off dominates a type list", func(t *testing.T) {
		// Rule 1 wins over rule 3. Answering this with a refusal would add
		// a second error path for no gain; answering it with edges would
		// ignore the strongest statement of intent the caller can make.
		got, err := resolveDepsRequest(map[string]any{
			"id":          "sk-1",
			"includeDeps": false,
			"depsTypes":   []any{"parent-child"},
		})
		if err != nil {
			t.Fatalf("includeDeps:false with a type list was refused (%v), want rule 1", err)
		}
		if got.Rule != depsRuleOff || got.Counters || len(got.Types) != 0 {
			t.Errorf("= rule %d counters=%v types=%v, want rule 1 with neither edges nor "+
				"counters", got.Rule, got.Counters, got.Types)
		}
	})
}

// TestResolveDepsRequest_MalformedInput covers the shapes that are not on
// either axis: a wrong type for a parameter, and a misspelled edge type.
func TestResolveDepsRequest_MalformedInput(t *testing.T) {
	cases := []struct {
		name      string
		args      map[string]any
		wantField string
		// wantNames is the offending type name the error must quote.
		wantNames []string
	}{
		{
			name:      "includeDeps is not a boolean",
			args:      map[string]any{"includeDeps": "false"},
			wantField: "includeDeps",
		},
		{
			name:      "depsTypes is a bare string",
			args:      map[string]any{"depsTypes": "parent-child"},
			wantField: "depsTypes",
		},
		{
			name:      "depsTypes contains a non-string",
			args:      map[string]any{"depsTypes": []any{42}},
			wantField: "depsTypes",
		},
		{
			name:      "depsTypes names an unknown edge type",
			args:      map[string]any{"depsTypes": []any{"parent_child"}},
			wantField: "depsTypes",
			wantNames: []string{"parent_child"},
		},
		{
			name:      "depsTypes is blank strings only",
			args:      map[string]any{"depsTypes": []any{"", "   "}},
			wantField: "depsTypes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveDepsRequest(tc.args)
			if err == nil {
				t.Fatal("malformed input was accepted")
			}
			if err.Field != tc.wantField {
				t.Errorf("error names field %q, want %q", err.Field, tc.wantField)
			}
			for _, name := range tc.wantNames {
				found := false
				for _, got := range err.UnknownTypes {
					if got == name {
						found = true
					}
				}
				if !found {
					t.Errorf("error does not name the offending type %q (got %v) — a silent "+
						"empty result for a misspelled type is indistinguishable from «there "+
						"are none»", name, err.UnknownTypes)
				}
				if !strings.Contains(err.Message, name) {
					t.Errorf("error text %q does not quote %q", err.Message, name)
				}
			}
		})
	}
}

// TestResolveDepsRequest_TypesAreCanonicalised: duplicates collapse and
// surrounding whitespace is ignored, so the same intent spelled two ways
// produces one filter rather than two.
func TestResolveDepsRequest_TypesAreCanonicalised(t *testing.T) {
	got, err := resolveDepsRequest(map[string]any{
		"depsTypes": []any{"parent-child", " parent-child ", "blocks"},
	})
	if err != nil {
		t.Fatalf("resolveDepsRequest: %v", err)
	}
	if len(got.Types) != 2 {
		t.Fatalf("types = %v, want two distinct types", got.Types)
	}
}

// ---------------------------------------------------------------------
// The per-type edge valve, and the direction it can zero.
//
// The budget is per TYPE across both directions and is spent
// outgoing-first, so a node with more than the cap of outgoing edges of
// the requested type returns `incoming: []`. An aggregate-only marker
// (returned / total) does not tell the caller WHICH side was cut, so that
// empty array is indistinguishable from «this node has no incoming edges
// of that type» — a narrowing that does not describe itself, which is
// what INV-1 forbids. The report therefore carries both directions.
// ---------------------------------------------------------------------

func TestFilterDepsByType_ZeroedDirectionIsIdentifiable(t *testing.T) {
	const outgoingCount = ShowDepsEdgeCapPerType + 5
	const incomingCount = 3

	var list dependencies.DependencyList
	for i := 0; i < outgoingCount; i++ {
		list.Outgoing = append(list.Outgoing, dependencies.DependencyEdge{
			FromID: "sk-1", ToID: fmt.Sprintf("sk-1.%d", i+1),
			Type: dependencies.DepParentChild,
		})
	}
	for i := 0; i < incomingCount; i++ {
		list.Incoming = append(list.Incoming, dependencies.DependencyEdge{
			FromID: fmt.Sprintf("sk-2.%d", i+1), ToID: "sk-1",
			Type: dependencies.DepParentChild,
		})
	}

	outgoing, incoming, truncated := filterDepsByType(&list, []string{dependencies.DepParentChild})

	if len(outgoing)+len(incoming) != ShowDepsEdgeCapPerType {
		t.Fatalf("returned %d+%d edges, want the cap of %d — the shared budget is what keeps "+
			"the declared result ceiling honest and it must not change",
			len(outgoing), len(incoming), ShowDepsEdgeCapPerType)
	}
	if len(incoming) != 0 {
		t.Fatalf("incoming = %d edges — the fixture is meant to spend the whole budget on "+
			"outgoing", len(incoming))
	}

	cell, ok := truncated[dependencies.DepParentChild]
	if !ok {
		t.Fatalf("no truncation marker although %d edges were cut: %#v",
			outgoingCount+incomingCount-ShowDepsEdgeCapPerType, truncated)
	}
	if cell.Incoming.Total != incomingCount || cell.Incoming.Returned != 0 {
		t.Errorf("truncated.incoming = {returned:%d total:%d}, want {0 %d}. `incoming: []` next "+
			"to an aggregate-only marker cannot be told apart from «there are no incoming "+
			"edges of this type — INV-1: a narrowing must be self-describing",
			cell.Incoming.Returned, cell.Incoming.Total, incomingCount)
	}
	if cell.Outgoing.Returned != ShowDepsEdgeCapPerType || cell.Outgoing.Total != outgoingCount {
		t.Errorf("truncated.outgoing = {returned:%d total:%d}, want {%d %d}",
			cell.Outgoing.Returned, cell.Outgoing.Total, ShowDepsEdgeCapPerType, outgoingCount)
	}
	// The aggregate stays what it was — the direction split is additive.
	if cell.Returned != ShowDepsEdgeCapPerType || cell.Total != outgoingCount+incomingCount {
		t.Errorf("truncated aggregate = {returned:%d total:%d}, want {%d %d}",
			cell.Returned, cell.Total, ShowDepsEdgeCapPerType, outgoingCount+incomingCount)
	}
	// …and both halves must survive to the wire, since that is where the
	// caller reads them.
	raw, err := json.Marshal(cell)
	if err != nil {
		t.Fatalf("marshal truncation cell: %v", err)
	}
	var onWire map[string]any
	if err := json.Unmarshal(raw, &onWire); err != nil {
		t.Fatalf("unmarshal truncation cell: %v", err)
	}
	for _, key := range []string{"returned", "total", "outgoing", "incoming", "continue"} {
		if _, ok := onWire[key]; !ok {
			t.Errorf("the truncation marker has no %q key on the wire: %s", key, raw)
		}
	}
}

// The mirror image: the same valve, spent by the incoming side, must name
// the outgoing side as the zeroed one. Without this the direction split
// could be hard-wired the wrong way round and the first test would not
// notice.
func TestFilterDepsByType_ZeroedOutgoingIsIdentifiable(t *testing.T) {
	var list dependencies.DependencyList
	for i := 0; i < 4; i++ {
		list.Outgoing = append(list.Outgoing, dependencies.DependencyEdge{
			FromID: "sk-1", ToID: fmt.Sprintf("sk-9.%d", i+1),
			Type: dependencies.DepBlocks,
		})
	}
	for i := 0; i < ShowDepsEdgeCapPerType+2; i++ {
		list.Incoming = append(list.Incoming, dependencies.DependencyEdge{
			FromID: fmt.Sprintf("sk-8.%d", i+1), ToID: "sk-1",
			Type: dependencies.DepBlocks,
		})
	}

	// Outgoing is served first, so it is the INCOMING side that gets cut
	// here — and the marker has to say so.
	outgoing, incoming, truncated := filterDepsByType(&list, []string{dependencies.DepBlocks})
	cell := truncated[dependencies.DepBlocks]
	if cell.Outgoing.Returned != len(outgoing) || cell.Outgoing.Total != 4 {
		t.Errorf("truncated.outgoing = {returned:%d total:%d}, want {%d 4}",
			cell.Outgoing.Returned, cell.Outgoing.Total, len(outgoing))
	}
	if cell.Incoming.Returned != len(incoming) || cell.Incoming.Total != ShowDepsEdgeCapPerType+2 {
		t.Errorf("truncated.incoming = {returned:%d total:%d}, want {%d %d}",
			cell.Incoming.Returned, cell.Incoming.Total, len(incoming), ShowDepsEdgeCapPerType+2)
	}
}

// ---------------------------------------------------------------------
// sk-282 T011 — the three completeness fields (test-plan row 34).
//
// "Matrix of the three completeness fields, the rule incomplete ⇔ listed fewer than
// total for all five modes, including the composite window + whole thread."
//
// The five modes differ only in what POPULATION they count and how many
// entries they list; the rule on top is one and the same, and that is the
// whole point — the guide's prescription keys on this flag, so a second
// interpretation anywhere puts the loop back.
//
// Each row therefore states the mode's (listed, total) pair as US-4's
// table defines it and asserts the single rule over it. Pure: no store,
// no context.
// ---------------------------------------------------------------------

func TestThreadIncomplete_AllFiveModes(t *testing.T) {
	cases := []struct {
		mode string
		// listed / total as US-4's table defines them for this mode.
		listed int
		total  int
		// capApplied is the safety valve in play for this mode — the
		// index cap in the whole-thread modes, none where the caller's
		// own limit decides the page size.
		capApplied int
		want       bool
		why        string
	}{
		{
			mode: "no parameters, thread longer than the tail",
			// "3 (or fewer, if the population is smaller)" vs the whole thread.
			listed: 3, total: 48, want: true,
			why: "three of forty-eight were listed",
		},
		{
			mode:   "no parameters, thread shorter than the tail",
			listed: 2, total: 2, want: false,
			why: "the whole population fitted in the default page",
		},
		{
			mode:   "N last, N below the population",
			listed: 10, total: 48, want: true,
			why: "ten of forty-eight",
		},
		{
			mode: "N last, N above the population",
			// "min(N, total) — actually returned, not requested."
			listed: 2, total: 2, want: false,
			why: "the population is shorter than N, and what was listed is all of it",
		},
		{
			mode: "time window, everything in the window listed",
			// The population IS the window.
			listed: 7, total: 7, want: false,
			why: "total counts the window, not the thread behind it",
		},
		{
			mode:   "time window, window longer than the page",
			listed: 3, total: 7, want: true,
			why: "three of the window's seven",
		},
		{
			mode:   "whole thread, population within the index cap",
			listed: 48, total: 48, capApplied: ShowThreadIndexCap, want: false,
			why: "this is the row that opens the loop: the answer to the prescription is " +
				"complete",
		},
		{
			mode:   "whole thread, population past the index cap",
			listed: ShowThreadIndexCap, total: ShowThreadIndexCap + 7,
			capApplied: ShowThreadIndexCap, want: true,
			why: "two hundred most recent of the population were listed, and there are more",
		},
		{
			mode: "window plus whole thread, window within the cap",
			// The composite mode needs no rule of its own — the window
			// narrows the population and the last two rows apply.
			listed: 12, total: 12, capApplied: ShowThreadIndexCap, want: false,
			why: "the cap applies to the window, and the window was listed in full",
		},
		{
			mode:   "window plus whole thread, window past the cap",
			listed: ShowThreadIndexCap, total: ShowThreadIndexCap + 4,
			capApplied: ShowThreadIndexCap, want: true,
			why: "the cap applied to the WINDOW, and the window holds more than the cap",
		},
		{
			mode:   "empty thread",
			listed: 0, total: 0, want: false,
			why: "nothing was withheld, so nothing may claim it was",
		},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			if got := threadIncomplete(tc.listed, tc.total, tc.capApplied); got != tc.want {
				t.Errorf("threadIncomplete(listed=%d, total=%d, cap=%d) = %v, want %v — %s",
					tc.listed, tc.total, tc.capApplied, got, tc.want, tc.why)
			}
		})
	}
}

// TestThreadIncomplete_IsExactlyTheOneRule guards against the rule being
// re-derived somewhere with a different meaning — "incomplete" must not creep
// back to «some bodies were withheld».
func TestThreadIncomplete_IsExactlyTheOneRule(t *testing.T) {
	// Listing MORE than the total is nonsense, but it must not read as
	// incomplete either: the flag is about a shortfall, not about a
	// mismatch.
	if threadIncomplete(5, 4, 0) {
		t.Error("threadIncomplete(5, 4) = true — the flag means «fewer were listed than exist», " +
			"and an over-count is a bug elsewhere, not an incomplete listing")
	}
	if !threadIncomplete(0, 1, 0) {
		t.Error("threadIncomplete(0, 1) = false — nothing listed out of one is incomplete")
	}
}

// TestThreadIncomplete_AtTheCapNeverClaimsCompleteness pins the guard
// against the ONE race direction that can make the completeness flag lie.
//
// ListComments takes `total` and the page in two statements and the spec
// promises no snapshot between them. Nearly every interleaving errs toward
// false INcompleteness, which costs a re-read and nothing else. The
// exception: a thread that grew between the two statements while the page
// hit the index cap yields listed == cap > total, and the shortfall rule
// alone (listed < total) would then report a listing that was CUT at the
// valve as complete — an agent following US-8 would stop reading a thread
// it has not seen the whole of, with nothing in the response to warn it.
func TestThreadIncomplete_AtTheCapNeverClaimsCompleteness(t *testing.T) {
	capAt := ShowThreadIndexCap

	// The race: total was measured before the thread grew, so it is
	// SMALLER than what the capped page returned.
	if !threadIncomplete(capAt, capAt-50, capAt) {
		t.Errorf("threadIncomplete(listed=%d, total=%d, cap=%d) = false — a listing that is "+
			"exactly as long as the valve that produced it must never report completeness: "+
			"total was read by a different statement than the page, and this is the one "+
			"direction of that race the caller cannot detect",
			capAt, capAt-50, capAt)
	}
	// Exactly at the cap with a total that agrees: still incomplete. The
	// page cannot tell «the population is exactly 200» from «the valve cut
	// it at 200», and over-setting the marker is the safe half.
	if !threadIncomplete(capAt, capAt, capAt) {
		t.Errorf("threadIncomplete(listed=%d, total=%d, cap=%d) = false — at the valve the "+
			"listing is capped, not proven complete", capAt, capAt, capAt)
	}
	// One below the cap under the same valve: the shortfall rule and
	// nothing else. A guard that fired here would make the flag useless.
	if threadIncomplete(capAt-1, capAt-1, capAt) {
		t.Errorf("threadIncomplete(listed=%d, total=%d, cap=%d) = true — below the valve the "+
			"rule is the shortfall and only the shortfall", capAt-1, capAt-1, capAt)
	}
	// A caller-chosen limit is NOT a valve: the whole point of US-4 is
	// that the default read of a short thread reports completeness, or the
	// prescription loops.
	if threadIncomplete(showThreadTailSize, showThreadTailSize, 0) {
		t.Errorf("threadIncomplete(listed=%d, total=%d, cap=0) = true — the default page size "+
			"is a request, not a safety valve, and marking it incomplete re-opens the US-8 "+
			"loop", showThreadTailSize, showThreadTailSize)
	}
}

// TestBuildCommentIndex_PreviewAndTombstone covers the pure half of the
// index entry: the INV-1 marker on the ENTRY, and the tombstone.
func TestBuildCommentIndex_PreviewAndTombstone(t *testing.T) {
	deletedAt := time.Date(2026, 8, 20, 4, 0, 0, 0, time.UTC)
	entries := buildCommentIndex([]issues.CommentDTO{
		{
			ID:        uuid.MustParse("01900000-0000-7000-8000-00000000000a"),
			Author:    "alice",
			Body:      "short",
			CreatedAt: time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC),
		},
		{
			ID:        uuid.MustParse("01900000-0000-7000-8000-00000000000b"),
			Author:    "alice",
			Body:      strings.Repeat("ж", ThreadIndexPreviewMaxRunes+5),
			CreatedAt: time.Date(2026, 8, 20, 3, 1, 0, 0, time.UTC),
		},
		{
			ID:        uuid.MustParse("01900000-0000-7000-8000-00000000000c"),
			Author:    "alice",
			Body:      "",
			CreatedAt: time.Date(2026, 8, 20, 3, 2, 0, 0, time.UTC),
			DeletedAt: &deletedAt,
		},
	})

	if len(entries) != 3 {
		t.Fatalf("index = %d entries, want 3 — every comment gets one, deleted included", len(entries))
	}
	if entries[0].PreviewTruncated {
		t.Error("a body shorter than the fragment carries the truncation marker")
	}
	if !entries[1].PreviewTruncated {
		t.Error("a body longer than the fragment carries no truncation marker")
	}
	if got := len([]rune(entries[1].Preview)); got != ThreadIndexPreviewMaxRunes {
		t.Errorf("preview is %d runes, want %d — the cut is by runes, so Cyrillic is not "+
			"halved", got, ThreadIndexPreviewMaxRunes)
	}
	if entries[2].DeletedAt == nil {
		t.Error("the tombstone entry lost deletedAt")
	}
	if entries[2].PreviewTruncated {
		t.Error("the tombstone claims a truncated preview — nothing was cut from a blanked body")
	}
}
