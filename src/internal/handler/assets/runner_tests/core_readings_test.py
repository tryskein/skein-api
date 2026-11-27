#!/usr/bin/env python3
"""Tests of four readings of the runner core on synthetic records, before
the records that need them (Orca, Paseo):

- the answer sign: a decimal element of a dotted path indexes a list, and a
  path may be required to be absent (registry.go#Match.JSONAbsent), so two
  receipts that differ in the length of a list give two outcomes;
- the addressee check whose values the preflight captures
  (registry.go#AddresseeCommand.Capture): the preflight saves them, and a
  transfer goes only while the check gives them again;
- a program named by a variable ("{env:NAME}" in binaries);
- a channel that serves only some agent records
  (registry.go#Conditions.Records), and a literal phrase on stderr as an
  answer sign (registry.go#Match.StderrContains);
- a host that launches agents (registry.go#Channel.Launcher): an agent it
  launched skips its own methods but the kept ones and the host goes first
  at step 5; a nested agent, the host's own terminal and the variables
  without the host's process are not launched by it.

Run on its own: python3 internal/handler/assets/runner_tests/core_readings_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import stat
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as delivery  # noqa: E402
import fake_channel  # noqa: E402
import fake_proc  # noqa: E402
import harness  # noqa: E402

TOKEN = "skm_" + "R" * 43
PROBE = [{"type": "argv", "source": "test", "argv": ["probe"]}]
SH = {"linux": ["sh"], "macos": ["sh"]}
SHOW = {"argv": ["show", "{env:HOST_AGENT_ID}"],
        "results": [{"match": {"exitCode": 0}, "result": "same"}],
        "capture": ["result.terminal.incarnationId", "result.terminal.agentIdentity"]}

REGISTRY = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": ["general record used"]},
    "agents": [
        {"id": "alpha", "process": {"executables": ["alpha"]},
         "methods": [{"kind": "stream", "conditions": {}, "instruction": "x"}]},
        {"id": "beta", "process": {"executables": ["beta"]},
         "methods": [{"kind": "delivery", "channel": "own-q", "conditions": {"env": [{"name": "BETA_THREAD"}]},
                      "instruction": "x"}]},
        {"id": "gamma", "process": {"executables": ["gamma"]}, "methods": []},
    ],
    "channels": [
        {"id": "own-q", "step": 3, "order": 1, "kind": "addressed", "blind": False,
         "detect": {"env": [{"name": "BETA_THREAD"}]}, "probes": PROBE, "binaries": SH,
         "addressee": {"process": "none"},
         "address": {"env": "BETA_THREAD", "perProcess": True}},
        {"id": "first", "step": 5, "order": 1, "kind": "addressed", "blind": False,
         "detect": {"env": [{"name": "FIRST_ID"}]}, "probes": PROBE, "binaries": SH,
         "addressee": {"process": "none"}, "address": {"env": "FIRST_ID"}},
        {"id": "launch", "step": 5, "order": 2, "kind": "addressed", "blind": False,
         "detect": {"env": [{"name": "HOST_AGENT_ID"}]}, "probes": PROBE, "binaries": SH,
         "addressee": {"process": "none", "command": SHOW}, "address": {"env": "HOST_AGENT_ID"},
         "launcher": {"process": {"executables": ["host-daemon"]}, "keeps": {"alpha": ["stream"]}}},
        # One terminal environment with a channel for gamma and one for the
        # other records (cmux: agent message for Claude, paste for others).
        {"id": "msg6", "step": 6, "order": 1, "kind": "addressed", "blind": False,
         "detect": {"env": [{"name": "HOST6_ID"}], "records": ["gamma"]}, "probes": PROBE, "binaries": SH,
         "addressee": {"process": "none"}, "address": {"env": "HOST6_ID"}},
        {"id": "paste6", "step": 6, "order": 2, "kind": "addressed", "blind": False,
         "detect": {"env": [{"name": "HOST6_ID"}], "recordsAbsent": ["gamma"]}, "probes": PROBE,
         "binaries": SH, "addressee": {"process": "none"}, "address": {"env": "HOST6_ID"}},
    ],
    "shells": {"source": "test", "names": ["sh", "bash"]},
    "zavodEngines": {},
}
RUNNER = harness.load_runner(registry=base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii"))
REG = RUNNER.load_registry()
RUNNER_PROC = ("python3", ["python3", "-c", "_SKEIN_LOADER_MODE = 'client'"])
SHOW_ANSWER = {"result": {"terminal": {"incarnationId": "inc-1", "agentIdentity": "codex"}}}
CAPTURED = [["result.terminal.incarnationId", "inc-1"], ["result.terminal.agentIdentity", "codex"]]


class FakeProbe(object):
    """Every probe available; capture answers what it is given (a list of
    values or an exception to raise)."""

    def __init__(self, capture=None):
        self.captured = capture if capture is not None else CAPTURED

    def run(self, chan, probe, env):
        return RUNNER.PROBE_AVAILABLE

    def capture(self, chan, env):
        if isinstance(self.captured, Exception):
            raise self.captured
        return self.captured


def context(procs, client, env, prober=None):
    """A context over a fake chain (the runner first) with the identity the
    registry detector's address specs give."""
    source, pids = fake_proc.linear(RUNNER_PROC, *procs)
    record = RUNNER.resolve_record(REG, client)
    placement = RUNNER.place(RUNNER.ancestry(source, pids[0]), REG, record, self_pid=pids[0])
    settings = RUNNER.Settings(record=record, platform="posix")
    detector = RUNNER.RegistryDetector()
    ctx = RUNNER.Context(REG, settings, env, placement, detector, prober or FakeProbe())
    ctx.identity = RUNNER.identity(placement, REG, env, detector.address_specs(ctx), self_pid=pids[0])
    return ctx


