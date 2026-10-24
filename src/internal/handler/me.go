package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/review"
	"skein-api/internal/spaces"
	"skein-api/internal/status"
)

// MeHandler hosts /api/v1/me/* endpoints that don't fit the existing
// notifications / issues / users handlers — currently the consolidated
// header-counts endpoint (Phase 2 ui-redesign B6) and the watching
// feed (sk-39). New header-bar endpoints land here unless they have a
// dedicated domain handler.
type MeHandler struct {
	pool        *pgxpool.Pool
	issuesStore *issues.Store
	notifStore  *notifications.Store
	// countsGate batches the header-count computations of one user
	// (sk-622.21 T024, K8) — see countsgate.go.
	countsGate *countsGate[notificationCountsResponse]
}

// NewMeHandler constructs a handler bound to the shared pool. The
// issues/notifications stores are optional — only the /me/watching
// route uses them; tests for the counts endpoint can still pass nil
// when wiring just that surface. main.go always provides both.
func NewMeHandler(pool *pgxpool.Pool, issuesStore *issues.Store, notifStore *notifications.Store) *MeHandler {
	return &MeHandler{
		pool:        pool,
		issuesStore: issuesStore,
		notifStore:  notifStore,
		countsGate:  newCountsGate[notificationCountsResponse](countsGateWindow),
	}
}

// BindCountsLifetime ties the header-counts gate to the service lifetime
// ctx: when ctx ends the gate opens no
// new batch and cancels every running computation; the returned channel
// closes once every computation has returned. main binds it to the workers'
// context and awaits the channel at shutdown with the other workers. Call it
// once.
func (h *MeHandler) BindCountsLifetime(ctx context.Context) <-chan struct{} {
	return h.countsGate.bindLifetime(ctx)
}

