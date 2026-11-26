# --- the ledger of deliveries (skein-monitor/ledger/, P-5, P-11, R-6) --------
# One file per Skein server, shared by every monitor of the user, is the
# authority on "was this event delivered to this addressee" (INV-1). It is
# changed only under ledger/ledger.lock, held for a short read-change-write,
# never across a call of a channel.
#
# The addressee ("Addressee and check", item 5): two monitors serve one
# addressee when they share the agent process or one address with all its
# qualifiers. An entry keeps the union of the addresses of its monitors
# (inherited addresses never reach it: the identity leaves them out); a
# monitor whose identity matches several entries merges them. A monitor
# without a process or an address (stdout methods) is its own addressee,
# keyed by the monitor.
#
# The status of one event for an addressee: intent -> delivering -> outcome.
# An attempt of delivery (P-11):
#   1. take a non-blocking lock of a new file ledger/attempts/<key>;
#   2. under ledger.lock resolve the addressee again, read the event's record
#      and the addressee's rate window; publish "delivering" with the key and
#      the owner only when the event has no outcome for the addressee, no
#      other unfinished attempt, and the window is free; otherwise the loser
#      releases its attempt and follows the winner;
#   3. call the channel without ledger.lock (every call is counted first);
#   4. classify;
#   5. under ledger.lock write the outcome;
#   6. only then release the attempt lock and remove its file.
# A "delivering" record is never visible without its attempt lock held. A
# monitor that finds one takes that lock without waiting: busy, the owner is
# alive and the event waits for its outcome; taken, it reads the record
# again under ledger.lock and either takes the outcome written meanwhile or,
# still "delivering" with that key, turns it into "unknown" (INV-9) — once.
#
# Outcomes close an event for the addressee (queued, input written, input
# without receipt, turn started, unknown, addressee gone: no monitor of the
# addressee delivers it again) or for the monitor that wrote them (refused
# before send, does not fit, access lost, own echo: marked with the monitor,
# other monitors deliver with their channel). A monitor's own outcome keeps
# the number of channel calls made and the time of the last one, so a
# restart continues the budget of INV-8 from the ledger, not from the state.
#
# Rate: at most one delivery per addressee per 10 s, 60 s while a series of
# one session lasts (US-13), shared by all monitors of the addressee; the
# monitor that publishes takes the window for its batch, the others wait for
# the next one. A record lives at least 24 h; an addressee holds at most
# 10 000 records younger than that: a full ledger, or one that cannot be
# written, is LedgerUnavailable — no external delivery (state_unavailable).
#
# Deferred tails (P-12, OST-2-6): journal lines a stopping monitor could not
# write go to ledger/tails/, one file per journal, each line with its own
# lineId. Any monitor of the user appends them, under ledger.lock, to the
# journal they belong to once it takes a write: lines whose lineId the
# journal holds are skipped, a partial last line is cut, and the tail file
# is removed after the append. A ledger directory that does not take the
# tail keeps neither the lines nor the stop summary that is part of them.

import hashlib
import json
import os
import time

try:
    import fcntl as _ledger_fcntl
except ImportError:  # not POSIX
    _ledger_fcntl = None

LEDGER_VERSION = 1
LEDGER_LOCK_NAME = "ledger.lock"
ATTEMPTS_DIR_NAME = "attempts"
TAILS_DIR_NAME = "tails"

# Outcomes (the registry's names) and the runner's own ones.
OUTCOME_REFUSED = "refused-before-send"
OUTCOME_QUEUED = "queued"
OUTCOME_INPUT_WRITTEN = "input-written"
OUTCOME_INPUT_NO_RECEIPT = "input-without-receipt"
OUTCOME_TURN_STARTED = "turn-started"
OUTCOME_UNKNOWN = "unknown"
OUTCOME_ADDRESSEE_GONE = "addressee-gone"
OUTCOME_UNFIT = "does-not-fit"
OUTCOME_ACCESS_LOST = "access-lost"
OUTCOME_OWN_ECHO = "own-echo"
# Outcomes that close an event for every monitor of the addressee.
ADDRESSEE_OUTCOMES = frozenset((OUTCOME_QUEUED, OUTCOME_INPUT_WRITTEN, OUTCOME_INPUT_NO_RECEIPT,
                                OUTCOME_TURN_STARTED, OUTCOME_UNKNOWN, OUTCOME_ADDRESSEE_GONE))
