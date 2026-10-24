package tools

// ack.go — the acknowledgement contract for write-tools (sk-282 T007,
// spec US-5).
//
// # What a write-tool owes its caller
//
// Before sk-282, a confirming tool answered with the issue card — around
// forty-five keys — of which the caller already held most: it had just sent
// them. US-5 splits what comes back in two, and the split is the whole
// rule:
//
//	FORBIDDEN — free text the caller is holding anyway: the close reason,
//	  the source context, the description, notes, acceptance criteria, a
//	  comment body. It is an ECHO. It sits in the agent's context twice
//	  (as the argument and as the result) and is re-sent on every later
//	  turn of the session.
//
//	REQUIRED — authoritative post-state that proves the server applied the
//	  write: status, version, the lock and claim stamps, the identifiers
//	  of what was created. This is NOT an echo — the caller cannot derive
//	  the new version from its own request, and INV-7 says the optimistic
//	  cycle must continue without an extra read.
//
// # The shape is not invented here
//
// The batch tools already answer in exactly the target form: one row per
// issue `{id, ok, version, structured error}` plus a summary
// `{requested, succeeded, failed}` (close_many.go). US-5 names that the
// reference form and draws two conclusions: single acknowledgements are
// brought TO it, and the batch tools are NOT touched — "fixing what is
// already right is forbidden". So this file builds on batchResultRow /
// batchSummary rather than beside them, and changes neither.
//
// # Why an allowlist here, when the row projections use denylists
//
// issuerow.go removes named keys from the real type, so a new field flows
// through automatically — right for a projection that is meant to be
// "everything except". An acknowledgement is the opposite: it is meant to
// be "these few things only", and a denylist would let every future field
// onto a response whose entire purpose is being small. SC-006 (a close
// whose service part is under 200 characters) is unreachable any other
// way.
//
// That makes the acknowledgement an UNCONDITIONAL narrowing, which INV-1
// covers with its third marker form — a uniform documented contract, with
// the way to the full data named in the tool's description. No per-response
// flag is needed or wanted: a flag that is always set carries no
// information and costs bytes on every call. The way out (INV-4) is
// `verbose=true`, which returns the previous full response untouched.
//
// # What this file deliberately does NOT do
//
// It is now the ONLY shape a write-tool of the US-5 class returns. Group
// G-A4 (T020–T025) wired every one of them, including the four call sites
// that used to bypass the common path (create, the close warning branch,
// the claim batch branch, epic_create), and removed the card-shaped
// helper those sites could otherwise have kept using — applying the class
// to only some of them is the partial application FP-2 and FP-9 warn
// about, and a second serialisation point left lying around is how that
// arrives a release later.

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/issues"
)

