"""Tests of the narrow YAML setting reader (runner/settings.py, format
yaml-scalar of registry.go#SettingCondition), through the reading a method
condition does (runner/preflight.py#read_setting).

The forms come from the agent whose settings it serves, Hermes Agent
(24b14819): hermes_yaml.py#roundtrip_yaml writes config.yaml in block style,
mappings indented by 2, sequences by 4 with the dash at 2, no line folding,
and strings YAML 1.1 reads otherwise ("off", "yes") in double quotes
(utils/files.py#_YAML11_AMBIGUOUS_WORDS); hermes_yaml.py#_load reads it as
YAML 1.1, where a bare off is the boolean false. A user writes the file by
hand too: a sequence at its key's indentation, single quotes, comments.

Matrix row #111: a scalar at a path of keys through block mappings is read;
anchors, flow syntax, multi-line scalars, tabs, duplicate keys and other
forms give no value.

Run on its own: python3 internal/handler/assets/runner_tests/settings_yaml_test.py
Standard library of Python 3.6 only.
"""

import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness  # noqa: E402

RUNNER = harness.load_runner()

# A config.yaml as Hermes writes it: the comment of a created file, sections
# in block style, a sequence indented under its key, an ambiguous word
# quoted.
HERMES_WRITTEN = """# Hermes Agent configuration
model:
  default: anthropic/claude-sonnet-4
  provider: auto
toolsets:
  - hermes-cli
  - web
display:
  compact: false
  busy_input_mode: queue
  background_process_notifications: "off"
  tool_progress: all
_config_version: 35
"""

# The same settings written by hand.
HAND_WRITTEN = """toolsets:
- hermes-cli
- web
system_prompt: |
  display:
    busy_input_mode: interrupt
display:   # what the terminal shows
  busy_input_mode: 'queue'   # follow-ups wait for the turn
  background_process_notifications: off
  note: 'it''s here'
  escaped: "a \\"quoted\\" word"
  hash: value#not-a-comment
  empty:
providers:
  local:
    url: http://localhost:8080/v1
"""


