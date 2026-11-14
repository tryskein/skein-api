package authz

import (
	"skein-api/internal/apikeys"
)

// KeyRight is the right of an API key an operation requires. The levels are
// ordered — read ⊂ write ⊂ manage — and admin is an independent flag: a key
// may hold admin without any level. KeyRightNone is an operation every key
// may call.
//
// The key right is the first of the checks of a call by key and comes before
// any object is looked up, so a refusal never reveals whether the object
// exists. The role of the key's owner in the object's project (authorship,
// delete_any_comment, force_unlock, the action table) is checked afterwards
// by the operation itself, inside the key's scope.
type KeyRight uint8

// The key rights.
const (
	KeyRightNone KeyRight = iota
	KeyRightRead
	KeyRightWrite
	KeyRightManage
	KeyRightAdmin
)

// String is the name of the right as API clients see it in a refusal.
func (r KeyRight) String() string {
	switch r {
	case KeyRightNone:
		return "none"
	case KeyRightRead:
		return "read"
	case KeyRightWrite:
		return "write"
	case KeyRightManage:
		return "manage"
	case KeyRightAdmin:
		return "admin"
	}
	return "unknown"
}

// KeyRequirement is what one call requires of the key: a right and, for
// creating a project, a scope wider than a list of projects.
type KeyRequirement struct {
	Right KeyRight
	// WideScope — the key's scope must be "spaces" or "all": a key limited
	// to listed projects cannot create a project.
	WideScope bool
}

// KeyMeets reports whether a key with grant g may make a call with
// requirement req. The admin flag of g must already be the effective one (the
// owner is an instance admin now — auth.RequestGrant).
func KeyMeets(g apikeys.Grant, req KeyRequirement) bool {
	if req.WideScope && g.Scope.Mode != apikeys.ScopeSpaces && g.Scope.Mode != apikeys.ScopeAll {
		return false
	}
	switch req.Right {
	case KeyRightNone:
		return true
	case KeyRightAdmin:
		return g.Admin
	case KeyRightRead:
		return levelRank(g.Level) >= 1
	case KeyRightWrite:
		return levelRank(g.Level) >= 2
	case KeyRightManage:
		return levelRank(g.Level) >= 3
	}
	return false
}

// levelRank orders the levels of a key: no level, read, write, manage. A
// value outside the closed set ranks below every level, so it meets nothing
// but KeyRightNone.
func levelRank(l apikeys.Level) int {
	switch l {
	case apikeys.LevelRead:
		return 1
	case apikeys.LevelWrite:
		return 2
	case apikeys.LevelManage:
		return 3
	}
	return 0
}

// toolRight is one row of the MCP matrix: the right of an ordinary call and
// whether force=true raises it to manage (the privileged mode of a guarded
// operation: bypass another holder's claim, the version check and the pin in
// one call). The owner's force_unlock role is checked by the tool itself.
type toolRight struct {
	right   KeyRight
	guarded bool
	wide    bool
}

// toolKey addresses a row: the tool and, for a consolidated tool, its action.
type toolKey struct {
	tool, action string
}

