#!/usr/bin/env python3
"""Tests of stream and exit-on-event with client (runner/modes_stream.py):
matrix rows #40, #41, #42.

The runner is loaded in process; the poll goes through a fake executor (the
boundary of the request process), time through a fake clock, and stdout
into a buffer that records each write. The journal and the state file are
the real ones, in a temporary state home and a private token directory.

Run on its own: python3 internal/handler/assets/runner_tests/modes_stream_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

RUNNER = harness.load_runner()
ISSUE = "sk-40"
PREFLIGHT_ID = 4
COMMAND_MARK = "SKEIN_MONITOR_MODE=run"


def event(nid, preview="text"):
    return {"id": nid, "type": "comment_added", "entity": {"type": "issue", "id": ISSUE, "title": "a title"},
            "actor": {"username": "bob"}, "commentId": "c-%d" % nid, "bodyPreview": preview,
            "summary": "bob commented"}


def page(ids, cursor_id, root_closed=False):
    out = {"notifications": [event(i) for i in ids], "cursor": {"ts": "2026-01-01T00:00:%02dZ" % cursor_id,
                                                               "id": cursor_id}}
    if root_closed:
        out["rootClosed"] = True
    return out


class FakeClock(object):
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now

    def sleep(self, sec):
        self.now += max(sec, 0)


class Out(object):
    """stdout that records every write (one write is one batch) and the time
    of the fake clock it happened at."""

    def __init__(self, clock=None, ops=None):
        self.writes = []
        self.clock = clock
        self.ops = ops

    def write(self, text):
        self.writes.append((self.clock() if self.clock else None, text))
        if self.ops is not None:
            self.ops.append("write")

    def flush(self):
        if self.ops is not None:
            self.ops.append("flush")

    def lines(self):
        return [json.loads(l) for _, t in self.writes for l in t.splitlines() if l]


class FakeExecutor(object):
    """Answers each poll with the next page; records the URLs."""

    def __init__(self, answers):
        self.answers = list(answers)
        self.urls = []

    def request(self, url, token):
        self.urls.append(url)
        if not self.answers:
            return {"status": 401}
        answer = self.answers.pop(0)
        if isinstance(answer, dict):
            return {"status": 200, "body": json.dumps(answer)}
        return {"status": answer}

    def stop(self):
        pass


class Box(object):
    def __init__(self, test):
        self.root = tempfile.mkdtemp(prefix="skein-stream-")
        test.addCleanup(shutil.rmtree, self.root, True)
        self.token_dir = os.path.join(self.root, "skein-mon-stream")
        os.mkdir(self.token_dir, 0o700)
        self.token = os.path.join(self.token_dir, "token")
        with open(self.token, "w") as fh:
            fh.write("skm_" + "T" * 43)
        os.chmod(self.token, 0o600)
        self.env = {"HOME": self.root, "XDG_STATE_HOME": os.path.join(self.root, "state")}
        self.journal = RUNNER.Journal(self.token, ISSUE, secrets=["skm_" + "T" * 43], env=self.env)

    def output(self, limits, method, clock=None, out=None):
        clock = clock or FakeClock()
        pacer = RUNNER.Pacer(RUNNER.StreamLimits(limits), clock=clock, sleep=clock.sleep)
        return RUNNER.StreamOutput(RUNNER.StreamLimits(limits), self.journal, method, self.journal.path,
                                   out=out or Out(clock), pacer=pacer)

    def journal_lines(self):
        try:
            with open(self.journal.path) as fh:
                return [json.loads(l) for l in fh if l.strip()]
        except FileNotFoundError:
            return []

    def cycle(self, executor, output, method):
        """One run of the loop until its stop; returns the stop, after
        finish() printed its line through the output."""
        doc = {"method": method, "preflightId": PREFLIGHT_ID}
        with mock.patch.object(RUNNER.time, "sleep", lambda s: None):
            try:
                RUNNER.run_stream_loop(executor, "http://poll.test", "skm_x", ISSUE, "", self.token, doc, output)
            except RUNNER.Stop as stop:
                code = RUNNER.finish(stop, ISSUE, self.token, write=output.stop_line)
                return stop, code
        raise AssertionError("the loop returned")


def size(text, unit=RUNNER.UNIT_CHARS):
    return RUNNER.measure(text, unit)


class LimitsTest(unittest.TestCase):
    """Matrix row #40 (US-10 "Service lines within the limit", SC-003 "a batch
    longer than the limit")."""

    def test_service_lines_keep_to_the_line_limit(self):
        box = Box(self)
        line = {"event": "degraded", "cause": "poll_failing", "issue": ISSUE,
                "action": "Polls have been failing for a while; " * 5}
        # Too long: the long fields become "see journal".
        out = Out()
        box.output({"line": {"max": 120, "unit": "chars"}}, "stream", out=out).lifecycle(line)
        (_, text), = out.writes
        self.assertLessEqual(size(text.rstrip("\n")), 120)
        self.assertEqual(json.loads(text)["action"], RUNNER.SEE_JOURNAL)
        self.assertEqual(json.loads(text)["cause"], "poll_failing")
        # Still too long: event and cause only.
        out = Out()
        box.output({"line": {"max": 45, "unit": "chars"}}, "stream", out=out).lifecycle(line)
        (_, text), = out.writes
        self.assertEqual(json.loads(text), {"event": "degraded", "cause": "poll_failing"})
        # The full line is in the journal.
        self.assertTrue([l for l in box.journal_lines() if l.get("action") == line["action"]])

    def test_an_event_that_does_not_fit_gets_a_stand_in_or_only_the_journal(self):
        box = Box(self)
        long_id = "x" * 300
        huge = dict(event(9), actorSessionId=long_id)
        out = Out()
        output = box.output({"line": {"max": 250, "unit": "chars"}}, "stream", out=out)
        done = RUNNER.print_events(output, [huge], False, None)
        self.assertEqual(done, [huge])
        (stub,) = out.lines()
        self.assertEqual((stub["event"], stub["notificationId"]), ("notification_unfit", 9))
        # A limit too small even for the stand-in: only the journal.
        out = Out()
        output = box.output({"line": {"max": 20, "unit": "chars"}}, "stream", out=out)
        self.assertEqual(RUNNER.print_events(output, [huge], False, None), [huge])
        self.assertEqual(out.writes, [])
        unfit = [l for l in box.journal_lines() if l.get("notificationId") == 9 and l.get("status") == "unfit"]
        self.assertEqual(len(unfit), 2)

    def test_events_beyond_a_batch_go_into_the_next_one_and_into_the_journal(self):
        box = Box(self)
        out = Out()
        output = box.output({"line": {"max": 400, "unit": "chars"}, "batch": {"max": 2, "unit": "lines"}},
                            "stream", out=out)
        executor = FakeExecutor([page([1, 2, 3, 4, 5], 5)])
        stop, _ = box.cycle(executor, output, "stream")
        self.assertEqual(stop.reason, "token_expired")
        batches = [[json.loads(l) for l in t.splitlines()] for _, t in out.writes]
        events = [b for b in batches if b[0]["event"] == "notification"]
        self.assertEqual([[l["notification"]["id"] for l in b] for b in events], [[1, 2], [3, 4], [5]])
        printed = [l["notificationId"] for l in box.journal_lines()
                   if l.get("event") == "delivery" and l.get("status") == "printed"]
        self.assertEqual(printed, [1, 2, 3, 4, 5])
        # The next poll goes on from the cursor of the whole page.
        self.assertIn("since_id=5", executor.urls[1])

    def test_a_full_exit_on_event_cycle_with_the_short_delivered_line(self):
        box = Box(self)
        # A limit that leaves the delivered line only its event and reason
        # (and the events only the journal).
        limits = {"line": {"max": 50, "unit": "chars"}}
        out = Out()
        stop, code = box.cycle(FakeExecutor([page([1], 1)]), box.output(limits, "exit-on-event", out=out),
                               "exit-on-event")
        self.assertEqual((stop.reason, code), ("delivered", 0))
        self.assertEqual(out.lines(), [{"event": "paused", "reason": "delivered"}])
        # The same working command again: the poll goes on from the cursor.
        executor = FakeExecutor([page([2], 2)])
        stop, _ = box.cycle(executor, box.output(limits, "exit-on-event"), "exit-on-event")
        self.assertIn("since_id=1", executor.urls[0])
        self.assertEqual(stop.reason, "delivered")
        unfit = [l["notificationId"] for l in box.journal_lines() if l.get("status") == "unfit"]
        self.assertEqual(unfit, [1, 2])
        self.assertTrue(os.path.exists(box.token), "the token was not kept")


