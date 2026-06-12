// Command mirage-server is the sandbox-side binary. It ACCEPTS the
// client-initiated gRPC stream (never dials), then drives the protocol:
// originate ChunkRequests over the open stream and either reconstruct the
// published workspace into --out, FUSE-mount it at --mount so reads fault
// chunks lazily over the channel, or project it at --shim as a skeleton of
// sparse placeholders materialized on demand over a unix socket (Shimmer:
// the FUSE-free lazy mode for privilege-restricted platforms like Fargate).
package main

import (
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/suman724/mirage/internal/logging"
	"github.com/suman724/mirage/server/transport"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "listen address")
	out := flag.String("out", "./mirage-out", "directory to reconstruct the workspace into (reconstruct mode)")
	mount := flag.String("mount", "", "directory to FUSE-mount the workspace at; if set, runs in mount mode instead of reconstruct")
	shimDir := flag.String("shim", "", "directory to project the workspace into as a lazy skeleton (Shimmer, FUSE-free); if set, runs in shim mode")
	shimState := flag.String("shim-state", "", "shim mode: persistent dir for the state journal, chunk cache and supervisor socket (default: per-connection temp dir, no restart recovery)")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	flag.Parse()

	log := logging.Setup(*logLevel, *logFormat)

	if *mount != "" && *shimDir != "" {
		log.Error("--mount and --shim are mutually exclusive")
		os.Exit(2)
	}
	if *shimState != "" && *shimDir == "" {
		log.Error("--shim-state requires --shim")
		os.Exit(2)
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("failed to listen", "addr", *addr, "err", err)
		os.Exit(1)
	}

	gs := grpc.NewServer()
	var srv *transport.Server
	switch {
	case *mount != "":
		srv = transport.NewMounter(*mount, func(mi transport.MountInfo) {
			log.Info("workspace mounted; reads will fault chunks over the channel", "mountpoint", mi.Mountpoint)
		}, log)
	case *shimDir != "":
		srv = transport.NewShimmer(*shimDir, *shimState, func(si transport.ShimInfo) {
			log.Info("workspace projected; opens via the shim will materialize files lazily",
				"root", si.Root, "socket", si.SocketPath, "state", si.StateDir)
			log.Info("run tools with",
				"LD_PRELOAD", "libmirageshim.so",
				"MIRAGE_SHIM_ROOT", si.Root,
				"MIRAGE_SHIM_SOCK", si.SocketPath)
		}, log)
	default:
		srv = transport.New(*out, nil, log)
	}
	srv.Register(gs)

	// Graceful shutdown on SIGINT/SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stop
		log.Info("shutdown signal received; stopping gracefully", "signal", sig.String())
		gs.GracefulStop()
	}()

	mode, target := "reconstruct", *out
	switch {
	case *mount != "":
		mode, target = "mount", *mount
	case *shimDir != "":
		mode, target = "shim", *shimDir
	}
	log.Info("mirage-server listening; waiting for client to dial in",
		"addr", *addr, "mode", mode, "target", target)
	if err := gs.Serve(lis); err != nil {
		log.Error("serve error", "err", err)
		os.Exit(1)
	}
	log.Info("mirage-server stopped")
}
