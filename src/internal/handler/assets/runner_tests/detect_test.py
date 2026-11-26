#!/usr/bin/env python3
"""Tests of the detection of hosts (steps 5-6) and of blind channels
(runner/detect.py) with synthetic records, the fake process source, a fake
binding to panes and a fake probe: a blind channel is enabled only under the
three conditions of US-7, a detected host is detected even when its channel
is unavailable, and two terminal environments are ambiguous.

Run on its own: python3 internal/handler/assets/runner_tests/detect_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_proc  # noqa: E402
import harness  # noqa: E402

PROBE = [{"type": "argv", "source": "test", "argv": ["probe"]}]


def channel(cid, step, order, env=None, blind=False):
    return {"id": cid, "step": step, "order": order, "kind": "terminal", "blind": blind,
            "detect": {"env": [{"name": n} for n in (env or [])]}, "probes": PROBE,
            "binaries": {"linux": ["sh"], "macos": ["sh"]}}


REGISTRY = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": ["general record used"]},
    "agents": [
        {"id": "alpha", "process": {"executables": ["alpha"]},
         "methods": [{"kind": "stream", "conditions": {"env": [{"name": "ALPHA_STREAM"}]}, "instruction": "x"}]},
        {"id": "beta", "process": {"executables": ["beta"]}, "methods": []},
    ],
    "channels": [
        channel("zavod", 5, 1, ["ZAVOD_ME"]),
        channel("host-a", 5, 2, ["HOST_A_ID"]),
        channel("term-x", 6, 1, ["TERM_X_HANDLE"]),
        channel("term-y", 6, 2, ["TERM_Y_ID"]),
        channel("mux-1", 7, 1, blind=True),
        channel("mux-2", 7, 2, blind=True),
        channel("emu-1", 7, 3, blind=True),
    ],
    "shells": {"source": "test", "names": ["sh", "bash"]},
    "zavodEngines": {"claude": "alpha", "kimi": "alpha", "nkimi": "beta"},
}
RUNNER = harness.load_runner(registry=base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii"))
REG = RUNNER.load_registry()
RUNNER_PROC = ("python3", ["python3", "-c", "_SKEIN_LOADER_MODE = 'client'"])


class FakeProbe(object):
    """Answers per channel id (available by default) and records each call."""

    def __init__(self, answers=None):
        self.answers = answers or {}
        self.calls = []

    def run(self, chan, probe, env):
        self.calls.append(chan["id"])
        return self.answers.get(chan["id"], RUNNER.PROBE_AVAILABLE)


class FakeBinder(object):
    """Binds the channels it is given: {id: (confirmed, multiplexer, depth)}
    or {id: (confirmed, multiplexer, depth, descendant)}."""

    def __init__(self, bindings=None):
        self.bindings = bindings or {}

    def bind(self, ctx, chan):
        spec = self.bindings.get(chan["id"])
        if spec is None:
            return None
        confirmed, multiplexer, depth = spec[:3]
        descendant = spec[3] if len(spec) > 3 else None
        return RUNNER.Binding(chan, confirmed, multiplexer, depth, descendant=descendant)


def context(env, client="alpha", bindings=None, answers=None, nested=False, blind_off=False):
    procs = ["bash", "alpha", "bash", "alpha"] if nested else ["bash", "alpha", "sshd"]
    source, pids = fake_proc.linear(RUNNER_PROC, *procs)
    record = RUNNER.resolve_record(REG, client)
    placement = RUNNER.place(RUNNER.ancestry(source, pids[0]), REG, record, self_pid=pids[0])
    settings = RUNNER.Settings(record=record, platform="posix", blind_off=blind_off)
    detector = RUNNER.RegistryDetector(FakeBinder(bindings))
    prober = FakeProbe(answers)
    ctx = RUNNER.Context(REG, settings, env, placement, detector, prober)
    ctx.identity = RUNNER.identity(placement, REG, env, [], self_pid=pids[0])
    return ctx


BOUND = {"emu-1": (True, False, None)}

# The agent's own Zavod address: a UUID, the same in ZAVOD_ME and
# ZAVOD_AGENT_ID (US-5).
ZAVOD_UUID = "0f4c2a8e-3b1d-4e57-9a62-5d8b7c1e2f30"
ZAVOD_SELF = {"ZAVOD_ME": ZAVOD_UUID, "ZAVOD_AGENT_ID": ZAVOD_UUID}


class BlindConditionsTest(unittest.TestCase):
    """Matrix row #35 (INV-3, US-7)."""

    def test_all_three_conditions_enable_the_blind_channel(self):
        choice = RUNNER.choose(context({}, bindings=BOUND))
        self.assertEqual((choice.method, choice.channel), ("detached", "emu-1"))

    def test_an_available_own_method_keeps_blind_off(self):
        choice = RUNNER.choose(context({"ALPHA_STREAM": "1"}, bindings=BOUND))
        self.assertEqual((choice.method, choice.channel), ("stream", "stdout"))

    def test_a_detected_host_with_an_unavailable_channel_keeps_blind_off(self):
        ctx = context({"HOST_A_ID": "a-1"}, bindings=BOUND, answers={"host-a": RUNNER.PROBE_UNAVAILABLE})
        choice = RUNNER.choose(ctx)
        self.assertEqual((choice.method, choice.channel), ("journal", None))
        self.assertIn("host-a", choice.reason)
        self.assertEqual(ctx.detector.blind(ctx), [])
        self.assertNotIn("emu-1", ctx.prober.calls)

    def test_an_unconfirmed_binding_keeps_blind_off(self):
        # TMUX is set, but the runner is not bound to an agent process in a pane.
        for bindings in ({"emu-1": (False, False, None)}, {"mux-1": (False, True, 1)}, {}):
            with self.subTest(bindings=bindings):
                ctx = context({"TMUX": "/tmp/tmux-1/default,1,0"}, bindings=bindings)
                choice = RUNNER.choose(ctx)
                self.assertEqual((choice.method, choice.channel), ("journal", None))
                self.assertEqual(ctx.detector.blind(ctx), [])

    def test_a_forced_blind_channel_needs_a_confirmed_binding(self):
        for bindings, want in (({}, ("journal", None)), ({"emu-1": (False, False, None)}, ("journal", None)),
                               (BOUND, ("detached", "emu-1"))):
            with self.subTest(bindings=bindings):
                ctx = context({}, bindings=bindings)
                ctx.settings = RUNNER.Settings(record=ctx.settings.record, platform="posix",
                                               channel="emu-1", forced="emu-1")
                choice = RUNNER.choose(ctx)
                self.assertEqual((choice.method, choice.channel), want)

    def test_ban_disables_blind(self):
        choice = RUNNER.choose(context({}, bindings=BOUND, blind_off=True))
        self.assertEqual((choice.method, choice.channel), ("journal", None))


