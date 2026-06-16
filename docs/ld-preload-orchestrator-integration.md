# Integrating Mirage (LD_PRELOAD mode) into the agent orchestrator

A hand-off doc for wiring Mirage's **LD_PRELOAD shim** into an existing
containerized agent orchestrator, in the same Dockerfile. Self-contained: it
assumes no prior knowledge of Mirage.

> **Read this first — the one caveat that decides whether this is worth it.**
> LD_PRELOAD only intercepts programs that read files **through glibc**
> (`python`, `node`, `bash`, `cat`, GNU `grep`/`sed`, most dynamically-linked
> tools). **Statically-linked and Go binaries issue raw syscalls and bypass
> LD_PRELOAD entirely** — for those, Mirage in this mode is invisible and they
> read *placeholder zeros* (or get `EIO`). Agent harness toolchains often include
> Go/Rust static tools (`rg`/ripgrep, `gopls`, `go`, `dlv`, sometimes a static
> `git`). So LD_PRELOAD is a **good first integration test for the libc-tool
> path**, but it is *not* the production answer for a mixed toolchain. The
> production front-ends that cover Go/static at the syscall layer are
> `--seccomp` and `--ptrace` (see `docs/design-ptrace-interception.md`). Plan to
> graduate to one of those once the LD_PRELOAD test proves the data path.

---

## 1. What Mirage does, in one paragraph

A thin **client** runs on the user's laptop; a **server** runs in the cloud
container. The server presents the user's workspace as a normal directory tree
of **sparse placeholders** (real names, real sizes, zero bytes on disk). When a
tool opens a file, Mirage **materializes** it on demand — fetching its content as
content-addressed chunks over **one connection the client dials out** (the server
never dials in). The LD_PRELOAD shim is the interception mechanism: it hooks
`open()` so the file is filled *before* the tool reads it.

```
   LAPTOP (client)                         CONTAINER (orchestrator + Mirage)
   ┌───────────────┐   gRPC: client dials  ┌──────────────────────────────────┐
   │ mirage-client │ ────────────────────▶ │ mirage-server --shim /workspace   │
   │  --dir <ws>   │ ◀──── chunk reqs ────  │   builds the skeleton on connect, │
   └───────────────┘   (server pulls down   │   runs the shim supervisor on a   │
                        the open stream)     │   unix socket                     │
                                             │                                   │
                                             │ harness runs with LD_PRELOAD set: │
                                             │   open(/workspace/x) ─▶ ENSURE x  │
                                             │   ─▶ supervisor fills x ─▶ read   │
                                             └──────────────────────────────────┘
```

## 2. The moving parts you add to the image

| Artifact | Built from | Role |
|---|---|---|
| `mirage-server` | Mirage repo, `./server` (Go) | Runs in the container. `--shim` mode: builds the skeleton, runs the supervisor. |
| `mirage-client` | Mirage repo, `./client` (Go) | Runs on the laptop (or in-container for the smoke test). Publishes the workspace. |
| `libmirageshim.so` | Mirage repo, `shim/mirageshim.c` (C) | `LD_PRELOAD`ed into the **harness** process; hooks `open()`. |

**Contract the shim relies on (three env vars, read once at load):**

| Env var | Value | Meaning |
|---|---|---|
| `LD_PRELOAD` | path to `libmirageshim.so` | injects the shim |
| `MIRAGE_SHIM_ROOT` | the projected workspace root (e.g. `/workspace`) | opens *under this* are intercepted; everything else passes through untouched |
| `MIRAGE_SHIM_SOCK` | `<shim-state>/shim.sock` | the supervisor socket the shim talks to |
| `MIRAGE_SHIM_DEBUG` | `1` (optional) | trace each decision to stderr — use during bring-up |

The supervisor socket path is **deterministic**: if you start the server with
`--shim-state /var/mirage`, the socket is always `/var/mirage/shim.sock`. You do
not need to parse logs to find it.

## 3. Prerequisites

- **The Mirage source** must be reachable at image-build time (git clone, a
  submodule, or `COPY`ed in). It is a normal Go module.
