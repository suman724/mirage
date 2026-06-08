#!/usr/bin/env bash
# Interactive FUSE demo, meant to run INSIDE the Linux validation container
# (see `make fuse-demo`). It starts a mount-mode server and a real client over
# localhost, then reads a file off the FUSE mount so you can watch chunks fault
# over the channel — and a second read served with no new faults.
set -euo pipefail

ADDR=127.0.0.1:7777
WS=/tmp/mirage-ws
SRC=./testdata/workspace
SRVLOG=/tmp/mirage-server.log
FILE=src/main.go

mkdir -p "$WS"
go build -o /tmp/mirage-server ./server
go build -o /tmp/mirage-client ./client

# Server in MOUNT mode (debug logs to a file so we can count faults).
/tmp/mirage-server --addr "$ADDR" --mount "$WS" --log-level debug >"$SRVLOG" 2>&1 &
SRV=$!
trap 'kill "$CLI" 2>/dev/null || true; sleep 1; kill "$SRV" 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do grep -q "listening" "$SRVLOG" && break; sleep 0.1; done

# Client dials out, publishes the workspace index, then serves chunk requests.
/tmp/mirage-client --addr "$ADDR" --dir "$SRC" --log-level info >/tmp/mirage-client.log 2>&1 &
CLI=$!
for _ in $(seq 1 100); do [ -f "$WS/$FILE" ] && break; sleep 0.1; done

# grep -c already prints a count (0 on no match); `|| true` just swallows its
# non-zero exit so `set -e` doesn't trip.
faults() { grep -c "requesting chunk over channel" "$SRVLOG" 2>/dev/null || true; }

echo "================================================================"
echo "Mounted workspace at $WS (lazy — nothing materialized yet):"
ls -R "$WS" 2>/dev/null | sed 's/^/  /'
echo "================================================================"

echo
echo ">>> COLD read: cat $WS/$FILE"
b=$(faults)
cat "$WS/$FILE"
sleep 0.3
a=$(faults)
echo "--- chunks faulted over the channel for this read: $((a - b))"

echo
echo ">>> WARM read: cat the same file again"
b=$(faults)
cat "$WS/$FILE" >/dev/null
sleep 0.3
a=$(faults)
echo "--- chunks faulted over the channel for this read: $((a - b))  (0 = served from cache)"

echo
echo ">>> Server fault log:"
grep "requesting chunk over channel" "$SRVLOG" | sed 's/^/  /' || true
echo "================================================================"
echo "Done. (The cold read faulted bytes over the gRPC stream from the client;"
echo "the warm read was served from cache with no new network traffic.)"
