package outbox

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the outbox dispatcher. Names follow the
// `skein_<area>_<measure>` convention used elsewhere in skein-api.
//
// All metrics are package-level singletons registered with promauto in
// the default registerer. The /metrics endpoint mounted in cmd/api/main.go
// picks them up automatically.
//
// Metricy annotations live above each var; they document the metric for
// anyone reading the registry export.

var (
	// publishedTotal counts events successfully published to NATS and
	// committed back as published_at = now() in the same transaction.
	//
	// @metric description Total events published to NATS by the outbox dispatcher, labelled by NATS subject namespace. One increment per row per publish (at-least-once: a crash mid-flight re-publishes the same id, so this counter overcounts on dispatcher failure — pair with skein_outbox_publish_errors_total when reasoning about delivery).
	// @metric calculation Incremented in dispatcher.tick exactly once per row, after nc.Publish + nc.FlushWithContext + UPDATE published_at all succeed and the transaction commits.
	// @label subject_root NATS subject namespace prefix (bounded cardinality: exactly four values). Values: "skein.events.project", "skein.user", "skein.events.malformed", "other"
	publishedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_outbox_published_total",
			Help: "Total events published to NATS by the outbox dispatcher, by subject root",
		},
		[]string{"subject_root"},
	)

	// errorsTotal increments when a tick returns an error before the
	// commit. The classification label keeps cardinality bounded.
	//
	// @metric description Outbox dispatcher errors, classified by where the failure occurred. Operators alert on a sustained non-zero rate — a brief spike during NATS rolling restart is normal because the dispatcher applies exponential backoff.
	// @metric calculation Incremented in dispatcher.Run when dispatcher.tick returns a non-nil error. The reason label is unwrapped from the typed tickError attached at each call site, so labels are bound to the failure stage rather than parsed from the message text. A failure of the events phase (tickEvents) rolls its transaction back and skips the notifications pass of that tick; the claimed events are claimed again after backoff. Before the publish step (tx_begin, db_query — including the phase-2 neutral-row query —, scan, envelope) nothing of the batch has been published; at or after it (nats_publish, nats_flush, db_update, db_commit) part of the batch may already be on NATS and is published again (at-least-once). A failure of the notifications phase is counted too; the events of that tick are already committed.
	// @label reason Classification: "tx_begin", "db_query", "scan", "envelope", "nats_publish", "nats_flush", "db_update", "db_commit", "other". "db_query" also covers the phase-2 neutral-row query of issue events (sk-622.21, T027): its failure fails the whole tick, notifications included, until the query succeeds.
	errorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_outbox_publish_errors_total",
			Help: "Outbox dispatcher errors by classification",
		},
		[]string{"reason"},
	)

	// lagSeconds is the age of the oldest unpublished event. Refreshed
	// at the end of each successful tick. Alert when > 60s (per R3.2 plan).
	//
	// @metric description Lag in seconds — age of the oldest unpublished event in the outbox queue. Steady-state value is sub-second; growing values indicate the dispatcher cannot keep up (NATS down, DB slow, replica scaled to zero). Alert when > 60s for 1m.
	// @metric calculation now() - min(occurred_at) WHERE published_at IS NULL. Computed once per successful dispatcher tick via SELECT EXTRACT(EPOCH FROM (now() - min(occurred_at))). Also refreshed on the error path when the failure is non-DB (NATS / envelope) so external alerts fire on time during NATS outages. Resets to 0 when no rows are pending.
	lagSeconds = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "skein_outbox_lag_seconds",
			Help: "Age in seconds of the oldest unpublished event (now() - min(occurred_at) WHERE published_at IS NULL)",
		},
	)

	// batchSize tracks how many events each tick publishes. Useful to
	// see whether the dispatcher is consistently draining or running
	// near-empty (idle waiting for the next event burst).
	//
	// @metric description Events published per outbox tick. Bucket distribution shows whether the dispatcher mostly runs on small batches (steady-state low traffic) or saturates the BatchSize cap (catch-up after NATS outage). Buckets cover 1..500.
	// @metric calculation Observed in dispatcher.tick after a successful commit, with len(batch) as the value. Empty ticks are NOT observed so the histogram reflects work-doing ticks only.
	batchSize = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "skein_outbox_batch_size",
			Help:    "Events published per outbox tick (excluding empty ticks)",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500},
		},
	)

	// pushDeliveriesTotal counts individual web-push send attempts — one per
	// (subscription, send) call, not one per notification.
	//
	// @metric description Web-push send attempts made by the push runner, one per registered device per notification, labelled by the notification's reason and by what the provider answered. This counts DEVICE-LEVEL attempts; the per-notification verdict is skein_outbox_push_outcomes_total. A recipient with three devices produces three increments for one notification, so this counter is the wrong one for "how many notifications were delivered".
	// @metric calculation Incremented in PushRunner.deliver, once per push.Sender.Send call. result is derived from the Send return: nil → "sent"; push.ErrGone → "gone" (the dead endpoint was pruned by the sender); push.ErrForbidden → "forbidden" (sk-627: the push service answered 403, i.e. rejected our VAPID credentials; the subscription is kept, and a 403 never makes the notification retried by itself — but when ANOTHER device of the same recipient failed transiently, the notification is retried and every retry re-sends to the refusing device too, so one refusing device can add up to one "forbidden" increment per attempt); any other non-nil → "error". Notifications that never reach a Send (no devices, access revoked, preferences unreadable, sender not configured) are NOT counted here — they appear only in skein_outbox_push_outcomes_total.
	// @label reason Notification reason key from the server-side registry (bounded: the registry is a closed list). Values: "assignment", "accepted", "delegated_status", "reminder", "mention", "unaccepted_reminder", "review_reminder", "outcome_changed", "space_invitation", "unclassified". "unaccepted_reminder" and "review_reminder" are the two reminder digests activated in sk-303.14; "outcome_changed" is the close-outcome change of sk-489 and is addressed to the issue author; "space_invitation" is a space invitation (sk-674.18 T040) — to the invited person when sent, to the sender when accepted; they appear here as soon as their producer runs, and enumerating every push-capable reason of the registry is what keeps this line from going stale silently.
	// @label result Provider answer for one device. Values: "sent", "gone", "forbidden", "error". "forbidden" is a 403 from the push service — a refusal of our credentials (a configuration error), not a transport failure and not a dead device; it is a value of its own so a misconfigured VAPID subject is not hidden among transient errors
	pushDeliveriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_outbox_push_deliveries_total",
			Help: "Web-push send attempts by the push runner, per device, by reason and provider answer",
		},
		[]string{"reason", "result"},
	)

	// pushOutcomesTotal is the per-NOTIFICATION verdict: every push-eligible
	// row ends in exactly one of the closed set of final outcomes, and this is
	// where that is counted.
	//
	// @metric description Final outcomes of push-eligible notifications, one increment per notification row when it reaches a terminal state. This is the counter that answers "what happened to the pushes we owed": the outcomes are deliberately NOT merged, because "we never sent because the settings were unreadable" and "we sent and the transport failed" are different incidents with different fixes. Exactly one increment per notification over its lifetime.
	// @metric calculation Incremented in PushRunner.finalise, after the UPDATE that sets push_state='final' and push_outcome succeeds; in PushRunner.retireExpiredDeadlines once per row retired past its delivery deadline; and, for outcome="superseded" only, in outbox.ObserveSuperseded — called by the nudge worker after the transaction that displaced the older undelivered digests of one recipient-and-category pair commits, because displacement is decided by the producer that can see both rows, not by the runner.
	// @label reason Notification reason key from the closed server-side registry. Values: "assignment", "accepted", "delegated_status", "reminder", "mention", "unaccepted_reminder", "review_reminder", "outcome_changed", "space_invitation", "unclassified". "unaccepted_reminder" and "review_reminder" are the two reminder digests activated in sk-303.14; "outcome_changed" is the close-outcome change of sk-489 and is addressed to the issue author; "space_invitation" is a space invitation (sk-674.18 T040) — to the invited person when sent, to the sender when accepted; they appear here as soon as their producer runs, and enumerating every push-capable reason of the registry is what keeps this line from going stale silently.
	// @label outcome Terminal outcome. Values: "success", "no_device", "access_revoked", "prefs_unavailable", "configuration_no_send", "transport_exhausted", "superseded", "quiet_hours". "quiet_hours" (sk-340) is the row finished WITHOUT a send because the moment of delivery fell inside the recipient's quiet window: the feed row is written as usual and the person sees it in the morning, only the push is silent — and it is a value of its own because the acceptance requires telling "not sent because of quiet hours" apart from "not sent because the transport broke" in the ROW, not only in this label. "superseded" is the sk-303.14 value for a reminder digest displaced by a fresher one for the same recipient-and-category pair: it is finished without a send, and it is a value of its own because reusing an existing outcome would make displacement indistinguishable from a configuration refusal in the row itself, not only in this label. Enumerating the whole closed set (the schema constraint notifications_push_outcome_chk) is what keeps this hand-written line from going silent when the set grows.
	// @label cause Sub-cause, set only for configuration_no_send where the schema's closed outcome set cannot separate them. Values: "sender_disabled" (no VAPID keys configured in this deployment), "channel_disabled" (the recipient switched this reason's push channel off between the row being written and the send), "event_unresolvable" (the event the row points at resolves to no issue — or, for a digest, no readable payload — so nothing can be built), "digest_empty" (sk-303.14: every element of a reminder digest left the set or became invisible between selection and delivery, so the notification is finished without a send rather than sent with a stale count), "digest_oversize" (sk-303.14: a digest payload that does not fit the web-push record budget even in its shortest form — no list, count only; a degenerate-data DEFECT rather than a mode of operation, named separately so it is never mistaken for a transport failure), "provider_forbidden" (sk-627: no device accepted the push, none failed transiently, and at least one push service answered 403 — it rejected our VAPID credentials; the row is finished without retries and the subscription is kept, because the fix is the configuration, not the device), "invitation_stale" (sk-674.18 T040: a space-invitation push whose invitation lost its force before the send — no longer pending, its space deleted, or the recipient no longer its addressee), "" (empty for every other outcome)
	pushOutcomesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_outbox_push_outcomes_total",
			Help: "Final outcomes of push-eligible notifications, by reason, outcome and sub-cause",
		},
		[]string{"reason", "outcome", "cause"},
	)

	// pushRetriesTotal counts rows put back to pending after a transport
	// failure.
	//
	// @metric description Push notifications rescheduled after a transport failure. A transport error is explicitly NOT a final outcome: the row stays owed until its attempts or its deadline run out, and only then does it count as transport_exhausted. A rising retry rate with a flat exhausted rate is a provider hiccup being absorbed; both rising together is a real outage.
	// @metric calculation Incremented in PushRunner.retryOrExhaust when the row is rescheduled (push_state back to pending with a new push_next_attempt_at) rather than finalised.
	// @label reason Notification reason key from the closed server-side registry. Values: "assignment", "accepted", "delegated_status", "reminder", "mention", "unaccepted_reminder", "review_reminder", "outcome_changed", "space_invitation", "unclassified". "unaccepted_reminder" and "review_reminder" are the two reminder digests activated in sk-303.14; "outcome_changed" is the close-outcome change of sk-489 and is addressed to the issue author; "space_invitation" is a space invitation (sk-674.18 T040) — to the invited person when sent, to the sender when accepted; they appear here as soon as their producer runs, and enumerating every push-capable reason of the registry is what keeps this line from going stale silently.
	pushRetriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_outbox_push_retries_total",
			Help: "Push notifications rescheduled after a transport failure, by reason",
		},
		[]string{"reason"},
	)

	// pushLeaseReclaimsTotal counts rows taken back from a worker that died
	// holding them.
	//
	// @metric description Push notification rows returned to the pending queue because their worker lease expired — i.e. a push runner claimed them and did not finish (crash, pod eviction, network partition). Steady-state zero; a non-zero rate means workers are dying mid-delivery. The reclaimed attempt is NOT refunded, so this counter also explains attempts that produced no provider call.
	// @metric calculation Incremented in PushRunner.reclaimExpiredLeases by the number of rows the UPDATE moved from push_state='leased' back to 'pending' because push_leased_until had passed.
	pushLeaseReclaimsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_outbox_push_lease_reclaims_total",
			Help: "Push rows returned to pending after their worker lease expired",
		},
	)

	// pushCleanupDeletedTotal counts invisible push-only rows removed by the
	// retention sweep. Declared here with the rest of the push signals; the
	// sweep that increments it is T013.
	//
	// @metric description Notification rows deleted by the retention sweep — invisible push-only rows that reached a final delivery outcome more than the retention window ago. The feature exists to stop the storage accumulating rows nobody will ever see, so "the cleanup runs" has to be observable as a number rather than inferred from the existence of the code. A flat zero on a system that produces push-only rows means the sweep is not running.
	// @metric calculation Incremented by the number of rows each cleanup batch DELETEd. Rows that are still pending, leased, retryable, feed-visible or younger than the retention window are not eligible and are never counted.
	pushCleanupDeletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_outbox_push_cleanup_deleted_total",
			Help: "Invisible push-only notification rows removed by the retention sweep",
		},
	)

	// pushRunnerErrorsTotal counts push-runner ticks that failed outright.
	//
	// @metric description Push runner ticks that failed before any row could be processed — a claim, lease-reclaim or deadline-retirement statement erroring against the database. Distinct from delivery failures, which are per-notification and land in skein_outbox_push_outcomes_total; this one means the runner itself could not do its job and is backing off.
	// @metric calculation Incremented in PushRunner.Run when PushRunner.Tick returns a non-nil error (context cancellation excluded — that is a graceful shutdown, not a failure).
	pushRunnerErrorsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_outbox_push_runner_errors_total",
			Help: "Push runner ticks that failed against the database",
		},
	)
)