# Receipts in the form of an Orca terminal send (a list of stages): the
# rows tell the two lengths apart under one exit code.
STAGES = [
    {"match": {"exitCode": 0, "json": {"result.prompt.stages.0": "input_accepted",
                                       "result.prompt.stages.1": "turn_started"}},
     "outcome": "turn-started"},
    {"match": {"exitCode": 0, "json": {"result.prompt.stages.0": "input_accepted"},
               "jsonAbsent": ["result.prompt.stages.1"]},
     "outcome": "input-written"},
]


def receipt(stages):
    return {"result": {"prompt": {"stages": stages}}}


class ListAndAbsenceSignTest(unittest.TestCase):
    """A list element by its index and the absence of a path in the closed
    table of a channel (classification rule, item 1)."""

    def outcome(self, doc, exit_code=0):
        return RUNNER.classify(STAGES, exit_code=exit_code, doc=doc)[0]

    def test_the_second_element_tells_the_turn_from_the_input(self):
        self.assertEqual(self.outcome(receipt(["input_accepted", "turn_started"])), "turn-started")
        self.assertEqual(self.outcome(receipt(["input_accepted"])), "input-written")

    def test_answers_outside_both_rows_are_unknown(self):
        cases = {
            "an empty list": receipt([]),
            "another first stage": receipt(["queued"]),
            "a list of another second stage": receipt(["input_accepted", "observed"]),
            "no list but a string": {"result": {"prompt": {"stages": "input_accepted"}}},
            "no prompt": {"result": {}},
            "no JSON document": None,
        }
        for name, doc in cases.items():
            with self.subTest(case=name):
                self.assertEqual(self.outcome(doc), "unknown")
        self.assertEqual(self.outcome(receipt(["input_accepted"]), exit_code=1), "unknown")

    def test_only_ascii_digits_index_a_list(self):
        doc = {"stages": ["input_accepted", "turn_started"]}
        self.assertEqual(RUNNER.json_path_lookup(doc, "stages.1"), (True, "turn_started"))
        for element in ("\u00b2", "\u0661", "+1", " 1"):
            with self.subTest(element=element):
                self.assertEqual(RUNNER.json_path_lookup(doc, "stages." + element), (False, None))

    def test_an_object_with_the_key_0_is_no_list(self):
        # Round 2 of the wave 11 review: an element read as an index reads
        # only a list; an object with the key "0" proves no stage.
        doc = {"result": {"prompt": {"stages": {"0": "input_accepted"}}}}
        self.assertEqual(self.outcome(doc), "unknown")
        self.assertEqual(RUNNER.json_path_lookup(doc, "result.prompt.stages.0"), (False, None))

    def test_absence_needs_a_json_answer(self):
        rows = [{"match": {"exitCode": 0, "jsonAbsent": ["warning"]}, "outcome": "queued"}]
        self.assertEqual(RUNNER.classify(rows, exit_code=0, doc={"ok": True})[0], "queued")
        self.assertEqual(RUNNER.classify(rows, exit_code=0, doc={"warning": "stale"})[0], "unknown")
        self.assertEqual(RUNNER.classify(rows, exit_code=0, doc=None)[0], "unknown")