// ackRow is a single-issue acknowledgement.
//
// The first four fields ARE batchResultRow's, name for name, so a caller
// that can parse a batch row can parse this. They are spelled out rather
// than embedded because embedding a struct with a `json` tag-less
// promotion would still produce a flat object, but would tie the two
// shapes together so that any future change to the batch row silently
// changed every single acknowledgement too — and US-5 forbids touching the
// batch form at all.
//
// The remaining fields are the authoritative post-state US-5 requires.
// Every one of them is `omitempty` for a reason: a close has no lock
// stamps, a claim has no closed stamp, and emitting explicit nulls would
// spend bytes to say "not applicable" on every call.
type ackRow struct {
	// ID is the issue the write applied to.
	ID string `json:"id"`
	// OK reports whether the write succeeded. Always true on the single
	// path today (a failure is returned as a tool error envelope), but
	// present so the row is readable by the same code as a batch row.
	OK bool `json:"ok"`
	// Version is the post-write version — the token for the caller's next
	// conditional update (INV-7). Never omitted on a success.
	Version int `json:"version"`
	// Error carries the structured failure when OK is false, in the same
	// shape the batch rows use.
	Error *batchErrorDTO `json:"error,omitempty"`

	// Status is the authoritative post-state: what the issue IS now, not
	// what the caller asked it to become.
	Status string `json:"status,omitempty"`
	// Assignee is the post-write owner. Present because claim sets it and
	// the caller may not have named it.
	Assignee *string `json:"assignee,omitempty"`
	// LockedBy / LockedAt are the lock stamps — "lock stamps" in
	// US-5's required column. A claim's whole point is that they moved.
	LockedBy *string    `json:"lockedBy,omitempty"`
	LockedAt *time.Time `json:"lockedAt,omitempty"`
	// StartedAt is the claim stamp: when the issue first entered work.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// ClosedAt is the close stamp. ClosedReason is deliberately NOT here —
	// it is the free text the caller just sent.
	ClosedAt *time.Time `json:"closedAt,omitempty"`
	// AcceptedAt is the acceptance stamp. It is set by ackFromAcceptedIssue
	// ONLY — see that function for why it is not part of the common row.
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	// Dismissed is the CALLER'S OWN membership in the issue's dismissed_by
	// set after the write: true after skein_drop_delegated, false after
	// skein_resume_delegated. Set by ackFromDelegatedIssue ONLY — see that
	// function for why the two delegation tools need it and why nothing
	// else carries it.
	//
	// A *bool rather than a bool: `omitempty` erases a plain false, and
	// "false" is exactly what the resume path has to prove. A non-nil
	// false serialises as `"dismissed":false`; nil keeps the key off every
	// other surface's response.
	Dismissed *bool `json:"dismissed,omitempty"`

	// CloseOutcome is the issue's outcome AFTER the write (sk-489, US-9):
	// one of done / wont_do / duplicate / superseded.
	//
	// It is post-state the caller cannot derive from its own request —
	// omitting the outcome on a transition yields `done`, omitting it on a
	// re-close preserves whatever was there, and an archive yields
	// `wont_do` — so US-5's required column covers it exactly the way it
	// covers `status`.
	//
	// Same Go type as IssueDetail.CloseOutcome (*string), deliberately: the
	// drift guard asserts the acknowledgement's vocabulary against the
	// issue's, and a different type here would mean the two forms had
	// diverged about the same fact.
	//
	// Set by ackFromClosedIssue ONLY — the three surfaces that can close.
	// On every other acknowledgement the key is absent, which is honest
	// rather than economical: those tools do not close, so «this issue's
	// outcome» is not part of what they applied.
	CloseOutcome *string `json:"closeOutcome,omitempty"`
	// CloseLink says what happened to the close-target link THIS CALL
	// asked for: "created" (the edge is new) or "existed" (an edge of that
	// type and direction was already there, so none was added and the
	// close still proceeded, US-2).
	//
	// It is DERIVED, not a field of the issue: it describes the operation,
	// not the row, and two identical calls legitimately answer differently.
	// That is why the empty value is CloseLinkNone — with `omitempty` the
	// key simply does not appear on a call that named no target, so
	// «no target» and «target, nothing to do» stay distinguishable.
	CloseLink issues.CloseLinkState `json:"closeLink,omitempty"`
}

// ackForbiddenTextFields are the free-text keys US-5 forbids on an
// acknowledgement, by their wire names.
//
// They are listed here — rather than only being absent from ackRow —
// because "absent because we decided so" and "absent because someone
// forgot" look identical in a struct definition. The test asserts against
// this list, so the prohibition is checkable rather than implied.
var ackForbiddenTextFields = []string{
	// US-5's list, verbatim.
	"closedReason",       // close reason
	"sourceContext",      // source context
	"description",        // description
	"notes",              // notes
	"acceptanceCriteria", // acceptance criteria
	"body",               // comment body

	// Not named by US-5, added because it is the fourth member of the
	// issue's prose quartet: the spec lists description, notes and
	// acceptance criteria, and leaving `design` off a list whose three
	// siblings are on it would be an omission, not a decision.
	"design",
}

