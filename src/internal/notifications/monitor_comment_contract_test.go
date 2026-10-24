package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// monitor_comment_contract_test — sk-242.1 T060 (US-1/2/3, SC-001/003/004).
// Unit cases always run; DB-backed cases skip without TEST_DATABASE_URL
// (same gate as poll_for_monitor_test / fanout_integration_test).

// TestT060_UnitContracts re-asserts pure unit gates that T001/T012 own so
// T060's package acceptance stays green without a database.
func TestT060_UnitContracts(t *testing.T) {
	t.Run("INV-2 runes cap", func(t *testing.T) {
		if BodyPreviewMaxRunes != 1024 {
			t.Fatalf("BodyPreviewMaxRunes=%d want 1024", BodyPreviewMaxRunes)
		}
		in := strings.Repeat("я", BodyPreviewMaxRunes+5)
		got := truncateRunes(in, BodyPreviewMaxRunes)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) != BodyPreviewMaxRunes {
			t.Fatalf("truncate invalid or wrong length: valid=%v runes=%d",
				utf8.ValidString(got), utf8.RuneCountInString(got))
		}
	})

	t.Run("INV-5 human path no new keys", func(t *testing.T) {
		n := NotificationDTO{
			ID: 1, Type: "comment.created",
			Actor:  ActorDTO{Username: "u", Type: "human"},
			Entity: EntityDTO{Type: "issue", ID: "sk-1"}, Summary: "s",
		}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		for _, k := range []string{"commentId", "bodyPreview", "truncated"} {
			if strings.Contains(s, k) {
				t.Errorf("human DTO must not contain %q: %s", k, s)
			}
		}
	})

	t.Run("empty bodyPreview present", func(t *testing.T) {
		empty := ""
		n := NotificationDTO{ID: 1, Type: "comment.created", BodyPreview: &empty}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		v, ok := m["bodyPreview"]
		if !ok {
			t.Fatal("bodyPreview must be present for non-nil empty string")
		}
		if v != "" {
			t.Fatalf("bodyPreview=%v want \"\"", v)
		}
	})
}

