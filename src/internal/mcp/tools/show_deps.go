package tools

// show_deps.go — the dependency block of skein_show (sk-282 T009, spec
// US-1, plan "Wire form").
//
// # What changed and why it is a contract change, not a tuning knob
//
// Reading an issue used to return every edge in both directions. On the
// measured wave node that was 20 742 of 23 496 characters — 88% of the
// response — to answer a question the agent could have had in 240: how
// many children, and how many are closed. The default is now the counters;
// the edges are an explicit, TYPED request.
//
// # The input binding is a total function with rule priority
//
// This is the part that has to be exact, because two plausible readings
// each destroy a different half of the feature, and both were caught in
// review rather than in code:
//
//   - reading "includeDeps:true without a type list → refuse" literally
//     also refuses the DEFAULT call, since includeDeps declares a default
//     of true. That would make every parameterless read an error and take
//     SC-001 with it. Hence: the rules test for the PRESENCE of a key, not
//     for its effective value.
//   - a rule «no key → no edges» applied without priority also swallows a
//     call carrying a non-empty depsTypes and no includeDeps — which is
//     exactly the shape US-10 teaches agents to send (don't re-send
//     default flags). That would break the advertised «counters → edges of
//     the type I need» route on its first use.
//
// So the rules are ORDERED and checked top-down, first match wins, and the
// nine combinations of (includeDeps ∈ {absent, true, false}) × (depsTypes
// ∈ {absent, empty, non-empty}) each land in exactly one:
//
//	1  includeDeps:false present            → neither edges nor counters
//	2  depsTypes present and empty          → refusal naming the parameter
//	3  depsTypes present and non-empty      → edges of those types + counters
//	4  includeDeps:true present, no types   → refusal naming the parameter
//	5  otherwise (neither key present)      → counters, no edges
//
// Two of these decisions the spec leaves to the plan, and the plan makes
// them explicitly: rule 1 DOMINATES a non-empty depsTypes (an explicit
// switch-off is the strongest statement of intent, and answering it with a
// refusal would add a second error path for nothing), and rule 3 works
// WITHOUT includeDeps:true (that is the US-10 route, not a leniency).
//
// # Distinguishing «absent» from «true»
//
// req.GetBool("includeDeps", true) cannot tell them apart, and the whole
// binding turns on that difference. The raw argument map does: an MCP
// client sends only the keys the caller actually set — the schema default
// is documentation, not an injected value.

import (
	"fmt"
	"sort"
	"strings"

	"skein-api/internal/dependencies"
)

// ShowDepsEdgeCapPerType is the transport safety valve on an explicit edge
// request: at most this many edges per requested type.
//
// It is 200, symmetrical with the thread-index cap, and the number is not
// free-floating — it is one term of the arithmetic that decides whether
// skein_show's declared result ceiling still covers the CAPPED structural
// parts of a legal response: card and envelope ~2 800 + the thread index
// at its cap 200 × 222 = 44 400 + edges at this cap 200 × ~190 = 38 000,
// so 85 200 against a declared 100 000 — a margin of 14 800.
//
// Two corrections to the estimate this comment used to carry, because a
// reader who re-derives from the old prose gets a fivefold-wrong index
// term: an index entry is 222 characters as built (id + author + RFC3339
// date + an 80-rune preview + the truncation flag), not the ~90 the plan
// assumed, so the index at its cap is 44 400 and not ≈18 000. And the sum
// is NOT a bound on the heaviest legal answer: comment bodies are outside
// it, `commentsLimit: N` has no ceiling, and the issue's own prose is
// unbounded. It bounds the parts this file and show_comments.go cap.
//
// Lowering the declared ceiling to fit the ~3 200 default would cut the
// legal fat modes at the transport layer — a truncation with no marker,
// which is what INV-1 forbids.
const ShowDepsEdgeCapPerType = 200

