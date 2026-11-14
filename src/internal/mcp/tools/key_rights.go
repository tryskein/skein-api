package tools

import (
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/authz"
)

// MustCoverKeyRights panics when a registered tool, or an action in the
// `action` enum of a registered consolidated tool, has no row in the
// key-rights matrix (authz.MCPToolRequirement).
//
// The middleware mcp.KeyRights refuses a tool without a row to every key and
// lets an action outside the matrix through to the dispatcher (which refuses
// an action outside its own enum), so a registered action without a row
// would run with no key-rights check at all. Called once, after the whole
// surface is registered.
//
// Panic, not an error return, for the same reason as actionSet.with: it runs
// at process start inside the registration closure, which CI executes
// without a database (TestSearchHints_EveryTool).
func MustCoverKeyRights(s *server.MCPServer) {
	tools := s.ListTools()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		actions := toolActionEnum(tools[name])
		if len(actions) == 0 {
			if _, ok := authz.MCPToolRequirement(name, "", false); !ok {
				panic(fmt.Sprintf("key rights: tool %q has no row in authz.MCPToolRequirement", name))
			}
			continue
		}
		for _, action := range actions {
			if _, ok := authz.MCPToolRequirement(name, action, false); !ok {
				panic(fmt.Sprintf("key rights: action %q of tool %q has no row in authz.MCPToolRequirement", action, name))
			}
		}
	}
}

// toolActionEnum reads the `action` enum of a tool's input schema; nil for a
// tool without an action parameter.
func toolActionEnum(t *server.ServerTool) []string {
	prop, ok := t.Tool.InputSchema.Properties[actionParam].(map[string]any)
	if !ok {
		return nil
	}
	switch enum := prop["enum"].(type) {
	case []string:
		return enum
	case []any:
		out := make([]string, 0, len(enum))
		for _, v := range enum {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