- **The runtime image must be glibc-based** (debian/ubuntu, `python:3.x-slim`,
  etc.). Build `libmirageshim.so` against the **same libc** as the runtime image.
  The `golang:1.25` builder below is debian/glibc, which matches debian/ubuntu/
  `*-slim` runtimes. **If your base is Alpine (musl), this will not work as-is** —
  build the `.so` on an Alpine builder, and expect musl interposition to be more
  fragile.
- **A network path for the client to dial the server** (`:7777` by default).
  For the first test you can skip this and run the client in-container (§6).

## 4. Dockerfile changes (multi-stage)

Add a builder stage that compiles the three artifacts, then `COPY` them into your
existing orchestrator image. No Go or C toolchain is needed at runtime.

```dockerfile
# ── Mirage builder (debian/glibc — matches a debian/ubuntu/slim runtime) ──
FROM golang:1.25 AS mirage-builder
WORKDIR /mirage
# Bring in the Mirage source however you vendor it:
COPY mirage/ .
#   or:  RUN git clone --depth=1 <mirage-repo-url> .
RUN CGO_ENABLED=0 go build -o /out/mirage-server ./server \
 && CGO_ENABLED=0 go build -o /out/mirage-client ./client \
 && cc -shared -fPIC -O2 -Wall -Wextra -Werror \
        -o /out/libmirageshim.so shim/mirageshim.c -ldl
# (equivalently: `make build && make shim-lib`, which produces the same files in ./bin)

# ── your existing orchestrator image ──
FROM <your-existing-base>
# ... your existing build steps ...
COPY --from=mirage-builder /out/mirage-server      /usr/local/bin/mirage-server
COPY --from=mirage-builder /out/mirage-client      /usr/local/bin/mirage-client
COPY --from=mirage-builder /out/libmirageshim.so   /usr/local/lib/libmirageshim.so
```

`CGO_ENABLED=0` makes the Go binaries portable across glibc images. The `.so`
itself is C and links `libdl`; it must match the runtime libc (hence build it on
a base that matches your runtime).

## 5. Runtime wiring (startup order matters)

Mirage's `--shim` mode is **connection-scoped**: the server builds the skeleton
and opens the supervisor socket **only after the client connects and publishes**,
and tears them down when the client disconnects. So the order is:

1. **Start the server** (it waits for a client):
   ```sh
   mkdir -p /workspace /var/mirage
   mirage-server \
     --addr 0.0.0.0:7777 \
     --shim /workspace \
     --shim-state /var/mirage \
     --health-addr 0.0.0.0:8080 \
     --log-level info &
   ```
   `/workspace` is where the skeleton appears. `/var/mirage` holds the journal,
   the chunk cache, and `shim.sock`. `--health-addr` exposes `GET /healthz → 200`
   for an ALB/ELB readiness check (optional).

2. **The client connects and publishes** — from the laptop (through your
   proxy/ALB), or in-container for the smoke test (§6). The client must **stay
   connected** for the whole time the harness runs: it serves the file chunks the
   server pulls down on demand.

3. **Wait for readiness**, then **run the harness with the shim env set** — and
   set those env vars **only on the harness subprocess, not the orchestrator
   itself** (the orchestrator does its own unrelated opens; you don't want to
   route those through the shim, especially before the socket exists):
   ```sh
   # block until the server has built the skeleton and is serving:
   until [ -S /var/mirage/shim.sock ]; do sleep 0.2; done

   env \
     LD_PRELOAD=/usr/local/lib/libmirageshim.so \
     MIRAGE_SHIM_ROOT=/workspace \
     MIRAGE_SHIM_SOCK=/var/mirage/shim.sock \
     <your-harness-command> ...        # run it with cwd inside /workspace
   ```

In orchestrator code, the equivalent is: build the child's environment with those
three variables added to the inherited env, set its working directory under
`/workspace`, and spawn it **after** `shim.sock` exists. Nothing else about how
the harness is launched changes.

> **Why not set `LD_PRELOAD` globally (e.g. in the Dockerfile `ENV`)?** Because it
> would inject the shim into the orchestrator and every unrelated process,
> including ones that open files before the socket exists — those opens would hit
> a missing supervisor and fail `EIO`. Scope it to the harness, after readiness.

## 6. Local smoke test (prove the data path before involving the laptop)

