#!/usr/bin/env python3
"""SC-001 of the stream records of wave 4 - CodeBuddy, oh-my-pi, Xum and Qwen
Code (matrix row #109) - and oh-my-pi's pace and restart (row #110), over the
registry the server embeds.

The expected values come from the sources the records name, not from the
records:
- CodeBuddy (codebuddy-code d220986, docs/release-notes/v2.129.0.md,
  v2.168.0.md): Monitor is deferred (found with ToolSearch), on by default,
  off with CODEBUDDY_MONITOR_ENABLED=false; it runs at most 30 minutes;
  npm bin codebuddy, cbc (a Node script).
- oh-my-pi (can1357/oh-my-pi 03597e3, src/async/notify.ts#OutputNotifier):
  matches within 200 ms make one batch of at most 50 lines of at most 400
  characters (JS length), 30 batches, one more every 20 s, the batch that
  spends the allowance is `stopped`; bash takes notify (a regex) and
  timeout 0 (no deadline); proc://<id>/kill cancels a job.
- Xum (coder/xum d158f96): bash with run_in_background and monitor.filter;
  a wake carries no output, task_await reads it; 1024 bytes per line.
- Qwen Code (QwenLM/qwen-code df0f008d, monitor.ts): 5 lines, then one per
  second; a line is cut at 4096 (JS length); idle_timeout_ms at most
  600000; run_shell_command is_background for exit-on-event.

Run on its own: python3 internal/handler/assets/runner_tests/entries_stream_w4_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import re
import sys
import unittest
from urllib.parse import parse_qs, urlparse
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_stream_w1_test as w1  # noqa: E402
import fake_channel  # noqa: E402
import modes_stream_test as ms  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402

RUNNER = pm.RUNNER
REG = pm.REG

# oh-my-pi's OutputNotifier, from src/async/notify.ts.
OMP_BATCH_SEC = 0.2
OMP_MAX_LINES = 50
OMP_MAX_LINE_CHARS = 400
OMP_BURST = 30
OMP_REFILL_SEC = 20.0
# The regex its bash tool is told (the instruction), as grep reads it.
OMP_NOTIFY = re.compile(r'^\{"event"')
# Qwen Code's monitor, from monitor.ts.
QWEN_MAX_IDLE_MS = 600000
QWEN_LINE_CAP = 4096


def omp_notices(times):
    """What oh-my-pi's OutputNotifier makes of matching lines arriving at
    times (sorted): (lines omitted, whether a batch came stopped)."""
    allowance, refilled_at = OMP_BURST, times[0] if times else 0.0
    omitted, stopped, i = 0, False, 0
    while i < len(times) and not stopped:
        first = times[i]
        n = 0
        while i < len(times) and times[i] < first + OMP_BATCH_SEC:
            n += 1
            i += 1
        flush = first + OMP_BATCH_SEC
        refill = int((flush - refilled_at) // OMP_REFILL_SEC)
        if refill > 0:
            allowance = min(OMP_BURST, allowance + refill)
            refilled_at += refill * OMP_REFILL_SEC
        allowance -= 1
        omitted += max(0, n - OMP_MAX_LINES)
        stopped = allowance <= 0
    return omitted, stopped


def utf16(text):
    return len(text.encode("utf-16-le")) // 2


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class RecognitionTest(unittest.TestCase):
    """The process sign of each record: its executable, or the script a
    Node or Bun interpreter runs by its path; another name is not it."""

    def test_the_agent_process_of_each_record(self):
        cases = [("codebuddy", [("node", ["node", "/usr/local/bin/codebuddy"])], True),
                 ("codebuddy", [("node", ["node", "/home/u/.npm/bin/cbc", "-c"])], True),
                 ("codebuddy", [("codebuddy-desktop", None)], False),
                 ("omp", [("omp", None)], True),
                 ("omp", [("bun", ["bun", "/home/u/.bun/bin/omp"])], True),
                 ("omp", [("ompd", None)], False),
                 ("xum", [("xum", None)], True),
                 ("xum", [("Xum", ["/Applications/Xum.app/Contents/MacOS/Xum"])], True),
                 ("xum", [("xumx", None)], False),
                 ("qwen-code", [("node", ["node", "/usr/bin/qwen"])], True),
                 ("qwen-code", [("node", ["node", "/usr/lib/node_modules/@qwen-code/qwen-code/scripts/cli-entry.js"])],
                  True),
                 ("qwen-code", [("qwenx", None)], False)]
        for client, chain, found in cases:
            with self.subTest(client=client, chain=chain):
                agent = w1.agent_of(client, chain)
                self.assertEqual(agent is not None, found, agent and agent.name)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class InstructionTest(unittest.TestCase):
    """The tool and parameters of the method each record's preflight takes."""

    def line(self, client, chain, agent_argv=None, **env):
        box = pm.Box(self)
        return box.preflight(box.env(client, **env), chain, agent_argv=agent_argv)

    def test_codebuddy_finds_monitor_and_runs_it_at_its_longest_deadline(self):
        line = self.line("codebuddy", ["codebuddy"])
        self.assertEqual((line["client"], line["method"], line["channel"]), ("codebuddy", "stream", "stdout"))
        for part in ("ToolSearch", "Monitor tool", "timeout_ms 1800000", "start it with Monitor again"):
            self.assertIn(part, line["instruction"])

    def test_codebuddy_without_monitor_exits_on_an_event(self):
        line = self.line("codebuddy", ["codebuddy"], CODEBUDDY_MONITOR_ENABLED="false")
        self.assertEqual(line["method"], "exit-on-event", line)
        self.assertIn("run_in_background=true", line["instruction"])
        # Only the value the source names turns Monitor off.
        for value in ("true", "1"):
            with self.subTest(value=value):
                self.assertEqual(self.line("codebuddy", ["codebuddy"], CODEBUDDY_MONITOR_ENABLED=value)["method"],
                                 "stream")

    def test_oh_my_pi_runs_a_background_bash_job_with_its_notify_regex(self):
        line = self.line("omp", ["omp"])
        self.assertEqual((line["method"], line["channel"]), ("stream", "stdout"))
        instruction = line["instruction"]
        self.assertIn("bash tool", instruction)
        self.assertIn('"notify": "^\\\\{\\"event\\""', instruction)
        self.assertIn('"timeout": 0', instruction)
        # After a stopped notice: cancel that job, then the same command.
        self.assertIn("proc://<job id>/kill", instruction)
        self.assertIn("already_running", instruction)
        self.assertEqual(REG.agents["omp"]["windowsShell"], "git-bash")

    def test_xum_runs_bash_with_a_monitor_and_reads_with_task_await(self):
        line = self.line("xum", ["xum"])
        self.assertEqual((line["method"], line["channel"]), ("stream", "stdout"))
        for part in ("run_in_background true", '"filter"', "task_await", "no timeout_secs"):
            self.assertIn(part, line["instruction"])

    def test_qwen_runs_monitor_at_its_longest_idle_and_has_an_exit_on_event_method(self):
        line = self.line("qwen-code", ["node"], agent_argv=["node", "/usr/bin/qwen"])
        self.assertEqual((line["method"], line["channel"]), ("stream", "stdout"))
        self.assertIn("idle_timeout_ms %d" % QWEN_MAX_IDLE_MS, line["instruction"])
        self.assertIn("Idle timeout", line["instruction"])
        # The limit of 16 running monitors (monitorRegistry.ts
        # #MAX_CONCURRENT_MONITORS) is the agent's state, not a condition the
        # preflight reads: the instruction says what to do when it refuses.
        self.assertIn("maximum of concurrent monitors", line["instruction"])
        second = REG.agents["qwen-code"]["methods"][1]
        self.assertEqual(second["kind"], "exit-on-event")
        self.assertIn("is_background: true", second["instruction"])

    def test_oh_my_pi_under_paseo_is_woken_by_paseo(self):
        # SC-002 "agent under Paseo - oh-my-pi": Paseo keeps no own method
        # of oh-my-pi, so its channel goes first.
        box = pm.Box(self)
        fake_channel.FakeProgram(box.bin, "paseo")
        env = box.env("omp", PASEO_AGENT_ID="0f8fad5b-d9cb-469f-a165-70867728950e")
        line = box.preflight(env, ["omp", "Paseo Daemon"], shell_between=False)
        self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"), line)


