# --- Windows security descriptors (P-9) --------------------------------------
# One reading of a Windows security descriptor for the two criteria of P-9,
# shared by the Windows branch of `prepare` (served as its first part) and
# the Windows platform layer of the runner. A descriptor is read into a
# SecView: whether the object is a reparse point, its owner SID, its DACL as
# a list of (type, flags, mask, sid) ACEs (None: a NULL DACL, which grants
# everyone everything), the final path of the handle, whether the DACL is
# protected from inheritance and the file attributes. Everything after the reading is plain Python,
# so the tests hand in a fake source of such views.
#
# Comments, not docstrings: the served prepare loses full-line comments, and
# its size counts against every Windows response (SC-009).
#
# The text parses under Python 2.7 (the prepare that carries it does).

import ctypes
import ntpath

SID_WORLD = "S-1-1-0"
SID_SYSTEM = "S-1-5-18"
SID_ADMINS = "S-1-5-32-544"
SID_CREATOR_OWNER = "S-1-3-0"
SID_TRUSTED_INSTALLER = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

ACE_ALLOW = 0
ACE_DENY = 1
ACE_INHERIT_ONLY = 0x08

FILE_DELETE_CHILD = 0x40
DELETE = 0x10000
WRITE_DAC = 0x40000
WRITE_OWNER = 0x80000

# The generic mapping of files: GENERIC_READ, _WRITE, _EXECUTE and _ALL to
# FILE_GENERIC_READ, _WRITE, _EXECUTE and FILE_ALL_ACCESS.
_FILE_GENERIC = ((0x80000000, 0x120089), (0x40000000, 0x120116),
                 (0x20000000, 0x1200A0), (0x10000000, 0x1F01FF))


FILE_ATTRIBUTE_DIRECTORY = 0x10
FILE_ATTRIBUTE_REPARSE_POINT = 0x400


class SecView(object):
    def __init__(self, reparse, owner, dacl, final, protected=False, attributes=0):
        self.reparse = reparse
        self.owner = owner
        self.dacl = dacl
        self.final = final
        self.protected = protected
        self.attributes = attributes


# Criterion (a) of P-9, the term "private Windows directory": the cause of
# a refusal as a PlatformRefusal reason, or None. A directory has a DACL
# protected from inheritance; a file inside a private directory is judged
# by its own DACL, read from the handle that is then read. Allow ACEs name
# only the user, SYSTEM and the administrators; a Creator Owner ACE is
# allowed only as inherit-only; any other allow ACE, read-only too, refuses.
# A NULL DACL and an ACE of an unknown type refuse; deny ACEs take nothing
# away from the criterion. A reparse point is never followed. The owner is
# the user or a trusted system account (as in criterion (b)): an owner has
# READ_CONTROL and WRITE_DAC whatever the DACL says, so another owner could
# open the object to others (decision of the lead on OST-2-7).
def private_refusal(view, me, directory):
    if view.reparse:
        return "symlink"
    if view.owner not in (me, SID_SYSTEM, SID_ADMINS, SID_TRUSTED_INSTALLER):
        return "foreign_owner"
    if view.dacl is None or (directory and not view.protected):
        return "open_to_others"
    for kind, flags, mask, sid in view.dacl:
        if kind == ACE_DENY:
            continue
        if kind != ACE_ALLOW:
            return "open_to_others"
        if sid in (me, SID_SYSTEM, SID_ADMINS):
            continue
        if sid == SID_CREATOR_OWNER and flags & ACE_INHERIT_ONLY:
            continue
        return "open_to_others"
    return None


def secdesc_map_generic(mask):
    for generic, specific in _FILE_GENERIC:
        if mask & generic:
            mask = (mask & ~generic) | specific
    return mask


# The rights the DACL grants the account sid, walked in its order the way
# the access check of Windows walks it: an inherit-only ACE does not apply,
# an ACE applies to sid when it names sid or Everyone, a deny ACE takes the
# bits not granted yet, an allow ACE gives the bits not denied yet. An ACE
# of another type than allow or deny grants nothing here; chain_refusal
# refuses such an ACE of an untrusted account instead.
def secdesc_effective(dacl, sid):
    granted = denied = 0
    for kind, flags, mask, ace_sid in dacl:
        if flags & ACE_INHERIT_ONLY or ace_sid not in (sid, SID_WORLD):
            continue
        mask = secdesc_map_generic(mask)
        if kind == ACE_DENY:
            denied |= mask & ~granted
        elif kind == ACE_ALLOW:
            granted |= mask & ~denied
    return granted


