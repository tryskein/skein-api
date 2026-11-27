#!/usr/bin/env python3
"""Tests of the channel executor and the probes of the preflight
(runner/executors.py, its wiring into runner/preflight.py and the start):
matrix rows #43 and #44.

The channel programs are fake programs on PATH (fake_channel.py) that leave
a marker of every call: argv, stdin, the message files they were given and
the variables they saw.

Run on its own: python3 internal/handler/assets/runner_tests/executors_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import socket
import tempfile
import threading
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
import sys  # noqa: E402

sys.path.insert(0, HERE)

import fake_channel  # noqa: E402
import harness  # noqa: E402

RUNNER = harness.load_runner()
TOKEN = "skm_" + "T" * 43
# A variable of the agent's environment that happens to hold the token.
SECRET_VAR = "SOME_TOOL_SECRET"
# Text that would steer a shell or a program if it were not one element.
HOSTILE = "--flag; rm -rf / $(id) `id` | cat\nsecond line\r\nthird & echo pwned"


class Box(object):
    """A private token directory with its token and a bin directory."""

    def __init__(self, test):
        self.root = tempfile.mkdtemp(prefix="skein-exec-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.token_dir = os.path.join(self.root, "tok")
        os.mkdir(self.token_dir, 0o700)
        self.token = os.path.join(self.token_dir, "token")
        with open(self.token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(self.token, 0o600)
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin, 0o700)

    def env(self, **extra):
        env = fake_channel.with_path({"PATH": os.environ.get("PATH", os.defpath), "HOME": self.root,
                                      "XDG_STATE_HOME": os.path.join(self.root, "state"),
                                      SECRET_VAR: "prefix-" + TOKEN}, self.bin)
        env.update(extra)
        return env

    def messages(self):
        return sorted(n for n in os.listdir(self.token_dir) if n.startswith("msg-"))



def _gone(pid):
    """Ended: no such process, or a zombie no one has reaped yet (an orphan
    in a container whose first process reaps nothing)."""
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return True
    try:
        with open("/proc/%d/stat" % pid) as fh:
            return fh.read().rsplit(")", 1)[1].split()[0] == "Z"
    except (OSError, IndexError):
        return False


def _wait_gone(pid, sec=10):
    deadline = time.monotonic() + sec
    while not _gone(pid) and time.monotonic() < deadline:
        time.sleep(0.05)
    return _gone(pid)


class FakeMonotonic(object):
    """A monotonic clock each reading of which moves it on by a step: the
    deadlines of a call run out after a bounded number of its waits,
    whatever the real time."""

    def __init__(self, step=0.05):
        self.t = 1000.0
        self.step = step

    def __call__(self):
        self.t += self.step
        return self.t


@unittest.skipUnless(os.name == "posix", "the POSIX program group")
class ProgramWaitTest(unittest.TestCase):
    """Review findings C4 and V3 of wave 7 (P-7: a call is bounded by its
    timeout; on timeout the program and the processes of its session are
    killed): a descendant that keeps the program's pipes open holds no call
    past its timeout, and neither it nor the program outlives the call -
    nor a stop signal during it."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-wait-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.pidfile = os.path.join(self.root, "descendant.pid")
        self.addCleanup(self.kill_left)

    def kill_left(self):
        pid = self.descendant(wait=False)
        if pid:
            try:
                os.kill(pid, 9)
            except OSError:
                pass

    def descendant(self, wait=True):
        deadline = time.monotonic() + (10 if wait else 0)
        while True:
            try:
                with open(self.pidfile) as fh:
                    text = fh.read().strip()
                if text:
                    return int(text)
            except OSError:
                pass
            if time.monotonic() >= deadline:
                return None
            time.sleep(0.02)

    def test_a_descendant_holding_the_pipes_after_the_program_ended(self):
        argv = ["sh", "-c", "sleep 60 & echo $! > %s; echo done" % self.pidfile]
        clock = FakeMonotonic()
        with mock.patch.object(RUNNER.time, "monotonic", clock):
            started = clock.t
            result = RUNNER.run_program(argv, dict(os.environ), None, 10)
            took = clock.t - started
        self.assertEqual((result.exit_code, result.stdout.strip()), (0, b"done"))
        # The program ended at once; the wait does not last the descendant's
        # minute, nor the call's timeout of 10 s.
        self.assertLess(took, 10)
        self.assertTrue(_wait_gone(self.descendant()), "a descendant on the pipes outlived the call")

    def test_a_stdout_cut_at_the_cap_is_no_answer_of_the_table(self):
        # Review finding 31 of wave 7: a prefix of a longer answer that
        # happens to parse is not the answer.
        code = "import sys; sys.stdout.write('{\"state\":\"queued\"}' + ' ' * 70000 + 'x')"
        result = RUNNER.run_program([sys.executable, "-c", code], dict(os.environ), None, 10)
        self.assertTrue(result.truncated)
        self.assertIsNone(result.json)

    def test_a_timeout_kills_every_process_of_the_program(self):
        argv = ["sh", "-c", "sleep 60 & echo $! > %s; sleep 60" % self.pidfile]
        clock = FakeMonotonic()
        with mock.patch.object(RUNNER.time, "monotonic", clock):
            started = clock.t
            result = RUNNER.run_program(argv, dict(os.environ), b"text", 1)
            took = clock.t - started
        self.assertTrue(result.timed_out)
        self.assertIsNone(result.exit_code)
        # The timeout and the grace of the readers, on the clock the call
        # reads: not the minute of the program.
        self.assertLess(took, 1 + 5)
        self.assertTrue(_wait_gone(self.descendant()), "a descendant outlived the timeout")

    def test_a_stop_signal_during_the_call_kills_the_program_and_its_descendants(self):
        argv = ["sh", "-c", "echo $$ > %s.prog; sleep 60 & echo $! > %s; sleep 60" % (self.pidfile, self.pidfile)]
        real = RUNNER.select.select

        def select(*args):
            if self.descendant(wait=False):
                raise RUNNER.StopSignal()
            return real(*args)

        with mock.patch.object(RUNNER.select, "select", select):
            with self.assertRaises(RUNNER.StopSignal):
                RUNNER.run_program(argv, dict(os.environ), None, 30)
        with open(self.pidfile + ".prog") as fh:
            program = int(fh.read().strip())
        self.assertTrue(_wait_gone(program), "the program outlived the stop")
        self.assertTrue(_wait_gone(self.descendant()), "a descendant outlived the stop")

    def test_a_descendant_that_left_the_pipes_does_not_outlive_the_call(self):
        # Review round 2 of wave 7, K4: both pipes reach their end before the
        # grace (the descendant redirected them), and the descendant is still
        # killed with the program's group.
        argv = ["sh", "-c", "(exec >/dev/null 2>&1 </dev/null; exec sleep 60) & echo $! > %s; echo done"
                % self.pidfile]
        result = RUNNER.run_program(argv, dict(os.environ), None, 10)
        self.assertEqual((result.exit_code, result.stdout.strip()), (0, b"done"))
        self.assertTrue(_wait_gone(self.descendant()), "a descendant without the pipes outlived the call")

    def test_a_failure_while_setting_up_the_pipes_kills_the_program(self):
        # Review round 2 of wave 7, row 19: the clean-up is armed before the
        # set-up of the pipes.
        started = []
        real_popen = RUNNER.subprocess.Popen

        def popen(*args, **kw):
            proc = real_popen(*args, **kw)
            started.append(proc)
            return proc
        with mock.patch.object(RUNNER.subprocess, "Popen", popen), \
                mock.patch.object(RUNNER.os, "set_blocking", mock.Mock(side_effect=RuntimeError("set-up"))):
            with self.assertRaises(RuntimeError):
                RUNNER.run_program(["sh", "-c", "sleep 60"], dict(os.environ), None, 10)
        self.assertEqual(len(started), 1)
        self.assertIsNotNone(started[0].returncode, "the program outlived a failed set-up")