# Outcomes that close an event only for the monitor that wrote them.
MONITOR_OUTCOMES = frozenset((OUTCOME_REFUSED, OUTCOME_UNFIT, OUTCOME_ACCESS_LOST, OUTCOME_OWN_ECHO))

# The rate window of an addressee, and its length during a series of one
# session (US-13).
WINDOW_SEC = 10
WINDOW_SERIES_SEC = 60
# A series: SERIES_COUNT deliveries in a row, all of one session, within
# SERIES_SPAN_SEC; it ends with a delivery of another session or after
# SERIES_SPAN_SEC without a delivery of this one.
SERIES_COUNT = 30
SERIES_SPAN_SEC = 600
# Records live at least this long; an addressee holds at most LEDGER_MAX
# records younger than that.
RETAIN_SEC = 24 * 3600
LEDGER_MAX = 10000


class LedgerUnavailable(Exception):
    """The ledger cannot be read or written, or is full: no external
    delivery to this addressee (state_unavailable)."""

    def __init__(self, reason):
        Exception.__init__(self, "ledger unavailable (%s)" % reason)
        self.reason = reason


class AttemptLost(Exception):
    """A published attempt no longer holds the reservation of an event it
    carries (addressees merged and another attempt kept it): it hands
    nothing over."""


class LedgerFull(LedgerUnavailable):
    def __init__(self):
        LedgerUnavailable.__init__(self, "full")


def _private_dir(path):
    try:
        current_platform().ensure_private_dir(path)
    except PlatformRefusal as exc:
        raise LedgerUnavailable(exc.reason)


def _ledger_root(env):
    try:
        root = ledger_dir(env)
        _private_dir(os.path.dirname(root))
    except PlatformRefusal as exc:
        raise LedgerUnavailable(exc.reason)
    _private_dir(root)
    return root


class _FileLock(object):
    """An exclusive flock of a file in a private directory, held while
    open. blocking=False raises LockBusy when another process holds it."""

    def __init__(self, path, blocking=True, create=True):
        if _ledger_fcntl is None:
            raise LedgerUnavailable("unsupported")
        flags = os.O_RDWR | getattr(os, "O_NOFOLLOW", 0) | (os.O_CREAT if create else 0)
        try:
            self.fd = os.open(path, flags, 0o600)
        except FileNotFoundError:
            raise
        except OSError as exc:
            raise LedgerUnavailable(exc.strerror or "open_failed")
        try:
            _ledger_fcntl.flock(self.fd, _ledger_fcntl.LOCK_EX | (0 if blocking else _ledger_fcntl.LOCK_NB))
        except BlockingIOError:
            os.close(self.fd)
            raise LockBusy(path)
        except OSError as exc:
            os.close(self.fd)
            raise LedgerUnavailable(exc.strerror or "lock_failed")
        self.path = path

    def release(self):
        fd, self.fd = self.fd, None
        if fd is not None:
            os.close(fd)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.release()
        return False


def _server_key(server):
    return hashlib.sha256(server.rstrip("/").encode("utf-8", "surrogateescape")).hexdigest()[:32]


def _address_keys(identity, monitor):
    """The address keys of an identity: [channel, value, [[q, v], ...]]; a
    monitor without a process or an address is keyed by itself."""
    keys = []
    if identity is not None:
        for a in identity.addresses:
            keys.append([a.channel, a.value, [list(q) for q in a.qualifiers]])
    if not keys and (identity is None or identity.process is None):
        keys.append(["monitor", monitor, []])
    return keys


def _process_of(identity):
    if identity is None or identity.process is None:
        return None
    return list(identity.process)


class Decision(object):
    """What step 2 of an attempt decided for each event of a batch.

    attempt    the published Attempt (None when nothing was published);
    published  ids of the events the attempt carries;
    closed     {id: outcome} closed for the addressee (or recovered here);
    recovered  ids this monitor turned from "delivering" into "unknown"
               (journal them);
    pending    ids another live attempt is delivering (wait for its outcome);
    own        {id: record} this monitor's final outcome for the monitor;
    wait_until when the rate window is taken, the time it frees (the
               events wait for it), else None."""

    def __init__(self):
        self.attempt = None
        self.published = []
        self.closed = {}
        self.recovered = []
        self.pending = []
        self.own = {}
        self.wait_until = None


