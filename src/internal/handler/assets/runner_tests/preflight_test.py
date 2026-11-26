#!/usr/bin/env python3
"""Tests of the preflight (runner/preflight.py): the method order takes the
first available method and only it, a probe takes part in the choice, the
preflight line carries the working command with the number of the result,
and the preflight neither polls nor holds the runner lock.

Run on its own: python3 internal/handler/assets/runner_tests/preflight_test.py
Standard library of Python 3.6 only.
"""

import base64
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_proc  # noqa: E402
import harness  # noqa: E402

PROBE = [{"type": "argv", "source": "test", "argv": ["probe"]}]


def channel(cid, step, order, blind=False, probe_undefined=False):
    chan = {"id": cid, "step": step, "order": order, "kind": "terminal", "blind": blind, "detect": {}}
    if probe_undefined:
        chan["probeUndefined"] = True
    else:
        chan["probes"] = PROBE
    return chan


REGISTRY = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": ["general record used"]},
    "agents": [
        {"id": "alpha", "process": {"executables": ["alpha"]}, "methods": [
            {"kind": "stream", "conditions": {"env": [{"name": "ALPHA_STREAM"}]}, "instruction": "s"},
            {"kind": "delivery", "channel": "own-q",
             "conditions": {"env": [{"name": "OWN_Q_ID"}], "programs": ["own-cli"]}, "instruction": "d"},
            {"kind": "exit-on-event", "instruction": "e", "conditions": {"settings": [
                {"path": "{env:CFG_DIR}/cfg.json", "format": "json", "key": "a.mode", "equals": "on"}]}},
        ]},
    ],
    "channels": [
        channel("own-q", 3, 1),
        channel("host-1", 5, 1),
        channel("host-2", 5, 2),
        channel("host-u", 5, 3, probe_undefined=True),
        channel("blind-1", 7, 1, blind=True),
    ],
    "shells": {"source": "test", "names": ["sh", "bash"]},
    "zavodEngines": {},
}
REGISTRY_B64 = base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii")
RUNNER = harness.load_runner(registry=REGISTRY_B64)
REG = RUNNER.load_registry()
RUNNER_PROC = ("python3", ["python3", "-c", "_SKEIN_LOADER_MODE = 'client'"])
TOKEN = "skm_" + "P" * 43


class FakeProbe(object):
    """Answers per channel: "available" (default), "unavailable" or "error"
    (the probe raises); records the channels it was asked about."""

    def __init__(self, answers=None):
        self.answers = answers or {}
        self.calls = []

    def run(self, chan, probe, env):
        self.calls.append(chan["id"])
        answer = self.answers.get(chan["id"], "available")
        if answer == "error":
            raise OSError("probe crashed")
        return answer


class FakeDetector(RUNNER.Detector):
    """Hands the preflight the hosts, blind channels and launching host it is
    given; on_hosts runs while the preflight asks for hosts."""

    def __init__(self, hosts=(), blind=(), under=None, on_hosts=None, bound=None, panes=None):
        RUNNER.Detector.__init__(self)
        self._hosts = list(hosts)
        self._blind = list(blind)
        self._under = under
        self.on_hosts = on_hosts
        # Blind channels whose pane binding is confirmed: those it finds,
        # unless told otherwise.
        self._bound = list(blind if bound is None else bound)
        self._panes = panes or {}

    def under_host(self, ctx):
        return self._under

    def hosts(self, ctx):
        if self.on_hosts:
            self.on_hosts()
        return list(self._hosts)

    def blind(self, ctx):
        return list(self._blind)

    def binding(self, ctx, channel_id):
        if channel_id not in self._bound:
            return None
        return RUNNER.Binding(ctx.registry.channel(channel_id), True, pane=self._panes.get(channel_id))


def placement(client="alpha"):
    source, pids = fake_proc.linear(RUNNER_PROC, "bash", "alpha", "sshd")
    return RUNNER.place(RUNNER.ancestry(source, pids[0]), REG, RUNNER.resolve_record(REG, client),
                        self_pid=pids[0])


