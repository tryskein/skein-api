# --- the agent process and the identity of the addressee ---------------------
# The rule "addressee and check", items 1-2 of the spec, over a chain of
# ancestor processes (proctree.py), runner first.
#
# 1. The agent process of a named record is the nearest ancestor that bears
#    the process sign of the record (an executable name or an argument). For
#    the general record it is the nearest ancestor that is neither the
#    loader/runner nor a shell of the registry list.
# 2. The agent is nested when a recognised agent process stands above it: a
#    process of another record (with or without a non-agent between), or of
#    the same record behind a non-agent (a shell, an editor). Processes of one
#    record in a row, with no non-agent between, are one agent. The variables
#    of hosts and terminal environments, and inherited ids of own addressed
#    channels, then belong to the outer agent; ids the harness sets on every
#    agent process stay the nested agent's own.
#
# What the registry records say about an address comes in as an AddressSpec
# built by the caller from the record: the variable of the address, its
# qualifiers, whether the harness sets it on every agent process, whether it
# is a host or terminal environment of steps 5-6, the root process variable
# of a host that names the root of its agent, and the process sign of a host
# that launches agents itself (its daemon).

import os

# Both loaders put this name into the body's namespace; it appears in the
# source they run with `python3 -c`, which marks a loader process.
LOADER_MARK = "_SKEIN_LOADER_MODE"

# How a host that launches agents (HostSign) relates to the runner.
HOST_FORM_TERMINAL = "terminal"  # no agent process between runner and host
HOST_FORM_AGENT = "agent"  # the agent process was launched by the host
HOST_FORM_FOREIGN = "foreign"  # another agent stands between: not ours


def sign_matches(proc, sign):
    """Whether proc bears a process sign ({"executables": [...], "argv":
    [...]}, as in a registry record)."""
    if not sign:
        return False
    if proc.names() & set(sign.get("executables") or []):
        return True
    wanted = sign.get("argv") or []
    return bool(proc.argv) and any(a in proc.argv for a in wanted)


def record_of(proc, registry):
    """The id of the agent record whose process sign proc bears, or None."""
    for agent_id in sorted(registry.agents):
        if sign_matches(proc, registry.agents[agent_id].get("process")):
            return agent_id
    return None


def is_loader(proc, self_pid):
    """The runner itself or a loader (`python3 -c <loader source>`)."""
    if proc.pid == self_pid:
        return True
    argv = proc.argv or []
    for i, arg in enumerate(argv[:-1]):
        if arg == "-c" and LOADER_MARK in argv[i + 1]:
            return True
    return False


def is_shell(proc, registry):
    shells = set((registry.doc.get("shells") or {}).get("names") or [])
    return bool(proc.names() & shells)


class Placement(object):
    """Where the agent stands in the chain.

    agent_index is the index of the agent process (None: not determined),
    top the index of the topmost process of its block (the same record in a
    row), record the id of the record its process bears (None: none does),
    outer the outer agent process when nested."""

    def __init__(self, chain, agent_index, top, record, outer):
        self.chain = chain
        self.agent_index = agent_index
        self.top = top
        self.record = record
        self.outer = outer

    @property
    def agent(self):
        return None if self.agent_index is None else self.chain[self.agent_index]

    @property
    def nested(self):
        return self.outer is not None


def place(chain, registry, record, self_pid=None):
    """The Placement of the agent of record (a registry.Record) in chain."""
    self_pid = os.getpid() if self_pid is None else self_pid
    index = None
    for i, proc in enumerate(chain):
        if is_loader(proc, self_pid):
            continue
        if record.agent is not None:
            if sign_matches(proc, record.agent.get("process")):
                index = i
                break
        elif not is_shell(proc, registry):
            index = i
            break
    if index is None:
        return Placement(chain, None, None, None, None)
    own = record_of(chain[index], registry)
    top = index
    while own is not None and top + 1 < len(chain) and record_of(chain[top + 1], registry) == own:
        top += 1
    outer = None
    for proc in chain[top + 1:]:
        if record_of(proc, registry) is not None:
            # The same record directly above would have joined the block, so
            # any recognised agent here is behind a non-agent or of another
            # record: the agent is nested.
            outer = proc
            break
    return Placement(chain, index, top, own, outer)


def owns_root(placement, root_pid, registry):
    """Whether the root process a host names (root_pid) is the agent's: the
    agent process is the root, or the first agent process under it."""
    if placement.agent is None:
        return False
    for i in range(placement.agent_index, len(placement.chain)):
        proc = placement.chain[i]
        if proc.pid == root_pid:
            return True
        if i > placement.top and record_of(proc, registry) is not None:
            return False
    return False


