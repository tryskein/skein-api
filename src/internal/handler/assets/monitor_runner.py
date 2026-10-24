#!/usr/bin/env python3
# Skein agent task-monitor poll runner (sk-142.6).
#
# Pure Python 3 standard library — NO third-party dependencies — so it runs
# unchanged in any agent environment that ships python3. It is served verbatim
# (with the timing placeholders below substituted) by
# GET /agent/v1/monitor-script and is meant to be launched under Claude Code's
# `Monitor` tool, which streams its stdout into the agent's context.
#
# Contract with the Monitor (do not break):
#   - EXACTLY one JSON object per line on stdout, flushed immediately. Claude
#     Monitor reads line-by-line; a buffered stdout means the agent sees
#     nothing until the process exits, which destroys the near-real-time goal.
#   - Notification lines carry the poll payload verbatim under "notification".
#   - Lifecycle lines carry {"event": "..."}: monitor_started, degraded and
#     recovered are non-terminal; a stop prints exactly one last line per
#     the stop table (STOPS). Only a stop line carries "reason" — degraded
#     names its cause in "cause" — so a reader can tell a stop by that field.
#     Nothing is printed for an empty poll — an idle issue produces
#     NO stdout at all. Claude Code's Monitor tool already surfaces its own
#     "N monitor(s) still running" liveness indicator, so the runner does not
#     duplicate that with a heartbeat: every stdout line wakes the consuming
#     agent for a full turn, and a periodic heartbeat would burn a turn on
#     nothing every cycle for as long as the monitor runs.
#
# Auth + secret hygiene (spec §6 / R8):
#   - The skm_ token is read once from the 0600 file at SKEIN_TOKEN_FILE —
#     never from argv / process-env / shell history. The file's fate on each
#     stop is the token-file column of the stop table: it is kept on a
#     signal or an internal error, so the same bootstrap can restart the
#     monitor while the token lives, and deleted when the token is dead.
#
# Cursor (spec §6 / §9):
#   - The FIRST poll sends NO since_ts/since_id; the server live-anchors the
#     cursor at its own now() so the runner gets no historical backlog.
#   - Every later poll echoes back the cursor (ts, id) from the prior response.
#   - The server uses a watermark+lag fence, so the overlap can re-deliver a
#     row; a bounded seen-id set dedupes across that overlap.
#
# Env:
#   SKEIN_TOKEN_FILE      path to a 0600 file holding the skm_ token (required)
#   SKEIN_PUBLIC_URL      base URL of the agent door, e.g. https://host (required)
#   SKEIN_ISSUE           issue id this monitor targets (optional; for labelling)
#   SKEIN_EXCLUDE_SESSION minting agent's MCP session, sent verbatim as the
#                         exclude_session poll query-param for server best-effort
#                         echo suppression (sk-164 FR-2). Optional; read once at
#                         start (this runner is detached and cannot learn a
#                         rotated session — the reader-side self-skip in the
#                         monitor guide is the authoritative echo layer). Empty
#                         = not sent = no server-side suppression (safe, INV-9).

import json
import os
import random
import signal
import subprocess
import sys
import time
import traceback
import urllib.parse

try:
    import fcntl
except ImportError:  # not POSIX (Python on Windows): no OS file locks
    fcntl = None

# --- timings, templated by the serving handler (integer seconds) ------------
# Each placeholder token below is replaced by plain string substitution in
# agent_script.go before the body is served (e.g. the right side becomes
# int("2")). They sit inside int("...") so the RAW asset is still valid Python
# (parses, py_compile/linters pass); only execution before substitution would
# raise a clear ValueError rather than a NameError on module init. All four
# come from ONE server-side derivation (config.RunnerTimings + the stall
# threshold), which is what lets the server check INV-6 at startup.
POLL_INTERVAL_SEC = int("__SKEIN_POLL_INTERVAL_SEC__")
# Cap on every wait between two requests: the backoff after failures, the
# growth of quiet polls, and any server Retry-After.
BACKOFF_CEILING_SEC = int("__SKEIN_BACKOFF_CEILING_SEC__")
# Hard bound on ONE request (name resolution, connect, body). The request runs
# in a separate executor process; past this bound the executor is killed, so
# the I/O really stops and the next attempt can start at once.
REQUEST_LIMIT_SEC = int("__SKEIN_REQUEST_LIMIT_SEC__")
# A run of failures longer than this prints one non-terminal "degraded" line.
STALL_THRESHOLD_SEC = int("__SKEIN_STALL_THRESHOLD_SEC__")

