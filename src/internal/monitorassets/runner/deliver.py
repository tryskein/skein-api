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
# that cannot be used (full, unwritable) - at any step: the attempt, a
# counted call, an outcome, a withdrawal, an outcome without a call - sends
# nothing outside for the rest of the process: degraded state_unavailable
# once, the events go to the journal only, the poll goes on. An outcome a
# call already gave keeps its outcome in the state and the journal.

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


class EchoNow(Exception):
    """An event of the batch became one's own echo after the batch was
    assembled (the exclusion file changed): nothing is handed over, the
    attempt is withdrawn and the events are taken again (US-13)."""


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

    def known(self):
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
    addresses = [Address(a["channel"], a["value"], tuple(tuple(q) for q in a.get("qualifiers") or []), a.get("env"))
                 for a in doc.get("addresses") or []]
    return Identity(tuple(process) if process else None, addresses, list(doc.get("inherited") or []),
                    doc.get("terminal"))


def _call_seconds(channel, fallback):
    """The own timeout of the channel's call (executor.timeoutSec)."""
    return (channel.get("executor") or {}).get("timeoutSec") or fallback


# The time an attempt keeps for its two addressee checks besides the wait
# and the call.
_CHECKS_SEC = 2 * 10


def _attempt_seconds(channel, fallback):
    """The time of one attempt: the idle wait's own, the call's own and the
    checks around them."""
    idle = _idle_seconds(channel)
    return _call_seconds(channel, fallback) + (idle + _CHECKS_SEC if idle else 0)


def _idle_seconds(channel):
    """The own timeout of the channel's idle wait: 0 without an argv wait."""
    idle = channel.get("idle") or {}
    if idle.get("type") != "argv":
        return 0
    return idle.get("timeoutSec") or 0


