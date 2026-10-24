package tools

// issuerow.go — the ONE issue-row projection of the MCP layer (sk-282
// T005, spec US-2).
//
// # What a "row" is and why it needs a projection
//
// Three MCP surfaces emit issue rows — the issue list, the ready board,
// and session priming — and today each of them serialises a shared struct
// whole. Measured on the reference issue, that row is 615 characters, and
// about 45% of it is three identifiers and two timestamps no agent reads.
// US-2 replaces "whatever the struct happens to hold" with a NAMED default
// set, plus an explicit per-call selection for narrow queries.
//
// # Why a JSON map and not a parallel struct
//
// Same reason lean.go gives, and it is the load-bearing constraint here
// (R-2, D-1): a hand-written MCPIssueRow struct would silently omit every
// field later added to issues.IssueSummary, and that silence looks exactly
// like a deliberate cut. Marshalling the REAL type and then selecting keys
// means a new field flows into the universe of known names automatically —
// and D-9's drift-guard (T006) then forces someone to classify it.
//
// The shared types are NOT touched. issues.IssueSummary and
// dependencies.IssueSummary are serialised by REST handlers, and INV-2
// forbids changing a REST response's field set — including additively. The
// narrowing lives here, at the MCP wire edge, and nowhere else.
//
// # The two carriers, and why "not provided" is declared, not inferred
//
// Both carriers marshal `labels` and `componentIds` as `[]`. But
// dependencies.IssueSummary never LOADS them (the ready query skips the
// aggregate joins to avoid N+1), while issues.IssueSummary does. After
// marshalling, "the field is in the default set and this issue genuinely
// has no labels" and "this carrier does not load labels at all" are the
// same three bytes on the wire — and INV-1 exists precisely to keep those
// two apart, because an agent that reads `[]` as "no labels" plans
// differently than one that reads "not asked".
//
// It cannot be derived from the data, so the CALLER declares it:
// IssueRowOptions.NotProvided lists the keys this carrier does not supply,
// and those keys are removed from the row entirely. An empty-but-really-
// loaded set stays `[]`. That is the third form of the INV-1 marker — a
// uniform documented contract — and it is legitimate here because the
// omission is UNCONDITIONAL for that surface, not mode-dependent.
//
// # What this file deliberately does NOT do
//
//   - It wires itself into no tool. Consumers are T014 (list), T017
//     (ready), T018 (prime) and, for the counter slot, T012. The plan
//     promises no change on the wire.
//   - It does not implement the drift-guard. That is T006, and it lives in
//     the test file so a new unclassified field fails a test rather than
//     changing a response.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Field names. They are constants because three consumers, the drift-guard
// and the `fields` parameter's documentation all have to spell them the
// same way; a typo in one of five string literals is a silently missing
// column.
const (
	IssueRowFieldID                    = "id"
	IssueRowFieldProjectID             = "projectId"
	IssueRowFieldParentID              = "parentIdCurrent"
	IssueRowFieldVersion               = "version"
	IssueRowFieldTitle                 = "title"
	IssueRowFieldType                  = "type"
	IssueRowFieldPriority              = "priority"
	IssueRowFieldStatus                = "status"
	IssueRowFieldAssignee              = "assignee"
	IssueRowFieldLabels                = "labels"
	IssueRowFieldIsBlocked             = "isBlocked"
	IssueRowFieldUpdatedAt             = "updatedAt"
	IssueRowFieldChildrenCount         = "childrenCount"
	IssueRowFieldUnclosedChildrenCount = "unclosedChildrenCount"
)

// issueRowDefaultFields is US-2's normative default set, in the order the
// spec lists it. This is a NAMED LIST, not a heuristic: the spec calls it
// "a normative list, not 'identifying and decisive'" exactly so
// that nobody has to guess whether a new field is "identifying enough".
//
// Included per US-2: identifier · title · status · type · priority ·
// assignee · labels · blocked flag · parent identifier · version ·
// last-modified stamp · child counters (US-3).
//
// The cross-project identifier is NOT here — it is added by
// IssueRowOptions.CrossProject, because US-2 puts it in the default set
// only in cross-project mode ("without it the listing is unreadable").
//
// Anything the spec does not list is OUT. That reading matters for the
// fields the spec's two lists do not mention at all — `pinned` is the one
// on today's carriers — and it is the conservative one: a normative list
// that silently admits unlisted fields is not a list. T006's drift-guard
// makes that decision visible instead of implicit.
var issueRowDefaultFields = []string{
	IssueRowFieldID,
	IssueRowFieldTitle,
	IssueRowFieldStatus,
	IssueRowFieldType,
	IssueRowFieldPriority,
	IssueRowFieldAssignee,
	IssueRowFieldLabels,
	IssueRowFieldIsBlocked,
	IssueRowFieldParentID,
	IssueRowFieldVersion,
	IssueRowFieldUpdatedAt,
	IssueRowFieldChildrenCount,
	IssueRowFieldUnclosedChildrenCount,
}