# --- runner-side tuning (not operator-configurable) -------------------------
# Cap on the dedupe set so a long-lived monitor cannot grow it without bound.
SEEN_IDS_CAP = 2000
# Step of the executor wait loop. The deadline is checked between steps on
# time.monotonic(); the step only bounds how late a finished answer is
# noticed.
REQUEST_WAIT_STEP_SEC = 0.05

# The request executor: a tiny stdlib loop run as `python3 -c`. One JSON line
# in ({"url", "token", "timeout"}) → one JSON line out ({"status", "body"} on
# 2xx, {"status", "retryAfter"} on an HTTP error, {"status": 0, "error"} on
# anything else). The token travels over the pipe — never argv or env.
EXECUTOR_SRC = r"""
import json, sys, urllib.error, urllib.request
for line in sys.stdin:
    req = json.loads(line)
    try:
        r = urllib.request.Request(req["url"], headers={"X-Skein-Api-Key": req["token"]})
        with urllib.request.urlopen(r, timeout=req["timeout"]) as resp:
            out = {"status": resp.status, "body": resp.read().decode("utf-8", "replace")}
    except urllib.error.HTTPError as exc:
        out = {"status": exc.code, "retryAfter": exc.headers.get("Retry-After") if exc.headers else None}
    except Exception as exc:
        out = {"status": 0, "error": type(exc).__name__}
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
"""


def emit(obj):
    """Write one JSON object as a single flushed stdout line (the Monitor contract)."""
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def log_stderr(msg):
    """Diagnostics go to stderr so they never pollute the stdout line protocol."""
    sys.stderr.write(msg + "\n")
    sys.stderr.flush()


def jitter(base):
    """Full-jitter: a random point in [base/2, base] to de-sync a fleet of runners."""
    if base <= 0:
        return 0.0
    return base / 2.0 + random.random() * (base / 2.0)


def open_token(path):
    """Open the token file, take the one-runner lock, read the token.

    Order is open → lock → read (sk-579 INV-13): a missing file is reported
    as missing, never as somebody else's lock. Raises Stop for every outcome
    that ends the runner; returns (fd, token) otherwise. The fd stays open for
    the life of the process — it holds the lock, which the kernel releases on
    any death of the process, SIGKILL included.
    """
    try:
        fd = os.open(path, os.O_RDONLY)
    except FileNotFoundError:
        raise Stop("token_file_missing")
    except OSError:
        # Unreadable (a directory, no permission): not ours to touch.
        raise Stop("token_file_missing")
    if fcntl is None:
        log_stderr("monitor runner: this platform has no OS file locks; running without the "
                   "one-runner lock, so a second runner on this token file is not prevented")
    else:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            raise Stop("already_running")
        except OSError as exc:
            # The lock guards against a duplicate; it is not a condition of
            # running (same decision as a platform without fcntl): say so
            # once and run without it.
            log_stderr("monitor runner: cannot lock the token file (%s); running without the "
                       "one-runner lock, so a second runner on this token file is not prevented" % exc)
    try:
        chunks = []
        while True:
            chunk = os.read(fd, 65536)
            if not chunk:
                break
            chunks.append(chunk)
        token = b"".join(chunks).decode("utf-8").strip()
    except (OSError, UnicodeDecodeError):
        os.close(fd)
        raise Stop("token_file_missing")
    if not token:
        os.close(fd)
        raise Stop("token_file_missing", delete_token=True)
    return fd, token