class Guard(object):
    """The checks of one attempt, within one deadline for all its steps (the
    address check, the idle wait and the call share the attempt's timeout).
    before_send checks the access boundary - no stop answer, the last
    successful poll at most two intervals old (US-12) - then the addressee
    and the idle wait, then the boundary again right before the text is
    handed over, and counts the call in the ledger last; it raises when
    nothing may be handed over. before_commit is asked before the final step
    of a two-step driver: the boundary, then the addressee; False withholds
    the step, and what failed is kept (access lost, addressee changed or
    gone)."""

    def __init__(self, pipeline, channel, attempt, timeout, batch=()):
        self.pipeline = pipeline
        self.channel = channel
        self.attempt = attempt
        # The events of the text, checked against the exclusion in force
        # right before the hand-over.
        self.batch = list(batch)
        self.deadline = pipeline.clock.now() + timeout
        self.total = timeout
        # Set when before_commit failed because access was lost: the
        # journal line goes without the preview.
        self.access_lost = False
        # Set when before_commit found another or an ended addressee.
        self.changed = False
        self.gone = False
        # Set once the text may be handed to the channel (every check held).
        self.handed = False
        # The stop answer before_commit saw: the monitor stops on it once
        # the outcome of the batch is written.
        self.cancel_reason = None

    def remaining(self):
        """The time left of the attempt's deadline; RefusedBeforeSend when
        none is left (nothing is handed over)."""
        left = self.deadline - self.pipeline.clock.now()
        if left <= 0:
            raise RefusedBeforeSend("the time of the call ran out")
        return left

    def boundary(self):
        """The access boundary: DeliveryCancelled for a stop answer,
        PollStale while the last successful poll is too old."""
        reason = self.pipeline.cancel.check()
        if reason is not None:
            raise DeliveryCancelled(reason)
        if not self.pipeline.cancel.may_send():
            raise PollStale()

    def call_timeout(self):
        """The time the call of the channel gets: its own timeout, within
        what is left of the attempt."""
        return min(self.remaining(), _call_seconds(self.channel, self.remaining()))

    def reserve(self):
        """The time kept for the call: its own timeout, less when the
        attempt is shorter than the wait, the checks and the call together
        (a short idle window)."""
        return max(min(_call_seconds(self.channel, 0),
                       self.total - _idle_seconds(self.channel) - _CHECKS_SEC), 0)

    def wait_and_keep_the_call(self):
        """The idle wait within its own timeout and never into the time kept
        for the call."""
        idle = _idle_seconds(self.channel)
        room = self.remaining() - self.reserve()
        self.pipeline.wait_idle(self.channel, max(min(room, idle) if idle else room, 0.001))

    def keep_the_call(self):
        """After the last check: the time kept for the call must be left, or
        nothing is sent (a refusal before send)."""
        if self.remaining() < self.reserve():
            raise RefusedBeforeSend("the idle wait left the call too little of its time")

    def before_send(self):
        self.boundary()
        # Counted first: a refusal of the checks below spends the budget of
        # INV-8 like a refusal of the channel.
        self.count_call()
        self.pipeline.check_addressee(self.channel, self.remaining())
        self.wait_and_keep_the_call()
        # After the wait: the access boundary, which may poll the server -
        # the last step that may take time on the network - then the
        # addressee it waited for and the reservation of every event (a
        # merge of the addressee's records may have taken one meanwhile).
        self.boundary()
        self.pipeline.check_addressee(self.channel, self.remaining())
        self.keep_the_call()
        self.check_echo()
        self.attempt.confirm()
        # Last, right before the hand-over and without the network: what is
        # already known of access (a stop answer, the age of the last poll).
        self.local_boundary()
        self.remaining()
        self.handed = True
        self.attempt.handed = True

    def check_echo(self):
        """Read the exclusion file again (local, no poll): EchoNow when an
        event of the text is now one's own echo."""
        exclusion = getattr(self.pipeline, "exclusion", None)
        current = getattr(exclusion, "current", None)
        if current is None:
            return
        current()
        if any(exclusion.own(n) for n in self.batch):
            raise EchoNow()

    def local_boundary(self):
        """The access boundary from what is known now, without a poll:
        DeliveryCancelled for a stop answer already received, PollStale
        while the last successful poll is too old."""
        known = getattr(self.pipeline.cancel, "known", None)
        reason = known() if known is not None else None
        if reason is not None:
            raise DeliveryCancelled(reason)
        if not self.pipeline.cancel.may_send():
            raise PollStale()

    def count_call(self):
        self.attempt.note_call()

    def before_commit(self):
        # The boundary that may poll the server first, then the addressee,
        # so no network wait comes between the addressee check and Enter.
        if not self._commit_boundary(self.pipeline.cancel.check):
            return False
        try:
            self.pipeline.check_addressee(self.channel, self.remaining())
        except AddresseeChanged:
            self.changed = True
            return False
        except AddresseeGone:
            self.gone = True
            return False
        except RefusedBeforeSend:
            return False
        # Last, right before the final step and without the network: a stop
        # answer already received or a poll grown stale withholds it.
        return self._commit_boundary(getattr(self.pipeline.cancel, "known", None))

    def _commit_boundary(self, ask):
        reason = ask() if ask is not None else None
        if reason is not None:
            self.access_lost = True
            self.cancel_reason = reason
            return False
        return bool(self.pipeline.cancel.may_send())


class _ReceiptGuard(Guard):
    """The guard of a stop receipt: no event, no ledger attempt, no cancel
    (the stop is already decided); the addressee is checked as for any
    send, within the receipt's deadline."""

    def __init__(self, pipeline, channel, timeout):
        Guard.__init__(self, pipeline, channel, None, timeout)

    def before_send(self):
        # The order of every transfer: the check, the wait, the check again
        # (the agent of a PTY may change during the wait), then nothing that
        # waits or goes to the network before the hand-over.
        self.pipeline.check_addressee(self.channel, self.remaining())
        self.wait_and_keep_the_call()
        self.pipeline.check_addressee(self.channel, self.remaining())
        self.keep_the_call()

    def before_commit(self):
        try:
            self.pipeline.check_addressee(self.channel, self.remaining())
        except (AddresseeChanged, AddresseeGone, RefusedBeforeSend):
            return False
        return True


