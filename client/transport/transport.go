// Package transport is the CLIENT side of the Mirage connection. It is the
// ONLY package that ever dials (design §3, §10): the client, behind
// NAT/firewall, opens exactly one outbound bidi stream and then answers the
// server's ChunkRequests over it. The server never dials in.
package transport

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/suman724/mirage/client/chunkstore"
	"github.com/suman724/mirage/client/index"
	"github.com/suman724/mirage/internal/chunk"
	"github.com/suman724/mirage/internal/logging"
	miragev1 "github.com/suman724/mirage/proto/mirage/v1"
)

// Client holds an outbound connection to the Mirage server.
type Client struct {
	conn   *grpc.ClientConn
	client miragev1.MirageClient
	log    *slog.Logger
}

// Dial opens the outbound connection to addr. Plaintext over localhost is fine
// for this spike; TLS comes later. This is the only Dial in the codebase.
// logger may be nil (defaults to slog.Default()).
func Dial(addr string, logger *slog.Logger) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("transport: dial %q: %w", addr, err)
	}
	return &Client{
		conn:   conn,
		client: miragev1.NewMirageClient(conn),
		log:    logging.OrDefault(logger),
	}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Serve indexes dir, opens the Connect stream, publishes the index, then
// answers ChunkRequests from the local chunkstore until the server closes the
// stream (reconstruction complete) or ctx is cancelled.
func (c *Client) Serve(ctx context.Context, dir string) error {
	manifest, store, err := index.Build(dir)
	if err != nil {
		return fmt.Errorf("index workspace %q: %w", dir, err)
	}
	c.log.Info("workspace indexed",
		"dir", dir,
		"files", len(manifest.Files),
		"unique_chunks", store.Len(),
		"total_bytes", manifest.TotalBytes())

	stream, err := c.client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Hello handshake.
	if err := stream.Send(&miragev1.ClientFrame{
		Payload: &miragev1.ClientFrame_Hello{
			Hello: &miragev1.Hello{
				ClientVersion: "0.1.0-spike",
				Os:            "darwin",
				Workspace:     &miragev1.WorkspaceMeta{RootName: "workspace"},
			},
		},
	}); err != nil {
		return err
	}

	// Publish the index (manifest of chunk hashes).
	caidx, err := manifest.Marshal()
	if err != nil {
		return err
	}
	if err := stream.Send(&miragev1.ClientFrame{
		Payload: &miragev1.ClientFrame_IndexPublish{
			IndexPublish: &miragev1.IndexPublish{
				Caidx:       caidx,
				TotalChunks: manifest.TotalChunks(),
				TotalBytes:  manifest.TotalBytes(),
			},
		},
	}); err != nil {
		return fmt.Errorf("publish index: %w", err)
	}
	c.log.Info("index published; serving chunk requests")

	// Answer server frames until the stream closes (reconstruction complete).
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			c.log.Info("server closed the stream; reconstruction complete")
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive server frame: %w", err)
		}
		switch p := frame.Payload.(type) {
		case *miragev1.ServerFrame_HelloAck:
			c.log.Info("handshake acknowledged", "sandbox_id", p.HelloAck.GetSandboxId())
		case *miragev1.ServerFrame_ChunkRequest:
			if err := c.answerChunkRequest(stream, store, p.ChunkRequest); err != nil {
				return fmt.Errorf("answer chunk request: %w", err)
			}
		default:
			c.log.Debug("ignoring out-of-scope server frame")
		}
	}
}

// answerChunkRequest serves the requested hashes from the store, rejecting any
// hash not present in the published index (design §6).
func (c *Client) answerChunkRequest(stream miragev1.Mirage_ConnectClient, store *chunkstore.Store, req *miragev1.ChunkRequest) error {
	resp := &miragev1.ChunkResponse{RequestId: req.GetRequestId()}
	for _, raw := range req.GetChunkHashes() {
		h, err := chunk.HashFromBytes(raw)
		if err != nil {
			c.log.Warn("rejecting chunk request: bad hash", "request_id", req.GetRequestId(), "err", err)
			resp.Error = fmt.Sprintf("bad hash: %v", err)
			resp.Chunks = nil
			break
		}
		data, found := store.Get(h)
		if !found {
			// REJECT: the server asked for a hash the client never published.
			// This is the security boundary — e.g. a secret file's chunk.
			c.log.Warn("rejecting chunk request: hash not in published index",
				"request_id", req.GetRequestId(), "hash", h.String())
			resp.Error = fmt.Sprintf("hash %s not in published index", h)
			resp.Chunks = nil
			break
		}
		c.log.Debug("serving chunk", "request_id", req.GetRequestId(), "hash", h.String(), "bytes", len(data))
		resp.Chunks = append(resp.Chunks, &miragev1.Chunk{Hash: h[:], Data: data})
	}
	return stream.Send(&miragev1.ClientFrame{
		Payload: &miragev1.ClientFrame_ChunkResponse{ChunkResponse: resp},
	})
}
