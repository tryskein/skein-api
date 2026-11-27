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
- config.yaml lies in HERMES_HOME, else in the platform default with the
  literal suffix HERMES_DATA_DIR_SUFFIX: ~/.hermes<suffix> on POSIX,
  %LOCALAPPDATA%\hermes<suffix> on Windows, ~/AppData/Local/hermes<suffix>
  when LOCALAPPDATA is empty (hermes_constants.py#_platform_default_hermes_home);
- a variable is read stripped, an empty one gives way to the file, and a
  mode is compared lower-cased (run_config_loaders.py#_env_or_cfg_str,
  #_load_busy_input_mode, background_process_notifications);
- `hermes chat --resume ID --query-file - -Q` reads the text from stdin and
  holds its caller for the turn; 'Error: No session found matching' exits 1,
  'Error: Resume retains gateway session policy' 2, 'Your message was not
  sent.' 3 (hermes_cli/gateway_chat.py, gateway_chat_view.py).

Run on its own: python3 internal/handler/assets/runner_tests/entries_hermes_test.py
Standard library of Python 3.6 only.
"""

import os
import shutil
import sys
import tempfile
import unittest
import unittest.mock

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

    def test_the_data_suffix_names_the_default_home(self):
        resume = "display:\n  background_process_notifications: off\n  busy_input_mode: queue\n"
        self.config(resume, home=os.path.join(self.box.root, ".hermes-work"))
        self.assertEqual(self.line(HERMES_DATA_DIR_SUFFIX="-work")["channel"], "hermes-resume")
        # Without the suffix that home is not Hermes's: the default applies.
        self.assertEqual(self.line()["channel"], "stdout")
        # With it, ~/.hermes is not read.
        self.config(resume)
        self.assertEqual(self.line(HERMES_DATA_DIR_SUFFIX="-other")["channel"], "stdout")

    def test_switches_are_read_stripped_and_lower_cased(self):
        for name, text, env, channel in (
                ("the variable off in capitals and spaces", "display:\n  busy_input_mode: queue\n",
                 {"HERMES_BACKGROUND_NOTIFICATIONS": " OFF "}, "hermes-resume"),
                ("the variable of the mode with spaces and capitals",
                 "display:\n  background_process_notifications: off\n",
                 {"HERMES_GATEWAY_BUSY_INPUT_MODE": " Queue "}, "hermes-resume"),
                ("the file's off with spaces in quotes",
                 "display:\n  background_process_notifications: ' Off '\n  busy_input_mode: queue\n", {},
                 "hermes-resume"),
                ("the file's mode in capitals", "display:\n  background_process_notifications: off\n"
                 "  busy_input_mode: \"QUEUE\"\n", {}, "hermes-resume"),
                ("oFf is off", "display:\n  background_process_notifications: oFf\n  busy_input_mode: queue\n",
                 {}, "hermes-resume")):
            with self.subTest(case=name):
                self.config(text)
                self.assertEqual(self.line(**env)["channel"], channel)

    def test_a_variable_of_white_space_gives_way_to_the_file(self):
        # _env_or_cfg_str: the variable stripped and empty -> the YAML.
        for name, text, env, channel in (
                ("notifications: the file's off", "display:\n  background_process_notifications: off\n"
                 "  busy_input_mode: queue\n", {"HERMES_BACKGROUND_NOTIFICATIONS": "   "}, "hermes-resume"),
                ("notifications: the file's default", "display:\n  busy_input_mode: queue\n",
                 {"HERMES_BACKGROUND_NOTIFICATIONS": " \t "}, "stdout"),
                ("mode: the file's queue", "display:\n  background_process_notifications: off\n"
                 "  busy_input_mode: queue\n", {"HERMES_GATEWAY_BUSY_INPUT_MODE": "  "}, "hermes-resume"),
                ("mode: the file's interrupt", "display:\n  background_process_notifications: off\n"
                 "  busy_input_mode: interrupt\n", {"HERMES_GATEWAY_BUSY_INPUT_MODE": "  "}, None)):
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

    def test_hermes_home_is_read_as_hermes_reads_it(self):
        # Review round 2 of wave 14, row 17 (hermes_constants.py
        # #get_process_hermes_home, #_expand_hermes_home): HERMES_HOME is
        # read stripped, then expanded - variables, then "~".
        home = os.path.join(self.box.root, "profile")
        self.config("display:\n  background_process_notifications: off\n  busy_input_mode: queue\n", home=home)
        for name, env in (("padded with white space", {"HERMES_HOME": "  %s \t" % home}),
                          ("under ~", {"HERMES_HOME": "~/profile"}),
                          ("by a variable", {"HERMES_HOME": "$BASE/profile", "BASE": self.box.root}),
                          ("by a braced variable", {"HERMES_HOME": "${BASE}/profile", "BASE": self.box.root})):
            with self.subTest(case=name):
                self.assertEqual(self.line(**env)["channel"], "hermes-resume")

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


class WindowsHomeTest(unittest.TestCase):
    """The Windows default home (hermes_constants.py#_platform_default_hermes_home):
    %LOCALAPPDATA%\\hermes<suffix>, ~/AppData/Local/hermes<suffix> when
    LOCALAPPDATA is empty; read by the conditions of the resume method on a
    runner of that platform."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-hermes-win-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.method = [m for m in REG.agents["hermes"]["methods"] if m.get("channel") == "hermes-resume"][0]

    def config(self, directory):
        os.makedirs(directory, exist_ok=True)
        with open(os.path.join(directory, "config.yaml"), "w") as fh:
            fh.write("display:\n  background_process_notifications: off\n  busy_input_mode: queue\n")

    def holds(self, env, platform):
        env = dict({"HERMES_SESSION_ID": SESSION, "HOME": self.root}, **env)
        with unittest.mock.patch.object(RUNNER, "runner_platform", lambda: platform):
            return RUNNER.conditions_hold(self.method["conditions"], env)[0]

    def test_the_local_app_data_home(self):
        local = os.path.join(self.root, "Local")
        self.config(os.path.join(local, "hermes-x"))
        self.assertTrue(self.holds({"LOCALAPPDATA": local, "HERMES_DATA_DIR_SUFFIX": "-x"}, "windows"))
        # On POSIX that directory is not Hermes's home.
        self.assertFalse(self.holds({"LOCALAPPDATA": local, "HERMES_DATA_DIR_SUFFIX": "-x"}, "posix"))

    def test_local_app_data_is_read_stripped(self):
        # Review round 2 of wave 14, row 17: LOCALAPPDATA is read stripped
        # (#_platform_default_hermes_home), one of white space only is empty.
        local = os.path.join(self.root, "Local")
        self.config(os.path.join(local, "hermes"))
        self.assertTrue(self.holds({"LOCALAPPDATA": "  %s  " % local}, "windows"))
        self.config(os.path.join(self.root, "AppData", "Local", "hermes"))
        self.assertTrue(self.holds({"LOCALAPPDATA": "   "}, "windows"))

    def test_the_profile_home_without_local_app_data(self):
        self.config(os.path.join(self.root, "AppData", "Local", "hermes"))
        self.assertTrue(self.holds({}, "windows"))
        # ~/.hermes is the POSIX home, not the Windows one.
        self.assertFalse(self.holds({}, "posix"))


class PathExpansionTest(unittest.TestCase):
    """Review round 2 of wave 14, row 17 and info row 23: the expansion of a
    setting path is Python's (os.path.expandvars, os.path.expanduser,
    Path.home of CPython 3.8 and later) with the variables of the agent
    process, on POSIX and on Windows."""

    def nt(self):
        return unittest.mock.patch.object(RUNNER.os, "name", "nt")

    def test_variables_on_posix(self):
        env = {"A": "a", "B": "b"}
        self.assertEqual(RUNNER.expand_vars("$A/${B}/$C/${D}/x", env), "a/b/$C/${D}/x")
        self.assertEqual(RUNNER.expand_vars("no variable", env), "no variable")

    def test_variables_on_windows(self):
        env = {"LocalAppData": "C:\\L", "A": "a"}
        with self.nt():
            # %NAME%, ${NAME}, $NAME; names of any case; an unknown one, a
            # quoted part, %% and $$ stay.
            self.assertEqual(RUNNER.expand_vars("%LOCALAPPDATA%\\h", env), "C:\\L\\h")
            self.assertEqual(RUNNER.expand_vars("${a}/$A/%Z%/'%A%'/%%/$$", env), "a/a/%Z%/'%A%'/%/$")
            # A hyphen is part of a $NAME there.
            self.assertEqual(RUNNER.expand_vars("$A-x", env), "$A-x")

    def test_the_home_on_posix(self):
        env = {"HOME": "/home/u", "USERPROFILE": "/elsewhere"}
        self.assertEqual(RUNNER.expand_user("~/h", env), "/home/u/h")
        self.assertEqual(RUNNER.expand_user("~", env), "/home/u")
        self.assertEqual(RUNNER.expand_user("/abs/h", env), "/abs/h")
        # The home of another account is no setting of this agent.
        self.assertIsNone(RUNNER.expand_user("~other/h", env))
        self.assertIsNone(RUNNER.expand_user("~/h", {}))

    def test_the_home_on_windows(self):
        with self.nt():
            # USERPROFILE first, HOME not read (Python 3.8 and later).
            self.assertEqual(RUNNER.user_home({"HOME": "C:\\h", "USERPROFILE": "C:\\Users\\u"}),
                             "C:\\Users\\u")
            self.assertEqual(RUNNER.user_home({"HOMEDRIVE": "D:", "HOMEPATH": "\\Users\\v"}), "D:\\Users\\v")
            self.assertIsNone(RUNNER.user_home({"HOME": "C:\\h"}))
            self.assertEqual(RUNNER.expand_user("~\\h", {"USERPROFILE": "C:\\U"}), "C:\\U\\h")
            self.assertEqual(RUNNER.expand_path("~/AppData/x", {"HOME": "C:\\h", "USERPROFILE": "C:\\U"}),
                             RUNNER.os.path.join("C:\\U", "AppData/x"))

    def test_a_variable_of_white_space_only_is_not_set(self):
        env = {"V": "  ", "S": " -x "}
        self.assertIsNone(RUNNER.expand_path("{env:V}/c.yaml", env))
        self.assertEqual(RUNNER.expand_path("/a/h{env?:V}/c.yaml", env), "/a/h/c.yaml")
        # A value that is not white space only is taken as it is.
        self.assertEqual(RUNNER.expand_path("/a/h{env?:S}/c.yaml", env), "/a/h -x /c.yaml")


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
                # Review round 2 of wave 14, row 11: the id is a line of its
                # own, "session_id: " and the id; the prefix with no id, an id
                # with more after it, or the words inside another line (a hint
                # before the submit) name no finished turn.
                (0, b"\nsession_id: 20261127_091500_ab12cd\r\n", "queued"),
                (0, b"\nsession_id: \n", "unknown"),
                (0, b"\nsession_id: 20261127_091500_ab12cd and more\n", "unknown"),
                (0, b"hint: resume with session_id: 20261127_091500_ab12cd\n", "unknown"),
                (0, b" session_id: 20261127_091500_ab12cd\n", "unknown"),
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
        # A call left running ends with the test, as with the monitor.
        self.addCleanup(RUNNER.DETACHED_CALLS.end_all)

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
