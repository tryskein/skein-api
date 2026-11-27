#!/usr/bin/env python3
"""Tests of one's own echo (runner/echo.py with the delivery pipeline and
the ledger): matrix rows #63 and #64.

A monitor here is the DeliverySink of the working runner over the real
ledger, journal and argv executor, with its exclusion of one's own echo
(echo.py); its channels are fake programs on PATH (fake_channel.py) and its
clock is a virtual one (the world of deliver_test.py), so the rate window of
an addressee is read from the times of the channel calls.

Expected values are those of the spec (US-13): before the exclusion file is
written, the session of the mint; a valid file replaces it and is read on
every poll; an empty, missing or unreadable file keeps the last known value;
the last valid value survives a restart; an event of the excluded session
is not sent, its outcome is "own echo"; an event without actorSessionId or
of another session goes by the ordinary rules. 30 deliveries in a row to
one addressee within 10 min, all of one session: one degraded
single_session_burst and a window of 60 s instead of 10 s; it goes back to
10 s after a delivery with another session's event or 10 min without a
delivery of that session; no event is lost.

Run on its own: python3 internal/handler/assets/runner_tests/echo_test.py
Standard library of Python 3.6 only.
"""

import json
import os
import sys
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import deliver_test as dt  # noqa: E402

MINT = "mcp-session-mint"
NEW = "mcp-session-new"
OTHER = "mcp-session-other"
EMPTY = {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 0}}
OTHER_ADDRESS = {"process": None, "addresses": [{"channel": "A", "value": "addr-2", "qualifiers": []}],
                 "inherited": [], "terminal": None}


def cursor(i):
    return {"ts": "2026-01-01T00:00:00Z", "id": 1000 + i}


class PollExecutor(object):
    """The request executor of a poll: records the URLs, answers an empty
    page."""

    def __init__(self):
        self.urls = []

    def request(self, url, _token):
        self.urls.append(url)
        return {"status": 200, "body": json.dumps(EMPTY)}

    def excluded(self):
        """The exclude_session of the last poll."""
        from urllib.parse import parse_qs, urlparse
        values = parse_qs(urlparse(self.urls[-1]).query).get("exclude_session")
        return values[0] if values else None


class EchoWorld(dt.World):
    """The world of deliver_test with monitors that carry an exclusion."""

    def monitor(self, name="tok-a", channel="A", fallbacks=(), identity=None, mint=MINT, cancel=None):
        mon = dt.World.monitor(self, name=name, channel=channel, fallbacks=fallbacks, identity=identity,
                               cancel=cancel)
        mon.exclusion = self.runner.Exclusion(mon.token, mint)
        mon.sink.exclusion = mon.exclusion
        return mon

    def write_exclusion(self, mon, content, mode=0o600):
        path = os.path.join(os.path.dirname(mon.token), "exclude_session")
        with open(path, "w") as fh:
            fh.write(content)
        os.chmod(path, mode)
        return path


