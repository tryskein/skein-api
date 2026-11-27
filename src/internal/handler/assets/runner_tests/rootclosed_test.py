#!/usr/bin/env python3
"""Tests of reading on after rootClosed (runner/rootclosed.py with the poll
loops): matrix rows #65, #66, #67 and #130.

The stream and exit-on-event loop runs against a fake request executor that
answers each poll with the next page (modes_stream_test.py); the detached
loop runs the DeliverySink of deliver_test.py with a refusing channel.

Expected values are those of the spec (US-12, US-3, INV-11): the flag is
sticky; full pages are read on, at most 20 pages after the first one with
rootClosed (21 in all), then the stop says reading on was limited and the
journal gets the read_limited summary; an answer at the same cursor spends
nothing; the summary counts the events from the page of the first
rootClosed on; in exit-on-event root_closed ends only the cycle whose batch
held the rest of such a page with nothing more to read, otherwise the cycle
ends delivered and the next one takes the rest from the state, without a new
poll of the page; the kept rest gets the current exclusion of one's own
echo, and a rest spends no page of the budget. Without `client` the first
page with rootClosed ends the monitor (US-1).

Run on its own: python3 internal/handler/assets/runner_tests/rootclosed_test.py
Standard library of Python 3.6 only.
"""

import io
import json
import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402
import modes_stream_test as ms  # noqa: E402

RUNNER = ms.RUNNER
ISSUE = ms.ISSUE
FULL = 50
BUDGET_PAGES = 21
SESSION = "mcp-session-agent"


def full_page(first_id, cursor_id, root_closed=False, size=FULL):
    return ms.page(list(range(first_id, first_id + size)), cursor_id, root_closed=root_closed)


def cycle(box, executor, output, method, exclusion=""):
    """One run of the stream loop until its stop, after finish() printed its
    line; returns the stop."""
    doc = {"method": method, "preflightId": ms.PREFLIGHT_ID}
    with mock.patch.object(RUNNER.time, "sleep", lambda s: None):
        try:
            RUNNER.run_stream_loop(executor, "http://poll.test", "skm_x", ISSUE, exclusion, box.token, doc,
                                   output)
        except RUNNER.Stop as stop:
            RUNNER.finish(stop, ISSUE, box.token, write=output.stop_line)
            return stop
    raise AssertionError("the loop returned")


def printed_ids(box):
    return [l["notificationId"] for l in box.journal_lines() if l.get("event") == "delivery"
            and l.get("status") == "printed"]


def outcome_lines(box):
    return [(l["notificationId"], l["status"]) for l in box.journal_lines() if l.get("event") == "delivery"]


class BudgetTest(unittest.TestCase):
    """Row #65."""

    def test_full_pages_stop_after_21_with_the_summary_line(self):
        box = ms.Box(self)
        answers = [full_page(1, 1, root_closed=True)] + [full_page(1 + FULL * k, 1 + k) for k in range(1, 30)]
        executor = ms.FakeExecutor(answers)
        stop = cycle(box, executor, box.output(None, "stream"), "stream")
        self.assertEqual(stop.reason, "root_closed")
        self.assertTrue(stop.detail["readLimited"])
        self.assertEqual(len(executor.urls), BUDGET_PAGES)
        self.assertEqual(len(printed_ids(box)), FULL * BUDGET_PAGES)
        summaries = [l for l in box.journal_lines() if l.get("summary") == "read_limited"]
        self.assertEqual(len(summaries), 1)

    def test_an_answer_at_the_same_cursor_spends_nothing(self):
        box = ms.Box(self)
        answers = [full_page(1, 1, root_closed=True)]
        for k in range(1, 30):
            # A failed poll and its repeat at the same cursor between pages.
            answers.extend([500, full_page(1 + FULL * k, 1 + k)])
        executor = ms.FakeExecutor(answers)
        stop = cycle(box, executor, box.output(None, "stream"), "stream")
        self.assertTrue(stop.detail["readLimited"])
        self.assertEqual(len(printed_ids(box)), FULL * BUDGET_PAGES)

    def test_a_page_that_is_not_full_ends_the_reading(self):
        box = ms.Box(self)
        executor = ms.FakeExecutor([full_page(1, 1, root_closed=True), full_page(51, 2, size=3),
                                    full_page(900, 3)])
        stop = cycle(box, executor, box.output(None, "stream"), "stream")
        self.assertEqual(stop.reason, "root_closed")
        self.assertFalse(stop.detail["readLimited"])
        self.assertEqual(len(executor.urls), 2)
        self.assertNotIn(900, printed_ids(box))

    def test_the_flag_is_sticky_a_reopen_does_not_move_the_stop(self):
        box = ms.Box(self)
        # After the close the root is reopened (no rootClosed) and new events
        # keep coming in full pages: the budget still ends the reading.
        answers = [full_page(1, 1, root_closed=True)] + [full_page(1 + FULL * k, 1 + k) for k in range(1, 40)]
        executor = ms.FakeExecutor(answers)
        stop = cycle(box, executor, box.output(None, "stream"), "stream")
        self.assertEqual(stop.reason, "root_closed")
        self.assertEqual(len(executor.urls), BUDGET_PAGES)

    def test_the_summary_counts_from_the_first_rootclosed_page(self):
        box = ms.Box(self)
        # A line limit that leaves every event only the journal: does not fit.
        limits = {"line": {"max": 50, "unit": "chars"}}
        executor = ms.FakeExecutor([ms.page([1, 2], 1), ms.page([3], 2, root_closed=True)])
        stop = cycle(box, executor, box.output(limits, "stream"), "stream")
        self.assertEqual(stop.reason, "root_closed")
        self.assertEqual(stop.detail["doesNotFit"], 1)

    def test_the_budget_survives_a_rearm(self):
        box = ms.Box(self)
        cycles = 0
        page_no = 0
        stop = None
        while cycles < 40:
            cycles += 1
            answer = full_page(1 + FULL * page_no, 1 + page_no, root_closed=(page_no == 0))
            stop = cycle(box, ms.FakeExecutor([answer]), box.output(None, "exit-on-event"), "exit-on-event")
            page_no += 1
            if stop.reason != "delivered":
                break
        self.assertEqual(stop.reason, "root_closed")
        self.assertTrue(stop.detail["readLimited"])
        self.assertEqual(cycles, BUDGET_PAGES)


