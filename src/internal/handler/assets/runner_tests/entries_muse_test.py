#!/usr/bin/env python3
"""SC-001 of the Muse Code record (matrix row #117; row #117b does not apply:
the review of T049 found no method - the tool of Muse's Monitor and its
parameters are not published in dev.meta.ai/docs/muse-code nor in
muse-code-sdk e6ef4a6), over the registry the server embeds.

The record is "a host or the journal": recognised by its executable muse,
a host that launched it delivers, else the journal only (method journal
with the journal path); its source is the review.

Run on its own: python3 internal/handler/assets/runner_tests/entries_muse_test.py
Standard library of Python 3.6 only.
"""

import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import entries_journal_only_test as jo  # noqa: E402
import entries_stream_w1_test as w1  # noqa: E402
import preflight_matrix_test as pm  # noqa: E402

REG = pm.REG


@unittest.skipUnless(sys.platform.startswith("linux"), "the POSIX preflight")
class MuseTest(unittest.TestCase):

    def test_recognition(self):
        self.assertIsNotNone(w1.agent_of("muse", [("muse", None)]))
        self.assertIsNotNone(w1.agent_of("muse", [("muse.exe", None)]))
        self.assertIsNone(w1.agent_of("muse", [("musescore", None)]))

    def test_no_own_method_and_the_source_of_the_review(self):
        record = REG.agents["muse"]
        self.assertEqual(record["methods"], [])
        for part in ("dev.meta.ai/docs/muse-code", "muse-code-sdk e6ef4a6", "not stated"):
            self.assertIn(part, record["source"])

    def test_without_a_host_the_journal(self):
        box = pm.Box(self)
        jo.check_journal_line(self, box.preflight(box.env("muse"), ["muse"]))

    def test_the_journal_runner_writes_the_events(self):
        # Row #117 by the product path: no method found, so the working
        # command runs the journal runner, which writes each event.
        jo.check_journal_run(self, "muse")

    def test_with_a_host_that_launched_it_the_host(self):
        box = pm.Box(self)
        jo.fake_channel.FakeProgram(box.bin, "paseo")
        env = box.env("muse", PASEO_AGENT_ID=jo.PASEO_ID)
        line = box.preflight(env, ["muse", "Paseo Daemon"], shell_between=False)
        self.assertEqual((line["method"], line["channel"]), ("detached", "paseo"), line)


if __name__ == "__main__":
    unittest.main()
