package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// defaultShowCommentsLimit is the agent-facing default for skein_show
// comments (sk-242.1 FR-006 / US-5). Last-N of the (optionally since-
// filtered) population. 0 no longer means "the whole filtered set": since
// sk-282 T011 it returns the thread INDEX — one entry per comment up to
// ShowThreadIndexCap — plus the last showThreadTailSize bodies in full.
// US-8's obligation rides on the same route: when commentsTruncated=true
// on an unfiltered request, take the index with commentsLimit:0 before
// substantive work and fetch the entries that matter with
// skein_comment_get.
const defaultShowCommentsLimit = 3

// RegisterShow wires the skein_show tool. The DEFAULT answer is the issue
// card, the dependency COUNTS (`depsCounts`, direction × type × status,
// with an `unavailable` bucket for edges whose other end the caller cannot
// see), the child counters, and the last 3 comments plus their metadata.
//
// Three things are opt-in and none of them is "everything": edges arrive
// only for the types `depsTypes` names (`deps.outgoing` / `deps.incoming`,
// cross-project redaction applied, capped per type); `commentsLimit:0`
// gives the thread index plus the last three bodies; `includeEvents` adds
// the recent event timeline.
//
// Annotations:
//
//   - readOnlyHint: true
//   - destructiveHint: false
//   - idempotentHint: true
//   - openWorldHint: false
//
// _meta:
//
//   - anthropic/maxResultSizeChars: 100000 — NOT lowered by sk-282, and the
//     arithmetic behind that is restated here because the number the plan
//     used was wrong.
//
//     The default answer is now around 3 200 characters, so a cap fitted to
//     it would be a tenth of this. It is not fitted to it, because the
//     default is not the heaviest LEGAL answer and INV-5 sizes a
//     declaration on the latter. The sum below is not that heaviest answer
//     either — it is what the parts with a CEILING come to, the thread
//     index at its cap plus a page of edges at theirs:
//
//     card + envelope + metadata                     ~2 800
//     thread index, 200 entries × 222 (measured)     ~44 400
//     edges, 200 × ~190 on the reference issue       ~38 000
//     ─────────
//     ~85 200
//
//     Three things 85 200 does NOT price, and each of them rides on top of
//     it: comment BODIES (`commentsLimit: N` takes any N — that parameter
//     has no ceiling), the issue's own prose, and the per-row label count.
//     That is precisely why the declaration is not lowered to hug the sum.
//     show_deps.go carries the second copy of it; the two are meant to
//     read alike.
//
//     The plan estimated the index entry at ~90 characters and the whole
//     index at ~18 000; the entry as actually built (id + author + date +
//     80-rune preview + truncation flag) is 222, so the index alone is two
//     and a half times the estimate. The CONCLUSION survives — the capped
//     parts fit under 100 000 — but what is left for the uncapped ones is
//     14 800, not the 41 200 the estimate implied. Anyone tempted to lower
//     this number should redo the sum with 222, not with 90.
//
//     Lowering it under the default instead would not truncate the answer:
//     an over-budget result is written to disk and replaced by a file
//     reference in the conversation (see result_size.go), i.e. a legal
//     answer taken out of the conversation. That is the cost, and it is why
//     this stays where it is.
func RegisterShow(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store, depsStore *dependencies.Store) {
	tool := mcpgo.NewTool(
		"skein_show",
		mcpgo.WithDescription(
			"Read one issue by id. BY DEFAULT you get: the issue card, "+
				"dependency COUNTS (`depsCounts`) and no edges, child "+
				"counters, and the last 3 comments. Edges of a named type, "+
				"the thread index and the event feed are opt-in — each is "+
				"named below. Use "+
				"this when you have an id; for browsing without one use "+
				"skein_list. `id` must be the full hierarchical id (e.g. "+
				"\"demo-42\" or \"demo-42.1\") — bare numbers are rejected.\n\n"+
				"Navigating DOWN a tree without pulling the graph: read the "+
				"counters here (`unclosedChildrenCount`, or "+
				"`depsCounts.down[\"parent-child\"]`), then list the children "+
				"with skein_list(parentId=<this id>), then ask for the edges "+
				"you actually need with skein_show(depsTypes:[\"parent-child\"]). "+
				"Each step is bounded; the graph dump is not.\n\n"+
				"Comments (when includeComments=true, the default):\n"+
				"  - Default returns the last 3 comments ASC (to save tokens).\n"+
				"  - commentsTotal / commentsReturned / commentsTruncated are "+
				"always present (including 0/false on empty threads).\n"+
				"  - commentsLimit:0 returns the thread INDEX — one entry per "+
				"comment (id, author, date, first "+strconv.Itoa(ThreadIndexPreviewMaxRunes)+
				" characters, and a per-entry truncation flag), capped at "+
				strconv.Itoa(ShowThreadIndexCap)+" most recent of the population — plus the last "+
				strconv.Itoa(showThreadTailSize)+" comments in full. Read any one of them whole "+
				"with skein_comment_get(id); to get N bodies at once ask for "+
				"commentsLimit:N.\n"+
				"  - commentsSince (RFC3339) is a coarse exclusive lower bound "+
				"on createdAt (not a lossless cursor).\n"+
				"  - commentsTruncated means the thread's ENUMERATION is not "+
				"known to be complete — NOT that some bodies were withheld. "+
				"It is set when fewer entries were listed than the population "+
				"holds, AND whenever the listing hit the index cap ("+
				strconv.Itoa(ShowThreadIndexCap)+"): a listing exactly as long as the valve "+
				"that produced it is reported incomplete regardless of "+
				"`commentsTotal`, because `total` and the page are read in two "+
				"statements and a thread that grew between them would "+
				"otherwise pass a CUT listing off as whole. So on a thread of "+
				"exactly "+strconv.Itoa(ShowThreadIndexCap)+" comments, commentsLimit:0 returns "+
				"returned == total with commentsTruncated=true — the safe "+
				"direction, not a contradiction. On an unfiltered show with "+
				"commentsTruncated=true, commentsLimit:0 lists every comment "+
				"up to that cap.\n"+
				"When includeComments=false: comments is null, meta keys are "+
				"absent, and limit/since params are ignored (even if invalid).\n\n"+
				"Description: pass includeDescription:false for the cheap re-read "+
				"of `version` before a write — the `description` key then arrives "+
				"as null (present, not absent); the other prose fields are "+
				"unaffected.\n\n"+
				"Dependencies: the default is `depsCounts` — a "+
				"direction × type × status aggregate (`up` = parent side, `down` "+
				"= children side, plus an `unavailable` bucket per cell for edges "+
				"whose other end you cannot see) — and NO edges. A non-zero "+
				"`unavailable` means the counts cannot answer \"is this branch "+
				"finished\". Ask for edges by naming types in `depsTypes`; "+
				"`includeDeps:false` drops the block entirely — including the "+
				"card's `childrenCount` / `unclosedChildrenCount`, which ride "+
				"with it. The `deps` key is "+
				"no longer guaranteed to be present.",
		),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id, e.g. \"demo-42\" or \"demo-42.1\"."),
		),
		mcpgo.WithBoolean("includeDescription",
			mcpgo.Description("Include the issue `description` body. Default: true. "+
				"When false the `description` key is still PRESENT and renders as "+
				"null (not omitted), so \"not requested\" stays distinguishable "+
				"from \"empty\" (\"\"). The other prose fields (`design`, "+
				"`acceptanceCriteria`, `notes`) are returned as usual — this flag "+
				"elides the description only. Typical use: the cheap read of "+
				"`version` before a write (expectedVersion cycle), where a long "+
				"description can be tens of KB of dead context on the single most "+
				"frequent call of a run; pair with includeComments:false and "+
				"includeDeps:false for the minimal read."),
			mcpgo.DefaultBool(true),
		),
		mcpgo.WithBoolean("includeComments",
			mcpgo.Description("Include comment list in the response. Default: true. "+
				"When true the field is always an array (empty [] if none) and "+
				"commentsTotal/commentsReturned/commentsTruncated are always set; "+
				"when false the JSON field is null (not omitted) and meta keys "+
				"are absent. commentsLimit/commentsSince are only validated when true."),
			mcpgo.DefaultBool(true),
		),
		mcpgo.WithNumber("commentsLimit",
			mcpgo.Description("When includeComments=true: max number of most-recent "+
				"comments to return IN FULL from the filtered population (ASC order). "+
				"Default 3. 0 = the thread INDEX — one entry per comment (id, author, "+
				"date, first "+strconv.Itoa(ThreadIndexPreviewMaxRunes)+" characters) for up to "+
				strconv.Itoa(ShowThreadIndexCap)+" most recent of the population, plus the last "+
				strconv.Itoa(showThreadTailSize)+" comments in full — NOT every body. Read a body "+
				"by id with skein_comment_get, or ask for commentsLimit:N to get N bodies. "+
				"Must be a whole integer ≥ 0; "+
				"negative or non-integer → validation_error. Ignored when comments off."),
			mcpgo.DefaultNumber(defaultShowCommentsLimit),
			mcpgo.Min(0),
		),
		mcpgo.WithString("commentsSince",
			mcpgo.Description("When includeComments=true: exclusive lower bound on "+
				"comment createdAt (RFC3339). Only comments with createdAt > since "+
				"enter the population; total/truncated are computed after this filter. "+
				"Omit or empty = no time filter. Invalid timestamp → validation_error. "+
				"Not a lossless cursor (equal timestamps may skip siblings). "+
				"Ignored when comments off."),
		),
		mcpgo.WithBoolean("includeDeps",
			mcpgo.Description("Dependency block switch. OMIT IT to get `depsCounts` "+
				"(counts only, no edges) plus the child counters "+
				"(`childrenCount` / `unclosedChildrenCount`) on the issue card — "+
				"that is the effective default and the cheapest answer to \"how "+
				"many children, how many closed\". The default cannot be spelled "+
				"as a value, which is why this parameter declares none: passing "+
				"true WITHOUT `depsTypes` is REJECTED (it used to mean \"every "+
				"edge\", the 20 KB this tool no longer sends by accident) — name "+
				"the types in `depsTypes` instead. Pass false to drop the whole "+
				"block: no edges, no `depsCounts`, AND no child counters on the "+
				"card — the cheapest re-read of `version`, and the one mode where "+
				"the counters are absent rather than zero."),
		),
		mcpgo.WithArray("depsTypes",
			mcpgo.Description("Edge types to return in full, e.g. [\"parent-child\"]. "+
				"Supplying it returns every edge of those types (up to "+
				strconv.Itoa(ShowDepsEdgeCapPerType)+" per type) ALONGSIDE `depsCounts`, and "+
				"needs no includeDeps. An empty array is rejected — it does not "+
				"mean \"all types\". The cap is per TYPE across BOTH directions and "+
				"is spent outgoing-first, so a node with more than the cap of "+
				"outgoing edges of a requested type comes back with `incoming: []`. "+
				"Over the cap the response therefore carries "+
				"`deps.truncated[<type>]` with the aggregate `returned` / `total`, "+
				"the route to the rest, AND the two direction halves — "+
				"`outgoing` and `incoming`, each with its own `returned` / "+
				"`total`. Read the halves, not the aggregate, to tell a direction "+
				"the budget zeroed (incoming.returned 0 with incoming.total > 0) "+
				"from one that genuinely has no edges (both 0). Known types: blocks, waits-for, related, "+
				"discovered-from, supersedes, duplicates, parent-child."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithBoolean("includeEvents",
			mcpgo.Description("Include up to 20 most-recent events. Default: false. "+
				"Every returned event contains actorSessionId: the originating MCP session "+
				"id, or explicit null for session-less actors. "+
				"When true the field is always an array (empty [] if none); when false "+
				"the JSON field is null (not omitted)."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/maxResultSizeChars": 100000,
	})

	addTool(s, tool, handleShow(pool, issuesStore, depsStore))
}

// showResponse is the wire shape for skein_show. Issue is the full
// IssueDetail (so callers can pull labels, components, status, etc);
// it is typed `any` because includeDescription=false swaps in the
// JSON-map projection from issueWithoutDescription, which carries the
// same keys with `description` rewritten to null (sk-244.1).
// Deps mirrors the REST /issues/{id}/dependencies shape; comments and
// events are honored based on the include flags — when an include
// flag is true the slice is always emitted (empty becomes []), when
// the flag is false the field renders as null. This lets the agent
// branch on null-vs-[] cleanly.
//
// sk-242.1 US-5 / FR-004/005: commentsTotal / commentsReturned /
// commentsTruncated use pointers so JSON omitempty keeps them ABSENT
// when comments are off, and present (including 0 / false) when on.
type showResponse struct {
	Issue any `json:"issue"`
	// DepsCounts is the new default of the dependency block (sk-282 T009):
	// the «direction × type × status» aggregate that answers "how many
	// children, how many closed" without shipping the graph. Present
	// whenever the block is on at all; absent only when the caller
	// switched dependencies off outright.
	DepsCounts *dependencies.DependencyCounts `json:"depsCounts,omitempty"`
	// Deps carries EDGES, and — unlike before T009 — it is no longer
	// guaranteed to be there by default. It appears only on an explicit
	// typed request (depsTypes), which is what turned an 88%-edges
	// response into a counted one.
	Deps     *showDeps           `json:"deps,omitempty"`
	Comments []issues.CommentDTO `json:"comments"`
	// CommentsIndex is the thread index (sk-282 T011): one entry per
	// comment of the population, up to ShowThreadIndexCap, returned when
	// the caller asks for the whole thread. Absent in every other mode —
	// the three unchanged modes answer exactly as they did.
	//
	// A POINTER for the same reason CommentsTotal is one: an empty thread
	// asked for in full must render `[]`, not nothing. A plain slice with
	// omitempty cannot say «listed, and there was none» — it drops the key
	// exactly where the caller needs to see that the enumeration happened.
	CommentsIndex     *[]commentIndexEntry `json:"commentsIndex,omitempty"`
	CommentsTotal     *int                 `json:"commentsTotal,omitempty"`
	CommentsReturned  *int                 `json:"commentsReturned,omitempty"`
	CommentsTruncated *bool                `json:"commentsTruncated,omitempty"`
	Events            []issues.EventDTO    `json:"events"`
}

// showDeps wraps the outgoing/incoming edges.
//
// Since sk-282 T010 the entries are the THIN projection (depedge.go), not
// dependencies.DependencyEdge: the REST endpoint keeps the full shape —
// INV-2 forbids changing it — while the MCP wire drops the empty metadata
// map, the version, the created-at and the nested project id, which
// together were about two thirds of an edge. The `redacted` flag and both
// side ids survive unconditionally, because on an unreachable edge they
// are the only identification there is.
type showDeps struct {
	Outgoing []depEdge `json:"outgoing"`
	Incoming []depEdge `json:"incoming"`
	// Truncated reports, per edge type, that the transport safety valve
	// fired: how many edges came back, how many exist for this caller, and
	// the named route to the rest. Absent when nothing was cut — INV-1's
	// truncation marker is set only when truncation really happened.
	Truncated map[string]showDepsTruncation `json:"truncated,omitempty"`
}

func handleShow(pool *pgxpool.Pool, issuesStore *issues.Store, depsStore *dependencies.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		rawID, err := req.RequireString("id")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "id is required", nil), nil
		}
		id := strings.TrimSpace(rawID)
		if !looksLikeHierarchicalID(id) {
			return skmcp.ToolError(ErrCodeValidationError,
				"id must be a full hierarchical id like \"demo-42\" — bare numbers are not accepted",
				map[string]any{"id": rawID}), nil
		}

		includeComments := req.GetBool("includeComments", true)
		includeEvents := req.GetBool("includeEvents", false)

		// sk-282 T009: the dependency block is decided by a total function
		// over the RAW argument map — req.GetBool cannot tell an absent
		// includeDeps from an explicit true, and the whole binding turns
		// on that difference (see show_deps.go).
		depsReq, depsErr := resolveDepsRequest(req.GetArguments())
		if depsErr != nil {
			details := map[string]any{"field": depsErr.Field}
			if len(depsErr.UnknownTypes) > 0 {
				details["unknownTypes"] = depsErr.UnknownTypes
			}
			details["knownTypes"] = allDepTypes()
			return skmcp.ToolError(ErrCodeValidationError, depsErr.Message, details), nil
		}
		includeDescription := req.GetBool("includeDescription", true)

		// Load the issue first; not-found short-circuits everything.
		issue, err := issuesStore.GetIssue(ctx, id)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_show",
				"reason", "get issue failed",
				"userId", userID.String(), "issueId", id, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil), nil
		}

		// Visibility check on the project. Same rule as REST: if the
		// project isn't visible, mask as "issue_not_found" — never leak
		// the existence of issues in projects the caller can't see.
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, issue.ProjectID, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"issue does not exist or is not visible",
					map[string]any{"id": id}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_show",
				"reason", "visibility check failed",
				"userId", userID.String(), "issueId", id, "error", vErr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil), nil
		}

		resp := showResponse{Issue: issue}
		if !includeDescription {
			// sk-244.1: elide the description at the wire edge. The
			// projection can only fail if IssueDetail stops being
			// JSON-marshalable; surface that as internal_error rather
			// than silently falling back to the full body, which would
			// undo the point of the flag.
			projected, perr := issueWithoutDescription(issue)
			if perr != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_show",
					"reason", "issue detail projection failed",
					"userId", userID.String(), "issueId", id, "error", perr.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
			}
			resp.Issue = projected
		}

		if depsReq.Counters {
			scope := authz.FromContext(ctx)
			counts, err := depsStore.CountDependencies(ctx, id, scope)
			if err != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_show",
					"reason", "deps counts failed",
					"userId", userID.String(), "issueId", id, "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to load deps", nil), nil
			}
			resp.DepsCounts = counts

			// sk-282 T012: the US-3 child counters ride with the
			// dependency block, and that is a deliberate tie rather than a
			// convenience. They are computed on the MCP path only (D-2 /
			// INV-2), so somebody has to decide when to pay for them, and
			// the same sentence that protects the cheapest read protects
			// this: a caller that switched the dependency block off is
			// re-reading `version` and wants neither aggregate. Tying them
			// together also means the two always arrive in the SAME
			// response, which is what makes INV-10's identity —
			// childrenCount = down/parent-child − standing − unavailable —
			// checkable by the caller rather than only by us.
			childCounts, err := issuesStore.CountChildren(ctx, id, scope)
			if err != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_show",
					"reason", "child counts failed",
					"userId", userID.String(), "issueId", id, "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to load child counts", nil), nil
			}
			card, cerr := withIssueChildCounts(resp.Issue, childCounts)
			if cerr != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_show",
					"reason", "issue card projection failed",
					"userId", userID.String(), "issueId", id, "error", cerr.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
			}
			resp.Issue = card

			// Edges only on an explicit typed request. The counters ride
			// along with them on purpose: an agent that asked for one type
			// still needs to see that the others exist, or the typed
			// request becomes a narrower lie than the default.
			if len(depsReq.Types) > 0 {
				deps, err := depsStore.List(ctx, id, scope)
				if err != nil {
					slog.ErrorContext(ctx, "mcp tool error",
						"tool", "skein_show",
						"reason", "deps list failed",
						"userId", userID.String(), "issueId", id, "error", err.Error())
					return skmcp.ToolError(ErrCodeInternalError, "failed to load deps", nil), nil
				}
				outgoing, incoming, truncated := filterDepsByType(deps, depsReq.Types)
				resp.Deps = &showDeps{
					Outgoing:  projectDepEdges(outgoing),
					Incoming:  projectDepEdges(incoming),
					Truncated: truncated,
				}
			}
		}

		if includeComments {
			// sk-242.1 T040: limit/since only validated when comments on
			// (FR-005/009/010). Off path ignores bad params entirely.
			opts, verr := parseShowCommentsOpts(req.GetArguments())
			if verr != nil {
				return verr, nil
			}
			// No origin filter (sk-16) — agents get the complete trail;
			// Humans/Agents chips live on the REST list endpoint.
			// sk-282 T011: «the whole thread» (commentsLimit=0) is now the
			// index plus the last three in full. The store is asked for
			// the most recent ShowThreadIndexCap of the population in ONE
			// call — the same call that yields `total`, so the count and
			// the listing can never describe different populations (D-7).
			wholeThread := opts.Limit == 0
			if wholeThread {
				opts.Limit = ShowThreadIndexCap
			}

			comments, total, err := issuesStore.ListComments(ctx, id, issues.CommentOriginAll, opts)
			if err != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_show",
					"reason", "comments list failed",
					"userId", userID.String(), "issueId", id, "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to load comments", nil), nil
			}
			if comments == nil {
				comments = make([]issues.CommentDTO, 0)
			}

			returned := len(comments)
			if wholeThread {
				// «Listed» is the index, not the bodies: the flag now
				// means «the thread's ENUMERATION is incomplete», and the
				// three full bodies are a convenience on top of it.
				index := buildCommentIndex(comments)
				resp.CommentsIndex = &index
				comments = threadTail(comments)
				returned = len(index)
			}
			// The index cap is the only valve in play, and only in the
			// whole-thread mode: a caller-supplied commentsLimit is a
			// request, not a valve (see threadIncomplete).
			capApplied := 0
			if wholeThread {
				capApplied = ShowThreadIndexCap
			}
			truncated := threadIncomplete(returned, total, capApplied)
			resp.Comments = comments
			// Always set pointers when comments on (incl 0/false) so
			// omitempty does not drop zero values (FR-004 / INV-7).
			resp.CommentsTotal = &total
			resp.CommentsReturned = &returned
			resp.CommentsTruncated = &truncated
		}
		// includeComments=false: Comments stays nil → JSON null; meta
		// pointers stay nil → keys absent (FR-005).

		if includeEvents {
			events, err := issuesStore.ListEvents(ctx, id, 20)
			if err != nil {
				slog.ErrorContext(ctx, "mcp tool error",
					"tool", "skein_show",
					"reason", "events list failed",
					"userId", userID.String(), "issueId", id, "error", err.Error())
				return skmcp.ToolError(ErrCodeInternalError, "failed to load events", nil), nil
			}
			if events == nil {
				events = make([]issues.EventDTO, 0)
			}
			resp.Events = events
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_show",
			"userId", userID.String(), "issueId", id,
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(resp)
	}
}

