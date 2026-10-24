package tools

// Shared parameter/description blurbs for the MCP tool schemas
// (sk-255.2).
//
// Every string here is inlined into more than one tool descriptor, so
// its length is multiplied by the number of tools that use it in the
// tools/list payload the agent pays for on every session. The rule
// applied to each: keep exactly the contract the caller needs at the
// call site (which values exist, which combinations are rejected,
// which tool to reach for instead), and move the table, the worked
// example and the rationale into the agent guide behind
// skein_get_instructions — where it is stored once and fetched only by
// agents that actually need it.
//
// Keep the domain vocabulary. These descriptions also feed the client's
// Tool Search ranking, so the words a caller would type ("dependency",
// "blocks", "parent", "commit", "standing") must survive the trim even
// when the sentence around them does not.

// depTypeParamDescription documents the `type` enum shared by
// skein_dep_add, skein_dep_add_many and skein_create's inline
// `dependencies[]`. The enum itself carries the seven legal values;
// this only has to say what direction they mean and where the full
// table lives.
const depTypeParamDescription = "Dependency type; direction is canonical per type " +
	"(read as from → to): blocks = from is blocked by to · waits-for = " +
	"from waits on to (optional `gate` stored, not evaluated) · related = symmetric · " +
	"discovered-from · supersedes · duplicates · parent-child = from is " +
	"the child of to. Full table in skein_get_instructions " +
	"(\"Dependency edge types\")."

// depGateParamDescription documents the `gate` parameter shared by
// skein_dep_add, skein_dep_add_many and skein_create's inline
// `dependencies[]`. It states what the code does, not what the gate was
// designed to do (sk-483): the value is validated and stored, but
// dependencies.Store.ListReady does not read it — a waits-for edge holds
// its FROM side like `blocks`, until the target itself is closed or
// standing. Pinned by TestListReady_WaitsForGateIsNotEvaluated.
const depGateParamDescription = "Only on type=waits-for; optional. Stored with " +
	"the edge but NOT evaluated today: skein_ready holds the from-side until " +
	"the target itself is closed or standing, whatever its children's state."

// depHierarchyNote is the parent-child caveat repeated by every tool
// that can write a parent-child edge. It exists because the three verbs
// (add / move / drop) are genuinely easy to confuse, so the pointer has
// to be at the call site even though the reasoning is in the guide.
const depHierarchyNote = "parent-child only ADDS a parent (`parent_already_set` if one " +
	"exists); prefer `parentId` on skein_create for new children, " +
	"skein_reparent to move an existing issue, skein_dep_remove to drop " +
	"an edge."

// commitsArrayDescription documents the `commits[]` array shared by
// skein_close, skein_close_many (per item) and skein_commits_attach.
// The per-field shape is in commitInputItemSchema; the repo-resolution
// rule and the re-close caveat are in the guide under "Commit refs".
// The dedup clause names its scope because the surrounding sentence is
// about choosing a repo, and an unscoped "hashes dedup" reads as
// per-issue — which would make linking one commit to two repos look
// impossible. The real key is the issue_commits primary key,
// (issue_id, repo_id, hash): the same hash under a different repo is a
// distinct row, not a swallowed duplicate.
const commitsArrayDescription = "Commit-refs to link to this issue. Max 10. Each needs `hash`; " +
	"identify the repo with `repoId` or `repoUrl` from the project's " +
	"gitRepos[], or omit both when the project has exactly one repo. " +
	"Re-sends dedup silently per (issue, repo, hash). " +
	"See skein_get_instructions (\"Commit refs\")."

// standingConceptNote defines what a standing task IS, for the two tools
// that can produce one: skein_create's `standing` flag and skein_update's
// `status=standing` leg.
//
// This is the part that is genuinely common, and it is the expensive
// part. Both call sites need the same three facts — the issue is worked
// by spawning children, it is never claimed in place, and it disappears
// from open-work counts / skein_ready / inbox — because both leave the
// caller holding an issue that behaves that way. Before sk-255.2 the two
// tools stated them in separately-edited prose and had already drifted:
// create mentioned the counter exclusion, update did not.
//
// What is deliberately NOT here: the rejection codes. They are not
// shared — create can only hit `nested_standing_forbidden`, while update
// owns the four transition guards — and folding both sets into one
// string would make each tool advertise errors it cannot return. Each
// call site appends its own, then standingGuideRef. The full table
// (every code, every "do this instead") stays in the guide.
const standingConceptNote = "STANDING task = perpetual obligation: acted on via spawned " +
	"children (skein_create parentId=<this>), never claimed or worked " +
	"in place, excluded from open-work counts / skein_ready / inbox."

// standingGuideRef is the trailing pointer both standing call sites end
// with. Shared so the section name cannot drift away from the heading
// full.md actually carries — TestFullGuide_HasStandingSection pins that
// heading from the other side.
const standingGuideRef = "See skein_get_instructions (\"Standing tasks\")."

// fieldBleedNote is the tool-call-wrapper warning carried by the two
// tools that take multi-line prose from an agent's own draft:
// skein_create and skein_update. Before sk-255.10 the two spelled it out
// in ~640 identical characters each.
//
// It was also WRONG about the guard, and that is why this const names
// literals so carefully. The old text told callers that `</description>`
// and `</design>` "are rejected with a validation_error". The validator
// scans for neither — internal/textvalidate.needles is the closed
// rejected set and contains only wrapper tags — so a value carrying
// `</description>` passes this guard untouched. What the old text got
// right by accident is that such a call usually IS rejected: a genuine
// mis-close makes the client glue the following tokens into the value,
// and those carry `<parameter name=` or `</invoke>`, which the validator
// does scan. So the lie was about WHICH literal to preflight against —
// a caller trusting it would strip the wrong substring and would believe
// the guard covers a set it does not. The guide's table always listed
// the real set; the schemas were the side that drifted.
//
// So the literals quoted here are exactly ones the validator does reject
// (they double as the Tool Search keywords a caller would type), and the
// pointer explicitly advertises that the guide also documents the
// look-alikes that are NOT rejected — the fact the old text inverted.
const fieldBleedNote = "⚠ Field bleed: tool-call wrapper literals (`<invoke`, " +
	"`<parameter name=`, `</parameter>`) in the prose fields (description / " +
	"acceptanceCriteria / notes / design) are rejected with " +
	"`validation_error` (`code: tool_call_syntax_leaked`) — a hit means you " +
	"closed a parameter block with the wrong tag. Closed set + the " +
	"look-alikes that are NOT rejected and corrupt silently: " +
	"skein_get_instructions (\"Text fields — forbidden literals\")."
