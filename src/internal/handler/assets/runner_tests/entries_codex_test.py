#!/usr/bin/env python3
"""SC-001 of the Codex record of wave 1 and its codex-queue channel
(matrix rows #75, #76, #77), over the registry the server embeds.

The expected values come from the Codex source at ae21235 (codex-rs), not
from the record: `codex queue --thread <id> --message <text>` queues a
user message (cli/src/queue_cmd.rs#QueueCommand); on success it prints
"Queued message <id> for thread <id>." and exits 0, and its only warning,
on stderr, belongs to --resume (tui/src/session_queue_commands.rs#
run_session_queue_command); the queue lives in the state database under
CODEX_SQLITE_HOME, else CODEX_HOME, else ~/.codex (state/src/lib.rs#
SQLITE_HOME_ENV, utils/home-dir find_codex_home); every command of a thread
gets CODEX_THREAD_ID (core/src/exec_env.rs), and no variable carries the
address given to --remote.

Run on its own: python3 internal/handler/assets/runner_tests/entries_codex_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import stat
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import failures_test as ft  # noqa: E402
import fake_proc  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG
THREAD = "019a8e3c-5d2b-7c41-9f0e-bbbbbbbbbbbb"
QUEUED_OUT = "Queued message 0193-q for thread %s.\n" % THREAD
WARNING = "Warning: message was queued, but could not resume thread %s: no daemon\n" % THREAD

_CODEX = r'''#!%(python)s
import json, os, sys
HERE = %(here)r
with open(os.path.join(HERE, "codex.calls.jsonl"), "a") as fh:
    fh.write(json.dumps({"argv": sys.argv[1:], "CODEX_HOME": os.environ.get("CODEX_HOME")}) + "\n")
try:
    with open(os.path.join(HERE, "codex.answers.json")) as fh:
        answers = json.load(fh)
except OSError:
    answers = []
count = os.path.join(HERE, "codex.count")
n = int(open(count).read()) if os.path.exists(count) else 0
open(count, "w").write(str(n + 1))
a = answers[min(n, len(answers) - 1)] if answers else {"exit": 0, "stdout": "", "stderr": ""}
sys.stdout.write(a.get("stdout", ""))
sys.stderr.write(a.get("stderr", ""))
sys.exit(a.get("exit", 0))
'''


def fake_codex(world, answers):
    """Put a fake codex on PATH that answers in turn with stdout, stderr and
    an exit code, and records its argv and CODEX_HOME."""
    path = os.path.join(world.bin, "codex")
    with open(path, "w") as fh:
        fh.write(_CODEX % {"python": sys.executable, "here": world.bin})
    os.chmod(path, stat.S_IRWXU)
    with open(os.path.join(world.bin, "codex.answers.json"), "w") as fh:
        json.dump(answers, fh)


def codex_calls(world):
    try:
        with open(os.path.join(world.bin, "codex.calls.jsonl")) as fh:
            return [json.loads(l) for l in fh if l.strip()]
    except OSError:
        return []


def codex_home(world, name="codex-home", mode=0o700):
    path = os.path.join(world.root, name)
    os.makedirs(path, exist_ok=True)
    os.chmod(path, mode)
    return path


def preflight_with_argv(world, name, agent_argv, prober=None, **extra):
    """A preflight of client codex whose agent process runs with agent_argv."""
    env = world.env(name, "codex", **extra)
    source = fake_proc.FakeProcesses()
    source.add(ft.RUNNER_PID, ft.RUNNER_PID - 1, "python3", ft.RUNNER_PROC_ARGV)
    source.add(ft.RUNNER_PID - 1, os.getpid(), "bash")
    source.add(os.getpid(), 1, "codex", agent_argv, start=ft.start_of(os.getpid()))
    pl = RUNNER.place(RUNNER.ancestry(source, ft.RUNNER_PID), REG, RUNNER.resolve_record(REG, "codex"),
                      self_pid=ft.RUNNER_PID)
    settings = RUNNER.read_settings(env, REG, mode="client")
    return RUNNER.preflight(env["SKEIN_TOKEN_FILE"], env, REG, settings, detector=RUNNER.RegistryDetector(),
                            prober=prober or ft.FakeProbe(), placement=pl)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class CodexQueueTest(unittest.TestCase):
    """Row #75: the call and every sign of its table."""

    def test_the_call_and_its_outcomes(self):
        world = ft.World(self)
        home = codex_home(world)
        fake_codex(world, [{"exit": 0, "stdout": QUEUED_OUT},
                           {"exit": 0, "stdout": QUEUED_OUT, "stderr": WARNING},
                           {"exit": 1, "stderr": "Error: No active session found matching 'x'.\n"},
                           {"exit": 2, "stdout": "unexpected answer of another version\n"}])
        mon = world.monitor("tok", "codex", "codex", CODEX_THREAD_ID=THREAD, CODEX_HOME=home)
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "codex-queue"))
        for nid in (1, 2, 3, 4):
            mon.page([ft.note(nid)], cursor={"ts": "t", "id": nid})
        calls = codex_calls(world)
        self.assertEqual(len(calls), 4)
        for call in calls:
            self.assertEqual(call["argv"][:4], ["queue", "--thread", THREAD, "--message"])
            self.assertEqual(len(call["argv"]), 5)
            self.assertNotIn("--resume", call["argv"])
            self.assertEqual(call["CODEX_HOME"], home)
        self.assertIn('notification="1"', calls[0]["argv"][4])
        # exit 0 without a warning: queued; with a warning: unknown; an
        # error after the request may have gone: unknown; an exit code the
        # source does not give: unknown.
        self.assertEqual(mon.statuses(), [(1, "queued"), (2, "unknown"), (3, "unknown"), (4, "unknown")])

    def test_no_program_is_a_refusal_before_send(self):
        world = ft.World(self)
        fake_codex(world, [{"exit": 0, "stdout": QUEUED_OUT}])
        mon = world.monitor("tok", "codex", "codex", CODEX_THREAD_ID=THREAD, CODEX_HOME=codex_home(world))
        os.remove(os.path.join(world.bin, "codex"))
        mon.page([ft.note(1)])
        self.assertEqual(codex_calls(world), [])
        self.assertNotIn((1, "queued"), mon.statuses())

    def test_no_thread_or_remote_takes_the_next_method(self):
        world = ft.World(self)
        fake_codex(world, [{"exit": 0, "stdout": QUEUED_OUT}])
        line, _ = world.preflight("tok", "codex", "codex", CODEX_HOME=codex_home(world))
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertIn("CODEX_THREAD_ID", line["reason"])
        for argv in (["codex", "--remote", "ws://127.0.0.1:4500"], ["codex", "--remote=ws://127.0.0.1:4500"]):
            with self.subTest(argv=argv):
                line = preflight_with_argv(world, "tok-r", argv, CODEX_THREAD_ID=THREAD, CODEX_HOME=codex_home(world),
                                           **world.zavod_env("codex"))
                self.assertEqual((line["method"], line["channel"]), ("detached", "zavod"), line)
                self.assertIn("runs with --remote", line["reason"])
        line = preflight_with_argv(world, "tok-l", ["codex", "--yolo"], CODEX_THREAD_ID=THREAD,
                                   CODEX_HOME=codex_home(world))
        self.assertEqual(line["channel"], "codex-queue")


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class ProbeTest(unittest.TestCase):
    """Row #76: the one connection form the record declares is the local
    queue store; its boundary is probed without delivering. A closed
    boundary gives its cause and the next method; an open one, the method.
    The daemon socket and --remote get no probe (the source: an embedded
    app server writes the queue when the socket does not answer; no
    variable gives the remote address)."""

    def line(self, world, **extra):
        env = dict(CODEX_THREAD_ID=THREAD, **world.zavod_env("codex"))
        env.update(extra)
        line, _ = world.preflight("tok", "codex", "codex", prober=RUNNER.SystemProbeRunner(), **env)
        return line

    def test_the_store_directory_decides(self):
        world = ft.World(self)
        fake_codex(world, [{"exit": 0, "stdout": QUEUED_OUT}])
        open_dir = codex_home(world, "open")
        # A boundary that takes no write whoever runs the test: a file where
        # the directory should be.
        closed = os.path.join(world.root, "closed")
        with open(closed, "w") as fh:
            fh.write("not a directory")
        dot_codex = os.path.join(world.root, ".codex")
        cases = [
            ("CODEX_HOME open", {"CODEX_HOME": open_dir}, "codex-queue"),
            ("CODEX_HOME closed", {"CODEX_HOME": closed}, "zavod"),
            ("CODEX_SQLITE_HOME first, open", {"CODEX_SQLITE_HOME": open_dir, "CODEX_HOME": closed}, "codex-queue"),
            ("CODEX_SQLITE_HOME first, closed", {"CODEX_SQLITE_HOME": closed, "CODEX_HOME": open_dir}, "zavod"),
            ("~/.codex missing", {"CODEX_HOME": None}, "zavod"),
        ]
        for name, extra, want in cases:
            with self.subTest(case=name):
                env = dict(extra)
                if env.get("CODEX_HOME") is None and "CODEX_HOME" in env:
                    del env["CODEX_HOME"]
                    saved = world.base_env.pop("CODEX_HOME")
                    self.addCleanup(world.base_env.__setitem__, "CODEX_HOME", saved)
                line = self.line(world, **env)
                self.assertEqual(line["channel"], want, line)
                if want == "zavod":
                    self.assertIn("codex-queue: probe dir-writable: unavailable", line["reason"])
        os.mkdir(dot_codex, 0o700)
        line = self.line(world)
        self.assertEqual(line["channel"], "codex-queue", line)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class AddresseeTest(unittest.TestCase):
    """Row #77 (SC-003): a thread whose Codex process has ended still takes
    the message (queued); one thread under two CODEX_HOME is two
    addressees, two deliveries."""

    def test_a_thread_after_its_process_ended_is_queued(self):
        world = ft.World(self)
        fake_codex(world, [{"exit": 0, "stdout": QUEUED_OUT}])
        agent = ft.Sleeper(self)
        mon = world.monitor("tok", "codex", "codex", agent_pid=agent.pid, CODEX_THREAD_ID=THREAD,
                            CODEX_HOME=codex_home(world))
        agent.end()
        mon.page([ft.note(1)])
        self.assertEqual(len(codex_calls(world)), 1)
        self.assertEqual(mon.statuses(), [(1, "queued")])

    def test_two_codex_homes_are_two_addressees(self):
        world = ft.World(self)
        fake_codex(world, [{"exit": 0, "stdout": QUEUED_OUT}])
        a = world.monitor("tok-a", "codex", "codex", CODEX_THREAD_ID=THREAD, CODEX_HOME=codex_home(world, "home-a"))
        b = world.monitor("tok-b", "codex", "codex", agent_pid=ft.Sleeper(self).pid, CODEX_THREAD_ID=THREAD,
                          CODEX_HOME=codex_home(world, "home-b"))
        a.page([ft.note(1)])
        b.page([ft.note(1)])
        homes = [c["CODEX_HOME"] for c in codex_calls(world)]
        self.assertEqual(homes, [os.path.join(world.root, "home-a"), os.path.join(world.root, "home-b")])
        self.assertEqual((a.statuses(), b.statuses()), ([(1, "queued")], [(1, "queued")]))


if __name__ == "__main__":
    unittest.main()
