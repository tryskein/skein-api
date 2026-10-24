package tools

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestIntegrationConcurrency_ClaimRace fires two skein_claim calls at
// the same issue from two distinct sessions of the same user. Exactly
// one should succeed; the other must surface `already_claimed`. The
// store wraps the predicate in a SERIALIZABLE transaction so the loser
// sees a deterministic error, never a Postgres serialization-failure
// leak.
//
// Run with `-race -count=10` to flake-flush.
func TestIntegrationConcurrency_ClaimRace(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "iccr")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "race", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)

	ctxA := withCtxSession(t, userID, "admin", "sess-A-race")
	ctxB := withCtxSession(t, userID, "admin", "sess-B-race")

	var (
		wg           sync.WaitGroup
		successCount int32
		alreadyCount int32
		otherErr     atomic.Value // string
	)

	// Two goroutines firing concurrently. Don't add real synchronisation
	// — the SERIALIZABLE store IS the synchronisation point.
	wg.Add(2)
	for _, ctx := range []context.Context{ctxA, ctxB} {
		ctx := ctx
		go func() {
			defer wg.Done()
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"id": issueID}
			res, err := handler(ctx, req)
			if err != nil {
				otherErr.Store("handler error: " + err.Error())
				return
			}
			if res.IsError {
				body := extractToolJSON(t, res)
				if got, _ := body["error"].(string); got == ErrCodeAlreadyClaimed {
					atomic.AddInt32(&alreadyCount, 1)
				} else {
					otherErr.Store("unexpected error code: " + got)
				}
				return
			}
			atomic.AddInt32(&successCount, 1)
		}()
	}
	wg.Wait()

	if v := otherErr.Load(); v != nil {
		t.Fatalf("unexpected: %v", v)
	}
	if atomic.LoadInt32(&successCount) != 1 {
		t.Fatalf("expected exactly 1 success, got %d", successCount)
	}
	if atomic.LoadInt32(&alreadyCount) != 1 {
		t.Fatalf("expected exactly 1 already_claimed, got %d", alreadyCount)
	}
}

// TestIntegrationConcurrency_UpdateRace fires two skein_update calls at
// the same free issue from two sessions; one should succeed, the other
// should hit version_conflict (since neither session holds the lock —
// no lock predicate fails — but optimistic concurrency catches the loser).
func TestIntegrationConcurrency_UpdateRace(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "icur")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "race-update", "open", 2)

	store := issues.NewStore(pool)
	handler := handleUpdate(pool, store)

	ctxA := withCtxSession(t, userID, "admin", "sess-upd-A")
	ctxB := withCtxSession(t, userID, "admin", "sess-upd-B")

	var (
		wg           sync.WaitGroup
		successCount int32
		conflictCnt  int32
		otherErr     atomic.Value
	)

	wg.Add(2)
	for _, ctx := range []context.Context{ctxA, ctxB} {
		ctx := ctx
		go func() {
			defer wg.Done()
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"id":              issueID,
				"expectedVersion": 1,
				"title":           "concurrent",
			}
			res, err := handler(ctx, req)
			if err != nil {
				otherErr.Store("handler error: " + err.Error())
				return
			}
			if res.IsError {
				body := extractToolJSON(t, res)
				if got, _ := body["error"].(string); got == ErrCodeVersionConflict {
					atomic.AddInt32(&conflictCnt, 1)
				} else {
					otherErr.Store("unexpected error: " + got)
				}
				return
			}
			atomic.AddInt32(&successCount, 1)
		}()
	}
	wg.Wait()

	if v := otherErr.Load(); v != nil {
		t.Fatalf("unexpected: %v", v)
	}
	if atomic.LoadInt32(&successCount) != 1 {
		t.Fatalf("expected exactly 1 success, got %d", successCount)
	}
	if atomic.LoadInt32(&conflictCnt) != 1 {
		t.Fatalf("expected exactly 1 version_conflict, got %d", conflictCnt)
	}
}

// TestIntegrationConcurrency_ForceOverrideAuditEvent verifies that a
// force-override path writes an audit event whose reason field contains
// "force-overridden". This is the trail SREs follow when they see "why
// did demo-42 close while sess-X was holding the lock".
func TestIntegrationConcurrency_ForceOverrideAuditEvent(t *testing.T) {
	pool := newTestPool(t)
	admin, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, admin, "icfoae")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, admin, issueID, "audit", "open", 2)

	store := issues.NewStore(pool)
	claimHandler := handleClaim(pool, store)
	closeHandler := handleClose(pool, store)

	// sess-A holds the lock.
	ctxA := withCtxSession(t, admin, "admin", "sess-stuck")
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctxA, cl); err != nil || r.IsError {
		t.Fatalf("seed claim: %v %+v", err, r)
	}

	// sess-B (admin, force=true) closes.
	ctxB := withCtxSession(t, admin, "admin", "sess-B-force")
	clo := mcpgo.CallToolRequest{}
	clo.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 2,
		"closeOutcome":    "done",
		"reason":          "stuck-session",
		"force":           true,
	}
	res, err := closeHandler(ctxB, clo)
	if err != nil || res.IsError {
		t.Fatalf("admin force close failed: %v %+v", err, res)
	}

	// Verify the issue.closed event reason contains "force-overridden".
	var closedReason string
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(reason, '') FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.closed'
 ORDER BY id DESC LIMIT 1`,
		issueID).Scan(&closedReason); err != nil {
		t.Fatalf("query closed event: %v", err)
	}
	if !strings.Contains(closedReason, "force-overridden") {
		t.Fatalf("issue.closed reason = %q, want substring \"force-overridden\"", closedReason)
	}

	// Verify the issue.unclaimed event reason is "force-overridden".
	var unclaimReason string
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(reason, '') FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.unclaimed'
 ORDER BY id DESC LIMIT 1`,
		issueID).Scan(&unclaimReason); err != nil {
		t.Fatalf("query unclaimed event: %v", err)
	}
	if unclaimReason != "force-overridden" {
		t.Fatalf("issue.unclaimed reason = %q, want \"force-overridden\"", unclaimReason)
	}
}