class Sandbox(object):
    """A token directory, a state home, a PATH with own-cli and a config
    directory whose cfg.json holds a.mode = "on"."""

    def __init__(self, test):
        self.root = tempfile.mkdtemp(prefix="skein-preflight-")
        test.addCleanup(self.cleanup)
        self.token_dir = os.path.join(self.root, "skein-mon-zz-77-0123456789abcdef")
        os.mkdir(self.token_dir, 0o700)
        self.token = os.path.join(self.token_dir, "token")
        with open(self.token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(self.token, 0o600)
        self.state_home = os.path.join(self.root, "state")
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin)
        cli = os.path.join(self.bin, "own-cli")
        with open(cli, "w") as fh:
            fh.write("#!/bin/sh\nexit 0\n")
        os.chmod(cli, stat.S_IRWXU)
        self.cfg = os.path.join(self.root, "cfg")
        os.mkdir(self.cfg)
        with open(os.path.join(self.cfg, "cfg.json"), "w") as fh:
            json.dump({"a": {"mode": "on"}}, fh)

    def cleanup(self):
        for path, _, _ in os.walk(self.root):
            try:
                os.chmod(path, 0o700)
            except OSError:
                pass
        shutil.rmtree(self.root, True)

    def env(self, **extra):
        env = {"HOME": self.root, "XDG_STATE_HOME": self.state_home, "PATH": self.bin,
               "ALPHA_STREAM": "1", "OWN_Q_ID": "q-1", "CFG_DIR": self.cfg,
               "SKEIN_PUBLIC_URL": "http://127.0.0.1:9", "SKEIN_ISSUE": "zz-77", "SKEIN_TOKEN_FILE": self.token,
               "SKEIN_EXCLUDE_SESSION": "sess-1", "SKEIN_RUNNER_SHA256": "ab" * 32,
               "SKEIN_LOADER_SHA256": "cd" * 32, "SKEIN_CLIENT": "alpha", "SKEIN_PLATFORM": "posix"}
        env.update(extra)
        return dict((k, v) for k, v in env.items() if v is not None)

    def preflight_file(self):
        return os.path.join(self.token_dir, "preflight.json")


def settings(client="alpha", channel=None, forced=None, blind_off=False):
    return RUNNER.Settings(RUNNER.resolve_record(REG, client), "posix", channel, forced, blind_off)


def choose(env, detector=None, prober=None, **kw):
    ctx = RUNNER.Context(REG, settings(**kw), env, placement(), detector or FakeDetector(),
                         prober or FakeProbe())
    ctx.identity = RUNNER.identity(ctx.placement, REG, env, [])
    return RUNNER.choose(ctx)


