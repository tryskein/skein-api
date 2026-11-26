#!/usr/bin/env python3
"""SC-001 of the Kimi Code terminal UI record of wave 1 (matrix row #74),
over the registry the server embeds.

The expected values come from the record's source, not from the record:
Kimi Code starts a turn when a background Bash command started with
run_in_background=true ends, its output while it runs starts none, and
without disable_timeout the command ends timed out
(kimi-code loop-actor.ts#drainDetachedCompletions); its process carries the
title kimi-code and is started as kimi (npm @moonshot-ai/kimi-code
dist/main.mjs#PROCESS_NAME). The method of the record comes before a host
of step 5; that order is checked with a synthetic host, the real pairing
with Zavod is row #83.

Run on its own: python3 internal/handler/assets/runner_tests/entries_exit_w1_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import stat
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402
from entries_stream_w1_test import agent_of  # noqa: E402

RUNNER = pm.RUNNER


def synthetic_host():
    return {"id": "host-s", "about": "synthetic host", "source": "test", "step": 5, "order": 9,
            "kind": "addressed", "blind": False, "detect": {"env": [{"name": "HOST_S_ID"}]},
            "executor": {"type": "argv", "argv": ["host-s", "{env:HOST_S_ID}"], "feed": "stdin", "timeoutSec": 5},
            "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "queued"}],
            "probeUndefined": True, "addressee": {"process": "none"},
            "binaries": {"linux": ["host-s"], "macos": ["host-s"]}}


def registry(kimi_available=True):
    """The embedded registry with the synthetic host; Kimi's own method made
    unavailable (a condition no environment holds) when told."""
    with open(harness.REGISTRY_JSON) as fh:
        doc = json.load(fh)
    doc["channels"].append(synthetic_host())
    if not kimi_available:
        for agent in doc["agents"]:
            if agent["id"] == "kimi":
                agent["methods"][0]["conditions"] = {"env": [{"name": "SKEIN_TEST_NEVER_SET"}]}
    b64 = base64.b64encode(json.dumps(doc).encode("utf-8")).decode("ascii")
    runner = harness.load_runner(registry=b64)
    return runner, runner.load_registry()


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class KimiRecordTest(unittest.TestCase):
    """Recognition, the instruction, the order before a host."""

    def test_the_agent_process(self):
        cases = [([("kimi-code", None)], True), ([("kimi", ["kimi", "--yolo"])], True),
                 ([("node", ["node", "/usr/lib/kimi/dist/main.mjs"])], False), ([("kimi-desktop", None)], False)]
        for chain, found in cases:
            with self.subTest(chain=chain):
                self.assertEqual(agent_of("kimi", chain) is not None, found)

    def test_the_instruction_is_a_background_bash_without_timeout(self):
        box = pm.Box(self)
        line = box.preflight(box.env("kimi"), ["kimi-code"])
        self.assertEqual((line["client"], line["method"], line["channel"]), ("kimi", "exit-on-event", "stdout"))
        for part in ("Bash tool", "run_in_background=true", "disable_timeout=true"):
            self.assertIn(part, line["instruction"])

    def test_the_own_method_comes_before_a_host(self):
        box = pm.Box(self)
        host = os.path.join(box.bin, "host-s")
        with open(host, "w") as fh:
            fh.write("#!/bin/sh\ncat >/dev/null\nexit 0\n")
        os.chmod(host, stat.S_IRWXU)
        env = box.env("kimi", HOST_S_ID="h-1")
        runner, reg = registry()
        line = box.preflight(env, ["kimi-code"], runner=runner, registry=reg)
        self.assertEqual((line["method"], line["channel"]), ("exit-on-event", "stdout"), line)
        runner, reg = registry(kimi_available=False)
        line = box.preflight(env, ["kimi-code"], runner=runner, registry=reg)
        self.assertEqual((line["method"], line["channel"]), ("detached", "host-s"), line)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX working start")
class TwoCyclesTest(unittest.TestCase):
    """Two cycles of exit-on-event in a row: each prints its batch and the
    delivered line; nothing is lost and nothing comes twice."""

    def test_two_cycles_without_loss_or_repeat(self):
        box = pm.Box(self)
        server = pm.page_server(self, [{"notifications": [pm.event(1), pm.event(2)], "cursor": {"ts": "t1", "id": 2}},
                                       {"notifications": [pm.event(3)], "cursor": {"ts": "t2", "id": 3}}])
        line = box.preflight(box.env("kimi"), ["kimi-code"])
        env = box.env("kimi", SKEIN_PUBLIC_URL=server.url(), SKEIN_MONITOR_MODE="run",
                      SKEIN_MONITOR_PREFLIGHT=str(line["preflightId"]))
        seen = []
        for _ in range(2):
            code, lines = box.run(env)
            self.assertEqual(code, 0, lines)
            self.assertEqual((lines[-1]["event"], lines[-1]["reason"]), ("paused", "delivered"), lines)
            self.assertEqual([l["event"] for l in lines].count("paused"), 1)
            seen.append([l["notification"]["id"] for l in lines if l.get("event") == "notification"])
        self.assertEqual(seen, [[1, 2], [3]])
        self.assertTrue(os.path.exists(box.token))


if __name__ == "__main__":
    unittest.main()
