#!/usr/bin/env python3
"""Tests of the registry in the runner and of the client mode settings
(runner/registry.py, runner/overrides.py): an invalid override stops the
start with config_invalid naming the variable instead of falling back to the
automatic choice, the ban on blind channels beats a forced blind channel, and
in client mode the record is resolved by SKEIN_CLIENT.

Run on its own: python3 internal/handler/assets/runner_tests/registry_overrides_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

# A registry with two agents, a host and a blind channel. The runner reads
# the registry, it does not validate it, so only the fields it reads matter.
SYNTHETIC = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": ["general record used"]},
    "agents": [
        {"id": "agent-a", "about": "a", "source": "test", "process": {"executables": ["agent-a"]},
         "methods": [], "warnings": ["warning of agent-a"]},
        {"id": "agent-b", "about": "b", "source": "test", "process": {"executables": ["agent-b"]},
         "methods": []},
    ],
    "channels": [
        {"id": "host-x", "step": 5, "order": 1, "kind": "addressed", "blind": False},
        {"id": "blind-y", "step": 7, "order": 1, "kind": "terminal", "blind": True},
    ],
    "shells": {"source": "test", "names": ["sh"]},
    "zavodEngines": {},
}
SYNTHETIC_B64 = base64.b64encode(json.dumps(SYNTHETIC).encode("utf-8")).decode("ascii")


def other_platform():
    """The SKEIN_PLATFORM value the runner does not run on."""
    return "posix" if os.name == "nt" else "windows"


def this_platform():
    return "windows" if os.name == "nt" else "posix"


def load_body(mode, registry=None):
    """The body run the way a loader runs it: the loader mode marker put into
    its namespace before it executes."""
    return harness.load_runner(namespace={"_SKEIN_LOADER_MODE": mode} if mode is not None else None,
                               registry=registry)


class OverridesTest(unittest.TestCase):
    """Matrix row #31 (US-9, US-11)."""

    def setUp(self):
        self.runner = load_body("client", SYNTHETIC_B64)
        self.registry = self.runner.load_registry()

    def env(self, **extra):
        env = {"SKEIN_CLIENT": "agent-a", "SKEIN_PLATFORM": this_platform()}
        env.update(extra)
        return env

    def test_invalid_value_is_config_invalid_naming_the_variable(self):
        cases = [
            ("SKEIN_MONITOR_CHANNEL", {"SKEIN_MONITOR_CHANNEL": "no-such-channel"}),
            ("SKEIN_MONITOR_CHANNEL", {"SKEIN_MONITOR_CHANNEL": "agent-a"}),
            ("SKEIN_MONITOR_BLIND", {"SKEIN_MONITOR_BLIND": "on"}),
            ("SKEIN_MONITOR_BLIND", {"SKEIN_MONITOR_BLIND": "OFF"}),
            ("SKEIN_MONITOR_BLIND", {"SKEIN_MONITOR_BLIND": "1"}),
            ("SKEIN_PLATFORM", {"SKEIN_PLATFORM": other_platform()}),
            ("SKEIN_PLATFORM", {"SKEIN_PLATFORM": "linux"}),
            ("SKEIN_PLATFORM", {"SKEIN_PLATFORM": ""}),
        ]
        for variable, extra in cases:
            # A valid forced channel next to the invalid value: still no
            # settings, so nothing is chosen.
            env = self.env(SKEIN_MONITOR_CHANNEL="host-x")
            env.update(extra)
            with self.subTest(env=env):
                with self.assertRaises(self.runner.ConfigInvalid) as ctx:
                    self.runner.read_settings(env, self.registry)
                self.assertEqual(ctx.exception.stop_reason, "config_invalid")
                self.assertEqual(ctx.exception.variable, variable)
                self.assertIs(ctx.exception.delete_token, False)

    def test_valid_overrides(self):
        for value, forced in (("host-x", "host-x"), ("blind-y", "blind-y"), ("journal", "journal")):
            with self.subTest(channel=value):
                settings = self.runner.read_settings(self.env(SKEIN_MONITOR_CHANNEL=value), self.registry)
                self.assertEqual(settings.channel, value)
                self.assertEqual(settings.forced, forced)
                self.assertIs(settings.blind_off, False)
        settings = self.runner.read_settings(self.env(SKEIN_MONITOR_BLIND="off"), self.registry)
        self.assertIs(settings.blind_off, True)
        self.assertIsNone(settings.forced)
        self.assertEqual(settings.platform, this_platform())

    def test_blind_ban_beats_a_forced_blind_channel(self):
        settings = self.runner.read_settings(
            self.env(SKEIN_MONITOR_CHANNEL="blind-y", SKEIN_MONITOR_BLIND="off"), self.registry)
        self.assertEqual(settings.forced, "journal")
        # The working command repeats the override as given.
        self.assertEqual(settings.channel, "blind-y")
        self.assertTrue([w for w in settings.warnings if "blind-y" in w], settings.warnings)
        # A forced non-blind channel is not touched by the ban.
        settings = self.runner.read_settings(
            self.env(SKEIN_MONITOR_CHANNEL="host-x", SKEIN_MONITOR_BLIND="off"), self.registry)
        self.assertEqual(settings.forced, "host-x")

    def test_record_is_resolved_by_client(self):
        for agent in SYNTHETIC["agents"]:
            with self.subTest(client=agent["id"]):
                settings = self.runner.read_settings(self.env(SKEIN_CLIENT=agent["id"]), self.registry)
                self.assertEqual(settings.record.id, agent["id"])
                self.assertIsNone(settings.record.general)
                self.assertEqual(settings.record.warnings, agent.get("warnings", []))
        settings = self.runner.read_settings(self.env(SKEIN_CLIENT="no-such-agent"), self.registry)
        self.assertEqual(settings.record.id, "general")
        self.assertEqual(settings.record.client, "no-such-agent")
        self.assertEqual(settings.warnings, SYNTHETIC["general"]["warnings"])

    def test_every_id_of_the_built_in_registry_is_its_own_record(self):
        runner = load_body("client")
        registry = runner.load_registry()
        with open(harness.REGISTRY_JSON, "rb") as fh:
            doc = json.loads(fh.read().decode("utf-8"))
        for agent in doc["agents"]:
            with self.subTest(client=agent["id"]):
                settings = runner.read_settings(self.env(SKEIN_CLIENT=agent["id"]), registry)
                self.assertEqual(settings.record.id, agent["id"])
        self.assertTrue(doc["general"]["warnings"])
        settings = runner.read_settings(self.env(SKEIN_CLIENT="no-such-agent-zz"), registry)
        self.assertEqual(settings.record.id, "general")
        self.assertEqual(settings.warnings, doc["general"]["warnings"])

    def test_malformed_client_name_is_config_invalid(self):
        for client in ("", "Agent", "a b", "x" * 65):
            with self.subTest(client=client):
                with self.assertRaises(self.runner.ConfigInvalid) as ctx:
                    self.runner.read_settings(self.env(SKEIN_CLIENT=client), self.registry)
                self.assertEqual(ctx.exception.variable, "SKEIN_CLIENT")

    def test_legacy_mode_reads_no_variable(self):
        env = {"SKEIN_CLIENT": "Bad Name", "SKEIN_PLATFORM": other_platform(),
               "SKEIN_MONITOR_CHANNEL": "no-such-channel", "SKEIN_MONITOR_BLIND": "on"}
        for mode in ("legacy", None):
            with self.subTest(mode=mode):
                runner = load_body(mode, SYNTHETIC_B64)
                settings = runner.read_settings(env, runner.load_registry())
                self.assertIsNone(settings.record)
                self.assertIsNone(settings.platform)
                self.assertIsNone(settings.forced)
                self.assertIs(settings.blind_off, False)


if __name__ == "__main__":
    unittest.main()
