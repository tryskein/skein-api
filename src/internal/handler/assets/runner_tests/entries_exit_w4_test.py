#!/usr/bin/env python3
"""SC-001 of the exit-on-event records of wave 4 (matrix row #113) and the
preflight cases of row #114, over the registry the server embeds.

The expected values come from the sources the records name, not from the
records:
- Gemini CLI (03862c4): run_shell_command is_background; an idle session
  takes a background completion only with experimental.modelSteering true
  and tools.shell.backgroundCompletionBehavior inject (settingsSchema.ts,
  AppContainer.tsx), inject carries up to 5000 characters; ~/.gemini/
  settings.json; npm bin gemini (bundle/gemini.js).
- DeepSeek Harness (4b399c39): bash or pwsh run_in_background, output
  inline up to inlineOutputBytes 2048; npm bin dsh.
- Command Code (docs): monitor_command, checkAfterMs at most 24 h (86400000),
  notify "scheduled", maxDurationMs ends the process (so not given).
- OpenClaude (9b92b89), ZCode (a4b44451), Cursor (agent), Antigravity (agy),
  Copilot (copilot), Droid (droid): the background tool each documents.

Run on its own: python3 internal/handler/assets/runner_tests/entries_exit_w4_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_stream_w1_test as w1  # noqa: E402
import entries_tmux_test as et  # noqa: E402
import fake_channel  # noqa: E402
import modes_stream_test as ms  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402

RUNNER = pm.RUNNER
REG = pm.REG

GEMINI_SETTINGS = {"experimental": {"modelSteering": True}, "tools": {"shell": {"backgroundCompletionBehavior": "inject"}}}
DSH_INLINE_BYTES = 2048
COMMAND_CODE_MAX_CHECK_MS = 86400000
PASEO_ID = "0f8fad5b-d9cb-469f-a165-70867728950e"


def write_gemini_settings(home, doc):
    os.makedirs(os.path.join(home, ".gemini"), exist_ok=True)
    with open(os.path.join(home, ".gemini", "settings.json"), "w") as fh:
        json.dump(doc, fh)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class RecognitionTest(unittest.TestCase):

    def test_the_agent_process_of_each_record(self):
        cases = [("gemini", [("node", ["node", "/usr/bin/gemini"])], True),
                 ("gemini", [("node", ["node", "--max-old-space-size=8192", "/usr/lib/gemini-cli/bundle/gemini.js"])],
                  True),
                 ("gemini", [("geminix", None)], False),
                 ("openclaude", [("node", ["node", "/usr/bin/openclaude"])], True),
                 ("openclaude", [("claude", None)], False),
                 ("zcode", [("node", ["node", "/opt/zcode/dist/zcode.cjs"])], True),
                 ("zcode", [("zcoder", None)], False),
                 ("dsh", [("node", ["node", "/usr/bin/dsh"])], True),
                 ("dsh", [("dshell", None)], False),
                 ("cursor", [("agent", None)], True),
                 ("cursor", [("cursor-agent", None)], True),
                 ("cursor", [("cursor", None)], False),
                 ("antigravity", [("agy", None)], True),
                 ("antigravity", [("antigravity-app", None)], False),
                 ("command-code", [("node", ["node", "/usr/bin/cmdc"])], True),
                 ("command-code", [("cmd-x", None)], False),
                 ("copilot", [("copilot", None)], True),
                 ("copilot", [("copilot-lsp", None)], False),
                 ("droid", [("droid", None)], True),
                 ("droid", [("android", None)], False)]
        for client, chain, found in cases:
            with self.subTest(client=client, chain=chain):
                self.assertEqual(w1.agent_of(client, chain) is not None, found)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class InstructionTest(unittest.TestCase):
    """The method each preflight takes and the tool and parameters it
    names."""

    def line(self, client, chain, settings=None, **env):
        box = pm.Box(self)
        if settings is not None:
            write_gemini_settings(box.root, settings)
        return box.preflight(box.env(client, **env), chain)

    def test_each_record_exits_on_an_event_with_its_tool(self):
        for client, chain, parts in (
                ("openclaude", ["openclaude"], ("Bash tool", "run_in_background: true", "not with Monitor")),
                ("zcode", ["zcode"], ("Bash tool", "run_in_background: true", "TaskOutput", "foreground sleep")),
                ("dsh", ["dsh"], ("bash tool", "pwsh on Windows", "run_in_background: true", "job_output wait:true")),
                ("cursor", ["agent"], ("background task", "Ctrl+B twice")),
                ("antigravity", ["agy"], ("background shell task",)),
                ("command-code", ["command-code"], ("monitor_command", "checkAfterMs %d" % COMMAND_CODE_MAX_CHECK_MS,
                                                    "notify \"scheduled\"", "no maxDurationMs")),
                ("copilot", ["copilot"], ("bash tool", "detached background command", "read_bash")),
                ("droid", ["droid"], ("background Task subagent", "in the foreground"))):
            with self.subTest(client=client):
                line = self.line(client, chain)
                self.assertEqual((line["client"], line["method"], line["channel"]), (client, "exit-on-event", "stdout"))
                for part in parts:
                    self.assertIn(part, line["instruction"])
        self.assertEqual(REG.agents["dsh"]["windowsShell"], "pwsh")

    def test_gemini_needs_both_settings(self):
        line = self.line("gemini", ["gemini"], settings=GEMINI_SETTINGS)
        self.assertEqual(line["method"], "exit-on-event", line)
        self.assertIn("run_shell_command", line["instruction"])
        self.assertIn("is_background: true", line["instruction"])
        for name, doc in (("no steering", {"tools": GEMINI_SETTINGS["tools"]}),
                          ("silent completions", {"experimental": {"modelSteering": True},
                                                  "tools": {"shell": {"backgroundCompletionBehavior": "silent"}}}),
                          ("steering as a string", {"experimental": {"modelSteering": "true"},
                                                    "tools": GEMINI_SETTINGS["tools"]})):
            with self.subTest(case=name):
                self.assertEqual(self.line("gemini", ["gemini"], settings=doc)["method"], "journal")
        self.assertEqual(self.line("gemini", ["gemini"])["method"], "journal")


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX stream output")
class DeepSeekBatchTest(unittest.TestCase):
    """A batch of DeepSeek Harness stays within the 2048 bytes put inline,
    Unicode counted in bytes."""

    def test_a_batch_of_unicode_keeps_to_2048_bytes(self):
        limits = REG.agents["dsh"]["methods"][0]["limits"]
        box = ms.Box(self)
        output = box.output(limits, "exit-on-event")
        pages = [{"notifications": [dict(ms.event(i), bodyPreview=u"привет 世界 "
                                         * 40) for i in range(1, 9)],
                  "cursor": {"ts": "2026-01-01T00:00:01Z", "id": 8}}]
        box.cycle(ms.FakeExecutor(pages), output, "exit-on-event")
        self.assertTrue(output.out.writes)
        for _, text in output.out.writes:
            self.assertLessEqual(len(text.encode("utf-8")), DSH_INLINE_BYTES)
            for line in text.splitlines():
                json.loads(line)
        printed = [l for l in output.out.lines() if l.get("event") in ("notification", "notification_unfit")]
        self.assertTrue(printed)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class GeminiInTmuxTest(et.TmuxCase):
    """Row #114: Gemini without model steering under tmux - its own method
    is unavailable, so the blind tmux is taken by its three conditions."""

    def test_gemini_without_steering_takes_the_blind_tmux(self):
        self.fresh()
        line, _ = self.preflight(client="gemini", agent="gemini")
        self.assertEqual((line["method"], line["channel"]), ("detached", "tmux"), line)
        self.assertIn(RUNNER.BLIND_WARNING, line["warnings"])

    def test_gemini_with_its_settings_keeps_its_own_method(self):
        self.fresh()
        home = self.env("tok", "gemini")["HOME"]
        write_gemini_settings(home, GEMINI_SETTINGS)
        line, _ = self.preflight(client="gemini", agent="gemini")
        self.assertEqual(line["method"], "exit-on-event", line)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class AcpAgentsUnderPaseoTest(unittest.TestCase):
    """Row #114: an ACP agent of a real record under Paseo is woken by Paseo
    first; Paseo keeps no own method of these records."""

    def test_paseo_goes_first(self):
        for client, agent, argv in (("gemini", "node", ["node", "/usr/bin/gemini", "--acp"]), ("kimi", "kimi", None),
                                    ("copilot", "copilot", None), ("cursor", "agent", ["agent", "acp"]),
                                    ("antigravity", "agy", None)):
            with self.subTest(client=client):
                box = pm.Box(self)
                fake_channel.FakeProgram(box.bin, "paseo")
                write_gemini_settings(box.root, GEMINI_SETTINGS)
                line = box.preflight(box.env(client, PASEO_AGENT_ID=PASEO_ID), [agent, "Paseo Daemon"],
                                     shell_between=False, agent_argv=argv)
                self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"), line)


if __name__ == "__main__":
    unittest.main()
