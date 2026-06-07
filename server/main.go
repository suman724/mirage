// Command mirage-server is the sandbox-side binary. It ACCEPTS the
// client-initiated gRPC stream (never dials), then drives the protocol:
// originate ChunkRequests over the open stream and reconstruct the published
// workspace into --out, verifying each chunk by hash.
package main

import (
	"flag"
	"log"
	"net"

	"google.golang.org/grpc"

	"github.com/suman724/mirage/server/transport"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "listen address")
	out := flag.String("out", "./mirage-out", "directory to reconstruct the workspace into")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}

	gs := grpc.NewServer()
	srv := transport.New(*out, func(r transport.Result) {
		if r.Err != nil {
			log.Printf("reconstruction FAILED after %d chunk requests: %v", r.ChunkRequests, r.Err)
			return
		}
		log.Printf("reconstructed %d files (%d bytes) into %s via %d chunk requests over the stream",
			r.Files, r.Bytes, r.OutDir, r.ChunkRequests)
	})
	srv.Register(gs)

	log.Printf("mirage-server listening on %s, reconstructing into %s (waiting for client to dial in)", *addr, *out)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
