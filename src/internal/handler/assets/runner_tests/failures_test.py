#!/usr/bin/env python3
"""End-to-end failures of the core with the records of wave 1 (SC-003,
INV-6, INV-12, US-12): matrix rows #86 and #126.

Each monitor here is built the way the working runner builds it: a real
preflight over the embedded registry.json gives the result (method,
channel, fallbacks, the identity of the addressee), and the DeliverySink of
that result runs over the real ledger, journal and argv executor. The
channels are fake programs named like the real ones (zavod, codex) on PATH
(fake_channel.py); the clock of a sink is a virtual one. The agent process
of a fake chain is a live process of this test (this process, or a child
the test ends), so the addressee checks of the working runner hold or fail
as they would.

Run on its own: python3 internal/handler/assets/runner_tests/failures_test.py
Standard library of Python 3.6 only.
"""

import base64
import io
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402
import fake_channel  # noqa: E402
import fake_proc  # noqa: E402
import harness  # noqa: E402
import modes_stream_test as ms  # noqa: E402

RUNNER = harness.load_runner()
BODY = harness.render_runner()
REG = RUNNER.load_registry()
SERVER = "https://skein.example"
ISSUE = "sk-86"
TOKEN = "skm_" + "F" * 43
ZAVOD_UUID = "6c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
CODEX_THREAD = "019a8e3c-5d2b-7c41-9f0e-aaaaaaaaaaaa"
RUNNER_PID = 71001
RUNNER_PROC_ARGV = ["python3", "-c", "_SKEIN_LOADER_MODE = 'client'"]
CURSOR = {"ts": "2026-01-01T00:00:00Z", "id": 3}
ZAVOD_OK = {"exit": 0, "stdout": {"zavod": 1, "command": "send", "outcome": "ok", "exit_code": 0, "code": None}}
ZAVOD_NO_CONNECTION = {"exit": 12, "stdout": {"zavod": 1, "command": "send", "outcome": "no_connection",
                                              "exit_code": 12, "code": "no_connection"}}
ZAVOD_UNKNOWN = {"exit": 13, "stdout": {"zavod": 1, "command": "send", "outcome": "unknown", "exit_code": 13,
                                        "code": "unknown"}}
# Values no surface may show (row #126): the token, a key of the channel
# and the full body of the comment, each distinct and recognisable.
CHANNEL_KEY = "chankey-7f3a91c2e8b4"
FULL_BODY = "FULL-BODY-0d9e8f7a6b5c4d3e2f1a beyond the preview " + "z" * 2000
PREVIEW = "allowed preview 5e6f"
FORBIDDEN = (TOKEN, CHANNEL_KEY, FULL_BODY[:40])


def root_life(pid):
    """(boot id, start in clock ticks) of a live process from
    /proc/sys/kernel/random/boot_id and field 22 of /proc/<pid>/stat."""
    with open("/proc/sys/kernel/random/boot_id") as fh:
        boot = fh.read().strip()
    with open("/proc/%d/stat" % pid) as fh:
        start = fh.read().rpartition(")")[2].split()[19]
    return boot, start


def start_of(pid):
    return RUNNER.current_platform().process_info(pid).start


def placement(client, agent_name, agent_pid=None, agent_argv=None):
    """A fake chain runner -> bash -> agent -> bash -> tmux whose agent
    process is agent_pid (this process by default) with its real start."""
    pid = os.getpid() if agent_pid is None else agent_pid
    source = fake_proc.FakeProcesses()
    source.add(RUNNER_PID, RUNNER_PID - 1, "python3", RUNNER_PROC_ARGV)
    source.add(RUNNER_PID - 1, pid, "bash")
    source.add(pid, RUNNER_PID - 2, agent_name, agent_argv, start=start_of(pid))
    source.add(RUNNER_PID - 2, RUNNER_PID - 3, "bash")
    source.add(RUNNER_PID - 3, 1, "tmux")
    return RUNNER.place(RUNNER.ancestry(source, RUNNER_PID), REG, RUNNER.resolve_record(REG, client),
                        self_pid=RUNNER_PID)


class FakeProbe(object):
    def __init__(self, answers=None):
        self.answers = answers or {}

    def run(self, chan, probe, env):
        return self.answers.get(chan["id"], "available")


