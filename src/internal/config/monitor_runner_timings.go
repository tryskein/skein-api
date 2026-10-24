package config

import "time"

// MonitorRunnerRequestLimit is the hard upper bound on the I/O of ONE request
// the served Python runner makes (connect, name resolution, reading the body).
// The runner abandons a request at this limit and may retry immediately
// (sk-579 plan, decision "Single runner request limit"). It equals the
// runner's historical per-request socket timeout, so the bound the stall
// threshold is checked against is the one live runners already observe.
const MonitorRunnerRequestLimit = 15 * time.Second

// monitorRunnerMinBackoffCeiling is the floor of the runner's backoff ceiling:
// even at a 1s poll cadence a failing runner may wait up to 10s between
// attempts. Together with the 5× multiplier below it reproduces the formula
// the runner has always used (`max(5·P, 10)`, US-1 «Unchanged»).
const monitorRunnerMinBackoffCeiling = 10 * time.Second

// monitorRunnerBackoffMultiplier scales the poll cadence into the backoff
// ceiling (see monitorRunnerMinBackoffCeiling).
const monitorRunnerBackoffMultiplier = 5

// MonitorRunnerTimings is the single derivation point of every timing the
// served runner works with (sk-579, INV-6). The server templates these values
// into the runner body AND checks the stall threshold against them at startup,
// so the threshold can never drift below what a live runner actually does.
type MonitorRunnerTimings struct {
	// PollIntervalSec is the base cadence in whole seconds, floored at 1 so a
	// sub-second interval never templates a busy loop into the runner.
	PollIntervalSec int
	// BackoffCeiling caps every wait between two runner requests: the
	// exponential backoff after failures and any server Retry-After alike.
	BackoffCeiling time.Duration
	// RequestLimit is the hard bound on one request's I/O
	// (MonitorRunnerRequestLimit).
	RequestLimit time.Duration
}

// MaxPollGap is the longest interval between the starts of two consecutive
// requests of a live runner: the longest wait (BackoffCeiling) plus the
// longest single request (RequestLimit). INV-6 requires the stall threshold
// to be strictly greater than this.
func (t MonitorRunnerTimings) MaxPollGap() time.Duration {
	return t.BackoffCeiling + t.RequestLimit
}

// RunnerTimings derives the runner timings from the configured poll interval
// (SKEIN_MONITOR_RUNNER_POLL_INTERVAL). The interval is truncated to whole
// seconds and floored at 1 — exactly the value templated into the runner —
// and the backoff ceiling is max(5·P, 10s) of that integer cadence.
func RunnerTimings(pollInterval time.Duration) MonitorRunnerTimings {
	pollSec := int(pollInterval / time.Second)
	if pollSec < 1 {
		pollSec = 1
	}
	ceiling := time.Duration(monitorRunnerBackoffMultiplier*pollSec) * time.Second
	if ceiling < monitorRunnerMinBackoffCeiling {
		ceiling = monitorRunnerMinBackoffCeiling
	}
	return MonitorRunnerTimings{
		PollIntervalSec: pollSec,
		BackoffCeiling:  ceiling,
		RequestLimit:    MonitorRunnerRequestLimit,
	}
}
