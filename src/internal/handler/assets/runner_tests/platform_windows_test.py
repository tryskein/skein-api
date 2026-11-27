#!/usr/bin/env python3
"""The Windows platform layer of the runner and the loader on a fake source
of security descriptors (fake_secdesc.py): matrix rows #90 (criterion (a)
of P-9, the term "private Windows directory"), #91 (no untrusted text for a
.cmd/.bat program, the message path and directory, the Codex executable)
and #125 (a monitor without client on Windows keeps its behaviour).

The expected values come from the spec, not from the code. Criterion (a):
the DACL exists and is protected from inheritance (a directory); allow
entries name only the user, SYSTEM and the administrators; Creator Owner
only as inherit-only; any other allow entry, read-only too, refuses; a
reparse point refuses; a file inside the private directory is judged by its
own DACL from the handle it is read through. US-11: a .cmd/.bat program
gets no byte of untrusted text in its arguments; a message path for it is
of ASCII letters, digits and \\ : . _ - only; the message directory is the
first private ASCII candidate, and without one the channel is not used;
Codex is its native executable, else a text of checked ids only. INV-7:
without client, Windows keeps the previous behaviour, with no new privacy
refusal.

Windows runs nowhere here: the tests run the platform layer in process with
os.name reading "nt" and the fake in place of the system source; what only
Windows shows is the SC-007 smoke (T034).

Run on its own: python3 internal/handler/assets/runner_tests/platform_windows_test.py
"""

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

import fake_secdesc as fs  # noqa: E402
import harness  # noqa: E402

RUNNER = harness.load_runner()


def load_loader_client():
    ns = {"__name__": "loader_client_under_test"}
    exec(compile(harness._join_manifest(harness.LOADER_CLIENT_MANIFEST), "<skein-loader-client>", "exec"), ns)
    return ns


LOADER = load_loader_client()

ME = fs.ME
FOREIGN_READ = (fs.ALLOW, 0, fs.FILE_GENERIC_READ, fs.USERS)


def private_dir_view(extra=()):
    return fs.View(owner=ME, dacl=list(extra) + fs.private_dacl(), protected=True)


def private_file_view(extra=()):
    inherited = 0x10  # INHERITED_ACE: a file takes the entries of its directory
    return fs.View(owner=ME, dacl=list(extra) + [(fs.ALLOW, inherited, fs.FILE_ALL_ACCESS, ME),
                                                 (fs.ALLOW, inherited, fs.FILE_ALL_ACCESS, fs.SYSTEM),
                                                 (fs.ALLOW, inherited, fs.FILE_ALL_ACCESS, fs.ADMINS)])


