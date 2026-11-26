#!/usr/bin/env python3
"""The monitor on the lowest Python of POSIX (R-9, P-10, SC-004): matrix
row #87, run by the progon kind pyunit36 on a Python 3.6 image (and by
pyunit on the current one).

Under the interpreter that runs this file: the runner body and the served
loader are assembled and executed; a call without `client` runs its
bootstrap starter -> loader -> runner, streams a notification line and
stops with the protocol's stop line and exit code; a call with `client`
runs the starter file -> client loader -> preflight -> the working command
of the preflight line -> a detached start that writes the event to the
journal. The server is a fake one serving the real loader and body.

Run on its own: python3 internal/handler/assets/runner_tests/compat_py36_test.py
Standard library of Python 3.6 only.
"""

import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

ISSUE = "zz-87"
TOKEN = "skm_" + "C" * 43


def sha(data):
    return hashlib.sha256(data).hexdigest()


class Server(object):
    """The fake server: the served loader, the runner body, and polls that
    answer with the queued pages, then empty pages, or 401 once revoked."""

    def __init__(self, test):
        self.body = harness.render_runner().encode("utf-8")
        self.loader = harness.served_loader().encode("utf-8")
        self.pages = []
        self.polls = 0
        self.revoked = False
        self.lock = threading.Lock()
        self.srv = harness.LoaderServer(lambda h: h.send_body(200, self.body), self.poll,
                                        lambda h: h.send_body(200, self.loader))
        test.addCleanup(self.srv.stop)

    def poll(self, h):
        with self.lock:
            self.polls += 1
            if self.revoked:
                h.send_body(401, b"{}")
                return
            page = self.pages.pop(0) if self.pages else harness.EMPTY
        h.send_body(200, json.dumps(page).encode("utf-8"))

    def url(self):
        return self.srv.url()


def event(nid):
    return {"id": nid, "type": "comment_added", "entity": {"type": "issue", "id": ISSUE},
            "actor": {"username": "bob"}, "actorSessionId": "s-1", "commentId": "c%d" % nid,
            "bodyPreview": "hello", "truncated": False}