@unittest.skipUnless(os.name == "posix", "the POSIX preflight")
class MethodOrderTest(unittest.TestCase):
    """Matrix row #33 (INV-15, the method order, steps 1-8)."""

    def setUp(self):
        self.box = Sandbox(self)

    def test_first_available_and_only_it(self):
        detector = FakeDetector(hosts=["host-1", "host-2"], blind=["blind-1"])
        # Each row takes one more method away; the next one is chosen.
        steps = [
            ({}, {}, ("stream", "stdout")),
            ({"ALPHA_STREAM": None}, {}, ("detached", "own-q")),
            ({"ALPHA_STREAM": None}, {"own-q": "unavailable"}, ("exit-on-event", "stdout")),
            ({"ALPHA_STREAM": None, "CFG_DIR": os.path.join(self.box.root, "none")},
             {"own-q": "unavailable"}, ("detached", "host-1")),
            ({"ALPHA_STREAM": None, "CFG_DIR": None}, {"own-q": "error", "host-1": "unavailable"},
             ("detached", "host-2")),
            ({"ALPHA_STREAM": None, "OWN_Q_ID": None, "CFG_DIR": None},
             {"host-1": "unavailable", "host-2": "error"}, ("detached", "blind-1")),
            ({"ALPHA_STREAM": None, "OWN_Q_ID": None, "CFG_DIR": None},
             {"host-1": "unavailable", "host-2": "unavailable", "blind-1": "unavailable"}, ("journal", None)),
        ]
        for extra, answers, want in steps:
            with self.subTest(env=extra, answers=answers):
                choice = choose(self.box.env(**extra), detector, FakeProbe(answers))
                self.assertEqual((choice.method, choice.channel), want)

    def test_unmet_conditions_skip_a_method(self):
        cases = [
            ("program not found", {"PATH": os.path.join(self.box.root, "nothing")}),
            ("variable not set", {"OWN_Q_ID": None}),
        ]
        for name, extra in cases:
            with self.subTest(case=name):
                choice = choose(self.box.env(ALPHA_STREAM=None, **extra))
                self.assertEqual(choice.method, "exit-on-event")
        # An unreadable setting (not JSON) or one with another value.
        with open(os.path.join(self.box.cfg, "cfg.json"), "w") as fh:
            fh.write("{not json")
        self.assertEqual(choose(self.box.env(ALPHA_STREAM=None, OWN_Q_ID=None)).method, "journal")
        with open(os.path.join(self.box.cfg, "cfg.json"), "w") as fh:
            json.dump({"a": {"mode": "off"}}, fh)
        self.assertEqual(choose(self.box.env(ALPHA_STREAM=None, OWN_Q_ID=None)).method, "journal")

    def test_probe_takes_part_in_the_choice(self):
        prober = FakeProbe({"own-q": "unavailable"})
        choice = choose(self.box.env(ALPHA_STREAM=None), FakeDetector(hosts=["host-1"]), prober)
        self.assertEqual(choice.method, "exit-on-event")
        self.assertIn("own-q", prober.calls)
        self.assertIn("own-q: probe argv: unavailable", choice.reason)

    def test_undefined_probe_is_available(self):
        prober = FakeProbe({"host-1": "unavailable"})
        choice = choose(self.box.env(ALPHA_STREAM=None, OWN_Q_ID=None, CFG_DIR=None),
                        FakeDetector(hosts=["host-1", "host-u"]), prober)
        self.assertEqual((choice.method, choice.channel), ("detached", "host-u"))
        self.assertNotIn("host-u", prober.calls)

    def test_forced_channel_is_strict(self):
        detector = FakeDetector(hosts=["host-1", "host-2"])
        choice = choose(self.box.env(), detector, channel="host-2", forced="host-2")
        self.assertEqual((choice.method, choice.channel), ("detached", "host-2"))
        choice = choose(self.box.env(), detector, FakeProbe({"host-2": "unavailable"}),
                        channel="host-2", forced="host-2")
        self.assertEqual((choice.method, choice.channel), ("journal", None))
        choice = choose(self.box.env(), detector, channel="journal", forced="journal")
        self.assertEqual((choice.method, choice.channel), ("journal", None))

    def test_forced_blind_channel_needs_its_pane_binding(self):
        # US-7, decision 8: forcing lifts conditions (1) and (2), not the
        # binding to the pane; without it the strict override gives the
        # journal, with it the channel.
        env = self.box.env()
        choice = choose(env, FakeDetector(bound=[]), channel="blind-1", forced="blind-1")
        self.assertEqual((choice.method, choice.channel), ("journal", None))
        self.assertIn("binding", choice.reason)
        choice = choose(env, FakeDetector(bound=["blind-1"]), channel="blind-1", forced="blind-1")
        self.assertEqual((choice.method, choice.channel), ("detached", "blind-1"))

    def test_blind_ban(self):
        choice = choose(self.box.env(ALPHA_STREAM=None, OWN_Q_ID=None, CFG_DIR=None),
                        FakeDetector(blind=["blind-1"]), blind_off=True)
        self.assertEqual(choice.method, "journal")

    def test_agent_under_a_host_skips_own_methods_but_kept_ones(self):
        detector = FakeDetector(hosts=["host-1", "host-2"], under=("host-2", ["stream"]))
        self.assertEqual(choose(self.box.env(), detector).method, "stream")
        choice = choose(self.box.env(ALPHA_STREAM=None), detector)
        self.assertEqual((choice.method, choice.channel), ("detached", "host-2"))
        self.assertIn("not used under host host-2", choice.reason)

    def test_unavailable_probe_gives_its_cause_in_the_line(self):
        # The server answered (the body was loaded), the CLI is found, the
        # probe of the channel says unavailable: the cause is in the line and
        # the next method is chosen.
        env = self.box.env(ALPHA_STREAM=None)
        line = RUNNER.preflight(self.box.token, env, REG, settings(), FakeDetector(),
                                FakeProbe({"own-q": "unavailable"}), placement())
        self.assertEqual(line["method"], "exit-on-event")
        self.assertIn("own-q: probe argv: unavailable", line["reason"])


