#!/usr/bin/env python3
"""Tests of the client branch of the loader (monitorassets/loader_client
with the runner's end sequence): matrix rows #69, #70, #71, #72 and #129.

The real starter.py of a call with `client` runs the real served loader
against a fake server (harness.LoaderServer) whose runner route answers as a
case needs: 401, 403, 500, a body of another digest, or the rendered runner.
The one-runner lock of a live owner is held by this process (a flock belongs
to the open file, so the loader in its own process finds it held). Row #129
drives the end of the client branch in this process, in the namespace of
its parts, to stop it at a named point of the end sequence.

Expected values are those of the spec (US-12 "stop in detached", "restart
and the starter", "file fate", "reissue commands"; R-9; the table of stops):
401 is token_expired and 403 visibility_lost with different actions, the
events the state keeps get their journal lines without the preview, then
the files go; 5xx or a timeout is server_unreachable and keeps everything;
a live owner of the lock leaves the files alone; runner_mismatch, an empty
or insecure token of ours and misconfigured delete the token with its files;
a missing token stays missing and an unreadable one is left alone.

Run on its own: python3 internal/handler/assets/runner_tests/loader_client_test.py
Standard library of Python 3.6 only.
"""

import base64
import glob
import hashlib
import json
import os
import re
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

ISSUE = "zz-69"
TOKEN = "skm_" + "L" * 43
CURSOR = {"ts": "2026-01-01T00:00:00Z", "id": 7}
NEXT = {"ts": "2026-01-01T00:00:01Z", "id": 9}
PREVIEW = "a secret preview"
PREPARE_SRC = os.path.join(os.path.dirname(harness.LOADER_SRC), "monitor_prepare.py")


def sha(data):
    return hashlib.sha256(data).hexdigest()


def note(nid):
    return {"id": nid, "type": "comment_added", "entity": {"type": "issue", "id": ISSUE},
            "actor": {"username": "bob"}, "commentId": "c-%d" % nid, "bodyPreview": PREVIEW,
            "truncated": False}


def closed_url():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return "http://127.0.0.1:%d" % port