def outcomes(mon):
    return dict((d[0], d[1]) for d in mon.deliveries())


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class ExclusionValueTest(unittest.TestCase):
    """Row #63: the value of the exclusion on a poll."""

    def setUp(self):
        self.world = EchoWorld(self, dt.two_channels())
        self.mon = self.world.monitor()
        self.poll = PollExecutor()

    def poll_once(self, exclusion):
        self.world.runner.poll_once(self.poll, dt.SERVER, dt.TOKEN, None, exclusion)
        return self.poll.excluded()

    def test_the_mint_session_until_the_file_is_written_then_the_file(self):
        self.assertEqual(self.poll_once(self.mon.exclusion), MINT)
        self.world.write_exclusion(self.mon, NEW + "\n")
        self.assertEqual(self.poll_once(self.mon.exclusion), NEW)

    def test_a_file_that_is_no_value_keeps_the_last_one(self):
        self.world.write_exclusion(self.mon, NEW)
        self.assertEqual(self.poll_once(self.mon.exclusion), NEW)
        path = os.path.join(os.path.dirname(self.mon.token), "exclude_session")
        for name, content in (("empty", ""), ("cut mid-write", "mcp-sess\x00"), ("not a session", "a b c")):
            with self.subTest(case=name):
                self.world.write_exclusion(self.mon, content)
                self.assertEqual(self.poll_once(self.mon.exclusion), NEW)
        with self.subTest(case="missing"):
            os.remove(path)
            self.assertEqual(self.poll_once(self.mon.exclusion), NEW)

    def test_the_last_valid_value_survives_a_restart(self):
        self.world.write_exclusion(self.mon, NEW)
        self.poll_once(self.mon.exclusion)
        os.remove(os.path.join(os.path.dirname(self.mon.token), "exclude_session"))
        restarted = self.world.runner.Exclusion(self.mon.token, MINT)
        self.assertEqual(self.poll_once(restarted), NEW)

    def test_without_client_the_mint_session_alone(self):
        self.world.write_exclusion(self.mon, NEW)
        self.assertEqual(self.poll_once(MINT), MINT)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class OwnEchoDeliveryTest(unittest.TestCase):
    """Row #63: an event of the excluded session that was received and not
    yet sent gets "own echo" and is not sent; an event without
    actorSessionId and one of another session are delivered."""

    def test_own_is_not_sent_others_are(self):
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        world.write_exclusion(mon, NEW)
        mon.exclusion.current()
        anonymous = dt.note(2)
        del anonymous["actorSessionId"]
        mon.page([dt.note(1, session=NEW), anonymous, dt.note(3, session=OTHER)])
        self.assertEqual(outcomes(mon), {1: "own-echo", 2: "queued", 3: "queued"})
        texts = [t for _, _, t in mon.spy.calls]
        self.assertFalse(any("c1" in t for t in texts))
        self.assertTrue(any("c3" in t for t in texts))

    def test_an_exclusion_written_during_the_checks_holds_back_the_text(self):
        # Review of wave 8, row 12: the file is written after the page loop
        # read it and before the hand-over; the event of the new session is
        # not sent, the other one is.
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        real = mon.sink.check_addressee
        written = []

        def check_addressee(*args):
            if not written:
                world.write_exclusion(mon, NEW)
                written.append(True)
            return real(*args)
        mon.sink.check_addressee = check_addressee
        mon.page([dt.note(1, session=NEW), dt.note(2, session=OTHER)])
        self.assertEqual(outcomes(mon), {1: "own-echo", 2: "queued"})
        texts = [t for _, _, t in mon.spy.calls]
        self.assertFalse(any("c1" in t for t in texts))
        self.assertTrue(any("c2" in t for t in texts))

    def test_after_the_last_check_the_exclusion_is_only_read(self):
        # Warning 7 of wave 13 round 2: between the last check of the
        # addressee and the hand-over nothing is written or synced. A file
        # written during the last check still holds the event back (it is
        # read), and its value is saved only after the transfer.
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        real = mon.sink.check_addressee
        checks = []
        saves = []
        real_save = world.runner.save_exclusion

        def check_addressee(*args, **kw):
            checks.append(1)
            if len(checks) == 2:
                world.write_exclusion(mon, NEW)
            return real(*args, **kw)

        def save(token_file, value):
            saves.append((len(checks), value))
            return real_save(token_file, value)
        mon.sink.check_addressee = check_addressee
        with mock.patch.object(world.runner, "save_exclusion", save):
            mon.page([dt.note(1, session=NEW)])
        self.assertEqual(outcomes(mon), {1: "own-echo"})
        self.assertEqual([t for _, _, t in mon.spy.calls], [])
        # Not saved within the transfer whose last check saw it.
        self.assertNotIn((2, NEW), saves)

    def test_the_new_session_before_the_file_is_delivered(self):
        # The agent learns of its new session from this very event.
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        mon.page([dt.note(1, session=NEW)])
        self.assertEqual(outcomes(mon), {1: "queued"})

    def test_a_filtered_event_seen_again_after_the_change_then_another_session(self):
        # Sequence one: an event filtered as one's own, the exclusion
        # changes, the server has not passed it yet and sends it again with
        # an event of another session: the first keeps "own echo" and is not
        # sent, the other is delivered.
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        mon.page([dt.note(1, session=MINT)])
        world.write_exclusion(mon, NEW)
        mon.exclusion.current()
        world.clock.t += 60
        mon.page([dt.note(1, session=MINT), dt.note(2, session=OTHER)])
        self.assertEqual(outcomes(mon), {1: "own-echo", 2: "queued"})
        self.assertEqual(len(world.prog("chan-a").calls()), 1)

    def test_a_filtered_event_seen_again_after_the_change_alone(self):
        # Sequence two: the same without a later event: nothing is sent.
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        mon.page([dt.note(1, session=MINT)])
        world.write_exclusion(mon, NEW)
        mon.exclusion.current()
        world.clock.t += 60
        mon.page([dt.note(1, session=MINT)])
        self.assertEqual(outcomes(mon), {1: "own-echo"})
        self.assertEqual(world.prog("chan-a").calls(), [])


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class FilteredByTheServerThenReturnedTest(unittest.TestCase):
    """Review of wave 8, row 22, the two sequences of US-13 as the spec
    words them: the server filtered the event of the mint's session on the
    first poll (it never came), the exclusion then changed, and the server,
    which had not passed it, returns it: it is delivered, and the agent
    knows it by its actorSessionId."""

    def first_poll_filtered(self, world, mon):
        mon.page([], cursor=cursor(0))
        world.write_exclusion(mon, NEW)
        mon.exclusion.current()
        world.clock.t += 60

    def test_with_an_event_of_another_session_after_it(self):
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        self.first_poll_filtered(world, mon)
        mon.page([dt.note(1, session=MINT), dt.note(2, session=OTHER)], cursor=cursor(1))
        self.assertEqual(outcomes(mon), {1: "queued", 2: "queued"})
        texts = [t for _, _, t in mon.spy.calls]
        self.assertTrue(any("c1" in t for t in texts))

    def test_alone(self):
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        self.first_poll_filtered(world, mon)
        mon.page([dt.note(1, session=MINT)], cursor=cursor(1))
        self.assertEqual(outcomes(mon), {1: "queued"})
        self.assertEqual(len(world.prog("chan-a").calls()), 1)