class ArgvDriver(object):
    """One call of the channel's executor (its program, or its request)
    carries the text."""

    def __init__(self, channel, executor):
        self.channel = channel
        self.executor = executor

    def send(self, text, guard):
        guard.before_send()
        spec = self.channel.get("executor") or {}
        if spec.get("type") not in ("argv", "http"):
            raise RefusedBeforeSend("executor %s is not available" % spec.get("type"))
        try:
            result = self.executor.call(spec, text, guard.call_timeout())
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
            first = self.executor.call(self.insert["executor"], text, guard.call_timeout())
        except ExecutorRefusal as exc:
            raise RefusedBeforeSend(exc.reason)
        outcome, sign = classify_result(self.insert.get("outcomes"), first)
        if outcome != OUTCOME_INPUT_WRITTEN:
            return outcome, sign
        if not guard.before_commit():
            return OUTCOME_UNKNOWN, {"commit": "withheld"}
        try:
            second = self.executor.call(self.commit["executor"], "", guard.remaining())
        except (ExecutorRefusal, RefusedBeforeSend):
            return OUTCOME_UNKNOWN, {"commit": "notStarted"}
        return classify_result(self.commit.get("outcomes"), second, written=True)


class DeliverySink(object):
    """The sink of a detached monitor: work() hands it every page."""

    def __init__(self, journal, token_file, doc, registry, env, ledger, executor, issue,
                 cancel=None, clock=None, drivers=None, idle_window_sec=None, exclusion=None):
        self.journal = journal
        self.token_file = token_file
        self.doc = doc
        self.registry = registry
        self.env = env
        self.ledger = ledger
        self.executor = executor
        # The secrets of the channels (the keys of the http channels) are
        # the journal's too: a preview that carries one is journaled
        # redacted (INV-6).
        for secret in getattr(executor, "secrets", None) or []:
            journal.add_secret(secret)
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
        self.exclude_file = exclude_file_path(token_file)
        # The exclusion in force (echo.py), shared with the polls that read
        # it again; None applies none.
        self.exclusion = as_exclusion(exclusion) if exclusion is not None else None
        # Channels whose budget ran out: a delivery by one writes recovered.
        self.failing = set()
        # (event id, channel id) of events a channel refused as too long (a
        # shortening row of its table): that channel's next call carries
        # them, and the events that join them, in the minimal form.
        self._shorten = set()
        # After "addressee changed" the process writes only the journal.
        self.journal_only = False
        # Set when the channel's check says the addressee has ended.
        self.gone = False
        # Set once the ledger could not be used: no external delivery.
        self.ledger_off = False
        self._said = set()
        self._tails_at = None
        # The answer of the last refusal before send, for channel_failing.
        self._last_refusal = None
        # The cursor of the last page (the one a cursor_reset rejects).
        self._cursor = None
        # Ids of the current page whose journal line goes without the
        # preview (kept in the state with the outcomes).
        self._redacted = set()
        # A page left undone by the journal, (cursor, notifications): a stop
        # by lost access closes it (end_open_page).
        self._open = None

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

    def _ledger_down(self):
        """The ledger refused: no external delivery from now on."""
        self.ledger_off = True
        self._degraded_once("state_unavailable",
                            action="The ledger of deliveries cannot be used; events of issue %s "
                                   "go only to the journal." % self.issue)

    def _ledger_step(self, step, *args):
        """A step of the ledger whose refusal turns external delivery off;
        returns its value, or None when it was refused."""
        try:
            return step(*args)
        except LedgerUnavailable:
            self._ledger_down()
            return None

    def _degraded_once(self, cause, **fields):
        if cause in self._said:
            return
        self._said.add(cause)
        self.lifecycle(dict(fields, event="degraded", cause=cause, issue=self.issue))

    # --- checks -----------------------------------------------------------------

    def check_addressee(self, channel, timeout=None):
        """INV-4 before a transfer: the agent process of the preflight for a
        terminal channel (alive, same start; in the foreground where the
        record asks), the address of the preflight - the call takes its
        address from the environment of this run, which must still name the
        addressee the preflight saw (value and qualifiers) - and the
        record's own check where it has one, run within timeout."""
        for a in self.identity.get("addresses") or []:
            if a.get("channel") != channel.get("id"):
                continue
            if a.get("env") and self.env.get(a["env"], "") != a.get("value"):
                raise AddresseeChanged()
            paths = (channel.get("address") or {}).get("qualifierPaths")
            for q in a.get("qualifiers") or []:
                if len(q) == 2 and qualifier_value(q[0], self.env, paths) != q[1]:
                    raise AddresseeChanged()
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
        life = check.get("rootLife")
        if life:
            self._check_root_life(channel, life)
        command = check.get("command")
        if command:
            try:
                env = child_env(self.env, self.executor.secrets)
                argv = self._program(expand_argv(command.get("argv") or [], env))
                limit = (channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC
                result = run_program(argv, env, None, limit if timeout is None else min(limit, timeout),
                                     self.executor.secrets)
            except ExecutorRefusal as exc:
                raise RefusedBeforeSend(exc.reason)
            verdict = None
            if not result.timed_out:
                for row in command.get("results") or []:
                    if outcome_sign_matches(row.get("match") or {}, exit_code=result.exit_code, doc=result.json,
                                            stderr=result.sign_stderr):
                        verdict = row.get("result")
                        break
            # Only "same" hands over (INV-4): "changed" is another
            # addressee, "gone" an addressee that ended (a stop); a timeout,
            # a failed check or an answer the table does not know proves
            # neither, and nothing was sent: a refusal before send, within
            # the budget of INV-8.
            if verdict == "gone":
                raise AddresseeGone()
            if verdict == "changed":
                raise AddresseeChanged()
            if verdict != "same":
                raise RefusedBeforeSend("the addressee check gave no answer of its table")
            # The values the preflight saw (its process incarnation), a path
            # it saw absent absent again: anything else, or no saved value,
            # is another addressee.
            saved = dict((p, v) for p, v in ((self.identity.get("captures") or {}).get(channel.get("id")) or []))
            for path in command.get("capture") or []:
                if path not in saved or result.timed_out or result.json is None or \
                        capture_value(result.json, path) != saved[path]:
                    raise AddresseeChanged()

    def _program(self, argv):
        """argv with its program as the preflight found it (preflight.json
        "programs": a path under the home directory, the agent's own
        executable, the program a variable names)."""
        programs = getattr(self.executor, "programs", None)
        if programs is None:
            # An executor without the programs of the preflight runs no
            # check by a bare name.
            raise RefusedBeforeSend("no programs of the preflight")
        if argv and argv[0] in programs:
            argv = [programs[argv[0]]] + list(argv[1:])
        elif getattr(self.executor, "strict", False):
            raise RefusedBeforeSend("program %s is not one the preflight found" % (argv[0] if argv else ""))
        return argv

    def _check_root_life(self, channel, life):
        """The root process of the addressee lives the life the preflight saw
        (the qualifiers of its address): alive, with the same start and boot
        id. Its start, as the host records it, is field 22 of
        /proc/<pid>/stat with the boot id of the system, the form of the
        start of the Linux platform layer. Another life, or one that cannot
        be read, is a changed addressee."""
        saved = {}
        for a in self.identity.get("addresses") or []:
            if a.get("channel") == channel.get("id"):
                saved.update((q[0], q[1]) for q in a.get("qualifiers") or [] if len(q) == 2)
        pid, start, boot = (saved.get(life.get(k), "") for k in ("pidEnv", "startEnv", "bootIdEnv"))
        if not (pid.isdigit() and start and boot):
            raise AddresseeChanged()
        try:
            info = current_platform().process_info(int(pid))
        except PlatformRefusal:
            info = None
        if info is None or info.start != "%s:%s" % (boot, start):
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
            env = child_env(self.env, self.executor.secrets)
            argv = self._program(expand_argv(idle.get("argv") or [], env))
            result = run_program(argv, env, None, min(idle.get("timeoutSec") or timeout, timeout),
                                 self.executor.secrets)
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

    def taken_over(self):
        """A runner that writes only the journal (its addressee changed)
        hands over to a successor that waits for its lock (takeover.py)."""
        return self.journal_only and takeover_requested(self.token_file, self.doc.get("preflightId"))

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
        self._open = (cursor, notifications)
        try:
            self._page(cursor, notifications, state)
        except JournalUnavailable:
            # Undone: the same page comes again, or a stop closes it.
            raise
        except DeliveryCancelled as exc:
            self._open = None
            outcomes, journaled = state["outcomes"], state["journaled"]
            waiting = [n for n in state["pending"] if str(n["id"]) not in outcomes]
            # Outcomes fixed on this page but not in the journal yet (it
            # refused their line) go with the stop too, without the preview,
            # into the journal or the deferred tail.
            byid = dict((str(n["id"]), n) for n in notifications if isinstance(n, dict) and n.get("id") is not None)
            known = [(byid[eid], status) for eid, status in sorted(outcomes.items())
                     if eid not in journaled and eid in byid]
            close_waiting(self, cursor, outcomes, journaled, waiting, OUTCOME_ACCESS_LOST, exc.reason, known=known)
            raise access_stop(exc.reason)
        self._open = None
        if self.gone:
            outcomes, journaled = state["outcomes"], state["journaled"]
            waiting = [n for n in state["pending"] if str(n["id"]) not in outcomes]
            close_waiting(self, cursor, outcomes, journaled, waiting, OUTCOME_ADDRESSEE_GONE, "addressee_gone")
            raise Stop("addressee_gone", delete_token=True)

    def _page(self, cursor, notifications, state):
        self._maybe_append_tails()
        events = [n for n in notifications if isinstance(n, dict) and n.get("id") is not None]
        outcomes, journaled = load_page_outcomes(self.token_file, cursor)
        self._redacted = load_page_redacted(self.token_file, cursor)
        state["outcomes"], state["journaled"] = outcomes, journaled
        byid = dict((str(n["id"]), n) for n in events)
        # Outcomes the ledger and the state hold but the journal does not
        # yet: they come first after the journal is back, with the preview
        # decision they were given.
        for eid, status in list(outcomes.items()):
            if eid not in journaled and eid in byid:
                self._journal(cursor, outcomes, journaled, byid[eid], status, None, None,
                              with_preview=eid not in self._redacted)
        pending = [n for n in events if str(n["id"]) not in outcomes]
        state["pending"] = list(pending)
        step = dict((str(n["id"]), 0) for n in pending)
        reason = self.cancel.check()
        if reason is not None:
            raise DeliveryCancelled(reason)
        while pending:
            if self.gone:
                return
            pending = self._own_echo(cursor, outcomes, journaled, pending)
            if not pending:
                return
            if self.journal_only or self.ledger_off:
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
            text, count = self._text(channel, group, limit, unit, False)
            if text is None:
                step[str(head["id"])] = index + 1
                if index + 1 >= len(self.channels):
                    self._ledger_step(self.ledger.record, [str(head["id"])], OUTCOME_UNFIT, channel_id)
                    self._close(cursor, outcomes, journaled, head, OUTCOME_UNFIT, channel_id, None)
                    pending.pop(0)
                continue
            batch = group[:count]
            # The attempt begin publishes is owned from the moment begin hands
            # it over (owner) to its release, on every path - a stop signal
            # right after begin returns included: one that ends before the
            # text was handed over and before its end was written is
            # withdrawn (P-11).
            owner = []
            try:
                try:
                    decision = self.ledger.begin([{"id": str(n["id"]), "session": n.get("actorSessionId")}
                                                  for n in batch], channel_id, owner=owner)
                except LedgerUnavailable:
                    self._ledger_down()
                    continue
                attempt = decision.attempt
                done = set()
                for eid, status in decision.closed.items():
                    self._close(cursor, outcomes, journaled, byid[eid], status, channel_id, None)
                    done.add(eid)
                for eid, rec in decision.own.items():
                    self._close(cursor, outcomes, journaled, byid[eid], rec.get("outcome"), rec.get("channel"),
                                None)
                    done.add(eid)
                pending = [n for n in pending if str(n["id"]) not in done]
                if attempt is None:
                    if decision.wait_until is not None:
                        self._wait(decision.wait_until)
                    elif decision.pending:
                        self._wait(self.clock.now() + WAIT_STEP_SEC)
                    continue
                try:
                    self._deliver(cursor, outcomes, journaled, attempt, channel_id, channel,
                                  [byid[e] for e in decision.published], step)
                except LedgerUnavailable:
                    # The ledger refused a step. Before the hand-over
                    # nothing reached the channel: the attempt is withdrawn.
                    # After it the text may have: the record stays
                    # "delivering" and its recovery gives "unknown" (INV-9).
                    if not attempt.handed:
                        self._ledger_step(attempt.abandon)
                    self._ledger_down()
                pending = [n for n in pending if str(n["id"]) not in outcomes]
            except BaseException:
                held = owner[0] if owner else None
                if held is not None and not held.settled and not held.handed:
                    try:
                        held.abandon()
                    except LedgerUnavailable:
                        pass
                raise
            finally:
                if owner:
                    owner[0].release()

    def _own_echo(self, cursor, outcomes, journaled, pending):
        """Give the events of the excluded session among those not yet handed
        to a channel the outcome "own echo" (US-13): the value in force now,
        which a poll during a wait may have changed. Returns the rest."""
        if self.exclusion is None:
            return pending
        own = [n for n in pending if self.exclusion.own(n)]
        if not own:
            return pending
        if not self.ledger_off:
            self._ledger_step(self.ledger.record, [str(n["id"]) for n in own], OUTCOME_OWN_ECHO, None)
        self._close_batch(cursor, outcomes, journaled, own, OUTCOME_OWN_ECHO, None, None)
        return [n for n in pending if str(n["id"]) not in outcomes]

    def _text(self, channel, batch, limit, unit, minimal):
        """(text, count) of a batch by the one formatter the channel takes:
        the reservation of an attempt and its text are sized by the same
        one. A batch program of Windows gets no untrusted text (US-11): the
        id-only form, or no text at all (a refusal before send)."""
        if channel_takes_ids_only(channel, self.doc.get("programs"), self.env):
            text, count, _ = ids_only_batch(batch, limit, unit)
        else:
            text, count, _ = plain_batch(batch, self.exclude_file, limit, unit, channel.get("textNotes"), minimal)
        return text, count

    def _deliver(self, cursor, outcomes, journaled, attempt, channel_id, channel, batch, step):
        now = self.clock.now()
        deadline = (attempt.first_call if attempt.first_call is not None else now) + self.idle_window
        if attempt.calls >= MAX_CALLS or (attempt.calls and now >= deadline):
            self._ledger_step(attempt.finish, OUTCOME_REFUSED)
            attempt.release()
            self._exhausted(channel_id, batch, step)
            return
        if attempt.calls:
            due = (attempt.last_call or now) + RETRY_DELAYS_SEC[attempt.calls - 1]
            if due > now:
                self._ledger_step(attempt.abandon)
                attempt.release()
                if due >= deadline:
                    self._exhausted(channel_id, batch, step)
                    return
                self._wait(due)
                return
        # The attempt's time is the idle wait's own plus the call's own, so
        # a long wait leaves the call its whole timeout (or no call).
        timeout = min(_attempt_seconds(channel, self.idle_window), deadline - now)
        if len(batch) < len(attempt.events):
            batch = [n for n in batch if str(n["id"]) in attempt.events]
        limit, unit = _line_limit(channel)
        # A channel that refused an event as too long gets it, and the
        # events that join it in a batch, in the minimal form.
        minimal = any((str(n["id"]), channel_id) in self._shorten for n in batch)
        text, count = self._text(channel, batch, limit, unit, minimal)
        if text is not None and count < len(batch):
            # The text must carry every event the attempt holds, or a
            # delivered outcome would be written for events it never
            # carried (INV-1): nothing is handed over.
            text = None
        guard = Guard(self, channel, attempt, timeout, batch)
        driver = self.drivers.get(channel_id) or ArgvDriver(channel, self.executor)
        try:
            if text is None:
                raise RefusedBeforeSend("no text of this channel carries the batch")
            outcome, sign = driver.send(text, guard)
        except PollStale:
            self._ledger_step(attempt.abandon)
            self._wait(self.clock.now() + WAIT_STEP_SEC)
            return
        except EchoNow:
            # The page loop gives those events "own echo" and sends the rest.
            self._ledger_step(attempt.abandon)
            return
        except RefusedBeforeSend as exc:
            outcome, sign = OUTCOME_REFUSED, {_NOT_SENT: exc.reason}
        except AddresseeChanged:
            outcome, sign = "addressee-changed", {_NOT_SENT: "addressee changed"}
        except AddresseeGone:
            outcome, sign = OUTCOME_ADDRESSEE_GONE, None
        except DeliveryCancelled:
            self._ledger_step(attempt.abandon)
            raise
        except AttemptLost:
            # Another attempt holds an event of the batch (addressees
            # merged): nothing was handed over; the events wait for it.
            self._ledger_step(attempt.abandon)
            return
        except (StopSignal, Exception) as exc:
            # A stop (or an error) while the channel may have the text:
            # unknown, never a second send (INV-9) - in the ledger, the
            # state and the journal before the stop goes on, so its
            # receipt never comes before this outcome.
            if not guard.handed:
                # Before the hand-over: nothing reached the channel.
                self._ledger_step(attempt.abandon)
                raise
            if isinstance(exc, (LedgerUnavailable, JournalUnavailable)):
                raise
            self._ledger_step(attempt.finish, OUTCOME_UNKNOWN)
            self._close_unsure(cursor, outcomes, journaled, batch, channel_id)
            raise
        changed = outcome == "addressee-changed"
        if not changed and outcome != OUTCOME_REFUSED and not self._same_process(channel):
            # The agent process changed while a one-step call ran: the
            # text may have reached another process ("unknown", never
            # sent again), later events go to the journal.
            changed = True
        if guard.changed:
            # Another addressee before the final step: the text was
            # written, not submitted.
            changed = True
        if changed:
            # Not sent; the event's outcome is "unknown" (the table of
            # outcomes), later events go to the journal.
            outcome = OUTCOME_UNKNOWN
        # The call was made: its outcome stands even when the ledger
        # refuses to take it.
        burst = self._ledger_step(attempt.finish, outcome)
        # The owner of the attempt (_page) releases it, after a withdrawal
        # when nothing was handed over.
        if burst:
            self.lifecycle({"event": "degraded", "cause": "single_session_burst", "issue": self.issue,
                            "actorSessionId": batch[0].get("actorSessionId")})
        if outcome == OUTCOME_REFUSED:
            # Kept pending: the next turn calls again when due, or switches.
            self._last_refusal = sign
            if any(row.get("shorten") and row.get("match") == sign for row in channel.get("outcomes") or []):
                # Too long for the channel: the next call carries the
                # minimal form; refused again, the budget goes on (INV-8).
                self._shorten.update((str(n["id"]), channel_id) for n in batch)
            return
        if guard.cancel_reason is not None:
            # A stop answer withheld the final step: it wins over the rest of
            # the page (US-12), once this batch has its outcome - as far as
            # the state and the journal take it: a refusal of either does
            # not turn the stop by lost access into another stop.
            try:
                self._close_batch(cursor, outcomes, journaled, batch, outcome, channel_id, sign,
                                  with_preview=False)
            except (OSError, PlatformRefusal, JournalUnavailable):
                pass
            raise DeliveryCancelled(guard.cancel_reason)
        self._close_batch(cursor, outcomes, journaled, batch, outcome, channel_id, sign,
                          with_preview=not guard.access_lost)
        if outcome == OUTCOME_ADDRESSEE_GONE or guard.gone:
            # The stop follows when the page loop returns (page()).
            self.gone = True
        elif changed:
            self.journal_only = True
            try:
                # The mark lets a new preflight of this token in while this
                # runner lives (takeover.py).
                mark_addressee_changed(self.token_file, self.doc.get("preflightId"))
            except (OSError, PlatformRefusal):
                pass
            self._degraded_once("addressee_changed",
                                action="The addressee of the monitor of issue %s is not the one its "
                                       "preflight saw: events go only to the journal. Run the preflight "
                                       "again, then the working command it prints, to take the monitor "
                                       "over." % self.issue)
        elif outcome == OUTCOME_UNKNOWN:
            self.lifecycle({"event": "degraded", "cause": "delivery_unknown", "issue": self.issue,
                            "channel": channel_id, "notificationIds": [n["id"] for n in batch]})
        elif outcome in DELIVERED_OUTCOMES and channel_id in self.failing:
            self.failing.discard(channel_id)
            self.lifecycle({"event": "recovered", "issue": self.issue, "channel": channel_id})

    def receipt(self, text, short=None):
        """The receipt of a stop that keeps access (US-12): one text by the
        first channel of the preflight, after the addressee check, within
        the budget of INV-8 and the channel's limit of a text (short, the
        shortest form with its action, when text is over it; neither fits:
        no receipt). A failure leaves only the journal line. A monitor whose
        ledger is off, or that writes only the journal (another addressee),
        sends nothing outside: the journal line is the receipt."""
        if self.ledger_off or self.journal_only:
            return
        known = getattr(self.cancel, "known", None)
        if known is not None and known() is not None:
            # Access is known to be lost: a stop for another reason (a
            # signal, an error) after it sends nothing outside.
            return
        channel_id = self.channels[0]
        channel = self.registry.channel(channel_id) or {}
        notes = [clean(n) for n in channel.get("textNotes") or []]
        if notes:
            # The notes of the channel go with every text it delivers.
            text = " ".join([text] + notes)
            short = " ".join([short] + notes) if short is not None else None
        limit, unit = _line_limit(channel)
        if limit is not None and measure(text, unit) > limit:
            if short is None or measure(short, unit) > limit:
                return
            text = short
        driver = self.drivers.get(channel_id) or ArgvDriver(channel, self.executor)
        start = self.clock.now()
        for calls in range(MAX_CALLS):
            if calls:
                due = start + sum(RETRY_DELAYS_SEC[:calls])
                if due - start >= self.idle_window:
                    return
                while self.clock.now() < due:
                    self.clock.sleep(min(WAIT_STEP_SEC, due - self.clock.now()))
            # Every call within what is left of the idle window (INV-8).
            left = start + self.idle_window - self.clock.now()
            if left <= 0:
                return
            guard = _ReceiptGuard(self, channel, min(_attempt_seconds(channel, PROBE_TIMEOUT_SEC), left))
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

    def _close_unsure(self, cursor, outcomes, journaled, batch, channel_id):
        """The batch of a call a stop interrupted: unknown, as far as the
        state and the journal take it (what they refuse, the next start
        recovers from the ledger)."""
        try:
            self._close_batch(cursor, outcomes, journaled, batch, OUTCOME_UNKNOWN, channel_id, {"interrupted": True})
        except (JournalUnavailable, OSError, PlatformRefusal):
            pass

    def _close_batch(self, cursor, outcomes, journaled, batch, status, channel, sign, with_preview=True):
        """The outcome of a batch the ledger took in one record: every event
        into the state in one save, then the journal line of each. A journal
        that refuses a line leaves the state saying what the ledger says, so
        a later stop does not count those events as waiting."""
        for n in batch:
            outcomes[str(n["id"])] = status
            if not with_preview:
                self._redacted.add(str(n["id"]))
        save_page_outcomes(self.token_file, cursor, outcomes, journaled, self._redacted)
        for n in batch:
            self._journal(cursor, outcomes, journaled, n, status, channel, sign, with_preview)

    def _close(self, cursor, outcomes, journaled, n, status, channel, sign, with_preview=True):
        """The outcome of one event: into the state, then the journal."""
        eid = str(n["id"])
        outcomes[eid] = status
        # A closed event is never sent again: its minimal-form marks go.
        self._shorten = set(k for k in self._shorten if k[0] != eid)
        if not with_preview:
            self._redacted.add(eid)
        save_page_outcomes(self.token_file, cursor, outcomes, journaled, self._redacted)
        self._journal(cursor, outcomes, journaled, n, status, channel, sign, with_preview)

    def _journal(self, cursor, outcomes, journaled, n, status, channel, sign, with_preview=True):
        self.journal.delivery(n, status, method=self.doc.get("method"), channel=channel, answer=sign,
                              with_preview=with_preview)
        journaled.add(str(n["id"]))
        save_page_outcomes(self.token_file, cursor, outcomes, journaled, self._redacted)

    def end_open_page(self, reason):
        """A stop by lost access (401, 403) while a page is undone because
        the journal refused: its waiting events get "access lost", and the
        outcomes the journal does not hold yet get their lines - all without
        the preview - in the journal or, when it takes no write, in the
        deferred tail with the stop summary."""
        if self._open is None:
            return
        cursor, notifications = self._open
        self._open = None
        events = [n for n in notifications if isinstance(n, dict) and n.get("id") is not None]
        outcomes, journaled = load_page_outcomes(self.token_file, cursor)
        self._redacted = load_page_redacted(self.token_file, cursor)
        known = [(n, outcomes[str(n["id"])]) for n in events
                 if str(n["id"]) in outcomes and str(n["id"]) not in journaled]
        waiting = [n for n in events if str(n["id"]) not in outcomes]
        close_waiting(self, cursor, outcomes, journaled, waiting, OUTCOME_ACCESS_LOST, reason, known=known)


def _line_limit(channel):
    line = (channel.get("limits") or {}).get("line") or {}
    if line.get("max"):
        return line["max"], line.get("unit") or UNIT_CHARS
    return None, UNIT_CHARS
