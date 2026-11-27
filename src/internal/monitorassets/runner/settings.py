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
# entry is followed to its end as YAML finds it (_ValueScan): a flow
# collection or a quoted scalar ends on its line or on lines deeper than its
# key; one that runs on at the key's indentation, a bracket closed by the
# other kind, or text after the value, leaves the document not read.

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
        lines, raws = _significant_lines(text)
        if not lines:
            return SETTING_ABSENT
        return _read(lines, 0, len(lines), lines[0][0], list(keys), raws)
    except _NotRead:
        return SETTING_NOT_READ


def _significant_lines(text):
    """(indent, content, index among the lines) of every line that is not
    blank or a comment, and every line of text (its CR dropped)."""
    if text.startswith("\ufeff"):
        text = text[1:]
    out = []
    raws = []
    for raw in text.split("\n"):
        if raw.endswith("\r"):
            raw = raw[:-1]
        raws.append(raw)
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
        out.append((indent, content, len(raws) - 1))
    return out, raws


def _is_sequence_item(content):
    return content == "-" or content.startswith("- ")


def _entries(lines, start, end, indent, raws):
    """{key: (rest, first child line, end of its lines)} of the block mapping
    whose entries are the lines start..end at indent (raws: every line of
    the document)."""
    entries = {}
    current = None  # [key, rest, line]
    for i in range(start, end):
        line_indent, content, _ = lines[i]
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
                _value_ends(rest, lines[i][2], indent, raws)
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
# The bracket that closes each opening one.
_CLOSING = {"[": "]", "{": "}"}


def _value_ends(rest, line, indent, raws):
    """Raise _NotRead unless the value on the entry's line (rest; line: its
    index among raws, the entry at indent) ends where YAML ends it: on its
    line, or - a flow collection or a quoted scalar that runs on - on lines
    deeper than the entry, which the mapping skips as the entry's own. A
    block scalar's header is checked; its lines are deeper anyway."""
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
    scan = _ValueScan()
    if scan.feed(rest):
        return
    for raw in raws[line + 1:]:
        content = raw.lstrip(" ")
        if not content.strip():
            # A blank line inside the value (a fold of a quoted scalar).
            continue
        if len(raw) - len(content) <= indent:
            # The value runs on at the entry's own indentation or out of the
            # mapping: no YAML the lines after it can be read by.
            raise _NotRead()
        if scan.feed(content):
            return
    raise _NotRead()


class _ValueScan(object):
    """The end of a flow collection or a quoted scalar as YAML finds it,
    fed line by line: a quote starts a quoted scalar only where a node
    starts (at the value's start, after an opening bracket, a comma or the
    colon of a flow entry), inside a plain scalar it is a character; each
    closing bracket must close the bracket it pairs with; "#" after white
    space starts a comment outside quotes, inside a flow collection too; and
    after the value only white space and a comment may follow."""

    def __init__(self):
        self.stack = []
        self.quote = None
        self.node_start = True
        self.done = False

    def _closed(self):
        self.node_start = False
        if not self.stack:
            self.done = True

    def feed(self, text):
        """Scan one line; whether the value ended."""
        i = 0
        n = len(text)
        # The start of a line counts as white space before a "#".
        space = True
        while i < n:
            ch = text[i]
            if self.quote == "'":
                if ch == "'":
                    if text[i + 1:i + 2] == "'":
                        i += 2
                        continue
                    self.quote = None
                    self._closed()
                i += 1
                continue
            if self.quote == '"':
                if ch == "\\":
                    i += 2
                    continue
                if ch == '"':
                    self.quote = None
                    self._closed()
                i += 1
                continue
            if ch in " \t":
                space = True
                i += 1
                continue
            if ch == "#" and space:
                break
            if self.done:
                raise _NotRead()
            space = False
            if ch in "'\"" and self.node_start:
                self.quote = ch
                self.node_start = False
            elif ch in "[{":
                self.stack.append(ch)
                self.node_start = True
            elif ch in "]}":
                if not self.stack or _CLOSING[self.stack[-1]] != ch:
                    raise _NotRead()
                self.stack.pop()
                self._closed()
            elif ch == "," and self.stack:
                self.node_start = True
            elif ch == ":" and self.stack and (i + 1 == n or text[i + 1] in " \t,[]{}"):
                self.node_start = True
            else:
                self.node_start = False
            i += 1
        return self.done


def _read(lines, start, end, indent, keys, raws):
    entries = _entries(lines, start, end, indent, raws)
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
        return _read(lines, child_start, child_end, lines[child_start][0], keys[1:], raws)
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