class StderrJsonSignTest(unittest.TestCase):
    """The JSON document a program wrote to its stderr as a sign (a CLI that
    reports its errors there): only that document carries it, not stdout."""

    ROWS = [{"match": {"exitCode": 1, "stderrJson": {"error.code": "DAEMON_UNREACHABLE"}},
             "outcome": "refused-before-send"}]

    def outcome(self, stdout_doc, stderr):
        return RUNNER.classify(self.ROWS, exit_code=1, doc=stdout_doc, stderr=stderr)[0]

    def test_the_code_on_stderr_is_the_sign(self):
        err = json.dumps({"error": {"code": "DAEMON_UNREACHABLE", "message": "x"}}, indent=2).encode()
        self.assertEqual(self.outcome(None, err), "refused-before-send")

    def test_a_stderr_cut_at_the_cap_gives_no_sign(self):
        # A prefix of a longer stderr is not its JSON, its emptiness or its
        # phrase: the answer is unknown.
        err = json.dumps({"error": {"code": "DAEMON_UNREACHABLE"}}).encode()
        rows = self.ROWS + [{"match": {"exitCode": 2, "stderrEmpty": True}, "outcome": "queued"},
                            {"match": {"exitCode": 3, "stderrContains": "nothing was sent"}, "outcome": "refused-before-send"}]
        whole = RUNNER.CallResult(1, b"", err, False)
        self.assertEqual(RUNNER.classify_result(rows, whole)[0], "refused-before-send")
        for code, stderr in ((1, err), (2, b""), (3, b"so nothing was sent")):
            with self.subTest(exit=code):
                cut = RUNNER.CallResult(code, b"", stderr, False, stderr_truncated=True)
                self.assertEqual(RUNNER.classify_result(rows, cut)[0], "unknown")

    def test_other_answers_are_unknown(self):
        cases = {
            "another code": (None, json.dumps({"error": {"code": "SEND_FAILED"}}).encode()),
            "the code on stdout only": ({"error": {"code": "DAEMON_UNREACHABLE"}}, b""),
            "no JSON on stderr": (None, b"Error: Cannot connect to daemon"),
            "no stderr read": (None, None),
        }
        for name, (doc, err) in cases.items():
            with self.subTest(case=name):
                self.assertEqual(self.outcome(doc, err), "unknown")


