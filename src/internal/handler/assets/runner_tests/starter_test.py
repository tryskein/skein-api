#!/usr/bin/env python3
"""Tests of the monitor starter (src/internal/mcp/tools/assets/monitor_starter.py)
in both of its forms: the text of a bootstrap without `client` (run with
`python3 -c`) and the starter.py file `prepare` writes for a call with
`client`. A fake server serves the loader; the starter must take it only from
SKEIN_PUBLIC_URL, run it only when its SHA-256 matches, hand it the mode of
the call, read no token and touch no file, and answer every refusal of the
starter refusal table with exactly one stop line.

Run on its own: python3 internal/handler/assets/runner_tests/starter_test.py
Standard library of Python 3.6 only.
"""

import hashlib
import json
import os
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

ISSUE = "zz-31"
# A loader that reports the mode and the module name it was run with.
MODE_LOADER = (b"import json, sys\n"
               b"sys.stdout.write(json.dumps({'event': 'loader_ran', "
               b"'mode': globals().get('_SKEIN_LOADER_MODE'), 'name': __name__}) + '\\n')\n"
               b"sys.exit(0)\n")


def sha(body):
    return hashlib.sha256(body).hexdigest()


class TokenDir(object):
    """A private token directory as prepare leaves it after the token was
    written and a monitor ran: token, state, echo exclusion file and
    starter.py."""

    def __init__(self, test):
        self.root = tempfile.mkdtemp(prefix="skein-starter-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.dir = os.path.join(self.root, "skein-mon-zz-31-0123456789abcdef")
        os.mkdir(self.dir, 0o700)
        self.state_home = os.path.join(self.root, "state")
        os.mkdir(self.state_home, 0o700)
        self.token = os.path.join(self.dir, "token")
        files = {
            "token": b"skm_" + b"s" * 43,
            "state.json": b'{"version": 1, "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 7}, '
                          b'"pending": [{"id": 8, "type": "comment.created"}]}',
            "exclude_session": b"sess-31\n",
            "starter.py": harness.starter(client=True).encode("ascii"),
        }
        for name, data in files.items():
            with open(os.path.join(self.dir, name), "wb") as fh:
                fh.write(data)
            os.chmod(os.path.join(self.dir, name), 0o600)
        self.starter_file = os.path.join(self.dir, "starter.py")

    def snapshot(self):
        """Every file of the token directory and the state home, with its
        bytes and mode."""
        out = {}
        for base in (self.dir, self.state_home):
            for dirpath, _, names in os.walk(base):
                for name in names:
                    path = os.path.join(dirpath, name)
                    with open(path, "rb") as fh:
                        out[path] = (fh.read(), stat.S_IMODE(os.lstat(path).st_mode))
        return out


def starter_argv(form, box):
    """The argv of the starter: the text of the bootstrap without client, or
    the starter.py file of the preflight and working command."""
    if form == "text":
        return [sys.executable, "-c", harness.starter(client=False)]
    return [sys.executable, box.starter_file]


def run_starter(form, box, url, loader_sha, extra_env=None, drop=(), on_start=None):
    """Run the starter once; returns (exit code, stdout lines, raw stdout,
    stderr)."""
    env = dict(os.environ)
    for name in list(env):
        if name.startswith("SKEIN_"):
            del env[name]
    env.update({
        "SKEIN_PUBLIC_URL": url, "SKEIN_ISSUE": ISSUE, "SKEIN_TOKEN_FILE": box.token,
        "SKEIN_EXCLUDE_SESSION": "sess-31", "SKEIN_RUNNER_SHA256": "ab" * 32,
        "SKEIN_LOADER_SHA256": loader_sha, "XDG_STATE_HOME": box.state_home,
    })
    if form == "file":
        env.update({"SKEIN_CLIENT": "alpha", "SKEIN_PLATFORM": "posix", "SKEIN_MONITOR_MODE": "preflight"})
    for name in drop:
        env.pop(name, None)
    env.update(extra_env or {})
    proc = subprocess.Popen(starter_argv(form, box), stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            env=env, universal_newlines=True)
    if on_start is not None:
        on_start(proc)
    try:
        out, err = proc.communicate(timeout=60)
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
    lines = [json.loads(l) for l in out.splitlines() if l.strip()]
    return proc.returncode, lines, out, err


def closed_url():
    """The URL of a local port nothing listens on."""
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return "http://127.0.0.1:%d" % port


def site_dir(test, source):
    """A directory with a sitecustomize.py of the given source."""
    d = tempfile.mkdtemp(prefix="skein-starter-site-")
    test.addCleanup(shutil.rmtree, d, True)
    with open(os.path.join(d, "sitecustomize.py"), "w") as fh:
        fh.write(source)
    return d


def loader_server(test, body=MODE_LOADER, loader=None, script=None, poll=None):
    srv = harness.LoaderServer(script or (lambda h: h.send_body(500, b"")), poll,
                               loader or (lambda h: h.send_body(200, body)))
    test.addCleanup(srv.stop)
    return srv


def redirect_to(target):
    def answer(handler):
        handler.send_response(302)
        handler.send_header("Location", target + "/agent/v1/monitor-loader")
        handler.send_header("Content-Length", "0")
        handler.end_headers()
    return answer


FORMS = ("text", "file")


class StarterRefusalTest(unittest.TestCase):
    """Matrix row #143: every row of the starter refusal table, in both forms
    of the starter: one stop line with its reason, event and exit code, an
    action naming the loader download, no traceback and no exception text,
    and every file of the token directory untouched."""

    def assert_refusal(self, form, box, before, result, reason, event, code):
        rc, lines, out, err = result
        self.assertEqual(rc, code, (form, reason, out, err))
        self.assertEqual(len(lines), 1, (form, reason, out))
        line = lines[0]
        self.assertEqual((line.get("reason"), line.get("event")), (reason, event), (form, line))
        self.assertTrue(line.get("action", "").startswith("Loader download: "), (form, line))
        for text in (out, err):
            self.assertNotIn("Traceback", text, (form, reason))
            self.assertNotIn("Error", text, (form, reason))
        self.assertEqual(box.snapshot(), before, (form, reason))

    def test_missing_url_or_digest_is_misconfigured(self):
        srv = loader_server(self)
        for form in FORMS:
            for drop in ("SKEIN_PUBLIC_URL", "SKEIN_LOADER_SHA256"):
                with self.subTest(form=form, drop=drop):
                    box = TokenDir(self)
                    before = box.snapshot()
                    result = run_starter(form, box, srv.url(), sha(MODE_LOADER), drop=(drop,))
                    self.assert_refusal(form, box, before, result, "misconfigured", "closed", 1)

    def test_network_failures_are_server_unreachable(self):
        srv = loader_server(self)
        other = loader_server(self)
        error = loader_server(self, loader=lambda h: h.send_body(500, b"no"))
        moved = loader_server(self, loader=redirect_to(other.url()))
        site = os.path.join(harness.SITE_DIR)
        for form in FORMS:
            cases = [
                ("network", closed_url(), None),
                ("other code", error.url(), None),
                ("redirect", moved.url(), None),
                ("timeout", srv.url(), {"PYTHONPATH": site, "SKEIN_TEST_SITE": "1",
                                        "SKEIN_TEST_URLOPEN_TIMEOUT": "1"}),
            ]
            for name, url, extra in cases:
                with self.subTest(form=form, case=name):
                    box = TokenDir(self)
                    before = box.snapshot()
                    result = run_starter(form, box, url, sha(MODE_LOADER), extra_env=extra)
                    self.assert_refusal(form, box, before, result, "server_unreachable", "paused", 1)
        self.assertEqual(other.loader_hits, 0)

    def test_digest_mismatch_is_runner_mismatch(self):
        srv = loader_server(self)
        for form in FORMS:
            with self.subTest(form=form):
                box = TokenDir(self)
                before = box.snapshot()
                result = run_starter(form, box, srv.url(), sha(b"another loader"))
                self.assert_refusal(form, box, before, result, "runner_mismatch", "closed", 1)

    def test_old_python_is_config_invalid(self):
        srv = loader_server(self)
        site = site_dir(self, "import sys\nsys.version_info = (3, 5, 9, 'final', 0)\n")
        for form in FORMS:
            with self.subTest(form=form):
                box = TokenDir(self)
                before = box.snapshot()
                result = run_starter(form, box, srv.url(), sha(MODE_LOADER), extra_env={"PYTHONPATH": site})
                self.assert_refusal(form, box, before, result, "config_invalid", "closed", 1)
                self.assertIn("3.6", result[1][0]["action"])
        self.assertEqual(srv.loader_hits, 0)

    def test_signal_during_the_download_is_stopped(self):
        for form in FORMS:
            with self.subTest(form=form):
                asked = threading.Event()
                release = threading.Event()
                self.addCleanup(release.set)

                def hold(handler):
                    asked.set()
                    release.wait(30)
                    handler.send_body(200, MODE_LOADER)
                srv = loader_server(self, loader=hold)
                box = TokenDir(self)
                before = box.snapshot()

                def signal_when_asked(proc):
                    if not asked.wait(30):
                        raise AssertionError("the starter never asked for the loader")
                    proc.send_signal(signal.SIGTERM)
                result = run_starter(form, box, srv.url(), sha(MODE_LOADER), on_start=signal_when_asked)
                release.set()
                self.assert_refusal(form, box, before, result, "stopped", "paused", 0)


class StarterExecutionTest(unittest.TestCase):
    """Matrix row #144: what the starter runs."""

    def test_the_loader_comes_only_from_the_public_url_and_no_redirect(self):
        other = loader_server(self)
        moved = loader_server(self, loader=redirect_to(other.url()))
        for form in FORMS:
            with self.subTest(form=form):
                box = TokenDir(self)
                rc, lines, out, _ = run_starter(form, box, moved.url(), sha(MODE_LOADER))
                self.assertEqual(rc, 1, out)
                self.assertEqual(lines[0]["reason"], "server_unreachable")
                self.assertEqual(moved.loader_hits, FORMS.index(form) + 1)
        self.assertEqual(other.loader_hits, 0)
        self.assertEqual(other.script_hits, 0)

    def test_only_the_matching_bytes_run(self):
        marker = os.path.join(tempfile.mkdtemp(prefix="skein-starter-marker-"), "ran")
        self.addCleanup(shutil.rmtree, os.path.dirname(marker), True)
        body = ("open(%r, 'w').write('x')\n" % marker).encode("utf-8")
        srv = loader_server(self, body=body)
        for form in FORMS:
            with self.subTest(form=form):
                box = TokenDir(self)
                rc, lines, out, _ = run_starter(form, box, srv.url(), sha(b"the minted loader"))
                self.assertEqual((rc, lines[0]["reason"]), (1, "runner_mismatch"), out)
                self.assertFalse(os.path.exists(marker), "a loader with another digest was run")
        box = TokenDir(self)
        rc, _, out, err = run_starter("text", box, srv.url(), sha(body))
        self.assertEqual(rc, 0, (out, err))
        self.assertTrue(os.path.exists(marker), "the loader with the minted digest was not run")

    def test_the_token_is_not_opened(self):
        # The loader the starter runs opens exclude_session next to the
        # token: the positive control that the I/O counter sees opens in the
        # token directory at all. Everything before that is the starter.
        control = (b"import os, sys\n"
                   b"open(os.path.join(os.path.dirname(os.environ['SKEIN_TOKEN_FILE']), 'exclude_session')).close()\n"
                   b"sys.stdout.write('{\"event\": \"loader_ran\"}\\n')\n"
                   b"sys.exit(0)\n")
        srv = loader_server(self, body=control)
        for form in FORMS:
            with self.subTest(form=form):
                box = TokenDir(self)
                log = os.path.join(box.root, "io.log")
                rc, lines, out, err = run_starter(form, box, srv.url(), sha(control), extra_env={
                    "PYTHONPATH": harness.SITE_DIR, "SKEIN_TEST_SITE": "1",
                    "SKEIN_TEST_IO_LOG": log, "SKEIN_TEST_IO_WATCH": box.dir})
                self.assertEqual(rc, 0, (out, err))
                self.assertEqual(lines, [{"event": "loader_ran"}])
                with open(log) as fh:
                    entries = fh.read().splitlines()
                control_path = os.path.join(box.dir, "exclude_session")
                self.assertTrue([e for e in entries if e.startswith("open %s " % control_path)],
                                "the I/O counter did not see the control open: %r" % entries)
                self.assertEqual([e for e in entries if box.token + " " in e or e.endswith(box.token)], [],
                                 "the token file was opened")

    def test_the_mode_travels_in_the_namespace(self):
        srv = loader_server(self)
        box = TokenDir(self)
        # The text of a bootstrap without client, with SKEIN_CLIENT in the
        # environment: the loader still gets no client mode.
        rc, lines, out, err = run_starter("text", box, srv.url(), sha(MODE_LOADER),
                                          extra_env={"SKEIN_CLIENT": "alpha", "SKEIN_PLATFORM": "posix",
                                                     "_SKEIN_LOADER_MODE": "client"})
        self.assertEqual(rc, 0, (out, err))
        self.assertEqual(lines, [{"event": "loader_ran", "mode": None, "name": "__main__"}])
        # starter.py of the preflight: the client branch.
        rc, lines, out, err = run_starter("file", box, srv.url(), sha(MODE_LOADER))
        self.assertEqual(rc, 0, (out, err))
        self.assertEqual(lines, [{"event": "loader_ran", "mode": "client", "name": "__main__"}])


class LiveRunner(object):
    """A legacy monitor started through the starter text: the served loader
    and the rendered runner from a fake server; pushed notifications are
    answered once."""

    def __init__(self, test, box):
        self.pending = []
        self.lock = threading.Lock()
        self.polled = threading.Event()
        self.runner = harness.render_runner(poll_sec=1).encode("utf-8")
        self.srv = loader_server(test, loader=lambda h: h.send_body(200, harness.served_loader().encode("utf-8")),
                                 script=lambda h: h.send_body(200, self.runner), poll=self.poll)
        self.loader_sha = sha(harness.served_loader().encode("utf-8"))
        env = dict(os.environ)
        for name in list(env):
            if name.startswith("SKEIN_"):
                del env[name]
        env.update({"SKEIN_PUBLIC_URL": self.srv.url(), "SKEIN_ISSUE": ISSUE, "SKEIN_TOKEN_FILE": box.token,
                    "SKEIN_EXCLUDE_SESSION": "", "SKEIN_RUNNER_SHA256": sha(self.runner),
                    "SKEIN_LOADER_SHA256": self.loader_sha, "XDG_STATE_HOME": box.state_home})
        self.proc = subprocess.Popen([sys.executable, "-c", harness.starter(client=False)],
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
                                     universal_newlines=True)
        test.addCleanup(self.stop)
        self.lines = []
        self.queue = []
        self.cond = threading.Condition()
        threading.Thread(target=self._pump, daemon=True).start()

    def poll(self, handler):
        with self.lock:
            notes, self.pending = self.pending, []
        self.polled.set()
        handler.send_body(200, json.dumps({"notifications": notes,
                                           "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 40}}).encode())

    def push(self, nid):
        with self.lock:
            self.pending.append({"id": nid, "type": "comment.created"})

    def _pump(self):
        for line in self.proc.stdout:
            with self.cond:
                self.lines.append(json.loads(line))
                self.cond.notify_all()

    def wait_for(self, pred, timeout=30):
        deadline = time.time() + timeout
        with self.cond:
            while True:
                for line in self.lines:
                    if pred(line):
                        return line
                left = deadline - time.time()
                if left <= 0:
                    raise AssertionError("no such line; lines: %r" % self.lines)
                self.cond.wait(left)

    def stop(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait()


@unittest.skipUnless(os.name == "posix", "the POSIX one-runner lock")
class StarterRestartTest(unittest.TestCase):
    """Matrix row #145 (SC-003, US-12 «Restart and the starter», the
    rollout boundary)."""

    def test_a_second_start_refused_by_the_starter_leaves_the_live_owner_alone(self):
        box = TokenDir(self)
        os.remove(os.path.join(box.dir, "state.json"))
        owner = LiveRunner(self, box)
        owner.wait_for(lambda l: l.get("event") == "monitor_started")
        if not owner.polled.wait(30):
            raise AssertionError("the owner never polled")
        owner.push(41)
        owner.wait_for(lambda l: l.get("event") == "notification")
        before = box.snapshot()
        refused = loader_server(self, body=b"a loader of another build\n")
        for form in FORMS:
            for name, url, digest, reason in (
                    ("digest", refused.url(), owner.loader_sha, "runner_mismatch"),
                    ("network", closed_url(), owner.loader_sha, "server_unreachable")):
                with self.subTest(form=form, case=name):
                    rc, lines, out, _ = run_starter(form, box, url, digest)
                    self.assertEqual((rc, len(lines), lines[0]["reason"]), (1, 1, reason), out)
                    self.assertEqual(box.snapshot(), before, (form, name))
        self.assertIsNone(owner.proc.poll(), "the owner stopped")
        owner.push(42)
        line = owner.wait_for(lambda l: l.get("event") == "notification" and l["notification"]["id"] == 42)
        self.assertEqual(line["notification"]["id"], 42)

    def test_a_tail_without_owner_stays_as_it_is_while_the_starter_refuses(self):
        box = TokenDir(self)
        before = box.snapshot()
        srv = loader_server(self, body=b"a loader of another build\n")
        for form in FORMS:
            for _ in range(2):
                for name, kw, reason in (
                        ("runner_mismatch", {}, "runner_mismatch"),
                        ("misconfigured", {"drop": ("SKEIN_PUBLIC_URL",)}, "misconfigured")):
                    with self.subTest(form=form, case=name):
                        rc, lines, out, _ = run_starter(form, box, srv.url(), sha(MODE_LOADER), **kw)
                        self.assertEqual((rc, len(lines), lines[0]["reason"]), (1, 1, reason), out)
        # Files and state intact; no outcome of any event was written (no
        # journal, no ledger under the state home).
        self.assertEqual(box.snapshot(), before)
        self.assertEqual(os.listdir(box.state_home), [])


if __name__ == "__main__":
    unittest.main()
