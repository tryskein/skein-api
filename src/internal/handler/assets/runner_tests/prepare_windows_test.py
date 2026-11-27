#!/usr/bin/env python3
"""The prepare mode of the loader on Windows (R-10, US-11 "Before the
token is written"; P-9 (b)) on a fake source of security descriptors
(fake_secdesc.py): matrix rows #89 (the predicate of the path chain, as a
table) and #128 (the outcome of prepare over the open handles of the
chain). The Windows `prepare` of the response checks the Python version
and runs the starter in prepare mode; the loader does the rest.

The expected values come from the spec, not from the code: a component
from the root of the drive down to the token directory may be replaced by
another account when that account has DELETE, WRITE_DAC or WRITE_OWNER on
it or FILE_DELETE_CHILD on its parent, effective by the order of the DACL
with GENERIC_* mapped to the rights of files; inherit-only entries do not
count, a NULL DACL refuses, an empty DACL does not; the owner is the user,
SYSTEM, the administrators or TrustedInstaller; a reparse point and a final
path other than the requested one refuse. The ready line comes only after
the whole chain is confirmed, while its handles are still open, and
starter.py is written only then, byte for byte from the text prepare hands
over. A refusal of the starter in prepare mode is a prepare_failed line.

The client branch of the loader is loaded in process from its manifest
(the parts the route serves) and given the fake in place of the system
source.

Run on its own: python3 internal/handler/assets/runner_tests/prepare_windows_test.py
"""

import io
import json
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_secdesc as fs  # noqa: E402
import harness  # noqa: E402

PREPARE_WINDOWS_SRC = os.path.join(os.path.dirname(harness.LOADER_SRC), "monitor_prepare_windows.py")


def load_loader_client():
    """The namespace of the client branch of the loader, its functions
    defined and nothing run."""
    ns = {"__name__": "loader_client_under_test"}
    exec(compile(harness._join_manifest(harness.LOADER_CLIENT_MANIFEST), "<skein-loader-client>", "exec"), ns)
    return ns


LOADER = load_loader_client()
STARTER = harness.starter(client=True)


ROOT = "C:\\"
USERS_DIR = "C:\\Users"
PROFILE = "C:\\Users\\me"
LOCAL = "C:\\Users\\me\\AppData\\Local"
TOKEN_DIR = LOCAL + "\\skein-mon-sk-1-0011223344556677"
CHAIN = [ROOT, USERS_DIR, PROFILE, "C:\\Users\\me\\AppData", LOCAL, TOKEN_DIR]


def standard_views():
    """The rights a standard profile has, as the spec describes them: the
    root owned by TrustedInstaller, letting authenticated users create
    children and holding inherit-only entries for the creator owner; the
    profile owned by the user."""
    io_flags = fs.OBJECT_INHERIT | fs.CONTAINER_INHERIT | fs.INHERIT_ONLY
    root = fs.View(owner=fs.TRUSTED_INSTALLER, dacl=[
        (fs.ALLOW, 0, fs.FILE_ALL_ACCESS, fs.SYSTEM),
        (fs.ALLOW, 0, fs.FILE_ALL_ACCESS, fs.ADMINS),
        (fs.ALLOW, 0, fs.FILE_GENERIC_READ, fs.USERS),
        (fs.ALLOW, 0, fs.FILE_ADD_SUBDIRECTORY, fs.AUTHENTICATED),
        (fs.ALLOW, io_flags, fs.GENERIC_ALL, fs.CREATOR_OWNER),
        (fs.ALLOW, io_flags, fs.DELETE | fs.GENERIC_WRITE, fs.AUTHENTICATED)])
    users = fs.View(owner=fs.SYSTEM, dacl=[
        (fs.ALLOW, 0, fs.FILE_ALL_ACCESS, fs.SYSTEM), (fs.ALLOW, 0, fs.FILE_ALL_ACCESS, fs.ADMINS),
        (fs.ALLOW, 0, fs.FILE_GENERIC_READ, fs.USERS), (fs.ALLOW, 0, fs.FILE_GENERIC_READ, fs.EVERYONE)])
    profile = fs.View(owner=fs.ME, dacl=fs.trusted_dacl())
    return {ROOT: root, USERS_DIR: users, PROFILE: profile}


