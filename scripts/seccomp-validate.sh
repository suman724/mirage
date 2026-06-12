#!/usr/bin/env bash
# Shimmer S3′ validation (docs/design-shimmer.md §3.3): the seccomp
# user-notification interception path, end to end, UNPRIVILEGED.
#
#   harness chunks a fixture → builds the skeleton → starts the seccomp
#   supervisor → spawns the C launcher (installs the filter) → execs a workload.
#   The workload reads workspace files; each open traps to the supervisor, which
#   materializes the file and lets the open continue against real content.
#
# The headline check: a STATIC Go binary (raw syscalls, no libc — the case the
# LD_PRELOAD shim structurally cannot intercept) reads correct content.
#
# Runs with NO capabilities and NO devices — the Fargate property (G4).
set -euo pipefail

FIX=/tmp/fix
ROOT=/tmp/ws
BIN=/tmp/bin
H="$BIN/mirage-seccomp-harness"

log()  { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; [ -f /tmp/run.err ] && { echo '--- harness stderr ---' >&2; cat /tmp/run.err >&2; }; exit 1; }

# run <root> <outfile> -- <cmd...> : project a fresh skeleton and run the
# workload under seccomp interception. Workload stdout -> outfile, harness +
# workload stderr -> /tmp/run.err.
run() {
    local root="$1" outf="$2"; shift 2
    [ "$1" = "--" ] && shift
    rm -rf "$root"
    "$H" --src "$FIX" --root "$root" --launcher "$BIN/mirage-launcher" --log-level error \
        -- "$@" >"$outf" 2>/tmp/run.err
}

stats() { grep -o 'SECCOMP_STATS.*' /tmp/run.err || echo "SECCOMP_STATS (none)"; }

log "build harness, launcher, and a STATIC Go reader (raw syscalls, bypasses libc)"
mkdir -p "$BIN"
go build -o "$H" ./cmd/mirage-seccomp-harness
gcc -O2 -Wall -Wextra -Werror -o "$BIN/mirage-launcher" shim/launcher.c
cat > /tmp/static-reader.go <<'GO'
package main

import "os"

// Reads argv[1] and copies it to stdout. Built CGO_ENABLED=0 => static, and
// like every Go binary it issues raw openat syscalls (no libc) — the exact
// case an LD_PRELOAD shim misses and seccomp must catch.
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
python3 -c "open('$FIX/sub/deep/big.bin','wb').write(bytes((i%251)+1 for i in range(300*1024)))"

log "HEADLINE: a static Go binary reads a workspace file via seccomp"
run "$ROOT" /tmp/out.bin -- "$BIN/static-reader" "$ROOT/sub/deep/big.bin"
cmp /tmp/out.bin "$FIX/sub/deep/big.bin" || fail "static Go binary read wrong content"
grep -q 'errors=0' /tmp/run.err || fail "supervisor reported errors: $(stats)"
echo "  static Go read OK — $(stats)"

log "laziness: only the opened file materialized; siblings stay sparse"
[ "$(stat -c %b "$ROOT/sub/deep/big.bin")" -gt 0 ] || fail "opened file not materialized"
[ "$(stat -c %b "$ROOT/alpha.txt")" -eq 0 ] || fail "unopened file lost sparseness"

log "libc tool: cat"
run "$ROOT" /tmp/out.txt -- cat "$ROOT/alpha.txt"
cmp /tmp/out.txt "$FIX/alpha.txt" || fail "cat read wrong content"

log "python3 (fopen/openat path)"
run "$ROOT" /tmp/out.py -- python3 -c "import sys; sys.stdout.buffer.write(open('$ROOT/sub/beta.txt','rb').read())"
cmp /tmp/out.py "$FIX/sub/beta.txt" || fail "python read wrong content"

log "grep -R: full-tree recursive read, correct content (2 text files match; big.bin is binary)"
run "$ROOT" /tmp/out.grep -- grep -rl MIRAGE_SENTINEL "$ROOT"
[ "$(wc -l < /tmp/out.grep)" -eq 2 ] || fail "grep -R found $(wc -l < /tmp/out.grep) sentinel files, want 2"
echo "  grep -R OK — $(stats)"

log "no NUL bytes were ever observed in any workload output"
for f in /tmp/out.bin /tmp/out.txt /tmp/out.py; do
    # big.bin is binary-but-nonzero by construction; text outputs must have no NUL
    case "$f" in */out.bin) continue;; esac
    if grep -qP '\x00' "$f"; then fail "NUL byte in $f (placeholder leak)"; fi
done

log "rough per-trap latency (grep -R wall time)"
start=$(date +%s%N)
run "$ROOT" /tmp/out.grep2 -- grep -rl MIRAGE_SENTINEL "$ROOT"
end=$(date +%s%N)
echo "  (indicative only; in-memory store, not network) grep -R wall=$(( (end - start) / 1000000 ))ms — $(stats)"

printf '\nSECCOMP-VALIDATE: ALL CHECKS PASSED\n'