class Attempt(object):
    """A published attempt: its key, the events it carries, the channel, and
    the calls already made for these events on this channel (prior budget).
    note_call before every call of the channel, finish with the outcome,
    release last."""

    def __init__(self, ledger, key, lock, events, channel, calls, last_call, first_call):
        self.ledger = ledger
        self.key = key
        self._lock = lock
        self.events = events
        self.channel = channel
        self.calls = calls
        self.last_call = last_call
        # The time of the first call of this delivery on this channel: the
        # budget of INV-8 runs from it.
        self.first_call = first_call

    def note_call(self):
        """Count a call of the channel in the ledger before it is made.
        Raises AttemptLost, counting nothing, when the attempt no longer
        holds the reservation of every event it carries."""
        now = self.ledger.clock()

        def change(entry):
            recs = [entry["events"].get(eid) for eid in self.events]
            if not all(rec and (rec.get("delivering") or {}).get("attempt") == self.key for rec in recs):
                return True
            for rec in recs:
                rec["delivering"]["calls"] = self.calls + 1
                rec["delivering"]["lastCall"] = now
                rec["delivering"]["firstCall"] = self.first_call if self.first_call is not None else now
            return False
        if self.ledger._change(change):
            raise AttemptLost()
        self.calls += 1
        self.last_call = now
        if self.first_call is None:
            self.first_call = now

    def finish(self, outcome):
        """Write the authoritative outcome of the attempt (step 5). Returns
        True when it starts a series of one session (degraded
        single_session_burst, once)."""
        now = self.ledger.clock()
        result = {}

        def change(entry):
            sessions = set()
            for eid in self.events:
                rec = entry["events"].get(eid)
                if rec is None or (rec.get("delivering") or {}).get("attempt") != self.key:
                    continue
                d = rec.pop("delivering")
                sessions.update(d.get("sessions") or [])
                rec["at"] = now
                if outcome in ADDRESSEE_OUTCOMES:
                    rec["addressee"] = {"outcome": outcome, "monitor": self.ledger.monitor,
                                        "channel": self.channel, "at": now}
                else:
                    monitors = rec.setdefault("monitors", {})
                    budgets = _budgets(monitors.get(self.ledger.monitor))
                    # The budget of INV-8 is the channel's: a refusal by one
                    # channel does not overwrite what another one spent.
                    budgets[self.channel] = {"calls": d.get("calls", 0), "lastCall": d.get("lastCall"),
                                             "firstCall": d.get("firstCall")}
                    monitors[self.ledger.monitor] = {
                        "outcome": outcome, "channel": self.channel, "calls": d.get("calls", 0),
                        "lastCall": d.get("lastCall"), "firstCall": d.get("firstCall"), "at": now,
                        "budgets": budgets}
            window = entry.get("window") or {}
            if outcome not in ADDRESSEE_OUTCOMES:
                # Nothing reached the addressee: the window it took is free
                # again.
                if window.get("attempt") == self.key:
                    entry["window"] = {"until": window.get("prev", 0)}
            elif outcome != OUTCOME_ADDRESSEE_GONE:
                result["burst"] = _note_series(entry, sessions, now)
        self.ledger._change(change)
        return result.get("burst", False)

    def abandon(self):
        """Withdraw a published attempt that made no call (a retry not due
        yet, a cancel before the send): its records go back to what they
        were, the window it took is free again."""
        def change(entry):
            for eid in self.events:
                rec = entry["events"].get(eid)
                if rec is not None and (rec.get("delivering") or {}).get("attempt") == self.key:
                    rec.pop("delivering")
                    if not rec.get("addressee") and not rec.get("monitors"):
                        del entry["events"][eid]
            window = entry.get("window") or {}
            if window.get("attempt") == self.key:
                entry["window"] = {"until": window.get("prev", 0)}
        self.ledger._change(change)

    def release(self):
        """Release the attempt lock and remove its file (step 6)."""
        lock, self._lock = self._lock, None
        if lock is not None:
            _remove_quietly(lock.path)
            lock.release()


def _budgets(mine):
    """{channel: {calls, lastCall, firstCall}} of a monitor's own record:
    what each channel spent of the budget of INV-8 on the event."""
    if not mine:
        return {}
    budgets = dict(mine.get("budgets") or {})
    if not budgets and mine.get("channel") is not None:
        budgets[mine["channel"]] = {"calls": mine.get("calls") or 0, "lastCall": mine.get("lastCall"),
                                    "firstCall": mine.get("firstCall")}
    return budgets


