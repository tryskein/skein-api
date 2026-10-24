package notifications

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFormatSummary_LifecycleVerbs covers the static cases — same shape
// pre and post R3.2b, with the issue.assigned / issue.priority_changed
// specific verbs removed (those event types no longer exist).
func TestFormatSummary_LifecycleVerbs(t *testing.T) {
	cases := []struct {
		evtType string
		actor   string
		ref     string
		want    string
	}{
		{"issue.created", "alice", "demo-1", "alice created demo-1"},
		{"issue.status_changed", "alice", "demo-1", "alice changed status of demo-1"},
		{"issue.archived", "alice", "demo-1", "alice archived demo-1"},
		{"comment.created", "alice", "demo-1", "alice commented on demo-1"},
		{"unknown.event", "alice", "demo-1", "alice updated demo-1"},
		// sk-489: without a payload the outcome-change line names the fact, but not the
		// "was -> became" pair — half a pair must not be printed.
		{EventTypeOutcomeChanged, "alice", "demo-1", "alice changed the outcome of demo-1"},
	}
	for _, tc := range cases {
		got := FormatSummary(tc.evtType, tc.actor, tc.ref, nil)
		if got != tc.want {
			t.Errorf("FormatSummary(%q) = %q, want %q", tc.evtType, got, tc.want)
		}
	}
}