@unittest.skipUnless(os.name == "posix", "the POSIX process group stands in for the Windows job")
class ThreadWaitTest(ProgramWaitTest):
    """Review round 2 of wave 7, K3: the wait of a platform without select
    on pipes (reader threads), run here on POSIX, where the process tree of
    the call is its session: the call returns within its timeout plus the
    readers' grace whatever the descendants do, and none of them outlives
    it. The cases of ProgramWaitTest run through it."""

    def setUp(self):
        ProgramWaitTest.setUp(self)
        patcher = mock.patch.object(RUNNER, "_run_posix", RUNNER._run_threads)
        patcher.start()
        self.addCleanup(patcher.stop)

    # Review round 3 of wave 7, row 11: this wait blocks on Popen.wait and
    # Thread.join, which read the real clock, so its bounds are taken on
    # the real clock (the timeout of the call, then one grace of the
    # readers), with a margin for a loaded machine - far below the minute a
    # descendant holds the pipes.

    def test_a_descendant_holding_the_pipes_after_the_program_ended(self):
        argv = ["sh", "-c", "sleep 60 & echo $! > %s; echo done" % self.pidfile]
        started = time.monotonic()
        result = RUNNER.run_program(argv, dict(os.environ), None, 10)
        took = time.monotonic() - started
        self.assertEqual((result.exit_code, result.stdout.strip()), (0, b"done"))
        self.assertLess(took, RUNNER._READER_GRACE_SEC + 5)
        self.assertTrue(_wait_gone(self.descendant()), "a descendant on the pipes outlived the call")

    def test_a_timeout_kills_every_process_of_the_program(self):
        argv = ["sh", "-c", "sleep 60 & echo $! > %s; sleep 60" % self.pidfile]
        started = time.monotonic()
        result = RUNNER.run_program(argv, dict(os.environ), b"text", 1)
        took = time.monotonic() - started
        self.assertTrue(result.timed_out)
        self.assertIsNone(result.exit_code)
        self.assertLess(took, 1 + RUNNER._READER_GRACE_SEC + 5)
        self.assertTrue(_wait_gone(self.descendant()), "a descendant outlived the timeout")

    def test_a_stop_signal_during_the_call_kills_the_program_and_its_descendants(self):
        argv = ["sh", "-c", "echo $$ > %s.prog; sleep 60 & echo $! > %s; sleep 60" % (self.pidfile, self.pidfile)]
        real_wait = RUNNER.subprocess.Popen.wait

        def wait(proc, timeout=None):
            # The stop arrives while the call waits for the program.
            if timeout is not None and self.descendant():
                raise RUNNER.StopSignal()
            return real_wait(proc, timeout)

        with mock.patch.object(RUNNER.subprocess.Popen, "wait", wait):
            with self.assertRaises(RUNNER.StopSignal):
                RUNNER.run_program(argv, dict(os.environ), None, 30)
        with open(self.pidfile + ".prog") as fh:
            program = int(fh.read().strip())
        self.assertTrue(_wait_gone(program), "the program outlived the stop")
        self.assertTrue(_wait_gone(self.descendant()), "a descendant outlived the stop")

    def test_a_failure_while_setting_up_the_pipes_kills_the_program(self):
        started = []
        real_popen = RUNNER.subprocess.Popen

        def popen(*args, **kw):
            proc = real_popen(*args, **kw)
            started.append(proc)
            return proc
        with mock.patch.object(RUNNER.subprocess, "Popen", popen), \
                mock.patch.object(RUNNER, "_Reader", mock.Mock(side_effect=RuntimeError("set-up"))):
            with self.assertRaises(RuntimeError):
                RUNNER.run_program(["sh", "-c", "sleep 60"], dict(os.environ), None, 10)
        self.assertEqual(len(started), 1)
        self.assertIsNotNone(started[0].returncode, "the program outlived a failed set-up")



