// Command mirage-client is the user-machine binary. It DIALS OUT to the server
// (the only place a Dial happens), indexes --dir, publishes the index, then
// serves chunk bytes by hash in answer to the server's ChunkRequests.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/suman724/mirage/client/transport"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "server address to dial")
	dir := flag.String("dir", ".", "workspace directory to publish")
	flag.Parse()

	c, err := transport.Dial(*addr)
	if err != nil {
		log.Fatalf("dial %s: %v", *addr, err)
	}
	defer c.Close()

	log.Printf("mirage-client dialed %s, publishing %s", *addr, *dir)
	if err := c.Serve(context.Background(), *dir); err != nil {
		log.Fatalf("serve: %v", err)
	}
	log.Printf("done: server finished reconstruction; stream closed")
}
