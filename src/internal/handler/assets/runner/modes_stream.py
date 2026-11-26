# --- stream and exit-on-event with client (US-2, US-3, US-10) -----------------
# The working runner of a stream or exit-on-event result prints on its own
# stdout, which the agent's tool reads. The limits are those of the record's
# method the preflight took (preflight.json "limits"): a line limit, a batch
# limit, a rate (a burst of batches, then one per interval) and a window
# model of the tool.
#
#   - Every line keeps to the line limit, service lines included (monitor_
#     started, degraded, recovered, a stop): too long, it is shortened by
#     text.lifecycle_line ("see journal", then event and reason only). An
#     event whose minimal form does not fit is replaced by a stand-in line
#     with its notification id that says to read the journal; when even that
#     does not fit, the event is only in the journal. Both are the outcome
#     "unfit", and the cursor passes the event - once the journal holds it:
#     the journal is its only carrier, so the unfit record is written before
#     the stand-in is printed, and an event whose record cannot be written
#     has no outcome yet. It waits, the cursor does not pass it, and the
#     monitor says degraded journal_unavailable once (recovered when the
#     journal takes a line again). A printed event is delivered by its line;
#     its journal copy failing costs the copy, said by the same degraded.
#   - A batch is one write of whole lines within the batch limit and the
#     window's line limit; service lines count in both. Events that did not
#     go into a batch go into the next one: the cursor of a page is saved
#     only after every event of the page has an outcome.
#   - The rate is a bucket of `burst` batches refilled by one every
#     `intervalSec`; the window budget is a bucket of `budget` windows
#     refilled by one every `refillSec`. A batch waits for both, and for the
#     window of the previous batch to close (its length plus a margin), so
#     two batches never share a window.
#   - stream with client writes every printed event to the journal too.
#   - A page whose events do not all get an outcome in one round keeps the
#     rest (and the cursor after the page, and its rootClosed): the next
#     round takes them from there, not from a new poll whose page may differ
#     (US-12); its poll only keeps the access boundary. exit-on-event keeps
#     the rest in the state for the next cycle.
#   - exit-on-event: a batch, flushed; then the cursor (or, for a page only
#     partly printed, its rest); then exactly one stop line,
#     delivered (paused, exit 0, token kept), whose action names the working
#     command of the preflight result by its number without repeating it.
#     A runner killed between the output and the cursor repeats those events
#     in the next cycle: a repeat, not a loss (US-3). root_closed comes
#     instead of delivered only when the whole last page is out (INV-11).

import json
import sys
import time

# The margin after a window closes before the next batch may start.
WINDOW_MARGIN_SEC = 0.1
# A wait shorter than this is none: the refill of a bucket is computed in
# floating point and may stop a hair short of a whole token, a remainder no
# sleep can usefully wait out.
_WAIT_EPSILON_SEC = 1e-6
# Statuses of an event in the journal of a stream or exit-on-event monitor.
STATUS_PRINTED = "printed"
STATUS_UNFIT = "unfit"
# Line limit units of the registry and their measure in text.py.
_LINE_UNITS = {"chars": UNIT_CHARS, "bytes": UNIT_BYTES}
# Batch limit units counted in lines.
_COUNT_UNITS = ("lines", "events")


class StreamLimits(object):
    """The limits of the method the preflight took (registry form, checked
    by the server's validator); every limit is optional."""

    def __init__(self, limits=None):
        limits = limits if isinstance(limits, dict) else {}
        line = limits.get("line") if isinstance(limits.get("line"), dict) else {}
        self.line_max = line.get("max") if line.get("unit") in _LINE_UNITS else None
        self.line_unit = _LINE_UNITS.get(line.get("unit"), UNIT_CHARS)
        batch = limits.get("batch") if isinstance(limits.get("batch"), dict) else {}
        self.batch_max = batch.get("max")
        self.batch_unit = batch.get("unit")
        rate = limits.get("rate") if isinstance(limits.get("rate"), dict) else {}
        self.burst = rate.get("burst")
        self.interval_sec = rate.get("intervalSec")
        window = limits.get("window") if isinstance(limits.get("window"), dict) else {}
        self.window_sec = window["lengthMs"] / 1000.0 if window else None
        self.window_lines = window.get("lines") if window else None
        self.window_budget = window.get("budget") if window else None
        self.window_refill_sec = window.get("refillSec") if window else None

    def lines_cap(self):
        """The most lines one batch may hold, or None."""
        caps = [c for c in (self.window_lines,
                            self.batch_max if self.batch_unit in _COUNT_UNITS else None) if c]
        return min(caps) if caps else None

    def batch_size(self, texts):
        """The size of the lines as one batch in the unit of the batch limit
        (a line and its newline in chars or bytes)."""
        if self.batch_unit in _COUNT_UNITS or self.batch_unit is None:
            return len(texts)
        unit = _LINE_UNITS.get(self.batch_unit, UNIT_CHARS)
        return sum(measure(t, unit) + 1 for t in texts)

    def fits(self, texts):
        cap = self.lines_cap()
        if cap is not None and len(texts) > cap:
            return False
        if self.batch_max and self.batch_unit not in _COUNT_UNITS and self.batch_size(texts) > self.batch_max:
            return False
        return True


