# --- finalize: the access boundary and the end of a client-mode monitor ------
# The access boundary (US-12). While a detached monitor waits to deliver (a
# retry, the rate window, an attempt of another monitor) it keeps polling
# with the cursor of its last fixed outcome, so events already waiting are
# not queued again and the token stays renewed. A send is allowed only when
# the last successful poll is at most two poll intervals old and no answer
# since said stop. 401 (expired, revoked, monitor_stop) and 403 (visibility
# lost) win over waiting events: events not yet handed to a channel get
# "access lost" (for the monitor) without the preview, in the journal or,
# when the journal takes no write, in the deferred tail with the stop
# summary; an attempt already begun keeps the outcome its driver fixed
# (deliver.py). The addressee that has ended (addressee_gone) gives the
# waiting events "addressee gone" (for the addressee) and stops the monitor.
#
# The end (P-12), one sequence for every stop of a runner that holds the
# one-runner lock in client mode:
#   1. the outcome of the current attempt is fixed (by its driver, before
#      the stop reaches here) and the waiting events have theirs;
#   2. a stop that keeps access (root_closed, stopped, internal_error) is
#      reported to the agent by the same channel, one receipt within the
#      budget of INV-8, after the addressee check; a failure of the receipt
#      leaves only the journal line;
#   3. the stop line goes to the journal;
#   4. the files of the token directory get their fate (the stop table):
#      with the token go the state, the exclusion file, the starter, the
#      preflight result, the state lock and message files, then the
#      directory when it is empty; a kept token keeps them all;
#   5. the one-runner lock is released last (the caller): after it the
#      process touches no file of the monitor, and a competing start that
#      takes the lock sees the final fate.

import os
import uuid

# The files of the token directory that share the token's fate (P-5).
TOKEN_DIR_FILES = (STATE_FILE_NAME, EXCLUDE_FILE_NAME, CLIENT_STARTER_FILE, PREFLIGHT_FILE_NAME,
                   TAKEOVER_FILE_NAME, STATE_LOCK_NAME)
# Named points of the end sequence. A test may stop the end at one (the
# point "token_dir_removed" is the state "directory gone, lock still held");
# a running monitor registers nothing here.
FINALIZE_POINTS = {}
# Descriptors of files of the token directory the runner holds open; the end
# closes them before any file is removed (P-12). The runner registers here
# whatever it keeps open there.
TOKEN_DIR_FDS = set()
# Stops after which the agent still has access: reported by the channel.
RECEIPT_STOPS = ("root_closed", "stopped", "internal_error")
# What the poll answers mean for a waiting delivery.
_ACCESS_STOPS = {401: "token_expired", 403: "visibility_lost"}


def poll_page_ok(payload):
    """Whether a 200 answer of the poll is a page (what the working loop
    takes): a dict whose notifications is a list. A successful poll for the
    access boundary only then; any other 200 is a failed poll."""
    return isinstance(payload, dict) and isinstance(payload.get("notifications"), list)


class PollCancel(object):
    """The cancel signal of the working runner: during a wait it polls with
    the cursor of the last fixed outcome once a poll interval has passed,
    and remembers an answer that says stop. A poll that failed (no page, a
    5xx, a 429) is repeated no sooner than a poll interval later, or than
    its Retry-After."""

    def __init__(self, executor, base, token, exclude_session, clock, interval=None):
        self.executor = executor
        self.base = base
        self.token = token
        self.exclude_session = exclude_session
        self.clock = clock
        self.interval = POLL_INTERVAL_SEC if interval is None else interval
        self.cursor = None
        self.last_ok = clock.now()
        # (reason, status) once a poll answered 401 or 403.
        self.stop = None
        # The earliest time of the next poll after a failed one.
        self.next_poll = None

    def note_ok(self):
        """A successful poll of the working loop."""
        self.last_ok = self.clock.now()
        self.next_poll = None

    def check(self):
        now = self.clock.now()
        if (self.stop is None and now - self.last_ok >= self.interval
                and (self.next_poll is None or now >= self.next_poll)):
            status, payload, retry_after = poll_once(self.executor, self.base, self.token, self.cursor,
                                                     self.exclude_session)
            if status == 200 and poll_page_ok(payload):
                # The events of this answer are not queued: the page being
                # delivered holds them, the cursor does not move.
                self.note_ok()
            elif status in _ACCESS_STOPS:
                self.stop = (_ACCESS_STOPS[status], status)
            else:
                wait = self.interval
                if status == 429:
                    wait = max(wait, retry_after_seconds(retry_after, self.interval))
                self.next_poll = now + wait
        return self.stop[0] if self.stop is not None else None

    def may_send(self):
        """A send is allowed: no stop answer, and the last successful poll at
        most two intervals old."""
        return self.stop is None and self.clock.now() - self.last_ok <= 2 * self.interval


def access_stop(reason):
    """The Stop of a cancel reason."""
    status = dict((v, k) for k, v in _ACCESS_STOPS.items()).get(reason)
    return Stop(reason, status=status, delete_token=True)


