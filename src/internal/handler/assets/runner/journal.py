# --- monitor journal (skein-monitor/journal/) --------------------------------
# One JSON object per line for every lifecycle event and every change of a
# delivery's status, in a file per monitor (keyed by the token directory)
# under the user directory, apart from the state next to the token: the
# journal outlives the token. Files are 0600 in a 0700 directory of the user,
# checked on every open; a file is rotated at 1 MiB and at most 5 files are
# kept per monitor. No token or channel secret is ever written: every value
# the monitor knows to be secret is redacted from every field first.
#
# The stop summary for a journal that cannot be written is not made here: it
# exists only in the deferred tail of the ledger.

import datetime
import hashlib
import json
import os
import re
import stat

# The subdirectory of the journals in the user directory.
JOURNAL_DIR_NAME = "journal"
# A journal file is rotated before it would grow past this size.
JOURNAL_MAX_BYTES = 1 << 20
# Files kept per monitor: the current one and the rotated ones.
JOURNAL_KEEP_FILES = 5
# What a refused journal directory means at the start of a monitor.
JOURNAL_START_REFUSAL = "config_invalid"
# Replaces a secret value wherever it appears in a journal line.
REDACTED = "[redacted]"


class JournalUnavailable(Exception):
    """The journal cannot be written. reason is the platform refusal reason
    (open_to_others, foreign_owner, symlink, ...) or "write_failed";
    stop_reason is what the start of a monitor answers to it."""

    stop_reason = JOURNAL_START_REFUSAL

    def __init__(self, reason, path):
        Exception.__init__(self, "journal unavailable (%s): %s" % (reason, path))
        self.reason = reason
        self.path = path


def journal_dir(env=None):
    return os.path.join(user_dir(env), JOURNAL_DIR_NAME)


def journal_path(token_file, env=None):
    """The journal file of the monitor of token_file: the token directory's
    name (made safe for a file name) and a hash of its absolute path."""
    token_dir = os.path.dirname(os.path.abspath(token_file))
    key = hashlib.sha256(token_dir.encode("utf-8", "surrogateescape")).hexdigest()[:16]
    name = re.sub(r"[^A-Za-z0-9._-]", "_", os.path.basename(token_dir))[:80] or "monitor"
    return os.path.join(journal_dir(env), "%s-%s.jsonl" % (name, key))


