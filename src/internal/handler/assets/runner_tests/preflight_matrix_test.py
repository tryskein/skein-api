#!/usr/bin/env python3
"""The preflight matrix of wave 1 over the registry the server embeds
(SC-002, US-9, US-6): matrix rows #80, #81, #83, #84, #85 and #122.

The preflight runs in process against registry.json with the records of
wave 1 (claude, grok, kimi, codex, the codex-queue channel and the zavod
host). The chain of ancestor processes is a fake one (fake_proc.py) whose
agent process is this test process under the name of the record, so the
working runner's check that the agent process is alive holds. A property
of the general algorithm that needs a record wave 1 does not have (a blind
channel, a terminal environment, a setting) is checked with a synthetic
record added to a copy of the registry; such a test does not stand for a
row of the epic that names the real record.

Run on its own: python3 internal/handler/assets/runner_tests/preflight_matrix_test.py
Standard library of Python 3.6 only.
"""

import base64
import io
import json
import os
import shutil
import signal
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_channel  # noqa: E402
import fake_proc  # noqa: E402
import harness  # noqa: E402

RUNNER = harness.load_runner()
BODY = harness.render_runner()
REG = RUNNER.load_registry()
ISSUE = "sk-80"
TOKEN = "skm_" + "M" * 43
# A Zavod address and the engine a test names.
ZAVOD_UUID = "4b8f0c2e-1d3a-4c5b-9e6f-7a8b9c0d1e2f"
CODEX_THREAD = "019a8e3c-5d2b-7c41-9f0e-1a2b3c4d5e6f"
# The runner process in a fake chain: python3 running the loader.
RUNNER_PID = 70001
RUNNER_PROC_ARGV = ["python3", "-c", "_SKEIN_LOADER_MODE = 'client'"]
STOP_EVENTS = {"config_invalid": "closed", "already_running": "duplicate", "delivered": "paused"}


def own_start():
    return RUNNER.current_platform().process_info(os.getpid()).start


def chain(agents, shell_between=True, agent_argv=None):
    """A fake chain, the runner first: the runner, a shell, then the agent
    processes of agents (record executable names), the nearest first, each
    behind a shell when shell_between. The nearest agent process is this
    test process (its real pid and start). Returns (placement source, pids
    of the agent processes)."""
    source = fake_proc.FakeProcesses()
    pids = []
    procs = [("python3", RUNNER_PROC_ARGV, RUNNER_PID, None), ("bash", None, RUNNER_PID - 1, None)]
    for i, name in enumerate(agents):
        pid = os.getpid() if i == 0 else RUNNER_PID - 10 - i
        argv = agent_argv if (i == 0 and agent_argv is not None) else None
        procs.append((name, argv, pid, own_start() if i == 0 else None))
        pids.append(pid)
        if shell_between and i + 1 < len(agents):
            procs.append(("bash", None, RUNNER_PID - 20 - i, None))
    procs.append(("tmux", None, RUNNER_PID - 50, None))
    for i, (name, argv, pid, start) in enumerate(procs):
        ppid = procs[i + 1][2] if i + 1 < len(procs) else 1
        source.add(pid, ppid, name, argv, start=start)
    return source, pids


def placement(registry, client, agents, **kw):
    source, pids = chain(agents, **kw)
    return RUNNER.place(RUNNER.ancestry(source, RUNNER_PID), registry, RUNNER.resolve_record(registry, client),
                        self_pid=RUNNER_PID), pids


class FakeProbe(object):
    """Answers every probe "available" unless told otherwise per channel."""

    def __init__(self, answers=None):
        self.answers = answers or {}

    def run(self, chan, probe, env):
        return self.answers.get(chan["id"], "available")


class Binder(object):
    """Confirms the binding of the runner to a pane of the blind channels
    named."""

    def __init__(self, channels=()):
        self.channels = list(channels)

    def bind(self, ctx, channel):
        if channel["id"] not in self.channels:
            return None
        return RUNNER.Binding(channel, True, pane={"pane": "%1"})


