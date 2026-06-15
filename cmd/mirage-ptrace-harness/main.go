//go:build linux

// Command mirage-ptrace-harness wires the ptrace interception path end to end
// for validation (docs/design-ptrace-interception.md §4/§5/§7), without the gRPC
// client/server: it chunks a fixture dir into an in-memory store, builds the
// skeleton, starts the ptrace tracer on an attach socket, spawns the C
// trace-launcher (which requests attach, installs the RET_TRACE filter, and
// execs the workload), and runs the workload under interception.
//
//	mirage-ptrace-harness --src DIR --root DIR --launcher PATH -- CMD [ARGS...]
//
// Unlike the seccomp harness, the tracer reaps the workload itself (it owns the
// ptrace wait loop), so this harness never calls child.Wait(); it reads the
// workload exit code from the tracer. Prints a PTRACE_STATS line on exit.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/folbricht/desync"

	"github.com/suman724/mirage/client/index"
	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
	"github.com/suman724/mirage/server/ptrace"
	"github.com/suman724/mirage/server/shim"
)

func main() {
	src := flag.String("src", "", "fixture directory to publish (chunked into an in-memory store)")
	root := flag.String("root", "", "workspace directory to project the skeleton into")
	launcher := flag.String("launcher", "./bin/mirage-trace-launcher", "path to the mirage-trace-launcher binary")
	logLevel := flag.String("log-level", "info", "log level")
	flag.Parse()
	cmdArgs := flag.Args()

	log := logging.Setup(*logLevel, "text")
	if *src == "" || *root == "" || len(cmdArgs) == 0 {
		log.Error("usage: --src DIR --root DIR [--launcher PATH] -- CMD [ARGS...]")
		os.Exit(2)
	}

	// 1. Chunk the fixture (reusing the client indexer, secret-exclusion and all)
	//    into an in-memory desync store.
	manifest, cs, err := index.Build(*src)
	if err != nil {
		log.Error("index fixture", "err", err)
		os.Exit(1)
	}
	store := chunkstoreAdapter{cs}

	// 2. Skeleton + state table + materializer (reused from S1, unchanged).
	stateDir, err := os.MkdirTemp("", "mirage-ptrace-state-")
	if err != nil {
		log.Error("state dir", "err", err)
		os.Exit(1)
	}
	defer os.RemoveAll(stateDir)
	table, err := shim.OpenTable(stateDir+"/journal.jsonl", log)
	if err != nil {
		log.Error("open table", "err", err)
		os.Exit(1)
	}
	defer table.Close()
	buildTime := time.Now()
	if _, err := shim.BuildSkeleton(*root, manifest, table, buildTime, log); err != nil {
		log.Error("build skeleton", "err", err)
		os.Exit(1)
	}
	mat, err := shim.NewMaterializer(*root, manifest, store, table, buildTime, log)
	if err != nil {
		log.Error("materializer", "err", err)
		os.Exit(1)
	}

	// 3. ptrace tracer + attach socket. Start serving BEFORE spawning the
	//    launcher; the launcher retries the connect, so a brief race is fine.
	tracer, err := ptrace.New(mat, log)
	if err != nil {
		log.Error("ptrace tracer", "err", err)
		os.Exit(1)
	}
	attachSock := stateDir + "/attach.sock"
	serveErr := make(chan error, 1)
	go func() { serveErr <- tracer.Serve(attachSock) }()

	// 4. Spawn the trace-launcher. It connects to attachSock, sends
	//    "ATTACH <pid>", waits for the tracer to seize it, installs the
	//    RET_TRACE filter, then execs the workload. We do NOT Wait() on it —
	//    the tracer's ptrace loop reaps it (see package doc).
	child := exec.Command(*launcher, cmdArgs...)
	child.Env = append(os.Environ(), "MIRAGE_ATTACH_SOCK="+attachSock)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		log.Error("start trace-launcher", "err", err)
		os.Exit(1)
	}

	// 5. The tracer returns when the root workload exits.
	if err := <-serveErr; err != nil {
		log.Error("tracer", "err", err)
		os.Exit(1)
	}

	st := tracer.Stats()
	fmt.Fprintf(os.Stderr,
		"PTRACE_STATS traps=%d workspace=%d errors=%d exit=%d\n",
		st.Traps, st.Workspace, st.Errors, tracer.ExitCode())

	if code := tracer.ExitCode(); code != 0 {
		os.Exit(code)
	}
}

// chunkstoreAdapter exposes the client chunkstore as a desync.Store so the
// materializer can fault chunks from the in-memory fixture.
type chunkstoreAdapter struct {
	cs interface {
		Get(chunk.Hash) ([]byte, bool)
	}
}

func (a chunkstoreAdapter) GetChunk(id desync.ChunkID) (*desync.Chunk, error) {
	b, ok := a.cs.Get(chunk.Hash(id))
	if !ok {
		return nil, desync.ChunkMissing{ID: id}
	}
	return desync.NewChunkWithID(id, b, false)
}
func (a chunkstoreAdapter) HasChunk(id desync.ChunkID) (bool, error) {
	_, ok := a.cs.Get(chunk.Hash(id))
	return ok, nil
}
func (a chunkstoreAdapter) Close() error   { return nil }
func (a chunkstoreAdapter) String() string { return "fixture-memstore" }