def probe_journal_dir(env=None):
    """The check of preflight and of a monitor start: the journal directory
    exists (made when absent) as a private directory of the user and takes a
    write. An existing private directory is fine. Returns its path; raises
    JournalUnavailable, whose stop_reason a start answers with."""
    directory = journal_dir(env)
    plat = current_platform()
    try:
        plat.ensure_private_dir(os.path.dirname(directory))
        plat.ensure_private_dir(directory)
    except PlatformRefusal as exc:
        raise JournalUnavailable(exc.reason, exc.path)
    probe = os.path.join(directory, ".probe-%d-%s" % (os.getpid(), os.urandom(6).hex()))
    try:
        fd = os.open(probe, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        try:
            os.write(fd, b"probe\n")
        finally:
            os.close(fd)
        os.remove(probe)
    except OSError:
        raise JournalUnavailable("write_failed", directory)
    return directory


def notification_fields(notification, with_preview=True):
    """The fields of an event a journal line carries, the same as a delivery
    text: type, notification id, issue, author, actorSessionId, commentId,
    and the preview with its truncation flag, or the mark of a deleted
    comment. with_preview=False (after access is lost) leaves the comment
    text and its marks out."""
    n = notification if isinstance(notification, dict) else {}
    entity = n.get("entity") if isinstance(n.get("entity"), dict) else {}
    actor = n.get("actor") if isinstance(n.get("actor"), dict) else {}
    out = {"notificationId": n.get("id"), "type": n.get("type")}
    if entity.get("type") == "issue" and "id" in entity:
        out["issueId"] = entity["id"]
    if "username" in actor:
        out["author"] = actor["username"]
    for key in ("actorSessionId", "commentId"):
        if key in n:
            out[key] = n[key]
    if with_preview and "commentId" in n:
        if "bodyPreview" in n:
            out["bodyPreview"] = n["bodyPreview"]
            out["truncated"] = n.get("truncated") is True
        else:
            # The poll sends no preview for a deleted comment.
            out["deleted"] = True
    return out


class Journal(object):
    """The journal of one monitor."""

    def __init__(self, token_file, issue, secrets=(), env=None):
        self.path = journal_path(token_file, env)
        self.monitor = os.path.basename(os.path.dirname(os.path.abspath(token_file)))
        self.issue = issue
        self._secrets = [s for s in secrets if s]
        self._env = env

    def add_secret(self, value):
        """Redact value from every later line too."""
        if value and value not in self._secrets:
            self._secrets.append(value)

    def lifecycle(self, event, **fields):
        """A lifecycle line: monitor_started, degraded, recovered, a stop."""
        self._write(dict(fields, event=event))

    def delivery(self, notification, status, method=None, channel=None, addressee=None,
                 answered=None, reason=None, with_preview=True):
        """A change of the status of one event's delivery."""
        line = {"event": "delivery", "status": status, "method": method, "channel": channel,
                "addressee": addressee, "channelAnswered": answered, "reason": reason}
        line.update(notification_fields(notification, with_preview))
        self._write(line)

    def summary_cursor_reset(self, cursor):
        """The server rejected the cursor: events between it and the new live
        start have no line of their own."""
        self._write({"event": "summary", "summary": "cursor_reset", "cursor": cursor})

    def summary_read_limited(self, pages):
        """Reading on after rootClosed stopped at its page budget: later
        events have no line of their own."""
        self._write({"event": "summary", "summary": "read_limited", "pages": pages})

    def _write(self, record):
        record = dict(record, ts=_utc_now(), monitor=self.monitor, issue=self.issue)
        record = _redact(record, self._secrets)
        text = json.dumps(record, separators=(",", ":"), sort_keys=True)
        if _holds_secret(json.loads(text), self._secrets):
            # Redaction missed it (a value of another type): the line is not
            # written. Checked on the decoded line, so an escaped form of a
            # secret is found too.
            raise JournalUnavailable("secret_in_line", self.path)
        data = (text + "\n").encode("utf-8")
        if len(data) > JOURNAL_MAX_BYTES:
            # No file may pass the limit, so a line longer than a whole file
            # is not written; the caller records the failure.
            raise JournalUnavailable("too_large", self.path)
        directory = os.path.dirname(self.path)
        plat = current_platform()
        try:
            plat.ensure_private_dir(os.path.dirname(directory))
            plat.ensure_private_dir(directory)
        except PlatformRefusal as exc:
            raise JournalUnavailable(exc.reason, exc.path)
        try:
            self._append(data)
        except OSError:
            raise JournalUnavailable("write_failed", self.path)

    def _append(self, data):
        fd = self._open()
        try:
            size = os.fstat(fd).st_size
            if size and size + len(data) > JOURNAL_MAX_BYTES:
                os.close(fd)
                fd = None
                self._rotate()
                fd = self._open()
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view):]
        finally:
            if fd is not None:
                os.close(fd)

    def _open(self):
        """Open the current file for appending; it must be a regular 0600-ish
        file of ours."""
        try:
            fd = os.open(self.path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0),
                         0o600)
        except OSError as exc:
            raise JournalUnavailable(_open_refusal(exc), self.path)
        st = os.fstat(fd)
        reason = None
        if not stat.S_ISREG(st.st_mode):
            reason = "not_regular"
        elif hasattr(os, "getuid") and st.st_uid != os.getuid():
            reason = "foreign_owner"
        elif st.st_mode & 0o077:
            reason = "open_to_others"
        if reason:
            os.close(fd)
            raise JournalUnavailable(reason, self.path)
        return fd

    def _rotate(self):
        """current → .1 → … → .(KEEP-1); the oldest is dropped."""
        oldest = "%s.%d" % (self.path, JOURNAL_KEEP_FILES - 1)
        try:
            os.remove(oldest)
        except FileNotFoundError:
            pass
        for i in range(JOURNAL_KEEP_FILES - 2, 0, -1):
            try:
                os.replace("%s.%d" % (self.path, i), "%s.%d" % (self.path, i + 1))
            except FileNotFoundError:
                pass
        os.replace(self.path, self.path + ".1")


def _utc_now():
    now = datetime.datetime.now(datetime.timezone.utc)
    return now.strftime("%Y-%m-%dT%H:%M:%S.") + "%03dZ" % (now.microsecond // 1000)


def _redact(value, secrets):
    if not secrets:
        return value
    if isinstance(value, str):
        for s in secrets:
            value = value.replace(s, REDACTED)
        return value
    if isinstance(value, dict):
        # Keys are redacted too: a secret may name a field of free-form details.
        return dict((_redact(str(k), secrets), _redact(v, secrets)) for k, v in value.items())
    if isinstance(value, (list, tuple)):
        return [_redact(v, secrets) for v in value]
    return value


def _holds_secret(value, secrets):
    """True when a secret appears in any key or string of value."""
    if isinstance(value, str):
        return any(s in value for s in secrets)
    if isinstance(value, dict):
        return any(_holds_secret(k, secrets) or _holds_secret(v, secrets) for k, v in value.items())
    if isinstance(value, list):
        return any(_holds_secret(v, secrets) for v in value)
    return False