class _Bucket(object):
    """capacity tokens, one more every refill_sec, at most capacity."""

    def __init__(self, capacity, refill_sec, now):
        self.capacity = float(capacity)
        self.refill_sec = float(refill_sec)
        self.tokens = float(capacity)
        self.stamp = now

    def wait(self, now):
        """Seconds until a token is there."""
        self.tokens = min(self.capacity, self.tokens + (now - self.stamp) / self.refill_sec)
        self.stamp = now
        return 0.0 if self.tokens >= 1.0 else (1.0 - self.tokens) * self.refill_sec

    def take(self):
        self.tokens = max(self.tokens - 1.0, 0.0)


class Pacer(object):
    """When the next batch may be written: a rate token, a window of the
    budget, and the previous window closed. clock and sleep are the
    boundary a test drives with fake time."""

    def __init__(self, limits, clock=None, sleep=None):
        self.clock = clock or time.monotonic
        self.sleep = sleep or time.sleep
        now = self.clock()
        self.buckets = []
        if limits.burst and limits.interval_sec:
            self.buckets.append(_Bucket(limits.burst, limits.interval_sec, now))
        if limits.window_sec:
            self.buckets.append(_Bucket(limits.window_budget, limits.window_refill_sec, now))
        self.window_sec = limits.window_sec
        self.closed_at = None

    def wait_turn(self):
        """Wait until a batch may be written and take its turn."""
        while True:
            now = self.clock()
            waits = [b.wait(now) for b in self.buckets]
            if self.closed_at is not None:
                waits.append(self.closed_at - now)
            longest = max(waits) if waits else 0.0
            if longest <= _WAIT_EPSILON_SEC:
                break
            self.sleep(longest)
        for b in self.buckets:
            b.take()
        if self.window_sec:
            # The batch is written now, in one write: its window closes after
            # the window length; the next batch waits a margin more.
            self.closed_at = self.clock() + self.window_sec + WINDOW_MARGIN_SEC


