#!/usr/bin/env python3
"""SC-001 of the Paseo (agent) record and the rule "agent under Paseo"
(matrix rows #95, #96), over the registry the server embeds.

The expected values come from the Paseo source at c1b43ee5 (0.17.4), not
from the record:
- the daemon gives every agent process it launches PASEO_AGENT_ID
  (packages/server/src/server/agent/agent-manager.ts) and runs as the
  process titled "Paseo Daemon" (packages/server/src/server/daemon-worker.ts);
- `paseo agent send <id> --prompt-file <file> --no-wait --json` reads the
  text from the file and prints {agentId, status, message}, status
  "queued" when the daemon queued it (packages/cli/src/commands/agent/
  send.ts#buildNoWaitResult); errors go to stderr as {"error": {"code"}}
  with exit 1 (packages/cli/src/output/with-output.ts, render.ts#
  renderError); DAEMON_UNREACHABLE and AUTH_REQUIRED come from connecting,
  before anything is sent (packages/cli/src/utils/client.ts#
  connectToDaemon); a failure after it is SEND_FAILED;
- `paseo agent inspect <id> --json` prints the agent (Id, Status, Archived)
  and AGENT_NOT_FOUND on stderr when the daemon does not know it
  (packages/cli/src/commands/agent/inspect.ts);
- the app installs the CLI as ~/.local/bin/paseo, which the agent's PATH
  may lack (packages/desktop/src/integrations/cli-install/install.ts);
- agents under Paseo: Claude keeps its own Monitor (Agent SDK), Codex
  (app-server), Kimi and the other ACP agents get events by `paseo agent
  send`; the ACP terminal of Paseo is the daemon's own terminal.

Run on its own: python3 internal/handler/assets/runner_tests/entries_paseo_test.py
Standard library of Python 3.6 only.
"""

import base64
import copy
import json
import os
import stat
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_orca_test as eo  # noqa: E402
import failures_test as ft  # noqa: E402
import harness  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG
AGENT_ID = "3f6c2a1e-8b4d-4e9a-9c07-5a1b2c3d4e5f"
DAEMON = ("Paseo Daemon", ["Paseo Daemon"], None)
THREAD = "019a8e3c-5d2b-7c41-9f0e-eeeeeeeeeeee"

_PASEO = r'''#!%(python)s
import json, os, sys
HERE = %(here)r
NAME = %(name)r
args = sys.argv[1:]
sub = args[1] if len(args) > 1 else ""
text = None
if "--prompt-file" in args:
    path = args[args.index("--prompt-file") + 1]
    with open(path, encoding="utf-8") as fh:
        text = fh.read()
with open(os.path.join(HERE, NAME + ".calls.jsonl"), "a") as fh:
    fh.write(json.dumps({"argv": args, "text": text}) + "\n")
with open(os.path.join(HERE, NAME + ".answers.json")) as fh:
    answers = json.load(fh).get(sub) or [{"exit": 0}]
count = os.path.join(HERE, NAME + "." + sub + ".count")
n = int(open(count).read()) if os.path.exists(count) else 0
open(count, "w").write(str(n + 1))
a = answers[min(n, len(answers) - 1)]
for stream, key in ((sys.stdout, "stdout"), (sys.stderr, "stderr")):
    out = a.get(key, "")
    stream.write(out if isinstance(out, str) else json.dumps(out, indent=2))
sys.exit(a.get("exit", 0))
'''


def inspected(archived=False):
    return {"exit": 0, "stdout": {"Id": AGENT_ID, "Name": "-", "Provider": "codex", "Status": "idle",
                                  "Archived": archived, "CreatedAt": "2026-11-25T10:00:00.000Z"}}


def queued():
    return {"exit": 0, "stdout": {"agentId": AGENT_ID, "status": "queued", "activeTurnBehavior": "queue",
                                  "message": "Queued. The agent is idle, so your message starts a turn in about 2 "
                                             "seconds"}}


def error(code):
    return {"exit": 1, "stderr": {"error": {"code": code, "message": "failure"}}}


