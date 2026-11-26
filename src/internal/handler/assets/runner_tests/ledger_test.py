#!/usr/bin/env python3
"""Tests of the ledger of deliveries (runner/ledger.py): matrix rows #45 to
#50 — one accepted delivery per addressee and event with one authoritative
outcome under the attempt order of P-11, the recovery of an attempt whose
owner died, the addressee rule ("Addressee and check", item 5), the
classes of outcomes (R-6), the limits and the rate window, the deferred
journal tail (P-12).

Two monitors are two Ledger objects of the same user directory; a monitor
that dies is a forked process killed with SIGKILL at a named step. The
clock of the ledger is a virtual one.

Run on its own: python3 internal/handler/assets/runner_tests/ledger_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import shutil
import signal
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

RUNNER = harness.load_runner()
SERVER = "https://skein.example"
QUEUED = "queued"
UNKNOWN = "unknown"
REFUSED = "refused-before-send"


def addr(channel, value, **qualifiers):
    return RUNNER.Address(channel, value, tuple(sorted(qualifiers.items())))


def ident(addresses=(), process=None, inherited=()):
    return RUNNER.Identity(process, list(addresses), list(inherited), None)


def ev(eid, session="sess-1"):
    return {"id": eid, "session": session}


class Clock(object):
    def __init__(self, t=1000000.0):
        self.t = t

    def __call__(self):
        return self.t


class Base(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-ledger-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.env = {"HOME": self.root, "XDG_STATE_HOME": os.path.join(self.root, "state")}
        self.clock = Clock()

    def ledger(self, monitor, identity, server=SERVER):
        return RUNNER.Ledger(server, monitor, identity, env=self.env, clock=self.clock)

    def attempts_dir(self):
        return os.path.join(RUNNER.ledger_dir(self.env), RUNNER.ATTEMPTS_DIR_NAME)

    def attempt_files(self):
        try:
            return os.listdir(self.attempts_dir())
        except OSError:
            return []

    def deliver(self, ledger, events, channel="c1", outcome=QUEUED):
        """One whole attempt: begin, a call, the outcome, the release.
        Returns the Decision; None calls when nothing was published."""
        d = ledger.begin([ev(e) if not isinstance(e, dict) else e for e in events], channel)
        if d.attempt is not None:
            d.attempt.note_call()
            d.attempt.finish(outcome)
            d.attempt.release()
        return d


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class AttemptLockTest(Base):
    """Review finding C5 of wave 7: an attempt lock is held only by a
    published attempt (P-11 step 6: the lock goes with its attempt). A
    begin that ends in an exception leaves no lock, no open descriptor and
    no file behind."""

    def open_fds(self):
        return set(os.listdir("/proc/self/fd")) if os.path.isdir("/proc/self/fd") else None

    def test_a_save_that_fails_releases_the_attempt_lock(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        led.begin([ev("warm")], "c1").attempt.release()
        before = self.open_fds()
        with mock.patch.object(RUNNER.Ledger, "_save", side_effect=RUNNER.LedgerUnavailable("write_failed")):
            for i in range(3):
                with self.assertRaises(RUNNER.LedgerUnavailable):
                    led.begin([ev("e%d" % i)], "c1")
        self.assertEqual(self.attempt_files(), [], "attempt files were left behind")
        if before is not None:
            self.assertEqual(self.open_fds(), before, "descriptors were left open")

    def test_a_full_ledger_releases_the_attempt_lock(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        with mock.patch.object(RUNNER, "LEDGER_MAX", 0):
            with self.assertRaises(RUNNER.LedgerFull):
                led.begin([ev("e1")], "c1")
        self.assertEqual(self.attempt_files(), [])


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class AttemptFileRaceTest(Base):
    """Review finding 34 of wave 7 (P-11: a "delivering" record is never
    visible without its attempt lock held): an attempt file removed by the
    sweep before it was locked is not the one an attempt publishes."""

    def test_an_attempt_file_removed_before_its_lock(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        led.begin([ev("warm")], "c1").attempt.release()
        real = RUNNER._ledger_fcntl.flock
        removed = []

        def flock(fd, op):
            path = os.readlink("/proc/self/fd/%d" % fd)
            if not removed and os.path.dirname(path) == os.path.realpath(self.attempts_dir()):
                os.remove(path)    # the sweep, between open and flock
                removed.append(path)
            return real(fd, op)

        with mock.patch.object(RUNNER._ledger_fcntl, "flock", flock):
            d = led.begin([ev("e1")], "c1")
        self.assertTrue(removed)
        self.assertTrue(os.path.exists(os.path.join(self.attempts_dir(), d.attempt.key)),
                        "the published attempt has no file")
        d.attempt.abandon()
        d.attempt.release()


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class RecordLimitTest(Base):
    """Review finding 17 of wave 7 (P-11: an addressee holds at most 10 000
    records younger than 24 h; a full ledger is unavailable): an outcome
    without a call keeps to the limit too, and old records make room."""

    def test_an_outcome_without_a_call_keeps_to_the_limit(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        with mock.patch.object(RUNNER, "LEDGER_MAX", 3):
            led.record(["u1", "u2", "u3"], "does-not-fit")
            with self.assertRaises(RUNNER.LedgerFull):
                led.record(["u4"], "does-not-fit")
            self.clock.t += 24 * 3600 + 1
            led.record(["u4"], "does-not-fit")
        self.assertEqual(led.outcome_of("u1"), (None, None), "a record older than 24 h was kept")
        self.assertEqual(led.outcome_of("u4")[0], "does-not-fit")


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class WholeLedgerBoundTest(Base):
    """Review finding 36 of wave 7 (P-11: a record lives at least 24 h):
    records of other addressees older than that and the empty addressees
    go too, so the ledger file does not grow without end."""

    def test_old_records_of_other_addressees_go(self):
        old = self.ledger("mon-old", ident([addr("orca", "t-old")]))
        self.deliver(old, ["x1", "x2"])
        self.clock.t += 24 * 3600 + 1
        now = self.ledger("mon-new", ident([addr("orca", "t-new")]))
        self.deliver(now, ["y1"])
        with open(os.path.join(RUNNER.ledger_dir(self.env), RUNNER._server_key(SERVER) + ".json")) as fh:
            doc = json.load(fh)
        ids = [eid for entry in doc["addressees"] for eid in entry["events"]]
        self.assertEqual(ids, ["y1"])
        self.assertEqual(len(doc["addressees"]), 1)


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class ChannelBudgetTest(Base):
    """Review finding 14 of wave 7 (INV-8: at most 4 calls of a channel
    that refuses before send, kept in the ledger so a restart continues
    them): the budget is the channel's - a refusal by the next channel
    does not give the first one its budget back."""

    def test_a_refusal_by_another_channel_keeps_the_first_budget(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        for _ in range(4):
            self.deliver(led, ["e1"], channel="c1", outcome=REFUSED)
            self.clock.t += 121
        self.deliver(led, ["e1"], channel="c2", outcome=REFUSED)
        self.clock.t += 121
        # The restart: the ledger says what c1 already spent.
        d = led.begin([ev("e1")], "c1")
        self.assertEqual(d.attempt.calls, 4)
        d.attempt.abandon()
        d.attempt.release()


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class _Series(Base):
    """Deliveries of a series (US-13) for the tests below."""

    def series(self, led):
        for i in range(30):
            d = led.begin([ev("s%d" % i, session="same")], "c1")
            d.attempt.note_call()
            d.attempt.finish(QUEUED)
            d.attempt.release()
            self.clock.t += 10

    def window_after_one(self, led, eid, session):
        d = led.begin([ev(eid, session=session)], "c1")
        d.attempt.note_call()
        d.attempt.finish(QUEUED)
        d.attempt.release()
        t0 = self.clock.t
        self.clock.t = t0 + 11
        d = led.begin([ev(eid + "-next", session=session)], "c1")
        if d.attempt is not None:
            d.attempt.abandon()
            d.attempt.release()
            return 10
        return d.wait_until - t0


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class SeriesEndTest(_Series):
    """Review finding 15 of wave 7 (US-13: a series is 30 deliveries of one
    session within 10 min; it ends with a delivery of another session or
    after 10 min without one; during it the window is 60 s, else 10 s)."""

    def test_a_pause_over_ten_minutes_ends_the_series(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        self.series(led)
        self.clock.t += 601
        self.assertEqual(self.window_after_one(led, "after-pause", "same"), 10)


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class WindowFromTransferTest(_Series):
    """Review finding 35 of wave 7 (P-11 rate: one delivery per addressee
    per 10 s, 60 s during a series of one session): the window is counted
    from the delivery, and the delivery that makes the series already
    gives 60 s."""

    def test_the_window_runs_from_the_transfer(self):
        a = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        b = self.ledger("mon-b", ident([addr("orca", "t-1")]))
        d = a.begin([ev("e1")], "c1")
        start = self.clock.t
        self.clock.t = start + 15    # a long idle wait before the transfer
        d.attempt.note_call()
        d.attempt.finish(QUEUED)
        d.attempt.release()
        self.clock.t = start + 16
        self.assertEqual(b.begin([ev("e2")], "c1").wait_until, start + 15 + 10)

    def test_the_thirtieth_delivery_gives_the_long_window(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        self.series(led)
        last = self.clock.t - 10
        d = led.begin([ev("next", session="same")], "c1")
        self.assertIsNone(d.attempt)
        self.assertEqual(d.wait_until, last + 60)

    def test_another_session_ends_the_series(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        self.series(led)
        self.clock.t += 50
        self.assertEqual(self.window_after_one(led, "other", "another"), 10)


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class BatchHistoryTest(Base):
    """Review finding 16 of wave 7 (INV-8 per event: at most 4 calls within
    the idle window from its own first call): an event without calls is
    not carried by an attempt with another event's four calls."""

    def test_a_new_event_is_not_carried_by_a_spent_budget(self):
        led = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        for _ in range(3):
            self.deliver(led, ["old"], outcome=REFUSED)
            self.clock.t += 121
        d = led.begin([ev("old"), ev("new")], "c1")
        self.assertEqual((d.published, d.attempt.calls), (["old"], 3))
        d.attempt.abandon()
        d.attempt.release()
        d = led.begin([ev("new"), ev("old")], "c1")
        self.assertEqual((d.published, d.attempt.calls), (["new"], 0))
        d.attempt.abandon()
        d.attempt.release()


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class MergedAttemptsTest(Base):
    """Review finding V6 of wave 7 (INV-1: one accepted delivery per
    addressee and event): two attempts published for two addressees that a
    third identity then merges into one - only the attempt that keeps the
    reservation may make its call."""

    def test_only_the_kept_reservation_calls(self):
        a = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        b = self.ledger("mon-b", ident([addr("codex", "th-9")]))
        da = a.begin([ev("e1")], "c1")
        db = b.begin([ev("e1")], "c2")
        self.assertIsNotNone(da.attempt)
        self.assertIsNotNone(db.attempt)
        # A third monitor serves both: the two entries are one addressee now.
        c = self.ledger("mon-c", ident([addr("orca", "t-1"), addr("codex", "th-9")]))
        self.clock.t += 11   # past the rate window of 10 s
        c.begin([ev("e2")], "c1").attempt.release()
        called = []
        for d in (da, db):
            try:
                d.attempt.note_call()
                called.append(d.attempt.ledger.monitor)
            except RUNNER.AttemptLost:
                d.attempt.abandon()
            finally:
                d.attempt.release()
        self.assertEqual(len(called), 1, called)


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class OneDeliveryTest(Base):
    """Row #45: attempts of two monitors of one addressee publish one; the
    loser frees its attempt; every competitor at every step of the owner
    sees one outcome and none writes "unknown" while the owner lives; the
    check at publish time is made under ledger.lock."""

    def setUp(self):
        Base.setUp(self)
        self.a = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        self.b = self.ledger("mon-b", ident([addr("orca", "t-1"), addr("codex", "th-9", home="/h")]))

    def gated(self, ledger):
        """Make ledger wait after step 1 (its attempt file is locked) until
        the returned event is set, before it takes ledger.lock."""
        gate = threading.Event()
        original = ledger._locked

        def locked():
            gate.wait(10)
            return original()
        ledger._locked = locked
        return gate

    def begin_in_thread(self, ledger, events, channel):
        out = {}

        def run():
            out["decision"] = ledger.begin([ev(e) for e in events], channel)
        t = threading.Thread(target=run)
        t.start()
        return t, out

    def wait_files(self, n):
        deadline = time.monotonic() + 10
        while len(self.attempt_files()) < n and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertEqual(len(self.attempt_files()), n)

    def test_two_prepared_attempts_publish_one_even_on_two_channels(self):
        ga, gb = self.gated(self.a), self.gated(self.b)
        ta, oa = self.begin_in_thread(self.a, ["e1"], "orca")
        tb, ob = self.begin_in_thread(self.b, ["e1"], "codex")
        self.wait_files(2)
        ga.set()
        gb.set()
        ta.join(10)
        tb.join(10)
        published = [o["decision"] for o in (oa, ob) if o["decision"].attempt is not None]
        lost = [o["decision"] for o in (oa, ob) if o["decision"].attempt is None]
        self.assertEqual(len(published), 1)
        self.assertEqual(lost[0].pending, ["e1"])
        self.assertEqual(len(self.attempt_files()), 1, "the loser did not free its attempt")
        winner = published[0].attempt
        winner.note_call()
        winner.finish(QUEUED)
        winner.release()
        self.assertEqual(self.attempt_files(), [])
        for led in (self.a, self.b):
            again = led.begin([ev("e1")], "orca")
            self.assertIsNone(again.attempt)
            self.assertEqual(again.closed, {"e1": QUEUED})

    def test_every_competitor_of_a_live_owner_sees_one_outcome(self):
        d = self.a.begin([ev("e1")], "orca")
        attempt = d.attempt
        seen = []
        seen.append(self.b.begin([ev("e1")], "codex"))      # published, before the call
        attempt.note_call()
        seen.append(self.b.begin([ev("e1")], "codex"))      # during the call
        attempt.finish(QUEUED)
        seen.append(self.b.begin([ev("e1")], "codex"))      # outcome written, attempt held
        attempt.release()
        seen.append(self.b.begin([ev("e1")], "codex"))      # released
        for i, dec in enumerate(seen):
            self.assertIsNone(dec.attempt, "step %d" % i)
            self.assertEqual(dec.recovered, [], "step %d" % i)
            self.assertNotIn(UNKNOWN, dec.closed.values(), "step %d" % i)
        self.assertEqual([dec.pending for dec in seen[:2]], [["e1"], ["e1"]])
        self.assertEqual([dec.closed for dec in seen[2:]], [{"e1": QUEUED}, {"e1": QUEUED}])

    def test_publish_rechecks_the_record_of_the_event(self):
        gate = self.gated(self.a)
        t, out = self.begin_in_thread(self.a, ["e1"], "orca")
        self.wait_files(1)
        self.deliver(self.b, ["e1"], "codex")
        gate.set()
        t.join(10)
        self.assertIsNone(out["decision"].attempt)
        self.assertEqual(out["decision"].closed, {"e1": QUEUED})
        self.assertEqual(self.attempt_files(), [])

    def test_publish_rechecks_the_union_of_addresses(self):
        x = self.ledger("mon-x", ident([addr("orca", "x")]))
        y = self.ledger("mon-y", ident([addr("codex", "y")]))
        both = self.ledger("mon-xy", ident([addr("orca", "x"), addr("codex", "y")]))
        self.deliver(x, ["warm"])  # x and y are two addressees so far
        self.deliver(y, ["warm2"])
        self.clock.t += 60
        gate = self.gated(x)
        t, out = self.begin_in_thread(x, ["e1"], "orca")
        self.wait_files(1)
        # A monitor that holds both addresses makes them one addressee and
        # delivers the event first.
        self.deliver(both, ["e1"], "codex")
        gate.set()
        t.join(10)
        self.assertIsNone(out["decision"].attempt)
        self.assertEqual(out["decision"].closed, {"e1": QUEUED})
        self.assertEqual(y.begin([ev("e1")], "codex").closed, {"e1": QUEUED})

    def test_publish_rechecks_the_rate_window(self):
        gate = self.gated(self.a)
        t, out = self.begin_in_thread(self.a, ["e1"], "orca")
        self.wait_files(1)
        self.deliver(self.b, ["e2"], "codex")
        gate.set()
        t.join(10)
        dec = out["decision"]
        self.assertIsNone(dec.attempt)
        self.assertEqual(dec.wait_until, self.clock.t + RUNNER.WINDOW_SEC)
        self.assertEqual(self.attempt_files(), [])


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class DeadOwnerTest(Base):
    """Row #46: the owner of an attempt dies at a step; another monitor of
    the addressee turns "delivering" into "unknown" exactly once, or takes
    the outcome already written. ledger.lock is not held across a call."""

    def owner_dies_at(self, step, identity):
        r, w = os.pipe()
        pid = os.fork()
        if pid == 0:
            try:
                os.close(r)
                led = RUNNER.Ledger(SERVER, "mon-dead", identity, env=self.env, clock=self.clock)
                d = led.begin([ev("e1")], "orca")
                if step >= 2:
                    d.attempt.note_call()
                if step >= 3:
                    d.attempt.finish(QUEUED)
                os.write(w, b"x")
                time.sleep(60)
            finally:
                os._exit(0)
        os.close(w)
        self.assertEqual(os.read(r, 1), b"x")
        os.close(r)
        os.kill(pid, signal.SIGKILL)
        os.waitpid(pid, 0)

    def test_death_at_each_step(self):
        identity = ident([addr("orca", "t-1")])
        for step, name in ((1, "published"), (2, "calling"), (3, "outcome written")):
            with self.subTest(step=name):
                self.root = tempfile.mkdtemp(prefix="skein-ledger-")
                self.addCleanup(shutil.rmtree, self.root, True)
                self.env = {"HOME": self.root, "XDG_STATE_HOME": os.path.join(self.root, "state")}
                self.owner_dies_at(step, identity)
                other = self.ledger("mon-live", identity)
                first = other.begin([ev("e1")], "codex")
                second = other.begin([ev("e1")], "codex")
                self.assertIsNone(first.attempt)
                self.assertIsNone(second.attempt)
                if step < 3:
                    self.assertEqual(first.recovered, ["e1"])
                    self.assertEqual(first.closed, {"e1": UNKNOWN})
                else:
                    self.assertEqual(first.recovered, [])
                    self.assertEqual(first.closed, {"e1": QUEUED})
                self.assertEqual(second.recovered, [])
                self.assertEqual(second.closed, first.closed)

    def test_another_addressee_is_served_while_a_channel_hangs(self):
        hung = self.ledger("mon-a", ident([addr("orca", "t-1")]))
        d = hung.begin([ev("e1")], "orca")
        d.attempt.note_call()   # the call hangs: nothing more from this owner
        self.addCleanup(d.attempt.release)
        other = self.ledger("mon-b", ident([addr("orca", "t-2")]))
        done = {}

        def run():
            done["d"] = self.deliver(other, ["e1"])
        t = threading.Thread(target=run)
        t.start()
        t.join(10)
        self.assertFalse(t.is_alive(), "ledger.lock is held across a call")
        self.assertEqual(done["d"].published, ["e1"])


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class AddresseeTest(Base):
    """Row #47: one addressee by the agent process or by one address with
    all its qualifiers; otherwise two; an inherited address does not join;
    two servers keep two ledgers."""

    def both_publish(self, ia, ib, server_b=SERVER):
        a = self.ledger("mon-a", ia)
        b = self.ledger("mon-b", ib, server=server_b)
        da = self.deliver(a, ["e1"])
        db = b.begin([ev("e1")], "c2")
        return da.published == ["e1"], db.attempt is not None

    def test_one_addressee(self):
        cases = {
            "same agent process": (ident([addr("orca", "x")], process=(41, "s41")),
                                   ident([addr("codex", "y")], process=(41, "s41"))),
            "one address with all qualifiers": (ident([addr("codex", "th", home="/h1"), addr("orca", "o")]),
                                                ident([addr("codex", "th", home="/h1")])),
        }
        for name, (ia, ib) in cases.items():
            with self.subTest(case=name):
                self.setUp()
                self.assertEqual(self.both_publish(ia, ib), (True, False))

    def test_two_addressees(self):
        cases = {
            "one thread under two CODEX_HOME": (ident([addr("codex", "th", CODEX_HOME="/h1")]),
                                                ident([addr("codex", "th", CODEX_HOME="/h2")])),
            "%1 in two multiplexer sessions": (ident([addr("tmux", "%1", session="s1")]),
                                               ident([addr("tmux", "%1", session="s2")])),
            "an inherited address": (ident([addr("codex", "a")], inherited=["orca"]),
                                     ident([addr("codex", "b")], inherited=["orca"])),
        }
        for name, (ia, ib) in cases.items():
            with self.subTest(case=name):
                self.setUp()
                self.assertEqual(self.both_publish(ia, ib), (True, True))

    def test_two_servers_keep_two_deliveries(self):
        same = ident([addr("orca", "x")])
        self.assertEqual(self.both_publish(same, same, server_b="https://other.example"), (True, True))


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class OutcomeClassTest(Base):
    """Row #48: an outcome for the addressee closes the event for every
    monitor; an outcome for the monitor is marked with it and does not stop
    another monitor; at a restart the ledger, not the state, gives the own
    outcome and the calls already made."""

    def setUp(self):
        Base.setUp(self)
        self.identity = ident([addr("orca", "t-1")])
        self.a = self.ledger("mon-a", self.identity)
        self.b = self.ledger("mon-b", self.identity)

    def test_an_outcome_for_the_addressee_closes_the_event_for_every_monitor(self):
        for outcome in ("queued", "input-written", "input-without-receipt", "turn-started", "unknown",
                        "addressee-gone"):
            with self.subTest(outcome=outcome):
                eid = "e-" + outcome
                self.deliver(self.a, [eid], outcome=outcome)
                self.clock.t += 61
                d = self.b.begin([ev(eid)], "c2")
                self.assertIsNone(d.attempt)
                self.assertEqual(d.closed, {eid: outcome})

    def test_an_outcome_for_the_monitor_does_not_stop_another_monitor(self):
        for outcome in ("refused-before-send", "does-not-fit", "access-lost", "own-echo"):
            with self.subTest(outcome=outcome):
                eid = "e-" + outcome
                if outcome == REFUSED:
                    self.deliver(self.a, [eid], outcome=outcome)
                else:
                    self.a.record([eid], outcome, "c1")
                self.clock.t += 61
                d = self.b.begin([ev(eid)], "c2")
                self.assertEqual(d.published, [eid])
                d.attempt.abandon()
                d.attempt.release()
                if outcome != REFUSED:
                    own = self.a.begin([ev(eid)], "c1")
                    self.assertIsNone(own.attempt)
                    self.assertEqual(own.own[eid]["outcome"], outcome)

    def test_a_restart_reads_its_own_refusal_and_calls_from_the_ledger(self):
        d = self.a.begin([ev("e1")], "c1")
        d.attempt.note_call()
        first = self.clock.t
        self.clock.t += 5
        d.attempt.note_call()
        last = self.clock.t
        d.attempt.finish(REFUSED)
        d.attempt.release()
        # The state never heard of it: a new process of the same monitor.
        restarted = self.ledger("mon-a", self.identity)
        self.assertEqual(restarted.outcome_of("e1")[0], REFUSED)
        self.clock.t += 30
        again = restarted.begin([ev("e1")], "c1")
        self.assertEqual(again.published, ["e1"])
        self.assertEqual(again.attempt.calls, 2)
        self.assertEqual(again.attempt.last_call, last)
        self.assertEqual(again.attempt.first_call, first)
        again.attempt.abandon()
        again.attempt.release()


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class LimitsAndRateTest(Base):
    """Row #49: 10 000 live records per addressee, none younger than 24 h
    pushed out; a ledger that cannot be used sends nothing; one rate window
    per addressee shared by its monitors; a batch takes the window; the
    window of a series of one session is 60 s."""

    def setUp(self):
        Base.setUp(self)
        self.identity = ident([addr("orca", "t-1")])
        self.a = self.ledger("mon-a", self.identity)
        self.b = self.ledger("mon-b", self.identity)

    def test_a_full_ledger_refuses_and_keeps_young_records(self):
        self.a.record(["old-%d" % i for i in range(10000)], "does-not-fit", "c1")
        with self.assertRaises(RUNNER.LedgerFull):
            self.a.begin([ev("new")], "c1")
        self.assertEqual(self.a.outcome_of("old-0")[0], "does-not-fit")
        self.clock.t += 24 * 3600 + 1
        self.assertEqual(self.a.begin([ev("new")], "c1").published, ["new"])

    def test_a_ledger_that_cannot_be_used_refuses(self):
        path = os.path.join(RUNNER.ledger_dir(self.env), RUNNER._server_key(SERVER) + ".json")
        self.deliver(self.a, ["e0"])
        os.remove(path)
        os.mkdir(path)
        with self.assertRaises(RUNNER.LedgerUnavailable):
            self.a.begin([ev("e1")], "c1")

    def test_one_window_per_addressee_shared_by_its_monitors(self):
        start = self.clock.t
        batch = self.deliver(self.a, ["e1", "e2", "e3"])
        self.assertEqual(batch.published, ["e1", "e2", "e3"])
        self.clock.t = start + 5
        for led, eid in ((self.b, "e4"), (self.a, "e5")):
            d = led.begin([ev(eid)], "c1")
            self.assertIsNone(d.attempt)
            self.assertEqual(d.wait_until, start + RUNNER.WINDOW_SEC)
        other = self.ledger("mon-c", ident([addr("orca", "t-2")]))
        self.assertEqual(other.begin([ev("e4")], "c1").published, ["e4"])
        self.clock.t = start + RUNNER.WINDOW_SEC
        d = self.b.begin([ev("e4")], "c1")
        self.assertEqual(d.published, ["e4"], "an event of the other monitor was lost")

    def test_a_refusal_gives_the_window_back(self):
        self.deliver(self.a, ["e1"], outcome=REFUSED)
        self.assertEqual(self.b.begin([ev("e2")], "c1").published, ["e2"])

    def test_a_series_of_one_session_widens_the_window(self):
        # US-13: 30 deliveries of one session within 10 min make a series;
        # the window is then 60 s instead of 10 s, and the series is said
        # once (single_session_burst).
        burst = []
        for i in range(30):
            d = self.a.begin([ev("s%d" % i, session="same")], "c1")
            self.assertEqual(d.published, ["s%d" % i], i)
            d.attempt.note_call()
            burst.append(d.attempt.finish(QUEUED))
            d.attempt.release()
            self.clock.t += 10
        self.assertEqual(burst.count(True), 1)
        self.assertTrue(burst[-1])
        last = self.clock.t - 10
        d = self.a.begin([ev("next", session="same")], "c1")
        self.assertEqual(d.wait_until, last + 60)
        self.clock.t = last + 60
        d = self.a.begin([ev("next", session="same")], "c1")
        t0 = self.clock.t
        d.attempt.note_call()
        d.attempt.finish(QUEUED)
        d.attempt.release()
        self.clock.t = t0 + 30
        waiting = self.b.begin([ev("later")], "c1")
        self.assertEqual(waiting.wait_until, t0 + 60)