// ErrAckNilIssue is returned when an acknowledgement is asked for without
// a post-state to build it from.
//
// It is an ERROR and not a zero row, because a zero row is a LIE that
// passes for a success: `{"id":"","ok":false,"version":0}` carries none of
// the three things INV-7 says an acknowledgement must carry (identifier,
// authoritative post-state, version), and `ok:false` with no `error`
// beside it is a failure with no reason — the very shape this file
// declares the reference form for.
//
// The condition it names is real, not defensive: G-A4 wires six surfaces
// to this helper, and the way each of them can go wrong is skipping the
// post-write load. That mistake must reach the caller as a tool error, not
// as a confirmation that the write applied.
var ErrAckNilIssue = errors.New("acknowledgement requires the post-write issue state, got nil")

// ackFromIssue builds the acknowledgement for a successful single-issue
// write.
//
// It takes the freshly-loaded IssueDetail — the post-state, not the
// caller's arguments — and keeps only what US-5's required column names.
func ackFromIssue(d *issues.IssueDetail) (ackRow, error) {
	if d == nil {
		return ackRow{}, ErrAckNilIssue
	}
	return ackRow{
		ID:      d.ID,
		OK:      true,
		Version: d.Version,

		Status:    d.Status,
		Assignee:  d.Assignee,
		LockedBy:  d.LockedBy,
		LockedAt:  d.LockedAt,
		StartedAt: d.StartedAt,
		ClosedAt:  d.ClosedAt,
	}, nil
}

// ackFromAcceptedIssue is ackFromIssue plus the acceptance stamp.
//
// # Why the acceptance stamp is not on the common row (sk-282 T021)
//
// It was, in the change that introduced this file, and measuring the close
// against SC-006 is what showed it cannot stay. A realistic close — an
// issue that was claimed, therefore accepted, therefore closed — carries
// THREE microsecond-precision timestamps, and each costs 42 characters on
// the wire. The measurement, on a live row rather than a fixture with
// whole-hour times:
//
//	{"id":"sk-282.18.4.3","ok":true,"version":3,"status":"closed",
//	 "assignee":"alice","startedAt":…,"closedAt":…,"acceptedAt":…}
//	  → 207 characters. SC-006's budget is 200. Without the acceptance
//	    stamp: 164.
//
// The tie is broken by US-5 rather than by the eight characters. Its
// REQUIRED column is exact — "status, version, lock and claim stamps,
// identifiers of what was created" — and an ACCEPTANCE stamp is none
// of those: it is neither the lock nor the claim, and on a close, an
// update or a reparent it is unchanged history the caller did not ask
// about. So it is not "trimmed to fit"; it was never in the declared
// contract.
//
// It stays for exactly one surface. skein_accept moves nothing else —
// status does not change and a repeat call is an idempotent no-op with no
// version bump — so without this stamp its acknowledgement would carry no
// proof at all that the accept applied, which is the one thing US-5 says
// an acknowledgement is for.
func ackFromAcceptedIssue(d *issues.IssueDetail) (ackRow, error) {
	row, err := ackFromIssue(d)
	if err != nil {
		return ackRow{}, err
	}
	row.AcceptedAt = d.AcceptedAt
	return row, nil
}

// ackFromDelegatedIssue is ackFromIssue plus the caller's own membership
// in dismissed_by — the acknowledgement for skein_drop_delegated and
// skein_resume_delegated.
//
// # Why these two need their own row (sk-282 T024 review)
//
// The exception granted to skein_accept was granted by a RULE, not by
// sympathy: an acknowledgement has to prove the server applied the write,
// and skein_accept moves nothing the common row carries — status does not
// change, a repeat call is an idempotent no-op with no version bump — so
// without the acceptance stamp its confirmation would prove nothing.
//
// The delegation pair fails that same rule, and the change shipped without
// noticing. Neither store method bumps the version, neither touches
// status, assignee or the lock stamps, and dismissed_by is not on the
// common row — so, measured on a live database, drop and resume answered
// BYTE-FOR-BYTE identically, and identically to the state before either
// call. Under the previous full-card response the caller could at least
// read `dismissedBy` out of the card; the acknowledgement removed that
// without putting anything in its place.
//
// So the rule is applied where it already pointed instead of gaining a
// second, silent exception: the row carries the one piece of authoritative
// post-state these tools actually move.
//
// It is the CALLER'S membership, not the whole array. The array is other
// users' identities — a permission surface, not this caller's post-state —
// and on a heavily-delegated issue it is unbounded weight on a response
// whose entire purpose is being small. The boolean answers the only
// question the caller can act on: after this call, is the issue in my
// Delegated lens or not.
//
// MCP-only projection (INV-2): the REST dismiss / resume endpoints keep
// returning the untouched IssueDetail, dismissedBy array included.
func ackFromDelegatedIssue(caller uuid.UUID) func(*issues.IssueDetail) (ackRow, error) {
	return func(d *issues.IssueDetail) (ackRow, error) {
		row, err := ackFromIssue(d)
		if err != nil {
			return ackRow{}, err
		}
		dismissed := slices.Contains(d.DismissedBy, caller)
		row.Dismissed = &dismissed
		return row, nil
	}
}