@unittest.skipUnless(os.name == "posix", "the POSIX pipeline")
class SingleSessionBurstTest(unittest.TestCase):
    """Row #64: the guard against a loop of self-wakeups is a rate limit on
    a series of one session for one addressee."""

    def deliver(self, world, monitors, count, session=OTHER, start=0):
        """count one-event pages of session, in turn through monitors;
        returns the call times of the addressee's channel."""
        for i in range(start, start + count):
            mon = monitors[i % len(monitors)]
            mon.page([dt.note(i + 1, session=session)], cursor=cursor(i))
        return world.prog("chan-a")

    def burst_lines(self, monitors):
        return [l for m in monitors for l in m.degraded("single_session_burst")]

    def test_29_then_30_through_two_monitors_of_one_addressee(self):
        world = EchoWorld(self, dt.two_channels())
        mons = [world.monitor("tok-a", mint=MINT), world.monitor("tok-b", mint=MINT)]
        self.deliver(world, mons, 29)
        self.assertEqual(self.burst_lines(mons), [])
        self.deliver(world, mons, 1, start=29)
        lines = self.burst_lines(mons)
        self.assertEqual(len(lines), 1)
        self.assertEqual(lines[0].get("actorSessionId"), OTHER)
        self.deliver(world, mons, 2, start=30)
        times = [t for p, t, _ in mons[0].spy.calls + mons[1].spy.calls if p == "chan-a"]
        times.sort()
        gaps = [b - a for a, b in zip(times, times[1:])]
        self.assertEqual(gaps[:29], [10.0] * 29)
        self.assertEqual(gaps[29:], [60.0, 60.0])
        self.assertEqual(len(self.burst_lines(mons)), 1)
        delivered = dict((d[0], d[1]) for m in mons for d in m.deliveries())
        self.assertEqual(sorted(delivered), list(range(1, 33)))
        self.assertEqual(set(delivered.values()), {"queued"})

    def test_another_addressee_with_the_same_session_has_its_own_series(self):
        world = EchoWorld(self, dt.two_channels())
        first = world.monitor("tok-a")
        second = world.monitor("tok-b", identity=OTHER_ADDRESS)
        for i in range(31):
            first.page([dt.note(i + 1, session=OTHER)], cursor=cursor(i))
            if i < 5:
                second.page([dt.note(100 + i, session=OTHER)], cursor=cursor(100 + i))
        self.assertEqual(len(first.degraded("single_session_burst")), 1)
        self.assertEqual(second.degraded("single_session_burst"), [])
        times = second.spy.times("chan-a")
        self.assertEqual([b - a for a, b in zip(times, times[1:])], [10.0] * 4)

    def test_a_mixed_batch_and_ten_quiet_minutes_end_the_series(self):
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        self.deliver(world, [mon], 30)
        self.assertEqual(len(mon.degraded("single_session_burst")), 1)
        # A batch with an event of another session: back to 10 s.
        mon.page([dt.note(31, session=OTHER), dt.note(32, session="mcp-session-third")], cursor=cursor(31))
        mon.page([dt.note(33, session=OTHER)], cursor=cursor(32))
        times = mon.spy.times("chan-a")
        self.assertEqual(times[-1] - times[-2], 10.0)
        # A long series again, then 10 min without a delivery: back to 10 s.
        self.deliver(world, [mon], 30, start=40)
        world.clock.t += 601
        self.deliver(world, [mon], 2, start=80)
        times = mon.spy.times("chan-a")
        self.assertEqual(times[-1] - times[-2], 10.0)

    def test_a_change_of_the_exclusion_reaches_events_waiting_in_the_batch(self):
        world = EchoWorld(self, dt.two_channels())
        mon = world.monitor()
        mon.page([dt.note(1, session=OTHER)], cursor=cursor(0))
        hits = []

        def poll_during_the_wait():
            # The agent writes the file; the poll of the wait reads it.
            if not hits:
                hits.append(1)
                world.write_exclusion(mon, NEW)
                mon.exclusion.current()
        world.clock.on_sleep = poll_during_the_wait
        mon.page([dt.note(2, session=NEW), dt.note(3, session=NEW), dt.note(4, session=OTHER)],
                 cursor=cursor(1))
        world.clock.on_sleep = None
        self.assertEqual(hits, [1])
        result = outcomes(mon)
        self.assertEqual((result[2], result[3], result[4]), ("own-echo", "own-echo", "queued"))
        texts = [t for _, _, t in mon.spy.calls]
        self.assertFalse(any("c2" in t or "c3" in t for t in texts))


if __name__ == "__main__":
    unittest.main()
