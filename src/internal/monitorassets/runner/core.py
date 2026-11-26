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
#   - The skm_ token is read once from the file at SKEIN_TOKEN_FILE — never
#     from argv / process-env / shell history. The file must be no one else's
#     to read: ours, not a symlink, and either without group/other bits or
#     inside a directory of ours without them (the mint's `prepare`);
#     otherwise the runner stops with token_file_insecure. The file's fate on each
#     stop is the token-file column of the stop table: it is kept on a
#     signal or an internal error, so the same bootstrap can restart the
#     monitor while the token lives, and deleted when the token is dead.
#
# Cursor (spec §6 / §9):
#   - The FIRST poll of a new token sends NO since_ts/since_id; the server
#     live-anchors the cursor at its own now() so the runner gets no
#     historical backlog.
#   - Every later poll echoes back the cursor (ts, id) from the prior response.
#   - The cursor is saved next to the token (state.json, the state part) after
#     every printed page, so a restart with the same token file resumes from
#     it; the state file is deleted with the token file.
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
import signal
import subprocess
import sys
import time
import traceback
import urllib.parse

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
# The sliding renewal window of the monitor token (whole seconds): the
# server renews the token on every successful poll for this long.
TOKEN_IDLE_SEC = int("__SKEIN_TOKEN_IDLE_SEC__")
# The monitor registry (registry.json of the server, base64): the agents,
# their methods and the delivery channels. It travels inside the body so the
# body's SHA-256, which the loader checks, covers it too.
REGISTRY_B64 = "__SKEIN_REGISTRY_B64__"

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
# Redirects are not followed: urllib would resend X-Skein-Api-Key to any
# host and scheme; a 3xx comes back as an HTTP error status instead.
EXECUTOR_SRC = r"""
import json, sys, urllib.error, urllib.request
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None
opener = urllib.request.build_opener(NoRedirect)
for line in sys.stdin:
    req = json.loads(line)
    try:
        r = urllib.request.Request(req["url"], headers={"X-Skein-Api-Key": req["token"]})
        with opener.open(r, timeout=req["timeout"]) as resp:
            out = {"status": resp.status, "body": resp.read().decode("utf-8", "replace")}
    except urllib.error.HTTPError as exc:
        out = {"status": exc.code, "retryAfter": exc.headers.get("Retry-After") if exc.headers else None}
    except Exception as exc:
        out = {"status": 0, "error": type(exc).__name__}
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
"""


def build_url(base, cursor, exclude_session="", limit=None):
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
    # A client-mode poll names its page size: a full page means more may
    # follow (reading on after rootClosed, rootclosed.py).
    if limit is not None:
        params["limit"] = limit
    if params:
        poll_url += "?" + urllib.parse.urlencode(params)
    return poll_url


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


def poll_once(executor, base, token, cursor, exclude_session="", limit=None):
    """Issue one poll. exclude_session is the mint's session id or the
    Exclusion of a client-mode monitor, read again for this poll (US-13);
    limit is the page size a client-mode poll asks for.

    Returns (status, payload, retry_after): status is the HTTP status (0 for a
    network failure, a timeout or a request past its limit); payload is the
    decoded JSON value of a 2xx body, or None when it did not parse;
    retry_after is the raw Retry-After header of an HTTP error, or None.
    """
    answer = executor.request(build_url(base, cursor, exclusion_value(exclude_session), limit), token)
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


class StdoutSink(object):
    """Where the lines of a legacy monitor go: stdout, one JSON line each,
    beginning with its monitor_started line."""

    def __init__(self, issue):
        self.issue = issue

    def started(self):
        emit({"event": "monitor_started", "issue": self.issue,
              "pollIntervalSec": POLL_INTERVAL_SEC})

    def lifecycle(self, line):
        emit(line)

    def notification(self, n):
        emit({"event": "notification", "notification": n})


