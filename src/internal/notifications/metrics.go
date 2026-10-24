package notifications

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metrics.go — sk-303.13 / T012: the WRITE-side failure signals.
//
// The spec makes observability a requirement rather than a courtesy: "two
// refusal paths are separately observable: a push not sent because settings
// were unavailable, and an event that mapped onto no reason — both must
// produce a metric and a log record, not silence".
//
// This file owns the write-side half of that pair (the delivery-side half
// lives in internal/outbox). Both counters are label-free ON PURPOSE: the
// details that would make a useful label — which event type, which project,
// which recipient — are unbounded or high-cardinality, so they go into the log
// record, where they cost nothing per series and can be read at the moment of
// the incident.

var (
	// fanoutPrefsUnavailableTotal is the write-side counterpart of the
	// delivery-side prefs failure. Do not merge the two: they happen at
	// different moments, cost different things, and have different fixes.
	//
	// @metric description Fan-out passes that created NO notification rows because the recipients' reason preferences could not be read. Each increment is normally one user-facing operation (a comment, an edit, an issue creation, a mention) whose notifications were silently dropped — the operation itself still committed, which is exactly why this counter has to exist: without it the loss is invisible. Steady-state zero; any sustained rate means notifications are being lost wholesale. RETRY CAVEAT (sk-303.17): on the issues-package call sites that run FanOut/WriteMentionNotification inside an issues.Store.withSerializableRetry closure (AddComment, UpdateComment, DeleteComment, CreateEpicWithChildren, setPinned) — see internal/issues/store.go — a SERIALIZABLE abort on a LATER statement of the SAME attempt replays the whole closure, this increment included; the increment from the aborted attempt is not rolled back with the transaction. One retried user-facing operation can therefore inflate this counter by more than one per operation when a degraded preferences read and a serialization abort land in the same attempt. This is per-ATTEMPT, not strictly per-operation, on those call sites; a spike coinciding with elevated "exhausted serialization retries" WARN log volume is retry inflation, not N distinct preference failures.
	// @metric calculation Incremented once per FanOut or WriteMentionNotification call whose preferences read failed, at the same place the accompanying error log record is written. It counts CALLS, not recipients: one aborted fan-out over fifty watchers is one increment. Under a SERIALIZABLE replay (see RETRY CAVEAT above) it can fire once per replayed attempt for the same logical operation.
	fanoutPrefsUnavailableTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_notifications_fanout_prefs_unavailable_total",
			Help: "Fan-out passes that created no rows because the reason preferences were unreadable",
		},
	)

	// fanoutUnmappedEventsTotal counts the configuration error the closed
	// registry makes possible: an event type that claims a delivery path the
	// registry cannot name.
	//
	// @metric description Events that reached the fan-out with a delivery path but no reason in the closed registry. This is a CONFIGURATION ERROR, not a runtime condition: adding an event type without mapping it to a reason is the mistake the narrow default makes possible, and the price the ADR accepts for it. Audit-only event types — the ones that exist purely as journal records — are NOT counted here, because their silence is a decision rather than an omission. Any non-zero value should be treated as a defect to fix, not as traffic to watch. RETRY CAVEAT (sk-303.17): the same replay inflation documented on fanoutPrefsUnavailableTotal applies here — a SERIALIZABLE replay on one of issues.Store's withSerializableRetry call sites (AddComment, UpdateComment, DeleteComment, CreateEpicWithChildren, setPinned) can increment this once per replayed attempt for one user-facing operation if a later statement in the same attempt aborts after this classifier already ran; it is per-ATTEMPT, not strictly per-event, on those call sites.
	// @metric calculation Incremented in FanOut when the classifier answers OutcomeUnmapped for the event, before any recipient is selected. The offending event type is written to the accompanying error log record rather than carried as a label: an unmapped type is by definition one the registry does not know, so it cannot be bounded in advance. Under a SERIALIZABLE replay (see RETRY CAVEAT above) this can fire once per replayed attempt rather than once per event.
	fanoutUnmappedEventsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_notifications_fanout_unmapped_events_total",
			Help: "Events with a delivery path that map to no reason in the registry",
		},
	)
)