class WindowsWorld(object):
    """A token directory with a token and starter.py on disk, the fake
    source answering for them, and os.name reading "nt" while active."""

    def __init__(self, test, dir_view=None, token_view=None, starter_view=None, fail_view=()):
        self.root = tempfile.mkdtemp(prefix="skein-win-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.token_dir = os.path.join(self.root, "skein-mon-sk-1-0011223344556677")
        os.mkdir(self.token_dir, 0o700)
        self.token = os.path.join(self.token_dir, "token")
        self.starter = os.path.join(self.token_dir, "starter.py")
        for path, text in ((self.token, "skm_" + "A" * 43), (self.starter, "print(1)\n")):
            with open(path, "w") as fh:
                fh.write(text)
        self.fake = fs.FakeSecurity(views={
            self.token_dir: dir_view or private_dir_view(),
            self.token: token_view or private_file_view(),
            self.starter: starter_view or private_file_view(),
        }, fail_view=fail_view)

    def active(self, ns):
        """Patches that make ns run its Windows layer on the fake."""
        stack = [mock.patch.object(os, "name", "nt"),
                 mock.patch.dict(ns, {"_WIN_SECURITY": [(self.fake, ME)]})]
        return _Stack(stack)

    def reads(self):
        """The handles opened to read data (a refused file is not read)."""
        return [h for h in self.fake.opened if h.path == self.token]


class _Stack(object):
    def __init__(self, patches):
        self.patches = patches

    def __enter__(self):
        for p in self.patches:
            p.start()
        return self

    def __exit__(self, *exc):
        for p in reversed(self.patches):
            p.stop()


class PrivateCriterionTest(unittest.TestCase):
    """Row #90: criterion (a) as a table, on a directory and on a file."""

    def test_refusals_and_passes(self):
        refuse = RUNNER.private_refusal
        io_flags = fs.OBJECT_INHERIT | fs.CONTAINER_INHERIT | fs.INHERIT_ONLY
        cases = [
            ("a read-only allow ACE of another account on the directory",
             private_dir_view([FOREIGN_READ]), True, "open_to_others"),
            ("a read-only allow ACE of another account on a file in the private directory",
             private_file_view([FOREIGN_READ]), False, "open_to_others"),
            ("a NULL DACL", fs.View(owner=ME, null_dacl=True, protected=True), True, "open_to_others"),
            ("a DACL not protected from inheritance", fs.View(owner=ME, dacl=fs.private_dacl()), True,
             "open_to_others"),
            ("a reparse point", fs.View(owner=ME, dacl=fs.private_dacl(), protected=True, reparse=True), True,
             "symlink"),
            ("Creator Owner applying to the directory itself",
             private_dir_view([(fs.ALLOW, 0, fs.FILE_ALL_ACCESS, fs.CREATOR_OWNER)]), True, "open_to_others"),
            ("Creator Owner inherit-only", private_dir_view([(fs.ALLOW, io_flags, fs.FILE_ALL_ACCESS,
                                                               fs.CREATOR_OWNER)]), True, None),
            ("the user, SYSTEM and the administrators", private_dir_view(), True, None),
            ("a file of the user, SYSTEM and the administrators", private_file_view(), False, None),
            ("a deny ACE of another account", private_dir_view([(fs.DENY, 0, fs.FILE_ALL_ACCESS, fs.USERS)]),
             True, None),
        ]
        for name, view, directory, want in cases:
            with self.subTest(name):
                self.assertEqual(refuse(view, ME, directory), want)

    def test_a_file_is_refused_before_it_is_read(self):
        cases = [
            ("a foreign read ACE on the token file", dict(token_view=private_file_view([FOREIGN_READ]))),
            ("a foreign read ACE on the token directory", dict(dir_view=private_dir_view([FOREIGN_READ]))),
            ("a token directory that inherits", dict(dir_view=fs.View(owner=ME, dacl=fs.private_dacl()))),
        ]
        for name, kwargs in cases:
            with self.subTest(name):
                world = WindowsWorld(self, **kwargs)
                with world.active(RUNNER.__dict__):
                    with self.assertRaises(RUNNER.PlatformRefusal) as ctx:
                        RUNNER.WindowsPlatform.read_token(world.token)
                self.assertEqual(ctx.exception.reason, "open_to_others")

    def test_an_unreadable_descriptor_of_the_open_file_refuses(self):
        world = WindowsWorld(self, fail_view=[os.path.join(tempfile.gettempdir(), "never")])
        world.fake.fail_view = {world.token}
        with world.active(RUNNER.__dict__):
            with self.assertRaises(RUNNER.PlatformRefusal) as ctx:
                RUNNER.WindowsPlatform.read_token(world.token)
        self.assertEqual(ctx.exception.reason, "unreadable")

    def test_the_loader_refuses_starter_and_token_directory_before_the_token(self):
        cases = [
            ("starter.py open to others", dict(starter_view=private_file_view([FOREIGN_READ]))),
            ("the token directory open to others", dict(dir_view=private_dir_view([FOREIGN_READ]))),
        ]
        for name, kwargs in cases:
            with self.subTest(name):
                world = WindowsWorld(self, **kwargs)
                with world.active(LOADER):
                    self.assertFalse(LOADER["loader_client_trusted"](world.token))
                # The decision came before any read of the token.
                self.assertEqual(world.reads(), [])

    def test_the_loader_reads_no_token_that_is_open_to_others(self):
        world = WindowsWorld(self, token_view=private_file_view([FOREIGN_READ]))
        with world.active(LOADER):
            with self.assertRaises(LOADER["Stop"]) as ctx:
                LOADER["loader_client_read_token"](world.token)
        self.assertEqual(ctx.exception.reason, "token_file_insecure")


class BatchProgramTest(unittest.TestCase):
    """Row #91: a .cmd/.bat program of Windows and the Codex executable."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-win-batch-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.calls = []
        # Absolute paths of this system: the executor keeps only absolute
        # program paths; the extension decides a batch program.
        self.paseo_cmd = os.path.join(self.root, "npm", "paseo.cmd")
        self.codex_cmd = os.path.join(self.root, "npm", "codex.cmd")
        self.codex_exe = os.path.join(self.root, "vendor", "codex.exe")

    def run_call(self, token_dir, feed, argv, text, env=None, programs=None):
        os.makedirs(token_dir, exist_ok=True)
        token = os.path.join(token_dir, "token")
        executor = RUNNER.ArgvExecutor(token, dict(env or {}), ["skm_secret"], programs)

        def run_program(argv, env, stdin_data, timeout_sec, secrets=()):
            self.calls.append(list(argv))
            return RUNNER.CallResult(0, b"", b"", False, False)

        fake = fs.FakeSecurity()
        with mock.patch.object(os, "name", "nt"), mock.patch.dict(RUNNER.__dict__, {
                "run_program": run_program, "_WIN_SECURITY": [(fake, ME)]}):
            return executor.call({"argv": argv, "feed": feed}, text, 5)

    def test_untrusted_text_never_reaches_the_arguments(self):
        text = 'preview: <<<SKEIN-DATA hi & del "x" %PATH% ^ | Иван SKEIN-DATA>>>'
        with self.assertRaises(RUNNER.ExecutorRefusal):
            self.run_call(os.path.join(self.root, "tok"), "arg", ["paseo", "send", "{text}"], text,
                          programs={"paseo": self.paseo_cmd})
        self.assertEqual(self.calls, [])

    def test_the_message_path_rule(self):
        ok = ["C:\\Users\\me\\AppData\\Local\\skein-monitor\\messages\\msg-0a1b2c",
              "D:\\a_b.c-d\\msg-1"]
        bad = ["C:\\Users\\Иван\\AppData\\Local\\msg-1", "C:\\Users\\O'Brien\\msg-1",
               "C:\\Program Files\\msg-1", "C:\\a%PATH%\\msg-1", "C:\\a&b\\msg-1"]
        for path in ok:
            self.assertTrue(RUNNER.BATCH_SAFE_PATH.match(path), path)
        for path in bad:
            self.assertFalse(RUNNER.BATCH_SAFE_PATH.match(path), path)

    def test_a_message_file_goes_to_the_first_private_ascii_directory(self):
        # The temporary directories here are POSIX paths: their separator
        # "/" stands for the "\\" of Windows, the rest of the rule is the
        # rule (test_the_message_path_rule).
        rule = RUNNER.re.compile(r"\A[A-Za-z0-9\\/:._-]+\Z")
        ascii_root = os.path.join(self.root, "local")
        os.mkdir(ascii_root)
        unicode_dir = os.path.join(self.root, "Иван", "tok")
        with mock.patch.dict(RUNNER.__dict__, {"BATCH_SAFE_PATH": rule}):
            self.run_call(unicode_dir, "file", ["paseo", "send", "--prompt-file", "{textFile}"], "body & more",
                          env={"LOCALAPPDATA": ascii_root}, programs={"paseo": self.paseo_cmd})
        self.assertEqual(len(self.calls), 1)
        path = self.calls[0][-1]
        self.assertEqual(os.path.dirname(path), os.path.join(ascii_root, "skein-monitor", "messages"))
        # The message file is gone after the call.
        self.assertFalse(os.path.exists(path))

    def test_without_an_ascii_private_directory_the_program_is_not_used(self):
        unicode_root = os.path.join(self.root, "Иван")
        os.mkdir(unicode_root)
        with self.assertRaises(RUNNER.ExecutorRefusal):
            self.run_call(os.path.join(unicode_root, "tok"), "file",
                          ["paseo", "send", "--prompt-file", "{textFile}"], "body",
                          env={"LOCALAPPDATA": unicode_root}, programs={"paseo": self.paseo_cmd})
        self.assertEqual(self.calls, [])

    def test_a_native_program_takes_the_full_text(self):
        text = 'preview: <<<SKEIN-DATA hi & "x" SKEIN-DATA>>>'
        self.run_call(os.path.join(self.root, "tok"), "arg", ["codex", "queue", "--message", "{text}"], text,
                      programs={"codex": self.codex_exe})
        self.assertEqual(self.calls, [[self.codex_exe, "queue", "--message", text]])

    def test_a_codex_shim_takes_the_id_only_text(self):
        channel = {"executor": {"feed": "arg", "argv": ["codex", "queue", "--message", "{text}"]}}
        with mock.patch.object(os, "name", "nt"):
            self.assertTrue(RUNNER.channel_takes_ids_only(channel, {"codex": self.codex_cmd}, {}))
            self.assertFalse(RUNNER.channel_takes_ids_only(channel, {"codex": self.codex_exe}, {}))
        note = {"id": 12, "type": "comment_added", "entity": {"type": "issue", "id": "sk-1-2"}, "commentId": "c5",
                "actor": {"username": "Иван & co"}, "bodyPreview": "secret body & del", "summary": "x"}
        text, count, _ = RUNNER.ids_only_batch([note])
        self.assertEqual(count, 1)
        for untrusted in ("Иван", "secret body", "&"):
            self.assertNotIn(untrusted, text)
        for value in ("comment_added", "12", "sk-1-2", "c5"):
            self.assertIn(value, text)
        # Through the shim the text passes the argument rule of cmd.exe.
        self.run_call(os.path.join(self.root, "tok"), "arg", ["codex", "queue", "--message", "{text}"], text,
                      programs={"codex": self.codex_cmd})
        self.assertEqual(len(self.calls), 1)

    def test_an_id_outside_its_pattern_has_no_id_only_form(self):
        note = {"id": 12, "type": "comment_added&calc", "entity": {"type": "issue", "id": "sk-1-2"}}
        self.assertEqual(RUNNER.ids_only_batch([note]), (None, 0, False))

    def test_the_agent_codex_executable_is_the_program(self):
        bin_dir = os.path.join(self.root, "vendor")
        os.mkdir(bin_dir)
        exe = os.path.join(bin_dir, "codex.exe")
        with open(exe, "w") as fh:
            fh.write("")
        os.chmod(exe, stat.S_IRWXU)

        class Agent(object):
            executable = exe

        with mock.patch.object(os, "name", "nt"):
            self.assertEqual(RUNNER.channel_program("codex", {"PATH": ""}, Agent()), exe)


class LegacyOnWindowsTest(unittest.TestCase):
    """Row #125: without client, Windows keeps the previous behaviour. A
    token the previous contract accepts (readable by others) is read, and
    the state is written, though the fake source would answer "another
    account" to every Windows check."""

    def test_the_legacy_token_and_state_take_no_windows_check(self):
        root = tempfile.mkdtemp(prefix="skein-win-legacy-")
        self.addCleanup(shutil.rmtree, root, True)
        token = os.path.join(root, "token")
        with open(token, "w") as fh:
            fh.write("skm_" + "B" * 43)
        os.chmod(token, 0o644)
        hostile = fs.View(owner=fs.OTHER, dacl=[FOREIGN_READ, (fs.ALLOW, 0, fs.GENERIC_ALL, fs.EVERYONE)])
        fake = fs.FakeSecurity(views={root: hostile, token: hostile})
        self.assertEqual(RUNNER.LOADER_MODE, "legacy")
        with mock.patch.object(os, "name", "nt"), mock.patch.dict(RUNNER.__dict__, {"_WIN_SECURITY": [(fake, ME)]}):
            fd, value = RUNNER.open_token(token)
            os.close(fd)
            RUNNER.save_cursor(token, {"ts": "t", "id": 3})
        self.assertEqual(value, "skm_" + "B" * 43)
        with open(RUNNER.state_file_path(token)) as fh:
            self.assertEqual(json.load(fh)["cursor"], {"ts": "t", "id": 3})
        # No Windows check ran: the fake was never asked.
        self.assertEqual(fake.opened, [])


if __name__ == "__main__":
    unittest.main()
