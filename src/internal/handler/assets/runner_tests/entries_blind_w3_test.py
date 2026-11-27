#!/usr/bin/env python3
"""SC-001 of the Zellij, WezTerm, kitty and Paseo tab records (matrix row
#105) and the epic's "nested tmux/Zellij" (row #106), over the registry the
server embeds.

The expected values come from the sources, not from the records:
- Zellij d73e7d5 (0.46.1), zellij-utils/src/cli.rs#CliAction: action paste
  <chars> --pane-id <id>, action send-keys --pane-id <id> Enter; the answer
  of list-panes (data.rs#PaneInfo, #PaneListEntry) has no PID of a pane's
  process; panes are children of the Zellij server;
- WezTerm 0c655c1, wezterm/src/cli/send_text.rs#SendText: cli send-text
  --pane-id <id> reads stdin; --no-paste writes it as is (a carriage return
  is Enter); list.rs#CliListResultItem gives no PID;
- kitty 121ef18 (0.51.1): send-text --match state:self --bracketed-paste
  auto --stdin, send-key --match state:self enter, "errors are not reported"
  (input without a receipt); ls --match state:self gives the window's pid at
  0.tabs.0.windows.0.pid;
- Paseo c1b43ee5, packages/cli/src/commands/terminal/send-keys.ts: terminal
  send-keys <id> <text> Enter in one call, no receipt; no PID of a tab's
  process; Windows not found.
The rules (spec, US-7): a channel without a PID of its pane is never used,
automatically or forced; tmux, Zellij, WezTerm, kitty, the Paseo tab in this
order, the last three skipped in a multiplexer pane; of nested multiplexers
the nearest decides: Zellij gives no PID, so tmux is used only when it is
the nearest, otherwise no blind channel, with the cause in the line.

Run on its own: python3 internal/handler/assets/runner_tests/entries_blind_w3_test.py
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
import entries_tmux_test as et  # noqa: E402
import failures_test as ft  # noqa: E402
import fake_channel  # noqa: E402
import fake_proc  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG
out = ah.out
ZELLIJ = {"ZELLIJ_PANE_ID": "7", "ZELLIJ_SESSION_NAME": "work"}
WEZTERM = {"WEZTERM_PANE": "12"}
KITTY = {"KITTY_WINDOW_ID": "3", "KITTY_LISTEN_ON": "unix:/tmp/kitty-1"}
TAB = {"PASEO_TERMINAL_ID": "term-5"}
TMUX = {"TMUX_PANE": "%1", "TMUX": et.SERVER_A}
NO_TMUX = {"TMUX_PANE": None, "TMUX": None}


def kitty_ls(pid):
    return out(stdout=[{"id": 1, "tabs": [{"id": 2, "windows": [{"id": 3, "pid": pid}]}]}])


def chain(agent, above):
    """The runner, the shell of the agent's command, the agent (in the
    foreground of its tty unless above says otherwise) and the processes
    above it, nearest first: (pid, name)."""
    source = fake_proc.FakeProcesses()
    source.add(et.RUNNER_PID, 4999, "python3", ft.RUNNER_PROC_ARGV)
    source.add(4999, et.AGENT_PID, "bash")
    parent = above[0][0] if above else 1
    source.add(et.AGENT_PID, parent, agent, [agent], pgid=et.AGENT_PID, tpgid=et.AGENT_PID)
    for i, (pid, name) in enumerate(above):
        ppid = above[i + 1][0] if i + 1 < len(above) else 1
        source.add(pid, ppid, name, [name.split(":")[0]], pgid=pid, tpgid=et.AGENT_PID)
    return source


# A shell in a pane, the multiplexer's server above it.
IN_PANE = [(et.PANE_PID, "bash"), (et.SERVER_PID, "tmux: server")]


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class BlindCase(unittest.TestCase):
    def fresh(self):
        self.world = eo.new_world(self)
        self.cli = dict((name, ah.FakeCli(self.world.bin, name)) for name in ("tmux", "zellij", "kitten", "paseo"))
        # WezTerm's Enter is a carriage return on stdin: a program that reads
        # stdin as bytes keeps it as sent.
        self.cli["wezterm"] = fake_channel.FakeProgram(self.world.bin, "wezterm")
        self.cli["wezterm"].respond([{"exit": 0}])
        self.cli["tmux"].respond(**{"display-message": [out(stdout="%d\n" % et.PANE_PID)]})
        self.cli["kitten"].respond(**{"@ ls": [kitty_ls(et.PANE_PID)]})

    def preflight(self, above=IN_PANE, agent="aider", client="aider", forced=None, **extra):
        source = chain(agent, above)
        patch = mock.patch.object(RUNNER.PosixPlatform, "process_info", staticmethod(source.info))
        patch.start()
        self.addCleanup(patch.stop)
        change = dict(NO_TMUX, SKEIN_MONITOR_CHANNEL=forced or "")
        change.update(extra)
        env = self.world.env("tok", client, **change)
        env = dict((k, v) for k, v in env.items() if v is not None)
        settings = RUNNER.read_settings(env, REG, mode="client")
        pl = RUNNER.place(RUNNER.ancestry(source, et.RUNNER_PID), REG, settings.record, self_pid=et.RUNNER_PID)
        return RUNNER.preflight(env["SKEIN_TOKEN_FILE"], env, REG, settings, detector=RUNNER.RegistryDetector(),
                                prober=et.Probe(), placement=pl)

    def mon(self, channel, program, env, foreground=True):
        """The monitor of the working runner over a result that names
        channel (built here: a channel without a pane PID is never chosen,
        but its record's commands are the subject), the agent in its pane."""
        source = chain("aider", IN_PANE)
        if not foreground:
            source.procs[et.AGENT_PID].tpgid = et.PANE_PID
        patch = mock.patch.object(RUNNER.PosixPlatform, "process_info", staticmethod(source.info))
        patch.start()
        self.addCleanup(patch.stop)
        env = dict(self.world.env("tok", "aider", **env))
        token = env["SKEIN_TOKEN_FILE"]
        rec = REG.channel(channel)
        address = rec["address"]
        identity = {"process": [et.AGENT_PID, "s%d" % et.AGENT_PID], "inherited": [], "terminal": None,
                    "addresses": [{"channel": channel, "value": env[address["env"]], "env": address["env"],
                                   "qualifiers": [[q, env[q]] for q in address.get("qualifiers") or []]}]}
        doc = {"method": "detached", "channel": channel, "fallbacks": [], "identity": identity, "preflightId": 1,
               "programs": {program: self.cli[program].path}}
        journal = RUNNER.Journal(token, ft.ISSUE, secrets=[ft.TOKEN], env=env)
        ledger = RUNNER.Ledger(ft.SERVER, "tok", RUNNER.identity_from_doc(identity), env=env,
                               clock=self.world.clock.now)
        spy = dt.Spy(RUNNER.ArgvExecutor(token, env, [ft.TOKEN], doc["programs"]), self.world.clock)
        sink = RUNNER.DeliverySink(journal, token, doc, REG, env, ledger, spy, ft.ISSUE, cancel=dt.FakeCancel(),
                                   clock=self.world.clock)
        return ft.Monitor(self.world, "tok", sink, journal, doc, spy)

    def calls(self, program, prefix=None):
        calls = self.cli[program].calls() if prefix is None else self.cli[program].calls(prefix)
        return [c["argv"] for c in calls]

    def outcomes(self, channel, program, env, cases):
        """Every sign of the table: (name, answers by subcommand, outcome,
        the subcommands called)."""
        for i, (name, answers, outcome, called) in enumerate(cases):
            with self.subTest(channel=channel, case=name):
                self.fresh()
                mon = self.mon(channel, program, env)
                self.cli[program].respond(**answers)
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                self.assertEqual([" ".join(c[:2]) for c in self.calls(program)], called)


class ZellijRecordTest(BlindCase):
    """Row #105: Zellij - paste by argument, then Enter; no PID of a pane:
    never chosen, automatically or forced."""

    def test_the_paste_and_enter_and_every_sign(self):
        self.fresh()
        mon = self.mon("zellij", "zellij", ZELLIJ)
        mon.page([ft.note(1)])
        paste, enter = self.calls("zellij")
        self.assertEqual(paste[:4], ["action", "paste", "--pane-id", "7"])
        self.assertIn('notification="1"', paste[4])
        self.assertEqual(enter, ["action", "send-keys", "--pane-id", "7", "Enter"])
        self.outcomes("zellij", "zellij", ZELLIJ, [
            ("pasted, entered", {}, "input-written", ["action paste", "action send-keys"]),
            ("the paste failed", {"action paste": [out(1)]}, "unknown", ["action paste"]),
            ("Enter failed", {"action send-keys": [out(1)]}, "unknown", ["action paste", "action send-keys"]),
        ])

    def test_without_a_pane_pid_it_is_never_chosen(self):
        for name, forced in (("automatically", None), ("forced", "zellij")):
            with self.subTest(case=name):
                self.fresh()
                line = self.preflight(above=[(et.PANE_PID, "bash"), (et.SERVER_PID, "zellij")], forced=forced,
                                      **ZELLIJ)
                self.assertEqual((line["method"], line["channel"]), ("journal", None))
                self.assertIn("zellij", line["reason"])
                self.assertEqual(self.calls("zellij", "action paste"), [])


class WezTermRecordTest(BlindCase):
    """Row #105: WezTerm - the text as a bracketed paste on stdin, then a
    carriage return without paste; no PID of a pane: never chosen."""

    def test_the_text_and_the_carriage_return_and_every_sign(self):
        self.fresh()
        mon = self.mon("wezterm", "wezterm", WEZTERM)
        mon.page([ft.note(1)])
        text, enter = self.cli["wezterm"].calls()
        self.assertEqual(text["argv"], ["cli", "send-text", "--pane-id", "12"])
        self.assertIn('notification="1"', text["stdin"])
        self.assertEqual((enter["argv"], enter["stdin"]), (["cli", "send-text", "--pane-id", "12", "--no-paste"], "\r"))
        # Every sign, by the order of the calls: the text, then Enter.
        for i, (name, answers, outcome, calls) in enumerate((
                ("sent, entered", [{"exit": 0}], "input-written", 2),
                ("the text failed", [{"exit": 1}], "unknown", 1),
                ("Enter failed", [{"exit": 0}, {"exit": 1}], "unknown", 2))):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon("wezterm", "wezterm", WEZTERM)
                self.cli["wezterm"].respond(answers)
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                self.assertEqual(len(self.cli["wezterm"].calls()), calls)

    def test_without_a_pane_pid_it_is_never_chosen(self):
        self.fresh()
        line = self.preflight(above=[(et.PANE_PID, "bash")], **WEZTERM)
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertIn("wezterm: its record gives no source of the pane PID", line["reason"])


class KittyRecordTest(BlindCase):
    """Row #105: kitty - its own window by state:self, the window's PID by
    ls, both steps input without a receipt."""

    def test_recognised_and_bound_by_its_window(self):
        self.fresh()
        line = self.preflight(above=[(et.PANE_PID, "bash")], **KITTY)
        self.assertEqual(line["channel"], "kitty")
        self.assertIn(["@", "ls", "--match", "state:self"], self.calls("kitten"))
        self.fresh()
        self.cli["kitten"].respond(**{"@ ls": [kitty_ls(77)]})
        line = self.preflight(above=[(et.PANE_PID, "bash")], **KITTY)
        self.assertEqual(line["channel"], None)

    def test_the_text_and_enter_and_every_sign(self):
        self.fresh()
        mon = self.mon("kitty", "kitten", KITTY)
        mon.page([ft.note(1)])
        text, enter = self.cli["kitten"].calls()
        self.assertEqual(text["argv"], ["@", "send-text", "--match", "state:self", "--bracketed-paste", "auto", "--stdin"])
        self.assertIn('notification="1"', text["stdin"])
        self.assertEqual(enter["argv"], ["@", "send-key", "--match", "state:self", "enter"])
        self.outcomes("kitty", "kitten", KITTY, [
            ("sent, entered", {}, "input-without-receipt", ["@ send-text", "@ send-key"]),
            ("the text failed", {"@ send-text": [out(1)]}, "unknown", ["@ send-text"]),
            ("Enter failed", {"@ send-key": [out(1)]}, "unknown", ["@ send-text", "@ send-key"]),
        ])

    def test_an_agent_out_of_the_foreground_gets_nothing(self):
        self.fresh()
        mon = self.mon("kitty", "kitten", KITTY, foreground=False)
        mon.page([ft.note(1)])
        self.assertEqual(self.calls("kitten"), [])
        self.assertEqual(mon.statuses(), [(1, "refused-before-send")])


class PaseoTabRecordTest(BlindCase):
    """Row #105: a Paseo tab - the text and Enter in one call, no receipt;
    no PID of a tab: never chosen; on Windows the journal."""

    def test_the_text_and_enter_in_one_call(self):
        self.fresh()
        mon = self.mon("paseo-tab", "paseo", TAB)
        mon.page([ft.note(1)])
        [send] = self.calls("paseo")
        self.assertEqual(send[:3] + send[4:], ["terminal", "send-keys", "term-5", "Enter"])
        self.assertIn('notification="1"', send[3])
        self.outcomes("paseo-tab", "paseo", TAB, [
            ("sent", {}, "input-without-receipt", ["terminal send-keys"]),
            ("failed", {"terminal send-keys": [out(1)]}, "unknown", ["terminal send-keys"]),
        ])

    def test_never_chosen_and_the_journal_on_windows(self):
        for platform in ("linux", "windows"):
            with self.subTest(platform=platform):
                self.fresh()
                with mock.patch.object(RUNNER, "_platform_key", lambda: platform):
                    line = self.preflight(above=[(et.PANE_PID, "bash")], forced="paseo-tab", **TAB)
                self.assertEqual((line["method"], line["channel"]), ("journal", None))
                self.assertEqual(self.calls("paseo", "terminal send-keys"), [])


class BlindOrderTest(BlindCase):
    """Row #105: tmux first; WezTerm, kitty and the Paseo tab are skipped in
    a multiplexer pane."""

    def test_a_multiplexer_pane_skips_the_terminal_channels(self):
        self.fresh()
        line = self.preflight(**dict(TMUX, **KITTY))
        self.assertEqual(line["channel"], "tmux")
        # kitty's own window, outside any multiplexer.
        self.fresh()
        line = self.preflight(above=[(et.PANE_PID, "bash")], **KITTY)
        self.assertEqual(line["channel"], "kitty")
        # In a pane of Zellij (its server among the ancestors): kitty is
        # skipped, and Zellij gives no PID - no blind channel.
        self.fresh()
        line = self.preflight(above=[(et.PANE_PID, "bash"), (et.SERVER_PID, "zellij")], **dict(ZELLIJ, **KITTY))
        self.assertEqual(line["channel"], None)


class NestedMultiplexersTest(BlindCase):
    """Row #106: tmux and Zellij nested. Zellij gives no PID of a pane, so
    it is never confirmed: tmux is chosen only when it is the nearest with a
    confirmed binding, otherwise no blind channel, the cause in the line."""

    def test_tmux_inside_zellij(self):
        # runner <- agent <- bash (tmux pane) <- tmux server <- bash (Zellij
        # pane) <- Zellij server: tmux is the nearer one.
        self.fresh()
        line = self.preflight(above=[(et.PANE_PID, "bash"), (et.SERVER_PID, "tmux: server"), (4990, "bash"),
                                     (4989, "zellij")], **dict(TMUX, **ZELLIJ))
        self.assertEqual(line["channel"], "tmux")

    def test_zellij_inside_tmux(self):
        # runner <- agent <- bash (Zellij pane) <- Zellij server <- bash
        # (tmux pane) <- tmux server: Zellij is the nearer one and is not
        # confirmed, so tmux is not used.
        self.fresh()
        self.cli["tmux"].respond(**{"display-message": [out(stdout="4990\n")]})
        line = self.preflight(above=[(et.PANE_PID, "bash"), (et.SERVER_PID, "zellij"), (4990, "bash"),
                                     (4989, "tmux: server")], **dict(TMUX, **ZELLIJ))
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertIn("zellij: its record gives no source of the pane PID", line["reason"])
        self.assertEqual(self.calls("tmux", "load-buffer"), [])


if __name__ == "__main__":
    unittest.main()
