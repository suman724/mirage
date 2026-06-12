# Shimmer — FUSE-free workspace projection (design)

**Status:** S1 (skeleton + supervisor) and S2 (C shim + Docker validation)
are **implemented and validated** (`server/shim`, `shim/mirageshim.c`,
`make shim-validate`); §10 milestones S3–S6 remain. The S2 decision in §4.1
(open()-only + pristine check) is in effect; STATS was added as a fourth
supervisor verb and `--shim-state` makes the journal persistent.
**Scope:** server-side only. The wire protocol gains one field (mtime); the
client gains one opt-in flag (.git indexing). Everything else is additive.
**Background:** `docs/mirage-on-fargate.md` (plain-language analysis of why
FUSE is unavailable on Fargate and how the alternatives compare). This doc
assumes that context and specifies the build.

---

## 1. Problem

Mirage's lazy workspace mount (`--mount`) requires FUSE, which needs
`/dev/fuse` + `CAP_SYS_ADMIN`. AWS Fargate (and similar locked-down
container platforms) forbid both. We want the Mirage experience — a
`/workspace` that materializes on demand — with **zero kernel privileges**.

**Target workload (explicit, narrow):** one Go process we control (the
sandbox agent, which needs git operations), plus arbitrary **dynamically
linked libc tools** (python, node, shell, grep, sed, compilers…). Arbitrary
third-party Go/static binaries are explicitly **out of scope** for lazy
access; the design fails *loudly* (or falls back to full materialization)
when one appears, never silently.

## 2. Goals / non-goals

Goals:

- G1. libc tools read `/workspace` lazily, **per-file** (materialize on
  first open), with native metadata (`ls`, `find`, `stat` need no
  interception).
- G2. The embedded Go agent gets **per-chunk** lazy git via go-git over a
  Mirage-backed `billy.Filesystem`.
- G3. Correctness over laziness: no process ever observes wrong file
  *content*. Uninterceptable binaries are blocked or get a fully
  materialized tree.
- G4. Runs unprivileged: no mounts, no devices, no capabilities.
- G5. Reuse the existing store chain (`Cache(DedupQueue(channelstore),
  LocalStore)`) and `ReadRange`/`IndexFromRefs` unchanged. Iron rules
  (client-only dial, hash-based protocol, client-side secret exclusion)
  are untouched — Shimmer is purely a new *consumer* of the store chain.

Non-goals:

- Write-back to the laptop **in this workstream**. Write-back remains an
  eventual Mirage goal (original design M4 / Phase 4), deliberately deferred
  until the basics are proven. Shimmer keeps the door open rather than
  closing it: the `local` state in the table (§3.2) is exactly the set of
  paths a future write-back would ship, so nothing here needs undoing.
- Intercepting third-party Go/static binaries (see seccomp spike, §11).
- Replacing the FUSE mode. `--mount` remains the preferred mode where FUSE
  exists; Shimmer is the constrained-platform alternative.

## 3. Architecture

```
        sandbox container (no privileges)
  ┌──────────────────────────────────────────────────────────┐
  │  libc tools (python, grep, sed…)                         │
  │     │  LD_PRELOAD=libmirageshim.so                       │
  │     │  open()/exec() intercepted; stat/readdir native    │
  │     ▼                                                    │
  │  /workspace  ← real dir: SKELETON (dirs + placeholder    │
  │     ▲          files w/ true size/mode/mtime); files     │
  │     │          filled in-place on first open             │
  │     │ ENSURE(path) over unix socket                      │
  │  ┌──┴───────────────────────────────────────────┐        │
  │  │ mirage-server --shim /workspace (supervisor) │        │
  │  │  skeleton builder · materializer ·           │        │
  │  │  state table · exec-gate policy ·            │        │
  │  │  billy/fs.FS adapters (in-process git)       │        │
  │  └──┬───────────────────────────────────────────┘        │
  │     │ store chain: Cache(DedupQueue(channelstore), Local)│
  └─────┼────────────────────────────────────────────────────┘
        │ existing chunk protocol, over the connection
        ▼ the CLIENT dialed (unchanged)
     laptop client
```

One new server mode, `--shim DIR`, sits beside `--out` and `--mount` in
`server/transport.drive()`. On `IndexPublish` it:

1. builds the **skeleton** under DIR,
2. starts the **supervisor** listening on a unix socket,
3. optionally hands the embedding agent a `billy.Filesystem` / `fs.FS`.

### 3.1 Skeleton

