//go:build linux

// Command mirage-seccomp-harness wires the Shimmer seccomp path end to end for
// validation (docs/design-shimmer.md §3.3, milestone S3′), without the gRPC
// client/server: it chunks a fixture dir into an in-memory store, builds the
// skeleton, starts the seccomp supervisor, spawns the C launcher as a CHILD
// (so the supervisor is an ancestor — the ptrace_scope=1 requirement), and runs
// the given workload under interception.
//
//	mirage-seccomp-harness --src DIR --root DIR --launcher PATH [--workers N] -- CMD [ARGS...]
//
// The workload's own behaviour (reading materialized files) is the test; this
// harness prints a STATS line on exit for the validation script to assert on.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/folbricht/desync"
	"golang.org/x/sys/unix"

	"github.com/suman724/mirage/client/index"
	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
	"github.com/suman724/mirage/server/seccomp"
	"github.com/suman724/mirage/server/shim"
)

func main() {
	src := flag.String("src", "", "fixture directory to publish (chunked into an in-memory store)")
	root := flag.String("root", "", "workspace directory to project the skeleton into")
	launcher := flag.String("launcher", "./bin/mirage-launcher", "path to the mirage-launcher binary")
	workers := flag.Int("workers", 4, "concurrent notification servicers")
	logLevel := flag.String("log-level", "info", "log level")
	flag.Parse()
	cmdArgs := flag.Args()

	log := logging.Setup(*logLevel, "text")
	if *src == "" || *root == "" || len(cmdArgs) == 0 {
		log.Error("usage: --src DIR --root DIR [--launcher PATH] -- CMD [ARGS...]")
		os.Exit(2)
	}

	// 1. Chunk the fixture (reusing the client indexer, secret-exclusion and
	//    all) into an in-memory desync store.
	manifest, cs, err := index.Build(*src)
	if err != nil {
		log.Error("index fixture", "err", err)
		os.Exit(1)
	}
	store := chunkstoreAdapter{cs}

	// 2. Skeleton + state table + materializer (reused from S1, unchanged).
	stateDir, err := os.MkdirTemp("", "mirage-seccomp-state-")
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

	// 3. Seccomp supervisor.
	sup, err := seccomp.New(mat, log)
	if err != nil {
		log.Error("seccomp supervisor", "err", err)
		os.Exit(1)
	}

	// 4. Listen for the launcher's listener-fd hand-off.
	sockPath := stateDir + "/launcher.sock"
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Error("listen launcher socket", "err", err)
		os.Exit(1)
	}
	defer ln.Close()

	// 5. Spawn the launcher as our child, which installs the filter and execs
	//    the workload. It connects back to sockPath with the listener fd.
	child := exec.Command(*launcher, cmdArgs...)
	child.Env = append(os.Environ(), "MIRAGE_SUPERVISOR_SOCK="+sockPath)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		log.Error("start launcher", "err", err)
		os.Exit(1)
	}

	// 6. Receive the listener fd, start servicing, then ack so the launcher execs.
	listenerFd, conn, err := acceptListenerFd(ln)
	if err != nil {
		log.Error("receive listener fd", "err", err)
		_ = child.Process.Kill()
		os.Exit(1)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- sup.Serve(listenerFd, *workers) }()
	if _, err := conn.Write([]byte("OK\n")); err != nil {
		log.Error("ack launcher", "err", err)
	}
	conn.Close()

	// 7. Wait for the workload to finish, then stop servicing.
	cmdErr := child.Wait()
	sup.Stop() // signals the receiver to finish (poll-based)
	<-serveErr
	syscall.Close(listenerFd)

	st := sup.Stats()
	fmt.Fprintf(os.Stderr,
		"SECCOMP_STATS traps=%d workspace=%d fastpath=%d errors=%d\n",
		st.Traps, st.Workspace, st.FastPath, st.Errors)

	if cmdErr != nil {
		if ee, ok := cmdErr.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		log.Error("workload", "err", cmdErr)
		os.Exit(1)
	}
}

// acceptListenerFd accepts one connection and reads the SCM_RIGHTS listener fd.
// The returned conn is kept open so the caller can ack the launcher.
func acceptListenerFd(ln net.Listener) (int, net.Conn, error) {
	conn, err := ln.Accept()
	if err != nil {
		return 0, nil, fmt.Errorf("accept: %w", err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return 0, nil, fmt.Errorf("not a unix conn")
	}
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		conn.Close()
		return 0, nil, fmt.Errorf("read msg: %w", err)
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(scms) == 0 {
		conn.Close()
		return 0, nil, fmt.Errorf("parse control message: %w", err)
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil || len(fds) == 0 {
		conn.Close()
		return 0, nil, fmt.Errorf("parse rights: %w", err)
	}
	return fds[0], conn, nil
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
