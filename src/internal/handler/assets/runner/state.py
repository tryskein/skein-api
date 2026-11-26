# --- monitor state next to the token (state.json) ---------------------------
# The poll cursor survives a restart with the same token file, in every mode
# of the loader, the inline loader of a call without `client` included: the
# runner saves it after every page whose lines it has printed and starts from
# it on the next run. A new token has no state, so its first poll is
# live-start. The state file shares the token file's fate: it is deleted with
# the token and kept when the token is kept.
#
# The loader puts _SKEIN_LOADER_MODE ("legacy" or "client") into the body's
# namespace before running it; a body without it is "legacy". The mode
# follows the loader the agent ran, never an environment variable, so in
# "legacy" mode no variable of the agent's machine changes the runner.

import json
import os

LOADER_MODE = "client" if globals().get("_SKEIN_LOADER_MODE") == "client" else "legacy"

# The state file, in the directory of the token file.
STATE_FILE_NAME = "state.json"
# The version of the state document; a document of another version is not
# read (the monitor starts live).
STATE_VERSION = 1


def state_file_path(token_file):
    return os.path.join(os.path.dirname(os.path.abspath(token_file)), STATE_FILE_NAME)


def load_cursor(token_file):
    """The saved poll cursor of this token file, or None: no state, a state
    file that is not ours or others can read, a link, or one that does not
    parse as a state document. None means a live-start poll."""
    doc = _load_state_doc(token_file)
    if doc is None:
        return None
    cursor = doc.get("cursor")
    if (isinstance(cursor, dict) and isinstance(cursor.get("ts"), str)
            and isinstance(cursor.get("id"), int) and not isinstance(cursor.get("id"), bool)):
        return {"ts": cursor["ts"], "id": cursor["id"]}
    return None


def load_remainder(token_file, cursor):
    """The rest of the page polled at cursor that an exit-on-event cycle
    printed only in part: (events, cursor after the page, rootClosed), or
    None. The next cycle takes these events from here, not from a new poll
    of the same page, whose events may differ by then (US-12). A rest saved
    with another cursor does not count."""
    doc = _load_state_doc(token_file)
    if doc is None or doc.get("cursor") != cursor:
        return None
    rest = doc.get("remainder")
    if not isinstance(rest, dict) or not isinstance(rest.get("events"), list) or not rest["events"]:
        return None
    nxt = rest.get("next")
    return (list(rest["events"]), nxt if isinstance(nxt, dict) else None, rest.get("rootClosed") is True)


def save_remainder(token_file, cursor, events, next_cursor, root_closed):
    """Keep the cursor and the rest of its page: the events without an
    outcome yet, the cursor after the page and its rootClosed. Raises
    OSError or PlatformRefusal when it cannot."""
    _save_state_doc(token_file, {"version": STATE_VERSION, "cursor": cursor,
                                 "remainder": {"events": list(events), "next": next_cursor,
                                               "rootClosed": bool(root_closed)}})


def load_page_outcomes(token_file, cursor):
    """The outcomes of the events of the page polled at cursor that a
    detached monitor fixed, and the ids of those already in the journal:
    ({id: outcome}, set of ids). Outcomes saved with another cursor do not
    count."""
    doc = _load_state_doc(token_file)
    if doc is None or doc.get("cursor") != cursor:
        return {}, set()
    outcomes = doc.get("outcomes")
    journaled = doc.get("journaled")
    return (dict(outcomes) if isinstance(outcomes, dict) else {},
            set(journaled) if isinstance(journaled, list) else set())


def save_page_outcomes(token_file, cursor, outcomes, journaled, redacted=()):
    """Keep the cursor and the outcomes of its page (R-6: after the ledger,
    before the journal and the cursor), and the ids whose journal line goes
    without the preview (access was lost), so a line written later keeps
    that decision. Raises OSError or PlatformRefusal when it cannot."""
    _save_state_doc(token_file, {"version": STATE_VERSION, "cursor": cursor, "outcomes": dict(outcomes),
                                 "journaled": sorted(journaled), "redacted": sorted(redacted)})


def load_page_redacted(token_file, cursor):
    """The ids of the page polled at cursor whose journal line goes without
    the preview."""
    doc = _load_state_doc(token_file)
    if doc is None or doc.get("cursor") != cursor or not isinstance(doc.get("redacted"), list):
        return set()
    return set(doc["redacted"])


def _load_state_doc(token_file):
    path = state_file_path(token_file)
    try:
        if os.name == "posix":
            raw = _read_checked(path, _state_private)
        else:
            with open(path, "rb") as fh:
                raw = fh.read()
    except (PlatformRefusal, OSError):
        return None
    try:
        doc = json.loads(raw.decode("utf-8"))
    except ValueError:
        return None
    if not isinstance(doc, dict) or doc.get("version") != STATE_VERSION:
        return None
    return doc


def save_cursor(token_file, cursor):
    """Write the cursor (None: live-start next time) to the state file
    atomically. Raises OSError or PlatformRefusal when it cannot."""
    _save_state_doc(token_file, {"version": STATE_VERSION, "cursor": cursor})


def _save_state_doc(token_file, doc):
    data = json.dumps(doc, separators=(",", ":"), sort_keys=True).encode("utf-8")
    path = state_file_path(token_file)
    if os.name == "posix":
        PosixPlatform.atomic_write(path, data, dir_check=_state_dir_ok)
        return
    tmp = "%s.tmp-%d" % (path, os.getpid())
    try:
        with open(tmp, "wb") as fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.remove(tmp)
        except OSError:
            pass
        raise


def remove_state_file(token_file):
    """Delete the state file of this token file when it is ours."""
    path = state_file_path(token_file)
    try:
        if hasattr(os, "getuid") and os.lstat(path).st_uid != os.getuid():
            return
        os.remove(path)
    except OSError:
        pass


def _state_dir_ok(dir_st):
    """The token directory may hold the state: it is ours and no one else
    can create, replace or remove files in it."""
    return dir_st.st_uid == os.getuid() and not dir_st.st_mode & 0o022


def _state_private(file_st, dir_st):
    """A state file is read when it is ours (checked by the caller), has no
    group/other bits and lies in a directory the state may live in. The
    directory itself is opened without following a link."""
    return not file_st.st_mode & 0o077 and _state_dir_ok(dir_st)