def registry_with(channels=(), agents=()):
    """A copy of the embedded registry with synthetic channels and agents
    added: (runner, registry)."""
    with open(harness.REGISTRY_JSON) as fh:
        doc = json.load(fh)
    doc["channels"].extend(channels)
    doc["agents"].extend(agents)
    b64 = base64.b64encode(json.dumps(doc).encode("utf-8")).decode("ascii")
    runner = harness.load_runner(registry=b64)
    return runner, runner.load_registry(), b64


def synthetic_blind(cid="blind-x"):
    return {"id": cid, "about": "synthetic blind channel", "source": "test", "step": 7, "order": 1,
            "kind": "terminal", "blind": True, "detect": {"env": [{"name": "BLIND_X_PANE"}]},
            "executor": {"type": "argv", "argv": ["blind-x", "{env:BLIND_X_PANE}", "{text}"], "feed": "arg",
                         "timeoutSec": 5},
            "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}],
            "probeUndefined": True, "addressee": {"process": "foreground"},
            "panePid": {"argv": ["blind-x", "pid"]}, "binaries": {"linux": ["blind-x"], "macos": ["blind-x"]}}


def synthetic_terminal(cid, var, order):
    return {"id": cid, "about": "synthetic terminal environment", "source": "test", "step": 6, "order": order,
            "kind": "terminal", "blind": False, "detect": {"env": [{"name": var}]},
            "executor": {"type": "argv", "argv": [cid, "send", "{env:%s}" % var], "feed": "stdin", "timeoutSec": 5},
            "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}],
            "probeUndefined": True, "addressee": {"process": "alive"},
            "binaries": {"linux": [cid], "macos": [cid]}}


