package main

// holder_namespace_test.go — matrix row 44 (INV-10 of the lock holder).
//
// Reference — the spec, written before the code: R-5 — the lock holder of
// REST is an executor in the namespace of the key; holders of REST and MCP
// never coincide. An Mcp-Session-Id shaped like a REST executor
// (api:<keyId>:<workerId>) sent to /mcp therefore does not make the MCP
// session that executor: its claim gets a holder of the MCP namespace, and
// the REST executor of the same key does not count the lock as its own.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/agents"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

func TestMCPSessionIDCannotTakeTheRESTNamespace(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	p := s.f.AMembers
	actor := testsupport.ActorOnlyA
	u := s.f.User(t, actor)

	var keyID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1`,
		u.ID).Scan(&keyID); err != nil {
		t.Fatalf("the actor's key: %v", err)
	}
	restHolder := agents.RESTHolder(keyID, "w")

	var n int
	if err := s.pool.QueryRow(ctx, `UPDATE project_counters SET counter = counter + 1
 WHERE project_id = $1 RETURNING counter`, p.ID).Scan(&n); err != nil {
		t.Fatalf("next number: %v", err)
	}
	id := testsupport.SeedIssue(t, s.pool, p.ID, p.OwnerID, fmt.Sprintf("%s-%d", p.IDPrefix, n), "t016 namespace", "open", 2)

	r := s.tool(actor, restHolder, "skein_claim", map[string]any{"id": id})
	if r.isError {
		t.Fatalf("claim over /mcp with a REST-shaped session id: %s", r.text)
	}

	var lockedBy string
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT locked_by, version FROM issues WHERE id = $1`, id).
		Scan(&lockedBy, &version); err != nil {
		t.Fatalf("read the lock: %v", err)
	}
	if lockedBy == restHolder || agents.IsRESTHolder(lockedBy) {
		t.Fatalf("locked_by = %q: an MCP session took a holder of the REST namespace", lockedBy)
	}

	// The REST executor of the same key and worker id is someone else to
	// this lock: a write as that executor is refused as another holder's.
	title := "t016 written by the REST executor"
	_, err := issues.NewStore(s.pool).PatchIssue(ctx, id, issues.PatchIssueInput{
		ExpectedVersion: version, Title: &title, SessionID: restHolder,
	}, issues.Actor{UserID: u.ID, Username: u.Username, Role: "agent", SessionID: restHolder})
	var other *issues.ErrLockedByOther
	if !errors.As(err, &other) {
		t.Errorf("write as the REST executor %s = %v, want ErrLockedByOther", restHolder, err)
	}
}
