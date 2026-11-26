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
// @Description  journal directory that takes no write, an old Python;
// @Description  `action` names what to
// @Description  fix; kept, 1). One runner per token file: the runner
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
