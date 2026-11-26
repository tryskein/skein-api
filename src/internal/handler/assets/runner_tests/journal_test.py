#!/usr/bin/env python3
"""Tests of the monitor journal (runner/journal.py): no secret in it, the
fields of a line, private files, rotation at 1 MiB with at most 5 files, and
the start probe of its directory.

Run on its own: python3 internal/handler/assets/runner_tests/journal_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

RUNNER = harness.load_runner()
TOKEN = "skm_" + "J" * 43
MIB = 1 << 20


@unittest.skipUnless(os.name == "posix", "the POSIX journal")
class JournalTest(unittest.TestCase):
    """Matrix row #27 (INV-10, INV-6, "Journal")."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-journal-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.env = {"XDG_STATE_HOME": os.path.join(self.root, "state"), "HOME": self.root}
        token_dir = os.path.join(self.root, "skein-mon-zz-77-0123456789abcdef")
        os.mkdir(token_dir, 0o700)
        self.token_file = os.path.join(token_dir, "token")
        self.journal = RUNNER.Journal(self.token_file, "zz-77", secrets=[TOKEN], env=self.env)

    def lines(self, path=None):
        with open(path or self.journal.path) as fh:
            return [json.loads(line) for line in fh.read().splitlines()]

    def test_token_in_any_field_is_not_written(self):
        note = {"id": 41, "type": "comment.created", "entity": {"type": "issue", "id": "zz-77"},
                "actor": {"username": "user " + TOKEN}, "actorSessionId": "s-" + TOKEN,
                "commentId": "c-1", "bodyPreview": "my token is " + TOKEN + " ok", "truncated": True}
        self.journal.delivery(note, "failed", method="detached", channel="zavod",
                              addressee={"zavod": TOKEN}, answered=False, reason="refused " + TOKEN)
        self.journal.lifecycle("degraded", cause="channel_failing", detail=[TOKEN, {"k": TOKEN}])
        self.journal.summary_cursor_reset({"ts": TOKEN, "id": 3})
        with open(self.journal.path, "rb") as fh:
            data = fh.read()
        self.assertEqual(len(data.splitlines()), 3)
        self.assertNotIn(TOKEN.encode("ascii"), data)

    def test_token_in_a_key_is_not_written(self):
        # A key that json escapes (non-ASCII, a quote, a backslash) around the
        # token: no raw form of it may reach the file either.
        details = {"\u043a\u043b\u044e\u0447 \"%s\\" % TOKEN: "v", "nested": {TOKEN: [TOKEN]}}
        self.journal.lifecycle("degraded", cause="channel_failing", details=details)
        with open(self.journal.path, "rb") as fh:
            data = fh.read()
        self.assertNotIn(TOKEN.encode("ascii"), data)
        line = json.loads(data.decode("utf-8"))

        def strings(value):
            if isinstance(value, dict):
                for k, v in value.items():
                    yield k
                    for s in strings(v):
                        yield s
            elif isinstance(value, list):
                for v in value:
                    for s in strings(v):
                        yield s
            elif isinstance(value, str):
                yield value
        self.assertFalse([s for s in strings(line) if TOKEN in s], line)

    def test_delivery_line_carries_the_journal_fields(self):
        note = {"id": 41, "type": "comment.created", "entity": {"type": "issue", "id": "zz-77"},
                "actor": {"username": "alice"}, "actorSessionId": "sess-1", "commentId": "c-1",
                "bodyPreview": "hello", "truncated": True}
        self.journal.delivery(note, "delivered", method="detached", channel="zavod",
                              addressee="agent-1", answered=True, reason=None)
        deleted = {"id": 42, "type": "comment.created", "entity": {"type": "issue", "id": "zz-77"},
                   "actor": {"username": "bob"}, "commentId": "c-2"}
        self.journal.delivery(deleted, "delivered", method="detached", channel="zavod",
                              addressee="agent-1", answered=True)
        self.journal.delivery(note, "not_delivered", reason="access_lost", with_preview=False)
        first, gone, lost = self.lines()
        for key, want in (("event", "delivery"), ("monitor", "skein-mon-zz-77-0123456789abcdef"),
                          ("issue", "zz-77"), ("notificationId", 41), ("method", "detached"),
                          ("channel", "zavod"), ("addressee", "agent-1"), ("status", "delivered"),
                          ("channelAnswered", True), ("type", "comment.created"), ("issueId", "zz-77"),
                          ("author", "alice"), ("actorSessionId", "sess-1"), ("commentId", "c-1"),
                          ("bodyPreview", "hello"), ("truncated", True)):
            self.assertEqual(first.get(key), want, key)
        self.assertIn("ts", first)
        self.assertIn("reason", first)
        # A deleted comment: no text, the mark of deletion.
        self.assertNotIn("bodyPreview", gone)
        self.assertIs(gone.get("deleted"), True)
        # After access is lost: no preview at all.
        self.assertNotIn("bodyPreview", lost)
        self.assertEqual(lost.get("reason"), "access_lost")

    def test_summary_records(self):
        # A rejected cursor and a capped read-ahead after rootClosed leave
        # events without a line of their own; each is one summary line.
        self.journal.summary_cursor_reset({"ts": "2026-01-01T00:00:05Z", "id": 5})
        self.journal.summary_read_limited(20)
        reset, limited = self.lines()
        for line in (reset, limited):
            self.assertEqual(line["event"], "summary")
            self.assertEqual(line["monitor"], "skein-mon-zz-77-0123456789abcdef")
            self.assertEqual(line["issue"], "zz-77")
            self.assertIn("ts", line)
        self.assertEqual(reset["summary"], "cursor_reset")
        self.assertEqual(reset["cursor"], {"ts": "2026-01-01T00:00:05Z", "id": 5})
        self.assertEqual(limited["summary"], "read_limited")
        self.assertEqual(limited["pages"], 20)

    def test_files_are_private(self):
        self.journal.lifecycle("monitor_started")
        self.assertEqual(os.stat(self.journal.path).st_mode & 0o777, 0o600)
        self.assertEqual(os.stat(os.path.dirname(self.journal.path)).st_mode & 0o777, 0o700)

    def test_rotation_keeps_five_files_of_at_most_one_mib(self):
        filler = "x" * (100 * 1024)
        written = 0
        # Enough lines for more than six files of 1 MiB.
        while written < 7 * MIB:
            self.journal.lifecycle("degraded", seq=written, pad=filler)
            written += len(filler)
        directory = os.path.dirname(self.journal.path)
        files = sorted(f for f in os.listdir(directory) if f.startswith(os.path.basename(self.journal.path)))
        self.assertEqual(len(files), 5, files)
        for name in files:
            self.assertLessEqual(os.path.getsize(os.path.join(directory, name)), MIB, name)
        kept = []
        for name in files:
            kept.extend(line["seq"] for line in self.lines(os.path.join(directory, name)))
        # The oldest lines were pushed out by the sixth file; the newest is kept.
        self.assertNotIn(0, kept)
        self.assertIn(written - len(filler), [line["seq"] for line in self.lines()])

    def test_start_probe(self):
        with self.subTest(case="directory made when absent"):
            path = RUNNER.probe_journal_dir(self.env)
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o700)
        with self.subTest(case="an existing private directory is not refused"):
            self.assertEqual(RUNNER.probe_journal_dir(self.env), path)
        with self.subTest(case="a directory open to others is config_invalid"):
            os.chmod(path, 0o755)
            with self.assertRaises(RUNNER.JournalUnavailable) as ctx:
                RUNNER.probe_journal_dir(self.env)
            self.assertEqual(ctx.exception.stop_reason, "config_invalid")
            self.assertEqual(ctx.exception.reason, "open_to_others")


if __name__ == "__main__":
    unittest.main()