Run the client and server in the same container against a throwaway fixture. This
validates the build and the LD_PRELOAD path end to end with **no proxy and no
laptop**.

```sh
# fixture with recognisable content (non-zero, so a placeholder leak is obvious)
mkdir -p /tmp/fix && printf 'HELLO_FROM_MIRAGE\n' > /tmp/fix/hello.txt

# 1. server in shim mode
mirage-server --addr 127.0.0.1:7777 --shim /workspace --shim-state /var/mirage \
  --log-level debug &

# 2. client publishes the fixture and STAYS connected
mirage-client --addr 127.0.0.1:7777 --dir /tmp/fix &

# 3. wait for the skeleton + socket
until [ -S /var/mirage/shim.sock ]; do sleep 0.2; done

# 4. a libc tool reads the file THROUGH the shim — must print HELLO_FROM_MIRAGE
env LD_PRELOAD=/usr/local/lib/libmirageshim.so \
    MIRAGE_SHIM_ROOT=/workspace \
    MIRAGE_SHIM_SOCK=/var/mirage/shim.sock \
    cat /workspace/hello.txt
```

Expected: `HELLO_FROM_MIRAGE`. If you see zeros, an empty result, or `EIO`, see
troubleshooting below. (The repo's `scripts/shim-validate.sh` runs a fuller
matrix — `python`, `node`, `grep`, `sed` — and is the reference for "working".)

## 7. Verifying with the real harness

Point the harness at files under `/workspace` and watch for two failure
signatures:

- **Correct content** for libc tools (python/node/bash/etc.) → the path works.
- **Zeros / truncated reads / `EIO`** from a specific tool → that tool is almost
  certainly **static or Go** and bypassing the shim. Confirm with
  `ldd $(command -v <tool>)`: "not a dynamic executable" or no `libc.so` means
  LD_PRELOAD can't see it.

Turn on `MIRAGE_SHIM_DEBUG=1` to log every interception decision (which opens
were intercepted, ENSURE/DIRTY sent, EIO raised) to the harness's stderr.

## 8. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Tool reads **zero bytes** / placeholder | tool is static/Go → bypasses libc | expected limit; needs `--seccomp`/`--ptrace` instead |
| `open` fails with **EIO** | supervisor unreachable or fill failed (socket wrong, client disconnected, chunk unavailable) | check `MIRAGE_SHIM_SOCK` is `<shim-state>/shim.sock`; ensure the client is still connected; check server logs |
| `shim.sock` never appears | client never connected/published, or server not in `--shim` mode | confirm the client dialed and published; check server logs for "workspace projected" |
| Shim seems inert (no interception) | `MIRAGE_SHIM_ROOT`/`MIRAGE_SHIM_SOCK` unset or wrong | the shim is inert unless **both** are set; the opened path must be **under** `MIRAGE_SHIM_ROOT` |
| Loader error: `cannot open shared object` / wrong ELF | `.so` built for a different libc/arch than the runtime | rebuild the `.so` on a base matching the runtime image (glibc vs musl, amd64 vs arm64) |
| Everything `EIO` right at orchestrator start | `LD_PRELOAD` set globally before the socket existed | scope the env to the harness subprocess, after `shim.sock` exists (§5) |

## 9. Known limits and exit criteria

LD_PRELOAD mode covers **libc-based, read-path** access only. It does **not** see:

- **Go / static binaries** (raw syscalls) — the big one for agent toolchains.
- **Namespace syscalls** (`rename`/`unlink`/`mkdir`) — a server-side pristine
  check guards the data-loss case, but these aren't intercepted.
- **Write-back to the laptop** — not in this mode.

**Decide the test passed if:** your harness's *libc* tools read correct content
lazily, and you've enumerated which tools are static/Go (via `ldd`). **Move to
`--seccomp` or `--ptrace`** (same `mirage-server`, different flag; same
`/workspace` projection and the same on-demand materialization, but interception
at the **syscall layer** so Go/static binaries are covered too) once you need the
full toolchain. The `--ptrace` path additionally lets mirage-server run as a
*side* process and attach to a workload the orchestrator owns — see
`docs/design-ptrace-interception.md` and `docs/cli-integration-design.md`.