// parseShowCommentsOpts reads commentsLimit + commentsSince under the
// "comments on" validation gate (FR-009/010). Default limit is 3; 0 means
// the thread INDEX plus the last showThreadTailSize bodies, NOT every
// body. Rejects non-whole numbers (silent GetInt floor of 3.7 is
// forbidden).
func parseShowCommentsOpts(args map[string]any) (issues.ListCommentsOpts, *mcpgo.CallToolResult) {
	var opts issues.ListCommentsOpts

	limit, err := parseOptionalWholeInt(args, "commentsLimit", defaultShowCommentsLimit)
	if err != nil {
		return opts, skmcp.ToolError(ErrCodeValidationError, err.Error(),
			map[string]any{"field": "commentsLimit"})
	}
	if limit < 0 {
		return opts, skmcp.ToolError(ErrCodeValidationError,
			"commentsLimit must be a whole integer ≥ 0 (0 = the thread index "+
				"plus the last "+strconv.Itoa(showThreadTailSize)+" comments in full, "+
				"not every body)",
			map[string]any{"field": "commentsLimit", "commentsLimit": limit})
	}
	opts.Limit = limit

	rawSince, ok := args["commentsSince"]
	if ok && rawSince != nil {
		s, isStr := rawSince.(string)
		if !isStr {
			return opts, skmcp.ToolError(ErrCodeValidationError,
				"commentsSince must be an RFC3339 timestamp string",
				map[string]any{"field": "commentsSince"})
		}
		s = strings.TrimSpace(s)
		if s != "" {
			// Accept RFC3339 and RFC3339Nano (DB timestamps often have ns).
			t, perr := time.Parse(time.RFC3339, s)
			if perr != nil {
				t, perr = time.Parse(time.RFC3339Nano, s)
			}
			if perr != nil {
				return opts, skmcp.ToolError(ErrCodeValidationError,
					"commentsSince must be an RFC3339 timestamp",
					map[string]any{"field": "commentsSince", "commentsSince": s})
			}
			ts := t.UTC()
			opts.Since = &ts
		}
	}
	return opts, nil
}