// ackFromClosedIssue is ackFromIssue plus the close outcome and the
// close-target link state — the acknowledgement for the three surfaces
// that can close an issue or edit its outcome (sk-489, US-9):
// skein_close, skein_close_many and skein_update.
//
// # Why a third exception rather than two more fields on the common row
//
// The precedent is ackFromAcceptedIssue / ackFromDelegatedIssue, and the
// rule those two established is «the row carries what THIS write moved».
// Eight of the eleven write-tools cannot close anything: skein_claim,
// skein_reparent, skein_commits_attach and the rest would carry
// `closeOutcome` as a fact about the issue's past, not about what they
// just applied — and on an issue that is not closed they would carry the
// key as absent, which reads as «this tool did not tell me» rather than
// «this issue has no outcome». Both readings are wrong for those tools.
//
// The limit of the precedent is stated because it is exceeded here:
// ackFromDelegatedIssue adds ONE field to ONE pair of tools, this adds TWO
// to THREE. The shape is the same and the justification is the same; the
// count is not, and a fourth such builder would be the point to ask
// whether the row wants a variant mechanism instead of a fourth closure.
//
// `link` is an argument rather than something read off the issue because
// it is not on the issue: it is the answer the WRITER returned about this
// call (issues.Store.CloseWithResult / PatchIssueWithResult).
func ackFromClosedIssue(link issues.CloseLinkState) func(*issues.IssueDetail) (ackRow, error) {
	return func(d *issues.IssueDetail) (ackRow, error) {
		row, err := ackFromIssue(d)
		if err != nil {
			return ackRow{}, err
		}
		row.CloseOutcome = d.CloseOutcome
		row.CloseLink = link
		return row, nil
	}
}

// ackPayload returns what a write-tool should serialise: the full
// IssueDetail when the caller asked for it, the acknowledgement otherwise.
//
// Its signature was chosen to match the card-shaped helper it replaced, so
// that G-A4 could convert the class one call site at a time instead of
// rewriting each; that helper is gone now, and the shape stays because
// every write-tool of the class calls through here.
//
// `verbose=true` returns the untouched IssueDetail: INV-4 requires a named
// way to the full data, and a "full" mode that is itself narrowed would
// not be one.
func ackPayload(d *issues.IssueDetail, verbose bool) (any, error) {
	return ackPayloadWith(d, verbose, ackFromIssue)
}

// ackPayloadWith is ackPayload with the row builder chosen by the caller,
// so the one surface that owes its caller the acceptance stamp
// (skein_accept) can say so at its own call site instead of widening the
// row for all eleven.
func ackPayloadWith(d *issues.IssueDetail, verbose bool, build func(*issues.IssueDetail) (ackRow, error)) (any, error) {
	// The nil check comes before the verbose split on purpose: a nil
	// detail serialised as the "full" response is the JSON literal `null`,
	// which is the same fake success as the zero row, just louder.
	if d == nil {
		return nil, ErrAckNilIssue
	}
	if verbose {
		return d, nil
	}
	return build(d)
}

