#!/usr/bin/env bash
# Ptrace production-path validation (docs/design-ptrace-interception.md §13): the
# REAL mirage-server in --ptrace mode, driven by the REAL mirage-client over
# gRPC, with the workload attaching from an INDEPENDENT process — the same shape
# as the Fargate deployment (orchestrator owns the workload, mirage-server
# side-attaches), minus the ALB.
#
#   mirage-server --ptrace /ws        (waits for a client; then listens on
#        ▲ gRPC (client dials)         the attach socket — launches NO workload)
#   mirage-client --dir <fixture>
#   trace-launcher (separate proc) --> ATTACH <pid> --> mirage-server SEIZEs it
#        runs <workload> reading /ws       (non-descendant => CAP_SYS_PTRACE)
#
# Asserts: a libc tool (cat) AND a static Go binary read materialized files
# byte-identically through ptrace interception, the HTTP health endpoint answers
# 200, the server entered ptrace mode, and no placeholder zeros leak.
set -euo pipefail

FIX=/tmp/fix
WS=/tmp/ws
STATE=/tmp/state
BIN=/tmp/bin
ADDR=127.0.0.1:7777
HEALTH=127.0.0.1:8080
SOCK="$STATE/attach.sock"

log()  { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; echo '--- server.out ---' >&2; cat /tmp/server.out >&2 2>/dev/null; echo '--- wl.out ---' >&2; cat /tmp/wl.out >&2 2>/dev/null; exit 1; }

log "build server, client, trace-launcher, and a static Go reader"
mkdir -p "$BIN"
go build -o "$BIN/mirage-server" ./server
go build -o "$BIN/mirage-client" ./client
gcc -O2 -Wall -Wextra -Werror -o "$BIN/mirage-trace-launcher" shim/trace-launcher.c
cat > /tmp/static-reader.go <<'GO'
package main

import "os"

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

log "sentinel fixture (no zero bytes => any placeholder leak is detectable)"
rm -rf "$FIX" "$WS" "$STATE"
mkdir -p "$FIX/sub" "$STATE"
printf 'MIRAGE_SENTINEL alpha\n' > "$FIX/alpha.txt"
printf 'MIRAGE_SENTINEL beta\n'  > "$FIX/sub/beta.txt"
python3 -c "open('$FIX/big.bin','wb').write(bytes((i%251)+1 for i in range(200*1024)))"

# Workload (runs in the EXTERNAL process under the RET_TRACE filter): a libc tool
# and a static Go binary both read the projected workspace, then signal done.
WORKLOAD="cat $WS/alpha.txt $WS/sub/beta.txt; $BIN/static-reader $WS/big.bin > /tmp/big.out; echo MIRAGE_WORKLOAD_DONE"

log "start mirage-server --ptrace (waits for the client to publish, then for an attach)"
"$BIN/mirage-server" --addr "$ADDR" \
    --ptrace "$WS" --ptrace-state "$STATE" \
    --health-addr "$HEALTH" --log-level info \
    > /tmp/server.out 2>&1 &
SERVER_PID=$!
trap 'kill $SERVER_PID ${CLIENT_PID:-} ${WL_PID:-} 2>/dev/null || true' EXIT

log "health endpoint answers before any connection (ALB readiness)"
ok=""
for _ in $(seq 1 100); do
    if python3 -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://$HEALTH/healthz',timeout=1).status==200 else 1)" 2>/dev/null; then ok=1; break; fi
    sleep 0.1
done
[ -n "$ok" ] || fail "health endpoint never came up at http://$HEALTH/healthz"
echo "  /healthz -> 200"

log "run mirage-client (publishes the fixture; triggers skeleton + attach socket)"
"$BIN/mirage-client" --addr "$ADDR" --dir "$FIX" > /tmp/client.out 2>&1 &
CLIENT_PID=$!

log "wait for the attach socket to appear (server built the skeleton and is listening)"
ready=""
for _ in $(seq 1 150); do
    [ -S "$SOCK" ] && { ready=1; break; }
    kill -0 $SERVER_PID 2>/dev/null || break
    sleep 0.2
done
[ -n "$ready" ] || fail "attach socket $SOCK never appeared"
echo "  attach socket ready"

log "run the workload in an INDEPENDENT process that side-attaches (CAP_SYS_PTRACE)"
MIRAGE_ATTACH_SOCK="$SOCK" "$BIN/mirage-trace-launcher" sh -c "$WORKLOAD" > /tmp/wl.out 2>&1 &
WL_PID=$!

log "wait for the workload to finish reading through the ptrace projection"
done=""
for _ in $(seq 1 150); do
    grep -q MIRAGE_WORKLOAD_DONE /tmp/wl.out 2>/dev/null && { done=1; break; }
    kill -0 $WL_PID 2>/dev/null || break
    sleep 0.2
done
[ -n "$done" ] || fail "workload did not complete (see wl.out)"

log "assert: libc tool (cat) read materialized content byte-identically"
grep -q "MIRAGE_SENTINEL alpha" /tmp/wl.out || fail "cat did not read alpha.txt content"
grep -q "MIRAGE_SENTINEL beta"  /tmp/wl.out || fail "cat did not read beta.txt content"

log "assert: STATIC Go binary read the large file byte-identically (the libc-blind case)"
cmp /tmp/big.out "$FIX/big.bin" || fail "static Go binary read wrong content for big.bin"

log "assert: no NUL bytes (no placeholder zeros) in the text output"
sed -n '/MIRAGE_SENTINEL/p' /tmp/wl.out | grep -qP '\x00' && fail "NUL byte in text output (placeholder leak)" || true

log "server log shows ptrace mode ran"
grep -q "ptrace tracer listening for orchestrator attach" /tmp/server.out || fail "server did not enter ptrace mode"

printf '\nPTRACE-SERVER-VALIDATE: ALL CHECKS PASSED\n'
