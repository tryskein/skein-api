#!/usr/bin/env python3
"""Tests of the POSIX platform layer of the monitor runner
(runner/platform.py, runner/platform_posix.py).

  - What is checked is what is read: a token or own file is opened once,
    checked on that open descriptor and read through it; a file others may
    read, a file of another user and a symbolic link are refused before any
    read. Observed in a subprocess through the I/O counter of
    testdata/runner_site/sitecustomize.py (SKEIN_TEST_IO_LOG).
  - Atomic write, and the one-runner lock on its stable file
    skein-monitor/locks/<key>.lock, in process.

Run on its own: python3 internal/handler/assets/runner_tests/platform_posix_test.py
Standard library of Python 3.6 only.
"""

import hashlib
import io
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

RUNNER = harness.load_runner()
PLATFORM = RUNNER.PosixPlatform

# Runs one platform-layer read in a fresh interpreter with the test hooks on:
# argv = [operation, path]; prints {"data": ...} or {"refused": reason}.
_DRIVER = r"""
import json, sys
sys.path.insert(0, sys.argv[1])
import harness
runner = harness.load_runner()
try:
    data = getattr(runner.PosixPlatform, sys.argv[2])(sys.argv[3])
    print(json.dumps({"data": data.decode("utf-8")}))
except runner.PlatformRefusal as exc:
    print(json.dumps({"refused": exc.reason}))
"""


def _write(path, text, mode):
    with open(path, "w") as fh:
        fh.write(text)
    os.chmod(path, mode)


# The I/O counter of sitecustomize.py names an open relative to a directory
# descriptor through /proc/self/fd, which only Linux has.
@unittest.skipUnless(sys.platform.startswith("linux"),
                     "the I/O counter resolves directory-relative opens through /proc/self/fd")