def build_url(base, cursor, exclude_session=""):
    poll_url = base.rstrip("/") + "/agent/v1/notifications/poll"
    params = {}
    # First poll: cursor is None → send no since_* params → server live-anchors.
    if cursor is not None:
        ts = cursor.get("ts")
        cid = cursor.get("id")
        if ts is not None and cid is not None:
            params["since_ts"] = ts
            params["since_id"] = cid
    # sk-164 FR-2: pass the agent's session for server best-effort echo
    # suppression. Sent on EVERY poll (incl. the first live-start) when
    # non-empty; empty → omitted so an unset SKEIN_EXCLUDE_SESSION leaves the
    # request byte-identical to the pre-rebind runner. Narrow-only server-side.
    if exclude_session:
        params["exclude_session"] = exclude_session
    if params:
        poll_url += "?" + urllib.parse.urlencode(params)
    return poll_url


# --- stop protocol (sk-579 US-3) ---------------------------------------------
# Every controlled stop prints exactly ONE last stdout line
# {"event", "reason", "action"[, "status"]}; "action" is a finished instruction
# to the agent with the root issue id filled in. The table below is the single
# place a reason's event, exit code and token-file fate are decided (the
# columns of the US-3 table). Diagnostics, tracebacks included, go to stderr.
#
# reason              event     exit  token file
# token_expired       closed    0     deleted
# visibility_lost     closed    0     deleted
# token_file_missing  closed    1     missing stays missing; empty → deleted;
#                                     unreadable → not touched
# misconfigured       closed    1     deleted when its path is known
# stopped             paused    0     kept
# internal_error      paused    1     kept
# root_closed         closed    0     deleted
# already_running     duplicate 0     not touched
STOPS = {
    "token_expired": ("closed", 0),
    "visibility_lost": ("closed", 0),
    "token_file_missing": ("closed", 1),
    "misconfigured": ("closed", 1),
    "stopped": ("paused", 0),
    "internal_error": ("paused", 1),
    "root_closed": ("closed", 0),
    "already_running": ("duplicate", 0),
}


def stop_action(reason, issue):
    mint = 'skein_get_monitor_script(id="%s")' % issue
    if reason == "token_expired":
        return ("The monitor for issue %s has ended: its token expired or was revoked. If you "
                "stopped it yourself, do nothing; otherwise mint a new monitor with %s." % (issue, mint))
    if reason == "visibility_lost":
        return ("The monitor for issue %s has ended because you no longer have access to its project. "
                "Do not mint it again until you regain access to that project; then mint a new "
                "monitor with %s." % (issue, mint))
    if reason == "token_file_missing":
        return ("The monitor for issue %s cannot run: its token file is missing, empty or unreadable. "
                "Mint a new monitor with %s." % (issue, mint))
    if reason == "misconfigured":
        return ("The bootstrap of the monitor for issue %s is broken (the server address or the token "
                "file path is not set). Mint a new monitor with %s." % (issue, mint))
    if reason == "root_closed":
        return ("Issue %s is closed, so its monitor has finished after delivering everything it owed. "
                "Nothing to do; if the issue is reopened and you still need to follow it, mint a new "
                "monitor with %s." % (issue, mint))
    if reason == "already_running":
        return ("A runner is already monitoring issue %s with this token file, so this second one "
                "stopped. Do nothing: the monitor is already running." % issue)
    if reason == "stopped":
        return ("The monitor for issue %s was stopped by a signal. Run the same bootstrap again to "
                "resume; its token stays valid until the renewal window runs out. Notifications from "
                "the break are not delivered — check the issue with skein_show(id=\"%s\")." % (issue, issue))
    return ("The monitor for issue %s hit an unexpected error. Run the same bootstrap again; if it "
            "happens again, mint a new monitor with %s." % (issue, mint))


class Stop(Exception):
    """A controlled stop: reason (a STOPS key), optional HTTP status, and
    whether the token file is to be deleted."""

    def __init__(self, reason, status=None, delete_token=False):
        global _stopping
        # Deciding a stop starts the final phase: from here on a signal no
        # longer raises, so it cannot replace this stop or print a second line.
        _stopping = True
        super().__init__(reason)
        self.reason = reason
        self.status = status
        self.delete_token = delete_token


