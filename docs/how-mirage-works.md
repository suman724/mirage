# How Mirage Works (a plain-language tour)

**Audience:** a software engineer who has *never* heard of desync, "content-defined
chunking," FUSE, or "single-flight." This document explains the ideas from
scratch, then shows how Mirage wires them together. No prior background needed.

If you only remember one sentence: **Mirage lets a program running in a cloud
Linux machine read files from your laptop as if they were local — without
copying the whole project up front, and without the cloud machine ever being
able to "call into" your laptop uninvited.**

---

## 1. The problem we're solving

Imagine an AI coding agent (or any tool) that runs in a **cloud sandbox** — a
fresh, throwaway Linux machine in a data center. You want it to work on *your*
project, which lives on *your* laptop (Windows or Mac).

Two naive approaches both fail:

1. **Upload the whole repo to the cloud.** A large monorepo is gigabytes. Your
   home upload speed makes this painfully slow, and you re-pay that cost every
   session. Most of those files the agent will never even open.
2. **Let the cloud machine reach back into your laptop over the network.** Your
   laptop sits behind a home router/firewall (NAT). Letting a server on the
   internet open connections *into* your machine is both hard (firewalls block
   it) and a security nightmare.

Mirage's answer: the cloud machine sees what *looks* like a complete local
filesystem, but the file contents are fetched **on demand, only when actually
read**, and they travel over a connection that **your laptop opened outward** —
never one opened inward. Hence the name: a *mirage* — a filesystem that appears
to be there but materializes only when you touch it.

There are two roles throughout this doc:

- **Client** — the thin program on your laptop. It holds the real files.
- **Server** — the program in the cloud sandbox. It wants to read the files.

---

## 2. Building block #1: content-addressed "chunks"

### The everyday way to name a file

Normally we identify a file by its **path**: `src/main.go`. The path says
*where* it is, not *what's in it*.

### The Mirage way: identify data by its content

Take any blob of bytes and run it through a **hash function** — a math function
that turns any input into a fixed-size fingerprint (Mirage uses SHA-512/256,
which yields a 32-byte fingerprint). The key properties:

- The same bytes always produce the same fingerprint.
- Different bytes (in practice) produce different fingerprints.

So instead of asking "give me `src/main.go`," you can ask "give me the blob
whose fingerprint is `a3f9…`." This is **content addressing**: the data's *name
is its content's fingerprint*. We call that fingerprint a **hash** (type
`chunk.Hash` in the code).

Why this matters for Mirage:

- **Deduplication.** If two files (or two copies of one file) contain identical
  bytes, they have the same hash — so you store and transfer that data **once**.
- **Verification.** When the server receives some bytes claiming to be chunk
  `a3f9…`, it re-hashes them. If the fingerprint doesn't match, the data was
  corrupted or tampered with, and it's rejected.
- **Security boundary** (more in §7): the server can only ask for fingerprints
  it has been *told about*. It can't ask for "`~/.ssh/id_rsa`" because it never
  learns that file's fingerprint.

### Why "chunks," not whole files

We don't hash whole files; we split each file into **chunks** (typically tens of
kilobytes each) and hash each chunk. Why bother?

Consider a 100 MB file where you change one line. If the unit of dedup were the
whole file, that one-line edit changes the file's hash, and the *entire* 100 MB
counts as "new." If instead the file is split into small chunks, only the chunk
containing your edit changes; every other chunk keeps its old fingerprint and is
already known. You transfer kilobytes, not 100 MB.

### How do we split? "Content-defined chunking" (CDC)

The obvious way to split is into fixed 64 KB blocks. This has a fatal flaw: if
you *insert* one byte near the start of a file, every fixed block after it
shifts by one byte, so every block's content — and therefore every hash —
changes. Dedup collapses to nothing. This is the "boundary-shift" problem.

**Content-defined chunking** fixes it by choosing chunk boundaries based on the
*data itself*, not on byte position. As it scans the bytes, it maintains a
rolling fingerprint of a small sliding window and cuts a boundary whenever that
fingerprint hits a certain pattern. Because the boundary is decided by local
content, inserting a byte only disturbs the one chunk around the insertion — the
boundaries elsewhere fall in exactly the same places as before. Dedup survives
edits, insertions, and shifts.