def _history(spent):
    """The comparable form of what a channel spent on an event."""
    spent = spent or {}
    return (spent.get("calls") or 0, spent.get("lastCall"), spent.get("firstCall"))


def _note_series(entry, sessions, now):
    """Count a delivery in the series of one session; True when it makes
    the series (30 in a row within 10 min)."""
    series = entry.get("series") or {}
    if len(sessions) != 1 or None in sessions:
        entry["series"] = {}
        return False
    session = next(iter(sessions))
    times = series.get("times") or []
    if series.get("session") != session or (times and now - times[-1] > SERIES_SPAN_SEC):
        times = []
    times = (times + [now])[-SERIES_COUNT:]
    was = bool(series.get("long"))
    is_long = was or (len(times) >= SERIES_COUNT and times[-1] - times[0] <= SERIES_SPAN_SEC)
    entry["series"] = {"session": session, "times": times, "long": is_long}
    return is_long and not was


def _window_len(entry, now):
    series = entry.get("series") or {}
    times = series.get("times") or []
    if series.get("long") and times and now - times[-1] <= SERIES_SPAN_SEC:
        return WINDOW_SERIES_SEC
    return WINDOW_SEC


def _remove_quietly(path):
    try:
        os.remove(path)
    except OSError:
        pass


class Ledger(object):
    """The ledger of one Skein server as seen by one monitor: monitor is its
    stable id (the same across restarts of the token), identity the
    addressee of the preflight."""

    def __init__(self, server, monitor, identity, env=None, clock=time.time):
        self.server = server
        self.monitor = monitor
        self.identity = identity
        self.env = env
        self.clock = clock

    # --- files ---------------------------------------------------------------

    def _root(self):
        return _ledger_root(self.env)

    def _path(self):
        return os.path.join(self._root(), _server_key(self.server) + ".json")

    def _load(self):
        try:
            raw = current_platform().read_own_file(self._path())
        except PlatformRefusal as exc:
            if exc.reason == "not_found":
                return {"version": LEDGER_VERSION, "addressees": []}
            raise LedgerUnavailable(exc.reason)
        except OSError as exc:
            raise LedgerUnavailable(exc.strerror or "read_failed")
        try:
            doc = json.loads(raw.decode("utf-8"))
        except ValueError:
            raise LedgerUnavailable("corrupt")
        if not isinstance(doc, dict) or doc.get("version") != LEDGER_VERSION:
            raise LedgerUnavailable("corrupt")
        return doc

    def _save(self, doc):
        data = json.dumps(doc, separators=(",", ":"), sort_keys=True).encode("utf-8")
        try:
            current_platform().atomic_write(self._path(), data, dir_check=_ledger_dir_ok)
        except PlatformRefusal as exc:
            raise LedgerUnavailable(exc.reason)
        except OSError as exc:
            raise LedgerUnavailable(exc.strerror or "write_failed")

    def _locked(self):
        return _FileLock(os.path.join(self._root(), LEDGER_LOCK_NAME))

    # --- the addressee -------------------------------------------------------

    def _resolve(self, doc):
        """The entry of this monitor's addressee in doc: the entries that
        share its process or an address are merged into one, which takes the
        monitor's process and addresses."""
        keys = _address_keys(self.identity, self.monitor)
        process = _process_of(self.identity)
        matched, rest = [], []
        for entry in doc["addressees"]:
            if (process is not None and process in entry.get("processes", [])) or \
                    any(k in entry.get("addresses", []) for k in keys):
                matched.append(entry)
            else:
                rest.append(entry)
        entry = _merge(matched) if matched else {"processes": [], "addresses": [], "events": {}, "window": {}}
        if process is not None and process not in entry["processes"]:
            entry["processes"].append(process)
        for k in keys:
            if k not in entry["addresses"]:
                entry["addresses"].append(k)
        doc["addressees"] = rest + [entry]
        return entry

    def _change(self, change):
        """Run change(entry) on this monitor's addressee under ledger.lock and
        save the ledger."""
        with self._locked():
            doc = self._load()
            entry = self._resolve(doc)
            out = change(entry)
            self._save(doc)
            return out

    # --- an attempt ----------------------------------------------------------

    def begin(self, events, channel):
        """Steps 1-2 of an attempt for a batch: events are [{"id", "session"}]
        in order. Returns a Decision. Raises LedgerUnavailable (full or not
        writable): no external delivery."""
        root = self._root()
        attempts = os.path.join(root, ATTEMPTS_DIR_NAME)
        _private_dir(attempts)
        key = os.urandom(16).hex()
        lock = _FileLock(os.path.join(attempts, key))
        decision = Decision()
        # The attempt lock goes back unless an Attempt that holds it is
        # returned: any exception before that (a save refused, a full
        # ledger) releases the lock and its file.
        handed = False
        try:
            with self._locked():
                doc = self._load()
                entry = self._resolve(doc)
                now = self.clock()
                _prune(entry, now)
                carry, spent_of = [], {}
                for ev in events:
                    eid = str(ev["id"])
                    rec = entry["events"].get(eid)
                    if rec is not None and rec.get("delivering"):
                        if not self._recover(rec, attempts, now):
                            decision.pending.append(eid)
                            continue
                        decision.recovered.append(eid)
                    if rec is not None and rec.get("addressee"):
                        decision.closed[eid] = rec["addressee"]["outcome"]
                        continue
                    mine = (rec or {}).get("monitors", {}).get(self.monitor)
                    if mine and mine.get("outcome") != OUTCOME_REFUSED:
                        decision.own[eid] = mine
                        continue
                    spent_of[eid] = _budgets(mine).get(channel)
                    carry.append(ev)
                # One attempt carries one budget: the events whose history
                # on this channel is the first one's; the others wait for an
                # attempt of their own.
                prior = spent_of.get(str(carry[0]["id"])) if carry else None
                carry = [ev for ev in carry if _history(spent_of.get(str(ev["id"]))) == _history(prior)]
                window = entry.get("window") or {}
                if carry and now < (window.get("until") or 0):
                    decision.wait_until = window["until"]
                    carry = []
                if carry:
                    live = [r for r in entry["events"].values() if r.get("at", 0) > now - RETAIN_SEC]
                    new = [ev for ev in carry if str(ev["id"]) not in entry["events"]]
                    if len(live) + len(new) > LEDGER_MAX:
                        raise LedgerFull()
                    calls = (prior or {}).get("calls") or 0
                    last = (prior or {}).get("lastCall")
                    first = (prior or {}).get("firstCall")
                    for ev in carry:
                        rec = entry["events"].setdefault(str(ev["id"]), {})
                        rec["at"] = now
                        rec["delivering"] = {"attempt": key, "monitor": self.monitor, "channel": channel,
                                             "calls": calls, "lastCall": last, "firstCall": first,
                                             "sessions": [ev.get("session")]}
                    entry["window"] = {"until": now + _window_len(entry, now), "attempt": key,
                                       "prev": window.get("until") or 0}
                    published = [str(ev["id"]) for ev in carry]
                self._save(doc)
                if carry:
                    decision.published = published
                    decision.attempt = Attempt(self, key, lock, published, channel, calls, last, first)
                    handed = True
        finally:
            if not handed:
                _remove_quietly(lock.path)
                lock.release()
        return decision

    def _recover(self, rec, attempts, now):
        """A "delivering" record found under ledger.lock: True when its owner
        is dead and the record now holds an outcome (taken or turned into
        "unknown" here), False while the owner holds its attempt."""
        d = rec["delivering"]
        path = os.path.join(attempts, d.get("attempt", ""))
        try:
            other = _FileLock(path, blocking=False, create=False)
        except LockBusy:
            return False
        except (FileNotFoundError, LedgerUnavailable):
            other = None
        try:
            # The owner is gone and the record still says "delivering" with
            # its key (we hold ledger.lock, so no one changes it now).
            rec.pop("delivering")
            rec["at"] = now
            rec["addressee"] = {"outcome": OUTCOME_UNKNOWN, "monitor": self.monitor,
                                "channel": d.get("channel"), "at": now, "recovered": True}
        finally:
            if other is not None:
                _remove_quietly(path)
                other.release()
        return True

    def record(self, events, outcome, channel=None):
        """Write an outcome no channel call produced (does not fit, own echo,
        access lost before any send) for events of this monitor."""
        now = self.clock()

        def change(entry):
            for eid in events:
                rec = entry["events"].setdefault(str(eid), {})
                rec["at"] = now
                if rec.get("delivering") or rec.get("addressee"):
                    continue
                if outcome in ADDRESSEE_OUTCOMES:
                    rec["addressee"] = {"outcome": outcome, "monitor": self.monitor, "channel": channel, "at": now}
                else:
                    rec.setdefault("monitors", {})[self.monitor] = {
                        "outcome": outcome, "channel": channel, "calls": 0, "lastCall": None, "at": now}
        self._change(change)

    def outcome_of(self, event_id):
        """(outcome, record) of an event for this monitor as the ledger holds
        it: the outcome for the addressee, else this monitor's own; (None,
        None) when there is neither."""
        with self._locked():
            doc = self._load()
            rec = None
            keys = _address_keys(self.identity, self.monitor)
            process = _process_of(self.identity)
            for entry in doc["addressees"]:
                if (process is not None and process in entry.get("processes", [])) or \
                        any(k in entry.get("addresses", []) for k in keys):
                    rec = entry["events"].get(str(event_id)) or rec
        if not rec:
            return None, None
        if rec.get("addressee"):
            return rec["addressee"]["outcome"], rec["addressee"]
        mine = (rec.get("monitors") or {}).get(self.monitor)
        if mine:
            return mine["outcome"], mine
        return None, None


