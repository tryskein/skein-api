#!/usr/bin/env python3
"""Stdlib smoke test for monitor_runner.py (sk-142.6).

Pure standard library — unittest + http.server (a fake poll endpoint) +
subprocess. NO pytest, NO third-party deps (the test image has neither).

Run directly:

    python3 internal/handler/assets/monitor_runner_test.py

or, from the assets dir:

    cd internal/handler/assets && python3 monitor_runner_test.py

It substitutes the cadence placeholder (poll=1s) into a temp copy of the
runner exactly as the Go handler does, then drives it against a controllable
fake server and asserts the stdout/lifecycle contract.
"""

import atexit
import contextlib
import io
import json
import os
import queue
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

HERE = os.path.dirname(os.path.abspath(__file__))
RUNNER_SRC = os.path.join(HERE, "monitor_runner.py")
# Shared interpreter hooks for runner subprocesses (virtual clock, pinned
# random, blocked resolver) — see its header.
SITE_DIR = os.path.join(HERE, "..", "testdata", "runner_site")
# The bootstrap loader asset (run as `python3 -c`, exactly like the bootstrap).
LOADER_SRC = os.path.join(HERE, "..", "..", "mcp", "tools", "assets", "monitor_loader.py")


_TMPDIRS = []


def make_tmpdir(prefix):
    """A temporary directory removed when the test process exits."""
    path = tempfile.mkdtemp(prefix=prefix)
    _TMPDIRS.append(path)
    return path


@atexit.register
def _remove_tmpdirs():
    for path in _TMPDIRS:
        shutil.rmtree(path, ignore_errors=True)


def render_runner(poll_sec=1, ceiling_sec=10, limit_sec=15, stall_sec=180):
    """Substitute the timing placeholders the same way agent_script.go does."""
    with open(RUNNER_SRC, "r") as fh:
        body = fh.read()
    body = body.replace("__SKEIN_POLL_INTERVAL_SEC__", str(poll_sec))
    body = body.replace("__SKEIN_BACKOFF_CEILING_SEC__", str(ceiling_sec))
    body = body.replace("__SKEIN_REQUEST_LIMIT_SEC__", str(limit_sec))
    body = body.replace("__SKEIN_STALL_THRESHOLD_SEC__", str(stall_sec))
    if "__SKEIN_" in body:
        raise AssertionError("placeholder left unsubstituted in rendered runner")
    return body


class _FakeHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        self.server.requests.append((parsed.path, qs))
        mode = self.server.mode
        if mode == "notify":
            # sk-164 FR-3: the notification carries actorSessionId; the runner
            # must pass the whole notification object through verbatim (the
            # reader-side self-skip happens in the agent, not the runner).
            payload = {
                "notifications": [{"id": 1, "type": "comment.created", "issueId": "sk-1",
                                   "actorSessionId": "mcp-session-peer"}],
                "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 1},
            }
            self._json(200, payload)
        elif mode == "empty":
            self._json(200, {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 0}})
        elif mode == "401":
            self._json(401, {"error": "unauthorized"})
        elif mode == "403":
            # sk-164 INV-8: fail-closed visibility loss — a TERMINAL 403, distinct
            # from 401 token death. The runner must exit 0 with a closed line whose
            # status is 403 (so the agent can tell visibility-loss from revocation).
            self._json(403, {"error": "visibility_lost"})
        elif mode == "dup":
            # Always return the SAME notification id: the runner's seen-set
            # must dedupe it so it is emitted exactly once across many polls.
            self._json(200, {
                "notifications": [{"id": 7, "type": "comment.created", "issueId": "sk-1"}],
                "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 7},
            })
        else:
            self._json(200, {"notifications": [], "cursor": {}})

    def _json(self, status, obj, extra_headers=None):
        self._raw(status, json.dumps(obj).encode("utf-8"),
                  content_type="application/json", extra_headers=extra_headers)

    def _raw(self, status, body, content_type="application/json", extra_headers=None):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        for k, v in (extra_headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass  # keep the test output clean


class RunnerHarness:
    """Starts a fake poll server + the runner subprocess, streaming stdout lines."""

    def __init__(self, mode, poll_sec=1, exclude_session=None):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _FakeHandler)
        self.server.mode = mode
        self.server.requests = []
        self.port = self.server.server_address[1]
        self.httpd_thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.httpd_thread.start()

        self.tmpdir = make_tmpdir(prefix="skein-runner-test-")
        self.runner_path = os.path.join(self.tmpdir, "runner.py")
        with open(self.runner_path, "w") as fh:
            fh.write(render_runner(poll_sec=poll_sec))
        self.token_path = os.path.join(self.tmpdir, "tok")
        with open(self.token_path, "w") as fh:
            fh.write("skm_" + "a" * 43)
        os.chmod(self.token_path, 0o600)

        env = dict(os.environ)
        env["SKEIN_PUBLIC_URL"] = "http://127.0.0.1:%d" % self.port
        env["SKEIN_TOKEN_FILE"] = self.token_path
        env["SKEIN_ISSUE"] = "sk-1"
        # sk-164 FR-2: the rebind session, sent as exclude_session on every poll.
        # A None value leaves the env var UNSET (the pre-rebind runner contract).
        if exclude_session is not None:
            env["SKEIN_EXCLUDE_SESSION"] = exclude_session
        else:
            env.pop("SKEIN_EXCLUDE_SESSION", None)
        self.proc = subprocess.Popen(
            [sys.executable, self.runner_path],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            env=env,
        )
        self.lines = queue.Queue()
        self._reader = threading.Thread(target=self._pump, daemon=True)
        self._reader.start()
        # stderr is collected on its own thread so a backoff/malformed test can
        # assert a diagnostic was logged without blocking on the live pipe.
        self.errlines = []
        self._errreader = threading.Thread(target=self._pump_err, daemon=True)
        self._errreader.start()

    def _pump(self):
        try:
            for line in self.proc.stdout:
                self.lines.put(line.rstrip("\n"))
        finally:
            self.lines.put(None)

    def _pump_err(self):
        try:
            for line in self.proc.stderr:
                self.errlines.append(line.rstrip("\n"))
        except (ValueError, OSError):
            pass

    def stderr_text(self):
        return "\n".join(self.errlines)

    def next_line(self, timeout):
        return self.lines.get(timeout=timeout)

    def read_event(self, event, timeout):
        """Block until a JSON line with {"event": event} appears, or time out."""
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AssertionError("timed out waiting for event %r" % event)
            line = self.lines.get(timeout=remaining)
            if line is None:
                raise AssertionError("runner stdout closed before event %r" % event)
            try:
                obj = json.loads(line)
            except ValueError:
                continue
            if obj.get("event") == event:
                return obj

    def try_read_event(self, event, timeout):
        """Like read_event but returns None on timeout instead of raising.

        Used to assert an event does NOT appear within a window (dedupe).
        """
        try:
            return self.read_event(event, timeout)
        except (AssertionError, queue.Empty):
            return None

    def close(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)
        for stream in (self.proc.stdout, self.proc.stderr):
            try:
                if stream is not None:
                    stream.close()
            except OSError:
                pass
        self.server.shutdown()
        self.server.server_close()


