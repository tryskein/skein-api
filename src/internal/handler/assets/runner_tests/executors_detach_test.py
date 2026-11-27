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
if mode == "orphan":
    # The program ends at once and leaves a descendant of its tree running.
    child = __import__("subprocess").Popen([sys.executable, "-c", "import time; time.sleep(600)"])
    with open(os.environ["MARK"] + ".pids", "w") as fh:
        fh.write("%d %d" % (os.getpid(), child.pid))
    sys.exit(0)
if mode == "noisy":
    # More output than the cap, on both streams.
    size = int(os.environ["NOISE"])
    sys.stdout.write("o" * size)
    sys.stderr.write("e" * size)
    sys.exit(0)
if mode == "noisy-live":
    # More output than the cap on both streams while the call lives, then
    # it waits to be let go.
    size = int(os.environ["NOISE"])
    sys.stdout.write("o" * size)
    sys.stdout.flush()
    sys.stderr.write("e" * size)
    sys.stderr.flush()
    with open(os.environ["MARK"] + ".written", "w") as fh:
        fh.write(str(os.getpid()))
    while not os.path.exists(os.environ["MARK"] + ".go"):
        time.sleep(0.05)
    sys.exit(0)
if mode == "slow":
    while not os.path.exists(os.environ["MARK"] + ".go"):
        time.sleep(0.05)
    with open(os.environ["MARK"] + ".done", "w") as fh:
        fh.write("done")