@unittest.skipUnless(os.name == "posix", "the POSIX preflight")
class PreflightLineTest(unittest.TestCase):
    """Matrix row #34 (R-2, INV-14)."""

    def setUp(self):
        self.box = Sandbox(self)

    def run_preflight(self, env=None, detector=None, **kw):
        return RUNNER.preflight(self.box.token, env or self.box.env(), REG, settings(**kw),
                                detector or FakeDetector(), FakeProbe(), placement())

    def test_line_carries_the_working_command(self):
        # Row #141: the working command repeats the variables, the starter
        # path and both digests of the preflight command, with
        # SKEIN_MONITOR_MODE=run, SKEIN_MONITOR_PREFLIGHT and the overrides.
        line = self.run_preflight(channel="journal", forced="journal", blind_off=True)
        self.assertEqual(sorted(line), ["channel", "client", "command", "event", "instruction", "journal",
                                        "method", "preflightId", "reason", "warnings"])
        self.assertEqual(line["event"], "preflight")
        self.assertEqual(line["preflightId"], 1)
        self.assertEqual(os.path.dirname(line["journal"]),
                         os.path.join(self.box.state_home, "skein-monitor", "journal"))
        variables = ("SKEIN_PUBLIC_URL='http://127.0.0.1:9' SKEIN_ISSUE='zz-77' SKEIN_TOKEN_FILE='%s' "
                     "SKEIN_EXCLUDE_SESSION='sess-1' SKEIN_RUNNER_SHA256='%s' SKEIN_LOADER_SHA256='%s' "
                     "SKEIN_CLIENT='alpha' SKEIN_PLATFORM='posix' python3 '%s'"
                     % (self.box.token, "ab" * 32, "cd" * 32, os.path.join(self.box.token_dir, "starter.py")))
        self.assertEqual(line["command"],
                         "SKEIN_MONITOR_MODE=run SKEIN_MONITOR_PREFLIGHT=1 SKEIN_MONITOR_CHANNEL='journal' "
                         "SKEIN_MONITOR_BLIND='off' " + variables)

    def test_next_preflight_takes_the_next_number(self):
        self.assertEqual(self.run_preflight()["preflightId"], 1)
        line = self.run_preflight()
        self.assertEqual(line["preflightId"], 2)
        self.assertTrue(line["command"].startswith("SKEIN_MONITOR_MODE=run SKEIN_MONITOR_PREFLIGHT=2 "))
        with open(self.box.preflight_file()) as fh:
            saved = json.load(fh)
        self.assertEqual(saved["preflightId"], 2)
        self.assertEqual(saved["method"], line["method"])
        # What the first full monitor_started repeats is saved with it.
        for key in ("reason", "warnings", "journal"):
            self.assertEqual(saved[key], line[key], key)
        self.assertEqual(os.stat(self.box.preflight_file()).st_mode & 0o777, 0o600)

    def test_live_runner_leaves_the_result_unchanged(self):
        self.run_preflight()
        with open(self.box.preflight_file(), "rb") as fh:
            before = fh.read()
        env = self.box.env()
        lock = RUNNER.PosixPlatform.acquire_runner_lock(self.box.token, 0, env)
        self.addCleanup(lock.release)
        with self.assertRaises(RUNNER.Stop) as ctx:
            self.run_preflight(env)
        self.assertEqual(ctx.exception.reason, "already_running")
        with open(self.box.preflight_file(), "rb") as fh:
            self.assertEqual(fh.read(), before)

    def test_preflight_does_not_hold_the_runner_lock(self):
        env = self.box.env()
        taken = []

        def take_lock():
            lock = RUNNER.PosixPlatform.acquire_runner_lock(self.box.token, 0, env)
            taken.append(True)
            lock.release()

        self.run_preflight(env, FakeDetector(on_hosts=take_lock))
        self.assertEqual(taken, [True])
        # The working start after the preflight takes it itself.
        RUNNER.PosixPlatform.acquire_runner_lock(self.box.token, 0, env).release()

    def test_the_pane_binding_of_a_blind_channel_is_saved(self):
        # "Addressee and check" p. 3: the working runner checks the agent
        # process in the pane against the binding the preflight confirmed.
        pane = {"server": "/tmp/tmux-1000/default", "session": "$1", "pane": "%3",
                "pid": 4242, "start": "1732600000.25"}
        env = self.box.env(ALPHA_STREAM=None, OWN_Q_ID=None, CFG_DIR=None)
        line = self.run_preflight(env, FakeDetector(blind=["blind-1"], panes={"blind-1": pane}))
        self.assertEqual((line["method"], line["channel"]), ("detached", "blind-1"))
        with open(self.box.preflight_file()) as fh:
            saved = json.load(fh)
        self.assertEqual(saved["binding"], {"channel": "blind-1", "multiplexer": False, "depth": None,
                                            "pane": pane})
        # No blind channel chosen: no binding.
        self.run_preflight()
        with open(self.box.preflight_file()) as fh:
            self.assertIsNone(json.load(fh)["binding"])

    def test_directory_without_write_is_named(self):
        with self.subTest(directory="journal"):
            base = os.path.join(self.box.state_home, "skein-monitor")
            os.makedirs(base, 0o700)
            with open(os.path.join(base, "journal"), "w") as fh:
                fh.write("not a directory")
            with self.assertRaises(RUNNER.ConfigInvalid) as ctx:
                self.run_preflight()
            self.assertIn("journal", ctx.exception.variable)
            os.remove(os.path.join(base, "journal"))
        with self.subTest(directory="ledger"):
            with open(os.path.join(base, "ledger"), "w") as fh:
                fh.write("not a directory")
            line = self.run_preflight()
            self.assertTrue([w for w in line["warnings"] if "ledger" in w], line["warnings"])
        with self.subTest(directory="token"):
            if os.geteuid() == 0:
                # Permission bits do not stop root: a directory that is gone
                # takes no write either.
                token = os.path.join(self.box.root, "gone", "token")
            else:
                os.chmod(self.box.token_dir, 0o500)
                token = self.box.token
            with self.assertRaises(RUNNER.ConfigInvalid) as ctx:
                RUNNER.preflight(token, self.box.env(), REG, settings(), FakeDetector(), FakeProbe(), placement())
            self.assertEqual(ctx.exception.variable, os.path.dirname(token))