// TestFormatSummary_IssueUpdated_FieldList renders the "(field,field)"
// suffix for issue.updated, listing only notifiable fields from the
// payload.changes array.
func TestFormatSummary_IssueUpdated_FieldList(t *testing.T) {
	mk := func(fields ...string) []byte {
		var changes []map[string]any
		for _, f := range fields {
			changes = append(changes, map[string]any{
				"field":    f,
				"oldValue": "x",
				"newValue": "y",
			})
		}
		b, _ := json.Marshal(map[string]any{"changes": changes})
		return b
	}

	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"only priority", mk("priority"), "alice updated demo-1 (priority)"},
		{"priority+assignee", mk("priority", "assignee"), "alice updated demo-1 (priority, assignee)"},
		{"title alone (non-notifiable)", mk("title"), "alice updated demo-1"},
		{"title + priority filtered", mk("title", "priority"), "alice updated demo-1 (priority)"},
		{"empty payload", []byte(""), "alice updated demo-1"},
		{"unparseable", []byte("not json"), "alice updated demo-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatSummary("issue.updated", "alice", "demo-1", tc.payload)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatSummary_IssueUpdated_OrderPreserved verifies that the
// rendered field list mirrors the order in payload.changes (the registry
// order at write time), not an alphabetical sort.
func TestFormatSummary_IssueUpdated_OrderPreserved(t *testing.T) {
	payload := []byte(`{"changes":[{"field":"assignee","oldValue":"","newValue":"x"},{"field":"priority","oldValue":1,"newValue":5}]}`)
	got := FormatSummary("issue.updated", "alice", "demo-1", payload)
	if !strings.Contains(got, "(assignee, priority)") {
		t.Fatalf("got %q, want order preserved (assignee then priority)", got)
	}
}

// TestFormatSummary_CommentMention renders the R4.3 comment-mention
// verb. payload is intentionally not consulted (no body quote per plan
// §F-10) — the format is "<actor> mentioned you in <entityRef>".
func TestFormatSummary_CommentMention(t *testing.T) {
	got := FormatSummary("comment.mention", "alice", "demo-1", nil)
	want := "alice mentioned you in demo-1"
	if got != want {
		t.Errorf("FormatSummary(comment.mention) = %q, want %q", got, want)
	}
}

// TestFormatSummary_IssueMention covers the R4.3 issue-mention verb
// with the field suffix derived from payload.fields.
func TestFormatSummary_IssueMention(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "single field",
			payload: []byte(`{"fields":["description"]}`),
			want:    "alice mentioned you in demo-1 (description)",
		},
		{
			name:    "multi field",
			payload: []byte(`{"fields":["description","notes"]}`),
			want:    "alice mentioned you in demo-1 (description, notes)",
		},
		{
			name:    "no fields key",
			payload: []byte(`{}`),
			want:    "alice mentioned you in demo-1",
		},
		{
			name:    "nil payload",
			payload: nil,
			want:    "alice mentioned you in demo-1",
		},
		{
			name:    "unparseable",
			payload: []byte("not json"),
			want:    "alice mentioned you in demo-1",
		},
		{
			name:    "all four fields",
			payload: []byte(`{"fields":["description","notes","acceptance_criteria","design"]}`),
			want:    "alice mentioned you in demo-1 (description, notes, acceptance_criteria, design)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatSummary("issue.mention", "alice", "demo-1", tc.payload)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// sk-489 / US-3 — the outcome in the short summary (matrix row 44, SC-011)
// ---------------------------------------------------------------------------

// closingPayloadJSON builds the payload of a closing event in the same shape
// in which issues/close_outcome.go#closeOutcomePayload writes it: the outcome always,
// the previous outcome only when there was a change.
func closingPayloadJSON(t *testing.T, outcome, previous string) []byte {
	t.Helper()
	p := map[string]any{"closeOutcome": outcome}
	if previous != "" {
		p["previousCloseOutcome"] = previous
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal closing payload: %v", err)
	}
	return b
}

// TestFormatSummary_NamesTheOutcome — SC-011 on the short-summary surface.
//
// Four assertions, and three of them are GUARDS of the former text. Without them
// an implementation appending "(outcome: …)" to every close would be
// green: the gain of the new line shows only against the fact that the line of
// an ordinary close did not change by a single byte.
func TestFormatSummary_NamesTheOutcome(t *testing.T) {
	cases := []struct {
		name    string
		evType  string
		payload []byte
		want    string
	}{
		{
			"closing without work names the outcome",
			"issue.status_changed",
			closingPayloadJSON(t, "wont_do", ""),
			"alice changed status of demo-1 (outcome: won't do)",
		},
		{
			"closing as done — text as today",
			"issue.status_changed",
			closingPayloadJSON(t, "done", ""),
			"alice changed status of demo-1",
		},
		{
			"status change without payload — text as today",
			"issue.status_changed",
			nil,
			"alice changed status of demo-1",
		},
		{
			"unreadable payload does not break the line",
			"issue.status_changed",
			[]byte("{not json"),
			"alice changed status of demo-1",
		},
		{
			"duplicate is called by its own word, not \"not done\"",
			"issue.status_changed",
			closingPayloadJSON(t, "duplicate", ""),
			"alice changed status of demo-1 (outcome: duplicate)",
		},
		{
			"unknown outcome is printed as is (INV-2)",
			"issue.status_changed",
			closingPayloadJSON(t, "abandoned_in_2031", ""),
			"alice changed status of demo-1 (outcome: abandoned_in_2031)",
		},
		{
			"outcome change prints the \"was -> became\" pair",
			EventTypeOutcomeChanged,
			closingPayloadJSON(t, "wont_do", "done"),
			"alice changed the outcome of demo-1 (done → won't do)",
		},
		{
			"outcome change between the dropped ones",
			EventTypeOutcomeChanged,
			closingPayloadJSON(t, "duplicate", "wont_do"),
			"alice changed the outcome of demo-1 (won't do → duplicate)",
		},
		{
			"half a pair is not printed",
			EventTypeOutcomeChanged,
			closingPayloadJSON(t, "wont_do", ""),
			"alice changed the outcome of demo-1",
		},
		// issue.closed — a GUARD of a reason unreachable today: the type is declared
		// journal-only, no notification rows with it exist. It is checked not
		// "just in case", but because the branch exists in the code, and the test must
		// say what exactly it asserts.
		{
			"guard: closing without work, if the type ever gets a reason",
			"issue.closed",
			closingPayloadJSON(t, "wont_do", ""),
			"alice closed demo-1 (outcome: won't do)",
		},
		{
			"guard: closing as done grows no tail",
			"issue.closed",
			closingPayloadJSON(t, "done", ""),
			"alice closed demo-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatSummary(tc.evType, "alice", "demo-1", tc.payload); got != tc.want {
				t.Errorf("FormatSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatSummary_GuardIsAuditOnlyForIssueClosed — the half without which the
// previous "guard" would lie to the reader: issue.closed IS journal-only,
// that is, its branch in FormatSummary is unreachable today. The claim
// "unreachable" is a claim about the code just like the text itself, and it
// must be checked, not stand as a comment.
func TestFormatSummary_GuardIsAuditOnlyForIssueClosed(t *testing.T) {
	if !IsAuditOnlyEventType("issue.closed") {
		t.Fatal("issue.closed is no longer journal-only — the FormatSummary branch has become a working path, " +
			"and its doc block \"guard\" is due for a rewrite together with the test")
	}
}

// TestCloseOutcomeLabel_UnknownValuePrintsRaw: an outcome the label map does
// not know is printed as is (INV-2), never dropped or blanked.
func TestCloseOutcomeLabel_UnknownValuePrintsRaw(t *testing.T) {
	if got := closeOutcomeLabel("something_new"); got != "something_new" {
		t.Errorf("unknown outcome = %q, want it printed as is (INV-2)", got)
	}
}
