# Running Mirage on AWS Fargate (and why it's hard)

**Audience:** a software engineer who has never heard of Fargate, FUSE,
LD_PRELOAD, or "syscall interception." Everything is explained from scratch.
Read `how-mirage-works.md` first if you haven't — this doc assumes you know
what the Mirage client and server do, but nothing else.

If you only remember one sentence: **Fargate forbids the kernel feature
(FUSE) that Mirage's lazy file mount needs, and every workaround trades away
either correctness or laziness — the only fully-correct cheap option is to
run on EC2 instead.**

---

## 1. Quick recap: what the Mirage server needs to do

The Mirage server runs in a cloud Linux machine. Its job is to make a
directory like `/workspace` *look* like it contains your whole project, while
actually fetching file contents from your laptop only when a program reads
them.

The server has two modes today:

- **Reconstruct mode (`--out`)** — download *everything* up front and write
  real files to disk. Simple, always works, but not lazy: you pay the full
  transfer cost before any work starts.
- **Mount mode (`--mount`)** — the interesting one. It uses **FUSE** to
  present a fake directory whose contents materialize on demand.

To understand the Fargate problem you need to know what FUSE is.

## 2. What FUSE is, in one minute

Normally, filesystems live *inside* the Linux kernel (ext4, NFS, etc.). When
a program calls `read()`, the kernel answers directly.

**FUSE** ("Filesystem in USErspace") is a kernel feature that says: *"for
this one directory, don't answer yourself — forward every filesystem
operation to a normal user program, and relay back whatever it replies."*

That user program is, for us, the Mirage server. When `grep` reads
`/workspace/main.go`, the kernel forwards the read to Mirage, Mirage fetches
the needed chunks from your laptop, and hands the bytes back. `grep` has no
idea any of this happened — it thinks it read a local file.

This is the gold standard for our problem, for one crucial reason: **FUSE
sits below every program.** It doesn't matter what language a tool is written
in or how it does I/O — every read funnels through the kernel, and the kernel
funnels it to us. Nothing can accidentally bypass it.

But there's a catch: *attaching* a FUSE filesystem to a directory is a
**mount**, and mounting is a privileged operation. Concretely you need:

1. access to the device file `/dev/fuse`, and
2. the Linux **capability** `CAP_SYS_ADMIN` (capabilities are fine-grained
   slices of root power; `CAP_SYS_ADMIN` is the one that covers mounting),
   or a fully "privileged" container.

On your own Linux box or VM, that's a non-issue — you're root. Which brings
us to Fargate.

## 3. What Fargate is, and what it forbids

**AWS Fargate** is "serverless containers": you hand AWS a container image,
and AWS runs it. You never see or manage the underlying machine. That's the
appeal — no servers to patch or scale.

