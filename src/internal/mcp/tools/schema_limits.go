package tools

// schema_limits.go — the values the MCP layer itself applies to paged
// tools, as named constants.
//
// Two kinds of number decide a paged tool's contract: what its schema
// DECLARES (the `default` / `maximum` a client reads out of tools/list)
// and what the code APPLIES. INV-5 says the two must agree; naming the
// applied value keeps it in one place.
//
// Most applied values live in the store or service that enforces them
// (issues.ListIssuesDefaultLimit / issues.ListIssuesMaxLimit,
// search.DefaultPageLimit / search.MaxPageLimit,
// projects.ListProjectsDefaultLimit / projects.ListProjectsMaxLimit),
// because those layers are shared with REST and are where the clamp
// actually happens. This file holds the ones the MCP handler applies on
// its own — a value the shared layer never sees.

// ProjectListDefaultLimit is the page size skein_project_list sends when
// the caller names none.
//
// It is a SEPARATE constant from the projects store's own default
// (ListProjectsDefaultLimit) because the MCP handler substitutes its value
// before the store ever gets a chance to default — so the store's constant
// is not what an agent experiences, whatever the two happen to be equal to.
//
// They ARE equal today, both 50, and that is a coincidence rather than a
// link: sk-282 T016 lowered the MCP side from 100 to 50 (a project list is
// a navigation aid, and a hundred rows of it is a page nobody reads in
// full) and the store's 50 was already there for REST. Before T016 this
// comment said the two «deliberately differ», which stopped being true in
// that same commit — the reason for the separate constant is the
// substitution order, not the arithmetic, and it stands either way: the
// store's default is REST's and INV-2 freezes it, so the MCP side must be
// able to move without dragging REST along.
//
// The CEILING is untouched and remains the projects store's own.
const ProjectListDefaultLimit = 50

// ListLimitDefault / ListLimitMax are the page size skein_list applies
// when the caller names none, and the ceiling it clamps to (sk-282 T014,
// spec US-2 / SC-003).
//
// They are applied in the MCP handler, NOT in the issues store: that store
// is shared with the REST list endpoints, and INV-2 forbids changing what
// REST returns — including its page size. Before T014 the handler passed
// the caller's value straight through and the store's own 500/500 decided,
// which is why the schema announced 50/100 while an agent actually got up
// to five hundred rows.
const (
	ListLimitDefault = 25
	ListLimitMax     = 100
)

// resolveMCPLimit applies a default-and-ceiling pair to a caller's page
// size. Shared by the paged MCP tools so «declared equals applied» has one
// implementation to be true of.
//
// A non-positive request means «not named» and takes the default; anything
// above the ceiling is clamped rather than refused, which is the behaviour
// the schema's `maximum` already promises.
func resolveMCPLimit(requested, def, max int) int {
	if requested <= 0 {
		return def
	}
	if requested > max {
		return max
	}
	return requested
}
