package tools

// inbox_invitation_test.go — sk-674.18, the test delivery of row 131 (T029,
// US-5 "Agent", INV-17). Reference — the maintainers' decision 2026-10-12 as the
// spec words it, written before the code: an agent sees the invitation in
// its Inbox through MCP, WITHOUT the token and the link, and in the number
// `inbox`; accepting or declining it is only a person's act in the web — the
// MCP accept action on an invitation is refused with a code.
//
// The invitation carries a live token (auth_tokens, current_token_id), so a
// leak of it — its id, its hash, an /invite/ link — would show in the row.
// The row's key set is the contract's (spaces.PendingInvitation): who
// invites to which space and until when, nothing else.
//
// Red if the Inbox does not show the invitation, shows any other key (the
// token, the link, the e-mail), or skein_accept on it does anything but
// refuse invitation_agent_forbidden and leave it pending.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

func TestInboxInvitation_ShownWithoutTokenAcceptRefused(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	agentUser, _ := seedUser(t, pool, "user")
	inviter, inviterName := seedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, inviter)
	var spaceShort, email string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&spaceShort); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, agentUser).Scan(&email); err != nil {
		t.Fatal(err)
	}

	var invitation, token uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, status, lang, expires_at)
VALUES ($1, lower(btrim($2)), $3, 'pending', 'ru', now() + interval '7 days') RETURNING id`,
		space, email, inviter).Scan(&invitation); err != nil {
		t.Fatalf("seed invitation: %v", err)
	}
	hash := sha256.Sum256([]byte("inbox-invitation-" + invitation.String()))
	if err := pool.QueryRow(ctx, `
INSERT INTO auth_tokens (purpose, token_hash, invitation_id, expires_at)
VALUES ('space_invite', $1, $2, now() + interval '7 days') RETURNING id`, hash[:], invitation).Scan(&token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE space_invitations SET current_token_id = $2 WHERE id = $1`, invitation, token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `UPDATE space_invitations SET current_token_id = NULL WHERE id = $1`, invitation)
		_, _ = pool.Exec(c, `DELETE FROM auth_tokens WHERE id = $1`, token)
		_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, invitation)
	})
	ctxAgent := withCtx(t, agentUser, "user")

	t.Run("inbox shows the invitation without the token", func(t *testing.T) {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"scope": "all"}
		res, err := handleInbox(notifications.NewStore(pool))(ctxAgent, req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if res.IsError {
			t.Fatalf("inbox error: %+v", extractToolJSON(t, res))
		}
		body := extractToolJSON(t, res)
		invs, _ := body["invitations"].([]any)
		var row map[string]any
		for _, raw := range invs {
			if m, ok := raw.(map[string]any); ok && m["id"] == invitation.String() {
				row = m
			}
		}
		if row == nil {
			t.Fatalf("the pending invitation is not in the agent's Inbox: %v", body["invitations"])
		}
		var keys []string
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := []string{"expiresAt", "id", "invitedBy", "spaceId", "spaceName"}
		if !slices.Equal(keys, want) {
			t.Errorf("invitation row keys %v, want exactly %v", keys, want)
		}
		if row["spaceId"] != spaceShort || row["invitedBy"] != inviterName {
			t.Errorf("row %v, want spaceId %s and invitedBy %s", row, spaceShort, inviterName)
		}
		raw := strings.ToLower(resultText(t, res))
		for _, leak := range []string{token.String(), hex.EncodeToString(hash[:]), "/invite/", "token"} {
			if strings.Contains(raw, strings.ToLower(leak)) {
				t.Errorf("the Inbox answer carries %q", leak)
			}
		}
	})

	t.Run("skein_accept on the invitation is refused with a code", func(t *testing.T) {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"id": invitation.String()}
		res, err := handleAccept(pool, issues.NewStore(pool))(ctxAgent, req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if !res.IsError {
			t.Fatalf("accepting an invitation through MCP succeeded: %+v", extractToolJSON(t, res))
		}
		if code := extractToolJSON(t, res)["error"]; code != ErrCodeInvitationAgentForbidden {
			t.Errorf("error = %v, want %s", code, ErrCodeInvitationAgentForbidden)
		}
		var status string
		var member bool
		if err := pool.QueryRow(ctx, `
SELECT si.status, EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = si.space_id AND sm.user_id = $2)
  FROM space_invitations si WHERE si.id = $1`, invitation, agentUser).Scan(&status, &member); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || member {
			t.Errorf("after the refused accept: status %q, participant %v; want pending and not a participant", status, member)
		}
	})
}
