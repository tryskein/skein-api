#!/usr/bin/env python3
"""SC-001 of the tmux record (matrix row #103) and the epic's preflight
cases with the real tmux record (row #104), over the registry the server
embeds.

The expected values come from the tmux source at dace7f5 (3.8), not from
the record:
- cmd-load-buffer.c: load-buffer -b <name> - reads the buffer from stdin; a
  failure loads nothing, so nothing can be pasted;
- cmd-paste-buffer.c: paste-buffer -p -d -b <name> -t <pane> writes the
  buffer into the pane (an error such as "no buffer <name>" before any
  write) and deletes it; exit 0 only says the bytes were written;
- cmd-send-keys.c: send-keys -t <pane> Enter;
- cmd-set-buffer.c: delete-buffer -b <name>;
- cmd-display-message.c: display-message -p -t <pane> '#{pane_pid}' prints
  the PID of the pane's process;
- TMUX_PANE is the pane, TMUX the server: one pane id under two servers is
  two panes; Claude Squad runs plain tmux sessions (claudesquad_*).
The epic's cases (spec, SC-002 and US-7): Zavod in tmux - the Zavod host is
detected, the blind tmux is off; TMUX without kinship to the pane - tmux is
not detected; Orca in tmux - step 6 is skipped, tmux wakes the agent under
the conditions of US-7; an unknown client with the blind channel - the line
names the general record and carries the blind warning, not the warning
that the name was not found.

Run on its own: python3 internal/handler/assets/runner_tests/entries_tmux_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402
import entries_addressed_hosts_test as ah  # noqa: E402
import entries_orca_test as eo  # noqa: E402
import failures_test as ft  # noqa: E402
import fake_proc  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG
out = ah.out
PANE = "%1"
SERVER_A = "/tmp/tmux-1000/default,4100,0"
SERVER_B = "/tmp/tmux-1000/other,4200,0"
# The fake chain: the runner, the shell of the agent's command, the agent,
# the pane's shell, the tmux server.
RUNNER_PID, AGENT_PID, PANE_PID, SERVER_PID = 5000, 4998, 4997, 4996
ZAVOD_UUID = "0f4c2a8e-3b1d-4e57-9a62-5d8b7c1e2f30"


# The state of the pane the check reads (tmux format.c#format_cb_pane_input_off,
# #format_cb_pane_synchronized, #format_cb_pane_in_mode): input on, not
# synchronized, no mode.
STATE_OK = {"inputOff": 0, "synchronized": 0, "modes": 0}


def processes(agent, foreground=True, agent_pid=AGENT_PID):
    source = fake_proc.FakeProcesses()
    fg = agent_pid if foreground else PANE_PID
    source.add(RUNNER_PID, 4999, "python3", ft.RUNNER_PROC_ARGV)
    source.add(4999, agent_pid, "bash")
    source.add(agent_pid, PANE_PID, agent, [agent], pgid=agent_pid, tpgid=fg)
    source.add(PANE_PID, SERVER_PID, "bash", pgid=PANE_PID, tpgid=fg)
    source.add(SERVER_PID, 1, "tmux: server", ["tmux"])
    return source


class Probe(object):
    """The probes of the preflight (not the subject here): available unless
    answers says otherwise per channel; Orca's capture as its terminal
    answers it."""

    def __init__(self, answers=None):
        self.answers = answers or {}
        self.secrets = []

    def run(self, chan, probe, env):
        return self.answers.get(chan["id"], "available")

    def capture(self, chan, env):
        return [["result.terminal.incarnationId", "pty-1:3"], ["result.terminal.agentIdentity", "codex"]]


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class TmuxCase(unittest.TestCase):
    def fresh(self, pane_pid=PANE_PID):
        self.world = eo.new_world(self)
        self.tmux = ah.FakeCli(self.world.bin, "tmux")
        # The CLI answers by subcommand; every other call exits 0.
        self.tmux.respond(**{"display-message": [out(stdout="%d\n" % pane_pid)],
                             "display-message -p -F": [out(stdout=STATE_OK)]})

    def env(self, name, client, **extra):
        change = {"TMUX_PANE": PANE, "TMUX": SERVER_A}
        change.update(extra)
        env = self.world.env(name, client, **change)
        return dict((k, v) for k, v in env.items() if v is not None)

    def preflight(self, name="tok", client="aider", agent="aider", foreground=True, prober=None, agent_pid=AGENT_PID,
                  **extra):
        """The preflight line of the agent agent under tmux; the platform
        layer answers by the fake chain from here on."""
        source = processes(agent, foreground, agent_pid)
        patch = mock.patch.object(RUNNER.PosixPlatform, "process_info", staticmethod(source.info))
        patch.start()
        self.addCleanup(patch.stop)
        env = self.env(name, client, **extra)
        settings = RUNNER.read_settings(env, REG, mode="client")
        pl = RUNNER.place(RUNNER.ancestry(source, RUNNER_PID), REG, settings.record, self_pid=RUNNER_PID)
        line = RUNNER.preflight(env["SKEIN_TOKEN_FILE"], env, REG, settings, detector=RUNNER.RegistryDetector(),
                                prober=prober or Probe(), placement=pl)
        return line, env

    def mon(self, name="tok", **extra):
        """The monitor the working runner builds over that preflight."""
        line, env = self.preflight(name, **extra)
        token = env["SKEIN_TOKEN_FILE"]
        doc = RUNNER.load_preflight(token)
        journal = RUNNER.Journal(token, ft.ISSUE, secrets=[ft.TOKEN], env=env)
        ledger = RUNNER.Ledger(ft.SERVER, name, RUNNER.identity_from_doc(doc["identity"]), env=env,
                               clock=self.world.clock.now)
        spy = dt.Spy(RUNNER.ArgvExecutor(token, env, [ft.TOKEN], doc.get("programs")), self.world.clock)
        sink = RUNNER.DeliverySink(journal, token, doc, REG, env, ledger, spy, ft.ISSUE, cancel=dt.FakeCancel(),
                                   clock=self.world.clock)
        mon = ft.Monitor(self.world, name, sink, journal, doc, spy)
        mon.line = line
        return mon

    def argvs(self, prefix=None):
        return [c["argv"] for c in self.tmux.calls(prefix)]


class TmuxRecordTest(TmuxCase):
    """Row #103 (SC-001): recognition by the pane and the server, the pane
    PID command, the calls of one delivery under a buffer of its own, every
    sign of the table, the check of the addressee."""

    def test_recognised_by_the_pane_and_the_server(self):
        for name, extra, chosen in (
                ("pane and server", {}, "tmux"),
                ("a session of Claude Squad", {"TMUX": "/tmp/tmux-1000/default,4100,3"}, "tmux"),
                ("no server", {"TMUX": None}, None),
                ("no pane", {"TMUX_PANE": None}, None)):
            with self.subTest(case=name):
                self.fresh()
                line, _ = self.preflight(**extra)
                self.assertEqual(line["channel"], chosen)
                if chosen:
                    self.assertEqual(self.argvs("display-message"),
                                     [["display-message", "-p", "-t", PANE, "#{pane_pid}"]])

    def test_each_delivery_loads_pastes_and_enters_by_a_buffer_of_its_own(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        mon.page([ft.note(2)], cursor={"ts": "t", "id": 2})
        self.assertEqual(mon.statuses(), [(1, "input-written"), (2, "input-written")])
        loads, pastes = self.tmux.calls("load-buffer"), self.argvs("paste-buffer")
        names = [c["argv"][2] for c in loads]
        self.assertEqual(len(set(names)), 2, names)
        for i, name in enumerate(names):
            self.assertEqual(loads[i]["argv"], ["load-buffer", "-b", name, "-"])
            self.assertIn('notification="%d"' % (i + 1), loads[i]["stdin"])
            self.assertEqual(pastes[i], ["paste-buffer", "-p", "-d", "-b", name, "-t", PANE])
        self.assertEqual(self.argvs("send-keys"), [["send-keys", "-t", PANE, "Enter"]] * 2)
        self.assertEqual(self.argvs("delete-buffer"), [])

    def test_every_sign_gives_one_outcome(self):
        cases = [
            ("loaded, pasted, entered", {}, "input-written", ["load-buffer", "paste-buffer", "send-keys"]),
            ("the buffer not loaded", {"load-buffer": [out(1, stderr="no space\n")]}, "refused-before-send",
             ["load-buffer", "delete-buffer"] * 4),
            ("the paste failed", {"paste-buffer": [out(1, stderr="no buffer skein-x\n")]}, "unknown",
             ["load-buffer", "paste-buffer", "delete-buffer"]),
            ("an unfamiliar answer of the paste", {"paste-buffer": [out(2)]}, "unknown",
             ["load-buffer", "paste-buffer", "delete-buffer"]),
            ("Enter failed", {"send-keys": [out(1)]}, "unknown", ["load-buffer", "paste-buffer", "send-keys"]),
        ]
        for i, (name, answers, outcome, calls) in enumerate(cases):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.tmux.respond(**answers)
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                self.assertEqual([c[0] for c in self.argvs() if c[0] != "display-message"], calls)

    def test_the_state_of_the_pane_before_the_insert_and_before_enter(self):
        # Input off: paste-buffer and send-keys would write nothing and exit
        # 0 - nothing is sent. Synchronized panes would send Enter to the
        # others too, a pane in copy mode reads Enter as a key of the mode:
        # after the insert, no Enter and unknown.
        for name, states, outcome, calls in (
                ("input off", [dict(STATE_OK, inputOff=1)], "refused-before-send", []),
                ("synchronized", [STATE_OK, STATE_OK, dict(STATE_OK, synchronized=1)], "unknown",
                 ["load-buffer", "paste-buffer"]),
                ("in copy mode", [STATE_OK, STATE_OK, dict(STATE_OK, modes=1)], "unknown",
                 ["load-buffer", "paste-buffer"])):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.tmux.respond(**{"display-message -p -F": [out(stdout=st) for st in states]})
                mon.page([ft.note(1)])
                self.assertEqual(mon.statuses(), [(1, outcome)])
                self.assertEqual([c[0] for c in self.argvs() if c[0] not in ("display-message", "delete-buffer")],
                                 calls)
                self.assertEqual(self.argvs("send-keys"), [])

    def test_an_agent_out_of_the_foreground_gets_nothing(self):
        self.fresh()
        mon = self.mon(foreground=False)
        mon.page([ft.note(1)])
        self.assertEqual(self.argvs("load-buffer") + self.argvs("paste-buffer"), [])
        self.assertEqual(mon.statuses(), [(1, "refused-before-send")])


class TmuxEpicMatrixTest(TmuxCase):
    """Row #104 (SC-002 with the real tmux record, US-7, the edge "%1 in two
    tmux sessions")."""

    def zavod(self):
        return {"ZAVOD_ME": ZAVOD_UUID, "ZAVOD_AGENT_ID": ZAVOD_UUID, "ZAVOD_AGENT_ENGINE_ID": "claude",
                "ZAVOD_AGENT_ROOT_PID": str(AGENT_PID), "ZAVOD_AGENT_ROOT_START": "1", "ZAVOD_AGENT_ROOT_BOOT_ID": "b"}

    def test_zavod_in_tmux_keeps_the_blind_tmux_off(self):
        for name, answers, chosen in (("Zavod available", {}, "zavod"),
                                      ("Zavod unavailable", {"zavod": "unavailable"}, None)):
            with self.subTest(case=name):
                self.fresh()
                line, _ = self.preflight(prober=Probe(answers), **self.zavod())
                self.assertEqual(line["channel"], chosen)
                self.assertNotIn("tmux", RUNNER.load_preflight(self.world.token("tok"))["fallbacks"])

    def test_tmux_without_kinship_to_the_pane_is_not_detected(self):
        # The variables are set, but the pane's process is no ancestor of
        # the runner: tmux is not the runner's, and step 6 is reached.
        self.fresh(pane_pid=77)
        eo.FakeOrca(self.world.bin)
        line, _ = self.preflight(client="codex", agent="codex", ORCA_TERMINAL_HANDLE=eo.HANDLE)
        self.assertEqual(line["channel"], "orca")
        self.fresh(pane_pid=77)
        line, _ = self.preflight()
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertIn("tmux: the runner is not in its pane", line["reason"])

    def test_orca_in_tmux_skips_step_6(self):
        self.fresh()
        eo.FakeOrca(self.world.bin)
        line, _ = self.preflight(client="codex", agent="codex", ORCA_TERMINAL_HANDLE=eo.HANDLE)
        self.assertEqual(line["channel"], "tmux")
        self.assertNotIn("orca", RUNNER.load_preflight(self.world.token("tok"))["fallbacks"])
        self.assertIn(RUNNER.BLIND_WARNING, line["warnings"])

    def test_an_unknown_client_with_the_blind_channel(self):
        self.fresh()
        line, env = self.preflight(client="no-such-agent")
        self.assertEqual((line["client"], line["channel"]), ("general", "tmux"))
        self.assertIn(RUNNER.BLIND_WARNING, line["warnings"])
        for warning in REG.general.get("warnings") or []:
            self.assertNotIn(warning, line["warnings"])
        started = RUNNER.monitor_started_line(RUNNER.load_preflight(env["SKEIN_TOKEN_FILE"]), ft.ISSUE, True)
        self.assertIn(RUNNER.BLIND_WARNING, started["warnings"])

    def test_one_pane_id_under_two_servers_is_two_addressees(self):
        # Two agents, one in pane %1 of each server (one process has one
        # TMUX: monitors of one agent process are one addressee).
        self.fresh()
        a = self.mon("tok-a", TMUX=SERVER_A)
        a.page([ft.note(1)])
        b = self.mon("tok-b", TMUX=SERVER_B, agent_pid=4988)
        b.page([ft.note(1)])
        self.assertEqual(len(self.argvs("paste-buffer")), 2)
        self.assertEqual([a.statuses(), b.statuses()], [[(1, "input-written")], [(1, "input-written")]])
        # A second monitor of the same pane of the same server: the event
        # is not pasted again.
        again = self.mon("tok-c", TMUX=SERVER_A)
        again.page([ft.note(1)])
        self.assertEqual(len(self.argvs("paste-buffer")), 2)


if __name__ == "__main__":
    unittest.main()