class LimitsFromTheSourceTest(unittest.TestCase):
    """The limits of each record against the numbers of its source."""

    def test_the_limits_keep_within_the_tools(self):
        omp = REG.agents["omp"]["methods"][0]["limits"]
        self.assertLessEqual(omp["line"]["max"], OMP_MAX_LINE_CHARS)
        self.assertEqual(omp["line"]["unit"], "utf16")
        self.assertLessEqual(omp["batch"]["max"], OMP_MAX_LINES)
        self.assertEqual(omp["window"], {"lengthMs": int(OMP_BATCH_SEC * 1000), "lines": OMP_MAX_LINES,
                                         "budget": OMP_BURST, "refillSec": int(OMP_REFILL_SEC)})
        qwen = REG.agents["qwen-code"]["methods"][0]["limits"]
        self.assertLessEqual(qwen["line"]["max"], QWEN_LINE_CAP)
        self.assertEqual((qwen["line"]["unit"], qwen["batch"]), ("utf16", {"max": 1, "unit": "lines"}))
        # One line a batch, below 5 at once and one a second.
        self.assertLessEqual(qwen["rate"]["burst"], 5)
        self.assertGreaterEqual(qwen["rate"]["intervalSec"], 1)
        xum = REG.agents["xum"]["methods"][0]["limits"]
        self.assertEqual(xum["line"], {"max": 1024, "unit": "bytes"})


