#!/usr/bin/env python3
"""SC-001 of the environments without a channel (matrix row #108), over the
registry the server embeds: each is recognised by the variables its source
sets on the agent and only by them, and says its kind of host message.

The expected values come from the sources and the catalogue of the spec
(US-8, appendix A.2), not from the records:
- cannot wake the agent: Maestro (maestro-server/src/agent/spawn.rs,
  MAESTRO_SESSION=1), Emdash (runtime.ts#prepareHookEnv EMDASH_PTY_ID,
  workspace-env.ts EMDASH_TASK_ID), Happy in remote mode
  (happyEntrypoint.ts, CLAUDE_CODE_ENTRYPOINT=remote_mobile), Warp
  (local_tty/unix.rs, TERM_PROGRAM=WarpTerminal), Zed (terminal.rs, ZED_TERM),
  Crystal (terminalPanelManager.ts, CRYSTAL_SESSION_ID), crystl
  (documentation, CRYSTL_SHARD), Parallel Code (agent-hooks,
  PARALLEL_CODE_AGENT_ID), Windows Terminal (ConptyConnection.cpp,
  WT_SESSION), Agentastic (documentation, AGENTASTIC_TERMINAL_ID and
  AGENTASTIC_SOCKET_PATH);
- a monitor inside is not supported: Vibe Kanban (container.rs,
  VK_WORKSPACE_ID), Jean (jean-core/src/chat, JEAN_SESSION_ID);
- not recognised, no message: Nimbalyst, the Codex app, Happy in local mode
  (no variable of their own).

Run on its own: python3 internal/handler/assets/runner_tests/entries_nochannel_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_orca_test as eo  # noqa: E402
import entries_tmux_test as et  # noqa: E402
import failures_test as ft  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG
WAKE = "host_cannot_wake"
INSIDE = "host_unsupported"

# (environment, its variables, the code of its message)
RECOGNISED = [
    ("Maestro", {"MAESTRO_SESSION": "1"}, WAKE),
    ("Emdash", {"EMDASH_PTY_ID": "conv-1"}, WAKE),
    ("Emdash", {"EMDASH_TASK_ID": "task-1"}, WAKE),
    ("Emdash", {"EMDASH_PTY_ID": "conv-1", "EMDASH_TASK_ID": "task-1"}, WAKE),
    ("Happy", {"CLAUDE_CODE_ENTRYPOINT": "remote_mobile"}, WAKE),
    ("Warp", {"TERM_PROGRAM": "WarpTerminal"}, WAKE),
    ("Zed", {"ZED_TERM": "true"}, WAKE),
    ("Crystal", {"CRYSTAL_SESSION_ID": "s-1"}, WAKE),
    ("crystl", {"CRYSTL_SHARD": "shard-1"}, WAKE),
    ("Parallel Code", {"PARALLEL_CODE_AGENT_ID": "a-1"}, WAKE),
    ("Windows Terminal", {"WT_SESSION": "0b1e4f7a-2c3d-4e5f-8a9b-0c1d2e3f4a5b"}, WAKE),
    ("Agentastic", {"AGENTASTIC_TERMINAL_ID": "t-1", "AGENTASTIC_SOCKET_PATH": "/tmp/agentastic.sock"}, WAKE),
    ("Vibe Kanban", {"VK_WORKSPACE_ID": "w-1"}, INSIDE),
    ("Jean", {"JEAN_SESSION_ID": "j-1"}, INSIDE),
]

# Variables that are not the sign of the source (another value, half of a
# pair), and environments without a variable of their own.
NOT_RECOGNISED = [
    ("Maestro with another value", {"MAESTRO_SESSION": "0"}),
    ("Happy in local mode, Claude's own entry point", {"CLAUDE_CODE_ENTRYPOINT": "cli"}),
    ("another terminal program", {"TERM_PROGRAM": "iTerm.app"}),
    ("half of Agentastic", {"AGENTASTIC_TERMINAL_ID": "t-1"}),
    ("an empty variable", {"VK_WORKSPACE_ID": ""}),
    ("Nimbalyst, the Codex app, local Happy: nothing of their own", {}),
]


def codes(warnings):
    return [w.split(":", 1)[0] for w in warnings if w.startswith("host_")]


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class NoChannelRecordTest(unittest.TestCase):
    """Row #108: recognition, positive and negative, and the kind of each
    message."""

    def preflight(self, **extra):
        world = eo.new_world(self)
        env = world.env("tok", "aider", **extra)
        settings = RUNNER.read_settings(env, REG, mode="client")
        source = et.processes("aider")
        pl = RUNNER.place(RUNNER.ancestry(source, et.RUNNER_PID), REG, settings.record, self_pid=et.RUNNER_PID)
        return RUNNER.preflight(env["SKEIN_TOKEN_FILE"], env, REG, settings, detector=RUNNER.RegistryDetector(),
                                prober=et.Probe(), placement=pl)

    def test_each_environment_by_the_variables_of_its_source(self):
        for name, env, code in RECOGNISED:
            with self.subTest(environment=name, variables=sorted(env)):
                line = self.preflight(**env)
                self.assertEqual(codes(line["warnings"]), [code])
                [message] = [w for w in line["warnings"] if w.startswith(code)]
                self.assertIn(name, message)

    def test_no_message_without_the_sign(self):
        for name, env in NOT_RECOGNISED:
            with self.subTest(case=name):
                line = self.preflight(**env)
                self.assertEqual(codes(line["warnings"]), [])


if __name__ == "__main__":
    unittest.main()
