// Command mirage-client is the user-machine binary. It DIALS OUT to the server
// (the only place a Dial happens), indexes --dir, publishes the index, then
// serves chunk bytes by hash in answer to the server's ChunkRequests.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/suman724/mirage/client/transport"
	"github.com/suman724/mirage/internal/logging"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "server address to dial")
	dir := flag.String("dir", ".", "workspace directory to publish")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	flag.Parse()

	log := logging.Setup(*logLevel, *logFormat)

	// Cancel the session on SIGINT/SIGTERM so the stream tears down cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c, err := transport.Dial(*addr, log)
	if err != nil {
		log.Error("failed to dial server", "addr", *addr, "err", err)
		os.Exit(1)
	}
	defer c.Close()

	log.Info("mirage-client dialed server", "addr", *addr, "dir", *dir)
	if err := c.Serve(ctx, *dir); err != nil {
		log.Error("serve error", "err", err)
		os.Exit(1)
	}
	log.Info("mirage-client done")
}