class StopSignal(BaseException):
    """Raised by the SIGTERM/SIGINT handler. A BaseException so no ordinary
    handler swallows it on the way out."""


# Set once the runner has started its final stop: a late signal must not
# start a second stop line. The handler sets it too, so it raises at most once.
_stopping = False


def _on_signal(signum, frame):
    global _stopping
    if not _stopping:
        _stopping = True
        raise StopSignal()


class Executor:
    """The request executor process and its bounded wait.

    At most one executor process is alive at a time. A request that does not
    answer within REQUEST_LIMIT_SEC gets its executor killed and reaped, so its
    I/O (including a hung name resolution) stops; the next request starts a
    fresh executor.
    """

    def __init__(self):
        self.proc = None
        self.pending = []

    def _start(self):
        self.proc = subprocess.Popen(
            [sys.executable, "-c", EXECUTOR_SRC],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            close_fds=True,
        )
        os.set_blocking(self.proc.stdout.fileno(), False)
        self.pending = []

    def stop(self):
        """Kill and reap the executor, if any."""
        proc, self.proc = self.proc, None
        if proc is None:
            return
        try:
            proc.kill()
        except OSError:
            pass
        try:
            proc.wait()
        except OSError:
            pass
        for stream in (proc.stdin, proc.stdout):
            try:
                stream.close()
            except OSError:
                pass

    def request(self, url, token):
        """Run one request; returns the executor's answer dict, or a status-0
        failure dict when the executor died or the request outlived its limit."""
        if self.proc is None or self.proc.poll() is not None:
            self.stop()
            self._start()
        line = json.dumps({"url": url, "token": token, "timeout": REQUEST_LIMIT_SEC}) + "\n"
        try:
            self.proc.stdin.write(line.encode("utf-8"))
            self.proc.stdin.flush()
        except OSError:
            self.stop()
            return {"status": 0, "error": "executor_unavailable"}
        deadline = time.monotonic() + REQUEST_LIMIT_SEC
        fd = self.proc.stdout.fileno()
        # The answer is one line; collect chunks and join once (no quadratic
        # copying). The deadline bounds the WHOLE request, transfer of the
        # answer included: it is checked on every iteration and once more
        # before an answer is accepted.
        chunks = self.pending
        self.pending = []
        while True:
            if time.monotonic() >= deadline:
                self.stop()
                return {"status": 0, "error": "request_limit"}
            try:
                chunk = os.read(fd, 65536)
            except BlockingIOError:
                time.sleep(REQUEST_WAIT_STEP_SEC)
                continue
            if not chunk:
                # EOF: the executor died mid-request.
                self.stop()
                return {"status": 0, "error": "executor_died"}
            newline = chunk.find(b"\n")
            if newline < 0:
                chunks.append(chunk)
                continue
            chunks.append(chunk[:newline])
            self.pending = [chunk[newline + 1:]] if newline + 1 < len(chunk) else []
            if time.monotonic() >= deadline:
                self.stop()
                return {"status": 0, "error": "request_limit"}
            try:
                answer = json.loads(b"".join(chunks).decode("utf-8"))
            except ValueError:
                answer = None
            if isinstance(answer, dict):
                return answer
            self.stop()
            return {"status": 0, "error": "executor_garbled"}


def poll_once(executor, base, token, cursor, exclude_session=""):
    """Issue one poll.

    Returns (status, payload, retry_after): status is the HTTP status (0 for a
    network failure, a timeout or a request past its limit); payload is the
    decoded JSON value of a 2xx body, or None when it did not parse;
    retry_after is the raw Retry-After header of an HTTP error, or None.
    """
    answer = executor.request(build_url(base, cursor, exclude_session), token)
    status = answer.get("status")
    if not isinstance(status, int):
        return 0, None, None
    if 200 <= status < 300:
        try:
            return status, json.loads(answer.get("body") or ""), None
        except ValueError:
            return status, None, None
    return status, None, answer.get("retryAfter")


