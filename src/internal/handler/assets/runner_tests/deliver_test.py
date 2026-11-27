#!/usr/bin/env python3
"""Tests of the delivery pipeline of a detached monitor (runner/deliver.py,
runner/classify.py with the ledger and the executor): matrix rows #51 to
#57, #118, #119, #120, #134 and #152.

A monitor here is the DeliverySink of the working runner over the real
ledger, journal and argv executor; its channels are fake programs on PATH
(fake_channel.py) answering by a script, and its clock is a virtual one, so
the retry schedule of INV-8 is read from the times of the calls. A spy
around the executor records each call with the virtual time and can make
the runner "die" at a named point.

Run on its own: python3 internal/handler/assets/runner_tests/deliver_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_channel  # noqa: E402
import harness  # noqa: E402

TOKEN = "skm_" + "D" * 43
SERVER = "https://skein.example"
ISSUE = "sk-9"
CURSOR = {"ts": "2026-01-01T00:00:00Z", "id": 7}
QUEUED = {"exit": 0, "stdout": {"state": "queued"}}
REFUSE = {"exit": 3}
TABLE = [{"match": {"exitCode": 0, "json": {"state": "queued"}}, "outcome": "queued"},
         {"match": {"exitCode": 0, "json": {"state": "written"}}, "outcome": "input-written"},
         {"match": {"exitCode": 3}, "outcome": "refused-before-send"},
         {"match": {"exitCode": 4}, "outcome": "turn-started"},
         {"match": {"exitCode": 5, "json": {"error": "session_not_found"}}, "outcome": "refused-before-send"}]
ADDRESS = {"process": None, "addresses": [{"channel": "A", "value": "addr-1", "qualifiers": []}],
           "inherited": [], "terminal": None}


class Crash(BaseException):
    """The runner dies here (nothing catches it on the way out)."""


class Clock(object):
    def __init__(self):
        self.t = 1000000.0
        self.on_sleep = None

    def now(self):
        return self.t

    def sleep(self, sec):
        if self.on_sleep is not None:
            self.on_sleep()
        self.t += max(sec, 0)


class FakeCancel(object):
    """The cancel signal: answers in turn, then None."""

    def __init__(self, answers=()):
        self.answers = list(answers)

    def check(self):
        return self.answers.pop(0) if self.answers else None

    def known(self):
        # A stop answer received earlier: this fake answers only when asked
        # with check(), so it knows none in between.
        return None

    def may_send(self):
        return True


class FlagCancel(object):
    """The cancel signal as the test sets it: a stop answer, or a poll that
    has gone stale."""

    def __init__(self):
        self.stop = None
        self.stale = False
        # One stale answer, then a fresh poll.
        self.stale_once = False

    def check(self):
        return self.stop

    def known(self):
        return self.stop

    def may_send(self):
        if self.stale_once:
            self.stale_once = False
            return False
        return self.stop is None and not self.stale


class Spy(object):
    """The argv executor with a record of every call (program, virtual
    time, text) and hooks before and after the real call."""

    def __init__(self, inner, clock):
        self.inner = inner
        self.clock = clock
        self.secrets = inner.secrets
        self.calls = []
        self.before = None
        self.after = None

    def call(self, spec, text, timeout):
        program = spec["argv"][0]
        if self.before is not None:
            self.before(program)
        self.calls.append((program, self.clock.now(), text))
        result = self.inner.call(spec, text, timeout)
        if self.after is not None:
            self.after(program)
        return result

    def times(self, program):
        return [t for p, t, _ in self.calls if p == program]


def chan(cid, program, kind="addressed", process="none", limit=None, blind=False, table=TABLE):
    rec = {"id": cid, "about": cid, "source": "test", "step": 7 if blind else 3, "order": 1, "kind": kind,
           "blind": blind, "detect": {},
           "executor": {"type": "argv", "argv": [program], "feed": "stdin", "timeoutSec": 2},
           "idle": {"type": "none"}, "outcomes": table, "addressee": {"process": process},
           "binaries": {"linux": [program], "macos": [program]}, "probeUndefined": True}
    if limit:
        rec["limits"] = {"line": {"max": limit, "unit": "chars"}}
    return rec


def note(i, session="s-1", preview="hello there"):
    return {"id": i, "type": "comment_added", "entity": {"type": "issue", "id": "sk-9"},
            "actor": {"username": "bob"}, "actorSessionId": session, "commentId": "c%d" % i,
            "bodyPreview": preview, "truncated": False}


class Monitor(object):
    def __init__(self, world, sink, spy, journal, token):
        self.world = world
        self.sink = sink
        self.spy = spy
        self.journal = journal
        self.token = token

    def page(self, notes, cursor=CURSOR):
        return self.sink.page(cursor, notes)

    def lines(self):
        try:
            with open(self.journal.path) as fh:
                return [json.loads(line) for line in fh if line.strip()]
        except OSError:
            return []

    def deliveries(self):
        return [(l["notificationId"], l["status"], l.get("channel"), "bodyPreview" in l)
                for l in self.lines() if l.get("event") == "delivery"]

    def degraded(self, cause):
        return [l for l in self.lines() if l.get("event") == "degraded" and l.get("cause") == cause]

    def recovered(self):
        return [l for l in self.lines() if l.get("event") == "recovered"]


class World(object):
    """A user directory, a bin directory with the channel programs, a
    virtual clock and the runner loaded with these channels."""

    def __init__(self, test, channels):
        self.test = test
        self.root = tempfile.mkdtemp(prefix="skein-deliver-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin, 0o700)
        self.env = fake_channel.with_path({"PATH": os.environ.get("PATH", os.defpath), "HOME": self.root,
                                           "XDG_STATE_HOME": os.path.join(self.root, "state")}, self.bin)
        self.clock = Clock()
        self.progs = {}
        for c in channels:
            program = c["executor"]["argv"][0]
            if program not in self.progs:
                self.progs[program] = fake_channel.FakeProgram(self.bin, program)
                self.progs[program].respond([QUEUED])
        registry = {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
                    "agents": [], "channels": channels, "shells": {"source": "t", "names": ["sh"]},
                    "zavodEngines": {}}
        self.runner = harness.load_runner(registry=base64.b64encode(json.dumps(registry).encode()).decode())
        self.reg = self.runner.load_registry()

    def prog(self, name):
        return self.progs[name]

    def monitor(self, name="tok-a", channel="A", fallbacks=("B",), identity=None, cancel=None, drivers=None,
                idle=None):
        token_dir = os.path.join(self.root, name)
        if not os.path.isdir(token_dir):
            os.mkdir(token_dir, 0o700)
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(token, 0o600)
        identity = ADDRESS if identity is None else identity
        doc = {"method": "detached", "channel": channel, "fallbacks": list(fallbacks), "identity": identity,
               "preflightId": 1}
        r = self.runner
        journal = r.Journal(token, ISSUE, secrets=[TOKEN], env=self.env)
        ledger = r.Ledger(SERVER, name, r.identity_from_doc(identity), env=self.env, clock=self.clock.now)
        spy = Spy(r.ArgvExecutor(token, self.env, [TOKEN]), self.clock)
        sink = r.DeliverySink(journal, token, doc, self.reg, self.env, ledger, spy, ISSUE,
                              cancel=cancel or FakeCancel(), clock=self.clock, drivers=drivers,
                              idle_window_sec=idle)
        return Monitor(self, sink, spy, journal, token)


def two_channels(a_kw=None, b_kw=None):
    return [chan("A", "chan-a", **(a_kw or {})), chan("B", "chan-b", **(b_kw or {}))]


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class ClassificationAndBudgetTest(unittest.TestCase):
    """Row #51: every sign of the closed table gives exactly one outcome,
    anything else (an unfamiliar or corrupt answer, a timeout after the
    send) is unknown; a channel is called again only after a proven refusal
    before send, after 5, 30 and 120 s, at most 4 times within the idle
    window; then the next non-blind channel of the preflight, a forced one
    only the journal."""

    def test_each_sign_gives_one_outcome(self):
        cases = [("queued", QUEUED, "queued"),
                 ("input written", {"exit": 0, "stdout": {"state": "written"}}, "input-written"),
                 ("turn started", {"exit": 4}, "turn-started"),
                 ("unfamiliar code", {"exit": 9}, "unknown"),
                 ("corrupt answer", {"exit": 0, "stdout": "{not json"}, "unknown"),
                 ("timeout after the send", {"hang": True}, "unknown")]
        for name, answer, outcome in cases:
            with self.subTest(case=name):
                world = World(self, two_channels())
                world.prog("chan-a").respond([answer])
                mon = world.monitor()
                mon.page([note(1)])
                self.assertEqual(len(world.prog("chan-a").calls()), 1)
                self.assertEqual(world.prog("chan-b").calls(), [])
                self.assertEqual([d[:3] for d in mon.deliveries()], [(1, outcome, "A")])

    def test_refusals_follow_the_schedule_then_the_next_channel(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([REFUSE])
        mon = world.monitor()
        t0 = world.clock.now()
        mon.page([note(1)])
        self.assertEqual(mon.spy.times("chan-a"), [t0, t0 + 5, t0 + 35, t0 + 155])
        self.assertEqual(len(world.prog("chan-b").calls()), 1)
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "B")])

    def test_the_budget_ends_with_the_idle_window(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([REFUSE])
        mon = world.monitor(idle=60)
        t0 = world.clock.now()
        mon.page([note(1)])
        self.assertEqual(mon.spy.times("chan-a"), [t0, t0 + 5, t0 + 35])
        self.assertEqual(len(world.prog("chan-b").calls()), 1)

    def test_a_forced_channel_has_only_the_journal_after_its_budget(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([REFUSE])
        mon = world.monitor(fallbacks=())
        mon.page([note(1)])
        self.assertEqual(len(world.prog("chan-a").calls()), 4)
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "refused-before-send")])

    def test_a_blind_channel_is_never_a_fallback(self):
        world = World(self, two_channels() + [chan("blind", "chan-blind", kind="terminal", blind=True)])
        world.prog("chan-a").respond([REFUSE])
        mon = world.monitor(fallbacks=("blind", "B"))
        mon.page([note(1)])
        self.assertEqual(world.prog("chan-blind").calls(), [])
        self.assertEqual(len(world.prog("chan-b").calls()), 1)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class UnknownTest(unittest.TestCase):
    """Row #52: unknown is not repeated and does not move to another
    channel; degraded delivery_unknown; a second monitor of the addressee
    does not deliver the event."""

    def test_unknown_is_final_for_every_monitor_of_the_addressee(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([{"exit": 9}])
        first = world.monitor("tok-a")
        first.page([note(1)])
        second = world.monitor("tok-b")
        world.clock.t += 60
        second.page([note(1)])
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual([d[:2] for d in first.deliveries()], [(1, "unknown")])
        self.assertEqual([d[:2] for d in second.deliveries()], [(1, "unknown")])
        self.assertEqual(len(first.degraded("delivery_unknown")), 1)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class CrashTest(unittest.TestCase):
    """Row #53: the runner dies before the call, after the channel took the
    text, after the ledger, after the state; the restart gives no second
    accepted delivery and every event an outcome in the journal; the cursor
    moves per page only after all its outcomes."""

    def crash_then_restart(self, arm, outcome, calls):
        world = World(self, two_channels())
        first = world.monitor()
        undo = arm(world, first)
        with self.assertRaises(Crash):
            first.page([note(1)])
        undo()
        world.clock.t += 60
        restarted = world.monitor()
        restarted.page([note(1)])
        self.assertEqual(len(world.prog("chan-a").calls()), calls)
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual([d[:2] for d in restarted.deliveries()][-1], (1, outcome))
        self.assertEqual(world.runner.load_page_outcomes(restarted.token, CURSOR)[0], {"1": outcome})

    def test_before_the_call(self):
        def arm(world, mon):
            def die(program):
                raise Crash()
            mon.spy.before = die
            return lambda: None
        self.crash_then_restart(arm, "unknown", 0)

    def test_after_the_channel_took_the_text(self):
        def arm(world, mon):
            def die(program):
                raise Crash()
            mon.spy.after = die
            return lambda: None
        self.crash_then_restart(arm, "unknown", 1)

    def test_after_the_ledger_before_the_state(self):
        def arm(world, mon):
            patch = mock.patch.object(world.runner, "save_page_outcomes", side_effect=Crash())
            patch.start()
            return patch.stop
        self.crash_then_restart(arm, "queued", 1)

    def test_after_the_state_before_the_journal(self):
        def arm(world, mon):
            patch = mock.patch.object(mon.journal, "delivery", side_effect=Crash())
            patch.start()
            return patch.stop
        self.crash_then_restart(arm, "queued", 1)

    def test_the_cursor_moves_per_page_after_all_its_outcomes(self):
        world = World(self, two_channels())
        mon = world.monitor()
        pages = {None: {"notifications": [note(1), note(2)], "cursor": {"ts": "2026-01-01T00:00:01Z", "id": 2}},
                 "2": {"notifications": [note(3)], "cursor": {"ts": "2026-01-01T00:00:02Z", "id": 3}}}

        def respond(h, qs):
            since = (qs.get("since_id") or [None])[0]
            if since in pages:
                h.send_json(200, pages[since])
            else:
                h.send_json(401, {})
        server = harness.ScriptedServer(respond)
        self.addCleanup(server.stop)
        executor = world.runner.Executor()
        self.addCleanup(executor.stop)
        with self.assertRaises(world.runner.Stop) as ctx:
            world.runner.work(executor, server.url(), TOKEN, ISSUE, "", mon.token, sink=mon.sink)
        self.assertEqual(ctx.exception.reason, "token_expired")
        self.assertEqual([(q.get("since_id") or [None])[0] for q in server.requests], [None, "2", "3"])
        self.assertEqual(sorted(d[:2] for d in mon.deliveries()), [(1, "queued"), (2, "queued"), (3, "queued")])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class AddresseeCheckTest(unittest.TestCase):
    """Row #54: a terminal channel whose agent process is not the one of
    the preflight before the first transfer — "addressee changed", the
    journal from then on; the same process not in the foreground — a
    refusal before send; an addressed channel does not check the process;
    "session not found" before acceptance is a refusal by the table; a
    minimal form over the limit of the first channel only goes to the next,
    over every limit "does not fit" with the full form in the journal."""

    def dead_process(self):
        proc = subprocess.Popen(["sleep", "30"])
        info = harness_info(proc.pid)
        proc.kill()
        proc.wait()
        return [proc.pid, info.start]

    def test_a_terminal_channel_with_another_agent_process(self):
        world = World(self, two_channels({"kind": "terminal", "process": "alive"}))
        identity = dict(ADDRESS, process=self.dead_process())
        mon = world.monitor(identity=identity)
        mon.page([note(1)])
        mon.page([note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown"), (2, "journal")])
        self.assertEqual(len(mon.degraded("addressee_changed")), 1)

    def test_the_same_process_not_in_the_foreground_is_a_refusal(self):
        world = World(self, two_channels({"kind": "terminal", "process": "foreground"}))
        identity = dict(ADDRESS, process=[4242, "start-1"])
        info = world.runner.ProcessInfo(4242, 1, "start-1", 50, 60, "agent")
        with mock.patch.object(world.runner.PosixPlatform, "process_info", staticmethod(lambda pid: info)):
            mon = world.monitor(identity=identity)
            mon.page([note(1)])
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual(len(world.prog("chan-b").calls()), 1)
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "B")])

    def test_an_addressed_channel_does_not_check_the_process(self):
        world = World(self, two_channels())
        mon = world.monitor(identity=dict(ADDRESS, process=self.dead_process()))
        mon.page([note(1)])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "A")])

    def test_session_not_found_before_acceptance_is_a_refusal(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([{"exit": 5, "stdout": {"error": "session_not_found"}}])
        mon = world.monitor()
        mon.page([note(1)])
        self.assertEqual(len(world.prog("chan-a").calls()), 4)
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "B")])

    def test_a_minimal_form_over_the_first_limit_goes_to_the_next_channel(self):
        world = World(self, two_channels({"limit": 30}))
        mon = world.monitor()
        mon.page([note(1)])
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "B")])

    def test_over_every_limit_the_event_does_not_fit(self):
        world = World(self, two_channels({"limit": 30}, {"limit": 30}))
        mon = world.monitor()
        mon.page([note(1)])
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual(mon.deliveries(), [(1, "does-not-fit", "B", True)])


def harness_info(pid):
    runner = harness.load_runner()
    return runner.PosixPlatform.process_info(pid)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class TwoStepCancelTest(unittest.TestCase):
    """Row #55: a cancel signal after a confirmed insert of a two-step
    channel: no Enter, no repeat, no other channel; "unknown" for the
    addressee without the preview; a second monitor does not deliver it."""

    def test_no_enter_after_a_cancel(self):
        world = World(self, two_channels())
        insert, enter = fake_channel.two_step(world.bin)
        r = world.runner
        step = {"executor": {"type": "argv", "argv": ["insert"], "feed": "stdin", "timeoutSec": 2},
                "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}]}
        commit = {"executor": {"type": "argv", "argv": ["enter"], "feed": "stdin", "timeoutSec": 2},
                  "outcomes": [{"match": {"exitCode": 0}, "outcome": "turn-started"}]}
        cancel = FlagCancel()
        first = world.monitor(cancel=cancel)
        first.sink.drivers["A"] = r.TwoStepDriver(world.reg.channel("A"), first.spy, step, commit)

        def after(program):
            if program == "insert":
                cancel.stop = "token_expired"
        first.spy.after = after
        # The stop answer that withheld Enter stops the monitor with its
        # access stop once the event has its outcome (US-12).
        with self.assertRaises(r.Stop) as caught:
            first.page([note(1)])
        self.assertEqual(caught.exception.reason, "token_expired")
        self.assertEqual(len(insert.calls()), 1)
        self.assertEqual(enter.calls(), [])
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual(first.deliveries(), [(1, "unknown", "A", False)])
        second = world.monitor("tok-b")
        world.clock.t += 60
        second.page([note(1)])
        self.assertEqual(len(insert.calls()), 1)
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual([d[:2] for d in second.deliveries()], [(1, "unknown")])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class AccessBoundaryAtHandOverTest(unittest.TestCase):
    """Review finding C1 of wave 7 (US-12, the access boundary: a send is
    allowed only when the last successful poll is at most two poll
    intervals old and no answer since said stop; 401/403 win over waiting
    events): the boundary holds right before the text is handed over and
    before Enter, not only before the checks and the wait that precede
    them."""

    def step_and_commit(self):
        step = {"executor": {"type": "argv", "argv": ["insert"], "feed": "stdin", "timeoutSec": 2},
                "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}]}
        commit = {"executor": {"type": "argv", "argv": ["enter"], "feed": "stdin", "timeoutSec": 2},
                  "outcomes": [{"match": {"exitCode": 0}, "outcome": "turn-started"}]}
        return step, commit

    def test_a_stop_answer_during_the_idle_wait_sends_nothing(self):
        world = World(self, two_channels())
        cancel = FlagCancel()
        mon = world.monitor(cancel=cancel)

        def idle(sink, channel, timeout):
            cancel.stop = "token_expired"
        with mock.patch.object(world.runner.DeliverySink, "wait_idle", idle):
            with self.assertRaises(world.runner.Stop) as stop:
                mon.page([note(1)])
        self.assertEqual(stop.exception.reason, "token_expired")
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual(mon.deliveries(), [(1, "access-lost", None, False)])

    def test_a_poll_gone_stale_during_the_idle_wait_holds_the_send(self):
        world = World(self, two_channels())
        cancel = FlagCancel()
        mon = world.monitor(cancel=cancel)
        waits = []

        def idle(sink, channel, timeout):
            waits.append(1)
            if len(waits) == 1:
                # The poll goes stale during the first wait; a fresh one
                # comes after.
                cancel.stale_once = True
        with mock.patch.object(world.runner.DeliverySink, "wait_idle", idle):
            mon.page([note(1)])
        # The first attempt sent nothing; the send came after the fresh poll.
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertEqual(len(waits), 2)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "queued")])

    def test_the_steps_of_an_attempt_share_one_deadline(self):
        # Review finding 20 of wave 7 (INV-8: every call within the idle
        # window): the idle wait spends of the attempt's time, the call gets
        # what is left, not a whole timeout again.
        world = World(self, two_channels())
        mon = world.monitor()
        timeouts = []
        inner = mon.spy.call

        def call(spec, text, timeout):
            timeouts.append(timeout)
            return inner(spec, text, timeout)
        mon.spy.call = call

        def idle(sink, channel, timeout):
            world.clock.t += 1.5
        with mock.patch.object(world.runner.DeliverySink, "wait_idle", idle):
            mon.page([note(1)])
        # The channel's timeout is 2 s (chan()).
        self.assertEqual(len(timeouts), 1)
        self.assertLessEqual(timeouts[0], 0.5 + 1e-9)

    def test_a_poll_gone_stale_after_the_insert_withholds_enter(self):
        world = World(self, two_channels())
        insert, enter = fake_channel.two_step(world.bin)
        cancel = FlagCancel()
        mon = world.monitor(cancel=cancel)
        step, commit = self.step_and_commit()
        mon.sink.drivers["A"] = world.runner.TwoStepDriver(world.reg.channel("A"), mon.spy, step, commit)

        def after(program):
            if program == "insert":
                cancel.stale = True
        mon.spy.after = after
        mon.page([note(1)])
        self.assertEqual(len(insert.calls()), 1)
        self.assertEqual(enter.calls(), [], "Enter after the last successful poll went stale")
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown")])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class AddressAgainTest(unittest.TestCase):
    """Review finding V4 of wave 7 ("Addressee and check": the address of
    the addressee is checked before a transfer): a run whose environment
    names another address of the channel than the preflight saw - a value
    or a qualifier - does not deliver to it; "addressee changed"."""

    IDENTITY = {"process": None, "addresses": [{"channel": "A", "value": "addr-1", "env": "A_ADDR",
                                                "qualifiers": [["A_HOME", "/h"]]}],
                "inherited": [], "terminal": None}

    def test_another_address_in_the_environment_is_another_addressee(self):
        for name, value in (("A_ADDR", "addr-2"), ("A_HOME", "/other")):
            with self.subTest(variable=name):
                world = World(self, two_channels())
                world.env.update({"A_ADDR": "addr-1", "A_HOME": "/h"})
                world.env[name] = value
                mon = world.monitor(identity=self.IDENTITY)
                mon.page([note(1)])
                mon.page([note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
                self.assertEqual(world.prog("chan-a").calls(), [])
                self.assertEqual(world.prog("chan-b").calls(), [])
                self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown"), (2, "journal")])
                self.assertEqual(len(mon.degraded("addressee_changed")), 1)

    def test_the_same_address_is_delivered(self):
        world = World(self, two_channels())
        world.env.update({"A_ADDR": "addr-1", "A_HOME": "/h"})
        mon = world.monitor(identity=self.IDENTITY)
        mon.page([note(1)])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "queued")])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class StopDuringCallTest(unittest.TestCase):
    """Review finding V1 of wave 7 (INV-9: "unknown" is never sent again;
    US-12: the receipt of a stop comes after the events' outcomes): a stop
    while the channel may have the text fixes "unknown" in the state and
    the journal before the stop goes on; a stop before the call was counted
    hands nothing over and fixes nothing."""

    def test_a_stop_after_the_channel_took_the_text(self):
        world = World(self, two_channels())
        mon = world.monitor()

        def stop(program):
            raise world.runner.StopSignal()
        mon.spy.after = stop
        with self.assertRaises(world.runner.StopSignal):
            mon.page([note(1)])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "unknown", "A")])
        outcomes, _ = world.runner.load_page_outcomes(mon.token, CURSOR)
        self.assertEqual(outcomes, {"1": "unknown"})
        # The next start of the token sends nothing again.
        again = world.monitor()
        again.page([note(1)])
        self.assertEqual(len(world.prog("chan-a").calls()), 1)

    def test_a_stop_before_the_call_fixes_nothing(self):
        world = World(self, two_channels())
        mon = world.monitor()

        def stop(sink, channel, timeout):
            raise world.runner.StopSignal()
        with mock.patch.object(world.runner.DeliverySink, "wait_idle", stop):
            with self.assertRaises(world.runner.StopSignal):
                mon.page([note(1)])
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual(mon.deliveries(), [])
        # Not lost: the next start delivers it.
        again = world.monitor()
        again.page([note(1)])
        self.assertEqual([d[:2] for d in again.deliveries()], [(1, "queued")])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class JournalAndLedgerTest(unittest.TestCase):
    """Row #56: a journal that takes no write after the channel took the
    text holds new sends and the page; after it is back the outcome of the
    begun delivery is written first, from the ledger and the state. A
    ledger that cannot be used with a working journal: state_unavailable,
    the journal only."""

    def test_the_journal_outage_holds_sends_and_the_outcome_comes_first_after_it(self):
        world = World(self, two_channels())
        mon = world.monitor()
        outage = world.runner.JournalUnavailable("write_failed", mon.journal.path)
        with mock.patch.object(mon.journal, "delivery", side_effect=outage):
            with self.assertRaises(world.runner.JournalUnavailable):
                mon.page([note(1), note(2)])
            with self.assertRaises(world.runner.JournalUnavailable):
                mon.page([note(1), note(2)])
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertEqual(mon.deliveries(), [])
        mon.page([note(1), note(2)])
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "queued"), (2, "queued")])

    def test_a_ledger_that_cannot_be_used_sends_nothing(self):
        world = World(self, two_channels())
        mon = world.monitor()
        user = os.path.join(world.env["XDG_STATE_HOME"], world.runner.USER_DIR_NAME)
        os.makedirs(user, 0o700, exist_ok=True)
        with open(os.path.join(user, "ledger"), "w") as fh:
            fh.write("not a directory")
        mon.page([note(1)])
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "journal")])
        self.assertEqual(len(mon.degraded("state_unavailable")), 1)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class LedgerRefusalTest(unittest.TestCase):
    """Review finding C3 of wave 7: a ledger that refuses at any step while
    the monitor works - the spec ("Failures": the ledger directory
    unavailable while the journal is available, at the start and while
    working): the monitor keeps working, degraded state_unavailable, no
    external delivery, the events go to the journal."""

    CURSOR2 = {"ts": "2026-01-01T00:00:01Z", "id": 8}

    def refusing(self, world, method):
        return mock.patch.object(world.runner.Attempt if method != "record" else world.runner.Ledger, method,
                                 side_effect=world.runner.LedgerUnavailable("write_failed"))

    def test_a_counted_call_refused_sends_nothing(self):
        world = World(self, two_channels())
        mon = world.monitor()
        with self.refusing(world, "note_call"):
            mon.page([note(1)])
        mon.page([note(2)], cursor=self.CURSOR2)
        self.assertEqual(world.prog("chan-a").calls(), [])
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "journal"), (2, "journal")])
        self.assertEqual(len(mon.degraded("state_unavailable")), 1)

    def test_an_outcome_refused_after_the_call_keeps_it_and_stops_external_delivery(self):
        world = World(self, two_channels())
        mon = world.monitor()
        with self.refusing(world, "finish"):
            mon.page([note(1)])
        mon.page([note(2)], cursor=self.CURSOR2)
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "queued"), (2, "journal")])
        self.assertEqual(len(mon.degraded("state_unavailable")), 1)

    def test_an_outcome_without_a_call_refused(self):
        world = World(self, two_channels({"limit": 30}, {"limit": 30}))
        mon = world.monitor()
        with self.refusing(world, "record"):
            mon.page([note(1)])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "does-not-fit")])
        self.assertEqual(len(mon.degraded("state_unavailable")), 1)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class CursorResetTest(unittest.TestCase):
    """Row #57: 400 invalid_cursor in a detached monitor — degraded
    cursor_reset, a summary line in the journal, the monitor goes on from
    the new start."""

    def test_cursor_reset_in_a_detached_monitor(self):
        world = World(self, two_channels())
        mon = world.monitor()

        def respond(h, qs):
            n = len(h.server.requests)
            if n == 1:
                h.send_json(200, {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:05Z", "id": 5}})
            elif n == 2:
                h.send_json(400, {"error": "invalid_cursor"})
            else:
                h.send_json(401, {})
        server = harness.ScriptedServer(respond)
        self.addCleanup(server.stop)
        executor = world.runner.Executor()
        self.addCleanup(executor.stop)
        with self.assertRaises(world.runner.Stop):
            world.runner.work(executor, server.url(), TOKEN, ISSUE, "", mon.token, sink=mon.sink)
        self.assertEqual([(q.get("since_id") or [None])[0] for q in server.requests], [None, "5", None])
        self.assertEqual(len(mon.degraded("cursor_reset")), 1)
        summaries = [l for l in mon.lines() if l.get("event") == "summary"]
        self.assertEqual([s["summary"] for s in summaries], ["cursor_reset"])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class ChannelFailingTest(unittest.TestCase):
    """Row #118: one channel_failing per switch on an exhausted budget, with
    the channel and its last answer; the switch holds for the current event
    only; recovered comes only from a delivery by the failed channel
    itself; a forced channel tries again on the next event."""

    def test_failing_and_recovered_by_the_same_channel(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([REFUSE, REFUSE, REFUSE, REFUSE, QUEUED])
        mon = world.monitor()
        mon.page([note(1)])
        failing = mon.degraded("channel_failing")
        self.assertEqual(len(failing), 1)
        self.assertEqual(failing[0]["channel"], "A")
        self.assertEqual(failing[0]["lastAnswer"], {"exitCode": 3})
        self.assertEqual(mon.recovered(), [])
        mon.page([note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
        self.assertEqual(len(world.prog("chan-a").calls()), 5)
        self.assertEqual(len(mon.degraded("channel_failing")), 1)
        self.assertEqual([l["channel"] for l in mon.recovered()], ["A"])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "B"), (2, "queued", "A")])

    def test_a_forced_channel_is_tried_again_on_the_next_event(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([REFUSE])
        mon = world.monitor(fallbacks=())
        mon.page([note(1)])
        mon.page([note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
        self.assertEqual(len(world.prog("chan-a").calls()), 8)
        # One line per switch: each event's budget ran out once.
        self.assertEqual(len(mon.degraded("channel_failing")), 2)
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "refused-before-send"),
                                                              (2, "refused-before-send")])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class RealExecutorFailureTest(unittest.TestCase):
    """Row #119: the channel program gone from PATH — retries in the
    budget, then the fallback gets the event exactly once; the agent process
    changing during a one-step call — no repeat, later events to the
    journal."""

    def test_a_program_gone_from_path(self):
        world = World(self, two_channels())
        world.prog("chan-a").remove()
        mon = world.monitor()
        mon.page([note(1)])
        self.assertEqual(len(mon.spy.times("chan-a")), 4)
        self.assertEqual(len(world.prog("chan-b").calls()), 1)
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "B")])

    def test_the_agent_process_changes_during_a_one_step_call(self):
        world = World(self, two_channels({"kind": "terminal", "process": "alive"}))
        agent = subprocess.Popen(["sleep", "60"])
        self.addCleanup(agent.wait)
        self.addCleanup(agent.kill)
        identity = dict(ADDRESS, process=[agent.pid, harness_info(agent.pid).start])
        world.prog("chan-a").respond([dict(QUEUED, kill=agent.pid)])
        mon = world.monitor(identity=identity)
        mon.page([note(1)])
        mon.page([note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
        self.assertEqual(len(world.prog("chan-a").calls()), 1)
        self.assertEqual(world.prog("chan-b").calls(), [])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown"), (2, "journal")])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class PeriodicTailTest(unittest.TestCase):
    """Row #120: a tail of another, stopped monitor that appears while this
    one works is appended to that monitor's journal on a periodic step,
    without a new start and without double lines; then it is removed."""

    def test_tails_are_appended_periodically(self):
        world = World(self, two_channels())
        mon = world.monitor()
        r = world.runner
        other_dir = os.path.join(world.root, "tok-stopped")
        os.mkdir(other_dir, 0o700)
        other = r.Journal(os.path.join(other_dir, "token"), ISSUE, env=world.env)

        def defer(i):
            r.defer_lines(other.path, [other.render({"event": "delivery", "status": "access-lost",
                                                     "notificationId": i}, line_id="line-%d" % i)], world.env)
        mon.page([])
        defer(1)
        mon.page([])
        self.assertFalse(os.path.exists(other.path), "appended before its period")
        world.clock.t += r.TAILS_EVERY_SEC + 1
        mon.page([])
        defer(2)
        world.clock.t += r.TAILS_EVERY_SEC + 1
        mon.page([])
        with open(other.path) as fh:
            ids = [json.loads(line)["lineId"] for line in fh]
        self.assertEqual(ids, ["line-1", "line-2"])
        self.assertEqual(os.listdir(os.path.join(r.ledger_dir(world.env), r.TAILS_DIR_NAME)), [])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class BudgetAfterRestartTest(unittest.TestCase):
    """Row #134: after two refused calls the runner dies between the ledger
    and the state; the restart continues the same schedule from the ledger
    (two calls left, at 30 and 120 s), then the fallback, with exactly one
    channel_failing."""

    def test_the_restart_continues_the_budget(self):
        world = World(self, two_channels())
        world.prog("chan-a").respond([REFUSE])
        first = world.monitor()
        t0 = world.clock.now()

        def die():
            if len(first.spy.times("chan-a")) == 2:
                raise Crash()
        world.clock.on_sleep = die
        with self.assertRaises(Crash):
            first.page([note(1)])
        world.clock.on_sleep = None
        restarted = world.monitor()
        restarted.page([note(1)])
        times = first.spy.times("chan-a") + restarted.spy.times("chan-a")
        self.assertEqual(times, [t0, t0 + 5, t0 + 35, t0 + 155])
        self.assertEqual(len(world.prog("chan-b").calls()), 1)
        self.assertEqual(len(restarted.degraded("channel_failing")), 1)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class BatchTextTest(unittest.TestCase):
    """Row #152: a plain delivery of a batch carries the label and the echo
    phrase with the literal path of the exclusion file once, and the fields
    of every event."""

    def test_label_and_echo_phrase_once_per_delivery(self):
        world = World(self, two_channels())
        mon = world.monitor()
        mon.page([note(1, "s-1"), note(2, "s-2"), note(3, "s-3")])
        calls = world.prog("chan-a").calls()
        self.assertEqual(len(calls), 1)
        text = calls[0]["stdin"]
        r = world.runner
        exclusion = os.path.join(os.path.dirname(mon.token), "exclude_session")
        self.assertEqual(text.count(r.DELIVERY_LABEL), 1)
        self.assertEqual(text.count(r.echo_phrase(exclusion)), 1)
        self.assertEqual(text.count(json.dumps(exclusion)), 1)
        for i in (1, 2, 3):
            self.assertIn('notification="%d"' % i, text)
            self.assertIn('actorSessionId="s-%d"' % i, text)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class AttemptOwnershipTest(unittest.TestCase):
    """Review round 2 of wave 7, K1 (P-11): the attempt begin published is
    owned from its begin to its release on every path; a failure before
    the hand-over withdraws it, so the page goes on and nothing becomes
    unknown."""

    def setUp(self):
        self.world = World(self, two_channels())
        self.sleeps = []

        def bounded():
            # A held attempt lock would make the page wait without end.
            self.sleeps.append(1)
            if len(self.sleeps) > 500:
                raise AssertionError("the page waits without end")
        self.world.clock.on_sleep = bounded
        # Event 1 already has its outcome for the addressee.
        self.world.monitor("tok-a").page([note(1)])

    def failing_first_journal_write(self, mon, error):
        real = mon.journal.delivery
        left = [1]

        def delivery(*args, **kw):
            if left:
                left.pop()
                raise error
            return real(*args, **kw)
        mon.journal.delivery = delivery

    def test_a_journal_refusal_on_a_closed_outcome_leaves_no_held_attempt(self):
        mon = self.world.monitor("tok-b")
        self.failing_first_journal_write(mon, self.world.runner.JournalUnavailable("write_failed", "j"))
        with self.assertRaises(self.world.runner.JournalUnavailable):
            mon.page([note(1), note(2)])
        mon.page([note(1), note(2)])
        self.assertEqual(dict((d[0], d[1]) for d in mon.deliveries())[2], "queued")
        self.assertEqual(len(self.world.prog("chan-a").calls()), 2)

    def test_a_stop_signal_on_a_closed_outcome_withdraws_the_attempt(self):
        mon = self.world.monitor("tok-b")
        self.failing_first_journal_write(mon, self.world.runner.StopSignal())
        with self.assertRaises(self.world.runner.StopSignal):
            mon.page([note(1), note(2)])
        restarted = self.world.monitor("tok-b")
        restarted.page([note(1), note(2)])
        self.assertEqual(dict((d[0], d[1]) for d in restarted.deliveries())[2], "queued")


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class OwnershipWindowTest(unittest.TestCase):
    """Review round 3 of wave 7, rows 16 and 19: a stop signal between the
    publish of an attempt and its owner, and a withdrawal the ledger
    refuses, leave no reservation that a later monitor turns into
    "unknown"."""

    def test_a_signal_right_after_begin_returns_withdraws_the_attempt(self):
        world = World(self, two_channels())
        mon = world.monitor("tok-a")
        r = world.runner
        real = mon.sink.ledger.begin

        def begin_then_signal(*args, **kw):
            decision = real(*args, **kw)
            raise r.StopSignal()
        mon.sink.ledger.begin = begin_then_signal
        with self.assertRaises(r.StopSignal):
            mon.page([note(1)])
        again = world.monitor("tok-b").sink.ledger.begin([{"id": "1", "session": "s-1"}], "A")
        self.addCleanup(lambda: again.attempt and again.attempt.release())
        self.assertEqual(again.closed, {})
        self.assertIsNotNone(again.attempt)

    def test_a_signal_inside_begin_after_the_save_withdraws_the_attempt(self):
        world = World(self, two_channels())
        mon = world.monitor("tok-a")
        r = world.runner
        ledger = mon.sink.ledger
        real = ledger._save
        state = {"armed": True}

        def save_then_signal(doc):
            real(doc)
            if state["armed"]:
                state["armed"] = False
                raise r.StopSignal()
        ledger._save = save_then_signal
        with self.assertRaises(r.StopSignal):
            ledger.begin([{"id": "1", "session": "s-1"}], "A")
        again = world.monitor("tok-b").sink.ledger.begin([{"id": "1", "session": "s-1"}], "A")
        self.addCleanup(lambda: again.attempt and again.attempt.release())
        self.assertEqual(again.closed, {})
        self.assertIsNotNone(again.attempt)

    def test_a_refused_withdrawal_is_not_recovered_as_unknown(self):
        world = World(self, two_channels())
        r = world.runner
        ledger = world.monitor("tok-a").sink.ledger
        attempt = ledger.begin([{"id": "1", "session": "s-1"}], "A").attempt
        real = ledger._change

        def refuse(change):
            raise r.LedgerUnavailable("write_failed")
        ledger._change = refuse
        with self.assertRaises(r.LedgerUnavailable):
            attempt.abandon()
        ledger._change = real
        attempt.release()
        again = world.monitor("tok-b").sink.ledger.begin([{"id": "1", "session": "s-1"}], "A")
        self.addCleanup(lambda: again.attempt and again.attempt.release())
        # The owner withdrew the reservation without a call: the event has
        # no outcome and goes out again, it is not "unknown".
        self.assertEqual(again.closed, {})
        self.assertIsNotNone(again.attempt)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class LastBoundaryTest(unittest.TestCase):
    """Review round 2 of wave 7, K2 and rows 13, 14, 16: what a blocking
    check or wait may change is checked again, and the access boundary is
    the last step before the hand-over and before Enter."""

    def setUp(self):
        self.fresh()

    def fresh(self):
        self.world = World(self, two_channels())
        self.cancel = FlagCancel()
        self.mon = self.world.monitor(cancel=self.cancel)
        self.channel = self.world.reg.channel("A")

    def attempt(self, eid="5"):
        decision = self.mon.sink.ledger.begin([{"id": eid, "session": "s-1"}], "A")
        self.addCleanup(decision.attempt.release)
        return decision.attempt

    def test_a_stop_answer_during_the_addressee_check_withholds_enter(self):
        r = self.world.runner
        for name, arm in (("401", lambda: setattr(self.cancel, "stop", "token_expired")),
                          ("stale poll", lambda: setattr(self.cancel, "stale", True))):
            with self.subTest(case=name):
                # A world of its own: the attempt of the case before holds
                # the addressee's window.
                self.fresh()
                guard = self.world.runner.Guard(self.mon.sink, self.channel, self.attempt(name), 30)
                self.mon.sink.check_addressee = lambda channel, timeout=None: arm()
                self.assertFalse(guard.before_commit())
                if name == "401":
                    self.assertEqual(guard.cancel_reason, "token_expired")

    def test_the_addressee_is_checked_again_after_the_idle_wait(self):
        r = self.world.runner
        guard = r.Guard(self.mon.sink, self.channel, self.attempt(), 30)
        calls = []

        def check(channel, timeout=None):
            calls.append(1)
            if len(calls) == 2:
                raise r.AddresseeGone()
        self.mon.sink.check_addressee = check
        self.mon.sink.wait_idle = lambda channel, timeout: None
        with self.assertRaises(r.AddresseeGone):
            guard.before_send()
        self.assertFalse(guard.handed)

    def test_a_reservation_lost_during_the_wait_hands_nothing_over(self):
        r = self.world.runner
        attempt = self.attempt()
        guard = r.Guard(self.mon.sink, self.channel, attempt, 30)
        self.mon.sink.check_addressee = lambda channel, timeout=None: None

        def merge_takes_it(channel, timeout):
            def change(entry):
                entry["events"]["5"]["delivering"]["attempt"] = "another-attempt"
            self.mon.sink.ledger._change(change)
        self.mon.sink.wait_idle = merge_takes_it
        with self.assertRaises(r.AttemptLost):
            guard.before_send()
        self.assertFalse(guard.handed)

    def test_a_merge_during_the_last_poll_hands_nothing_over(self):
        # Review round 3 of wave 7, row 15: the poll of the last network
        # boundary may take long; a third monitor merging the addressee's
        # records meanwhile takes the reservation. The reservation is
        # checked after that poll, so nothing is handed over (INV-1).
        r = self.world.runner
        attempt = self.attempt()
        sink = self.mon.sink
        guard = r.Guard(sink, self.channel, attempt, 30)
        sink.check_addressee = lambda channel, timeout=None: None
        sink.wait_idle = lambda channel, timeout: None
        polls = []

        class SlowPoll(object):
            def check(self):
                polls.append(1)
                if len(polls) == 2:
                    def change(entry):
                        entry["events"]["5"]["delivering"]["attempt"] = "another-attempt"
                    sink.ledger._change(change)
                return None

            def known(self):
                return None

            def may_send(self):
                return True

        sink.cancel = SlowPoll()
        with self.assertRaises(r.AttemptLost):
            guard.before_send()
        self.assertFalse(guard.handed)
        self.assertEqual(len(polls), 2)

    def test_the_addressee_is_checked_after_the_last_poll_before_enter(self):
        # Row 20: the agent ends while the poll before Enter is in flight;
        # the addressee check comes after that poll and withholds Enter.
        r = self.world.runner
        guard = r.Guard(self.mon.sink, self.channel, self.attempt(), 30)
        state = {"ended": False}

        class EndingPoll(object):
            def check(self):
                state["ended"] = True
                return None

            def known(self):
                return None

            def may_send(self):
                return True

        def check(channel, timeout=None):
            if state["ended"]:
                raise r.AddresseeChanged()
        self.mon.sink.cancel = EndingPoll()
        self.mon.sink.check_addressee = check
        self.assertFalse(guard.before_commit())
        self.assertTrue(guard.changed)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class BatchOutcomeAndReceiptTest(unittest.TestCase):
    """Review round 2 of wave 7, rows 17 and 18."""

    def test_the_state_holds_the_outcome_of_the_whole_batch_before_the_journal(self):
        world = World(self, two_channels())
        mon = world.monitor()
        real = mon.journal.delivery
        left = [1]

        def delivery(*args, **kw):
            if left:
                left.pop()
                raise world.runner.JournalUnavailable("write_failed", "j")
            return real(*args, **kw)
        mon.journal.delivery = delivery
        with self.assertRaises(world.runner.JournalUnavailable):
            mon.page([note(1), note(2)])
        self.assertEqual(len(world.prog("chan-a").calls()), 1, "the two events were not one batch")
        self.assertEqual(world.runner.load_page_outcomes(mon.token, CURSOR)[0], {"1": "queued", "2": "queued"})

    def test_no_receipt_goes_outside_when_the_ledger_is_off(self):
        world = World(self, two_channels())
        for flag in ("ledger_off", "journal_only"):
            with self.subTest(flag=flag):
                mon = world.monitor("tok-%s" % flag)
                setattr(mon.sink, flag, True)
                mon.sink.receipt("receipt text", "short")
        self.assertEqual(world.prog("chan-a").calls(), [])


if __name__ == "__main__":
    unittest.main()
