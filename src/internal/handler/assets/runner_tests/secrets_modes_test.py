#!/usr/bin/env python3
"""INV-6 in every mode of the monitor: the key of an http channel that the
agent's environment sets never leaves in a journal line or on stdout, when
an event's preview carries it - the journal method (the detached child, the
path of every detached monitor), stream, exit-on-event and the stdout
monitor of a client. The secrets of a monitor are defined in one place
(executors.py#monitor_secrets): the token and, per http record, the key it
would send, when it is long enough to be told from ordinary text.

The expected values come from the spec: INV-6 (the key reaches only the
Authorization header of its request) and the review of wave 12, item 24 (a
key shorter than the lower bound is not redacted as a substring).

Run on its own: python3 internal/handler/assets/runner_tests/secrets_modes_test.py
Standard library of Python 3.6 only.
"""

import base64
import copy
import io
import json
import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402
import start_test as st  # noqa: E402

KEY = "kk_" + "Q" * 30
SHORT = "kk_short"
KEYED = {"id": "keyed", "about": "keyed", "source": "test", "step": 3, "order": 1, "kind": "addressed",
         "blind": False, "detect": {"env": [{"name": "KEYED_ID"}]},
         "executor": {"type": "http", "timeoutSec": 5,
                      "http": {"method": "POST", "url": "https://keyed.example/{env:KEYED_ID}",
                               "keyEnv": ["KEYED_KEY"], "bodyField": "text"}},
         "idle": {"type": "none"}, "outcomes": [{"match": {"httpStatus": 201}, "outcome": "queued"}],
         "probeUndefined": True, "addressee": {"process": "none"}}
REGISTRY = copy.deepcopy(st.REGISTRY)
REGISTRY["channels"] = [KEYED]
REGISTRY_B64 = base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii")
RUNNER = harness.load_runner(registry=REGISTRY_B64)
BODY = harness.render_runner(registry=REGISTRY_B64)
REG = RUNNER.load_registry()


def page(nid, preview):
    p = st.event_page(nid)
    p["notifications"][0]["bodyPreview"] = preview
    return p


def serve(preview, then=None):
    """The first poll an event whose preview carries the key, the next ones
    then (empty pages by default)."""
    served = []

    def respond(h, qs):
        served.append(1)
        if len(served) == 1:
            h.send_json(200, page(1, preview))
        elif then is not None:
            then(h)
        else:
            h.send_json(200, harness.EMPTY)
    return respond


class Box(st.Box):
    """The start box of start_test over the registry with the http record."""

    def env(self, **extra):
        return st.Box.env(self, **dict({"KEYED_KEY": KEY}, **extra))

    def preflight(self, **extra):
        env = self.env(**extra)
        settings = RUNNER.read_settings(env, REG, mode="client")
        return RUNNER.preflight(self.token, env, REG, settings, detector=RUNNER.Detector())

    def start(self, number, **extra):
        run = {"SKEIN_MONITOR_MODE": "run", "SKEIN_MONITOR_PREFLIGHT": str(number)}
        run.update(extra)
        env = self.env(**run)
        lines = []
        spawn = RUNNER.PosixPlatform.spawn_detached

        def spy(*args):
            pid, receipt = spawn(*args)
            self.pids.append(pid)
            return pid, receipt

        out = io.StringIO()
        with mock.patch.dict(os.environ, env, clear=True), \
                mock.patch.object(RUNNER, "LOADER_MODE", "client"), \
                mock.patch.object(RUNNER, "_skein_loader_body", BODY, create=True), \
                mock.patch.object(RUNNER.PosixPlatform, "spawn_detached", staticmethod(spy)), \
                mock.patch.object(RUNNER, "emit", lambda obj: lines.append(json.dumps(obj))), \
                mock.patch("sys.stdout", out):
            code = RUNNER.run()
        self.stdout = "\n".join(lines + out.getvalue().splitlines())
        return code

    def journal_text(self):
        return json.dumps(self.journal_lines())


def stop_on_poll(n):
    real = RUNNER.poll_once
    calls = []

    def poll(*args, **kwargs):
        calls.append(1)
        if len(calls) >= n:
            raise RUNNER.StopSignal()
        return real(*args, **kwargs)
    return mock.patch.object(RUNNER, "poll_once", poll)


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX starts")
class KeyInEveryModeTest(unittest.TestCase):
    """The preview of an event carries the key; no mode prints it or
    journals it."""

    def assert_kept(self, box, delivered):
        self.assertTrue(st.wait_until(delivered), box.journal_lines())
        self.assertNotIn(KEY, box.journal_text())
        self.assertNotIn(KEY, box.stdout)

    def test_the_journal_method_of_a_detached_child(self):
        box = Box(self, serve("leak %s here" % KEY))
        code = box.start(box.preflight()["preflightId"])
        self.assertEqual(code, 0)
        delivered = lambda: [l for l in box.journal_lines() if l.get("event") == "delivery"]
        self.assert_kept(box, delivered)
        self.assertIn(RUNNER.REDACTED, delivered()[0]["bodyPreview"])

    def test_stream(self):
        box = Box(self, serve("leak %s here" % KEY))
        pre = box.preflight(SKEIN_CLIENT="streamer")
        self.assertEqual(pre["method"], "stream")
        with stop_on_poll(2):
            box.start(pre["preflightId"], SKEIN_CLIENT="streamer")
        self.assertIn('"notification"', box.stdout)
        self.assert_kept(box, lambda: True)
        self.assertIn(RUNNER.REDACTED, box.stdout)

    def test_exit_on_event(self):
        box = Box(self, serve("leak %s here" % KEY))
        pre = box.preflight(SKEIN_CLIENT="exiter")
        self.assertEqual(pre["method"], "exit-on-event")
        box.start(pre["preflightId"], SKEIN_CLIENT="exiter")
        self.assertIn('"notification"', box.stdout)
        self.assert_kept(box, lambda: True)
        self.assertIn(RUNNER.REDACTED, box.stdout)

    def test_the_stdout_monitor_of_a_client(self):
        box = Box(self, serve("leak %s here" % KEY, then=lambda h: h.send_json(401, {"error": "unauthorized"})))
        printed = []
        executor = RUNNER.Executor()
        self.addCleanup(executor.stop)
        with mock.patch.dict(os.environ, box.env(), clear=True), \
                mock.patch.object(RUNNER, "emit", lambda obj: printed.append(json.dumps(obj))):
            with self.assertRaises(RUNNER.Stop):
                RUNNER.work(executor, box.server.url(), "skm_" + "S" * 43, st.ISSUE, "", box.token)
        text = "\n".join(printed)
        self.assertIn('"notification"', text)
        self.assertNotIn(KEY, text)
        self.assertIn(RUNNER.REDACTED, text)

    def test_a_short_key_is_not_a_secret_of_texts(self):
        # Below the lower bound the value is ordinary text: it is not cut
        # out of previews and lines.
        self.assertEqual(RUNNER.monitor_secrets("tok", {"KEYED_KEY": SHORT}, REG), ["tok"])
        self.assertEqual(RUNNER.monitor_secrets("tok", {"KEYED_KEY": KEY}, REG), ["tok", KEY])


if __name__ == "__main__":
    unittest.main()
