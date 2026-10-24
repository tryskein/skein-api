package tools

// attachment_actions_test.go — sk-674.18 T066 (sk-689.37), changed by
// sk-741: the MCP attachment half of «for every issue write path — a
// permission to a participant of the project and to an instance admin
// without membership» (the REST attachment routes:
// handler/attachments_actions_test.go; the other paths, both channels:
// cmd/api/issue_write_actions_test.go). Reference — the action table of US-6
// as changed by the owner 2026-10-17: attaching is writing issues, which the
// instance admin may do in a non-personal project with the owner's rights.
// Project A.members of testsupport.SeedTenancy; adm0 sees it without any
// membership, uA is its member.

import (
	"fmt"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/attachments"
	"skein-api/internal/testsupport"
)

func TestAttachmentTools_FollowTheActionTable(t *testing.T) {
	pool := newTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	p := f.AMembers
	deps := attachmentDeps{pool: pool, store: attachments.NewStore(pool), s3: &fakeS3{},
		bucket: "skein-attachments", maxSize: 1 << 30, urlTTL: time.Minute}
	n := 0
	fresh := func() string {
		n++
		id := fmt.Sprintf("%s-%d", p.IDPrefix, 8100+n)
		return testsupport.SeedIssue(t, pool, p.ID, p.OwnerID, id, "t066 "+id, "open", 2)
	}

	type path struct {
		name string
		run  func(a testsupport.Actor) *mcpgo.CallToolResult
	}
	run := func(a testsupport.Actor, h server.ToolHandlerFunc, args map[string]any) *mcpgo.CallToolResult {
		t.Helper()
		u := f.User(t, a)
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := h(withCtx(t, u.ID, u.Role), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		return res
	}
	paths := []path{
		{"skein_attachments upload_url", func(a testsupport.Actor) *mcpgo.CallToolResult {
			return run(a, handleAttachmentUploadURL(deps), map[string]any{
				"issueId": fresh(), "filename": "t066.txt", "contentType": "text/plain", "sizeBytes": 10})
		}},
		{"skein_attachments confirm (own upload)", func(a testsupport.Actor) *mcpgo.CallToolResult {
			id := testsupport.SeedAttachment(t, pool, fresh(), f.User(t, a).ID, "pending")
			return run(a, handleAttachmentConfirm(deps), map[string]any{"attachmentId": id.String()})
		}},
		{"skein_attachments delete", func(a testsupport.Actor) *mcpgo.CallToolResult {
			id := testsupport.SeedAttachment(t, pool, fresh(), p.OwnerID, "ready")
			return run(a, handleAttachmentDelete(deps), map[string]any{"attachmentId": id.String()})
		}},
	}
	// The table allows every path to both actors.
	for _, actor := range []testsupport.Actor{testsupport.ActorAdmin0, testsupport.ActorOnlyA} {
		for _, pth := range paths {
			t.Run(string(actor)+"/"+pth.name, func(t *testing.T) {
				res := pth.run(actor)
				if res.IsError {
					t.Errorf("the table allows it, got %v", extractToolJSON(t, res))
				}
			})
		}
	}
}
