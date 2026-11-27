#!/usr/bin/env python3
"""The address of a record is its id with its qualifiers ("Terms" of the
spec: each address with its qualifiers; INV-1, INV-4), over the registry
the server embeds: where the source names the server, socket, port or
workspace that serves the id, one id under two of them is two addressees,
and a server that changed between the preflight and a transfer is another
addressee - nothing is handed over (the key of an http channel never goes
to another origin).

The qualifiers come from the sources: Conductor CONDUCTOR_API_URL (the base
of its API), Paradev SUPERSET_WORKSPACE_ID (the workspace of the terminal),
cmux CMUX_SOCKET_PATH (the socket of the app), Sculptor SCULPT_API_PORT
(the port of its API).

Run on its own: python3 internal/handler/assets/runner_tests/address_qualifiers_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402
import failures_test as ft  # noqa: E402

RUNNER = ft.RUNNER
REG = ft.REG

# (record, its address variable, its id, the qualifier, two servers)
CASES = [
    ("conductor", "CONDUCTOR_SESSION_ID", "sess-1", "CONDUCTOR_API_URL",
     ("https://a.conductor.example", "https://b.conductor.example")),
    ("paradev", "SUPERSET_TERMINAL_ID", "term-1", "SUPERSET_WORKSPACE_ID", ("ws-a", "ws-b")),
    ("cmux-message", "CMUX_SURFACE_ID", "surface:1", "CMUX_SOCKET_PATH", ("/tmp/cmux-a.sock", "/tmp/cmux-b.sock")),
    ("cmux-paste", "CMUX_SURFACE_ID", "surface:1", "CMUX_SOCKET_PATH", ("/tmp/cmux-a.sock", "/tmp/cmux-b.sock")),
    ("sculptor", "SCULPT_AGENT_ID", "agent-1", "SCULPT_API_PORT", ("5050", "5051")),
]


class Calls(object):
    """An executor that records each call and hands nothing over."""

    secrets = []
    programs = None
    strict = False

    def __init__(self):
        self.calls = []

    def call(self, spec, text, timeout):
        self.calls.append(spec)
        raise RUNNER.ExecutorRefusal("not called in this test")


def address_of(record, env):
    """The Address the record's address declaration gives in env."""
    spec = RUNNER.AddressSpec(record, REG.channel(record)["address"]["env"],
                              qualifiers=REG.channel(record)["address"].get("qualifiers") or ())
    return RUNNER._address(spec, env)


class AddressQualifierTest(unittest.TestCase):

    def test_one_id_under_two_servers_is_two_addressees(self):
        for record, var, value, qualifier, (a, b) in CASES:
            with self.subTest(record=record):
                one = address_of(record, {var: value, qualifier: a})
                other = address_of(record, {var: value, qualifier: b})
                self.assertNotEqual(one.key(), other.key())
                self.assertEqual(one.key(), address_of(record, {var: value, qualifier: a}).key())

    def sink(self, record, var, value, qualifier, saved, current):
        """The sink of a monitor whose preflight saw the server saved and
        whose environment now names current; the agent is this process."""
        world = ft.World(self)
        env = dict(world.env("tok", "aider"), **{var: value, qualifier: current})
        token = env["SKEIN_TOKEN_FILE"]
        quals = REG.channel(record)["address"]["qualifiers"]
        identity = {"process": [os.getpid(), ft.start_of(os.getpid())], "inherited": [], "terminal": None,
                    "addresses": [{"channel": record, "value": value, "env": var,
                                   "qualifiers": [[q, saved if q == qualifier else env.get(q, "")] for q in quals]}]}
        doc = {"method": "detached", "channel": record, "fallbacks": [], "identity": identity, "preflightId": 1}
        journal = RUNNER.Journal(token, ft.ISSUE, secrets=[ft.TOKEN], env=env)
        ledger = RUNNER.Ledger(ft.SERVER, "tok", RUNNER.identity_from_doc(identity), env=env, clock=world.clock.now)
        calls = Calls()
        sink = RUNNER.DeliverySink(journal, token, doc, REG, env, ledger, calls, ft.ISSUE, cancel=dt.FakeCancel(),
                                   clock=world.clock)
        return world, sink, journal, doc, calls

    def test_the_check_before_a_transfer_compares_the_server(self):
        for record, var, value, qualifier, (a, b) in CASES:
            with self.subTest(record=record):
                _, sink, _, _, _ = self.sink(record, var, value, qualifier, a, a)
                sink.check_addressee(REG.channel(record))
                _, sink, _, _, _ = self.sink(record, var, value, qualifier, a, b)
                with self.assertRaises(RUNNER.AddresseeChanged):
                    sink.check_addressee(REG.channel(record))

    def test_the_key_does_not_go_to_another_origin(self):
        world, sink, journal, doc, calls = self.sink(*CASES[0][:4], saved=CASES[0][4][0], current=CASES[0][4][1])
        mon = ft.Monitor(world, "tok", sink, journal, doc, calls)
        mon.page([ft.note(1)])
        self.assertEqual(calls.calls, [])
        self.assertEqual(mon.statuses(), [(1, "unknown")])


if __name__ == "__main__":
    unittest.main()