Mirage does **not** implement CDC by hand. It uses a mature, well-tested Go
library called **[`desync`](https://github.com/folbricht/desync)** (a Go port of
the `casync` tool). desync gives us the chunker, the chunk-hashing, and a lot
more we'll meet later. In Mirage this lives behind one small function,
`chunk.Split(data)`, which returns the ordered list of chunk fingerprints plus
the chunk bytes. Mirage's chunk sizes: ~16 KB minimum, ~64 KB average, ~256 KB
maximum.

---

## 3. Building block #2: the manifest (a.k.a. the index)

Splitting files into chunks gives us a pile of fingerprinted blobs. To
reconstruct a file we also need a recipe: *which chunks, in what order, make up
which file*. That recipe is the **manifest** (desync calls its on-disk form an
"index"; the wire protocol field is named `caidx` for that reason).

In Mirage the manifest is small JSON, roughly:

```json
{
  "files": [
    { "path": "src/main.go", "mode": 420,
      "chunks": [ {"hash": "a3f9…", "size": 47220}, {"hash": "5736…", "size": 55} ] },
    { "path": "docs/README.md", "mode": 420,
      "chunks": [ {"hash": "2d58…", "size": 97} ] }
  ]
}
```

It lists every file, its permission bits, and the ordered fingerprints of its
chunks. Crucially, it is **tiny** compared to the actual file contents — it's
just a list of hashes. The client sends this manifest **up front**. The chunk
*contents* are fetched later, lazily, only as needed.

(Code: `internal/chunk` defines `Hash`, `Ref`, `FileEntry`, and `Manifest`.)

---

## 4. Building block #3: a "Store" — fetching a chunk by its hash

Everything that needs chunk bytes goes through one tiny abstraction: **give me
the chunk with this fingerprint.** desync calls this interface a `Store`:

```
GetChunk(id)  ->  the bytes of the chunk named `id`
```

The beauty of this is that *where* the bytes come from is hidden behind the
interface. desync ships Stores that fetch from disk, HTTP, S3, etc. Mirage
writes **its own Store** whose `GetChunk` does something unusual: it asks for the
chunk **over the network connection the laptop opened** (see §5). That custom
Store is the single bridge between "a filesystem read" and "a network request,"
and it's the heart of the whole system.

---

## 5. Building block #4: the connection (who dials vs. who asks)

This is the cleverest part, so we'll go slowly.

### The constraint

- Your laptop is behind NAT/a firewall. Nothing on the internet can open a
  connection *into* it.
- But the cloud server is the one that needs data *from* the laptop.

These seem contradictory: the side that needs to *pull* data is the side that
isn't allowed to *connect*.

### The resolution: separate "who opens the socket" from "who sends requests"

The trick is that a network connection, once open, is a **two-way street**.
Whoever opened it doesn't have to be the only one who sends messages on it.

So in Mirage:

1. The **client (laptop) dials out** and opens **one** long-lived connection to
   the server. This is an ordinary outbound connection — firewalls allow it,
   just like loading a web page. (Mirage uses **gRPC bidirectional streaming**:
   think of it as a single open pipe over which either end can send typed
   messages at any time. gRPC is a popular RPC framework; "bidi streaming" is its
   mode where both sides can keep sending messages on one call.)
2. Once that pipe is open, the **server sends requests *down* it.** When the
   server needs a chunk, it sends a `ChunkRequest` message toward the client. The
   client answers with a `ChunkResponse` carrying the bytes.

So the **client opens the socket**, but the **server originates the chunk
requests** on it. Outbound-only is satisfied, and the server still gets to pull
data. This is the same proven pattern used by reverse SSH tunnels, language
servers (LSP), and self-hosted CI runners.

```
   LAPTOP (client)                                  CLOUD (server)
   has the real files                               wants to read files
        │                                                  ▲
        │   1. dials OUT, opens one bidi pipe ───────────► │
        │                                                  │
        │ ◄──── 2. server sends ChunkRequest(hash) ─────── │   "I need chunk a3f9…"
        │                                                  │
        │ ───── 3. client sends ChunkResponse(bytes) ────► │   "here are its bytes"
        │                                                  │
   The client dialed. The server asks. Data flows up the pipe the laptop opened.
```

(Code: only `client/transport` is ever allowed to *dial*; `server/transport`
only ever *accepts* and then drives the protocol over the accepted stream.)

### The message types

The protocol (defined once in `proto/mirage/v1/mirage.proto`) has two envelopes:

- **Client → server:** `Hello` (handshake), `IndexPublish` (the manifest), and
  `ChunkResponse` (chunk bytes in answer to a request).
- **Server → client:** `HelloAck`, and `ChunkRequest` (please send these
  fingerprints).