// ackBatchResponse is the group acknowledgement: one acknowledgement row
// per input issue plus the same summary rollup the batch tools use.
//
// The rows are ackRow, not batchResultRow, and that is required rather
// than convenient: US-5's acceptance for the batch claim says the response
// carries "identifier, state, version and claim stamp, and nothing
// beyond" — a bare batch row has no state and no claim stamp, so it would
// drop the very proof the write applied. ackRow's first four keys ARE the
// batch row's, so the shape stays readable by the same code.
type ackBatchResponse struct {
	Results []ackRow     `json:"results"`
	Summary batchSummary `json:"summary"`
}

// ackBatchVerboseResponse is the group response under `verbose=true`:
// the SAME envelope as the acknowledgement, with the untouched
// IssueDetail in each row's place.
//
// The envelope is deliberately the same in both modes. On a group tool
// the envelope is itself part of what US-5 changes — the claim batch used
// to answer with a bare JSON array — so "the previous full response"
// cannot mean "the previous array": the array IS the thing being
// replaced. What INV-4 asks for is a named way to the FULL DATA, and the
// analogue of every single-issue tool is exact — same shape, the full
// detail in the acknowledgement's place. A mode that also swapped the
// envelope would leave an agent parsing two different shapes depending on
// a flag it may not have set itself.
type ackBatchVerboseResponse struct {
	Results []*issues.IssueDetail `json:"results"`
	Summary batchSummary          `json:"summary"`
}

// ackBatchPayload returns what a group write-tool should serialise: the
// full details when the caller asked for them, the group acknowledgement
// otherwise. Mirrors ackPayload for the group case.
func ackBatchPayload(details []*issues.IssueDetail, verbose bool) (any, error) {
	if !verbose {
		return ackBatchFromIssues(details)
	}
	// The same nil rule as ackBatchFromIssues, and for the same reason: a
	// nil slot serialised as `null` inside `results` is a row that claims
	// a write applied while carrying no identifier and no version.
	for i, d := range details {
		if d == nil {
			return nil, fmt.Errorf("verbose row %d: %w", i, ErrAckNilIssue)
		}
	}
	return ackBatchVerboseResponse{
		Results: details,
		Summary: batchSummary{
			Requested: len(details),
			Succeeded: len(details),
			Failed:    0,
		},
	}, nil
}

// ackBatchFromIssues builds the group acknowledgement for a successful
// group write.
//
// This exists for the group tools that do NOT already answer in the
// reference form — the claim batch and epic_create, whose responses are
// currently issue cards. It does NOT touch skein_close_many or
// skein_update_many: those already ARE the reference form, and US-5 says
// re-doing what is already right is forbidden.
//
// Atomicity is untouched by construction. This helper only projects rows
// the caller already decided on; it neither drives a transaction nor
// tolerates a partial batch. The claim batch and epic_create stay
// all-or-nothing — turning "claimed none" into "claimed nine of ten"
// would change what other agents see, which is a write-semantics change
// and out of scope (Q10 / FP-10). A failed group write never reaches this
// helper: it is still returned as a tool error, exactly as today, so
// Failed is always zero here.
// A nil slot fails the whole call rather than becoming a row: the summary
// counts every returned row as Succeeded, so a nil that produced a row
// would be reported as a write that applied while carrying no identifier
// and no version. The group tools are all-or-nothing, so "one slot has no
// post-state" is not a partial batch — it is a missing load, and the
// caller gets a tool error.
func ackBatchFromIssues(details []*issues.IssueDetail) (ackBatchResponse, error) {
	rows := make([]ackRow, 0, len(details))
	for i, d := range details {
		row, err := ackFromIssue(d)
		if err != nil {
			return ackBatchResponse{}, fmt.Errorf("acknowledgement row %d: %w", i, err)
		}
		rows = append(rows, row)
	}
	return ackBatchResponse{
		Results: rows,
		Summary: batchSummary{
			Requested: len(details),
			Succeeded: len(rows),
			Failed:    0,
		},
	}, nil
}