class ProgramFromVariableTest(unittest.TestCase):
    """A program of a channel named by a variable: the program it names
    when set (a path or a name PATH finds), none when unset."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-readings-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.program = os.path.join(self.root, "host-cli")
        with open(self.program, "w") as fh:
            fh.write("#!/bin/sh\nexit 0\n")
        os.chmod(self.program, stat.S_IRWXU)

    @unittest.skipUnless(os.name == "posix", "POSIX paths")
    def test_the_variable_names_a_path_or_a_name(self):
        name = "{env:HOST_CLI}"
        self.assertEqual(RUNNER.channel_program(name, {"PATH": "/nonexistent", "HOST_CLI": self.program}, None),
                         self.program)
        self.assertEqual(RUNNER.channel_program(name, {"PATH": self.root, "HOST_CLI": "host-cli"}, None),
                         "host-cli")
        self.assertIsNone(RUNNER.channel_program(name, {"PATH": self.root}, None))
        self.assertIsNone(RUNNER.channel_program(name, {"PATH": self.root, "HOST_CLI": "absent-cli"}, None))

    @unittest.skipUnless(os.name == "posix", "POSIX paths")
    def test_the_executor_runs_the_program_the_variable_names(self):
        chan = {"executor": {"argv": ["host", "send"]},
                "binaries": {"linux": ["host", "{env:HOST_CLI}"], "macos": ["host", "{env:HOST_CLI}"]}}
        env = {"PATH": "/nonexistent", "HOST_CLI": self.program}
        self.assertEqual(RUNNER.channel_programs(chan, env, None), {"host": self.program})
        # A name the variable gives runs by the path PATH finds for it: the
        # executor takes only a path.
        env = {"PATH": self.root, "HOST_CLI": "host-cli"}
        self.assertEqual(RUNNER.channel_programs(chan, env, None), {"host": self.program})

    def test_an_unset_variable_is_named_in_the_cause(self):
        cause = RUNNER._missing_programs_cause("host", ["host", "{env:HOST_CLI}"], {"PATH": self.root})
        self.assertIn("HOST_CLI not set", cause)


@unittest.skipUnless(os.name == "posix", "the POSIX probe executor")
class CaptureAtPreflightTest(unittest.TestCase):
    """The preflight runs the addressee check once and saves its values; a
    check that does not answer "same" or lacks a value leaves the channel
    unavailable with the cause."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-capture-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin)
        self.show = fake_channel.FakeProgram(self.bin, "show")
        self.env = fake_channel.with_path({"PATH": os.environ.get("PATH", os.defpath), "HOME": self.root,
                                           "HOST_AGENT_ID": "agent-1"}, self.bin)
        self.chan = REG.channel("launch")

    def test_the_values_of_a_same_answer_are_captured(self):
        self.show.respond([{"exit": 0, "stdout": SHOW_ANSWER}])
        self.assertEqual(RUNNER.SystemProbeRunner().capture(self.chan, self.env), CAPTURED)
        self.assertEqual([c["argv"] for c in self.show.calls()], [["agent-1"]])

    def test_a_path_the_answer_does_not_hold_is_captured_absent(self):
        self.show.respond([{"exit": 0, "stdout": {"result": {"terminal": {"incarnationId": "inc-1"}}}}])
        self.assertEqual(RUNNER.SystemProbeRunner().capture(self.chan, self.env),
                         [["result.terminal.incarnationId", "inc-1"], ["result.terminal.agentIdentity", None]])

    def test_no_same_or_no_json_is_no_capture(self):
        cases = {
            "no same": {"exit": 1, "stdout": SHOW_ANSWER},
            "no JSON": {"exit": 0, "stdout": "not json"},
        }
        for name, answer in cases.items():
            with self.subTest(case=name):
                self.show.respond([answer])
                with self.assertRaises(RUNNER.ProbeError):
                    RUNNER.SystemProbeRunner().capture(self.chan, self.env)

    def test_a_failed_capture_makes_the_channel_unavailable(self):
        env = {"PATH": os.environ.get("PATH", os.defpath), "HOST_AGENT_ID": "agent-1"}
        ctx = context(["bash", "gamma", "host-daemon"], "gamma", env,
                      FakeProbe(RUNNER.ProbeError("the addressee check does not answer same")))
        ok, cause = RUNNER.channel_available(ctx, "launch")
        self.assertFalse(ok)
        self.assertIn("addressee check of launch", cause)
        ctx = context(["bash", "gamma", "host-daemon"], "gamma", env)
        self.assertEqual(RUNNER.channel_available(ctx, "launch"), (True, None))
        self.assertEqual(ctx.captures["launch"], CAPTURED)

    def test_the_preflight_result_keeps_the_values_of_the_chosen_channel(self):
        token_dir = os.path.join(self.root, "skein-mon-zz-77-0123456789abcdef")
        os.mkdir(token_dir, 0o700)
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(token, 0o600)
        env = dict(self.env, XDG_STATE_HOME=os.path.join(self.root, "state"), SKEIN_PUBLIC_URL="http://127.0.0.1:9",
                   SKEIN_ISSUE="zz-77", SKEIN_TOKEN_FILE=token, SKEIN_EXCLUDE_SESSION="sess-1",
                   SKEIN_RUNNER_SHA256="ab" * 32, SKEIN_LOADER_SHA256="cd" * 32, SKEIN_CLIENT="gamma",
                   SKEIN_PLATFORM="posix")
        ctx = context(["bash", "gamma", "host-daemon"], "gamma", env)
        line = RUNNER.preflight(token, env, REG, ctx.settings, RUNNER.RegistryDetector(), FakeProbe(),
                                ctx.placement)
        self.assertEqual(line["channel"], "launch")
        with open(os.path.join(token_dir, "preflight.json")) as fh:
            saved = json.load(fh)
        self.assertEqual(saved["identity"]["captures"], {"launch": CAPTURED})


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class CaptureBeforeTransferTest(unittest.TestCase):
    """Before each transfer the addressee check gives the values the
    preflight saved, else the addressee changed: no call of the channel,
    the event "unknown" and the journal from then on. The check and the idle
    wait run their program as the preflight found it."""

    def world(self, show="show", idle=None):
        chan = delivery.chan("A", "chan-a")
        chan["addressee"] = {"process": "none", "command": {
            "argv": [show], "results": [{"match": {"exitCode": 0}, "result": "same"}],
            "capture": ["inc"]}}
        if idle is not None:
            chan["idle"] = {"type": "argv", "argv": [idle], "timeoutSec": 2}
        world = delivery.World(self, [chan, delivery.chan("B", "chan-b")])
        return world

    def identity(self, captures):
        ident = dict(delivery.ADDRESS)
        if captures is not None:
            ident["captures"] = captures
        return ident

    def test_the_saved_value_again_lets_the_transfer_go(self):
        world = self.world()
        show = fake_channel.FakeProgram(world.bin, "show")
        show.respond([{"exit": 0, "stdout": {"inc": "inc-1"}}])
        mon = world.monitor(identity=self.identity({"A": [["inc", "inc-1"]]}))
        mon.page([delivery.note(1)])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "A")])
        # Before the idle wait and again right before the hand-over.
        self.assertEqual(len(show.calls()), 2)

    def test_another_value_or_none_is_another_addressee(self):
        cases = {
            "another incarnation": ({"A": [["inc", "inc-1"]]}, {"exit": 0, "stdout": {"inc": "inc-2"}}),
            "no value in the answer": ({"A": [["inc", "inc-1"]]}, {"exit": 0, "stdout": {"other": "inc-1"}}),
            "no value saved by the preflight": (None, {"exit": 0, "stdout": {"inc": "inc-1"}}),
            "a value where the preflight saw none": ({"A": [["inc", None]]}, {"exit": 0, "stdout": {"inc": "inc-1"}}),
            # Each kind apart: an object or a list is no absence, null is not
            # the string "null", another object is another value.
            "an object where the preflight saw none": ({"A": [["inc", None]]},
                                                       {"exit": 0, "stdout": {"inc": {"pty": 1}}}),
            "a list where the preflight saw none": ({"A": [["inc", None]]}, {"exit": 0, "stdout": {"inc": [1]}}),
            "null where the preflight saw the string null": ({"A": [["inc", "null"]]},
                                                             {"exit": 0, "stdout": {"inc": None}}),
            "another object": ({"A": [["inc", {"json": '{"pty":1}'}]]}, {"exit": 0, "stdout": {"inc": {"pty": 2}}}),
        }
        for name, (captures, answer) in cases.items():
            with self.subTest(case=name):
                world = self.world()
                fake_channel.FakeProgram(world.bin, "show").respond([answer])
                mon = world.monitor(identity=self.identity(captures))
                mon.page([delivery.note(1)])
                self.assertEqual(world.prog("chan-a").calls(), [])
                self.assertEqual(world.prog("chan-b").calls(), [])
                self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown")])
                self.assertEqual(len(mon.degraded("addressee_changed")), 1)

    def test_a_path_absent_again_lets_the_transfer_go(self):
        world = self.world()
        fake_channel.FakeProgram(world.bin, "show").respond([{"exit": 0, "stdout": {"other": "x"}}])
        mon = world.monitor(identity=self.identity({"A": [["inc", None]]}))
        mon.page([delivery.note(1)])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "A")])

    def test_the_same_object_again_lets_the_transfer_go(self):
        world = self.world()
        fake_channel.FakeProgram(world.bin, "show").respond([{"exit": 0, "stdout": {"inc": {"pty": 1, "gen": 2}}}])
        mon = world.monitor(identity=self.identity({"A": [["inc", {"json": '{"gen":2,"pty":1}'}]]}))
        mon.page([delivery.note(1)])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "A")])

    def test_the_capture_keeps_each_kind_apart(self):
        values = {"s": "x", "n": None, "t": "null", "o": {"a": 1}, "l": [1], "b": False}
        doc = {"v": values}
        self.assertEqual([RUNNER.capture_value(doc, "v." + k) for k in ("s", "n", "t", "o", "l", "b", "gone")],
                         ["x", {"json": "null"}, "null", {"json": '{"a":1}'}, {"json": "[1]"}, {"json": "false"},
                          None])

    def test_the_check_and_the_idle_wait_run_the_programs_of_the_preflight(self):
        world = self.world(show="show", idle="idle")
        real_show = fake_channel.FakeProgram(world.bin, "show-real")
        real_show.respond([{"exit": 0, "stdout": {"inc": "inc-1"}}])
        real_idle = fake_channel.FakeProgram(world.bin, "idle-real")
        mon = world.monitor(identity=self.identity({"A": [["inc", "inc-1"]]}))
        mon.spy.programs = {"show": real_show.path, "idle": real_idle.path}
        mon.page([delivery.note(1)])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "A")])
        self.assertEqual(len(real_show.calls()), 2)
        self.assertEqual(len(real_idle.calls()), 1)