Each `ChunkRequest` carries a **`request_id`**. The matching `ChunkResponse`
echoes that id. That's how many in-flight requests can share the one pipe
without getting confused (see §8).

---

## 6. Putting it together: the end-to-end flow

Here's everything that happens in the milestone we've built, start to finish:

1. **Index.** The client walks the chosen directory, skips secrets and junk
   (§7), splits each remaining file into chunks (CDC), and builds (a) the
   manifest and (b) an in-memory store of `hash → bytes`. (`client/index`)
2. **Dial.** The client opens the single outbound gRPC stream to the server and
   sends `Hello`. The server replies `HelloAck`. (`client/transport` →
   `server/transport`)
3. **Publish.** The client sends `IndexPublish` carrying the manifest (the small
   list of hashes). The server now knows the file tree and which chunks compose
   each file — but has **no bytes** yet.
4. **Fault.** The server walks the manifest and, for each chunk it needs, calls
   `GetChunk(hash)` on its store. That store sends a `ChunkRequest` down the open
   pipe. (`server/channelstore`)
5. **Serve (or reject).** The client looks up the requested hash in the store it
   built at step 1. If present, it replies with the bytes. If the hash was never
   published (e.g. a secret), it **rejects** the request. (`client/chunkstore`,
   `client/transport`)
6. **Verify & assemble.** The server checks the returned bytes hash to the
   fingerprint it asked for, then appends them. When all of a file's chunks are
   in, it writes the file out. (`server/transport`)
7. **Done.** When every file is reconstructed, the server closes the stream; the
   client sees the close and exits.

The automated integration test (`test/`) runs exactly this over real localhost
gRPC and asserts the reconstructed tree is **byte-for-byte identical** to the
source — and that the server obtained the data **only** via chunk requests,
never by reading the source directory (it has no access to it).

---

## 7. Why this is safe (the security model)

Two properties do the heavy lifting:

1. **The server can only ask for hashes the client published.** The protocol is
   *fingerprint-based, not path-based*. The server literally cannot say "send me
   `/etc/passwd`" — it can only name 32-byte fingerprints, and the client only
   answers for fingerprints in the manifest it chose to publish. Anything else is
   rejected. (You can see this rejection in `client/transport`.)
2. **Secrets are excluded before they're ever chunked.** At index time the
   client skips a denylist — `.env*`, `*.pem`, `*.key`, `id_*` (SSH keys),
   `.netrc`, credential files — plus noise like `.git/` and `node_modules/`.
   Because those files are never split into chunks, their fingerprints never
   enter the manifest or the store, so the server can never request them. The
   security boundary lives on the client, where the files actually are.
   (`client/index.IsSecret`.)

Our test tree includes a fake `.env` and `id_rsa`; the tests confirm they are
**never** transferred or reconstructed.

(Out of scope for this milestone but planned: TLS encryption, authentication
tokens, and write-back conflict checks.)

---

## 8. Caching, deduplication, and concurrency — for free

A lazy filesystem has one tax: the **first** read of a chunk costs a network
round-trip. Three things keep that tax small, and Mirage gets all three from
desync rather than hand-rolling them:

- **Local cache.** Once a chunk arrives, the server keeps a copy on local disk.
  Re-reading it (or reading another file that shares it) is free — no network.
  This is desync's `Cache` store fronting our network store
  (`desync.NewCache(…, LocalStore)`).
- **Deduplication.** Because chunks are content-addressed, two identical files
  share chunks automatically. In our test tree two files are byte-identical, so
  the server fetches their shared chunk **once** over the wire and serves the
  second occurrence from cache. (You can see this in the logs:
  `chunk_requests: 8, cache_hits: 1` for 9 total chunk references.)
- **"Single-flight" for concurrency.** Reads happen in parallel — many file
  reads can need chunks at the same moment. If two readers ask for the *same*
  not-yet-cached chunk simultaneously, we don't want two identical network
  fetches. **Single-flight** means: the first request goes out; any others for
  the same fingerprint *wait* and share its result. This is desync's
  `DedupQueue`. Combined with the `request_id` correlation (§5), many concurrent
  faults multiplex correctly over the single pipe — proven by a 200-way
  concurrency test run under Go's race detector.

So the server's "store" is actually a small stack:

```
   GetChunk(hash)
        │
        ▼
   desync Cache          ← already on local disk? return it (a "cache hit")
        │  (miss)
        ▼
   desync DedupQueue     ← someone already fetching this exact hash? wait & share
        │  (first asker)
        ▼
   channelstore          ← send a ChunkRequest down the open pipe, await bytes
```

