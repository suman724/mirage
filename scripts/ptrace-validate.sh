#!/usr/bin/env bash
# Ptrace interception validation (docs/design-ptrace-interception.md §4/§5/§7):
# the ptrace + seccomp-RET_TRACE front-end, end to end.
#
#   harness chunks a fixture → builds the skeleton → starts the ptrace tracer on
#   an attach socket → spawns the C trace-launcher, which requests attach, lets
#   the tracer PTRACE_SEIZE it, installs a RET_TRACE filter (open+exec family),
#   then execs a workload. Each trapped syscall raises a PTRACE_EVENT_SECCOMP
#   stop the tracer services: read path → materialize → resume the syscall.
#
# Headline checks:
#   (1) a STATIC Go binary (raw syscalls, no libc) reads correct content via the
#       open trap — the case LD_PRELOAD can't catch.
#   (2) EXECUTING a workspace file is intercepted via the exec trap — executing a
#       file is NOT an open, so this proves the exec-family coverage (design §6).
#
# In the harness the tracer is the launcher's real parent, so PTRACE_SEIZE works
# without CAP_SYS_PTRACE; production side-attach to a non-descendant needs the
# cap (the Makefile target adds it to mirror production).
set -euo pipefail

FIX=/tmp/fix
ROOT=/tmp/ws
BIN=/tmp/bin
H="$BIN/mirage-ptrace-harness"

log()  { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; [ -f /tmp/run.err ] && { echo '--- harness stderr ---' >&2; cat /tmp/run.err >&2; }; exit 1; }

# run <root> <outfile> -- <cmd...> : project a fresh skeleton and run the
# workload under ptrace interception. Workload stdout -> outfile, harness +
# workload stderr -> /tmp/run.err.
run() {
    local root="$1" outf="$2"; shift 2
    [ "$1" = "--" ] && shift
    rm -rf "$root"
    "$H" --src "$FIX" --root "$root" --launcher "$BIN/mirage-trace-launcher" --log-level error \
        -- "$@" >"$outf" 2>/tmp/run.err
}

stats() { grep -o 'PTRACE_STATS.*' /tmp/run.err || echo "PTRACE_STATS (none)"; }

log "build harness, trace-launcher, and a STATIC Go reader (raw syscalls, bypasses libc)"
mkdir -p "$BIN"
go build -o "$H" ./cmd/mirage-ptrace-harness
gcc -O2 -Wall -Wextra -Werror -o "$BIN/mirage-trace-launcher" shim/trace-launcher.c
cat > /tmp/static-reader.go <<'GO'
package main

import "os"

// Reads argv[1] and copies it to stdout. Built CGO_ENABLED=0 => static, and
// like every Go binary it issues raw openat syscalls (no libc) — the exact
// case an LD_PRELOAD shim misses and the trap must catch.
func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Stdout.Write(b)
}
GO
( cd /tmp && CGO_ENABLED=0 go build -o "$BIN/static-reader" static-reader.go )
if ldd "$BIN/static-reader" 2>&1 | grep -q 'not a dynamic executable'; then
    echo "  confirmed statically linked (ldd)"
else
    echo "  (ldd inconclusive; CGO_ENABLED=0 Go binary is static and uses raw syscalls regardless)"
fi

log "create sentinel fixture (no zero bytes => any placeholder leak is detectable)"
rm -rf "$FIX"
mkdir -p "$FIX/sub/deep"
printf 'MIRAGE_SENTINEL alpha\n' > "$FIX/alpha.txt"
printf 'MIRAGE_SENTINEL beta\n'  > "$FIX/sub/beta.txt"
printf '#!/bin/sh\necho MIRAGE_EXEC_OK\n' > "$FIX/run.sh"
chmod +x "$FIX/run.sh"
python3 -c "open('$FIX/sub/deep/big.bin','wb').write(bytes((i%251)+1 for i in range(300*1024)))"

log "HEADLINE 1: a static Go binary reads a workspace file via the open trap"
run "$ROOT" /tmp/out.bin -- "$BIN/static-reader" "$ROOT/sub/deep/big.bin"
cmp /tmp/out.bin "$FIX/sub/deep/big.bin" || fail "static Go binary read wrong content"
grep -q 'errors=0' /tmp/run.err || fail "tracer reported errors: $(stats)"
echo "  static Go read OK — $(stats)"

log "HEADLINE 2: EXECUTING a workspace file is intercepted (exec trap, not an open)"
run "$ROOT" /tmp/out.exec -- "$ROOT/run.sh"
grep -q 'MIRAGE_EXEC_OK' /tmp/out.exec || fail "executing workspace script failed: $(cat /tmp/out.exec 2>/dev/null)"
grep -q 'errors=0' /tmp/run.err || fail "tracer reported errors on exec: $(stats)"
echo "  exec of workspace file OK — $(stats)"

log "laziness: only the opened file materialized; siblings stay sparse"
run "$ROOT" /tmp/out.bin -- "$BIN/static-reader" "$ROOT/sub/deep/big.bin"
[ "$(stat -c %b "$ROOT/sub/deep/big.bin")" -gt 0 ] || fail "opened file not materialized"
[ "$(stat -c %b "$ROOT/alpha.txt")" -eq 0 ] || fail "unopened file lost sparseness"

log "libc tool: cat"
run "$ROOT" /tmp/out.txt -- cat "$ROOT/alpha.txt"
cmp /tmp/out.txt "$FIX/alpha.txt" || fail "cat read wrong content"

log "python3 (fopen/openat path)"
run "$ROOT" /tmp/out.py -- python3 -c "import sys; sys.stdout.buffer.write(open('$ROOT/sub/beta.txt','rb').read())"
cmp /tmp/out.py "$FIX/sub/beta.txt" || fail "python read wrong content"

log "grep -R: full-tree recursive read, correct content (2 text files + run.sh match; big.bin is binary)"
run "$ROOT" /tmp/out.grep -- grep -rl MIRAGE_SENTINEL "$ROOT"
[ "$(wc -l < /tmp/out.grep)" -eq 2 ] || fail "grep -R found $(wc -l < /tmp/out.grep) sentinel files, want 2"
echo "  grep -R OK — $(stats)"

log "no NUL bytes were ever observed in any text workload output"
for f in /tmp/out.txt /tmp/out.py /tmp/out.exec; do
    if grep -qP '\x00' "$f"; then fail "NUL byte in $f (placeholder leak)"; fi
done

log "rough per-trap latency (grep -R wall time)"
start=$(date +%s%N)
run "$ROOT" /tmp/out.grep2 -- grep -rl MIRAGE_SENTINEL "$ROOT"
end=$(date +%s%N)
echo "  (indicative only; in-memory store, not network) grep -R wall=$(( (end - start) / 1000000 ))ms — $(stats)"

printf '\nPTRACE-VALIDATE: ALL CHECKS PASSED\n'
