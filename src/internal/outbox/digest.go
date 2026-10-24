package outbox

// digest.go — sk-303.14, task T015: the SECOND PAYLOAD KIND of the
// delivery tract.
//
// # What this wave changes in the tract, and why it is not a special case
//
// Until this wave the tract could do exactly one thing: "one event — one
// issue". It resolved the event to a single issue (issueForEvent), built a
// title, body and link from it, and finalised an event that did not resolve
// to an issue as a configuration refusal without delivering. A digest speaks
// of SEVERAL issues, is addressed to a person rather than to a project, and
// leads to a surface rather than to a card — so this is a second delivery
// kind, not a special case of the first (R-2, the spec's DRAFT ADR).
//
// A queue of its own and a direct send were both rejected by the owner and
// reconfirmed: either would have forced a DUPLICATE of the unsubscribe check
// — the most consequential part of the tract — and two implementations drift
// apart over time, after which the system starts writing to people who
// unsubscribed. The cost is accepted on purpose: the tract stops being
// single-shaped. The price is paid once, and it is paid here.
//
// # Re-check at delivery time — why per-element, and why fail-closed
//
// INV-14: access is checked at delivery time, per element, and fail-closed.
// Between the nudge worker's selection and the actual send the person may
// have lost access to a project, and an issue may have left the set: an
// acceptance mark appeared, the issue was reassigned, closed, reviewed, the
// flag was cleared, the author declined it, the mechanic was switched off.
// The snapshot from selection time is NOT used: a stale reminder ("you have
// 1 unreviewed" after the person has already reviewed it) is not allowed by
// the product.
//
// Three consequences follow, each of them testable:
//
//   - an inaccessible issue is revealed by NEITHER its identifier NOR its
//     title — it simply never leaves the database, because the predicate
//     sits in the WHERE, not in a filter after the read;
//   - the count and the list remainder are computed AFTER the re-check, not
//     taken from the event payload;
//   - a READ failure leaves the row leased and is retried. Not "send as-is":
//     the one thing this gate exists for is not to hand data to someone who
//     no longer has a right to it.
//
// Membership predicates are NOT rewritten here. Both live as a single
// instance in internal/review (INV-3, task T012) and are spliced as text
// into this query's WHERE. A copy of the predicate here would be a fourth
// consumer with its own opinion of what "unreviewed" means — exactly the
// way three surfaces of the previous wave drifted apart.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
	"skein-api/internal/notifications"
	"skein-api/internal/push"
	"skein-api/internal/review"
)

// ---------------------------------------------------------------------------
// List limit and length budget
// ---------------------------------------------------------------------------

const (
	// digestListLimit — how many identifiers the digest shows as a list
	// before the remainder goes as a number.
	//
	// The spec names a RANGE, not a number: "no fewer than three and no more
	// than ten identifiers, the remainder as a number". A range, not a
	// value, because the bound that is being tested is a product one (the
	// list must exist and must be short), and the concrete value inside it
	// is a mechanism question. Five is the middle: three is already almost
	// "just a number", ten on a mobile screen does not fit the three grey
	// lines the OS gives a notification body.
	digestListLimit = 5

	// digestPayloadHeadroomBytes — slack between the builder's budget and
	// the transport ceiling.
	//
	// The slack is there because the ceiling is reached at exactly the
	// moment the message stops going out: webpush-go returns
	// ErrMaxPadExceeded BEFORE touching the network, and in the outcomes
	// that looks like a transport failure — which sends incident triage
	// toward a provider nobody bothered. Half a kilobyte covers what the
	// budget does not own: the title, the tap URL (grows with
	// APP_BASE_URL), the reason key and JSON escaping.
	digestPayloadHeadroomBytes = 512

	// digestPayloadBudgetBytes — the digest's length budget, in bytes of
	// the finished JSON payload.
	//
	// What is measured is the payload as a whole, not the body alone:
	// it is the payload that is encrypted and that hits the record
	// ceiling, not the body string. The budget is DERIVED from the
	// web-push record size (push.MaxPayloadBytes, itself derived from
	// webpush.MaxRecordSize minus the coding header, the GCM tag and
	// the padding limiter), not assigned: a hand-assigned constant
	// drifts from the library in silence.
	digestPayloadBudgetBytes = push.MaxPayloadBytes - digestPayloadHeadroomBytes
)

// digestEllipsis is appended to a truncated issue title when a one-element
// digest does not fit the budget because of the title's length. It NEVER
// applies to the list: the list is shortened only by whole identifiers.
const digestEllipsis = "…"