Every layer here is a `desync.Store`, so they snap together like Lego. Mirage
only wrote the bottom one (`channelstore`); the cache and dedup layers are
desync's.

---

## 9. Building block #5: FUSE — making reads *feel* local

So far the server *reconstructs whole files* by faulting all their chunks. The
real goal is subtler: the agent should be able to run normal code —
`open()`, `read()` — on `workspace/foo.py` and have the bytes appear, faulting
chunks **only for the parts actually read**.

That's what **FUSE** ("Filesystem in Userspace") provides. Normally a filesystem
is kernel code. FUSE lets a *normal program* implement a filesystem: you mount
it at a directory, and whenever anything reads a file under that directory, the
kernel calls back into your program to ask "what are the bytes at offset N?"
Your program can compute or fetch them however it likes.

Mirage's FUSE layer (`server/fuse`) builds a directory tree from the manifest —
real-looking folders and files — but each file is empty until read. When
something reads bytes `[offset, offset+length)` of a file, Mirage:

1. figures out which chunk(s) of that file overlap that byte range (from the
   manifest's chunk list),
2. fetches just those chunks through the store stack of §8 (cache → dedup →
   network),
3. copies the requested slice back to the reader.

Read one byte of a 100-chunk file and you fault exactly **one** chunk — that
laziness is unit-tested (`ReadRange`). Read the same bytes again and the local
cache (and the kernel's own page cache) serve them with no network at all.

> **Status note.** The FUSE *code* is written and the read logic is unit-tested.
> Actually *mounting* requires a FUSE kernel module, which isn't present on the
> macOS dev machine (it needs macFUSE + a reboot). So the live mount test skips
> locally and is set up to run in a Linux container (`make fuse-validate`),
> which matches the real cloud-sandbox target. That validation is the one
> remaining step for this part.

---

## 10. The code, mapped to the ideas

| Idea (this doc) | Where it lives |
|---|---|
| Chunk hash, manifest, CDC split | `internal/chunk` (uses desync) |
| Walk dir, exclude secrets, build manifest + store | `client/index`, `client/chunkstore` |
| Dial out, publish, answer/reject chunk requests | `client/transport` |
| Accept the connection, drive the protocol, reconstruct | `server/transport` |
| The custom Store that fetches over the pipe | `server/channelstore` |
| Cache + dedup + single-flight (reused from desync) | desync `Cache` + `DedupQueue` in `server/transport` |
| Lazy POSIX reads via a mounted tree | `server/fuse` |
| The wire protocol (single source of truth) | `proto/mirage/v1/mirage.proto` |
| Structured logging | `internal/logging` |
| End-to-end test over real gRPC | `test/` |

Two runnable binaries tie it together: `client/` (dials out, serves chunks) and
`server/` (accepts, reconstructs). Both take `--log-level`/`--log-format`.

---

## 11. Try it yourself

```bash
make build          # builds bin/mirage-server and bin/mirage-client
make test           # unit + integration tests
make test-race      # the same under the race detector

# Two terminals, a real localhost run:
make run-server     # accepts a connection, reconstructs into ./mirage-out
make run-client     # dials out, publishes testdata/workspace
diff -r testdata/workspace ./mirage-out   # only the excluded secrets differ
```

Add `--log-level debug` to the client to watch individual chunks being served,
and to the server to watch cache hits vs. network faults.

---

## 12. What's built vs. what's next

**Built and validated:** content-defined chunking (desync), the manifest, the
outbound-only client-dials/server-asks connection, fingerprint-based fetch with
verification, secret exclusion, the cache + dedup + single-flight store stack,
and the FUSE read logic. End-to-end reconstruction is byte-for-byte correct, and
the whole suite passes under the race detector.

**Next (not yet done):**

- Run the **live FUSE mount** in a Linux container to validate real POSIX reads
  end to end (`make fuse-validate`), plus a small "stub harness" that reads the
  mount.
- **git fast-path:** when the workspace is a git repo with a remote, the sandbox
  clones the bulk from the git host at cloud speed and the client only ships the
  *uncommitted* changes.
- **Write-back:** edits made in the sandbox flow back to the laptop, with
  conflict detection and a user prompt.
- **Search index:** so a repo-wide search doesn't accidentally fault every file.
- **TLS + authentication**, then a terminal UI.

See `TASKS.md` for the live task tracker and `docs/workspace-fs-and-transport.md`
for the full design.
