# --- narrow reading of a YAML setting (yaml-scalar) ----------------------------
# A method condition may read one setting of an agent's YAML configuration
# (registry.go#SettingYAMLScalar): the scalar at a path of keys through block
# mappings. There is no YAML library in the standard library, and a reader of
# the whole language is out of place here, so this part reads only a narrow
# form and gives SETTING_NOT_READ for anything else - a setting not read is a
# condition that does not hold.
#
# Read: block mappings, a line per entry `key:` with an optional one-line
# scalar and an optional comment, nested by spaces; the scalar plain, in
# single quotes ('' for a quote) or in double quotes (escapes \\ and \" only).
# A plain scalar is typed as YAML 1.1 types it (the reading of the agents
# whose settings this serves, e.g. Hermes, hermes_yaml.py#_load): true/false/
# yes/no/on/off in three spellings are booleans, ~/null/empty is null, a
# decimal integer is a number, the rest is a string.
#
# Not read: an anchor, alias, tag or merge key, flow syntax, a multi-line or
# block scalar, a complex key on the path or as the value; a tab in an
# indentation, a document marker or directive, a line break other than LF or
# CRLF anywhere in the file; a duplicate key or a line that is no entry in a
# mapping the path goes through; a plain scalar YAML 1.1 reads as a number,
# date or other type in a form other than a decimal integer, or as a boolean
# by y/n. The entries the path does not go through are skipped by their
# indentation: whatever they hold is deeper than their key, as YAML requires
# of a block mapping's values (a sequence may also start at the key's own
# indentation, `key:` then `- item` lines). The value on the line of such an
# entry must still end on its line, or start a block scalar: a flow
# collection or a quoted scalar left open there runs on past its line, so
# the lines after it cannot be told apart by indentation, and the document is
# not read.

import re

SETTING_NOT_READ = object()
# The file holds no such key (an empty document, a missing key, a scalar or
# null where the path needs a mapping): the agent's default applies.
SETTING_ABSENT = object()

_YAML_BOOLEANS = {}
for _word, _value in (("true", True), ("false", False), ("yes", True), ("no", False),
                      ("on", True), ("off", False)):
    for _spelling in (_word, _word.capitalize(), _word.upper()):
        _YAML_BOOLEANS[_spelling] = _value
_YAML_NULLS = ("~", "null", "Null", "NULL")
# YAML 1.1 reads these as booleans too, depending on the reader: not read.
_YAML_AMBIGUOUS = ("y", "Y", "n", "N", "=", "<<")
_DECIMAL = re.compile(r"[-+]?(?:0|[1-9][0-9]*)\Z")
# An entry of a block mapping: a plain key (no indicator first, no ":" or
# "#" in it), ":", then end of line or spaces and the rest.
_ENTRY = re.compile(r"(?P<key>[^\s\-?:,\[\]{}#&*!|>'\"%@`][^:#]*?) *:(?: +(?P<rest>.*))?\Z")
# Characters a value may not start with (flow, block scalar, anchor, alias,
# tag, reserved, sequence, complex key).
_VALUE_INDICATORS = "[]{}|>&*!%@`,"
# Line breaks YAML 1.1 knows beyond LF and CRLF.
_OTHER_BREAKS = ("\r", "\x85", "\u2028", "\u2029")


class _NotRead(Exception):
    pass


def read_yaml_scalar(text, keys):
    """The scalar at the path of keys of the YAML document text (a str);
    SETTING_ABSENT when the document holds no such key; SETTING_NOT_READ
    when the document is outside the narrow form or the path leads to a
    collection."""
    if not keys:
        return SETTING_NOT_READ
    try:
        lines = _significant_lines(text)
        if not lines:
            return SETTING_ABSENT
        return _read(lines, 0, len(lines), lines[0][0], list(keys))
    except _NotRead:
        return SETTING_NOT_READ


def _significant_lines(text):
    """(indent, content) of every line that is not blank or a comment."""
    if text.startswith("\ufeff"):
        text = text[1:]
    out = []
    for raw in text.split("\n"):
        if raw.endswith("\r"):
            raw = raw[:-1]
        if any(b in raw for b in _OTHER_BREAKS):
            raise _NotRead()
        content = raw.lstrip(" ")
        indent = len(raw) - len(content)
        if content.startswith("\t"):
            raise _NotRead()
        if raw.startswith(("---", "...", "%")):
            raise _NotRead()
        if not content or content.startswith("#"):
            continue
        out.append((indent, content))
    return out


def _is_sequence_item(content):
    return content == "-" or content.startswith("- ")


def _entries(lines, start, end, indent):
    """{key: (rest, first child line, end of its lines)} of the block mapping
    whose entries are the lines start..end at indent."""
    entries = {}
    current = None  # [key, rest, line]
    for i in range(start, end):
        line_indent, content = lines[i]
        if line_indent < indent:
            raise _NotRead()
        if line_indent > indent:
            if current is None:
                raise _NotRead()
            continue
        if _is_sequence_item(content):
            # A sequence at the key's own indentation: the value of an entry
            # with nothing after its colon.
            if current is None or current[1] is not None:
                raise _NotRead()
            continue
        match = _ENTRY.match(content)
        if not match:
            raise _NotRead()
        key = match.group("key")
        if key in entries or key == "<<":
            raise _NotRead()
        # Only spaces or a comment after the colon: nothing on the line,
        # the same as no rest at all.
        rest = match.group("rest")
        if rest is not None:
            rest = rest.strip(" ")
            if not rest or rest.startswith("#"):
                rest = None
            else:
                _ends_on_its_line(rest)
        if current is not None:
            entries[current[0]] = (current[1], current[2] + 1, i)
        current = [key, rest, i]
        entries[key] = None
    if current is not None:
        entries[current[0]] = (current[1], current[2] + 1, end)
    return entries


