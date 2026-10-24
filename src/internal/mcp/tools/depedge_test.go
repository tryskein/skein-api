package tools

// depedge_test.go — sk-282 T010, test-plan rows 28, 29, 30.
//
//	row 28  US-1a: the projection carries identification of the other side
//	            and the character of the link; the service ballast (empty
//	            metadata map, version, created-at, nested project id) is gone
//	row 29  SC-009: an edge on the reference issue is ≤190 characters with a
//	            service part ≤130; an edge to an invisible issue is ≤110
//	row 30  US-1a Unchanged: an edge to an invisible issue keeps both side
//	            ids and the redaction flag, and the missing card does not
//	            read as «there is no link»
//
// Row 31 is the mirror of row 28 on the REST side and lives with the REST
// handler — the point of that row is that the fields removed HERE are the
// exact leak vector THERE.
//
// Pure unit: no pool, no database.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/dependencies"
)

// sc009EdgeBudget / sc009ServiceBudget / sc009RedactedBudget are SC-009's
// three numbers, and referenceTitleRunes is the title length they are
// stated against.
const (
	sc009EdgeBudget     = 190
	sc009ServiceBudget  = 130
	sc009RedactedBudget = 110
	sc009ReferenceTitle = 60
)

// referenceEdge is an edge on US-2's reference issue: the other side has a
// 60-character title, which is what SC-009 shifts its budget by.
//
// The ids are short on purpose. SC-009's service budget of 130 is derived
// from the NAMED COMPOSITION of US-1a — "on US-1a's named composition a
// compact JSON comes to about 117 characters" — and that estimate is only
// reproducible with ids of the shape a first-level issue has. Ids are
// variable-length text the spec fixes no budget shift for; see
// TestProjectDepEdge_DeepHierarchicalIdsCostTheServiceBudget for what
// happens when they are long, which is reported rather than asserted.
func referenceEdge() dependencies.DependencyEdge {
	return dependencies.DependencyEdge{
		FromID:    "demo-42.1",
		ToID:      "demo-42",
		Type:      dependencies.DepParentChild,
		Metadata:  map[string]any{},
		Version:   1,
		CreatedAt: time.Date(2026, 8, 20, 2, 5, 41, 488283000, time.UTC),
		Issue: &dependencies.IssueRef{
			ID:        "demo-42",
			ProjectID: uuid.MustParse("01900000-0000-7000-8000-0000000000a1"),
			Title:     strings.Repeat("x", sc009ReferenceTitle),
			Status:    "in_progress",
			Priority:  1,
			Type:      "task",
		},
	}
}

// redactedEdge is an edge whose other side lives in a project the caller
// cannot see: no card at all.
func redactedEdge() dependencies.DependencyEdge {
	return dependencies.DependencyEdge{
		FromID:    "demo-42.1",
		ToID:      "other-4816",
		Type:      dependencies.DepRelated,
		Metadata:  map[string]any{},
		Version:   1,
		CreatedAt: time.Date(2026, 8, 20, 2, 5, 41, 488283000, time.UTC),
		Redacted:  true,
	}
}

// row 28 — what the projection keeps and what it drops.
func TestProjectDepEdge_KeepsIdentificationDropsBallast(t *testing.T) {
	encoded := marshalEdge(t, projectDepEdge(referenceEdge()))

	var got map[string]any
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("unmarshal edge: %v", err)
	}

	t.Run("identification of the other side and of the link", func(t *testing.T) {
		want := map[string]any{
			"fromId":    "demo-42.1",
			"toId":      "demo-42",
			"type":      "parent-child",
			"title":     strings.Repeat("x", sc009ReferenceTitle),
			"status":    "in_progress",
			"issueType": "task",
			"priority":  float64(1),
		}
		for key, wantVal := range want {
			gotVal, ok := got[key]
			if !ok {
				t.Errorf("edge is missing %q — US-1a: the edge carries identification of the "+
					"other side and the character of the link", key)
				continue
			}
			if gotVal != wantVal {
				t.Errorf("edge[%q] = %#v, want %#v", key, gotVal, wantVal)
			}
		}
	})

	t.Run("the ballast is gone", func(t *testing.T) {
		// Named one by one out of US-1a's Current sentence: "always an empty
		// metadata map, version, created stamp and a nested card of the
		// other side with the project identifier."
		for _, key := range []string{"metadata", "version", "createdAt", "issue", "projectId"} {
			if v, ok := got[key]; ok {
				t.Errorf("edge still carries %q = %#v — that is the ~two thirds of an edge "+
					"which answers no question an agent reading an issue has", key, v)
			}
		}
	})

	t.Run("a visible edge carries no redaction flag", func(t *testing.T) {
		// The flag means «you cannot see this one». Always emitting it
		// would spend bytes on every edge to say nothing.
		if v, ok := got["redacted"]; ok {
			t.Errorf("redacted = %#v on a visible edge — the flag is set only when it is true", v)
		}
	})

	t.Run("no key survives that the projection did not name", func(t *testing.T) {
		allowed := map[string]bool{
			"fromId": true, "toId": true, "type": true,
			"title": true, "status": true, "issueType": true, "priority": true,
			"redacted": true,
		}
		for key := range got {
			if !allowed[key] {
				t.Errorf("edge carries an unnamed key %q — the projection is an allowlist, and "+
					"a field that appears without a decision is how the ballast came back", key)
			}
		}
	})
}