// showDepsContinueParentChild names the route to the rest of the children
// when the valve fires on parent-child.
//
// A named route is required rather than nice: INV-4 says every narrowing
// has an explicit, named way to the full data, so a cap without one would
// make US-1 violate the invariant it is written under. The route is not
// invented for this case either — paging children through the list tool is
// the path R-3 already calls the primary one, which is why the valve
// firing here means the typed edge request was never the right tool.
const showDepsContinueParentChild = "skein_list(parentId=<id>) with cursor paging"

// showDepsContinueOther is what the valve reports for every other type.
//
// There is deliberately no per-type paging behind it: hundreds of blockers
// on one issue is a degenerate graph, and the contract's job is to make
// that VISIBLE — marker plus total — not to make it comfortable to walk.
const showDepsContinueOther = "narrow depsTypes, or read the other side directly by id"

// depsRule identifies which row of the binding table matched. Exposed so
// the guard test can assert that every argument combination lands in
// exactly one rule, rather than merely producing a plausible answer.
type depsRule int

const (
	depsRuleOff        depsRule = 1 // includeDeps:false present
	depsRuleEmptyTypes depsRule = 2 // depsTypes present and empty → refuse
	depsRuleTypedEdges depsRule = 3 // depsTypes present and non-empty
	depsRuleNoTypes    depsRule = 4 // includeDeps:true present, no depsTypes → refuse
	depsRuleCountsOnly depsRule = 5 // neither key present
)

// depsRequest is the resolved dependency block request.
type depsRequest struct {
	// Rule is the row of the binding table that matched.
	Rule depsRule
	// Counters is true when the response carries depsCounts.
	Counters bool
	// Types lists the edge types to return, canonicalised and deduped.
	// Non-empty only under depsRuleTypedEdges.
	Types []string
}

// depsRequestError is a refusal from the binding. It names the parameter
// the caller has to fix — "a refusal with a clear error" is the contract, and
// an error that does not say WHICH parameter leaves the caller guessing
// between two of them.
type depsRequestError struct {
	// Rule is the row that refused, for the guard test.
	Rule depsRule
	// Field is the parameter name to surface in the error envelope.
	Field string
	// Message is the human-readable detail.
	Message string
	// UnknownTypes carries the offending names when the refusal is about
	// type spelling rather than about the parameter being empty.
	UnknownTypes []string
}

func (e *depsRequestError) Error() string { return e.Message }

// resolveDepsRequest implements the binding table. Pure: it reads the raw
// argument map and returns a decision, touching no store and no context.
//
// Purity is a requirement from the plan, not a preference — the nine-
// combination guard is a `unit` row of the test matrix, and a binding
// buried in the handler could only be tested with a database up, moving
// five matrix rows from unit to integration.
func resolveDepsRequest(args map[string]any) (depsRequest, *depsRequestError) {
	rawIncludeDeps, includeDepsPresent := args["includeDeps"]
	rawTypes, typesPresent := args["depsTypes"]

	// Rule 1 — an explicit switch-off, checked first so it dominates.
	// This is also the cheapest read in a session (re-reading `version`
	// before a write), and computing counters for a caller that just said
	// "no dependency block" would be a regression on the single most
	// frequent call of a run.
	if includeDepsPresent {
		enabled, ok := rawIncludeDeps.(bool)
		if !ok {
			return depsRequest{}, &depsRequestError{
				Rule:    depsRuleOff,
				Field:   "includeDeps",
				Message: "includeDeps must be a boolean",
			}
		}
		if !enabled {
			return depsRequest{Rule: depsRuleOff}, nil
		}
	}

	if typesPresent {
		types, err := parseDepsTypes(rawTypes)
		if err != nil {
			return depsRequest{}, err
		}
		// Rule 2 — present but empty. An empty list is not "give me
		// everything": it is the same request as rule 4 spelled a
		// different way, and answering it with the whole graph is the
		// behaviour R-1 blames for flags failing — the agent optimises
		// for completeness and the saving evaporates.
		if len(types) == 0 {
			return depsRequest{}, &depsRequestError{
				Rule:  depsRuleEmptyTypes,
				Field: "depsTypes",
				Message: "depsTypes was supplied but names no type — name the edge types you " +
					"need (e.g. [\"parent-child\"]); an empty list does not mean \"all edges\"",
			}
		}
		// Rule 3 — the real request. Note it does NOT require
		// includeDeps:true: that is the US-10 route, where an agent that
		// saw a non-zero counter asks for the one type it cares about.
		return depsRequest{Rule: depsRuleTypedEdges, Counters: true, Types: types}, nil
	}

	// Rule 4 — includeDeps:true spelled out, with no types. Only reachable
	// when the key was really sent, which is what keeps the DEFAULT call
	// out of this branch.
	if includeDepsPresent {
		return depsRequest{}, &depsRequestError{
			Rule:  depsRuleNoTypes,
			Field: "depsTypes",
			Message: "includeDeps:true no longer returns every edge — name the types you need " +
				"in depsTypes (e.g. [\"parent-child\"]). Omit both parameters to get " +
				"depsCounts, which answers \"how many children, how many closed\" without " +
				"any edges",
		}
	}

	// Rule 5 — the new default.
	return depsRequest{Rule: depsRuleCountsOnly, Counters: true}, nil
}