class StreamOutput(object):
    """The stdout of a stream or exit-on-event monitor within its limits."""

    def __init__(self, limits, journal, method, out=None, pacer=None, issue=""):
        self.limits = limits
        self.journal = journal
        self.method = method
        self.issue = issue
        self.out = out
        self.pacer = pacer or Pacer(limits)
        # A line reserved in the batch just written (the stop line of an
        # exit-on-event cycle): it is written without a turn of its own, when
        # it still fits that batch.
        self.reserved = False
        self.last_batch = []
        # When the journal stopped taking lines (the pacer's clock), or None.
        self._journal_failed_at = None

    def _write(self, texts):
        out = self.out or sys.stdout
        out.write("".join(t + "\n" for t in texts))
        out.flush()

    def batch(self, texts, reserve=False):
        """Write texts as one batch; reserve keeps a line of it for the stop
        line that follows."""
        self.pacer.wait_turn()
        self._write(texts)
        self.reserved = reserve
        self.last_batch = list(texts)

    def line(self, obj):
        """A service line within the line limit, or None when not even its
        event and reason fit."""
        return lifecycle_line(obj, self.limits.line_max, self.limits.line_unit)

    def lifecycle(self, obj):
        """A service line: to the journal, and to stdout as a batch of its
        own (or in the reserved place). True when it went to stdout."""
        self._journal_lifecycle(obj)
        return self._print_service(obj)

    def _print_service(self, obj):
        text = self.line(obj)
        if text is None:
            return False
        if self.reserved and self.limits.fits(self.last_batch + [text]):
            # The line the batch kept room for: no turn of its own.
            self.reserved = False
            self.last_batch.append(text)
            self._write([text])
        else:
            # Not reserved, or longer than the room kept: a batch of its own.
            self.batch([text])
        return True

    def longest_line(self, objs):
        """The longest, in the unit of the batch limit, of the service lines
        of objs within the line limit (None for none)."""
        texts = [t for t in (self.line(o) for o in objs or []) if t is not None]
        if not texts:
            return None
        return max(texts, key=lambda t: self.limits.batch_size([t]) if self.limits.batch_unit not in _COUNT_UNITS
                   else measure(t, UNIT_BYTES))

    def event_text(self, n):
        """(text, status) of an event: its line, or the stand-in of an event
        that does not fit (status unfit), or (None, unfit): journal only."""
        text = notification_line(n, self.limits.line_max, self.limits.line_unit)
        if text is not None and self.limits.fits([text]):
            return text, STATUS_PRINTED
        nid = n.get("id") if isinstance(n, dict) else None
        stub = unfit_stub_line(nid, self.limits.line_max, self.limits.line_unit)
        if stub is not None and self.limits.fits([stub]):
            return stub, STATUS_UNFIT
        return None, STATUS_UNFIT

    def take(self, texts, reserve_text=None):
        """How many of texts, from the start, form one batch (with
        reserve_text after them, when given); at least one when the first
        fits alone."""
        tail = [reserve_text] if reserve_text is not None else []
        k = 0
        while k < len(texts) and self.limits.fits(texts[:k + 1] + tail):
            k += 1
        return k

    def record(self, n, status):
        """The outcome of an event in the journal; True when the journal
        holds it. A journal that cannot be written is said once
        (degraded journal_unavailable); the caller decides what the event
        lost: its outcome (unfit, the journal is its only carrier) or a copy
        (printed)."""
        try:
            self.journal.delivery(n, status, method=self.method, channel=CHANNEL_STDOUT)
        except JournalUnavailable as exc:
            self._journal_down(exc)
            return False
        self._journal_up()
        return True

    def _journal_lifecycle(self, obj):
        fields = dict(obj)
        event = fields.pop("event", "")
        try:
            self.journal.lifecycle(event, **fields)
        except JournalUnavailable as exc:
            self._journal_down(exc)
            return
        self._journal_up()

    def _journal_down(self, exc):
        if self._journal_failed_at is not None:
            return
        self._journal_failed_at = self.pacer.clock()
        log_stderr("monitor runner: the journal cannot be written (%s)" % exc.reason)
        # stdout only: the journal is what failed.
        self._print_service({
            "event": "degraded", "cause": "journal_unavailable", "issue": self.issue,
            "action": "The journal of the monitor for issue %s cannot be written, so notifications that "
                      "only the journal can carry wait (they are not lost) and are delivered once it can "
                      "be written again; the monitor keeps running and retries on its own, so do not "
                      "restart it. If this does not clear, check the journal directory named in the "
                      "preflight line." % self.issue})

    def _journal_up(self):
        if self._journal_failed_at is None:
            return
        failed_for = self.pacer.clock() - self._journal_failed_at
        self._journal_failed_at = None
        self.lifecycle({"event": "recovered", "cause": "journal_unavailable", "issue": self.issue,
                        "failedForSec": int(failed_for)})

    def journal_down(self):
        """Whether the journal is failing (an event may be waiting for it)."""
        return self._journal_failed_at is not None

    def stop_line(self, line):
        """finish() writes the stop line through here."""
        self.lifecycle(line)


