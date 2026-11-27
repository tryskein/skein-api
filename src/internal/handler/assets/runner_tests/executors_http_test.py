#!/usr/bin/env python3
"""Tests of the http executor of a channel (runner/executors.py, type
http, with its wiring into the delivery pipeline and the preflight):
matrix row #97.

The channel is a synthetic record of step 5 whose request goes to a test
HTTP server on 127.0.0.1 (fake_channel.FakeHTTPChannel); a second server
is where a redirect points. The monitor is the DeliverySink of the working
runner over the real ledger, journal and channel executor, with a virtual
clock. The key is the value of a variable of the agent process (the first
of the record's keyEnv that is set).

Run on its own: python3 internal/handler/assets/runner_tests/executors_http_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import socket
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as delivery  # noqa: E402
import fake_channel  # noqa: E402
import harness  # noqa: E402

TOKEN = "skm_" + "H" * 43
KEY = "cdk_" + "K" * 40
TOKEN_KEY = "cdt_" + "W" * 40
SERVER = "https://skein.example"
ISSUE = "sk-9"
CURSOR = {"ts": "2026-01-01T00:00:00Z", "id": 7}
# A session id with characters a path must escape: it stays one element.
SESSION = "s 1/../x?y"
SESSION_IN_PATH = "s%201%2F..%2Fx%3Fy"
TABLE = [{"match": {"httpStatus": 201, "json": {"state": "queued"}}, "outcome": "queued"},
         {"match": {"httpStatus": 201, "json": {"state": "sent"}}, "outcome": "input-written"},
         {"match": {"httpStatus": 409}, "outcome": "refused-before-send"}]


def http_channel(timeout=2, key_env=("HOST_KEY", "HOST_TOKEN")):
    rec = {"id": "H", "about": "http host", "source": "test", "step": 5, "order": 1, "kind": "addressed",
           "blind": False, "detect": {"env": [{"name": "HOST_SESSION"}]},
           "executor": {"type": "http", "timeoutSec": timeout,
                        "http": {"method": "POST",
                                 "url": "{env:HOST_URL}/v0/sessions/{env:HOST_SESSION}/messages",
                                 "bodyField": "message"}},
           "idle": {"type": "none"}, "outcomes": TABLE, "addressee": {"process": "none"},
           "address": {"env": "HOST_SESSION"}, "probeUndefined": True}
    if key_env:
        rec["executor"]["http"]["keyEnv"] = list(key_env)
    return rec


def argv_channel():
    """A channel program next to the http channel: it must not see a key."""
    return {"id": "P", "about": "program", "source": "test", "step": 3, "order": 1, "kind": "addressed",
            "blind": False, "detect": {},
            "executor": {"type": "argv", "argv": ["chan-p"], "feed": "stdin", "timeoutSec": 2},
            "idle": {"type": "none"}, "outcomes": [{"match": {"exitCode": 0}, "outcome": "queued"}],
            "addressee": {"process": "none"}, "binaries": {"linux": ["chan-p"], "macos": ["chan-p"]},
            "probeUndefined": True}


def http_channel_b():
    """The next channel of the preflight after the http one: a program."""
    rec = argv_channel()
    rec.update({"id": "B", "executor": {"type": "argv", "argv": ["chan-b"], "feed": "stdin", "timeoutSec": 2},
                "binaries": {"linux": ["chan-b"], "macos": ["chan-b"]}})
    return rec


def note(i, preview="hello there"):
    return {"id": i, "type": "comment_added", "entity": {"type": "issue", "id": ISSUE},
            "actor": {"username": "bob"}, "actorSessionId": "s-1", "commentId": "c%d" % i,
            "bodyPreview": preview, "truncated": False}


class Calls(object):
    """The channel executor with a record of every call: the executor
    type, the virtual time and the timeout the pipeline gave it."""

    def __init__(self, inner, clock):
        self.inner = inner
        self.clock = clock
        self.secrets = inner.secrets
        self.programs = inner.programs
        self.calls = []

    def call(self, spec, text, timeout):
        self.calls.append((spec["type"], self.clock.now(), timeout))
        return self.inner.call(spec, text, timeout)


class World(object):
    """A user directory, the test server of the channel, the runner loaded
    with the channels and the agent's environment."""

    def __init__(self, test, channels=None):
        self.root = tempfile.mkdtemp(prefix="skein-http-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.bin = os.path.join(self.root, "bin")
        os.mkdir(self.bin, 0o700)
        self.server = fake_channel.FakeHTTPChannel()
        test.addCleanup(self.server.close)
        self.server.respond([{"status": 201, "json": {"messageId": "m-1", "state": "queued", "deepLink": "x"}}])
        self.env = fake_channel.with_path({"PATH": os.environ.get("PATH", os.defpath), "HOME": self.root,
                                           "XDG_STATE_HOME": os.path.join(self.root, "state"),
                                           "HOST_URL": self.server.url, "HOST_SESSION": SESSION,
                                           "HOST_KEY": KEY, "HOST_TOKEN": TOKEN_KEY}, self.bin)
        self.clock = delivery.Clock()
        registry = {"schemaVersion": 1, "general": {"about": "g", "source": "t", "warnings": ["w"]},
                    "agents": [], "channels": channels or [http_channel()],
                    "shells": {"source": "t", "names": ["sh"]}, "zavodEngines": {}}
        self.runner = harness.load_runner(registry=base64.b64encode(json.dumps(registry).encode()).decode())
        self.reg = self.runner.load_registry()

    def monitor(self, channel="H", fallbacks=(), idle=None):
        token_dir = os.path.join(self.root, "tok")
        if not os.path.isdir(token_dir):
            os.mkdir(token_dir, 0o700)
        token = os.path.join(token_dir, "token")
        with open(token, "w") as fh:
            fh.write(TOKEN)
        os.chmod(token, 0o600)
        identity = {"process": None, "addresses": [{"channel": channel, "value": SESSION, "qualifiers": []}],
                    "inherited": [], "terminal": None}
        doc = {"method": "detached", "channel": channel, "fallbacks": list(fallbacks), "identity": identity,
               "preflightId": 1}
        r = self.runner
        journal = r.Journal(token, ISSUE, secrets=[TOKEN], env=self.env)
        ledger = r.Ledger(SERVER, "tok", r.identity_from_doc(identity), env=self.env, clock=self.clock.now)
        calls = Calls(r.ChannelExecutor(token, self.env, [TOKEN], None, self.reg), self.clock)
        sink = r.DeliverySink(journal, token, doc, self.reg, self.env, ledger, calls, ISSUE,
                              cancel=delivery.FakeCancel(), clock=self.clock, idle_window_sec=idle)
        return delivery.Monitor(self, sink, calls, journal, token)

    def journal_text(self, mon):
        try:
            with open(mon.journal.path) as fh:
                return fh.read()
        except OSError:
            return ""


class RequestFromTheRecordTest(unittest.TestCase):
    """Row #97: the method, the URL and the body come from the record; the
    key comes from the first variable of keyEnv that is set and goes in the
    Authorization header; the answer is classified by the status and the
    JSON field of the body."""

    def test_the_request_is_the_record_s(self):
        world = World(self)
        mon = world.monitor()
        mon.page([note(1, "please look")])
        reqs = world.server.requests()
        self.assertEqual(len(reqs), 1)
        self.assertEqual(reqs[0]["method"], "POST")
        self.assertEqual(reqs[0]["path"], "/v0/sessions/%s/messages" % SESSION_IN_PATH)
        body = json.loads(reqs[0]["body"])
        self.assertEqual(list(body), ["message"])
        self.assertIn("please look", body["message"])
        self.assertEqual(reqs[0]["headers"].get("Authorization"), "Bearer " + KEY)
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "H")])

    def test_the_second_key_variable_when_the_first_is_unset(self):
        world = World(self)
        world.env["HOST_KEY"] = ""
        mon = world.monitor()
        mon.page([note(1)])
        self.assertEqual(world.server.requests()[0]["headers"].get("Authorization"), "Bearer " + TOKEN_KEY)

    def test_each_answer_gives_one_outcome(self):
        cases = [("201 queued", {"status": 201, "json": {"state": "queued"}}, "queued"),
                 ("201 sent", {"status": 201, "json": {"state": "sent"}}, "input-written"),
                 ("201 of an unfamiliar state", {"status": 201, "json": {"state": "delivered"}}, "unknown"),
                 ("200 queued", {"status": 200, "json": {"state": "queued"}}, "unknown"),
                 ("401", {"status": 401, "json": {"error": "unauthorized"}}, "unknown"),
                 ("500", {"status": 500, "json": {}}, "unknown")]
        for name, answer, outcome in cases:
            with self.subTest(case=name):
                world = World(self)
                world.server.respond([answer])
                mon = world.monitor()
                mon.page([note(1)])
                self.assertEqual(len(world.server.requests()), 1)
                self.assertEqual([d[:3] for d in mon.deliveries()], [(1, outcome, "H")])