class HostDetectionTest(unittest.TestCase):
    """Matrix row #35: detected = variables + not nested + the engine map."""

    def test_variables_of_a_nested_agent_detect_nothing(self):
        ctx = context({"HOST_A_ID": "a-1", "TERM_X_HANDLE": "t-1"}, nested=True, bindings=BOUND)
        self.assertEqual(ctx.detector.hosts(ctx), [])
        # Not detected, so condition (2) holds: the blind channel decides.
        self.assertEqual(ctx.detector.blind(ctx), ["emu-1"])

    def test_zavod_by_the_engine_map(self):
        cases = [("alpha", "kimi", True), ("alpha", "claude", True), ("alpha", "nkimi", False),
                 ("alpha", "unknown", False), ("alpha", "", False), ("no-such-client", "nkimi", True)]
        for client, engine, detected in cases:
            with self.subTest(client=client, engine=engine):
                env = dict(ZAVOD_SELF)
                if engine:
                    env["ZAVOD_AGENT_ENGINE_ID"] = engine
                ctx = context(env, client=client)
                self.assertEqual("zavod" in ctx.detector.hosts(ctx), detected)

    def test_zavod_needs_its_own_uuid_address_and_the_cli(self):
        empty = tempfile.mkdtemp(prefix="skein-detect-path-")
        self.addCleanup(shutil.rmtree, empty, True)
        base = dict(ZAVOD_SELF, ZAVOD_AGENT_ENGINE_ID="kimi")
        cases = [
            ("own UUID address, CLI found", {}, True),
            ("ZAVOD_ME not a UUID", {"ZAVOD_ME": "z-1", "ZAVOD_AGENT_ID": "z-1"}, False),
            ("ZAVOD_ME of another agent", {"ZAVOD_AGENT_ID": "5e1d9c3a-7b2f-4a68-8d10-2c4b6e8f0a91"}, False),
            ("no ZAVOD_AGENT_ID", {"ZAVOD_AGENT_ID": None}, False),
            ("no CLI on PATH", {"PATH": empty}, False),
        ]
        for name, extra, detected in cases:
            with self.subTest(case=name):
                env = dict(base, **extra)
                env = dict((k, v) for k, v in env.items() if v is not None)
                ctx = context(env)
                self.assertEqual("zavod" in ctx.detector.hosts(ctx), detected)

    def test_a_forced_zavod_passes_the_detection_checks(self):
        # Forced Zavod whose engine does not match the client, or whose
        # address is not the agent's own: the journal, not Zavod.
        for name, env, want in (
                ("engine of another record", dict(ZAVOD_SELF, ZAVOD_AGENT_ENGINE_ID="nkimi"), ("journal", None)),
                ("address of another agent", {"ZAVOD_ME": ZAVOD_UUID, "ZAVOD_AGENT_ENGINE_ID": "kimi"},
                 ("journal", None)),
                ("this agent", dict(ZAVOD_SELF, ZAVOD_AGENT_ENGINE_ID="kimi"), ("detached", "zavod"))):
            with self.subTest(case=name):
                ctx = context(env)
                ctx.settings = RUNNER.Settings(record=ctx.settings.record, platform="posix",
                                               channel="zavod", forced="zavod")
                choice = RUNNER.choose(ctx)
                self.assertEqual((choice.method, choice.channel), want)

    def test_order_of_hosts(self):
        ctx = context(dict(ZAVOD_SELF, HOST_A_ID="a-1", ZAVOD_AGENT_ENGINE_ID="kimi", TERM_X_HANDLE="t-1"))
        self.assertEqual(ctx.detector.hosts(ctx), ["zavod", "host-a", "term-x"])
        choice = RUNNER.choose(ctx)
        self.assertEqual(choice.channel, "zavod")
        self.assertEqual(choice.fallbacks, ["host-a", "term-x"])