def print_events(output, events, exit_on_event, stop_lines):
    """Print the events (each with its text from output.event_text) in
    batches. exit_on_event prints one batch, with room for the longest of
    stop_lines (the stop lines that may end the cycle right after it), and
    leaves the rest. Returns the events that got an outcome
    (printed or unfit), in order. An unfit event gets its outcome only when
    the journal takes its record, before its stand-in is printed; one the
    journal refuses has none and is neither printed nor returned (it waits
    for the next poll of the same page)."""
    done = []
    pending = []
    for n in events:
        text, status = output.event_text(n)
        if text is None:
            # Only the journal: the outcome is unfit once the journal has
            # it; then the cursor passes it.
            if output.record(n, STATUS_UNFIT):
                done.append(n)
            continue
        pending.append((n, text, status))
    reserve = output.longest_line(stop_lines) if exit_on_event else None
    while pending:
        texts = [t for _, t, _ in pending]
        k = output.take(texts, reserve)
        with_reserve = exit_on_event and k > 0
        if k == 0:
            # Not even one event beside the stop line: the first events go
            # in a batch of their own (each fits alone), the stop line in
            # the next one.
            k = max(output.take(texts), 1)
        chunk, pending = pending[:k], pending[k:]
        # A stand-in points at the journal record of its event: that record
        # first. Dropping lines from a batch keeps it within its limits.
        chunk = [c for c in chunk if c[2] != STATUS_UNFIT or output.record(c[0], STATUS_UNFIT)]
        if not chunk:
            continue
        output.batch([t for _, t, _ in chunk], reserve=with_reserve)
        for n, _, status in chunk:
            if status == STATUS_PRINTED:
                # Delivered by its line; a refused copy is said by degraded.
                output.record(n, status)
            done.append(n)
        if exit_on_event:
            break
    return done


def run_stream_loop(executor, base, token, issue, exclude_session, token_file, doc, output):
    """The poll loop of a stream or exit-on-event monitor. Never returns: it
    ends by raising Stop (delivered ends each exit-on-event cycle)."""
    exit_on_event = doc.get("method") == METHOD_EXIT_ON_EVENT
    # The stop lines that may follow the batch of an exit-on-event cycle at
    # once: the batch keeps room for the longest.
    slot_lines = [cycle_stop_line(reason, issue, doc["preflightId"])
                  for reason in ("delivered", "root_closed", "stopped", "internal_error")]
    cursor = load_cursor(token_file)

    def hold(save, *args):
        _save_or_hold(save, token_file, args, output, executor, base, token, cursor, exclude_session, issue)

    # (events, cursor after the page, rootClosed) of a page received but not
    # yet given an outcome in full; an exit-on-event cycle resumes it from
    # the state.
    remainder = load_remainder(token_file, cursor) if exit_on_event else None
    seen = set()
    backoff = float(POLL_INTERVAL_SEC)
    series_started = None
    series_degraded = False
    while True:
        status, payload, retry_after = poll_once(executor, base, token, cursor, exclude_session)
        if status == 200 and isinstance(payload, dict) and isinstance(payload.get("notifications") or [], list):
            if series_degraded:
                output.lifecycle({"event": "recovered", "issue": issue,
                                  "failedForSec": int(time.monotonic() - series_started)})
            series_started = None
            series_degraded = False
            if remainder is not None:
                # The rest of a page already received: its events, not those
                # of this poll, which only kept the access boundary (US-12).
                source, nxt, root_closed = remainder
            else:
                source = payload.get("notifications") or []
                nxt = payload.get("cursor") if isinstance(payload.get("cursor"), dict) else cursor
                root_closed = payload.get("rootClosed") is True
            events = [n for n in source if not isinstance(n, dict) or n.get("id") not in seen]
            done = print_events(output, events, exit_on_event, slot_lines)
            for n in done:
                if isinstance(n, dict) and n.get("id") is not None:
                    seen.add(n.get("id"))
            done_ids = set(id(n) for n in done)
            left = [n for n in events if id(n) not in done_ids]
            if not left:
                remainder = None
                if root_closed:
                    raise Stop("root_closed", delete_token=True)
                hold(save_cursor, nxt)
                if exit_on_event and events:
                    raise Stop("delivered", detail=doc["preflightId"])
                cursor = nxt
                backoff = float(POLL_INTERVAL_SEC) if events else min(backoff + POLL_INTERVAL_SEC,
                                                                      float(BACKOFF_CEILING_SEC))
                time.sleep(max(jitter(backoff), float(POLL_INTERVAL_SEC)))
                continue
            # Events of the page without an outcome yet: they are kept, the
            # cursor stays before the page, and the next round takes them
            # from here, not from a new poll whose page may differ.
            remainder = (left, nxt, root_closed)
            if exit_on_event and done:
                # The next cycle takes the rest from the state.
                hold(save_remainder, cursor, left, nxt, root_closed)
                raise Stop("delivered", detail=doc["preflightId"])
            # Waiting for the journal (stream prints the whole rest of a
            # page): the rest again after a poll interval.
            time.sleep(jitter(float(POLL_INTERVAL_SEC)))
            continue
        if status == 401:
            raise Stop("token_expired", status=401, delete_token=True)
        if status == 403:
            raise Stop("visibility_lost", status=403, delete_token=True)
        if status == 400 and cursor is not None:
            cursor = None
            if remainder is not None:
                # Received events keep their delivery; the page after them
                # starts from now.
                remainder = (remainder[0], None, remainder[2])
                hold(save_remainder, None, remainder[0], None, remainder[2])
            else:
                hold(save_cursor, None)
            output.lifecycle({"event": "degraded", "cause": "cursor_reset", "issue": issue,
                              "action": "The server rejected this monitor's poll cursor, so the monitor "
                                        "restarted from now: notifications since its last successful poll "
                                        "may have been missed — check issue %s with skein_show. "
                                        "Do not restart anything; the monitor keeps running." % issue})
            time.sleep(jitter(float(POLL_INTERVAL_SEC)))
            continue
        now = time.monotonic()
        if series_started is None:
            series_started = now
        if not series_degraded and now - series_started > STALL_THRESHOLD_SEC:
            series_degraded = True
            output.lifecycle({"event": "degraded", "cause": "poll_failing", "issue": issue,
                              "failingForSec": int(now - series_started), "lastStatus": status,
                              "action": "Polls for issue %s have been failing for a while; the monitor "
                                        "keeps retrying on its own, so do not restart it. If this does not "
                                        "clear, check that the Skein server is reachable." % issue})
        if status == 429:
            wait = retry_after_seconds(retry_after, BACKOFF_CEILING_SEC)
            time.sleep(jitter(min(max(float(wait), float(POLL_INTERVAL_SEC)), float(BACKOFF_CEILING_SEC))))
            continue
        backoff = failure_backoff(backoff)
        time.sleep(jitter(backoff))