class LauncherTest(unittest.TestCase):
    """A host that launches agents: the agent it launched skips its own
    methods but the kept ones, the host goes first at step 5 (the method
    order, "agent under Paseo"); the host's own terminal keeps the same;
    not launched by it: a nested agent, the variables without the host's
    process."""

    ENV = {"PATH": os.environ.get("PATH", os.defpath), "HOST_AGENT_ID": "agent-1", "FIRST_ID": "first-1",
           "BETA_THREAD": "thread-1"}

    def test_an_agent_launched_by_the_host_skips_its_own_delivery(self):
        ctx = context(["bash", "beta", "host-daemon"], "beta", self.ENV)
        self.assertEqual(ctx.detector.under_host(ctx), ("launch", ()))
        choice = RUNNER.choose(ctx)
        self.assertEqual((choice.method, choice.channel), (RUNNER.METHOD_DETACHED, "launch"))
        self.assertIn("delivery: not used under host launch", choice.reasons)
        # The host of order 1, detected too, comes after it.
        self.assertEqual(choice.fallbacks, ["first"])

    def test_a_kept_method_stays(self):
        ctx = context(["bash", "alpha", "host-daemon"], "alpha", self.ENV)
        self.assertEqual(ctx.detector.under_host(ctx), ("launch", ("stream",)))
        self.assertEqual(RUNNER.choose(ctx).method, RUNNER.METHOD_STREAM)

    def test_not_launched_by_the_host(self):
        cases = {
            # beta under alpha, launched by the host: beta is nested, the
            # host's address belongs to alpha; beta's own thread stays its.
            "a nested agent": (["bash", "beta", "bash", "alpha", "host-daemon"], "beta", "own-q"),
            # The host's variables inherited without its process above.
            "no host process": (["bash", "beta", "sshd"], "beta", "own-q"),
        }
        for name, (procs, client, chosen) in cases.items():
            with self.subTest(case=name):
                ctx = context(procs, client, self.ENV)
                self.assertIsNone(ctx.detector.under_host(ctx))
                self.assertIn("launch", ctx.identity.inherited)
                self.assertEqual(RUNNER.choose(ctx).channel, chosen)

    def test_the_host_terminal_is_its_session_and_keeps_what_the_launcher_names(self):
        # The addressee is the host's session (identity.terminal), and its
        # record keeps only the launcher's keeps, as an agent the host
        # launched (the ACP terminal of Paseo): gamma keeps nothing.
        ctx = context(["bash", "host-daemon"], "gamma", self.ENV)
        self.assertEqual(ctx.identity.terminal, "launch")
        self.assertEqual(ctx.detector.under_host(ctx), ("launch", ()))
        self.assertEqual(RUNNER.choose(ctx).channel, "launch")


