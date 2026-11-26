# --- reading on after rootClosed (US-12, INV-11) -------------------------------
# Without `client` a runner stops after the first page with rootClosed (US-1).
# A client-mode monitor brings what it received to outcomes and reads on:
#
#   * the flag is sticky: the page where rootClosed first comes starts the
#     close; new events after it and a reopen of the root do not move it;
#   * while the pages are full, the monitor reads the next ones from its
#     cursor, at most READ_ON_PAGES pages after the first one with rootClosed
#     for the whole monitor. The unit of this budget is a poll answer that
#     moved the cursor: an answer at the same cursor (a poll during a wait, a
#     repeat after a failure) and the rest of a page an exit-on-event cycle
#     kept spend nothing;
#   * the summary counts every event received from the page of the first
#     rootClosed on (that page whole): not delivered (refused), unknown,
#     input without a receipt, does not fit. Earlier events are not in it;
#   * the stop comes once the last page read is not full, or the budget is
#     spent (the summary then says reading on was limited and the journal
#     gets a summary line: later events have no line of their own).
#
# The flag, the pages spent, the page being read and the counts live in the
# monitor state, saved with the cursor that passes the page, so they survive
# a re-arm and a restart. A lost final answer is polled again from the saved
# cursor: its events come again, as they are by then, with rootClosed.

import uuid

# The pages read after the first page with rootClosed, at most.
READ_ON_PAGES = 20
# The page size a client-mode poll asks for: a page of this many events is
# full (more may follow it).
POLL_PAGE_LIMIT = 50
# The summary keys of the outcomes the summary counts.
SUMMARY_KEYS = ("undelivered", "unknown", "inputWithoutReceipt", "doesNotFit")


# The longest summary a stop line can carry: the room an exit-on-event
# batch keeps for the root_closed line (more events than any monitor reads
# after the close).
LONGEST_SUMMARY = dict([(k, 10 ** 6 - 1) for k in SUMMARY_KEYS] + [("readLimited", True)])


def _summary_key(status):
    return {OUTCOME_REFUSED: "undelivered", OUTCOME_UNKNOWN: "unknown",
            OUTCOME_INPUT_NO_RECEIPT: "inputWithoutReceipt", OUTCOME_UNFIT: "doesNotFit",
            STATUS_UNFIT: "doesNotFit"}.get(status)


class RootClosed(object):
    """The close of a client-mode monitor: the sticky flag, the budget and
    the summary, kept in the state."""

    def __init__(self, token_file):
        doc = load_root_closed(token_file)
        self.active = doc is not None
        doc = doc or {}
        pages = doc.get("pages")
        self.pages = pages if isinstance(pages, int) and not isinstance(pages, bool) and pages >= 0 else 0
        counts = doc.get("counts") if isinstance(doc.get("counts"), dict) else {}
        self.counts = dict((k, counts[k] if isinstance(counts.get(k), int) else 0) for k in SUMMARY_KEYS)
        # The page being read: its cursor and whether it was full.
        page = doc.get("page") if isinstance(doc.get("page"), dict) else {}
        self.page_cursor = page.get("cursor")
        self.page_full = page.get("full") is True
        self.limited = False

    def received(self, cursor, events, root_closed):
        """A page polled at cursor arrived (not the rest of a kept one)."""
        if root_closed and not self.active:
            self.active = True
        self.page_cursor = cursor
        self.page_full = len(events) >= POLL_PAGE_LIMIT

    def tally(self, statuses):
        """Count the outcomes of events that got one now."""
        if not self.active:
            return
        for status in statuses:
            key = _summary_key(status)
            if key is not None:
                self.counts[key] += 1

    def page_done(self, cursor, next_cursor):
        """Every event of the page polled at cursor has its outcome; True when
        the monitor stops (root_closed) instead of reading on."""
        if not self.active:
            return False
        if next_cursor != cursor:
            self.pages += 1
        full = self.page_full and self.page_cursor == cursor
        if not full:
            return True
        if self.pages >= 1 + READ_ON_PAGES:
            self.limited = True
            return True
        return False

    def record(self):
        """What the state keeps (None before the close)."""
        if not self.active:
            return None
        return {"pages": self.pages, "counts": dict(self.counts),
                "page": {"cursor": self.page_cursor, "full": self.page_full}}

    def summary(self):
        """The summary of the stop line (Stop.detail of root_closed)."""
        out = dict(self.counts)
        out["readLimited"] = self.limited
        return out


def summary_text(summary):
    """The summary in the action of the root_closed line."""
    text = ("closed; not delivered %d (refused), unknown %d, input without a receipt %d, "
            "does not fit %d" % (summary.get("undelivered", 0), summary.get("unknown", 0),
                                 summary.get("inputWithoutReceipt", 0), summary.get("doesNotFit", 0)))
    if summary.get("readLimited"):
        text += "; reading on was limited (later notifications are not delivered, see the issue with skein_show)"
    return text


def note_read_limited(journal, root):
    """The journal summary line of a stop at the page budget."""
    if root.limited:
        try:
            journal.summary_read_limited(root.pages)
        except JournalUnavailable:
            pass


def close_stored(journal, method, events, reason, env):
    """A stop by lost access with events of a kept page: each gets "access
    lost" without the preview, in the journal or, when the journal takes no
    write, in the deferred tail with a stop summary (US-12)."""
    deferred = []
    for n in events:
        if not isinstance(n, dict) or n.get("id") is None:
            continue
        if not deferred:
            try:
                journal.delivery(n, OUTCOME_ACCESS_LOST, method=method, reason=reason, with_preview=False)
                continue
            except JournalUnavailable:
                pass
        record = {"event": "delivery", "status": OUTCOME_ACCESS_LOST, "method": method, "channel": None,
                  "addressee": None, "channelAnswer": None, "reason": reason}
        record.update(notification_fields(n, with_preview=False))
        deferred.append(record)
    if deferred and env is not None:
        try:
            lines = [journal.render(r, line_id=uuid.uuid4().hex) for r in deferred]
            lines.append(journal.render({"event": "summary", "summary": "stopped_journal_unavailable",
                                         "reason": reason, "events": len(deferred)},
                                        line_id=uuid.uuid4().hex))
            defer_lines(journal.path, lines, env)
        except (LedgerUnavailable, JournalUnavailable):
            pass
