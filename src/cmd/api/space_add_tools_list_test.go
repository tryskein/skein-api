package main

// space_add_tools_list_test.go — sk-674.18 T024, matrix row 98 (INV-17),
// the tools/list half; the admin gate of space_add lives in
// internal/mcp/tools/space_add_test.go. Reference — INV-17: through MCP one
// cannot invite to a space, accept or decline an invitation. The production
// wiring is asked for its tools: no tool and no action of a tool speaks of
// invitations, and the only space-membership action is skein_user space_add.

import (
	"net/http"
	"strings"
	"testing"

	"skein-api/internal/testsupport"
)

func TestSpaceInvitations_NotInToolsList(t *testing.T) {
	s := newParityStack(t)
	for _, a := range []testsupport.Actor{testsupport.ActorAdmin0, testsupport.ActorOnlyA} {
		t.Run(string(a), func(t *testing.T) {
			sid := s.session(a)
			code, out, _ := s.mcp(a, sid, "tools/list", map[string]any{})
			if code != http.StatusOK || out["error"] != nil {
				t.Fatalf("tools/list: %d %v", code, out)
			}
			result, _ := out["result"].(map[string]any)
			list, _ := result["tools"].([]any)
			if len(list) == 0 {
				t.Fatalf("tools/list is empty: %v", out)
			}
			var spaceActions []string
			for _, raw := range list {
				tool, _ := raw.(map[string]any)
				name, _ := tool["name"].(string)
				if strings.Contains(strings.ToLower(name), "invit") {
					t.Errorf("tool %s is an invitation tool", name)
				}
				schema, _ := tool["inputSchema"].(map[string]any)
				props, _ := schema["properties"].(map[string]any)
				action, _ := props["action"].(map[string]any)
				enum, _ := action["enum"].([]any)
				for _, v := range enum {
					act, _ := v.(string)
					if strings.Contains(strings.ToLower(act), "invit") {
						t.Errorf("tool %s has the invitation action %q", name, act)
					}
					if strings.HasPrefix(act, "space_") {
						spaceActions = append(spaceActions, name+"."+act)
					}
				}
			}
			if len(spaceActions) != 1 || spaceActions[0] != "skein_user.space_add" {
				t.Errorf("space-membership actions %v, want only skein_user.space_add", spaceActions)
			}
		})
	}
}