class ExitOnEventTest(unittest.TestCase):
    """Row #66."""

    def test_the_rest_goes_over_cycles_and_only_the_last_one_ends_root_closed(self):
        box = ms.Box(self)
        limits = {"batch": {"max": 3, "unit": "lines"}}
        reasons = []
        executor = ms.FakeExecutor([ms.page(list(range(1, 8)), 1, root_closed=True)])
        reasons.append(cycle(box, executor, box.output(limits, "exit-on-event"), "exit-on-event").reason)
        while reasons[-1] == "delivered" and len(reasons) < 20:
            # The poll of a cycle with a kept rest only keeps the access
            # boundary: its page is not the one the rest comes from.
            other = ms.FakeExecutor([ms.page([900, 901], 9)])
            reasons.append(cycle(box, other, box.output(limits, "exit-on-event"), "exit-on-event").reason)
        self.assertEqual(reasons[-1], "root_closed")
        self.assertGreater(len(reasons), 2)
        self.assertEqual(set(reasons[:-1]), {"delivered"})
        self.assertEqual(sorted(printed_ids(box)), list(range(1, 8)))
        self.assertEqual(len(printed_ids(box)), 7)

    def test_the_counts_survive_a_rearm(self):
        # Review of wave 8, row 25: the summary of the close counts what the
        # earlier cycles of the monitor fixed, not only the last cycle.
        box = ms.Box(self)
        limits = {"batch": {"max": 3, "unit": "lines"}}
        unfit = {2, 6}

        def output():
            out = box.output(limits, "exit-on-event")
            real = out.event_text
            out.event_text = lambda n: (None, ms.RUNNER.STATUS_UNFIT) if n.get("id") in unfit else real(n)
            return out
        stop = cycle(box, ms.FakeExecutor([ms.page(list(range(1, 8)), 1, root_closed=True)]), output(),
                     "exit-on-event")
        cycles = 1
        while stop.reason == "delivered" and cycles < 20:
            stop = cycle(box, ms.FakeExecutor([ms.page([900], 9)]), output(), "exit-on-event")
            cycles += 1
        self.assertEqual(stop.reason, "root_closed")
        self.assertGreater(cycles, 2)
        self.assertEqual(stop.detail["doesNotFit"], 2)
        self.assertEqual(sorted(printed_ids(box)), [1, 3, 4, 5, 7])

    def test_a_refusing_channel_gives_not_delivered_n(self):
        world = dt.World(self, [dt.chan("A", "chan-a")])
        world.prog("chan-a").respond([dt.REFUSE])
        mon = world.monitor(fallbacks=())
        executor = ms.FakeExecutor([{"notifications": [dt.note(1), dt.note(2)], "cursor": {"ts": "t", "id": 5},
                                     "rootClosed": True}])
        with mock.patch.object(world.runner.time, "sleep", lambda s: None):
            with self.assertRaises(world.runner.Stop) as caught:
                world.runner.work(executor, "http://poll.test", dt.TOKEN, dt.ISSUE, "", mon.token, sink=mon.sink,
                                  root=world.runner.RootClosed(mon.token))
        self.assertEqual(caught.exception.reason, "root_closed")
        self.assertEqual(caught.exception.detail["undelivered"], 2)

    def test_a_lost_final_answer_comes_again_from_the_saved_cursor(self):
        box = ms.Box(self)
        first = cycle(box, ms.FakeExecutor([ms.page([1], 1)]), box.output(None, "exit-on-event"), "exit-on-event")
        self.assertEqual(first.reason, "delivered")
        # The final answer is lost (no answer); the restart polls again from
        # the saved cursor and gets the events with rootClosed.
        executor = ms.FakeExecutor([0, ms.page([2], 2, root_closed=True)])
        stop = cycle(box, executor, box.output(None, "exit-on-event"), "exit-on-event")
        self.assertEqual(stop.reason, "root_closed")
        self.assertTrue(all("since_id=1" in u for u in executor.urls))
        self.assertEqual(printed_ids(box), [1, 2])


