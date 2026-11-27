#!/usr/bin/env python3
"""SC-001 of the Hermes Agent record (matrix row #112), over the registry the
server embeds.

The expected values come from Hermes Agent 24b14819, not from the record:
- stream: terminal(background=true, heartbeat=60, persist_on_release=true),
  a tick keeps the last 2000 characters (tools/process_registry_notifications
  .py#HEARTBEAT_OUTPUT_CHARS), ticks come at least 60 s apart
  (tools/process_registry.py#HEARTBEAT_MIN_SECONDS);
- background notifications are off by HERMES_BACKGROUND_NOTIFICATIONS, else
  by display.background_process_notifications: "off" or a bare off (YAML 1.1
  false) (gateway/run_config_loaders.py);
- the busy input mode is HERMES_GATEWAY_BUSY_INPUT_MODE, else
  display.busy_input_mode, interrupt by default (#_load_busy_input_mode);
- config.yaml lies in HERMES_HOME, else ~/.hermes (hermes_constants.py);
- `hermes chat --resume ID --query-file - -Q` reads the text from stdin and
  holds its caller for the turn; 'Error: No session found matching' exits 1,
  'Error: Resume retains gateway session policy' 2, 'Your message was not
  sent.' 3 (hermes_cli/gateway_chat.py, gateway_chat_view.py).

Run on its own: python3 internal/handler/assets/runner_tests/entries_hermes_test.py
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

SESSION = "20261127_091500_ab12cd"
HEARTBEAT_OUTPUT_CHARS = 2000
HEARTBEAT_MIN_SECONDS = 60


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class HermesPreflightTest(unittest.TestCase):

    def setUp(self):
        self.box = pm.Box(self)
        self.hermes = fake_channel.FakeProgram(self.box.bin, "hermes")

    def config(self, text, home=None):
        directory = home or os.path.join(self.box.root, ".hermes")
        os.makedirs(directory, exist_ok=True)
        with open(os.path.join(directory, "config.yaml"), "w") as fh:
            fh.write(text)

    def line(self, chain=None, **env):
        return self.box.preflight(self.box.env("hermes", HERMES_SESSION_ID=SESSION, **env), chain or ["hermes"])

    def test_the_agent_process(self):
        for chain, found in (([("python3", ["python3", "/home/u/.local/bin/hermes", "chat"])], True),
                             ([("hermes", None)], True), ([("hermes-agent-x", None)], False)):
            with self.subTest(chain=chain):
                self.assertEqual(w1.agent_of("hermes", chain) is not None, found)

    def test_stream_by_default_with_its_heartbeat(self):
        line = self.line()
        self.assertEqual((line["method"], line["channel"]), ("stream", "stdout"), line)
        for part in ("terminal tool", "background=true", "heartbeat=60", "persist_on_release=true", "no notify"):
            self.assertIn(part, line["instruction"])
        limits = REG.agents["hermes"]["methods"][0]["limits"]
        self.assertLessEqual(limits["batch"]["max"], HEARTBEAT_OUTPUT_CHARS)
        self.assertLessEqual(limits["line"]["max"], HEARTBEAT_OUTPUT_CHARS)
        self.assertGreater(limits["rate"]["intervalSec"], HEARTBEAT_MIN_SECONDS)

    def test_resume_only_with_notifications_off_and_the_queue_mode(self):
        cases = [
            ("off quoted, queue", "display:\n  background_process_notifications: \"off\"\n  busy_input_mode: queue\n",
             {}, "hermes-resume"),
            ("bare off, queue", "display:\n  background_process_notifications: off\n  busy_input_mode: queue\n",
             {}, "hermes-resume"),
            ("off, interrupt", "display:\n  background_process_notifications: off\n  busy_input_mode: interrupt\n",
             {}, None),
            ("off, no mode (interrupt by default)", "display:\n  background_process_notifications: off\n", {}, None),
            ("notifications on", "display:\n  background_process_notifications: all\n  busy_input_mode: queue\n",
             {}, "stdout"),
            ("the variable of the mode wins", "display:\n  background_process_notifications: off\n"
             "  busy_input_mode: interrupt\n", {"HERMES_GATEWAY_BUSY_INPUT_MODE": "queue"}, "hermes-resume"),
            ("the variable of the mode wins against the file", "display:\n  background_process_notifications: off\n"
             "  busy_input_mode: queue\n", {"HERMES_GATEWAY_BUSY_INPUT_MODE": "interrupt"}, None),
            ("the variable of the notifications wins", "display:\n  background_process_notifications: off\n"
             "  busy_input_mode: queue\n", {"HERMES_BACKGROUND_NOTIFICATIONS": "all"}, "stdout"),
            ("the variable turns them off", "display:\n  busy_input_mode: queue\n",
             {"HERMES_BACKGROUND_NOTIFICATIONS": "off"}, "hermes-resume"),
            ("a config not read", "display: {background_process_notifications: off, busy_input_mode: queue}\n",
             {}, None),
        ]
        for name, text, env, channel in cases:
            with self.subTest(case=name):
                self.config(text)
                line = self.line(**env)
                if channel is None:
                    self.assertEqual(line["method"], "journal", line)
                else:
                    self.assertEqual(line["channel"], channel, line)

    def test_the_config_of_hermes_home(self):
        home = os.path.join(self.box.root, "profile")
        self.config("display:\n  background_process_notifications: off\n  busy_input_mode: queue\n", home=home)
        self.assertEqual(self.line(HERMES_HOME=home)["channel"], "hermes-resume")
        # ~/.hermes is not read when HERMES_HOME names another home.
        self.config("display:\n  background_process_notifications: off\n  busy_input_mode: queue\n")
        self.assertEqual(self.line(HERMES_HOME=os.path.join(self.box.root, "empty"))["channel"], "stdout")

    def test_a_delegated_hermes_does_not_resume_its_parent(self):
        # A delegated child keeps its parent's HERMES_SESSION_ID: a nested
        # agent ("Addressee and check", item 2) gets no delivery to it, also
        # when the channel is forced.
        self.config("display:\n  background_process_notifications: off\n  busy_input_mode: queue\n")
        chain = ["hermes", "hermes"]
        for env in ({}, {"SKEIN_MONITOR_CHANNEL": "hermes-resume"}):
            with self.subTest(env=env):
                line = self.line(chain=chain, **env)
                self.assertNotEqual(getattr(line, "get", lambda k: None)("channel"), "hermes-resume", line)


class ResumeOutcomeTest(unittest.TestCase):
    """Each sign of the table to exactly one outcome; unknown answers are
    unknown."""

    ROWS = REG.channel("hermes-resume")["outcomes"]

    def test_each_sign_gives_one_outcome(self):
        for exit_code, stderr, outcome in (
                # A finished turn of -Q (gateway_chat_view.py#_print_exit_ids):
                # the session line before the submit, the id line after it.
                (0, b"Session: 20261127_091500_ab12cd\n\nsession_id: 20261127_091500_ab12cd\n", "queued"),
                # Exit 0 with nothing admitted (#run, a voice stop phrase):
                # no id line, so no sign the text was taken.
                (0, b"Session: 20261127_091500_ab12cd\n", "unknown"),
                (0, b"", "unknown"),
                (1, b"Error: No session found matching 'x'. Use 'hermes sessions list' to see available sessions\n",
                 "refused-before-send"),
                (2, b"Error: Resume retains gateway session policy; creation overrides are unsupported\n",
                 "refused-before-send"),
                (3, b"Your message was not sent. To continue, run `hermes sessions discard x --yes`\n",
                 "refused-before-send"),
                (1, b"Error: Gateway connection/read failed; no local fallback\n", "unknown"),
                (1, b"", "unknown"),
                (7, b"", "unknown")):
            with self.subTest(exit=exit_code, stderr=stderr[:30]):
                self.assertEqual(RUNNER.classify(self.ROWS, exit_code=exit_code, stderr=stderr)[0], outcome)
        self.assertEqual(RUNNER.classify(self.ROWS, still_running=True)[0], "unknown")


@unittest.skipUnless(os.name == "posix", "the fake program is a POSIX script")
class ResumeCallTest(unittest.TestCase):
    """The argument list and the text of the call, and a call that outlives
    its time."""

    def setUp(self):
        self.box = pm.Box(self)
        self.hermes = fake_channel.FakeProgram(self.box.bin, "hermes")
        self.env = self.box.env("hermes", HERMES_SESSION_ID=SESSION)
        self.executor = RUNNER.ArgvExecutor(self.box.token, self.env, [pm.TOKEN])
        self.spec = REG.channel("hermes-resume")["executor"]

    def test_the_text_goes_to_stdin_of_a_resume_of_the_session(self):
        text = "--model x\nquote \" and $(id)"
        result = self.executor.call(self.spec, text, 15)
        self.assertEqual(result.exit_code, 0)
        call, = self.hermes.calls()
        self.assertEqual(call["argv"], ["chat", "--resume", SESSION, "--query-file", "-", "-Q"])
        self.assertEqual(call["stdin"], text)

    def test_a_resume_that_holds_its_caller_is_left_running(self):
        self.hermes.respond([{"exit": 0, "delay": 3}])
        result = self.executor.call(self.spec, "event", 1)
        self.assertTrue(result.still_running)
        self.assertEqual(RUNNER.classify_result(REG.channel("hermes-resume")["outcomes"], result)[0], "unknown")


if __name__ == "__main__":
    unittest.main()