class BlindOrderTest(unittest.TestCase):
    """Matrix row #35: the nearest multiplexer first; other blind channels and
    step 6 skipped inside a multiplexer pane."""

    def test_nearest_multiplexer_first_and_emulators_skipped(self):
        bindings = {"mux-1": (True, True, 3), "mux-2": (True, True, 2), "emu-1": (True, False, None)}
        ctx = context({}, bindings=bindings)
        self.assertEqual(ctx.detector.blind(ctx), ["mux-2"])
        self.assertEqual(RUNNER.choose(ctx).channel, "mux-2")

    def test_step_6_is_skipped_inside_a_multiplexer_pane(self):
        ctx = context({"TERM_X_HANDLE": "t-1"}, bindings={"mux-1": (True, True, 2)})
        self.assertEqual(ctx.detector.hosts(ctx), [])
        self.assertEqual(RUNNER.choose(ctx).channel, "mux-1")
        # A descendant of a tmux pane whose binding is not confirmed: step 6
        # and the other blind channels are still skipped, and the
        # multiplexer is not used.
        ctx = context({"TERM_X_HANDLE": "t-1"},
                      bindings={"mux-1": (False, True, 2, True), "emu-1": (True, False, None)})
        self.assertEqual(ctx.detector.hosts(ctx), [])
        self.assertEqual(ctx.detector.blind(ctx), [])
        # Not in any multiplexer pane: step 6 is reached.
        ctx = context({"TERM_X_HANDLE": "t-1"}, bindings={"mux-1": (False, True, 2, False)})
        self.assertEqual(ctx.detector.hosts(ctx), ["term-x"])


class AmbiguousTerminalTest(unittest.TestCase):
    """Matrix row #36 (SC-002 "two terminal environments", US-6)."""

    def test_two_terminal_environments(self):
        ctx = context({"TERM_X_HANDLE": "t-1", "TERM_Y_ID": "t-2"}, bindings=BOUND)
        choice = RUNNER.choose(ctx)
        self.assertEqual((choice.method, choice.channel), ("journal", None))
        self.assertEqual(ctx.detector.degraded,
                         {"cause": "ambiguous_terminal", "variables": ["TERM_X_HANDLE", "TERM_Y_ID"]})
        warning = [w for w in ctx.detector.warnings if "ambiguous_terminal" in w]
        self.assertEqual(len(warning), 1)
        self.assertIn("TERM_X_HANDLE", warning[0])
        self.assertIn("TERM_Y_ID", warning[0])
        # No channel of the two environments and no blind channel was touched.
        self.assertEqual(ctx.prober.calls, [])
        self.assertEqual(ctx.detector.blind(ctx), [])

    def test_no_ambiguity_before_step_6(self):
        # An own method available: steps 5-6 are not reached, so no
        # ambiguous_terminal warning advises an override that would turn
        # the stream into detached.
        ctx = context({"TERM_X_HANDLE": "t-1", "TERM_Y_ID": "t-2", "ALPHA_STREAM": "1"}, bindings=BOUND)
        choice = RUNNER.choose(ctx)
        self.assertEqual((choice.method, choice.channel), ("stream", "stdout"))
        self.assertIsNone(ctx.detector.degraded)
        self.assertEqual([w for w in ctx.detector.warnings if "ambiguous_terminal" in w], [])


if __name__ == "__main__":
    unittest.main()
