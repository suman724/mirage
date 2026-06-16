#!/usr/bin/env bash
# mirage_trace (Python) validation: the orchestrator-side helper installs the
# RET_TRACE filter itself, then reads a workspace file — and the Go tracer
# materializes it. This proves the production CLI path (design §4.1): the
# orchestrator calls mirage_trace.enable() and Mirage handles the rest.
#
#   mirage-ptrace-harness --no-spawn   (tracer side: skeleton + attach socket)
#        ▲ ATTACH <pid> / OK
#   python3 -c 'mirage_trace.enable(sock); read(workspace_file)'
#        (an INDEPENDENT process: seize of a non-descendant => CAP_SYS_PTRACE)
#
# Exercises the dependency-free ctypes fallback (the golang image has no
# python3-seccomp), which builds the BPF by hand for the host arch.
set -euo pipefail

FIX=/tmp/fix
ROOT=/tmp/ws
BIN=/tmp/bin
H="$BIN/mirage-ptrace-harness"
SOCK=/tmp/attach.sock
PYDIR="$PWD/python"

log()  { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; echo '--- harness stderr ---' >&2; cat /tmp/run.err >&2 2>/dev/null; echo '--- python stderr ---' >&2; cat /tmp/py.err >&2 2>/dev/null; exit 1; }

log "build the ptrace harness; create a sentinel fixture"
mkdir -p "$BIN"
go build -o "$H" ./cmd/mirage-ptrace-harness
rm -rf "$FIX" "$ROOT"
mkdir -p "$FIX/sub"
printf 'MIRAGE_SENTINEL alpha\n' > "$FIX/alpha.txt"
python3 -c "open('$FIX/big.bin','wb').write(bytes((i%251)+1 for i in range(200*1024)))"

log "import smoke-test: mirage_trace loads and exposes enable()"
PYTHONPATH="$PYDIR" python3 -c "import mirage_trace; assert hasattr(mirage_trace,'enable'); print('  mirage_trace import OK')"

log "start the tracer (--no-spawn: waits for an external process to attach)"
rm -f "$SOCK"
"$H" --src "$FIX" --root "$ROOT" --no-spawn --attach-sock "$SOCK" --log-level error 2>/tmp/run.err &
HPID=$!
trap 'kill $HPID 2>/dev/null || true' EXIT

log "wait for the attach socket"
ready=""
for _ in $(seq 1 100); do
    [ -S "$SOCK" ] && { ready=1; break; }
    kill -0 $HPID 2>/dev/null || break
    sleep 0.1
done
[ -n "$ready" ] || fail "attach socket $SOCK never appeared"

log "orchestrator path: python calls mirage_trace.enable(), then reads a workspace file"
MIRAGE_ATTACH_SOCK="$SOCK" PYTHONPATH="$PYDIR" python3 - "$ROOT/big.bin" <<'PY' > /tmp/out.bin 2>/tmp/py.err
import os, sys, mirage_trace
mirage_trace.enable(os.environ["MIRAGE_ATTACH_SOCK"])
sys.stderr.write("filter installed (enabled=%s)\n" % mirage_trace.is_enabled())
with open(sys.argv[1], "rb") as f:
    sys.stdout.buffer.write(f.read())
PY

log "wait for the tracer to finish (python exited => root tracee gone)"
wait "$HPID" || fail "tracer exited non-zero"

log "assert: the file read AFTER enable() returned real materialized content"
cmp /tmp/out.bin "$FIX/big.bin" || fail "python read wrong content (materialize via mirage_trace failed)"
grep -q 'errors=0' /tmp/run.err || fail "tracer reported errors: $(grep -o 'PTRACE_STATS.*' /tmp/run.err)"
grep -q 'filter installed' /tmp/py.err || fail "mirage_trace.enable() did not confirm install"
echo "  $(grep -o 'PTRACE_STATS.*' /tmp/run.err)"

printf '\nMIRAGE-TRACE-VALIDATE: ALL CHECKS PASSED\n'