sys.exit(0)
'''

# A detached call lives no longer than the call window of its record plus
# this, then its tree is killed (spec, INV-8), not from the runner.
DETACHED_GRACE_SPEC_SEC = 30

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
            # Just before the window plus the grace of INV-8 (review round 3
            # of wave 14, row 10): it runs on through two more looks of the
            # watcher.
            now[0] = 1000.0 + 0.2 + DETACHED_GRACE_SPEC_SEC - 0.001
            seen = looks[0]
            self.assertTrue(wait_until(lambda: looks[0] >= seen + 2), "the watcher does not look")
            self.assertTrue(alive(program) and alive(child))
            self.assertEqual(calls.running(), ["channel"])
            # At the deadline: the watcher kills the program and its
            # descendant.
            now[0] = 1000.0 + 0.2 + DETACHED_GRACE_SPEC_SEC
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
        # Review round 2 of wave 14, row 5, and round 3, row 1: the record of
        # a channel leaves the table only once its tree is killed, under the
        # lock a new call is admitted under. The second call is seen waiting
        # on that lock while the kill runs (an event, not a wait of a fixed
        # time).
        now = [1000.0]
        calls = RUNNER.DetachedCalls(clock=lambda: now[0])
        self.addCleanup(calls.end_all)
        contended = threading.Event()
        calls._lock = _SeenLock(calls._lock, contended)
        self.assertTrue(self.detached("tree", calls).still_running)
        program, child = self.pids()
        end_tree = RUNNER._end_tree
        killing, killed = threading.Event(), threading.Event()
        order = []

        def slow_end_tree(proc, tree):
            killing.set()
            killed.wait(30)
            failure = end_tree(proc, tree)
            order.append("old tree killed")
            return failure

        def second_call():
            try:
                calls.reserve("channel")
                order.append("second call admitted")
            except RUNNER.ExecutorRefusal:
                order.append("second call refused")
        with mock.patch.object(RUNNER, "_end_tree", slow_end_tree):
            now[0] = 1000.0 + 0.2 + DETACHED_GRACE_SPEC_SEC
            reaper = threading.Thread(target=calls.reap)
            reaper.start()
            self.assertTrue(killing.wait(30), "the deadline did not kill the tree")
            second = threading.Thread(target=second_call)
            second.start()
            waited = contended.wait(10)
            killed.set()
            reaper.join(30)
            second.join(30)
        self.assertTrue(waited, "the second call was not held while the old tree was killed")
        self.assertEqual(order, ["old tree killed", "second call admitted"])
        self.assertTrue(gone(program) and gone(child))

    def test_the_output_kept_of_a_call_is_capped(self):
        # Review round 2 of wave 14, row 16: the output of a detached call
        # is kept to the cap on both streams, the rest is read and dropped.
        self.env["NOISE"] = str(RUNNER.OUTPUT_CAP_BYTES + 4096)
        calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
        self.addCleanup(calls.end_all)
        result = RUNNER.run_detached([sys.executable, self.script, "noisy"], self.env, b"", 30, key="channel",
                                     calls=calls)
        self.assertEqual(result.exit_code, 0)
        self.assertEqual((len(result.stdout), result.truncated), (RUNNER.OUTPUT_CAP_BYTES, True))
        self.assertEqual((len(result.stderr), result.stderr_truncated), (RUNNER.OUTPUT_CAP_BYTES, True))
        self.assertIsNone(result.json)

    def test_the_output_of_a_live_call_goes_to_no_file(self):
        # Review round 3 of wave 14, row 12: while a call left to run lives,
        # its output goes through pipes read to their end - no file on disk
        # holds it - so a program that writes past the cap on both streams
        # is never held on a full pipe either.
        if not os.path.isdir("/proc"):
            self.skipTest("the descriptors of a process are read from /proc")
        self.env["NOISE"] = str(4 * RUNNER.OUTPUT_CAP_BYTES)
        calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
        self.addCleanup(calls.end_all)
        result = RUNNER.run_detached([sys.executable, self.script, "noisy-live"], self.env, b"", 0.2,
                                     key="channel", calls=calls)
        self.assertTrue(result.still_running)
        self.assertTrue(self.wait_for(self.mark + ".written"), "the live call was held writing its output")
        with open(self.mark + ".written") as fh:
            pid = int(fh.read())
        for fd in (1, 2):
            target = os.readlink("/proc/%d/fd/%d" % (pid, fd))
            self.assertTrue(target.startswith("pipe:"), "fd %d of the live call is %s" % (fd, target))

    def failing_kill(self):
        """The tree kill of the platform fails and kills nothing, as a job
        object call of Windows that failed may, until self.kill_works is
        set; then it is the platform's own."""
        self.kill_works = False
        real = RUNNER.PosixPlatform.tree_kill

        def tree_kill(proc, job):
            if self.kill_works:
                return real(proc, job)
            return FAILURE
        return mock.patch.object(RUNNER.PosixPlatform, "tree_kill", staticmethod(tree_kill))

    def test_a_kill_that_cannot_end_the_tree_keeps_its_channel(self):
        # Review round 2 of wave 14, row 3, and round 3, row 2: a failed
        # kill is in the outcome (unknown with the sign treeKill), and the
        # call keeps its channel while its tree may live: a second call is
        # refused before send; the watcher kills the tree again and frees
        # the channel once the kill works.
        with self.failing_kill(), mock.patch.object(RUNNER, "_DETACHED_WATCH_STEP_SEC", 0.02):
            calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
            self.addCleanup(calls.end_all)
            early = self.detached("orphan", calls)
            self.assertEqual(early.exit_code, 0)
            self.assertEqual(RUNNER.classify_result(ROWS, early), ("unknown", {"treeKill": FAILURE}))
            program, child = self.pids()
            self.assertTrue(alive(child))
            self.assertEqual(calls.running(), ["channel"])
            with self.assertRaises(RUNNER.ExecutorRefusal):
                self.detached("ok", calls)
            self.kill_works = True
            self.assertTrue(wait_until(lambda: calls.running() == []), "the watcher did not kill the tree again")
            self.assertTrue(gone(child), "the tree outlived its kill")
            self.assertEqual(self.detached("ok", calls).exit_code, 0)

    def test_a_closing_monitor_keeps_a_tree_it_could_not_kill(self):
        with self.failing_kill():
            calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
            calls.closing = True
            killed = self.detached("tree", calls)
            self.assertTrue(killed.still_running)
            self.assertEqual(RUNNER.classify_result(ROWS, killed), ("unknown", {"treeKill": FAILURE}))
            program, child = self.pids()
            self.assertEqual(calls.running(), ["channel"])
            self.kill_works = True
            calls.close()
            self.assertEqual(calls.running(), [])
            self.assertTrue(gone(program) and gone(child))

    def test_a_failed_kill_at_the_stop_is_told(self):
        # The call was given as unknown at the end of its window: a failed
        # kill at the stop is told to the listener of the calls.
        with self.failing_kill():
            told = []
            calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
            calls.on_failure = lambda key, failure: told.append((key, failure))
            self.assertTrue(self.detached("tree", calls).still_running)
            program, child = self.pids()
            self.assertEqual(told, [])
            calls.close()
            self.assertEqual(told, [("channel", FAILURE)])
            self.kill_works = True
            calls.close()
            self.assertEqual(told, [("channel", FAILURE)])
            self.assertTrue(gone(program) and gone(child))

    def test_a_stop_signal_during_the_call_loses_no_failed_kill(self):
        # Review round 3 of wave 14, row 4: a stop signal before the call is
        # left to run kills the tree; a kill that failed is told and the
        # call keeps its channel, so the stop of the monitor kills it again.
        def stop(*args):
            raise RUNNER.StopSignal()
        with self.failing_kill():
            told = []
            calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
            calls.on_failure = lambda key, failure: told.append((key, failure))
            with mock.patch.object(RUNNER, "_wait_unreaped", stop):
                with self.assertRaises(RUNNER.StopSignal):
                    self.detached("tree", calls)
            program, child = self.pids()
            self.assertEqual(told, [("channel", FAILURE)])
            self.assertEqual(calls.running(), ["channel"])
            self.kill_works = True
            calls.close()
            self.assertEqual(calls.running(), [])
            self.assertTrue(gone(program) and gone(child))

    def test_a_stop_signal_while_the_readers_start_stays_a_stop_signal(self):
        # Review round 3 of wave 14, row 9: a stop signal between the starts
        # of the two readers reaches the monitor as itself, and nothing of
        # the call is left.
        start = RUNNER._Reader.start
        starts = [0]

        def second_start_stops(reader):
            starts[0] += 1
            if starts[0] == 2:
                raise RUNNER.StopSignal()
            return start(reader)
        calls = RUNNER.DetachedCalls(clock=lambda: 0.0)
        with mock.patch.object(RUNNER._Reader, "start", second_start_stops):
            with self.assertRaises(RUNNER.StopSignal):
                self.detached("tree", calls)
        self.assertEqual(calls.running(), [])

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


class _SeenLock(object):
    """A lock that sets seen when an acquire has to wait for it."""

    def __init__(self, lock, seen):
        self.lock = lock
        self.seen = seen

    def acquire(self, blocking=True, timeout=-1):
        if self.lock.acquire(False):
            return True
        if not blocking:
            return False
        self.seen.set()
        return self.lock.acquire(True, timeout)

    def release(self):
        self.lock.release()

    def __enter__(self):
        self.acquire()
        return self

    def __exit__(self, *exc):
        self.release()


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