class MonitorRunnerTest(unittest.TestCase):
    def test_emits_lines_immediately_unbuffered(self):
        """monitor_started must reach the parent before the process exits."""
        h = RunnerHarness("empty")
        try:
            obj = h.read_event("monitor_started", timeout=8)
            self.assertEqual(obj.get("issue"), "sk-1")
            # The process must still be alive: the line came from a flush, not
            # from the pipe draining on exit.
            self.assertIsNone(h.proc.poll(), "runner exited instead of streaming live")
        finally:
            h.close()

    def test_first_poll_has_no_cursor(self):
        """First poll sends NO since_ts/since_id (server live-anchors)."""
        h = RunnerHarness("empty")
        try:
            h.read_event("monitor_started", timeout=8)
            # Wait until at least one poll request landed.
            deadline = time.monotonic() + 8
            while not h.server.requests and time.monotonic() < deadline:
                time.sleep(0.05)
            self.assertTrue(h.server.requests, "no poll request reached the fake server")
            _, first_qs = h.server.requests[0]
            self.assertNotIn("since_ts", first_qs)
            self.assertNotIn("since_id", first_qs)
        finally:
            h.close()

    def test_notification_line(self):
        """A 200 with a notification → one notification JSON line on stdout.

        sk-164 FR-3: the runner passes the notification object through VERBATIM,
        including actorSessionId — reader-side self-skip is the agent's job, not
        the runner's, so the field must survive the emit intact.
        """
        h = RunnerHarness("notify")
        try:
            obj = h.read_event("notification", timeout=8)
            self.assertEqual(obj["notification"]["id"], 1)
            self.assertEqual(obj["notification"]["type"], "comment.created")
            # actorSessionId must pass through unchanged (echo self-skip marker).
            self.assertEqual(obj["notification"].get("actorSessionId"), "mcp-session-peer",
                             "runner must pass actorSessionId through verbatim")
        finally:
            h.close()

    def test_exclude_session_sent_on_every_poll(self):
        """sk-164 FR-2: a set SKEIN_EXCLUDE_SESSION rides on EVERY poll request.

        The bootstrap bakes the session; the runner must attach it as the
        exclude_session query-param on every poll (including the first
        live-start poll), or the server-side echo suppression never engages.
        """
        h = RunnerHarness("empty", exclude_session="mcp-session-self")
        try:
            h.read_event("monitor_started", timeout=8)
            deadline = time.monotonic() + 8
            while len(h.server.requests) < 2 and time.monotonic() < deadline:
                time.sleep(0.05)
            self.assertGreaterEqual(len(h.server.requests), 2, "need >=2 polls to prove 'every poll'")
            for path, qs in h.server.requests:
                self.assertIn("exclude_session", qs,
                              "exclude_session must be on every poll (missing on %s %s)" % (path, qs))
                self.assertEqual(qs["exclude_session"], ["mcp-session-self"])
        finally:
            h.close()

    def test_exclude_session_absent_when_unset(self):
        """No SKEIN_EXCLUDE_SESSION → the poll carries no exclude_session param.

        A legacy / no-rebind runner must produce a byte-identical request to the
        pre-sk-164 runner (the param is purely additive).
        """
        h = RunnerHarness("empty")  # exclude_session unset
        try:
            h.read_event("monitor_started", timeout=8)
            deadline = time.monotonic() + 8
            while not h.server.requests and time.monotonic() < deadline:
                time.sleep(0.05)
            self.assertTrue(h.server.requests, "no poll request reached the fake server")
            for _, qs in h.server.requests:
                self.assertNotIn("exclude_session", qs,
                                 "exclude_session must be omitted when SKEIN_EXCLUDE_SESSION is unset")
        finally:
            h.close()

    def test_duplicate_id_emitted_once(self):
        """The same notification id on successive polls is emitted exactly once."""
        h = RunnerHarness("dup")
        try:
            first = h.read_event("notification", timeout=8)
            self.assertEqual(first["notification"]["id"], 7)
            # The server keeps returning id=7 on every poll; the seen-set must
            # suppress all repeats. No second notification within the window.
            dup = h.try_read_event("notification", timeout=4)
            # The runner must still be ALIVE — otherwise a crashed/exited runner
            # (stdout closed) would make try_read_event return None and be
            # mistaken for "no duplicate".
            self.assertIsNone(h.proc.poll(), "runner exited during dedupe window — None above is not a real no-dup")
            self.assertIsNone(dup, "duplicate notification id was emitted more than once")
            # And the server really was polled repeatedly (proves dedupe, not
            # just a single poll).
            self.assertGreaterEqual(len(h.server.requests), 2,
                                    "runner only polled once — dedupe not exercised")
        finally:
            h.close()



# ---------------------------------------------------------------------------
# In-process harness (sk-579 matrix rows 37–41).
#
# The rendered runner is loaded as a module and run() is called directly. Its
# `time` and `random` names are replaced in the module namespace: sleeps are
# virtual (recorded, never waited) and jitter is pinned. A sleep shorter than
# 0.1 s is the executor wait step: it advances the virtual clock by its own
# length after a tiny real pause, so a real request in flight can finish while
# a hung one reaches its limit in milliseconds of real time. The run is
# stopped by StopRun (a BaseException, so no handler in run() swallows it).
# Requests go through the runner's real executor process to a local server.
# ---------------------------------------------------------------------------

_real_sleep = time.sleep


class StopRun(BaseException):
    pass


class FakeTime:
    def __init__(self, budget=None, on_sleep=None, on_budget=None):
        self.now = 1000.0
        self.start = self.now
        self.sleeps = []
        self.budget = budget
        self.on_sleep = on_sleep
        self.on_budget = on_budget

    def elapsed(self):
        return self.now - self.start

    def monotonic(self):
        return self.now

    def time(self):
        return self.now

    def sleep(self, seconds):
        seconds = float(seconds)
        if seconds < 0.1:
            _real_sleep(0.002)
            self.now += seconds
        else:
            self.sleeps.append(seconds)
            self.now += seconds
            if self.on_sleep is not None:
                self.on_sleep(self)
        if self.budget is not None and self.elapsed() >= self.budget:
            if self.on_budget is not None:
                self.on_budget(self)
            raise StopRun()


class FakeRandom:
    def __init__(self, value):
        self.value = value

    def random(self):
        return self.value


def load_runner(**timings):
    mod = types.ModuleType("monitor_runner_under_test")
    exec(compile(render_runner(**timings), RUNNER_SRC, "exec"), mod.__dict__)
    return mod


class _ScriptedHandler(BaseHTTPRequestHandler):
    """Answers each poll with whatever server.respond(handler, qs) decides."""

    def do_GET(self):
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        self.server.requests.append(qs)
        self.server.respond(self, qs)

    def send_json(self, status, obj, headers=None):
        self.send_raw(status, json.dumps(obj).encode("utf-8"), headers)

    def send_raw(self, status, body, headers=None):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def reset_connection(self):
        """Abort the TCP connection (RST) without any HTTP response."""
        self.request.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
        self.close_connection = True
        self.request.close()

    def log_message(self, *args):
        pass


class ScriptedServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, respond):
        super().__init__(("127.0.0.1", 0), _ScriptedHandler)
        self.respond = respond
        self.requests = []
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def url(self):
        return "http://127.0.0.1:%d" % self.server_address[1]

    def handle_error(self, request, client_address):
        pass  # aborted connections are the point of some scenarios

    def stop(self):
        self.shutdown()
        self.server_close()


EMPTY = {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 0}}

# The transient failure classes of US-1, as server behaviours.
FAILURE_CLASSES = {
    "connection_reset": lambda h: h.reset_connection(),
    "http_500": lambda h: h.send_json(500, {"error": "boom"}),
    "http_429_retry_after": lambda h: h.send_json(429, {"error": "slow"}, {"Retry-After": "900"}),
    "http_429_no_header": lambda h: h.send_json(429, {"error": "slow"}),
    "malformed_json": lambda h: h.send_raw(200, b"this-is-not-json{{{"),
    "non_object_200": lambda h: h.send_json(200, [1, 2, 3]),
    "http_404": lambda h: h.send_json(404, {"error": "nope"}),
    "http_409": lambda h: h.send_json(409, {"error": "conflict"}),
}


