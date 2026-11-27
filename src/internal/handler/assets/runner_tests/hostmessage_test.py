#!/usr/bin/env python3
"""Tests of the host message of an environment without a channel (US-8,
matrix row #107) on a synthetic registry: an environment recognised by its
variables adds its message - a warning that starts with the code of its kind
- to the preflight line and to the first full monitor_started of that result,
and the method chosen is the one chosen without the environment.

The expected values come from US-8 of the spec: such an environment "cannot
wake the agent, the agent's own method works" (host_cannot_wake) or "a
monitor inside is not supported" (host_unsupported); the choice of the
method does not change.

Run on its own: python3 internal/handler/assets/runner_tests/hostmessage_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_proc  # noqa: E402
import harness  # noqa: E402
import preflight_line_test as pl  # noqa: E402

REGISTRY = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": ["not found"]},
    "agents": [
        {"id": "alpha", "process": {"executables": ["alpha"]}, "methods": [
            {"kind": "stream", "conditions": {"env": [{"name": "ALPHA_STREAM"}]}, "instruction": "use the stream"},
            {"kind": "exit-on-event", "conditions": {}, "instruction": "exit on an event"},
        ]},
    ],
    "channels": [],
    "environments": [
        {"id": "env-wake", "name": "Env Wake", "about": "synthetic", "source": "test",
         "detect": {"env": [{"name": "ENV_WAKE_ID"}]}, "hostMessage": "cannot-wake"},
        {"id": "env-inside", "name": "Env Inside", "about": "synthetic", "source": "test",
         "detect": {"env": [{"name": "ENV_INSIDE_ID"}, {"name": "ENV_INSIDE_TOKEN"}]}, "hostMessage": "unsupported"},
    ],
    "shells": {"source": "test", "names": ["sh", "bash"]},
    "zavodEngines": {},
}
RUNNER = harness.load_runner(registry=base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii"))
REG = RUNNER.load_registry()


def codes(warnings):
    """The codes of the host messages among warnings, in order."""
    return [w.split(":", 1)[0] for w in warnings if w.startswith("host_")]


@unittest.skipUnless(os.name == "posix", "the POSIX preflight")
class HostMessageTest(unittest.TestCase):
    """Row #107: the message of an environment without a channel, in both
    lines; the choice as without it."""

    def setUp(self):
        self.box = pl.Box(self)

    def preflight(self, **extra):
        env = self.box.env(**extra)
        settings = RUNNER.read_settings(env, REG, mode="client")
        source, pids = fake_proc.linear(pl.RUNNER_PROC, "bash", "alpha", "sshd")
        placement = RUNNER.place(RUNNER.ancestry(source, pids[0]), REG, settings.record, self_pid=pids[0])
        line = RUNNER.preflight(self.box.token, env, REG, settings, RUNNER.RegistryDetector(), RUNNER.ProbeRunner(),
                                placement)
        started = RUNNER.monitor_started_line(RUNNER.load_preflight(self.box.token), pl.FIXTURE_ISSUE, True)
        return line, started

    def test_each_kind_is_in_the_preflight_line_and_in_monitor_started(self):
        for name, env, code in (
                ("cannot wake", {"ENV_WAKE_ID": "w-1"}, "host_cannot_wake"),
                ("not supported inside", {"ENV_INSIDE_ID": "i-1", "ENV_INSIDE_TOKEN": "t-1"}, "host_unsupported")):
            with self.subTest(case=name):
                line, started = self.preflight(**env)
                self.assertEqual(codes(line["warnings"]), [code])
                self.assertEqual(codes(started["warnings"]), [code])

    def test_the_choice_is_the_one_without_the_environment(self):
        for name, method in (("own stream", {}), ("exit-on-event", {"ALPHA_STREAM": None})):
            with self.subTest(method=name):
                plain, _ = self.preflight(**method)
                inside, _ = self.preflight(ENV_WAKE_ID="w-1", ENV_INSIDE_ID="i-1", ENV_INSIDE_TOKEN="t-1", **method)
                self.assertEqual(codes(inside["warnings"]), ["host_cannot_wake", "host_unsupported"])
                self.assertEqual((inside["method"], inside["channel"], inside["reason"]),
                                 (plain["method"], plain["channel"], plain["reason"]))

    def test_an_environment_whose_variables_are_not_all_set_says_nothing(self):
        for name, env in (("none", {}), ("one of two", {"ENV_INSIDE_ID": "i-1"}), ("empty", {"ENV_WAKE_ID": ""})):
            with self.subTest(case=name):
                line, started = self.preflight(**env)
                self.assertEqual(codes(line["warnings"]), [])
                self.assertEqual(codes(started["warnings"]), [])


if __name__ == "__main__":
    unittest.main()