class LiveSeries(w1.BoundaryExecutor):
    """The pages of a long series; every third poll the monitor also says
    something about itself (a service line among the events)."""

    def __init__(self, pages, output):
        w1.BoundaryExecutor.__init__(self, pages)
        self.output = output
        self.polls = 0

    def request(self, url, token):
        self.polls += 1
        if self.polls % 3 == 0:
            self.output.lifecycle({"event": "degraded", "cause": "poll_failing", "issue": ms.ISSUE})
            self.output.lifecycle({"event": "recovered", "cause": "poll_failing", "issue": ms.ISSUE})
        return w1.BoundaryExecutor.request(self, url, token)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX stream output")
class OhMyPiPaceTest(unittest.TestCase):
    """Row #109: a long series of oh-my-pi with service lines among the
    events, on fake clocks."""

    def run_series(self, pages=60):
        limits = REG.agents["omp"]["methods"][0]["limits"]
        box = ms.Box(self)
        clock = ms.FakeClock()
        output = box.output(limits, "stream", clock=clock)
        executor = LiveSeries([ms.page([i], i) for i in range(1, pages + 1)], output)
        with mock.patch.object(ms.RUNNER.time, "sleep", lambda s: None):
            with self.assertRaises(ms.RUNNER.Stop):
                ms.RUNNER.run_stream_loop(executor, "http://poll.test", "skm_x", ms.ISSUE, "", box.token,
                                          {"method": "stream", "preflightId": ms.PREFLIGHT_ID}, output)
        return output

    def test_the_runner_writes_at_most_25_batches_at_once_then_one_every_20_s(self):
        output = self.run_series()
        # A batch is the writes of one window of the tool: a service line
        # written into the place its batch kept joins that batch.
        batches = []
        for t, _ in output.out.writes:
            if not batches or t >= batches[-1] + OMP_BATCH_SEC:
                batches.append(t)
        self.assertGreater(len(batches), 40)
        self.assertLess(batches[24] - batches[0], OMP_REFILL_SEC)
        # The 26th batch comes no sooner than 20 s after the first, and
        # each later one 20 s after the one before.
        self.assertGreaterEqual(batches[25] - batches[0], OMP_REFILL_SEC - 1e-6)
        for k in range(26, len(batches)):
            self.assertGreaterEqual(batches[k] - batches[k - 1], OMP_REFILL_SEC - 1e-6, k)

    def test_the_notifier_of_the_tool_omits_nothing_and_never_stops(self):
        output = self.run_series()
        times = []
        for t, text in output.out.writes:
            for line in text.splitlines():
                if line:
                    # Every line, a service line too, matches the regex of
                    # the instruction and spends the tool's budget.
                    self.assertTrue(OMP_NOTIFY.match(line), line)
                    self.assertLessEqual(utf16(line), OMP_MAX_LINE_CHARS)
                    times.append(t)
        events = [l for l in output.out.lines() if l.get("event") == "notification"]
        self.assertEqual(len(events), 60)
        self.assertTrue([l for l in output.out.lines() if l.get("event") == "degraded"])
        self.assertEqual(omp_notices(times), (0, False))

    def test_a_task_id_of_1000_characters_keeps_every_line_valid(self):
        limits = REG.agents["omp"]["methods"][0]["limits"]
        box = ms.Box(self)
        clock = ms.FakeClock()
        output = box.output(limits, "stream", clock=clock)
        issue = "sk-" + "x" * 997
        output.lifecycle({"event": "degraded", "cause": "journal_unavailable", "issue": issue})
        output.lifecycle({"event": "recovered", "cause": "journal_unavailable", "issue": issue})
        with mock.patch.object(ms.RUNNER.time, "sleep", lambda s: None):
            try:
                ms.RUNNER.run_stream_loop(ms.FakeExecutor([]), "http://poll.test", "skm_x", issue, "", box.token,
                                          {"method": "stream", "preflightId": ms.PREFLIGHT_ID}, output)
            except ms.RUNNER.Stop as stop:
                ms.RUNNER.finish(stop, issue, box.token, write=output.stop_line)
        lines = [l for _, t in output.out.writes for l in t.splitlines() if l]
        self.assertEqual([json.loads(l)["event"] for l in lines][:2], ["degraded", "recovered"])
        for line in lines:
            self.assertLessEqual(utf16(line), OMP_MAX_LINE_CHARS, line[:80])
            self.assertIn("event", json.loads(line))
        self.assertGreaterEqual(len(lines), 3, lines)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX stream output")
