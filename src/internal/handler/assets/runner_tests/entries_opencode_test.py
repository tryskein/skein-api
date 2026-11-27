#!/usr/bin/env python3
"""SC-001 of the OpenCode record (matrix row #115), over the registry the
server embeds, with the synthetic channel as the lead decided (T047b): the
shared OpenCode service's registration file gives the address and the Basic
password, and no CLI ever runs (`opencode api` without --server starts the
service).

The expected values come from anomalyco/opencode 2255b89, not from the
record: POST /api/session/:sessionID/synthetic with {text, description,
delivery}, 200 on success with {data: Session.Inbox.Synthetic} (type
synthetic, the sessionID it went to: packages/schema/src/session-inbox.ts),
404 SessionNotFoundError, 409 ConflictError (packages/protocol/src/groups/
session.ts, errors.ts); GET /api/session/:sessionID, 200 with {data:
Session.Info} whose id is the session (packages/schema/src/session.ts);
the registration file
$XDG_STATE_HOME/opencode/service.json, else ~/.local/state/opencode/
service.json, {url, pid, password?} with mode 0600 (packages/client/src/
effect/service.ts, service-probe.ts#fallback); Basic user opencode
(server-connection.ts); shell commands get OPENCODE_SESSION_ID; a
background shell command without timeout wakes the session when it ends.

Run on its own: python3 internal/handler/assets/runner_tests/entries_opencode_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_stream_w1_test as w1  # noqa: E402
import fake_channel  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402

RUNNER = pm.RUNNER
REG = pm.REG

SESSION = "ses_01JD0000000000000000000000"
# The receipt of a synthetic message (session-inbox.ts#Synthetic).
RECEIPT = {"data": {"id": "msg_01JD0000000000000000000001", "sessionID": SESSION,
                    "time": {"created": 1764230400000}, "type": "synthetic",
                    "payload": {"text": "event"}, "delivery": "queue"}}
PASSWORD = "0b4f2c9e"


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class OpenCodeTest(unittest.TestCase):

    def setUp(self):
        self.box = pm.Box(self)
        self.service = fake_channel.FakeHTTPChannel()
        self.addCleanup(self.service.close)
        self.env = self.box.env("opencode", OPENCODE_SESSION_ID=SESSION)

    def register(self, doc=None, mode=0o600):
        directory = os.path.join(self.box.state_home, "opencode")
        os.makedirs(directory, exist_ok=True)
        path = os.path.join(directory, "service.json")
        with open(path, "w") as fh:
            json.dump(doc or {"url": self.service.url, "pid": 4242, "password": PASSWORD}, fh)
        os.chmod(path, mode)

    def line(self, env=None):
        return self.box.preflight(env or self.env, ["opencode"], prober=RUNNER.SystemProbeRunner())

    def test_the_agent_process(self):
        for chain, found in (([("opencode", None)], True), ([("node", ["node", "/usr/lib/opencode/bin/opencode.cjs"])],
                                                            True), ([("opencoder", None)], False)):
            with self.subTest(chain=chain):
                self.assertEqual(w1.agent_of("opencode", chain) is not None, found)

    def test_synthetic_when_the_shared_service_holds_the_session(self):
        self.register()
        self.service.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        line = self.line()
        self.assertEqual((line["method"], line["channel"]), ("detached", "opencode-synthetic"), line)
        probe, = self.service.requests()
        self.assertEqual((probe["method"], probe["path"]), ("GET", "/api/session/" + SESSION))

    def test_exit_on_event_when_the_service_does_not_know_the_tui(self):
        # A TUI with --standalone or --server keeps its session elsewhere:
        # the shared service answers 404; no registered service at all, or
        # a registration file others may read, is no known server either.
        for name, prepare in (("session not found", lambda: (self.register(), self.service.respond(
                                  [{"status": 404, "json": {"_tag": "SessionNotFoundError"}}]))),
                              ("a 200 without the session", lambda: (self.register(), self.service.respond(
                                  [{"status": 200, "json": {}}]))),
                              ("a 200 of another session", lambda: (self.register(), self.service.respond(
                                  [{"status": 200, "json": {"data": {"id": "ses_01JD0000000000000000000099"}}}]))),
                              ("no registration", lambda: None),
                              ("registration open to others", lambda: self.register(mode=0o644))):
            with self.subTest(case=name):
                self.box = pm.Box(self)
                self.env = self.box.env("opencode", OPENCODE_SESSION_ID=SESSION)
                prepare()
                line = self.line()
                self.assertEqual(line["method"], "exit-on-event", line)
                self.assertIn("background: true", line["instruction"])
                self.assertIn("no timeout", line["instruction"])

    def test_the_body_keeps_its_fields_whatever_the_text(self):
        self.register()
        spec = REG.channel("opencode-synthetic")["executor"]
        text = 'a "quote", a closing "} and\na new line -- --resume'
        self.service.respond([{"status": 200, "json": RECEIPT}])
        result = RUNNER.HTTPExecutor(self.env, []).call(spec, text, 10)
        request = self.service.requests()[0]
        self.assertEqual((request["method"], request["path"]), ("POST", "/api/session/%s/synthetic" % SESSION))
        body = json.loads(request["body"])
        self.assertEqual(set(body), {"text", "description", "delivery"})
        self.assertEqual((body["text"], body["delivery"]), (text, "queue"))
        self.assertEqual(request["headers"]["Authorization"],
                         "Basic " + base64.b64encode(("opencode:" + PASSWORD).encode()).decode())
        self.assertEqual(RUNNER.classify_result(REG.channel("opencode-synthetic")["outcomes"], result)[0], "queued")

    def test_each_sign_gives_one_outcome(self):
        # Only the success the source declares, a 200 with the receipt of a
        # synthetic message (its id and session), is queued; a 200 whose
        # body is empty, broken or another item's is a damaged receipt:
        # unknown. That the session is the addressed one is the check's: the
        # GET before the POST compares its id.
        # Statuses the source does not declare (201, 202, 204) are unknown.
        rows = REG.channel("opencode-synthetic")["outcomes"]
        for name, status, doc, outcome in (
                ("the receipt", 200, RECEIPT, "queued"),
                ("no session in the item", 200, {"data": dict((k, v) for k, v in RECEIPT["data"].items()
                                                             if k != "sessionID")}, "unknown"),
                ("no id of the item", 200, {"data": dict((k, v) for k, v in RECEIPT["data"].items()
                                                        if k != "id")}, "unknown"),
                ("an empty body", 200, None, "unknown"),
                ("an empty object", 200, {}, "unknown"),
                ("no data", 200, {"ok": True}, "unknown"),
                ("a user item", 200, {"data": dict(RECEIPT["data"], type="user")}, "unknown"),
                ("a session instead of an item", 200, {"data": {"id": SESSION}}, "unknown"),
                ("201", 201, RECEIPT, "unknown"),
                ("202", 202, RECEIPT, "unknown"),
                ("204", 204, None, "unknown"),
                ("not found", 404, {"_tag": "SessionNotFoundError"}, "refused-before-send"),
                ("conflict", 409, {}, "unknown"),
                ("server error", 500, {}, "unknown"),
                ("unauthorized", 401, {}, "unknown")):
            with self.subTest(case=name):
                self.assertEqual(RUNNER.classify(rows, http_status=status, doc=doc)[0], outcome)

    def test_the_driver_of_a_delivery_reads_the_receipt(self):
        self.register()
        channel = REG.channel("opencode-synthetic")
        driver = RUNNER.ArgvDriver(channel, RUNNER.ChannelExecutor(self.box.token, self.env, [pm.TOKEN],
                                                                   None, REG))
        guard = mock.Mock()
        guard.endpoint = None
        guard.call_timeout.return_value = 10
        for answer, outcome in ((RECEIPT, "queued"), ({"data": {"id": SESSION}}, "unknown"), ({}, "unknown")):
            with self.subTest(outcome=outcome, answer=answer):
                self.service.respond([{"status": 200, "json": answer}])
                self.assertEqual(driver.send("event", guard)[0], outcome)

    def test_a_broken_or_cut_body_of_a_200_is_unknown(self):
        self.register()
        spec = REG.channel("opencode-synthetic")["executor"]
        rows = REG.channel("opencode-synthetic")["outcomes"]
        whole = json.dumps(RECEIPT).encode("utf-8")
        for name, raw in (("broken", b"{\"data\": {\"type\": \"synthetic\","), ("empty", b""),
                          ("not JSON", b"queued"), ("cut", whole[:len(whole) // 2])):
            with self.subTest(case=name):
                self.service.respond([{"status": 200, "raw": raw}])
                result = RUNNER.HTTPExecutor(self.env, []).call(spec, "event", 10)
                self.assertEqual(RUNNER.classify_result(rows, result)[0], "unknown")

    def test_the_check_before_a_send_finds_the_session(self):
        self.register()
        sink = mock.Mock()
        sink.identity = {"addresses": [{"channel": "opencode-synthetic", "env": "OPENCODE_SESSION_ID",
                                        "value": SESSION}]}
        sink.env = self.env
        sink.executor.secrets = []
        sink.doc = {"endpoints": {"opencode-synthetic": self.service.url}}
        channel = REG.channel("opencode-synthetic")
        self.service.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        RUNNER.DeliverySink.check_addressee(sink, channel)
        # Only a 200 that names this session is the same addressee: any
        # other body is no session found.
        for answer in ({"status": 404, "json": {}}, {"status": 200, "json": {}},
                       {"status": 200, "raw": b""}, {"status": 200, "raw": b"{\"data\": "},
                       {"status": 200, "json": {"data": {"id": "ses_01JD0000000000000000000099"}}},
                       {"status": 204, "raw": b""}):
            with self.subTest(answer=answer):
                self.service.respond([answer])
                with self.assertRaises(RUNNER.RefusedBeforeSend):
                    RUNNER.DeliverySink.check_addressee(sink, channel)

    def sink(self):
        sink = mock.Mock()
        sink.identity = {"addresses": [{"channel": "opencode-synthetic", "env": "OPENCODE_SESSION_ID",
                                        "value": SESSION}]}
        sink.env = self.env
        sink.executor.secrets = []
        sink.doc = {"endpoints": {"opencode-synthetic": self.service.url}}
        return sink

    def test_the_preflight_keeps_the_end_of_the_service(self):
        self.register()
        self.service.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        line = self.line()
        self.assertEqual(line["channel"], "opencode-synthetic", line)
        self.assertEqual(RUNNER.load_preflight(self.box.token)["endpoints"],
                         {"opencode-synthetic": self.service.url})

    def test_another_end_than_the_preflight_s_is_another_addressee(self):
        # The service registered again elsewhere between the preflight and
        # the attempt: nothing goes there.
        other = fake_channel.FakeHTTPChannel()
        self.addCleanup(other.close)
        self.register({"url": other.url, "pid": 4243, "password": "c0ffee11"})
        other.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        with self.assertRaises(RUNNER.AddresseeChanged):
            RUNNER.DeliverySink.check_addressee(self.sink(), REG.channel("opencode-synthetic"))
        self.assertEqual(other.requests(), [])

    def test_the_check_and_the_request_of_an_attempt_go_to_one_end(self):
        # One read of the registration file per attempt: a registration
        # published again after the check does not move the request, nor
        # its password, to the new end.
        self.register()
        channel = REG.channel("opencode-synthetic")
        guard = mock.Mock()
        guard.endpoint = None
        guard.call_timeout.return_value = 10
        self.service.respond([{"status": 200, "json": {"data": {"id": SESSION}}},
                              {"status": 200, "json": {"data": {"id": SESSION}}}, {"status": 200, "json": RECEIPT}])
        RUNNER.DeliverySink.check_addressee(self.sink(), channel, guard=guard)
        other = fake_channel.FakeHTTPChannel()
        self.addCleanup(other.close)
        self.register({"url": other.url, "pid": 4243, "password": "c0ffee11"})
        RUNNER.DeliverySink.check_addressee(self.sink(), channel, guard=guard)
        driver = RUNNER.ArgvDriver(channel, RUNNER.ChannelExecutor(self.box.token, self.env, [pm.TOKEN], None, REG))
        self.assertEqual(driver.send("event", guard)[0], "queued")
        self.assertEqual(other.requests(), [])
        methods = [(r["method"], r["headers"]["Authorization"]) for r in self.service.requests()]
        basic = "Basic " + base64.b64encode(("opencode:" + PASSWORD).encode()).decode()
        self.assertEqual(methods, [("GET", basic), ("GET", basic), ("POST", basic)])

    def test_a_password_registered_after_the_start_becomes_a_secret(self):
        self.register({"url": self.service.url, "pid": 4242, "password": "n3w"})
        self.service.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        sink = self.sink()
        RUNNER.DeliverySink.check_addressee(sink, REG.channel("opencode-synthetic"))
        self.assertIn("n3w", sink.executor.secrets)
        sink.journal.add_secret.assert_called_with("n3w")

    def test_the_probes_of_the_preflight_keep_the_password_of_the_record_out(self):
        # The password of the record's endpoint file is a secret of every
        # probe: a variable that holds it does not reach a probe program.
        self.register()
        self.service.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        seen = []
        real = RUNNER.SystemProbeRunner

        def spy(secrets, *args, **kw):
            seen.append(list(secrets))
            return real(secrets, *args, **kw)
        with mock.patch.object(RUNNER, "SystemProbeRunner", spy):
            placement, _ = pm.placement(REG, self.env["SKEIN_CLIENT"], ["opencode"])
            settings = RUNNER.read_settings(self.env, REG, mode="client")
            RUNNER.preflight(self.env["SKEIN_TOKEN_FILE"], self.env, REG, settings,
                             detector=RUNNER.RegistryDetector(pm.Binder()), placement=placement)
        self.assertEqual(len(seen), 1)
        self.assertIn(PASSWORD, seen[0])
        self.assertNotIn("SAME_AS_PASSWORD", RUNNER.child_env(dict(self.env, SAME_AS_PASSWORD=PASSWORD), seen[0]))

    def test_no_opencode_process_runs_on_any_path(self):
        self.register()
        spawned = []
        real = RUNNER.subprocess.Popen

        def watch(argv, *args, **kw):
            spawned.append(argv)
            return real(argv, *args, **kw)
        self.service.respond([{"status": 200, "json": {"data": {"id": SESSION}}}])
        with mock.patch.object(RUNNER.subprocess, "Popen", watch):
            self.line()
            RUNNER.HTTPExecutor(self.env, []).call(REG.channel("opencode-synthetic")["executor"], "event", 10)
        self.assertFalse([a for a in spawned if "opencode" in os.path.basename(str(a[0]))], spawned)

    def test_opencode_in_paseo_is_woken_by_paseo(self):
        self.register()
        fake_channel.FakeProgram(self.box.bin, "paseo")
        env = self.box.env("opencode", OPENCODE_SESSION_ID=SESSION,
                           PASEO_AGENT_ID="0f8fad5b-d9cb-469f-a165-70867728950e")
        line = self.box.preflight(env, ["opencode", "Paseo Daemon"], shell_between=False)
        self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"), line)


if __name__ == "__main__":
    unittest.main()