def _norm_path(path):
    if path.startswith("\\\\?\\"):
        path = path[4:]
    return ntpath.normcase(ntpath.normpath(path))


# Components of a path from the root of its drive down to the path itself.
def path_components(path):
    out = []
    while True:
        out.append(path)
        parent = ntpath.dirname(path)
        if parent == path:
            return out[::-1]
        path = parent


# Criterion (b) of P-9 (US-11, "Before the token is written") over the views of every
# component, root first, each with the path it was opened by: the cause of
# a refusal, or None when no account but the user and the trusted system
# accounts can replace a component. A component is replaced by deleting or
# renaming it (DELETE on it, FILE_DELETE_CHILD on its parent) or by changing
# its rights or owner (WRITE_DAC, WRITE_OWNER); creating children
# (FILE_ADD_*) replaces nothing. An empty DACL grants nothing; a NULL DACL
# grants everything.
# A cause names the component by its role, never by its path: the line
# that carries it does not repeat the path (INV-17).
def _component_role(i, last):
    if i == last:
        return "the token directory"
    if i == last - 1:
        return "the parent of the token directory"
    return "component %d of the path from its root" % (i + 1)


def chain_refusal(views, me):
    trusted = (me, SID_SYSTEM, SID_ADMINS, SID_TRUSTED_INSTALLER)
    last = len(views) - 1
    for i, (path, view) in enumerate(views):
        role = _component_role(i, last)
        if view.reparse:
            return "a reparse point: " + role
        if _norm_path(view.final) != _norm_path(path):
            return "it resolves elsewhere: " + role
        if view.owner not in trusted:
            return "owned by another account: " + role
        if view.dacl is None:
            return "no DACL: " + role
        others = set([SID_WORLD])
        for kind, flags, mask, sid in view.dacl:
            if flags & ACE_INHERIT_ONLY or sid in trusted:
                continue
            if kind not in (ACE_ALLOW, ACE_DENY):
                return "an access entry of an unknown type: " + role
            others.add(sid)
        wanted = DELETE | WRITE_DAC | WRITE_OWNER
        if i < last:
            # The next component is its child.
            wanted |= FILE_DELETE_CHILD
        for sid in sorted(others):
            if secdesc_effective(view.dacl, sid) & wanted:
                return "another account may replace it: " + role
    return None


class _SecurityAttributes(ctypes.Structure):
    _fields_ = [("length", ctypes.c_uint32), ("descriptor", ctypes.c_void_p), ("inherit", ctypes.c_int)]