# The header of a block scalar: | or >, its indicators, an optional comment.
_BLOCK_HEADER = re.compile(r"[|>][-+1-9]{0,2}(?: +#.*)?\Z")
# An anchor or a tag before a value.
_PROPERTY = re.compile(r"[&!][^ ]* +")


def _ends_on_its_line(rest):
    """Raise _NotRead unless the value on an entry's line ends on that line
    (or is the header of a block scalar, whose lines are deeper): a flow
    collection with its brackets closed, a quoted scalar with its quote."""
    while True:
        prop = _PROPERTY.match(rest)
        if not prop:
            break
        rest = rest[prop.end():]
    if rest[:1] in "|>":
        if not _BLOCK_HEADER.match(rest):
            raise _NotRead()
        return
    if rest[:1] not in "[{'\"":
        return
    depth = 0
    quote = None
    i = 0
    while i < len(rest):
        ch = rest[i]
        if quote == "'":
            if ch == "'":
                if rest[i + 1:i + 2] == "'":
                    i += 2
                    continue
                quote = None
        elif quote == '"':
            if ch == "\\":
                i += 2
                continue
            if ch == '"':
                quote = None
        elif ch in "'\"":
            quote = ch
        elif ch in "[{":
            depth += 1
        elif ch in "]}":
            depth -= 1
            if depth < 0:
                raise _NotRead()
        elif ch == "#" and i > 0 and rest[i - 1] == " " and depth == 0:
            break
        i += 1
        if depth == 0 and quote is None:
            # The value ended: only spaces and a comment may follow it.
            tail = rest[i:].lstrip(" ")
            if tail and not tail.startswith("#"):
                raise _NotRead()
            return
    raise _NotRead()


def _read(lines, start, end, indent, keys):
    entries = _entries(lines, start, end, indent)
    entry = entries.get(keys[0])
    if entry is None:
        return SETTING_ABSENT
    rest, child_start, child_end = entry
    has_children = child_start < child_end
    if len(keys) > 1:
        if rest is not None:
            if has_children:
                # The scalar goes on below its key (a multi-line scalar) or
                # the lines below are no value of it: not read either way.
                raise _NotRead()
            # A scalar where the path needs a block mapping: no such key
            # below it. Any other form (flow, anchor, tag) is not read.
            _scalar(rest)
            return SETTING_ABSENT
        if not has_children:
            # Null where the path needs a mapping: no such key below it.
            return SETTING_ABSENT
        if _is_sequence_item(lines[child_start][1]):
            return SETTING_NOT_READ
        return _read(lines, child_start, child_end, lines[child_start][0], keys[1:])
    if rest is None:
        # A collection is no scalar; nothing at all is null.
        return SETTING_NOT_READ if has_children else None
    if has_children:
        # The scalar goes on below its key: a multi-line scalar.
        raise _NotRead()
    return _scalar(rest)


def _scalar(rest):
    if "\t" in rest:
        raise _NotRead()
    first = rest[0]
    if first == "'":
        return _yaml_quoted(rest, "'")
    if first == '"':
        return _yaml_quoted(rest, '"')
    if first in _VALUE_INDICATORS or rest[:2] in ("- ", "? ", ": ") or rest in ("-", "?", ":"):
        raise _NotRead()
    cut = rest.find(" #")
    value = (rest if cut < 0 else rest[:cut]).rstrip(" ")
    if ": " in value or value.endswith(":"):
        raise _NotRead()
    if value in _YAML_BOOLEANS:
        return _YAML_BOOLEANS[value]
    if value in _YAML_NULLS:
        return None
    if value in _YAML_AMBIGUOUS:
        raise _NotRead()
    if _DECIMAL.match(value):
        return int(value)
    if value[0] in "0123456789.+-":
        # Octal, hexadecimal, binary, sexagesimal, a float, .inf, a date: a
        # number or a timestamp in YAML 1.1, in a form not read here.
        raise _NotRead()
    return value


def _yaml_quoted(rest, quote):
    out = []
    i = 1
    while i < len(rest):
        ch = rest[i]
        if quote == "'" and ch == "'":
            if rest[i + 1:i + 2] == "'":
                out.append("'")
                i += 2
                continue
            return _after_quoted(rest[i + 1:], "".join(out))
        if quote == '"' and ch == "\\":
            escaped = rest[i + 1:i + 2]
            if escaped not in ("\\", '"'):
                raise _NotRead()
            out.append(escaped)
            i += 2
            continue
        if quote == '"' and ch == '"':
            return _after_quoted(rest[i + 1:], "".join(out))
        out.append(ch)
        i += 1
    # Not closed on its line: a multi-line scalar.
    raise _NotRead()


def _after_quoted(tail, value):
    stripped = tail.lstrip(" ")
    if stripped and (not stripped.startswith("#") or stripped == tail):
        raise _NotRead()
    return value