class FakePaseo(object):
    """A fake paseo CLI at directory/paseo: per subcommand (send, inspect)
    it answers in turn; it records argv and the text of --prompt-file."""

    def __init__(self, directory):
        self.dir = directory
        os.makedirs(directory, exist_ok=True)
        self.path = os.path.join(directory, "paseo")
        with open(self.path, "w") as fh:
            fh.write(_PASEO % {"python": sys.executable, "here": directory, "name": "paseo"})
        os.chmod(self.path, stat.S_IRWXU)
        self.respond()

    def respond(self, send=None, inspect=None):
        with open(os.path.join(self.dir, "paseo.answers.json"), "w") as fh:
            json.dump({"send": send or [queued()], "inspect": inspect or [inspected()]}, fh)
        for sub in ("send", "inspect"):
            count = os.path.join(self.dir, "paseo." + sub + ".count")
            if os.path.exists(count):
                os.remove(count)

    def calls(self, sub=None):
        try:
            with open(os.path.join(self.dir, "paseo.calls.jsonl")) as fh:
                calls = [json.loads(l) for l in fh if l.strip()]
        except OSError:
            return []
        return [c for c in calls if sub is None or c["argv"][1] == sub]


def launched(name, pid):
    """The runner under a shell under an agent process (pid, a live process
    other than this one: the runner's own pid is never an agent's) the
    Paseo daemon launched."""
    return [("bash", None, None), (name, [name], pid), DAEMON]