The price of that convenience is that AWS locks the container down hard,
because your container shares AWS-managed infrastructure. The relevant rules
(all confirmed in AWS's own docs as of mid-2026):

| What | On Fargate? |
|---|---|
| `privileged` containers | ❌ not allowed |
| Mapping device files like `/dev/fuse` | ❌ not allowed |
| Adding capabilities | ❌ — with **one exception**: `CAP_SYS_PTRACE` (see §6) |
| Custom seccomp profiles | ❌ not allowed |

Cross-reference the list in §2: FUSE needs `/dev/fuse` and `CAP_SYS_ADMIN`,
and Fargate grants neither. There is no flag, setting, or workaround that
changes this — it applies to ECS-on-Fargate and EKS-on-Fargate alike. (AWS's
own `mountpoint-s3` FUSE client fails on Fargate for exactly this reason.)

So: **Mirage's `--mount` mode cannot run on Fargate. Period.**

`--out` reconstruct mode runs fine (it just writes ordinary files), and the
networking model is also fine — the Mirage *client* dials out to the server,
so the Fargate task only needs an ordinary inbound listener behind a load
balancer. The blocker is purely the mount privilege.

Everything below is about what you can do *instead* of FUSE if Fargate is a
hard requirement.

## 4. The suggested workaround: LD_PRELOAD

Here's the idea that was suggested to us, and it's a clever one. To follow
it you need two pieces of background.

**Piece 1: programs usually don't talk to the kernel directly.** When a C
program (or Python, or grep) opens a file, it calls a function like `open()`
from **libc** — the C standard library, a shared library that almost every
Linux program loads at startup. libc is a thin wrapper: *your code →
libc's `open()` → the kernel's actual `open` system call.*

**Piece 2: LD_PRELOAD lets you cut in line.** `LD_PRELOAD` is an environment
variable understood by Linux's program loader. It says: *"before loading a
program's normal libraries, load my library first."* If your library defines
a function named `open`, the program finds **yours** before libc's. Your
function gets called instead — and it can do anything it wants, including
calling the real `open` afterwards. This is called **interposition**, and
it's a legitimate, decades-old technique.

The proposed design, then:

> Ship a small shared library and set `LD_PRELOAD` for every process in the
> sandbox. The library intercepts `open`, `read`, `stat` (get file
> metadata), `readdir` (list a directory), etc. For paths under
> `/workspace`, it answers from Mirage's chunk store — fetching from the
> laptop on demand. For every other path, it passes through to the real
> libc. No mount, no privileges, runs on Fargate today.

And the claim — *"most tools (python, node, grep, shell) work unmodified"* —
is **true as far as it goes**. We verified it: Python, grep, coreutils, the
shell, git, Java, and (under current defaults) Node.js all do their file I/O
through libc, so interposition catches them.

The problem is what the claim leaves out. There are three holes, and one of
them is disqualifying for a developer-workspace product.

## 5. The three holes in LD_PRELOAD

### Hole 1: Go programs are invisible to it (this is the fatal one)

LD_PRELOAD can only intercept calls that go *through libc*. Programs written
in **Go don't use libc for file I/O at all** — the Go runtime contains its
own assembly that issues system calls straight to the kernel. This is true
even when the Go binary is dynamically linked. `os.Open` in Go never touches
the `open` symbol our library overrides.

Why this is fatal rather than annoying: think about what runs in a dev
sandbox. `go build`, `gopls`, plus a huge slice of modern dev tooling that
happens to be written in Go — docker, kubectl, terraform, gh, and friends.
Every one of them would look at `/workspace`, bypass our shim, see the
**real** directory underneath — which is empty — and conclude the project
has no files.

Note the failure mode: **not an error, but silently wrong data**. A profiler
that misses some calls is lossy; a *filesystem* that misses some calls is
lying. The same applies to any statically-linked binary (common for
distributed CLI tools), since static binaries don't load shared libraries at
all and so never see `LD_PRELOAD`.

### Hole 2: you must intercept ~40+ functions, and even that isn't airtight

"Intercept open, read, stat, readdir" sounds like four functions. In
practice:

- libc has many variants of each: `open`, `open64`, `openat`, `openat64`,
  `creat`, the `fopen` family, `stat`/`lstat`/`fstat`/`fstatat`/`statx`,
  plus *legacy* symbol names (`__xstat` and friends) that binaries compiled
  against older libc versions still call. Miss one variant and the tools
  using it slip through.
- Worse, **libc calling itself bypasses you entirely**. When a program calls
  `fopen`, libc internally calls its own private `open` through an internal
  alias — not through the public symbol you overrode. So you can't intercept
  one "choke point"; you must separately wrap every public entry door, and
  some doors (like the newer `openat2` syscall) have no libc wrapper to
  override at all.
- New libc releases add new variants. This is a permanent maintenance
  treadmill, and projects that tried it (fakechroot is the best-documented
  example) have a long public history of breaking whenever libc or coreutils
  evolved.

### Hole 3: `mmap` — some reads never call `read()` at all

`mmap` ("memory map") is a way to read a file without read calls: a program
asks the kernel to map a file directly into its memory, then just accesses
that memory like an array. The kernel fills pages in on demand. **No
`read()` ever happens**, so there is nothing for our library to intercept.

Tools that read this way include git (its pack files), ripgrep, and the
clang compiler and LLD linker. For these, the only correct move is: at
`open()` time, fully download the file, write it to a local cache, and hand
back a **real** file descriptor to that real file. Then `mmap`, `read`, and
everything else work naturally, because the bytes genuinely exist on disk.

That design is called **materialize-on-open**, and it changes Mirage's
granularity: instead of fetching individual *chunks* of a file as they're
read, we fetch *whole files* the first time they're opened. That's still a
big win — measurements in the container world (the Slacker paper) found that
typical workloads read only ~6% of available data — but huge files (large
binaries, ML weights) lose their partial-read benefit.

### The verdict from prior art

We surveyed everyone who tried LD_PRELOAD-as-a-filesystem (physics-grid
shims like XRootD and dCache, fakechroot, the Darshan I/O profiler). The
pattern is consistent: it survives as a *convenience layer for cooperative,
dynamically-linked tools*, and **every project that needed correctness moved
down to syscall-level interception or into the kernel**. The rr debugger's
authors said it plainly: library preloading is "insufficient (applications
make direct system calls) and fragile."