def chain_views(overrides=None):
    views = standard_views()
    views.update(overrides or {})
    out = []
    for path in CHAIN:
        view = views.get(path) or fs.View(owner=fs.ME)
        if view.final is None:
            view.final = path
        out.append((path, view))
    return out


def refusal(overrides=None):
    return LOADER["chain_refusal"](chain_views(overrides), fs.ME)


def with_ace(ace, owner=fs.ME, before=(), after=()):
    return fs.View(owner=owner, dacl=list(before) + [ace] + list(after) + fs.trusted_dacl())


class ChainPredicateTest(unittest.TestCase):
    """Row #89: the predicate of criterion (b) as a table."""

    def test_the_standard_profile_passes(self):
        self.assertIsNone(refusal())

    def test_refusals(self):
        allow = lambda mask, sid=fs.OTHER: (fs.ALLOW, 0, mask, sid)  # noqa: E731
        cases = [
            ("an untrusted owner", {PROFILE: fs.View(owner=fs.OTHER)}),
            ("DELETE of another account", {LOCAL: with_ace(allow(fs.DELETE))}),
            ("DELETE of a group of users", {LOCAL: with_ace(allow(fs.DELETE, fs.USERS))}),
            ("DELETE of everyone", {PROFILE: with_ace(allow(fs.DELETE, fs.EVERYONE))}),
            ("FILE_DELETE_CHILD on the parent of tokenDir",
             {"C:\\Users\\me\\AppData": with_ace(allow(fs.FILE_DELETE_CHILD))}),
            ("FILE_DELETE_CHILD on tokenDir, the parent of the token directory",
             {LOCAL: with_ace(allow(fs.FILE_DELETE_CHILD))}),
            ("WRITE_DAC", {USERS_DIR: with_ace(allow(fs.WRITE_DAC), owner=fs.SYSTEM)}),
            ("WRITE_OWNER", {USERS_DIR: with_ace(allow(fs.WRITE_OWNER), owner=fs.SYSTEM)}),
            ("GENERIC_ALL", {LOCAL: with_ace(allow(fs.GENERIC_ALL))}),
            ("a NULL DACL on an ancestor", {USERS_DIR: fs.View(owner=fs.SYSTEM, null_dacl=True)}),
            ("a deny after the allow of the same right",
             {LOCAL: with_ace(allow(fs.DELETE), after=[(fs.DENY, 0, fs.DELETE, fs.OTHER)])}),
            ("a reparse point", {"C:\\Users\\me\\AppData": fs.View(owner=fs.ME, reparse=True)}),
            ("a final path other than the requested one",
             {LOCAL: fs.View(owner=fs.ME, final="C:\\Users\\other\\AppData\\Local")}),
            ("a replacing right on the token directory itself",
             {TOKEN_DIR: with_ace(allow(fs.DELETE))}),
        ]
        for name, overrides in cases:
            with self.subTest(name):
                self.assertIsNotNone(refusal(overrides), name)

    def test_not_refusals(self):
        io_flags = fs.OBJECT_INHERIT | fs.CONTAINER_INHERIT | fs.INHERIT_ONLY
        cases = [
            ("the same rights inherit-only",
             {LOCAL: with_ace((fs.ALLOW, io_flags, fs.DELETE | fs.WRITE_DAC | fs.WRITE_OWNER
                               | fs.FILE_DELETE_CHILD | fs.GENERIC_ALL, fs.OTHER))}),
            ("a deny before the allow of the same right",
             {LOCAL: with_ace((fs.ALLOW, 0, fs.DELETE, fs.OTHER), before=[(fs.DENY, 0, fs.DELETE, fs.OTHER)])}),
            ("a deny of everyone before the allow of a group",
             {LOCAL: with_ace((fs.ALLOW, 0, fs.DELETE, fs.USERS), before=[(fs.DENY, 0, fs.DELETE, fs.EVERYONE)])}),
            ("an empty DACL on an ancestor", {USERS_DIR: fs.View(owner=fs.SYSTEM, dacl=[])}),
            ("FILE_ADD_* of other accounts in the ancestors",
             {USERS_DIR: with_ace((fs.ALLOW, 0, fs.FILE_ADD_FILE | fs.FILE_ADD_SUBDIRECTORY, fs.AUTHENTICATED),
                                  owner=fs.SYSTEM)}),
            ("owned by TrustedInstaller", {PROFILE: fs.View(owner=fs.TRUSTED_INSTALLER)}),
            ("owned by SYSTEM", {PROFILE: fs.View(owner=fs.SYSTEM)}),
            ("owned by the administrators", {PROFILE: fs.View(owner=fs.ADMINS)}),
            ("a final path in the \\\\?\\ form and other case",
             {LOCAL: fs.View(owner=fs.ME, final="\\\\?\\c:\\users\\ME\\AppData\\Local")}),
        ]
        for name, overrides in cases:
            with self.subTest(name):
                self.assertIsNone(refusal(overrides), name)