class JournalDown(object):
    """The journal of a box refusing its first `failures` writes."""

    def __init__(self, box, failures):
        self.left = failures
        self.real_delivery = box.journal.delivery
        self.real_lifecycle = box.journal.lifecycle
        box.journal.delivery = self.delivery
        box.journal.lifecycle = self.lifecycle

    def _gate(self):
        if self.left > 0:
            self.left -= 1
            raise RUNNER.JournalUnavailable("write_failed", "/journal")

    def delivery(self, *args, **kwargs):
        self._gate()
        return self.real_delivery(*args, **kwargs)

    def lifecycle(self, *args, **kwargs):
        self._gate()
        return self.real_lifecycle(*args, **kwargs)


class JournalUnavailableTest(unittest.TestCase):
    """Review finding C1 of wave 6: an event whose only carrier is the
    journal has no outcome while the journal refuses it (US-10, the
    journal_unavailable degraded line of "Runner stdout lines")."""

    LONG = dict(event(9), actorSessionId="x" * 300)

    def unfit_records(self, box, nid):
        return [l for l in box.journal_lines() if l.get("notificationId") == nid and l.get("status") == "unfit"]

    def test_a_journal_only_event_waits_for_the_journal_and_the_cursor_stays(self):
        for method in ("stream", "exit-on-event"):
            with self.subTest(method=method):
                box = Box(self)
                JournalDown(box, 1)
                out = Out()
                # 20 chars: neither the event nor its stand-in fit, the
                # journal is its only carrier.
                executor = FakeExecutor([page([9], 9), page([9], 9)])
                executor.answers[0]["notifications"] = [self.LONG]
                executor.answers[1]["notifications"] = [self.LONG]
                limits = {"line": {"max": 60, "unit": "chars"}}
                stop, _ = box.cycle(executor, box.output(limits, method, out=out), method)
                # The first page did not move the cursor: the second poll asks
                # from the same place (the first poll of a new token has none).
                self.assertNotIn("since_id", executor.urls[1])
                lines = out.lines()
                self.assertEqual(lines[0], {"event": "degraded", "cause": "journal_unavailable"})
                self.assertEqual(lines[1]["event"], "recovered")
                self.assertEqual(len(self.unfit_records(box, 9)), 1)
                if method == "stream":
                    self.assertIn("since_id=9", executor.urls[2])
                    self.assertEqual(stop.reason, "token_expired")
                else:
                    self.assertEqual(stop.reason, "delivered")
                    self.assertEqual(RUNNER.load_cursor(box.token)["id"], 9)

    def test_a_stand_in_is_printed_only_after_its_journal_record(self):
        box = Box(self)
        JournalDown(box, 1)
        out = Out()
        output = box.output({"line": {"max": 250, "unit": "chars"}}, "stream", out=out)
        self.assertEqual(RUNNER.print_events(output, [self.LONG], False, None), [])
        self.assertEqual([l["event"] for l in out.lines()], ["degraded"])
        self.assertEqual(self.unfit_records(box, 9), [])
        # The journal is back: the record, then the stand-in that points at it.
        self.assertEqual(RUNNER.print_events(output, [self.LONG], False, None), [self.LONG])
        self.assertEqual([l["event"] for l in out.lines()], ["degraded", "recovered", "notification_unfit"])
        self.assertEqual(len(self.unfit_records(box, 9)), 1)

    def test_a_printed_event_whose_copy_fails_is_delivered_once(self):
        box = Box(self)
        JournalDown(box, 1)
        out = Out()
        output = box.output({"line": {"max": 400, "unit": "chars"}}, "stream", out=out)
        self.assertEqual(RUNNER.print_events(output, [event(1)], False, None), [event(1)])
        events = [l["event"] for l in out.lines()]
        self.assertEqual(events, ["notification", "degraded"])


