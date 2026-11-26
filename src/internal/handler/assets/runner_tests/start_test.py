#!/usr/bin/env python3
"""Tests of the working start of client mode (runner/detach.py, run through
runner/core.py#run): matrix rows #37, #38, #39 and the removal of stale
lock files (P-5).

The synchronous process runs in this process, the way the loader runs the
body in client mode; the detached child it starts is a real process that
runs the rendered body and polls a local fake server. The child's pid comes
from a spy on spawn_detached, so a test can stop it with a signal.

Run on its own: python3 internal/handler/assets/runner_tests/start_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import signal
import sys
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

GENERAL_WARNING = "WARN-general-6b31"
REGISTRY = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": [GENERAL_WARNING]},
    "agents": [
        {"id": "streamer", "process": {"executables": ["streamer"]}, "methods": [
            {"kind": "stream", "conditions": {}, "instruction": "INSTR-stream"}]},
    ],
    "channels": [],
    "shells": {"source": "test", "names": ["sh", "bash"]},
    "zavodEngines": {},
}
REGISTRY_B64 = base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii")
RUNNER = harness.load_runner(registry=REGISTRY_B64)
BODY = harness.render_runner(registry=REGISTRY_B64)
REG = RUNNER.load_registry()
ISSUE = "sk-37"
# The keys of the full monitor_started of the first start by a result (C1).
FULL_KEYS = {"event", "issue", "pollIntervalSec", "preflightId", "method", "channel", "reason",
             "warnings", "journal"}
DAY = 24 * 3600


def wait_until(pred, sec=30):
    deadline = time.monotonic() + sec
    while time.monotonic() < deadline:
        if pred():
            return True
        time.sleep(0.05)
    return False


class Box(object):
    """A private token directory with its token, a state home, a fake poll
    server and the working environment of client "alpha" (the general
    record: the journal method)."""

    def __init__(self, test, respond=None):
        self.test = test
        self.root = tempfile.mkdtemp(prefix="skein-start-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.token_dir = os.path.join(self.root, "skein-mon-start")
        os.mkdir(self.token_dir, 0o700)
        self.token = os.path.join(self.token_dir, "token")
        with open(self.token, "w") as fh:
            fh.write("skm_" + "S" * 43)
        os.chmod(self.token, 0o600)
        self.state_home = os.path.join(self.root, "state")
        self.server = harness.ScriptedServer(respond or (lambda h, qs: h.send_json(200, harness.EMPTY)))
        test.addCleanup(self.server.stop)
        self.pids = []
        test.addCleanup(self.kill_children)

    def env(self, **extra):
        env = {"PATH": os.environ.get("PATH", ""), "HOME": self.root, "XDG_STATE_HOME": self.state_home,
               "SKEIN_PUBLIC_URL": self.server.url(), "SKEIN_ISSUE": ISSUE, "SKEIN_TOKEN_FILE": self.token,
               "SKEIN_EXCLUDE_SESSION": "", "SKEIN_RUNNER_SHA256": "ab" * 32, "SKEIN_LOADER_SHA256": "cd" * 32,
               "SKEIN_CLIENT": "alpha", "SKEIN_PLATFORM": "posix", "SKEIN_MONITOR_CHANNEL": "",
               "SKEIN_MONITOR_BLIND": ""}
        for k, v in extra.items():
            if v is None:
                env.pop(k, None)
            else:
                env[k] = v
        return env

    def preflight(self, **extra):
        """A real preflight of this token; returns its line."""
        env = self.env(**extra)
        settings = RUNNER.read_settings(env, REG, mode="client")
        return RUNNER.preflight(self.token, env, REG, settings, detector=RUNNER.Detector())

    def start(self, number, before_spawn=None, **extra):
        """The working command with SKEIN_MONITOR_PREFLIGHT=number, run as
        the loader runs it; returns (exit code, printed lines)."""
        run = {"SKEIN_MONITOR_MODE": "run", "SKEIN_MONITOR_PREFLIGHT": str(number)}
        run.update(extra)
        env = self.env(**run)
        lines = []
        spawn = RUNNER.PosixPlatform.spawn_detached

        def spy(*args):
            if before_spawn is not None:
                before_spawn()
            pid, receipt = spawn(*args)
            self.pids.append(pid)
            return pid, receipt

        with mock.patch.dict(os.environ, env, clear=True), \
                mock.patch.object(RUNNER, "LOADER_MODE", "client"), \
                mock.patch.object(RUNNER, "_skein_loader_body", BODY, create=True), \
                mock.patch.object(RUNNER.PosixPlatform, "spawn_detached", staticmethod(spy)), \
                mock.patch.object(RUNNER, "emit", lines.append):
            code = RUNNER.run()
        return code, lines

    def lock_busy(self):
        """Whether a process holds the one-runner lock of the token."""
        try:
            lock = RUNNER.PosixPlatform.acquire_runner_lock(self.token, 0, self.env())
        except RUNNER.LockBusy:
            return True
        lock.release()
        return False

    def stop_child(self):
        pid = self.pids.pop()
        os.kill(pid, signal.SIGTERM)
        if not wait_until(lambda: not alive(pid)):
            raise AssertionError("the detached runner did not stop")

    def kill_children(self):
        for pid in self.pids:
            try:
                os.killpg(pid, signal.SIGKILL)
            except OSError:
                pass

    def journal_lines(self):
        path = RUNNER.journal_path(self.token, self.env())
        try:
            with open(path) as fh:
                return [json.loads(l) for l in fh if l.strip()]
        except FileNotFoundError:
            return []


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    return True


def one_stop(test, code, lines, reason, want_code):
    """Exactly one line, the stop line of reason, and no monitor_started."""
    test.assertEqual(len(lines), 1, lines)
    test.assertEqual(lines[0]["reason"], reason, lines)
    test.assertEqual(lines[0]["event"], RUNNER.STOPS[reason][0])
    test.assertEqual(code, want_code)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX detached start")
class MonitorStartedTest(unittest.TestCase):
    """Matrix row #37 (R-3, P-6, C1, SC-012): monitor_started if and only if
    a live holder of the lock runs the current result."""

    def test_monitor_started_means_a_live_holder_of_the_lock(self):
        box = Box(self)
        pre = box.preflight()
        self.assertEqual(pre["method"], "journal")
        code, lines = box.start(pre["preflightId"])
        self.assertEqual((code, len(lines)), (0, 1), lines)
        first = lines[0]
        self.assertEqual(first["event"], "monitor_started")
        # At the return the child is alive and holds the lock.
        self.assertTrue(alive(box.pids[-1]))
        self.assertTrue(box.lock_busy(), "monitor_started while no process holds the lock")
        # The first start by the result: the full line repeats the preflight.
        self.assertEqual(set(first), FULL_KEYS)
        self.assertEqual((first["preflightId"], first["method"], first["channel"]),
                         (pre["preflightId"], pre["method"], pre["channel"]))
        self.assertEqual((first["reason"], first["warnings"], first["journal"]),
                         (pre["reason"], pre["warnings"], pre["journal"]))
        # The general record: no list of ids (its warning) in the line.
        self.assertNotIn(GENERAL_WARNING, json.dumps(first))
        self.assertTrue(wait_until(lambda: box.server.requests), "the detached runner never polled")

        # A restart by the same command: the short line only.
        box.stop_child()
        code, lines = box.start(pre["preflightId"])
        self.assertEqual((code, len(lines)), (0, 1), lines)
        self.assertEqual(lines[0], {"event": "monitor_started", "preflightId": pre["preflightId"],
                                    "method": "journal", "channel": None})
        self.assertLessEqual(len(json.dumps(lines[0], separators=(",", ":")).encode("utf-8")), 256)

        # A new result: its first start is full again.
        box.stop_child()
        again = box.preflight()
        code, lines = box.start(again["preflightId"])
        self.assertEqual((code, set(lines[0]), lines[0]["preflightId"]), (0, FULL_KEYS, again["preflightId"]))

    def test_a_failed_start_of_the_child_prints_one_stop_line(self):
        cases = []

        def no_token(box):
            os.remove(box.token)
        cases.append(("no token", no_token, "token_file_missing", 1))

        def lock_held(box):
            box.held = RUNNER.PosixPlatform.acquire_runner_lock(box.token, 0, box.env())
            self.addCleanup(box.held.release)
        cases.append(("lock held", lock_held, "already_running", 0))

        def lock_os_error(box):
            locks = os.path.join(box.state_home, "skein-monitor", "locks")
            shutil.rmtree(locks)
            with open(locks, "w") as fh:
                fh.write("not a directory")
        cases.append(("lock OS error", lock_os_error, "config_invalid", 1))

        def journal_open(box):
            os.chmod(os.path.join(box.state_home, "skein-monitor", "journal"), 0o755)
        cases.append(("journal open to others", journal_open, "config_invalid", 1))

        for name, spoil, reason, want_code in cases:
            with self.subTest(case=name):
                box = Box(self)
                pre = box.preflight()
                spoil(box)
                code, lines = box.start(pre["preflightId"])
                one_stop(self, code, lines, reason, want_code)
                self.assertEqual(box.server.requests, [])
                for pid in box.pids:
                    self.assertTrue(wait_until(lambda: not alive(pid)), "a refused child is still running")


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX detached start")
class StaleResultTest(unittest.TestCase):
    """Matrix row #38 (OST-2-9, P-6): a result that is not the current one
    never runs: config_invalid with no ready, no poll and no delivery."""

    def assert_refused_without_poll(self, box, code, lines, variable):
        one_stop(self, code, lines, "config_invalid", 1)
        self.assertIn(variable, lines[0]["action"])
        self.assertEqual(box.server.requests, [], "polled before the result was checked")
        self.assertEqual(box.journal_lines(), [], "the journal got lines of a refused start")
        self.assertTrue(os.path.exists(box.token), "the token was not kept")

    def test_a_result_replaced_before_the_child_holds_the_lock(self):
        box = Box(self)
        pre = box.preflight()
        # The controlled break: after the check of the synchronous process,
        # before the child takes the lock, a new preflight replaces the result.
        code, lines = box.start(pre["preflightId"], before_spawn=box.preflight)
        self.assert_refused_without_poll(box, code, lines, "SKEIN_MONITOR_PREFLIGHT")
        self.assertTrue(wait_until(lambda: not alive(box.pids[-1])))
        self.assertFalse(box.lock_busy())

    def test_every_case_of_the_table_is_refused_before_a_poll(self):
        def no_result(box, pre):
            os.remove(os.path.join(box.token_dir, "preflight.json"))
            return box.start(pre["preflightId"])

        def no_number(box, pre):
            return box.start(pre["preflightId"], SKEIN_MONITOR_PREFLIGHT=None)

        def other_number(box, pre):
            return box.start(pre["preflightId"] + 1)

        def other_override(box, pre):
            return box.start(pre["preflightId"], SKEIN_MONITOR_CHANNEL="journal")

        def journal_dir_unavailable(box, pre):
            os.chmod(os.path.join(box.state_home, "skein-monitor", "journal"), 0o755)
            return box.start(pre["preflightId"])

        def old_python(box, pre):
            with mock.patch.object(RUNNER, "MIN_PYTHON", (99, 0)):
                return box.start(pre["preflightId"])

        def other_platform(box, pre):
            return box.start(pre["preflightId"], SKEIN_PLATFORM="windows")

        for name, run, variable in (
                ("no result", no_result, "SKEIN_MONITOR_PREFLIGHT"),
                ("no SKEIN_MONITOR_PREFLIGHT", no_number, "SKEIN_MONITOR_PREFLIGHT"),
                ("another number", other_number, "SKEIN_MONITOR_PREFLIGHT"),
                ("another override", other_override, "SKEIN_MONITOR_CHANNEL"),
                ("journal directory unavailable", journal_dir_unavailable, "journal"),
                ("Python version", old_python, "python3"),
                ("platform", other_platform, "SKEIN_PLATFORM")):
            with self.subTest(case=name):
                box = Box(self)
                pre = box.preflight()
                code, lines = run(box, pre)
                self.assert_refused_without_poll(box, code, lines, variable)
                self.assertEqual(box.pids, [], "a child was started for a refused result")


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX detached start")
class JournalMethodTest(unittest.TestCase):
    """Matrix row #39 (US-8, the life of the journal method)."""

    def test_the_journal_method_starts_detached_and_writes_the_journal(self):
        page = {"notifications": [{"id": 5, "type": "comment_added", "entity": {"type": "issue", "id": ISSUE},
                                   "actor": {"username": "bob"}, "commentId": "c-5", "bodyPreview": "hello"}],
                "cursor": {"ts": "2026-01-01T00:00:05Z", "id": 5}}
        served = []

        def respond(h, qs):
            h.send_json(200, harness.EMPTY if served else page)
            served.append(1)

        box = Box(self, respond)
        pre = box.preflight()
        code, lines = box.start(pre["preflightId"])
        self.assertEqual((code, [l["event"] for l in lines]), (0, ["monitor_started"]))
        self.assertEqual(lines[0]["method"], "journal")
        # Returned to the agent; the detached process holds the lock, polls
        # and writes the event to the journal.
        self.assertTrue(box.lock_busy())
        delivered = lambda: [l for l in box.journal_lines()
                             if l.get("event") == "delivery" and l.get("notificationId") == 5]
        self.assertTrue(wait_until(delivered), box.journal_lines())
        self.assertEqual(delivered()[0]["status"], "journal")
        self.assertEqual(delivered()[0]["bodyPreview"], "hello")
        self.assertTrue([l for l in box.journal_lines() if l.get("event") == "monitor_started"])

    def test_misconfigured_is_the_broken_bootstrap_only(self):
        box = Box(self)
        pre = box.preflight()
        code, lines = box.start(pre["preflightId"], SKEIN_PUBLIC_URL="")
        one_stop(self, code, lines, "misconfigured", 1)
        self.assertEqual(lines[0]["action"], RUNNER.stop_action("misconfigured", ISSUE))
        self.assertEqual(box.pids, [])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX flock")
