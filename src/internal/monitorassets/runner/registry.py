# --- the monitor registry inside the body (registry.json) --------------------
# The server embeds its registry.json into the body in base64 (REGISTRY_B64,
# the core part), so the SHA-256 the loader checks covers it. It is the only
# source of the agents, their methods and the channels for the runner, as it
# is for the server. The runner reads it, it does not validate it: the server
# refuses to start on an invalid registry.
#
# A record is resolved by the client name only in "client" mode: the name of
# a record of the registry gives that record; any other name gives the
# general record with its warning ("the client name was not found").

import base64
import json

# The value of SKEIN_MONITOR_CHANNEL that forces step 8 of the method order:
# the journal is a method of every record, not a channel of the registry.
JOURNAL_CHANNEL = "journal"


class Registry(object):
    """The parsed registry document."""

    def __init__(self, doc):
        self.doc = doc
        self.agents = dict((a["id"], a) for a in doc.get("agents") or [])
        self.channels = dict((c["id"], c) for c in doc.get("channels") or [])
        self.general = doc.get("general") or {}

    def channel(self, channel_id):
        """The channel record with this id, or None."""
        return self.channels.get(channel_id)

    def is_blind(self, channel_id):
        channel = self.channels.get(channel_id)
        return bool(channel and channel.get("blind"))

    def forceable(self, channel_id):
        """Whether SKEIN_MONITOR_CHANNEL may name this id: a channel or host
        of the registry, or the journal."""
        return channel_id == JOURNAL_CHANNEL or channel_id in self.channels


class Record(object):
    """The agent record a monitor runs by: client is the client name it was
    resolved from, agent the record of the registry (None for the general
    record), warnings what the agent is to be told about the resolution."""

    def __init__(self, client, agent, general, warnings):
        self.client = client
        self.agent = agent
        self.general = general
        self.warnings = warnings

    @property
    def id(self):
        """The id of the record: the agent id, or "general"."""
        return self.agent["id"] if self.agent is not None else "general"


def load_registry(b64=None):
    """The registry of the body (REGISTRY_B64 unless b64 is given)."""
    raw = base64.b64decode(REGISTRY_B64 if b64 is None else b64)
    return Registry(json.loads(raw.decode("utf-8")))


def resolve_record(registry, client):
    """The record of the client name: the agent record of that id, else the
    general record with the warnings of the general record."""
    agent = registry.agents.get(client)
    if agent is not None:
        return Record(client, agent, None, list(agent.get("warnings") or []))
    general = registry.general
    return Record(client, None, general, list(general.get("warnings") or []))
