#!/usr/bin/env python3
"""SC-001 of the two cmux channels (matrix row #101): an agent message for
Claude, a paste for every other record, over the registry the server
embeds.

The expected values come from the cmux source at 079d294b (0.65.2), not
from the records:
- docs/agent-messages.md: "cmux delivers it through that agent's own hooks,
  never by typing into its terminal"; a message starts queued; "A refused
  send stores nothing";
- CLI/CMUXCLI+AgentMessages.swift#parseAgentMessageSendArguments and
  #runAgentMessageSend: after -- every word is the body, the body - is read
  from stdin; under the global --json the stored message is printed, its
  state queued (Sources/AgentMessageCenter.swift#payload);
  Sources/TerminalController+AgentMessageSettings.swift: "Agent messages are
  turned off (agentMessages.enabled is false).", "Recipient %@ has messages
  disabled.", "Recipient workspace %@ has messages disabled.";
- CLI/CMUXCLI+Paste.swift: - reads the text from stdin; --submit presses the
  submit key; without --force #ensureAgentPromptIsFree refuses first with
  "...is waiting on a question or dialog, so nothing was sent." or "...holds
  text someone is typing, so nothing was sent."; when the submit key was not
  sent the text is already at the prompt: submitted false and a warning;
- CLI/cmux.swift: an error is "Error: <message>" on stderr with exit 1; the
  socket at CMUX_SOCKET_PATH missing is "Socket not found at <path>", before
  any request;
- only macOS (the platform of the record is put in place here).

Run on its own: python3 internal/handler/assets/runner_tests/entries_cmux_test.py
Standard library of Python 3.6 only.
"""

import os
import socket
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_addressed_hosts_test as ah  # noqa: E402
import entries_orca_test as eo  # noqa: E402
import failures_test as ft  # noqa: E402

RUNNER = ft.RUNNER
out = ah.out
SURFACE = "surface:12"
MESSAGE = "--json agent message"
PASTE = "--json paste"


def stored(state="queued"):
    return out(stdout={"id": "3f2a", "thread_id": "3f2a", "sender_name": "skein-monitor", "recipient_surface_id":
                       "S-12", "state": state, "recipient_surface_ref": SURFACE, "recipient_has_agent": True})


def pasted(submitted=True):
    answer = out(stdout={"surface_id": "S-12", "submitted": submitted})
    if not submitted:
        answer["stderr"] = "warning: text was pasted but the submit key was not sent (unknown)\n"
    return answer


def error(text):
    return out(1, stderr="Error: %s\n" % text)


def agent(name):
    return [("bash", None, None), (name, [name], os.getpid()), ("bash", None, None)]


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class CmuxCase(ah.HostCase):
    PROGRAM = "cmux"

    def fresh(self):
        ah.HostCase.fresh(self)
        self.socket_path = os.path.join(self.world.root, "cmux.sock")
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(self.socket_path)
        self.listener.listen(8)
        self.addCleanup(self.listener.close)
        patcher = mock.patch.object(RUNNER, "_platform_key", lambda: "macos")
        patcher.start()
        self.addCleanup(patcher.stop)

    def answers(self):
        self.cli.respond(**{MESSAGE: [stored()], PASTE: [pasted()]})

    def mon(self, name="tok", client="codex", **change):
        env = {"CMUX_SURFACE_ID": SURFACE, "CMUX_SOCKET_PATH": self.socket_path}
        env.update(change)
        return eo.monitor(self.world, name, client, agent(client), **env)

    def cases(self, prefix, client, cases, **env):
        for i, (name, answer, outcome) in enumerate(cases):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon(client=client, **env)
                self.cli.respond(**{prefix: [answer]})
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                self.assertEqual(len(self.cli.calls(prefix)), 1)

    def repeated(self, prefix, client, refusals, accepted, outcome, **env):
        for name, answer in refusals:
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon(client=client, **env)
                self.cli.respond(**{prefix: [answer, accepted]})
                mon.page([ft.note(1)])
                sends = self.cli.calls(prefix)
                self.assertEqual(len(sends), 2)
                self.assertEqual(sends[0]["argv"], sends[1]["argv"])
                self.assertEqual(mon.statuses(), [(1, outcome)])