@contextlib.contextmanager
def runner_env(base_url, extra=None):
    """Point run() at base_url with a fresh 0600 token file; restore env after."""
    tmpdir = make_tmpdir(prefix="skein-runner-inproc-")
    token_path = os.path.join(tmpdir, "tok")
    with open(token_path, "w") as fh:
        fh.write("skm_" + "b" * 43)
    os.chmod(token_path, 0o600)
    values = {"SKEIN_PUBLIC_URL": base_url, "SKEIN_TOKEN_FILE": token_path,
              "SKEIN_ISSUE": "zz-77", "SKEIN_EXCLUDE_SESSION": None}
    values.update(extra or {})
    saved = {k: os.environ.get(k) for k in values}
    try:
        for k, v in values.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        yield token_path
    finally:
        for k, v in saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v


def run_in_process(mod, fake_time, rnd=1.0):
    """Run mod.run() under fake time/random; returns (rc, stdout JSON lines).
    rc is None when the run was stopped by the test (it never returned)."""
    mod.time = fake_time
    mod.random = FakeRandom(rnd)
    out = io.StringIO()
    err = io.StringIO()
    rc = None
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        try:
            rc = mod.run()
        except StopRun:
            rc = None
    lines = [json.loads(line) for line in out.getvalue().splitlines() if line.strip()]
    return rc, lines


def events(lines, name):
    return [l for l in lines if l.get("event") == name]


STOP_EVENTS = ("closed", "error", "paused", "duplicate")


class TransientFailureTest(unittest.TestCase):
    """Row 37 (INV-2, SC-001, R2): no count and no duration of transient
    failures ends the runner while the token lives; recovery resumes from the
    last accepted cursor."""

    def test_series_of_each_class_never_stops_and_resumes_cursor(self):
        twelve_hours = 12 * 3600
        for name, fail in FAILURE_CLASSES.items():
            with self.subTest(failure=name):
                state = {"phase": "first", "failures": 0}

                def respond(h, qs, state=state, fail=fail):
                    if state["phase"] == "first":
                        state["phase"] = "fail"
                        h.send_json(200, {"notifications": [],
                                          "cursor": {"ts": "2026-01-01T00:00:01Z", "id": 5}})
                    elif state["phase"] == "fail":
                        state["failures"] += 1
                        fail(h)
                    else:
                        state["phase"] = "done"
                        h.send_json(200, {"notifications": [{"id": 99, "type": "comment.created"}],
                                          "cursor": {"ts": "2026-01-01T00:00:02Z", "id": 99}})

                def on_sleep(ft, state=state):
                    if state["phase"] == "fail" and state["failures"] >= 50 and ft.elapsed() > twelve_hours:
                        state["phase"] = "recover"
                    elif state["phase"] == "done":
                        raise StopRun()

                srv = ScriptedServer(respond)
                try:
                    mod = load_runner(poll_sec=1, ceiling_sec=1000, limit_sec=15, stall_sec=60)
                    with runner_env(srv.url()) as token_path:
                        ft = FakeTime(on_sleep=on_sleep)
                        rc, lines = run_in_process(mod, ft)
                    self.assertIsNone(rc, "run() returned during a transient failure series")
                    self.assertEqual(state["phase"], "done", "runner never reached the recovery poll")
                    self.assertGreaterEqual(state["failures"], 50)
                    self.assertGreater(ft.elapsed(), twelve_hours)
                    self.assertFalse([l for l in lines if l.get("event") in STOP_EVENTS],
                                     "a stop line was printed during the failure series: %r" % lines)
                    # Every poll after the first (failures and recovery) re-sent the
                    # last ACCEPTED cursor — a transient failure never resets it.
                    for qs in srv.requests[1:]:
                        self.assertEqual(qs.get("since_ts"), ["2026-01-01T00:00:01Z"])
                        self.assertEqual(qs.get("since_id"), ["5"])
                    notifs = events(lines, "notification")
                    self.assertEqual([n["notification"]["id"] for n in notifs], [99])
                finally:
                    srv.stop()

    def test_401_after_a_degraded_series_stops_once(self):
        state = {"phase": "fail"}

        def respond(h, qs):
            if state["phase"] == "fail":
                h.send_json(500, {"error": "boom"})
            else:
                h.send_json(401, {"error": "unauthenticated"})

        def on_sleep(ft):
            if ft.elapsed() > 120:
                state["phase"] = "dead"

        srv = ScriptedServer(respond)
        try:
            mod = load_runner(poll_sec=1, ceiling_sec=10, limit_sec=15, stall_sec=60)
            with runner_env(srv.url()) as token_path:
                rc, lines = run_in_process(mod, FakeTime(budget=3600, on_sleep=on_sleep))
            self.assertEqual(len(events(lines, "degraded")), 1, lines)
            stops = [l for l in lines if l.get("event") in STOP_EVENTS]
            self.assertEqual(len(stops), 1, "want exactly one stop line, got %r" % lines)
            self.assertIs(stops[0], lines[-1], "the stop line must be the last line")
            self.assertEqual(stops[0].get("reason"), "token_expired")
            self.assertEqual(stops[0].get("status"), 401)
            self.assertEqual(rc, 0)
            self.assertFalse(os.path.exists(token_path), "token file kept after 401")
        finally:
            srv.stop()


class RetryAfterAndCeilingTest(unittest.TestCase):
    """Row 38: Retry-After is honoured but capped by the substituted ceiling;
    every transient class saturates at the ceiling; jitter is kept."""

    def _first_sleep(self, header, ceiling, rnd):
        def respond(h, qs):
            h.send_json(429, {"error": "slow"}, {"Retry-After": header})

        def on_sleep(ft):
            raise StopRun()

        srv = ScriptedServer(respond)
        try:
            mod = load_runner(poll_sec=1, ceiling_sec=ceiling, limit_sec=15, stall_sec=600)
            with runner_env(srv.url()):
                ft = FakeTime(on_sleep=on_sleep)
                run_in_process(mod, ft, rnd=rnd)
            return ft.sleeps[0]
        finally:
            srv.stop()

    def test_retry_after_capped_by_ceiling(self):
        self.assertEqual(self._first_sleep("100000", 13, 1.0), 13.0)
        self.assertEqual(self._first_sleep("100000", 13, 0.0), 6.5)

    def test_retry_after_below_ceiling_is_honoured(self):
        self.assertEqual(self._first_sleep("6", 10, 1.0), 6.0)
        self.assertEqual(self._first_sleep("6", 10, 0.0), 3.0)

    def test_every_class_saturates_at_the_ceiling(self):
        classes = dict(FAILURE_CLASSES)
        del classes["http_429_retry_after"]
        for name, fail in classes.items():
            for rnd, want in ((1.0, 13.0), (0.0, 6.5)):
                with self.subTest(failure=name, r=rnd):
                    def respond(h, qs, fail=fail):
                        fail(h)

                    def on_sleep(ft):
                        if len(ft.sleeps) >= 12:
                            raise StopRun()

                    srv = ScriptedServer(respond)
                    try:
                        mod = load_runner(poll_sec=1, ceiling_sec=13, limit_sec=15, stall_sec=600)
                        with runner_env(srv.url()):
                            ft = FakeTime(on_sleep=on_sleep)
                            run_in_process(mod, ft, rnd=rnd)
                    finally:
                        srv.stop()
                    self.assertTrue(all(s <= 13.0 for s in ft.sleeps), ft.sleeps)
                    self.assertEqual(ft.sleeps[-3:], [want] * 3, ft.sleeps)