class ExitOnEventTest(unittest.TestCase):
    """Matrix row #41 (US-3 "the stdout guarantee", INV-11)."""

    def test_batch_flush_cursor_then_one_delivered_line(self):
        box = Box(self)
        ops = []
        out = Out(ops=ops)
        real_save = RUNNER.save_cursor

        def save(token_file, cursor):
            ops.append("cursor")
            real_save(token_file, cursor)

        with mock.patch.object(RUNNER, "save_cursor", save):
            stop, code = box.cycle(FakeExecutor([page([1, 2], 2)]), box.output(None, "exit-on-event", out=out),
                                   "exit-on-event")
        self.assertEqual(ops, ["write", "flush", "cursor", "write", "flush"])
        lines = out.lines()
        self.assertEqual([l["notification"]["id"] for l in lines[:-1]], [1, 2])
        last = lines[-1]
        self.assertEqual((last["event"], last["reason"], code), ("paused", "delivered", 0))
        self.assertEqual([l for l in lines if l.get("reason")], [last], "more than one stop line")
        self.assertIn("preflight %d" % PREFLIGHT_ID, last["action"])
        self.assertNotIn(COMMAND_MARK, last["action"])
        self.assertLessEqual(len(out.writes[-1][1].rstrip("\n").encode("utf-8")), 256)
        self.assertTrue(os.path.exists(box.token))

    def test_killed_between_the_output_and_the_cursor_repeats_not_loses(self):
        box = Box(self)
        RUNNER.save_cursor(box.token, page([], 0)["cursor"])

        class Killed(BaseException):
            pass

        def die(token_file, cursor):
            raise Killed()

        out = Out()
        with mock.patch.object(RUNNER, "save_cursor", die):
            with self.assertRaises(Killed):
                box.cycle(FakeExecutor([page([7, 8], 8)]), box.output(None, "exit-on-event", out=out),
                          "exit-on-event")
        self.assertEqual([l["notification"]["id"] for l in out.lines()], [7, 8])
        # The next cycle polls from the old cursor and prints the same ids.
        executor = FakeExecutor([page([7, 8], 8)])
        out = Out()
        stop, _ = box.cycle(executor, box.output(None, "exit-on-event", out=out), "exit-on-event")
        self.assertIn("since_id=0", executor.urls[0])
        self.assertEqual([l["notification"]["id"] for l in out.lines() if l["event"] == "notification"], [7, 8])
        self.assertEqual(stop.reason, "delivered")

    def test_a_partial_page_goes_on_in_the_next_cycle_and_root_closed_only_at_its_end(self):
        box = Box(self)
        limits = {"batch": {"max": 3, "unit": "lines"}}
        closing = page([1, 2, 3, 4], 4, root_closed=True)
        got = []
        for _ in range(2):
            out = Out()
            stop, _ = box.cycle(FakeExecutor([closing]), box.output(limits, "exit-on-event", out=out),
                                "exit-on-event")
            got.append(([l["notification"]["id"] for l in out.lines() if l["event"] == "notification"],
                        stop.reason))
        # Two events and delivered fill the first batch of three lines; the
        # rest of the same page, then root_closed (INV-11).
        self.assertEqual(got, [([1, 2], "delivered"), ([3, 4], "root_closed")])