def close_waiting(sink, cursor, outcomes, journaled, waiting, outcome, reason, known=()):
    """Give every waiting event (not handed to a channel) its outcome by the
    stop: into the ledger and the state, then the journal without the
    preview; known are (event, outcome) whose outcome is fixed but not in
    the journal yet: their lines go the same way, without the preview. When
    the journal takes no write, the lines and a stop summary go to the
    deferred tail (P-12, OST-2-6) before any file of the token is
    touched."""
    redacted = getattr(sink, "_redacted", set())
    items = [(n, outcome, True) for n in waiting] + [(n, status, False) for n, status in known]
    deferred = []
    for n, status, new in items:
        eid = str(n["id"])
        redacted.add(eid)
        if new:
            try:
                sink.ledger.record([eid], status)
            except LedgerUnavailable:
                pass
            outcomes[eid] = status
        try:
            save_page_outcomes(sink.token_file, cursor, outcomes, journaled, redacted)
        except (OSError, PlatformRefusal):
            pass
        record = {"event": "delivery", "status": status, "method": sink.doc.get("method"), "channel": None,
                  "addressee": None, "channelAnswer": None, "reason": reason}
        record.update(notification_fields(n, with_preview=False))
        if not deferred:
            try:
                sink.journal.delivery(n, status, method=sink.doc.get("method"), reason=reason,
                                      with_preview=False)
                journaled.add(eid)
                continue
            except JournalUnavailable:
                pass
        deferred.append(record)
    if deferred:
        try:
            lines = [sink.journal.render(r, line_id=uuid.uuid4().hex) for r in deferred]
            lines.append(sink.journal.render({"event": "summary", "summary": "stopped_journal_unavailable",
                                              "reason": reason, "events": len(deferred)},
                                             line_id=uuid.uuid4().hex))
            defer_lines(sink.journal.path, lines, sink.env)
        except (LedgerUnavailable, JournalUnavailable):
            # The ledger directory took nothing (or a line could not be
            # rendered): neither the lines nor the summary exist (a
            # documented boundary).
            pass


def _point(name):
    hook = FINALIZE_POINTS.get(name)
    if hook is not None:
        hook()


def close_token_dir_fds():
    """Close every descriptor of the token directory the runner holds."""
    while TOKEN_DIR_FDS:
        fd = TOKEN_DIR_FDS.pop()
        try:
            os.close(fd)
        except OSError:
            pass


def apply_file_fate(token_file, delete):
    """Step 4 of P-12: the descriptors of the token directory are closed;
    with the token go the files that share its fate and the leftover
    message files, then the token directory when it is empty."""
    close_token_dir_fds()
    _point("descriptors_closed")
    if not delete or not token_file:
        return
    # The parts this process held go with the state.
    _STATE_PARTS.pop(state_file_path(token_file), None)
    directory = os.path.dirname(os.path.abspath(token_file))
    for path in [token_file] + [os.path.join(directory, name) for name in TOKEN_DIR_FILES]:
        try:
            os.remove(path)
        except OSError:
            pass
    sweep_message_files(token_file)
    _point("files_removed")
    try:
        os.rmdir(directory)
    except OSError:
        return
    _point("token_dir_removed")


def finalize_stored(token_file, issue, reason, env, secrets=()):
    """Step 1 of P-12 for a stop by lost access that has no live page (the
    client loader got 401 or 403 for the runner): the events the state keeps
    get their journal lines without the preview - the rest of a page an
    exit-on-event cycle kept gets "access lost", an outcome fixed before the
    stop but not in the journal yet keeps its own - in the journal or, when
    it takes no write, in the deferred tail with the stop summary. The
    caller holds the one-runner lock."""
    cursor = load_cursor(token_file)
    items = []
    rest = load_remainder(token_file, cursor)
    if rest is not None:
        items.extend((n, OUTCOME_ACCESS_LOST) for n in rest[0])
    outcomes, journaled = load_page_outcomes(token_file, cursor)
    items.extend(({"id": eid}, status) for eid, status in sorted(outcomes.items()) if eid not in journaled)
    if not items:
        return
    method = (load_preflight(token_file) or {}).get("method")
    close_stored(Journal(token_file, issue, secrets=list(secrets), env=env), method, items, reason, env)


def receipt_text(outcome, issue, preflight=None):
    """The plain text of the receipt of a stop that keeps access; in client
    mode its action names the working command of preflight `preflight`."""
    return "%s. monitor stopped: reason=%s issue=%s. %s" % (
        DELIVERY_LABEL, outcome.reason, issue, stop_action(outcome.reason, issue, outcome.detail, preflight))


def receipt_short_text(outcome, issue):
    """The receipt within a short limit of the channel: the label, the
    reason and the issue; the action is in the stop line of the journal."""
    return "%s. monitor stopped: reason=%s issue=%s." % (DELIVERY_LABEL, outcome.reason, issue)


def finish_client(outcome, issue, token_file, sink=None, write=None, preflight=None):
    """Steps 2-4 of P-12 for a runner that holds the one-runner lock: the
    receipt, the stop line in the journal, the fate of the files, the stop
    line on stdout (through write when given; its action names the working
    command of preflight `preflight`). Returns the exit code; the caller
    releases the lock after it."""
    event, code = STOPS[outcome.reason]
    line = {"event": event, "reason": outcome.reason}
    if outcome.status is not None:
        line["status"] = outcome.status
    if outcome.reason == "root_closed" and isinstance(outcome.detail, dict):
        # The summary of the close (rootclosed.py) in the journal line.
        line["summary"] = dict(outcome.detail)
    if sink is not None:
        if outcome.reason in RECEIPT_STOPS and hasattr(sink, "receipt"):
            sink.receipt(receipt_text(outcome, issue, preflight), receipt_short_text(outcome, issue))
        sink.lifecycle(line)
    apply_file_fate(token_file, outcome.delete_token)
    return finish(Stop(outcome.reason, status=outcome.status, detail=outcome.detail, preflight=preflight),
                  issue, token_file, write=write)