# The system source of views and private directories (ctypes, stdlib only).
# Handles are opened without following a reparse point and without
# FILE_SHARE_DELETE, so nobody deletes or renames a component while one is
# held; a caller keeps them until it no longer needs the path.
class WinSecurity(object):
    def __init__(self):
        k = self._k = ctypes.WinDLL("kernel32", use_last_error=True)
        a = self._a = ctypes.WinDLL("advapi32", use_last_error=True)
        P, D, H, B = ctypes.c_void_p, ctypes.c_uint32, ctypes.c_void_p, ctypes.c_int
        W, PP = ctypes.c_wchar_p, ctypes.POINTER(ctypes.c_void_p)
        for fn, args, res in (
                (k.CreateFileW, (W, D, D, P, D, D, H), H),
                (k.CloseHandle, (H,), B),
                (k.GetFileInformationByHandle, (H, P), B),
                (k.GetFinalPathNameByHandleW, (H, W, D, D), D),
                (k.CreateDirectoryW, (W, P), B),
                (k.LocalFree, (P,), P),
                (k.GetCurrentProcess, (), H),
                (a.OpenProcessToken, (H, D, PP), B),
                (a.GetTokenInformation, (H, B, P, D, ctypes.POINTER(D)), B),
                (a.GetSecurityInfo, (H, B, D, PP, PP, PP, PP, PP), D),
                (a.GetAce, (P, D, PP), B),
                (a.ConvertSidToStringSidW, (P, PP), B),
                (a.ConvertStringSecurityDescriptorToSecurityDescriptorW, (W, D, PP, P), B)):
            fn.argtypes, fn.restype = args, res

    def _fail(self, what):
        err = ctypes.get_last_error()
        raise OSError(err, "%s failed (Windows error %d)" % (what, err))

    def _sid(self, psid):
        out = ctypes.c_void_p()
        if not self._a.ConvertSidToStringSidW(psid, ctypes.byref(out)):
            self._fail("ConvertSidToStringSidW")
        try:
            return ctypes.wstring_at(out.value)
        finally:
            self._k.LocalFree(out)

    # The SID of the user of this process (TokenUser).
    def me(self):
        token = ctypes.c_void_p()
        if not self._a.OpenProcessToken(self._k.GetCurrentProcess(), 0x0008, ctypes.byref(token)):
            self._fail("OpenProcessToken")
        try:
            size = ctypes.c_uint32()
            self._a.GetTokenInformation(token, 1, None, 0, ctypes.byref(size))
            buf = ctypes.create_string_buffer(size.value)
            if not self._a.GetTokenInformation(token, 1, buf, size, ctypes.byref(size)):
                self._fail("GetTokenInformation")
            return self._sid(ctypes.c_void_p.from_buffer(buf).value)
        finally:
            self._k.CloseHandle(token)

    # Create path as a private directory of the user in one call: a DACL
    # protected from inheritance that allows the user, SYSTEM and the
    # administrators (P-9 (a)). An existing path is FileExistsError.
    def make_private_dir(self, path):
        sd = ctypes.c_void_p()
        sddl = "D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" % self.me()
        if not self._a.ConvertStringSecurityDescriptorToSecurityDescriptorW(sddl, 1, ctypes.byref(sd), None):
            self._fail("ConvertStringSecurityDescriptorToSecurityDescriptorW")
        try:
            sa = _SecurityAttributes(ctypes.sizeof(_SecurityAttributes), sd.value, 0)
            if not self._k.CreateDirectoryW(path, ctypes.byref(sa)):
                if ctypes.get_last_error() == 183:  # ERROR_ALREADY_EXISTS
                    raise FileExistsError(path)
                self._fail("CreateDirectoryW")
        finally:
            self._k.LocalFree(sd)

    # A handle of an existing file or directory: READ_CONTROL and
    # FILE_READ_ATTRIBUTES, and GENERIC_READ when data is asked for; shared
    # for reading and writing, not for deletion; a reparse point is opened
    # itself, not followed (FILE_FLAG_OPEN_REPARSE_POINT), a directory too
    # (FILE_FLAG_BACKUP_SEMANTICS).
    def open(self, path, data=False):
        access = 0x20080 | (0x80000000 if data else 0)
        # A file opened to read its data shares deletion (FILE_SHARE_DELETE):
        # a writer of the monitor replaces it by os.replace, which a handle
        # without that share would make fail; the reader keeps the content
        # it opened.
        share = 0x7 if data else 0x3
        h = self._k.CreateFileW(path, access, share, None, 3, 0x02200000, None)
        if h is None or h == ctypes.c_void_p(-1).value:
            self._fail("CreateFileW")
        return h

    def close(self, h):
        self._k.CloseHandle(h)

    def view(self, h):
        info = ctypes.create_string_buffer(52)
        if not self._k.GetFileInformationByHandle(h, info):
            self._fail("GetFileInformationByHandle")
        attributes = ctypes.c_uint32.from_buffer(info).value
        reparse = bool(attributes & FILE_ATTRIBUTE_REPARSE_POINT)
        buf = ctypes.create_unicode_buffer(32768)
        if not self._k.GetFinalPathNameByHandleW(h, buf, 32768, 0):
            self._fail("GetFinalPathNameByHandleW")
        owner, dacl, sd = ctypes.c_void_p(), ctypes.c_void_p(), ctypes.c_void_p()
        # SE_FILE_OBJECT; OWNER_ and DACL_SECURITY_INFORMATION.
        err = self._a.GetSecurityInfo(h, 1, 0x5, ctypes.byref(owner), None, ctypes.byref(dacl), None,
                                      ctypes.byref(sd))
        if err:
            raise OSError(err, "GetSecurityInfo failed (Windows error %d)" % err)
        try:
            control = ctypes.c_uint16.from_address(sd.value + 2).value
            return SecView(reparse, self._sid(owner) if owner.value else None,
                           self._aces(dacl.value) if dacl.value else None, buf.value,
                           bool(control & 0x1000), attributes)
        finally:
            self._k.LocalFree(sd)

    def _aces(self, acl):
        out = []
        for i in range(ctypes.c_uint16.from_address(acl + 4).value):
            ace = ctypes.c_void_p()
            if not self._a.GetAce(acl, i, ctypes.byref(ace)):
                self._fail("GetAce")
            kind = ctypes.c_uint8.from_address(ace.value).value
            flags = ctypes.c_uint8.from_address(ace.value + 1).value
            mask = ctypes.c_uint32.from_address(ace.value + 4).value
            # Allow and deny ACEs carry the SID after the mask; others are
            # kept without one.
            sid = self._sid(ace.value + 8) if kind in (ACE_ALLOW, ACE_DENY) else None
            out.append((kind, flags, mask, sid))
        return out
