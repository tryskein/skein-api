#!/usr/bin/env python3
"""Tests of the persistent poll cursor (runner/state.py, runner/core.py), the
fate of the state file next to the token (runner and the inline loader
monitor_loader.py) and the loader mode of the inline loader.

  - A restart with the same token file resumes from the saved cursor; a new
    token starts live; a rejected cursor is reset as before; a tail comes no
    faster than one page per poll interval.
  - Every path that deletes the token file deletes state.json with it and
    leaves no skein-mon-* directory; the paths that keep the token keep the
    cursor.
  - The inline loader runs the body in "legacy" mode whatever the agent's
    environment holds.

Run on its own: python3 internal/handler/assets/runner_tests/state_test.py
"""

import hashlib
import io
import json
import os
import queue
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler
from urllib.parse import parse_qs, urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

TOKEN = "skm_" + "s" * 43
# One poll per second at most, also when the run is quiet, so a pushed event
# is picked up within about a second.
RUNNER_BODY = harness.render_runner(poll_sec=1, ceiling_sec=1).encode("utf-8")
# Events per page of the fake server.
PAGE_SIZE = 2
_real_sleep = time.sleep


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        srv = self.server
        if parsed.path == "/agent/v1/monitor-script":
            status = srv.script_status()
            self._send(status, RUNNER_BODY if status == 200 else b'{"error":"unauthenticated"}',
                       "text/x-python")
            return
        qs = parse_qs(parsed.query)
        with srv.lock:
            srv.polls.append(qs)
            status = srv.poll_status(qs)
            if status != 200:
                body = {"error": "rejected"}
                if status == 400:
                    body = {"error": "invalid_cursor"}
            elif "since_id" not in qs:
                # Live-start: no backlog, the cursor anchored at the newest event.
                body = {"notifications": [], "cursor": srv.cursor_at(srv.newest())}
            else:
                since = int(qs["since_id"][0])
                page = [e for e in srv.events if e["id"] > since][:PAGE_SIZE]
                body = {"notifications": page,
                        "cursor": srv.cursor_at(page[-1]["id"] if page else since)}
                if srv.root_closed:
                    body["rootClosed"] = True
        self._send(status, json.dumps(body).encode("utf-8"), "application/json")

    def _send(self, status, body, ctype):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass


class EventServer(harness.ThreadingHTTPServer):
    """A Skein server for the monitor: serves the runner body and answers
    polls from its event list by the cursor, like the real poll door: no
    cursor → live-start at the newest event; a cursor → the next events
    after it, PAGE_SIZE at most."""

    daemon_threads = True

    def __init__(self):
        harness.ThreadingHTTPServer.__init__(self, ("127.0.0.1", 0), _Handler)
        self.lock = threading.Lock()
        self.events = []
        self.polls = []
        self.root_closed = False
        self.script_status = lambda: 200
        self.poll_status = lambda qs: 200
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def newest(self):
        return self.events[-1]["id"] if self.events else 0

    @staticmethod
    def cursor_at(event_id):
        return {"ts": "2026-01-01T00:00:%02dZ" % (event_id % 60), "id": event_id}

    def wait_polls(self, count, timeout=20):
        """Wait until the server has answered count polls: a monitor's first
        poll is live-start and anchors past every event pushed before it."""
        deadline = time.monotonic() + timeout
        while len(self.polls) < count:
            if time.monotonic() > deadline:
                raise AssertionError("only %d polls, want %d" % (len(self.polls), count))
            _real_sleep(0.05)

    def push(self):
        with self.lock:
            nid = self.newest() + 1
            self.events.append({"id": nid, "type": "comment.created", "issueId": harness.ROOT})
            return nid

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