class StaleLockSweepTest(unittest.TestCase):
    """P-5: the start of a monitor removes the files of locks/ older than
    24 h that no one holds; a held one and a fresh one stay."""

    def test_old_unlocked_lock_files_are_removed_at_start(self):
        box = Box(self)
        pre = box.preflight()
        locks = os.path.join(box.state_home, "skein-monitor", "locks")
        paths = {}
        for name in ("old", "held", "fresh"):
            paths[name] = os.path.join(locks, "%s.lock" % name)
            fd = os.open(paths[name], os.O_RDWR | os.O_CREAT, 0o600)
            os.close(fd)
        old = time.time() - DAY - 3600
        os.utime(paths["old"], (old, old))
        os.utime(paths["held"], (old, old))
        import fcntl
        held = os.open(paths["held"], os.O_RDWR)
        self.addCleanup(os.close, held)
        fcntl.flock(held, fcntl.LOCK_EX)
        code, lines = box.start(pre["preflightId"])
        self.assertEqual((code, lines[0]["event"]), (0, "monitor_started"), lines)
        self.assertFalse(os.path.exists(paths["old"]), "an old unlocked lock file was kept")
        self.assertTrue(os.path.exists(paths["held"]), "a held lock file was removed")
        self.assertTrue(os.path.exists(paths["fresh"]), "a fresh lock file was removed")
        # The start's own lock file stays and is held.
        self.assertTrue(os.path.exists(RUNNER.runner_lock_path(box.token, box.env())))
        self.assertTrue(box.lock_busy())


if __name__ == "__main__":
    unittest.main()
