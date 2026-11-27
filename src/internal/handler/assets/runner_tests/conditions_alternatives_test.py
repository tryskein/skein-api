"""Tests of the method conditions envAbsent, settingsNot and anyOf
(runner/preflight.py#conditions_hold, registry.go#Conditions) on synthetic
conditions: a variable that must be unset, a setting that must not carry a
value (held when it is read and differs, or when the file or the key is
absent; not held when the file is not read), and alternatives of which one
must hold besides the other conditions.

Run on its own: python3 internal/handler/assets/runner_tests/conditions_alternatives_test.py
Standard library of Python 3.6 only.
"""

import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

RUNNER = harness.load_runner()

OFF = [{"path": "~/agent/config.yaml", "format": "yaml-scalar", "key": "display.notify", "equals": v}
       for v in ("off", False)]


class ConditionsTest(unittest.TestCase):

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-conds-")
        self.addCleanup(shutil.rmtree, self.root, True)
        os.mkdir(os.path.join(self.root, "agent"))
        self.env = {"HOME": self.root, "PATH": os.environ.get("PATH", os.defpath)}

    def config(self, text):
        with open(os.path.join(self.root, "agent", "config.yaml"), "w") as fh:
            fh.write(text)

    def holds(self, conds, env=None):
        return RUNNER.conditions_hold(conds, env if env is not None else self.env)[0]

    def test_a_variable_that_must_be_unset(self):
        conds = {"envAbsent": ["AGENT_MODE"]}
        self.assertTrue(self.holds(conds))
        self.assertTrue(self.holds(conds, dict(self.env, AGENT_MODE="")))
        self.assertFalse(self.holds(conds, dict(self.env, AGENT_MODE="queue")))

    def test_a_setting_that_must_not_carry_a_value(self):
        conds = {"settingsNot": OFF}
        for name, text, holds in (
                ("another value", "display:\n  notify: all\n", True),
                ("the string off", "display:\n  notify: \"off\"\n", False),
                # YAML 1.1: a bare off is the boolean false.
                ("the boolean false", "display:\n  notify: off\n", False),
                ("no such key", "display:\n  compact: true\n", True),
                ("no such section", "model: x\n", True),
                ("a form not read", "display: {notify: all}\n", False)):
            with self.subTest(case=name):
                self.config(text)
                self.assertEqual(self.holds(conds), holds)

    def test_a_setting_that_must_not_carry_a_value_holds_without_the_file(self):
        self.assertTrue(self.holds({"settingsNot": OFF}))
        # The positive reading of the same absent setting does not hold.
        self.assertFalse(self.holds({"settings": [dict(OFF[0], equals="all")]}))

    def test_one_alternative_holding_is_enough(self):
        # A variable decides when it is set; the file only without it.
        conds = {"anyOf": [
            {"env": [{"name": "AGENT_MODE", "pattern": "[Qq]ueue"}]},
            {"envAbsent": ["AGENT_MODE"],
             "settings": [{"path": "~/agent/config.yaml", "format": "yaml-scalar", "key": "display.mode",
                           "equals": "queue"}]},
        ]}
        self.config("display:\n  mode: queue\n")
        self.assertTrue(self.holds(conds))
        self.assertTrue(self.holds(conds, dict(self.env, AGENT_MODE="Queue")))
        # The variable set to another value overrides the file.
        self.assertFalse(self.holds(conds, dict(self.env, AGENT_MODE="interrupt")))
        self.config("display:\n  mode: interrupt\n")
        self.assertFalse(self.holds(conds))

    def test_alternatives_add_to_the_other_conditions(self):
        conds = {"env": [{"name": "AGENT_SESSION"}],
                 "anyOf": [{"env": [{"name": "A"}]}, {"env": [{"name": "B"}]}]}
        self.assertFalse(self.holds(conds, dict(self.env, A="1")))
        self.assertTrue(self.holds(conds, dict(self.env, A="1", AGENT_SESSION="s")))
        self.assertTrue(self.holds(conds, dict(self.env, B="1", AGENT_SESSION="s")))
        self.assertFalse(self.holds(conds, dict(self.env, AGENT_SESSION="s")))


if __name__ == "__main__":
    unittest.main()