// digestRemainderSep is the tail that carries the unlisted remainder as a
// number (" and N more"). Distinctive on purpose so a parser can split the
// list from the tail without guessing.
const digestRemainderSep = " and "

// ---------------------------------------------------------------------------
// Tap routes
// ---------------------------------------------------------------------------

// Routes are named one-per-case, and that is a contract with the client, not
// decoration: "the target state matches where the control's tap leads".
// Query-parameter values are the dictionary in
// skein-frontend/src/lib/shellUrlState.ts (LENS_PARAM / SELECT_PARAM /
// TAB_PARAM and their closed value sets); they must not drift, or a tap on
// the notification opens something other than what the control shows.
const (
	// digestRouteInbox — several UNACCEPTED issues lead to the inbox.
	digestRouteInbox = "/?tab=inbox"
	// digestRouteUnreviewed — several UNREVIEWED results lead to the
	// delegated surface with the unreviewed selection APPLIED. Two
	// parameters, not one: the lens says WHICH surface, the selection
	// says which subset of it.
	digestRouteUnreviewed = "/?lens=delegated&select=unreviewed"
)

// digestCategoryText is everything the two categories differ by in copy and
// navigation.
//
// The table is two rows on purpose: CATEGORIES DO NOT MERGE. An unaccepted
// digest and an unreviewed digest are two separate notifications with
// different reasons, different unsubscribe switches and different routes, not
// one text with two sections. Merging them would mean unsubscribing from one
// category silences the other.
type digestCategoryText struct {
	// countLabel — the subject of the count line.
	//
	// A NOUN PHRASE, NEVER A PAST-TENSE VERB. This is a notification
	// contract, not style: the original Russian copy forbade past-tense
	// verbs because they carry grammatical gender and users carry none, so
	// any verb form misgenders half the team. English does not have that
	// gender, but the same rule still holds — labels name the STATE
	// ("Issues awaiting acceptance"), they do not narrate an action
	// ("accepted two of them"). The same rule, for the same reason, sits
	// on the status labels in push.go — do not "fix" either into verbs.
	countLabel string
	// route — the tap path for a digest OF SEVERAL issues.
	route string
}

var digestTexts = map[review.Category]digestCategoryText{
	review.CategoryUnaccepted: {
		countLabel: "Issues awaiting acceptance",
		route:      digestRouteInbox,
	},
	review.CategoryUnreviewed: {
		countLabel: "Results awaiting review",
		route:      digestRouteUnreviewed,
	},
}

// ---------------------------------------------------------------------------
// Shared access expressions
// ---------------------------------------------------------------------------

// projectVisibleExpr — "the recipient can still see this project": the
// recipient filter of the access relation (authz.RecipientVisibleExpr,
// sk-674.18 T010) — the per-row access check on a single-issue delivery and
// the per-element re-check of an unaccepted digest.
//
// projectExpr is the expression that yields the project id: a placeholder
// when checking one row, and the column i.project_id on a per-element
// re-check.
func projectVisibleExpr(userPh, projectExpr string) string {
	return authz.RecipientVisibleExpr(userPh+"::uuid", projectExpr)
}

// ---------------------------------------------------------------------------
// Digest assembly
// ---------------------------------------------------------------------------

// digestElement is one element that survived the re-check.
type digestElement struct {
	id    string
	title string
}

// digestEventPayload reads the raw payload of a digest event.
//
// Three outcomes are told apart on purpose: a read error (retry), the event
// is gone (terminal — the event and the notification are written in one
// transaction, so a vanished event is gone for good), the payload was read.
func (r *PushRunner) digestEventPayload(ctx context.Context, eventID int64) ([]byte, bool, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx,
		`SELECT payload FROM events WHERE id = $1`, eventID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading digest event payload: %w", err)
	}
	return raw, true, nil
}