class Box(object):
    """A private token directory with its token, a state home, a bin
    directory with fake zavod and codex programs, and the client
    environment of one monitor."""

    def __init__(self, test, name="tok"):
        self.test = test
        self.root = tempfile.mkdtemp(prefix="skein-pfm-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin, 0o700)
        self.progs = {}
        for program in ("zavod", "codex", "blind-x", "term-a", "term-b"):
            self.progs[program] = fake_channel.FakeProgram(self.bin, program, watch=("CODEX_HOME",))
        self.progs["zavod"].respond([{"exit": 0, "stdout": {"zavod": 1, "command": "send", "outcome": "ok",
                                                            "exit_code": 0, "code": None}}])
        self.state_home = os.path.join(self.root, "state")
        self.tokens = {}
        self.token = self.add_token(name)
        self.pids = []
        test.addCleanup(self.kill_children)

    def add_token(self, name):
        token_dir = os.path.join(self.root, "skein-mon-" + name)
        os.mkdir(token_dir, 0o700)
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(token, 0o600)
        self.tokens[name] = token
        return token

    def env(self, client="claude", token=None, **extra):
        env = {"PATH": self.bin + os.pathsep + os.environ.get("PATH", os.defpath), "HOME": self.root,
               "XDG_STATE_HOME": self.state_home, "SKEIN_PUBLIC_URL": "http://127.0.0.1:9", "SKEIN_ISSUE": ISSUE,
               "SKEIN_TOKEN_FILE": token or self.token, "SKEIN_EXCLUDE_SESSION": "", "SKEIN_RUNNER_SHA256": "ab" * 32,
               "SKEIN_LOADER_SHA256": "cd" * 32, "SKEIN_CLIENT": client, "SKEIN_PLATFORM": "posix",
               "SKEIN_MONITOR_CHANNEL": "", "SKEIN_MONITOR_BLIND": ""}
        for k, v in extra.items():
            if v is None:
                env.pop(k, None)
            else:
                env[k] = v
        return env

    def zavod_env(self, engine, root_pid=None, **extra):
        """The variables Zavod sets on its agent's process, its root process
        being root_pid (this test process by default)."""
        values = {"ZAVOD_ME": ZAVOD_UUID, "ZAVOD_AGENT_ID": ZAVOD_UUID, "ZAVOD_AGENT_ENGINE_ID": engine,
                  "ZAVOD_AGENT_ROOT_PID": str(os.getpid() if root_pid is None else root_pid),
                  "ZAVOD_AGENT_ROOT_START": "181027616", "ZAVOD_AGENT_ROOT_BOOT_ID": "boot-1"}
        values.update(extra)
        return values

    def preflight(self, env, agents, runner=RUNNER, registry=REG, prober=None, binder=None, **kw):
        """A preflight of the token of env from the agent chain agents;
        returns its line, or the ConfigInvalid it raised."""
        pl, _ = placement(registry, env["SKEIN_CLIENT"], agents, **kw)
        try:
            settings = runner.read_settings(env, registry, mode="client")
            return runner.preflight(env["SKEIN_TOKEN_FILE"], env, registry, settings,
                                    detector=runner.RegistryDetector(binder or Binder()),
                                    prober=prober or FakeProbe(), placement=pl)
        except runner.ConfigInvalid as exc:
            return exc

    def saved(self, token=None):
        with open(os.path.join(os.path.dirname(token or self.token), "preflight.json")) as fh:
            return json.load(fh)

    def run(self, env, entry="start", runner=RUNNER, body=BODY):
        """A start of env as the loader runs it in client mode; returns
        (exit code, lines printed)."""
        lines = []
        spawn = runner.PosixPlatform.spawn_detached

        def spy(*args):
            pid, receipt = spawn(*args)
            self.pids.append(pid)
            return pid, receipt

        out = io.StringIO()
        with mock.patch.dict(os.environ, env, clear=True), \
                mock.patch.object(runner, "LOADER_MODE", "client"), \
                mock.patch.object(runner, "_skein_loader_body", body, create=True), \
                mock.patch.object(runner.PosixPlatform, "spawn_detached", staticmethod(spy)), \
                mock.patch.object(runner, "emit", lines.append), \
                mock.patch("sys.stdout", out):
            code = getattr(runner, entry)()
        return code, lines + [json.loads(l) for l in out.getvalue().splitlines() if l]

    def kill_children(self):
        for pid in self.pids:
            try:
                os.killpg(pid, signal.SIGKILL)
            except OSError:
                pass

    def journal_lines(self, env):
        try:
            with open(RUNNER.journal_path(env["SKEIN_TOKEN_FILE"], env)) as fh:
                return [json.loads(l) for l in fh if l.strip()]
        except FileNotFoundError:
            return []


def merged(env, extra):
    """env with extra applied: a value None removes the variable."""
    out = dict(env)
    for k, v in extra.items():
        if v is None:
            out.pop(k, None)
        else:
            out[k] = v
    return out


def wait_until(pred, sec=30):
    deadline = time.monotonic() + sec
    while time.monotonic() < deadline:
        if pred():
            return True
        time.sleep(0.05)
    return False


def one_stop(test, code, lines, reason, want_code):
    test.assertEqual(len(lines), 1, lines)
    test.assertEqual(lines[0]["reason"], reason, lines)
    test.assertEqual(lines[0]["event"], STOP_EVENTS[reason])
    test.assertEqual(code, want_code)


def event(nid, session="s-other", preview="hello"):
    return {"id": nid, "type": "comment_added", "entity": {"type": "issue", "id": ISSUE},
            "actor": {"username": "bob"}, "actorSessionId": session, "commentId": "c%d" % nid,
            "bodyPreview": preview, "truncated": False}


def page_server(test, pages):
    """A poll server answering with pages in turn, then empty pages; the
    number of polls it saw is in .polls."""
    queue = list(pages)
    counter = {"n": 0}

    def respond(h, qs):
        counter["n"] += 1
        if queue:
            h.send_json(200, queue.pop(0))
        else:
            h.send_json(200, harness.EMPTY)

    server = harness.ScriptedServer(respond)
    server.counter = counter
    test.addCleanup(server.stop)
    return server


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight and detached start")
class ForcedChannelTest(unittest.TestCase):
    """Row #80 (SC-002 of the epic "forced channel, also for Claude", US-9):
    SKEIN_MONITOR_CHANNEL=zavod chooses Zavod; for Claude, a stream record,
    the monitor is detached and its stdout carries no notification line;
    a refusing forced channel has the journal, never another channel."""

    def test_forced_zavod_for_claude_is_detached_without_stdout_delivery(self):
        box = Box(self)
        server = page_server(self, [{"notifications": [event(1)], "cursor": {"ts": "t", "id": 1}}])
        env = box.env("claude", SKEIN_PUBLIC_URL=server.url(), SKEIN_MONITOR_CHANNEL="zavod",
                      **box.zavod_env("claude"))
        line = box.preflight(env, ["claude"])
        self.assertEqual((line["method"], line["channel"]), ("detached", "zavod"), line)
        self.assertEqual(box.saved()["fallbacks"], [])
        self.assertNotIn("Monitor tool", line["instruction"])
        code, lines = box.run(dict(env, SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT=str(line["preflightId"])))
        self.assertEqual(code, 0)
        self.assertEqual([l["event"] for l in lines], ["monitor_started"], lines)
        self.assertEqual(lines[0]["channel"], "zavod")
        self.assertTrue(wait_until(lambda: box.progs["zavod"].calls()), "the event never reached zavod")
        call = box.progs["zavod"].calls()[0]
        self.assertEqual(call["argv"], ["--json", "send", ZAVOD_UUID, "--text-file", "-"])
        self.assertIn('notification="1"', call["stdin"])
        self.assertFalse(any(l.get("event") == "notification" for l in lines))

    def test_a_forced_channel_that_is_unavailable_gives_the_journal(self):
        box = Box(self)
        # Codex under Zavod: without forcing, codex queue first and Zavod its
        # fallback; forced Zavod has no fallback; forced and unavailable, the
        # journal - codex queue is not taken instead.
        base = box.env("codex", CODEX_THREAD_ID=CODEX_THREAD, **box.zavod_env("codex"))
        line = box.preflight(base, ["codex"])
        self.assertEqual((line["method"], line["channel"]), ("detached", "codex-queue"))
        self.assertEqual(box.saved()["fallbacks"], ["zavod"])
        line = box.preflight(dict(base, SKEIN_MONITOR_CHANNEL="zavod"), ["codex"])
        self.assertEqual((line["channel"], box.saved()["fallbacks"]), ("zavod", []))
        line = box.preflight(dict(base, SKEIN_MONITOR_CHANNEL="zavod"), ["codex"],
                             prober=FakeProbe({"zavod": "unavailable"}))
        self.assertEqual((line["method"], line["channel"]), ("journal", None), line)
        self.assertIn("forced zavod unavailable", line["reason"])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class OverrideValidityTest(unittest.TestCase):
    """Row #81 (SC-002 of the epic "blind ban", "invalid override",
    "unreadable setting"): BLIND=off wins over a forced blind channel
    (synthetic blind record); an invalid value is config_invalid in the
    preflight and at the working start; a setting that cannot be read makes
    its method unavailable and the next one is chosen (synthetic record)."""

    def test_the_blind_ban_wins_over_a_forced_blind_channel(self):
        runner, reg, _ = registry_with(channels=[synthetic_blind()])
        box = Box(self)
        env = box.env("no-such-client", BLIND_X_PANE="%1", SKEIN_MONITOR_CHANNEL="blind-x")
        line = box.preflight(env, ["bash-agent"], runner=runner, registry=reg, binder=Binder(["blind-x"]))
        self.assertEqual((line["method"], line["channel"]), ("detached", "blind-x"), line)
        line = box.preflight(dict(env, SKEIN_MONITOR_BLIND="off"), ["bash-agent"], runner=runner, registry=reg,
                             binder=Binder(["blind-x"]))
        self.assertEqual((line["method"], line["channel"]), ("journal", None), line)
        self.assertTrue(any("forbids blind channels" in w for w in line["warnings"]), line["warnings"])

    def test_an_invalid_override_is_config_invalid_in_the_preflight_and_at_the_start(self):
        box = Box(self)
        for name, value in (("SKEIN_MONITOR_CHANNEL", "no-such-channel"), ("SKEIN_MONITOR_BLIND", "yes")):
            with self.subTest(variable=name):
                env = box.env("claude", **{name: value})
                code, lines = box.run(dict(env, SKEIN_MONITOR_MODE="preflight"))
                one_stop(self, code, lines, "config_invalid", 1)
                self.assertIn(name, lines[0]["action"])
                self.assertFalse(os.path.exists(os.path.join(os.path.dirname(box.token), "preflight.json")))
                code, lines = box.run(dict(env, SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT="1"))
                one_stop(self, code, lines, "config_invalid", 1)
                self.assertIn(name, lines[0]["action"])
                # The token is kept (US-9).
                self.assertTrue(os.path.exists(box.token))

    def test_a_setting_that_cannot_be_read_makes_the_method_unavailable(self):
        cfg_dir = tempfile.mkdtemp(prefix="skein-pfm-cfg-")
        self.addCleanup(shutil.rmtree, cfg_dir, True)
        setty = {"id": "setty", "about": "synthetic", "source": "test", "process": {"executables": ["setty"]},
                 "methods": [
                     {"kind": "stream", "instruction": "stream it", "conditions": {"settings": [
                         {"path": "{env:SETTY_DIR}/cfg.json", "format": "json", "key": "monitor", "equals": True}]}},
                     {"kind": "exit-on-event", "instruction": "exit it", "conditions": {}}]}
        runner, reg, _ = registry_with(agents=[setty])
        box = Box(self)
        env = box.env("setty", SETTY_DIR=cfg_dir)
        cases = [("no file", None), ("not JSON", "{monitor: true"), ("another value", json.dumps({"monitor": False})),
                 ("as required", json.dumps({"monitor": True}))]
        for name, content in cases:
            with self.subTest(case=name):
                path = os.path.join(cfg_dir, "cfg.json")
                if content is None:
                    if os.path.exists(path):
                        os.remove(path)
                else:
                    with open(path, "w") as fh:
                        fh.write(content)
                line = box.preflight(env, ["setty"], runner=runner, registry=reg)
                want = "stream" if name == "as required" else "exit-on-event"
                self.assertEqual(line["method"], want, line)
                if want != "stream":
                    self.assertIn("setting monitor", line["reason"])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class ZavodRecordsTest(unittest.TestCase):
    """Row #83 (SC-002): a nested agent of the same type with the outer
    agent's Zavod variables gets no delivery to the outer one, forced or
    not; the Claude wrapper under Zavod (kimi -> claude) and the native Kimi
    (nkimi -> kimi) find the host only when the engine map matches the
    client; Kimi TUI under Zavod takes its own exit-on-event before the
    host."""

    def test_a_nested_agent_does_not_deliver_to_the_outer_one(self):
        box = Box(self)
        outer = RUNNER_PID - 11
        env = box.env("codex", **box.zavod_env("codex", root_pid=outer))
        line = box.preflight(env, ["codex", "codex"])
        self.assertEqual((line["method"], line["channel"]), ("journal", None), line)
        result = box.preflight(dict(env, SKEIN_MONITOR_CHANNEL="zavod"), ["codex", "codex"])
        self.assertIsInstance(result, RUNNER.ConfigInvalid)
        self.assertEqual(result.variable, "SKEIN_MONITOR_CHANNEL")
        self.assertIn("outer agent", result.detail)
        # Control: the same agent, not nested, is Zavod's.
        env_own = box.env("codex", **box.zavod_env("codex"))
        line = box.preflight(env_own, ["codex"])
        self.assertEqual((line["method"], line["channel"]), ("detached", "zavod"), line)

    def test_the_engine_map_decides_whether_zavod_is_this_agents(self):
        box = Box(self)
        # (client, process name, engine, Zavod found)
        cases = [("claude", "claude", "kimi", True), ("claude", "claude", "zai-claude", True),
                 ("kimi", "kimi-code", "nkimi", True), ("kimi", "kimi-code", "kimi", False),
                 ("claude", "claude", "nkimi", False), ("codex", "codex", "claude", False)]
        for client, proc, engine, found in cases:
            with self.subTest(client=client, engine=engine):
                env = box.env(client, SKEIN_MONITOR_CHANNEL="zavod", **box.zavod_env(engine))
                line = box.preflight(env, [proc])
                want = ("detached", "zavod") if found else ("journal", None)
                self.assertEqual((line["method"], line["channel"]), want, line)

    def test_kimi_tui_under_zavod_takes_its_own_exit_on_event(self):
        box = Box(self)
        env = box.env("kimi", **box.zavod_env("nkimi"))
        line = box.preflight(env, ["kimi-code"])
        self.assertEqual((line["method"], line["channel"]), ("exit-on-event", "stdout"), line)
        self.assertIn("run_in_background=true", line["instruction"])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class GeneralRecordTest(unittest.TestCase):
    """Row #84 (SC-002): an unknown client gets the general record, which
    the preflight line names; the property "Zavod detected - no blind
    channel" with a synthetic blind record (not the epic row Zavod + tmux)."""

    def test_an_unknown_client_gets_the_general_record(self):
        box = Box(self)
        line = box.preflight(box.env("no-such-agent"), ["bash-agent"])
        self.assertEqual(line["client"], "general")
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        line = box.preflight(box.env("no-such-agent", **box.zavod_env("codex")), ["bash-agent"])
        self.assertEqual((line["client"], line["method"], line["channel"]), ("general", "detached", "zavod"), line)

    def test_a_detected_zavod_keeps_the_blind_channel_off(self):
        runner, reg, _ = registry_with(channels=[synthetic_blind()])
        box = Box(self)
        env = box.env("no-such-agent", BLIND_X_PANE="%1")
        binder = Binder(["blind-x"])
        line = box.preflight(env, ["bash-agent"], runner=runner, registry=reg, binder=binder)
        self.assertEqual((line["method"], line["channel"]), ("detached", "blind-x"), line)
        # Zavod detected but its channel unavailable: no blind channel.
        line = box.preflight(dict(env, **box.zavod_env("codex")), ["bash-agent"], runner=runner, registry=reg,
                             binder=binder, prober=FakeProbe({"zavod": "unavailable"}))
        self.assertEqual((line["method"], line["channel"]), ("journal", None), line)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight and working start")
class PreflightResultsTest(unittest.TestCase):
    """Row #85 (SC-002, results of the preflight)."""

    def start_env(self, box, line, **extra):
        env = box.env("kimi", SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT=str(line["preflightId"]))
        env.update(extra)
        return env

    def test_a_working_start_without_a_result_or_with_another_one(self):
        box = Box(self)
        server = page_server(self, [])
        env = box.env("kimi", SKEIN_PUBLIC_URL=server.url(), SKEIN_MONITOR_MODE="run", SKEIN_MONITOR_PREFLIGHT="1")
        code, lines = box.run(env)
        one_stop(self, code, lines, "config_invalid", 1)
        self.assertIn("SKEIN_MONITOR_PREFLIGHT", lines[0]["action"])
        first = box.preflight(box.env("kimi"), ["kimi-code"])
        second = box.preflight(box.env("kimi"), ["kimi-code"])
        self.assertEqual(second["preflightId"], first["preflightId"] + 1)
        cases = [("the first of two results", {"SKEIN_MONITOR_PREFLIGHT": str(first["preflightId"])},
                  "SKEIN_MONITOR_PREFLIGHT"),
                 ("another override", {"SKEIN_MONITOR_PREFLIGHT": str(second["preflightId"]),
                                       "SKEIN_MONITOR_CHANNEL": "zavod"}, "SKEIN_MONITOR_CHANNEL"),
                 ("no number", {"SKEIN_MONITOR_PREFLIGHT": None}, "SKEIN_MONITOR_PREFLIGHT"),
                 ("a mode other than run", {"SKEIN_MONITOR_MODE": "go"}, "SKEIN_MONITOR_MODE"),
                 ("another platform", {"SKEIN_PLATFORM": "windows"}, "SKEIN_PLATFORM")]
        for name, extra, variable in cases:
            with self.subTest(case=name):
                code, lines = box.run(merged(env, extra))
                one_stop(self, code, lines, "config_invalid", 1)
                self.assertIn(variable, lines[0]["action"])
        # None of these starts polled.
        self.assertEqual(server.counter["n"], 0)

    def test_a_preflight_with_a_live_runner_answers_already_running(self):
        box = Box(self)
        env = box.env("kimi")
        first = box.preflight(env, ["kimi-code"])
        lock = RUNNER.PosixPlatform.acquire_runner_lock(box.token, 0, env)
        self.addCleanup(lock.release)
        pl, _ = placement(REG, "kimi", ["kimi-code"])
        with self.assertRaises(RUNNER.Stop) as caught:
            RUNNER.preflight(box.token, env, REG, RUNNER.read_settings(env, REG, mode="client"),
                             detector=RUNNER.RegistryDetector(Binder()), prober=FakeProbe(), placement=pl)
        self.assertEqual(caught.exception.reason, "already_running")
        self.assertEqual(box.saved()["preflightId"], first["preflightId"])

    def test_a_working_start_waits_out_the_instant_a_preflight_holds_the_lock(self):
        box = Box(self)
        env = box.env("kimi")
        box.preflight(env, ["kimi-code"])
        # The preflight's instant: state.lock and the runner lock held.
        state = RUNNER.StateLock(box.token)
        runner_lock = RUNNER.PosixPlatform.acquire_runner_lock(box.token, 0, env)
        got = {}

        def start():
            try:
                got["lock"] = RUNNER.take_runner_lock(box.token, env)
            except Exception as exc:  # noqa: BLE001 - the outcome is what the test reads
                got["error"] = exc

        t = threading.Thread(target=start)
        t.start()
        time.sleep(0.3)
        self.assertNotIn("error", got)
        self.assertNotIn("lock", got)
        runner_lock.release()
        state.__exit__(None, None, None)
        t.join(10)
        self.assertIn("lock", got, got)
        got["lock"].release()

    def test_the_same_working_command_starts_again(self):
        # Re-arm and the cycle of exit-on-event: the same command, the same
        # number, is an ordinary start each time, in an environment that
        # sets no override (the command carries them).
        box = Box(self)
        server = page_server(self, [{"notifications": [event(1)], "cursor": {"ts": "t1", "id": 1}},
                                    {"notifications": [event(2)], "cursor": {"ts": "t2", "id": 2}}])
        line = box.preflight(box.env("kimi"), ["kimi-code"])
        self.assertEqual(line["method"], "exit-on-event")
        env = self.start_env(box, line, SKEIN_PUBLIC_URL=server.url())
        for _ in range(2):
            code, lines = box.run(env)
            self.assertEqual(code, 0, lines)
            self.assertEqual(lines[-1]["reason"], "delivered", lines)
        notes = [l["notification"]["id"] for l in lines if l.get("event") == "notification"]
        self.assertEqual(notes, [2])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class AmbiguousTerminalTest(unittest.TestCase):
    """Row #122 (US-6): two terminal environments are ambiguous; a new
    preflight that names one of them chooses it, one delivery goes there and
    no blind channel is a fallback (synthetic records, fake programs)."""

    def test_delivery_starts_after_the_override(self):
        runner, reg, _ = registry_with(channels=[synthetic_terminal("term-a", "TERM_A_ID", 1),
                                                 synthetic_terminal("term-b", "TERM_B_ID", 2), synthetic_blind()])
        box = Box(self)
        env = box.env("no-such-agent", TERM_A_ID="a-1", TERM_B_ID="b-1", BLIND_X_PANE="%1")
        binder = Binder(["blind-x"])
        line = box.preflight(env, ["bash-agent"], runner=runner, registry=reg, binder=binder)
        self.assertEqual((line["method"], line["channel"]), ("journal", None), line)
        self.assertTrue(any("ambiguous_terminal" in w for w in line["warnings"]), line["warnings"])
        self.assertEqual(box.saved()["degraded"]["cause"], "ambiguous_terminal")
        line = box.preflight(dict(env, SKEIN_MONITOR_CHANNEL="term-a"), ["bash-agent"], runner=runner,
                             registry=reg, binder=binder)
        self.assertEqual((line["method"], line["channel"]), ("detached", "term-a"), line)
        doc = box.saved()
        self.assertEqual(doc["fallbacks"], [])
        journal = runner.Journal(box.token, ISSUE, secrets=[TOKEN], env=env)
        ledger = runner.Ledger("http://127.0.0.1:9", "tok", runner.identity_from_doc(doc["identity"]), env=env)
        executor = runner.ArgvExecutor(box.token, env, [TOKEN])
        sink = runner.DeliverySink(journal, box.token, doc, reg, env, ledger, executor, ISSUE)
        sink.page({"ts": "t", "id": 1}, [event(1)])
        self.assertEqual(len(box.progs["term-a"].calls()), 1)
        self.assertEqual(box.progs["term-b"].calls(), [])
        self.assertEqual(box.progs["blind-x"].calls(), [])


if __name__ == "__main__":
    unittest.main()