def in_terminal(name):
    """The runner under a shell under an agent (this process) a person
    started in a terminal."""
    return [("bash", None, None), (name, [name], os.getpid()), ("bash", None, None)]


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class PaseoRecordTest(unittest.TestCase):
    """Row #95 (SC-001, US-5): recognition, the call with the text in a
    file, every sign of the table, the check and the probe, the CLI outside
    PATH, and Paseo after Stop."""

    def fresh(self, outside_path=False):
        self.agent = ft.Sleeper(self)
        self.world = eo.new_world(self)
        directory = os.path.join(self.world.root, ".local", "bin") if outside_path else self.world.bin
        self.paseo = FakePaseo(directory)

    def mon(self, client="codex", procs=None, **extra):
        return eo.monitor(self.world, "tok", client, procs or launched(client, self.agent.pid),
                          PASEO_AGENT_ID=AGENT_ID, **extra)

    def test_recognised_by_the_agent_id_under_the_daemon(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "paseo"))
        line = eo.monitor(self.world, "tok-none", "codex", launched("codex", self.agent.pid)).line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        line = eo.monitor(self.world, "tok-bad", "codex", launched("codex", self.agent.pid),
                          PASEO_AGENT_ID="agent-7").line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_text_goes_in_a_file_that_is_removed(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        [send] = self.paseo.calls("send")
        argv = send["argv"]
        self.assertEqual(argv[:4], ["agent", "send", AGENT_ID, "--prompt-file"])
        self.assertEqual(argv[5:], ["--no-wait", "--json"])
        self.assertNotIn("--behavior", argv)
        self.assertIn('notification="1"', send["text"])
        self.assertNotIn(ft.PREVIEW, " ".join(argv))
        self.assertFalse(os.path.exists(argv[4]))
        # The check before the transfer is the inspection of the agent.
        self.assertTrue(self.paseo.calls("inspect"))
        self.assertTrue(all(c["argv"] == ["agent", "inspect", AGENT_ID, "--json"]
                            for c in self.paseo.calls("inspect")))

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        cases = [
            ("queued", queued(), "queued"),
            ("sent, an older host", {"exit": 0, "stdout": {"agentId": AGENT_ID, "status": "sent",
                                                            "message": "Message sent, not waiting for completion"}},
             "unknown"),
            ("a failure after connecting", error("SEND_FAILED"), "unknown"),
            ("another connection code", error("AUTH_FAILED"), "unknown"),
            ("the refusal code on stdout", {"exit": 1, "stdout": {"error": {"code": "DAEMON_UNREACHABLE"}}}, "unknown"),
            ("no JSON", {"exit": 0, "stdout": "Queued.\n"}, "unknown"),
            ("an exit code of another version", {"exit": 2, "stdout": queued()["stdout"]}, "unknown"),
        ]
        for i, (name, answer, outcome) in enumerate(cases):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.paseo.respond(send=[answer])
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                self.assertEqual(len(self.paseo.calls("send")), 1)

    def test_only_the_same_agent_gets_the_event(self):
        # INV-4, the check before each transfer: an archived agent and one
        # the daemon does not know have ended (the stop addressee_gone, as
        # Zavod's caller_not_in_roster); nothing is sent to them.
        for name, answer in (("archived", inspected(archived=True)), ("not found", error("AGENT_NOT_FOUND"))):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.paseo.respond(inspect=[answer])
                with self.assertRaises(RUNNER.Stop) as caught:
                    mon.page([ft.note(1)])
                self.assertEqual(caught.exception.reason, "addressee_gone")
                self.assertEqual(self.paseo.calls("send"), [])

    def test_a_check_that_proves_nothing_is_a_refusal_before_send(self):
        # A check that fails, times out or answers outside its table proves
        # neither the same agent nor another: nothing is sent, and the
        # delivery is called again within the budget of INV-8.
        cases = [("the daemon unreachable", error("DAEMON_UNREACHABLE")),
                 ("no JSON", {"exit": 0, "stdout": "agent 3f6c\n"}),
                 ("an exit code of another version", {"exit": 3, "stdout": inspected()["stdout"]})]
        for name, answer in cases:
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.paseo.respond(inspect=[answer, inspected()])
                mon.page([ft.note(1)])
                self.assertEqual(mon.statuses(), [(1, "queued")])
                self.assertEqual(len(self.paseo.calls("send")), 1)
                self.assertGreaterEqual(len(self.paseo.calls("inspect")), 2)

    def test_a_refusal_while_connecting_is_repeated(self):
        for code in ("DAEMON_UNREACHABLE", "AUTH_REQUIRED"):
            with self.subTest(code=code):
                self.fresh()
                mon = self.mon()
                self.paseo.respond(send=[error(code), queued()])
                mon.page([ft.note(1)])
                self.assertEqual(len(self.paseo.calls("send")), 2)
                self.assertEqual(mon.statuses(), [(1, "queued")])

    def test_after_stop_queued_is_final(self):
        # A Stop holds the queued message until the person's next prompt
        # (delivery.ts): the receipt is still "queued" — no repeat, no
        # fallback (Zavod stands behind it).
        self.fresh()
        mon = self.mon(**self.world.zavod_env("codex", root_pid=self.agent.pid))
        self.assertEqual(mon.doc["fallbacks"], ["zavod"])
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        self.assertEqual(len(self.paseo.calls("send")), 1)
        # The preflight probed Zavod (version); nothing was sent there.
        self.assertEqual([c for c in self.world.calls("zavod") if "send" in c["argv"]], [])

    def test_the_probe_is_the_inspection_of_the_agent(self):
        self.fresh()
        self.paseo.respond(inspect=[error("AGENT_NOT_FOUND")])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertIn("paseo: probe argv: unavailable", line["reason"])
        self.assertEqual(self.paseo.calls("send"), [])

    def test_the_cli_outside_path(self):
        self.fresh(outside_path=True)
        mon = self.mon()
        self.assertEqual(mon.doc["programs"], {"paseo": self.paseo.path})
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        self.assertEqual(len(self.paseo.calls("send")), 1)

    def test_the_warning_names_the_idle_unload(self):
        self.fresh()
        mon = self.mon()
        self.assertIn("idle for an hour", mon.line["instruction"])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class AgentUnderPaseoTest(unittest.TestCase):
    """Row #96 (SC-002): under Paseo its channel goes first, before codex
    queue and Zavod, except Claude's own Monitor; the same agents in a
    terminal keep their own methods; the ACP terminal of Paseo is its
    session, an agent nested in it is not; an ACP agent's own method is
    skipped under Paseo (synthetic record)."""

    def setUp(self):
        self.world = eo.new_world(self)
        self.paseo = FakePaseo(self.world.bin)
        self.agent = ft.Sleeper(self)
        self.n = 0

    def line(self, client, procs, **extra):
        self.n += 1
        return eo.monitor(self.world, "tok-%d" % self.n, client, procs, **extra).line

    def test_codex_tui_against_codex_in_paseo(self):
        env = dict(PASEO_AGENT_ID=AGENT_ID, CODEX_THREAD_ID=THREAD)
        line = self.line("codex", in_terminal("codex"), **env)
        self.assertEqual(line["channel"], "codex-queue")
        line = self.line("codex", launched("codex", self.agent.pid), **env)
        self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"))
        self.assertIn("delivery: not used under host paseo", line["reason"])

    def test_claude_in_paseo_keeps_its_monitor(self):
        line = self.line("claude", launched("claude", self.agent.pid), PASEO_AGENT_ID=AGENT_ID)
        self.assertEqual(line["method"], "stream")

    def test_paseo_before_zavod(self):
        line = self.line("codex", launched("codex", self.agent.pid), PASEO_AGENT_ID=AGENT_ID,
                         **self.world.zavod_env("codex", root_pid=self.agent.pid))
        self.assertEqual(line["channel"], "paseo")
        self.assertEqual(RUNNER.load_preflight(self.world.env("tok-%d" % self.n, "codex")["SKEIN_TOKEN_FILE"])
                         ["fallbacks"], ["zavod"])

    def test_kimi_tui_against_kimi_in_paseo(self):
        line = self.line("kimi", in_terminal("kimi-code"), PASEO_AGENT_ID=AGENT_ID)
        self.assertEqual(line["method"], "exit-on-event")
        line = self.line("kimi", launched("kimi-code", self.agent.pid), PASEO_AGENT_ID=AGENT_ID)
        self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"))

    def test_the_acp_terminal_and_an_agent_nested_in_it(self):
        line = self.line("gemini", [("bash", None, None), DAEMON], PASEO_AGENT_ID=AGENT_ID)
        self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"))
        nested = [("bash", None, None), ("codex", ["codex"], self.agent.pid), ("bash", None, None), DAEMON]
        line = self.line("codex", nested, PASEO_AGENT_ID=AGENT_ID)
        self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_acp_terminal_keeps_only_what_the_launcher_names(self):
        # The ACP terminal of Paseo (runner <- shell <- the daemon, no agent
        # process between): the addressee is the Paseo session and the
        # record keeps only the launcher's keeps, as an agent Paseo
        # launched. Kimi keeps nothing (its exit-on-event is skipped, it
        # gets paseo); Claude keeps its stream.
        terminal = [("bash", None, None), DAEMON]
        line = self.line("kimi", terminal, PASEO_AGENT_ID=AGENT_ID)
        self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"))
        self.assertIn("exit-on-event: not used under host paseo", line["reason"])
        line = self.line("claude", terminal, PASEO_AGENT_ID=AGENT_ID)
        self.assertEqual(line["method"], "stream")

    def test_the_own_method_of_an_acp_agent_is_skipped(self):
        doc = copy.deepcopy(REG.doc)
        doc["agents"].append({"id": "acp-agent", "about": "synthetic ACP agent", "source": "test",
                              "process": {"executables": ["acp-agent"]},
                              "methods": [{"kind": "exit-on-event", "conditions": {}, "instruction": "x"}]})
        runner = harness.load_runner(registry=base64.b64encode(json.dumps(doc).encode("utf-8")).decode("ascii"))
        reg = runner.load_registry()

        def choose(procs):
            source = eo.chain(("python3", ft.RUNNER_PROC_ARGV, None), *procs)
            record = runner.resolve_record(reg, "acp-agent")
            pl = runner.place(runner.ancestry(source, ft.RUNNER_PID), reg, record, self_pid=ft.RUNNER_PID)
            env = dict(self.world.base_env, PASEO_AGENT_ID=AGENT_ID)
            detector = runner.RegistryDetector()
            ctx = runner.Context(reg, runner.Settings(record=record, platform="posix"), env, pl, detector,
                                 runner.SystemProbeRunner())
            ctx.identity = runner.identity(pl, reg, env, detector.address_specs(ctx), self_pid=ft.RUNNER_PID)
            return runner.choose(ctx)

        launched_choice = choose(launched("acp-agent", self.agent.pid))
        self.assertEqual(launched_choice.channel, "paseo")
        self.assertIn("exit-on-event: not used under host paseo", launched_choice.reasons)
        self.assertEqual(choose(in_terminal("acp-agent")).method, "exit-on-event")


if __name__ == "__main__":
    unittest.main()
