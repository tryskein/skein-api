"""Tests of the detached argv call (runner/executors.py#run_detached,
registry.go#Executor.Detach) and of the stillRunning sign
(runner/classify.py) on a synthetic channel: a command that holds its caller
for a whole turn of the agent (Hermes `chat --resume -Q`) is waited on only
for an early exit, which the table classifies; a program still running then
is left to run and the call is classified by the row stillRunning. A
program left running lives at most its window plus one grace
(DETACHED_GRACE_SEC): then its tree is killed, as it is when the monitor
stops; one call of a channel runs at a time.

Run on its own: python3 internal/handler/assets/runner_tests/executors_detach_test.py
Standard library of Python 3.6 only.
"""

import os
import shutil
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

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
if mode == "tree":
    # A descendant that would outlive the program if only it were killed.
    child = __import__("subprocess").Popen([sys.executable, "-c", "import time; time.sleep(600)"])
    with open(os.environ["MARK"] + ".pids", "w") as fh:
        fh.write("%d %d" % (os.getpid(), child.pid))
    time.sleep(600)
if mode == "slow":
    while not os.path.exists(os.environ["MARK"] + ".go"):
        time.sleep(0.05)
    with open(os.environ["MARK"] + ".done", "w") as fh:
        fh.write("done")
sys.exit(0)
'''

# What a kill of a tree that failed reports (a Windows job object call).
FAILURE = "TerminateJobObject error 5"

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
        # Within its deadline it is not killed: let go, it finishes its work.
        self.assertFalse(os.path.exists(self.mark + ".done"))
        self.release()
        self.assertTrue(self.wait_for(self.mark + ".done"), "the detached program was killed")

    def detached(self, mode, calls, key="channel"):
        spec_argv = [sys.executable, self.script, mode]
        return RUNNER.run_detached(spec_argv, self.env, b"event text", 0.2, key=key, calls=calls)

    def pids(self):
        self.assertTrue(self.wait_for(self.mark + ".pids"))
        for _ in range(400):
            with open(self.mark + ".pids") as fh:
                text = fh.read().split()
            if len(text) == 2:
                return [int(p) for p in text]
            time.sleep(0.05)
        self.fail("no pids written")

    def test_a_hung_program_is_killed_with_its_tree_at_its_deadline(self):
        # Review round 2 of wave 14, row 4: the watcher of the call reaps it
        # on its own; the test only moves the clock and waits for the
        # watcher's next looks.
        now = [1000.0]
        looks = [0]

        def clock():
            looks[0] += 1
            return now[0]
        calls = RUNNER.DetachedCalls(clock=clock)
        self.addCleanup(calls.end_all)
        with mock.patch.object(RUNNER, "_DETACHED_WATCH_STEP_SEC", 0.02):
            result = self.detached("tree", calls)
            self.assertTrue(result.still_running)
            program, child = self.pids()
            # One second before the window plus the grace: it runs on
            # through two more looks of the watcher.
            now[0] = 1000.0 + 0.2 + RUNNER.DETACHED_GRACE_SEC - 1
            seen = looks[0]
            self.assertTrue(wait_until(lambda: looks[0] >= seen + 2), "the watcher does not look")
            self.assertTrue(alive(program) and alive(child))
            self.assertEqual(calls.running(), ["channel"])
            # At the deadline: the watcher kills the program and its
            # descendant.
            now[0] = 1000.0 + 0.2 + RUNNER.DETACHED_GRACE_SEC
            self.assertTrue(wait_until(lambda: calls.running() == []), "the watcher did not end the call")
        self.assertTrue(gone(program) and gone(child), "the tree of the hung call outlived its deadline")

    def test_one_call_of_a_channel_runs_at_a_time(self):
        calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
        self.addCleanup(calls.end_all)
        self.assertTrue(self.detached("slow", calls).still_running)
        with self.assertRaises(RUNNER.ExecutorRefusal):
            self.detached("ok", calls)
        # Another channel is not held by it.
        self.assertTrue(self.detached("slow", calls, key="other").still_running)
        self.assertEqual(calls.running(), ["channel", "other"])

    def test_no_second_call_of_a_channel_while_its_old_tree_is_being_killed(self):
        # Review round 2 of wave 14, row 5: the record of a channel leaves
        # the table only once its tree is killed, under the lock a new call
        # is admitted under.
        now = [1000.0]
        calls = RUNNER.DetachedCalls(clock=lambda: now[0])
        self.addCleanup(calls.end_all)
        self.assertTrue(self.detached("tree", calls).still_running)
        end_tree = RUNNER._end_tree
        killing, killed = threading.Event(), threading.Event()
        order = []

        def slow_end_tree(proc, tree):
            killing.set()
            killed.wait(30)
            end_tree(proc, tree)
            order.append("old tree killed")

        def second_call():
            try:
                calls.admit("channel")
                order.append("second call admitted")
            except RUNNER.ExecutorRefusal:
                order.append("second call refused")
        with mock.patch.object(RUNNER, "_end_tree", slow_end_tree):
            now[0] = 1000.0 + 0.2 + RUNNER.DETACHED_GRACE_SEC
            reaper = threading.Thread(target=calls.reap)
            reaper.start()
            self.assertTrue(killing.wait(30), "the deadline did not kill the tree")
            second = threading.Thread(target=second_call)
            second.start()
            # A second call let in now would be admitted while the old tree
            # lives: it is given the time to get through before the kill.
            second.join(0.5)
            killed.set()
            reaper.join(30)
            second.join(30)
        self.assertEqual(order, ["old tree killed", "second call admitted"])

    def failing_kill(self):
        """The tree kill of the platform kills the group but reports a
        failure, as a job object call of Windows that failed does."""
        kill_group = RUNNER.PosixPlatform.kill_group

        def tree_kill(proc, job):
            kill_group(proc.pid)
            return FAILURE
        return mock.patch.object(RUNNER.PosixPlatform, "tree_kill", staticmethod(tree_kill))

    def test_a_kill_that_cannot_end_the_tree_is_never_lost(self):
        # Review round 2 of wave 14, row 3: whatever way the call ends, a
        # failed kill of its tree is told; a call that gave its outcome
        # carries it (unknown with the sign treeKill), a call already given
        # as unknown tells the listener of the calls.
        with self.failing_kill():
            calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
            self.addCleanup(calls.end_all)
            early = self.detached("ok", calls)
            self.assertEqual(early.exit_code, 0)
            self.assertEqual(RUNNER.classify_result(ROWS, early), ("unknown", {"treeKill": FAILURE}))

            closing = RUNNER.DetachedCalls(clock=lambda: 0.0)
            closing.closing = True
            killed = self.detached("slow", closing)
            self.assertTrue(killed.still_running)
            self.assertEqual(RUNNER.classify_result(ROWS, killed), ("unknown", {"treeKill": FAILURE}))
            self.assertEqual(closing.running(), [])

            told = []
            stopped = RUNNER.DetachedCalls(clock=lambda: 0.0)
            stopped.on_failure = lambda key, failure: told.append((key, failure))
            self.assertTrue(self.detached("slow", stopped).still_running)
            self.assertEqual(told, [])
            stopped.close()
            self.assertEqual(told, [("channel", FAILURE)])

    def test_a_program_left_to_run_is_reaped_once_it_ends(self):
        calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
        self.addCleanup(calls.end_all)
        with mock.patch.object(RUNNER, "_DETACHED_WATCH_STEP_SEC", 0.02):
            self.assertTrue(self.detached("slow", calls).still_running)
            self.release()
            self.assertTrue(self.wait_for(self.mark + ".done"))
            # Its watcher reaps it.
            self.assertTrue(wait_until(lambda: calls.running() == []), "the ended program was not reaped")
        # The channel takes a call again.
        self.assertFalse(self.detached("ok", calls).still_running)


def wait_until(pred):
    for _ in range(600):
        if pred():
            return True
        time.sleep(0.05)
    return False


def _state(pid):
    """The state letter of pid from /proc, or None when it is gone."""
    try:
        with open("/proc/%d/stat" % pid) as fh:
            return fh.read().rsplit(")", 1)[1].split()[0]
    except (OSError, IndexError):
        return None


def alive(pid):
    if os.path.isdir("/proc"):
        return _state(pid) not in (None, "Z", "X")
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def gone(pid):
    # A killed descendant is reaped by whoever adopted it: wait for it.
    for _ in range(100):
        if not alive(pid):
            return True
        time.sleep(0.05)
    return False


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