class KeptRestTest(unittest.TestCase):
    """Row #67 (OST-2-5)."""

    def test_a_rest_after_delivered_gets_the_changed_exclusion_and_spends_no_page(self):
        box = ms.Box(self)
        notes = full_page(1, 1, root_closed=True)
        for n in notes["notifications"]:
            n["actorSessionId"] = SESSION if n["id"] == 45 else "mcp-session-other"
        limits = {"batch": {"max": 30, "unit": "lines"}}
        exclusion = RUNNER.Exclusion(box.token, "mcp-session-mint")
        stop = cycle(box, ms.FakeExecutor([notes]), box.output(limits, "exit-on-event"), "exit-on-event", exclusion)
        self.assertEqual(stop.reason, "delivered")
        first = set(printed_ids(box))
        self.assertNotIn(45, first)
        self.assertEqual(RUNNER.load_root_closed(box.token)["pages"], 0)
        # Between the cycles the agent writes its session to the exclusion
        # file, and issue 50 leaves the subtree: the poll of the next cycle
        # no longer has it, but the rest comes from the state.
        with open(os.path.join(box.token_dir, "exclude_session"), "w") as fh:
            fh.write(SESSION)
        moved = ms.page([], 1)
        while True:
            exclusion = RUNNER.Exclusion(box.token, "mcp-session-mint")
            stop = cycle(box, ms.FakeExecutor([moved]), box.output(limits, "exit-on-event"), "exit-on-event",
                         exclusion)
            if stop.reason != "delivered" or RUNNER.load_remainder(box.token, RUNNER.load_cursor(box.token)) is None:
                break
        lines = outcome_lines(box)
        self.assertIn((45, "own-echo"), lines)
        self.assertIn((50, "printed"), lines)
        # Every event of the page has exactly one outcome.
        self.assertEqual(sorted(i for i, _ in lines), list(range(1, 51)))
        # The cursor is the one after the page, and the rest spent no page:
        # the page itself is the first of the budget.
        self.assertEqual(RUNNER.load_cursor(box.token), notes["cursor"])
        self.assertEqual(RUNNER.load_root_closed(box.token)["pages"], 1)


class WithoutClientTest(unittest.TestCase):
    """Row #130: without `client` the first page with rootClosed ends it."""

    def test_the_old_runner_stops_after_the_first_page(self):
        box = ms.Box(self)
        executor = ms.FakeExecutor([full_page(1, 1, root_closed=True), full_page(900, 2)])
        out = io.StringIO()
        with mock.patch.object(RUNNER.time, "sleep", lambda s: None), mock.patch.object(sys, "stdout", out):
            with self.assertRaises(RUNNER.Stop) as caught:
                RUNNER.work(executor, "http://poll.test", "skm_x", ISSUE, "", box.token)
        self.assertEqual(caught.exception.reason, "root_closed")
        self.assertEqual(len(executor.urls), 1)
        lines = [json.loads(l) for l in out.getvalue().splitlines() if l]
        ids = [l["notification"]["id"] for l in lines if l.get("event") == "notification"]
        self.assertEqual(ids, list(range(1, FULL + 1)))


if __name__ == "__main__":
    unittest.main()