For every manifest entry: create parent dirs, then a **sparse placeholder**
(`truncate` to true size) with the manifest mode and mtime (§6). Cost is
metadata-only — O(files), no chunk fetches.

Consequence: `stat`, `readdir`, `find`, `du --apparent-size`, shell globs
all work **natively for every binary** with zero interception. This is what
collapses the libc interception surface from ~40 symbols to the open family
(~10) plus exec (~6) — the classic LD_PRELOAD minefield (legacy `__xstat`
symbols, `statx`, `readdir64`, glibc-internal bypasses of stat paths) is
mostly sidestepped because metadata is *real*.

Residual risk: a placeholder not yet materialized contains zeros. Only a
process that bypasses the shim could read one (libc tools can't reach
`read()` without passing the intercepted `open()`). That is exactly the
Go/static-binary hole, handled by the exec gate (§5) — and it fails loud,
not silent.

### 3.2 State table

The supervisor keeps per-path state: `placeholder | materializing |
materialized | local` (local = created or opened-for-write by a tool; its
content diverges from the manifest). In-memory map + append-only journal
file under the cache dir so a supervisor restart doesn't re-serve
placeholders as materialized. Per-path singleflight so concurrent ENSUREs
of one file fetch once (chunk-level dedup already exists in the store
chain; this is file-level).

**Important limit (see §4.1):** with only `open()` intercepted, the `local`
set is a *lower bound* on what actually diverged from the manifest — tree
mutations that don't pass through `open()` (rename, unlink, mkdir, chmod…)
are invisible to it. The state table is therefore not a complete change
ledger until either the namespace syscalls are also intercepted or a
sync-time rescan reconciles it.

## 4. The shim (`libmirageshim.so`)

**Language: C** (~400 LOC target). Not Go: a `c-shared` Go runtime injected
into every process is fork-unsafe and heavyweight; the shim must be safe in
any process, including ones that fork immediately.

Interposed symbols:

- Open family: `open`, `open64`, `openat`, `openat64`, `creat`, `creat64`,
  `fopen`, `fopen64`, `freopen`, `freopen64`.
- Exec family (§5): `execve`, `execv`, `execvp`, `execvpe`, `execl*`,
  `posix_spawn`, `posix_spawnp`.

Open-path logic (everything else passes straight through to the real call
via `dlsym(RTLD_NEXT, …)`):

```
if path not under $MIRAGE_SHIM_ROOT → real open
canonicalize (handle relative paths, dirfd for *at variants)
if O_CREAT and file does not exist → real open   (new local file;
                                                  supervisor learns via INVAL)
send "ENSURE <path>\n" on $MIRAGE_SHIM_SOCK, await "OK" | "ERR <msg>"
on OK  → real open (content now real)
on ERR → return -1, errno=EIO (loud)
```

Modifying the bytes of an existing file needs no special casing beyond
ENSURE-before-open: once content is real, `O_WRONLY/O_RDWR/O_APPEND/mmap/
sendfile` all operate on a real file. Opens for write additionally send
`DIRTY <path>` (fire-and-forget) so the supervisor flips the state to
`local` — the billy adapter (§7) must know the manifest no longer describes
this file. **But "writing" is more than in-place byte changes; see §4.1 for
what `open()`-only interception does *not* catch and why it matters even
before write-back.**

Protocol: newline-delimited text over `SOCK_STREAM` unix socket, one
request per connection, 30s timeout. Deliberately primitive — the C side
stays trivial, and the socket is same-container with filesystem
permissions (0700 dir) as the trust boundary.

### 4.1 Writes: what `open()` covers, and what it doesn't

`open()` is the choke point for file **content**: once a process holds an fd
to a real file, all of `write/pwrite/writev/ftruncate(fd)/mmap(MAP_SHARED)/
fsync/close` go to the kernel against that real inode with no further
interception. So for *changing the bytes of a file that already exists in
the manifest*, intercepting `open()` is a complete and correct foundation.

What `open()` does **not** see are the syscalls that mutate the *tree shape*
rather than a file's contents: `rename`, `unlink`, `rmdir`, `mkdir`,
`link`, `symlink`, `chmod`/`chown`, `utimensat`, and path-form
`truncate(path)`. None take a workspace fd via our intercepted open, so the
state table never learns about them.

This is not only a future-write-back concern — it has a **read-correctness
bug today** via the most common safe-save pattern (editors, many libraries):

```
open("f.tmp", O_CREAT|O_WRONLY)   → new path, shim passes through, bytes land
write(...) ; close(...)
rename("f.tmp", "f")              → INVISIBLE to an open-only shim
```

