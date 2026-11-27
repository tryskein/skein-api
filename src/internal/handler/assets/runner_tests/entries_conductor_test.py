#!/usr/bin/env python3
"""SC-001 of the Conductor record (matrix row #98), over the registry the
server embeds.

The expected values come from the Conductor documentation (a closed
product), read 2026-11-27, not from the record:
- conductor.build/docs/reference/environment-variables: CONDUCTOR_SESSION_ID
  is the current session in agent processes; CONDUCTOR_API_URL is set in
  every cloud workspace and not in local ones; CONDUCTOR_API_KEY is the
  user's own key and "takes precedence over the workspace token"
  CONDUCTOR_API_TOKEN;
- conductor.build/docs/api: "Pass your API key as a bearer token:
  Authorization: Bearer <your API key>";
- api.conductor.build/v0/openapi.json: POST /v0/sessions/{sessionId}/messages
  with the body {message}; 201 with messageId, state and deepLink, state one
  of "queued" and "sent"; 400, 401, 404 and a default error otherwise; /me
  lies outside /v0, so the documented "$CONDUCTOR_API_URL/me" makes the URL
  variable the base without /v0.

The API is a test HTTP server on 127.0.0.1 (fake_channel.FakeHTTPChannel).

Run on its own: python3 internal/handler/assets/runner_tests/entries_conductor_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402
import entries_orca_test as eo  # noqa: E402
import failures_test as ft  # noqa: E402
import fake_channel  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG
SESSION = "0b9a4c2e-6d1f-4a8b-9e3c-7f5d2a1b0c9e"
KEY = "cdk_" + "k" * 40
WORKSPACE_TOKEN = "cdt_" + "w" * 40


def created(state):
    return {"status": 201, "json": {"messageId": "4d2c1b0a-9e8f-4a7b-8c6d-5e4f3a2b1c0d", "state": state,
                                    "deepLink": "https://app.conductor.build/s/1"}}


def monitor(world, name, client, procs, **extra):
    """A monitor built as the working runner builds it (its channel
    executor takes the http channels too), over the preflight result."""
    env = world.env(name, client, **extra)
    pl = eo.placement(client, *procs)
    settings = RUNNER.read_settings(env, REG, mode="client")
    line = RUNNER.preflight(env["SKEIN_TOKEN_FILE"], env, REG, settings, detector=RUNNER.RegistryDetector(),
                            prober=eo.CaptureProbe(), placement=pl)
    token = env["SKEIN_TOKEN_FILE"]
    doc = RUNNER.load_preflight(token)
    journal = RUNNER.Journal(token, ft.ISSUE, secrets=[ft.TOKEN], env=env)
    ledger = RUNNER.Ledger(ft.SERVER, name, RUNNER.identity_from_doc(doc["identity"]), env=env,
                           clock=world.clock.now)
    executor = RUNNER.ChannelExecutor(token, env, [ft.TOKEN], doc.get("programs"), REG)
    spy = Calls(executor)
    sink = RUNNER.DeliverySink(journal, token, doc, REG, env, ledger, spy, ft.ISSUE, cancel=dt.FakeCancel(),
                               clock=world.clock)
    mon = ft.Monitor(world, name, sink, journal, doc, spy)
    mon.line = line
    mon.env = env
    return mon


class Calls(object):
    """The channel executor with a count of its calls."""

    def __init__(self, inner):
        self.inner = inner
        self.secrets = inner.secrets
        self.programs = inner.programs
        self.calls = []

    def call(self, spec, text, timeout):
        self.calls.append(spec["type"])
        return self.inner.call(spec, text, timeout)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX pipeline")
class ConductorRecordTest(unittest.TestCase):
    """Row #98 (SC-001, US-5): recognition only with the session, the URL
    and a key; the request; every sign of the table; the cloud warning."""

    def fresh(self):
        self.world = eo.new_world(self)
        # The proxy in front of the API answers 403 to urllib's own
        # signature (conductor.build/docs/api, Tips).
        self.api = fake_channel.FakeHTTPChannel(reject_agent="Python-urllib")
        self.addCleanup(self.api.close)
        self.api.respond([created("queued")])

    def variables(self, **change):
        env = {"CONDUCTOR_SESSION_ID": SESSION, "CONDUCTOR_API_URL": self.api.url,
               "CONDUCTOR_API_KEY": KEY, "CONDUCTOR_API_TOKEN": WORKSPACE_TOKEN}
        env.update(change)
        return env

    def mon(self, name="tok", **change):
        return monitor(self.world, name, "codex", eo.codex_agent(), **self.variables(**change))

    def test_the_request_signs_as_its_own_client(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        [request] = self.api.requests()
        self.assertEqual(request["headers"].get("User-Agent"), "skein-monitor")
        # The default signature would be refused: the fixture is the proxy.
        spec = dict(REG.channel("conductor")["executor"])
        spec["http"] = dict(spec["http"], userAgent="")
        result = RUNNER.HTTPExecutor(self.variables(), []).call(spec, "hello", 5)
        self.assertEqual(result.http_status, 403)

    def test_recognised_only_with_the_session_the_url_and_a_key(self):
        self.fresh()
        mon = self.mon()
        self.assertEqual((mon.doc["method"], mon.doc["channel"]), ("detached", "conductor"))
        for name, change in [("no session", {"CONDUCTOR_SESSION_ID": ""}),
                             ("a local workspace: no URL", {"CONDUCTOR_API_URL": ""}),
                             ("no key", {"CONDUCTOR_API_KEY": "", "CONDUCTOR_API_TOKEN": ""})]:
            with self.subTest(case=name):
                line = self.mon("tok-" + str(len(name)), **change).line
                self.assertEqual((line["method"], line["channel"]), ("journal", None))
        self.assertEqual(self.api.requests(), [])

    def test_the_text_goes_as_a_message_of_the_session(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1)])
        self.assertEqual(mon.statuses(), [(1, "queued")])
        [req] = self.api.requests()
        self.assertEqual(req["method"], "POST")
        self.assertEqual(req["path"], "/v0/sessions/%s/messages" % SESSION)
        body = json.loads(req["body"])
        self.assertEqual(list(body), ["message"])
        self.assertIn('notification="1"', body["message"])
        self.assertEqual(req["headers"].get("Authorization"), "Bearer " + KEY)

    def test_the_workspace_token_when_there_is_no_own_key(self):
        self.fresh()
        mon = self.mon(CONDUCTOR_API_KEY="")
        mon.page([ft.note(1)])
        [req] = self.api.requests()
        self.assertEqual(req["headers"].get("Authorization"), "Bearer " + WORKSPACE_TOKEN)

    def test_every_sign_gives_one_outcome(self):
        cases = [
            ("201 queued", created("queued"), "queued"),
            ("201 sent", created("sent"), "input-written"),
            ("201 of a state of another version", created("delivered"), "unknown"),
            ("200 queued", dict(created("queued"), status=200), "unknown"),
            ("201 without a state", {"status": 201, "json": {"messageId": "m"}}, "unknown"),
            ("400", {"status": 400, "json": {"error": "invalid"}}, "unknown"),
            ("401", {"status": 401, "json": {"error": "unauthorized"}}, "unknown"),
            ("404", {"status": 404, "json": {"error": "not found"}}, "unknown"),
            ("500", {"status": 500, "json": {}}, "unknown"),
        ]
        for i, (name, answer, outcome) in enumerate(cases):
            with self.subTest(case=name):
                self.fresh()
                mon = self.mon()
                self.api.respond([answer])
                mon.page([ft.note(i + 1)], cursor={"ts": "t", "id": i + 1})
                self.assertEqual(mon.statuses(), [(i + 1, outcome)])
                self.assertEqual(len(self.api.requests()), 1)

    def test_the_key_stays_out_of_the_journal_and_the_text(self):
        self.fresh()
        mon = self.mon()
        mon.page([ft.note(1, preview="key %s here" % KEY)])
        [req] = self.api.requests()
        self.assertNotIn(KEY, req["body"])
        journal = json.dumps(mon.lines())
        self.assertTrue(mon.lines())
        self.assertNotIn(KEY, journal)

    def test_the_instruction_warns_of_the_cloud(self):
        self.fresh()
        line = self.mon().line
        self.assertIn("Conductor cloud API", line["instruction"])


if __name__ == "__main__":
    unittest.main()