class TokenDir(object):
    """A skein-mon-* token directory (0700) of the mint, with its token file
    and optionally a saved state."""

    def __init__(self, test, token=TOKEN, token_mode=0o600, dir_mode=0o700, cursor_id=None):
        root = tempfile.mkdtemp(prefix="skein-state-")
        test.addCleanup(shutil.rmtree, root, True)
        self.dir = os.path.join(root, "skein-mon-zz-77-0123456789abcdef")
        os.mkdir(self.dir)
        os.chmod(self.dir, dir_mode)
        self.token = os.path.join(self.dir, "token")
        with open(self.token, "w") as fh:
            fh.write(token)
        os.chmod(self.token, token_mode)
        self.state = os.path.join(self.dir, "state.json")
        if cursor_id is not None:
            self.write_state(cursor_id)

    def write_state(self, cursor_id):
        with open(self.state, "w") as fh:
            json.dump({"version": 1, "cursor": EventServer.cursor_at(cursor_id)}, fh)
        os.chmod(self.state, 0o600)

    def saved_cursor_id(self):
        try:
            with open(self.state) as fh:
                cursor = json.load(fh).get("cursor")
        except (OSError, ValueError):
            return None
        return cursor.get("id") if isinstance(cursor, dict) else None

    def wait_saved(self, cursor_id, timeout=20):
        deadline = time.monotonic() + timeout
        while self.saved_cursor_id() != cursor_id:
            if time.monotonic() > deadline:
                raise AssertionError("cursor %r never saved (have %r)" % (cursor_id, self.saved_cursor_id()))
            _real_sleep(0.05)


def base_env(url, token_path, extra=None):
    env = dict(os.environ)
    env.pop("SKEIN_EXCLUDE_SESSION", None)
    env.update({"SKEIN_ISSUE": harness.ROOT, "SKEIN_PUBLIC_URL": url, "SKEIN_TOKEN_FILE": token_path,
                "SKEIN_RUNNER_SHA256": hashlib.sha256(RUNNER_BODY).hexdigest()})
    env.update(extra or {})
    return env


def site_env(**hooks):
    env = {"PYTHONPATH": os.path.abspath(harness.SITE_DIR), "SKEIN_TEST_SITE": "1"}
    env.update(hooks)
    return env


