# --- deliver: the delivery pipeline of a detached monitor --------------------
# Every page of the poll is carried to the addressee before the cursor
# passes it (INV-9). For each run of events of a page:
#   attempt (ledger.py, P-11) -> check of the addressee ("Addressee and
#   check", items 3-4) -> the channel's driver -> one outcome by the closed
#   table (classify.py) -> the outcome into the ledger -> the state -> the
#   journal; the cursor moves only when every event of the page has its
#   outcome.
#
# Budget (INV-8): a channel that refuses before send is called again after
# 5, 30 and 120 s — at most 4 calls, all within the idle window of the token
# from the first one; the calls made are kept in the ledger, so a restart
# continues the schedule. Then the event goes to the next non-blind channel
# the preflight found, else the journal (a forced channel has none). The
# switch holds for the current event only: every event starts again with
# the first channel. Each switch on an exhausted budget writes one degraded
# channel_failing with the channel and its last answer; the first delivery
# by that very channel later writes recovered with its name (a delivery by
# another channel does not).
#
# "Unknown" is never repeated and never moves to another channel (INV-2):
# degraded delivery_unknown. "Addressee changed" before the first transfer
# gives the event "unknown" and sends every later event of this process to
# the journal only (degraded addressee_changed). An event whose minimal
# form fits no non-blind channel of the preflight "does not fit": its full
# form goes to the journal and the cursor passes it.
#
# A driver that has a step which may write the text before a final one
# (insert, then Enter) owns the outcome of an attempt it began: before the
# final step it checks the cancel signal and the addressee; a failed check
# withholds the final step and the outcome is "unknown" for the addressee,
# journaled without the preview when access was lost (OST-2-4). The cancel
# signal (access lost, a stop) comes through Cancel.check(); the working
# runner's poll during waits supplies it.
#
# A journal that cannot be written stops new sends and the cursor
# (JournalUnavailable leaves the page undone; the poll goes on). A ledger
# that cannot be used (full, unwritable) sends nothing outside: degraded
# state_unavailable, the events go to the journal only.

import os
import time

# INV-8: the pauses before the 2nd, 3rd and 4th call of a channel that
# refused before send.
RETRY_DELAYS_SEC = (5, 30, 120)
MAX_CALLS = 1 + len(RETRY_DELAYS_SEC)
# How often the working loop appends deferred journal tails (P-12).
TAILS_EVERY_SEC = 60
# The step of a wait: the cancel signal is checked between steps.
WAIT_STEP_SEC = 1.0
# The file next to the token where the agent writes the session id of its
# own echo (P-5).
EXCLUDE_FILE_NAME = "exclude_session"
# The answer sign of a call that was not made.
_NOT_SENT = "notSent"


class DeliveryCancelled(Exception):
    """The cancel signal arrived before the text was handed to a channel:
    reason is what the signal said (token_expired, visibility_lost,
    stopped)."""

    def __init__(self, reason):
        Exception.__init__(self, reason)
        self.reason = reason


class AddresseeChanged(Exception):
    """The addressee is not the one of the preflight (before the first
    transfer)."""


class AddresseeGone(Exception):
    """The channel's check says the addressee has ended."""


class PollStale(Exception):
    """The last successful poll is older than two intervals: no send until a
    fresh one (US-12); not a refusal of the channel."""


class RefusedBeforeSend(Exception):
    """Nothing was handed to the channel: the addressee is not ready (not in
    the foreground, busy), or the call could not be made."""

    def __init__(self, reason):
        Exception.__init__(self, reason)
        self.reason = reason


class NoCancel(object):
    """The cancel signal of a pipeline without a poll: never cancels."""

    def check(self):
        return None

    def may_send(self):
        return True


class SystemClock(object):
    def now(self):
        return time.time()

    def sleep(self, sec):
        if sec > 0:
            time.sleep(sec)


def identity_from_doc(doc):
    """The Identity of preflight.json's identity document."""
    if not isinstance(doc, dict):
        return None
    process = doc.get("process")
    addresses = [Address(a["channel"], a["value"], tuple(tuple(q) for q in a.get("qualifiers") or []))
                 for a in doc.get("addresses") or []]
    return Identity(tuple(process) if process else None, addresses, list(doc.get("inherited") or []),
                    doc.get("terminal"))


