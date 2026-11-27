#!/usr/bin/env python3
"""SC-001 of the Orca record (matrix rows #92, #93, #94, #124), over the
registry the server embeds.

The expected values come from the Orca source at cbeddb92, not from the
record:
- every PTY gets ORCA_TERMINAL_HANDLE (src/main/ipc/pty/ipc/spawn-options.ts),
  and a send without --terminal goes to the active terminal of the worktree
  (src/cli/terminals/terminal-selector.ts#getTerminalHandle);
- `orca terminal send --text <t> --enter --wait-submit <s> --json` prints
  {ok: true, result: {send: {accepted, writeSettlement?, prompt: {stages,
  observation, submit?}}}} (src/shared/terminal/runtime/
  runtime-terminal-send-contract.ts#RuntimeTerminalSend); stages
  ['input_accepted', 'turn_started'] once the turn started
  (src/main/runtime/terminal/input/terminal-agent-prompt.ts);
  accepted:false exits 1 (src/cli/terminals/terminal-send.ts#
  terminalSendHandler), without a write settlement it is a guard refusal
  before the write and a settlement 'refused' a write declined before the
  transport (src/main/runtime/rpc/methods/terminal/terminal-send-method.ts#
  refusedSend, src/shared/terminal/pty/lifecycle/pty-write-settlement.ts);
- a failure prints {ok: false, error: {code, data}} and exits 1
  (src/cli/output/cli-error.ts#reportCliError), a local error of the CLI
  with id 'local' and data only when its raiser gave some
  (#cliErrorJsonResponse); runtime_unavailable without data is raised only
  before any connection (src/cli/runtime/profile/metadata.ts, transport.ts
  before createConnection, terminal-send.ts "so no input was sent"), while
  the advice TRANSIENT_DISCONNECT_NEXT_STEPS comes with a failed connect
  and with a close before the answer alike; a failure of the send call itself
  carries data.orchestrationRequestId or data.deliveryOutcome 'unknown'
  (src/cli/runtime/client.ts#call, src/cli/runtime/connection/
  terminal-prompt-mutation-recovery.ts); incompatible_runtime and
  agent_prompt_blocked come before and after the transfer;
- `terminal wait --for tui-idle` exits 1 when not idle, `terminal show
  --json` gives result.terminal with incarnationId and agentIdentity
  (absent when unknown) and sends nothing (src/cli/terminals/terminal.ts);
- the program is orca-ide on Linux, ORCA_CLI_COMMAND in a managed WSL,
  orca.exe on Windows (src/shared/cli/orca-cli-command-name.ts,
  src/main/cli/cli-installer.ts).

Run on its own: python3 internal/handler/assets/runner_tests/entries_orca_test.py
Standard library of Python 3.6 only.
"""

import base64
import copy
import json
import os
import shutil
import stat
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402
import failures_test as ft  # noqa: E402
import fake_proc  # noqa: E402
import harness  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG
HANDLE = "term_7f2c41"
THREAD_A = "019a8e3c-5d2b-7c41-9f0e-cccccccccccc"
THREAD_B = "019a8e3c-5d2b-7c41-9f0e-dddddddddddd"

_ORCA = r'''#!%(python)s
import json, os, sys
HERE = %(here)r
NAME = %(name)r
sub = sys.argv[2] if len(sys.argv) > 2 else ""
with open(os.path.join(HERE, NAME + ".calls.jsonl"), "a") as fh:
    fh.write(json.dumps({"argv": sys.argv[1:]}) + "\n")
with open(os.path.join(HERE, NAME + ".answers.json")) as fh:
    answers = json.load(fh).get(sub) or [{"exit": 0, "stdout": ""}]
count = os.path.join(HERE, NAME + "." + sub + ".count")
n = int(open(count).read()) if os.path.exists(count) else 0
open(count, "w").write(str(n + 1))
a = answers[min(n, len(answers) - 1)]
out = a.get("stdout", "")
sys.stdout.write(out if isinstance(out, str) else json.dumps(out, indent=2))
sys.exit(a.get("exit", 0))
'''