// mcpKeyRights is the closed key-rights matrix of the MCP tools, including
// the tools the public REST API does not carry. A tool or action outside it
// is refused to every key.
//
// force on skein_create is not a privilege — it only bypasses the duplicate
// gate — so the tool is not guarded.
var mcpKeyRights = map[toolKey]toolRight{
	{"skein_get_instructions", ""}: {right: KeyRightNone},
	{"skein_whoami", ""}:           {right: KeyRightNone},

	{"skein_prime", ""}:              {right: KeyRightRead},
	{"skein_get_monitor_script", ""}: {right: KeyRightRead},
	{"skein_project_list", ""}:       {right: KeyRightRead},
	{"skein_project_resolve", ""}:    {right: KeyRightRead},
	{"skein_list", ""}:               {right: KeyRightRead},
	{"skein_ready", ""}:              {right: KeyRightRead},
	{"skein_show", ""}:               {right: KeyRightRead},
	{"skein_search", ""}:             {right: KeyRightRead},
	{"skein_comment_get", ""}:        {right: KeyRightRead},
	{"skein_move", ""}:               {right: KeyRightRead},

	{"skein_create", ""}:           {right: KeyRightWrite},
	{"skein_epic_create", ""}:      {right: KeyRightWrite},
	{"skein_update", ""}:           {right: KeyRightWrite, guarded: true},
	{"skein_update_many", ""}:      {right: KeyRightWrite, guarded: true},
	{"skein_close", ""}:            {right: KeyRightWrite, guarded: true},
	{"skein_close_many", ""}:       {right: KeyRightWrite, guarded: true},
	{"skein_unclaim", ""}:          {right: KeyRightWrite, guarded: true},
	{"skein_commits_attach", ""}:   {right: KeyRightWrite, guarded: true},
	{"skein_commits_detach", ""}:   {right: KeyRightWrite, guarded: true},
	{"skein_claim", ""}:            {right: KeyRightWrite},
	{"skein_accept", ""}:           {right: KeyRightWrite},
	{"skein_reparent", ""}:         {right: KeyRightWrite},
	{"skein_dep_add", ""}:          {right: KeyRightWrite},
	{"skein_dep_add_many", ""}:     {right: KeyRightWrite},
	{"skein_dep_remove", ""}:       {right: KeyRightWrite},
	{"skein_drop_delegated", ""}:   {right: KeyRightWrite},
	{"skein_resume_delegated", ""}: {right: KeyRightWrite},
	{"skein_comment", ""}:          {right: KeyRightWrite},
	{"skein_comment_update", ""}:   {right: KeyRightWrite},
	{"skein_comment_delete", ""}:   {right: KeyRightWrite},

	{"skein_project_create", ""}: {right: KeyRightManage, wide: true},
	{"skein_project_update", ""}: {right: KeyRightManage},

	{"skein_project_members", "list"}:   {right: KeyRightRead},
	{"skein_project_members", "add"}:    {right: KeyRightManage},
	{"skein_project_members", "remove"}: {right: KeyRightManage},

	{"skein_component", "list"}:    {right: KeyRightRead},
	{"skein_component", "create"}:  {right: KeyRightManage},
	{"skein_component", "update"}:  {right: KeyRightManage},
	{"skein_component", "archive"}: {right: KeyRightManage},

	{"skein_attachments", "list"}:       {right: KeyRightRead},
	{"skein_attachments", "metadata"}:   {right: KeyRightRead},
	{"skein_attachments", "get_url"}:    {right: KeyRightRead},
	{"skein_attachments", "upload_url"}: {right: KeyRightWrite},
	{"skein_attachments", "confirm"}:    {right: KeyRightWrite},
	{"skein_attachments", "delete"}:     {right: KeyRightWrite},

	{"skein_notifications", "inbox"}:           {right: KeyRightRead},
	{"skein_notifications", "mark_read"}:       {right: KeyRightWrite},
	{"skein_notifications", "mark_read_issue"}: {right: KeyRightWrite},
	{"skein_notifications", "mark_read_card"}:  {right: KeyRightWrite},
	{"skein_notifications", "mark_read_all"}:   {right: KeyRightWrite},
	{"skein_notifications", "archive"}:         {right: KeyRightWrite},

	{"skein_watchers", "list"}:            {right: KeyRightRead},
	{"skein_watchers", "filter_list"}:     {right: KeyRightRead},
	{"skein_watchers", "monitor_stop"}:    {right: KeyRightRead},
	{"skein_watchers", "watch"}:           {right: KeyRightWrite},
	{"skein_watchers", "unwatch"}:         {right: KeyRightWrite},
	{"skein_watchers", "project_watch"}:   {right: KeyRightWrite},
	{"skein_watchers", "project_unwatch"}: {right: KeyRightWrite},
	{"skein_watchers", "filter_create"}:   {right: KeyRightWrite},
	{"skein_watchers", "filter_delete"}:   {right: KeyRightWrite},

	{"skein_remind", "list"}:    {right: KeyRightRead},
	{"skein_remind", "set"}:     {right: KeyRightWrite},
	{"skein_remind", "snooze"}:  {right: KeyRightWrite},
	{"skein_remind", "dismiss"}: {right: KeyRightWrite},

	{"skein_theme", "instructions"}:      {right: KeyRightRead},
	{"skein_theme", "describe"}:          {right: KeyRightRead},
	{"skein_theme", "fork"}:              {right: KeyRightWrite},
	{"skein_theme", "preview_variables"}: {right: KeyRightWrite},
	{"skein_theme", "set_variables"}:     {right: KeyRightWrite},
	{"skein_theme", "inspect_clear"}:     {right: KeyRightWrite},

	{"skein_user", "list"}:      {right: KeyRightAdmin},
	{"skein_user", "create"}:    {right: KeyRightAdmin},
	{"skein_user", "update"}:    {right: KeyRightAdmin},
	{"skein_user", "space_add"}: {right: KeyRightAdmin},
}

// MCPToolRequirement returns what a call of an MCP tool requires of the key.
// action is the `action` argument of a consolidated tool ("" for the others);
// force is the call's force argument. ok is false for a tool or action
// outside the matrix: such a call is refused to every key.
func MCPToolRequirement(tool, action string, force bool) (req KeyRequirement, ok bool) {
	row, ok := mcpKeyRights[toolKey{tool, action}]
	if !ok {
		return KeyRequirement{}, false
	}
	req = KeyRequirement{Right: row.right, WideScope: row.wide}
	if force && row.guarded {
		req.Right = KeyRightManage
	}
	return req, true
}

// MCPToolHasActions reports whether tool is a consolidated tool: one whose
// rows in the matrix are per action.
func MCPToolHasActions(tool string) bool {
	for k := range mcpKeyRights {
		if k.tool == tool && k.action != "" {
			return true
		}
	}
	return false
}