class RedirectTest(unittest.TestCase):
    """Row #97, the class "the key does not go to another address": a 3xx to
    another host is the answer — no request is sent again, the other
    server gets nothing, and the outcome is unknown (not repeated)."""

    def test_a_redirect_to_another_host_is_not_followed(self):
        for status in (301, 302, 307, 308):
            with self.subTest(status=status):
                world = World(self)
                other = fake_channel.FakeHTTPChannel()
                self.addCleanup(other.close)
                other.respond([{"status": 201, "json": {"state": "queued"}}])
                target = other.url.replace("127.0.0.1", "localhost") + "/v0/sessions/x/messages"
                world.server.respond([{"status": status, "headers": {"Location": target}, "json": {}}])
                mon = world.monitor()
                mon.page([note(1)])
                self.assertEqual(other.requests(), [])
                self.assertEqual(len(world.server.requests()), 1)
                self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "unknown", "H")])


class KeyIsASecretTest(unittest.TestCase):
    """Row #97, INV-6: the key reaches only the Authorization header of its
    request — not the text of a delivery, not the journal, not the argument
    list or the environment of a channel program."""

    def test_a_text_that_carries_the_key_sends_it_redacted(self):
        world = World(self)
        mon = world.monitor()
        mon.page([note(1, "leak %s and %s here" % (KEY, TOKEN))])
        body = world.server.requests()[0]["body"]
        self.assertNotIn(KEY, body)
        self.assertNotIn(TOKEN, body)
        self.assertIn("leak", json.loads(body)["message"])
        journal = world.journal_text(mon)
        self.assertTrue(journal, "the journal holds the delivery")
        self.assertNotIn(KEY, journal)

    def test_a_refused_request_does_not_journal_the_key(self):
        world = World(self)
        world.server.respond([{"status": 409, "json": {"detail": "busy"}}])
        mon = world.monitor()
        mon.page([note(1, "key %s" % KEY)])
        journal = world.journal_text(mon)
        self.assertTrue(journal)
        self.assertNotIn(KEY, journal)

    def test_a_channel_program_does_not_see_the_key(self):
        world = World(self, [http_channel(), argv_channel()])
        prog = fake_channel.FakeProgram(world.bin, "chan-p", watch=("HOST_KEY", "HOST_TOKEN"))
        world.env["KEY_COPY"] = KEY
        mon = world.monitor(channel="P")
        mon.page([note(1, "key %s" % KEY)])
        calls = prog.calls()
        self.assertEqual(len(calls), 1)
        marker = json.dumps(calls[0])
        self.assertNotIn(KEY, marker)
        self.assertNotIn(TOKEN_KEY, marker)
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "P")])