def work(executor, base, token, issue, exclude_session, token_file, sink=None, root=None):
    """The polling loop. Never returns: it ends by raising Stop. sink takes
    the lines (stdout by default); a sink with page(cursor, notifications)
    takes each page whole. A sink that raises JournalUnavailable for a
    notification or a page leaves the page undone: the cursor stays and the
    same page is polled again. root is the RootClosed of a client-mode
    monitor, which reads on after rootClosed (rootclosed.py); without it the
    first page with rootClosed ends the monitor."""
    if sink is None:
        sink = StdoutSink(issue)
    if hasattr(sink, "started"):
        sink.started()

    # A restart with the same token file resumes from the saved cursor; a
    # new token (no state) starts live.
    cursor = load_cursor(token_file)
    # The cursor the state file holds: a failed save is retried on every
    # later page until the file matches the cursor in memory again.
    saved = [cursor]
    state_failed = [False]

    def persist(value):
        # The cursor is saved only after its page was printed. A state that
        # cannot be written costs the resume after a restart, not the run:
        # say so once and go on.
        if value == saved[0]:
            return
        try:
            save_cursor(token_file, value, root.record() if root is not None else None)
            saved[0] = value
            state_failed[0] = False
        except (OSError, PlatformRefusal) as exc:
            if not state_failed[0]:
                log_stderr("monitor runner: cannot save the poll cursor (%s); until it is saved, "
                           "a restart resumes from an older cursor or from now" % exc)
            state_failed[0] = True

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
        if hasattr(sink, "maybe_tails"):
            # A journal-only monitor appends the user's deferred tails
            # periodically too (a detached one does it with its pages).
            sink.maybe_tails()
        status, payload, retry_after = poll_once(executor, base, token, cursor, exclude_session,
                                                 POLL_PAGE_LIMIT if root is not None else None)

        if status == 200 and isinstance(payload, dict) and isinstance(payload.get("notifications") or [], list):
            if root is not None:
                root.received(cursor, payload.get("notifications") or [], payload.get("rootClosed") is True)
            if hasattr(sink, "note_poll_ok"):
                sink.note_poll_ok()
            if series_degraded:
                sink.lifecycle({"event": "recovered", "issue": issue,
                                "failedForSec": int(time.monotonic() - series_started)})
            series_started = None
            series_degraded = False
            notifs = payload.get("notifications") or []
            new_count = 0
            try:
                if hasattr(sink, "page"):
                    # A detached sink carries the whole page to outcomes
                    # (deliver.py); its state, not the seen set, says what
                    # is done when the page comes again.
                    sink.page(cursor, notifs)
                    new_count = len(notifs)
                    notifs = []
                for n in notifs:
                    nid = n.get("id") if isinstance(n, dict) else None
                    if nid is not None and nid in seen:
                        continue
                    sink.notification(n)
                    new_count += 1
                    if nid is not None:
                        seen.add(nid)
                        seen_order.append(nid)
                        if len(seen_order) > SEEN_IDS_CAP:
                            old = seen_order.pop(0)
                            seen.discard(old)
            except JournalUnavailable as exc:
                # The page is not done: the same cursor is polled again; the
                # events already written are skipped by the seen set.
                log_stderr("monitor runner: the journal cannot be written (%s); the page is "
                           "polled again" % exc.reason)
                time.sleep(jitter(float(POLL_INTERVAL_SEC)))
                continue
            nxt = payload.get("cursor")
            nxt = nxt if isinstance(nxt, dict) else cursor
            if root is not None:
                # Every event of the page has its outcome: count them, then
                # stop or read on (rootclosed.py).
                if hasattr(sink, "page"):
                    root.tally(load_page_outcomes(token_file, cursor)[0].values())
                if root.page_done(cursor, nxt):
                    if hasattr(sink, "journal"):
                        note_read_limited(sink.journal, root)
                    raise Stop("root_closed", delete_token=True, detail=root.summary())
            elif payload.get("rootClosed") is True:
                # The root issue is closed and everything owed is delivered
                # (the server decides, INV-12): stop only after printing
                # this page.
                raise Stop("root_closed", delete_token=True)
            # Advance the watermark cursor to whatever the server returned.
            cursor = nxt
            persist(cursor)
            if new_count > 0:
                # Active dialog → reset to the base cadence.
                backoff = float(POLL_INTERVAL_SEC)
            else:
                # Quiet but healthy → grow the sleep toward the ceiling.
                backoff = min(backoff + POLL_INTERVAL_SEC, float(BACKOFF_CEILING_SEC))
            # At most one page per poll interval: a tail waiting after a
            # restart comes no faster than ordinary polling.
            sleep_or_hand_over(max(jitter(backoff), float(POLL_INTERVAL_SEC)), sink)
            continue

        if status in (401, 403) and hasattr(sink, "end_open_page"):
            # A page the journal left undone gets its outcomes by the lost
            # access before the stop.
            sink.end_open_page("token_expired" if status == 401 else "visibility_lost")
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
            persist(None)
            sink.lifecycle({"event": "degraded", "cause": "cursor_reset", "issue": issue,
                            "action": "The server rejected this monitor's poll cursor, so the monitor "
                                      "restarted from now: notifications since its last successful poll "
                                      "may have been missed — check issue %s with skein_show. "
                                      "Do not restart anything; the monitor keeps running." % issue})
            sleep_or_hand_over(jitter(float(POLL_INTERVAL_SEC)), sink)
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
            sink.lifecycle({"event": "degraded", "cause": "poll_failing", "issue": issue,
                            "failingForSec": int(now - series_started), "lastStatus": status,
                            "action": "Polls for issue %s have been failing for a while; the monitor "
                                      "keeps retrying on its own, so do not restart it. If this does not "
                                      "clear, check that the Skein server is reachable." % issue})
        if status == 429:
            wait = retry_after_seconds(retry_after, BACKOFF_CEILING_SEC)
            wait = min(max(float(wait), float(POLL_INTERVAL_SEC)), float(BACKOFF_CEILING_SEC))
            log_stderr("poll rate-limited (429); waiting up to %.1fs" % wait)
            sleep_or_hand_over(jitter(wait), sink)
            continue
        backoff = failure_backoff(backoff)
        if status == 200:
            log_stderr("malformed poll response; backing off %.1fs" % backoff)
        else:
            log_stderr("transient poll failure status=%s; backing off %.1fs" % (status, backoff))
        sleep_or_hand_over(jitter(backoff), sink)