// IssueRowDefaultFields returns a copy of the normative default set, in
// spec order. A copy, because the slice is package state and a consumer
// that appended to it would change every other surface's default.
func IssueRowDefaultFields() []string {
	out := make([]string, len(issueRowDefaultFields))
	copy(out, issueRowDefaultFields)
	return out
}

// IssueRowDefaultFieldsSentence renders the default set as the backticked,
// comma-separated list a tool DESCRIPTION shows an agent.
//
// It exists because the descriptions used to spell that list out by hand,
// and by a later stage of sk-282 two of them named `componentIds` — a field the
// default set does not contain and the drift-guard classifies as
// deliberately out. Nothing was wrong with the code; the text an agent
// reads was wrong, which for an agent is the same thing: it plans against
// the description and does not check the wire.
//
// A hand-written list can only be kept true by someone re-reading it. This
// one is the constant, formatted — so a field that enters or leaves the
// default set changes every description in the same edit, and cannot be
// forgotten in one of them.
//
// crossProject appends the project identifier, exactly as
// resolveIssueRowFields does for the row itself.
func IssueRowDefaultFieldsSentence(crossProject bool) string {
	names := IssueRowDefaultFields()
	if crossProject {
		names = append(names, IssueRowFieldProjectID)
	}
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, "`"+n+"`")
	}
	return strings.Join(quoted, ", ")
}

// issueRowInjectedFields are the row keys that come from the CALLER rather
// than from the carrier struct. The child counters are computed on the MCP
// path only (INV-2 keeps them out of the shared type and therefore out of
// every REST response), so they cannot be found by reflecting the carrier
// — they have to be injected, and they have to be validated separately.
var issueRowInjectedFields = map[string]bool{
	IssueRowFieldChildrenCount:         true,
	IssueRowFieldUnclosedChildrenCount: true,
}

// IssueRowChildCounts is the US-3 counter pair injected into a row.
//
// Both numbers are ALWAYS emitted when injected, zero included: US-3 is
// explicit that a zero must be readable as "counted, and it is none",
// never as "not counted". Omitting a zero would collapse the two.
type IssueRowChildCounts struct {
	// Children is the number of children visible to the caller.
	Children int
	// Unclosed is how many of those are not closed — the number that
	// answers "is the wave finished?".
	Unclosed int
}

// IssueRowOptions configures one projection.
type IssueRowOptions struct {
	// Fields is the caller's explicit selection.
	//
	// nil  → the normative default set (plus the project identifier when
	//        CrossProject is set).
	// []   → the identifier alone. An empty selection is a legitimate
	//        request, not an error: US-2's edge-case table says an empty
	//        set yields the identifier, the minimal recognisable minimum.
	// [..] → exactly these, plus the identifier, which is returned always.
	Fields []string

	// CrossProject adds the project identifier to the DEFAULT set. It has
	// no effect on an explicit selection — a caller naming its fields has
	// already decided.
	CrossProject bool

	// NotProvided lists keys this carrier does not supply at all. They are
	// removed from the row entirely rather than emitted empty, so the
	// caller can tell "not supplied here" from "supplied and empty"
	// (INV-1). Declared by the caller because it cannot be inferred from
	// the marshalled value — see the file comment.
	//
	// Names are validated exactly like Fields: an unknown one is an
	// UnknownIssueRowFieldError, not a no-op. The universe is wider than
	// the carrier's own fields (see issueRowDeclarableFields), because
	// declaring a default-set key the carrier cannot supply is the whole
	// point of the mechanism.
	NotProvided []string

	// ChildCounts fills the US-3 counter slot. nil means this surface does
	// not compute them, in which case the two counter keys must appear in
	// NotProvided — otherwise the projection fails loudly rather than
	// dropping a mandatory field silently.
	ChildCounts *IssueRowChildCounts

	// IsBlocked injects the US-2 blocked flag for a carrier that does not
	// declare it.
	//
	// It exists for exactly one shape of surface: one whose QUERY decides
	// blocked-ness but whose carrier struct has no column for the answer.
	// The "ready to take" carrier (dependencies.IssueSummary) is that
	// case — its selection predicate is `NOT EXISTS(blocks|waits-for edge
	// whose target still gates readiness)` — stronger than the negation
	// of the issues store's isBlockedExprFor, which since sk-423 counts
	// only blockers the reader can see — so «false» here is not an
	// assumption bolted onto a silent row: it is what the query already
	// computed.
	//
	// nil means "this surface really does not know", and then the key
	// belongs in NotProvided like any other unsupplied field — INV-1 is
	// unchanged, this only distinguishes "not known" from "known false".
	IsBlocked *bool
}