## 6. Two stronger alternatives that do run on Fargate

Both of these intercept at the **system-call layer** — below libc — so they
catch *everything*, including Go and static binaries. They're more work to
build, but they don't have Hole 1 or Hole 2.

### Option A: ptrace (proven on Fargate)

`ptrace` is the kernel facility debuggers use: a supervisor process can stop
another process at every system call, inspect it, and rewrite it. A
supervisor could catch every `open` of `/workspace/...`, materialize the
file, and redirect the call — the same trick the `proot` tool uses to fake a
chroot without privileges.

Why it's interesting for Fargate: `CAP_SYS_PTRACE` is the **one and only**
capability AWS lets you add to a Fargate task (platform version 1.4+, ECS
only). It's proven territory — the Falco security monitor runs on Fargate in
production using exactly this mechanism (their `pdig` tool).

Cost: ptrace stops the traced process at *every* syscall, which is slow —
measured overheads range from ~10% to ~80% on syscall-heavy workloads
(an optimization using seccomp to skip boring syscalls gets it to ~9–25%,
but we haven't verified that optimization works on Fargate).

### Option B: seccomp user notification (the elegant one, but unproven there)

**seccomp** is a kernel feature for filtering system calls. Its modern
extension, "user notification," lets a supervisor process *service* a
syscall on another process's behalf: when the target calls `open()` on a
watched path, the kernel pauses it and asks the supervisor, which can open
(or create) the real file itself and **inject the resulting file descriptor
directly into the target process**. One kernel developer aptly described
the mechanism as "FUSE for syscalls."

It's a near-perfect fit for materialize-on-open, and unlike ptrace it only
pays overhead on the syscalls you actually filter. Installing the filter
requires *no* special capability.

The open question: Fargate's kernel (Amazon Linux 2, 5.10) has every needed
feature, but nobody has publicly demonstrated this mechanism on Fargate, and
Fargate does restrict some kernel surfaces. **A one-day experiment — a tiny
Fargate task that installs a filter and injects an fd — would settle it.**
If it works, this is the best FUSE-free architecture available.

## 7. A second suggestion: a custom Go `fs.FS`

Another idea that came up: Go has a standard filesystem *interface*,
`io/fs.FS` (added in Go 1.16). It's a small contract — "give me a thing with
an `Open(name)` method and I'll read files from it" — and parts of the Go
standard library accept one (HTTP file servers, template parsing, zip/tar
writers). So: could Mirage implement a custom `fs.FS` backed by its chunk
store, and skip FUSE entirely?

The crucial thing to understand is **what layer this lives at**. FUSE and the
options in §6 sit *below* programs, in the kernel's syscall path — they work
on any process, in any language, unmodified. An `fs.FS` is the opposite: it's
an **in-process library interface**. It only exists inside one Go program's
memory, and only code *written to accept it* ever sees it. Three hard limits
follow:

1. **Go-only, and opt-in only.** There is no way to point an *unmodified* Go
   binary at a custom `fs.FS`. Go's `os.Open` does not route through `fs.FS`
   — the dependency goes the other way (`os.DirFS` is a thin wrapper over
   `os.Open`). The program's author must have written `func Work(fsys fs.FS)`
   and let you inject yours. `go build`, `gopls`, kubectl etc. were not.
2. **It dies at the process boundary.** An `fs.FS` is an interface value in
   one process's heap. The moment you `exec` a child process — `python`,
   `grep`, anything — that child talks to the kernel, not to your Go
   interface. There is no mechanism, existing or proposed, to hand an
   `fs.FS` to a subprocess. A sandbox whose whole point is running arbitrary
   tools is *made of* subprocesses.
3. **Read-only, with optional random access.** `io/fs` has no write support
   (the proposal for it was never accepted), and seeking/`ReadAt` are
   optional extensions consumers can't rely on — our implementation would
   need to provide them explicitly.

So as a *replacement* for the FUSE mount, the answer is simply **no** — it
can't serve the sandbox use case, on Fargate or anywhere else. Where
LD_PRELOAD (§4–5) was "the right layer with leaky coverage," `fs.FS` is
"perfect coverage of a very thin slice, at the wrong layer."

But it's worth keeping on the shelf, because the thin slice is genuinely
nice:

- **If the sandbox workload is a Go program we control** — say, an AI agent
  runtime written in Go — it could read the workspace through a `MirageFS`
  directly, with **full per-chunk laziness** (better than the
  materialize-on-open compromise in §5), zero privileges, zero kernel
  involvement. Works on Fargate today.
- **It's cheap to build.** Mirage's `server/fuse` package already has the
  pure pieces (`IndexFromRefs`, `ReadRange` — chunk-granular random reads
  over the store chain, which is already safe for concurrent use). A
  read-only `fs.FS` over the manifest is roughly 300 lines, about the size
  of the FUSE layer itself.
- **Some real Go libraries accept a pluggable filesystem** — notably go-git
  (via its `billy.Filesystem` interface), meaning in-process git operations
  over a virtual workspace are possible. That's a plausible future "git
  fast-path."

One related dead end to record: "run a userspace NFS server in the
container and mount it on localhost" comes up in this design space (there
are good Go libraries for it). It doesn't help on Fargate — *mounting* NFS
is still a privileged mount, the same `CAP_SYS_ADMIN` wall as FUSE.