class RequestLimitTest(unittest.TestCase):
    """Row 39 (INV-6, decision "Single runner request limit"): a request past
    its limit has its executor killed; attempts go on; at most one executor
    beyond the current one is alive; a hung executor does not delay a 401."""

    LIMIT = 5
    CEILING = 10

    def _alive(self, pid):
        try:
            os.kill(pid, 0)
        except OSError:
            return False
        return True

    def test_blocked_resolver_executors_are_killed(self):
        tmpdir = make_tmpdir(prefix="skein-runner-dns-")
        dns_log = os.path.join(tmpdir, "dns")
        snapshot = {}
        max_alive = [0]

        def pids():
            try:
                with open(dns_log) as fh:
                    return [int(line.split()[0]) for line in fh if line.strip()]
            except OSError:
                return []

        def on_sleep(ft):
            max_alive[0] = max(max_alive[0], sum(1 for p in pids() if self._alive(p)))

        def on_budget(ft):
            snapshot["pids"] = pids()
            snapshot["alive"] = [self._alive(p) for p in snapshot["pids"]]

        mod = load_runner(poll_sec=1, ceiling_sec=self.CEILING, limit_sec=self.LIMIT, stall_sec=600)
        site = {"PYTHONPATH": os.path.abspath(SITE_DIR), "SKEIN_TEST_SITE": "1",
                "SKEIN_TEST_DNS_LOG": dns_log}
        with runner_env("http://127.0.0.1:9", site):
            budget = 6 * (self.LIMIT + self.CEILING)
            rc, lines = run_in_process(mod, FakeTime(budget=budget, on_sleep=on_sleep, on_budget=on_budget))
        self.assertIsNone(rc)
        self.assertGreaterEqual(len(snapshot["pids"]), 5, "resolver entered %d times" % len(snapshot["pids"]))
        self.assertEqual(len(set(snapshot["pids"])), len(snapshot["pids"]), "an executor was reused after its limit")
        self.assertFalse(any(snapshot["alive"][:-1]), "an earlier executor outlived its limit: %r" % snapshot)
        self.assertLessEqual(max_alive[0], 2)

    def test_answer_trickling_past_the_limit_is_cut(self):
        # The answer reaches the runner in chunks and every chunk costs a
        # virtual second (the boundary is the pipe read). With a 1s limit the
        # request must end as request_limit — not be accepted once the last
        # chunk arrives — and the next request runs on a fresh executor.
        srv = ScriptedServer(lambda h, qs: h.send_json(200, EMPTY))
        mod = load_runner(poll_sec=1, ceiling_sec=10, limit_sec=1, stall_sec=600)
        ft = FakeTime()
        mod.time = ft
        chunks = [b'{"status":200,', b'"body":"{}"', b'}\n']

        class TricklingPipe:
            def __getattr__(self, name):
                return getattr(os, name)

            def read(self, fd, n):
                if chunks:
                    ft.now += 1.0
                    return chunks.pop(0)
                return os.read(fd, n)

        mod.os = TricklingPipe()
        ex = mod.Executor()
        url = srv.url() + "/agent/v1/notifications/poll"
        try:
            self.assertEqual(ex.request(url, "skm_x"), {"status": 0, "error": "request_limit"})
            self.assertIsNone(ex.proc, "the executor of a request past its limit was kept")
            # The fresh executor needs real start-up time; give the second
            # request an ordinary limit so only freshness and success count.
            mod.os = os
            mod.REQUEST_LIMIT_SEC = 15
            answer = ex.request(url, "skm_x")
            self.assertEqual(answer.get("status"), 200, answer)
            self.assertIsNotNone(ex.proc)
        finally:
            ex.stop()
            srv.stop()

    def _silent_server(self, answer_401_after=None):
        """A TCP server that accepts and never answers (until answer_401_after
        connections have been held, then it answers 401)."""
        lsock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        lsock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        lsock.bind(("127.0.0.1", 0))
        lsock.listen(16)
        stats = {"accepted": 0, "open": 0, "max_open": 0}
        lock = threading.Lock()

        def hold(conn, answer):
            with conn:
                if answer:
                    try:
                        conn.recv(65536)
                        body = b'{"error":"unauthenticated"}'
                        conn.sendall(b"HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\n"
                                     b"Content-Length: %d\r\nConnection: close\r\n\r\n" % len(body) + body)
                    except OSError:
                        pass
                else:
                    try:
                        while conn.recv(65536):
                            pass
                    except OSError:
                        pass
            with lock:
                stats["open"] -= 1

        def accept_loop():
            while True:
                try:
                    conn, _ = lsock.accept()
                except OSError:
                    return
                with lock:
                    stats["accepted"] += 1
                    stats["open"] += 1
                    stats["max_open"] = max(stats["max_open"], stats["open"])
                    answer = answer_401_after is not None and stats["accepted"] > answer_401_after
                threading.Thread(target=hold, args=(conn, answer), daemon=True).start()

        threading.Thread(target=accept_loop, daemon=True).start()
        return lsock, stats

    def test_silent_server_attempts_continue_with_bounded_connections(self):
        lsock, stats = self._silent_server()
        try:
            mod = load_runner(poll_sec=1, ceiling_sec=self.CEILING, limit_sec=self.LIMIT, stall_sec=600)
            with runner_env("http://127.0.0.1:%d" % lsock.getsockname()[1]):
                rc, _ = run_in_process(mod, FakeTime(budget=6 * (self.LIMIT + self.CEILING)))
            self.assertIsNone(rc)
            self.assertGreaterEqual(stats["accepted"], 5)
            self.assertLessEqual(stats["max_open"], 2)
        finally:
            lsock.close()

    def test_401_after_hung_requests_stops_the_runner(self):
        lsock, stats = self._silent_server(answer_401_after=3)
        try:
            mod = load_runner(poll_sec=1, ceiling_sec=self.CEILING, limit_sec=self.LIMIT, stall_sec=600)
            with runner_env("http://127.0.0.1:%d" % lsock.getsockname()[1]):
                rc, lines = run_in_process(mod, FakeTime(budget=3600))
            self.assertEqual(rc, 0, "a 401 after hung requests did not stop the runner: %r" % lines)
            self.assertEqual(lines[-1].get("status"), 401)
        finally:
            lsock.close()


class DegradedRecoveredTest(unittest.TestCase):
    """Row 40: one degraded per failing run longer than the threshold, one
    recovered on the first success after it (even an empty one)."""

    def test_degraded_and_recovered_per_series(self):
        T = 60
        state = {"phase": "long1"}

        def respond(h, qs):
            phase = state["phase"]
            if phase in ("long1", "short", "long2"):
                h.send_json(503, {"error": "down"})
            else:
                h.send_json(200, EMPTY)

        def on_sleep(ft):
            phase = state["phase"]
            if phase == "long1" and ft.elapsed() > 10 * T:
                state["phase"], state["mark"] = "ok1", ft.elapsed()
            elif phase == "ok1":
                state["phase"], state["mark"] = "short", ft.elapsed()
            elif phase == "short" and ft.elapsed() - state["mark"] >= 20:
                state["phase"] = "ok2"
            elif phase == "ok2":
                state["phase"], state["mark"] = "long2", ft.elapsed()
            elif phase == "long2" and ft.elapsed() - state["mark"] > 3 * T:
                raise StopRun()

        srv = ScriptedServer(respond)
        try:
            mod = load_runner(poll_sec=1, ceiling_sec=10, limit_sec=15, stall_sec=T)
            with runner_env(srv.url()):
                rc, lines = run_in_process(mod, FakeTime(on_sleep=on_sleep))
        finally:
            srv.stop()
        self.assertIsNone(rc)
        marks = [l["event"] for l in lines if l.get("event") in ("degraded", "recovered")]
        # long1 → one degraded (even over 10T); ok1 (empty) → one recovered;
        # short (< T) → nothing; long2 → a fresh degraded.
        self.assertEqual(marks, ["degraded", "recovered", "degraded"], lines)