class Box(object):
    """A private token directory with the token and, for a call with
    client, the starter.py prepare writes; a state home."""

    def __init__(self, test):
        self.root = tempfile.mkdtemp(prefix="skein-py36-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.dir = os.path.join(self.root, "skein-mon-zz-87-0123456789abcdef")
        os.mkdir(self.dir, 0o700)
        self.token = os.path.join(self.dir, "token")
        with open(self.token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(self.token, 0o600)
        self.starter = os.path.join(self.dir, "starter.py")
        with open(self.starter, "w") as fh:
            fh.write(harness.starter(client=True))
        os.chmod(self.starter, 0o600)
        self.state_home = os.path.join(self.root, "state")

    def env(self, server, **extra):
        env = dict((k, v) for k, v in os.environ.items() if not k.startswith(("SKEIN_", "ZAVOD_", "TMUX")))
        env.update({"HOME": self.root, "XDG_STATE_HOME": self.state_home, "SKEIN_PUBLIC_URL": server.url(),
                    "SKEIN_ISSUE": ISSUE, "SKEIN_TOKEN_FILE": self.token, "SKEIN_EXCLUDE_SESSION": "",
                    "SKEIN_RUNNER_SHA256": sha(server.body), "SKEIN_LOADER_SHA256": sha(server.loader)})
        env.update(extra)
        return env


def read_lines(stream, out, done):
    for line in iter(stream.readline, ""):
        if line.strip():
            out.append(json.loads(line))
    done.set()


def wait_until(pred, sec=60):
    deadline = time.time() + sec
    while time.time() < deadline:
        if pred():
            return True
        time.sleep(0.05)
    return False


@unittest.skipUnless(os.name == "posix", "the POSIX minimum")
class BodyTest(unittest.TestCase):
    """The parts assemble and run on this interpreter."""

    def test_the_body_and_the_loader_run_here(self):
        runner = harness.load_runner()
        self.assertTrue(runner.load_registry().agents)
        compile(harness.served_loader(), "loader", "exec")
        compile(harness.starter(client=False), "starter", "exec")
        compile(harness.starter(client=True), "starter", "exec")


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX minimum")
class WithoutClientTest(unittest.TestCase):
    """The call without client: bootstrap with the starter, stream, stop."""

    def test_stream_and_the_stop_line(self):
        server = Server(self)
        box = Box(self)
        proc = subprocess.Popen([sys.executable, "-c", harness.starter(client=False)], env=box.env(server),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True,
                                start_new_session=True)
        self.addCleanup(lambda: proc.poll() is None and os.killpg(proc.pid, signal.SIGKILL))
        lines, done = [], threading.Event()
        threading.Thread(target=read_lines, args=(proc.stdout, lines, done), daemon=True).start()
        self.assertTrue(wait_until(lambda: lines), "no line")
        self.assertEqual(lines[0]["event"], "monitor_started", lines)
        self.assertTrue(wait_until(lambda: server.polls >= 1), "no poll")
        with server.lock:
            server.pages.append({"notifications": [event(5)], "cursor": {"ts": "t", "id": 5}})
        self.assertTrue(wait_until(lambda: any(l.get("event") == "notification" for l in lines)), lines)
        note = [l for l in lines if l.get("event") == "notification"][0]
        self.assertEqual(note["notification"]["id"], 5)
        os.killpg(proc.pid, signal.SIGTERM)
        self.assertTrue(done.wait(60))
        proc.wait(60)
        self.assertEqual((lines[-1]["event"], lines[-1]["reason"]), ("paused", "stopped"))
        self.assertEqual(proc.returncode, 0)
        self.assertTrue(os.path.exists(box.token))


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX minimum")
class WithClientTest(unittest.TestCase):
    """The call with client: starter file, client loader, preflight, the
    working command, a detached start that journals the event."""

    def test_preflight_working_command_and_detached_journal(self):
        server = Server(self)
        box = Box(self)
        env = box.env(server, SKEIN_CLIENT="no-such-agent", SKEIN_PLATFORM="posix", SKEIN_MONITOR_MODE="preflight")
        proc = subprocess.Popen([sys.executable, box.starter], env=env, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, universal_newlines=True)
        out, err = proc.communicate(timeout=60)
        self.assertEqual(proc.returncode, 0, err)
        pre = json.loads(out.strip().splitlines()[-1])
        self.assertEqual((pre["event"], pre["method"]), ("preflight", "journal"), pre)
        self.addCleanup(self.revoke, server, box)
        run = subprocess.Popen(["sh", "-c", pre["command"]], env=env, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, universal_newlines=True)
        out, err = run.communicate(timeout=60)
        self.assertEqual(run.returncode, 0, err)
        started = json.loads(out.strip().splitlines()[-1])
        self.assertEqual((started["event"], started["method"]), ("monitor_started", "journal"), started)
        self.assertTrue(wait_until(lambda: server.polls >= 1), "the detached runner never polled")
        with server.lock:
            server.pages.append({"notifications": [event(9)], "cursor": {"ts": "t", "id": 9}})
        journal = pre["journal"]

        def delivered():
            try:
                with open(journal) as fh:
                    return [json.loads(l) for l in fh if l.strip() and json.loads(l).get("event") == "delivery"]
            except OSError:
                return []

        self.assertTrue(wait_until(lambda: delivered()), "the event never reached the journal")
        self.assertEqual(delivered()[0]["notificationId"], 9)

    def revoke(self, server, box):
        """End the detached runner: its token answers 401, it stops and
        removes the token."""
        server.revoked = True
        wait_until(lambda: not os.path.exists(box.token), 30)


if __name__ == "__main__":
    unittest.main()