@harness.subprocess_runner
class CheckedReadTest(unittest.TestCase):
    """Matrix row #22: what is checked is what is read; another user's or an
    open file is not read."""

    def setUp(self):
        self.root = os.path.realpath(tempfile.mkdtemp(prefix="skein-plat-"))
        self.addCleanup(shutil.rmtree, self.root, True)
        self.log = os.path.join(self.root, "io.log")
        self.files = os.path.join(self.root, "files")
        os.mkdir(self.files)

    def directory(self, mode):
        path = tempfile.mkdtemp(dir=self.files)
        os.chmod(path, mode)
        return path

    def read(self, operation, path, foreign=None):
        """Run the operation on path in a subprocess; returns (result, I/O
        log lines)."""
        env = dict(os.environ, PYTHONPATH=os.path.abspath(harness.SITE_DIR), SKEIN_TEST_SITE="1",
                   SKEIN_TEST_IO_LOG=self.log, SKEIN_TEST_IO_WATCH=self.files)
        if foreign:
            env["SKEIN_TEST_FOREIGN_INODE"] = foreign
        if os.path.exists(self.log):
            os.remove(self.log)
        proc = subprocess.run([sys.executable, "-c", _DRIVER, HERE, operation, path],
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, timeout=60)
        self.assertEqual(proc.returncode, 0, proc.stderr.decode("utf-8", "replace"))
        lines = []
        if os.path.exists(self.log):
            with open(self.log) as fh:
                lines = [line.split() for line in fh.read().splitlines()]
        return json.loads(proc.stdout.decode("utf-8")), lines

    def opens_of(self, lines, path):
        return [line for line in lines if line[0] == "open" and line[1] == path]

    def assert_read_after_check(self, lines, path):
        """The file was opened by path exactly once and every read went
        through that descriptor after its fstat; no open by path followed."""
        opens = self.opens_of(lines, path)
        self.assertEqual(len(opens), 1, lines)
        fd = opens[0][2]
        events = [line for line in lines if line[0] in ("fstat", "read", "close") and line[1] == fd]
        reads = [i for i, line in enumerate(events) if line[0] == "read"]
        self.assertTrue(reads, "the file was not read: %s" % lines)
        first_fstat = [i for i, line in enumerate(events) if line[0] == "fstat"]
        self.assertTrue(first_fstat and first_fstat[0] < reads[0], lines)
        self.assertEqual([line for line in lines if line[0] == "read" and line[1] != fd], [], lines)

    def assert_not_read(self, lines):
        self.assertEqual([line for line in lines if line[0] == "read"], [], lines)

    def test_private_and_write_tool_tokens_are_read(self):
        cases = (
            ("0600 token in an open directory", 0o600, 0o755),
            ("0644 token in the user's 0700 directory", 0o644, 0o700),
        )
        for name, file_mode, dir_mode in cases:
            with self.subTest(case=name):
                token = os.path.join(self.directory(dir_mode), "token")
                _write(token, "skm_secret\n", file_mode)
                result, lines = self.read("read_token", token)
                self.assertEqual(result, {"data": "skm_secret\n"})
                self.assert_read_after_check(lines, token)

    def test_open_or_foreign_files_are_refused_before_any_read(self):
        # (case, operation, file mode, directory mode, foreign: "file" | "dir" | None, reason)
        cases = (
            ("0644 token in a directory with group/other bits", "read_token", 0o644, 0o750, None, "open_to_others"),
            ("0644 token in another user's 0700 directory", "read_token", 0o644, 0o700, "dir", "open_to_others"),
            ("token of another user", "read_token", 0o600, 0o700, "file", "foreign_owner"),
            ("own state file with group/other bits", "read_own_file", 0o640, 0o700, None, "open_to_others"),
            ("own file of another user", "read_own_file", 0o600, 0o700, "file", "foreign_owner"),
        )
        for name, operation, file_mode, dir_mode, foreign, reason in cases:
            with self.subTest(case=name):
                directory = self.directory(dir_mode)
                path = os.path.join(directory, "file")
                _write(path, "secret\n", file_mode)
                result, lines = self.read(operation, path,
                                          foreign={"file": path, "dir": directory}.get(foreign))
                self.assertEqual(result, {"refused": reason})
                self.assertEqual(len(self.opens_of(lines, path)), 1, lines)
                self.assert_not_read(lines)

    def test_own_file_needs_a_private_directory(self):
        for name, dir_mode, expected in (("0600 in a 0700 directory", 0o700, {"data": "state\n"}),
                                         ("0600 in a 0755 directory", 0o755, {"refused": "open_to_others"})):
            with self.subTest(case=name):
                path = os.path.join(self.directory(dir_mode), "state.json")
                _write(path, "state\n", 0o600)
                result, lines = self.read("read_own_file", path)
                self.assertEqual(result, expected)
                if "data" in expected:
                    self.assert_read_after_check(lines, path)
                else:
                    self.assert_not_read(lines)

    def test_symlink_is_not_followed(self):
        directory = self.directory(0o700)
        target = os.path.join(directory, "real-token")
        _write(target, "skm_secret\n", 0o600)
        link = os.path.join(directory, "token")
        os.symlink(target, link)
        for operation in ("read_token", "read_own_file"):
            with self.subTest(operation=operation):
                result, lines = self.read(operation, link)
                self.assertEqual(result, {"refused": "symlink"})
                self.assertEqual(self.opens_of(lines, target), [], lines)
                self.assert_not_read(lines)


@unittest.skipUnless(os.name == "posix", "the POSIX platform layer")
class AtomicWriteTest(unittest.TestCase):
    """Matrix row #23: a write cut before os.replace leaves no partial file."""

    def test_write_cut_before_replace_keeps_the_old_file(self):
        directory = tempfile.mkdtemp(prefix="skein-plat-")
        self.addCleanup(shutil.rmtree, directory, True)
        path = os.path.join(directory, "state.json")
        PLATFORM.atomic_write(path, b'{"v":1}')
        with mock.patch.object(RUNNER.os, "replace", side_effect=OSError("cut")):
            with self.assertRaises(OSError):
                PLATFORM.atomic_write(path, b'{"v":2,"long":"' + b"x" * 100000 + b'"}')
        with open(path, "rb") as fh:
            self.assertEqual(fh.read(), b'{"v":1}')
        self.assertEqual(os.listdir(directory), ["state.json"])

    def test_write_replaces_with_a_private_file(self):
        directory = tempfile.mkdtemp(prefix="skein-plat-")
        self.addCleanup(shutil.rmtree, directory, True)
        path = os.path.join(directory, "state.json")
        PLATFORM.atomic_write(path, b"old")
        PLATFORM.atomic_write(path, b"new")
        with open(path, "rb") as fh:
            self.assertEqual(fh.read(), b"new")
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
        self.assertEqual(os.listdir(directory), ["state.json"])


