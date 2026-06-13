#!/usr/bin/env bash
# Shimmer production-path validation (design §3.3): the REAL mirage-server in
# --seccomp mode, driven by the REAL mirage-client over gRPC — the same shape as
# the Fargate deployment, minus the ALB. Runs UNPRIVILEGED (no caps, no devices).
#
#   mirage-server --seccomp /ws -- <workload>   (waits for a client)
#        ▲ gRPC (client dials)                    │ on publish: build skeleton,
#   mirage-client --dir <fixture>                 │ launch <workload> under the
#                                                 ▼ C launcher, service its opens
#
# Asserts: a libc tool (cat) AND a static Go binary read materialized files
# byte-identically through the seccomp interception, the HTTP health endpoint
# answers 200, and no placeholder zeros leak.
set -euo pipefail

FIX=/tmp/fix
WS=/tmp/ws
STATE=/tmp/state
BIN=/tmp/bin
ADDR=127.0.0.1:7777
HEALTH=127.0.0.1:8080

log()  { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; echo '--- server.out ---' >&2; cat /tmp/server.out >&2 2>/dev/null; exit 1; }

log "build server, client, launcher, and a static Go reader"
mkdir -p "$BIN"
go build -o "$BIN/mirage-server" ./server
go build -o "$BIN/mirage-client" ./client
gcc -O2 -Wall -Wextra -Werror -o "$BIN/mirage-launcher" shim/launcher.c
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

# Workload (runs on the server side, under the seccomp filter): a libc tool and
# a static Go binary both read the projected workspace, then signal done.
WORKLOAD="cat $WS/alpha.txt $WS/sub/beta.txt; $BIN/static-reader $WS/big.bin > /tmp/big.out; echo MIRAGE_WORKLOAD_DONE"

log "start mirage-server --seccomp (waits for the client to publish)"
"$BIN/mirage-server" --addr "$ADDR" \
    --seccomp "$WS" --seccomp-state "$STATE" --seccomp-launcher "$BIN/mirage-launcher" \
    --health-addr "$HEALTH" --log-level info \
    -- sh -c "$WORKLOAD" > /tmp/server.out 2>&1 &
SERVER_PID=$!
trap 'kill $SERVER_PID ${CLIENT_PID:-} 2>/dev/null || true' EXIT

log "health endpoint answers before any connection (ALB readiness)"
ok=""
for _ in $(seq 1 100); do
    if python3 -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://$HEALTH/healthz',timeout=1).status==200 else 1)" 2>/dev/null; then ok=1; break; fi
    sleep 0.1
done
[ -n "$ok" ] || fail "health endpoint never came up at http://$HEALTH/healthz"
echo "  /healthz -> 200"

log "run mirage-client (publishes the fixture; triggers skeleton + workload)"
"$BIN/mirage-client" --addr "$ADDR" --dir "$FIX" > /tmp/client.out 2>&1 &
CLIENT_PID=$!

log "wait for the workload to finish reading through the seccomp projection"
done=""
for _ in $(seq 1 150); do
    grep -q MIRAGE_WORKLOAD_DONE /tmp/server.out 2>/dev/null && { done=1; break; }
    kill -0 $SERVER_PID 2>/dev/null || break
    sleep 0.2
done
[ -n "$done" ] || fail "workload did not complete (see server.out)"

log "assert: libc tool (cat) read materialized content byte-identically"
grep -q "MIRAGE_SENTINEL alpha" /tmp/server.out || fail "cat did not read alpha.txt content"
grep -q "MIRAGE_SENTINEL beta"  /tmp/server.out || fail "cat did not read beta.txt content"

log "assert: STATIC Go binary read the large file byte-identically (the C-shim-blind case)"
cmp /tmp/big.out "$FIX/big.bin" || fail "static Go binary read wrong content for big.bin"

log "assert: no NUL bytes (no placeholder zeros) in the libc tool output"
# server.out mixes logs + cat output; check the cat'd sentinel lines specifically
if grep -aqP '\x00' /tmp/big.out; then : ; fi   # big.bin is binary; checked via cmp above
sed -n '/MIRAGE_SENTINEL/p' /tmp/server.out | grep -qP '\x00' && fail "NUL byte in text output (placeholder leak)" || true

log "server log shows seccomp interception ran"
grep -q "running workload under seccomp interception" /tmp/server.out || fail "server did not enter seccomp mode"

printf '\nSECCOMP-SERVER-VALIDATE: ALL CHECKS PASSED\n'