def cycle_stop_line(reason, issue, preflight):
    """The stop line finish() prints for reason in a client-mode cycle of
    preflight result `preflight`."""
    return {"event": STOPS[reason][0], "reason": reason,
            "action": stop_action(reason, issue, preflight if reason == "delivered" else None, preflight)}


def limited_write(doc):
    """The writer of a stop line that keeps to the line limit of a stream or
    exit-on-event result doc (its start failed before its output existed);
    None for another method or no result: the line is printed as it is."""
    if not isinstance(doc, dict) or doc.get("method") not in (METHOD_STREAM, METHOD_EXIT_ON_EVENT):
        return None
    limits = StreamLimits(doc.get("limits"))

    def write(line):
        text = lifecycle_line(line, limits.line_max, limits.line_unit)
        if text is not None:
            sys.stdout.write(text + "\n")
            sys.stdout.flush()
    return write


def _save_or_hold(save, token_file, args, output, executor, base, token, cursor, exclude_session, issue):
    """Write the monitor state (the cursor, or the ids printed from a
    partial page). The run goes on only once it is written: the cursor in
    memory moves after its state, and exit-on-event prints delivered only
    for a saved cycle (an unsaved one would print the same events again on
    every run). While the state cannot be written the monitor holds: it says
    degraded state_unavailable once, keeps the access boundary by polling at
    the last saved cursor (401 and 403 still stop it; what such a poll
    returns is not delivered) and tries again every poll interval; recovered
    once it is written."""
    failed_at = None
    while True:
        try:
            save(token_file, *args)
        except (OSError, PlatformRefusal) as exc:
            if failed_at is None:
                failed_at = time.monotonic()
                log_stderr("monitor runner: cannot save the monitor state (%s)" % exc)
                output.lifecycle({
                    "event": "degraded", "cause": "state_unavailable", "issue": issue,
                    "action": "The monitor for issue %s cannot save its state next to its token file, so it "
                              "holds new notifications (they are not lost) until it can; it keeps running "
                              "and retries on its own, so do not restart it. If this does not clear, check "
                              "that the directory of the token file can be written." % issue})
            time.sleep(jitter(float(POLL_INTERVAL_SEC)))
            status, _, _ = poll_once(executor, base, token, cursor, exclude_session)
            if status == 401:
                raise Stop("token_expired", status=401, delete_token=True)
            if status == 403:
                raise Stop("visibility_lost", status=403, delete_token=True)
            continue
        if failed_at is not None:
            output.lifecycle({"event": "recovered", "cause": "state_unavailable", "issue": issue,
                              "failedForSec": int(time.monotonic() - failed_at)})
        return