class _FakeClock(object):
    """time.monotonic and time.sleep of the runner: sleep advances the clock;
    on_sleep runs after each sleep."""

    def __init__(self, on_sleep=None):
        self.now = 1000.0
        self.slept = 0.0
        self.on_sleep = on_sleep

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds
        self.slept += seconds
        if self.on_sleep:
            self.on_sleep()

    def patch(self):
        return mock.patch.multiple(RUNNER.time, monotonic=self.monotonic, sleep=self.sleep)


@unittest.skipUnless(os.name == "posix", "the POSIX platform layer")
class RunnerLockTest(unittest.TestCase):
    """Matrix row #23: the one-runner lock lives on a stable file outside the
    token directory, keyed by the token path."""

    def setUp(self):
        self.root = os.path.realpath(tempfile.mkdtemp(prefix="skein-plat-"))
        self.addCleanup(shutil.rmtree, self.root, True)
        self.env = {"XDG_STATE_HOME": os.path.join(self.root, "state"), "HOME": self.root}
        self.token_dir = os.path.join(self.root, "skein-mon-sk-1-0123")
        os.mkdir(self.token_dir, 0o700)
        self.token = os.path.join(self.token_dir, "token")
        _write(self.token, "skm_secret\n", 0o644)

    def acquire(self, token=None, wait_sec=0):
        lock = PLATFORM.acquire_runner_lock(token or self.token, wait_sec=wait_sec, env=self.env)
        self.addCleanup(lock.release)
        return lock

    def test_lock_file_is_stable_and_outside_the_token_directory(self):
        key = hashlib.sha256(self.token.encode("utf-8")).hexdigest()
        expected = os.path.join(self.root, "state", "skein-monitor", "locks", key + ".lock")
        lock = self.acquire()
        self.assertEqual(lock.path, expected)
        shutil.rmtree(self.token_dir)
        self.assertTrue(os.path.isfile(expected))
        with self.assertRaises(RUNNER.LockBusy):
            PLATFORM.acquire_runner_lock(self.token, env=self.env)
        lock.release()
        self.assertTrue(os.path.isfile(expected))
        self.acquire()

    def test_second_non_blocking_acquire_is_refused(self):
        self.acquire()
        with self.assertRaises(RUNNER.LockBusy):
            PLATFORM.acquire_runner_lock(self.token, env=self.env)

    def test_waiting_acquire_gives_up_after_the_wait(self):
        self.acquire()
        clock = _FakeClock()
        with clock.patch():
            with self.assertRaises(RUNNER.LockBusy):
                PLATFORM.acquire_runner_lock(self.token, wait_sec=3, env=self.env)
        # It waited the whole bound and no longer.
        self.assertAlmostEqual(clock.slept, 3, places=6)

    def test_waiting_acquire_takes_the_lock_once_released(self):
        holder = self.acquire()
        clock = _FakeClock(on_sleep=holder.release)
        with clock.patch():
            lock = self.acquire(wait_sec=3)
        self.assertGreater(clock.slept, 0)
        self.assertLess(clock.slept, 3)
        self.assertEqual(lock.path, holder.path)

    def renewing_flock(self, times):
        """fcntl.flock that, on its first `times` exclusive calls, replaces
        the lock file by a new one before locking the descriptor it got:
        the lock then holds a file the path no longer names."""
        real = RUNNER.fcntl.flock
        calls = [0]

        def flock(fd, op):
            if op & RUNNER.fcntl.LOCK_EX and calls[0] < times:
                calls[0] += 1
                path = os.readlink("/proc/self/fd/%d" % fd) if os.path.exists("/proc/self/fd") else None
                if path is None:
                    raise unittest.SkipTest("needs /proc/self/fd")
                os.remove(path)
                fd2 = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
                os.close(fd2)
            return real(fd, op)
        return mock.patch.object(RUNNER.fcntl, "flock", flock)

    def test_lock_on_a_renewed_file_is_taken_again(self):
        with self.renewing_flock(times=1):
            lock = self.acquire()
        held = os.fstat(lock._fd)
        now = os.lstat(lock.path)
        self.assertEqual((held.st_dev, held.st_ino), (now.st_dev, now.st_ino))
        # Another taker of the same path finds it locked.
        with self.assertRaises(RUNNER.LockBusy):
            PLATFORM.acquire_runner_lock(self.token, env=self.env)

    def test_lock_file_renewed_without_end_gives_up(self):
        with self.renewing_flock(times=1000):
            with self.assertRaises(RUNNER.LockBusy):
                PLATFORM.acquire_runner_lock(self.token, env=self.env)

    def test_two_tokens_do_not_share_a_lock(self):
        other_dir = os.path.join(self.root, "skein-mon-sk-2-4567")
        os.mkdir(other_dir, 0o700)
        other = os.path.join(other_dir, "token")
        _write(other, "skm_other\n", 0o644)
        first = self.acquire()
        second = self.acquire(token=other)
        self.assertNotEqual(first.path, second.path)
        self.assertEqual(os.path.dirname(first.path), os.path.dirname(second.path))


