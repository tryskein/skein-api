# --- takeover after "addressee changed" (US-12, OST-2-8, OST-2-9) --------------
# A detached runner whose addressee changed writes only the journal and marks
# that in its state. A new preflight of the token is then allowed while that
# runner lives: it writes a new result into preflight.json, the part of the
# state only the preflight writes. The next working start of that result
# takes the monitor over:
#
#   1. it finds the one-runner lock (locks/<key>.lock, P-5) held, checks the
#      result it names against the last preflight.json (an outdated number is
#      config_invalid, without a poll or a delivery) and that the holder's
#      addressee changed (else already_running);
#   2. it writes the takeover request (takeover.json, beside the token) and
#      waits for the lock up to TAKEOVER_WAIT_SEC;
#   3. the old runner sees the request between pages, when every outcome,
#      its cursor and its journal lines are written, and releases the lock
#      as its last step without writing anything more (P-12);
#   4. the successor, holding the lock, checks the result again (P-6), and
#      once its monitor_started is out writes "taken_over" to the journal
#      and removes the request. A successor that fails before its
#      monitor_started leaves the token and the state with the saved cursor
#      and writes "takeover_incomplete".
#
# The wait runs out: already_running, the request is withdrawn and the old
# runner goes on into the journal (no "taken_over" line). An old runner that
# stopped on its own before the handover acted by its own stop reason, and
# the successor sees its result (token_file_missing after root_closed, for
# one). The result in force is the last preflight: of the starts with it the
# one that takes the lock goes on, the other gets already_running.

import json
import os
import time
import uuid

# How long a working start waits for the one-runner lock of a runner it
# takes over (the common wait of a start, US-12).
TAKEOVER_WAIT_SEC = 30
# The takeover request, beside the token (it shares the token's fate).
TAKEOVER_FILE_NAME = "takeover.json"
# How often a waiting runner looks for a takeover request.
TAKEOVER_CHECK_SEC = 1.0


class TakenOver(Exception):
    """A successor asked for this runner's lock: hand over and end."""


def takeover_file_path(token_file):
    return os.path.join(os.path.dirname(os.path.abspath(token_file)), TAKEOVER_FILE_NAME)


def load_takeover(token_file):
    """The takeover request, or None."""
    try:
        if os.name == "posix":
            raw = _read_checked(takeover_file_path(token_file), _state_private)
        else:
            with open(takeover_file_path(token_file), "rb") as fh:
                raw = fh.read()
        doc = json.loads(raw.decode("utf-8"))
    except (PlatformRefusal, OSError, ValueError):
        return None
    if not isinstance(doc, dict) or not isinstance(doc.get("preflightId"), int) or not doc.get("id"):
        return None
    return doc


def request_takeover(token_file, preflight_id):
    """Write the request of a start of result preflight_id (under
    state.lock); returns its id."""
    request = {"preflightId": preflight_id, "id": uuid.uuid4().hex}
    data = json.dumps(request, separators=(",", ":"), sort_keys=True).encode("utf-8")
    with StateLock(token_file, create=False):
        PosixPlatform.atomic_write(takeover_file_path(token_file), data, dir_check=_state_dir_ok)
    return request["id"]


def withdraw_takeover(token_file, request_id=None):
    """Remove the request (only the one of request_id, when given)."""
    try:
        with StateLock(token_file, create=False):
            doc = load_takeover(token_file)
            if doc is not None and (request_id is None or doc["id"] == request_id):
                os.remove(takeover_file_path(token_file))
    except (PlatformRefusal, OSError):
        pass


def takeover_requested(token_file, own_preflight):
    """A successor of the last preflight result, not this runner's own,
    waits for the lock."""
    request = load_takeover(token_file)
    if request is None:
        return False
    last = load_preflight(token_file)
    return (last is not None and last["preflightId"] == request["preflightId"]
            and request["preflightId"] != own_preflight)


def take_over(token_file, env, settings):
    """A working start found the one-runner lock held: take the monitor over
    when its result is the one in force and the holder's addressee changed.
    Returns (lock, request id); raises ConfigInvalid for an outdated result,
    Stop("already_running") when there is nothing to take over or the wait
    runs out (the request is withdrawn)."""
    try:
        # Only an existing state.lock: a holder that is ending removes it with
        # the token, and a competitor must not put it back (P-12).
        with StateLock(token_file, create=False):
            doc = check_result(token_file, env, settings)
    except PlatformRefusal as exc:
        if exc.reason == "not_found":
            raise Stop("already_running")
        raise ConfigInvalid(os.path.dirname(os.path.abspath(token_file)),
                            "the preflight result cannot be read (%s)" % exc.reason)
    except OSError as exc:
        raise ConfigInvalid(os.path.dirname(os.path.abspath(token_file)),
                            "the preflight result cannot be read (%s)" % exc)
    if not addressee_changed(token_file):
        raise Stop("already_running")
    try:
        request_id = request_takeover(token_file, doc["preflightId"])
    except PlatformRefusal as exc:
        if exc.reason == "not_found":
            raise Stop("already_running")
        raise ConfigInvalid(takeover_file_path(token_file),
                            "the takeover request cannot be written (%s)" % exc.reason)
    except OSError as exc:
        raise ConfigInvalid(takeover_file_path(token_file), "the takeover request cannot be written (%s)" % exc)
    try:
        lock = wait_runner_lock(token_file, env, TAKEOVER_WAIT_SEC)
    except LockBusy:
        withdraw_takeover(token_file, request_id)
        raise Stop("already_running")
    except PlatformRefusal as exc:
        withdraw_takeover(token_file, request_id)
        raise ConfigInvalid(runner_lock_path(token_file, env), "the runner lock cannot be taken (%s)" % exc.reason)
    return lock, request_id


def wait_runner_lock(token_file, env, wait_sec):
    """Wait up to wait_sec for the one-runner lock (the common wait of a
    start). Raises LockBusy when it is still held, PlatformRefusal."""
    return current_platform().acquire_runner_lock(token_file, wait_sec, env)


def sleep_or_hand_over(sec, sink):
    """Sleep sec seconds; a runner whose sink can be taken over looks for a
    takeover request meanwhile and raises TakenOver when one waits."""
    check = getattr(sink, "taken_over", None)
    if check is None:
        time.sleep(sec)
        return
    end = time.monotonic() + sec
    while True:
        if check():
            raise TakenOver()
        left = end - time.monotonic()
        if left <= 0:
            return
        time.sleep(min(left, TAKEOVER_CHECK_SEC))
