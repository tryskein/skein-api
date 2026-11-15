import signal


class _SkeinLoaderSignal(BaseException):
    pass


_skein_loader_stopping = [False]


def _skein_loader_on_signal(signum, frame):
    if not _skein_loader_stopping[0]:
        _skein_loader_stopping[0] = True
        raise _SkeinLoaderSignal()


class _SkeinLoaderStop(Exception):
    def __init__(self, reason, status=None, delete_token=False):
        Exception.__init__(self, reason)
        self.reason, self.status, self.delete_token = reason, status, delete_token


_SKEIN_LOADER_STOPS = {
    "token_expired": ("closed", 0),
    "token_file_missing": ("closed", 1),
    "token_file_insecure": ("closed", 1),
    "misconfigured": ("closed", 1),
    "runner_mismatch": ("closed", 1),
    "server_unreachable": ("paused", 1),
    "stopped": ("paused", 0),
    "internal_error": ("paused", 1),
}


def _skein_loader_action(reason, issue):
    mint = 'skein_get_monitor_script(id="%s")' % issue
    if reason == "token_expired":
        return ("The monitor for issue %s has ended: its token expired or was revoked. If you stopped "
                "it yourself, do nothing; otherwise mint a new monitor with %s." % (issue, mint))
    if reason == "token_file_missing":
        return ("The monitor for issue %s cannot start: its token file is missing, empty or unreadable. "
                "Mint a new monitor with %s." % (issue, mint))
    if reason == "token_file_insecure":
        return ("The token file of the monitor for issue %s can be read by other users, so the monitor "
                "did not start. Mint a new monitor with %s and run its `prepare` before writing the "
                "token." % (issue, mint))
    if reason == "misconfigured":
        return ("The bootstrap of the monitor for issue %s is broken (the server address, the token "
                "file path or the runner digest is not set). Mint a new monitor with %s." % (issue, mint))
    if reason == "runner_mismatch":
        return ("The runner downloaded for the monitor of issue %s differs from the one this bootstrap "
                "was minted for (the server was updated, or the download was altered), so it was not "
                "run. Mint a new monitor with %s." % (issue, mint))
    if reason == "server_unreachable":
        return ("The monitor for issue %s could not download its runner: the Skein server is "
                "unreachable or refused the download. Run the same bootstrap again later; its token "
                "stays valid until the renewal window runs out." % issue)
    if reason == "stopped":
        return ("The monitor for issue %s was stopped by a signal while starting. Run the same bootstrap "
                "again to resume; its token stays valid until the renewal window runs out. Notifications "
                "from the break are not delivered - check the issue with skein_show(id=\"%s\")." % (issue, issue))
    return ("The monitor for issue %s hit an unexpected error. Run the same bootstrap again; if it "
            "happens again, mint a new monitor with %s." % (issue, mint))


def _skein_loader_finish(stop):
    _skein_loader_stopping[0] = True
    import json, os, sys
    path = os.environ.get("SKEIN_TOKEN_FILE", "")
    if stop.delete_token and path:
        try:
            os.remove(path)
            parent = os.path.dirname(os.path.abspath(path))
            if os.path.basename(parent).startswith("skein-mon-"):
                os.rmdir(parent)
        except OSError:
            pass
    event, code = _SKEIN_LOADER_STOPS[stop.reason]
    line = {"event": event, "reason": stop.reason,
            "action": _skein_loader_action(stop.reason, os.environ.get("SKEIN_ISSUE", ""))}
    if stop.status is not None:
        line["status"] = stop.status
    sys.stdout.write(json.dumps(line, separators=(",", ":")) + "\n")
    sys.stdout.flush()
    sys.exit(code)