// row 29 — SC-009's three budgets.
func TestProjectDepEdge_FitsSC009(t *testing.T) {
	t.Run("visible edge", func(t *testing.T) {
		encoded := marshalEdge(t, projectDepEdge(referenceEdge()))
		total := len([]rune(encoded))
		// As in SC-002, the other side's title is user text and is not
		// truncatable, so it is outside the budget and the threshold
		// shifts by its actual length.
		service := total - sc009ReferenceTitle

		t.Logf("SC-009 visible edge: %d chars total, %d service (budgets %d / %d): %s",
			total, service, sc009EdgeBudget, sc009ServiceBudget, encoded)
		if total > sc009EdgeBudget {
			t.Errorf("edge is %d characters, SC-009 allows %d", total, sc009EdgeBudget)
		}
		if service > sc009ServiceBudget {
			t.Errorf("service part is %d characters, SC-009 allows %d", service, sc009ServiceBudget)
		}
	})

	t.Run("edge to an invisible issue", func(t *testing.T) {
		encoded := marshalEdge(t, projectDepEdge(redactedEdge()))
		total := len([]rune(encoded))
		t.Logf("SC-009 redacted edge: %d chars (budget %d): %s", total, sc009RedactedBudget, encoded)
		if total > sc009RedactedBudget {
			t.Errorf("redacted edge is %d characters, SC-009 allows %d — it carries no title "+
				"at all, which is why its budget is tighter", total, sc009RedactedBudget)
		}
	})
}

// row 30 — the Unchanged half, and the reason it is a separate row.
func TestProjectDepEdge_RedactedStaysIdentifiable(t *testing.T) {
	got := projectDepEdge(redactedEdge())

	if !got.Redacted {
		t.Error("the redaction flag was dropped — it is the one thing telling the caller the " +
			"other side exists but is out of reach")
	}
	if got.FromID == "" || got.ToID == "" {
		t.Fatalf("a redacted edge lost a side id (from=%q to=%q). The literal reading of the "+
			"original task — «keep only the card's fields» — destroys exactly this: with no "+
			"card and no ids the edge becomes an anonymous object whose sole content is that "+
			"it is unreachable, and INV-1 is violated head-on", got.FromID, got.ToID)
	}
	if got.Type == "" {
		t.Error("a redacted edge lost its link type — the caller may not see the other side, " +
			"but it is entitled to know WHAT the link is")
	}

	// The missing card must not look like a missing link: no empty title,
	// no zero priority pretending to be data.
	if got.Title != "" {
		t.Errorf("title = %q on a redacted edge — the caller may not see it, and an empty "+
			"string would claim the other side has none", got.Title)
	}
	if got.Priority != nil {
		t.Errorf("priority = %v on a redacted edge — a zero would be indistinguishable from a "+
			"real P0", *got.Priority)
	}

	// And the edge is still an entry in the list, not an omission: a
	// caller counting edges must see it.
	edges := projectDepEdges([]dependencies.DependencyEdge{redactedEdge()})
	if len(edges) != 1 {
		t.Errorf("projecting one redacted edge produced %d entries — the absence of a card is not "+
			"read as \"there is no edge\"", len(edges))
	}
}

// TestProjectDepEdges_EmptyRendersAsArray: an empty direction is `[]`, not
// `null`. Null would read as "this direction was not computed", which is
// the INV-1 confusion the counters exist to avoid.
func TestProjectDepEdges_EmptyRendersAsArray(t *testing.T) {
	encoded := marshalEdge(t, projectDepEdges(nil))
	if encoded != "[]" {
		t.Errorf("empty edge list marshals as %s, want []", encoded)
	}
}

func marshalEdge(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestProjectDepEdge_DeepHierarchicalIdsCostTheServiceBudget REPORTS, and
// deliberately does not assert, what deep ids do to SC-009's service part.
//
// SC-009 shifts its threshold for the other side's title, and SC-002 does
// the same for the project identifier — both because they are text of
// unbounded length that no projection may truncate. Issue ids are the same
// kind of text (`sk-282.18.2.5` is thirteen characters against `demo-42`'s
// seven), and BOTH of them appear on every edge, but the spec fixes no shift
// for them: the ~117 the 130 budget was derived from is only reachable
// with short ids.
//
// Asserting a shifted budget here would be inventing a criterion the spec
// does not state, and asserting the flat 130 would fail the projection for
// the length of a fixture's ids. So the number is measured and logged, and
// the observation is carried upward instead.
func TestProjectDepEdge_DeepHierarchicalIdsCostTheServiceBudget(t *testing.T) {
	deep := referenceEdge()
	deep.FromID = "sk-282.18.2.5"
	deep.ToID = "sk-282.18.2"
	deep.Issue.ID = "sk-282.18.2"
	deep.Issue.Status = "in_progress" // the longest status value

	encoded := marshalEdge(t, projectDepEdge(deep))
	service := len([]rune(encoded)) - sc009ReferenceTitle

	short := marshalEdge(t, projectDepEdge(referenceEdge()))
	shortService := len([]rune(short)) - sc009ReferenceTitle

	t.Logf("SC-009 service part: %d on short ids + status \"in_progress\"; %d on the "+
		"composition the budget was derived from (budget %d). The difference is the two id "+
		"fields and the status value, none of which the projection may shorten.",
		service, shortService, sc009ServiceBudget)

	if service <= shortService {
		t.Errorf("deep ids did not cost anything (%d vs %d) — the fixture is not exercising "+
			"what it claims to", service, shortService)
	}
}
