#!/usr/bin/env python3
"""SC-001 of the Agent Orchestrator, Herdr and Paradev records (matrix row
#100), over the registry the server embeds.

The expected values come from the sources, not from the records:
- Agent Orchestrator 80c941f6 (0.10.0): backend/internal/cli/send.go#
  sendMessage (without --steer: POST sessions/<id>/send); backend/internal/
  session_manager/manager.go#sendWithConfirmationStop (a pane gets paste and
  Enter and the call returns once the agent is active, but a session in an
  interface transition gets the message queued and a Chat session gets it
  relayed, both with the same exit 0: so exit 0 proves it queued only);
  backend/cmd/ao/main.go and internal/cli/root.go#ExitCode (an error is a
  text line on stderr; exit 2 for a usage error, before the daemon is
  called); internal/cli/client.go#apiError.String ("<message> (<code>)
  [request <id>]"); internal/service/session/service.go (SESSION_COMPOSER_BUSY,
  SESSION_AWAITING_DECISION and SESSION_STARTUP_PENDING are refusals of the
  guard before the paste; SESSION_DELIVERY_UNCONFIRMED comes after it);
- Herdr e8d7d970 (0.9.5): src/cli/agent/bindings.rs (agent prompt: "If the
  agent is already blocked, submission is rejected with agent_blocked before
  any input is sent"; after an accepted submission --wait needs an observed
  working or blocked state, else agent_prompt_stalled; a caller timeout
  returns timeout); src/app/api/agents.rs#validate_agent_prompt
  (agent_not_found, agent_not_ready before the write); src/api/wait.rs
  (agent_not_running after the write); src/cli.rs#print_response (an error
  is {id, error: {code, message}} on stderr, exit 1); src/main.rs#finish_cli
  (server_not_running when no server listens); src/pane.rs (HERDR_ENV=1 and
  HERDR_PANE_ID in every pane);
- Paradev d3feccef (2.0.11): packages/cli/src/commands/terminals/send/
  command.ts (--wait first needs the agent's hook binding; then the wait for
  --until; no activity within 10 s, an exited agent or a timeout is an error
  after the send; success {terminalId, submitted, state});
  packages/cli/src/commands/agents/wait/command.ts (agents wait --until
  idle); packages/cli-framework/src/runner.ts#handleError (every error is
  "Error: <message>" text on stderr with exit 1).

Run on its own: python3 internal/handler/assets/runner_tests/entries_terminal_hosts_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_addressed_hosts_test as ah  # noqa: E402
import failures_test as ft  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG

out = ah.out
AO_SESSION = "ao-17"
PANE = "p-3"
WORKSPACE = "ws-9f2"
TERMINAL = "term-41c"


def ao_error(message, code):
    return out(1, stderr="%s (%s) [request r-1]\n" % (message, code))


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class AgentOrchestratorRecordTest(ah.HostCase):
    """Row #100, Agent Orchestrator: recognition, ao send without --steer,
    every sign, the refusals of the guard repeated, the probe."""

    PROGRAM = "ao"
    ENV = {"AO_SESSION_ID": AO_SESSION}

    def answers(self):
        self.cli.respond(send=[out()], session_get=[out(stdout={"session": {"id": AO_SESSION, "isTerminated": False}})])

    def test_recognised_by_the_session_id(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "agent-orchestrator"))
        line = self.mon("tok-none", AO_SESSION_ID="").line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_text_is_the_message_argument_without_steering(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        [send] = self.cli.calls("send")
        argv = send["argv"]
        self.assertEqual(argv[:4], ["send", "--session", AO_SESSION, "--message"])
        self.assertEqual(len(argv), 5)
        self.assertIn('notification="1"', argv[4])
        self.assertNotIn("--steer", argv)
        self.assertNotIn("--client-message-id", argv)

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        self.outcome_cases("send", [
            # A confirmed pane turn, a queued and a relayed message alike.
            ("exit 0", out(), "queued"),
            ("written, the turn not confirmed", ao_error("Message was written to the terminal but the agent did "
                                                         "not start a turn; prompt may be unsubmitted",
                                                         "SESSION_DELIVERY_UNCONFIRMED"), "unknown"),
            ("the daemon unavailable", out(1, stderr="the Orchestrator daemon is unavailable\n"), "unknown"),
            ("a failure of another code", ao_error("Session not found", "SESSION_NOT_FOUND"), "unknown"),
            ("the refusal code on stdout", out(1, stdout="(SESSION_COMPOSER_BUSY)\n"), "unknown"),
            ("an exit code of another version", out(3), "unknown"),
        ])

    def test_a_refusal_of_the_guard_is_repeated(self):
        self.refusal_cases("send", [
            ("a human draft", ao_error("Session composer has an unsent draft; retry after the draft is submitted "
                                       "or cleared", "SESSION_COMPOSER_BUSY")),
            ("a permission dialog", ao_error("Session is paused on a permission decision; answer it in the "
                                             "session terminal first", "SESSION_AWAITING_DECISION")),
            ("the agent still starting", ao_error("Session agent is still starting; retry after the agent prompt "
                                                  "is ready", "SESSION_STARTUP_PENDING")),
            ("a usage error", out(2, stderr="usage: --message is required\n")),
        ], out(), "queued")

    def test_the_check_before_each_send_is_the_session_get(self):
        # session get must name the very session the send addresses; a
        # terminated session is a refusal before send ("Addressee and
        # check", item 4; correction of K5), not the end of the monitor.
        self.fresh()
        mon = self.mon()
        self.cli.respond(session_get=[out(stdout={"session": {"id": "ao-other", "isTerminated": False}})])
        mon.page([ft.note(1)])
        # Another session is another addressee; a terminated one is a
        # refusal before send.
        self.assertEqual(self.cli.calls("send"), [])
        self.assertEqual(mon.statuses(), [(1, "unknown")])
        self.assertEqual(len([l for l in mon.lines() if l.get("cause") == "addressee_changed"]), 1)
        self.fresh()
        mon = self.mon()
        self.cli.respond(session_get=[out(stdout={"session": {"id": AO_SESSION, "isTerminated": True}})])
        mon.page([ft.note(1)], cursor={"ts": "t", "id": 2})
        self.assertEqual(self.cli.calls("send"), [])
        self.assertEqual(mon.statuses(), [(1, "refused-before-send")])

    def test_the_probe_is_the_session(self):
        self.fresh()
        self.cli.respond(session_get=[out(1, stderr="Session not found (SESSION_NOT_FOUND)\n")])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.cli.calls("send"), [])
        self.assertTrue(all(c["argv"] == ["session", "get", AO_SESSION, "--json"]
                            for c in self.cli.calls("session get")))


def herdr_agent(status="working"):
    return out(stdout={"id": "cli:agent:prompt", "result": {"type": "agent_info", "agent": {
        "terminal_id": "t-1", "agent": "codex", "agent_status": status}}})


def herdr_error(code, rid="cli:agent:prompt"):
    return out(1, stderr={"id": rid, "error": {"code": code, "message": "failure"}})


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class HerdrRecordTest(ah.HostCase):
    """Row #100, Herdr: recognition, the idle wait then the prompt that waits
    for working, every sign, a wait after the send that runs out is unknown,
    the refusals before the write repeated, the probe."""

    PROGRAM = "herdr"
    ENV = {"HERDR_ENV": "1", "HERDR_PANE_ID": PANE}

    def answers(self):
        self.cli.respond(agent_wait=[out(stdout={"id": "cli:agent:wait", "result": {}})],
                         agent_prompt=[herdr_agent()], agent_get=[herdr_agent("idle")])

    def test_recognised_by_the_pane_of_herdr(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "herdr"))
        for name, change in [("no pane", {"HERDR_PANE_ID": ""}), ("not Herdr's value", {"HERDR_ENV": "0"})]:
            with self.subTest(case=name):
                line = self.mon("tok-" + name.replace(" ", "-").replace("'", ""), **change).line
                self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_idle_wait_then_the_prompt(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "turn-started")])
        calls = [c["argv"] for c in self.cli.calls() if c["argv"][1] in ("wait", "prompt")]
        self.assertEqual(calls[0], ["agent", "wait", PANE, "--until", "idle", "--timeout", "60000"])
        prompt = calls[-1]
        self.assertEqual(prompt[:9], ["agent", "prompt", PANE, "--wait", "--until", "working", "--timeout", "30000",
                                      "--"])
        self.assertEqual(len(prompt), 10)
        self.assertIn('notification="1"', prompt[9])

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        self.outcome_cases("agent prompt", [
            ("working", herdr_agent(), "turn-started"),
            # After the write: waits that run out are unknown, not refusals.
            ("stalled", herdr_error("agent_prompt_stalled"), "unknown"),
            ("timeout", herdr_error("timeout"), "unknown"),
            ("the agent changed", herdr_error("agent_not_running"), "unknown"),
            ("an unfamiliar code", herdr_error("internal_error"), "unknown"),
            ("the refusal code on stdout", out(1, stdout={"error": {"code": "agent_blocked"}}), "unknown"),
            ("no JSON", out(1, stderr="error: unexpected argument\n"), "unknown"),
        ])

    def test_a_refusal_before_the_write_is_repeated(self):
        self.refusal_cases("agent prompt", [
            ("blocked", herdr_error("agent_blocked")),
            ("not ready", herdr_error("agent_not_ready")),
            ("not found", herdr_error("agent_not_found")),
            ("no server", herdr_error("server_not_running")),
        ], herdr_agent(), "turn-started")

    def test_an_agent_not_idle_is_not_sent_to(self):
        self.fresh()
        mon = self.mon()
        self.cli.respond(agent_wait=[herdr_error("timeout", "cli:agent:wait"), out(stdout={"result": {}})])
        mon.page([ft.note(1)])
        self.assertEqual(len(self.cli.calls("agent wait")), 2)
        self.assertEqual(len(self.cli.calls("agent prompt")), 1)
        self.assertEqual(mon.statuses(), [(1, "turn-started")])

    def test_the_probe_is_the_agent_of_the_pane(self):
        self.fresh()
        self.cli.respond(agent_get=[herdr_error("agent_not_found", "cli:agent:get")])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.cli.calls("agent prompt"), [])


def paradev_sent(state="working"):
    return out(stdout={"terminalId": TERMINAL, "submitted": True, "state": state})


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class ParadevRecordTest(ah.HostCase):
    """Row #100, Paradev: recognition, the idle wait then the send that waits
    for working, every sign, the agent without hooks."""

    PROGRAM = "paradev"
    ENV = {"SUPERSET_TERMINAL_ID": TERMINAL, "SUPERSET_WORKSPACE_ID": WORKSPACE}

    def answers(self):
        self.cli.respond(agents_wait=[out(stdout={"terminalId": TERMINAL, "state": "idle"})],
                         terminals_send=[paradev_sent()])

    def test_recognised_by_the_terminal_and_the_workspace(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "paradev"))
        for name in ("SUPERSET_TERMINAL_ID", "SUPERSET_WORKSPACE_ID"):
            with self.subTest(missing=name):
                line = self.mon("tok-" + name.lower(), **{name: ""}).line
                self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_paradev_names_fill_in_when_the_superset_ones_are_unset(self):
        # notify-hook.template.sh: "SUPERSET_* wins so existing overrides
        # keep working; PARADEV_* fills in when it is unset." The runner reads
        # the agent's environment as agent_env gives it.
        world_env = ft.World.env
        for name, variables, terminal in (
                ("only the new names", {"SUPERSET_TERMINAL_ID": "", "SUPERSET_WORKSPACE_ID": "",
                                        "PARADEV_TERMINAL_ID": "term-new", "PARADEV_WORKSPACE_ID": "ws-new"}, "term-new"),
                ("both: the old names win", {"PARADEV_TERMINAL_ID": "term-new", "PARADEV_WORKSPACE_ID": "ws-new"},
                 TERMINAL)):
            with self.subTest(case=name):
                self.fresh()
                with mock.patch.object(self.world, "env",
                                       lambda n, c, **e: RUNNER.agent_env(world_env(self.world, n, c, **e), REG)):
                    mon = self.mon(**variables)
                self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "paradev"))
                mon.page([ft.note(1)])
                [send] = self.cli.calls("terminals send")
                self.assertEqual(send["argv"][6], terminal)

    def test_the_idle_wait_then_the_send(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "turn-started")])
        [wait] = self.cli.calls("agents wait")
        self.assertEqual(wait["argv"], ["agents", "wait", "--local", "--workspace", WORKSPACE, "--terminal", TERMINAL,
                                        "--until", "idle", "--timeout", "60", "--json"])
        [send] = self.cli.calls("terminals send")
        argv = send["argv"]
        self.assertEqual(argv[:8], ["terminals", "send", "--local", "--workspace", WORKSPACE, "--terminal",
                                    TERMINAL, "--text"])
        self.assertIn('notification="1"', argv[8])
        self.assertEqual(argv[9:], ["--wait", "--until", "working", "--timeout", "30", "--json"])

    def test_every_sign_gives_one_outcome(self):
        self.outcome_cases("terminals send", [
            ("working", paradev_sent(), "turn-started"),
            ("another state", paradev_sent("idle"), "unknown"),
            # After the send: no activity, a timeout, an exit are unknown.
            ("no activity", out(1, stderr="Error: The agent showed no activity within 10s of the send; it is idle\n"
                                          "Hint: The text may be sitting in its input box\n"), "unknown"),
            ("a timeout", out(1, stderr="Error: Timed out after 30s; agent is idle\n"), "unknown"),
            ("the agent exited", out(1, stderr="Error: The agent in terminal t is no longer running\n"), "unknown"),
            ("no JSON", out(stdout="Sent to terminal t; agent is working\n"), "unknown"),
        ])

    def test_an_agent_without_hooks_gets_nothing(self):
        self.fresh()
        mon = self.mon()
        self.cli.respond(agents_wait=[out(1, stderr="Error: No agent is running in terminal %s\n" % TERMINAL)])
        mon.page([ft.note(1)])
        self.assertEqual(self.cli.calls("terminals send"), [])
        self.assertEqual(mon.statuses(), [(1, "refused-before-send")])


class _Captured(BaseException):
    """Carries the environment an entry handed on, past its own handlers."""

    def __init__(self, env):
        BaseException.__init__(self)
        self.env = env


class AliasEntriesTest(unittest.TestCase):
    """Warnings 9 and 17 of the wave 12 review, round 2: each entry of the
    runner - the preflight, the detached child and the client run - reads
    the agent's environment with the registry's aliases, so a Paradev that
    sets only PARADEV_* is seen under the SUPERSET_* names; with both set,
    SUPERSET_* is kept (notify-hook.template.sh: "SUPERSET_* wins")."""

    BASE = {"SKEIN_TOKEN_FILE": "/nonexistent/token", "SKEIN_PUBLIC_URL": "https://skein.example",
            "SKEIN_ISSUE": "sk-9", "PATH": os.defpath}

    def entry_env(self, entry, step, variables):
        def capture(*args, **kw):
            env = [a for a in args if isinstance(a, dict) and "SKEIN_PUBLIC_URL" in a]
            raise _Captured(env[0])
        environ = dict(self.BASE, **variables)
        with mock.patch.dict(os.environ, environ, clear=True), mock.patch.object(RUNNER, step, capture):
            with self.assertRaises(_Captured) as caught:
                getattr(RUNNER, entry)()
        return caught.exception.env

    def test_each_entry_reads_the_aliases(self):
        for entry, step in (("run_preflight", "preflight"), ("run_detached_child", "read_settings"),
                            ("run_client", "read_settings")):
            for name, variables, terminal, workspace in (
                    ("only the new names", {"PARADEV_TERMINAL_ID": "term-new", "PARADEV_WORKSPACE_ID": "ws-new"},
                     "term-new", "ws-new"),
                    ("both: the old names win", {"PARADEV_TERMINAL_ID": "term-new", "PARADEV_WORKSPACE_ID": "ws-new",
                                                 "SUPERSET_TERMINAL_ID": "term-old",
                                                 "SUPERSET_WORKSPACE_ID": "ws-old"}, "term-old", "ws-old"),
                    ("an empty old name takes the new one", {"PARADEV_TERMINAL_ID": "term-new",
                                                             "SUPERSET_TERMINAL_ID": ""}, "term-new", None)):
                with self.subTest(entry=entry, case=name):
                    env = self.entry_env(entry, step, variables)
                    self.assertEqual(env.get("SUPERSET_TERMINAL_ID"), terminal)
                    self.assertEqual(env.get("SUPERSET_WORKSPACE_ID"), workspace)


if __name__ == "__main__":
    unittest.main()
