# Skein monitor loader, client mode. `prepare` writes this file into the
# private token directory; `preflight` and the working command run it with
# python3. The marker below reaches the runner body through exec: it is what
# makes the runner read SKEIN_CLIENT and the other client-mode settings. A
# body without it runs as the bootstrap of a call without `client`.
_SKEIN_LOADER_MODE = "client"