// TestPollForMonitor_CommentEnrichment is the T060 integration suite:
// live short, live >1024 runes, soft-delete, corrupt entity_id + sibling,
// full body never on wire, ListInbox/ListForIssue without new keys.
func TestPollForMonitor_CommentEnrichment(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpce1"
	seedTestIssue(t, pool, projectID, caller, issueID, 2)
	const old = "now() - interval '1 minute'"

	// --- helpers ---
	insertCommentWithBody := func(t *testing.T, body string, deleted bool) (commentID uuid.UUID, eventID int64) {
		t.Helper()
		commentID = uuid.Must(uuid.NewRandom())
		var deletedAt any
		if deleted {
			deletedAt = time.Now().UTC()
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body, deleted_at)
VALUES ($1, $2, 'tester', $3, 'human', $4, $5)`,
			commentID, issueID, actor, body, deletedAt); err != nil {
			t.Fatalf("insert comment: %v", err)
		}
		eventID = insertEvent(t, pool, actor, projectID,
			"comment", commentID.String(), "comment.created", old, nil)
		return commentID, eventID
	}

	poll := func(t *testing.T) PollResult {
		t.Helper()
		sinceTS, sinceID := epochCursor()
		res, err := store.PollForMonitor(ctx, PollParams{
			UserID:       caller,
			ProjectID:    projectID,
			ScopeIssueID: issueID,
			EventTypes:   []string{"comment.created"},
			SinceTS:      sinceTS,
			SinceID:      sinceID,
			Limit:        50,
			SafetyLag:    5 * time.Second,
		})
		if err != nil {
			t.Fatalf("PollForMonitor: %v", err)
		}
		return res
	}

	find := func(res PollResult, eventID int64) (NotificationDTO, bool) {
		for _, it := range res.Items {
			if it.ID == eventID {
				return it, true
			}
		}
		return NotificationDTO{}, false
	}

	// --- US-1 live short ---
	t.Run("live short body → commentId + preview, truncated absent", func(t *testing.T) {
		cid, eid := insertCommentWithBody(t, "hello peer", false)
		res := poll(t)
		it, ok := find(res, eid)
		if !ok {
			t.Fatalf("event %d not delivered", eid)
		}
		if it.CommentID == nil || *it.CommentID != cid.String() {
			t.Errorf("commentId=%v want %s", ptrStr(it.CommentID), cid.String())
		}
		if it.BodyPreview == nil || *it.BodyPreview != "hello peer" {
			t.Errorf("bodyPreview=%v want hello peer", ptrStr(it.BodyPreview))
		}
		if it.Truncated != nil {
			t.Errorf("truncated must be nil for short body, got %v", *it.Truncated)
		}
		// INV-9: Entity re-points comment→issue
		if it.Entity.Type != "issue" || it.Entity.ID != issueID {
			t.Errorf("entity re-point: type=%s id=%s want issue/%s", it.Entity.Type, it.Entity.ID, issueID)
		}
		// INV-1: full body never as a separate field; preview is the body when short
		raw, _ := json.Marshal(it)
		if strings.Contains(string(raw), `"body":`) {
			t.Errorf("poll wire must not carry full body field: %s", raw)
		}
	})

	// --- US-1 live long ---
	t.Run("live body >1024 runes → truncated true, preview ≤1024", func(t *testing.T) {
		long := strings.Repeat("🚀", BodyPreviewMaxRunes+20)
		cid, eid := insertCommentWithBody(t, long, false)
		res := poll(t)
		it, ok := find(res, eid)
		if !ok {
			t.Fatalf("event %d not delivered", eid)
		}
		if it.CommentID == nil || *it.CommentID != cid.String() {
			t.Errorf("commentId=%v want %s", ptrStr(it.CommentID), cid.String())
		}
		if it.BodyPreview == nil {
			t.Fatal("bodyPreview must be present")
		}
		if utf8.RuneCountInString(*it.BodyPreview) != BodyPreviewMaxRunes {
			t.Errorf("preview runes=%d want %d", utf8.RuneCountInString(*it.BodyPreview), BodyPreviewMaxRunes)
		}
		if !utf8.ValidString(*it.BodyPreview) {
			t.Error("preview not valid UTF-8")
		}
		if it.Truncated == nil || !*it.Truncated {
			t.Errorf("truncated must be true, got %v", it.Truncated)
		}
		// INV-1: full body not on wire
		raw, _ := json.Marshal(it)
		if strings.Contains(string(raw), long) {
			t.Error("full long body must never appear on poll wire")
		}
	})

	// --- US-2 soft-delete ---
	t.Run("soft-deleted → commentId present, preview/truncated absent", func(t *testing.T) {
		cid, eid := insertCommentWithBody(t, "should not leak", true)
		res := poll(t)
		it, ok := find(res, eid)
		if !ok {
			t.Fatalf("event %d not delivered (soft-deleted still scopes via live row)", eid)
		}
		if it.CommentID == nil || *it.CommentID != cid.String() {
			t.Errorf("commentId=%v want %s", ptrStr(it.CommentID), cid.String())
		}
		if it.BodyPreview != nil {
			t.Errorf("bodyPreview must be nil on soft-delete, got %q", *it.BodyPreview)
		}
		if it.Truncated != nil {
			t.Errorf("truncated must be nil on soft-delete, got %v", *it.Truncated)
		}
		raw, _ := json.Marshal(it)
		if strings.Contains(string(raw), "should not leak") {
			t.Error("INV-4: deleted text must not appear on poll wire")
		}
		if strings.Contains(string(raw), "bodyPreview") || strings.Contains(string(raw), "truncated") {
			t.Errorf("soft-delete JSON must omit bodyPreview/truncated: %s", raw)
		}
	})

	// --- US-2 corrupt entity_id: page ok, sibling kept ---
	t.Run("corrupt entity_id → page ok, sibling valid kept", func(t *testing.T) {
		// Valid sibling first so scope join succeeds for at least one row.
		_, validEID := insertCommentWithBody(t, "sibling ok", false)
		// Corrupt: entity_type=comment but non-UUID entity_id. Must NOT abort
		// the page (non-throwing join). Scope needs live comment issue_id so
		// this row is typically excluded; sibling must still be delivered.
		// Delete the corrupt row after the subtest so human-path queries that
		// still use a CASE-only cast (no regex) cannot trip over it later.
		corruptEID := insertEvent(t, pool, actor, projectID,
			"comment", "not-a-uuid-at-all", "comment.created", old, nil)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, corruptEID)
		})

		res := poll(t)
		// Page returned without error (asserted by poll helper).
		if _, ok := find(res, validEID); !ok {
			t.Fatalf("valid sibling event %d must still be delivered", validEID)
		}
		// Corrupt must not crash; if present it is degraded (no fields).
		if it, ok := find(res, corruptEID); ok {
			if it.CommentID != nil || it.BodyPreview != nil || it.Truncated != nil {
				t.Errorf("corrupt item must be degraded (no enrichment), got commentId=%v preview=%v truncated=%v",
					ptrStr(it.CommentID), ptrStr(it.BodyPreview), it.Truncated)
			}
		}
	})

	// --- US-3 human path: ListInbox carries no monitor-only keys ---
	// Run AFTER corrupt subtest cleanup so human SQL (CASE-only cast, no
	// regex guard — out of G-Monitor scope) never sees the poison entity_id.
	//
	// The per-issue list this used to check alongside ListInbox is gone
	// (sk-303.13 / T017 removed the Outbox surface it existed for), so the
	// remaining human read path is the inbox.
	t.Run("ListInbox JSON no monitor keys", func(t *testing.T) {
		// Materialise a human inbox row for a comment.created event that has
		// a live body — so if hydrate ever leaked enrichment it would show.
		_, eid := insertCommentWithBody(t, "human path body", false)
		seedInboxRow(t, pool, caller, eid, "comment.created", projectID, false, false)

		inbox, err := store.ListInbox(ctx, ListInboxArgs{
			UserID: caller,
			Scope:  "all",
			Limit:  50,
		})
		if err != nil {
			t.Fatalf("ListInbox: %v", err)
		}
		if len(inbox.Items) == 0 {
			t.Fatal("ListInbox returned no items; expected the seeded notification")
		}
		for _, it := range inbox.Items {
			raw, _ := json.Marshal(it)
			s := string(raw)
			for _, k := range []string{"commentId", "bodyPreview", "truncated"} {
				if strings.Contains(s, k) {
					t.Errorf("ListInbox item must not contain %q: %s", k, s)
				}
			}
			// Pointer fields must stay nil on human path.
			if it.CommentID != nil || it.BodyPreview != nil || it.Truncated != nil {
				t.Errorf("ListInbox DTO populated monitor fields: commentId=%v", ptrStr(it.CommentID))
			}
		}
	})
}