@unittest.skipUnless(os.name == "posix", "the POSIX executor")
class ArgvExecutorTest(unittest.TestCase):
    """Row #43: the text of an event never steers the call (INV-5), the
    token never reaches the program (INV-6), message files are private and
    removed, a call past its timeout is killed."""

    def setUp(self):
        self.box = Box(self)
        self.prog = fake_channel.FakeProgram(self.box.bin, "chan", watch=[SECRET_VAR])
        self.executor = RUNNER.ArgvExecutor(self.box.token, self.box.env(), [TOKEN])

    def call(self, argv, feed, text=HOSTILE, timeout=10):
        return self.executor.call({"type": "argv", "argv": argv, "feed": feed, "timeoutSec": timeout}, text,
                                  timeout)

    def test_the_text_is_exactly_one_element_of_argv(self):
        self.call(["chan", "--to", "x", "--", "{text}"], "arg")
        calls = self.prog.calls()
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["argv"], ["--to", "x", "--", HOSTILE])
        self.assertEqual(calls[0]["stdin"], "")

    def test_a_text_that_reads_as_an_option_is_not_passed_without_a_separator(self):
        with self.assertRaises(RUNNER.ExecutorRefusal):
            self.call(["chan", "--message", "{text}"], "arg", text="--delete-everything")
        self.assertEqual(self.prog.calls(), [])

    def test_the_text_goes_only_to_stdin(self):
        self.call(["chan", "--queue"], "stdin")
        calls = self.prog.calls()
        self.assertEqual(calls[0]["argv"], ["--queue"])
        self.assertEqual(calls[0]["stdin"], HOSTILE)

    def test_the_text_goes_only_to_a_private_message_file_removed_after_the_call(self):
        self.call(["chan", "--prompt-file", "{textFile}"], "file")
        call = self.prog.calls()[0]
        self.assertEqual(len(call["argv"]), 2)
        path = call["argv"][1]
        self.assertEqual(os.path.dirname(path), self.box.token_dir)
        self.assertTrue(os.path.basename(path).startswith("msg-"))
        self.assertEqual(call["files"][path]["content"], HOSTILE)
        self.assertEqual(call["files"][path]["mode"], 0o600)
        self.assertEqual(self.box.messages(), [])

    def test_the_token_is_in_neither_argv_nor_environment_nor_message_file(self):
        for argv, feed in ((["chan", "--", "{text}"], "arg"), (["chan"], "stdin"),
                           (["chan", "{textFile}"], "file")):
            with self.subTest(feed=feed):
                self.call(argv, feed)
        calls = self.prog.calls()
        self.assertEqual(len(calls), 3)
        for call in calls:
            self.assertIsNone(call["env"][SECRET_VAR])
            self.assertNotIn(TOKEN, json.dumps(call["argv"]))
            self.assertNotIn(TOKEN, call["stdin"])
            for f in call["files"].values():
                self.assertNotIn(TOKEN, f["content"])

    def test_a_text_that_holds_the_token_reaches_no_feed_with_it(self):
        # Review finding C2 of wave 7: the class "the token nowhere" (#43)
        # covers the text of the event too - a preview that quotes the
        # token - replaced as the journal replaces it.
        text = "see " + TOKEN + " here"
        for argv, feed in ((["chan", "--", "{text}"], "arg"), (["chan"], "stdin"),
                           (["chan", "{textFile}"], "file")):
            with self.subTest(feed=feed):
                self.call(argv, feed, text=text)
        calls = self.prog.calls()
        self.assertEqual(len(calls), 3)
        self.assertEqual(calls[0]["argv"][-1], "see [redacted] here")
        self.assertEqual(calls[1]["stdin"], "see [redacted] here")
        (content,) = [f["content"] for f in calls[2]["files"].values()]
        self.assertEqual(content, "see [redacted] here")

    def test_an_argument_from_a_variable_that_holds_the_token_is_refused_before_the_call(self):
        with self.assertRaises(RUNNER.ExecutorRefusal):
            self.call(["chan", "--key", "{env:%s}" % SECRET_VAR], "stdin")
        self.assertEqual(self.prog.calls(), [])

    def test_a_literal_argument_with_the_token_is_refused_before_the_call(self):
        with self.assertRaises(RUNNER.ExecutorRefusal):
            self.call(["chan", "--key", TOKEN], "stdin")
        self.assertEqual(self.prog.calls(), [])

    def test_a_call_past_its_timeout_is_killed(self):
        self.prog.respond([{"hang": True}])
        started = time.monotonic()
        result = self.call(["chan"], "stdin", timeout=1)
        self.assertTrue(result.timed_out)
        self.assertIsNone(result.exit_code)
        self.assertLess(time.monotonic() - started, 30)
        pid = self.prog.calls()[0]["pid"]

        def gone():
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return True
            return False
        deadline = time.monotonic() + 10
        while not gone() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue(gone(), "the program outlived its timeout")

    def test_message_files_left_behind_are_removed_when_a_monitor_of_the_token_starts(self):
        leftover = os.path.join(self.box.token_dir, "msg-left0ver")
        with open(leftover, "w") as fh:
            fh.write("old text")
        other = os.path.join(self.box.token_dir, "keep-me")
        with open(other, "w") as fh:
            fh.write("x")
        registry = {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
                    "agents": [], "channels": [], "shells": {"source": "t", "names": ["sh"]},
                    "zavodEngines": {}}
        runner = harness.load_runner(registry=base64.b64encode(json.dumps(registry).encode()).decode())
        reg = runner.load_registry()
        env = self.box.env(SKEIN_PUBLIC_URL="http://127.0.0.1:9", SKEIN_ISSUE="sk-43",
                           SKEIN_TOKEN_FILE=self.box.token, SKEIN_CLIENT="alpha", SKEIN_PLATFORM="posix",
                           SKEIN_MONITOR_CHANNEL="", SKEIN_MONITOR_BLIND="", SKEIN_MONITOR_MODE="run")
        settings = runner.read_settings(env, reg, mode="client")
        line = runner.preflight(self.box.token, env, reg, settings, detector=runner.Detector(),
                                placement=runner.place([], reg, settings.record))
        env["SKEIN_MONITOR_PREFLIGHT"] = str(line["preflightId"])
        held = runner.WorkingStart()
        with mock.patch.object(runner, "LOADER_MODE", "client"):
            held.begin(self.box.token, env, settings, "sk-43")
        self.addCleanup(held.release)
        self.assertFalse(os.path.exists(leftover))
        self.assertTrue(os.path.exists(other))


