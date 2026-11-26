#!/usr/bin/env python3
"""Tests of the access boundary and the end of a client-mode monitor
(runner/finalize.py with runner/deliver.py and runner/core.py): matrix rows
#58 to #62 and #121.

The monitors are the DeliverySink of deliver_test.py over fake channel
programs and a virtual clock; the poll of the access boundary goes to a
local scripted server through the runner's real request executor.

Run on its own: python3 internal/handler/assets/runner_tests/finalize_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_channel  # noqa: E402
import harness  # noqa: E402
from deliver_test import CURSOR, ISSUE, QUEUED, REFUSE, TABLE, TOKEN, World, chan, note  # noqa: E402

GONE_TABLE = TABLE + [{"match": {"exitCode": 6}, "outcome": "addressee-gone"}]


def server_answering(test, statuses, page=None):
    """A poll server answering the given statuses in turn (the last one
    repeats); a 200 carries page."""
    answers = list(statuses)

    def respond(h, qs):
        status = answers.pop(0) if len(answers) > 1 else answers[0]
        h.send_json(status, page if status == 200 else {})
    server = harness.ScriptedServer(respond)
    test.addCleanup(server.stop)
    return server


def poll_cancel(test, world, server, stale_by=0):
    executor = world.runner.Executor()
    test.addCleanup(executor.stop)
    cancel = world.runner.PollCancel(executor, server.url(), TOKEN, "", world.clock, interval=1)
    cancel.last_ok -= stale_by
    return cancel


def single_fit(world):
    """A line limit that takes one event of note() but not two."""
    text, _, _ = world.runner.plain_batch([note(1)], "/x/exclude_session")
    return len(text) + 40


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class AccessBoundaryTest(unittest.TestCase):
    """Row #58: after 401/403 nothing goes to a channel; waiting events get
    "access lost" without the preview; a send needs a successful poll at
    most two intervals old."""

    def test_no_call_after_a_stop_answer_and_waiting_events_lose_access(self):
        for status, reason in ((401, "token_expired"), (403, "visibility_lost")):
            with self.subTest(status=status):
                world = World(self, [chan("A", "chan-a"), chan("B", "chan-b")])
                world.prog("chan-a").respond([REFUSE])
                server = server_answering(self, [status])
                mon = world.monitor(cancel=poll_cancel(self, world, server))
                with self.assertRaises(world.runner.Stop) as ctx:
                    mon.page([note(1), note(2)])
                self.assertEqual((ctx.exception.reason, ctx.exception.status), (reason, status))
                self.assertTrue(ctx.exception.delete_token)
                self.assertEqual(len(world.prog("chan-a").calls()), 1)
                self.assertEqual(world.prog("chan-b").calls(), [])
                self.assertEqual(mon.deliveries(), [(1, "access-lost", None, False),
                                                    (2, "access-lost", None, False)])

    def test_the_age_of_the_last_successful_poll(self):
        world = World(self, [chan("A", "chan-a")])
        cancel = world.runner.PollCancel(None, "http://127.0.0.1:9", TOKEN, "", world.clock, interval=10)
        start = cancel.last_ok
        world.clock.t = start + 1
        self.assertTrue(cancel.may_send(), "a fresh answer")
        world.clock.t = start + 20
        self.assertTrue(cancel.may_send(), "exactly two intervals")
        world.clock.t = start + 20.5
        self.assertFalse(cancel.may_send(), "older than two intervals")
        world.clock.t = start + 1
        cancel.stop = ("token_expired", 401)
        self.assertFalse(cancel.may_send(), "a stop answer")

    def test_a_stale_poll_holds_the_send_until_a_fresh_one(self):
        world = World(self, [chan("A", "chan-a"), chan("B", "chan-b")])
        server = server_answering(self, [500, 500, 200], page={"notifications": [], "cursor": CURSOR})
        mon = world.monitor(cancel=poll_cancel(self, world, server, stale_by=5))
        polls_at_call = []
        mon.spy.before = lambda program: polls_at_call.append(len(server.requests))
        mon.page([note(1)])
        self.assertEqual(len(polls_at_call), 1)
        self.assertGreaterEqual(polls_at_call[0], 3)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "queued")])


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class BoundaryPollTest(unittest.TestCase):
    """Review findings V2 and 32 of wave 7: only a page is a successful poll
    for the boundary, and a failed poll is not repeated faster than a poll
    interval (or its Retry-After)."""

    def test_a_200_without_a_page_is_not_a_successful_poll(self):
        for body in (["not", "a", "page"], {"notifications": "nope"}):
            with self.subTest(body=body):
                world = World(self, [chan("A", "chan-a")])
                server = server_answering(self, [200], page=body)
                cancel = poll_cancel(self, world, server, stale_by=5)
                self.assertFalse(cancel.may_send())
                self.assertIsNone(cancel.check())
                self.assertEqual(len(server.requests), 1)
                self.assertFalse(cancel.may_send(), "a 200 without a page opened the boundary")

    def test_a_failed_poll_waits_an_interval(self):
        world = World(self, [chan("A", "chan-a")])
        server = server_answering(self, [500])
        cancel = poll_cancel(self, world, server, stale_by=5)
        cancel.check()
        cancel.check()
        world.clock.t += 0.5
        cancel.check()
        self.assertEqual(len(server.requests), 1)
        world.clock.t += 0.5
        cancel.check()
        self.assertEqual(len(server.requests), 2)

    def test_a_429_waits_its_retry_after(self):
        world = World(self, [chan("A", "chan-a")])
        server = harness.ScriptedServer(lambda h, qs: h.send_json(429, {}, headers={"Retry-After": "5"}))
        self.addCleanup(server.stop)
        cancel = poll_cancel(self, world, server, stale_by=5)
        cancel.check()
        world.clock.t += 4.5
        cancel.check()
        self.assertEqual(len(server.requests), 1)
        world.clock.t += 0.5
        cancel.check()
        self.assertEqual(len(server.requests), 2)


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class AccessLostAfterInsertTest(unittest.TestCase):
    """Row #59: a two-step channel, the insert confirmed, the poll during
    the wait answers 401 (or 403): no Enter, "unknown" without the preview,
    the other waiting events "access lost", a second monitor does not
    deliver the inserted event."""

    def test_no_enter_after_a_stop_answer(self):
        for status, reason in ((401, "token_expired"), (403, "visibility_lost")):
            with self.subTest(status=status):
                world = World(self, [chan("A", "chan-a"), chan("B", "chan-b")])
                limit = single_fit(world)
                world = World(self, [chan("A", "chan-a", limit=limit), chan("B", "chan-b")])
                insert, enter = fake_channel.two_step(world.bin)
                server = server_answering(self, [status])
                mon = world.monitor(cancel=poll_cancel(self, world, server))
                step = {"executor": {"type": "argv", "argv": ["insert"], "feed": "stdin", "timeoutSec": 2},
                        "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}]}
                commit = {"executor": {"type": "argv", "argv": ["enter"], "feed": "stdin", "timeoutSec": 2},
                          "outcomes": [{"match": {"exitCode": 0}, "outcome": "turn-started"}]}
                mon.sink.drivers["A"] = world.runner.TwoStepDriver(world.reg.channel("A"), mon.spy, step, commit)

                def after(program):
                    if program == "insert":
                        world.clock.t += 2   # the poll is due before Enter
                mon.spy.after = after
                with self.assertRaises(world.runner.Stop) as ctx:
                    mon.page([note(1), note(2)])
                self.assertEqual(ctx.exception.reason, reason)
                self.assertEqual(len(insert.calls()), 1)
                self.assertEqual(enter.calls(), [])
                self.assertEqual(world.prog("chan-b").calls(), [])
                self.assertEqual(mon.deliveries(), [(1, "unknown", "A", False), (2, "access-lost", None, False)])
                second = world.monitor("tok-b")
                world.clock.t += 60
                second.page([note(1)])
                self.assertEqual(len(insert.calls()), 1)
                self.assertEqual(world.prog("chan-a").calls(), [])
                self.assertEqual([d[:2] for d in second.deliveries()], [(1, "unknown")])


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class PollDuringWaitTest(unittest.TestCase):
    """Row #60: the poll during a wait asks from the cursor of the last
    fixed outcome and queues nothing again; the wait for a delivery is
    bounded by the idle window; an event that left the monitor's scope
    while it waited is still delivered (access is checked, not scope)."""

    def test_the_poll_asks_from_the_last_fixed_outcome_and_queues_nothing_again(self):
        world = World(self, [chan("A", "chan-a"), chan("B", "chan-b")])
        world.prog("chan-a").respond([REFUSE, QUEUED])
        server = server_answering(self, [200], page={"notifications": [note(1)],
                                                     "cursor": {"ts": "2026-01-01T00:00:09Z", "id": 99}})
        mon = world.monitor(cancel=poll_cancel(self, world, server))
        mon.page([note(1)])
        self.assertGreaterEqual(len(server.requests), 1)
        self.assertEqual(set((q.get("since_id") or [None])[0] for q in server.requests), {str(CURSOR["id"])})
        self.assertEqual(len(world.prog("chan-a").calls()), 2)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "queued")])

    def test_the_wait_is_bounded_by_the_idle_window(self):
        world = World(self, [chan("A", "chan-a")])
        world.prog("chan-a").respond([REFUSE])
        mon = world.monitor(fallbacks=(), idle=40)
        t0 = world.clock.now()
        mon.page([note(1)])
        times = mon.spy.times("chan-a")
        self.assertEqual(times, [t0, t0 + 5, t0 + 35])
        self.assertLessEqual(world.clock.now() - t0, 40 + 1)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "refused-before-send")])

    def test_an_event_that_left_the_scope_is_still_delivered(self):
        world = World(self, [chan("A", "chan-a")])
        world.prog("chan-a").respond([REFUSE, QUEUED])
        server = server_answering(self, [200], page={"notifications": [], "cursor": CURSOR})
        mon = world.monitor(fallbacks=(), cancel=poll_cancel(self, world, server))
        mon.page([note(1)])
        self.assertGreaterEqual(len(server.requests), 1)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "queued")])


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class EndUnderLockTest(unittest.TestCase):
    """Row #61: the end runs under the held one-runner lock: descriptors
    closed, then the files, then the empty directory, the lock released
    last; a competing start of the token does not begin meanwhile and sees
    the final fate afterwards; another token is independent; a stop that
    keeps access sends one receipt, a failed receipt leaves only the
    journal; addressee_gone sends none; a stop while the journal refuses
    keeps a tail that another start appends."""

    def setUp(self):
        self.world = World(self, [chan("A", "chan-a", table=GONE_TABLE), chan("B", "chan-b")])
        self.r = self.world.runner
        self.mon = self.world.monitor()
        self.token_dir = os.path.dirname(self.mon.token)
        for name in ("state.json", "exclude_session", "starter.py", "preflight.json", "state.lock", "msg-left"):
            with open(os.path.join(self.token_dir, name), "w") as fh:
                fh.write("x")
        self.lock = self.r.take_runner_lock(self.mon.token, self.world.env)
        self.addCleanup(self.lock.release)
        patch = mock.patch.dict(self.r.FINALIZE_POINTS, {}, clear=True)
        patch.start()
        self.addCleanup(patch.stop)
        self.lines = []

    def busy(self, token):
        try:
            lock = self.r.take_runner_lock(token, self.world.env)
        except self.r.LockBusy:
            return True
        lock.release()
        return False

    def end(self, stop, sink=None):
        code = self.r.finish_client(stop, ISSUE, self.mon.token, sink=sink, write=self.lines.append, preflight=3)
        self.lock.release()
        return code

    def test_the_order_of_the_end(self):
        fd = os.open(os.path.join(self.token_dir, "state.json"), os.O_RDONLY)
        self.r.TOKEN_DIR_FDS.add(fd)
        trace = []

        def is_open(n):
            try:
                os.fstat(n)
                return True
            except OSError:
                return False

        def at(name):
            return lambda: trace.append((name, is_open(fd), os.path.exists(self.mon.token),
                                         os.path.isdir(self.token_dir), self.busy(self.mon.token)))
        for name in ("descriptors_closed", "files_removed", "token_dir_removed"):
            self.r.FINALIZE_POINTS[name] = at(name)
        self.end(self.r.access_stop("token_expired"), sink=self.mon.sink)
        self.assertEqual(trace, [("descriptors_closed", False, True, True, True),
                                 ("files_removed", False, False, True, True),
                                 ("token_dir_removed", False, False, False, True)])
        self.assertFalse(self.busy(self.mon.token))

    def test_a_competing_start_waits_and_another_token_is_independent(self):
        other_dir = os.path.join(self.world.root, "tok-other")
        os.mkdir(other_dir, 0o700)
        other = os.path.join(other_dir, "token")
        with open(other, "w") as fh:
            fh.write(TOKEN)
        seen = {}

        def at_removed():
            seen["same"] = self.busy(self.mon.token)
            seen["other"] = self.busy(other)
        self.r.FINALIZE_POINTS["token_dir_removed"] = at_removed
        self.end(self.r.access_stop("visibility_lost"))
        self.assertEqual(seen, {"same": True, "other": False})
        after = self.r.take_runner_lock(self.mon.token, self.world.env)
        after.release()
        self.assertFalse(os.path.exists(self.mon.token))
        self.assertFalse(os.path.isdir(self.token_dir))

    def test_a_stop_that_keeps_access_sends_one_receipt(self):
        self.end(self.r.Stop("root_closed", delete_token=True), sink=self.mon.sink)
        calls = self.world.prog("chan-a").calls()
        self.assertEqual(len(calls), 1)
        self.assertIn("reason=root_closed", calls[0]["stdin"])
        last = self.mon.lines()[-1]
        self.assertEqual((last["event"], last["reason"]), ("closed", "root_closed"))

    def test_the_receipt_of_a_client_mode_stop_names_the_working_command(self):
        # Review finding 22 of wave 7 (the stop table: in client mode
        # stopped and internal_error name the working command of the
        # preflight, never the bootstrap).
        for reason in ("stopped", "internal_error"):
            with self.subTest(reason=reason):
                world = World(self, [chan("A", "chan-a")])
                mon = world.monitor()
                world.runner.finish_client(world.runner.Stop(reason), ISSUE, mon.token, sink=mon.sink,
                                           write=lambda line: None, preflight=3)
                (call,) = world.prog("chan-a").calls()
                self.assertIn("Run the working command of preflight 3 again", call["stdin"])
                self.assertNotIn("bootstrap", call["stdin"])

    def test_a_failed_receipt_leaves_only_the_journal_line(self):
        self.world.prog("chan-a").respond([{"exit": 9}])
        self.end(self.r.Stop("stopped"), sink=self.mon.sink)
        self.assertEqual(len(self.world.prog("chan-a").calls()), 1)
        last = self.mon.lines()[-1]
        self.assertEqual((last["event"], last["reason"]), ("paused", "stopped"))
        self.assertTrue(os.path.exists(self.mon.token))

    def test_addressee_gone_closes_for_the_addressee_without_a_receipt(self):
        limit = single_fit(self.world)
        world = World(self, [chan("A", "chan-a", table=GONE_TABLE, limit=limit), chan("B", "chan-b")])
        world.prog("chan-a").respond([{"exit": 6}])
        mon = world.monitor()
        lock = world.runner.take_runner_lock(mon.token, world.env)
        with self.assertRaises(world.runner.Stop) as ctx:
            mon.page([note(1), note(2)])
        stop = ctx.exception
        self.assertEqual((stop.reason, stop.delete_token), ("addressee_gone", True))
        world.runner.finish_client(stop, ISSUE, mon.token, sink=mon.sink, write=self.lines.append)
        lock.release()
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertFalse(os.path.exists(mon.token))
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "addressee-gone"), (2, "addressee-gone")])
        second = world.monitor("tok-b")
        world.clock.t += 60
        second.page([note(1), note(2)])
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertEqual([d[:2] for d in second.deliveries()], [(1, "addressee-gone"), (2, "addressee-gone")])

    def test_a_stop_while_the_journal_refuses_keeps_a_tail_for_another_start(self):
        world = self.world
        world.prog("chan-a").respond([REFUSE])
        server = server_answering(self, [401])
        self.mon.sink.cancel = poll_cancel(self, world, server)
        outage = self.r.JournalUnavailable("write_failed", self.mon.journal.path)
        with mock.patch.object(self.mon.journal, "delivery", side_effect=outage):
            with self.assertRaises(self.r.Stop) as ctx:
                self.mon.page([note(1)])
        tails = os.path.join(self.r.ledger_dir(world.env), self.r.TAILS_DIR_NAME)
        self.assertEqual(len(os.listdir(tails)), 1)
        self.assertTrue(os.path.exists(os.path.join(self.token_dir, "state.json")), "the state went before the tail")
        self.end(ctx.exception, sink=self.mon.sink)
        self.assertFalse(os.path.exists(os.path.join(self.token_dir, "state.json")))
        self.r.ledger_tails_at_start(world.env)   # the start of another monitor
        tail_lines = [l for l in self.mon.lines() if l.get("lineId")]
        self.assertEqual([(l["event"], l.get("status")) for l in tail_lines],
                         [("delivery", "access-lost"), ("summary", None)])
        self.assertNotIn("bodyPreview", tail_lines[0])
        self.assertEqual(os.listdir(tails), [])


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class CompetingStartTest(unittest.TestCase):
    """Review findings 19 and 40 of wave 7 (P-12: after the end the token
    directory is gone; a competing start of a live holder is busy, P-6: a
    lock that cannot be taken for an OS error is config_invalid, not busy)."""

    def setUp(self):
        self.world = World(self, [chan("A", "chan-a")])
        self.mon = self.world.monitor()
        self.r = self.world.runner
        self.held = self.r.PosixPlatform.acquire_runner_lock(self.mon.token, 0, self.world.env)
        self.addCleanup(self.held.release)
        self.state_lock = os.path.join(os.path.dirname(self.mon.token), "state.lock")

    def test_a_competitor_does_not_put_state_lock_back(self):
        if os.path.exists(self.state_lock):
            os.remove(self.state_lock)
        with self.assertRaises(self.r.LockBusy):
            self.r.take_runner_lock(self.mon.token, self.world.env)
        self.assertFalse(os.path.exists(self.state_lock), "a competing start created state.lock")

    def test_a_state_lock_that_cannot_be_opened_is_not_busy(self):
        if os.path.exists(self.state_lock):
            os.remove(self.state_lock)
        os.mkdir(self.state_lock, 0o700)
        with self.assertRaises(self.r.PlatformRefusal):
            self.r.take_runner_lock(self.mon.token, self.world.env)


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class JournalOutageThenStopTest(unittest.TestCase):
    """Review finding V5 of wave 7 (US-12, the access boundary and "a stop
    while the journal refuses": the lines go to the deferred tail; after a
    loss of access without the preview): a page the journal left undone
    still gets its outcomes when access is lost before it is polled again,
    and a line written later keeps the decision to drop the preview."""

    def test_a_stop_by_lost_access_closes_the_page_the_journal_left(self):
        world = World(self, [chan("A", "chan-a")])
        limit = single_fit(world)
        world = World(self, [chan("A", "chan-a", limit=limit), chan("B", "chan-b")])
        mon = world.monitor()
        page = {"notifications": [note(1), note(2)], "cursor": {"ts": "2026-01-01T00:00:09Z", "id": 9}}
        server = server_answering(self, [200, 401], page=page)
        executor = world.runner.Executor()
        self.addCleanup(executor.stop)
        outage = world.runner.JournalUnavailable("write_failed", mon.journal.path)
        with mock.patch.object(mon.journal, "delivery", side_effect=outage), \
                mock.patch.object(world.runner.time, "sleep", lambda s: None):
            with self.assertRaises(world.runner.Stop) as ctx:
                world.runner.work(executor, server.url(), TOKEN, ISSUE, "", mon.token, sink=mon.sink)
        self.assertEqual(ctx.exception.reason, "token_expired")
        # Event 1 went to the channel; event 2 waited.
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        world.runner.ledger_tails_at_start(world.env)
        tail = [l for l in mon.lines() if l.get("lineId")]
        self.assertEqual([(l["event"], l.get("notificationId"), l.get("status")) for l in tail],
                         [("delivery", 2, "access-lost"), ("delivery", 1, "queued"), ("summary", None, None)])
        self.assertFalse([l for l in tail if "bodyPreview" in l], "a line after the loss of access has the preview")

    def test_a_line_written_after_the_journal_is_back_keeps_the_dropped_preview(self):
        world = World(self, [chan("A", "chan-a")])
        insert, enter = fake_channel.two_step(world.bin)
        server = server_answering(self, [401])
        mon = world.monitor(cancel=poll_cancel(self, world, server))
        step = {"executor": {"type": "argv", "argv": ["insert"], "feed": "stdin", "timeoutSec": 2},
                "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}]}
        commit = {"executor": {"type": "argv", "argv": ["enter"], "feed": "stdin", "timeoutSec": 2},
                  "outcomes": [{"match": {"exitCode": 0}, "outcome": "turn-started"}]}
        mon.sink.drivers["A"] = world.runner.TwoStepDriver(world.reg.channel("A"), mon.spy, step, commit)

        def after(program):
            if program == "insert":
                world.clock.t += 2   # the poll before Enter answers 401
        mon.spy.after = after
        outage = world.runner.JournalUnavailable("write_failed", mon.journal.path)
        with mock.patch.object(mon.journal, "delivery", side_effect=outage):
            with self.assertRaises(world.runner.JournalUnavailable):
                mon.page([note(1)])
        self.assertEqual(enter.calls(), [])
        # The runner starts again with the same token; the journal is back.
        again = world.monitor()
        again.page([note(1)])
        self.assertEqual(again.deliveries(), [(1, "unknown", None, False)])


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class VisibilityLostTest(unittest.TestCase):
    """Row #62: the first poll answers 403 with a valid token:
    visibility_lost, the end of P-12, nothing sent."""

    def test_the_first_poll_answers_403(self):
        world = World(self, [chan("A", "chan-a")])
        mon = world.monitor()
        server = server_answering(self, [403])
        executor = world.runner.Executor()
        self.addCleanup(executor.stop)
        lock = world.runner.take_runner_lock(mon.token, world.env)
        with self.assertRaises(world.runner.Stop) as ctx:
            world.runner.work(executor, server.url(), TOKEN, ISSUE, "", mon.token, sink=mon.sink)
        lines = []
        world.runner.finish_client(ctx.exception, ISSUE, mon.token, sink=mon.sink, write=lines.append)
        lock.release()
        self.assertEqual((ctx.exception.reason, ctx.exception.status), ("visibility_lost", 403))
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertFalse(os.path.exists(mon.token))
        self.assertEqual(lines[0]["reason"], "visibility_lost")
        self.assertEqual(mon.lines()[-1]["reason"], "visibility_lost")


@unittest.skipUnless(os.name == "posix", "the POSIX runner")
class StopFateTest(unittest.TestCase):
    """Row #121: for every class of stop of client mode the machine event,
    reason and exit code, what the action names (a new mint, or the working
    command of the preflight by its number without its text), and the fate
    of the token, its files and its directory; the journal survives all."""

    SHARED = ("state.json", "exclude_session", "starter.py", "msg-left")

    def run_stop(self, make, under_lock=True):
        world = World(self, [chan("A", "chan-a")])
        r = world.runner
        mon = world.monitor()
        mon.journal.lifecycle("monitor_started")
        token_dir = os.path.dirname(mon.token)
        for name in self.SHARED:
            with open(os.path.join(token_dir, name), "w") as fh:
                fh.write("x")
        lines = []
        stop = make(r)
        if under_lock:
            lock = r.take_runner_lock(mon.token, world.env)
            code = r.finish_client(stop, ISSUE, mon.token, write=lines.append, preflight=3)
            lock.release()
        else:
            code = r.finish(stop, ISSUE, mon.token, write=lines.append)
        files = [n for n in self.SHARED if os.path.exists(os.path.join(token_dir, n))]
        kept = os.path.exists(mon.token)
        return lines[0], code, kept, files, os.path.isdir(token_dir), os.path.exists(mon.journal.path)

    def test_every_class_of_stop(self):
        mint = 'skein_get_monitor_script(id="%s")' % ISSUE
        cases = [
            ("root_closed", lambda r: r.Stop("root_closed", delete_token=True), True, "closed", 0, False, mint),
            ("stopped", lambda r: r.Stop("stopped"), True, "paused", 0, True, "working command of preflight 3"),
            ("internal_error", lambda r: r.Stop("internal_error"), True, "paused", 1, True,
             "working command of preflight 3"),
            ("token_expired", lambda r: r.access_stop("token_expired"), True, "closed", 0, False, mint),
            ("visibility_lost", lambda r: r.access_stop("visibility_lost"), True, "closed", 0, False, mint),
            ("addressee_gone", lambda r: r.Stop("addressee_gone", delete_token=True), True, "closed", 0, False, mint),
            ("delivered", lambda r: r.Stop("delivered", detail=3), True, "paused", 0, True, "preflight 3"),
            ("config_invalid", lambda r: r.Stop("config_invalid", detail="SKEIN_MONITOR_PREFLIGHT (stale)"), False,
             "closed", 1, True, "run the preflight again"),
            ("already_running", lambda r: r.Stop("already_running"), False, "duplicate", 0, True, "Do nothing"),
        ]
        for reason, make, under_lock, event, code, kept, action in cases:
            with self.subTest(reason=reason):
                line, got_code, got_kept, files, dir_left, journal = self.run_stop(make, under_lock)
                self.assertEqual((line["event"], line["reason"], got_code), (event, reason, code))
                self.assertIn(action, line["action"])
                self.assertNotIn("SKEIN_MONITOR_MODE", line["action"])
                self.assertEqual(got_kept, kept)
                if kept:
                    self.assertEqual(files, list(self.SHARED))
                    self.assertTrue(dir_left)
                else:
                    self.assertEqual(files, [])
                    self.assertFalse(dir_left)
                self.assertTrue(journal, "the journal did not survive")


if __name__ == "__main__":
    unittest.main()
