"""A fake source of Windows security descriptors for the monitor tests: the
views the runner's security part (winsec.py, P-9) reads from handles,
answered from a table instead of the system.

A view has the fields of winsec.SecView: reparse, owner (a SID string or
None), dacl (a list of (type, flags, mask, sid) ACEs, or None for a NULL
DACL), final (the final path of the handle) and protected (the DACL is
protected from inheritance). A path not in the table gets a trusted view:
owned by SYSTEM, a DACL that allows SYSTEM, the administrators and the
user, the final path equal to the path it was opened by.

make_private_dir creates a real directory (the prepare under test writes
starter.py into it) and gives it the view of a private directory of the
user. Every open, close and failure is recorded, so a test sees which
handles were still open at a given moment.

Standard library of Python 3.6 only.
"""

import os

ME = "S-1-5-21-1111111111-2222222222-3333333333-1001"
OTHER = "S-1-5-21-1111111111-2222222222-3333333333-1002"
SYSTEM = "S-1-5-18"
ADMINS = "S-1-5-32-544"
USERS = "S-1-5-32-545"
AUTHENTICATED = "S-1-5-11"
EVERYONE = "S-1-1-0"
CREATOR_OWNER = "S-1-3-0"
TRUSTED_INSTALLER = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

ALLOW = 0
DENY = 1
OBJECT_INHERIT = 0x01
CONTAINER_INHERIT = 0x02
INHERIT_ONLY = 0x08

FILE_ADD_FILE = 0x2
FILE_ADD_SUBDIRECTORY = 0x4
FILE_DELETE_CHILD = 0x40
DELETE = 0x10000
WRITE_DAC = 0x40000
WRITE_OWNER = 0x80000
FILE_ALL_ACCESS = 0x1F01FF
FILE_GENERIC_READ = 0x120089
GENERIC_ALL = 0x10000000
GENERIC_WRITE = 0x40000000


FILE_ATTRIBUTE_DIRECTORY = 0x10


class View(object):
    def __init__(self, owner=SYSTEM, dacl=None, final=None, reparse=False, protected=False,
                 null_dacl=False, attributes=None):
        self.owner = owner
        self.dacl = None if null_dacl else (list(dacl) if dacl is not None else trusted_dacl())
        self.final = final
        self.reparse = reparse
        self.protected = protected
        # None: taken from what lies at the path (a directory or not).
        self.attributes = attributes


def trusted_dacl():
    return [(ALLOW, 0, FILE_ALL_ACCESS, SYSTEM), (ALLOW, 0, FILE_ALL_ACCESS, ADMINS),
            (ALLOW, 0, FILE_ALL_ACCESS, ME)]


def private_dacl():
    inherit = OBJECT_INHERIT | CONTAINER_INHERIT
    return [(ALLOW, inherit, FILE_ALL_ACCESS, ME), (ALLOW, inherit, FILE_ALL_ACCESS, SYSTEM),
            (ALLOW, inherit, FILE_ALL_ACCESS, ADMINS)]


class Handle(object):
    def __init__(self, path, number):
        self.path = path
        self.number = number


class FakeSecurity(object):
    """views: {path: View}; fail_open / fail_view: paths whose open or view
    raises OSError; me: the SID of the user."""

    def __init__(self, views=None, fail_open=(), fail_view=(), me=ME):
        self.views = dict(views or {})
        self.fail_open = set(fail_open)
        self.fail_view = set(fail_view)
        self._me = me
        self.opened = []
        self.closed = []
        self.made = []

    def me(self):
        return self._me

    def make_private_dir(self, path):
        os.mkdir(path, 0o700)
        self.made.append(path)
        self.views.setdefault(path, View(owner=self._me, dacl=private_dacl(), protected=True))

    def open(self, path, data=False):
        if path in self.fail_open:
            raise OSError(5, "Access is denied")
        handle = Handle(path, len(self.opened))
        self.opened.append(handle)
        return handle

    def close(self, handle):
        self.closed.append(handle)

    def view(self, handle):
        if handle.path in self.fail_view:
            raise OSError(1336, "The access control list structure is invalid")
        view = self.views.get(handle.path) or View()
        attributes = view.attributes
        if attributes is None:
            attributes = FILE_ATTRIBUTE_DIRECTORY if os.path.isdir(handle.path) else 0
        return View(owner=view.owner, dacl=view.dacl, final=view.final or handle.path, reparse=view.reparse,
                    protected=view.protected, null_dacl=view.dacl is None, attributes=attributes)

    def open_handles(self):
        return [h for h in self.opened if h not in self.closed]