// digestElements is ONE query that answers both of INV-14's questions at
// once: which of the listed issues the recipient may still see, and which
// still belong to their category's set.
//
// The membership predicate is taken from internal/review, one per category,
// and that is why the two branches are NOT symmetric — the asymmetry is not
// accidental and is not fixed by adding a condition:
//
//   - the UNREVIEWED fragment is self-contained: the visibility horizon
//     (the access relation), project archival and the mechanic's
//     off-switch sit INSIDE it. Adding another access check here would
//     be a second copy of the same condition;
//   - the UNACCEPTED fragment carries no horizon, and that is correct: its
//     only public consumer is the reminder selection, where the recipient
//     is by construction the issue's assignee. The tract adds the access
//     check for it — with the same expression it uses on a single-issue
//     delivery.
//
// Order is set by the QUERY, not by the event payload. That is how "the
// same set of issues yields the same text" holds regardless of the order
// the nudge worker stacked the identifiers, and is stable across segments:
// two segments with the same set produce a byte-identical body.
func (r *PushRunner) digestElements(
	ctx context.Context, cat review.Category, userID uuid.UUID, ids []string,
) ([]digestElement, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	idsPh := bind(ids)

	var membership string
	switch cat {
	case review.CategoryUnaccepted:
		membership = review.UnacceptedPredicate(bind, review.UnacceptedArgs{
			Alias:       "i",
			AddresseeID: userID,
		})
		membership += " AND " + projectVisibleExpr(bind(userID), "i.project_id")
	case review.CategoryUnreviewed:
		membership = review.UnreviewedPredicate(bind, review.UnreviewedArgs{
			Alias:    "i",
			CallerID: userID,
		})
	default:
		return nil, fmt.Errorf("%w: %q", review.ErrUnknownCategory, string(cat))
	}

	sql := `
SELECT i.id, i.title
  FROM issues i
 WHERE i.id = ANY(` + idsPh + `::text[])
   AND ` + membership + `
 ORDER BY i.id`

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("re-checking digest elements: %w", err)
	}
	defer rows.Close()

	out := make([]digestElement, 0, len(ids))
	for rows.Next() {
		var el digestElement
		if err := rows.Scan(&el.id, &el.title); err != nil {
			return nil, fmt.Errorf("scanning digest elements: %w", err)
		}
		out = append(out, el)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("re-checking digest elements: %w", err)
	}
	return out, nil
}

// buildDigest takes one leased digest row to a finished payload — or
// finalises it itself and tells the caller to exit silently.
//
// ok=false means "nothing left to do": the row has either already been
// finalised, or has been LEFT leased ON PURPOSE so the next tick picks it
// up. The second is fail-closed: we could not read access, so we do not
// send and we do not invent, we try again, bounded by attempts and the
// deadline like any other failure.
func (r *PushRunner) buildDigest(
	ctx, work context.Context, c pushClaim, cat review.Category,
) ([]byte, bool) {
	raw, found, err := r.digestEventPayload(work, c.eventID)
	if err != nil {
		slog.WarnContext(ctx, "digest deferred: event payload unreadable",
			"notificationId", c.id, "eventId", c.eventID, "error", err.Error())
		return nil, false
	}
	if !found {
		slog.WarnContext(ctx, "digest not sent: the event is gone",
			"notificationId", c.id,
			"eventId", c.eventID,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
		)
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeEventUnresolvable)
		return nil, false
	}
	dp, err := notifications.ParseDigestPayload(raw)
	if err != nil {
		// Read succeeded, parse did not: the payload was not written to
		// the contract and will not fix itself. Terminal — otherwise the
		// row would wake the runner every lease until the deadline and
		// be recorded as a transport failure, blaming the provider for
		// somebody else's mistake.
		slog.ErrorContext(ctx, "digest not sent: malformed event payload",
			"notificationId", c.id,
			"eventId", c.eventID,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"error", err.Error(),
		)
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeEventUnresolvable)
		return nil, false
	}

	els, err := r.digestElements(work, cat, c.userID, dp.IssueIDs)
	if err != nil {
		// FAIL-CLOSED. The row stays leased: the lease expires and it is
		// claimed again. Sending "as-is" here would reveal identifiers
		// and titles to someone whose access we could not read.
		slog.WarnContext(ctx, "digest deferred: element re-check failed",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"category", cat.String(),
			"error", err.Error(),
		)
		return nil, false
	}

	if dropped := len(dp.IssueIDs) - len(els); dropped > 0 {
		// Not an error, the gate doing its job: the count and the
		// remainder are computed from the survivors, not from the
		// payload. Logged because "the person received fewer than the
		// selection held" is otherwise unexplainable from the outside.
		slog.InfoContext(ctx, "digest elements dropped at delivery",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"category", cat.String(),
			"selected", len(dp.IssueIDs),
			"delivered", len(els),
			"dropped", dropped,
		)
	}

	if len(els) == 0 {
		// Everything emptied — the notification is NOT sent and is
		// recorded as an outcome, not as empty copy. "You have 0
		// unreviewed" is exactly the stale reminder the product does
		// not allow.
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeDigestEmpty)
		return nil, false
	}

	built, err := marshalDigestPayload(reasonLabel(c.reason), cat, els, r.appBaseURL)
	if err != nil {
		if errors.Is(err, errDigestOversize) {
			// LOUD AND TERMINAL. Handing the transport something that
			// cannot fit is forbidden: webpush-go will reject it before
			// the network, and the row will be recorded as a transport
			// failure, blaming the provider. Retrying is pointless too
			// — assembly is deterministic, the next lease will build
			// the same row and arrive at the same place, only through
			// the deadline and with the same false outcome.
			slog.ErrorContext(ctx, "digest not sent: does not fit the transport budget",
				"notificationId", c.id,
				"userId", c.userID.String(),
				"reason", reasonLabel(c.reason),
				"category", cat.String(),
				"elements", len(els),
				"error", err.Error(),
			)
			r.finalise(ctx, c, outcomeConfigurationNoSend, causeDigestOversize)
			return nil, false
		}
		slog.ErrorContext(ctx, "digest not sent: payload marshal failed",
			"notificationId", c.id, "error", err.Error())
		r.retryOrExhaust(ctx, c)
		return nil, false
	}
	return built, true
}

