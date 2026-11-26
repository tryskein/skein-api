#!/usr/bin/env python3
"""SC-001 of the stream records of wave 1, Claude Code and Grok Build
(matrix row #73), over the registry the server embeds.

The expected values come from the sources the records name, not from the
records: Claude Code's Monitor tool runs a command with timeout_ms of at
most 1800000 (30 minutes; code.claude.com tools-reference "Monitor tool"),
then has to be started again, and needs Git Bash on Windows; Grok Build's
monitor tool takes `persistent: true`, turns every stdout line into one
event and lets 10 events through at once, one more every 2 s, suppressing
the rest and killing the monitor after 30 s of suppression
(xai-grok-build monitor/types.rs#RATE_LIMIT_CAPACITY,
#AUTO_KILL_THRESHOLD_MS).

Run on its own: python3 internal/handler/assets/runner_tests/entries_stream_w1_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_proc  # noqa: E402
import modes_stream_test as ms  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402

RUNNER = pm.RUNNER
REG = pm.REG

# Grok Build's monitor rate limit, from its source.
GROK_CAPACITY = 10
GROK_REFILL_SEC = 2.0
GROK_AUTO_KILL_SEC = 30.0
# Claude Code's Monitor deadline at its maximum, from the documentation.
CLAUDE_MAX_TIMEOUT_MS = 1800000


def agent_of(client, names):
    """The agent process the record finds in a chain runner -> shell ->
    names[0] -> ..., or None."""
    source = fake_proc.FakeProcesses()
    source.add(pm.RUNNER_PID, pm.RUNNER_PID - 1, "python3", pm.RUNNER_PROC_ARGV)
    source.add(pm.RUNNER_PID - 1, pm.RUNNER_PID - 2, "bash")
    pid = pm.RUNNER_PID - 2
    for i, (name, argv) in enumerate(names):
        source.add(pid, pid - 1 if i + 1 < len(names) else 1, name, argv)
        pid -= 1
    placement = RUNNER.place(RUNNER.ancestry(source, pm.RUNNER_PID), REG, RUNNER.resolve_record(REG, client),
                             self_pid=pm.RUNNER_PID)
    return placement.agent


def grok_suppressed(times):
    """The suppression Grok's bucket would make of lines arriving at times:
    (number of suppressed lines, the longest run of suppression in s)."""
    tokens, last, suppressed, since, longest = float(GROK_CAPACITY), None, 0, None, 0.0
    for t in times:
        if last is not None:
            tokens = min(GROK_CAPACITY, tokens + (t - last) / GROK_REFILL_SEC)
        last = t
        if tokens >= 1.0:
            tokens -= 1.0
            if since is not None:
                longest = max(longest, t - since)
                since = None
        else:
            suppressed += 1
            since = t if since is None else since
    return suppressed, longest


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class RecognitionTest(unittest.TestCase):
    """The process sign of each record: its own executable is found, a
    process of another name is not."""

    def test_the_agent_process_of_each_record(self):
        cases = [("claude", [("claude", ["claude", "--dangerously-skip-permissions"])], True),
                 ("claude", [("claude.exe", None)], True),
                 ("claude", [("claude-desktop", None)], False),
                 ("claude", [("node", ["node", "/usr/lib/claude/cli.js"])], False),
                 ("grok", [("grok", None)], True),
                 ("grok", [("grok-native", ["/usr/lib/grok/bin/grok-native"])], True),
                 ("grok", [("grokker", None)], False)]
        for client, chain, found in cases:
            with self.subTest(client=client, chain=chain):
                agent = agent_of(client, chain)
                self.assertEqual(agent is not None, found, agent and agent.name)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class InstructionTest(unittest.TestCase):
    """The tool and parameters of the stream method of each record."""

    def test_claude_runs_monitor_at_its_longest_deadline_and_rearms(self):
        box = pm.Box(self)
        line = box.preflight(box.env("claude"), ["claude"])
        self.assertEqual((line["client"], line["method"], line["channel"]), ("claude", "stream", "stdout"))
        self.assertIn("Monitor tool", line["instruction"])
        self.assertIn("timeout_ms %d" % CLAUDE_MAX_TIMEOUT_MS, line["instruction"])
        self.assertIn("same `command`", line["instruction"])
        self.assertEqual(REG.agents["claude"]["windowsShell"], "git-bash")

    def test_grok_runs_monitor_persistent(self):
        box = pm.Box(self)
        line = box.preflight(box.env("grok"), ["grok"])
        self.assertEqual((line["client"], line["method"], line["channel"]), ("grok", "stream", "stdout"))
        self.assertIn("monitor tool", line["instruction"])
        self.assertIn("persistent: true", line["instruction"])

    def test_a_forced_external_channel_makes_a_stream_record_detached(self):
        box = pm.Box(self)
        for client in ("claude", "grok"):
            with self.subTest(client=client):
                env = box.env(client, SKEIN_MONITOR_CHANNEL="zavod", **box.zavod_env(client))
                line = box.preflight(env, [client])
                self.assertEqual((line["method"], line["channel"]), ("detached", "zavod"), line)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX working start")
class RearmTest(unittest.TestCase):
    """Claude's monitor ends at its deadline; the same working command
    starts the same result again."""

    def test_the_same_command_starts_again_after_the_deadline(self):
        box = pm.Box(self)
        line = box.preflight(box.env("claude"), ["claude"])
        env = box.env("claude", SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT=str(line["preflightId"]))

        def deadline(*args, **kw):
            # The Monitor tool stops the command at its deadline.
            raise RUNNER.StopSignal()

        runs = []
        with mock.patch.object(RUNNER, "run_stream_loop", deadline):
            for _ in range(2):
                runs.append(box.run(env))
        for code, lines in runs:
            self.assertEqual(code, 0, lines)
            self.assertEqual(lines[0]["event"], "monitor_started", lines)
            self.assertEqual((lines[-1]["event"], lines[-1]["reason"]), ("paused", "stopped"), lines)
        # The first start prints the full line, the restart the short one.
        self.assertIn("journal", runs[0][1][0])
        self.assertEqual(set(runs[1][1][0]), {"event", "preflightId", "method", "channel"})
        self.assertTrue(os.path.exists(box.token))


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX stream output")
class GrokPaceTest(unittest.TestCase):
    """A long series under the record's limits stays inside Grok's bucket:
    nothing is suppressed, so the monitor is never killed."""

    def test_a_long_series_is_never_suppressed(self):
        limits = REG.agents["grok"]["methods"][0].get("limits")
        box = ms.Box(self)
        clock = ms.FakeClock()
        output = box.output(limits, "stream", clock=clock)
        pages = [ms.page(list(range(1 + 50 * i, 51 + 50 * i)), i + 1) for i in range(3)]
        executor = ms.FakeExecutor(pages)
        with mock.patch.object(ms.RUNNER.time, "sleep", lambda s: None):
            with self.assertRaises(ms.RUNNER.Stop):
                ms.RUNNER.run_stream_loop(executor, "http://poll.test", "skm_x", ms.ISSUE, "", box.token,
                                          {"method": "stream", "preflightId": ms.PREFLIGHT_ID}, output)
        times = []
        for t, text in output.out.writes:
            times.extend([t] * len([l for l in text.splitlines() if l]))
        notes = [l for l in output.out.lines() if l.get("event") == "notification"]
        self.assertEqual(len(notes), 150)
        suppressed, longest = grok_suppressed(times)
        self.assertEqual(suppressed, 0)
        self.assertLess(longest, GROK_AUTO_KILL_SEC)


if __name__ == "__main__":
    unittest.main()