class _Listener(object):
    """A listening socket that accepts connections and records every byte
    it receives."""

    def __init__(self, family, address):
        self.sock = socket.socket(family, socket.SOCK_STREAM)
        self.sock.bind(address)
        self.sock.listen(8)
        self.address = self.sock.getsockname()
        self.accepted = 0
        # Connections whose end the listener has seen (the peer closed).
        self.ended = 0
        self.received = b""
        self._stop = False
        self.thread = threading.Thread(target=self._serve)
        self.thread.daemon = True
        self.thread.start()

    def _serve(self):
        self.sock.settimeout(0.2)
        while not self._stop:
            try:
                conn, _ = self.sock.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            self.accepted += 1
            conn.settimeout(1)
            try:
                while True:
                    data = conn.recv(4096)
                    if not data:
                        break
                    self.received += data
            except OSError:
                pass
            finally:
                conn.close()
                self.ended += 1

    def settled(self, sec=10):
        """Wait until at least one connection was accepted and the end of
        every accepted one was seen: then no byte can still be on its way."""
        deadline = time.monotonic() + sec
        while time.monotonic() < deadline:
            if self.accepted >= 1 and self.ended >= self.accepted:
                return True
            time.sleep(0.02)
        return False

    def close(self):
        self._stop = True
        self.sock.close()
        self.thread.join(2)