def _skein_loader_fetch():
    import hashlib, http.client, os, stat, urllib.error, urllib.request as u
    path = os.environ.get("SKEIN_TOKEN_FILE", "")
    base = os.environ.get("SKEIN_PUBLIC_URL", "")
    want = os.environ.get("SKEIN_RUNNER_SHA256", "")
    if not path or not base or not want:
        raise _SkeinLoaderStop("misconfigured", delete_token=bool(path))
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except OSError:
        raise _SkeinLoaderStop("token_file_missing")
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            raise _SkeinLoaderStop("token_file_missing")
        # No one else may read the token: the file is ours and private, or
        # sits in a private directory of ours (made by the prepare command).
        if hasattr(os, "getuid"):
            uid = os.getuid()
            private = st.st_uid == uid and not st.st_mode & 0o077
            if st.st_uid == uid and not private:
                try:
                    d = os.lstat(os.path.dirname(os.path.abspath(path)))
                    private = stat.S_ISDIR(d.st_mode) and d.st_uid == uid and not d.st_mode & 0o077
                except OSError:
                    pass
            if not private:
                raise _SkeinLoaderStop("token_file_insecure")
        token = os.read(fd, 65536).decode("utf-8").strip()
    except (OSError, UnicodeDecodeError):
        raise _SkeinLoaderStop("token_file_missing")
    finally:
        os.close(fd)
    if not token:
        raise _SkeinLoaderStop("token_file_missing", delete_token=True)
    req = u.Request(base + "/agent/v1/monitor-script", headers={"X-Skein-Api-Key": token})

    class NoRedirect(u.HTTPRedirectHandler):
        # A redirect would carry the token to another host and run its code.
        def redirect_request(self, *args):
            return None
    try:
        with u.build_opener(NoRedirect).open(req, timeout=30) as resp:
            if resp.getcode() != 200:
                raise _SkeinLoaderStop("server_unreachable")
            body = resp.read()
    except urllib.error.HTTPError as exc:
        if exc.code == 401:
            raise _SkeinLoaderStop("token_expired", status=401, delete_token=True)
        raise _SkeinLoaderStop("server_unreachable")
    except (urllib.error.URLError, http.client.HTTPException, OSError):
        raise _SkeinLoaderStop("server_unreachable")
    # Only the exact runner the mint named is executed.
    if hashlib.sha256(body).hexdigest() != want:
        raise _SkeinLoaderStop("runner_mismatch", delete_token=True)
    return body.decode("utf-8")


# One protected phase from the moment the handlers exist: they are installed
# INSIDE the try, so a signal at any later point — heavy imports, download,
# between download and exec, the runner's own start-up before it installs its
# handlers — ends in exactly one "stopped" line (US-3, INV-3). After exec the
# runner owns the protocol and leaves through SystemExit, which passes.
#
# The signal handler raises at most once (it sets the stopping flag), and
# every inner handler sets that flag first. So the outer try only has to catch
# the one signal that can land at the very start of an inner handler.
_skein_loader_outcome = None
try:
    try:
        signal.signal(signal.SIGTERM, _skein_loader_on_signal)
        signal.signal(signal.SIGINT, _skein_loader_on_signal)
        _skein_loader_body = _skein_loader_fetch()
        exec(_skein_loader_body)
    except SystemExit:
        raise
    except _SkeinLoaderStop as _skein_loader_exc:
        _skein_loader_stopping[0] = True
        _skein_loader_outcome = _skein_loader_exc
    except _SkeinLoaderSignal:
        _skein_loader_outcome = _SkeinLoaderStop("stopped")
    except Exception:
        _skein_loader_stopping[0] = True
        import sys, traceback
        sys.stderr.write(traceback.format_exc())
        _skein_loader_outcome = _SkeinLoaderStop("internal_error")
    except BaseException:
        # A signal exception that is not ours: the runner's StopSignal
        # escaping its start-up before it could print anything.
        _skein_loader_stopping[0] = True
        _skein_loader_outcome = _SkeinLoaderStop("stopped")
except SystemExit:
    raise
except BaseException:
    _skein_loader_stopping[0] = True
    _skein_loader_outcome = _SkeinLoaderStop("stopped")
_skein_loader_finish(_skein_loader_outcome)
