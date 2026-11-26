#!/usr/bin/env python3
"""The monitor starter on the lowest Python of POSIX (R-9): matrix row
#146, run by the progon kind pyunit36 on a Python 3.6 image (and by
pyunit on the current one).

Under the interpreter that runs this file, in both forms of the starter
(the text of a bootstrap without `client`, the starter.py file of a call
with `client`): it downloads the loader from the fake server, checks its
SHA-256 and hands over control with the mode of its form; each refusal of
the starter refusal table is one stop line, with no traceback. The
expected lines are those of the table (starter_test.py#REFUSALS).

Run on its own: python3 internal/handler/assets/runner_tests/starter_py36_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import starter_test as st  # noqa: E402


@unittest.skipUnless(os.name == "posix", "the POSIX minimum")
class StarterOnThisPythonTest(unittest.TestCase):

    def test_both_forms_load_check_and_hand_over(self):
        srv = st.loader_server(self)
        for form, mode in (("text", None), ("file", "client")):
            with self.subTest(form=form):
                box = st.TokenDir(self)
                rc, lines, out, err = st.run_starter(form, box, srv.url(), st.sha(st.MODE_LOADER))
                self.assertEqual(rc, 0, (out, err))
                self.assertEqual(lines, [{"event": "loader_ran", "mode": mode, "name": "__main__"}])
        self.assertEqual(srv.loader_hits, 2)

    def test_each_refusal_is_one_line_without_a_traceback(self):
        srv = st.loader_server(self)
        for form in st.FORMS:
            cases = [("misconfigured", srv.url(), st.sha(st.MODE_LOADER), ("SKEIN_PUBLIC_URL",)),
                     ("server_unreachable", st.closed_url(), st.sha(st.MODE_LOADER), ()),
                     ("runner_mismatch", srv.url(), st.sha(b"another loader"), ())]
            for reason, url, digest, drop in cases:
                with self.subTest(form=form, reason=reason):
                    box = st.TokenDir(self)
                    before = box.snapshot()
                    rc, lines, out, err = st.run_starter(form, box, url, digest, drop=drop)
                    line, code = st.REFUSALS[reason]
                    self.assertEqual(out, json.dumps(line, separators=(",", ":")) + "\n")
                    self.assertEqual(rc, code)
                    self.assertNotIn("Traceback", err)
                    self.assertEqual(err, "")
                    self.assertEqual(box.snapshot(), before)


if __name__ == "__main__":
    unittest.main()