class Monitor(object):
    """One monitor: its token, its preflight result and its sink."""

    def __init__(self, world, name, sink, journal, doc, spy):
        self.world = world
        self.name = name
        self.sink = sink
        self.journal = journal
        self.doc = doc
        self.spy = spy
        self.token = sink.token_file

    def page(self, notes, cursor=CURSOR):
        return self.sink.page(cursor, notes)

    def lines(self):
        try:
            with open(self.journal.path) as fh:
                return [json.loads(line) for line in fh if line.strip()]
        except OSError:
            return []

    def statuses(self):
        return [(l["notificationId"], l["status"]) for l in self.lines() if l.get("event") == "delivery"]


class World(object):
    """A user directory shared by the monitors (one ledger), a bin directory
    with fake zavod and codex programs, a virtual clock."""

    def __init__(self, test):
        self.test = test
        self.root = tempfile.mkdtemp(prefix="skein-failures-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin, 0o700)
        self.progs = {}
        for program in ("zavod", "codex", "filechan"):
            self.progs[program] = fake_channel.FakeProgram(self.bin, program, watch=("CODEX_HOME",))
        self.progs["zavod"].respond([ZAVOD_OK])
        self.progs["codex"].respond([{"exit": 0, "stdout": "Queued message q-1 for thread t.\n"}])
        self.clock = dt.Clock()
        self.base_env = {"PATH": self.bin + os.pathsep + os.environ.get("PATH", os.defpath), "HOME": self.root,
                         "XDG_STATE_HOME": os.path.join(self.root, "state"), "SKEIN_PUBLIC_URL": SERVER,
                         "SKEIN_ISSUE": ISSUE, "SKEIN_EXCLUDE_SESSION": "", "SKEIN_RUNNER_SHA256": "ab" * 32,
                         "SKEIN_LOADER_SHA256": "cd" * 32, "SKEIN_PLATFORM": "posix",
                         "SKEIN_MONITOR_CHANNEL": "", "SKEIN_MONITOR_BLIND": "",
                         "CODEX_HOME": os.path.join(self.root, "codex-home"),
                         "SKEIN_TEST_CHANNEL_KEY": CHANNEL_KEY}

    def zavod_env(self, engine, root_pid=None):
        """The variables Zavod sets on its agent's process, with the life of
        its root process (this test process by default) as Zavod records
        it: field 22 of /proc/<pid>/stat and the boot id."""
        pid = os.getpid() if root_pid is None else root_pid
        boot, start = root_life(pid)
        return {"ZAVOD_ME": ZAVOD_UUID, "ZAVOD_AGENT_ID": ZAVOD_UUID, "ZAVOD_AGENT_ENGINE_ID": engine,
                "ZAVOD_AGENT_ROOT_PID": str(pid), "ZAVOD_AGENT_ROOT_START": start, "ZAVOD_AGENT_ROOT_BOOT_ID": boot}

    def token(self, name):
        token_dir = os.path.join(self.root, "skein-mon-" + name)
        if not os.path.isdir(token_dir):
            os.mkdir(token_dir, 0o700)
            token = os.path.join(token_dir, "token")
            with open(token, "w") as fh:
                fh.write(TOKEN)
            os.chmod(token, 0o600)
        return os.path.join(token_dir, "token")

    def env(self, name, client, **extra):
        env = dict(self.base_env, SKEIN_TOKEN_FILE=self.token(name), SKEIN_CLIENT=client)
        env.update(extra)
        return env

    def preflight(self, name, client, agent_name, agent_pid=None, registry=REG, runner=RUNNER, prober=None,
                  **extra):
        env = self.env(name, client, **extra)
        pl = placement(client, agent_name, agent_pid)
        settings = runner.read_settings(env, registry, mode="client")
        line = runner.preflight(env["SKEIN_TOKEN_FILE"], env, registry, settings,
                                detector=runner.RegistryDetector(), prober=prober or FakeProbe(), placement=pl)
        return line, env

    def monitor(self, name, client, agent_name, agent_pid=None, cancel=None, registry=REG, runner=RUNNER,
                exclusion=None, **extra):
        line, env = self.preflight(name, client, agent_name, agent_pid, registry=registry, runner=runner, **extra)
        token = env["SKEIN_TOKEN_FILE"]
        doc = runner.load_preflight(token)
        journal = runner.Journal(token, ISSUE, secrets=[TOKEN], env=env)
        ledger = runner.Ledger(SERVER, name, runner.identity_from_doc(doc["identity"]), env=env,
                               clock=self.clock.now)
        spy = dt.Spy(runner.ArgvExecutor(token, env, [TOKEN]), self.clock)
        sink = runner.DeliverySink(journal, token, doc, registry, env, ledger, spy, ISSUE,
                                   cancel=cancel or dt.FakeCancel(), clock=self.clock, exclusion=exclusion)
        mon = Monitor(self, name, sink, journal, doc, spy)
        mon.line = line
        mon.env = env
        return mon

    def calls(self, program):
        return self.progs[program].calls()


def note(nid, session="s-other", preview=PREVIEW):
    return {"id": nid, "type": "comment_added", "entity": {"type": "issue", "id": ISSUE},
            "actor": {"username": "bob"}, "actorSessionId": session, "commentId": "c%d" % nid,
            "bodyPreview": preview, "truncated": True, "body": FULL_BODY}


class Sleeper(object):
    """A live process that stands for an agent process the test ends."""

    def __init__(self, test):
        self.proc = subprocess.Popen(["sleep", "600"])
        test.addCleanup(self.end)

    @property
    def pid(self):
        return self.proc.pid

    def end(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait()


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class CoreScenariosTest(unittest.TestCase):
    """Row #86 (SC-003): the core with the records of wave 1, one scenario
    per mechanism."""

    def test_ledger_one_delivery_for_the_codex_and_the_zavod_monitor_of_one_agent(self):
        world = World(self)
        zavod = world.zavod_env("codex")
        codex_mon = world.monitor("tok-codex", "codex", "codex", CODEX_THREAD_ID=CODEX_THREAD, **zavod)
        zavod_mon = world.monitor("tok-zavod", "codex", "codex", CODEX_THREAD_ID=CODEX_THREAD,
                                  SKEIN_MONITOR_CHANNEL="zavod", **zavod)
        self.assertEqual(codex_mon.doc["channel"], "codex-queue")
        self.assertEqual(zavod_mon.doc["channel"], "zavod")
        codex_mon.page([note(1)])
        zavod_mon.page([note(1)])
        self.assertEqual(len(world.calls("codex")), 1)
        self.assertEqual(world.calls("zavod"), [])
        self.assertEqual(codex_mon.statuses(), [(1, "queued")])
        self.assertEqual(zavod_mon.statuses(), [(1, "queued")])

    def test_unknown_from_zavod_is_final_for_the_codex_monitor(self):
        world = World(self)
        world.progs["zavod"].respond([ZAVOD_UNKNOWN])
        zavod = world.zavod_env("codex")
        zavod_mon = world.monitor("tok-zavod", "codex", "codex", CODEX_THREAD_ID=CODEX_THREAD,
                                  SKEIN_MONITOR_CHANNEL="zavod", **zavod)
        codex_mon = world.monitor("tok-codex", "codex", "codex", CODEX_THREAD_ID=CODEX_THREAD, **zavod)
        zavod_mon.page([note(1)])
        codex_mon.page([note(1)])
        self.assertEqual(len(world.calls("zavod")), 1)
        self.assertEqual(world.calls("codex"), [])
        self.assertEqual(zavod_mon.statuses(), [(1, "unknown")])
        self.assertEqual(codex_mon.statuses(), [(1, "unknown")])

    def test_a_journal_outage_holds_zavod_sends_until_it_is_back(self):
        world = World(self)
        mon = world.monitor("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod", **world.zavod_env("claude"))
        outage = RUNNER.JournalUnavailable("write_failed", mon.journal.path)
        with mock.patch.object(mon.journal, "delivery", side_effect=outage):
            with self.assertRaises(RUNNER.JournalUnavailable):
                mon.page([note(1), note(2)])
            with self.assertRaises(RUNNER.JournalUnavailable):
                mon.page([note(1), note(2)])
            with self.assertRaises(RUNNER.JournalUnavailable):
                mon.page([note(1), note(2), note(3)])
        # The batch reached Zavod once; nothing more went while the journal
        # took no line.
        self.assertEqual(len(world.calls("zavod")), 1)
        mon.page([note(1), note(2), note(3)])
        self.assertEqual(len(world.calls("zavod")), 2)
        self.assertEqual(mon.statuses(), [(1, "queued"), (2, "queued"), (3, "queued")])
        self.assertIn('notification="3"', world.calls("zavod")[1]["stdin"])
        self.assertNotIn('notification="1"', world.calls("zavod")[1]["stdin"])

    def test_addressee_changed_then_a_new_preflight_takes_the_monitor_over(self):
        world = World(self)
        agent = Sleeper(self)
        mon = world.monitor("tok", "claude", "claude", agent_pid=agent.pid, SKEIN_MONITOR_CHANNEL="zavod",
                            **world.zavod_env("claude", root_pid=agent.pid))
        self.assertEqual(mon.doc["channel"], "zavod")
        agent.end()
        mon.page([note(1)])
        self.assertEqual(world.calls("zavod"), [])
        self.assertEqual(mon.statuses(), [(1, "unknown")])
        self.assertTrue(mon.sink.journal_only)
        mon.page([note(2)], cursor={"ts": "t", "id": 4})
        self.assertEqual(world.calls("zavod"), [])
        # The runner of the old addressee is alive (it holds the lock): a
        # preflight for the new one is let in and its start takes over.
        lock = RUNNER.PosixPlatform.acquire_runner_lock(mon.token, 0, mon.env)
        self.addCleanup(lock.release)
        line, env = world.preflight("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod",
                                    **world.zavod_env("claude"))
        self.assertEqual(line["preflightId"], mon.doc["preflightId"] + 1)
        self.assertFalse(mon.sink.taken_over())
        RUNNER.request_takeover(mon.token, line["preflightId"])
        self.assertTrue(mon.sink.taken_over())

    def test_access_lost_while_waiting_for_zavod(self):
        world = World(self)
        world.progs["zavod"].respond([ZAVOD_NO_CONNECTION])

        class AfterFirstCall(object):
            def check(self):
                return "token_expired" if world.calls("zavod") else None

            def may_send(self):
                return True

        mon = world.monitor("tok", "claude", "claude", cancel=AfterFirstCall(), SKEIN_MONITOR_CHANNEL="zavod",
                            **world.zavod_env("claude"))
        with self.assertRaises(RUNNER.Stop) as caught:
            mon.page([note(1)])
        self.assertEqual(caught.exception.reason, "token_expired")
        self.assertEqual(len(world.calls("zavod")), 1)
        lines = [l for l in mon.lines() if l.get("event") == "delivery"]
        self.assertEqual([(l["notificationId"], l["status"]) for l in lines], [(1, "access-lost")])
        self.assertNotIn("bodyPreview", lines[0])

    def test_own_echo_in_claude_stream_and_codex_detached(self):
        mine, other = "mcp-session-mine", "mcp-session-other"
        # Claude stream: the event of its own session is not printed.
        box = ms.Box(self)
        output = box.output({}, "stream")
        executor = ms.FakeExecutor([{"notifications": [note(1, mine), note(2, other)], "cursor": {"ts": "t", "id": 2}}])
        with mock.patch.object(ms.RUNNER.time, "sleep", lambda s: None):
            try:
                ms.RUNNER.run_stream_loop(executor, "http://poll.test", TOKEN, ms.ISSUE, mine, box.token,
                                          {"method": "stream", "preflightId": ms.PREFLIGHT_ID}, output)
            except ms.RUNNER.Stop as stop:
                self.assertEqual(stop.reason, "token_expired")
        printed = [l["notificationId"] for l in box.journal_lines()
                   if l.get("event") == "delivery" and l.get("status") == "printed"]
        self.assertEqual(printed, [2])
        shown = [l["notification"]["id"] for l in output.out.lines() if l.get("event") == "notification"]
        self.assertEqual(shown, [2])
        # Codex detached in the same series: only the other session's event
        # goes to the thread; its own echo gets "own echo".
        world = World(self)
        mon = world.monitor("tok", "codex", "codex", CODEX_THREAD_ID=CODEX_THREAD, exclusion=mine)
        mon.page([note(1, mine), note(2, other)])
        calls = world.calls("codex")
        self.assertEqual(len(calls), 1)
        self.assertIn('notification="2"', calls[0]["argv"][-1])
        self.assertNotIn('notification="1"', calls[0]["argv"][-1])
        statuses = dict(mon.statuses())
        self.assertEqual(statuses[2], "queued")
        self.assertNotIn(statuses[1], RUNNER.DELIVERED_OUTCOMES)

    def test_root_closed_kimi_cycle_and_a_refusing_zavod_monitor(self):
        root_page = {"notifications": [note(1), note(2)], "cursor": {"ts": "t", "id": 5}, "rootClosed": True}
        # Kimi exit-on-event: the whole last page fits, the cycle ends with
        # root_closed.
        box = ms.Box(self)
        output = box.output({}, "exit-on-event")
        stop, _ = box.cycle(ms.FakeExecutor([dict(root_page)]), output, "exit-on-event")
        self.assertEqual(stop.reason, "root_closed")
        # The Zavod monitor of the second agent: its channel refuses before
        # send through the whole budget; the summary counts both events as
        # not delivered, none unknown.
        world = World(self)
        world.progs["zavod"].respond([ZAVOD_NO_CONNECTION])
        mon = world.monitor("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod", **world.zavod_env("claude"))
        executor = ms.FakeExecutor([dict(root_page)])
        with mock.patch.object(RUNNER.time, "sleep", lambda s: None):
            with self.assertRaises(RUNNER.Stop) as caught:
                RUNNER.work(executor, "http://poll.test", TOKEN, ISSUE, "", mon.token, sink=mon.sink,
                            root=RUNNER.RootClosed(mon.token))
        self.assertEqual(caught.exception.reason, "root_closed")
        self.assertEqual(caught.exception.detail["undelivered"], 2)
        self.assertEqual(caught.exception.detail["unknown"], 0)
        self.assertEqual(len(world.calls("zavod")), RUNNER.MAX_CALLS)


def file_channel():
    """A synthetic channel that takes the text in a private file (feed
    "file")."""
    return {"id": "filechan", "about": "synthetic file channel", "source": "test", "step": 5, "order": 9,
            "kind": "addressed", "blind": False, "detect": {"env": [{"name": "FILECHAN_ID"}]},
            "executor": {"type": "argv", "argv": ["filechan", "--file", "{textFile}"], "feed": "file",
                         "timeoutSec": 5},
            "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "queued"}],
            "probeUndefined": True, "addressee": {"process": "none"},
            "binaries": {"linux": ["filechan"], "macos": ["filechan"]}}


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline and detached start")
class SecretsOnTheOutputTest(unittest.TestCase):
    """Row #126 (INV-6, INV-12, US-12): the token, a key of the channel (in
    the environment and in the channel's answer) and the full body of the
    comment reach no surface; every surface is non-empty and checked."""

    def assert_clean(self, text, what):
        self.assertTrue(text, "%s is empty" % what)
        for value in FORBIDDEN:
            self.assertNotIn(value, text, "%s shows a forbidden value" % what)

    def test_stream_stdout_carries_the_preview_only(self):
        box = ms.Box(self)
        output = box.output({}, "stream")
        executor = ms.FakeExecutor([{"notifications": [note(1)], "cursor": {"ts": "t", "id": 1}}])
        with mock.patch.object(ms.RUNNER.time, "sleep", lambda s: None):
            with self.assertRaises(ms.RUNNER.Stop):
                ms.RUNNER.run_stream_loop(executor, "http://poll.test", TOKEN, ms.ISSUE, "", box.token,
                                          {"method": "stream", "preflightId": ms.PREFLIGHT_ID}, output)
        text = "".join(t for _, t in output.out.writes)
        self.assert_clean(text, "stream stdout")
        self.assertIn(PREVIEW, text)

    def test_zavod_detached_start_stdin_argv_and_journal(self):
        world = World(self)
        world.progs["zavod"].respond([{"exit": 0, "stdout": dict(ZAVOD_OK["stdout"], message=CHANNEL_KEY)}])
        pages = [{"notifications": [note(1)], "cursor": {"ts": "t", "id": 1}}]

        def respond(h, qs):
            h.send_json(200, pages.pop(0) if pages else harness.EMPTY)

        server = harness.ScriptedServer(respond)
        self.addCleanup(server.stop)
        line, env = world.preflight("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod",
                                    SKEIN_PUBLIC_URL=server.url(), **world.zavod_env("claude"))
        self.assertEqual(line["channel"], "zavod")
        run_env = dict(env, SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT=str(line["preflightId"]))
        lines, pids = [], []
        spawn = RUNNER.PosixPlatform.spawn_detached

        def spy(*args):
            pid, receipt = spawn(*args)
            pids.append(pid)
            return pid, receipt

        def kill():
            for pid in pids:
                try:
                    os.killpg(pid, signal.SIGKILL)
                except OSError:
                    pass

        self.addCleanup(kill)
        with mock.patch.dict(os.environ, run_env, clear=True), \
                mock.patch.object(RUNNER, "LOADER_MODE", "client"), \
                mock.patch.object(RUNNER, "_skein_loader_body", BODY, create=True), \
                mock.patch.object(RUNNER.PosixPlatform, "spawn_detached", staticmethod(spy)), \
                mock.patch.object(RUNNER, "emit", lines.append), \
                mock.patch("sys.stdout", io.StringIO()):
            code = RUNNER.run()
        self.assertEqual(code, 0)
        self.assertEqual([l["event"] for l in lines], ["monitor_started"])
        self.assert_clean(json.dumps(lines[0]), "monitor_started")
        deadline = time.monotonic() + 30
        journal = RUNNER.journal_path(env["SKEIN_TOKEN_FILE"], env)

        def journaled():
            try:
                with open(journal) as fh:
                    return [json.loads(l) for l in fh if l.strip()]
            except OSError:
                return []

        while time.monotonic() < deadline and not any(l.get("event") == "delivery" for l in journaled()):
            time.sleep(0.05)
        calls = world.calls("zavod")
        self.assertEqual(len(calls), 1)
        self.assert_clean(calls[0]["stdin"], "zavod stdin")
        self.assertIn(PREVIEW, calls[0]["stdin"])
        argv = " ".join(calls[0]["argv"])
        for value in FORBIDDEN + (PREVIEW,):
            self.assertNotIn(value, argv)
        entries = journaled()
        self.assertEqual([(l["notificationId"], l["status"]) for l in entries if l.get("event") == "delivery"],
                         [(1, "queued")])
        with open(journal) as fh:
            self.assert_clean(fh.read(), "journal")

    def test_a_file_channel_message_carries_the_preview_only(self):
        with open(harness.REGISTRY_JSON) as fh:
            doc = json.load(fh)
        doc["channels"].append(file_channel())
        b64 = base64.b64encode(json.dumps(doc).encode("utf-8")).decode("ascii")
        runner = harness.load_runner(registry=b64)
        reg = runner.load_registry()
        world = World(self)
        mon = world.monitor("tok", "no-such-agent", "bash-agent", registry=reg, runner=runner,
                            SKEIN_MONITOR_CHANNEL="filechan", FILECHAN_ID="f-1")
        self.assertEqual(mon.doc["channel"], "filechan")
        mon.page([note(1)])
        calls = world.calls("filechan")
        self.assertEqual(len(calls), 1)
        files = list(calls[0]["files"].values())
        self.assertEqual(len(files), 1)
        self.assert_clean(files[0]["content"], "msg file")
        self.assertIn(PREVIEW, files[0]["content"])

    def test_after_lost_access_with_the_journal_down_the_tail_and_the_journal(self):
        world = World(self)
        mon = world.monitor("tok", "claude", "claude", SKEIN_MONITOR_CHANNEL="zavod", **world.zavod_env("claude"))
        outage = RUNNER.JournalUnavailable("write_failed", mon.journal.path)
        with mock.patch.object(mon.journal, "delivery", side_effect=outage):
            with self.assertRaises(RUNNER.JournalUnavailable):
                mon.page([note(1), note(2)])
            # Access is lost while the page is undone and the journal is down.
            mon.sink.end_open_page("token_expired")
        tails = []
        for path, _, files in os.walk(world.root):
            for name in files:
                full = os.path.join(path, name)
                if os.path.basename(path) == RUNNER.TAILS_DIR_NAME:
                    with open(full, errors="replace") as fh:
                        tails.append(fh.read())
        self.assertTrue(tails, "no deferred tail was formed")
        for text in tails:
            self.assert_clean(text, "deferred tail")
            self.assertNotIn(PREVIEW, text)
        RUNNER.append_tails(mon.env)
        events = [l for l in mon.lines() if l.get("event") == "delivery"]
        self.assertTrue(events)
        for l in events:
            self.assertNotIn("bodyPreview", l)
        with open(mon.journal.path) as fh:
            self.assert_clean(fh.read(), "journal after the tail")


if __name__ == "__main__":
    unittest.main()