// notificationCountsResponse is the wire shape for
// GET /api/v1/me/notification-counts. Field order is informational
// only — JS clients access by name.
type notificationCountsResponse struct {
	// Inbox is the count of issues assigned to the caller that are
	// open and not yet accepted (assignee = me, accepted_at IS NULL,
	// status = 'open'). NOT a notifications-table query — the Inbox
	// surface is the live "needs my acknowledgement" set. sk-674.18 T029
	// (US-13): plus the pending invitations to the caller, and only tasks
	// of projects he may work in now (INV-30).
	Inbox int `json:"inbox"`
	// Mentions is the count of unread, non-archived mention
	// notifications (notifications.type IN ('comment.mention',
	// 'issue.mention')).
	Mentions int `json:"mentions"`
	// Watching is the count of unread, non-archived notifications that
	// carry a Watching card and were written inside the Watching window
	// (WATCHING_FEED_WINDOW_DAYS, sk-622.21 US-5). The rule is "there was a
	// subscription": a notification gets its card when it is written, if
	// the caller then had a subscription on the task it is about (the card
	// is that task) or a subtree subscription on an ancestor (the card is
	// the nearest such ancestor); mentions never get one. Authorship plays
	// no part. Reading a notification does not extend the window.
	//
	// Equal to GET /api/v1/me/watching's unreadTotal — same predicate and
	// window, see countWatching (INV-5).
	Watching int `json:"watching"`
	// OnMeOpenCount is the count of NON-CLOSED issues in the caller's
	// "On me" lens, aggregated across every project the caller can see
	// (admins see all). Lens predicate (sk-54): assignee = me AND
	// created_by <> me AND accepted_at IS NOT NULL AND status <> 'closed'.
	// Non-closed = open + in_progress + deferred. Drives the live "On me"
	// tab badge in the new shell. The row-level predicate is the EXACT same
	// SQL the per-project on_me_*_count counters use (internal/projects), so
	// the badge always equals the cross-project "On me" tab list (useTasks:
	// assignee=me, createdBy=not-me, acceptedAtNotNull) — the authoritative
	// source. It equals the SUM of the per-project pills only when the lens
	// holds no deferred-unblocked rows: the pills split open/in_progress and
	// carve blocked separately (omitting deferred-unblocked), whereas this is
	// a flat status<>'closed'. Explicitly-prefixed field name
	// per the CLAUDE.md counter-scoping convention — does NOT overload any
	// existing {metric}Count.
	OnMeOpenCount int `json:"onMeOpenCount"`
	// DelegatedOpenCount is the count of NON-CLOSED issues in the caller's
	// "Delegated" lens, aggregated across every visible project (admins
	// see all). Lens predicate (sk-54): assignee <> me AND status <>
	// 'closed' AND (created_by = me OR (me ∈ previous_assignees AND me ∉
	// dismissed_by)) — the extended Delegated chain predicate with sticky
	// opt-out, the same row-level predicate as the per-project
	// my_outbox_*_count counters and the cross-project Delegated list
	// (list_by_user.go ModeCreatedBy) — so the badge always equals that
	// list. As with On me, it equals the per-project pill-SUM only when no
	// deferred-unblocked rows are present (the pills omit them; this flat
	// status<>'closed' count includes them). Drives the live "Delegated"
	// tab badge.
	DelegatedOpenCount int `json:"delegatedOpenCount"`

	// --- sk-303.14, contract C2 — the second delegated number. ---
	//
	// ReviewMechanicEnabled is the EXPLICIT flag of whether the review
	// mechanic is on for the caller (INV-6). It and only it distinguishes
	// "there is no second number at all" from "the second number is zero"
	// (US-6, SC-016).
	//
	// The flag exists because neither a missing field nor a zero IS a
	// state: the counters object on the client is filled with defaults,
	// so "the field is absent" and "the field is zero" are
	// indistinguishable on it and both would be read as "there is nothing
	// unreviewed". The spec forbids relying on them outright. A consumer
	// MUST branch on THIS field.
	ReviewMechanicEnabled bool `json:"reviewMechanicEnabled"`

	// DelegatedUnreviewedCount is the second number: the size of the
	// caller's unreviewed set (closed results they created, delegated
	// and have not yet marked as reviewed).
	//
	// A pointer, and the key is ALWAYS present: with the mechanic off
	// the field renders as null — "there is no number", not "zero".
	// Omitting the key would be exactly the indistinguishable form
	// ReviewMechanicEnabled was introduced against; null is at least
	// observable. Authority still rests with the flag: a client that
	// unwraps null to 0 must learn the truth from it.
	//
	// The number is derived from review.UnreviewedPredicate — the same
	// and only definition of the set from which the list-path filter is
	// built (INV-3, R-3). The counter has no predicate of its own.
	//
	// INV-13: the second number lives ONLY here, in the filter and in
	// the digest. Project- and component-tree row counters and the
	// tasks column neither carry it nor recompute it — a deliberate
	// narrowing, not an omission.
	//
	// SC-014: DelegatedOpenCount above does not change value or
	// semantics — it is still UNCLOSED work now sitting with others,
	// including issues where the caller is merely a previous assignee.
	// The two numbers come from DIFFERENT sets, and their divergence
	// along the previous-assignee chain is a membership rule, not a
	// loss.
	DelegatedUnreviewedCount *int `json:"delegatedUnreviewedCount"`
}

