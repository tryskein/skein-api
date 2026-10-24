package handler

import (
	_ "embed"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// monitorRunnerSource is the Python poll-runner served verbatim (after timing
// substitution) by GET /agent/v1/monitor-script. It is embedded at compile
// time so the running binary has zero filesystem dependency on the source tree
// (the production image is a scratch/distroless layer). The four __SKEIN_*__
// placeholders are substituted once, at construction — see
// NewAgentScriptHandler.
//
//go:embed assets/monitor_runner.py
var monitorRunnerSource string

// Placeholder tokens the served body carries until the handler substitutes
// the operator-configured timings. Each sits inside int("…") in the .py so the
// substituted result stays valid Python (plain string replacement, NOT
// text/template, to avoid {{…}} clashing with Python).
const (
	pollIntervalPlaceholder   = "__SKEIN_POLL_INTERVAL_SEC__"
	backoffCeilingPlaceholder = "__SKEIN_BACKOFF_CEILING_SEC__"
	requestLimitPlaceholder   = "__SKEIN_REQUEST_LIMIT_SEC__"
	stallThresholdPlaceholder = "__SKEIN_STALL_THRESHOLD_SEC__"
)

// AgentScriptHandler serves the agent task-monitor poll runner (sk-142.6) on
// the /agent/v1 data plane. Like AgentPollHandler it is mounted under
// RequireInternalSecret + RequireMonitorToken, so a request only reaches Script
// with a valid skm_ monitor token — the gate is for audit (the body itself is
// generic: it carries no secret, the token is read by the runner from a 0600
// file at runtime).
type AgentScriptHandler struct {
	// body is the runner source with the timing placeholders already
	// substituted once at construction time — the served bytes are identical
	// for every request, so there is no need to re-template per call.
	body string
}

// NewAgentScriptHandler builds the handler, substituting the runner timings
// into the embedded body (sk-579, INV-6). The caller passes all four values
// explicitly — in production config.RunnerTimings(cfg.MonitorRunnerPollInterval)
// for the cadence, backoff ceiling and per-request limit, and
// cfg.MonitorStallThreshold — so the body runs exactly the timings the server
// checked the stall threshold against at startup; the handler derives nothing
// itself. Every value is templated as whole seconds floored at 1, so a
// sub-second value never templates a 0 (busy loop, zero cap or zero limit).
func NewAgentScriptHandler(pollIntervalSec int, backoffCeiling, requestLimit, stallThreshold time.Duration) *AgentScriptHandler {
	if pollIntervalSec < 1 {
		pollIntervalSec = 1
	}
	body := monitorRunnerSource
	body = strings.ReplaceAll(body, pollIntervalPlaceholder, strconv.Itoa(pollIntervalSec))
	body = strings.ReplaceAll(body, backoffCeilingPlaceholder, strconv.Itoa(wholeSecondsAtLeastOne(backoffCeiling)))
	body = strings.ReplaceAll(body, requestLimitPlaceholder, strconv.Itoa(wholeSecondsAtLeastOne(requestLimit)))
	body = strings.ReplaceAll(body, stallThresholdPlaceholder, strconv.Itoa(wholeSecondsAtLeastOne(stallThreshold)))
	return &AgentScriptHandler{body: body}
}

// wholeSecondsAtLeastOne truncates d to whole seconds, floored at 1.
func wholeSecondsAtLeastOne(d time.Duration) int {
	if secs := int(d / time.Second); secs >= 1 {
		return secs
	}
	return 1
}

// Script handles GET /agent/v1/monitor-script.
//
// @Summary      Serve the agent task-monitor poll runner (Python)
// @Description  Returns the Python 3 stdlib poll-runner body, ready to be
// @Description  executed by the calling agent under Claude Code's Monitor
// @Description  tool. The body is generic — it carries no secret; the runner
// @Description  reads its skm_ token from a 0600 file (SKEIN_TOKEN_FILE) at
// @Description  runtime. The operator-configured timings — poll cadence,
// @Description  backoff ceiling, per-request limit and stall threshold
// @Description  (sk-579) — are substituted into the body before it is served.
// @Description  The runner never gives up on transient failures while its
// @Description  token lives: it backs off within the ceiling, aborts any single
// @Description  request past the limit, and prints one `degraded` line on a
// @Description  failing run longer than the stall threshold and one
// @Description  `recovered` line on the first success after it (both
// @Description  non-terminal; `degraded` names its cause in `cause`).
// @Description
// @Description  Stop protocol (sk-579 US-3): every controlled stop prints
// @Description  exactly one last stdout line `{event, reason, action[, status]}`
// @Description  — the only line that carries `reason`. `event` is `closed`
// @Description  (this bootstrap cannot restart the monitor), `paused` (the
// @Description  same bootstrap can) or `duplicate` (a second runner on a
// @Description  token file that already has one). `action` is a finished
// @Description  instruction naming the root issue. Reasons and token-file
// @Description  fate: `token_expired` (401, `status` 401, file deleted, exit
// @Description  0); `visibility_lost` (403, `status` 403, deleted, 0);
// @Description  `root_closed` (the poll reported `rootClosed`, after that
// @Description  page's notifications; deleted, 0); `token_file_missing`
// @Description  (exit 1; a missing file stays missing, an empty one is
// @Description  deleted, an unreadable one is left alone); `misconfigured`
// @Description  (exit 1; deleted when its path is known); `stopped`
// @Description  (SIGTERM/SIGINT; kept, 0); `internal_error` (kept, 1,
// @Description  traceback to stderr only); `already_running` (`duplicate`,
// @Description  file untouched, 0). One runner per token file: the runner
// @Description  takes an exclusive non-blocking flock on the token file it
// @Description  opened and reads the token once from that descriptor; on a
// @Description  platform without OS file locks it runs unlocked and says so
// @Description  once on stderr. The endpoint is gated by RequireMonitorToken
// @Description  purely for audit.
// @Tags         agent-monitor
// @Produce      text/x-python
// @Security     ApiKeyAuth
// @Success      200  {string}  string  "Python runner source"
// @Failure      401  {object}  map[string]any  "unauthenticated"
// @Router       /agent/v1/monitor-script [get]
func (h *AgentScriptHandler) Script(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(h.body)); err != nil {
		slog.WarnContext(r.Context(), "monitor-script: write response failed", "error", err)
	}
}