class RecordsConditionTest(unittest.TestCase):
    """A channel that serves only some agent records
    (registry.go#Conditions.Records, RecordsAbsent): of two channels of one
    environment exactly one is detected for a record, so they are not an
    ambiguity, and the other is unavailable for it."""

    ENV = {"PATH": os.environ.get("PATH", os.defpath), "HOST6_ID": "surface-1"}

    def choose(self, procs, client):
        ctx = context(procs, client, self.ENV)
        choice = RUNNER.choose(ctx)
        self.assertNotIn(RUNNER.AMBIGUOUS_TERMINAL, json.dumps(list(ctx.detector.warnings)))
        return ctx, choice

    def test_the_named_record_gets_its_channel(self):
        ctx, choice = self.choose(["bash", "gamma", "bash"], "gamma")
        self.assertEqual((choice.method, choice.channel), (RUNNER.METHOD_DETACHED, "msg6"))
        self.assertNotIn("paste6", choice.fallbacks)
        self.assertEqual([a.channel for a in ctx.identity.addresses], ["msg6"])

    def test_every_other_record_gets_the_other_channel(self):
        for procs, client in ((["bash", "beta", "bash"], "beta"), (["bash", "zeta", "bash"], "zeta")):
            with self.subTest(client=client):
                ctx, choice = self.choose(procs, client)
                self.assertEqual((choice.method, choice.channel), (RUNNER.METHOD_DETACHED, "paste6"))
                self.assertNotIn("msg6", choice.fallbacks)
                ok, cause = RUNNER.channel_available(ctx, "msg6")
                self.assertFalse(ok)
                self.assertIn("does not serve the record", cause)