class WindowModelTest(unittest.TestCase):
    """Matrix row #42 (the window model of a harness, US-10 pace), fake
    clock, a synthetic record."""

    LIMITS = {"line": {"max": 400, "unit": "chars"},
              "window": {"lengthMs": 200, "lines": 3, "budget": 4, "refillSec": 20}}

    def test_batches_keep_to_the_windows_and_their_budget(self):
        box = Box(self)
        clock = FakeClock()
        start = clock.now
        out = Out(clock)
        output = box.output(self.LIMITS, "stream", clock=clock, out=out)
        # A service line spends a window of the same budget.
        output.lifecycle({"event": "monitor_started", "preflightId": PREFLIGHT_ID, "method": "stream",
                          "channel": "stdout"})
        RUNNER.print_events(output, [event(i) for i in range(1, 11)], False, None)
        times = [t for t, _ in out.writes]
        sizes = [len(t.splitlines()) for _, t in out.writes]
        self.assertEqual(sum(sizes), 11)
        # No window holds more than its lines, service lines included.
        self.assertTrue(all(n <= 3 for n in sizes), sizes)
        # Two batches never share a window.
        gaps = [b - a for a, b in zip(times, times[1:])]
        self.assertTrue(all(g > 0.2 for g in gaps), gaps)
        # The budget of 4 windows: the fifth waits for a window to come back.
        self.assertGreaterEqual(times[4] - start, 20 - 1e-6)
        self.assertLess(times[3] - start, 20)


if __name__ == "__main__":
    unittest.main()