@unittest.skipUnless(sys.platform.startswith("linux"), "reads /proc of Linux")
class ProcessTest(unittest.TestCase):
    """The process facts and the detached start of the platform layer."""

    # /proc/<pid>/stat of a sleeping process: ppid 1, pgrp 77, tpgid 77,
    # starttime 4242 (field 22).
    STAT = b"123 (my (odd) name) S 1 77 77 34816 77 4194560 1 0 0 0 0 0 0 0 20 0 1 0 4242 0 0"

    def facts(self, stat_bytes=None, error=None):
        def fake_open(path, mode="r", *args, **kwargs):
            if error is not None:
                raise error
            if path.endswith("/stat"):
                return io.BytesIO(stat_bytes)
            return io.StringIO("boot-1\n")
        with mock.patch.object(RUNNER, "open", fake_open, create=True):
            return PLATFORM.process_info(123)

    def test_facts_of_a_live_process(self):
        info = self.facts(self.STAT)
        self.assertEqual((info.ppid, info.pgid, info.tpgid, info.start, info.name),
                         (1, 77, 77, "boot-1:4242", "my (odd) name"))
        self.assertTrue(info.foreground())

    def test_unknown_process_is_none_not_an_error(self):
        for case, kwargs in (("short stat", {"stat_bytes": b"123 (x) S 1"}),
                             ("garbled stat", {"stat_bytes": b"123 (x) S one 77 77 1 77 x"}),
                             ("zombie", {"stat_bytes": self.STAT.replace(b") S ", b") Z ")}),
                             ("hidden (hidepid)", {"error": PermissionError(13, "denied")}),
                             ("gone", {"error": FileNotFoundError(2, "gone")})):
            with self.subTest(case=case):
                self.assertIsNone(self.facts(**kwargs))

    def spawn_detached(self, *args):
        """PLATFORM.spawn_detached, whose child, if it still runs at the end
        of the test, is killed with its group and reaped then, on every way
        out of the test. A child already reaped by the platform is left
        alone: its pid may name another process by then."""
        started = []
        self.addCleanup(end_unreaped, started)
        popen = subprocess.Popen

        def recording(*popen_args, **popen_kwargs):
            proc = popen(*popen_args, **popen_kwargs)
            started.append(proc)
            return proc
        with mock.patch.object(RUNNER.subprocess, "Popen", recording):
            return PLATFORM.spawn_detached(*args)

    def test_detached_child_is_reaped(self):
        pid, receipt = self.spawn_detached(
            [sys.executable, "-c", "import sys; sys.stdin.read(); print('ready')"],
            b"body", dict(os.environ), 30)
        self.assertEqual(receipt, "ready")
        deadline = time.monotonic() + 10
        while os.path.exists("/proc/%d" % pid) and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertFalse(os.path.exists("/proc/%d" % pid), "the ended child stayed a zombie")

    def test_the_stdin_of_a_child_stays_open_until_its_receipt_is_dealt_with(self):
        # The child prints its receipt, then reads its stdin to the end.
        child = ("import sys; sys.stdin.buffer.readline(); print('ready', flush=True); "
                 "sys.stdin.buffer.read(); print('went on', flush=True)")
        seen = []

        def on_receipt(receipt):
            seen.append(receipt)
            return True

        pid, receipt = self.spawn_detached([sys.executable, "-c", child], b"body\n",
                                           dict(os.environ), 30, on_receipt)
        self.assertEqual((receipt, seen), ("ready", ["ready"]))
        deadline = time.monotonic() + 10
        while os.path.exists("/proc/%d" % pid) and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertFalse(os.path.exists("/proc/%d" % pid), "the child did not go on after its stdin closed")

    def test_a_receipt_turned_down_kills_the_child(self):
        child = "import sys, time; print('odd', flush=True); time.sleep(60)"
        pid, receipt = self.spawn_detached([sys.executable, "-c", child], b"",
                                           dict(os.environ), 30, lambda r: False)
        self.assertEqual(receipt, "odd")
        deadline = time.monotonic() + 10
        while os.path.exists("/proc/%d" % pid) and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertFalse(os.path.exists("/proc/%d" % pid), "a turned-down child is still running")


