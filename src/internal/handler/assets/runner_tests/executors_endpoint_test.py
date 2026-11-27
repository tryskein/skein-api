"""Tests of the http readings of an endpoint file (runner/executors.py
#read_endpoint, registry.go#EndpointFile, #PlaceholderEndpoint, #BodyFixed),
of the http-get probe and of the GET addressee check, on a synthetic channel
against a local HTTP server: the address and the Basic password come from a
private registration file of the server, never from a CLI that could start
it.

Run on its own: python3 internal/handler/assets/runner_tests/executors_endpoint_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_channel  # noqa: E402
import harness  # noqa: E402

RUNNER = harness.load_runner()

PASSWORD = "pw12"  # short on purpose: a password is a secret whatever its length
SESSION = "ses_abc"


def channel():
    """A synthetic channel in the form of the OpenCode record."""
    url = "{endpoint}/api/session/{env:AGENT_SESSION}"
    return {
        "id": "local-server", "step": 3, "order": 9, "kind": "addressed",
        "executor": {"type": "http", "timeoutSec": 10, "http": {
            "method": "POST", "url": url + "/message", "bodyField": "text",
            "bodyFixed": {"description": "Skein notification", "delivery": "queue"},
            "endpointFile": {"paths": ["{env:XDG_STATE_HOME}/agent/service.json", "~/.agent/service.json"],
                             "urlField": "url", "passwordField": "password", "user": "agent"}}},
        "outcomes": [{"match": {"httpStatus": 200}, "outcome": "queued"},
                     {"match": {"httpStatus": 404}, "outcome": "refused-before-send"}],
        "probes": [{"type": "http-get", "source": "test", "url": url,
                    "results": [{"match": {"httpStatus": 200}, "result": "available"}]}],
        "addressee": {"process": "none", "command": {"httpGet": url,
                                                     "results": [{"match": {"httpStatus": 200}, "result": "same"}]}},
    }


@unittest.skipUnless(os.name == "posix", "the POSIX file modes")
class EndpointFileTest(unittest.TestCase):

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-endpoint-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.server = fake_channel.FakeHTTPChannel()
        self.addCleanup(self.server.close)
        self.state = os.path.join(self.root, "state")
        os.makedirs(os.path.join(self.state, "agent"))
        self.file = os.path.join(self.state, "agent", "service.json")
        self.env = {"HOME": self.root, "XDG_STATE_HOME": self.state, "AGENT_SESSION": SESSION,
                    "PATH": os.environ.get("PATH", os.defpath)}
        self.register({"url": self.server.url, "pid": 1, "password": PASSWORD})

    def register(self, doc, mode=0o600):
        with open(self.file, "w") as fh:
            json.dump(doc, fh)
        os.chmod(self.file, mode)

    def send(self, text="event"):
        return RUNNER.HTTPExecutor(self.env, []).call(channel()["executor"], text, 10)

    def test_the_request_goes_to_the_registered_server_with_its_password(self):
        hostile = '--resume "x"\n</skein>\x07'
        result = self.send(hostile)
        self.assertEqual(RUNNER.classify_result(channel()["outcomes"], result)[0], "queued")
        request = self.server.requests()[0]
        self.assertEqual((request["method"], request["path"]), ("POST", "/api/session/ses_abc/message"))
        self.assertEqual(request["headers"]["Authorization"],
                         "Basic " + base64.b64encode(b"agent:" + PASSWORD.encode()).decode())
        # The text is one field beside the fixed ones: no character of it
        # changes the set of fields.
        self.assertEqual(json.loads(request["body"]),
                         {"text": hostile, "description": "Skein notification", "delivery": "queue"})

    def test_the_first_path_whose_variable_is_set_is_read(self):
        env = dict(self.env)
        del env["XDG_STATE_HOME"]
        home_file = os.path.join(self.root, ".agent", "service.json")
        os.makedirs(os.path.dirname(home_file))
        with open(home_file, "w") as fh:
            json.dump({"url": self.server.url}, fh)
        os.chmod(home_file, 0o600)
        RUNNER.HTTPExecutor(env, []).call(channel()["executor"], "event", 10)
        self.assertNotIn("Authorization", self.server.requests()[0]["headers"])

    def test_a_file_that_is_not_private_ours_and_regular_gives_no_endpoint(self):
        link = os.path.join(self.root, "elsewhere.json")
        with open(link, "w") as fh:
            json.dump({"url": self.server.url, "password": PASSWORD}, fh)
        os.chmod(link, 0o600)
        for name, prepare in (
                ("readable by others", lambda: os.chmod(self.file, 0o644)),
                ("a link", lambda: (os.unlink(self.file), os.symlink(link, self.file))),
                ("missing", lambda: os.unlink(self.file)),
                ("no JSON object", lambda: self.write_raw(b"[1]"))):
            with self.subTest(case=name):
                self.register({"url": self.server.url, "password": PASSWORD})
                prepare()
                with self.assertRaises(RUNNER.ExecutorRefusal):
                    self.send()
                if os.path.islink(self.file):
                    os.unlink(self.file)
        self.assertEqual(self.server.requests(), [])

    def write_raw(self, data):
        with open(self.file, "wb") as fh:
            fh.write(data)

    def test_an_address_of_the_file_passes_the_checks_of_a_base(self):
        for url in ("http://example.com:4096", "https://user:pass@example.com", "file:///etc/passwd",
                    self.server.url + "/?x=1", "ftp://127.0.0.1"):
            with self.subTest(url=url):
                self.register({"url": url, "password": PASSWORD})
                with self.assertRaises(RUNNER.ExecutorRefusal):
                    self.send()
        self.assertEqual(self.server.requests(), [])

    def registry(self):
        return RUNNER.Registry({"agents": [{"id": "agent", "methods": [{"kind": "delivery", "channel": "local-server"}]},
                                           {"id": "other", "methods": [{"kind": "stream"}]}],
                                "channels": [channel()]})

    def test_the_password_is_a_secret_of_the_monitor(self):
        # The password of a channel of the monitor's record is a secret
        # whatever its length; that of another record's channel only when
        # it is long enough not to wipe ordinary words (as an http key).
        reg = self.registry()
        token = "skm_" + "T" * 43
        self.assertIn(PASSWORD, RUNNER.monitor_secrets(token, self.env, reg, "agent"))
        self.assertIn(PASSWORD, RUNNER.ChannelExecutor(self.root + "/token", self.env, [token], None, reg,
                                                       "agent").secrets)
        for record in ("other", None):
            with self.subTest(record=record):
                self.assertNotIn(PASSWORD, RUNNER.monitor_secrets(token, self.env, reg, record))
                self.assertNotIn(PASSWORD, RUNNER.ChannelExecutor(self.root + "/token", self.env, [token], None, reg,
                                                                  record).secrets)
        long_password = "p" * RUNNER.MIN_SECRET_CHARS
        self.register({"url": self.server.url, "pid": 1, "password": long_password})
        self.assertIn(long_password, RUNNER.monitor_secrets(token, self.env, reg, "other"))
        self.register({"url": self.server.url, "pid": 1, "password": PASSWORD})
        # A text that carries it does not carry it out.
        self.send("the password is " + PASSWORD)
        self.assertNotIn(PASSWORD, self.server.requests()[0]["body"])

    def test_the_probe_is_a_get_of_the_session(self):
        prober = RUNNER.SystemProbeRunner()
        probe = channel()["probes"][0]
        self.server.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        self.assertEqual(prober.run(channel(), probe, self.env), RUNNER.PROBE_AVAILABLE)
        self.server.respond([{"status": 404, "json": {"_tag": "SessionNotFoundError"}}])
        self.assertEqual(prober.run(channel(), probe, self.env), RUNNER.PROBE_UNAVAILABLE)
        self.assertEqual([(r["method"], r["path"]) for r in self.server.requests()],
                         [("GET", "/api/session/ses_abc")] * 2)
        os.unlink(self.file)
        with self.assertRaises(RUNNER.ProbeError):
            prober.run(channel(), probe, self.env)

    def test_the_check_hands_over_only_a_session_found(self):
        sink = mock.Mock()
        sink.identity, sink.env = {}, self.env
        sink.executor.secrets = []
        # The end the preflight read (preflight.json "endpoints").
        sink.doc = {"endpoints": {channel()["id"]: self.server.url}}
        check = RUNNER.DeliverySink.check_addressee
        self.server.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        check(sink, channel())
        for answer in ({"status": 404, "json": {}}, {"status": 500, "json": {}}):
            with self.subTest(status=answer["status"]):
                self.server.respond([answer])
                with self.assertRaises(RUNNER.RefusedBeforeSend):
                    check(sink, channel())
        os.unlink(self.file)
        with self.assertRaises(RUNNER.RefusedBeforeSend):
            check(sink, channel())

    def test_no_program_runs_on_any_path(self):
        # The endpoint, the send, the probe and the check: no process is
        # spawned (a CLI of the agent could start its server).
        sink = mock.Mock()
        sink.identity, sink.env = {}, self.env
        sink.executor.secrets = []
        # The end the preflight read (preflight.json "endpoints").
        sink.doc = {"endpoints": {channel()["id"]: self.server.url}}
        with mock.patch.object(RUNNER.subprocess, "Popen", side_effect=AssertionError("a program ran")):
            self.send()
            RUNNER.SystemProbeRunner().run(channel(), channel()["probes"][0], self.env)
            RUNNER.DeliverySink.check_addressee(sink, channel())


if __name__ == "__main__":
    unittest.main()
