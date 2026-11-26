# --- start: the mode of a start (SKEIN_MONITOR_MODE) ----------------------------
# In client mode every command names its mode: the preflight command
# SKEIN_MONITOR_MODE=preflight, the working command SKEIN_MONITOR_MODE=run.
# Each command sets the variable itself, so a value left in the shell by an
# earlier command does not change it. Any other value, or none, is
# config_invalid before anything else runs. Without `client` (legacy mode)
# the variable is not read (INV-7).

import os

MODE_PREFLIGHT = "preflight"
MODE_RUN = "run"
MONITOR_MODES = (MODE_PREFLIGHT, MODE_RUN)


def monitor_mode(env, loader_mode=None):
    """The mode of this start: MODE_PREFLIGHT or MODE_RUN in client mode,
    None in legacy mode (env is not read). Raises ConfigInvalid."""
    if (LOADER_MODE if loader_mode is None else loader_mode) != "client":
        return None
    mode = env.get("SKEIN_MONITOR_MODE", "")
    if mode not in MONITOR_MODES:
        raise ConfigInvalid("SKEIN_MONITOR_MODE", "not one of %s" % ", ".join(MONITOR_MODES))
    return mode


def start():
    """Start the runner by its mode; returns the process exit code."""
    try:
        mode = monitor_mode(os.environ)
    except ConfigInvalid as exc:
        return finish(Stop("config_invalid", detail="%s (%s)" % (exc.variable, exc.detail)),
                      os.environ.get("SKEIN_ISSUE", ""), os.environ.get("SKEIN_TOKEN_FILE", ""))
    if mode == MODE_PREFLIGHT:
        return run_preflight()
    return run()