def end_unreaped(procs):
    """Kill the group of each process of procs not reaped yet, and reap it."""
    for proc in procs:
        if proc.poll() is not None:
            continue
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except OSError:
            pass
        proc.wait()


class ControllingTerminalTest(unittest.TestCase):
    """The terminal of a process as each platform prints it (P-8), from
    lines in the form of the source of the platform, not a stand-in tty.

    macOS: Apple ps prints tdev as "major/minor" of the device and "??"
    without one (adv_cmds ps/print.c#tdev); the device number is
    major << 24 | minor (sys/types.h#makedev). Linux: field 7 of
    /proc/<pid>/stat is tty_nr, 0 without a terminal (proc(5))."""

    def test_apple_ps_lines(self):
        for line, tty in (
                ("  812  812  930   16/3 Wed Nov 26 11:00:00 2026 /bin/zsh\n", (16 << 24) | 3),
                ("  930  930  930   16/12 Wed Nov 26 11:02:41 2026 /usr/local/bin/claude\n", (16 << 24) | 12),
                ("    1    1    0     ?? Mon Nov 24 08:00:00 2026 /sbin/launchd\n", None)):
            with self.subTest(line=line):
                info = RUNNER._parse_darwin_ps(4242, line)
                self.assertIsNotNone(info)
                self.assertEqual(info.tty, tty)
        # Two processes on one terminal compare equal; another terminal not.
        a = RUNNER._parse_darwin_ps(1, "  1  1  1   16/3 Wed Nov 26 11:00:00 2026 /bin/zsh")
        b = RUNNER._parse_darwin_ps(2, "  1  2  1   16/3 Wed Nov 26 11:00:01 2026 claude")
        c = RUNNER._parse_darwin_ps(3, "  1  3  3   16/4 Wed Nov 26 11:00:02 2026 claude")
        self.assertEqual(a.tty, b.tty)
        self.assertNotEqual(a.tty, c.tty)

    def test_linux_stat_lines(self):
        # pts/1: major 136, minor 1 -> tty_nr 136 << 8 | 1.
        on_pts = ("4242 (claude code) S 4100 4242 4100 34817 4242 4194560 1 0 0 0 5 2 0 0 20 0 9 0 "
                  "123456 0 0 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0")
        no_tty = ("17 (kworker) S 2 0 0 0 -1 69238880 0 0 0 0 0 0 0 0 20 0 1 0 "
                  "50 0 0 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0")
        self.assertEqual(RUNNER._parse_linux_stat(4242, on_pts, "boot").tty, (136 << 8) | 1)
        self.assertIsNone(RUNNER._parse_linux_stat(17, no_tty, "boot").tty)


if __name__ == "__main__":
    unittest.main()