def _merge(entries):
    """One entry of several that turned out to be one addressee."""
    out = {"processes": [], "addresses": [], "events": {}, "window": {}, "series": {}}
    for e in entries:
        for p in e.get("processes", []):
            if p not in out["processes"]:
                out["processes"].append(p)
        for a in e.get("addresses", []):
            if a not in out["addresses"]:
                out["addresses"].append(a)
        for eid, rec in (e.get("events") or {}).items():
            out["events"][eid] = _merge_record(out["events"].get(eid), rec)
        if (e.get("window") or {}).get("until", 0) > out["window"].get("until", 0):
            out["window"] = dict(e["window"])
        series = e.get("series") or {}
        if series and _series_rank(series) > _series_rank(out["series"]):
            out["series"] = dict(series)
    return out


def _series_rank(series):
    """Which series a merged addressee keeps: a long one first, then the one
    with the latest delivery, so the count of a series still in its run-up
    survives the merge."""
    times = series.get("times") or []
    return (bool(series.get("long")), times[-1] if times else float("-inf"))


def _merge_record(a, b):
    if a is None:
        return b
    if a.get("addressee") or b.get("addressee"):
        winner = a if a.get("addressee") else b
        if a.get("addressee") and b.get("addressee") and b["addressee"].get("at", 0) < a["addressee"].get("at", 0):
            winner = b
        out = dict(winner)
    else:
        out = dict(a if a.get("delivering") else b)
    monitors = dict(a.get("monitors") or {})
    monitors.update(b.get("monitors") or {})
    if monitors:
        out["monitors"] = monitors
    out["at"] = max(a.get("at", 0), b.get("at", 0))
    return out