@unittest.skipUnless(os.name == "posix", "the POSIX probes")
class PreflightProbeTest(unittest.TestCase):
    """Row #44: the preflight runs the real probe of each type; a probe that
    refuses moves the choice to the next method with the cause, a probe that
    passes takes the channel without any delivery call."""

    def setUp(self):
        self.box = Box(self)
        self.deliver1 = fake_channel.FakeProgram(self.box.bin, "deliver-one")
        self.deliver2 = fake_channel.FakeProgram(self.box.bin, "deliver-two")
        self.probe = fake_channel.FakeProgram(self.box.bin, "probe-one")

    def registry(self, probe):
        def channel(cid, program, probes):
            rec = {"id": cid, "about": cid, "source": "test", "step": 3, "order": 1 if cid == "one" else 2,
                   "kind": "addressed", "blind": False, "detect": {},
                   "executor": {"type": "argv", "argv": [program], "feed": "stdin", "timeoutSec": 1},
                   "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "queued"}],
                   "addressee": {"process": "none"},
                   "binaries": {"linux": [program], "macos": [program]}}
            if probes:
                rec["probes"] = probes
            else:
                rec["probeUndefined"] = True
            return rec
        doc = {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
               "agents": [{"id": "alpha", "about": "a", "source": "t", "process": {"executables": ["alpha"]},
                           "methods": [
                               {"kind": "delivery", "channel": "one", "conditions": {}, "instruction": "I1"},
                               {"kind": "delivery", "channel": "two", "conditions": {}, "instruction": "I2"}]}],
               "channels": [channel("one", self.deliver1.path, [probe]), channel("two", self.deliver2.path, None)],
               "shells": {"source": "t", "names": ["sh"]}, "zavodEngines": {}}
        return base64.b64encode(json.dumps(doc).encode("utf-8")).decode("ascii")

    def preflight(self, probe, **extra):
        runner = harness.load_runner(registry=self.registry(probe))
        reg = runner.load_registry()
        env = self.box.env(SKEIN_PUBLIC_URL="http://127.0.0.1:9", SKEIN_ISSUE="sk-44",
                           SKEIN_TOKEN_FILE=self.box.token, SKEIN_CLIENT="alpha", SKEIN_PLATFORM="posix",
                           SKEIN_MONITOR_CHANNEL="", SKEIN_MONITOR_BLIND="", **extra)
        settings = runner.read_settings(env, reg, mode="client")
        return runner.preflight(self.box.token, env, reg, settings, detector=runner.Detector(),
                                placement=runner.place([], reg, settings.record))

    def assert_no_delivery(self):
        self.assertEqual(self.deliver1.calls(), [])
        self.assertEqual(self.deliver2.calls(), [])

    def assert_moved_on(self, line, cause):
        self.assertEqual(line["channel"], "two")
        self.assertIn("one: probe", line["reason"])
        self.assertIn(cause, line["reason"])
        self.assert_no_delivery()

    def argv_probe(self):
        return {"type": "argv", "source": "t", "argv": [self.probe.path, "status"],
                "results": [{"match": {"exitCode": 0}, "result": "available"},
                            {"match": {"exitCode": 7}, "result": "unavailable"}]}

    def test_an_argv_probe_answering_unavailable_moves_on(self):
        self.probe.respond([{"exit": 7}])
        self.assert_moved_on(self.preflight(self.argv_probe()), "unavailable")
        call = self.probe.calls()[0]
        self.assertEqual(call["argv"], ["status"])
        self.assertEqual(call["stdin"], "")
        self.assertEqual(call["files"], {})
        self.assertEqual(self.box.messages(), [])

    def test_a_hanging_argv_probe_times_out_and_moves_on(self):
        self.probe.respond([{"hang": True}])
        self.assert_moved_on(self.preflight(self.argv_probe()), "timed out")

    def test_a_directory_without_write_moves_on_and_keeps_no_file(self):
        if os.geteuid() == 0:
            # Permission bits do not stop root: a directory that is gone
            # takes no write either.
            target = os.path.join(self.box.root, "gone", "queue")
        else:
            target = os.path.join(self.box.root, "queue")
            os.mkdir(target, 0o500)
        line = self.preflight({"type": "dir-writable", "source": "t", "path": "{env:QUEUE_DIR}"},
                              QUEUE_DIR=target)
        self.assert_moved_on(line, "unavailable")
        if os.path.isdir(target):
            self.assertEqual(os.listdir(target), [])

    def test_a_socket_that_refuses_moves_on(self):
        path = os.path.join(self.box.root, "nobody.sock")
        line = self.preflight({"type": "socket-connect", "source": "t", "path": "{env:CHAN_SOCK}"},
                              CHAN_SOCK=path)
        self.assert_moved_on(line, "unavailable")

    def test_an_unreachable_tcp_address_moves_on(self):
        free = socket.socket()
        free.bind(("127.0.0.1", 0))
        port = free.getsockname()[1]
        free.close()
        line = self.preflight({"type": "tcp-reach", "source": "t", "address": "{env:CHAN_ADDR}"},
                              CHAN_ADDR="127.0.0.1:%d" % port)
        self.assert_moved_on(line, "unavailable")

    def test_every_probe_type_that_passes_takes_the_channel_without_a_delivery(self):
        sock_path = os.path.join(self.box.root, "chan.sock")
        unix = _Listener(socket.AF_UNIX, sock_path)
        self.addCleanup(unix.close)
        tcp = _Listener(socket.AF_INET, ("127.0.0.1", 0))
        self.addCleanup(tcp.close)
        queue = os.path.join(self.box.root, "queue-ok")
        os.mkdir(queue, 0o700)
        self.probe.respond([{"exit": 0}])
        cases = [
            ("argv", self.argv_probe(), {}),
            ("dir-writable", {"type": "dir-writable", "source": "t", "path": "{env:QUEUE_DIR}"},
             {"QUEUE_DIR": queue}),
            ("socket-connect", {"type": "socket-connect", "source": "t", "path": "{env:CHAN_SOCK}"},
             {"CHAN_SOCK": sock_path}),
            ("tcp-reach", {"type": "tcp-reach", "source": "t", "address": "{env:CHAN_ADDR}"},
             {"CHAN_ADDR": "127.0.0.1:%d" % tcp.address[1]}),
        ]
        for kind, probe, extra in cases:
            with self.subTest(probe=kind):
                line = self.preflight(probe, **extra)
                self.assertEqual(line["method"], "detached")
                self.assertEqual(line["channel"], "one")
        self.assert_no_delivery()
        self.assertEqual(os.listdir(queue), [])
        self.assertTrue(unix.settled(), "the unix listener saw no whole connection")
        self.assertTrue(tcp.settled(), "the tcp listener saw no whole connection")
        self.assertEqual(unix.received, b"")
        self.assertEqual(tcp.received, b"")


class PolledWaitTest(unittest.TestCase):
    """The polled wait (Windows, Python 3.12+): after its timeout the call
    ends within one more grace even when the kill does not end the program
    (its outcome is unknown, a timeout)."""

    @unittest.skipUnless(sys.platform.startswith("linux"), "non-blocking pipes of POSIX")
    def test_a_program_the_kill_does_not_end_does_not_hold_the_call(self):
        box = {}

        def call():
            box["result"] = RUNNER._run_polled([sys.executable, "-c", "import time; time.sleep(8)"],
                                               dict(os.environ), None, 0.3)

        with mock.patch.object(RUNNER.PosixPlatform, "tree_kill", staticmethod(lambda proc, job: None)):
            worker = threading.Thread(target=call)
            worker.daemon = True
            worker.start()
            worker.join(6)
        self.assertFalse(worker.is_alive(), "the call waits for a program it could not kill")
        self.assertTrue(box["result"].timed_out)
        self.assertIsNone(box["result"].exit_code)


if __name__ == "__main__":
    unittest.main()