def show(incarnation="pty-1:3", agent="codex"):
    terminal = {"handle": HANDLE, "ptyId": "pty-1", "connected": True, "writable": True}
    if incarnation is not None:
        terminal["incarnationId"] = incarnation
    if agent is not None:
        terminal["agentIdentity"] = agent
    return {"exit": 0, "stdout": {"id": "r1", "ok": True, "result": {"terminal": terminal},
                                  "_meta": {"runtimeId": "rt-1"}}}


def sent(stages, observation="supported", accepted=True, exit_code=0, submit=None, **send):
    prompt = {"requestId": "req-1", "stages": stages, "provider": "codex", "observation": observation,
              "processIncarnation": "pty-1:3", "generation": 1, "baselineWorkingSequence": 0}
    if submit is not None:
        prompt["submit"] = submit
    body = {"handle": HANDLE, "accepted": accepted, "bytesWritten": 120 if accepted else 0, "prompt": prompt}
    body.update(send)
    return {"exit": exit_code, "stdout": {"id": "r2", "ok": True, "result": {"send": body},
                                          "_meta": {"runtimeId": "rt-1"}}}


def damaged_submit(stages, submit):
    """A receipt whose submit is present but no submit of the source (null,
    an empty object, not an object)."""
    answer = sent(stages)
    answer["stdout"]["result"]["send"]["prompt"]["submit"] = submit
    return answer


def refused_send(**send):
    body = {"handle": HANDLE, "accepted": False, "bytesWritten": 0}
    body.update(send)
    return {"exit": 1, "stdout": {"id": "r2", "ok": True, "result": {"send": body}, "_meta": {"runtimeId": "rt-1"}}}


def failed(code, data=None):
    error = {"code": code, "message": "failure"}
    if data is not None:
        error["data"] = data
    return {"exit": 1, "stdout": {"id": "local", "ok": False, "error": error, "_meta": {"runtimeId": None}}}


# src/cli/runtime/connection/transport.ts#TRANSIENT_DISCONNECT_NEXT_STEPS.
NEXT_STEPS = ["Orca is usually busy or restarting: wait a few seconds, then retry. If the command changes something, "
              "first check whether it already took effect, and retry with the request ID this error names, if any.",
              "Do not run 'orca open' to recover. Run it only if the user asks, or if 'orca status --json' reports "
              "Orca is not running or still cannot connect after about 30 seconds of retries."]
TURN = sent(["input_accepted", "turn_started"])
IDLE = {"exit": 0, "stdout": {"ok": True, "result": {"wait": {"satisfied": True}}}}
NOT_IDLE = {"exit": 1, "stdout": {"ok": True, "result": {"wait": {"satisfied": False}}}}


class FakeOrca(object):
    """A fake Orca CLI on PATH named name: per subcommand (show, wait,
    send) it answers in turn, later calls the last answer."""

    def __init__(self, bindir, name="orca-ide"):
        self.dir = bindir
        self.name = name
        self.path = os.path.join(bindir, name)
        with open(self.path, "w") as fh:
            fh.write(_ORCA % {"python": sys.executable, "here": bindir, "name": name})
        os.chmod(self.path, stat.S_IRWXU)
        self.respond()

    def respond(self, show_answers=None, wait=None, send=None):
        with open(os.path.join(self.dir, self.name + ".answers.json"), "w") as fh:
            json.dump({"show": show_answers or [show()], "wait": wait or [IDLE], "send": send or [TURN]}, fh)
        for sub in ("show", "wait", "send"):
            count = os.path.join(self.dir, self.name + "." + sub + ".count")
            if os.path.exists(count):
                os.remove(count)

    def calls(self, sub=None):
        try:
            with open(os.path.join(self.dir, self.name + ".calls.jsonl")) as fh:
                calls = [json.loads(l)["argv"] for l in fh if l.strip()]
        except OSError:
            return []
        return [c for c in calls if sub is None or (len(c) > 1 and c[1] == sub)]


class CaptureProbe(RUNNER.SystemProbeRunner):
    """The probes and the addressee capture as the preflight runs them."""


def chain(*procs):
    """A fake chain, the runner first: each element is (name, argv, pid)
    with pid a live process (its real start) or None (a fake one)."""
    source = fake_proc.FakeProcesses()
    pids = []
    fake = ft.RUNNER_PID
    for name, argv, pid in procs:
        pids.append(fake if pid is None else pid)
        fake -= 1
    for i, (name, argv, pid) in enumerate(procs):
        parent = pids[i + 1] if i + 1 < len(pids) else 1
        if pid is None:
            source.add(pids[i], parent, name, argv)
        else:
            source.add(pids[i], parent, name, argv, start=ft.start_of(pid))
    return source


