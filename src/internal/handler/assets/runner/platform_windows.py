# --- platform layer: Windows ------------------------------------------------
# Not implemented yet: every operation is refused (unsupported) before it
# touches anything, so no caller on Windows can mistake a missing check for a
# passed one. The implementation (private directories and files by DACL, the
# lock through msvcrt, the process tree through ctypes, the detached start)
# replaces this part.


class WindowsPlatform(object):
    """The Windows implementation of the platform layer: refuses everything."""

    @staticmethod
    def make_private_dir(path):
        raise PlatformRefusal("unsupported", path)

    @staticmethod
    def ensure_private_dir(path):
        raise PlatformRefusal("unsupported", path)

    @staticmethod
    def read_token(path):
        raise PlatformRefusal("unsupported", path)

    @staticmethod
    def read_own_file(path):
        raise PlatformRefusal("unsupported", path)

    @staticmethod
    def atomic_write(path, data):
        raise PlatformRefusal("unsupported", path)

    @staticmethod
    def acquire_runner_lock(token_path, wait_sec=0, env=None):
        raise PlatformRefusal("unsupported", token_path)

    @staticmethod
    def process_info(pid):
        raise PlatformRefusal("unsupported", "process_info")

    @staticmethod
    def spawn_detached(argv, stdin_data, env, receipt_timeout_sec):
        raise PlatformRefusal("unsupported", "spawn_detached")