@unittest.skipUnless(os.name == "posix", "the POSIX ledger")
class TailTest(Base):
    """Row #50: a deferred tail goes into the journal of its own monitor
    after that monitor stopped; a crash between the append and the removal
    repeats without double lines and a cut last line is restored; a ledger
    directory that takes nothing keeps neither lines nor summary; another
    monitor's start does not depend on a tail; the start appends tails."""

    def setUp(self):
        Base.setUp(self)
        self.token_dir = os.path.join(self.root, "skein-mon-stopped")
        os.mkdir(self.token_dir, 0o700)
        self.journal = RUNNER.Journal(os.path.join(self.token_dir, "token"), "sk-50", env=self.env)

    def lines(self, n=2):
        out = [self.journal.render({"event": "delivery", "status": "access-lost", "notificationId": i},
                                   line_id="line-%d" % i) for i in range(n)]
        out.append(self.journal.render({"event": "summary", "summary": "stopped_journal_unavailable"},
                                       line_id="line-summary"))
        return out

    def journal_ids(self):
        with open(self.journal.path) as fh:
            return [json.loads(line).get("lineId") for line in fh]

    def tails(self):
        try:
            return os.listdir(os.path.join(RUNNER.ledger_dir(self.env), RUNNER.TAILS_DIR_NAME))
        except OSError:
            return []

    def test_the_tail_goes_into_its_own_journal_and_is_removed(self):
        RUNNER.defer_lines(self.journal.path, self.lines(), self.env)
        self.assertFalse(os.path.exists(self.journal.path))
        self.assertEqual(RUNNER.append_tails(self.env), 1)
        self.assertEqual(self.journal_ids(), ["line-0", "line-1", "line-summary"])
        self.assertEqual(self.tails(), [])

    def test_a_repeat_after_a_crash_writes_no_line_twice_and_restores_a_cut_line(self):
        lines = self.lines()
        RUNNER.defer_lines(self.journal.path, lines, self.env)
        os.makedirs(os.path.dirname(self.journal.path), 0o700, exist_ok=True)
        fd = os.open(self.journal.path, os.O_WRONLY | os.O_CREAT, 0o600)
        os.write(fd, (lines[0] + "\n" + lines[1][:10]).encode("utf-8"))
        os.close(fd)
        RUNNER.append_tails(self.env)
        self.assertEqual(self.journal_ids(), ["line-0", "line-1", "line-summary"])
        self.assertEqual(self.tails(), [])

    def test_a_ledger_directory_that_takes_nothing_keeps_neither_lines_nor_summary(self):
        state = self.env["XDG_STATE_HOME"]
        os.makedirs(os.path.join(state, RUNNER.USER_DIR_NAME), 0o700)
        with open(RUNNER.ledger_dir(self.env), "w") as fh:
            fh.write("not a directory")
        with self.assertRaises(RUNNER.LedgerUnavailable):
            RUNNER.defer_lines(self.journal.path, self.lines(), self.env)
        self.assertFalse(os.path.exists(self.journal.path))

    def test_a_tail_whose_journal_refuses_does_not_stop_another_start(self):
        RUNNER.defer_lines(self.journal.path, self.lines(), self.env)
        jdir = os.path.dirname(self.journal.path)
        with open(jdir, "w") as fh:
            fh.write("not a directory")
        RUNNER.ledger_tails_at_start(self.env)  # returns, the start goes on
        self.assertEqual(len(self.tails()), 1)

    def test_the_start_of_a_monitor_appends_the_tails(self):
        RUNNER.defer_lines(self.journal.path, self.lines(), self.env)
        registry = {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
                    "agents": [], "channels": [], "shells": {"source": "t", "names": ["sh"]},
                    "zavodEngines": {}}
        import base64
        runner = harness.load_runner(registry=base64.b64encode(json.dumps(registry).encode()).decode())
        reg = runner.load_registry()
        token_dir = os.path.join(self.root, "skein-mon-new")
        os.mkdir(token_dir, 0o700)
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write("skm_" + "N" * 43)
        os.chmod(token, 0o600)
        env = dict(self.env, PATH=os.environ.get("PATH", ""), SKEIN_PUBLIC_URL="http://127.0.0.1:9",
                   SKEIN_ISSUE="sk-50", SKEIN_TOKEN_FILE=token, SKEIN_CLIENT="alpha", SKEIN_PLATFORM="posix",
                   SKEIN_MONITOR_CHANNEL="", SKEIN_MONITOR_BLIND="", SKEIN_MONITOR_MODE="run")
        settings = runner.read_settings(env, reg, mode="client")
        line = runner.preflight(token, env, reg, settings, detector=runner.Detector(),
                                placement=runner.place([], reg, settings.record))
        env["SKEIN_MONITOR_PREFLIGHT"] = str(line["preflightId"])
        held = runner.WorkingStart()
        with mock.patch.object(runner, "LOADER_MODE", "client"):
            held.begin(token, env, settings, "sk-50")
        self.addCleanup(held.release)
        self.assertEqual(self.journal_ids(), ["line-0", "line-1", "line-summary"])
        self.assertEqual(self.tails(), [])


if __name__ == "__main__":
    unittest.main()