## 8. Can we combine LD_PRELOAD and fs.FS?

A natural follow-up: do the two suggestions compose into something with
better coverage? **Not by themselves.** Look at what each covers:
LD_PRELOAD covers third-party libc tools; `fs.FS` covers Go code *we write*.
But LD_PRELOAD's fatal gap was third-party Go and static binaries — and
`fs.FS` does nothing for those (you can't inject a Go interface into someone
else's compiled binary). The two slices are complementary but the dangerous
hole sits in neither, so the silent-wrong-data failure mode survives the
combination untouched.

What rescues the idea is a third piece: an **exec gate**. The binaries we
can't intercept can still be *detected at launch time* — and we control what
the filesystem looks like before they run. The full hybrid:

1. **Our own Go agent** (the sandbox entrypoint) reads the workspace through
   `MirageFS` in-process — per-chunk lazy, no privileges (§7).
2. **libc-based tools it spawns** (python, grep, git…) get the `LD_PRELOAD`
   shim doing materialize-on-open: intercept `open` under `/workspace`,
   download the whole file into the *real* directory, return a real fd
   (which also neutralizes the mmap problem, §5.3). At startup, pre-create
   the **directory skeleton** (dirs, names, modes from the manifest) so
   `ls`/`find`/`stat` work natively for every binary with zero interception.
3. **The exec gate:** the shim also wraps `execve`. Before launching a
   child, inspect the target binary: dynamically linked against libc →
   proceed lazily. Static or Go-built → **pause, fully materialize the
   workspace, then exec.** The Go tool runs against a complete real tree.

The key property: the failure mode flips from *silently wrong* to *merely
slower*. A Go binary never sees a half-empty directory; worst case, the
first `go build` pays a full sync — i.e. it degrades to `--out` mode, but
only if and when a non-interceptable tool actually touches the workspace.
Laziness degrades gracefully: per-chunk (our agent) → per-file (libc tools)
→ full materialize, once (Go/static tools).

Honest caveats:

- **A Go-heavy workload hits the gate immediately** (`go build`, `gopls`),
  at which point this is an elaborate `--out` mode. Know the workload first.
- **Detection must fail closed**: materialize unless the binary is *provably*
  libc-dynamic. A false positive costs laziness; a false negative brings
  back silent wrong data.
- **Environment scrubbing** (`env -i`, setuid) still drops `LD_PRELOAD` from
  children — the skeleton makes that loud (missing files) rather than
  silent, but it's not airtight.
- If the seccomp-notification experiment (§6B) succeeds, it does all of this
  with one uniform mechanism and no ELF-sniffing. This hybrid is the
  fallback if that fails and EC2 is off the table.

### 8.1 A concrete special case: a Go git client + libc tools only

If the sandbox workload narrows to "one Go git client we control, plus
arbitrary libc tools," the hybrid gets much stronger — this removes its
biggest weakness. The exec gate existed to guard against *third-party* Go
binaries; when the only Go program is ours, it shrinks to a cheap assertion
(refuse to exec unknown static binaries) instead of a full-materialize
fallback.

The git client doesn't need LD_PRELOAD at all: **go-git** (the standard
pure-Go git library) is built on a pluggable filesystem interface,
`billy.Filesystem`. We hand it a Mirage-backed implementation and git reads
the workspace in-process with per-chunk laziness — `billy.File` requires
`ReadAt`/`Seek`, which map directly onto the existing `ReadRange`
(`server/fuse/read.go`). Pack-file access (seeky reads into large files) is
exactly where per-chunk faulting shines.

