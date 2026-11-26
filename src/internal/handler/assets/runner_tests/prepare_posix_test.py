#!/usr/bin/env python3
"""Tests of the POSIX `prepare` of a monitor call with `client`.

The prepare script is rendered the way the server renders it
(internal/mcp/tools/monitor_loader.go): the client loader joined from
loader_client/parts.txt, full-line comments dropped, put into the script as
a JSON string literal. The script then runs as `python3 -c`, exactly like the
command of the response. Standard library of Python 3.6 only.
"""

import ast
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

import harness

TOOLS_ASSETS = os.path.join(harness.HERE, "..", "..", "..", "mcp", "tools", "assets")


def _strip_comment_lines(src):
    return "".join(line for line in src.splitlines(True) if not line.lstrip(" \t").startswith("#"))


def client_loader():
    names = []
    with open(os.path.join(TOOLS_ASSETS, "loader_client", "parts.txt")) as fh:
        for line in fh:
            name = line.strip()
            if name and not name.startswith("#"):
                names.append(name)
    parts = []
    for name in names:
        with open(os.path.join(TOOLS_ASSETS, name)) as fh:
            text = fh.read()
        if text and not text.endswith("\n"):
            text += "\n"
        parts.append(text)
    return _strip_comment_lines("".join(parts))


def prepare_script():
    with open(os.path.join(TOOLS_ASSETS, "monitor_prepare.py")) as fh:
        script = _strip_comment_lines(fh.read())
    return script.replace("__SKEIN_CLIENT_LOADER__", json.dumps(client_loader()), 1).rstrip("\n")


def run_prepare(token_dir):
    env = dict(os.environ)
    env["SKEIN_TOKEN_DIR"] = token_dir
    proc = subprocess.run([sys.executable, "-c", prepare_script()], env=env,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True, timeout=30)
    lines = [json.loads(line) for line in proc.stdout.splitlines() if line.strip()]
    return proc.returncode, lines


def loader_mode(path):
    """The value the loader file assigns to _SKEIN_LOADER_MODE, or None."""
    with open(path) as fh:
        tree = ast.parse(fh.read())
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
                isinstance(t, ast.Name) and t.id == "_SKEIN_LOADER_MODE" for t in node.targets):
            return ast.literal_eval(node.value)
    return None


class PreparePOSIXTest(unittest.TestCase):
    """Row #18 (П-3): the token directory is created new with mode 0700, the
    client loader is written into it, and the ready line is printed only on
    success."""

    def setUp(self):
        self.base = tempfile.mkdtemp(prefix="skein-prepare-")
        self.addCleanup(shutil.rmtree, self.base, True)

    def test_new_directory_is_private_and_carries_the_client_loader(self):
        token_dir = os.path.join(self.base, "skein-mon-sk-1-0123456789abcdef")
        rc, lines = run_prepare(token_dir)
        self.assertEqual(rc, 0)
        self.assertEqual([l["event"] for l in lines], ["prepare_ready"])
        st = os.lstat(token_dir)
        self.assertTrue(stat.S_ISDIR(st.st_mode))
        self.assertEqual(stat.S_IMODE(st.st_mode), 0o700)
        self.assertEqual(st.st_uid, os.getuid())
        loader = os.path.join(token_dir, "loader.py")
        self.assertEqual(lines[0]["loader"], loader)
        self.assertEqual(loader_mode(loader), "client")

    def test_existing_directory_is_refused_without_the_ready_line(self):
        token_dir = os.path.join(self.base, "skein-mon-sk-1-taken")
        os.mkdir(token_dir, 0o700)
        rc, lines = run_prepare(token_dir)
        self.assertEqual(rc, 1)
        self.assertEqual([l["event"] for l in lines], ["prepare_failed"])
        self.assertEqual(lines[0]["reason"], "token_dir_exists")
        self.assertEqual(os.listdir(token_dir), [])

    def test_existing_directory_of_another_mode_is_refused_and_left_alone(self):
        token_dir = os.path.join(self.base, "skein-mon-sk-1-open")
        os.mkdir(token_dir)
        os.chmod(token_dir, 0o755)
        rc, lines = run_prepare(token_dir)
        self.assertEqual(rc, 1)
        self.assertNotIn("prepare_ready", [l["event"] for l in lines])
        self.assertEqual(stat.S_IMODE(os.lstat(token_dir).st_mode), 0o755)
        self.assertEqual(os.listdir(token_dir), [])


if __name__ == "__main__":
    unittest.main()