def placement(client, *procs):
    source = chain(("python3", ft.RUNNER_PROC_ARGV, None), *procs)
    return RUNNER.place(RUNNER.ancestry(source, ft.RUNNER_PID), REG, RUNNER.resolve_record(REG, client),
                        self_pid=ft.RUNNER_PID)


def monitor(world, name, client, procs, **extra):
    """A monitor built as the working runner builds it, over the preflight
    result of the agent chain procs (the runner's parents)."""
    env = world.env(name, client, **extra)
    pl = placement(client, *procs)
    settings = RUNNER.read_settings(env, REG, mode="client")
    line = RUNNER.preflight(env["SKEIN_TOKEN_FILE"], env, REG, settings, detector=RUNNER.RegistryDetector(),
                            prober=CaptureProbe(), placement=pl)
    token = env["SKEIN_TOKEN_FILE"]
    doc = RUNNER.load_preflight(token)
    journal = RUNNER.Journal(token, ft.ISSUE, secrets=[ft.TOKEN], env=env)
    ledger = RUNNER.Ledger(ft.SERVER, name, RUNNER.identity_from_doc(doc["identity"]), env=env,
                           clock=world.clock.now)
    executor = RUNNER.ArgvExecutor(token, env, [ft.TOKEN], doc.get("programs"))
    spy = dt.Spy(executor, world.clock)
    sink = RUNNER.DeliverySink(journal, token, doc, REG, env, ledger, spy, ft.ISSUE, cancel=dt.FakeCancel(),
                               clock=world.clock)
    mon = ft.Monitor(world, name, sink, journal, doc, spy)
    mon.line = line
    mon.env = env
    return mon


def new_world(test):
    """A world whose Codex home exists (the store the codex-queue probe
    checks)."""
    world = ft.World(test)
    os.makedirs(world.base_env["CODEX_HOME"], mode=0o700, exist_ok=True)
    return world


