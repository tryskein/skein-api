#!/usr/bin/env python3
"""Tests of the blind channel mechanism (runner/blind.py with the delivery
pipeline and the detection): matrix rows #102 and #123, on a synthetic
blind record and a fake process source.

The expected values come from the spec, US-7 and "Addressee and check"
items 3-4: before the first transfer an agent that ended (the pane's shell
still alive under the same root) is another addressee - no input; an agent
that is not in the foreground of its pane (suspended) is a refusal before
send; after the insert any failed check before Enter gives "unknown" for
the event without Enter, and another process gives "addressee changed" for
the next events; access lost after the insert (401/403): no Enter, "unknown"
without the preview (OST-2-4). A channel without a source of the pane PID
binds nothing; of two nested multiplexers whose bindings are confirmed the
one nearest the runner is used. A forced blind channel lifts conditions (1)
and (2) of US-7, never the binding nor the checks before insert and Enter
(decision 8).

Run on its own: python3 internal/handler/assets/runner_tests/blind_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402
import fake_channel  # noqa: E402
import fake_proc  # noqa: E402
import harness  # noqa: E402

RUNNER_PROC = ("python3", ["python3", "-c", "_SKEIN_LOADER_MODE = 'client'"])
PANE = "%3"
OK = {"exit": 0}


def argv(*a):
    return {"type": "argv", "argv": list(a), "feed": "stdin", "timeoutSec": 2}


def mux(cid="mux", program="mux", order=1, pid_source=True):
    """A synthetic blind multiplexer of the shape of the tmux record: a
    stage that carries the text, the insert, an unstage and a submit, the
    pane PID by a command of its program."""
    rec = {"id": cid, "about": cid, "source": "test", "step": 7, "order": order, "kind": "terminal",
           "blind": True, "multiplexer": True, "detect": {"env": [{"name": "MUX_PANE"}]},
           "stage": argv(program, "load", "{delivery}"),
           "executor": argv(program, "paste", "{delivery}", "{env:MUX_PANE}"),
           "unstage": argv(program, "drop", "{delivery}"),
           "submit": {"executor": argv(program, "enter", "{env:MUX_PANE}"),
                      "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}]},
           "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}],
           "probeUndefined": True, "addressee": {"process": "foreground"},
           "binaries": {"linux": [program], "macos": [program]}, "address": {"env": "MUX_PANE"}}
    if pid_source:
        rec["panePid"] = {"argv": [program, "pid", "{env:MUX_PANE}"]}
    return rec


HOST = {"id": "host", "about": "host", "source": "test", "step": 5, "order": 1, "kind": "addressed", "blind": False,
        "detect": {"env": [{"name": "HOST_ID"}]}, "executor": argv("hostcli", "send"), "idle": {"type": "none"},
        "outcomes": [{"match": {"exitCode": 0}, "outcome": "queued"}], "probeUndefined": True,
        "addressee": {"process": "none"}, "binaries": {"linux": ["hostcli"], "macos": ["hostcli"]}}


class Box(object):
    """A user directory with a bin directory of fake programs and the runner
    loaded with a registry of the agent alpha (its own stream when
    ALPHA_STREAM is set), a host and the given blind channels."""

    def __init__(self, test, channels):
        self.root = tempfile.mkdtemp(prefix="skein-blind-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin, 0o700)
        self.progs = {}
        for c in channels + [HOST]:
            name = c["executor"]["argv"][0]
            if name not in self.progs:
                self.progs[name] = fake_channel.FakeProgram(self.bin, name)
                self.progs[name].respond([OK])
        registry = {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
                    "agents": [{"id": "alpha", "process": {"executables": ["alpha"]}, "methods": [
                        {"kind": "stream", "conditions": {"env": [{"name": "ALPHA_STREAM"}]},
                         "instruction": "use the stream"}]}],
                    "channels": [HOST] + channels, "shells": {"source": "t", "names": ["sh", "bash"]},
                    "zavodEngines": {}}
        self.runner = harness.load_runner(registry=base64.b64encode(json.dumps(registry).encode()).decode())
        self.reg = self.runner.load_registry()
        self.env = fake_channel.with_path({"PATH": os.environ.get("PATH", os.defpath), "HOME": self.root,
                                           "XDG_STATE_HOME": os.path.join(self.root, "state"),
                                           "MUX_PANE": PANE}, self.bin)
        self.clock = dt.Clock()

    def calls(self, program="mux", sub=None):
        return [c["argv"] for c in self.progs[program].calls() if sub is None or c["argv"][:1] == [sub]]

    # --- the choice (preflight) ------------------------------------------------

    def choose(self, procs, pane_pids, forced=None, **env):
        """The Choice of the method order for the runner under procs (names,
        the runner first), the pane PID command of each channel answering
        the pid of the process at the index pane_pids names."""
        source, pids = fake_proc.linear(RUNNER_PROC, *procs)
        for program, index in pane_pids.items():
            self.progs[program].respond([{"exit": 0, "stdout": "%d\n" % pids[index]}])
        r = self.runner
        record = r.resolve_record(self.reg, "alpha")
        pl = r.place(r.ancestry(source, pids[0]), self.reg, record, self_pid=pids[0])
        e = dict(self.env, **env)
        e = dict((k, v) for k, v in e.items() if v is not None)
        settings = r.Settings(record=record, platform="posix", channel=forced, forced=forced)
        detector = r.RegistryDetector()
        ctx = r.Context(self.reg, settings, e, pl, detector, r.ProbeRunner())
        ctx.identity = r.identity(pl, self.reg, e, detector.address_specs(ctx), self_pid=pids[0])
        return r.choose(ctx)

    # --- the delivery (the working runner) -------------------------------------

    def monitor(self, procs, agent, name="tok-a", cancel=None, channel="mux"):
        """The DeliverySink of the working runner over the preflight result
        of channel with the agent process agent [pid, start]; procs is the
        fake process source the platform layer answers by."""
        token_dir = os.path.join(self.root, name)
        os.mkdir(token_dir, 0o700)
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write(dt.TOKEN)
        os.chmod(token, 0o600)
        identity = {"process": list(agent), "inherited": [], "terminal": None,
                    "addresses": [{"channel": channel, "value": PANE, "qualifiers": [], "env": "MUX_PANE"}]}
        # The binding the preflight confirmed: the pane, its process and
        # its terminal (the tty all fake processes sit on by default).
        binding = {"channel": channel, "multiplexer": True, "depth": 2,
                   "pane": {"id": PANE, "pid": 4241, "agent": list(agent), "tty": 1}}
        doc = {"method": "detached", "channel": channel, "fallbacks": [], "identity": identity, "preflightId": 1,
               "binding": binding}
        r = self.runner
        journal = r.Journal(token, dt.ISSUE, secrets=[dt.TOKEN], env=self.env)
        ledger = r.Ledger(dt.SERVER, name, r.identity_from_doc(identity), env=self.env, clock=self.clock.now)
        spy = SubSpy(r.ArgvExecutor(token, self.env, [dt.TOKEN]), self.clock)
        sink = r.DeliverySink(journal, token, doc, self.reg, self.env, ledger, spy, dt.ISSUE,
                              cancel=cancel or dt.FakeCancel(), clock=self.clock)
        mon = dt.Monitor(self, sink, spy, journal, token)
        patch = mock.patch.object(r.PosixPlatform, "process_info", staticmethod(procs.info))
        patch.start()
        mon.stop_patch = patch.stop
        return mon


class SubSpy(dt.Spy):
    """The spy whose hooks get the subcommand of the call (the program is
    the same for every step of the record)."""

    def call(self, spec, text, timeout):
        sub = (spec["argv"][1:2] or [""])[0]
        if self.before is not None:
            self.before(sub)
        self.calls.append((spec["argv"][0], self.clock.now(), text))
        result = self.inner.call(spec, text, timeout)
        if self.after is not None:
            self.after(sub)
        return result


def pane(agent_tpgid=None):
    """A pane: its shell (pid 4241) and the agent process alpha (pid 4242)
    in the foreground of the pane's tty, unless agent_tpgid says another
    foreground group."""
    procs = fake_proc.FakeProcesses()
    procs.add(4241, 1, "bash", start="s-shell", pgid=4241, tpgid=4242 if agent_tpgid is None else agent_tpgid)
    procs.add(4242, 4241, "alpha", start="s-agent", pgid=4242, tpgid=4242 if agent_tpgid is None else agent_tpgid)
    return procs


AGENT = (4242, "s-agent")


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class BlindAddresseeTest(unittest.TestCase):
    """Row #102: the checks of the addressee before the insert and before
    Enter, and what each failure gives; SC-003 "leaving the foreground
    before the insert and after a successful insert"."""

    def setUp(self):
        self.box = Box(self, [mux()])

    def mon(self, procs, **kw):
        mon = self.box.monitor(procs, AGENT, **kw)
        self.addCleanup(mon.stop_patch)
        return mon

    def test_one_transfer_is_stage_insert_enter_under_one_name(self):
        mon = self.mon(pane())
        mon.page([dt.note(1)])
        calls = self.box.calls()
        self.assertEqual([c[0] for c in calls], ["load", "paste", "enter"])
        self.assertEqual(calls[0][1], calls[1][1])
        self.assertNotIn("{delivery}", calls[0][1])
        self.assertIn("notification=\"1\"", self.box.progs["mux"].calls()[0]["stdin"])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "input-written")])

    def test_an_agent_that_ended_in_a_live_shell_gets_no_input(self):
        procs = pane()
        procs.end(4242)
        mon = self.mon(procs)
        mon.page([dt.note(1)])
        mon.page([dt.note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
        self.assertEqual(self.box.calls(), [])
        self.assertIsNotNone(procs.info(4241))
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown"), (2, "journal")])
        self.assertEqual(len(mon.degraded("addressee_changed")), 1)

    def test_a_suspended_agent_is_a_refusal_before_send(self):
        # Suspended, or out of the foreground before the insert: the shell's
        # group holds the foreground of the pane; nothing is typed.
        mon = self.mon(pane(agent_tpgid=4241))
        mon.page([dt.note(1)])
        self.assertEqual(self.box.calls(), [])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "refused-before-send")])

    def test_leaving_the_foreground_during_the_stage_inserts_nothing(self):
        # K1 of wave 13 round 1: the last check comes after the stage and
        # right before the insert. The agent leaves the foreground while
        # the text is being staged: no insert, the staged text is removed.
        procs = pane()
        mon = self.mon(procs)

        def after(sub):
            if sub == "load":
                procs.procs[4242].tpgid = procs.procs[4241].tpgid = 4241
        mon.spy.after = after
        mon.page([dt.note(1)])
        self.assertEqual([c[0] for c in self.box.calls()], ["load", "drop"])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "refused-before-send")])

    def test_an_agent_on_another_terminal_of_the_pane_gets_nothing(self):
        # K3 of wave 13 round 1 (P-8): the foreground of the agent's own
        # terminal is not the pane's. An agent in an editor's terminal
        # (nvim :terminal, vterm) on another PTY inside the pane is not its
        # addressee: no stage, no insert, no Enter.
        procs = fake_proc.FakeProcesses()
        procs.add(4241, 1, "bash", start="s-shell", pgid=4241, tpgid=4241, tty=1)
        procs.add(4242, 4241, "alpha", start="s-agent", pgid=4242, tpgid=4242, tty=2)
        mon = self.mon(procs)
        mon.page([dt.note(1)])
        self.assertEqual(self.box.calls(), [])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown")])
        self.assertEqual(len(mon.degraded("addressee_changed")), 1)

    def test_another_process_before_enter_withholds_it(self):
        procs = pane()
        mon = self.mon(procs)

        def after(sub):
            if sub == "paste":
                procs.end(4242)
                procs.add(4242, 4241, "alpha", start="s-other", pgid=4242, tpgid=4242)
        mon.spy.after = after
        mon.page([dt.note(1)])
        mon.spy.after = None
        mon.page([dt.note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
        self.assertEqual([c[0] for c in self.box.calls()], ["load", "paste"])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown"), (2, "journal")])
        self.assertEqual(len(mon.degraded("addressee_changed")), 1)

    def test_leaving_the_foreground_after_the_insert_withholds_enter_for_that_event(self):
        procs = pane()
        mon = self.mon(procs)

        def after(sub):
            if sub == "paste":
                procs.procs[4242].tpgid = procs.procs[4241].tpgid = 4241
        mon.spy.after = after
        mon.page([dt.note(1)])
        self.assertEqual([c[0] for c in self.box.calls()], ["load", "paste"])
        # The same process back in the foreground: the next event is typed.
        mon.spy.after = None
        procs.procs[4242].tpgid = procs.procs[4241].tpgid = 4242
        mon.page([dt.note(2)], cursor={"ts": "2026-01-01T00:00:01Z", "id": 8})
        self.assertEqual([c[0] for c in self.box.calls()], ["load", "paste", "load", "paste", "enter"])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown"), (2, "input-written")])
        self.assertEqual(mon.degraded("addressee_changed"), [])

    def test_access_lost_after_the_insert_withholds_enter_without_the_preview(self):
        for reason in ("token_expired", "visibility_lost"):
            with self.subTest(reason=reason):
                box = self.box = Box(self, [mux()])
                cancel = dt.FlagCancel()
                mon = self.mon(pane(), cancel=cancel)

                def after(sub):
                    if sub == "paste":
                        cancel.stop = reason
                mon.spy.after = after
                with self.assertRaises(box.runner.Stop) as stop:
                    mon.page([dt.note(1)])
                self.assertEqual(stop.exception.reason, reason)
                self.assertEqual([c[0] for c in box.calls()], ["load", "paste"])
                self.assertEqual(mon.deliveries(), [(1, "unknown", "mux", False)])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class BlindBindingTest(unittest.TestCase):
    """Row #102: a channel without a source of the pane PID binds nothing;
    of two nested multiplexers with confirmed bindings the nearest is used."""

    def test_the_binding_is_confirmed_by_the_pane_pid(self):
        box = Box(self, [mux()])
        # runner <- alpha <- bash (the pane's process, index 2)
        choice = box.choose(["alpha", "bash"], {"mux": 2})
        self.assertEqual((choice.method, choice.channel), ("detached", "mux"))

    def test_a_channel_without_a_pid_source_is_not_bound(self):
        box = Box(self, [mux(pid_source=False)])
        choice = box.choose(["alpha", "bash"], {})
        self.assertEqual((choice.method, choice.channel), ("journal", None))
        self.assertIn("mux: its record gives no source of the pane PID", choice.reason)
        self.assertEqual(box.calls(), [])

    def test_an_agent_on_another_terminal_is_not_bound(self):
        # The pane's process is an ancestor, but the agent runs on another
        # PTY inside it: the binding is not confirmed.
        box = Box(self, [mux()])
        source, pids = fake_proc.linear(RUNNER_PROC, ("bash", None), ("alpha", None), ("bash", None))
        source.procs[pids[2]].tty = 2
        box.progs["mux"].respond([{"exit": 0, "stdout": "%d\n" % pids[3]}])
        r = box.runner
        record = r.resolve_record(box.reg, "alpha")
        pl = r.place(r.ancestry(source, pids[0]), box.reg, record, self_pid=pids[0])
        settings = r.Settings(record=record, platform="posix")
        detector = r.RegistryDetector()
        ctx = r.Context(box.reg, settings, box.env, pl, detector, r.ProbeRunner())
        binding = detector.binding(ctx, box.reg.channel("mux"))
        self.assertFalse(binding.confirmed)
        self.assertIn("terminal of its pane", binding.cause)

    def test_only_a_confirmed_binding_gives_an_address(self):
        # Warning 6 of wave 13 round 1 (INV-1): an inherited pane variable
        # without a binding confirmed is no address of the agent, so it
        # does not merge two agents' records in the ledger.
        for name, pane_index, has in (("bound", 3, True), ("the runner is not in the pane", None, False)):
            with self.subTest(case=name):
                box = Box(self, [mux()])
                source, pids = fake_proc.linear(RUNNER_PROC, ("bash", None), ("alpha", None), ("bash", None))
                pane_pid = pids[pane_index] if pane_index is not None else 77
                box.progs["mux"].respond([{"exit": 0, "stdout": "%d\n" % pane_pid}])
                r = box.runner
                record = r.resolve_record(box.reg, "alpha")
                pl = r.place(r.ancestry(source, pids[0]), box.reg, record, self_pid=pids[0])
                ctx = r.Context(box.reg, r.Settings(record=record, platform="posix"), box.env, pl,
                                r.RegistryDetector(), r.ProbeRunner())
                channels = [spec.channel for spec in ctx.detector.address_specs(ctx)]
                self.assertEqual("mux" in channels, has, channels)

    def test_a_pane_pid_the_command_does_not_give_binds_nothing(self):
        for name, answer in (("failed", {"exit": 1}), ("no PID", {"exit": 0, "stdout": "\n"}),
                             ("not an ancestor", {"exit": 0, "stdout": "77\n"})):
            with self.subTest(case=name):
                box = Box(self, [mux()])
                box.progs["mux"].respond([answer])
                choice = box.choose(["alpha", "bash"], {})
                self.assertEqual((choice.method, choice.channel), ("journal", None))
                self.assertEqual(box.calls(sub="load"), [])

    def test_the_nearest_of_two_confirmed_multiplexers(self):
        # runner <- alpha <- bash (pane of mux-b) <- bash (pane of mux-a):
        # mux-a comes first in the registry, mux-b is the nearer one.
        box = Box(self, [mux("mux-a", "mux-a", 1), mux("mux-b", "mux-b", 2)])
        choice = box.choose(["alpha", "bash", "bash"], {"mux-a": 3, "mux-b": 2})
        self.assertEqual((choice.method, choice.channel), ("detached", "mux-b"))
        self.assertNotIn("mux-a", choice.fallbacks)
        # The other order of nesting: mux-a is the nearer one.
        box = Box(self, [mux("mux-a", "mux-a", 1), mux("mux-b", "mux-b", 2)])
        choice = box.choose(["alpha", "bash", "bash"], {"mux-a": 2, "mux-b": 3})
        self.assertEqual(choice.channel, "mux-a")


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class ForcedBlindTest(unittest.TestCase):
    """Row #123: forcing lifts conditions (1) and (2), never the binding nor
    the checks before the insert and before Enter."""

    def test_forcing_lifts_the_own_method_and_the_detected_host(self):
        box = Box(self, [mux()])
        env = {"ALPHA_STREAM": "1", "HOST_ID": "h-1"}
        self.assertEqual(box.choose(["alpha", "bash"], {"mux": 2}, **env).method, "stream")
        choice = box.choose(["alpha", "bash"], {"mux": 2}, forced="mux", **env)
        self.assertEqual((choice.method, choice.channel), ("detached", "mux"))

    def test_forcing_without_a_confirmed_binding_types_nothing(self):
        box = Box(self, [mux()])
        # The pane's process is not an ancestor of the runner.
        box.progs["mux"].respond([{"exit": 0, "stdout": "77\n"}])
        source_env = {"ALPHA_STREAM": "1", "HOST_ID": "h-1"}
        choice = box.choose(["alpha", "bash"], {}, forced="mux", **source_env)
        self.assertEqual((choice.method, choice.channel), ("journal", None))
        self.assertIn("binding to its pane is not confirmed", choice.reason)
        self.assertEqual(box.calls(sub="load") + box.calls(sub="paste"), [])

    def test_the_checks_before_the_insert_and_before_enter_stay(self):
        # The monitor of a forced blind channel (no fallbacks): an agent out
        # of the foreground gets nothing; another process after the insert
        # gets no Enter.
        box = Box(self, [mux()])
        mon = box.monitor(pane(agent_tpgid=4241), AGENT)
        self.addCleanup(mon.stop_patch)
        mon.page([dt.note(1)])
        self.assertEqual(box.calls(), [])
        box = Box(self, [mux()])
        procs = pane()
        mon = box.monitor(procs, AGENT)
        self.addCleanup(mon.stop_patch)

        def after(sub):
            if sub == "paste":
                procs.end(4242)
        mon.spy.after = after
        mon.page([dt.note(1)])
        self.assertEqual([c[0] for c in box.calls()], ["load", "paste"])
        self.assertEqual([d[:2] for d in mon.deliveries()], [(1, "unknown")])


if __name__ == "__main__":
    unittest.main()
