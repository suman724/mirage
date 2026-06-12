#!/usr/bin/env bash
# Shimmer S2 validation (docs/design-shimmer.md §9): the full Mirage loop with
# the C LD_PRELOAD shim driving lazy materialization for real libc tools.
#
#   client publishes a sentinel fixture ──gRPC──▶ server --shim /tmp/ws
#   tools (cat, grep -r, python3, node, sed -i, glob, find) run under
#   LD_PRELOAD=libmirageshim.so and must see byte-identical content, lazily.
#
# This script runs in a plain, UNPRIVILEGED container — no /dev/fuse, no
# CAP_SYS_ADMIN. That is the Fargate-shaped property Shimmer exists for (G4).
set -euo pipefail

ROOT=/tmp/ws
STATE=/tmp/shim-state
FIX=/tmp/fixture
SOCK=$STATE/shim.sock
BIN=/tmp/bin
ADDR=127.0.0.1:7777

log()  { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; [ -f /tmp/server.log ] && tail -40 /tmp/server.log >&2; exit 1; }

# req sends one supervisor request and prints the reply line.
req() {
    python3 - "$SOCK" "$1" <<'PY'
import socket, sys
s = socket.socket(socket.AF_UNIX)
s.settimeout(30)
s.connect(sys.argv[1])
s.sendall((sys.argv[2] + "\n").encode())
buf = b""
while not buf.endswith(b"\n"):
    chunk = s.recv(4096)
    if not chunk:
        break
    buf += chunk
print(buf.decode().strip())
PY
}

log "go tests (linux): shim package + end-to-end harness"
go test ./internal/fsutil/... ./server/shim/... >/dev/null || fail "shim unit tests"
go test -run TestShimHarness ./test/... >/dev/null || fail "Go e2e harness"

log "build server, client, and the C shim"
mkdir -p $BIN
go build -o $BIN/mirage-server ./server
go build -o $BIN/mirage-client ./client
gcc -shared -fPIC -O2 -Wall -Wextra -Werror -o /tmp/libmirageshim.so shim/mirageshim.c -ldl

log "create sentinel fixture (no zero bytes anywhere => zeros == placeholder leak)"
rm -rf "$FIX" "$ROOT" "$STATE"
mkdir -p $FIX/sub/deep
printf 'MIRAGE_SENTINEL alpha\n' > $FIX/alpha.txt
printf 'MIRAGE_SENTINEL beta\n'  > $FIX/sub/beta.txt
python3 -c "import sys; open('$FIX/sub/deep/big.bin','wb').write(bytes((i % 251) + 1 for i in range(300*1024)))"
printf 'echo MIRAGE_SENTINEL script-ran\n' > $FIX/run.sh && chmod 755 $FIX/run.sh
printf 'SECRET=never\n' > $FIX/.env   # secret: must never reach the server

log "start server (--shim, persistent state) and client"
$BIN/mirage-server --addr $ADDR --shim $ROOT --shim-state $STATE --log-level debug >/tmp/server.log 2>&1 &
SERVER_PID=$!
CLIENT_PID=""
trap 'kill $SERVER_PID $CLIENT_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 100); do grep -q "listening" /tmp/server.log 2>/dev/null && break; sleep 0.1; done
$BIN/mirage-client --addr $ADDR --dir $FIX >/tmp/client.log 2>&1 &
CLIENT_PID=$!
for _ in $(seq 1 100); do [ -S "$SOCK" ] && break; sleep 0.1; done
[ -S "$SOCK" ] || fail "supervisor socket never appeared"

log "skeleton: metadata is native and complete; nothing faulted yet"
[ "$(find $ROOT -type f | wc -l)" -eq 4 ] || fail "find sees $(find $ROOT -type f | wc -l) files, want 4"
[ ! -e $ROOT/.env ] || fail "secret .env was published to the server"
[ "$(stat -c %s $ROOT/sub/deep/big.bin)" -eq $((300*1024)) ] || fail "apparent size wrong"
[ "$(stat -c %a $ROOT/run.sh)" = "755" ] || fail "mode not projected"
[ "$(stat -c %b $ROOT/alpha.txt)" -eq 0 ] || fail "placeholder not sparse"
req STATS | grep -q "materialized=0 local=0" || fail "STATS not pristine: $(req STATS)"