class OhMyPiAfterStoppedTest(unittest.TestCase):
    """Row #110: what a stream with client printed is in the journal too, so
    the lines oh-my-pi no longer reported after a stopped notice are read
    there."""

    def test_the_printed_events_are_in_the_journal(self):
        limits = REG.agents["omp"]["methods"][0]["limits"]
        box = ms.Box(self)
        output = box.output(limits, "stream", clock=ms.FakeClock())
        executor = w1.BoundaryExecutor([ms.page([1, 2, 3], 1)])
        with mock.patch.object(ms.RUNNER.time, "sleep", lambda s: None):
            with self.assertRaises(ms.RUNNER.Stop):
                ms.RUNNER.run_stream_loop(executor, "http://poll.test", "skm_x", ms.ISSUE, "", box.token,
                                          {"method": "stream", "preflightId": ms.PREFLIGHT_ID}, output)
        printed = [l["notification"]["id"] for l in output.out.lines() if l.get("event") == "notification"]
        journal = [l for l in box.journal_lines() if l.get("event") == "delivery" and l.get("status") == "printed"]
        self.assertEqual(printed, [1, 2, 3])
        self.assertEqual(sorted(l.get("notificationId") for l in journal), [1, 2, 3])


class StepClock(object):
    """A managed clock of the holder wait (takeover.py#HOLDER_WAIT_CLOCK):
    sleep() moves the time on and runs the actions due by then."""

    def __init__(self, actions=None):
        self.t = 0.0
        self.actions = dict(actions or {})

    def now(self):
        return self.t

    def sleep(self, sec):
        self.t += sec
        for when in sorted(self.actions):
            if self.t >= when:
                self.actions.pop(when)()


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX working start")
class RearmWithALiveHolderTest(unittest.TestCase):
    """Row #110 (OST-2-1, US-10): the agent cancels the earlier job and runs
    the working command again while the cancelled runner still holds the
    lock: the new start waits for it up to TAKEOVER_WAIT_SEC (30 s, on a
    managed clock), then goes on; a holder that stays gets one
    already_running at the end of the wait."""

    def start_with_holder(self, release_at):
        box = pm.Box(self)
        line = box.preflight(box.env("omp"), ["omp"])
        env = box.env("omp", SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT=str(line["preflightId"]))
        held = RUNNER.PosixPlatform.acquire_runner_lock(box.token, 0, env)
        self.addCleanup(held.release)
        clock = StepClock({release_at: held.release} if release_at is not None else {})

        def stream_until_stopped(*args, **kw):
            raise RUNNER.StopSignal()
        with mock.patch.object(RUNNER, "HOLDER_WAIT_CLOCK", clock), \
                mock.patch.object(RUNNER, "run_stream_loop", stream_until_stopped):
            return box.run(env) + (clock.t,)

    def test_a_holder_that_ends_within_the_wait_hands_the_work_over(self):
        code, lines, waited = self.start_with_holder(10)
        self.assertEqual(code, 0, lines)
        self.assertEqual(lines[0]["event"], "monitor_started", lines)
        # It went on once the holder ended, not at the end of the wait.
        self.assertGreaterEqual(waited, 10)
        self.assertLess(waited, 10 + 2 * RUNNER.HOLDER_WAIT_STEP_SEC)

    def test_a_holder_that_stays_gets_one_already_running(self):
        code, lines, waited = self.start_with_holder(None)
        self.assertEqual([l.get("reason") for l in lines], ["already_running"], lines)
        self.assertGreaterEqual(waited, RUNNER.TAKEOVER_WAIT_SEC)
        self.assertLess(waited, RUNNER.TAKEOVER_WAIT_SEC + 2 * RUNNER.HOLDER_WAIT_STEP_SEC)


