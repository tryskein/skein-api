package tools

import (
	"maps"
	"slices"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// A mistyped assignee must refuse the read rather than silently broaden it.
// The same fixture also preserves each tool's existing string/null contract.
func TestReadAssigneeFilter(t *testing.T) {
	for _, surface := range []string{"list", "cross-project", "ready"} {
		t.Run(surface, func(t *testing.T) {
			pool := newTestPool(t)
			owner, ownerName := seedUser(t, pool, "user")
			_, otherName := seedUser(t, pool, "user")
			pid, prefix := seedProject(t, pool, owner, "asf")
			otherPID, otherPrefix := seedProject(t, pool, owner, "asfx")
			mine, theirs, unassigned := prefix+"-1", prefix+"-2", prefix+"-3"
			crossMine := otherPrefix + "-1"
			seedListIssueWithFields(t, pool, pid, owner, mine, "human", ownerName, nil)
			seedListIssueWithFields(t, pool, pid, owner, theirs, "human", otherName, nil)
			seedListIssueWithFields(t, pool, pid, owner, unassigned, "human", "", nil)
			seedListIssueWithFields(t, pool, otherPID, owner, crossMine, "human", ownerName, nil)

			args := map[string]any{"project": pid.String()}
			allIDs, mineIDs := []string{mine, theirs, unassigned}, []string{mine}
			var handler server.ToolHandlerFunc
			if surface == "ready" {
				handler = handleReady(pool, dependencies.NewStore(pool), projects.NewStore(pool))
			} else {
				handler = handleList(pool, issues.NewStore(pool), projects.NewStore(pool), nil)
				if surface == "cross-project" {
					args = map[string]any{"crossProject": true}
					allIDs, mineIDs = append(allIDs, crossMine), append(mineIDs, crossMine)
				}
			}
			meIDs := mineIDs
			if surface == "ready" {
				// Ready accepts an exact assignee, without list's special "me" alias.
				meIDs = nil
			}
			for _, tc := range []struct {
				name    string
				present bool
				value   any
				wantIDs []string
			}{
				{"missing", false, nil, allIDs},
				{"null", true, nil, allIDs},
				{"empty", true, "", allIDs},
				{"blank", true, " \t\n ", allIDs},
				{"username", true, ownerName, mineIDs},
				{"trimmed-username", true, " " + otherName + " ", []string{theirs}},
				{"me", true, " me ", meIDs},
				{"unmatched", true, "unknown-" + prefix, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					callArgs := maps.Clone(args)
					if tc.present {
						callArgs["assignee"] = tc.value
					}
					req := mcpgo.CallToolRequest{}
					req.Params.Arguments = callArgs
					res, err := handler(withCtx(t, owner, "user"), req)
					if err != nil {
						t.Fatalf("handler error: %v", err)
					}
					body := extractToolJSON(t, res)
					if res.IsError {
						t.Fatalf("unexpected tool error: %v", body)
					}
					got, want := listItemIDs(t, body), slices.Clone(tc.wantIDs)
					slices.Sort(got)
					slices.Sort(want)
					if !slices.Equal(got, want) {
						t.Fatalf("issue IDs = %v, want %v", got, want)
					}
				})
			}
			for _, tc := range []struct {
				name  string
				value any
			}{
				{"number", float64(42)},
				{"zero", float64(0)},
				{"array", []any{ownerName}},
				{"empty-array", []any{}},
				{"object", map[string]any{"username": ownerName}},
				{"empty-object", map[string]any{}},
				{"true", true},
				{"false", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					callArgs := maps.Clone(args)
					callArgs["assignee"] = tc.value
					req := mcpgo.CallToolRequest{}
					req.Params.Arguments = callArgs
					res, err := handler(withCtx(t, owner, "user"), req)
					if err != nil {
						t.Fatalf("handler error: %v", err)
					}
					body := extractToolJSON(t, res)
					if !res.IsError || body["error"] != "validation_error" {
						t.Fatalf("got error=%v with items=%v, want validation_error", body["error"], body["items"])
					}
					if body["code"] != "assignee_not_string" {
						t.Fatalf("error code = %v, want assignee_not_string", body["code"])
					}
					if _, present := body["items"]; present {
						t.Fatal("refusal contains an issue list")
					}
				})
			}
		})
	}
}