def http_workers(runner):
    return [t for t in threading.enumerate() if t.name == runner.HTTP_WORKER_NAME and t.is_alive()]


class BudgetTest(unittest.TestCase):
    """Row #97: a request is bounded by its timeout within the idle window;
    a request that runs out is unknown and its thread ends; a request that
    could not reach the server is a refusal before send, repeated within the
    budget of INV-8 like any refusal; unknown is never repeated and never
    moves to another channel."""

    def test_a_request_past_its_timeout_is_unknown_and_its_thread_ends(self):
        world = World(self, [http_channel(timeout=1)])
        # The server takes the request and never answers.
        world.server.respond([{"hang": True}])
        mon = world.monitor()
        mon.page([note(1)])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "unknown", "H")])
        self.assertEqual(len(mon.spy.calls), 1)
        # The thread of the request ends by its deadline plus one socket
        # wait: nothing can still go out later.
        done = threading.Event()

        def ended():
            while http_workers(world.runner):
                time.sleep(0.02)
            done.set()
        threading.Thread(target=ended, daemon=True).start()
        self.assertTrue(done.wait(1 + 2), "the thread of the request outlived its deadline")

    def test_the_idle_window_bounds_a_longer_timeout(self):
        # The channel's own timeout (30 s) is longer than the idle window
        # (1 s): the request ends with the window, unknown.
        world = World(self, [http_channel(timeout=30)])
        world.server.respond([{"hang": True}])
        mon = world.monitor(idle=1)
        page = threading.Thread(target=mon.page, args=([note(1)],), daemon=True)
        page.start()
        page.join(1 + 2)
        self.assertFalse(page.is_alive(), "the request outlived the idle window")
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "unknown", "H")])

    def test_a_closed_port_is_refused_before_send_within_the_budget(self):
        world = World(self, [http_channel(), http_channel_b()])
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()
        world.env["HOST_URL"] = "http://127.0.0.1:%d" % port
        fake_channel.FakeProgram(world.bin, "chan-b").respond([{"exit": 0}])
        mon = world.monitor(fallbacks=("B",))
        mon.page([note(1)])
        # At most four calls (INV-8), then the next channel of the preflight.
        self.assertEqual([kind for kind, _, _ in mon.spy.calls], ["http"] * 4 + ["argv"])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "queued", "B")])

    def test_unknown_is_not_repeated_and_moves_to_no_other_channel(self):
        world = World(self, [http_channel(), http_channel_b()])
        world.server.respond([{"status": 500, "json": {"error": "boom"}}])
        program = fake_channel.FakeProgram(world.bin, "chan-b")
        mon = world.monitor(fallbacks=("B",))
        mon.page([note(1)])
        self.assertEqual([kind for kind, _, _ in mon.spy.calls], ["http"])
        self.assertEqual(program.calls(), [])
        self.assertEqual([d[:3] for d in mon.deliveries()], [(1, "unknown", "H")])


