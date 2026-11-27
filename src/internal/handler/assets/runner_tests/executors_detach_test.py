"""Tests of the detached argv call (runner/executors.py#run_detached,
registry.go#Executor.Detach) and of the stillRunning sign
(runner/classify.py) on a synthetic channel: a command that holds its caller
for a whole turn of the agent (Hermes `chat --resume -Q`) is waited on only
for an early exit, which the table classifies; a program still running then
is left to run, never killed, and the call is classified by the row
stillRunning.

Run on its own: python3 internal/handler/assets/runner_tests/executors_detach_test.py
Standard library of Python 3.6 only.
"""

import os
import shutil
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

RUNNER = harness.load_runner()

# The program of the synthetic channel: it reads the text from stdin into
# the marker file, then refuses, answers, or works on past the call.
PROGRAM = r'''
import os, sys, time
data = sys.stdin.read()
with open(os.environ["MARK"], "w") as fh:
    fh.write(data)
mode = sys.argv[1]
if mode == "refuse":
    sys.stderr.write("Error: No session found matching 'x'.\n")
    sys.exit(1)
if mode == "slow":
    while not os.path.exists(os.environ["MARK"] + ".go"):
        time.sleep(0.05)
    with open(os.environ["MARK"] + ".done", "w") as fh:
        fh.write("done")
sys.exit(0)
'''

# The table of the synthetic channel, in the form of the Hermes record.
ROWS = [
    {"match": {"exitCode": 0}, "outcome": "queued"},
    {"match": {"exitCode": 1, "stderrContains": "Error: No session found matching"},
     "outcome": "refused-before-send"},
    {"match": {"stillRunning": True}, "outcome": "unknown"},
]


@unittest.skipUnless(os.name == "posix", "the detached program is a Python script run by its path")
class DetachedCallTest(unittest.TestCase):

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-detach-")
        self.addCleanup(shutil.rmtree, self.root, True)
        token_dir = os.path.join(self.root, "tok")
        os.mkdir(token_dir, 0o700)
        self.token = os.path.join(token_dir, "token")
        with open(self.token, "w") as fh:
            fh.write("skm_" + "T" * 43)
        self.script = os.path.join(self.root, "channel.py")
        with open(self.script, "w") as fh:
            fh.write(PROGRAM)
        self.mark = os.path.join(self.root, "mark")
        self.env = {"PATH": os.environ.get("PATH", os.defpath), "HOME": self.root, "MARK": self.mark}
        self.executor = RUNNER.ArgvExecutor(self.token, self.env, [])
        # A program left running is let go at the end of the test.
        self.addCleanup(self.release)

    def release(self):
        with open(self.mark + ".go", "w") as fh:
            fh.write("go")

    def call(self, mode, timeout_sec, text="event text"):
        spec = {"type": "argv", "argv": [sys.executable, self.script, mode], "feed": "stdin",
                "timeoutSec": timeout_sec, "detach": True}
        return self.executor.call(spec, text, timeout_sec)

    def wait_for(self, path):
        for _ in range(400):
            if os.path.exists(path):
                return True
            time.sleep(0.05)
        return False

    def test_an_early_exit_is_classified_by_the_table(self):
        for mode, outcome in (("refuse", "refused-before-send"), ("ok", "queued")):
            with self.subTest(mode=mode):
                result = self.call(mode, 30)
                self.assertFalse(result.still_running)
                self.assertEqual(RUNNER.classify_result(ROWS, result)[0], outcome)

    def test_the_text_goes_to_stdin_and_never_becomes_an_argument(self):
        text = "--resume other; a line\nand another"
        result = self.call("ok", 30, text=text)
        self.assertEqual(result.exit_code, 0)
        with open(self.mark) as fh:
            self.assertEqual(fh.read(), text)

    def test_a_program_still_running_is_left_to_run_and_the_call_is_unknown(self):
        result = self.call("slow", 1)
        self.assertTrue(result.still_running)
        self.assertIsNone(result.exit_code)
        outcome, sign = RUNNER.classify_result(ROWS, result)
        self.assertEqual((outcome, sign), ("unknown", {"stillRunning": True}))
        # Not killed: let go, it finishes its work after the call.
        self.assertFalse(os.path.exists(self.mark + ".done"))
        self.release()
        self.assertTrue(self.wait_for(self.mark + ".done"), "the detached program was killed")

    def test_a_program_left_to_run_is_reaped_once_it_ends(self):
        before = list(RUNNER._DETACHED)
        result = self.call("slow", 1)
        self.assertTrue(result.still_running)
        left = [p for p in RUNNER._DETACHED if p not in before]
        self.assertEqual(len(left), 1)
        pid = left[0].pid
        self.release()
        self.assertTrue(self.wait_for(self.mark + ".done"))
        for _ in range(400):
            RUNNER.reap_detached()
            if left[0] not in RUNNER._DETACHED:
                break
            time.sleep(0.05)
        self.assertNotIn(left[0], RUNNER._DETACHED)
        # Reaped: no zombie of it is left.
        with self.assertRaises(ChildProcessError):
            os.waitpid(pid, os.WNOHANG)


class StillRunningSignTest(unittest.TestCase):

    def test_only_the_row_still_running_matches_a_call_still_running(self):
        self.assertEqual(RUNNER.classify(ROWS, still_running=True), ("unknown", {"stillRunning": True}))
        # A finished call never matches the row of a call still running.
        self.assertFalse(RUNNER.outcome_sign_matches({"stillRunning": True}, exit_code=0, stderr=b""))
        self.assertTrue(RUNNER.outcome_sign_matches({"stillRunning": True}, still_running=True))
        # A call still running has no exit code: no other row matches it.
        self.assertFalse(RUNNER.outcome_sign_matches({"exitCode": 0}, still_running=True))

    def test_a_table_without_the_row_leaves_a_call_still_running_unknown(self):
        rows = [r for r in ROWS if "stillRunning" not in r["match"]]
        self.assertEqual(RUNNER.classify(rows, still_running=True), ("unknown", {"stillRunning": True}))


if __name__ == "__main__":
    unittest.main()
