#!/usr/bin/env python3
"""SC-001 of the Zavod host record of wave 1 (matrix rows #78, #79), over
the registry the server embeds.

The expected values come from the closed documentation of Zavod and from a
call of its CLI, not from the record. `zavod --json send <address>
--text-file -` takes the body on stdin and prints one JSON envelope whose
top-level exit_code and code carry the outcome (a refused call printed
{"zavod":1,"command":"send","outcome":"refused","exit_code":11,
"code":"letter_too_large","message":"the body is 70000 bytes, the limit is
65536; nothing was accepted",...}); exit 0 accepted, 10 partial effect, 11
refused (caller_not_in_roster: the caller is no live agent;
letter_too_large), 12 no connection, 13 unknown, 14 protocol error. The
own limit of a letter: the send threshold of 65536 bytes and the direct
ring of 10000 characters (skein-monitor-zavod-delivery.md, "Ограничения").
The address is ZAVOD_ME, a UUID equal to ZAVOD_AGENT_ID; the life of the
agent is ZAVOD_AGENT_ROOT_PID, _START and _BOOT_ID.

Run on its own: python3 internal/handler/assets/runner_tests/entries_zavod_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import failures_test as ft  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402

RUNNER = ft.RUNNER
SEND_LIMIT_BYTES = 65536
RING_LIMIT_CHARS = 10000
NO_REPLY = "Do not reply to this letter."
NO_CLEAR = "Do not clear the input field."


def envelope(exit_code, code=None, outcome="refused"):
    return {"exit": exit_code, "stdout": {"zavod": 1, "command": "send", "outcome": outcome,
                                          "exit_code": exit_code, "code": code, "message": "m",
                                          "request_id": None, "result": {}}}


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class RecognitionTest(unittest.TestCase):
    """Zavod is this agent's host only with its address, its CLI and an
    engine of the map; the instruction carries both phrases."""

    def test_the_conditions_of_detection(self):
        box = pm.Box(self)
        good = box.zavod_env("claude")
        cases = [("all held", {}, True),
                 ("ZAVOD_ME not a UUID", {"ZAVOD_ME": "not-a-uuid", "ZAVOD_AGENT_ID": "not-a-uuid"}, False),
                 ("ZAVOD_AGENT_ID another", {"ZAVOD_AGENT_ID": "11111111-2222-4333-8444-555555555555"}, False),
                 ("no ZAVOD_ME", {"ZAVOD_ME": None}, False),
                 ("engine of another record", {"ZAVOD_AGENT_ENGINE_ID": "codex"}, False),
                 ("an engine without a ring", {"ZAVOD_AGENT_ENGINE_ID": "claude-chat"}, False)]
        for name, extra, found in cases:
            with self.subTest(case=name):
                env = pm.merged(box.env("claude", SKEIN_MONITOR_CHANNEL="zavod", **good), extra)
                line = box.preflight(env, ["claude"])
                self.assertEqual(line["channel"] == "zavod", found, line)
        os.remove(os.path.join(box.bin, "zavod"))
        line = box.preflight(box.env("claude", SKEIN_MONITOR_CHANNEL="zavod", **good), ["claude"])
        self.assertEqual((line["method"], line["channel"]), ("journal", None), line)

    def test_the_instruction_names_both_phrases(self):
        box = pm.Box(self)
        line = box.preflight(box.env("claude", SKEIN_MONITOR_CHANNEL="zavod", **box.zavod_env("claude")), ["claude"])
        self.assertIn(NO_REPLY, line["instruction"])
        self.assertIn(NO_CLEAR, line["instruction"])


def zavod_monitor(test, answers, **extra):
    world = ft.World(test)
    world.progs["zavod"].respond(answers)
    mon = world.monitor("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod", **dict(world.zavod_env("claude"),
                                                                                          **extra))
    test.assertEqual(mon.doc["channel"], "zavod")
    return world, mon


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class SendTest(unittest.TestCase):
    """Row #78: the call, its text, every code of the table."""

    def test_the_argv_and_the_text(self):
        world, mon = zavod_monitor(self, [envelope(0, outcome="ok")])
        mon.page([ft.note(1)])
        call = world.calls("zavod")[0]
        self.assertEqual(call["argv"], ["--json", "send", ft.ZAVOD_UUID, "--text-file", "-"])
        self.assertNotIn(ft.PREVIEW, " ".join(call["argv"]))
        for part in ('notification="1"', ft.PREVIEW, NO_REPLY, NO_CLEAR):
            self.assertIn(part, call["stdin"])

    def test_each_code(self):
        cases = [("0", [envelope(0, outcome="ok")], "queued", 1),
                 ("10", [envelope(10, "partial")], "unknown", 1),
                 ("13", [envelope(13, "unknown")], "unknown", 1),
                 ("14", [envelope(14, "protocol_error")], "unknown", 1),
                 ("11 with another code", [envelope(11, "letter_rejected")], "unknown", 1),
                 ("11 with a corrupt envelope", [{"exit": 11, "stdout": "{not json"}], "unknown", 1),
                 ("12", [envelope(12, "no_connection")], "refused-before-send", RUNNER.MAX_CALLS)]
        for name, answers, status, calls in cases:
            with self.subTest(code=name):
                world, mon = zavod_monitor(self, answers)
                mon.page([ft.note(1)])
                self.assertEqual(mon.statuses(), [(1, status)])
                self.assertEqual(len(world.calls("zavod")), calls)

    def test_caller_not_in_roster_stops_the_monitor(self):
        world, mon = zavod_monitor(self, [envelope(11, "caller_not_in_roster")])
        with self.assertRaises(RUNNER.Stop) as caught:
            mon.page([ft.note(1)])
        self.assertEqual(caught.exception.reason, "addressee_gone")
        self.assertEqual(len(world.calls("zavod")), 1)

    def test_letter_too_large_is_sent_again_in_the_minimal_form(self):
        world, mon = zavod_monitor(self, [envelope(11, "letter_too_large"), envelope(0, outcome="ok")])
        mon.page([ft.note(1)])
        calls = world.calls("zavod")
        self.assertEqual(len(calls), 2)
        self.assertIn(ft.PREVIEW, calls[0]["stdin"])
        self.assertNotIn(ft.PREVIEW, calls[1]["stdin"])
        self.assertIn('notification="1"', calls[1]["stdin"])
        self.assertLess(len(calls[1]["stdin"]), len(calls[0]["stdin"]))
        self.assertEqual(mon.statuses(), [(1, "queued")])

    def test_a_letter_keeps_to_the_own_limit(self):
        world, mon = zavod_monitor(self, [envelope(0, outcome="ok")])
        preview = ("Предпросмотр с юникодом ✓ " * 50)[:1024]
        notes = [ft.note(i, preview=preview) for i in range(1, 31)]
        mon.page(notes)
        calls = world.calls("zavod")
        self.assertGreater(len(calls), 1)
        for call in calls:
            self.assertLessEqual(len(call["stdin"]), RING_LIMIT_CHARS)
            self.assertLessEqual(len(call["stdin"].encode("utf-8")), SEND_LIMIT_BYTES)
            self.assertIn(NO_REPLY, call["stdin"])
            self.assertIn(NO_CLEAR, call["stdin"])
        self.assertEqual(sorted(n for n, s in mon.statuses() if s == "queued"), list(range(1, 31)))


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class RecreatedAddresseeTest(unittest.TestCase):
    """Row #79 (SC-002): an agent recreated under the same address. A new
    life before the send is "addressee changed" and nothing is sent; after
    exit 0 the letter is Zavod's, and its ring reaches the current owner of
    the address (boundary B4): the monitor's outcome stays "queued"."""

    def test_a_new_life_before_the_send(self):
        world, mon = zavod_monitor(self, [envelope(0, outcome="ok")])
        # The working run sees another life of the root process under the
        # same address.
        mon.sink.env = dict(mon.sink.env, ZAVOD_AGENT_ROOT_START="2", ZAVOD_AGENT_ROOT_BOOT_ID="boot-2")
        mon.page([ft.note(1)])
        self.assertEqual(world.calls("zavod"), [])
        self.assertEqual(mon.statuses(), [(1, "unknown")])
        self.assertTrue(mon.sink.journal_only)

    def test_a_root_process_that_ended_is_another_life(self):
        """Review finding (line 17) of wave 9: the life is the root process
        itself, not the variables the run still has."""
        root = ft.Sleeper(self)
        world = ft.World(self)
        world.progs["zavod"].respond([envelope(0, outcome="ok")])
        mon = world.monitor("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod",
                            **world.zavod_env("claude", root_pid=root.pid))
        root.end()
        mon.page([ft.note(1)])
        self.assertEqual(world.calls("zavod"), [])
        self.assertEqual(mon.statuses(), [(1, "unknown")])
        self.assertTrue(mon.sink.journal_only)

    def test_a_root_pid_with_another_start_is_another_life(self):
        world = ft.World(self)
        world.progs["zavod"].respond([envelope(0, outcome="ok")])
        env = world.zavod_env("claude")
        # The pid lives, but not the life the variables name (a pid reused).
        env["ZAVOD_AGENT_ROOT_START"] = str(int(env["ZAVOD_AGENT_ROOT_START"]) + 1)
        mon = world.monitor("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod", **env)
        mon.page([ft.note(1)])
        self.assertEqual(world.calls("zavod"), [])
        self.assertEqual(mon.statuses(), [(1, "unknown")])

    def test_after_exit_0_the_letter_stands(self):
        world, mon = zavod_monitor(self, [envelope(0, outcome="ok")])
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        # The agent is recreated after the letter was accepted: the outcome
        # of that delivery is final; the next event meets the new life.
        mon.sink.env = dict(mon.sink.env, ZAVOD_AGENT_ROOT_START="2")
        mon.page([ft.note(2)], cursor={"ts": "t", "id": 4})
        self.assertEqual(len(world.calls("zavod")), 1)
        self.assertEqual(mon.statuses(), [(1, "queued"), (2, "unknown")])


if __name__ == "__main__":
    unittest.main()
