#!/usr/bin/env python3
"""Tests of the takeover of a monitor whose addressee changed
(runner/takeover.py with the working start of detach.py and the preflight):
matrix row #68.

Two working starts in one process contend for the one-runner lock like two
processes do (a flock belongs to the open file), so the old runner and its
successors run here as threads over the real preflight, state, lock and
journal. The old runner hands over the way its loop does: it looks for the
takeover request, writes its last cursor and releases the lock.

Expected values are those of the spec (US-12 "takeover", OST-2-8, OST-2-9):
a preflight is allowed while a runner whose addressee changed lives; the
next working start of the last result takes the monitor over, waiting up to
30 s for the lock; "taken_over" is in the journal only after the
successor's monitor_started; a wait that runs out is already_running, the
request is withdrawn and there is no "taken_over"; a successor that fails
before monitor_started leaves the token and the state with the saved cursor
and "takeover_incomplete", and the same working command then goes on from
that cursor; an outdated result is config_invalid, cured by a new preflight
and its working command; of two starts with the result in force exactly one
goes on; an old runner that ended on its own leaves its result
(token_file_missing).

Run on its own: python3 internal/handler/assets/runner_tests/takeover_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import sys
import tempfile
import threading
import types
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

ISSUE = "sk-68"
TOKEN = "skm_" + "K" * 43
REGISTRY = {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
            "agents": [], "channels": [], "shells": {"source": "t", "names": ["sh"]}, "zavodEngines": {}}
CURSOR = {"ts": "2026-01-01T00:00:00Z", "id": 41}


class Start(threading.Thread):
    """A working start of result `number` in a thread: its WorkingStart and
    how begin() ended (None, or the Stop / ConfigInvalid it raised)."""

    def __init__(self, world, number):
        threading.Thread.__init__(self, daemon=True)
        self.world = world
        self.env = dict(world.env, SKEIN_MONITOR_PREFLIGHT=str(number))
        self.held = world.runner.WorkingStart()
        self.error = None

    def run(self):
        try:
            self.held.begin(self.world.token, self.env, self.world.settings, ISSUE)
        except (self.world.runner.Stop, self.world.runner.ConfigInvalid) as exc:
            self.error = exc

    def reason(self):
        err = self.error
        if err is None:
            return None
        return getattr(err, "reason", "config_invalid")


class FakeClock(object):
    """Virtual seconds for the hand-over wait; each sleep also yields a
    little real time to the other threads of the test."""

    def __init__(self):
        self.t = 1000.0

    def now(self):
        return self.t

    def sleep(self, sec):
        self.t += sec
        time.sleep(0.01)


class OldRunner(threading.Thread):
    """The runner whose addressee changed, on the product path: its wait
    between pages (sleep_or_hand_over) asks the product's
    DeliverySink.taken_over, and on TakenOver it writes its cursor and
    hands over with the product's hand_over on a virtual clock; it goes on
    when the successor does not take the monitor."""

    def __init__(self, world, held, end_on_its_own=False):
        threading.Thread.__init__(self, daemon=True)
        self.world = world
        self.held = held
        self.end_on_its_own = end_on_its_own
        self.handed = threading.Event()
        self.stop = threading.Event()
        self.results = []
        self.clock = FakeClock()

    def run(self):
        r = self.world.runner
        sink = types.SimpleNamespace(journal_only=True, token_file=self.world.token, doc={"preflightId": 1})
        waits = types.SimpleNamespace(taken_over=lambda: r.DeliverySink.taken_over(sink))
        while not self.stop.is_set():
            try:
                r.sleep_or_hand_over(0.05, waits)
                continue
            except r.TakenOver:
                pass
            if self.end_on_its_own:
                # root_closed meanwhile: the token goes with its files.
                r.apply_file_fate(self.world.token, True)
                self.held.release()
                self.handed.set()
                return
            r.save_cursor(self.world.token, CURSOR)

            def release():
                self.held.release()
                self.handed.set()
            ended = r.hand_over(self.world.token, self.world.env, release, lambda: self.held.retake(self.world.env),
                                clock=self.clock.now, sleep=self.clock.sleep)
            self.results.append(ended)
            if ended:
                return


class World(object):
    def __init__(self, test):
        self.test = test
        self.root = tempfile.mkdtemp(prefix="skein-takeover-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.runner = harness.load_runner(registry=base64.b64encode(json.dumps(REGISTRY).encode()).decode())
        patcher = mock.patch.object(self.runner, "LOADER_MODE", "client")
        patcher.start()
        test.addCleanup(patcher.stop)
        self.reg = self.runner.load_registry()
        token_dir = os.path.join(self.root, "skein-mon-68")
        os.mkdir(token_dir, 0o700)
        self.token = os.path.join(token_dir, "token")
        with open(self.token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(self.token, 0o600)
        self.env = {"PATH": os.environ.get("PATH", ""), "HOME": self.root,
                    "XDG_STATE_HOME": os.path.join(self.root, "state"), "SKEIN_PUBLIC_URL": "http://127.0.0.1:9",
                    "SKEIN_ISSUE": ISSUE, "SKEIN_TOKEN_FILE": self.token, "SKEIN_CLIENT": "alpha",
                    "SKEIN_PLATFORM": "posix", "SKEIN_MONITOR_CHANNEL": "", "SKEIN_MONITOR_BLIND": "",
                    "SKEIN_MONITOR_MODE": "run"}
        self.settings = self.runner.read_settings(self.env, self.reg, mode="client")

    def preflight(self):
        """Run a preflight; its number, or the Stop it raised."""
        r = self.runner
        try:
            line = r.preflight(self.token, self.env, self.reg, self.settings, detector=r.Detector(),
                               placement=r.place([], self.reg, self.settings.record))
        except r.Stop as stop:
            return stop
        return line["preflightId"]

    def old_runner(self):
        """The first result's runner, holding the lock, its addressee
        changed."""
        self.test.assertEqual(self.preflight(), 1)
        held = self.runner.WorkingStart()
        held.begin(self.token, dict(self.env, SKEIN_MONITOR_PREFLIGHT="1"), self.settings, ISSUE)
        self.test.addCleanup(held.release)
        self.runner.mark_addressee_changed(self.token, 1)
        return held

    def journal(self):
        return self.runner.Journal(self.token, ISSUE, secrets=[TOKEN], env=self.env)

    def journal_events(self):
        path = self.journal().path
        try:
            with open(path) as fh:
                return [json.loads(l).get("event") for l in fh if l.strip()]
        except OSError:
            return []

    def request_file(self):
        return os.path.exists(self.runner.takeover_file_path(self.token))


@unittest.skipUnless(os.name == "posix", "the POSIX one-runner lock")
class TakeoverTest(unittest.TestCase):

    def test_a_preflight_with_a_live_runner_is_refused_unless_its_addressee_changed(self):
        world = World(self)
        self.assertEqual(world.preflight(), 1)
        held = world.runner.WorkingStart()
        held.begin(world.token, dict(world.env, SKEIN_MONITOR_PREFLIGHT="1"), world.settings, ISSUE)
        self.addCleanup(held.release)
        refused = world.preflight()
        self.assertEqual(getattr(refused, "reason", None), "already_running")
        world.runner.mark_addressee_changed(world.token, 1)
        self.assertEqual(world.preflight(), 2)

    def test_the_successor_takes_over_and_taken_over_follows_its_monitor_started(self):
        world = World(self)
        old = world.old_runner()
        old_path = old.lock.path
        self.assertEqual(world.preflight(), 2)
        loop = OldRunner(world, old)
        loop.start()
        start = Start(world, 2)
        start.start()
        start.join(10)
        self.addCleanup(start.held.release)
        self.assertTrue(loop.handed.is_set())
        self.assertIsNone(start.error)
        self.assertEqual(start.held.lock.path, old_path)
        # The successor sees the final cursor of the old runner.
        self.assertEqual(world.runner.load_cursor(world.token), CURSOR)
        self.assertNotIn("taken_over", world.journal_events())
        # The old runner has released the lock and waits for the mark.
        self.assertTrue(loop.is_alive())
        start.held.started(world.journal())
        self.assertEqual(world.journal_events().count("taken_over"), 1)
        loop.join(30)
        self.assertEqual(loop.results, [True])
        self.assertFalse(world.request_file())
        # The new runner starts with its own addressee: a further preflight
        # is refused while it lives.
        self.assertEqual(getattr(world.preflight(), "reason", None), "already_running")

    def test_two_starts_with_the_result_in_force_one_goes_on(self):
        world = World(self)
        old = world.old_runner()
        self.assertEqual(world.preflight(), 2)
        with mock.patch.object(world.runner, "TAKEOVER_WAIT_SEC", 2):
            starts = [Start(world, 2), Start(world, 2)]
            for s in starts:
                s.start()
            loop = OldRunner(world, old)
            loop.start()
            for s in starts:
                s.join(15)
        for s in starts:
            self.addCleanup(s.held.release)
        self.assertEqual(sorted(str(s.reason()) for s in starts), ["None", "already_running"])

    def test_two_preflights_in_any_order_the_last_result_is_in_force(self):
        world = World(self)
        old = world.old_runner()
        self.assertEqual((world.preflight(), world.preflight()), (2, 3))
        stale = Start(world, 2)
        stale.run()
        self.assertEqual(stale.reason(), "config_invalid")
        self.assertFalse(world.request_file())
        loop = OldRunner(world, old)
        loop.start()
        current = Start(world, 3)
        current.run()
        self.addCleanup(current.held.release)
        self.assertIsNone(current.error)

    def test_a_result_replaced_while_the_successor_waits_is_config_invalid(self):
        world = World(self)
        old = world.old_runner()
        self.assertEqual(world.preflight(), 2)
        start = Start(world, 2)
        start.start()
        deadline = time.time() + 10
        while not world.request_file() and time.time() < deadline:
            time.sleep(0.02)
        self.assertEqual(world.preflight(), 3)
        loop = OldRunner(world, old)
        # The request names result 2, no longer the last one: the old runner
        # does not hand over to it.
        loop.start()
        time.sleep(0.3)
        self.assertFalse(loop.handed.is_set())
        loop.stop.set()
        old.release()
        start.join(40)
        self.addCleanup(start.held.release)
        self.assertEqual(start.reason(), "config_invalid")
        start.held.failed(world.runner.Stop("config_invalid"))
        self.assertIn("takeover_incomplete", world.journal_events())
        self.assertTrue(os.path.exists(world.token))

    def test_the_owner_ending_on_its_own_leaves_its_result(self):
        world = World(self)
        old = world.old_runner()
        self.assertEqual(world.preflight(), 2)
        loop = OldRunner(world, old, end_on_its_own=True)
        loop.start()
        start = Start(world, 2)
        start.run()
        self.addCleanup(start.held.release)
        self.assertEqual(start.reason(), "token_file_missing")
        start.held.failed(start.error)
        self.assertNotIn("takeover_incomplete", world.journal_events())
        self.assertNotIn("taken_over", world.journal_events())

    def test_a_wait_that_runs_out_is_already_running_and_withdraws_the_request(self):
        world = World(self)
        world.old_runner()
        self.assertEqual(world.preflight(), 2)
        with mock.patch.object(world.runner, "TAKEOVER_WAIT_SEC", 1):
            start = Start(world, 2)
            start.run()
        self.assertEqual(start.reason(), "already_running")
        self.assertFalse(world.request_file())
        self.assertNotIn("taken_over", world.journal_events())

    def test_a_crash_before_monitor_started_then_the_same_command_goes_on(self):
        world = World(self)
        old = world.old_runner()
        self.assertEqual(world.preflight(), 2)
        loop = OldRunner(world, old)
        loop.start()
        start = Start(world, 2)
        start.run()
        self.assertIsNone(start.error)
        # The successor dies before its monitor_started: the old runner
        # takes the lock back and goes on (review of wave 8, row 8).
        start.held.failed(world.runner.Stop("internal_error"))
        start.held.release()
        deadline = time.time() + 30
        while not loop.results and time.time() < deadline:
            time.sleep(0.02)
        self.assertEqual(loop.results, [False])
        self.assertIn("takeover_incomplete", world.journal_events())
        self.assertTrue(os.path.exists(world.token))
        self.assertEqual(world.runner.load_cursor(world.token), CURSOR)
        # The same working command again goes on from the saved cursor.
        again = Start(world, 2)
        again.run()
        self.addCleanup(again.held.release)
        self.assertIsNone(again.error)
        self.assertEqual(world.runner.load_cursor(world.token), CURSOR)

    def test_config_invalid_is_cured_by_a_new_preflight_and_its_command(self):
        world = World(self)
        old = world.old_runner()
        self.assertEqual(world.preflight(), 2)
        stale = Start(world, 1)
        stale.run()
        self.assertEqual(stale.reason(), "config_invalid")
        # Running the same command again does not help.
        stale_again = Start(world, 1)
        stale_again.run()
        self.assertEqual(stale_again.reason(), "config_invalid")
        self.assertEqual(world.preflight(), 3)
        loop = OldRunner(world, old)
        loop.start()
        fresh = Start(world, 3)
        fresh.run()
        self.addCleanup(fresh.held.release)
        self.assertIsNone(fresh.error)

    def test_a_signal_during_the_wait_withdraws_the_request(self):
        # Review of wave 8, row 8: the successor stopped by a signal while it
        # waits for the lock leaves no request, so the old runner does not
        # hand over to nobody.
        world = World(self)
        world.old_runner()
        self.assertEqual(world.preflight(), 2)
        r = world.runner
        held = r.WorkingStart()
        with mock.patch.object(r, "wait_runner_lock", mock.Mock(side_effect=r.StopSignal())):
            with self.assertRaises(r.StopSignal):
                r.take_over(world.token, dict(world.env, SKEIN_MONITOR_PREFLIGHT="2"), world.settings, held=held)
        self.assertFalse(world.request_file())
        self.assertIsNone(held.takeover)

    def test_the_old_runner_goes_on_when_no_successor_comes(self):
        # A request whose successor never takes the lock (it died after
        # writing it): after the wait the old runner takes the lock back.
        world = World(self)
        old = world.old_runner()
        self.assertEqual(world.preflight(), 2)
        r = world.runner
        r.request_takeover(world.token, 2)
        clock = FakeClock()
        ended = r.hand_over(world.token, world.env, old.release, lambda: old.retake(world.env),
                            clock=clock.now, sleep=clock.sleep)
        self.assertFalse(ended)
        self.assertIsNotNone(old.lock)
        self.assertFalse(world.request_file())
        self.assertGreaterEqual(clock.t - 1000.0, r.TAKEOVER_WAIT_SEC)

    def test_the_old_runner_hands_over_from_its_wait_between_pages(self):
        world = World(self)
        world.old_runner()
        self.assertEqual(world.preflight(), 2)
        world.runner.request_takeover(world.token, 2)

        class Sink(object):
            def taken_over(self):
                return world.runner.takeover_requested(world.token, 1)
        with self.assertRaises(world.runner.TakenOver):
            world.runner.sleep_or_hand_over(5, Sink())


if __name__ == "__main__":
    unittest.main()