log "control: WITHOUT the shim a placeholder reads zeros (the gap S2 closes)"
[ "$(tr -d '\0' < $ROOT/sub/beta.txt | wc -c)" -eq 0 ] || fail "placeholder unexpectedly real"

export LD_PRELOAD=/tmp/libmirageshim.so
export MIRAGE_SHIM_ROOT=$ROOT
export MIRAGE_SHIM_SOCK=$SOCK

log "cat: single file materializes lazily, byte-identical"
cmp <(cat $ROOT/alpha.txt) $FIX/alpha.txt || fail "cat content mismatch"
req STATS | grep -q "materialized=1 " || fail "want exactly 1 materialized: $(req STATS)"
req STATS | grep -q "chunk_requests=1" || fail "want exactly 1 chunk faulted: $(req STATS)"
[ "$(stat -c %b $ROOT/sub/deep/big.bin)" -eq 0 ] || fail "untouched file lost sparseness"

log "python3: read via io stack (fopen-family path)"
python3 -c "
import sys
a = open('$ROOT/sub/beta.txt', 'rb').read()
b = open('$FIX/sub/beta.txt', 'rb').read()
sys.exit(0 if a == b and b'\0' not in a else 1)
" || fail "python read mismatch (placeholder zeros?)"

log "node: large binary read, byte-identical, no zeros"
node -e "
const fs = require('fs');
const a = fs.readFileSync('$ROOT/sub/deep/big.bin');
const b = fs.readFileSync('$FIX/sub/deep/big.bin');
if (!a.equals(b)) process.exit(1);
if (a.includes(0)) process.exit(2);
" || fail "node read mismatch"

log "shell: glob + script source"
for f in $ROOT/*.txt; do grep -q MIRAGE_SENTINEL "$f" || fail "glob read $f"; done
bash $ROOT/run.sh | grep -q "MIRAGE_SENTINEL script-ran" || fail "script content wrong"

log "grep -r: full-tree recursive read (worst case for laziness, best for coverage)"
HITS=$(grep -rl MIRAGE_SENTINEL $ROOT | wc -l)
[ "$HITS" -eq 3 ] || fail "grep -r found $HITS sentinel files, want 3"

log "sed -i: in-place edit (rename-over pattern) then re-read"
sed -i 's/MIRAGE_SENTINEL alpha/MIRAGE_SENTINEL ALPHA-EDITED/' $ROOT/alpha.txt
grep -q "MIRAGE_SENTINEL ALPHA-EDITED" $ROOT/alpha.txt || fail "sed edit lost on re-read"

log "python3 append: open-for-write sends DIRTY; ENSURE never clobbers a local file"
python3 -c "open('$ROOT/sub/beta.txt', 'a').write('MIRAGE_SENTINEL appended\n')"
req STATS | grep -q "local=1" || fail "append did not mark the file local: $(req STATS)"
# A fresh reader's open() still ENSUREs first; local content must survive it.
grep -q "MIRAGE_SENTINEL appended" $ROOT/sub/beta.txt || fail "local append lost after re-read"
grep -q "MIRAGE_SENTINEL beta" $ROOT/sub/beta.txt || fail "original content lost by append"

log "no placeholder zeros were ever observed anywhere"
if grep -rqP '\x00' $ROOT 2>/dev/null; then fail "NUL bytes found in workspace reads"; fi

log "MATERIALIZE_ALL: full sync of placeholders; local files are NOT clobbered"
req MATERIALIZE_ALL | grep -q "^OK" || fail "MATERIALIZE_ALL: $(req STATS)"
cmp $ROOT/sub/deep/big.bin $FIX/sub/deep/big.bin || fail "big.bin differs after full sync"
grep -q "MIRAGE_SENTINEL appended" $ROOT/sub/beta.txt || fail "MATERIALIZE_ALL clobbered a local edit"

unset LD_PRELOAD
log "final STATS: $(req STATS)"
printf '\nSHIM-VALIDATE: ALL CHECKS PASSED\n'