class Box(object):
    """A token directory as prepare and a monitor leave it: token, starter.py,
    the exclusion file and a state that keeps events."""

    def __init__(self, test, state=None, token=TOKEN):
        self.test = test
        self.root = tempfile.mkdtemp(prefix="skein-loader-client-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.state_home = os.path.join(self.root, "state")
        os.mkdir(self.state_home, 0o700)
        self.dir = os.path.join(self.root, "skein-mon-zz-69-0123456789abcdef")
        os.mkdir(self.dir, 0o700)
        self.token = os.path.join(self.dir, "token")
        doc = state if state is not None else {
            "version": 1, "cursor": CURSOR,
            "remainder": {"events": [note(8)], "next": NEXT, "rootClosed": False}}
        files = {"token": token.encode(), "starter.py": harness.starter(client=True).encode("ascii"),
                 "exclude_session": b"mcp-session-31\n", "state.json": json.dumps(doc).encode()}
        for name, data in files.items():
            path = os.path.join(self.dir, name)
            with open(path, "wb") as fh:
                fh.write(data)
            os.chmod(path, 0o600)
        self.starter_file = os.path.join(self.dir, "starter.py")

    def env(self, url, runner_sha="ab" * 32, extra=None):
        env = dict((k, v) for k, v in os.environ.items() if not k.startswith("SKEIN_"))
        env.update({"SKEIN_MONITOR_MODE": "run", "SKEIN_MONITOR_PREFLIGHT": "1", "SKEIN_MONITOR_CHANNEL": "",
                    "SKEIN_MONITOR_BLIND": "", "SKEIN_PUBLIC_URL": url, "SKEIN_ISSUE": ISSUE,
                    "SKEIN_TOKEN_FILE": self.token, "SKEIN_EXCLUDE_SESSION": "mcp-session-31",
                    "SKEIN_RUNNER_SHA256": runner_sha,
                    "SKEIN_LOADER_SHA256": sha(harness.served_loader().encode("utf-8")),
                    "SKEIN_CLIENT": "alpha", "SKEIN_PLATFORM": "posix", "XDG_STATE_HOME": self.state_home})
        env.update(extra or {})
        return env

    def run(self, url, runner_sha="ab" * 32, extra=None, drop=()):
        """The working command once; (exit code, stop lines, stderr)."""
        env = self.env(url, runner_sha, extra)
        for name in drop:
            env.pop(name, None)
        proc = subprocess.run([sys.executable, self.starter_file], stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, env=env, universal_newlines=True, timeout=90)
        return proc.returncode, [json.loads(l) for l in proc.stdout.splitlines() if l.strip()], proc.stderr

    def files(self):
        try:
            return sorted(os.listdir(self.dir))
        except FileNotFoundError:
            return None

    def snapshot(self):
        out = {}
        for dirpath, _, names in os.walk(self.dir):
            for name in names:
                path = os.path.join(dirpath, name)
                with open(path, "rb") as fh:
                    out[path] = (fh.read(), stat.S_IMODE(os.lstat(path).st_mode))
        return out

    def journal(self):
        lines = []
        for path in glob.glob(os.path.join(self.state_home, "skein-monitor", "journal", "*")):
            with open(path) as fh:
                lines.extend(json.loads(l) for l in fh if l.strip())
        return lines

    def deliveries(self):
        return [l for l in self.journal() if l.get("event") == "delivery"]


def server(test, script):
    srv = harness.LoaderServer(script)
    test.addCleanup(srv.stop)
    return srv


def status(code):
    return lambda h: h.send_body(code, b"")


def hold_lock(test, box):
    """The one-runner lock of the token held as a live owner holds it."""
    runner = harness.load_runner()
    lock = runner.current_platform().acquire_runner_lock(box.token, 0, {"XDG_STATE_HOME": box.state_home,
                                                                         "HOME": box.root})
    test.addCleanup(lock.release)
    return lock


def mint_args(action):
    """The parameters of the skein_get_monitor_script call an action names."""
    m = re.search(r"skein_get_monitor_script\((.*?)\)(?:\.|$| )", action)
    if m is None:
        raise AssertionError("no mint call in %r" % action)
    args = {}
    for part in re.findall(r'(\w+)=(\[[^\]]*\]|"[^"]*"|true|false)', m.group(1)):
        args[part[0]] = json.loads(part[1])
    return args


@unittest.skipUnless(os.name == "posix", "the POSIX client loader")
class LostAccessTest(unittest.TestCase):
    """Row #69."""

    def test_401_and_403_have_their_reasons_and_finish_the_kept_rest(self):
        actions = {}
        for code, reason in ((401, "token_expired"), (403, "visibility_lost")):
            with self.subTest(status=code):
                box = Box(self)
                srv = server(self, status(code))
                rc, lines, err = box.run(srv.url())
                self.assertEqual((rc, len(lines)), (0, 1), err)
                self.assertEqual((lines[0]["event"], lines[0]["reason"], lines[0]["status"]), ("closed", reason, code))
                actions[reason] = lines[0]["action"]
                self.assertIsNone(box.files(), "the token directory was kept")
                kept = [l for l in box.deliveries() if l["notificationId"] == 8]
                self.assertEqual([l["status"] for l in kept], ["access-lost"])
                self.assertNotIn("bodyPreview", kept[0])
        self.assertNotEqual(actions["token_expired"], actions["visibility_lost"])

    def test_the_journal_ends_with_the_stop_line_without_a_kept_tail(self):
        # Review of wave 8, row 21: with nothing waiting, the monitor the
        # loader ends still has its stop line in the journal.
        for code, reason in ((401, "token_expired"), (403, "visibility_lost")):
            with self.subTest(status=code):
                box = Box(self, state={"version": 1, "cursor": CURSOR})
                rc, lines, err = box.run(server(self, status(code)).url())
                self.assertEqual(lines[0]["reason"], reason, err)
                self.assertIsNone(box.files())
                last = box.journal()[-1]
                self.assertEqual((last["event"], last["reason"], last.get("status")), ("closed", reason, code))
                self.assertEqual(last["action"], lines[0]["action"])

    def test_an_outcome_fixed_but_not_journaled_gets_its_line(self):
        box = Box(self, state={"version": 1, "cursor": CURSOR, "outcomes": {"9": "queued"}, "journaled": []})
        srv = server(self, status(401))
        rc, lines, _ = box.run(srv.url())
        self.assertEqual(lines[0]["reason"], "token_expired")
        self.assertEqual([(l["notificationId"], l["status"]) for l in box.deliveries()], [("9", "queued")])

    def test_a_server_error_or_timeout_keeps_everything(self):
        for name, url_of in (("5xx", lambda: server(self, status(503)).url()), ("network", closed_url)):
            with self.subTest(case=name):
                box = Box(self)
                before = box.snapshot()
                rc, lines, _ = box.run(url_of())
                self.assertEqual((rc, lines[0]["event"], lines[0]["reason"]), (1, "paused", "server_unreachable"))
                self.assertEqual(box.snapshot(), before)
                self.assertEqual(box.deliveries(), [])

    def test_a_download_that_trickles_ends_within_the_timeout(self):
        # Review of wave 8, row 20: the timeout bounds the whole download,
        # not each socket operation. The served loader here waits 2 s.
        stop = threading.Event()
        self.addCleanup(stop.set)

        def trickle(h):
            h.send_response(200)
            h.send_header("Content-Length", str(1 << 20))
            h.end_headers()
            try:
                while not stop.is_set():
                    h.wfile.write(b"x")
                    h.wfile.flush()
                    time.sleep(0.5)
            except OSError:
                pass
        mark = "LOADER_DOWNLOAD_TIMEOUT_SEC = 30"
        loader = harness.served_loader()
        self.assertIn(mark, loader)
        short = loader.replace(mark, "LOADER_DOWNLOAD_TIMEOUT_SEC = 2").encode("utf-8")
        srv = harness.LoaderServer(trickle, loader=lambda h: h.send_body(200, short))
        self.addCleanup(srv.stop)
        box = Box(self)
        before = box.snapshot()
        started = time.monotonic()
        rc, lines, _ = box.run(srv.url(), extra={"SKEIN_LOADER_SHA256": sha(short)})
        self.assertLess(time.monotonic() - started, 15)
        self.assertEqual((rc, lines[0]["event"], lines[0]["reason"]), (1, "paused", "server_unreachable"))
        self.assertEqual(box.snapshot(), before)

    def test_client_stop_lines_name_the_working_command(self):
        # Review of wave 8, row 26: a call with `client` has no bootstrap.
        runner = harness.load_runner()
        runner.LOADER_MODE = "client"
        for reason in ("stopped", "internal_error"):
            with self.subTest(reason=reason, preflight="none"):
                action = runner.stop_action(reason, ISSUE)
                self.assertNotIn("bootstrap", action)
                self.assertIn("working command", action)
            with self.subTest(reason=reason, preflight=3):
                self.assertIn("working command of preflight 3", runner.stop_action(reason, ISSUE, preflight=3))

    def test_a_signal_during_the_download_names_the_working_command(self):
        stop = threading.Event()
        self.addCleanup(stop.set)
        asked = threading.Event()

        def slow(h):
            asked.set()
            h.send_response(200)
            h.send_header("Content-Length", str(1 << 20))
            h.end_headers()
            try:
                while not stop.is_set():
                    h.wfile.write(b"x")
                    h.wfile.flush()
                    time.sleep(0.2)
            except OSError:
                pass
        srv = server(self, slow)
        box = Box(self)
        with open(os.path.join(box.dir, "preflight.json"), "w") as fh:
            json.dump({"version": harness.load_runner().PREFLIGHT_VERSION, "preflightId": 4}, fh)
        os.chmod(os.path.join(box.dir, "preflight.json"), 0o600)
        proc = subprocess.Popen([sys.executable, box.starter_file], stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=box.env(srv.url()), universal_newlines=True)
        try:
            self.assertTrue(asked.wait(30), "the download never started")
            proc.send_signal(signal.SIGTERM)
            out, err = proc.communicate(timeout=30)
        finally:
            if proc.poll() is None:
                proc.kill()
        line = json.loads(out.splitlines()[-1])
        self.assertEqual(line["reason"], "stopped", err)
        self.assertNotIn("bootstrap", line["action"])
        self.assertIn("working command of preflight 4", line["action"])

    def test_a_live_owner_of_the_lock_is_left_to_end_the_monitor(self):
        for code in (401, 403):
            with self.subTest(status=code):
                box = Box(self)
                hold_lock(self, box)
                before = box.snapshot()
                rc, lines, _ = box.run(server(self, status(code)).url())
                self.assertEqual(len(lines), 1)
                self.assertEqual(lines[0]["status"], code)
                self.assertEqual(box.snapshot(), before)
                self.assertEqual(box.deliveries(), [])

    def test_of_two_failed_restarts_the_one_with_the_lock_ends_it(self):
        box = Box(self)
        srv = server(self, status(401))
        results = []

        def restart():
            results.append(box.run(srv.url()))
        threads = [threading.Thread(target=restart) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(120)
        # The start that took the lock ends the monitor (token_expired). The
        # other one sees token_expired too when it read the token first, or
        # token_file_missing when the token was already gone (review of wave
        # 8, row 23: which one is up to the scheduler, both are right).
        reasons = sorted(r[1][0]["reason"] for r in results)
        self.assertIn(reasons, (["token_expired", "token_expired"], ["token_expired", "token_file_missing"]))
        self.assertIsNone(box.files())
        self.assertEqual([l["status"] for l in box.deliveries() if l["notificationId"] == 8], ["access-lost"])


@unittest.skipUnless(os.name == "posix", "the POSIX client loader")
class FileFateTest(unittest.TestCase):
    """Row #70: each reason one stop line of the synchronous start, with its
    exit code, action and the fate of the token, state.json,
    exclude_session, starter.py and the token directory; the journal stays."""

    def assert_gone(self, box):
        self.assertIsNone(box.files(), "token, state.json, exclude_session and starter.py go together")

    def test_runner_mismatch_deletes_the_token_with_its_files(self):
        box = Box(self)
        srv = server(self, lambda h: h.send_body(200, b"print('another runner')\n"))
        rc, lines, _ = box.run(srv.url())
        self.assertEqual((rc, lines[0]["event"], lines[0]["reason"]), (1, "closed", "runner_mismatch"))
        self.assertIn("skein_get_monitor_script(", lines[0]["action"])
        self.assert_gone(box)

    def test_token_file_missing_by_its_kind(self):
        srv = server(self, status(500))
        missing = Box(self)
        os.remove(missing.token)
        rc, lines, _ = missing.run(srv.url())
        self.assertEqual((rc, lines[0]["reason"]), (1, "token_file_missing"))
        self.assertFalse(os.path.exists(missing.token))
        empty = Box(self, token="")
        rc, lines, _ = empty.run(srv.url())
        self.assertEqual((rc, lines[0]["reason"]), (1, "token_file_missing"))
        self.assert_gone(empty)
        if os.getuid() != 0:
            unreadable = Box(self)
            os.chmod(unreadable.token, 0)
            rc, lines, _ = unreadable.run(srv.url())
            self.assertEqual((rc, lines[0]["reason"]), (1, "token_file_missing"))
            self.assertIn("token", unreadable.files())

    def test_an_insecure_token_of_ours_goes_with_its_files(self):
        box = Box(self)
        os.chmod(box.dir, 0o755)
        os.chmod(box.token, 0o644)
        srv = server(self, status(500))
        rc, lines, _ = box.run(srv.url())
        self.assertEqual((rc, lines[0]["event"], lines[0]["reason"]), (1, "closed", "token_file_insecure"))
        self.assertEqual(srv.script_hits, 0, "the runner was downloaded")
        self.assertFalse(os.path.exists(box.token))
        self.assertFalse(os.path.exists(os.path.join(box.dir, "state.json")))
        self.assertFalse(os.path.exists(box.starter_file))

    def starter_elsewhere(self, box):
        """Run the starter from a file outside the token directory (the
        check looks at the token directory's starter.py)."""
        other = os.path.join(box.root, "elsewhere.py")
        shutil.copy(box.starter_file, other)
        os.chmod(other, 0o600)
        box.starter_file = other
        return other

    def test_the_starter_predicate_before_the_token_is_read(self):
        # Matrix row #153 by the privacy predicate of the spec (P-5): the
        # starter is a regular file of ours, not a link, and either has no
        # group/other bits or sits in a directory of ours that has none.
        # Refused: nothing downloads, which needs the token read.
        def link(box):
            real = self.starter_elsewhere(box)
            os.remove(os.path.join(box.dir, "starter.py"))
            os.symlink(real, os.path.join(box.dir, "starter.py"))

        def missing(box):
            self.starter_elsewhere(box)
            os.remove(os.path.join(box.dir, "starter.py"))
        for name, change in (("link", link), ("missing", missing)):
            with self.subTest(starter=name):
                box = Box(self)
                change(box)
                srv = server(self, status(401))
                rc, lines, _ = box.run(srv.url())
                self.assertEqual((rc, lines[0]["reason"]), (1, "token_file_insecure"))
                self.assertEqual(srv.script_hits, 0, "the token was read and sent")

    def test_group_bits_on_the_starter_in_a_private_directory_pass(self):
        # The same predicate: 0644 in a 0700 directory of ours is private
        # (no other user reaches the file), so the start goes on to the
        # download. The matrix row words bits on the file as
        # token_file_insecure; the predicate of the spec decides.
        box = Box(self)
        os.chmod(box.starter_file, 0o644)
        srv = server(self, status(503))
        rc, lines, _ = box.run(srv.url())
        self.assertEqual(lines[0]["reason"], "server_unreachable")
        self.assertEqual(srv.script_hits, 1)

    def test_misconfigured_deletes_a_known_path(self):
        box = Box(self)
        rc, lines, _ = box.run(server(self, status(500)).url(), drop=("SKEIN_RUNNER_SHA256",))
        self.assertEqual((rc, lines[0]["reason"]), (1, "misconfigured"))
        self.assert_gone(box)

    def test_server_unreachable_names_the_working_command(self):
        box = Box(self)
        rc, lines, _ = box.run(server(self, status(503)).url())
        self.assertEqual(lines[0]["reason"], "server_unreachable")
        self.assertIn("working command", lines[0]["action"])
        self.assertIn("starter.py", box.files())

    def test_the_journal_outlives_the_token(self):
        box = Box(self)
        box.run(server(self, status(401)).url())
        self.assertIsNone(box.files())
        self.assertTrue(box.journal())

    def test_the_minimum_python_on_both_platforms(self):
        cases = (("posix", (3, 5, 9), True), ("posix", (3, 6, 0), False),
                 ("win32", (3, 12, 3), True), ("win32", (3, 12, 4), False))
        for platform, version, refused in cases:
            with self.subTest(platform=platform, version=version):
                box = Box(self)
                before = box.snapshot()
                site = tempfile.mkdtemp(prefix="skein-pyver-")
                self.addCleanup(shutil.rmtree, site, True)
                with open(os.path.join(site, "sitecustomize.py"), "w") as fh:
                    fh.write("import sys\nsys.version_info = %r\nsys.platform = %r\n"
                             % (tuple(version) + ("final", 0), platform))
                rc, lines, err = box.run(closed_url(), extra={"PYTHONPATH": site})
                if refused:
                    self.assertEqual((rc, [l["reason"] for l in lines]), (1, ["config_invalid"]), err)
                else:
                    # Past the version check: what follows (the download of
                    # the loader from a closed port) is another refusal.
                    self.assertNotIn("config_invalid", [l.get("reason") for l in lines], err)
                self.assertEqual(box.snapshot(), before)


def fixture_registry(limits):
    return {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
            "agents": [{"id": "alpha", "process": {"executables": ["alpha"]},
                        "methods": [{"kind": "exit-on-event", "instruction": "e", "limits": limits}]}],
            "channels": [], "shells": {"source": "t", "names": ["sh"]}, "zavodEngines": {}}


class Pages(object):
    """The poll route: the answers in turn, then empty pages."""

    def __init__(self, answers):
        self.answers = list(answers)
        self.lock = threading.Lock()

    def __call__(self, handler):
        with self.lock:
            answer = self.answers.pop(0) if self.answers else harness.EMPTY
        if isinstance(answer, int):
            handler.send_body(answer, b"")
        else:
            handler.send_body(200, json.dumps(answer).encode())


@unittest.skipUnless(os.name == "posix", "the POSIX client loader")
class NamedPathTest(unittest.TestCase):
    """Row #71: prepare, a token written 0644 like the Write tool, the
    preflight and the working command of its line."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-named-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.state_home = os.path.join(self.root, "state")
        os.mkdir(self.state_home, 0o700)
        registry = base64.b64encode(json.dumps(fixture_registry({"batch": {"max": 3, "unit": "lines"}}))
                                    .encode()).decode()
        self.runner = harness.render_runner(registry=registry).encode("utf-8")
        self.pages = Pages([])
        self.srv = harness.LoaderServer(lambda h: h.send_body(200, self.runner), poll=self.pages)
        self.addCleanup(self.srv.stop)

    def prepare(self, token_dir):
        with open(PREPARE_SRC) as fh:
            script = fh.read().replace("__SKEIN_STARTER__", json.dumps(harness.starter(client=True)), 1)
        env = dict(os.environ, SKEIN_TOKEN_DIR=token_dir)
        proc = subprocess.run([sys.executable, "-c", script], stdout=subprocess.PIPE, env=env,
                              universal_newlines=True, timeout=30)
        return [json.loads(l) for l in proc.stdout.splitlines() if l.strip()]

    def env(self, token):
        env = dict((k, v) for k, v in os.environ.items() if not k.startswith("SKEIN_"))
        env.update({"SKEIN_MONITOR_MODE": "preflight", "SKEIN_PUBLIC_URL": self.srv.url(), "SKEIN_ISSUE": ISSUE,
                    "SKEIN_TOKEN_FILE": token, "SKEIN_EXCLUDE_SESSION": "mcp-session-31",
                    "SKEIN_RUNNER_SHA256": sha(self.runner),
                    "SKEIN_LOADER_SHA256": sha(harness.served_loader().encode("utf-8")),
                    "SKEIN_CLIENT": "alpha", "SKEIN_PLATFORM": "posix", "XDG_STATE_HOME": self.state_home})
        return env

    def run_cmd(self, argv, env):
        proc = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
                              universal_newlines=True, timeout=90)
        return [json.loads(l) for l in proc.stdout.splitlines() if l.strip()], proc.stderr

    def started_monitor(self):
        token_dir = os.path.join(self.root, "skein-mon-zz-69-00112233aabbccdd")
        self.assertEqual(self.prepare(token_dir), [{"event": "prepare_ready"}])
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(token, 0o644)
        lines, err = self.run_cmd([sys.executable, os.path.join(token_dir, "starter.py")], self.env(token))
        self.assertEqual(lines[-1]["event"], "preflight", err)
        env = dict((k, v) for k, v in os.environ.items() if not k.startswith("SKEIN_"))
        env["XDG_STATE_HOME"] = self.state_home
        return token, lines[-1]["command"], env

    def test_a_token_written_0644_runs_through_the_named_path(self):
        token, command, env = self.started_monitor()
        self.pages.answers = [{"notifications": [note(1)], "cursor": NEXT}]
        lines, err = self.run_cmd(["sh", "-c", command], env)
        events = [l.get("event") for l in lines]
        self.assertIn("monitor_started", events, err)
        self.assertEqual(lines[-1]["reason"], "delivered")

    def test_the_same_token_0644_outside_a_private_directory_is_insecure(self):
        token_dir = os.path.join(self.root, "skein-mon-open")
        self.assertEqual(self.prepare(token_dir), [{"event": "prepare_ready"}])
        os.chmod(token_dir, 0o755)
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(token, 0o644)
        env = self.env(token)
        env["SKEIN_MONITOR_MODE"] = "run"
        hits = self.srv.script_hits
        lines, _ = self.run_cmd([sys.executable, os.path.join(token_dir, "starter.py")], env)
        self.assertEqual(lines[-1]["reason"], "token_file_insecure")
        self.assertEqual(self.srv.script_hits, hits)

    def test_a_kept_rest_then_403_on_the_first_poll_is_visibility_lost(self):
        token, command, env = self.started_monitor()
        self.pages.answers = [{"notifications": [note(i) for i in range(1, 6)], "cursor": NEXT}]
        first, err = self.run_cmd(["sh", "-c", command], env)
        self.assertEqual(first[-1]["reason"], "delivered", err)
        printed = [l for l in first if l.get("event") == "notification"]
        self.assertLess(len(printed), 5, "the batch limit left no rest: %r" % first)
        # The restart: the runner downloads fine, its first poll is 403.
        self.pages.answers = [403]
        lines, err = self.run_cmd(["sh", "-c", command], env)
        self.assertEqual(lines[-1]["reason"], "visibility_lost", err)
        self.assertFalse(os.path.exists(token))
        journal = []
        for path in glob.glob(os.path.join(self.state_home, "skein-monitor", "journal", "*")):
            with open(path) as fh:
                journal.extend(json.loads(l) for l in fh if l.strip())
        lost = [l for l in journal if l.get("event") == "delivery" and l.get("status") == "access-lost"]
        self.assertTrue(lost, "the kept rest got no outcome; journal: %r" % journal)
        self.assertTrue(all("bodyPreview" not in l for l in lost))


@unittest.skipUnless(os.name == "posix", "the POSIX client loader")
class ReissueCommandTest(unittest.TestCase):
    """Row #72: the command of a new mint keeps the monitor's parameters;
    the command to go on is the working command."""

    def test_the_mint_keeps_the_parameters(self):
        cases = (({}, {"id": ISSUE, "client": "alpha", "platform": "posix"}),
                 ({"SKEIN_SUBTREE": "1", "SKEIN_EVENT_TYPES": "comment.created,issue.updated"},
                  {"id": ISSUE, "subtree": True, "eventTypes": ["comment.created", "issue.updated"],
                   "client": "alpha", "platform": "posix"}))
        for extra, want in cases:
            with self.subTest(extra=extra):
                box = Box(self)
                _, lines, _ = box.run(server(self, status(401)).url(), extra=extra)
                self.assertEqual(mint_args(lines[0]["action"]), want)

    def test_on_windows_the_mint_names_the_token_directory(self):
        runner = harness.load_runner()
        runner.LOADER_MODE = "client"
        env = {"SKEIN_CLIENT": "alpha", "SKEIN_PLATFORM": "windows",
               "SKEIN_TOKEN_FILE": os.path.join("base", "skein-mon-zz-69-x", "token")}
        args = mint_args(runner.mint_call(ISSUE, env) + ".")
        self.assertEqual(args["tokenDir"], os.path.abspath("base"))
        self.assertEqual(args["platform"], "windows")

    def test_the_command_to_go_on_is_the_working_command(self):
        box = Box(self)
        _, lines, _ = box.run(server(self, status(503)).url())
        self.assertNotIn("skein_get_monitor_script", lines[0]["action"])
        self.assertIn("working command", lines[0]["action"])


# The longest single argument Linux passes to a new program (MAX_ARG_STRLEN)
# and the most of the loader the starter reads (monitor_starter.py).
ARG_MAX_ONE = 32 * 4096
STARTER_LOADER_CAP = 1 << 20


@unittest.skipUnless(os.name == "posix", "the POSIX starters")
class StarterRunsTheBytesTest(unittest.TestCase):
    """The served loader is longer than one argument may be; both starters
    run it, because they execute the bytes they downloaded, never pass them
    to a new program. A starter that handed the loader on by argv would fail
    here (E2BIG) as soon as the loader outgrew one argument."""

    def test_the_loader_fits_the_starter_and_not_an_argument(self):
        size = len(harness.served_loader().encode("utf-8"))
        self.assertLess(size, STARTER_LOADER_CAP)
        self.assertGreater(size, ARG_MAX_ONE, "the loader fits one argument: this test proves nothing")

    def test_both_starters_run_the_served_loader(self):
        for form in ("file", "text"):
            with self.subTest(form=form):
                box = Box(self)
                srv = server(self, status(401))
                env = box.env(srv.url())
                if form == "file":
                    argv = [sys.executable, box.starter_file]
                else:
                    argv = [sys.executable, "-c", harness.starter(client=False)]
                    for name in ("SKEIN_MONITOR_MODE", "SKEIN_MONITOR_PREFLIGHT", "SKEIN_MONITOR_CHANNEL",
                                 "SKEIN_MONITOR_BLIND", "SKEIN_CLIENT", "SKEIN_PLATFORM"):
                        env.pop(name, None)
                proc = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
                                      universal_newlines=True, timeout=90)
                lines = [json.loads(l) for l in proc.stdout.splitlines() if l.strip()]
                self.assertEqual(srv.loader_hits, 1, proc.stderr)
                self.assertEqual(srv.script_hits, 1, "the loader did not run: %s" % proc.stderr)
                self.assertEqual([l["reason"] for l in lines], ["token_expired"], proc.stderr)


def loader_namespace():
    """The parts of the client branch of the loader, run in a namespace of
    this process."""
    ns = {"__name__": "loader_client_under_test", "_SKEIN_LOADER_MODE": "client"}
    exec(compile(harness._join_manifest(harness.LOADER_CLIENT_MANIFEST), "loader_client", "exec"), ns)
    return ns


@unittest.skipUnless(os.name == "posix", "the POSIX client loader")
class LiveOwnerTraceTest(unittest.TestCase):
    """Row #129: two points of one trace."""

    def test_a_live_owner_keeps_its_files_under_a_failed_load(self):
        for code in (401, 403):
            with self.subTest(status=code):
                box = Box(self)
                hold_lock(self, box)
                before = box.snapshot()
                _, lines, _ = box.run(server(self, status(code)).url())
                self.assertEqual(lines[0]["status"], code)
                self.assertEqual(box.snapshot(), before)

    def test_without_an_owner_the_loader_ends_it_under_the_lock(self):
        for code, reason in ((401, "token_expired"), (403, "visibility_lost")):
            with self.subTest(status=code):
                box = Box(self)
                ns = loader_namespace()
                env = {"XDG_STATE_HOME": box.state_home, "HOME": box.root}
                seen = {}

                def competitor():
                    # A competing start while the directory is gone and the
                    # lock is still held: it cannot take the lock, and it
                    # changes nothing.
                    try:
                        lock = ns["current_platform"]().acquire_runner_lock(box.token, 0, env)
                    except ns["LockBusy"]:
                        seen["busy"] = True
                        seen["files"] = box.files()
                        return
                    lock.release()
                    seen["busy"] = False
                ns["FINALIZE_POINTS"]["token_dir_removed"] = competitor
                stop = ns["Stop"](reason, status=code, delete_token=True)
                out = []
                ns["emit"] = out.append
                ns["loader_client_end"](stop, box.token, ISSUE, env, TOKEN)
                self.assertEqual(seen, {"busy": True, "files": None})
                self.assertEqual(out[0]["reason"], reason)
                with self.assertRaises(ns["Stop"]) as after:
                    ns["loader_client_read_token"](box.token)
                self.assertEqual(after.exception.reason, "token_file_missing")


if __name__ == "__main__":
    unittest.main()