def codex_agent(pid=None):
    """The runner under a shell under a Codex process (this process)."""
    return [("bash", None, None), ("codex", ["codex"], os.getpid() if pid is None else pid), ("bash", None, None)]


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class OrcaRecordTest(unittest.TestCase):
    """Row #92 (SC-001, US-6): recognition, the commands, every sign of the
    table, the addressee check, the programs."""

    def setUp(self):
        self.fresh()

    def fresh(self):
        """A world of its own (its own ledger) with a fake Orca."""
        self.world = new_world(self)
        self.orca = FakeOrca(self.world.bin)

    def mon(self, name="tok", **extra):
        return monitor(self.world, name, "codex", codex_agent(), ORCA_TERMINAL_HANDLE=HANDLE, **extra)

    def test_recognised_by_the_handle_of_the_terminal(self):
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "orca"))
        line = monitor(self.world, "tok-none", "codex", codex_agent()).line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        # The agent's own delivery comes first (step 3 before step 6).
        line = monitor(self.world, "tok-own", "codex", codex_agent(), ORCA_TERMINAL_HANDLE=HANDLE,
                       CODEX_THREAD_ID=THREAD_A).line
        self.assertEqual(line["channel"], "codex-queue")

    def test_the_commands_name_the_terminal_and_the_text_goes_in_an_argument(self):
        mon = self.mon()
        self.orca.respond()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "turn-started")])
        [wait] = self.orca.calls("wait")
        self.assertEqual(wait, ["terminal", "wait", "--terminal", HANDLE, "--for", "tui-idle", "--timeout-ms",
                                "60000", "--json"])
        [send] = self.orca.calls("send")
        self.assertEqual(send[:6], ["terminal", "send", "--terminal", HANDLE, "--text", send[5]])
        self.assertEqual(send[6:], ["--enter", "--wait-submit", "30", "--json"])
        self.assertIn('notification="1"', send[5])
        self.assertNotIn("--retry-request", send)
        # Before the wait and right before the send: terminal show.
        self.assertTrue(all(c[:4] == ["terminal", "show", "--terminal", HANDLE] for c in self.orca.calls("show")))

    def test_every_sign_other_than_a_refusal_gives_one_outcome(self):
        cases = [
            ("the turn started", TURN, "turn-started"),
            ("input accepted, observed", sent(["input_accepted"]), "input-written"),
            ("input accepted, the provider cannot report", sent(["input_accepted"], "unsupported"), "input-written"),
            ("input accepted, then a permission prompt", sent(["input_accepted"], "permission"), "input-written"),
            ("the terminal process was replaced", sent(["input_accepted"], "incarnation_replaced"), "unknown"),
            ("the paste without its Enter", sent(["input_accepted"], submit={"pastedAt": 1, "withheldFor": "terminal-input"}),
             "unknown"),
            # terminal-agent-prompt.ts: submit is {pastedAt, renderWait,
            # enteredAt} once Enter went (pastedAt and enteredAt numbers,
            # renderWait an optional "quiet", "draft" or "cap"), or absent;
            # anything else is damaged.
            ("the turn started, Enter sent", sent(["input_accepted", "turn_started"],
                                                  submit={"pastedAt": 1, "renderWait": "quiet", "enteredAt": 2}),
             "turn-started"),
            ("input accepted, Enter sent", sent(["input_accepted"], submit={"pastedAt": 1, "renderWait": "cap",
                                                                          "enteredAt": 2}), "input-written"),
            ("input accepted, Enter sent, no render wait", sent(["input_accepted"],
                                                                submit={"pastedAt": 1, "enteredAt": 2}),
             "input-written"),
            ("the turn started, a submit without pastedAt", sent(["input_accepted", "turn_started"],
                                                                 submit={"enteredAt": 2}), "unknown"),
            ("input accepted, a submit without pastedAt", sent(["input_accepted"], submit={"enteredAt": 2}), "unknown"),
            ("input accepted, a pastedAt that is no number", sent(["input_accepted"],
                                                                  submit={"pastedAt": "1", "enteredAt": 2}), "unknown"),
            ("the turn started, a null submit", damaged_submit(["input_accepted", "turn_started"], None), "unknown"),
            ("the turn started, an empty submit", damaged_submit(["input_accepted", "turn_started"], {}), "unknown"),
            ("input accepted, a null submit", damaged_submit(["input_accepted"], None), "unknown"),
            ("input accepted, an empty submit", damaged_submit(["input_accepted"], {}), "unknown"),
            ("input accepted, a submit that is no object", damaged_submit(["input_accepted"], "entered"), "unknown"),
            ("a write of unproven fate", refused_send(writeSettlement={"outcome": "unverifiable",
                                                                       "reason": "settlement_timeout"}), "unknown"),
            ("unreachable after the request", failed("runtime_unavailable", {"orchestrationRequestId": "req-1"}),
             "unknown"),
            ("unreachable, delivery unknown", failed("runtime_unavailable", {"deliveryOutcome": "unknown"}), "unknown"),
            # A damaged receipt proves no refusal: only a whole absence does
            # (no writeSettlement at all, no error.data at all).
            ("a refusal with an empty write settlement", refused_send(writeSettlement={}), "unknown"),
            ("a refusal with a null write settlement", refused_send(writeSettlement=None), "unknown"),
            ("unreachable with empty data", failed("runtime_unavailable", {}), "unknown"),
            ("the connection closed after the request", failed("runtime_unavailable", {
                "nextSteps": NEXT_STEPS, "orchestrationRequestId": "req-1"}), "unknown"),
            ("unreachable with null data", {"exit": 1, "stdout": {"id": "local", "ok": False, "error": {
                "code": "runtime_unavailable", "message": "failure", "data": None}, "_meta": {"runtimeId": None}}},
             "unknown"),
            # transport.ts: the same advice comes with a failed connect and
            # with a connection closed before the answer, after the request
            # may have gone out.
            ("the connection failed or closed, the advice only", failed("runtime_unavailable",
                                                                         {"nextSteps": NEXT_STEPS}), "unknown"),
            ("the advice in an object, not a list", failed("runtime_unavailable", {"nextSteps": {"0": NEXT_STEPS[0]}}),
             "unknown"),
            # A failure frame of the runtime itself (not a local error of the
            # CLI) proves nothing of the write.
            ("a failure frame of the runtime without data", {"exit": 1, "stdout": {
                "id": "r2", "ok": False, "error": {"code": "runtime_unavailable", "message": "failure"},
                "_meta": {"runtimeId": "rt-1"}}}, "unknown"),
            ("incompatible runtime before the send", failed("incompatible_runtime"), "unknown"),
            ("incompatible runtime after acceptance", failed("incompatible_runtime", {"deliveryOutcome": "unknown"}),
             "unknown"),
            ("a permission prompt before or after the paste", failed("agent_prompt_blocked"), "unknown"),
            ("a prompt blocked in the send call", failed("agent_prompt_blocked", {"orchestrationRequestId": "req-1"}),
             "unknown"),
            ("no JSON", {"exit": 0, "stdout": "sent\n"}, "unknown"),
            ("an exit code of another version", {"exit": 3, "stdout": TURN["stdout"]}, "unknown"),
        ]
        for i, (name, answer, outcome) in enumerate(cases):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.orca.respond(send=[answer])
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                # No repeat after any of them.
                self.assertEqual(len(self.orca.calls("send")), 1)

    def test_a_refusal_before_send_is_repeated_as_a_new_send(self):
        refusals = {
            "a guard refusal before the write": refused_send(),
            "a write refused before the transport": refused_send(writeSettlement={"outcome": "refused",
                                                                                  "reason": "endpoint_disconnected"}),
            "the host unreachable before the send call": failed("runtime_unavailable"),
        }
        for name, answer in refusals.items():
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.orca.respond(send=[answer, TURN])
                mon.page([ft.note(1)])
                self.assertEqual(mon.statuses(), [(1, "turn-started")])
                sends = self.orca.calls("send")
                self.assertEqual(len(sends), 2)
                self.assertEqual(sends[0], sends[1])
                self.assertNotIn("--retry-request", sends[1])

    def test_refusals_up_to_the_budget_then_the_journal(self):
        mon = self.mon()
        self.orca.respond(send=[failed("runtime_unavailable")])
        mon.page([ft.note(1)])
        self.assertEqual(len(self.orca.calls("send")), 4)
        self.assertEqual(mon.statuses(), [(1, "refused-before-send")])

    def test_an_agent_not_idle_is_not_sent_to(self):
        mon = self.mon()
        self.orca.respond(wait=[NOT_IDLE, IDLE])
        mon.page([ft.note(1)])
        self.assertEqual(len(self.orca.calls("wait")), 2)
        self.assertEqual(len(self.orca.calls("send")), 1)
        self.assertEqual(mon.statuses(), [(1, "turn-started")])

    def test_another_incarnation_or_agent_is_another_addressee(self):
        for answer in (show(incarnation="pty-1:4"), show(agent="claude"), show(incarnation=None)):
            with self.subTest(answer=answer["stdout"]["result"]["terminal"]):
                self.fresh()
                mon = self.mon()
                self.assertEqual(mon.doc["identity"]["captures"]["orca"],
                                 [["result.terminal.incarnationId", "pty-1:3"],
                                  ["result.terminal.agentIdentity", "codex"]])
                self.orca.respond(show_answers=[answer])
                mon.page([ft.note(1)])
                self.assertEqual(self.orca.calls("send"), [])
                self.assertEqual(mon.statuses(), [(1, "unknown")])
                self.assertEqual(len([l for l in mon.lines() if l.get("cause") == "addressee_changed"]), 1)

    def test_a_check_outside_its_table_is_a_refusal_before_send(self):
        # K5 for a terminal channel: an answer of terminal show that proves
        # neither this terminal nor another (a failure, no JSON, no answer in
        # time) hands nothing over and is called again within the budget of
        # INV-8, then the send goes once the check holds.
        # "No answer in time" is the result of a call its timeout ended,
        # given without a real wait: the first check of the page.
        real = RUNNER.run_program

        def first_check_times_out(timed_out):
            def run(argv, env, stdin_data, timeout_sec, secrets=()):
                if "show" in argv and not timed_out:
                    timed_out.append(timeout_sec)
                    return RUNNER.CallResult(None, b"", b"", True)
                return real(argv, env, stdin_data, timeout_sec, secrets)
            return run
        cases = [("ok false", failed("runtime_unavailable"), False),
                 ("no JSON", {"exit": 0, "stdout": "terminal term_7f2c41\n"}, False),
                 ("no answer in time", show(), True)]
        for name, answer, late in cases:
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                timed_out = []
                self.orca.respond(show_answers=[answer, show()] if not late else [show()])
                with mock.patch.object(RUNNER, "run_program",
                                       first_check_times_out(timed_out) if late else real):
                    mon.page([ft.note(1)])
                self.assertEqual(mon.statuses(), [(1, "turn-started")])
                self.assertEqual(len(self.orca.calls("send")), 1)
                self.assertGreaterEqual(len(self.orca.calls("show")) + len(timed_out), 2)
                if late:
                    self.assertEqual(len(timed_out), 1)
                self.assertEqual([l for l in mon.lines() if l.get("cause") == "addressee_changed"], [])

    def test_the_stop_receipt_checks_the_addressee_again_after_the_wait(self):
        # Every transfer: the check, the idle wait, the check again; the
        # agent of the PTY may change during the wait (INV-4).
        mon = self.mon()
        self.orca.respond(show_answers=[show(), show(incarnation="pty-1:4")])
        mon.sink.receipt("The monitor of sk-86 stopped.")
        self.assertEqual(len(self.orca.calls("wait")), 1)
        self.assertEqual(self.orca.calls("send"), [])
        # The same terminal after the wait (a world of its own): the receipt
        # goes.
        self.fresh()
        mon = self.mon("tok-same")
        self.orca.respond()
        mon.sink.receipt("The monitor of sk-86 stopped.")
        self.assertEqual(len(self.orca.calls("send")), 1)
        self.assertGreaterEqual(len(self.orca.calls("show")), 2)

    def test_an_agent_orca_does_not_identify_stays_unidentified(self):
        self.orca.respond(show_answers=[show(agent=None)])
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "turn-started")])

    def test_the_probe_is_terminal_show(self):
        self.orca.respond(show_answers=[failed("runtime_unavailable")])
        line = self.mon().line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertIn("orca: probe argv: unavailable", line["reason"])
        self.assertEqual(self.orca.calls("send"), [])
        self.assertTrue(all(c[1] == "show" for c in self.orca.calls()))

    def test_a_relative_program_runs_by_the_absolute_path_the_preflight_found(self):
        # A variable naming bin/orca-wsl and a relative component of PATH:
        # the preflight resolves them from the runner's directory, and the
        # working runner runs that path, never a bare orca (on Linux the
        # screen reader).
        cwd = os.getcwd()
        self.addCleanup(os.chdir, cwd)
        for name in ("a variable naming a relative path", "a relative component of PATH"):
            with self.subTest(case=name):
                self.fresh()
                os.chdir(self.world.root)
                decoy_dir = os.path.join(self.world.root, "decoy")
                os.mkdir(decoy_dir)
                decoy = FakeOrca(decoy_dir, "orca")
                if name.startswith("a variable"):
                    program = FakeOrca(self.world.bin, "orca-wsl")
                    env = {"ORCA_CLI_COMMAND": "bin/orca-wsl"}
                else:
                    program = self.orca
                    env = {"PATH": "bin" + os.pathsep + decoy_dir + os.pathsep + os.environ.get("PATH", os.defpath)}
                mon = self.mon("tok", **env)
                self.assertEqual(mon.doc["programs"], {"orca": program.path})
                self.assertTrue(os.path.isabs(mon.doc["programs"]["orca"]))
                mon.page([ft.note(1)])
                self.assertEqual(len(program.calls("send")), 1)
                self.assertEqual(decoy.calls(), [])

    def test_the_working_runner_runs_no_name_the_preflight_did_not_find(self):
        executor = RUNNER.ArgvExecutor(self.world.token("tok-strict"), self.world.env("tok-strict", "codex"),
                                       [ft.TOKEN], {})
        with self.assertRaises(RUNNER.ExecutorRefusal):
            executor.call({"type": "argv", "argv": ["orca", "terminal", "send", "--text", "{text}"], "feed": "arg",
                           "timeoutSec": 5}, "hello", 5)
        self.assertEqual(self.orca.calls(), [])

    def test_the_program_of_each_platform(self):
        mon = self.mon()
        self.assertEqual(mon.doc["programs"], {"orca": self.orca.path})
        self.assertEqual(self.orca.calls("send"), [])
        wsl = FakeOrca(self.world.bin, "orca-wsl")
        mon = self.mon("tok-wsl", ORCA_CLI_COMMAND=wsl.path)
        self.assertEqual(mon.doc["programs"], {"orca": wsl.path})
        mon.page([ft.note(1)])
        self.assertEqual(len(wsl.calls("send")), 1)
        root = tempfile.mkdtemp(prefix="skein-orca-win-")
        self.addCleanup(shutil.rmtree, root, True)
        for name in ("orca.exe", "orca.cmd"):
            with open(os.path.join(root, name), "w") as fh:
                fh.write("")
            os.chmod(os.path.join(root, name), stat.S_IRWXU)
        with mock.patch.object(os, "name", "nt"), mock.patch.object(RUNNER, "_platform_key", lambda: "windows"):
            # The native launcher, by an argument list; never the .cmd shim.
            self.assertEqual(RUNNER.channel_programs(REG.channel("orca"), {"PATH": root}, None),
                             {"orca": os.path.join(root, "orca.exe")})


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class NestingTest(unittest.TestCase):
    """Row #93: an agent of another harness that Claude in Orca starts
    without a shell does not own Orca's terminal, automatically or forced
    (SC-002); two Codex under Orca, one nested in the other, each get the
    event in their own thread: the inherited Orca handle does not join them
    (SC-003)."""

    def test_an_agent_claude_starts_directly_does_not_own_the_terminal(self):
        world = new_world(self)
        orca = FakeOrca(world.bin)
        procs = [("bash", None, None), ("codex", ["codex"], os.getpid()), ("claude", ["claude"], None),
                 ("bash", None, None)]
        line = monitor(world, "tok", "codex", procs, ORCA_TERMINAL_HANDLE=HANDLE).line
        self.assertEqual((line["method"], line["channel"]), ("journal", None))
        identity = RUNNER.load_preflight(world.env("tok", "codex")["SKEIN_TOKEN_FILE"])["identity"]
        self.assertIn("orca", identity["inherited"])
        with self.assertRaises(RUNNER.ConfigInvalid):
            monitor(world, "tok-forced", "codex", procs, ORCA_TERMINAL_HANDLE=HANDLE, SKEIN_MONITOR_CHANNEL="orca")
        self.assertEqual(orca.calls("send"), [])

    def test_two_codex_one_nested_get_the_event_each_in_its_thread(self):
        world = new_world(self)
        orca = FakeOrca(world.bin)
        outer = ft.Sleeper(self)
        outer_mon = monitor(world, "tok-outer", "codex", codex_agent(outer.pid), ORCA_TERMINAL_HANDLE=HANDLE,
                            CODEX_THREAD_ID=THREAD_A)
        inner_procs = [("bash", None, None), ("codex", ["codex"], os.getpid()), ("bash", None, None),
                       ("codex", ["codex"], outer.pid), ("bash", None, None)]
        inner_mon = monitor(world, "tok-inner", "codex", inner_procs, ORCA_TERMINAL_HANDLE=HANDLE,
                            CODEX_THREAD_ID=THREAD_B)
        self.assertEqual(outer_mon.doc["channel"], "codex-queue")
        self.assertEqual(inner_mon.doc["channel"], "codex-queue")
        self.assertIn("orca", inner_mon.doc["identity"]["inherited"])
        outer_mon.page([ft.note(1)])
        inner_mon.page([ft.note(1)])
        threads = [c["argv"][2] for c in world.calls("codex")]
        self.assertEqual(sorted(threads), sorted([THREAD_A, THREAD_B]))
        self.assertEqual(outer_mon.statuses(), [(1, "queued")])
        self.assertEqual(inner_mon.statuses(), [(1, "queued")])
        self.assertEqual(orca.calls("send"), [])


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class TwoChannelsOneAddresseeTest(unittest.TestCase):
    """Row #94 (SC-003, Edge): two monitors of one Codex, one by codex queue,
    the other by a forced Orca — one delivery of the event, whichever
    monitor comes first."""

    def test_one_delivery_in_either_order(self):
        for first in ("codex-queue", "orca"):
            with self.subTest(first=first):
                world = new_world(self)
                orca = FakeOrca(world.bin)
                env = dict(ORCA_TERMINAL_HANDLE=HANDLE, CODEX_THREAD_ID=THREAD_A)
                queue_mon = monitor(world, "tok-queue", "codex", codex_agent(), **env)
                orca_mon = monitor(world, "tok-orca", "codex", codex_agent(), SKEIN_MONITOR_CHANNEL="orca", **env)
                self.assertEqual(queue_mon.doc["channel"], "codex-queue")
                self.assertEqual(orca_mon.doc["channel"], "orca")
                order = (queue_mon, orca_mon) if first == "codex-queue" else (orca_mon, queue_mon)
                for mon in order:
                    mon.page([ft.note(1)])
                self.assertEqual(len(world.calls("codex")) + len(orca.calls("send")), 1)
                outcome = "queued" if first == "codex-queue" else "turn-started"
                self.assertEqual(queue_mon.statuses(), [(1, outcome)])
                self.assertEqual(orca_mon.statuses(), [(1, outcome)])