class Monitor(object):
    """The inline loader (or the runner itself, runner=True) as a process,
    its stdout streamed."""

    def __init__(self, test, url, token_path, extra_env=None, runner=False):
        if runner:
            path = os.path.join(tempfile.mkdtemp(prefix="skein-state-run-"), "runner.py")
            test.addCleanup(shutil.rmtree, os.path.dirname(path), True)
            with open(path, "wb") as fh:
                fh.write(RUNNER_BODY)
            argv = [sys.executable, path]
        else:
            with open(harness.LOADER_SRC) as fh:
                argv = [sys.executable, "-c", fh.read()]
        self.proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                     universal_newlines=True,
                                     env=base_env(url, token_path, extra_env))
        test.addCleanup(self.kill)
        self.lines = []
        self.err = []
        self.queue = queue.Queue()
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
                raise AssertionError("monitor exited; lines: %r" % self.lines)
            if pred(obj):
                return obj

    def wait_exit(self, timeout=30):
        rc = self.proc.wait(timeout=timeout)
        _real_sleep(0.1)  # let the pumps drain
        return rc

    def stop(self):
        self.proc.send_signal(signal.SIGTERM)
        return self.wait_exit()

    def kill(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait()

    def notification_ids(self):
        return [l["notification"]["id"] for l in self.lines if l.get("event") == "notification"]

    def stop_line(self):
        stops = [l for l in self.lines if "reason" in l]
        return stops[-1] if stops else None


def is_event(name):
    return lambda o: o.get("event") == name


def notification_id(nid):
    return lambda o: o.get("event") == "notification" and o["notification"].get("id") == nid


class CursorTest(unittest.TestCase):
    """Matrix row #24: the cursor survives a restart with the same token."""

    def setUp(self):
        self.srv = EventServer()
        self.addCleanup(self.srv.stop)

    def test_restart_with_the_same_token_resumes_from_the_saved_cursor(self):
        tok = TokenDir(self)
        first = Monitor(self, self.srv.url(), tok.token)
        first.wait_for(is_event("monitor_started"))
        self.srv.wait_polls(1)
        one = self.srv.push()
        first.wait_for(notification_id(one))
        tok.wait_saved(one)
        self.assertEqual(first.stop(), 0)
        self.assertEqual(first.stop_line()["reason"], "stopped")
        # The first poll of the new token was live-start.
        self.assertNotIn("since_id", self.srv.polls[0])
        self.assertNotIn("since_ts", self.srv.polls[0])

        missed = self.srv.push()
        polls_before = len(self.srv.polls)
        second = Monitor(self, self.srv.url(), tok.token)
        second.wait_for(notification_id(missed))
        resumed = self.srv.polls[polls_before]
        self.assertEqual(resumed.get("since_id"), [str(one)])
        self.assertEqual(resumed.get("since_ts"), [EventServer.cursor_at(one)["ts"]])
        # Nothing repeated, nothing lost.
        self.assertEqual(second.notification_ids(), [missed])

    def test_rejected_cursor_degrades_and_restarts_live(self):
        tok = TokenDir(self, cursor_id=5)
        self.srv.poll_status = lambda qs: 400 if "since_id" in qs else 200
        mon = Monitor(self, self.srv.url(), tok.token)
        line = mon.wait_for(is_event("degraded"))
        self.assertEqual(line.get("cause"), "cursor_reset")
        deadline = time.monotonic() + 20
        while len(self.srv.polls) < 2 and time.monotonic() < deadline:
            _real_sleep(0.05)
        self.assertEqual(self.srv.polls[0].get("since_id"), ["5"])
        self.assertNotIn("since_id", self.srv.polls[1])
        self.assertIsNone(mon.stop_line())


class _VirtualTime(object):
    """time for an in-process runner: sleeps of 0.1 s and more are recorded
    and advance the clock; shorter ones (the executor wait step) pause a
    little for real so the request in flight can finish."""

    def __init__(self, stop_after_sleeps):
        self.now = 1000.0
        self.sleeps = []
        self.stop_after = stop_after_sleeps

    def monotonic(self):
        return self.now

    def time(self):
        return self.now

    def sleep(self, seconds):
        seconds = float(seconds)
        if seconds < 0.1:
            _real_sleep(0.002)
        else:
            self.sleeps.append(seconds)
            if len(self.sleeps) >= self.stop_after:
                raise _EndRun()
        self.now += seconds


class _EndRun(BaseException):
    pass


class _LowRandom(object):
    """The lowest jitter: every random wait is half its base."""

    def random(self):
        return 0.0


class TailPaceTest(unittest.TestCase):
    """Matrix row #24: the tail waiting after a restart comes at most one
    page per poll interval (virtual clock, lowest jitter)."""

    POLL_SEC = 4

    def test_tail_after_restart_is_one_page_per_interval(self):
        srv = EventServer()
        self.addCleanup(srv.stop)
        for _ in range(4 * PAGE_SIZE):
            srv.push()
        tok = TokenDir(self, cursor_id=0)
        mod = harness.load_runner(poll_sec=self.POLL_SEC, ceiling_sec=60)
        clock = _VirtualTime(stop_after_sleeps=4)
        mod.time = clock
        mod.random = _LowRandom()
        saved = {k: os.environ.get(k) for k in ("SKEIN_PUBLIC_URL", "SKEIN_TOKEN_FILE", "SKEIN_ISSUE",
                                                 "SKEIN_EXCLUDE_SESSION")}
        self.addCleanup(_restore_env, saved)
        os.environ.update({"SKEIN_PUBLIC_URL": srv.url(), "SKEIN_TOKEN_FILE": tok.token,
                           "SKEIN_ISSUE": harness.ROOT})
        os.environ.pop("SKEIN_EXCLUDE_SESSION", None)
        devnull = open(os.devnull, "w")
        self.addCleanup(devnull.close)
        real_stdout = sys.stdout
        sys.stdout = devnull
        try:
            mod.run()
        except _EndRun:
            pass
        finally:
            sys.stdout = real_stdout
        # Four full pages of the tail, one wait after each.
        self.assertEqual(len(srv.polls), 4)
        self.assertEqual([qs.get("since_id") for qs in srv.polls], [["0"], ["2"], ["4"], ["6"]])
        self.assertEqual(len(clock.sleeps), 4)
        for wait in clock.sleeps:
            self.assertGreaterEqual(wait, self.POLL_SEC, clock.sleeps)


def _run_in_process(test, mod, srv, tok, stop_after_sleeps):
    """Run mod.run() on tok against srv under a virtual clock until its
    stop_after_sleeps-th wait; returns its stdout lines."""
    mod.time = _VirtualTime(stop_after_sleeps=stop_after_sleeps)
    mod.random = _LowRandom()
    saved = {k: os.environ.get(k) for k in ("SKEIN_PUBLIC_URL", "SKEIN_TOKEN_FILE", "SKEIN_ISSUE",
                                             "SKEIN_EXCLUDE_SESSION")}
    test.addCleanup(_restore_env, saved)
    os.environ.update({"SKEIN_PUBLIC_URL": srv.url(), "SKEIN_TOKEN_FILE": tok.token,
                       "SKEIN_ISSUE": harness.ROOT})
    os.environ.pop("SKEIN_EXCLUDE_SESSION", None)
    out = io.StringIO()
    real_stdout = sys.stdout
    sys.stdout = out
    try:
        mod.run()
    except _EndRun:
        pass
    finally:
        sys.stdout = real_stdout
    return [json.loads(line) for line in out.getvalue().splitlines() if line.strip()]


class CursorSaveRetryTest(unittest.TestCase):
    """US-12: a cursor whose save failed is saved on a later page, so a
    restart still delivers the event of the break."""

    def test_failed_save_is_retried_and_the_restart_keeps_the_gap_event(self):
        srv = EventServer()
        self.addCleanup(srv.stop)
        tok = TokenDir(self)
        first = harness.load_runner(poll_sec=1, ceiling_sec=1)
        real_save = first.save_cursor
        calls = []

        def flaky_save(token_file, value):
            calls.append(value)
            if len(calls) == 1:
                raise OSError("no space left on the device")
            return real_save(token_file, value)

        first.save_cursor = flaky_save
        # Live-start anchors at event 0 and its save fails; the next polls
        # answer the same cursor.
        _run_in_process(self, first, srv, tok, stop_after_sleeps=3)
        self.assertGreaterEqual(len(calls), 2, calls)
        self.assertEqual(tok.saved_cursor_id(), 0)

        gap = srv.push()
        second = harness.load_runner(poll_sec=1, ceiling_sec=1)
        lines = _run_in_process(self, second, srv, tok, stop_after_sleeps=1)
        self.assertEqual([l["notification"]["id"] for l in lines if l.get("event") == "notification"],
                         [gap])


def _restore_env(saved):
    for k, v in saved.items():
        if v is None:
            os.environ.pop(k, None)
        else:
            os.environ[k] = v


@unittest.skipUnless(os.name == "posix", "the POSIX state checks")
class StateDirectoryTest(unittest.TestCase):
    """The state is read and written only in a token directory of the user
    that no one else can write to, never through a link to it."""

    def setUp(self):
        self.runner = harness.load_runner()

    def test_state_in_a_directory_others_can_write_is_not_read(self):
        for mode, want in ((0o700, 7), (0o770, None), (0o757, None)):
            with self.subTest(mode=oct(mode)):
                tok = TokenDir(self, cursor_id=7)
                os.chmod(tok.dir, mode)
                cursor = self.runner.load_cursor(tok.token)
                self.assertEqual(cursor and cursor["id"], want)

    def test_state_is_not_written_through_a_linked_directory(self):
        tok = TokenDir(self)
        link = os.path.join(os.path.dirname(tok.dir), "linked-dir")
        os.symlink(tok.dir, link)
        with self.assertRaises(self.runner.PlatformRefusal):
            self.runner.save_cursor(os.path.join(link, "token"), {"ts": "2026-01-01T00:00:01Z", "id": 1})
        self.assertFalse(os.path.exists(tok.state))
        self.assertEqual(sorted(os.listdir(tok.dir)), ["token"])

    def test_state_is_not_written_into_a_directory_others_can_write(self):
        tok = TokenDir(self)
        os.chmod(tok.dir, 0o770)
        with self.assertRaises(self.runner.PlatformRefusal):
            self.runner.save_cursor(tok.token, {"ts": "2026-01-01T00:00:01Z", "id": 1})
        self.assertFalse(os.path.exists(tok.state))


class FileFateTest(unittest.TestCase):
    """Matrix row #25: the state file shares the token file's fate."""

    def setUp(self):
        self.srv = EventServer()
        self.addCleanup(self.srv.stop)

    def assert_all_gone(self, tok, mon):
        self.assertFalse(os.path.exists(tok.token), mon.lines)
        self.assertFalse(os.path.exists(tok.state), mon.lines)
        self.assertFalse(os.path.exists(tok.dir), "the skein-mon-* directory stayed: %r" % mon.lines)

    def assert_kept(self, tok, mon, cursor_id):
        self.assertTrue(os.path.exists(tok.token), mon.lines)
        self.assertEqual(tok.saved_cursor_id(), cursor_id, mon.lines)

    def test_runner_stops_that_delete_the_token_delete_the_state(self):
        # (reason, token, token mode, directory mode, server behaviour, extra env)
        cases = (
            ("token_expired", TOKEN, 0o600, 0o700, lambda: setattr(self.srv, "poll_status", lambda qs: 401), {}),
            ("visibility_lost", TOKEN, 0o600, 0o700, lambda: setattr(self.srv, "poll_status", lambda qs: 403), {}),
            ("root_closed", TOKEN, 0o600, 0o700, lambda: setattr(self.srv, "root_closed", True), {}),
            ("token_file_missing", "", 0o600, 0o700, None, {}),
            ("token_file_insecure", TOKEN, 0o644, 0o755, None, {}),
            ("misconfigured", TOKEN, 0o600, 0o700, None, {"SKEIN_PUBLIC_URL": ""}),
        )
        for reason, token, token_mode, dir_mode, server, extra in cases:
            with self.subTest(reason=reason):
                self.srv.poll_status = lambda qs: 200
                self.srv.root_closed = False
                if server:
                    server()
                tok = TokenDir(self, token=token, token_mode=token_mode, dir_mode=dir_mode, cursor_id=0)
                mon = Monitor(self, self.srv.url(), tok.token, extra_env=extra, runner=True)
                mon.wait_exit()
                self.assertEqual(mon.stop_line()["reason"], reason, mon.lines)
                self.assert_all_gone(tok, mon)

    def test_inline_loader_paths_that_delete_the_token_delete_the_state(self):
        # (reason, token, token mode, directory mode, extra env, script status)
        cases = (
            ("token_expired", TOKEN, 0o600, 0o700, {}, 401),
            ("runner_mismatch", TOKEN, 0o600, 0o700, {"SKEIN_RUNNER_SHA256": "0" * 64}, 200),
            ("token_file_missing", "", 0o600, 0o700, {}, 200),
            ("token_file_insecure", TOKEN, 0o644, 0o755, {}, 200),
            ("misconfigured", TOKEN, 0o600, 0o700, {"SKEIN_PUBLIC_URL": ""}, 200),
        )
        for reason, token, token_mode, dir_mode, extra, script in cases:
            with self.subTest(reason=reason):
                self.srv.script_status = lambda script=script: script
                tok = TokenDir(self, token=token, token_mode=token_mode, dir_mode=dir_mode, cursor_id=3)
                mon = Monitor(self, self.srv.url(), tok.token, extra_env=extra)
                mon.wait_exit()
                self.assertEqual(mon.stop_line()["reason"], reason, mon.lines)
                self.assert_all_gone(tok, mon)

    def test_missing_token_file_takes_its_state(self):
        for runner in (True, False):
            with self.subTest(by="runner" if runner else "inline loader"):
                tok = TokenDir(self, cursor_id=3)
                os.remove(tok.token)
                mon = Monitor(self, self.srv.url(), tok.token, runner=runner)
                mon.wait_exit()
                self.assertEqual(mon.stop_line()["reason"], "token_file_missing", mon.lines)
                self.assert_all_gone(tok, mon)

    def test_stops_that_keep_the_token_keep_the_cursor(self):
        with self.subTest(reason="server_unreachable"):
            tok = TokenDir(self, cursor_id=3)
            mon = Monitor(self, closed_port_url(), tok.token)
            mon.wait_exit()
            self.assertEqual(mon.stop_line()["reason"], "server_unreachable", mon.lines)
            self.assert_kept(tok, mon, 3)
        with self.subTest(reason="stopped"):
            tok = TokenDir(self, cursor_id=3)
            mon = Monitor(self, self.srv.url(), tok.token)
            mon.wait_for(is_event("monitor_started"))
            mon.stop()
            self.assertEqual(mon.stop_line()["reason"], "stopped", mon.lines)
            self.assert_kept(tok, mon, 3)
        with self.subTest(reason="internal_error"):
            tok = TokenDir(self, cursor_id=3)
            mon = Monitor(self, self.srv.url(), tok.token, extra_env=site_env(SKEIN_TEST_RANDOM="raise"))
            mon.wait_exit()
            self.assertEqual(mon.stop_line()["reason"], "internal_error", mon.lines)
            self.assert_kept(tok, mon, 3)

    def test_401_of_a_second_start_beside_a_live_runner(self):
        tok = TokenDir(self)
        live = Monitor(self, self.srv.url(), tok.token)
        live.wait_for(is_event("monitor_started"))
        self.srv.wait_polls(1)
        one = self.srv.push()
        live.wait_for(notification_id(one))
        tok.wait_saved(one)

        # The second start's download is refused: it deletes the token it
        # read and the state beside it, nothing else.
        self.srv.script_status = lambda: 401
        second = Monitor(self, self.srv.url(), tok.token)
        self.assertEqual(second.wait_exit(), 0)
        self.assertEqual(second.stop_line()["reason"], "token_expired", second.lines)
        self.assertFalse(os.path.exists(tok.token))
        self.assertFalse(os.path.exists(tok.state))

        # The live runner goes on without its state and ends on its own
        # reason, with no error about the missing state.
        two = self.srv.push()
        live.wait_for(notification_id(two))
        self.srv.poll_status = lambda qs: 401
        self.assertEqual(live.wait_exit(), 0)
        self.assertEqual(live.stop_line()["reason"], "token_expired", live.lines)
        self.assertEqual([l for l in live.lines if "reason" in l], [live.stop_line()])
        self.assertNotIn("Traceback", "\n".join(live.err))
        self.assertFalse(os.path.exists(tok.dir))


class LegacyModeTest(unittest.TestCase):
    """Matrix row #26: the inline loader runs the body in legacy mode, whatever
    the agent's environment holds."""

    def test_named_mode_variables_change_nothing(self):
        srv = EventServer()
        self.addCleanup(srv.stop)
        bin_dir = tempfile.mkdtemp(prefix="skein-state-bin-")
        self.addCleanup(shutil.rmtree, bin_dir, True)
        marker = os.path.join(bin_dir, "called")
        for name in ("zavod", "tmux", "codex"):
            path = os.path.join(bin_dir, name)
            with open(path, "w") as fh:
                fh.write("#!/bin/sh\necho %s \"$@\" >> '%s'\n" % (name, marker))
            os.chmod(path, stat.S_IRWXU)
        env = {
            "SKEIN_CLIENT": "codex", "SKEIN_PLATFORM": "posix", "SKEIN_MONITOR_MODE": "preflight",
            "SKEIN_MONITOR_CHANNEL": "zavod", "SKEIN_MONITOR_BLIND": "1", "SKEIN_MONITOR_PREFLIGHT": "1",
            "ZAVOD_ME": "00000000-0000-0000-0000-000000000001", "TMUX": "/tmp/tmux-1/default,1,0",
            "PATH": bin_dir + os.pathsep + os.environ.get("PATH", ""),
        }
        tok = TokenDir(self)
        mon = Monitor(self, srv.url(), tok.token, extra_env=env)
        mon.wait_for(is_event("monitor_started"))
        srv.wait_polls(1)
        nid = srv.push()
        mon.wait_for(notification_id(nid))
        self.assertEqual(mon.stop(), 0)
        self.assertEqual([l["event"] for l in mon.lines], ["monitor_started", "notification", "paused"])
        self.assertEqual(mon.stop_line()["reason"], "stopped")
        self.assertFalse(os.path.exists(marker), "a delivery program was run")


if __name__ == "__main__":
    unittest.main()