class _Stdout(io.StringIO):
    """stdout that notes how many handles of the fake were open when each
    line was written."""

    def __init__(self, fake):
        io.StringIO.__init__(self)
        self.fake = fake
        self.open_at = []

    def write(self, s):
        if s.strip():
            self.open_at.append((s.strip(), len(self.fake.open_handles())))
        return io.StringIO.write(self, s)


class PrepareOutcomeTest(unittest.TestCase):
    """Row #128: the outcome of prepare over the open handles of the chain."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-prepare-win-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.parent = os.path.join(self.root, "Иван O'Brien")
        os.mkdir(self.parent, 0o700)
        self.token_dir = os.path.join(self.parent, "skein-mon-sk-1-0011223344556677")

    def run_prepare(self, fake, starter=STARTER):
        out = _Stdout(fake)
        env = {"SKEIN_TOKEN_DIR": self.token_dir, "SKEIN_PREPARE_STARTER": starter}
        with mock.patch.object(sys, "stdout", out):
            code = LOADER["loader_client_prepare"](env, fake)
        lines = [json.loads(l) for l in out.getvalue().splitlines() if l.strip()]
        return code, lines, out.open_at

    def components(self):
        return LOADER["path_components"](self.token_dir)

    def assert_refused(self, code, lines, reason):
        self.assertNotEqual(code, 0)
        self.assertEqual([l["event"] for l in lines], ["prepare_failed"])
        self.assertEqual(lines[0]["reason"], reason)
        # INV-17: the line names the cause, never a path of the chain.
        for path in self.components()[1:]:
            self.assertNotIn(path, lines[0]["action"])
            self.assertNotIn(os.path.basename(path), lines[0]["action"])
        self.assertFalse(os.path.exists(os.path.join(self.token_dir, "starter.py")))
        self.assertFalse(os.path.exists(os.path.join(self.token_dir, "token")))

    def test_a_confirmed_chain_writes_the_starter_and_prints_the_ready_line_with_every_handle_open(self):
        fake = fs.FakeSecurity()
        code, lines, open_at = self.run_prepare(fake)
        self.assertEqual(code, 0)
        self.assertEqual(lines, [{"event": "prepare_ready"}])
        self.assertEqual(fake.made, [self.token_dir])
        self.assertEqual([h.path for h in fake.opened], self.components())
        # Every handle of the chain is still open when the ready line goes
        # out; none was closed before it.
        self.assertEqual(open_at, [('{"event":"prepare_ready"}', len(self.components()))])
        self.assertEqual(fake.closed, [])
        with open(os.path.join(self.token_dir, "starter.py"), "rb") as fh:
            self.assertEqual(fh.read(), STARTER.encode("ascii"))

    def test_a_refusal_on_a_late_component_leaves_no_starter(self):
        fake = fs.FakeSecurity(views={self.parent: fs.View(owner=fs.ME, dacl=[
            (fs.ALLOW, 0, fs.FILE_DELETE_CHILD, fs.OTHER)] + fs.trusted_dacl())})
        self.assert_refused(*self.run_prepare(fake)[:2], reason="token_dir_insecure")

    def test_an_unreadable_descriptor_of_the_last_component_leaves_no_starter(self):
        fake = fs.FakeSecurity(fail_view=[self.token_dir])
        self.assert_refused(*self.run_prepare(fake)[:2], reason="token_dir_insecure")

    def test_a_component_that_cannot_be_opened_leaves_no_starter(self):
        fake = fs.FakeSecurity(fail_open=[self.parent])
        self.assert_refused(*self.run_prepare(fake)[:2], reason="token_dir_insecure")

    def test_a_token_directory_that_resolves_elsewhere_leaves_no_starter(self):
        fake = fs.FakeSecurity(views={self.token_dir: fs.View(
            owner=fs.ME, dacl=fs.private_dacl(), final=os.path.join(self.root, "elsewhere"))})
        self.assert_refused(*self.run_prepare(fake)[:2], reason="token_dir_insecure")

    def test_the_cause_names_the_component_by_its_role(self):
        fake = fs.FakeSecurity(views={self.parent: fs.View(owner=fs.ME, dacl=[
            (fs.ALLOW, 0, fs.FILE_DELETE_CHILD, fs.OTHER)] + fs.trusted_dacl())})
        _, lines, _ = self.run_prepare(fake)
        self.assertIn("the parent of the token directory", lines[0]["action"])

    def test_an_existing_token_directory_is_not_used(self):
        os.mkdir(self.token_dir, 0o700)
        fake = fs.FakeSecurity()
        self.assert_refused(*self.run_prepare(fake)[:2], reason="token_dir_exists")
        self.assertEqual(fake.opened, [])

    def test_no_starter_text_is_misconfigured(self):
        fake = fs.FakeSecurity()
        self.assert_refused(*self.run_prepare(fake, starter="")[:2], reason="misconfigured")
        self.assertEqual(fake.made, [])


class StarterInPrepareModeTest(unittest.TestCase):
    """R-10: the Windows prepare runs the starter in prepare mode; a refusal
    of the starter there is a prepare_failed line with the reason of the
    starter refusal table, and no directory is created."""

    def test_a_starter_refusal_is_a_prepare_failed_line(self):
        root = tempfile.mkdtemp(prefix="skein-prepare-win-")
        self.addCleanup(shutil.rmtree, root, True)
        token_dir = os.path.join(root, "skein-mon-sk-1-0011223344556677")
        with open(PREPARE_WINDOWS_SRC) as fh:
            script = fh.read().replace("__SKEIN_STARTER__", json.dumps(STARTER), 1)
        out = io.StringIO()
        code = 0
        # No server address: the starter refuses before any download.
        with mock.patch.dict(os.environ, {"SKEIN_TOKEN_DIR": token_dir, "SKEIN_PUBLIC_URL": "",
                                          "SKEIN_LOADER_SHA256": "ab" * 32}), \
                mock.patch.object(sys, "stdout", out):
            try:
                exec(compile(script, "prepare", "exec"), {"__name__": "__main__"})
            except SystemExit as exc:
                code = exc.code
        lines = [json.loads(l) for l in out.getvalue().splitlines() if l.strip()]
        self.assertNotEqual(code, 0)
        self.assertEqual([(l["event"], l["reason"]) for l in lines], [("prepare_failed", "misconfigured")])
        self.assertFalse(os.path.exists(token_dir))

if __name__ == "__main__":
    unittest.main()