@unittest.skipUnless(os.name == "posix", "the POSIX preflight")
class PreflightProcessTest(unittest.TestCase):
    """Matrix rows #34, #141: the preflight as the agent runs it, through the
    starter.py that prepare writes, the served loader and the runner: one
    line, printed before the command returns, and no poll."""

    def test_one_line_and_no_poll(self):
        box = Sandbox(self)
        body = harness.render_runner().encode("utf-8")
        polls = []

        def poll(handler):
            polls.append(handler.path)
            handler.send_body(200, json.dumps(harness.EMPTY).encode("utf-8"))

        srv = harness.LoaderServer(lambda h: h.send_body(200, body), poll)
        self.addCleanup(srv.stop)
        starter = os.path.join(box.token_dir, "starter.py")
        with open(starter, "w") as fh:
            fh.write(harness.starter(client=True))
        env = dict(os.environ)
        env.update(box.env(SKEIN_PUBLIC_URL=srv.url(), SKEIN_RUNNER_SHA256=hashlib.sha256(body).hexdigest(),
                           SKEIN_LOADER_SHA256=hashlib.sha256(harness.served_loader().encode("utf-8")).hexdigest(),
                           SKEIN_CLIENT="no-such-agent", SKEIN_MONITOR_MODE="preflight",
                           PATH=os.environ.get("PATH", "")))
        proc = subprocess.run([sys.executable, starter], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              env=env, timeout=60, universal_newlines=True)
        lines = [json.loads(l) for l in proc.stdout.splitlines() if l.strip()]
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(len(lines), 1, lines)
        self.assertEqual(lines[0]["event"], "preflight")
        self.assertEqual(lines[0]["method"], "journal")
        # The general record is named by the client field, not by a warning
        # text (SC-012).
        self.assertEqual(lines[0]["client"], "general")
        self.assertEqual(srv.loader_hits, 1)
        self.assertEqual(srv.script_hits, 1)
        self.assertEqual(polls, [])
        self.assertFalse(os.path.exists(os.path.join(box.token_dir, "state.json")))
        self.assertTrue(os.path.exists(box.preflight_file()))


if __name__ == "__main__":
    unittest.main()