def run():
    """Run the monitor; returns the process exit code. Prints exactly one stop
    line (the last stdout line) on every controlled stop. In client mode this
    is the working start (run_client of the detach part), and in the detached
    child it starts, the child's run (run_detached_child)."""
    global _stopping
    if LOADER_MODE == "client":
        return run_detached_child() if DETACHED_CHILD else run_client()
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
        work(executor, base, token, issue, exclude_session, token_file)
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


def run_preflight():
    """Run the preflight; returns the process exit code. Prints the preflight
    line, or exactly one stop line."""
    global _stopping
    issue = os.environ.get("SKEIN_ISSUE", "")
    token_file = os.environ.get("SKEIN_TOKEN_FILE", "")
    try:
        if not token_file or not os.environ.get("SKEIN_PUBLIC_URL", ""):
            raise Stop("misconfigured", delete_token=bool(token_file))
        registry = load_registry()
        settings = read_settings(os.environ, registry)
        emit(preflight(token_file, dict(os.environ), registry, settings))
        return 0
    except ConfigInvalid as exc:
        outcome = Stop("config_invalid", detail="%s (%s)" % (exc.variable, exc.detail))
    except Stop as stop:
        outcome = stop
    except StopSignal:
        outcome = Stop("stopped")
    except Exception:
        _stopping = True
        log_stderr("monitor runner: unexpected error in preflight\n" + traceback.format_exc())
        outcome = Stop("internal_error")
    return finish(outcome, issue, token_file)


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
        code = start()
    except StopSignal:
        code = finish(Stop("stopped"), os.environ.get("SKEIN_ISSUE", ""),
                      os.environ.get("SKEIN_TOKEN_FILE", ""))
    sys.exit(code)


if __name__ == "__main__":
    main()