// marshalDigestPayload builds the payload and FITS IT INTO THE BUDGET,
// shortening the list by WHOLE identifiers.
//
// The list is never cut in the middle — of an identifier or of a separator:
// the loop drops an element whole and recomputes the remainder, it does not
// slice the string by bytes. A mid-identifier cut is worse than no list: it
// looks like a real identifier and leads nowhere.
//
// What is measured is the FINISHED payload, not the body length, because it
// is the payload that hits the record ceiling: the title, the tap URL, the
// reason key and JSON escaping take space on a par with the text.
//
// An issue identifier in the system is varchar(1000), so FIVE identifiers
// legally do not fit a web-push record. This is not a theoretical edge:
// without fitting, such a digest would not go out at all and would be
// recorded as a transport failure. THE BUDGET WINS. The spec asks for a
// list of "no fewer than three identifiers" and separately requires fitting
// a web-push record, and at maximum-length identifiers those two
// requirements conflict. Which one yields — the spec does not say, so the
// choice is made here and recorded here:
//
//	a short list  <  an unsent notification  <  a false attribution.
//
// The "no fewer than three" requirement is about the typical case, not the
// degenerate one: a person whose identifiers are a kilobyte each still
// benefits from a list of two, and an unsent notification benefits nobody.
// Worse than both is handing the transport something that cannot fit:
// webpush-go rejects it BEFORE touching the network, the row is recorded
// as a "transport failure" outcome, and incident triage goes to a provider
// nobody bothered.
//
// So the retreat order is: full list → one element shorter → … → COUNT-ONLY
// FORM, no identifiers (listed == 0). The "and N more" tail is deliberately
// not written at that step: there is no remainder — the count already names
// EVERYTHING, and "Issues awaiting acceptance: 7 and 7 more" would be a lie.
//
// If even that does not fit — that is a DEFECT, not a mode of operation, and
// it must be loud: an error outward, a word in the log and a terminal
// outcome with its own sub-cause. Silently handing over something that
// cannot fit is the one option this forbids.
func marshalDigestPayload(
	reason string, cat review.Category, els []digestElement, appBaseURL string,
) ([]byte, error) {
	listed := digestListLimit
	if listed > len(els) {
		listed = len(els)
	}
	for {
		p := renderDigestPayload(reason, cat, els, listed, appBaseURL)
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("marshalling digest payload: %w", err)
		}
		if len(raw) <= digestPayloadBudgetBytes {
			return raw, nil
		}
		if listed == 0 {
			return nil, fmt.Errorf("%w: %d bytes over a budget of %d",
				errDigestOversize, len(raw), digestPayloadBudgetBytes)
		}
		listed--
	}
}

// errDigestOversize — the payload does not fit the budget EVEN in the
// count-only form. A separate error, not a generic one: it is terminal and
// not retryable (assembly is deterministic — the next attempt will build
// the same row), whereas a serialisation error can in principle be
// transient.
var errDigestOversize = errors.New("digest payload does not fit the transport budget in its shortest form")