class Guard(object):
    """The checks of one attempt. before_send counts the call in the ledger,
    then checks the cancel signal, the addressee and the idle wait; it raises
    when nothing may be handed over. before_commit is asked before the
    final step of a two-step driver; False withholds it."""

    def __init__(self, pipeline, channel, attempt, timeout):
        self.pipeline = pipeline
        self.channel = channel
        self.attempt = attempt
        self.timeout = timeout
        # Set when before_commit failed because access was lost: the
        # journal line goes without the preview.
        self.access_lost = False

    def before_send(self):
        reason = self.pipeline.cancel.check()
        if reason is not None:
            raise DeliveryCancelled(reason)
        if not self.pipeline.cancel.may_send():
            raise PollStale()
        self.attempt.note_call()
        self.pipeline.check_addressee(self.channel)
        self.pipeline.wait_idle(self.channel, self.timeout)

    def before_commit(self):
        if self.pipeline.cancel.check() is not None:
            self.access_lost = True
            return False
        try:
            self.pipeline.check_addressee(self.channel)
        except (AddresseeChanged, AddresseeGone, RefusedBeforeSend):
            return False
        return True


class _ReceiptGuard(Guard):
    """The guard of a stop receipt: no event, no ledger attempt, no cancel
    (the stop is already decided); the addressee is checked as for any
    send."""

    def __init__(self, pipeline, channel, timeout):
        Guard.__init__(self, pipeline, channel, None, timeout)

    def before_send(self):
        self.pipeline.check_addressee(self.channel)
        self.pipeline.wait_idle(self.channel, self.timeout)

    def before_commit(self):
        try:
            self.pipeline.check_addressee(self.channel)
        except (AddresseeChanged, AddresseeGone, RefusedBeforeSend):
            return False
        return True


class ArgvDriver(object):
    """One call of the channel program carries the text."""

    def __init__(self, channel, executor):
        self.channel = channel
        self.executor = executor

    def send(self, text, guard):
        guard.before_send()
        spec = self.channel.get("executor") or {}
        if spec.get("type") != "argv":
            raise RefusedBeforeSend("executor %s is not available" % spec.get("type"))
        try:
            result = self.executor.call(spec, text, guard.timeout)
        except ExecutorRefusal as exc:
            raise RefusedBeforeSend(exc.reason)
        return classify_result(self.channel.get("outcomes"), result)


class TwoStepDriver(object):
    """A step that may write the text (an insert) and a final step that
    submits it (Enter), each an argv executor with its own closed table.
    The insert's table proves "input-written" or a refusal; once written,
    the outcome is the driver's: before the final step it asks the guard,
    and a refused check means no final step and "unknown"."""

    def __init__(self, channel, executor, insert, commit):
        self.channel = channel
        self.executor = executor
        self.insert = insert
        self.commit = commit

    def send(self, text, guard):
        guard.before_send()
        try:
            first = self.executor.call(self.insert["executor"], text, guard.timeout)
        except ExecutorRefusal as exc:
            raise RefusedBeforeSend(exc.reason)
        outcome, sign = classify_result(self.insert.get("outcomes"), first)
        if outcome != OUTCOME_INPUT_WRITTEN:
            return outcome, sign
        if not guard.before_commit():
            return OUTCOME_UNKNOWN, {"commit": "withheld"}
        try:
            second = self.executor.call(self.commit["executor"], "", guard.timeout)
        except ExecutorRefusal:
            return OUTCOME_UNKNOWN, {"commit": "notStarted"}
        return classify_result(self.commit.get("outcomes"), second, written=True)


