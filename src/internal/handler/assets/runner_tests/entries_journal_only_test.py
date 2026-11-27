#!/usr/bin/env python3
"""SC-001 of the records a host or the journal serves (matrix row #116),
over the registry the server embeds: Goose, Cline, Continue, Codebuff,
Freebuff, Autohand, Roo Code, Mistral Vibe, Devin, Auggie, Kiro, Rovo Dev,
Crush and, until plugins (sk-1106), Pi, Amp, Kilo Code and MiMo Code.

The expected executables and Windows shells come from the sources of the
records (spec appendix A.1): e.g. npm bin cn of @continuedev/cli, codebuff
and freebuff (Git Bash on Windows, WINDOWS.md), kiro-cli, acli, crush,
vibe and vibe-app-server, auggie, mimo. With a host that launched the agent
the host is taken (Kiro under Paseo, SC-002); without one the journal: the
preflight line and monitor_started name the method journal and the journal
path, and the instruction points to the `journal` field (not the path,
INV-17) with its format (a JSON object per line) and rotation (1 MiB x 5),
with no step of reading it on every turn (US-8).

Run on its own: python3 internal/handler/assets/runner_tests/entries_journal_only_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_stream_w1_test as w1  # noqa: E402
import fake_channel  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402

RUNNER = pm.RUNNER
REG = pm.REG

# client -> (an executable of its process, a process that is not it, the
# Windows shell of A.1 or None for the default)
RECORDS = {
    "goose": ("goose", "goosed", None),
    "cline": ("cline", "clinez", None),
    "continue": ("cn", "cnx", None),
    "codebuff": ("codebuff", "codebuffer", "git-bash"),
    "freebuff": ("freebuff", "freebuffer", "git-bash"),
    "autohand": ("autohand", "autohandx", None),
    "roo": ("roo", "rooster", None),
    "mistral-vibe": ("vibe", "vibes", None),
    "devin": ("devin", "devinx", None),
    "aug": ("auggie", "augx", None),
    "kiro": ("kiro-cli", "kiro", None),
    "rovo": ("acli", "acl", None),
    "crush": ("crush", "crusher", None),
    "pi": ("pi", "pix", None),
    "amp": ("amp", "ampx", None),
    "kilo": ("kilo", "kilox", None),
    "mimo-code": ("mimo", "mimox", None),
}
PASEO_ID = "0f8fad5b-d9cb-469f-a165-70867728950e"


def check_journal_line(test, line):
    """US-8: the method journal with its path; the instruction's meaning."""
    test.assertEqual((line["method"], line["channel"]), ("journal", None), line)
    test.assertTrue(line["journal"])
    instruction = line["instruction"]
    test.assertIn("`journal` field", instruction)
    test.assertNotIn(line["journal"], instruction)
    test.assertIn("JSON object per line", instruction)
    test.assertIn("1 MiB", instruction)
    test.assertIn("5 files", instruction)
    for promise in ("every turn", "each turn", "on every", "each step"):
        test.assertNotIn(promise, instruction)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class JournalOnlyTest(unittest.TestCase):

    def test_recognition_and_the_windows_shell(self):
        for client, (exe, other, shell) in sorted(RECORDS.items()):
            with self.subTest(client=client):
                self.assertIsNotNone(w1.agent_of(client, [(exe, None)]))
                self.assertIsNone(w1.agent_of(client, [(other, None)]))
                self.assertEqual(REG.agents[client]["methods"], [])
                self.assertEqual(REG.agents[client].get("windowsShell"), shell)

    def test_a_short_name_marks_only_the_program_or_the_script_node_runs(self):
        # A short entry-point name is an ordinary word of other command
        # lines: it marks the process only as its executable or as the
        # script node runs (the npm bin of the record), never as any
        # argument and never in a shell.
        for client, name in (("pi", "pi"), ("continue", "cn"), ("roo", "roo"), ("kilo", "kilo"),
                             ("command-code", "cmd")):
            with self.subTest(client=client):
                self.assertIsNotNone(w1.agent_of(client, [("node", ["node", "/usr/lib/node_modules/x/bin/" + name])]))
                self.assertIsNotNone(w1.agent_of(client, [("node", ["node", "--no-warnings", "/usr/bin/" + name])]))
                for other in ([("vim", ["vim", name])], [("less", ["less", "/tmp/" + name])],
                              [("node", ["node", "/srv/app.js", name])], [("bash", ["bash", "/usr/bin/" + name])],
                              [("cmd.exe", ["cmd.exe", "/c", name])]):
                    self.assertIsNone(w1.agent_of(client, other), other)

    def test_an_entry_point_name_does_not_mark_a_shell(self):
        # A shell runs whatever command line it got: opencode.cjs in its
        # arguments does not make it the agent.
        self.assertIsNotNone(w1.agent_of("opencode", [("node", ["node", "/usr/lib/opencode/bin/opencode.cjs"])]))
        self.assertIsNone(w1.agent_of("opencode", [("sh", ["sh", "-c", "/usr/lib/opencode/bin/opencode.cjs"])]))

    def test_without_a_host_the_journal(self):
        for client, (exe, _, _) in sorted(RECORDS.items()):
            with self.subTest(client=client):
                box = pm.Box(self)
                env = box.env(client)
                line = box.preflight(env, [exe])
                check_journal_line(self, line)
                started = RUNNER.monitor_started_line(RUNNER.load_preflight(env["SKEIN_TOKEN_FILE"]), pm.ISSUE, True)
                self.assertEqual((started["method"], started["journal"]), ("journal", line["journal"]))

    def test_with_a_host_that_launched_it_the_host(self):
        # SC-002 "Kiro under Paseo": Paseo keeps no own method of these
        # records, so its channel delivers.
        for client in ("kiro", "goose", "crush"):
            with self.subTest(client=client):
                box = pm.Box(self)
                fake_channel.FakeProgram(box.bin, "paseo")
                env = box.env(client, PASEO_AGENT_ID=PASEO_ID)
                line = box.preflight(env, [RECORDS[client][0], "Paseo Daemon"], shell_between=False)
                self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"), line)


if __name__ == "__main__":
    unittest.main()
