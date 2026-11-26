package handler

import (
	"bufio"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"skein-api/internal/monitorregistry"
)

// runnerParts holds the parts of the Python poll runner served by
// GET /agent/v1/monitor-script and the manifest that orders them. They are
// embedded at compile time so the running binary has zero filesystem
// dependency on the source tree (the production image is a scratch/distroless
// layer).
//
//go:embed assets/runner/parts.txt assets/runner/*.py
var runnerParts embed.FS

// runnerPartsDir is the directory of the parts and of their manifest.
const runnerPartsDir = "assets/runner"

// runnerTemplate is the runner body joined from its parts in manifest order,
// with every __SKEIN_*__ placeholder still in place. The Python tests join the
// same parts by the same manifest (runner_tests/harness.py).
var runnerTemplate = mustAssembleRunner()

// mustAssembleRunner joins the parts listed in parts.txt. The parts are
// compiled into the binary, so a missing or unreadable part is a defect of
// the build, never of the environment: it panics at start-up.
func mustAssembleRunner() string {
	body, err := assembleRunner(runnerParts, runnerPartsDir)
	if err != nil {
		panic(fmt.Sprintf("monitor runner: %v", err))
	}
	return body
}

// assembleRunner reads dir/parts.txt and joins the named files of dir in its
// order. Blank lines and lines starting with "#" are ignored; every part ends
// with a newline in the joined body.
func assembleRunner(fsys fs.FS, dir string) (string, error) {
	manifest, err := fs.ReadFile(fsys, path.Join(dir, "parts.txt"))
	if err != nil {
		return "", fmt.Errorf("reading the parts manifest: %w", err)
	}
	var b strings.Builder
	parts := 0
	sc := bufio.NewScanner(strings.NewReader(string(manifest)))
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		part, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("reading part %q: %w", name, err)
		}
		b.Write(part)
		if len(part) > 0 && part[len(part)-1] != '\n' {
			b.WriteByte('\n')
		}
		parts++
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading the parts manifest: %w", err)
	}
	if parts == 0 {
		return "", fmt.Errorf("the parts manifest lists no part")
	}
	return b.String(), nil
}

// Placeholder tokens the served body carries until the handler substitutes
// them. The timing and window tokens sit inside int("…") and the registry
// token inside a string literal, so the substituted result stays valid
// Python (plain string replacement, NOT text/template, to avoid {{…}}
// clashing with Python).
const (
	pollIntervalPlaceholder   = "__SKEIN_POLL_INTERVAL_SEC__"
	backoffCeilingPlaceholder = "__SKEIN_BACKOFF_CEILING_SEC__"
	requestLimitPlaceholder   = "__SKEIN_REQUEST_LIMIT_SEC__"
	stallThresholdPlaceholder = "__SKEIN_STALL_THRESHOLD_SEC__"
	tokenIdlePlaceholder      = "__SKEIN_TOKEN_IDLE_SEC__"
	registryPlaceholder       = "__SKEIN_REGISTRY_B64__"
	// placeholderPrefix starts every placeholder; none is left in a served
	// body.
	placeholderPrefix = "__SKEIN_"
)

// AgentScriptHandler serves the agent task-monitor poll runner (sk-142.6) on
// the /agent/v1 data plane. Like AgentPollHandler it is mounted under
// RequireInternalSecret + RequireMonitorToken, so a request only reaches Script
// with a valid skm_ monitor token — the gate is for audit (the body itself is
// generic: it carries no secret, the token is read by the runner from a private
// file at runtime).
type AgentScriptHandler struct {
	// body is the assembled runner with every placeholder already
	// substituted once at construction time — the served bytes are identical
	// for every request, so there is no need to re-template per call.
	body string
	// sha256 is the hex SHA-256 of body. The mint (skein_get_monitor_script)
	// bakes it into the bootstrap, and the loader refuses to run a
	// downloaded body with another digest: a tampered download is never
	// executed.
	sha256 string
}

// NewAgentScriptHandler builds the handler: it substitutes the runner
// timings (sk-579, INV-6), the token renewal window and the monitor registry
// into the assembled body. The caller passes every value explicitly — in
// production config.RunnerTimings(cfg.MonitorRunnerPollInterval) for the
// cadence, backoff ceiling and per-request limit, cfg.MonitorStallThreshold
// and cfg.MonitorTokenIdleTTL — so the body runs exactly the timings the
// server checked at startup; the handler derives nothing itself. Every
// duration is templated as whole seconds floored at 1, so a sub-second value
// never templates a 0 (busy loop, zero cap, zero limit or zero window). The
// registry is the embedded registry.json, base64-encoded, so the body's
// digest covers it.
func NewAgentScriptHandler(pollIntervalSec int, backoffCeiling, requestLimit, stallThreshold, tokenIdle time.Duration) *AgentScriptHandler {
	if pollIntervalSec < 1 {
		pollIntervalSec = 1
	}
	body := runnerTemplate
	body = strings.ReplaceAll(body, pollIntervalPlaceholder, strconv.Itoa(pollIntervalSec))
	body = strings.ReplaceAll(body, backoffCeilingPlaceholder, strconv.Itoa(wholeSecondsAtLeastOne(backoffCeiling)))
	body = strings.ReplaceAll(body, requestLimitPlaceholder, strconv.Itoa(wholeSecondsAtLeastOne(requestLimit)))
	body = strings.ReplaceAll(body, stallThresholdPlaceholder, strconv.Itoa(wholeSecondsAtLeastOne(stallThreshold)))
	body = strings.ReplaceAll(body, tokenIdlePlaceholder, strconv.Itoa(wholeSecondsAtLeastOne(tokenIdle)))
	body = strings.ReplaceAll(body, registryPlaceholder, base64.StdEncoding.EncodeToString(monitorregistry.Embedded()))
	if i := strings.Index(body, placeholderPrefix); i >= 0 {
		end := min(i+64, len(body))
		panic(fmt.Sprintf("monitor runner: placeholder left unsubstituted near %q", body[i:end]))
	}
	sum := sha256.Sum256([]byte(body))
	return &AgentScriptHandler{body: body, sha256: hex.EncodeToString(sum[:])}
}