// parseDepsTypes canonicalises the depsTypes argument.
//
// An unknown type name is REFUSED by name, for the same reason an unknown
// name in a field selection is: silently returning no edges of a
// misspelled type is indistinguishable from "there are none", and that is
// a wrong answer the caller cannot detect.
func parseDepsTypes(raw any) ([]string, *depsRequestError) {
	items, ok := raw.([]any)
	if !ok {
		// A bare string is the plausible mistake; accept nothing else
		// rather than guess.
		return nil, &depsRequestError{
			Rule:    depsRuleEmptyTypes,
			Field:   "depsTypes",
			Message: "depsTypes must be an array of edge type names",
		}
	}

	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	var unknown []string
	for _, item := range items {
		name, isString := item.(string)
		if !isString {
			return nil, &depsRequestError{
				Rule:    depsRuleEmptyTypes,
				Field:   "depsTypes",
				Message: "depsTypes must contain edge type names as strings",
			}
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !validDepType(name) {
			unknown = append(unknown, name)
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(unknown) > 0 {
		return nil, &depsRequestError{
			Rule:         depsRuleTypedEdges,
			Field:        "depsTypes",
			UnknownTypes: unknown,
			Message: fmt.Sprintf("depsTypes names unknown edge type(s): %s. Known types: %s",
				strings.Join(unknown, ", "), strings.Join(allDepTypes(), ", ")),
		}
	}
	return out, nil
}

// allDepTypes is the closed set of edge types, sorted, for error messages
// and for validation.
func allDepTypes() []string {
	out := []string{
		dependencies.DepBlocks,
		dependencies.DepWaitsFor,
		dependencies.DepRelated,
		dependencies.DepDiscoveredFrom,
		dependencies.DepSupersedes,
		dependencies.DepDuplicates,
		dependencies.DepParentChild,
	}
	sort.Strings(out)
	return out
}

func validDepType(name string) bool {
	for _, known := range allDepTypes() {
		if name == known {
			return true
		}
	}
	return false
}

// showDepsTruncation reports the safety valve firing on one edge type.
type showDepsTruncation struct {
	// Returned is how many edges of this type the response carries,
	// across both directions.
	Returned int `json:"returned"`
	// Total is how many the caller's visibility actually has, across both
	// directions.
	Total int `json:"total"`
	// Outgoing and Incoming split those two numbers by direction, and
	// they are the difference between a marker and a self-describing one.
	//
	// The budget is per TYPE across both directions and is spent
	// outgoing-first (see filterDepsByType), so a node with 200+ outgoing
	// edges of the requested type returns `incoming: []` — a value that
	// is indistinguishable from "this node has no incoming edges of that
	// type» as long as the marker only reports the aggregate. INV-1 says
	// a narrowing has to describe itself, and a direction silently zeroed
	// by the other direction's edges is the exact case it forbids: here
	// the caller reads incoming.total = 50 next to incoming.returned = 0
	// and knows which side was cut and by how much.
	Outgoing showDepsDirectionCount `json:"outgoing"`
	Incoming showDepsDirectionCount `json:"incoming"`
	// Continue names the route to the rest (INV-4).
	Continue string `json:"continue"`
}

// showDepsDirectionCount is one direction's half of a truncation report.
type showDepsDirectionCount struct {
	// Returned is how many edges of this type and direction the response
	// carries — 0 when the budget was already spent by the other
	// direction.
	Returned int `json:"returned"`
	// Total is how many exist in this direction for the caller.
	Total int `json:"total"`
}

// filterDepsByType keeps the requested types and applies the per-type
// valve, returning the surviving edges plus a truncation report per type
// that hit the cap.
//
// The cap is per TYPE across both directions rather than per type per
// direction: the arithmetic that keeps the declared result ceiling honest
// counts 200 edges in the response, and a per-direction cap would allow
// 400. Edges are kept in the order the store produced them (outgoing
// before incoming, then by type and other-end id under the id-order rule,
// sk-426), so a repeated call returns the same page rather than a random
// one.
//
// That shared budget is also why the report is per DIRECTION: spending it
// outgoing-first can return an empty `incoming` on a node that has plenty
// of incoming edges, and an aggregate-only marker leaves the caller
// unable to tell that emptiness from "there are none" (INV-1). The counts
// are therefore tracked per (type, direction) and both halves are
// reported; the budget itself is unchanged, so the ceiling arithmetic
// still holds.
func filterDepsByType(
	list *dependencies.DependencyList,
	types []string,
) (outgoing, incoming []dependencies.DependencyEdge, truncated map[string]showDepsTruncation) {
	wanted := make(map[string]bool, len(types))
	for _, t := range types {
		wanted[t] = true
	}

	totalsOut := make(map[string]int, len(types))
	totalsIn := make(map[string]int, len(types))
	for _, e := range list.Outgoing {
		if wanted[e.Type] {
			totalsOut[e.Type]++
		}
	}
	for _, e := range list.Incoming {
		if wanted[e.Type] {
			totalsIn[e.Type]++
		}
	}

	keptOut := make(map[string]int, len(types))
	keptIn := make(map[string]int, len(types))
	outgoing = make([]dependencies.DependencyEdge, 0, len(list.Outgoing))
	incoming = make([]dependencies.DependencyEdge, 0, len(list.Incoming))
	for _, e := range list.Outgoing {
		if !wanted[e.Type] || keptOut[e.Type]+keptIn[e.Type] >= ShowDepsEdgeCapPerType {
			continue
		}
		keptOut[e.Type]++
		outgoing = append(outgoing, e)
	}
	for _, e := range list.Incoming {
		if !wanted[e.Type] || keptOut[e.Type]+keptIn[e.Type] >= ShowDepsEdgeCapPerType {
			continue
		}
		keptIn[e.Type]++
		incoming = append(incoming, e)
	}

	for _, depType := range types {
		total := totalsOut[depType] + totalsIn[depType]
		if total <= ShowDepsEdgeCapPerType {
			continue
		}
		if truncated == nil {
			truncated = make(map[string]showDepsTruncation, 1)
		}
		route := showDepsContinueOther
		if depType == dependencies.DepParentChild {
			route = showDepsContinueParentChild
		}
		truncated[depType] = showDepsTruncation{
			Returned: keptOut[depType] + keptIn[depType],
			Total:    total,
			Outgoing: showDepsDirectionCount{Returned: keptOut[depType], Total: totalsOut[depType]},
			Incoming: showDepsDirectionCount{Returned: keptIn[depType], Total: totalsIn[depType]},
			Continue: route,
		}
	}
	return outgoing, incoming, truncated
}