def _prune(entry, now):
    """Drop records older than the retention that no attempt holds."""
    old = [eid for eid, rec in entry["events"].items()
           if rec.get("at", 0) <= now - RETAIN_SEC and not rec.get("delivering")]
    for eid in old:
        del entry["events"][eid]


def _ledger_dir_ok(dir_st):
    return dir_st.st_uid == os.getuid() and not dir_st.st_mode & 0o077


# --- deferred journal tails ---------------------------------------------------

def _tail_path(root, journal):
    key = hashlib.sha256(journal.encode("utf-8", "surrogateescape")).hexdigest()[:32]
    return os.path.join(root, TAILS_DIR_NAME, key + ".json")


def defer_lines(journal, lines, env=None):
    """Keep journal lines (rendered JSON texts, each carrying its lineId) in
    the deferred tail of the journal at path journal. Raises
    LedgerUnavailable when the ledger directory does not take them: then
    neither the lines nor a summary among them exist anywhere."""
    root = _ledger_root(env)
    _private_dir(os.path.join(root, TAILS_DIR_NAME))
    path = _tail_path(root, journal)
    with _FileLock(os.path.join(root, LEDGER_LOCK_NAME)):
        doc = _read_tail(path) or {"journal": journal, "lines": []}
        doc["lines"].extend(lines)
        data = json.dumps(doc, separators=(",", ":"), sort_keys=True).encode("utf-8")
        try:
            current_platform().atomic_write(path, data, dir_check=_ledger_dir_ok)
        except PlatformRefusal as exc:
            raise LedgerUnavailable(exc.reason)
        except OSError as exc:
            raise LedgerUnavailable(exc.strerror or "write_failed")


