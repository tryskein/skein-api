#!/usr/bin/env python3
"""Tests of the agent process and of the identity of the addressee
(runner/proctree.py, runner/identity.py) on synthetic chains of processes
from the fake process source: the rule "addressee and check", items 1-2 of
the spec. Variables of another agent never make it the addressee.

Run on its own: python3 internal/handler/assets/runner_tests/identity_test.py
Standard library of Python 3.6 only.
"""

import base64
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_proc  # noqa: E402
import harness  # noqa: E402

# Synthetic agent records: two by executable name, one by an argument.
REGISTRY = {
    "schemaVersion": 1,
    "general": {"about": "general", "source": "test", "warnings": ["general record used"]},
    "agents": [
        {"id": "alpha", "process": {"executables": ["alpha"]}, "methods": []},
        {"id": "beta", "process": {"executables": ["beta"]}, "methods": []},
        {"id": "gamma", "process": {"argv": ["/opt/gamma/cli.js"]}, "methods": []},
    ],
    "channels": [],
    "shells": {"source": "test", "names": ["sh", "bash", "zsh"]},
    "zavodEngines": {},
}
RUNNER = harness.load_runner(registry=base64.b64encode(json.dumps(REGISTRY).encode("utf-8")).decode("ascii"))
REG = RUNNER.load_registry()

# The runner process, as the bootstrap starts it: the loader source with -c.
RUNNER_PROC = ("python3", ["python3", "-c", "globals().setdefault('_SKEIN_LOADER_MODE', 'client')\n..."])
DAEMON_SIGN = {"executables": ["host-daemon"]}

ENV = {
    "HOST_ADDRESS": "host-1",
    "HERMES_SESSION_ID": "hermes-1",
    "CODEX_THREAD_ID": "thread-1",
    "CODEX_HOME": "/home/u/.codex",
    "OPENCODE_SESSION_ID": "oc-1",
    "PASEO_AGENT_ID": "paseo-1",
    "ZAVOD_ME": "zavod-1",
    "ZAVOD_AGENT_ROOT_START": "start-1",
}
SPECS = [
    RUNNER.AddressSpec("host-x", "HOST_ADDRESS", host=True),
    RUNNER.AddressSpec("hermes", "HERMES_SESSION_ID"),
    RUNNER.AddressSpec("codex", "CODEX_THREAD_ID", qualifiers=["CODEX_HOME"], per_process=True),
    RUNNER.AddressSpec("opencode", "OPENCODE_SESSION_ID", per_process=True),
]


def chain_of(*procs):
    source, pids = fake_proc.linear(RUNNER_PROC, *procs)
    return RUNNER.ancestry(source, pids[0]), pids


def record(client):
    return RUNNER.resolve_record(REG, client)


def place(chain, pids, client):
    return RUNNER.place(chain, REG, record(client), self_pid=pids[0])


def ident(placement, pids, env, specs):
    return RUNNER.identity(placement, REG, env, specs, self_pid=pids[0])


def channels(identity):
    return sorted(a.channel for a in identity.addresses)


class AgentProcessTest(unittest.TestCase):
    """Matrix row #32, item 1."""

    def test_named_record_is_the_nearest_ancestor_with_its_sign(self):
        chain, pids = chain_of("bash", "alpha", "bash", "alpha", "sshd")
        self.assertEqual(place(chain, pids, "alpha").agent.pid, pids[2])
        chain, pids = chain_of("sh", ("node", ["node", "/opt/gamma/cli.js", "--tui"]), "bash")
        self.assertEqual(place(chain, pids, "gamma").agent.pid, pids[2])
        # No ancestor bears the sign: not determined.
        chain, pids = chain_of("bash", "beta")
        self.assertIsNone(place(chain, pids, "alpha").agent)

    def test_general_record_is_the_first_neither_loader_nor_shell(self):
        loader = ("python3", ["python3", "-c", "x = 1\n_SKEIN_LOADER_MODE\n"])
        chain, pids = chain_of(loader, "sh", ("-zsh", ["-zsh"]), "editor-x", "alpha")
        self.assertEqual(place(chain, pids, "unknown-client").agent.pid, pids[4])
        chain, pids = chain_of("sh", "bash")
        self.assertIsNone(place(chain, pids, "unknown-client").agent)

    def test_undetermined_agent_owns_no_host_variable(self):
        chain, pids = chain_of("bash", "beta")
        identity = ident(place(chain, pids, "alpha"), pids, ENV, SPECS)
        self.assertIsNone(identity.process)
        self.assertIn("host-x", identity.inherited)
        self.assertNotIn("host-x", channels(identity))