// sk-622.21, T019 (US-8): the retention sweep (retention.go).
var (
	// notificationRetentionDeletedTotal counts notification rows removed by
	// the retention sweep.
	//
	// @metric description Notification rows deleted by the retention sweep because they were older than NOTIFICATIONS_RETENTION_DAYS (US-8). Mentions are never deleted and rows whose push is still owed (push_state pending or leased) are skipped, so neither is ever counted. Unit: rows. Every replica sweeps; rows are taken with FOR UPDATE SKIP LOCKED, so a row is deleted — and counted — by exactly one replica: the sum over replicas is the number of rows removed. A long-running zero while the table holds rows older than the window means the sweep is not running.
	// @metric calculation Incremented in RetentionRunner.Sweep after each bounded DELETE batch, by that batch's RowsAffected (only when it is positive). The existing push-only sweep (skein_outbox_push_cleanup_deleted_total) is separate and not counted here.
	notificationRetentionDeletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_notifications_retention_deleted_total",
			Help: "Notification rows deleted by the retention sweep",
		},
	)

	// notificationRetentionSweepDurationSeconds measures one run of the
	// retention sweep.
	//
	// @metric description Wall-clock duration of one retention sweep run (all its DELETE batches until a batch comes back short), in seconds. One observation per run per replica, at least once an hour. Growth means the backlog of expired rows or the cost of a batch is growing; a run that fails is observed too, up to the failing batch.
	// @metric calculation Observed in RetentionRunner.Sweep by a deferred call when the run returns, successful or not.
	notificationRetentionSweepDurationSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "skein_notifications_retention_sweep_duration_seconds",
			Help:    "Duration of one notification retention sweep run",
			Buckets: prometheus.DefBuckets,
		},
	)
)

// Metrics of the assignee re-resolution worker (assignee_resync.go,
// sk-622.21 T025): INV-15's background safety net after R2.
var (
	// assigneeResyncFixed is how many issues the last completed pass
	// re-resolved.
	//
	// @metric description Issues whose assignee_user_id the LAST COMPLETED assignee re-resolution pass of this replica found different from the resolution of their assignee text by the current users directory, and fixed. Unit: issues. The database triggers of migration 000060 keep issues.assignee_user_id equal to that resolution at every write (INV-15); every writer of users that changes the resolution (account created, e-mail or username changed — users.Store and the SSO provisioner) runs SERIALIZABLE, so SSI aborts the race with a concurrent SERIALIZABLE assignment instead of letting a stale id commit (sk-622.21 R25). This pass is a safety net, not the correctness path: a non-zero value after rollout means a writer defect — a users writer outside SERIALIZABLE, or a write that bypasses the triggers — and is to be investigated, not watched as traffic. Rows written before migration 000060 are the only legitimate source, on the first passes after it. A replica that did not get the pass lock leaves its value untouched — read the series of the replica with the newest skein_issues_assignee_resync_last_pass_timestamp_seconds.
	// @metric calculation Set at the end of AssigneeResyncRunner.Pass, after every page of the keyset walk of issues committed, to the number of rows the pages updated. Not touched by a pass that was skipped (pass lock held elsewhere) or failed.
	assigneeResyncFixed = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "skein_issues_assignee_resync_fixed",
			Help: "Issues re-resolved by the last completed assignee re-resolution pass",
		},
	)

	// assigneeResyncLastPassTimestamp is when this replica last completed a
	// pass.
	//
	// @metric description Unix time, in seconds, at which this replica last completed an assignee re-resolution pass over the whole issues table (all pages, no error). Tells a fresh zero of skein_issues_assignee_resync_fixed from a stale one; zero until the replica completes its first pass. Passes run at start-up and then hourly, one replica at a time.
	// @metric calculation Set at the end of AssigneeResyncRunner.Pass together with skein_issues_assignee_resync_fixed, to time.Now() of the completing replica.
	assigneeResyncLastPassTimestamp = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "skein_issues_assignee_resync_last_pass_timestamp_seconds",
			Help: "Unix time of the last completed assignee re-resolution pass",
		},
	)
)
