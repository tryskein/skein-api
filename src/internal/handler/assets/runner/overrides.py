# --- client mode settings and overrides (SKEIN_CLIENT, SKEIN_PLATFORM, ------
# --- SKEIN_MONITOR_CHANNEL, SKEIN_MONITOR_BLIND) ----------------------------
# Read only in "client" mode, from the environment the bootstrap runs the
# body with. In "legacy" mode none of them is read, whatever the agent's
# machine holds: a call without `client` keeps its old behaviour.
#
# An invalid value never falls back to the automatic choice: a value of
# SKEIN_MONITOR_CHANNEL that is neither a channel of the registry nor the
# journal, a SKEIN_MONITOR_BLIND other than "off", a SKEIN_PLATFORM that is
# not the platform the runner runs on, and a malformed client name all stop
# the start with config_invalid naming the variable; the token file is kept,
# so the agent fixes the variable (or runs the preflight again) instead of
# minting a new monitor.
#
# The ban on blind channels is stronger than forcing one: a forced blind
# channel with SKEIN_MONITOR_BLIND=off is not used, and the forced method is
# the journal (a forced channel has no fallback but the journal).

import os
import re

# The form of a client name (the MCP parameter `client`).
CLIENT_NAME = re.compile(r"[a-z0-9-]{1,64}\Z")
# The values of SKEIN_PLATFORM.
PLATFORMS = ("posix", "windows")
# The one value of SKEIN_MONITOR_BLIND.
BLIND_OFF = "off"
# The warning of a forced blind channel under the ban on blind channels.
BLIND_FORCE_REFUSED = ("SKEIN_MONITOR_CHANNEL names the blind channel %s, but SKEIN_MONITOR_BLIND=off "
                       "forbids blind channels; the journal is used.")


class ConfigInvalid(Exception):
    """A start refused with config_invalid: variable names what to fix. The
    token file is kept."""

    stop_reason = "config_invalid"
    delete_token = False

    def __init__(self, variable, detail):
        Exception.__init__(self, "%s: %s" % (variable, detail))
        self.variable = variable
        self.detail = detail


class Settings(object):
    """The client mode settings of a monitor. In legacy mode every field is
    empty: record and platform are None, nothing is forced, blind channels
    are not banned."""

    def __init__(self, record=None, platform=None, channel=None, forced=None,
                 blind_off=False, warnings=None):
        self.record = record
        self.platform = platform
        # The value of SKEIN_MONITOR_CHANNEL as given (part of the working
        # command), or None.
        self.channel = channel
        # The forced channel id after the ban on blind channels, or None.
        self.forced = forced
        self.blind_off = blind_off
        self.warnings = warnings or []


def actual_platform():
    """The platform the runner runs on, in the terms of SKEIN_PLATFORM."""
    return "windows" if os.name == "nt" else "posix"


def read_settings(env, registry, mode=None):
    """The settings of this start. mode is the loader mode (LOADER_MODE
    unless given); in legacy mode env is not read. Raises ConfigInvalid."""
    if (LOADER_MODE if mode is None else mode) != "client":
        return Settings()
    client = env.get("SKEIN_CLIENT", "")
    if not CLIENT_NAME.match(client):
        raise ConfigInvalid("SKEIN_CLIENT", "not a client name")
    platform = env.get("SKEIN_PLATFORM", "")
    if platform not in PLATFORMS:
        raise ConfigInvalid("SKEIN_PLATFORM", "not one of posix, windows")
    if platform != actual_platform():
        raise ConfigInvalid("SKEIN_PLATFORM", "the runner runs on %s" % actual_platform())
    channel = env.get("SKEIN_MONITOR_CHANNEL", "") or None
    if channel is not None and not registry.forceable(channel):
        raise ConfigInvalid("SKEIN_MONITOR_CHANNEL", "not a channel of the registry")
    blind = env.get("SKEIN_MONITOR_BLIND", "")
    if blind not in ("", BLIND_OFF):
        raise ConfigInvalid("SKEIN_MONITOR_BLIND", "the only value is off")
    blind_off = blind == BLIND_OFF
    record = resolve_record(registry, client)
    warnings = list(record.warnings)
    forced = channel
    if forced is not None and blind_off and registry.is_blind(forced):
        warnings.append(BLIND_FORCE_REFUSED % forced)
        forced = JOURNAL_CHANNEL
    return Settings(record, platform, channel, forced, blind_off, warnings)