class StoppingExecutor(ms.FakeExecutor):
    """Answers the pages, then the agent cancels the run (a stop signal
    while it polls)."""

    def request(self, url, token):
        if not self.answers:
            self.urls.append(url)
            raise RUNNER.StopSignal()
        return ms.FakeExecutor.request(self, url, token)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX working start")
class RearmDeliversTheNextEventTest(unittest.TestCase):
    """Row #110: after the re-arm the next event comes from the saved
    cursor. The first run prints events 1 and 2 and is cancelled; the
    working command runs again while the cancelled runner still holds the
    lock (released at 10 s of the managed wait); the new run polls from
    the cursor of event 2 and prints event 3, and only it."""

    def test_the_next_event_after_the_re_arm_comes_from_the_saved_cursor(self):
        box = pm.Box(self)
        line = box.preflight(box.env("omp"), ["omp"])
        env = box.env("omp", SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT=str(line["preflightId"]))
        first = StoppingExecutor([ms.page([1, 2], 1)])
        with mock.patch.object(RUNNER, "Executor", lambda: first), \
                mock.patch.object(RUNNER.time, "sleep", lambda s: None):
            code, lines = box.run(env)
        printed = [l["notification"]["id"] for l in lines if l.get("event") == "notification"]
        self.assertEqual(printed, [1, 2], lines)
        self.assertEqual(lines[-1].get("reason"), "stopped", lines)

        held = RUNNER.PosixPlatform.acquire_runner_lock(box.token, 0, env)
        self.addCleanup(held.release)
        clock = StepClock({10: held.release})
        second = StoppingExecutor([ms.page([3], 2)])
        with mock.patch.object(RUNNER, "Executor", lambda: second), \
                mock.patch.object(RUNNER, "HOLDER_WAIT_CLOCK", clock), \
                mock.patch.object(RUNNER.time, "sleep", lambda s: None):
            code, lines = box.run(env)
        self.assertGreaterEqual(clock.t, 10)
        self.assertEqual(lines[0]["event"], "monitor_started", lines)
        printed = [l["notification"]["id"] for l in lines if l.get("event") == "notification"]
        self.assertEqual(printed, [3], lines)
        since = parse_qs(urlparse(second.urls[0]).query).get("since_id")
        self.assertEqual(since, ["1"], second.urls[0])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX working start")
class HolderWaitGivesWayToATakeoverTest(unittest.TestCase):
    """A start B waits for a live holder A of the same addressee; meanwhile
    a start C asks to take the monitor over (takeover.json), or A's
    addressee changes, and A releases the lock to hand over: B gives way
    (already_running) without taking the lock, which stays free for C
    (T023, US-12)."""

    def setUp(self):
        self.box = pm.Box(self)
        self.line = self.box.preflight(self.box.env("omp"), ["omp"])
        self.env = self.box.env("omp", SKEIN_MONITOR_MODE="run",
                                SKEIN_MONITOR_PREFLIGHT=str(self.line["preflightId"]))
        self.held = RUNNER.PosixPlatform.acquire_runner_lock(self.box.token, 0, self.env)
        self.addCleanup(self.held.release)

    def wait_with(self, event):
        def at_five():
            event()
            self.held.release()
        clock = StepClock({5: at_five})
        with self.assertRaises(RUNNER.Stop) as stop:
            RUNNER.wait_for_holder(self.box.token, self.env, RUNNER.TAKEOVER_WAIT_SEC, clock)
        self.assertEqual(stop.exception.reason, "already_running")
        # The lock is free for the takeover: C takes it at once.
        successor = RUNNER.PosixPlatform.acquire_runner_lock(self.box.token, 0, self.env)
        successor.release()
        self.assertLess(clock.t, 5 + 2 * RUNNER.HOLDER_WAIT_STEP_SEC)

    def test_a_takeover_request_during_the_wait(self):
        self.wait_with(lambda: RUNNER.request_takeover(self.box.token, self.line["preflightId"]))

    def test_a_change_of_the_holder_s_addressee_during_the_wait(self):
        self.wait_with(lambda: RUNNER.mark_addressee_changed(self.box.token, self.line["preflightId"]))

    def test_a_request_already_taken_does_not_hold_the_wait(self):
        request = RUNNER.request_takeover(self.box.token, self.line["preflightId"])
        RUNNER.confirm_takeover(self.box.token, request)
        clock = StepClock({5: self.held.release})
        lock = RUNNER.wait_for_holder(self.box.token, self.env, RUNNER.TAKEOVER_WAIT_SEC, clock)
        lock.release()


if __name__ == "__main__":
    unittest.main()