After the rename, the inode at `f` is the tmp's real, user-written content,
but the supervisor still has `f` = `placeholder`. The next reader's open
triggers ENSURE, the supervisor sees `placeholder`, and **materializes
manifest content over the user's saved edit — silent data loss, within a
single session.**

Two safeguards, composable; the first is cheap and lands with the shim, the
second is the proper fix tracked for the write workstream:

1. **Pristine-placeholder check before materializing.** ENSURE must confirm
   the on-disk file is still an untouched placeholder (still sparse / a
   recorded inode+ctime marker) before overwriting it. If it isn't, the
   file was replaced out from under us → treat as `local`, never clobber.
   This closes the data-loss case without any new interception.
2. **Intercept the namespace syscalls** (listed above) so the state table
   stays live and accurate. This is the foundation a write-back ledger
   needs: the slice it ships is then exactly the `local` set. Alternatively
   (or additionally) a **sync-time rescan** — walk the overlay, diff against
   the manifest by size/mtime/hash — reconciles the table without any
   namespace hooks, at the cost of rehashing; the state table makes that
   diff cheap by marking which files are even candidates.

**Decision:** S2 ships `open()`-only plus safeguard (1). The namespace
syscalls are explicitly reserved, not built now (tracked as a Shimmer
issue), so the shim stays small and we don't speculatively build write-back
machinery before the basics are proven — while keeping the foundation sound
(no silent clobber) in the meantime.

Known escape hatches (accepted, documented): `env -i` / setuid strip
`LD_PRELOAD`; a libc tool could in principle issue raw `syscall(SYS_open)`.
These degrade to reading zeros from placeholders — detectable (§9 harness
greps for it), not fixable at this layer. The uniform fix is the seccomp
spike (§11).

## 5. Exec gate

Before delegating to the real exec, the shim classifies the target binary:

1. Read ELF header + program headers (not the whole file).
2. `PT_INTERP` present and contains `ld-linux`/`ld-musl` → **dynamic**:
   ensure `LD_PRELOAD`/`MIRAGE_*` vars survive in the child env, exec.
3. Otherwise (static, or unreadable) → policy from `MIRAGE_EXEC_POLICY`:
   - `deny` (default): fail the exec with a clear message naming the
     binary and this design doc.
   - `materialize`: send `MATERIALIZE_ALL` to the supervisor (full
     workspace sync — degrades to `--out` semantics), then exec.
   - `allow`: exec anyway (for workloads known not to touch /workspace).

Notes: shebang scripts are safe — the kernel execs the *interpreter*
(libc-dynamic) and the gate sees that. A Go binary exec'd by another Go
binary is unreachable by the gate, but the gate already fired (or denied)
on the first Go binary in the chain. Classification **fails closed**:
anything not provably libc-dynamic is treated as static.

## 6. Manifest change: mtime (the git-status linchpin)

`chunk.FileEntry` gains `MtimeUnixNs int64` (client populates from
`fi.ModTime()`; zero = unknown, treated as "now" at skeleton build).
Skeleton placeholders get this mtime via `utimensat`.

Why this is load-bearing: `git status`/`diff` hash every worktree file
*unless* the git index's size+mtime match — and hashing everything over a
lazy FS faults every chunk (a stealth full sync). With manifest mtimes
applied to the skeleton **and** the laptop's own `.git` indexed (§6.1, its
index mtimes refer to those same laptop files), go-git status is a
metadata-only walk. Wire compatibility: additive field in the JSON
manifest; old servers ignore it.

### 6.1 Opt-in `.git` indexing

`client --include-git` removes `.git` from `excludedDirs`
(`client/index/index.go:22`) with two guards:

- `.git/config` is **scrubbed at index time**: remote URLs of the form
  `scheme://user:secret@host` have credentials stripped (parse, rewrite).
  The scrubbed bytes are what get chunked — the secret never enters the
  store, same boundary as `IsSecret`.
- `.git/hooks/` is excluded entirely (server must never run laptop hooks).

Without this flag, a server-side git client has no repo data; the
alternative (clone from origin) loses uncommitted laptop state and defeats
the mtime shortcut, so `--include-git` is the recommended path.

## 7. Billy adapter (go-git integration)

New package `server/billyfs`: implements `billy.Filesystem` for the
embedding Go agent. Routing rule per call:

- **Metadata** (`Stat`, `Lstat`, `ReadDir`): the real skeleton (it is
  complete and carries true sizes/modes/mtimes, and includes files tools
  created locally).