class FakeBinder(object):
    """Confirms the binding of the runner to a pane of the synthetic
    multiplexer when told to."""

    def __init__(self, runner, bound):
        self.runner = runner
        self.bound = bound

    def bind(self, ctx, chan):
        if self.bound and chan["id"] == "mux":
            return self.runner.Binding(chan, True, multiplexer=True, depth=1)
        return None


class SkipInMultiplexerTest(unittest.TestCase):
    """Row #124 (US-6, US-7, decision 4): a runner in a multiplexer pane
    skips step 6 — Orca is detected but not chosen, the multiplexer wakes the
    agent; without the binding Orca is chosen. A synthetic multiplexer
    record stands in for the real ones (row #104 is the epic's
    "Orca + tmux")."""

    MUX = {"id": "mux", "about": "synthetic multiplexer", "source": "test", "step": 7, "order": 1,
           "kind": "terminal", "blind": True, "detect": {"env": [{"name": "MUX_PANE"}]},
           "executor": {"type": "argv", "argv": ["mux", "paste", "{text}"], "feed": "arg", "timeoutSec": 5},
           "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "input-written"}],
           "probeUndefined": True, "addressee": {"process": "foreground"}, "panePid": {"argv": ["mux", "pid"]},
           "binaries": {"linux": ["sh"], "macos": ["sh"]}}

    class Probe(object):
        def run(self, chan, probe, env):
            return "available"

        def capture(self, chan, env):
            return [["result.terminal.incarnationId", "pty-1:3"], ["result.terminal.agentIdentity", "codex"]]

    def choose(self, bound):
        doc = copy.deepcopy(REG.doc)
        doc["channels"].append(self.MUX)
        runner = harness.load_runner(registry=base64.b64encode(json.dumps(doc).encode("utf-8")).decode("ascii"))
        reg = runner.load_registry()
        source, pids = fake_proc.linear(("python3", ft.RUNNER_PROC_ARGV), "bash", "codex", "bash", "tmux")
        record = runner.resolve_record(reg, "codex")
        pl = runner.place(runner.ancestry(source, pids[0]), reg, record, self_pid=pids[0])
        bindir = tempfile.mkdtemp(prefix="skein-orca-mux-")
        self.addCleanup(shutil.rmtree, bindir, True)
        FakeOrca(bindir)
        env = {"PATH": bindir + os.pathsep + os.environ.get("PATH", os.defpath), "ORCA_TERMINAL_HANDLE": HANDLE,
               "MUX_PANE": "%3"}
        detector = runner.RegistryDetector(FakeBinder(runner, bound))
        ctx = runner.Context(reg, runner.Settings(record=record, platform="posix"), env, pl, detector, self.Probe())
        ctx.identity = runner.identity(pl, reg, env, detector.address_specs(ctx), self_pid=pids[0])
        return runner.choose(ctx)

    def test_in_a_multiplexer_pane_step_6_is_skipped(self):
        choice = self.choose(bound=True)
        self.assertEqual(choice.channel, "mux")
        self.assertNotIn("orca", choice.fallbacks)

    def test_outside_a_multiplexer_pane_orca_is_chosen(self):
        self.assertEqual(self.choose(bound=False).channel, "orca")


if __name__ == "__main__":
    unittest.main()
