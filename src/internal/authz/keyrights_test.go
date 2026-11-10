package authz_test

import (
	"testing"

	"skein-api/internal/apikeys"
	"skein-api/internal/authz"
)

// The expected rights below are literals copied from the spec (section
// "MCP changes" and the closed rights matrix of the API surface), not from
// authz: a row changed in the matrix without the spec changing turns red.
//
// Keys of every shape the grant model allows: one per level, the admin-only
// key, and manage+admin.
var (
	keyRead        = apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
	keyWrite       = apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
	keyManage      = apikeys.Grant{Level: apikeys.LevelManage, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
	keyAdminOnly   = apikeys.Grant{Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
	keyManageAdmin = apikeys.Grant{Level: apikeys.LevelManage, Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
)

// allowed is the expected outcome per key, in the order read, write, manage,
// admin-only, manage+admin.
type allowed [5]bool

var (
	needNone   = allowed{true, true, true, true, true}
	needRead   = allowed{true, true, true, false, true}
	needWrite  = allowed{false, true, true, false, true}
	needManage = allowed{false, false, true, false, true}
	needAdmin  = allowed{false, false, false, true, true}
)

func TestMCPToolRequirement_Matrix(t *testing.T) {
	keys := [5]apikeys.Grant{keyRead, keyWrite, keyManage, keyAdminOnly, keyManageAdmin}
	keyNames := [5]string{"read", "write", "manage", "admin-only", "manage+admin"}

	cases := []struct {
		tool, action string
		force        bool
		required     string
		want         allowed
	}{
		// No right: any key, the admin-only key included.
		{"skein_get_instructions", "", false, "none", needNone},
		{"skein_whoami", "", false, "none", needNone},

		// MCP-only tools named by the spec.
		{"skein_prime", "", false, "read", needRead},
		{"skein_get_monitor_script", "", false, "read", needRead},
		{"skein_watchers", "monitor_stop", false, "read", needRead},
		{"skein_theme", "instructions", false, "read", needRead},
		{"skein_theme", "describe", false, "read", needRead},
		{"skein_theme", "fork", false, "write", needWrite},
		{"skein_theme", "preview_variables", false, "write", needWrite},
		{"skein_theme", "set_variables", false, "write", needWrite},
		{"skein_theme", "inspect_clear", false, "write", needWrite},

		// Reads.
		{"skein_project_list", "", false, "read", needRead},
		{"skein_project_resolve", "", false, "read", needRead},
		{"skein_list", "", false, "read", needRead},
		{"skein_ready", "", false, "read", needRead},
		{"skein_show", "", false, "read", needRead},
		{"skein_search", "", false, "read", needRead},
		{"skein_comment_get", "", false, "read", needRead},
		{"skein_move", "", false, "read", needRead},
		{"skein_project_members", "list", false, "read", needRead},
		{"skein_component", "list", false, "read", needRead},
		{"skein_attachments", "list", false, "read", needRead},
		{"skein_attachments", "metadata", false, "read", needRead},
		{"skein_attachments", "get_url", false, "read", needRead},
		{"skein_notifications", "inbox", false, "read", needRead},
		{"skein_watchers", "list", false, "read", needRead},
		{"skein_watchers", "filter_list", false, "read", needRead},
		{"skein_remind", "list", false, "read", needRead},

		// Writes, and force=true on the seven guarded tools raising to manage.
		{"skein_update", "", false, "write", needWrite},
		{"skein_update", "", true, "manage", needManage},
		{"skein_close", "", false, "write", needWrite},
		{"skein_close", "", true, "manage", needManage},
		{"skein_unclaim", "", false, "write", needWrite},
		{"skein_unclaim", "", true, "manage", needManage},
		{"skein_commits_attach", "", false, "write", needWrite},
		{"skein_commits_attach", "", true, "manage", needManage},
		{"skein_commits_detach", "", false, "write", needWrite},
		{"skein_commits_detach", "", true, "manage", needManage},
		{"skein_update_many", "", false, "write", needWrite},
		{"skein_update_many", "", true, "manage", needManage},
		{"skein_close_many", "", false, "write", needWrite},
		{"skein_close_many", "", true, "manage", needManage},
		// force on create only bypasses the duplicate gate: still write.
		{"skein_create", "", false, "write", needWrite},
		{"skein_create", "", true, "write", needWrite},
		{"skein_epic_create", "", false, "write", needWrite},
		{"skein_claim", "", false, "write", needWrite},
		{"skein_accept", "", false, "write", needWrite},
		{"skein_reparent", "", false, "write", needWrite},
		{"skein_dep_add", "", false, "write", needWrite},
		{"skein_dep_add_many", "", false, "write", needWrite},
		{"skein_dep_remove", "", false, "write", needWrite},
		{"skein_drop_delegated", "", false, "write", needWrite},
		{"skein_resume_delegated", "", false, "write", needWrite},
		{"skein_comment", "", false, "write", needWrite},
		{"skein_comment_update", "", false, "write", needWrite},
		{"skein_comment_delete", "", false, "write", needWrite},
		{"skein_attachments", "upload_url", false, "write", needWrite},
		{"skein_attachments", "confirm", false, "write", needWrite},
		{"skein_attachments", "delete", false, "write", needWrite},
		{"skein_notifications", "mark_read", false, "write", needWrite},
		{"skein_notifications", "mark_read_issue", false, "write", needWrite},
		{"skein_notifications", "mark_read_card", false, "write", needWrite},
		{"skein_notifications", "mark_read_all", false, "write", needWrite},
		{"skein_notifications", "archive", false, "write", needWrite},
		{"skein_watchers", "watch", false, "write", needWrite},
		{"skein_watchers", "unwatch", false, "write", needWrite},
		{"skein_watchers", "project_watch", false, "write", needWrite},
		{"skein_watchers", "project_unwatch", false, "write", needWrite},
		{"skein_watchers", "filter_create", false, "write", needWrite},
		{"skein_watchers", "filter_delete", false, "write", needWrite},
		{"skein_remind", "set", false, "write", needWrite},
		{"skein_remind", "snooze", false, "write", needWrite},
		{"skein_remind", "dismiss", false, "write", needWrite},

		// Project structure, components, members.
		{"skein_project_create", "", false, "manage", needManage},
		{"skein_project_update", "", false, "manage", needManage},
		{"skein_project_members", "add", false, "manage", needManage},
		{"skein_project_members", "remove", false, "manage", needManage},
		{"skein_component", "create", false, "manage", needManage},
		{"skein_component", "update", false, "manage", needManage},
		{"skein_component", "archive", false, "manage", needManage},

		// Users: admin, and the admin-only key may call them.
		{"skein_user", "list", false, "admin", needAdmin},
		{"skein_user", "create", false, "admin", needAdmin},
		{"skein_user", "update", false, "admin", needAdmin},
		{"skein_user", "space_add", false, "admin", needAdmin},
	}
	for _, tc := range cases {
		name := tc.tool
		if tc.action != "" {
			name += "." + tc.action
		}
		if tc.force {
			name += "+force"
		}
		t.Run(name, func(t *testing.T) {
			req, ok := authz.MCPToolRequirement(tc.tool, tc.action, tc.force)
			if !ok {
				t.Fatalf("no row in the matrix")
			}
			if got := req.Right.String(); got != tc.required {
				t.Errorf("required right = %q, want %q", got, tc.required)
			}
			for i, g := range keys {
				if got := authz.KeyMeets(g, req); got != tc.want[i] {
					t.Errorf("key %s: allowed = %v, want %v", keyNames[i], got, tc.want[i])
				}
			}
		})
	}
}

// TestMCPToolRequirement_OutsideTheMatrix: a tool or an action the matrix
// does not name has no requirement, which the MCP middleware turns into a
// refusal for every key.
func TestMCPToolRequirement_OutsideTheMatrix(t *testing.T) {
	for _, tc := range []struct{ tool, action string }{
		{"skein_unknown_tool", ""},
		{"skein_watchers", "no_such_action"},
		{"skein_user", ""},
		{"skein_update", "list"},
	} {
		if _, ok := authz.MCPToolRequirement(tc.tool, tc.action, false); ok {
			t.Errorf("%s/%q: has a requirement, want none", tc.tool, tc.action)
		}
	}
}

// TestKeyMeets_CreateProjectNeedsAWideScope: creating a project needs manage
// and a scope of spaces or all; a key limited to listed projects is refused
// even with manage.
func TestKeyMeets_CreateProjectNeedsAWideScope(t *testing.T) {
	req, ok := authz.MCPToolRequirement("skein_project_create", "", false)
	if !ok {
		t.Fatal("skein_project_create has no row")
	}
	for _, tc := range []struct {
		name  string
		scope apikeys.Scope
		want  bool
	}{
		{"projects", apikeys.Scope{Mode: apikeys.ScopeProjects}, false},
		{"spaces", apikeys.Scope{Mode: apikeys.ScopeSpaces}, true},
		{"all", apikeys.Scope{Mode: apikeys.ScopeAll}, true},
	} {
		g := apikeys.Grant{Level: apikeys.LevelManage, Scope: tc.scope}
		if got := authz.KeyMeets(g, req); got != tc.want {
			t.Errorf("manage key with scope %s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