// renderDigestPayload builds the payload, showing the first listed
// identifiers.
//
// ONE issue — the notification leads to its card and looks like an ordinary
// issue notification: the same "identifier · title" order in the body, the
// same URL. That is not a simplification, it is a contract requirement —
// the person must not be able to tell from the shape of the notification
// that a one-element digest is inside.
//
// SEVERAL — the notification leads to its category's surface, and the body
// carries the count, the list and the remainder as a number.
func renderDigestPayload(
	reason string, cat review.Category, els []digestElement, listed int, appBaseURL string,
) pushPayload {
	title := pushHead(reason, eventIssue{})

	if len(els) == 1 {
		el := els[0]
		return singleElementPayload(reason, title, el.id,
			fitDigestTitle(reason, title, el.id, el.title, appBaseURL), appBaseURL)
	}

	txt := digestTexts[cat]
	body := txt.countLabel + ": " + strconv.Itoa(len(els))
	if listed > 0 {
		ids := make([]string, 0, listed)
		for _, el := range els[:listed] {
			ids = append(ids, el.id)
		}
		body += " · " + strings.Join(ids, ", ")
		if rest := len(els) - listed; rest > 0 {
			body += digestRemainderSep + strconv.Itoa(rest) + " more"
		}
	}

	return pushPayload{
		Title:    title,
		Body:     body,
		URL:      digestURL(appBaseURL, txt.route),
		Category: reason,
		// IssueID is empty on purpose: there is no single issue a digest
		// of several points at. The client service worker reads this
		// field as a grouping tag and, when it is empty, falls back to
		// the category — so a new digest of the same category REPLACES
		// the previous one on the device, instead of sitting next to it
		// as a second card.
		IssueID: "",
	}
}

// fitDigestTitle shortens the issue title if a one-element digest does not
// fit the budget because of it.
//
// The title is free text with no upper length bound, so one issue with a
// huge title would hit the record ceiling the same way five identifiers
// would. This does not apply to the list: the list is shortened by whole
// identifiers, and a title is ordinary text, so an ellipsis in it reads as
// an ellipsis, not as a foreign identifier.
func fitDigestTitle(reason, pushTitle, issueID, issueTitle, appBaseURL string) string {
	// The probe is built by THE SAME function as the outgoing payload. A
	// hand-copied one-element branch would measure a shape that, after the
	// first edit of the builder, is no longer the one that goes to the
	// sender — and would measure it in silence.
	probe := func(t string) int {
		raw, err := json.Marshal(singleElementPayload(reason, pushTitle, issueID, t, appBaseURL))
		if err != nil {
			return digestPayloadBudgetBytes + 1
		}
		return len(raw)
	}
	if probe(issueTitle) <= digestPayloadBudgetBytes {
		return issueTitle
	}

	// Binary search on the BOUNDARY, not successive halving. Halving
	// tries 1/2, 1/4, 1/8 … and takes the first that fits, which on
	// average leaves a fifth of the budget unused — the person sees half
	// as much of the title as would fit. Same number of iterations.
	//
	// Cut on RUNES: a byte slice on Cyrillic splits a character in half
	// and puts a broken string in the notification.
	runes := []rune(issueTitle)
	lo, hi := 0, len(runes) // lo fits (checked below), hi does not
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if probe(string(runes[:mid])+digestEllipsis) <= digestPayloadBudgetBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		// Even a single ellipsis does not fit: the title is not the
		// problem, there is no room for the identifier itself. An
		// ellipsis is returned anyway — "the body was truncated" is
		// more honest to the person than emptiness — and the payload
		// that still does not fit is rejected by marshalDigestPayload,
		// not by this function.
		return digestEllipsis
	}
	return string(runes[:lo]) + digestEllipsis
}

// singleElementPayload is the ONLY assembly of a one-element digest payload.
// Both the renderer and the title-fitting probe call it: two copies of one
// shape drift in silence, and a drifted probe measures something other than
// what goes out.
func singleElementPayload(reason, pushTitle, issueID, issueTitle, appBaseURL string) pushPayload {
	return pushPayload{
		Title:    pushTitle,
		Body:     issueID + " · " + issueTitle,
		URL:      digestURL(appBaseURL, "/issues/"+issueID),
		Category: reason,
		IssueID:  issueID,
	}
}

// digestURL joins the tap URL by the same rule as a single-issue delivery:
// absolute when a base is configured, otherwise relative — the client
// service worker resolves it against its own origin.
func digestURL(appBaseURL, path string) string {
	if base := strings.TrimRight(appBaseURL, "/"); base != "" {
		return base + path
	}
	return path
}