def host_form(placement, registry, host_sign, self_pid=None):
    """How a host that launches agents (its process bears host_sign) relates
    to the runner: HOST_FORM_TERMINAL when no agent process stands between
    the runner and the host process (the host's terminal: the addressee is
    the host's session), HOST_FORM_AGENT when the agent process was launched
    by the host, HOST_FORM_FOREIGN when an agent stands between that the host
    did not launch, None when no ancestor is the host."""
    self_pid = os.getpid() if self_pid is None else self_pid
    chain = placement.chain
    host = None
    for i, proc in enumerate(chain):
        if not is_loader(proc, self_pid) and sign_matches(proc, host_sign):
            host = i
            break
    if host is None:
        return None
    between = [p for p in chain[:host] if not is_loader(p, self_pid) and record_of(p, registry) is not None]
    if not between:
        return HOST_FORM_TERMINAL
    if placement.agent is not None and placement.top + 1 == host and placement.agent_index < host:
        return HOST_FORM_AGENT
    return HOST_FORM_FOREIGN


class AddressSpec(object):
    """What a record says of one address of the addressee.

    channel: the channel id; env: the variable holding the address;
    qualifiers: variables whose values qualify it (a server, a session, a
    life); per_process: the harness sets env on every agent process itself,
    so nesting does not take it away; host: a host or terminal environment of
    steps 5-6, which a nested agent never owns; root_pid_env: the variable
    naming the root process of the host's agent (the address is the agent's
    only when owns_root holds); host_sign: the process sign of a host that
    launches agents itself (host_form decides)."""

    def __init__(self, channel, env, qualifiers=(), per_process=False, host=False,
                 root_pid_env=None, host_sign=None, qualifier_paths=None):
        self.channel = channel
        self.env = env
        self.qualifiers = tuple(qualifiers)
        # {qualifier: default}: qualifiers that are directory paths
        # (qualifier_value).
        self.qualifier_paths = dict(qualifier_paths or {})
        self.per_process = per_process
        self.host = host
        self.root_pid_env = root_pid_env
        self.host_sign = host_sign


class Address(object):
    __slots__ = ("channel", "value", "qualifiers", "env")

    def __init__(self, channel, value, qualifiers, env=None):
        self.channel = channel
        self.value = value
        # ((variable, value), ...) in the order of the spec.
        self.qualifiers = qualifiers
        # The variable the value was read from: the working runner compares
        # it again before a transfer.
        self.env = env

    def key(self):
        return (self.channel, self.value, self.qualifiers)


class Identity(object):
    """The identity of the addressee: process is (pid, start) of the agent
    process or None, addresses the addresses it owns, inherited the channel
    ids whose address belongs to an outer agent, terminal the channel of a
    host terminal the runner stands in (None otherwise)."""

    def __init__(self, process, addresses, inherited, terminal):
        self.process = process
        self.addresses = addresses
        self.inherited = inherited
        self.terminal = terminal


def qualifier_value(name, env, paths):
    """The value of the qualifier name in env. A qualifier that is a
    directory path (paths: {name: default}) is the path the harness uses:
    the default when the variable is unset or empty, "~" expanded from HOME
    (USERPROFILE on Windows), made normal, so an explicit default and an
    unset variable are one addressee. A "~" that cannot be expanded stays
    as it is."""
    value = env.get(name, "")
    if name not in (paths or {}):
        return value
    value = value or paths[name]
    if value == "~" or value.startswith("~/") or value.startswith("~\\"):
        home = env.get("HOME", "") or (env.get("USERPROFILE", "") if os.name == "nt" else "")
        if not home:
            return value
        value = os.path.join(home, value[2:]) if len(value) > 1 else home
    if os.path.isabs(value):
        value = os.path.normcase(os.path.normpath(value))
    return value


def _address(spec, env):
    return Address(spec.channel, env[spec.env],
                   tuple((q, qualifier_value(q, env, spec.qualifier_paths)) for q in spec.qualifiers), spec.env)


def _root_pid(spec, env):
    try:
        return int(env.get(spec.root_pid_env, ""))
    except ValueError:
        return None


def identity(placement, registry, env, specs, self_pid=None):
    """The Identity of the addressee from the variables env holds for specs."""
    present = [s for s in specs if env.get(s.env)]
    for spec in present:
        if spec.host_sign and host_form(placement, registry, spec.host_sign, self_pid) == HOST_FORM_TERMINAL:
            others = [s.channel for s in present if s is not spec]
            return Identity(None, [_address(spec, env)], others, spec.channel)
    process = None if placement.agent is None else (placement.agent.pid, placement.agent.start)
    owned, inherited = [], []
    for spec in present:
        if spec.host:
            mine = placement.agent is not None and not placement.nested
        else:
            mine = spec.per_process or not placement.nested
        if mine and spec.root_pid_env:
            root = _root_pid(spec, env)
            mine = root is not None and owns_root(placement, root, registry)
        if mine and spec.host_sign:
            mine = host_form(placement, registry, spec.host_sign, self_pid) == HOST_FORM_AGENT
        if mine:
            owned.append(_address(spec, env))
        else:
            inherited.append(spec.channel)
    return Identity(process, owned, inherited, None)