- **Content reads** (`Open`/`OpenFile(O_RDONLY)` → `Read/ReadAt/Seek`):
  consult the state table. `local`/`materialized` → real file.
  `placeholder` → **stream from the store chain via
  `IndexFromRefs`+`ReadRange`, without materializing**. This is the
  per-chunk lazy path: go-git's seeky pack reads fault only the chunks
  they touch.
- **Writes** (`Create`, `OpenFile(O_WRONLY…)`, `Rename`, `Remove`): ENSURE
  (if placeholder) then operate on the real file; mark `local`.

Same process as the supervisor → the state table is shared memory, no
socket hop. A read-only `fs.FS` wrapper over the same core ships alongside
(trivial, and lets `testing/fstest.TestFS` exercise the tree logic).

Symlinks: the indexer skips them today; repos using symlinks will show
phantom deletions in git status. Documented limitation; revisit with
`fs.ReadLinkFS` (Go 1.25) if it bites.

## 8. Consistency model (summary)

- One source of truth per path, determined by state: `placeholder` ⇒
  manifest+chunks; `materialized` ⇒ real file ≡ manifest content;
  `local` ⇒ real file only.
- libc tools can only reach content through ENSURE ⇒ they never see
  placeholder zeros. The agent goes through the adapter ⇒ same guarantee.
- Writes are visible to subsequent readers of both kinds immediately
  (real file + state flip). The manifest is a session-frozen snapshot,
  as everywhere else in Mirage.

## 9. Validation

Extend the Docker harness (pattern: `make fuse-validate`) with
`make shim-validate`:

- Full loop: client publishes a fixture tree (incl. `--include-git` repo)
  → server `--shim` → run, under `LD_PRELOAD`: `cat`, `grep -r`, `python3`
  read script, `node` read script, `sed -i` then re-read, shell glob +
  `find`. Assert byte-identical content and that **placeholder zeros were
  never observed** (fixture files carry sentinel content; grep for zeros).
- Laziness assertions: instrument the store (the `memStore.gets` pattern
  from `server/fuse/read_test.go`): opening one file faults only its
  chunks; `git log -1` faults < N chunks of the pack.
- Exec gate: a static Go test binary is denied under `deny`, runs after
  full sync under `materialize`.
- go-git: in-process `status` (clean + after `sed -i`), `log`, `diff` on
  the fixture repo; status performs zero content reads when clean.
- Finally on Fargate itself (manual or CI job): the same harness image as
  a one-shot task — proves G4 where it matters.

## 10. Milestones

1. **S1 — skeleton + supervisor**: `--shim` mode, skeleton builder, state
   table, ENSURE materializer **with the pristine-placeholder check (§4.1
   safeguard 1)**, unit tests. (No shim yet — exercised via a test client on
   the socket.)
2. **S2 — shim**: C library, open family + ENSURE, Docker validation of
   the libc tool matrix.
3. **S3 — exec gate**: classification + 3 policies, env re-injection.
4. **S4 — mtime + .git**: manifest field, skeleton mtimes, `--include-git`
   with scrub.
5. **S5 — billy adapter**: `server/billyfs`, go-git demo + laziness tests.
6. **S6 — Fargate validation**: harness as a Fargate task; document.

Reserved (not scheduled; foundation for write-back, tracked as its own
issue): **namespace-syscall interception** (rename/unlink/mkdir/chmod…) per
§4.1 safeguard 2, to turn the `local` set into a complete change ledger.

S1–S2 deliver standalone value (lazy libc tools); S4–S5 deliver git.

## 11. Open questions

- **seccomp-unotify spike** (tracked separately): if a Fargate task can
  install a user-notification filter + `ADDFD`, a uniform syscall-level
  supervisor could replace the shim *and* the exec gate and cover Go
  binaries. Shimmer's supervisor/materializer/state-table layers are
  mechanism-agnostic and would be reused as-is; only §4–5 would retire.
  Run the spike before S3 — a positive result reshapes the plan.
- Placeholder representation: sparse files assumed; confirm the sandbox
  filesystem (Fargate ephemeral storage) preserves sparseness; fall back
  to truncated-empty + size-from-manifest-in-Stat if not.
- `.git/config` scrub: parse with `gcfg`-style parser vs regex; decide in
  S4 review.
- Should `materialize` policy prefetch in manifest order or fetch the
  whole `.caidx` via desync's bulk path? (Reuse-over-reinvent review.)