def retry_after_seconds(header, fallback):
    """Parse a Retry-After header (delta-seconds form); fallback otherwise."""
    if header:
        try:
            secs = int(str(header).strip())
            if secs >= 0:
                return secs
        except ValueError:
            pass
    return fallback


def failure_backoff(backoff):
    """Next backoff after a transient failure: doubled, capped at the ceiling."""
    return min(max(backoff, float(POLL_INTERVAL_SEC)) * 2, float(BACKOFF_CEILING_SEC))


def work(executor, base, token, issue, exclude_session):
    """The polling loop. Never returns: it ends by raising Stop."""
    emit({"event": "monitor_started", "issue": issue,
          "pollIntervalSec": POLL_INTERVAL_SEC})

    cursor = None
    seen = set()
    seen_order = []
    backoff = float(POLL_INTERVAL_SEC)
    # A run of consecutive failures (sk-579 US-1). There is NO failure
    # counter: while the token lives the runner keeps polling. The run
    # only decides the single "degraded" line (once it outlasts the stall
    # threshold) and the single "recovered" line on the first success
    # after it.
    series_started = None
    series_degraded = False

    while True:
        status, payload, retry_after = poll_once(executor, base, token, cursor, exclude_session)

        if status == 200 and isinstance(payload, dict) and isinstance(payload.get("notifications") or [], list):
            if series_degraded:
                emit({"event": "recovered", "issue": issue,
                      "failedForSec": int(time.monotonic() - series_started)})
            series_started = None
            series_degraded = False
            notifs = payload.get("notifications") or []
            new_count = 0
            for n in notifs:
                nid = n.get("id") if isinstance(n, dict) else None
                if nid is not None:
                    if nid in seen:
                        continue
                    seen.add(nid)
                    seen_order.append(nid)
                    if len(seen_order) > SEEN_IDS_CAP:
                        old = seen_order.pop(0)
                        seen.discard(old)
                emit({"event": "notification", "notification": n})
                new_count += 1
            # The root issue is closed and everything owed is delivered (the
            # server decides, INV-12): stop only after printing this page.
            if payload.get("rootClosed") is True:
                raise Stop("root_closed", delete_token=True)
            # Advance the watermark cursor to whatever the server returned.
            nxt = payload.get("cursor")
            if isinstance(nxt, dict):
                cursor = nxt
            if new_count > 0:
                # Active dialog → reset to the base cadence.
                backoff = float(POLL_INTERVAL_SEC)
            else:
                # Quiet but healthy → grow the sleep toward the ceiling.
                backoff = min(backoff + POLL_INTERVAL_SEC, float(BACKOFF_CEILING_SEC))
            time.sleep(jitter(backoff))
            continue

        if status == 401:
            # Token died (renewal window passed, 12h cap, revoked).
            raise Stop("token_expired", status=401, delete_token=True)
        if status == 403:
            # The owner lost visibility of the monitored project.
            raise Stop("visibility_lost", status=403, delete_token=True)

        if status == 400 and cursor is not None:
            # The server rejected the cursor we sent. Repeating it cannot
            # help: drop to a live-start poll and say once that the
            # notifications between the last accepted cursor and now may
            # be lost. Not part of a failure run — the next poll is fresh.
            cursor = None
            emit({"event": "degraded", "cause": "cursor_reset", "issue": issue,
                  "action": "The server rejected this monitor's poll cursor, so the monitor "
                            "restarted from now: notifications since its last successful poll "
                            "may have been missed — check issue %s with skein_show. "
                            "Do not restart anything; the monitor keeps running." % issue})
            time.sleep(jitter(float(POLL_INTERVAL_SEC)))
            continue

        # Every other outcome is a transient failure (network, timeout, a
        # request past its limit, 5xx, 429, a malformed 200, any other
        # 4xx): back off within the ceiling and poll again with the SAME
        # cursor.
        now = time.monotonic()
        if series_started is None:
            series_started = now
        if not series_degraded and now - series_started > STALL_THRESHOLD_SEC:
            series_degraded = True
            emit({"event": "degraded", "cause": "poll_failing", "issue": issue,
                  "failingForSec": int(now - series_started), "lastStatus": status,
                  "action": "Polls for issue %s have been failing for a while; the monitor "
                            "keeps retrying on its own, so do not restart it. If this does not "
                            "clear, check that the Skein server is reachable." % issue})
        if status == 429:
            wait = retry_after_seconds(retry_after, BACKOFF_CEILING_SEC)
            wait = min(max(float(wait), float(POLL_INTERVAL_SEC)), float(BACKOFF_CEILING_SEC))
            log_stderr("poll rate-limited (429); waiting up to %.1fs" % wait)
            time.sleep(jitter(wait))
            continue
        backoff = failure_backoff(backoff)
        if status == 200:
            log_stderr("malformed poll response; backing off %.1fs" % backoff)
        else:
            log_stderr("transient poll failure status=%s; backing off %.1fs" % (status, backoff))
        time.sleep(jitter(backoff))


