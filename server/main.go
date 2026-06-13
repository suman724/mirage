// Command mirage-server is the sandbox-side binary. It ACCEPTS the
// client-initiated gRPC stream (never dials), then drives the protocol:
// originate ChunkRequests over the open stream and project the published
// workspace one of four ways:
//
//	--out    reconstruct the tree to disk (default)
//	--mount  FUSE-mount it so reads fault chunks lazily (needs /dev/fuse)
//	--shim   project a lazy skeleton served over a unix socket to the C
//	         LD_PRELOAD shim (Shimmer fallback; superseded by --seccomp)
//	--seccomp project a lazy skeleton and run a workload under a seccomp
//	         user-notification filter, materializing files as it opens them —
//	         the FUSE-free production mode for Fargate (design §3.3). Run
//	         mirage-server as the container entrypoint (PID 1) so it is an
//	         ancestor of the workload (the ptrace_scope=1 requirement).
package main

import (
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/suman724/mirage/internal/logging"
	"github.com/suman724/mirage/server/transport"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "gRPC listen address")
	out := flag.String("out", "./mirage-out", "directory to reconstruct the workspace into (reconstruct mode)")
	mount := flag.String("mount", "", "directory to FUSE-mount the workspace at; if set, runs in mount mode")
	shimDir := flag.String("shim", "", "directory to project as a lazy skeleton served over a unix socket (LD_PRELOAD shim mode)")
	shimState := flag.String("shim-state", "", "shim mode: persistent dir for the state journal, chunk cache and supervisor socket")
	seccompDir := flag.String("seccomp", "", "directory to project as a lazy skeleton, materialized via a seccomp filter on the workload (Shimmer production mode); pass the workload after --")
	seccompState := flag.String("seccomp-state", "", "seccomp mode: persistent dir for the state journal, chunk cache and launcher socket (default: per-connection temp dir)")
	launcher := flag.String("seccomp-launcher", "mirage-launcher", "seccomp mode: path to the mirage-launcher binary")
	workers := flag.Int("seccomp-workers", 0, "seccomp mode: concurrent notification handlers (0 = default)")
	healthAddr := flag.String("health-addr", "", "if set, serve an HTTP health endpoint at this address (GET /healthz -> 200) for ALB/ELB health checks")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	flag.Parse()
	workload := flag.Args() // seccomp mode: the command to run under interception

	log := logging.Setup(*logLevel, *logFormat)

	// Exactly one projection mode (besides the default reconstruct).
	modes := 0
	for _, on := range []bool{*mount != "", *shimDir != "", *seccompDir != ""} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		log.Error("--mount, --shim and --seccomp are mutually exclusive")
		os.Exit(2)
	}
	if *shimState != "" && *shimDir == "" {
		log.Error("--shim-state requires --shim")
		os.Exit(2)
	}
	if *seccompState != "" && *seccompDir == "" {
		log.Error("--seccomp-state requires --seccomp")
		os.Exit(2)
	}
	if *seccompDir != "" && len(workload) == 0 {
		log.Error("--seccomp requires a workload command after -- (e.g. --seccomp /workspace -- bash)")
		os.Exit(2)
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("failed to listen", "addr", *addr, "err", err)
		os.Exit(1)
	}

	gs := grpc.NewServer()

	// gRPC health checking (grpc.health.v1) so an ALB gRPC target group can
	// health-check the task; report SERVING for the whole server.
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)

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
	case *seccompDir != "":
		srv = transport.NewSeccomp(*seccompDir, *seccompState, *launcher, workload, *workers, func(si transport.SeccompInfo) {
			log.Info("workspace projected; running workload under seccomp interception",
				"root", si.Root, "state", si.StateDir, "workload", si.Workload)
		}, log)
	default:
		srv = transport.New(*out, nil, log)
	}
	srv.Register(gs)

	// Optional HTTP health endpoint (for ALB/ELB HTTP health checks).
	if *healthAddr != "" {
		go serveHealth(*healthAddr, log)
	}

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
	case *seccompDir != "":
		mode, target = "seccomp", *seccompDir
	}
	log.Info("mirage-server listening; waiting for client to dial in",
		"addr", *addr, "mode", mode, "target", target)
	if err := gs.Serve(lis); err != nil {
		log.Error("serve error", "err", err)
		os.Exit(1)
	}
	log.Info("mirage-server stopped")
}

// serveHealth runs a minimal HTTP health server: GET /healthz -> 200 "ok".
// Used by load-balancer health checks (e.g. an ALB target group). It logs and
// returns on failure rather than killing the process — health is auxiliary.
func serveHealth(addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	log.Info("health endpoint listening", "addr", addr, "path", "/healthz")
	srv := &http.Server{Addr: addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("health endpoint failed", "err", err)
	}
}