class NestedAgentTest(unittest.TestCase):
    """Matrix row #32, item 2."""

    def assert_nested(self, chain, pids, client, outer_pid):
        placement = place(chain, pids, client)
        self.assertTrue(placement.nested)
        self.assertEqual(placement.outer.pid, outer_pid)
        identity = ident(placement, pids, ENV, SPECS)
        # Host variables and the inherited Hermes id are the outer agent's;
        # the ids the harness sets on every process are the nested agent's.
        self.assertEqual(channels(identity), ["codex", "opencode"])
        self.assertEqual(sorted(identity.inherited), ["hermes", "host-x"])
        self.assertEqual(identity.process, (pids[2], "s%d" % pids[2]))

    def test_same_record_behind_a_shell(self):
        chain, pids = chain_of("bash", "alpha", "bash", "alpha", "sshd")
        self.assert_nested(chain, pids, "alpha", pids[4])

    def test_other_record_without_a_shell(self):
        # A harness that starts another harness directly.
        chain, pids = chain_of("bash", "alpha", "beta", "sshd")
        self.assert_nested(chain, pids, "alpha", pids[3])

    def test_same_record_in_a_row_is_one_agent(self):
        chain, pids = chain_of("bash", "alpha", "alpha", "alpha", "bash", "sshd")
        placement = place(chain, pids, "alpha")
        self.assertFalse(placement.nested)
        self.assertEqual(placement.agent.pid, pids[2])
        identity = ident(placement, pids, ENV, SPECS)
        self.assertEqual(channels(identity), ["codex", "hermes", "host-x", "opencode"])
        self.assertEqual(identity.inherited, [])

    def test_qualifiers_travel_with_the_address(self):
        chain, pids = chain_of("bash", "alpha", "sshd")
        identity = ident(place(chain, pids, "alpha"), pids, ENV, SPECS)
        codex = [a for a in identity.addresses if a.channel == "codex"][0]
        self.assertEqual(codex.value, "thread-1")
        self.assertEqual(codex.qualifiers, (("CODEX_HOME", "/home/u/.codex"),))


class HostTerminalTest(unittest.TestCase):
    """Matrix row #32: the terminal of a host that launches agents (the ACP
    terminal of Paseo)."""

    SPEC = RUNNER.AddressSpec("paseo", "PASEO_AGENT_ID", host=True, host_sign=DAEMON_SIGN)

    def test_no_agent_process_before_the_daemon(self):
        for client in ("alpha", "unknown-client"):
            with self.subTest(client=client):
                chain, pids = chain_of("bash", "host-daemon", "sshd")
                identity = ident(place(chain, pids, client), pids, ENV, [self.SPEC] + SPECS)
                self.assertEqual(identity.terminal, "paseo")
                self.assertIsNone(identity.process)
                self.assertEqual([(a.channel, a.value) for a in identity.addresses], [("paseo", "paseo-1")])

    def test_agent_process_before_the_daemon_is_nested_in_its_terminal(self):
        chain, pids = chain_of("bash", ("node", ["node", "/opt/gamma/cli.js"]), "bash", "host-daemon")
        placement = place(chain, pids, "gamma")
        self.assertEqual(RUNNER.host_form(placement, REG, DAEMON_SIGN, pids[0]), RUNNER.HOST_FORM_FOREIGN)
        identity = ident(placement, pids, ENV, [self.SPEC])
        self.assertIsNone(identity.terminal)
        self.assertEqual(identity.addresses, [])
        self.assertEqual(identity.inherited, ["paseo"])

    def test_agent_launched_by_the_daemon_owns_its_session(self):
        chain, pids = chain_of("bash", "alpha", "host-daemon")
        placement = place(chain, pids, "alpha")
        self.assertEqual(RUNNER.host_form(placement, REG, DAEMON_SIGN, pids[0]), RUNNER.HOST_FORM_AGENT)
        identity = ident(placement, pids, ENV, [self.SPEC])
        self.assertEqual(channels(identity), ["paseo"])


class HostRootTest(unittest.TestCase):
    """Matrix row #32: a host naming the root of its agent (Zavod)."""

    QUALIFIERS = ["ZAVOD_AGENT_ROOT_PID", "ZAVOD_AGENT_ROOT_START"]

    def spec(self):
        return RUNNER.AddressSpec("zavod", "ZAVOD_ME", qualifiers=self.QUALIFIERS, host=True,
                                  root_pid_env="ZAVOD_AGENT_ROOT_PID")

    def env(self, root_pid):
        env = dict(ENV)
        env["ZAVOD_AGENT_ROOT_PID"] = str(root_pid)
        return env

    def test_root_is_the_agent_or_the_first_agent_under_it(self):
        chain, pids = chain_of("bash", "alpha", "wrapper", "sshd")
        placement = place(chain, pids, "alpha")
        for root in (pids[2], pids[3]):
            with self.subTest(root=root):
                identity = ident(placement, pids, self.env(root), [self.spec()])
                self.assertEqual(channels(identity), ["zavod"])
                self.assertEqual(identity.addresses[0].qualifiers,
                                 (("ZAVOD_AGENT_ROOT_PID", str(root)), ("ZAVOD_AGENT_ROOT_START", "start-1")))

    def test_root_of_another_agent_is_not_ours(self):
        chain, pids = chain_of("bash", "alpha", "bash", "beta", "wrapper")
        placement = place(chain, pids, "alpha")
        self.assertFalse(RUNNER.owns_root(placement, pids[5], REG))
        # A root that is not an ancestor at all, or not a pid.
        chain, pids = chain_of("bash", "alpha", "wrapper")
        placement = place(chain, pids, "alpha")
        for root in ("4242", "x"):
            with self.subTest(root=root):
                env = dict(ENV)
                env["ZAVOD_AGENT_ROOT_PID"] = root
                identity = ident(placement, pids, env, [self.spec()])
                self.assertEqual(identity.addresses, [])
                self.assertEqual(identity.inherited, ["zavod"])


class AncestryTest(unittest.TestCase):

    def test_chain_ends_at_an_unknown_process_and_at_a_loop(self):
        source = fake_proc.FakeProcesses()
        source.add(10, 11, "python3")
        source.add(11, 12, "bash")
        source.add(12, 10, "alpha")
        self.assertEqual([p.pid for p in RUNNER.ancestry(source, 10)], [10, 11, 12])
        source.end(11)
        self.assertEqual([p.pid for p in RUNNER.ancestry(source, 10)], [10])


if __name__ == "__main__":
    unittest.main()