// SHA256 returns the hex SHA-256 of the exact bytes Script serves.
func (h *AgentScriptHandler) SHA256() string {
	return h.sha256
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
// @Description  reads its skm_ token from a private file (SKEIN_TOKEN_FILE) at
// @Description  runtime. The operator-configured timings — poll cadence,
// @Description  backoff ceiling, per-request limit and stall threshold
// @Description  (sk-579) — are substituted into the body before it is served,
// @Description  together with the token renewal window
// @Description  (SKEIN_MONITOR_TOKEN_IDLE_TTL, whole seconds) and the monitor
// @Description  registry (the agents, their methods and the delivery channels,
// @Description  base64 of the server's registry document). The body is joined
// @Description  from ordered parts; its SHA-256, which the bootstrap names and
// @Description  the loader checks, covers the timings, the window and the
// @Description  registry.
// @Description  The runner never gives up on transient failures while its
// @Description  token lives: it backs off within the ceiling, aborts any single
// @Description  request past the limit, and prints one `degraded` line on a
// @Description  failing run longer than the stall threshold and one
// @Description  `recovered` line on the first success after it (both
// @Description  non-terminal; `degraded` names its cause in `cause`).
// @Description  The poll cursor is saved next to the token file (state.json)
// @Description  after every printed page: the first run of a new token is
// @Description  live-start, a restart with the same token file resumes from
// @Description  the saved cursor, at most one page per poll interval. The
// @Description  state file shares the token file's fate below. The loader
// @Description  names its mode to the body (`_SKEIN_LOADER_MODE`, `legacy`
// @Description  or `client`; none means `legacy`); in `legacy` mode no
// @Description  environment variable of the agent's machine changes the
// @Description  runner.
// @Description
// @Description  In `client` mode every command names its mode in
// @Description  SKEIN_MONITOR_MODE: `preflight` (the preflight command) or
// @Description  `run` (the working command); any other value or none is
// @Description  `config_invalid`. In `legacy` mode it is not read.
// @Description
// @Description  Preflight (`client` mode with SKEIN_MONITOR_MODE=preflight):
// @Description  the runner does not poll and does not move the cursor; it
// @Description  chooses how the monitor runs by the method order (override
// @Description  SKEIN_MONITOR_CHANNEL, the record's stream, own delivery
// @Description  command and exit-on-event, hosts, blind channels unless
// @Description  SKEIN_MONITOR_BLIND=off, journal; in the automatic walk a
// @Description  channel whose probe says unavailable is skipped, a forced
// @Description  channel that is unavailable gives the journal). It checks
// @Description  that the token and journal directories take a write (else
// @Description  `config_invalid` naming the directory); a ledger directory
// @Description  that takes no write is a warning and no external channel is
// @Description  used. It saves the result with its number, reason, warnings,
// @Description  journal, the addressee identity, the pane binding of a blind
// @Description  channel and a degraded cause of the detection in
// @Description  preflight.json next to the token under state.lock, and
// @Description  prints one line `{event:
// @Description  "preflight", preflightId, client, method, channel, reason,
// @Description  warnings, journal, command, instruction}`. `client` is the
// @Description  record used (its id, or `general` for an unknown name).
// @Description  `command` is the working command: the variables of the
// @Description  preflight command, the starter path and both digests with
// @Description  SKEIN_MONITOR_MODE=run, SKEIN_MONITOR_PREFLIGHT and every
// @Description  override set explicitly. `instruction` is what the agent
// @Description  does in the chosen method (tool and parameters, restart,
// @Description  warnings of the record and the channel), without the
// @Description  journal path. With a live runner of the token it answers
// @Description  `already_running` and changes nothing.
// @Description
// @Description  Working start (`client` mode with SKEIN_MONITOR_MODE=run, the
// @Description  `command` of the preflight line): the runner first checks
// @Description  that SKEIN_MONITOR_PREFLIGHT names the last saved preflight
// @Description  result and that SKEIN_MONITOR_CHANNEL and
// @Description  SKEIN_MONITOR_BLIND are the values it ran with, the Python
// @Description  version and the journal directory (else `config_invalid`,
// @Description  before any poll). `stream` and `exit-on-event` then run in
// @Description  the foreground; `detached` and `journal` start a detached
// @Description  process (a new session on POSIX) that gets this body on its
// @Description  stdin. In `stream` and `exit-on-event` an event whose only
// @Description  carrier is the journal (`unfit`: its line, or its stand-in
// @Description  that points at the journal) waits while the journal refuses
// @Description  its record (`degraded` `journal_unavailable` once, then
// @Description  `recovered`); the cursor does not pass it. Every start takes,
// @Description  in this order: the one-runner lock
// @Description  (the stable file skein-monitor/locks/<key>.lock of the token
// @Description  path), the token (opened, checked private, read, its
// @Description  descriptor closed), state.lock, a second check of the result
// @Description  and the overrides by the lock holder; a mismatch there is
// @Description  `config_invalid` with no poll. The start of any monitor
// @Description  removes files of locks/ older than 24 h that no one holds.
// @Description  The command that starts a detached process prints exactly
// @Description  one line and returns: `monitor_started` when the detached
// @Description  process is alive and holds the lock, or the stop line of its
// @Description  failed start. `monitor_started` of the first start by a
// @Description  result is `{event, preflightId, method, channel, issue,
// @Description  pollIntervalSec, reason, warnings, journal}`; a restart by
// @Description  the same command prints only `{event, preflightId, method,
// @Description  channel}` (the result is marked announced in preflight.json).
// @Description  A detached or journal monitor writes its lines and events to
// @Description  the journal. A detached monitor also carries every event of
// @Description  a page to the chosen channel before its cursor moves: an
// @Description  attempt is published in the shared ledger of the user
// @Description  (skein-monitor/ledger/, one delivery per addressee and event,
// @Description  10 s rate window per addressee), the addressee of the
// @Description  preflight is checked, the record's program is run by its
// @Description  argument list (never a shell; the text by stdin, a private
// @Description  message file or one argument), and its answer gives one
// @Description  outcome by the record's closed table (anything else is
// @Description  unknown). A refusal before send is retried after 5, 30 and
// @Description  120 s (at most 4 calls within the idle window), then the
// @Description  next non-blind channel of the preflight or the journal takes
// @Description  that event (`degraded` `channel_failing` once per switch,
// @Description  `recovered` when that channel delivers again); unknown is
// @Description  never retried (`degraded` `delivery_unknown`); a changed
// @Description  addressee sends later events to the journal only
// @Description  (`addressee_changed`); an unusable ledger sends nothing out
// @Description  (`state_unavailable`); a journal that takes no write holds
// @Description  new sends and the cursor until it does.
// @Description
// @Description  Stop protocol (sk-579 US-3): every controlled stop prints
// @Description  exactly one last stdout line `{event, reason, action[, status]}`
// @Description  — the only line with `reason` and `action` together (the
// @Description  preflight line also has a `reason`: why the method was
// @Description  chosen, never a stop). `event` is `closed`
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
// @Description  file untouched, 0); `config_invalid` (`client` mode: an
// @Description  invalid override, client name or platform, a token or
// @Description  journal directory that takes no write, an old Python, no
// @Description  preflight result, a SKEIN_MONITOR_PREFLIGHT that is missing
// @Description  or not the last result, overrides other than the
// @Description  preflight's, a one-runner lock that cannot be taken for an
// @Description  OS error; `action` names what to fix; kept, 1); `delivered`
// @Description  (`client` mode, exit-on-event: the batch is out; `paused`,
// @Description  kept, 0; `action` says to run the working command of the
// @Description  named preflight again); `addressee_gone` (`client` mode,
// @Description  detached: the channel's check says the agent has ended;
// @Description  `closed`, deleted, 0). In `client` mode `stopped` and
// @Description  `internal_error` name the working command of the preflight
// @Description  to resume, never repeat it. While a detached monitor waits
// @Description  to deliver it polls from the cursor of its last fixed
// @Description  outcome; it sends only when the last successful poll is at
// @Description  most two poll intervals old; 401 and 403 win over waiting
// @Description  events, which get "access lost" without the preview (in the
// @Description  journal, or in the deferred tail with a stop summary when the
// @Description  journal takes no write). A client-mode runner ends under its
// @Description  one-runner lock: waiting events get their outcomes, a stop
// @Description  that keeps access (`root_closed`, `stopped`,
// @Description  `internal_error`) is reported once by the same channel, the
// @Description  files sharing the token's fate (state, exclusion file,
// @Description  starter, preflight result, state lock, message files) and
// @Description  then the empty token directory are removed with a deleted
// @Description  token, and the lock is released last. One runner per token file: in
// @Description  `legacy` mode the runner takes an exclusive non-blocking
// @Description  flock on the token file it opened and reads the token once
// @Description  from that descriptor; on a platform without OS file locks it
// @Description  runs unlocked and says so once on stderr. In `client` mode
// @Description  the lock is skein-monitor/locks/<key>.lock (above). The
// @Description  endpoint is gated by RequireMonitorToken purely for audit.
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