class RejectedCursorTest(unittest.TestCase):
    """Row 41 (US-1 "Rejected cursor", SC-001)."""

    def test_400_on_cursor_restarts_live_with_one_degraded(self):
        def respond(h, qs):
            n = len(h.server.requests)
            if n == 1:
                h.send_json(200, {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:07Z", "id": 7}})
            elif qs.get("since_id") == ["7"]:
                h.send_json(400, {"error": "invalid_cursor"})
            else:
                h.send_json(200, {"notifications": [{"id": 42, "type": "comment.created"}],
                                  "cursor": {"ts": "2026-01-01T00:00:08Z", "id": 42}})

        def on_sleep(ft):
            if len(srv.requests) >= 3:
                raise StopRun()

        srv = ScriptedServer(respond)
        try:
            mod = load_runner(poll_sec=1, ceiling_sec=10, limit_sec=15, stall_sec=600)
            with runner_env(srv.url()):
                rc, lines = run_in_process(mod, FakeTime(on_sleep=on_sleep))
        finally:
            srv.stop()
        self.assertIsNone(rc, "runner exited on a rejected cursor")
        self.assertEqual(srv.requests[1].get("since_id"), ["7"])
        self.assertNotIn("since_ts", srv.requests[2], "the cursor the server rejected was sent again")
        self.assertNotIn("since_id", srv.requests[2])
        self.assertEqual(len(events(lines, "degraded")), 1, lines)
        self.assertEqual([n["notification"]["id"] for n in events(lines, "notification")], [42])



# ---------------------------------------------------------------------------
# Row 42 (US-3, SC-004, SC-005, INV-3, INV-4): the stop lines of the working
# phase, one runner subprocess per case.
# ---------------------------------------------------------------------------

ROOT = "zz-77"


def run_runner_subprocess(server_mode=None, token="skm_" + "c" * 43, token_as_dir=False,
                          no_token_path=False, no_url=False, send_signal=None, site_env=None):
    """Run the rendered runner to completion; returns (rc, stdout lines,
    stderr text, token path, server)."""
    server = None
    if server_mode is not None:
        server = ThreadingHTTPServer(("127.0.0.1", 0), _FakeHandler)
        server.mode = server_mode
        server.requests = []
        threading.Thread(target=server.serve_forever, daemon=True).start()
    tmpdir = make_tmpdir(prefix="skein-runner-stop-")
    runner_path = os.path.join(tmpdir, "runner.py")
    with open(runner_path, "w") as fh:
        fh.write(render_runner(poll_sec=1))
    token_path = os.path.join(tmpdir, "tok")
    if token_as_dir:
        os.mkdir(token_path)
    elif token is not None:
        with open(token_path, "w") as fh:
            fh.write(token)
        os.chmod(token_path, 0o600)
    env = dict(os.environ)
    env.pop("SKEIN_EXCLUDE_SESSION", None)
    env["SKEIN_ISSUE"] = ROOT
    env["SKEIN_PUBLIC_URL"] = "" if no_url else (
        "http://127.0.0.1:%d" % server.server_address[1] if server else "http://127.0.0.1:9")
    env["SKEIN_TOKEN_FILE"] = "" if no_token_path else token_path
    if site_env:
        env.update(site_env)
    proc = subprocess.Popen([sys.executable, runner_path], stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True, env=env)
    try:
        if send_signal is not None:
            first = proc.stdout.readline()
            if '"monitor_started"' not in first:
                raise AssertionError("runner did not start: %r" % first)
            proc.send_signal(send_signal)
            out, err = proc.communicate(timeout=30)
            out = first + out
        else:
            out, err = proc.communicate(timeout=30)
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
        if server is not None:
            server.shutdown()
            server.server_close()
    lines = [json.loads(line) for line in out.splitlines() if line.strip()]
    return proc.returncode, lines, err, token_path


class StopLineAssertions:
    """The machine contract every stop line shares (US-3, INV-3)."""

    def assert_single_stop_line(self, lines, reason, event, status=None):
        with_reason = [l for l in lines if "reason" in l]
        self.assertEqual(len(with_reason), 1, "want exactly one line with a reason: %r" % lines)
        last = lines[-1]
        self.assertIs(with_reason[0], last, "the stop line must be the last stdout line: %r" % lines)
        self.assertEqual(last.get("reason"), reason)
        self.assertEqual(last.get("event"), event)
        if status is None:
            self.assertNotIn("status", last)
        else:
            self.assertEqual(last.get("status"), status)
        self.assertTrue(last.get("action"), "empty action")
        self.assertIn(ROOT, last["action"], "action must name the root issue")


class StopLineTest(StopLineAssertions, unittest.TestCase):

    def test_token_expired_on_401(self):
        rc, lines, err, tok = run_runner_subprocess("401")
        self.assert_single_stop_line(lines, "token_expired", "closed", 401)
        self.assertEqual(rc, 0)
        self.assertFalse(os.path.exists(tok))

    def test_visibility_lost_on_403(self):
        rc, lines, err, tok = run_runner_subprocess("403")
        self.assert_single_stop_line(lines, "visibility_lost", "closed", 403)
        self.assertEqual(rc, 0)
        self.assertFalse(os.path.exists(tok))

    def test_token_file_missing(self):
        with self.subTest(case="no file"):
            rc, lines, err, tok = run_runner_subprocess(token=None)
            self.assert_single_stop_line(lines, "token_file_missing", "closed")
            self.assertEqual(rc, 1)
            self.assertFalse(os.path.exists(tok))
        with self.subTest(case="empty file"):
            rc, lines, err, tok = run_runner_subprocess(token="  \n")
            self.assert_single_stop_line(lines, "token_file_missing", "closed")
            self.assertEqual(rc, 1)
            self.assertFalse(os.path.exists(tok), "an empty token file must be deleted")
        with self.subTest(case="unreadable (a directory)"):
            rc, lines, err, tok = run_runner_subprocess(token_as_dir=True)
            self.assert_single_stop_line(lines, "token_file_missing", "closed")
            self.assertEqual(rc, 1)
            self.assertTrue(os.path.isdir(tok), "an unreadable path must not be touched")

    def test_misconfigured(self):
        with self.subTest(case="no server address"):
            rc, lines, err, tok = run_runner_subprocess(no_url=True)
            self.assert_single_stop_line(lines, "misconfigured", "closed")
            self.assertEqual(rc, 1)
            self.assertFalse(os.path.exists(tok), "the token file must be deleted when its path is known")
        with self.subTest(case="no token file path"):
            rc, lines, err, tok = run_runner_subprocess("empty", no_token_path=True)
            self.assert_single_stop_line(lines, "misconfigured", "closed")
            self.assertEqual(rc, 1)

    def test_stopped_by_signal_keeps_the_token(self):
        for sig in (signal.SIGTERM, signal.SIGINT):
            with self.subTest(signal=sig.name):
                rc, lines, err, tok = run_runner_subprocess("empty", send_signal=sig)
                self.assert_single_stop_line(lines, "stopped", "paused")
                self.assertEqual(rc, 0)
                self.assertTrue(os.path.exists(tok), "the token file must survive a signal")
                self.assertNotIn("Traceback", err)

    def test_signal_right_after_the_handler_is_installed(self):
        rc, lines, err, tok = run_runner_subprocess("empty", site_env=site_with(
            SKEIN_TEST_SIGNAL_AFTER_HANDLER="_on_signal"))
        self.assert_single_stop_line(lines, "stopped", "paused")
        self.assertEqual(rc, 0, err)
        self.assertTrue(os.path.exists(tok))
        self.assertNotIn("Traceback", err)

    def test_internal_error_keeps_the_token_and_traces_to_stderr(self):
        site = {"PYTHONPATH": os.path.abspath(SITE_DIR), "SKEIN_TEST_SITE": "1",
                "SKEIN_TEST_RANDOM": "raise"}
        rc, lines, err, tok = run_runner_subprocess("empty", site_env=site)
        self.assert_single_stop_line(lines, "internal_error", "paused")
        self.assertEqual(rc, 1)
        self.assertTrue(os.path.exists(tok))
        self.assertIn("Traceback", err)
        self.assertIn("RuntimeError", err)


# ---------------------------------------------------------------------------
# Rows 49 and 50 (US-3, SC-004, SC-005, INV-3, INV-4): the bootstrap loader
# asset run as `python3 -c`, alone (load phase) and with the real runner
# after exec (handover of the stop protocol and of the token file).
# ---------------------------------------------------------------------------


class _LoaderHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        srv = self.server
        if parsed.path == "/agent/v1/monitor-script":
            srv.script_hits += 1
            srv.script(self)
        else:
            srv.poll(self)

    def send_body(self, status, body, declared_len=None):
        self.send_response(status)
        self.send_header("Content-Type", "text/x-python")
        self.send_header("Content-Length", str(declared_len if declared_len is not None else len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass


class LoaderServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, script, poll=None):
        super().__init__(("127.0.0.1", 0), _LoaderHandler)
        self.script = script
        self.poll = poll or (lambda h: h.send_body(200, json.dumps(EMPTY).encode()))
        self.script_hits = 0
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def url(self):
        return "http://127.0.0.1:%d" % self.server_address[1]

    def handle_error(self, request, client_address):
        pass

    def stop(self):
        self.shutdown()
        self.server_close()


def closed_port_url():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return "http://127.0.0.1:%d" % port


def run_loader(url, token="skm_" + "d" * 43, token_as_dir=False, no_url=False,
               site_env=None, signal_when=None, sig=None, extra_env=None):
    """Run the loader asset as `python3 -c`; returns (rc, lines, stderr, token path).
    signal_when: a threading.Event set once the process may be signalled."""
    with open(LOADER_SRC) as fh:
        loader = fh.read()
    tmpdir = make_tmpdir(prefix="skein-loader-")
    token_path = os.path.join(tmpdir, "tok")
    if token_as_dir:
        os.mkdir(token_path)
    elif token is not None:
        with open(token_path, "w") as fh:
            fh.write(token)
        os.chmod(token_path, 0o600)
    env = dict(os.environ)
    env.pop("SKEIN_EXCLUDE_SESSION", None)
    env["SKEIN_ISSUE"] = ROOT
    env["SKEIN_PUBLIC_URL"] = "" if no_url else url
    env["SKEIN_TOKEN_FILE"] = token_path
    if site_env:
        env.update(site_env)
    if extra_env:
        env.update(extra_env)
    proc = subprocess.Popen([sys.executable, "-c", loader], stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True, env=env)
    try:
        if signal_when is not None:
            if not signal_when.wait(timeout=20):
                raise AssertionError("loader never reached the point to signal")
            proc.send_signal(sig)
        out, err = proc.communicate(timeout=30)
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
    lines = [json.loads(line) for line in out.splitlines() if line.strip()]
    return proc.returncode, lines, err, token_path


def site_with(**hooks):
    env = {"PYTHONPATH": os.path.abspath(SITE_DIR), "SKEIN_TEST_SITE": "1"}
    env.update(hooks)
    return env


class LoaderStopTest(StopLineAssertions, unittest.TestCase):
    """Row 49: every load-phase stop of the loader."""

    def test_signal_at_the_import_boundary_is_stopped(self):
        # A real SIGTERM while the loader imports http.client — after its
        # handler exists — is one "stopped" line, exit 0, file kept.
        rc, lines, err, tok = run_loader(closed_port_url(),
                                         site_env=site_with(SKEIN_TEST_SIGNAL_ON_IMPORT="http.client"))
        self.assert_single_stop_line(lines, "stopped", "paused")
        self.assertEqual(rc, 0, err)
        self.assertTrue(os.path.exists(tok))
        self.assertNotIn("Traceback", err)

    def test_signal_right_after_the_handler_is_installed_is_stopped(self):
        rc, lines, err, tok = run_loader(closed_port_url(), site_env=site_with(
            SKEIN_TEST_SIGNAL_AFTER_HANDLER="_skein_loader_on_signal"))
        self.assert_single_stop_line(lines, "stopped", "paused")
        self.assertEqual(rc, 0, err)
        self.assertTrue(os.path.exists(tok))
        self.assertNotIn("Traceback", err)

    def test_token_file_missing(self):
        for case, kwargs, exists in (("no file", {"token": None}, False),
                                     ("empty file", {"token": " \n"}, False),
                                     ("unreadable", {"token_as_dir": True}, True)):
            with self.subTest(case=case):
                rc, lines, err, tok = run_loader(closed_port_url(), **kwargs)
                self.assert_single_stop_line(lines, "token_file_missing", "closed")
                self.assertEqual(rc, 1)
                self.assertEqual(os.path.exists(tok), exists)

    def test_misconfigured(self):
        rc, lines, err, tok = run_loader("", no_url=True)
        self.assert_single_stop_line(lines, "misconfigured", "closed")
        self.assertEqual(rc, 1)
        self.assertFalse(os.path.exists(tok))

    def test_token_expired_on_401(self):
        srv = LoaderServer(lambda h: h.send_body(401, b'{"error":"unauthenticated"}'))
        try:
            rc, lines, err, tok = run_loader(srv.url())
        finally:
            srv.stop()
        self.assert_single_stop_line(lines, "token_expired", "closed", 401)
        self.assertEqual(rc, 0)
        self.assertFalse(os.path.exists(tok))

    def test_server_unreachable(self):
        marker_dir = make_tmpdir(prefix="skein-loader-mark-")
        marker = os.path.join(marker_dir, "executed")
        body = ('import os\nopen(%r, "w").write("x")\n' % marker).encode()
        cases = {
            "connection refused": (None, {}),
            "500": (lambda h: h.send_body(500, b"boom"), {}),
            "429": (lambda h: h.send_body(429, b"slow"), {}),
            "403": (lambda h: h.send_body(403, b"forbidden by proxy"), {}),
            "404": (lambda h: h.send_body(404, b"nope"), {}),
            "timeout": (lambda h: h.send_body(200, body),
                        {"PYTHONPATH": os.path.abspath(SITE_DIR), "SKEIN_TEST_SITE": "1",
                         "SKEIN_TEST_URLOPEN_TIMEOUT": "1"}),
            "body cut after 200 headers": (lambda h: h.send_body(200, body, declared_len=len(body) + 500), {}),
        }
        for case, (script, site) in cases.items():
            with self.subTest(case=case):
                srv = LoaderServer(script) if script else None
                try:
                    rc, lines, err, tok = run_loader(srv.url() if srv else closed_port_url(), site_env=site)
                finally:
                    if srv:
                        srv.stop()
                self.assert_single_stop_line(lines, "server_unreachable", "paused")
                self.assertEqual(rc, 1)
                self.assertTrue(os.path.exists(tok))
                self.assertFalse(os.path.exists(marker), "a body that was not fully received was executed")
                self.assertNotIn("Traceback", "".join(json.dumps(l) for l in lines))

    def test_stopped_by_signal_while_downloading(self):
        for sig in (signal.SIGTERM, signal.SIGINT):
            with self.subTest(signal=sig.name):
                arrived = threading.Event()
                release = threading.Event()

                def hold(h):
                    arrived.set()
                    release.wait(timeout=30)
                    h.send_body(200, b"pass\n")

                srv = LoaderServer(hold)
                try:
                    rc, lines, err, tok = run_loader(srv.url(), signal_when=arrived, sig=sig)
                finally:
                    release.set()
                    srv.stop()
                self.assert_single_stop_line(lines, "stopped", "paused")
                self.assertEqual(rc, 0)
                self.assertTrue(os.path.exists(tok))

    def test_internal_error_in_the_body(self):
        srv = LoaderServer(lambda h: h.send_body(200, b'raise RuntimeError("body is broken")\n'))
        try:
            rc, lines, err, tok = run_loader(srv.url())
        finally:
            srv.stop()
        self.assert_single_stop_line(lines, "internal_error", "paused")
        self.assertEqual(rc, 1)
        self.assertTrue(os.path.exists(tok))
        self.assertIn("Traceback", err)
        self.assertIn("RuntimeError", err)


class LoaderRunnerHandoverTest(StopLineAssertions, unittest.TestCase):
    """Row 50: after exec the runner owns the stop protocol — one stop line,
    the token file's fate by the runner's row of the table."""

    def _serve_runner(self, poll=None):
        body = render_runner(poll_sec=1).encode()
        return LoaderServer(lambda h: h.send_body(200, body), poll)

    def test_signal_in_the_working_phase(self):
        for sig in (signal.SIGTERM, signal.SIGINT):
            with self.subTest(signal=sig.name):
                polled = threading.Event()

                def poll(h):
                    polled.set()
                    h.send_body(200, json.dumps(EMPTY).encode())

                srv = self._serve_runner(poll)
                try:
                    rc, lines, err, tok = run_loader(srv.url(), signal_when=polled, sig=sig)
                finally:
                    srv.stop()
                self.assert_single_stop_line(lines, "stopped", "paused")
                self.assertEqual(lines[0].get("event"), "monitor_started")
                self.assertEqual(rc, 0)
                self.assertTrue(os.path.exists(tok))

    def test_signal_during_the_runner_start_up(self):
        # Before the runner installs its handlers (its module imports) the
        # loader's handler answers; right after it installs them, the
        # runner's own start-up does. Either way: one "stopped", exit 0.
        for case, hooks in (("runner module imports", {"SKEIN_TEST_SIGNAL_ON_IMPORT": "subprocess"}),
                            ("runner handler just installed", {"SKEIN_TEST_SIGNAL_AFTER_HANDLER": "_on_signal"})):
            with self.subTest(case=case):
                srv = self._serve_runner()
                try:
                    rc, lines, err, tok = run_loader(srv.url(), site_env=site_with(**hooks))
                finally:
                    srv.stop()
                self.assert_single_stop_line(lines, "stopped", "paused")
                self.assertEqual(rc, 0, err)
                self.assertTrue(os.path.exists(tok))
                self.assertNotIn("Traceback", err)

    def test_internal_error_of_the_runner(self):
        srv = self._serve_runner()
        try:
            rc, lines, err, tok = run_loader(srv.url(), site_env={
                "PYTHONPATH": os.path.abspath(SITE_DIR), "SKEIN_TEST_SITE": "1", "SKEIN_TEST_RANDOM": "raise"})
        finally:
            srv.stop()
        self.assert_single_stop_line(lines, "internal_error", "paused")
        self.assertEqual(rc, 1)
        self.assertTrue(os.path.exists(tok))

    def test_401_in_the_working_phase(self):
        srv = self._serve_runner(lambda h: h.send_body(401, b'{"error":"unauthenticated"}'))
        try:
            rc, lines, err, tok = run_loader(srv.url())
        finally:
            srv.stop()
        self.assert_single_stop_line(lines, "token_expired", "closed", 401)
        self.assertEqual(rc, 0)
        self.assertFalse(os.path.exists(tok))


# ---------------------------------------------------------------------------
# Rows 43–47 (US-4, INV-12 runner side, INV-13, INV-4): one runner per token
# file, the root-closed exit, restart of the same environment.
# ---------------------------------------------------------------------------


class PushServer(ThreadingHTTPServer):
    """A poll server that answers empty pages until the test pushes
    notifications; each pushed notification is answered once per poll queue."""

    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), _ScriptedHandler)
        self.requests = []
        self.lock = threading.Lock()
        self.pending = []
        self.next_id = 100
        self.respond = self._respond
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def _respond(self, h, qs):
        with self.lock:
            notes, self.pending = self.pending, []
        h.send_json(200, {"notifications": notes,
                          "cursor": {"ts": "2026-01-01T00:00:00Z", "id": self.next_id}})

    def push(self):
        with self.lock:
            self.next_id += 1
            self.pending.append({"id": self.next_id, "type": "comment.created", "issueId": ROOT})
            return self.next_id

    def url(self):
        return "http://127.0.0.1:%d" % self.server_address[1]

    def handle_error(self, request, client_address):
        pass

    def stop(self):
        self.shutdown()
        self.server_close()


class LiveRunner:
    """A runner subprocess on a given token path, its stdout streamed."""

    def __init__(self, url, token_path, site_env=None):
        tmpdir = make_tmpdir(prefix="skein-live-")
        self.runner_path = os.path.join(tmpdir, "runner.py")
        with open(self.runner_path, "w") as fh:
            fh.write(render_runner(poll_sec=1))
        env = dict(os.environ)
        env.pop("SKEIN_EXCLUDE_SESSION", None)
        env.update({"SKEIN_PUBLIC_URL": url, "SKEIN_TOKEN_FILE": token_path, "SKEIN_ISSUE": ROOT})
        if site_env:
            env.update(site_env)
        self.proc = subprocess.Popen([sys.executable, self.runner_path], stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, text=True, env=env)
        self.lines = []
        self.queue = queue.Queue()
        self.err = []
        threading.Thread(target=self._pump, daemon=True).start()
        threading.Thread(target=self._pump_err, daemon=True).start()

    def _pump(self):
        for line in self.proc.stdout:
            obj = json.loads(line)
            self.lines.append(obj)
            self.queue.put(obj)
        self.queue.put(None)

    def _pump_err(self):
        for line in self.proc.stderr:
            self.err.append(line.rstrip("\n"))

    def wait_for(self, pred, timeout=20):
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AssertionError("timed out; lines so far: %r" % self.lines)
            obj = self.queue.get(timeout=remaining)
            if obj is None:
                raise AssertionError("runner exited; lines: %r" % self.lines)
            if pred(obj):
                return obj

    def wait_exit(self, timeout=20):
        rc = self.proc.wait(timeout=timeout)
        _real_sleep(0.1)  # let the pumps drain
        return rc

    def kill(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait()


def is_event(name):
    return lambda o: o.get("event") == name


def notification_id(nid):
    return lambda o: o.get("event") == "notification" and o["notification"].get("id") == nid


def fresh_token(dirpath, name="tok"):
    path = os.path.join(dirpath, name)
    with open(path, "w") as fh:
        fh.write("skm_" + "e" * 43)
    os.chmod(path, 0o600)
    return path


class OneRunnerTest(StopLineAssertions, unittest.TestCase):
    """Row 43 (INV-13, R9, US-4 "one runner", SC-006) — POSIX."""

    def test_second_runner_on_the_same_file_stops_as_duplicate(self):
        srv = PushServer()
        tmpdir = make_tmpdir(prefix="skein-lock-")
        token = fresh_token(tmpdir)
        link = os.path.join(tmpdir, "symlink")
        os.symlink(token, link)
        hard = os.path.join(tmpdir, "hardlink")
        os.link(token, hard)
        first = LiveRunner(srv.url(), token)
        runners = [first]
        try:
            first.wait_for(is_event("monitor_started"))
            before = (open(token).read(), os.stat(token).st_mtime_ns)
            for path in (token, link, hard):
                with self.subTest(path=os.path.basename(path)):
                    second = LiveRunner(srv.url(), path)
                    runners.append(second)
                    rc = second.wait_exit()
                    self.assertEqual(rc, 0)
                    self.assertEqual(len(second.lines), 1, second.lines)
                    self.assert_single_stop_line(second.lines, "already_running", "duplicate")
                    self.assertNotIn("Traceback", "\n".join(second.err))
                    self.assertEqual((open(token).read(), os.stat(token).st_mtime_ns), before,
                                     "the second runner touched the token file")
            nid = srv.push()
            first.wait_for(notification_id(nid))
            self.assertIsNone(first.proc.poll(), "the first runner stopped")

            other = LiveRunner(srv.url(), fresh_token(tmpdir, "other"))
            runners.append(other)
            other.wait_for(is_event("monitor_started"))
            nid = srv.push()
            other.wait_for(notification_id(nid))
        finally:
            for r in runners:
                r.kill()
            srv.stop()


class NoFileLocksTest(unittest.TestCase):
    """Row 44 (INV-13 on a platform without fcntl, maintainers' decision)."""

    def test_runs_without_the_lock_and_says_so_once(self):
        srv = PushServer()
        tmpdir = make_tmpdir(prefix="skein-nolock-")
        token = fresh_token(tmpdir)
        site = {"PYTHONPATH": os.path.abspath(SITE_DIR), "SKEIN_TEST_SITE": "1", "SKEIN_TEST_NO_FCNTL": "1"}
        first = LiveRunner(srv.url(), token, site)
        second = None
        try:
            first.wait_for(is_event("monitor_started"))
            nid = srv.push()
            first.wait_for(notification_id(nid))
            second = LiveRunner(srv.url(), token, site)
            second.wait_for(is_event("monitor_started"))
            nid = srv.push()
            second.wait_for(notification_id(nid))
            self.assertIsNone(first.proc.poll())
            self.assertFalse(events(first.lines, "paused"), first.lines)
            self.assertEqual(len(first.err), 1, "want exactly one diagnostic line: %r" % first.err)
        finally:
            first.kill()
            if second:
                second.kill()
            srv.stop()


class LockFailureTest(unittest.TestCase):
    """A flock failure other than «held by another runner» (ENOLCK, EINVAL on a
    filesystem without flock) is not a reason to stop: one diagnostic line on
    stderr and the runner works unlocked (plan, "Platform without fcntl")."""

    def test_flock_error_runs_unlocked(self):
        import errno
        srv = PushServer()
        token = fresh_token(make_tmpdir(prefix="skein-flockerr-"))
        runner = LiveRunner(srv.url(), token, site_with(SKEIN_TEST_FLOCK_ERRNO=str(errno.ENOLCK)))
        try:
            runner.wait_for(is_event("monitor_started"))
            nid = srv.push()
            runner.wait_for(notification_id(nid))
            self.assertIsNone(runner.proc.poll())
            self.assertFalse([l for l in runner.lines if "reason" in l], runner.lines)
            self.assertEqual(len(runner.err), 1, "want exactly one diagnostic line: %r" % runner.err)
        finally:
            runner.kill()
            srv.stop()


class RootClosedTest(StopLineAssertions, unittest.TestCase):
    """Row 45 (US-4 "root closure", INV-12 runner side, SC-007(g))."""

    def _run(self, respond, stop_after=6):
        def on_sleep(ft):
            if len(srv.requests) >= stop_after:
                raise StopRun()

        srv = ScriptedServer(respond)
        try:
            mod = load_runner(poll_sec=1, ceiling_sec=10, limit_sec=15, stall_sec=600)
            with runner_env(srv.url()) as token_path:
                rc, lines = run_in_process(mod, FakeTime(on_sleep=on_sleep))
                exists = os.path.exists(token_path)
        finally:
            srv.stop()
        return rc, lines, exists

    def test_page_then_root_closed(self):
        def respond(h, qs):
            h.send_json(200, {"notifications": [{"id": 1, "type": "comment.created"},
                                                {"id": 2, "type": "issue.closed"}],
                              "cursor": {"ts": "2026-01-01T00:00:01Z", "id": 2}, "rootClosed": True})

        rc, lines, exists = self._run(respond)
        self.assertEqual([l["notification"]["id"] for l in events(lines, "notification")], [1, 2])
        self.assert_single_stop_line(lines, "root_closed", "closed")
        self.assertEqual(rc, 0)
        self.assertFalse(exists, "token file kept after root_closed")

    def test_false_or_absent_flag_keeps_working(self):
        for flag in (False, None):
            with self.subTest(rootClosed=flag):
                def respond(h, qs, flag=flag):
                    page = {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:01Z", "id": 1}}
                    if flag is not None:
                        page["rootClosed"] = flag
                    h.send_json(200, page)

                rc, lines, exists = self._run(respond)
                self.assertIsNone(rc, "the runner stopped without rootClosed: %r" % lines)
                self.assertTrue(exists)

    def test_rejected_cursor_then_root_closed_keeps_the_order(self):
        def respond(h, qs):
            n = len(h.server.requests)
            if n == 1:
                h.send_json(200, {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:01Z", "id": 3}})
            elif "since_id" in qs:
                h.send_json(400, {"error": "invalid_cursor"})
            else:
                h.send_json(200, {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:02Z", "id": 4},
                                  "rootClosed": True})

        rc, lines, exists = self._run(respond)
        self.assert_single_stop_line(lines, "root_closed", "closed")
        degraded = [i for i, l in enumerate(lines) if l.get("event") == "degraded"]
        self.assertEqual(len(degraded), 1, lines)
        self.assertEqual(lines[degraded[0]].get("cause"), "cursor_reset")
        self.assertLess(degraded[0], len(lines) - 1, "degraded must come before root_closed")
        self.assertEqual(rc, 0)
        self.assertFalse(exists)


class RestartTest(unittest.TestCase):
    """Row 46 (US-4 "restart", SC-006, INV-4) and row 47 (token file
    deleted from outside)."""

    def test_restart_after_sigterm_and_after_sigkill(self):
        for how in ("SIGTERM", "SIGKILL"):
            with self.subTest(stop=how):
                srv = PushServer()
                token = fresh_token(make_tmpdir(prefix="skein-restart-"))
                first = LiveRunner(srv.url(), token)
                second = None
                try:
                    first.wait_for(is_event("monitor_started"))
                    deadline = time.monotonic() + 10
                    while len(srv.requests) < 2 and time.monotonic() < deadline:
                        _real_sleep(0.05)
                    first.proc.send_signal(getattr(signal, how))
                    first.wait_exit()
                    self.assertTrue(os.path.exists(token), "token file gone after %s" % how)
                    seen = len(srv.requests)
                    second = LiveRunner(srv.url(), token)
                    second.wait_for(is_event("monitor_started"))
                    nid = srv.push()
                    second.wait_for(notification_id(nid))
                    restart_first = srv.requests[seen]
                    self.assertNotIn("since_ts", restart_first, "the restart continued the old cursor")
                    self.assertNotIn("since_id", restart_first)
                finally:
                    first.kill()
                    if second:
                        second.kill()
                    srv.stop()

    def test_token_file_deleted_while_running(self):
        srv = PushServer()
        token = fresh_token(make_tmpdir(prefix="skein-gone-"))
        runner = LiveRunner(srv.url(), token)
        try:
            runner.wait_for(is_event("monitor_started"))
            os.remove(token)
            nid = srv.push()
            runner.wait_for(notification_id(nid))
            self.assertFalse([l for l in runner.lines if "reason" in l], runner.lines)
            self.assertIsNone(runner.proc.poll())
        finally:
            runner.kill()
            srv.stop()

if __name__ == "__main__":
    unittest.main(verbosity=2)
