package mail

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics of the mail worker (sk-674.18, K-24). Package-level
// singletons on the default registerer, like the outbox metrics; the
// skein_instance label is added to every metric by cmd/api (instance_label.go).

var (
	// deliveriesTotal counts final outcomes of queued emails recorded by the
	// mail worker.
	//
	// @metric description Queued emails (email confirmation, password reset, space invitation) that reached a final outcome in the mail worker, by kind and outcome. One increment per email over its lifetime for every outcome the worker records; an email cancelled by the token protocol (internal/tokens Replace, Cancel or Use — a newer request, a revoked invitation, a completed action) in the transaction of that action is NOT counted here, because the worker never attempts it.
	// @metric calculation Incremented in mail.Runner after the conditional finalising UPDATE of outgoing_emails affected the row (the lease generation still matched): success after the relay accepted DATA, rejected after a permanent SMTP failure, cancelled when tokens.IssueForEmail refused the email at the start of an attempt; and in the deadline sweep (Runner tick and CleanupRunner), expired once per pending email whose deadline_at passed. A finalisation that lost its lease affects no row and is not counted.
	// @label kind Email kind. Values: email_confirm, password_reset, space_invite
	// @label outcome Final outcome. Values: success, rejected, expired, cancelled
	deliveriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_email_deliveries_total",
			Help: "Queued emails that reached a final outcome in the mail worker, by kind and outcome",
		},
		[]string{"kind", "outcome"},
	)

	// retriesTotal counts delivery attempts that ended in a retry.
	//
	// @metric description Email delivery attempts that failed temporarily and were scheduled again with back-off (SMTP 4xx, network, TLS, timeout, a 5xx at the sender and configuration stages — connection, EHLO, STARTTLS, AUTH, MAIL FROM —, a database error while issuing the link token). A sustained rate means the relay is unreachable or misconfigured; the emails wait until their deadline and then end as expired. Permanent failures (5xx to RCPT or DATA, invalid recipient) never count here.
	// @metric calculation Incremented in mail.Runner once per attempt whose conditional UPDATE back to pending (with the next attempt time) affected the row; an attempt that lost its lease to another worker is not counted.
	retriesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_email_retries_total",
			Help: "Email delivery attempts that failed temporarily and were scheduled again",
		},
	)

	// runnerErrorsTotal counts failed passes of the mail worker and its
	// cleanup.
	//
	// @metric description Failed passes of the mail worker or of the mail cleanup sweep — the database could not be reached or a statement of the pass failed before any email was attempted. The worker keeps running and retries the pass with back-off; alert on a sustained non-zero rate.
	// @metric calculation Incremented once per failed mail.Runner.Tick (lease reclaim, deadline sweep or claim failed) and once per failed CleanupRunner pass. Failures of a single email's attempt are not counted here — they are retries or outcomes.
	runnerErrorsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_email_runner_errors_total",
			Help: "Failed passes of the mail worker and its cleanup sweep",
		},
	)
)
