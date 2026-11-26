# --- one's own echo (US-13) ---------------------------------------------------
# An event the agent's own MCP session caused must not wake that agent. The
# value of the exclusion is a session id:
#
#   * before the agent writes the exclusion file, the session of the mint
#     (SKEIN_EXCLUDE_SESSION, as without `client`);
#   * a non-empty valid exclusion file replaces it; the file is read again on
#     every poll, so the agent writes a new session there after an MCP
#     reconnect;
#   * an empty, missing or unreadable file does not lift the suppression: the
#     last known value stays. The last valid value of the file is kept in the
#     monitor state and survives a restart.
#
# Every poll sends the current value (the server's best-effort filter), and
# before an event is handed to a channel the runner applies the current
# value to it: an event of that session is not sent, its outcome in the
# journal is "own echo". An event without actorSessionId or of another
# session goes by the ordinary rules. Without `client` the runner keeps the
# old mechanism: the mint's session in the poll query, no exclusion file.

import re

# The exclusion file, in the directory of the token file (P-5).
EXCLUDE_FILE_NAME = "exclude_session"
# A session id the exclusion file may hold (surrounding whitespace aside).
EXCLUDE_SESSION_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,255}\Z")
# The most of the exclusion file that is read: a longer file is not a
# session id.
EXCLUDE_FILE_MAX = 1024


def exclude_file_path(token_file):
    return os.path.join(os.path.dirname(os.path.abspath(token_file)), EXCLUDE_FILE_NAME)


def read_exclude_file(token_file):
    """The session id the exclusion file holds, or None: no file, a file
    that is empty, not ours, open to writes by others, a link, too long, or
    not one session id."""
    path = exclude_file_path(token_file)
    try:
        if os.name == "posix":
            raw = _read_checked(path, _exclude_file_ok)
        else:
            with open(path, "rb") as fh:
                raw = fh.read(EXCLUDE_FILE_MAX + 1)
    except (PlatformRefusal, OSError):
        return None
    if len(raw) > EXCLUDE_FILE_MAX:
        return None
    try:
        value = raw.decode("utf-8").strip()
    except UnicodeDecodeError:
        return None
    return value if EXCLUDE_SESSION_RE.match(value) else None


def _exclude_file_ok(file_st, dir_st):
    """The exclusion file is read when no one else can write it or the
    directory it lies in (the agent may write it with the mode of its own
    file tool, readable by others: a session id is not a secret)."""
    return not file_st.st_mode & 0o022 and _state_dir_ok(dir_st)


class Exclusion(object):
    """The exclusion of one's own echo of a client-mode monitor. Without a
    token file it is the mint's session alone (no exclusion file)."""

    def __init__(self, token_file, mint):
        self.token_file = token_file
        self._saved = load_exclusion(token_file) if token_file else None
        # The value in force: the last valid file value, else the mint's.
        self.value = self._saved or (mint or "")

    def current(self):
        """Read the exclusion file again (one poll) and return the value in
        force. A new file value is kept in the state; a state that cannot be
        written is tried again on the next poll, the value holds meanwhile."""
        if not self.token_file:
            return self.value
        got = read_exclude_file(self.token_file)
        if got is not None:
            self.value = got
            if got != self._saved:
                try:
                    save_exclusion(self.token_file, got)
                    self._saved = got
                except (OSError, PlatformRefusal):
                    pass
        return self.value

    def own(self, notification):
        """The event was caused by the excluded session."""
        if not self.value or not isinstance(notification, dict):
            return False
        return notification.get("actorSessionId") == self.value


def as_exclusion(exclude):
    """exclude as an Exclusion: a session id (the mint's, without `client`)
    becomes an exclusion of that session alone."""
    return exclude if isinstance(exclude, Exclusion) else Exclusion(None, exclude)


def exclusion_value(exclude):
    """The exclusion a poll sends: exclude is a session id or an Exclusion,
    read again for this poll."""
    return as_exclusion(exclude).current()
