package outbox

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/issues"
)

// TestBuildEnvelope_CommentPayloadSurvives — sk-201. The comment writers now
// store {"issueId": ...} in events.payload; this asserts the dispatcher hands
// it to subscribers intact rather than tripping rawIfPresent's empty-object
// filter. The frontend's live-invalidation router reads exactly
// envelope.payload.issueId, so this is the wire-contract half of the fix.
func TestBuildEnvelope_CommentPayloadSurvives(t *testing.T) {
	pid := uuid.Must(uuid.NewRandom())
	body, err := buildEnvelope(
		42, time.Now().UTC(), "comment.edited", "comment",
		"11111111-2222-3333-4444-555555555555",
		&pid,
		[]byte(`{"issueId": "sk-201"}`),
		nil,
		[]byte(`{"body": "edited"}`),
		EnvelopeEnrichment{},
	)
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}

	// Decode into the shape a subscriber actually sees, rather than
	// asserting on Envelope's Go struct — the wire JSON is the contract.
	var got struct {
		Payload struct {
			IssueID string `json:"issueId"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal envelope %s: %v", body, err)
	}
	if got.Payload.IssueID != "sk-201" {
		t.Errorf("envelope.payload.issueId = %q, want %q (envelope: %s)",
			got.Payload.IssueID, "sk-201", body)
	}
}

// TestBuildEnvelope_EmptyPayloadOmitted pins the inverse: writers that pass
// no payload store '{}', and that must stay off the wire (omitempty) so
// lifecycle events don't pay for a field nobody reads.
func TestBuildEnvelope_EmptyPayloadOmitted(t *testing.T) {
	for _, raw := range []string{"{}", "null", ""} {
		body, err := buildEnvelope(
			43, time.Now().UTC(), "issue.created", "issue", "sk-1",
			nil, []byte(raw), nil, nil, EnvelopeEnrichment{},
		)
		if err != nil {
			t.Fatalf("buildEnvelope(payload=%q): %v", raw, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal envelope %s: %v", body, err)
		}
		if _, present := got["payload"]; present {
			t.Errorf("payload=%q: envelope should omit the payload field, got %s", raw, body)
		}
	}
}

// TestEnvelope_RowCarriesNoReaderFields — sk-622.21, test matrix row 2
// (INV-14, the shape half; decisions K16 / K17). One event goes to readers
// with different visibility horizons, so the phase-2 row on the envelope must
// not carry the reader-dependent fields of the list row — `iAssigned`,
// `isBlocked`, `epicProgress` (spec US-15 / K17, written before this code) —
// and must carry the freshness order `rowSeq`. The row is decoded from the
// marshalled envelope, the shape a subscriber sees.
func TestEnvelope_RowCarriesNoReaderFields(t *testing.T) {
	pid := uuid.MustParse("0199a1b2-0000-7000-8000-0000000000a1")
	creator := uuid.MustParse("0199a1b2-0000-7000-8000-000000000001")
	assigneeID := uuid.MustParse("0199a1b2-0000-7000-8000-000000000002")
	parent := "sk-41"
	assignee := "bob"
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	// Every field populated: a reader field that leaked through an omitempty
	// tag would otherwise hide behind a zero value.
	rowJSON, err := json.Marshal(issues.NeutralRow{
		ID: "sk-42", ProjectID: pid, ParentID: &parent, Version: 7,
		Title: "Row title", Type: "epic", Priority: 1, Status: "in_progress",
		Pinned: true, Assignee: &assignee, AssigneeUserID: &assigneeID,
		CreatedBy: creator, ComponentIDs: []uuid.UUID{pid}, Labels: []string{"perf"},
		AcceptedAt: &now, UpdatedAt: now, CreatedAt: now,
		RowSeq: 9007,
	})
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	body, err := json.Marshal(Envelope{
		ID: 1, OccurredAt: now, EventType: "issue.updated", EntityType: "issue",
		EntityID: "sk-42", ProjectID: &pid, Row: rowJSON,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var env struct {
		Row map[string]json.RawMessage `json:"row"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope %s: %v", body, err)
	}
	if env.Row == nil {
		t.Fatalf("envelope has no row object: %s", body)
	}
	for _, readerField := range []string{"iAssigned", "isBlocked", "epicProgress"} {
		if v, present := env.Row[readerField]; present {
			t.Errorf("row carries the reader field %q = %s (INV-14)", readerField, v)
		}
	}
	var seq int64
	if err := json.Unmarshal(env.Row["rowSeq"], &seq); err != nil {
		t.Fatalf("row has no numeric rowSeq: %s", body)
	}
	if seq != 9007 {
		t.Errorf("row.rowSeq = %d, want 9007", seq)
	}
}