class AvailabilityTest(unittest.TestCase):
    """Row #97: the channel is available only when the variables of its URL
    and of its key are set and the URL is https or http of localhost; no
    request is made otherwise."""

    def available(self, world):
        ctx = world.runner.Context(world.reg, None, world.env, None, None, None)
        return world.runner.channel_available(ctx, "H")

    def test_the_variables_of_url_and_key(self):
        world = World(self)
        self.assertEqual(self.available(world), (True, None))
        for name, change in [("no URL", {"HOST_URL": ""}), ("no session", {"HOST_SESSION": ""}),
                             ("no key", {"HOST_KEY": "", "HOST_TOKEN": ""}),
                             ("http of another host", {"HOST_URL": "http://api.example.com"}),
                             ("another scheme", {"HOST_URL": "ftp://127.0.0.1"}),
                             # The authority by the standard parse (urlsplit).
                             ("user information before localhost", {"HOST_URL": "http://localhost:80@evil.example"}),
                             ("user information before 127.0.0.1",
                              {"HOST_URL": "http://127.0.0.1:80@evil.example"}),
                             ("https without a host", {"HOST_URL": "https://"}),
                             ("a port that is no number", {"HOST_URL": "https://api.example.com:abc"}),
                             ("a port out of range", {"HOST_URL": "https://api.example.com:99999"}),
                             ("a URL that cannot be parsed", {"HOST_URL": "http://["}),
                             ("a base with a query", {"HOST_URL": "https://api.example.com/?x=1"}),
                             ("a base with a fragment", {"HOST_URL": "https://api.example.com/#f"})]:
            with self.subTest(case=name):
                world.env.update(change)
                ok, cause = self.available(world)
                self.assertFalse(ok)
                self.assertNotIn(KEY, cause)
                world.env.update({"HOST_URL": world.server.url, "HOST_SESSION": SESSION,
                                  "HOST_KEY": KEY, "HOST_TOKEN": TOKEN_KEY})
        self.assertEqual(world.server.requests(), [])

    def test_a_url_that_is_not_https_is_refused_before_send(self):
        world = World(self)
        world.env["HOST_URL"] = "http://api.example.com"
        executor = world.runner.HTTPExecutor(world.env, [TOKEN])
        with self.assertRaises(world.runner.ExecutorRefusal):
            executor.call(http_channel()["executor"], "hello", 2)


if __name__ == "__main__":
    unittest.main()