def _read_tail(path):
    try:
        raw = current_platform().read_own_file(path)
        doc = json.loads(raw.decode("utf-8"))
    except (PlatformRefusal, OSError, ValueError):
        return None
    if not isinstance(doc, dict) or not isinstance(doc.get("journal"), str) or \
            not isinstance(doc.get("lines"), list):
        return None
    return doc


def append_tails(env=None):
    """Append every deferred tail of the user to its journal (P-12): at the
    start of any monitor and periodically in the working loop. A tail whose
    journal does not take a write stays for the next try. Returns the number
    of tails appended and removed. Raises LedgerUnavailable when the ledger
    directory cannot be used."""
    root = _ledger_root(env)
    tails = os.path.join(root, TAILS_DIR_NAME)
    try:
        names = sorted(os.listdir(tails))
    except FileNotFoundError:
        return 0
    except OSError as exc:
        raise LedgerUnavailable(exc.strerror or "read_failed")
    done = 0
    with _FileLock(os.path.join(root, LEDGER_LOCK_NAME)):
        for name in names:
            path = os.path.join(tails, name)
            doc = _read_tail(path)
            if doc is None:
                continue
            try:
                _append_to_journal(doc["journal"], doc["lines"], env)
            except (JournalUnavailable, OSError):
                continue
            _remove_quietly(path)
            done += 1
        _sweep_attempts(os.path.join(root, ATTEMPTS_DIR_NAME))
    return done


def _append_to_journal(path, lines, env):
    """Append the lines whose lineId the journal at path does not hold yet,
    after cutting a partial last line. Raises JournalUnavailable when the
    journal takes no write."""
    journal = Journal(None, None, env=env, path=path)
    directory = os.path.dirname(path)
    try:
        current_platform().ensure_private_dir(os.path.dirname(directory))
        current_platform().ensure_private_dir(directory)
    except PlatformRefusal as exc:
        raise JournalUnavailable(exc.reason, exc.path)
    fd = journal._open()
    try:
        with open(path, "rb") as fh:
            data = fh.read()
        if data and not data.endswith(b"\n"):
            # A line cut by a crash: drop it, its full form follows.
            keep = data.rfind(b"\n") + 1
            os.ftruncate(fd, keep)
            data = data[:keep]
    finally:
        os.close(fd)
    present = set()
    for raw in (data, _read_rotated(path)):
        for line in raw.splitlines():
            try:
                lid = json.loads(line.decode("utf-8")).get("lineId")
            except (ValueError, AttributeError, UnicodeDecodeError):
                continue
            if lid:
                present.add(lid)
    for text in lines:
        try:
            lid = json.loads(text).get("lineId")
        except (ValueError, AttributeError):
            continue
        if lid in present:
            continue
        journal._append((text + "\n").encode("utf-8"))
        present.add(lid)


def _read_rotated(path):
    """The last rotated file of a journal: a line appended just before a
    rotation lives there."""
    try:
        with open(path + ".1", "rb") as fh:
            return fh.read()
    except OSError:
        return b""


def _sweep_attempts(attempts):
    """Remove attempt files no process holds and no record names: a monitor
    that died between taking an attempt lock and publishing it."""
    try:
        names = os.listdir(attempts)
    except OSError:
        return
    for name in names:
        path = os.path.join(attempts, name)
        try:
            lock = _FileLock(path, blocking=False, create=False)
        except (LockBusy, FileNotFoundError, LedgerUnavailable):
            continue
        try:
            if not _named_by_record(os.path.dirname(attempts), name):
                _remove_quietly(path)
        finally:
            lock.release()


def _named_by_record(root, key):
    """Whether a "delivering" record of any server's ledger names the
    attempt key (then the monitor that finds it recovers it)."""
    try:
        names = os.listdir(root)
    except OSError:
        return True
    for name in names:
        if not name.endswith(".json"):
            continue
        try:
            doc = json.loads(current_platform().read_own_file(os.path.join(root, name)).decode("utf-8"))
        except (PlatformRefusal, OSError, ValueError):
            return True
        for entry in doc.get("addressees") or []:
            for rec in (entry.get("events") or {}).values():
                if (rec.get("delivering") or {}).get("attempt") == key:
                    return True
    return False
