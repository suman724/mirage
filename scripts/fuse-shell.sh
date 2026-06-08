#!/usr/bin/env bash
# Interactive FUSE shell, meant to run INSIDE the Linux validation container
# (see `make fuse-shell`). It starts a mount-mode server and a real client over
# localhost, mounts the workspace, then drops you into a shell sitting in the
# mount so you can `ls`/`cat` files and watch chunks fault over the channel.
#
# If extra args are given they are exec'd instead of an interactive shell (used
# for non-interactive smoke testing).
set -euo pipefail

ADDR=127.0.0.1:7777
WS=/tmp/mirage-ws
SRC=./testdata/workspace
SRVLOG=/tmp/mirage-server.log
CLILOG=/tmp/mirage-client.log

mkdir -p "$WS"
go build -o /tmp/mirage-server ./server
go build -o /tmp/mirage-client ./client

# Server in MOUNT mode (debug logs to a file so you can watch faults).
/tmp/mirage-server --addr "$ADDR" --mount "$WS" --log-level debug >"$SRVLOG" 2>&1 &
SRV=$!
cleanup() {
	kill "${CLI:-}" 2>/dev/null || true
	sleep 1
	kill "$SRV" 2>/dev/null || true
}
trap cleanup EXIT
for _ in $(seq 1 50); do grep -q "listening" "$SRVLOG" && break; sleep 0.1; done

# Client dials out, publishes the workspace index, then serves chunk requests.
/tmp/mirage-client --addr "$ADDR" --dir "$SRC" --log-level info >"$CLILOG" 2>&1 &
CLI=$!
for _ in $(seq 1 100); do [ -f "$WS/src/main.go" ] && break; sleep 0.1; done

if [ ! -f "$WS/src/main.go" ]; then
	echo "ERROR: workspace did not mount; server log:" >&2
	cat "$SRVLOG" >&2
	exit 1
fi

# Non-interactive smoke path: run whatever was passed and exit.
if [ "$#" -gt 0 ]; then
	exec "$@"
fi

# Handy helpers available inside the interactive shell.
cat > /tmp/mirage.bashrc <<RC
PS1='mirage-sandbox:\w\$ '
alias faults='grep -c "requesting chunk over channel" $SRVLOG 2>/dev/null || true'
alias watch-faults='tail -f $SRVLOG | grep --line-buffered "requesting chunk over channel"'
serverlog() { tail -n "\${1:-20}" $SRVLOG; }
cd $WS
RC

cat <<EOF
================================================================
  Mirage sandbox shell. The workspace is FUSE-mounted at:
      $WS   (lazy — file bytes fault over the channel on read)

  Try:
      ls -R $WS
      cat src/main.go            # cold read -> faults a chunk over the wire
      cat src/main.go            # warm read -> served from cache
      faults                     # how many chunks faulted over the channel so far
      serverlog                  # last 20 server log lines (watch the faults)
      cat src/blob.txt | wc -c   # a bigger, multi-chunk file

  The secrets (.env, id_rsa) were never published, so they are NOT here.
  Type 'exit' to tear everything down.
================================================================
EOF

exec bash --rcfile /tmp/mirage.bashrc -i
