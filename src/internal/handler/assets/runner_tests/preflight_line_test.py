#!/usr/bin/env python3
"""Tests of the preflight line (runner/preflight.py) and of the start mode
(runner/start.py) with synthetic records: the line names the record used,
carries the instruction of the chosen method only and a working command
that is short and sets every variable it reads; SKEIN_MONITOR_MODE is read
in client mode only, where it must be preflight or run.

Run on its own: python3 internal/handler/assets/runner_tests/preflight_line_test.py
Standard library of Python 3.6 only.
"""

import base64
import binascii
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_proc  # noqa: E402
import harness  # noqa: E402

REGISTRY = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": ["WARN-general-4f19"]},
    "agents": [
        {"id": "alpha", "process": {"executables": ["alpha"]}, "warnings": ["WARN-alpha-3e07"], "methods": [
            {"kind": "stream", "conditions": {"env": [{"name": "ALPHA_STREAM"}]},
             "instruction": "INSTR-alpha-stream-1c52"},
            {"kind": "exit-on-event", "conditions": {}, "instruction": "INSTR-alpha-exit-2d88"},
        ]},
        # Two methods of one kind: the instruction is that of the one taken.
        {"id": "beta", "process": {"executables": ["beta"]}, "methods": [
            {"kind": "stream", "conditions": {"env": [{"name": "BETA_FIRST"}]}, "instruction": "INSTR-beta-first-7a01"},
            {"kind": "stream", "conditions": {"env": [{"name": "BETA_SECOND"}]},
             "instruction": "INSTR-beta-second-7a02"},
        ]},
    ],
    "channels": [],
    "shells": {"source": "test", "names": ["sh", "bash"]},
    "zavodEngines": {},
}
RUNNER = harness.load_runner(registry=base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii"))
REG = RUNNER.load_registry()
RUNNER_PROC = ("python3", ["python3", "-c", "_SKEIN_LOADER_MODE = 'client'"])

# The fixed input of the measurement (cmd/api/measure_tools_test.go): the
# public URL, an issue id, a session and both digests; the token path has the
# server's form /tmp/skein-mon-<id>-<16 hex>/token.
FIXTURE_URL = "https://skein.example.test"
FIXTURE_ISSUE = "sk-42"
FIXTURE_SESSION = "mcp-session-0123456789abcdef0123456789abcdef"
FIXTURE_RUNNER_SHA = "0" * 64
FIXTURE_LOADER_SHA = "1" * 64


class Box(object):
    """A token directory of the server's path form, a state home and the
    environment of a preflight command for a client name."""

    def __init__(self, test):
        suffix = binascii.hexlify(os.urandom(8)).decode("ascii")
        self.token_dir = "/tmp/skein-mon-%s-%s" % (FIXTURE_ISSUE, suffix)
        os.mkdir(self.token_dir, 0o700)
        self.root = tempfile.mkdtemp(prefix="skein-line-")
        test.addCleanup(shutil.rmtree, self.token_dir, True)
        test.addCleanup(shutil.rmtree, self.root, True)
        self.token = os.path.join(self.token_dir, "token")
        with open(self.token, "w") as fh:
            fh.write("skm_" + "L" * 43)
        os.chmod(self.token, 0o600)
        self.state_home = os.path.join(self.root, "state")

    def env(self, client="alpha", **extra):
        env = {"HOME": self.root, "XDG_STATE_HOME": self.state_home, "PATH": os.environ.get("PATH", ""),
               "ALPHA_STREAM": "1",
               "SKEIN_PUBLIC_URL": FIXTURE_URL, "SKEIN_ISSUE": FIXTURE_ISSUE, "SKEIN_TOKEN_FILE": self.token,
               "SKEIN_EXCLUDE_SESSION": FIXTURE_SESSION, "SKEIN_RUNNER_SHA256": FIXTURE_RUNNER_SHA,
               "SKEIN_LOADER_SHA256": FIXTURE_LOADER_SHA, "SKEIN_CLIENT": client, "SKEIN_PLATFORM": "posix",
               "SKEIN_MONITOR_MODE": "preflight"}
        env.update(extra)
        return dict((k, v) for k, v in env.items() if v is not None)

    def preflight(self, client="alpha", **extra):
        env = self.env(client, **extra)
        settings = RUNNER.read_settings(env, REG, mode="client")
        source, pids = fake_proc.linear(RUNNER_PROC, "bash", "alpha", "sshd")
        placement = RUNNER.place(RUNNER.ancestry(source, pids[0]), REG, settings.record, self_pid=pids[0])
        return RUNNER.preflight(self.token, env, REG, settings, RUNNER.Detector(), RUNNER.ProbeRunner(), placement)


# A python3 that runs nothing: prints its arguments and every SKEIN_*
# variable it inherited as one JSON object.
FAKE_PYTHON = """#!/bin/sh
exec %s -c 'import json, os, sys; print(json.dumps({"argv": sys.argv[1:], "env": dict((k, v) for k, v in os.environ.items() if k.startswith("SKEIN_"))}))' "$@"
""" % sys.executable


@unittest.skipUnless(os.name == "posix", "the POSIX preflight")
class PreflightLineTest(unittest.TestCase):
    """Matrix row #149 (C2, INV-17, SC-012)."""

    def setUp(self):
        self.box = Box(self)

    def test_the_instruction_of_the_chosen_method_only(self):
        for extra, method, own, other in (
                ({}, "stream", "INSTR-alpha-stream-1c52", "INSTR-alpha-exit-2d88"),
                ({"ALPHA_STREAM": None}, "exit-on-event", "INSTR-alpha-exit-2d88", "INSTR-alpha-stream-1c52")):
            with self.subTest(method=method):
                line = self.box.preflight(**extra)
                self.assertEqual(sorted(line), ["channel", "client", "command", "event", "instruction",
                                                "journal", "method", "preflightId", "reason", "warnings"])
                self.assertEqual((line["client"], line["method"]), ("alpha", method))
                self.assertIn(own, line["instruction"])
                self.assertIn("WARN-alpha-3e07", line["instruction"])
                self.assertNotIn(other, line["instruction"])
                self.assertNotIn(line["journal"], line["instruction"])
                self.assertNotIn(os.path.dirname(line["journal"]), line["instruction"])

    def test_the_instruction_of_the_method_taken_among_two_of_a_kind(self):
        line = self.box.preflight(client="beta", BETA_SECOND="1")
        self.assertEqual((line["client"], line["method"]), ("beta", "stream"))
        self.assertIn("INSTR-beta-second-7a02", line["instruction"])
        self.assertNotIn("INSTR-beta-first-7a01", line["instruction"])

    def test_the_general_record_is_its_client_value(self):
        line = self.box.preflight(client="no-such-agent")
        self.assertEqual(line["client"], "general")
        self.assertNotIn("WARN-general-4f19", json.dumps(line))

    @harness.subprocess_runner
    def test_the_working_command_is_short_and_self_contained(self):
        line = self.box.preflight()
        command = line["command"]
        self.assertLessEqual(len(command.encode("utf-8")), 1024, command)
        bin_dir = tempfile.mkdtemp(prefix="skein-line-bin-")
        self.addCleanup(shutil.rmtree, bin_dir, True)
        fake = os.path.join(bin_dir, "python3")
        with open(fake, "w") as fh:
            fh.write(FAKE_PYTHON)
        os.chmod(fake, stat.S_IRWXU)
        # What an earlier command left in the shell: the preflight mode, a
        # foreign number and override, and mint parameters of another monitor
        # (review of wave 8, row 11: they must not reach the reissue).
        env = dict((k, v) for k, v in os.environ.items() if not k.startswith("SKEIN_"))
        env.update({"PATH": bin_dir + os.pathsep + os.environ.get("PATH", ""),
                    "SKEIN_MONITOR_MODE": "preflight", "SKEIN_MONITOR_PREFLIGHT": "99",
                    "SKEIN_MONITOR_CHANNEL": "tmux", "SKEIN_MONITOR_BLIND": "off",
                    "SKEIN_SUBTREE": "1", "SKEIN_EVENT_TYPES": "issue.updated"})
        out = subprocess.check_output(["sh", "-c", command], env=env, universal_newlines=True, timeout=30)
        seen = json.loads(out)
        self.assertEqual(seen["argv"], [os.path.join(self.box.token_dir, "starter.py")])
        got = seen["env"]
        want = dict((k, v) for k, v in self.box.env().items() if k.startswith("SKEIN_"))
        want.update({"SKEIN_MONITOR_MODE": "run", "SKEIN_MONITOR_PREFLIGHT": str(line["preflightId"]),
                     "SKEIN_MONITOR_CHANNEL": "", "SKEIN_MONITOR_BLIND": "",
                     "SKEIN_SUBTREE": self.box.env().get("SKEIN_SUBTREE", ""),
                     "SKEIN_EVENT_TYPES": self.box.env().get("SKEIN_EVENT_TYPES", "")})
        self.assertEqual(got, want)
        self.assertNotIn("subtree", RUNNER.mint_call(FIXTURE_ISSUE, env=got))
        # The runner reads it as a working start with these values.
        self.assertEqual(RUNNER.monitor_mode(got, loader_mode="client"), RUNNER.MODE_RUN)
        settings = RUNNER.read_settings(got, REG, mode="client")
        self.assertIsNone(settings.channel)
        self.assertFalse(settings.blind_off)


@unittest.skipUnless(os.name == "posix", "the POSIX preflight")
class MonitorModeTest(unittest.TestCase):
    """Matrix row #150 (SKEIN_MONITOR_MODE)."""

    def setUp(self):
        self.box = Box(self)

    def start(self, loader_mode, mode):
        """start() in process with the given loader mode and
        SKEIN_MONITOR_MODE (None: unset); returns (code, what ran, lines)."""
        ran = []
        lines = []
        env = self.box.env(SKEIN_MONITOR_MODE=None)
        if mode is not None:
            env["SKEIN_MONITOR_MODE"] = mode
        with mock.patch.dict(os.environ, env, clear=True), \
                mock.patch.object(RUNNER, "LOADER_MODE", loader_mode), \
                mock.patch.object(RUNNER, "run", lambda: ran.append("run") or 0), \
                mock.patch.object(RUNNER, "run_preflight", lambda: ran.append("preflight") or 0), \
                mock.patch.object(RUNNER, "emit", lines.append):
            code = RUNNER.start()
        return code, ran, lines

    def test_client_mode_takes_preflight_or_run(self):
        self.assertEqual(self.start("client", "preflight"), (0, ["preflight"], []))
        self.assertEqual(self.start("client", "run"), (0, ["run"], []))

    def test_client_mode_refuses_any_other_value(self):
        for mode in (None, "", "bogus", "Run", "preflight "):
            with self.subTest(mode=mode):
                code, ran, lines = self.start("client", mode)
                self.assertEqual((code, ran), (1, []))
                self.assertEqual(len(lines), 1, lines)
                self.assertEqual((lines[0]["event"], lines[0]["reason"]), ("closed", "config_invalid"))
                self.assertIn("SKEIN_MONITOR_MODE", lines[0]["action"])
                self.assertFalse(os.path.exists(os.path.join(self.box.token_dir, "preflight.json")))
                self.assertTrue(os.path.exists(self.box.token), "the token was not kept")

    def test_legacy_mode_does_not_read_it(self):
        for mode in (None, "bogus", "preflight", "run"):
            with self.subTest(mode=mode):
                self.assertEqual(self.start("legacy", mode), (0, ["run"], []))

    @harness.subprocess_runner
    def test_a_bootstrap_without_client_ignores_it(self):
        # The bootstrap without client, end to end: the starter text, the
        # served loader and the rendered runner. Whatever SKEIN_MONITOR_MODE
        # says, the first line is the legacy monitor_started.
        runner = harness.render_runner(poll_sec=1).encode("utf-8")
        srv = harness.LoaderServer(lambda h: h.send_body(200, runner))
        self.addCleanup(srv.stop)
        env = dict(os.environ)
        for name in list(env):
            if name.startswith("SKEIN_"):
                del env[name]
        env.update({"SKEIN_PUBLIC_URL": srv.url(), "SKEIN_ISSUE": FIXTURE_ISSUE, "SKEIN_TOKEN_FILE": self.box.token,
                    "SKEIN_EXCLUDE_SESSION": "", "SKEIN_RUNNER_SHA256": hashlib.sha256(runner).hexdigest(),
                    "SKEIN_LOADER_SHA256": hashlib.sha256(harness.served_loader().encode("utf-8")).hexdigest(),
                    "SKEIN_MONITOR_MODE": "bogus", "XDG_STATE_HOME": self.box.state_home})
        proc = subprocess.Popen([sys.executable, "-c", harness.starter(client=False)], stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=env, universal_newlines=True)
        try:
            first = json.loads(proc.stdout.readline())
        finally:
            proc.kill()
            proc.wait()
            proc.stdout.close()
            proc.stderr.close()
        self.assertEqual(sorted(first), ["event", "issue", "pollIntervalSec"])
        self.assertEqual(first["event"], "monitor_started")


if __name__ == "__main__":
    unittest.main()
