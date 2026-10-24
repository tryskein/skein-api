package signals

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// personalSignalsPublishedTotal counts personal signals handed to NATS, per
// area (sk-622.21 T014).
//
// @metric description Personal signals (`skein.user.{uid}.signal`) handed to NATS Core after a committed transaction, counted per area the signal names. One message naming three areas increments three series by one. It measures how often each personal area is announced to users, not how many frames reach browsers: skein-web coalesces signals per connection afterwards, and NATS Core delivery is best-effort. A message whose publish call failed is not counted (the failure is logged at warn level). Zero when NATS is disabled.
// @metric calculation Incremented in signals.Flush once per area of each message whose nc.Publish returned no error. Flush runs after a successful COMMIT only (signals.BeginTxFunc), so a rolled-back or retried attempt never increments it.
// @label area Personal area named by the signal (bounded: the closed contract set of seven). Values: "inbox", "mentions", "watching", "lens", "unreviewed", "tasks", "projects"
var personalSignalsPublishedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_personal_signals_published_total",
		Help: "Personal signals published to NATS after commit, per area",
	},
	[]string{"area"},
)