class DeliverySink(object):
    """The sink of a detached monitor: work() hands it every page."""

    def __init__(self, journal, token_file, doc, registry, env, ledger, executor, issue,
                 cancel=None, clock=None, drivers=None, idle_window_sec=None):
        self.journal = journal
        self.token_file = token_file
        self.doc = doc
        self.registry = registry
        self.env = env
        self.ledger = ledger
        self.executor = executor
        self.issue = issue
        self.cancel = cancel or NoCancel()
        self.clock = clock or SystemClock()
        # Drivers by channel id; a channel without one gets an ArgvDriver.
        self.drivers = dict(drivers or {})
        self.idle_window = TOKEN_IDLE_SEC if idle_window_sec is None else idle_window_sec
        # The chosen channel, then the non-blind ones the preflight found
        # after it (a forced channel has none): a blind channel is never a
        # fallback (INV-3).
        self.channels = [doc.get("channel")] + [c for c in doc.get("fallbacks") or []
                                                if not registry.is_blind(c)]
        self.identity = doc.get("identity") or {}
        # The exclusion file of one's own echo (P-5, US-13): its literal path
        # goes into every plain delivery once.
        self.exclude_file = os.path.join(os.path.dirname(os.path.abspath(token_file)), EXCLUDE_FILE_NAME)
        # Channels whose budget ran out: a delivery by one writes recovered.
        self.failing = set()
        # After "addressee changed" the process writes only the journal.
        self.journal_only = False
        # Set when the channel's check says the addressee has ended.
        self.gone = False
        self._said = set()
        self._tails_at = None
        # The answer of the last refusal before send, for channel_failing.
        self._last_refusal = None
        # The cursor of the last page (the one a cursor_reset rejects).
        self._cursor = None

    # --- lines ----------------------------------------------------------------

    def lifecycle(self, line):
        fields = dict(line)
        event = fields.pop("event")
        try:
            self.journal.lifecycle(event, **fields)
            if event == "degraded" and fields.get("cause") == "cursor_reset":
                # The events between the rejected cursor and the new start
                # have no line of their own: one summary says so.
                self.journal.summary_cursor_reset(self._cursor)
        except JournalUnavailable:
            pass

    def _degraded_once(self, cause, **fields):
        if cause in self._said:
            return
        self._said.add(cause)
        self.lifecycle(dict(fields, event="degraded", cause=cause, issue=self.issue))

    # --- checks -----------------------------------------------------------------

    def check_addressee(self, channel):
        """INV-4 before a transfer: the agent process of the preflight for a
        terminal channel (alive, same start; in the foreground where the
        record asks), the record's own check where it has one."""
        check = channel.get("addressee") or {}
        mode = check.get("process")
        if mode in ("alive", "foreground"):
            process = self.identity.get("process")
            if not process:
                raise AddresseeChanged()
            try:
                info = current_platform().process_info(process[0])
            except PlatformRefusal:
                info = None
            if info is None or info.start != process[1]:
                raise AddresseeChanged()
            if mode == "foreground" and not info.foreground():
                raise RefusedBeforeSend("the agent process is not in the foreground")
        command = check.get("command")
        if command:
            try:
                argv = expand_argv(command.get("argv") or [], self.env)
                result = run_program(argv, child_env(self.env, self.executor.secrets), None,
                                     (channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC)
            except ExecutorRefusal as exc:
                raise RefusedBeforeSend(exc.reason)
            verdict = None
            if not result.timed_out:
                for row in command.get("results") or []:
                    if outcome_sign_matches(row.get("match") or {}, exit_code=result.exit_code, doc=result.json):
                        verdict = row.get("result")
                        break
            if verdict == "gone":
                raise AddresseeGone()
            if verdict == "changed" or (verdict is None and channel.get("kind") == "terminal"):
                raise AddresseeChanged()

    def _same_process(self, channel):
        """For a channel that checks the agent process: it is still the
        process of the preflight (same pid and start). True for a channel
        that does not check it."""
        if (channel.get("addressee") or {}).get("process") not in ("alive", "foreground"):
            return True
        process = self.identity.get("process")
        if not process:
            return False
        try:
            info = current_platform().process_info(process[0])
        except PlatformRefusal:
            return False
        return info is not None and info.start == process[1]

    def wait_idle(self, channel, timeout):
        """The idle wait of a record that runs a command for it: exit code 0
        means idle; anything else, or no answer in time, is a refusal before
        send (the agent is busy)."""
        idle = channel.get("idle") or {}
        if idle.get("type") != "argv":
            return
        try:
            argv = expand_argv(idle.get("argv") or [], self.env)
            result = run_program(argv, child_env(self.env, self.executor.secrets), None,
                                 min(idle.get("timeoutSec") or timeout, timeout))
        except ExecutorRefusal as exc:
            raise RefusedBeforeSend(exc.reason)
        if result.timed_out or result.exit_code != 0:
            raise RefusedBeforeSend("the agent is not idle")

    def _wait(self, until):
        """Sleep until the time until, checking the cancel signal."""
        while True:
            reason = self.cancel.check()
            if reason is not None:
                raise DeliveryCancelled(reason)
            left = until - self.clock.now()
            if left <= 0:
                return
            self.clock.sleep(min(WAIT_STEP_SEC, left))

    def _maybe_append_tails(self):
        now = self.clock.now()
        if self._tails_at is not None and now - self._tails_at < TAILS_EVERY_SEC:
            return
        self._tails_at = now
        try:
            append_tails(self.env)
        except LedgerUnavailable:
            pass

    def note_poll_ok(self):
        """A successful poll of the working loop (the access boundary)."""
        if hasattr(self.cancel, "note_ok"):
            self.cancel.note_ok()

    # --- the page -------------------------------------------------------------

    def page(self, cursor, notifications):
        """Carry every event of the page to an outcome. Raises
        JournalUnavailable (the page is not done: the cursor stays), or Stop
        when access is lost or the addressee has ended: the waiting events
        get their outcome first (finalize.py)."""
        self._cursor = cursor
        if hasattr(self.cancel, "cursor"):
            # A poll during a wait asks from the last fixed outcome.
            self.cancel.cursor = cursor
        state = {"pending": []}
        try:
            self._page(cursor, notifications, state)
        except DeliveryCancelled as exc:
            outcomes, journaled = state["outcomes"], state["journaled"]
            waiting = [n for n in state["pending"] if str(n["id"]) not in outcomes]
            close_waiting(self, cursor, outcomes, journaled, waiting, OUTCOME_ACCESS_LOST, exc.reason)
            raise access_stop(exc.reason)
        if self.gone:
            outcomes, journaled = state["outcomes"], state["journaled"]
            waiting = [n for n in state["pending"] if str(n["id"]) not in outcomes]
            close_waiting(self, cursor, outcomes, journaled, waiting, OUTCOME_ADDRESSEE_GONE, "addressee_gone")
            raise Stop("addressee_gone", delete_token=True)

    def _page(self, cursor, notifications, state):
        self._maybe_append_tails()
        events = [n for n in notifications if isinstance(n, dict) and n.get("id") is not None]
        outcomes, journaled = load_page_outcomes(self.token_file, cursor)
        state["outcomes"], state["journaled"] = outcomes, journaled
        byid = dict((str(n["id"]), n) for n in events)
        # Outcomes the ledger and the state hold but the journal does not
        # yet: they come first after the journal is back.
        for eid, status in list(outcomes.items()):
            if eid not in journaled and eid in byid:
                self._journal(cursor, outcomes, journaled, byid[eid], status, None, None)
        pending = [n for n in events if str(n["id"]) not in outcomes]
        state["pending"] = list(pending)
        step = dict((str(n["id"]), 0) for n in pending)
        reason = self.cancel.check()
        if reason is not None:
            raise DeliveryCancelled(reason)
        while pending:
            if self.gone:
                return
            if self.journal_only:
                for n in pending:
                    self._close(cursor, outcomes, journaled, n, JOURNAL_ONLY_STATUS, None, None)
                return
            head = pending[0]
            index = step[str(head["id"])]
            if index >= len(self.channels):
                # Every channel ran out of its budget for this event.
                self._close(cursor, outcomes, journaled, head, OUTCOME_REFUSED, None, None)
                pending.pop(0)
                continue
            channel_id = self.channels[index]
            channel = self.registry.channel(channel_id) or {}
            group = [n for n in pending if step[str(n["id"])] == index]
            limit, unit = _line_limit(channel)
            text, count, _ = plain_batch(group, self.exclude_file, limit, unit)
            if text is None:
                step[str(head["id"])] = index + 1
                if index + 1 >= len(self.channels):
                    self.ledger.record([str(head["id"])], OUTCOME_UNFIT, channel_id)
                    self._close(cursor, outcomes, journaled, head, OUTCOME_UNFIT, channel_id, None)
                    pending.pop(0)
                continue
            batch = group[:count]
            try:
                decision = self.ledger.begin([{"id": str(n["id"]), "session": n.get("actorSessionId")}
                                              for n in batch], channel_id)
            except LedgerUnavailable:
                self._degraded_once("state_unavailable",
                                    action="The ledger of deliveries cannot be used; events of issue %s "
                                           "go only to the journal." % self.issue)
                for n in pending:
                    self._close(cursor, outcomes, journaled, n, JOURNAL_ONLY_STATUS, None, None)
                return
            done = set()
            for eid, status in decision.closed.items():
                self._close(cursor, outcomes, journaled, byid[eid], status, channel_id, None)
                done.add(eid)
            for eid, rec in decision.own.items():
                self._close(cursor, outcomes, journaled, byid[eid], rec.get("outcome"), rec.get("channel"), None)
                done.add(eid)
            pending = [n for n in pending if str(n["id"]) not in done]
            if decision.attempt is None:
                if decision.wait_until is not None:
                    self._wait(decision.wait_until)
                elif decision.pending:
                    self._wait(self.clock.now() + WAIT_STEP_SEC)
                continue
            self._deliver(cursor, outcomes, journaled, decision.attempt, channel_id, channel,
                          [byid[e] for e in decision.published], step)
            pending = [n for n in pending if str(n["id"]) not in outcomes]

    def _deliver(self, cursor, outcomes, journaled, attempt, channel_id, channel, batch, step):
        now = self.clock.now()
        deadline = (attempt.first_call if attempt.first_call is not None else now) + self.idle_window
        if attempt.calls >= MAX_CALLS or (attempt.calls and now >= deadline):
            attempt.finish(OUTCOME_REFUSED)
            attempt.release()
            self._exhausted(channel_id, batch, step)
            return
        if attempt.calls:
            due = (attempt.last_call or now) + RETRY_DELAYS_SEC[attempt.calls - 1]
            if due > now:
                attempt.abandon()
                attempt.release()
                if due >= deadline:
                    self._exhausted(channel_id, batch, step)
                    return
                self._wait(due)
                return
        timeout = min((channel.get("executor") or {}).get("timeoutSec") or self.idle_window,
                      max(deadline - now, 1))
        if len(batch) < len(attempt.events):
            batch = [n for n in batch if str(n["id"]) in attempt.events]
        limit, unit = _line_limit(channel)
        text, _, _ = plain_batch(batch, self.exclude_file, limit, unit)
        guard = Guard(self, channel, attempt, timeout)
        driver = self.drivers.get(channel_id) or ArgvDriver(channel, self.executor)
        try:
            try:
                outcome, sign = driver.send(text, guard)
            except PollStale:
                attempt.abandon()
                self._wait(self.clock.now() + WAIT_STEP_SEC)
                return
            except RefusedBeforeSend as exc:
                outcome, sign = OUTCOME_REFUSED, {_NOT_SENT: exc.reason}
            except AddresseeChanged:
                outcome, sign = "addressee-changed", {_NOT_SENT: "addressee changed"}
            except AddresseeGone:
                outcome, sign = OUTCOME_ADDRESSEE_GONE, None
            except DeliveryCancelled:
                attempt.abandon()
                raise
            except StopSignal:
                # A stop while the channel may have the text: unknown, never
                # a second send (INV-9).
                attempt.finish(OUTCOME_UNKNOWN)
                raise
            changed = outcome == "addressee-changed"
            if not changed and outcome != OUTCOME_REFUSED and not self._same_process(channel):
                # The agent process changed while a one-step call ran: the
                # text may have reached another process ("unknown", never
                # sent again), later events go to the journal.
                changed = True
            if changed:
                # Not sent; the event's outcome is "unknown" (the table of
                # outcomes), later events go to the journal.
                outcome = OUTCOME_UNKNOWN
            burst = attempt.finish(outcome)
        finally:
            attempt.release()
        if burst:
            self.lifecycle({"event": "degraded", "cause": "single_session_burst", "issue": self.issue,
                            "actorSessionId": batch[0].get("actorSessionId")})
        if outcome == OUTCOME_REFUSED:
            # Kept pending: the next turn calls again when due, or switches.
            self._last_refusal = sign
            return
        for n in batch:
            self._close(cursor, outcomes, journaled, n, outcome, channel_id, sign,
                        with_preview=not guard.access_lost)
        if outcome == OUTCOME_ADDRESSEE_GONE:
            # The stop follows when the page loop returns (page()).
            self.gone = True
        elif changed:
            self.journal_only = True
            self._degraded_once("addressee_changed",
                                action="The addressee of the monitor of issue %s is not the one its "
                                       "preflight saw: events go only to the journal. Run the preflight "
                                       "again to take the monitor over." % self.issue)
        elif outcome == OUTCOME_UNKNOWN:
            self.lifecycle({"event": "degraded", "cause": "delivery_unknown", "issue": self.issue,
                            "channel": channel_id, "notificationIds": [n["id"] for n in batch]})
        elif outcome in DELIVERED_OUTCOMES and channel_id in self.failing:
            self.failing.discard(channel_id)
            self.lifecycle({"event": "recovered", "issue": self.issue, "channel": channel_id})

    def receipt(self, text):
        """The receipt of a stop that keeps access (US-12): one text by the
        first channel of the preflight, after the addressee check, within
        the budget of INV-8. A failure leaves only the journal line."""
        channel_id = self.channels[0]
        channel = self.registry.channel(channel_id) or {}
        driver = self.drivers.get(channel_id) or ArgvDriver(channel, self.executor)
        start = self.clock.now()
        for calls in range(MAX_CALLS):
            if calls:
                due = start + sum(RETRY_DELAYS_SEC[:calls])
                if due - start >= self.idle_window:
                    return
                while self.clock.now() < due:
                    self.clock.sleep(min(WAIT_STEP_SEC, due - self.clock.now()))
            guard = _ReceiptGuard(self, channel, (channel.get("executor") or {}).get("timeoutSec")
                                  or PROBE_TIMEOUT_SEC)
            try:
                outcome, _ = driver.send(text, guard)
            except (RefusedBeforeSend, PollStale):
                continue
            except (AddresseeChanged, AddresseeGone, DeliveryCancelled):
                return
            if outcome != OUTCOME_REFUSED:
                return

    def _exhausted(self, channel_id, batch, step):
        """The budget of the channel ran out for the batch: one
        channel_failing for this switch, the events move to the next
        channel."""
        self.failing.add(channel_id)
        self.lifecycle({"event": "degraded", "cause": "channel_failing", "issue": self.issue,
                        "channel": channel_id, "lastAnswer": self._last_refusal})
        for n in batch:
            step[str(n["id"])] += 1

    def _close(self, cursor, outcomes, journaled, n, status, channel, sign, with_preview=True):
        """The outcome of one event: into the state, then the journal."""
        eid = str(n["id"])
        outcomes[eid] = status
        save_page_outcomes(self.token_file, cursor, outcomes, journaled)
        self._journal(cursor, outcomes, journaled, n, status, channel, sign, with_preview)

    def _journal(self, cursor, outcomes, journaled, n, status, channel, sign, with_preview=True):
        self.journal.delivery(n, status, method=self.doc.get("method"), channel=channel, answer=sign,
                              with_preview=with_preview)
        journaled.add(str(n["id"]))
        save_page_outcomes(self.token_file, cursor, outcomes, journaled)


def _line_limit(channel):
    line = (channel.get("limits") or {}).get("line") or {}
    if line.get("max"):
        return line["max"], line.get("unit") or UNIT_CHARS
    return None, UNIT_CHARS
