"""CLI wrapper: enable Mirage ptrace interception, then exec a command under it.

    python -m mirage_trace <attach_sock> -- <cmd> [args...]

This is the non-Python-caller path (the analogue of shim/trace-launcher.c, but in
Python): it attaches, installs the RET_TRACE filter, then execs <cmd>, which
inherits the filter. With no command it just enables and exits (filter is lost
when the process exits, so that form is only useful for a smoke test).
"""

import os
import sys

from . import MirageTraceError, enable


def main(argv=None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv:
        print("usage: python -m mirage_trace <attach_sock> [-- <cmd> [args...]]", file=sys.stderr)
        return 2

    attach_sock = argv[0]
    rest = argv[1:]
    if rest and rest[0] == "--":
        rest = rest[1:]

    try:
        enable(attach_sock)
    except MirageTraceError as e:
        print("mirage_trace: %s" % e, file=sys.stderr)
        return 1

    if not rest:
        return 0
    try:
        os.execvp(rest[0], rest)
    except OSError as e:
        print("mirage_trace: exec %r failed: %s" % (rest[0], e), file=sys.stderr)
        return 127


if __name__ == "__main__":
    raise SystemExit(main())
