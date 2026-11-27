#!/usr/bin/env python3
"""SC-001 of the Agent Deck, T3 Code and Sculptor records and SC-002 "an
addressed channel that does not tell an incarnation apart" (matrix row
#99), over the registry the server embeds.

The expected values come from the sources, not from the records:
- Agent Deck fec4fcbc (1.16.40), cmd/agent-deck/session_send_cmd.go#
  handleSessionSend: --message-file - reads stdin; --json without --wait,
  --stream, --draft, --defer-if-busy and --no-wait goes through the send
  queue (the synchronous --defer-if-busy path types into the pane itself
  and holds the call up to its defer timeout, so it is not used);
  --no-tag drops the [agent-deck from:<id>] envelope; an unknown session
  exits 2. session_send_queue_cmd.go#queuedSendFields: exit 0 with
  {success: true, delivery: "queued", submitted: false}; a send that could
  not be queued or failed at once exits 1 with success false and nothing
  typed; a muted trigger exits 0 with {success: true, muted: true} and no
  delivery. cli_utils.go#ErrorWithData: an error under --json is
  {success: false, error, code} on stdout. session_output_cmd.go#
  handleSessionCurrent: the session of the calling tmux session with its id;
- T3 Code 45a3880c (0.1.12), apps/server/src/cli/thread.ts#threadSendCommand:
  the message - is read from stdin, --json prints the t3_thread_send result
  whose delivery is started, queued, steered or restarted
  (packages/contracts/src/orchestratorMcp.ts); every failure is a text line
  on stderr (#reportThreadErrors);
- Sculptor cdd6dc68 (0.51.0.dev0), tools/sculpt/sculpt/commands/agent.py#send:
  exit 0 with {sent: true, agent_id, message} on --json; an answer of 400 or
  more becomes {error: detail, detail: ""} on stderr with exit 1
  (formatting.py#cli_error); a connection that failed gives detail
  CONNECTION_HINT; sculptor/web/app.py: 409 "agent is busy or not at its
  prompt", "this agent does not accept automated prompts", "terminal not
  running" and the AskUserQuestion refusal come before anything is queued
  or typed (terminal_input.py#deliver_prompt_to_terminal_agent).

Run on its own: python3 internal/handler/assets/runner_tests/entries_addressed_hosts_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import stat
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_orca_test as eo  # noqa: E402
import failures_test as ft  # noqa: E402

DECK_ID = "deck-5f1c2a"
THREAD = "thread-8c41e2"
SCULPT_ID = "agent-3b9d7e"
CONNECTION_HINT = ("Is the Sculptor app running? If it serves a non-default port, set SCULPT_API_PORT or pass "
                   "--base-url.")

_CLI = r'''#!%(python)s
import json, os, sys
HERE = %(here)r
NAME = %(name)r
args = sys.argv[1:]
data = ""
try:
    if not sys.stdin.isatty():
        data = sys.stdin.read()
except (OSError, ValueError):
    pass
with open(os.path.join(HERE, NAME + ".calls.jsonl"), "a") as fh:
    fh.write(json.dumps({"argv": args, "stdin": data}) + "\n")
with open(os.path.join(HERE, NAME + ".answers.json")) as fh:
    table = json.load(fh)
line = " ".join(args)
keys = [k for k in table if line == k or line.startswith(k + " ")]
key = max(keys, key=len) if keys else ""
answers = table.get(key) or [{"exit": 0}]
count = os.path.join(HERE, NAME + "." + key.replace(" ", "_") + ".count")
n = int(open(count).read()) if os.path.exists(count) else 0
open(count, "w").write(str(n + 1))
a = answers[min(n, len(answers) - 1)]
for stream, key in ((sys.stdout, "stdout"), (sys.stderr, "stderr")):
    out = a.get(key, "")
    stream.write(out if isinstance(out, str) else json.dumps(out, indent=2))
sys.exit(a.get("exit", 0))
'''


class FakeCli(object):
    """A fake CLI at directory/name: it answers by the longest subcommand
    prefix of its arguments ("session send"), in turn, and records argv and
    stdin of every call."""

    def __init__(self, directory, name):
        self.dir = directory
        self.name = name
        self.path = os.path.join(directory, name)
        with open(self.path, "w") as fh:
            fh.write(_CLI % {"python": sys.executable, "here": directory, "name": name})
        os.chmod(self.path, stat.S_IRWXU)
        self.table = {}
        self.respond()

    def respond(self, **answers):
        """answers: subcommand prefix (spaces as "_") -> list of answers;
        the rest keep what was set before."""
        for key, value in answers.items():
            self.table[key.replace("_", " ")] = value
        with open(os.path.join(self.dir, self.name + ".answers.json"), "w") as fh:
            json.dump(self.table, fh)
        for entry in os.listdir(self.dir):
            if entry.startswith(self.name + ".") and entry.endswith(".count"):
                os.remove(os.path.join(self.dir, entry))

    def calls(self, prefix=None):
        try:
            with open(os.path.join(self.dir, self.name + ".calls.jsonl")) as fh:
                calls = [json.loads(l) for l in fh if l.strip()]
        except OSError:
            return []
        return [c for c in calls if prefix is None or " ".join(c["argv"]).startswith(prefix)]


def out(exit_code=0, stdout="", stderr=""):
    return {"exit": exit_code, "stdout": stdout, "stderr": stderr}


class HostCase(unittest.TestCase):
    """A world of its own (its own ledger) with the host's fake CLI, and a
    Codex agent (this process) without its own thread."""

    PROGRAM = None
    ENV = {}

    def fresh(self):
        self.world = eo.new_world(self)
        self.cli = FakeCli(self.world.bin, self.PROGRAM)
        self.answers()

    def answers(self):
        pass

    def mon(self, name="tok", **change):
        env = dict(self.ENV)
        env.update(change)
        return eo.monitor(self.world, name, "codex", eo.codex_agent(), **env)

    def outcome_cases(self, prefix, cases):
        for i, (name, answer, outcome) in enumerate(cases):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.cli.respond(**{prefix.replace(" ", "_"): [answer]})
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                self.assertEqual(len(self.cli.calls(prefix)), 1)

    def refusal_cases(self, prefix, refusals, accepted, outcome):
        """A refusal before send is called again, then the accepted answer."""
        for name, answer in refusals:
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.cli.respond(**{prefix.replace(" ", "_"): [answer, accepted]})
                mon.page([ft.note(1)])
                sends = self.cli.calls(prefix)
                self.assertEqual(len(sends), 2)
                self.assertEqual(sends[0]["argv"], sends[1]["argv"])
                self.assertEqual(mon.statuses(), [(1, outcome)])


def deck_current(session_id=DECK_ID, tmux="agentdeck_alpha_1"):
    return out(stdout={"session": "alpha", "title": "alpha", "profile": "default", "id": session_id,
                       "path": "/work", "status": "waiting", "tool": "codex", "account": "",
                       "parent_session_id": "", "tmux_session": tmux})


def deck_queued():
    return out(stdout={"send_id": "s-1", "state": "queued", "session_id": DECK_ID, "submitted": False,
                       "success": True, "delivery": "queued", "confirmation": "unknown", "tagged": False})


def deck_error(exit_code, code="INVALID_OPERATION"):
    return out(exit_code, stdout={"success": False, "error": "cannot queue send", "code": code})


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class AgentDeckRecordTest(HostCase):
    """Row #99, Agent Deck: recognition, the queued send with the text on
    stdin, every sign, the check of the session, a recreated session."""

    PROGRAM = "agent-deck"
    ENV = {"AGENTDECK_INSTANCE_ID": DECK_ID}

    def answers(self):
        self.cli.respond(session_send=[deck_queued()], session_current=[deck_current()])

    def test_recognised_by_the_instance_id(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "agent-deck"))
        line = self.mon("tok-none", AGENTDECK_INSTANCE_ID="").line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_text_goes_on_stdin_through_the_queue(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        [send] = self.cli.calls("session send")
        self.assertEqual(send["argv"], ["session", "send", DECK_ID, "--message-file", "-", "--no-tag", "--json"])
        self.assertIn('notification="1"', send["stdin"])
        self.assertNotIn(ft.PREVIEW, " ".join(send["argv"]))

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        self.outcome_cases("session send", [
            ("queued", deck_queued(), "queued"),
            ("a muted trigger", out(stdout={"success": True, "muted": True, "session_id": DECK_ID}), "unknown"),
            ("a queued answer without its delivery", out(stdout={"success": True, "send_id": "s-1"}), "unknown"),
            ("no JSON", out(stdout="Queued s-1 for 'alpha' (unknown)\n"), "unknown"),
            ("exit 1 without JSON: a flag it does not know", out(1, stderr="flag provided but not defined"),
             "unknown"),
            ("an exit code of another version", out(3, stdout=deck_queued()["stdout"]), "unknown"),
        ])

    def test_a_refusal_before_anything_is_queued_is_repeated(self):
        self.refusal_cases("session send", [
            ("failed at once, nothing typed", deck_error(1, "DELIVERY_FAILED")),
            ("not queued", deck_error(1)),
            ("an unknown session", deck_error(2, "NOT_FOUND")),
        ], deck_queued(), "queued")

    def test_the_check_captures_the_session_id(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual(mon.doc["identity"]["captures"]["agent-deck"], [["id", DECK_ID]])
        self.assertTrue(all(c["argv"] == ["session", "current", "--json"] for c in self.cli.calls("session current")))

    def test_the_check_is_the_session_the_send_addresses(self):
        # The send addresses AGENTDECK_INSTANCE_ID; session current must
        # name that very session, at the preflight and before each send.
        self.fresh()
        self.cli.respond(session_current=[deck_current(session_id="deck-other")])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertIn("agent-deck", line["reason"])
        self.fresh()
        mon = self.mon()
        self.cli.respond(session_current=[deck_current(session_id="deck-other")])
        mon.page([ft.note(1)])
        self.assertEqual(self.cli.calls("session send"), [])
        self.assertEqual(mon.statuses(), [(1, "refused-before-send")])

    def test_a_session_recreated_under_the_same_id_gets_the_event(self):
        # SC-002: the channel does not tell the new session apart; it stays
        # in use and the event is delivered.
        self.fresh()
        mon = self.mon()
        self.cli.respond(session_current=[deck_current(tmux="agentdeck_alpha_2")])
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        self.assertEqual(len(self.cli.calls("session send")), 1)

    def test_another_session_is_another_addressee(self):
        self.fresh()
        mon = self.mon()
        self.cli.respond(session_current=[deck_current(session_id="deck-other")])
        mon.page([ft.note(1)])
        self.assertEqual(self.cli.calls("session send"), [])
        self.assertEqual(mon.statuses(), [(1, "unknown")])

    def test_the_probe_is_the_current_session(self):
        self.fresh()
        self.cli.respond(session_current=[out(1, stdout={"success": False, "error": "not in a tmux session",
                                                          "code": "NOT_FOUND"})])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.cli.calls("session send"), [])


def t3_sent(delivery):
    return out(stdout={"threadId": THREAD, "messageId": "m-1", "runId": "r-1", "status": "running",
                       "delivery": delivery})


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class T3CodeRecordTest(HostCase):
    """Row #99, T3 Code: recognition, the text on stdin, every sign, the
    probe, a recreated thread."""

    PROGRAM = "t3"
    ENV = {"T3CODE_THREAD_ID": THREAD}

    def answers(self):
        self.cli.respond(thread_send=[t3_sent("queued")], thread_show=[out(stdout={"thread": {"id": THREAD}})])

    def test_recognised_by_the_thread_id(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "t3code"))
        line = self.mon("tok-none", T3CODE_THREAD_ID="").line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_text_goes_on_stdin(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        [send] = self.cli.calls("thread send")
        self.assertEqual(send["argv"], ["thread", "send", THREAD, "-", "--json"])
        self.assertIn('notification="1"', send["stdin"])
        self.assertNotIn(ft.PREVIEW, " ".join(send["argv"]))

    def test_every_sign_gives_one_outcome(self):
        self.outcome_cases("thread send", [
            ("queued", t3_sent("queued"), "queued"),
            ("started", t3_sent("started"), "turn-started"),
            ("steered", t3_sent("steered"), "turn-started"),
            ("restarted", t3_sent("restarted"), "turn-started"),
            ("a delivery of another version", t3_sent("deferred"), "unknown"),
            ("no JSON", out(stdout="Queued.\n"), "unknown"),
            # A failure is a text line: none proves the message was not sent.
            ("the server not running", out(1, stderr="No running T3 Code server uses /home/u/.t3. Start T3 Code, "
                                                     "then retry.\n"), "unknown"),
            ("the server unreachable", out(1, stderr="Could not reach the T3 Code server at http://x.\n"),
             "unknown"),
        ])

    def test_a_thread_recreated_under_the_same_id_gets_the_event(self):
        self.fresh()
        mon = self.mon()
        self.cli.respond(thread_show=[out(stdout={"thread": {"id": THREAD, "createdAt": "later"}})])
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])

    def test_the_probe_is_the_thread_show(self):
        self.fresh()
        self.cli.respond(thread_show=[out(1, stderr="No running T3 Code server uses /x.\n")])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.cli.calls("thread send"), [])
        self.assertTrue(all(c["argv"] == ["thread", "show", THREAD, "--json"] for c in self.cli.calls("thread show")))


def sculpt_sent():
    return out(stdout={"sent": True, "agent_id": SCULPT_ID, "message": "x"})


def sculpt_error(error, detail=""):
    return out(1, stderr={"error": error, "detail": detail, "code": None})


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class SculptorRecordTest(HostCase):
    """Row #99, Sculptor: recognition, the text as one argument behind --,
    every sign, the 409 refusals repeated, a recreated agent."""

    PROGRAM = "sculpt"
    ENV = {"SCULPT_AGENT_ID": SCULPT_ID}

    def answers(self):
        self.cli.respond(agent_send=[sculpt_sent()], agent_status=[out(stdout={"id": SCULPT_ID})])

    def test_recognised_by_the_agent_id(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "sculptor"))
        line = self.mon("tok-none", SCULPT_AGENT_ID="").line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_text_is_one_argument_behind_the_separator(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        [send] = self.cli.calls("agent send")
        argv = send["argv"]
        self.assertEqual(argv[:5], ["agent", "send", "--json", SCULPT_ID, "--"])
        self.assertEqual(len(argv), 6)
        self.assertIn('notification="1"', argv[5])

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        self.outcome_cases("agent send", [
            ("sent", sculpt_sent(), "queued"),
            ("another 409", sculpt_error("A repository at /x could not be read"), "unknown"),
            ("an answer of 500", sculpt_error("Server returned 500"), "unknown"),
            ("the refusal text on stdout", out(1, stdout={"error": "agent is busy or not at its prompt",
                                                          "detail": ""}), "unknown"),
            ("no JSON", out(stdout="Message sent to agent x.\n"), "unknown"),
        ])

    def test_a_refusal_before_send_is_repeated(self):
        self.refusal_cases("agent send", [
            ("busy or not at its prompt", sculpt_error("agent is busy or not at its prompt")),
            ("no automated prompts", sculpt_error("this agent does not accept automated prompts")),
            ("no terminal", sculpt_error("terminal not running")),
            ("a question open", sculpt_error("Cannot send a message while the agent is waiting for a response to "
                                             "AskUserQuestion.")),
            ("no connection", sculpt_error("Could not connect to Sculptor server at http://localhost:5050",
                                           CONNECTION_HINT)),
        ], sculpt_sent(), "queued")

    def test_an_agent_recreated_under_the_same_id_gets_the_event(self):
        self.fresh()
        mon = self.mon()
        self.cli.respond(agent_status=[out(stdout={"id": SCULPT_ID, "status": "new"})])
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])

    def test_the_probe_is_the_agent_status(self):
        self.fresh()
        self.cli.respond(agent_status=[sculpt_error("Agent not found: x")])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.cli.calls("agent send"), [])


if __name__ == "__main__":
    unittest.main()