class StderrPhraseSignTest(unittest.TestCase):
    """A literal phrase on the stderr of a CLI that reports its errors only
    as text (registry.go#Match.StderrContains)."""

    ROWS = [{"match": {"exitCode": 1, "stderrContains": "so nothing was sent"}, "outcome": "refused-before-send"},
            {"match": {"exitCode": 1, "stderrContains": "(SESSION_COMPOSER_BUSY)"}, "outcome": "refused-before-send"},
            {"match": {"exitCode": 0}, "outcome": "input-written"}]

    def outcome(self, exit_code, stderr, doc=None):
        return RUNNER.classify(self.ROWS, exit_code=exit_code, doc=doc, stderr=stderr)[0]

    def test_the_phrase_on_stderr_is_the_sign(self):
        self.assertEqual(self.outcome(1, b"Error: paste: the agent prompt in s:1 holds text someone is typing, "
                                         b"so nothing was sent. Retry when the prompt is clear.\n"),
                         "refused-before-send")
        self.assertEqual(self.outcome(1, b"send s-1: composer (SESSION_COMPOSER_BUSY) [request r-1]\n"),
                         "refused-before-send")

    def test_other_answers_are_unknown(self):
        cases = {
            "another text": (1, b"Error: the socket closed\n"),
            "the phrase under another exit code": (2, b"so nothing was sent"),
            "no stderr": (1, b""),
            "both phrases at once": (1, b"so nothing was sent (SESSION_COMPOSER_BUSY)"),
            "the phrase in other words": (1, b"nothing was sent so"),
        }
        for name, (code, err) in cases.items():
            with self.subTest(case=name):
                self.assertEqual(self.outcome(code, err), "unknown")
        # Without a program there is no stderr: the phrase is no sign.
        self.assertEqual(self.outcome(1, None), "unknown")


if __name__ == "__main__":
    unittest.main()