// UnknownIssueRowFieldError is returned when `fields` names something the
// carrier does not have.
//
// It is an ERROR and not a silent skip on purpose. US-2's edge-case table
// forbids both alternatives by name: ignoring the unknown name silently is
// indistinguishable from "that field is empty", and falling back to all
// fields turns a typo into the fat response the whole change removes.
type UnknownIssueRowFieldError struct {
	// Field is the unknown name exactly as the caller spelled it.
	Field string
	// Known is the sorted set of names this carrier accepts, so a caller
	// can render a usable message without re-deriving it.
	Known []string
}

func (e *UnknownIssueRowFieldError) Error() string {
	return fmt.Sprintf("unknown field %q; known fields: %s",
		e.Field, strings.Join(e.Known, ", "))
}

// MissingIssueRowValueError is returned when a selected field has no value
// from either the carrier or the injected slot, and was not declared
// not-provided.
//
// This is a wiring bug in the consumer, not a caller error: it means the
// surface asked for the child counters without computing them, or declared
// nothing about them. Failing is the point — the alternative is a row that
// quietly lacks a field US-3 calls mandatory.
type MissingIssueRowValueError struct {
	Field string
}

func (e *MissingIssueRowValueError) Error() string {
	return fmt.Sprintf("field %q was selected but no value was supplied and it was not "+
		"declared not-provided", e.Field)
}

// ProjectIssueRow projects one issue row carrier (issues.IssueSummary,
// dependencies.IssueSummary, or any struct with JSON tags) onto the wire
// map an MCP tool should emit.
//
// The order of operations is the contract:
//
//  1. marshal the real carrier — so new fields exist automatically;
//  2. inject the caller's counters — so they select like any other field;
//  3. drop the not-provided keys — so absence is unconditional;
//  4. resolve the selection — default set, or the caller's, always with
//     the identifier;
//  5. copy the selected keys that have a value.
//
// Step 5 skips a selected key with no value ONLY when the carrier itself
// omitted it (a nil assignee, an absent parent) or the caller declared it
// not-provided. A selected injectable field with nothing injected is a
// MissingIssueRowValueError — see that type.
func ProjectIssueRow(carrier any, opts IssueRowOptions) (map[string]any, error) {
	if carrier == nil {
		return nil, fmt.Errorf("issue row projection: nil carrier")
	}

	b, err := json.Marshal(carrier)
	if err != nil {
		return nil, fmt.Errorf("marshal issue row: %w", err)
	}
	var full map[string]any
	if err := json.Unmarshal(b, &full); err != nil {
		return nil, fmt.Errorf("unmarshal issue row: %w", err)
	}

	if opts.ChildCounts != nil {
		full[IssueRowFieldChildrenCount] = opts.ChildCounts.Children
		full[IssueRowFieldUnclosedChildrenCount] = opts.ChildCounts.Unclosed
	}

	known := issueRowKnownFields(carrier)
	if opts.IsBlocked != nil {
		full[IssueRowFieldIsBlocked] = *opts.IsBlocked
		// Injected values are selectable like any other: a caller naming
		// `isBlocked` on a carrier that gets the flag injected must not be
		// told the field is unknown.
		known[IssueRowFieldIsBlocked] = true
	}
	declarable := issueRowDeclarableFields(known)

	notProvided := make(map[string]bool, len(opts.NotProvided))
	for _, k := range opts.NotProvided {
		// A name nobody can spell right is rejected here for the same
		// reason it is rejected in `fields`, and the asymmetry mattered:
		// an unknown name in `fields` fails loudly, while a typo in
		// NotProvided used to be a silent no-op that left the field ON the
		// wire — under the very mechanism that exists to state, without
		// exception, that the field is NOT provided (INV-1). A declaration
		// that can silently miss is not an unconditional contract, and the
		// resulting row says "this carrier loaded an empty set" about a
		// surface that never asked.
		if !declarable[k] {
			return nil, &UnknownIssueRowFieldError{Field: k, Known: sortedFieldNames(declarable)}
		}
		notProvided[k] = true
		delete(full, k)
	}

	selected, err := resolveIssueRowFields(opts, known)
	if err != nil {
		return nil, err
	}

	out := make(map[string]any, len(selected))
	for _, name := range selected {
		if v, ok := full[name]; ok {
			out[name] = v
			continue
		}
		if notProvided[name] {
			// Declared absent for this carrier — that IS the answer.
			continue
		}
		if issueRowInjectedFields[name] || !known[name] {
			// Either the counter slot was selected and nothing was
			// injected, or this carrier does not declare the field at all
			// (dependencies.IssueSummary has no `isBlocked`, for
			// instance). Both are wiring gaps in the consumer, and both
			// would otherwise produce a row missing a field the default
			// set promises — silently.
			return nil, &MissingIssueRowValueError{Field: name}
		}
		// The carrier declares the field but marshalled it away
		// (omitempty on a nil pointer). Absence IS the value: emitting an
		// explicit null would spend bytes to say "nothing", which the
		// field's absence already says.
	}
	return out, nil
}