class YamlSettingTest(unittest.TestCase):

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="skein-yaml-")
        self.addCleanup(shutil.rmtree, self.root, True)
        self.env = {"HERMES_HOME": self.root}

    def read(self, text, key):
        with open(os.path.join(self.root, "config.yaml"), "w", encoding="utf-8", newline="") as fh:
            fh.write(text)
        cond = {"path": "{env:HERMES_HOME}/config.yaml", "format": "yaml-scalar", "key": key, "equals": "queue"}
        return RUNNER.read_setting(cond, self.env)

    def assert_read(self, text, key, expected):
        value = self.read(text, key)
        self.assertIsNot(value, RUNNER._MISSING, "%s not read" % key)
        self.assertIsNot(value, RUNNER._ABSENT, "%s absent" % key)
        self.assertEqual(type(value), type(expected), "%s read as %r" % (key, value))
        self.assertEqual(value, expected)

    def assert_not_read(self, text, key):
        self.assertIs(self.read(text, key), RUNNER._MISSING)

    def test_the_scalars_of_a_file_hermes_wrote_are_read(self):
        for key, expected in (("display.busy_input_mode", "queue"),
                              # Quoted by the writer: the string, not the boolean.
                              ("display.background_process_notifications", "off"),
                              ("display.compact", False),
                              ("display.tool_progress", "all"),
                              ("_config_version", 35),
                              ("model.default", "anthropic/claude-sonnet-4")):
            with self.subTest(key=key):
                self.assert_read(HERMES_WRITTEN, key, expected)

    def test_the_scalars_of_a_file_written_by_hand_are_read(self):
        for key, expected in (("display.busy_input_mode", "queue"),
                              # YAML 1.1: a bare off is the boolean false.
                              ("display.background_process_notifications", False),
                              ("display.note", "it's here"),
                              ("display.escaped", 'a "quoted" word'),
                              ("display.hash", "value#not-a-comment"),
                              ("display.empty", None),
                              ("providers.local.url", "http://localhost:8080/v1")):
            with self.subTest(key=key):
                self.assert_read(HAND_WRITTEN, key, expected)

    def test_the_yaml_1_1_scalars_are_typed(self):
        for word, expected in (("yes", True), ("No", False), ("ON", True), ("Off", False),
                               ("true", True), ("FALSE", False), ("~", None), ("null", None),
                               ("0", 0), ("-12", -12), ("queue", "queue")):
            with self.subTest(word=word):
                self.assert_read("display:\n  busy_input_mode: %s\n" % word, "display.busy_input_mode", expected)

    def test_the_tail_of_an_entry_line_is_read_as_yaml_reads_it(self):
        # YAML 1.1: spaces and a comment after the colon are no value (the
        # key holds null, or the block below it); spaces after a scalar end
        # it; a BOM opens the file; CRLF ends a line. None of them may end
        # the reading with an exception: a form not read gives no value.
        key = "display.busy_input_mode"
        for name, text, expected in (
                ("nothing after the colon", "display:\n  busy_input_mode:\n", None),
                ("spaces after the colon", "display:\n  busy_input_mode:   \n", None),
                ("one space after the colon", "display:\n  busy_input_mode: \n", None),
                ("spaces and a comment after the colon", "display:\n  busy_input_mode:   # unset\n", None),
                ("spaces after the scalar", "display:\n  busy_input_mode: queue   \n", "queue"),
                ("spaces after a quoted scalar", "display:\n  busy_input_mode: 'queue'   \n", "queue"),
                ("spaces after the section's colon", "display:   \n  busy_input_mode: queue\n", "queue"),
                ("a comment after the section's colon", "display:  # shown\n  busy_input_mode: queue\n", "queue"),
                ("CRLF and spaces", "display:  \r\n  busy_input_mode:  queue  \r\n", "queue"),
                ("CRLF after an empty value", "display:\r\n  busy_input_mode:  \r\n", None),
                ("a BOM before the first key", "\ufeffdisplay:\n  busy_input_mode: queue\n", "queue"),
                ("the last line without a line break", "display:\n  busy_input_mode:  ", None)):
            with self.subTest(case=name):
                self.assert_read(text, key, expected)
        for name, text in (
                ("a tab after the colon", "display:\n  busy_input_mode:\tqueue\n"),
                ("a tab after the scalar", "display:\n  busy_input_mode: queue\t\n"),
                ("a tab after the section's colon", "display:\t\n  busy_input_mode: queue\n"),
                ("a tab before a comment", "display:\n  busy_input_mode: queue\t# set\n")):
            with self.subTest(case=name):
                self.assert_not_read(text, key)

    def test_a_key_the_file_does_not_hold_is_absent(self):
        # No such key: the agent's default applies (a setting that must not
        # carry a value holds then; one that must carry it does not).
        for name, text, key in (
                ("no such key", HERMES_WRITTEN, "display.busy_mode"),
                ("no such section", HERMES_WRITTEN, "terminal.busy_input_mode"),
                ("a scalar where a section is needed", "display: off\n", "display.busy_input_mode"),
                ("a null where a section is needed", "display:\ncompact: true\n", "display.busy_input_mode"),
                ("an empty file", "", "display.busy_input_mode"),
                ("only comments", "# display:\n#   busy_input_mode: queue\n", "display.busy_input_mode")):
            with self.subTest(case=name):
                self.assertIs(self.read(text, key), RUNNER._ABSENT)

    def test_a_missing_file_is_absent(self):
        cond = {"path": "{env:HERMES_HOME}/config.yaml", "format": "yaml-scalar",
                "key": "display.busy_input_mode", "equals": "queue"}
        self.assertIs(RUNNER.read_setting(cond, self.env), RUNNER._ABSENT)

    def test_a_path_that_leads_to_a_collection_gives_no_value(self):
        for name, key in (("a section is no scalar", "display"), ("a sequence is no scalar", "toolsets")):
            with self.subTest(case=name):
                self.assert_not_read(HERMES_WRITTEN, key)

    def test_a_form_outside_the_narrow_reading_gives_no_value(self):
        # Every text below holds busy_input_mode: queue for a full YAML 1.1
        # reader, or makes it ambiguous: the narrow reader gives no value.
        key = "display.busy_input_mode"
        for name, text in (
                ("an anchor on the path", "display: &d\n  busy_input_mode: queue\n"),
                ("an alias as the value", "base: &q queue\ndisplay:\n  busy_input_mode: *q\n"),
                ("a merge key on the path", "base: &b\n  busy_input_mode: queue\ndisplay:\n  <<: *b\n"),
                ("a tag on the value", "display:\n  busy_input_mode: !!str queue\n"),
                ("a flow mapping on the path", "display: {busy_input_mode: queue}\n"),
                ("a flow sequence as the value", "display:\n  busy_input_mode: [queue]\n"),
                ("a literal block scalar", "display:\n  busy_input_mode: |\n    queue\n"),
                ("a folded block scalar", "display:\n  busy_input_mode: >-\n    queue\n"),
                ("a plain scalar over two lines", "display:\n  busy_input_mode: queue\n    mode\n"),
                # A key on the path with a scalar and lines below it: a
                # multi-line scalar or no YAML at all, never "no such key".
                ("a section key with a scalar and lines below", "display: shown\n  busy_input_mode: queue\n"),
                ("a section key with a quoted scalar and lines below", "display: 'shown'\n  busy_input_mode: queue\n"),
                ("a quoted scalar over two lines", "display:\n  busy_input_mode: \"queue\n    \"\n"),
                ("a tab in the indentation", "display:\n\tbusy_input_mode: queue\n"),
                ("a tab after spaces", "display:\n  \tbusy_input_mode: queue\n"),
                ("a duplicate key", "display:\n  busy_input_mode: interrupt\n  busy_input_mode: queue\n"),
                ("a duplicate section", "display:\n  compact: true\ndisplay:\n  busy_input_mode: queue\n"),
                ("a document marker", "---\ndisplay:\n  busy_input_mode: queue\n"),
                ("a directive", "%YAML 1.1\n---\ndisplay:\n  busy_input_mode: queue\n"),
                ("a complex key", "? display\n: busy_input_mode: queue\n"),
                ("a quoted key on the path", "\"display\":\n  busy_input_mode: queue\n"),
                ("a line that is no entry", "display:\n  busy_input_mode: queue\n  just text\n"),
                ("an entry without a space after the colon", "display:\n  busy_input_mode:queue\n"),
                ("a value with a colon and a space", "display:\n  busy_input_mode: queue: now\n"),
                ("an escape beyond quote and backslash", "display:\n  busy_input_mode: \"qu\\x65ue\"\n"),
                ("text after a quoted scalar", "display:\n  busy_input_mode: 'queue' now\n"),
                ("a line deeper than the section's first entry", "display:\n    compact: true\n  busy_input_mode: queue\n"),
                ("carriage returns alone as line breaks", "display:\r  busy_input_mode: queue\r"),
                ("a next line character", "display:\x85  busy_input_mode: queue\n")):
            with self.subTest(case=name):
                self.assert_not_read(text, key)

    def test_a_neighbour_s_value_is_followed_to_its_end_as_yaml_ends_it(self):
        # Review round 2 of wave 14, row 15, and round 3, rows 3, 15 and 16:
        # the value on another entry's line of a mapping the path goes
        # through ends where YAML ends it. A quote inside a plain scalar of
        # a flow is a character, brackets close their own kind, "#" after
        # white space is a comment inside a flow too, and a flow or a quoted
        # scalar may run on over lines deeper than its key. Each form gives
        # the value of busy_input_mode a YAML reader gives, or no value.
        key = "display.busy_input_mode"
        tail = "display:\n  busy_input_mode: queue\n"
        for name, text, expected in (
                # Read: valid YAML.
                ("a quote inside a flow sequence", "other: [don't]\n" + tail, "queue"),
                ("a quote after a flow entry", "toolsets: [web, it's]\n" + tail, "queue"),
                ("a double quote inside a flow plain scalar", "aliases: {q: say \"hi}\n" + tail, "queue"),
                ("a quoted bracket inside a flow", "other: [\"]\", '}']\n" + tail, "queue"),
                ("a hash inside a quoted scalar", "other: \"a # b\"\n" + tail, "queue"),
                ("a hash glued to a plain scalar in a flow", "other: [a#b]\n" + tail, "queue"),
                ("a comment after a closed flow", "other: {a: 1} # note\n" + tail, "queue"),
                ("a flow over deeper lines", "toolsets: [web,\n  cli]\n" + tail, "queue"),
                ("a flow with a comment over deeper lines", "toolsets: [web, # first\n  cli]\n" + tail, "queue"),
                ("a quoted scalar over deeper lines", "note: \"a\n\n  # b\"\n" + tail, "queue"),
                ("an anchored closed flow", "other: &x [1]\n" + tail, "queue"),
                ("a block scalar of a neighbour", "other: >-\n  text [\n" + tail, "queue"),
                ("a neighbour beside the key", "display:\n  other: [a, {b: c}]\n  busy_input_mode: queue\n", "queue"),
                # Not read: no YAML, or a value that runs on into the mapping.
                ("a bracket closed by the other kind", "other: [x}\n" + tail, None),
                ("brackets crossed", "other: {a: [1}]\n" + tail, None),
                ("a comment that hides the closing bracket", "stray: {a: 1 # note}\n" + tail, None),
                ("an open flow at the key's indentation", "stray: [\n" + tail, None),
                ("an open flow beside the key", "display:\n  stray: {a: 1\n  busy_input_mode: queue\n", None),
                ("an open single quote at the key's indentation", "stray: 'open\n" + tail, None),
                ("an open double quote beside the key", "display:\n  stray: \"open\n  busy_input_mode: queue\n",
                 None),
                ("text after a closed flow", "display:\n  stray: [a] b\n  busy_input_mode: queue\n", None),
                ("text after a quoted scalar", "stray: 'a' b\n" + tail, None),
                ("a block scalar header of no form", "display:\n  stray: |x\n  busy_input_mode: queue\n", None)):
            with self.subTest(case=name):
                if expected is None:
                    self.assert_not_read(text, key)
                else:
                    self.assert_read(text, key, expected)

    def test_a_plain_scalar_yaml_1_1_reads_as_another_type_gives_no_value(self):
        # Booleans by y/n, octal, hexadecimal, sexagesimal, floats, dates.
        for word in ("y", "N", "010", "0x1A", "1:30", "1.5", ".inf", "2026-11-27", "+1.0e3"):
            with self.subTest(word=word):
                self.assert_not_read("display:\n  busy_input_mode: %s\n" % word, "display.busy_input_mode")

    def test_a_setting_condition_holds_only_on_a_read_value(self):
        # The method condition: a value read and equal holds; a form not
        # read does not hold, even where a full reader would see the value.
        path = os.path.join(self.root, "config.yaml")
        cond = {"path": "{env:HERMES_HOME}/config.yaml", "format": "yaml-scalar",
                "key": "display.busy_input_mode", "equals": "queue"}
        for text, holds in ((HERMES_WRITTEN, True), (HAND_WRITTEN, True),
                            ("display:\n  busy_input_mode: interrupt\n", False),
                            ("display: {busy_input_mode: queue}\n", False)):
            with self.subTest(text=text[:40]):
                with open(path, "w", encoding="utf-8", newline="") as fh:
                    fh.write(text)
                self.assertEqual(RUNNER.setting_holds(cond, self.env), holds)

    def test_a_file_that_is_not_utf8_gives_no_value(self):
        with open(os.path.join(self.root, "config.yaml"), "wb") as fh:
            fh.write(b"display:\n  busy_input_mode: qu\xffeue\n")
        cond = {"path": "{env:HERMES_HOME}/config.yaml", "format": "yaml-scalar",
                "key": "display.busy_input_mode", "equals": "queue"}
        self.assertIs(RUNNER.read_setting(cond, self.env), RUNNER._MISSING)


if __name__ == "__main__":
    unittest.main()