def run():
    """Run the monitor; returns the process exit code. Prints exactly one stop
    line (the last stdout line) on every controlled stop."""
    global _stopping
    issue = os.environ.get("SKEIN_ISSUE", "")
    token_file = os.environ.get("SKEIN_TOKEN_FILE", "")
    executor = Executor()
    outcome = None
    lock_fd = None
    try:
        base = os.environ.get("SKEIN_PUBLIC_URL", "")
        # sk-164 FR-2: read once at start (detached runner — env is static for
        # the process lifetime). Empty = no server-side echo suppression.
        exclude_session = os.environ.get("SKEIN_EXCLUDE_SESSION", "")
        if not token_file or not base:
            raise Stop("misconfigured", delete_token=bool(token_file))
        # lock_fd holds the one-runner lock for as long as the runner works;
        # it is released only when run() ends (or the process dies).
        lock_fd, token = open_token(token_file)
        work(executor, base, token, issue, exclude_session)
    except Stop as stop:
        outcome = stop
    except StopSignal:
        outcome = Stop("stopped")
    except Exception:
        _stopping = True
        log_stderr("monitor runner: unexpected error\n" + traceback.format_exc())
        outcome = Stop("internal_error")
    finally:
        executor.stop()
        if lock_fd is not None:
            try:
                os.close(lock_fd)
            except OSError:
                pass
    return finish(outcome, issue, token_file)


def finish(outcome, issue, token_file):
    """Apply the token-file fate of the stop, print its line, return its code."""
    event, code = STOPS[outcome.reason]
    if outcome.delete_token and token_file:
        try:
            os.remove(token_file)
        except OSError:
            pass
    line = {"event": event, "reason": outcome.reason, "action": stop_action(outcome.reason, issue)}
    if outcome.status is not None:
        line["status"] = outcome.status
    emit(line)
    return code


def main():
    # SIGTERM / SIGINT are the normal ways the Monitor (or a person) stops the
    # runner. Both raise StopSignal in the main thread — never sys.exit in the
    # handler — so run() turns them into the single "stopped" line and keeps
    # the token file for a restart of the same bootstrap. The handlers are
    # installed INSIDE the try: a signal that lands outside run()'s own
    # handling (right after installation, during stdout set-up, at the very
    # start of one of run()'s except blocks) still ends in exactly one
    # "stopped" line. The handler raises at most once, so one catch suffices.
    try:
        signal.signal(signal.SIGTERM, _on_signal)
        signal.signal(signal.SIGINT, _on_signal)
        # Line-buffer stdout where supported (Python 3.7+); the explicit flush
        # in emit() covers older interpreters and pipes regardless. This is
        # the CRITICAL bit — without it the Monitor sees nothing until exit.
        reconfigure = getattr(sys.stdout, "reconfigure", None)
        if callable(reconfigure):
            try:
                reconfigure(line_buffering=True)
            except ValueError:
                pass
        code = run()
    except StopSignal:
        code = finish(Stop("stopped"), os.environ.get("SKEIN_ISSUE", ""),
                      os.environ.get("SKEIN_TOKEN_FILE", ""))
    sys.exit(code)


if __name__ == "__main__":
    main()