// validateIssueRowFields checks a caller's `fields` selection against a
// carrier's universe of names WITHOUT projecting anything.
//
// It exists because the row projection runs per row, and a page with no
// rows therefore validated nothing: an unknown name came back as success
// with an empty page, which US-2 forbids — "the field was ignored silently"
// is indistinguishable from "the field is present but empty", and an empty page is
// where every fruitless filter lands, not an exotic edge.
//
// carrier is used for its TYPE only; the zero value is the intended
// argument. nil fields (the default set) validate trivially.
func validateIssueRowFields(fields []string, crossProject bool, carrier any) error {
	if fields == nil {
		return nil
	}
	_, err := resolveIssueRowFields(
		IssueRowOptions{Fields: fields, CrossProject: crossProject},
		issueRowKnownFields(carrier),
	)
	return err
}

// resolveIssueRowFields turns the options into the ordered list of keys to
// emit, validating an explicit selection against the carrier's universe.
func resolveIssueRowFields(opts IssueRowOptions, known map[string]bool) ([]string, error) {
	if opts.Fields == nil {
		out := IssueRowDefaultFields()
		if opts.CrossProject {
			// Appended rather than inserted: position in this slice is
			// presentation only — the wire shape is a JSON object.
			out = append(out, IssueRowFieldProjectID)
		}
		return out, nil
	}

	// The identifier is returned always — including for an empty
	// selection, which US-2 defines as "the minimal recognisable minimum".
	out := make([]string, 0, len(opts.Fields)+1)
	out = append(out, IssueRowFieldID)
	seen := map[string]bool{IssueRowFieldID: true}

	for _, name := range opts.Fields {
		if !known[name] {
			return nil, &UnknownIssueRowFieldError{Field: name, Known: sortedFieldNames(known)}
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// issueRowKnownFields is the universe of names a carrier accepts: every
// JSON key its type declares, plus the injectable counters.
//
// Derived by reflection rather than listed, for the same reason the
// projection marshals the real type: a field added to issues.IssueSummary
// becomes selectable immediately, and the decision about whether it joins
// the DEFAULT set is forced separately by T006's drift-guard. A
// hand-maintained list would answer "unknown field" for a field that
// demonstrably exists.
//
// Note this is deliberately WIDER than the default set. `fields` is the
// escape hatch of INV-4 — the named path to data the default omits — so
// refusing to serve a field merely because it is not in the default would
// close the very door the invariant requires to stay open.
func issueRowKnownFields(carrier any) map[string]bool {
	known := make(map[string]bool, 24)
	for name := range issueRowInjectedFields {
		known[name] = true
	}
	collectJSONFieldNames(reflect.TypeOf(carrier), known)
	return known
}

// issueRowDeclarableFields is the universe a NotProvided name may come
// from: everything the carrier accepts, PLUS every row key the projection
// itself knows about.
//
// It is deliberately wider than `known`. Declaring a key the carrier does
// not have is the mechanism, not a mistake: dependencies.IssueSummary has
// no `isBlocked` at all, and the ready surface says so by naming it
// not-provided — that is how a default-set field the carrier cannot supply
// is turned from a silent omission into a stated one. So the check here
// asks a narrower question than «is it on this struct»: is this a row key
// at all, or a typo.
func issueRowDeclarableFields(known map[string]bool) map[string]bool {
	out := make(map[string]bool, len(known)+len(issueRowDefaultFields)+1)
	for name := range known {
		out[name] = true
	}
	for _, name := range issueRowDefaultFields {
		out[name] = true
	}
	out[IssueRowFieldProjectID] = true
	return out
}

// collectJSONFieldNames walks a struct type and records the JSON name of
// every exported field, following pointers and embedded structs.
func collectJSONFieldNames(t reflect.Type, into map[string]bool) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			collectJSONFieldNames(f.Type, into)
			continue
		}
		if f.PkgPath != "" {
			continue // unexported — never on the wire
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		into[name] = true
	}
}

// sortedFieldNames returns a name-set's members in a stable order, so an
// error message reads the same on every run.
func sortedFieldNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