// parseOptionalWholeInt reads an optional whole-integer tool argument.
// Missing / nil → defaultVal. JSON numbers arrive as float64; non-integers
// (3.7) and non-numeric types are rejected so agents never get a silent
// floor (plan: "Strict int commentsLimit"; FR-009).
func parseOptionalWholeInt(args map[string]any, key string, defaultVal int) (int, error) {
	val, ok := args[key]
	if !ok || val == nil {
		return defaultVal, nil
	}
	switch v := val.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("%s must be a whole integer ≥ 0", key)
		}
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("%s must be a whole integer (got non-integer %v)", key, v)
		}
		if v > float64(math.MaxInt) || v < float64(math.MinInt) {
			return 0, fmt.Errorf("%s out of int range", key)
		}
		return int(v), nil
	case float32:
		f := float64(v)
		if f != math.Trunc(f) {
			return 0, fmt.Errorf("%s must be a whole integer (got non-integer %v)", key, v)
		}
		return int(f), nil
	case string:
		// Some MCP clients stringify numbers; accept base-10 integers only.
		s := strings.TrimSpace(v)
		// Explicit empty/whitespace is not "missing" — reject so agents
		// get validation_error instead of silent default (found in review).
		if s == "" {
			return 0, fmt.Errorf("%s must be a whole integer", key)
		}
		n, err := strconv.ParseInt(s, 10, 0)
		if err != nil {
			return 0, fmt.Errorf("%s must be a whole integer", key)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s must be a whole integer", key)
	}
}

// looksLikeHierarchicalID rejects bare numeric ids (e.g. "42") which would
// be ambiguous with custom-mode ids. The rule is "must contain at least
// one '-'": every project-prefixed id has the form `<prefix>-<suffix>`
// (vision §3 issue id rules), and even hierarchical children keep the
// dash from the root id (`demo-42.1`).
//
// We intentionally do NOT validate the full id charset here — the issues
// store already enforces that on read; this is purely the "looks like a
// full id, not a bare number" guard the spec calls out.
func looksLikeHierarchicalID(id string) bool {
	return id != "" && strings.Contains(id, "-")
}

// callerRole reads the role out of ctx, defaulting to "user" when
// missing. The api-key middleware always populates it; the fallback is
// defensive against a misconfigured wiring. We emit a warn so a
// regression in the auth middleware is observable in Loki without
// breaking running calls.
func callerRole(ctx context.Context) string {
	if role, ok := auth.UserRoleFrom(ctx); ok && role != "" {
		return role
	}
	slog.WarnContext(ctx, "auth role missing in mcp tool ctx",
		"defaulted_to", "user")
	return "user"
}