class CmuxMessageTest(CmuxCase):
    """Row #101, the agent message (Claude): the text on stdin to the
    surface, every sign, the refusals of messages turned off repeated."""

    FORCED = {"SKEIN_MONITOR_CHANNEL": "cmux-message"}

    def test_claude_gets_the_message_on_stdin(self):
        self.fresh()
        mon = self.mon(client="claude", **self.FORCED)
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "cmux-message"))
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        [send] = self.cli.calls(MESSAGE)
        self.assertEqual(send["argv"], ["--json", "agent", "message", SURFACE, "--from", "skein-monitor", "--", "-"])
        self.assertIn('notification="1"', send["stdin"])
        self.assertEqual(self.cli.calls(PASTE), [])

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        self.cases(MESSAGE, "claude", [
            ("queued", stored(), "queued"),
            ("a state of another version", stored("delivered"), "unknown"),
            ("no JSON", out(stdout="Queued message 3f2a for alpha (surface:12).\n"), "unknown"),
            ("an unfamiliar error", error("the app answered nothing"), "unknown"),
        ], **self.FORCED)

    def test_a_refusal_before_send_is_repeated(self):
        self.repeated(MESSAGE, "claude", [
            ("off everywhere", error("Agent messages are turned off (agentMessages.enabled is false).")),
            ("off for the agent", error("Recipient alpha has messages disabled.")),
            ("off for the workspace", error("Recipient workspace alpha has messages disabled.")),
            ("no socket", error("Socket not found at /tmp/cmux.sock")),
        ], stored(), "queued", **self.FORCED)

    def test_only_claude_gets_the_message(self):
        self.fresh()
        line = self.mon(client="codex", **self.FORCED).line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.cli.calls(MESSAGE), [])


class CmuxPasteTest(CmuxCase):
    """Row #101, the paste (every other record): recognition, the text on
    stdin, never --force, every sign, the draft and the dialog refused
    before the paste and repeated."""

    def test_recognised_by_the_surface_and_the_socket(self):
        # Codex without its own thread and an agent of no record: both reach
        # step 6 (Kimi and Grok keep their own methods first).
        for client in ("codex", "aider"):
            with self.subTest(client=client):
                self.fresh()
                mon = self.mon(client=client)
                self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "cmux-paste"))
                # One environment, one channel for the record: no ambiguity.
                self.assertNotIn("cmux-message", mon.doc["fallbacks"])
                self.assertNotIn("ambiguous_terminal", str(mon.line["warnings"]))
        self.fresh()
        for name, change in [("no surface", {"CMUX_SURFACE_ID": ""}), ("no socket", {"CMUX_SOCKET_PATH": ""})]:
            with self.subTest(case=name):
                line = self.mon("tok-" + name.replace(" ", "-"), **change).line
                self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_text_goes_on_stdin_without_force(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "input-written")])
        [send] = self.cli.calls(PASTE)
        self.assertEqual(send["argv"], ["--json", "paste", "--surface", SURFACE, "--submit", "-"])
        self.assertNotIn("--force", send["argv"])
        self.assertIn('notification="1"', send["stdin"])

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        self.cases(PASTE, "codex", [
            ("pasted and submitted", pasted(), "input-written"),
            # The text is at the prompt without its Enter.
            ("pasted, the submit key not sent", pasted(False), "unknown"),
            ("no JSON", out(stdout="OK\n"), "unknown"),
            ("an error after the paste could begin", error("the socket closed"), "unknown"),
            ("the phrase on stdout", out(1, stdout="so nothing was sent.\n"), "unknown"),
        ])

    def test_a_draft_or_a_dialog_is_refused_before_the_paste(self):
        self.repeated(PASTE, "codex", [
            ("a draft", error("paste: the agent prompt in S-12 holds text someone is typing, so nothing was sent. "
                              "Retry when the prompt is clear, or pass --force to send anyway.")),
            ("a dialog", error("paste: S-12 is waiting on a question or dialog, so nothing was sent. Retry once it "
                               "is answered, or pass --force to send anyway.")),
            ("no socket", error("Socket not found at /tmp/cmux.sock")),
        ], pasted(), "input-written")

    def test_claude_does_not_get_the_paste(self):
        self.fresh()
        line = self.mon(client="claude", SKEIN_MONITOR_CHANNEL="cmux-paste").line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))

    def test_the_probe_is_the_socket(self):
        self.fresh()
        self.listener.close()
        os.remove(self.socket_path)
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.cli.calls(PASTE), [])

    def test_only_macos(self):
        self.fresh()
        with mock.patch.object(RUNNER, "_platform_key", lambda: "linux"):
            line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))


if __name__ == "__main__":
    unittest.main()