// NotificationCounts handles GET /api/v1/me/notification-counts.
//
// Returns the counters that drive the header navigation:
//
//	{
//	  "inbox":                    3,    // assigned to me, open, not yet accepted
//	  "mentions":                 1,    // unread mention notifications
//	  "watching":                 5,    // unread notifications with a Watching card, in the window
//	  "onMeOpenCount":            4,    // non-closed issues in my "On me" lens
//	  "delegatedOpenCount":       6,    // non-closed issues in my "Delegated" lens
//	  "reviewMechanicEnabled":    true, // sk-303.14: review mechanic is on
//	  "delegatedUnreviewedCount": 2     // sk-303.14: second number; null when off
//	}
//
// sk-303.14 / T013 added TWO fields, and both are required for contract C2.
// `delegatedUnreviewedCount` is the second delegated number: closed
// results the caller created and has not yet read. It and
// `delegatedOpenCount` come from DIFFERENT sets and are not required to
// match — the first counts unclosed work sitting with others (including
// issues where the caller is merely a previous assignee), the second is
// strictly authorship. `delegatedOpenCount` did not change in value or
// in semantics (SC-014). `reviewMechanicEnabled` is the ONLY distinguisher
// of "there is no second number" from "the second number is zero": null
// and 0 are indistinguishable on the client, so branching on them is
// forbidden (SC-016).
//
// sk-303.13 / T017 removed the sixth, `outbox`. It counted events on
// tasks the caller had delegated — a third answer to the question
// `delegatedOpenCount` already answers, and one that disagreed with it by
// construction (events that go dark on read versus tasks that do not).
// The notifications behind it still exist and still feed Mentions and
// Watching; only the counter is gone. Consumers must tolerate the missing
// field.
//
// Algorithm:
//  1. Authenticate the caller via the cookie/BFF middleware
//     (RequireUser composes RequireInternalSecret + ParseUserHeaders
//     upstream — direct ingress can't reach this route).
//  2. Enter the counts gate (countsgate.go, K8): requests of the same
//     user arriving within countsGateWindow join one batch; the batch is
//     sealed when the window closes and only then computes, so every
//     request it answers arrived before the computation began (INV-11);
//     the result is not kept after the batch.
//  3. The batch computes every counter in ONE SQL statement
//     (computeCounts), each counter a scalar subquery served by its own
//     index: Inbox and the lenses by issues.assignee_user_id, Mentions and
//     the Watching badge by the caller's notification indexes, the
//     unreviewed set by review.UnreviewedPredicate unchanged (R7).
//  4. Pack into the wire shape and return 200.
//
// The endpoint is cookie-only by design — agents track their own
// inbox via `skein_notifications(action="inbox")` / `skein_prime`,
// which already surface the
// same numbers in a richer shape. Adding an MCP variant here would
// duplicate that surface for no upside.
//
// Errors:
//   - 401 unauthenticated — cookie missing or malformed
//   - 500 internal_error  — DB / connection fault
//
// @Summary Header counters for the caller
// @Description Returns the counters the header navigation renders: inbox, mentions, watching, the two lens buckets, and the delegated numbers. Business meaning: one round-trip for every badge, so the header cannot show numbers that disagree with each other.
// @Description
// @Description Every number counts only what the caller has access to NOW (INV-11): rows of a project he lost stay stored and are counted nowhere. `inbox` is the unaccepted tasks assigned to the caller in projects he may work in, plus the pending invitations to him (US-13).
// @Description
// @Description Algorithm: authenticate -> join the caller's batch: requests of the same user arriving within a few tens of milliseconds share ONE computation, which starts only after the batch stops collecting, so the answer is never older than any change the caller was signalled about before sending the request; nothing is cached after the batch -> compute every counter in one SQL statement (each counter scoped to the caller's own rows and visibility horizon; Inbox and the lenses match the assignee resolved to a user id — username, e-mail or id, any case) -> return 200.
// @Description
// @Description The second delegated number (sk-303.14): `delegatedUnreviewedCount` is the size of the caller's unreviewed set — closed results they created, delegated and have not yet marked as read. It is derived from the SAME single predicate as the `unreviewed=true` selection and the reminder recipients, so the number and the list it opens cannot diverge. The key is ALWAYS present: with the mechanic switched off it renders as `null` — «there is no number» — rather than being omitted, because an omitted key and a zero are indistinguishable on a client that fills defaults. The authority is nevertheless `reviewMechanicEnabled`, an explicit boolean on the same payload: a consumer that unwraps `null` to 0 must branch on that field, not on the number.
// @Description
// @Description `delegatedOpenCount` is NOT the same set and does not change meaning: it counts UNCLOSED work now sitting with other people, including issues where the caller is merely a previous assignee. The two numbers come from different sets on purpose, and their divergence along the previous-assignee chain is a membership rule rather than a loss.
// @Description
// @Description Deliberately narrow (INV-13): the second number lives only here, in the unreviewed selection and in the reminder digest. Project- and component-row counters and the tasks column neither carry it nor recompute it.
// @Description
// @Description `watching` counts the caller's unread, non-archived notifications that carry a Watching card and were written inside the Watching window (WATCHING_FEED_WINDOW_DAYS, default 21 days; reading does not extend it). It equals `unreadTotal` of GET /api/v1/me/watching. `mentions` has no window.
// @Description
// @Description Cookie-only by design — agents read the same numbers through skein_notifications / skein_prime. Side effects: none, this is a pure read.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} notificationCountsResponse "Header counters"
// @Failure 401 {object} map[string]any "unauthenticated — cookie missing or malformed"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/notification-counts [get]
func (h *MeHandler) NotificationCounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	resp, computed, err := h.countsGate.Do(ctx, c.UserID, func(ctx context.Context) (notificationCountsResponse, error) {
		return h.computeCounts(ctx, c.UserID)
	})
	outcome := "joined"
	if computed {
		outcome = "computed"
	}
	notificationCountsRequestsTotal.WithLabelValues(outcome).Inc()
	if err != nil {
		if ctx.Err() != nil {
			// The client went away while its batch was collecting or
			// computing; nobody reads the answer.
			return
		}
		slog.ErrorContext(ctx, "notification counts: query failed",
			"userId", c.UserID.String(), "outcome", outcome, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// computeCounts computes every header counter in ONE SQL statement
// (sk-622.21 T024, US-16): one round-trip per computation, and the numbers
// of one response are read from one snapshot, so they cannot contradict
// each other.
//
// Each counter is a scalar subquery with its own index, so none of them
// walks other people's tasks (SC-015, SC-002):
//
//   - inbox — open, unaccepted tasks whose resolved assignee is the caller
//     and whose creator is somebody else (Inbox is "from others" — sk-23):
//     issues.assignee_user_id = caller, which the database's triggers keep
//     equal to the resolution of the assignee text — username, e-mail or id,
//     any case, deactivated users included (INV-15);
//     idx_issues_assignee_user_active; only in a project the caller may work
//     in now (INV-30, sk-674.18 T029) — PLUS the pending invitations to the
//     caller (US-13; spaces.PendingInvitationsCountExpr);
//   - mentions — unread, non-archived feed notifications of the mention
//     family, of a project the caller sees now (INV-11); no window (INV-8);
//   - watching — notifications.WatchingBadgeCountExpr with the window of the
//     notifications store: equal to /me/watching's unreadTotal (INV-5);
//   - onMeOpenCount — active (not closed, not standing) tasks resolved to the
//     caller, accepted, created by somebody else, in a project the caller
//     sees and may work in now (the access relation, INV-30);
//     idx_issues_assignee_user_active;
//   - delegatedOpenCount — active tasks with an assignee that is not the
//     caller (an unresolved assignee text counts as "not the caller"),
//     created by the caller OR with the caller among previous_assignees and
//     not in dismissed_by, in a project the caller sees;
//     idx_issues_created_by ∪ idx_issues_previous_assignees (GIN, hence the
//     @> form of the membership tests);
//   - reviewMechanicEnabled and the unreviewed set — review.MechanicEnabledExpr
//     and review.UnreviewedPredicate, unchanged: membership still compares
//     the assignee TEXT with the author's username (R7);
//     idx_issues_unreviewed_creator.
func (h *MeHandler) computeCounts(ctx context.Context, userID uuid.UUID) (notificationCountsResponse, error) {
	window := notifications.DefaultWatchingWindow
	if h.notifStore != nil {
		window = h.notifStore.WatchingWindow()
	}
	var params []any
	bind := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}
	// One read of the relation (sk-674.18 T029): the visible projects — the
	// lenses, Mentions, Watching, and also Inbox and «on me», since the
	// caller may work in every project he sees (INV-30, sk-741).
	_, visibleSet, err := authz.ScopeFor(ctx, userID).Bind(ctx, h.pool)
	if err != nil {
		return notificationCountsResponse{}, err
	}
	me := bind(userID)
	vis := bind(visibleSet)
	win := bind(window)
	// visible is the visibility horizon of the lens buckets over the issues
	// alias a: the access relation for the caller (authz, sk-674.18 T010).
	visible := func(a string) string {
		return authz.VisibleExpr(a+".project_id", "false", vis)
	}
	mechanic := review.MechanicEnabledExpr(bind, userID)
	unreviewed := review.UnreviewedPredicate(bind, review.UnreviewedArgs{
		Alias:    "i",
		CallerID: userID,
	})
	// Each counter reads issues under its own alias (inb, onme, dlg, i) so
	// that a plan names the subquery a read belongs to.
	sql := `
SELECT
    (SELECT count(*) FROM issues inb
      WHERE inb.assignee_user_id = ` + me + `
        AND inb.status = 'open'
        AND inb.accepted_at IS NULL
        AND inb.created_by <> ` + me + `
        AND inb.project_id = ANY(` + vis + `::uuid[]))
      + ` + spaces.PendingInvitationsCountExpr(me+"::uuid") + ` AS inbox,
    (SELECT count(*) FROM notifications n
      WHERE n.user_id = ` + me + `
        AND n.feed_channel
        AND n.read_at IS NULL
        AND n.archived_at IS NULL
        AND n.type IN ('comment.mention', 'issue.mention')
        AND (n.project_id IS NULL OR n.project_id = ANY(` + vis + `::uuid[]))) AS mentions,
    ` + notifications.WatchingBadgeCountExpr(me, win, vis) + ` AS watching,
    (SELECT count(*) FROM issues onme
      WHERE onme.assignee_user_id = ` + me + `
        AND ` + status.ActiveWork("onme") + `
        AND onme.created_by <> ` + me + `
        AND onme.accepted_at IS NOT NULL
        AND ` + visible("onme") + `) AS on_me,
    (SELECT count(*) FROM issues dlg
      WHERE ` + status.ActiveWork("dlg") + `
        AND dlg.assignee IS NOT NULL
        AND dlg.assignee_user_id IS DISTINCT FROM ` + me + `
        AND (dlg.created_by = ` + me + `
             OR (dlg.previous_assignees @> ARRAY[` + me + `]::uuid[]
                 AND NOT dlg.dismissed_by @> ARRAY[` + me + `]::uuid[]))
        AND ` + visible("dlg") + `) AS delegated,
    ` + mechanic + ` AS mechanic_enabled,
    (SELECT count(*) FROM issues i WHERE ` + unreviewed + `) AS unreviewed`

	var (
		resp       notificationCountsResponse
		mechanicOn bool
		unrev      int
	)
	if err := h.pool.QueryRow(ctx, sql, params...).Scan(
		&resp.Inbox, &resp.Mentions, &resp.Watching,
		&resp.OnMeOpenCount, &resp.DelegatedOpenCount,
		&mechanicOn, &unrev,
	); err != nil {
		return notificationCountsResponse{}, fmt.Errorf("computing notification counts: %w", err)
	}
	resp.ReviewMechanicEnabled = mechanicOn
	// With the mechanic off there is no second number AT ALL (US-6, SC-016) —
	// so the pointer stays nil and the field goes out as null. A zero here
	// would be the answer to a different question: "the review workflow is on,
	// there is nothing unreviewed".
	if mechanicOn {
		resp.DelegatedUnreviewedCount = &unrev
	}
	return resp, nil
}

// watchingReasonOrder is the canonical render order for watcher
// reasons (sk-39): highest-investment relationship first. The reasons
// stored on issue_watchers.reasons[] arrive in insert order; the
// shell wants a deterministic display order so the most meaningful
// chip shows first.
//
// Ordering rationale:
//
//	manual    — explicit user click, highest signal of intent
//	assignee  — caller owns the work, second highest
//	creator   — caller filed it, still strong
//	mention   — caller was tagged, contextual
//	commenter — caller chimed in, weakest auto-derived signal
//
// 'mention' and 'commenter' are LEGACY values: being named once or
// replying once no longer creates a subscription (US-8 / R-7), and
// migration 000040 removed the accumulated ones. They stay in this map
// on purpose — a residual row (an environment migrated later, a restore
// from an older dump) must still render in a defined order rather than
// fall into the unknown-reason tail. Unknown reasons (forward-compat)
// sort after the known set.
var watchingReasonRank = map[string]int{
	"manual":    1,
	"assignee":  2,
	"creator":   3,
	"mention":   4,
	"commenter": 5,
}

func sortWatchingReasons(rs []string) []string {
	out := make([]string, 0, len(rs))
	out = append(out, rs...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, ok := watchingReasonRank[out[i]]
		if !ok {
			ri = 99
		}
		rj, ok := watchingReasonRank[out[j]]
		if !ok {
			rj = 99
		}
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// GetWatching handles GET /api/v1/me/watching (sk-622.21, US-5).
//
// Returns the caller's Watching cards. A card is a task, of two kinds:
//
//   - subscribed — the caller has a live subscription on it (silent
//     subscriptions are returned too, last);
//   - historical — no subscription, but the caller's notifications inside
//     the Watching window carry it as their card (e.g. after the automatic
//     unwatch on close). Empty reasons, null since.
//
// Each card carries the IssueSummary, the subscription reasons sorted by
// render priority, unreadEventsCount (unread notifications with this card
// inside the window), recentEvents (the last 5 notifications of the card
// within 7 days, read or unread) and since.
//
// Every notification's card was computed when it was written (the task, or
// the nearest ancestor watched with its subtree), so this read is a count
// over the caller's notifications in the window, not a match against the
// subscriptions. unreadTotal is uncapped, ignores the filter and equals the
// header Watching badge (INV-5, see countWatching).
//
// Order: newest notification in the window first, then issue id; cards
// without notifications last. Capped at notifications.WatchingFeedLimit
// (100); truncated says the cards shown are fewer than stateTotal.
//
// `?state=open|closed|all` narrows the cards of both kinds by the state of
// their task. Omitted = all.
//
// Errors:
//   - 400 validation_error — state outside open|closed|all
//   - 401 unauthenticated — cookie missing or malformed
//   - 500 internal_error  — DB / connection fault
//
// @Summary The caller's Watching column — subscribed and historical cards
// @Description Returns the caller's Watching cards: the tasks the caller has a live subscription on, plus the tasks the caller's non-archived notifications written inside the Watching window carry as their card (WATCHING_FEED_WINDOW_DAYS, default 21 days; reading a notification does not extend the window). A card with a subscription carries its reasons (manual / assignee / creator) and `since`; a historical card (no subscription — e.g. the subscription was dropped automatically when the task closed and the closing notification remains) has empty `reasons` and `since` = null and stays while it has notifications in the window.
// @Description
// @Description Each notification's card is fixed when the notification is written: the task it is about if the caller watched that task, otherwise the nearest ancestor the caller watched with its subtree; mentions never have a card. A card counts `unreadEventsCount` = its unread notifications in the window; `recentEvents` = its last 5 notifications within 7 days, read or unread. Order: newest notification in the window first, then issue id; cards without notifications last; at most 100 cards.
// @Description
// @Description Filter: `state=open` keeps cards whose task is not closed, `state=closed` cards whose task is closed, `state=all` (default) every card — for both kinds of card.
// @Description
// @Description Totals are uncapped: `subscriptionsTotal` (live subscriptions), `openTotal` + `closedTotal` (cards of both kinds by task state, always both), `closedAutoTotal` (closed cards without a manual subscription, historical ones included — what POST /api/v1/me/watching/unwatch {"state":"closed"} would take), `stateTotal` (cards matching the filter), `unreadTotal` (unread notifications over ALL cards in the window — equal to the header Watching badge; it does not follow the filter). `truncated` = the cards returned are fewer than `stateTotal`.
// @Description
// @Description Read-only, no side effects.
// @Tags Notifications
// @Produce json
// @Security ApiKeyAuth
// @Param state query string false "open | closed | all (default all)"
// @Success 200 {object} notifications.WatchingListResponse "Cards + totals"
// @Failure 400 {object} map[string]any "validation_error — unknown state"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/watching [get]
func (h *MeHandler) GetWatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	state := notifications.WatchingStateAll
	if raw := r.URL.Query().Get("state"); raw != "" {
		parsed, ok := notifications.ParseWatchingState(raw)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "validation_error",
				"state must be one of open, closed, all", nil)
			return
		}
		state = parsed
	}
	if h.issuesStore == nil || h.notifStore == nil {
		// Wiring error — main.go always provides both. Surface as 500
		// rather than nil-dereference panic.
		slog.ErrorContext(ctx, "watching feed: handler missing stores",
			"userId", c.UserID.String())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	feed, err := h.notifStore.ListWatchingFeed(ctx, c.UserID, state)
	if err != nil {
		slog.ErrorContext(ctx, "watching feed: base query failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	if len(feed.Rows) == 0 {
		writeJSON(w, http.StatusOK, watchingResponse([]notifications.WatchingRow{}, state, feed))
		return
	}

	ids := make([]string, 0, len(feed.Rows))
	for _, b := range feed.Rows {
		ids = append(ids, b.IssueID)
	}

	// Hydrate IssueSummary + recent events in parallel-friendly two-pass
	// fetches. Same caller is used for IAssigned/IsBlocked derivation so
	// the rows match the rest of the surface.
	summaries, err := h.issuesStore.SummariesForIDs(ctx, ids, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "watching feed: summaries hydration failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	recent, err := h.notifStore.HydrateWatchingRecentEvents(ctx, c.UserID, ids)
	if err != nil {
		slog.ErrorContext(ctx, "watching feed: recent events hydration failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}

	items := make([]notifications.WatchingRow, 0, len(feed.Rows))
	for _, b := range feed.Rows {
		sum, ok := summaries[b.IssueID]
		if !ok {
			// Issue was deleted/hidden between the watcher SELECT and
			// the summary SELECT — drop the row: the shell can't render
			// a row without a title.
			slog.WarnContext(ctx, "watching feed: summary missing for watched issue",
				"userId", c.UserID.String(), "issueId", b.IssueID)
			continue
		}
		events := recent[b.IssueID]
		if events == nil {
			events = []notifications.NotificationDTO{}
		}
		items = append(items, notifications.WatchingRow{
			Item:              sum,
			Reasons:           sortWatchingReasons(b.Reasons),
			UnreadEventsCount: b.UnreadEventsCount,
			RecentEvents:      events,
			Since:             b.Since,
			SpaceID:           b.SpaceID,
		})
	}

	writeJSON(w, http.StatusOK, watchingResponse(items, state, feed))
}

// MarkCardRead handles POST /api/v1/me/watching/{issueId}/read (sk-622.21,
// US-7): "mark card as read".
//
// @Summary Mark a Watching card read — the card's own and its descendants' notifications
// @Description Marks read every unread, non-archived notification of the CALLER whose Watching card is `issueId` — inside the Watching window and outside it. A notification's card is fixed when it is written: the task it is about if the caller watched that task, otherwise the nearest ancestor the caller watched with its subtree. So on an ancestor card watched with its subtree this reads the descendants' events too, which POST /api/v1/notifications/mark-read-by-entity (the task's own events only) does not; that endpoint is unchanged.
// @Description
// @Description Effect on the header: the Watching badge drops by exactly the card's unread notifications inside the window; notifications of other cards, mentions and other users' rows are not touched.
// @Description
// @Description Idempotent and never an error for a card: a card that is somebody else's, does not exist or has nothing unread answers 200 with `marked` = 0. Side effect: when something was marked, the caller's personal signal `watching` is published after the commit (live update of the caller's other tabs).
// @Tags Notifications
// @Produce json
// @Security ApiKeyAuth
// @Param issueId path string true "Full hierarchical id of the card's task, e.g. sk-42"
// @Success 200 {object} notifications.MarkCardReadResult "How many notifications were flipped to read"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/me/watching/{issueId}/read [post]
func (h *MeHandler) MarkCardRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if h.notifStore == nil {
		slog.ErrorContext(ctx, "mark card read: handler missing notifications store",
			"userId", c.UserID.String())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	issueID := chi.URLParam(r, "issueId")
	res, err := h.notifStore.MarkReadCard(ctx, c.UserID, issueID)
	if err != nil {
		slog.ErrorContext(ctx, "mark card read failed",
			"userId", c.UserID.String(), "issueId", issueID, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// bulkUnwatchRequest is the POST /api/v1/me/watching/unwatch body.
// Exactly one of IssueIDs and State.
type bulkUnwatchRequest struct {
	// IssueIDs — an explicit selection of Watching cards (full issue ids,
	// e.g. "sk-42.1"), at most 500. A subscribed card is stopped with an
	// opt-out, manual subscriptions included: picking it is the explicit
	// choice. A historical card (no subscription) is removed without an
	// opt-out. Ids that are neither are ignored.
	IssueIDs []string `json:"issueIds,omitempty"`
	// State — "open" | "closed" | "all": every live subscription of the
	// caller on tasks in that state (closed = status 'closed') is stopped
	// without an opt-out, and every historical card of such a task is
	// removed.
	State string `json:"state,omitempty"`
	// IncludeManual — with State only: also stop subscriptions the
	// caller made by hand. Default false — a filter sweep leaves them
	// and reports them as keptManual.
	IncludeManual bool `json:"includeManual,omitempty"`
}

// BulkUnwatch handles POST /api/v1/me/watching/unwatch (sk-551.10).
//
// @Summary Stop watching many issues at once — a selection or a filter
// @Description Stops the CALLER's own issue subscriptions and removes the caller's own Watching cards in bulk; nobody else's subscriptions or notifications are read or changed (sk-551.10, sk-622.21 US-6). Two mutually exclusive selectors:
// @Description - `issueIds` — an explicit selection (the Watching list's multi-select), handled per card kind. A card the caller is subscribed to is stopped exactly like DELETE /api/v1/issues/{id}/watch: the subscription is deleted whatever its reasons (manual included), an opt-out is recorded so the automatic paths (creator / assignee / reopen) do not subscribe the caller again, and the card is cleared. A historical card (no subscription, notifications still on it) is removed without an opt-out. Ids that are neither are ignored.
// @Description - `state` (`open` | `closed` | `all`) — every live subscription on tasks in that state (the Watching list's «Stop watching closed») is deleted WITHOUT an opt-out: reopening such a task subscribes the caller again. Subscriptions the caller made by hand (`manual`) are left alone and counted in `keptManual`, unless `includeManual` is true. Every historical card of a task in that state is removed too.
// @Description
// @Description "Card cleared / removed" means: every notification of the caller whose Watching card is that issue, archived ones included, loses the card — it leaves the Watching column and the badge, the notification stays in the inbox. A kept manual subscription keeps its card.
// @Description
// @Description The whole operation is one SERIALIZABLE transaction: it applies to everything selected or to nothing. A serialization conflict or deadlock is replayed, up to 3 attempts in all; a conflict that outlives the attempts is answered 500 internal_error and nothing is written. No notifications or events are produced.
// @Tags Notifications
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body bulkUnwatchRequest true "Selection: issueIds OR state (+ includeManual)"
// @Success 200 {object} notifications.BulkUnwatchResult "How many subscriptions were stopped, how many manual ones a state sweep kept, how many historical cards were removed"
// @Failure 400 {object} map[string]any "invalid_json / validation_error — neither or both selectors, empty issueIds, unknown state, more than 500 ids"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — a DB fault, or a serialization conflict that outlived the 3 attempts"
// @Router /api/v1/me/watching/unwatch [post]
func (h *MeHandler) BulkUnwatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if h.notifStore == nil {
		slog.ErrorContext(ctx, "bulk unwatch: handler missing notifications store",
			"userId", c.UserID.String())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	var req bulkUnwatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	in, msg := req.toInput()
	if msg != "" {
		writeProblem(w, http.StatusBadRequest, "validation_error", msg, nil)
		return
	}
	res, err := h.notifStore.BulkUnwatch(ctx, c.UserID, in)
	if err != nil {
		slog.ErrorContext(ctx, "bulk unwatch failed",
			"userId", c.UserID.String(), "state", string(in.State),
			"issueCount", len(in.IssueIDs), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "bulk unwatch",
		"userId", c.UserID.String(), "state", string(in.State),
		"issueCount", len(in.IssueIDs), "unwatched", res.Unwatched, "keptManual", res.KeptManual,
		"historicalRemoved", res.HistoricalRemoved)
	writeJSON(w, http.StatusOK, res)
}

// toInput validates the body's shape. A non-empty message is a 400: the
// selectors are malformed, not a business decision about a subscription.
func (req bulkUnwatchRequest) toInput() (notifications.BulkUnwatchInput, string) {
	hasIDs := req.IssueIDs != nil
	hasState := req.State != ""
	switch {
	case hasIDs && hasState:
		return notifications.BulkUnwatchInput{}, "pass either issueIds or state, not both"
	case !hasIDs && !hasState:
		return notifications.BulkUnwatchInput{}, "pass issueIds or state"
	case hasIDs:
		if len(req.IssueIDs) == 0 {
			return notifications.BulkUnwatchInput{}, "issueIds must not be empty"
		}
		if len(req.IssueIDs) > notifications.MaxBulkUnwatchIDs {
			return notifications.BulkUnwatchInput{}, fmt.Sprintf("at most %d issueIds per request", notifications.MaxBulkUnwatchIDs)
		}
		for _, id := range req.IssueIDs {
			if id == "" {
				return notifications.BulkUnwatchInput{}, "issueIds must not contain empty ids"
			}
		}
		return notifications.BulkUnwatchInput{IssueIDs: req.IssueIDs}, ""
	}
	st, ok := notifications.ParseWatchingState(req.State)
	if !ok {
		return notifications.BulkUnwatchInput{}, "state must be one of open, closed, all"
	}
	return notifications.BulkUnwatchInput{State: st, IncludeManual: req.IncludeManual}, ""
}

// watchingResponse assembles the wire shape from the cards and the store's
// totals — one place, so the empty and the non-empty branch cannot report
// different fields.
func watchingResponse(items []notifications.WatchingRow, state notifications.WatchingState, feed notifications.WatchingFeedResult) notifications.WatchingListResponse {
	return notifications.WatchingListResponse{
		Items:              items,
		UnreadTotal:        feed.UnreadTotal,
		SubscriptionsTotal: feed.SubscriptionsTotal,
		Truncated:          feed.Truncated,
		State:              state,
		StateTotal:         feed.StateTotal,
		OpenTotal:          feed.OpenTotal,
		ClosedTotal:        feed.ClosedTotal,
		ClosedAutoTotal:    feed.ClosedAutoTotal,
	}
}
