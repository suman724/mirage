# Hand-off: Mirage — Lazy Workspace Filesystem + Client-Initiated Transport

**Mirage** implements a **Claude-Code-style remote agent file layer**: a thin client on the user's machine (Windows + macOS), a harness running in a cloud Linux sandbox, and a **lazy, content-addressed filesystem** that lets the sandbox read the user's workspace *as if it were local* — without ever eagerly uploading the whole workspace, and without the sandbox ever connecting *into* the client. The name captures the illusion: a full local filesystem that isn't really there, materialized on demand.

> **Goal of the first milestone:** validate the hard part — *outbound-only connection, server drives over it, lazy chunk fetch backs a FUSE read* — end to end, quickly. Everything else builds on that.

---

## Read these first (in order)

1. **`docs/workspace-fs-and-transport.md`** — the design this repo implements. It is **self-contained**; you do not need any other doc to start. Start with §2 (the one picture), §3 (why outbound-only still lets the server pull data), §4 (desync + FUSE + git), §6 (security), §10 (this repo's layout).
2. **`proto/mirage.proto`** — the wire protocol. This is the contract between client and server; both sides generate from it.

> *Optional background only:* `remote-sandbox-harness.md` (if present) explains *why* a remote sandbox and the options trade-off. That decision is already made — it is not needed to write code.

---

## Locked decisions (do not relitigate without cause)

| Decision | Why |
|---|---|
| **Lazy fetch via desync/casync** (content-addressed chunks, fetch-on-read, cache) | Decouples workspace *size* from sync *cost*. See design §4. |
| **git fast-path** — clone from remote on the sandbox, client ships only working-tree delta | Committed bulk never crosses the client's slow uplink. Design §4.3. |
| **Connection always client→server** (outbound only); server never dials client | Client is behind NAT/firewall and holds sensitive data. Design §3. |
| **gRPC bidi default, WebSocket fallback** | Typed/multiplexed/flow-controlled; WS for proxy-hostile nets. Design §3. |
| **FUSE on the Linux sandbox only** — never on the client | Removes the Windows-sandbox problem entirely; client stays a thin Go binary. |
| **Client + server in one Go monorepo** | Shared protocol + chunk logic; evolve together. Design §10. |
| **Chunk-hash protocol, not path protocol** | Server can only request hashes the client published → can't name `~/.ssh`. Design §6. |

---

## Repo layout

See design §10 for the annotated tree. In short:

- `client/` — Go, Windows + macOS. Dials out, indexes the workspace, serves chunks by hash, applies write-backs, renders the TUI.
- `server/` — Go, cloud Linux sandbox. Accepts the connection, runs the FUSE mount, fetches chunks over the channel (`channelstore`), git fast-path, search index.
- `proto/` — the single shared protocol definition.
- `docs/` — the two design docs travel with the repo.

**The connection rule, in code:** only `client/transport` ever calls `Dial`. `server/transport` only ever `Accept`s, then sends `ServerFrame`s (including `ChunkRequest`) over the already-open stream.

---

## Tech stack

| Concern | Choice |
|---|---|
| Language (both sides) | Go |
| Content-addressed chunking + index + Store | `folbricht/desync` |
| FUSE | `hanwen/go-fuse` (or `casync mount` to start) |
| Transport (default) | `google.golang.org/grpc` bidi streaming over TLS |
| Transport (fallback) | `coder/websocket` over TLS |
| Client file watching | `fsnotify/fsnotify` |
| TUI | `charmbracelet/bubbletea` + `lipgloss` + `bubbles` |
| git lazy clone | native `git clone --filter=blob:none` |
| Server-side search | Zoekt / Sourcegraph (or homegrown trigram) |

---

## First milestones (prove the spine, then widen)

These mirror design §9. Don't skip ahead — each de-risks the next.

- **M0 — Transport spike.** Client dials out (gRPC bidi); server sends a request frame back *over the same connection*; client answers. **Done when:** server-originated request → client response works over a single client-opened stream, behind a NAT/firewall.
- **M1 — desync round-trip (no FUSE).** Client indexes a directory (with secret excludes) and serves chunks by hash; server's `channelstore.GetChunk(h)` fetches a known chunk over the channel. **Done when:** server reconstructs a file purely from chunk requests.
- **M2 — Lazy FUSE read.** Mount the index on the sandbox; a stub "harness" process reads a file; chunks fault over the channel; local cache makes re-reads free. **Done when:** `cat workspace/foo.py` on the sandbox returns correct bytes via faults, and a second read hits cache.
- **M3 — git fast-path.** Detect a remote, partial-clone on the sandbox, overlay the client's working-tree delta.
- **M4 — Write-back.** Sandbox edits stream back; base-hash conflict detection; TUI prompt.
- **M5 — Search index.** `search_files` queries the server-side index, **not** the FUSE tree (see risk below).

---

## Top risks to validate early (don't discover these late)

1. **Search fault-storm — the big one.** A repo-wide `ripgrep`/`search_files` over a lazy FUSE faults *every* file and floods the channel. **The server-side content index (M5) is mandatory, not optional.** Decide early whether to bring it forward.
2. **Cold-read latency.** Each missing chunk = one channel round-trip. Mitigate with local cache, prefetch (whole-dir-on-first-touch, neighbor chunks), and warm snapshots. Measure round-trip cost in M2.
3. **Write-back conflicts.** Local file changed since publish *and* sandbox changed it. Needs base-hash tracking + prompt-on-conflict UX.
4. **Disconnect/resume.** Connection drops mid-fault — retry semantics, resumable streams, session survival.
5. **CDC chunk-size tuning.** Many-small-files (source trees) vs. few-large-files changes fault count and dedup ratio.

---

## Out of scope for this repo

- The **Hermes Python harness** itself. It consumes the FUSE mount as a normal directory; integration is a new `BaseEnvironment` backend in the Hermes repo. Use a **stub harness** (a process that reads/writes the mount) for M0–M4.
- Control-plane concerns (auth provider, sandbox provisioning, billing/quotas) — assume a provisioned sandbox and a valid connection-bound token.

---

## Open questions to resolve with the design owner

See design §8. The ones that affect early code: gRPC-vs-WS default, CDC parameters, prefetch policy, search-index choice, write-back granularity.
</content>


Future tasks
    NEXT ITERATIONS (do not start yet): (a) add the server-side FUSE mount so a real POSIX read                                                                                      
    faults chunks via ChunkRequest; (b) a minimal non-interactive CLI to drive it; TUI comes                                                                                         
    last.   

Make sure we have a clean Makefile