Consistency between the two halves comes from a **shared overlay**: a local
directory holding everything materialized by the shim or written by any
tool. Both the shim and the billy adapter check the overlay first and fall
through to the chunk store on miss — so `git status` sees the edit a Python
script just made.

Three design decisions before building:

1. **Where the repo data comes from.** The indexer currently *excludes*
   `.git` entirely (`client/index/index.go`), so the manifest has no git
   data. Either index `.git` behind a flag (laptop's real repo state flows
   lazily; but `.git/config` can embed credentials in remote URLs and needs
   a content-aware scrub, and hooks must not run server-side), or clone from
   origin into a plain local dir (simpler security, but doesn't match the
   laptop's uncommitted state and pays a real clone).
2. **Add mtimes to the manifest — the performance linchpin.** `git status`
   and `git diff` hash the *entire worktree*, which over a lazy FS faults
   every chunk of every file — a stealth full materialization. Git avoids
   this via its index shortcut (size+mtime match → skip hashing). The
   manifest carries no mtime today; adding it (and indexing the laptop's
   `.git`, whose index mtimes match those same files) makes status a
   metadata-only walk. Without it, the lazy win quietly evaporates on the
   most common git command.
3. **Writes stay local** in the overlay (consistent with write-back being
   out of scope). Caveat: the indexer skips symlinks, so repos containing
   them show phantom deletions in status.

Effort: the billy surface is larger than `fs.FS` (OpenFile, Rename, Remove,
Lstat, TempFile, Chroot…), so adapter + overlay is roughly 600–900 LOC, plus
a small manifest change for mtime and the shim as already scoped. All pure
Go, zero privileges — runs on Fargate today.

## 9. Summary and recommendation

| Approach | Catches Go / static binaries? | Keeps per-chunk laziness? | Works on Fargate? | Effort |
|---|---|---|---|---|
| FUSE mount (current `--mount`) | ✅ everything | ✅ | ❌ **forbidden** | already built |
| Reconstruct (`--out`) | ✅ (real files) | ❌ none — full upfront copy | ✅ | already built |
| LD_PRELOAD shim | ❌ silently misses them | ❌ per-file (materialize-on-open) | ✅ | medium + permanent upkeep |
| ptrace supervisor | ✅ | ❌ per-file | ✅ proven (Falco) | high |
| seccomp user notification | ✅ | ❌ per-file | ❓ likely, needs a live test | high |
| Custom Go `fs.FS` | ❌ only Go code written to accept it; no subprocesses | ✅ per-chunk | ✅ | low (~300 LOC) |
| Hybrid: fs.FS + shim + exec gate (§8) | ✅ correct for all (Go tools trigger full materialize) | mixed: per-chunk → per-file → none | ✅ | high |

Recommendation, in order:

1. **If the launch type is negotiable, use ECS on EC2 and keep FUSE.** Same
   task definitions, but you control the host, so `/dev/fuse` and
   `CAP_SYS_ADMIN` are available. The mount code already exists and is
   validated; everything else in this doc is a workaround for one AWS
   restriction.
2. **If Fargate is a hard requirement**, build materialize-on-open backed by
   the existing chunk store (the store chain needs no changes), and choose
   the interception layer by stakes: LD_PRELOAD for a quick prototype with a
   *documented* "Go tools see an empty directory" blind spot; ptrace or
   seccomp-notification for correctness. Run the seccomp-on-Fargate
   experiment first — it's cheap and decides the architecture.
3. **`--out` reconstruct mode is the zero-risk fallback** that ships on
   Fargate today, if upfront transfer cost is acceptable.

---

*Researched June 2026. Key facts verified against AWS ECS documentation
(capabilities, devices, privileged restrictions), Go/glibc/libuv source
code, and the projects named above. See `fargate-interception-options` in
the project memory for the condensed version with links.*
