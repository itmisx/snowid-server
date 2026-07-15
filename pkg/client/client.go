// Package client is a Go client for snowid-server.
//
// It is a thin wrapper over the generated gRPC stub: it dials, fetches the ID
// layout once so that IDs can be decoded without a round trip, and calls Next.
// There is no buffering and no background goroutine — every Next is one RPC.
//
//	c, err := client.New(ctx, "dns:///snowid:50052")
//	defer c.Close()
//
//	id, err := c.Next(ctx)
//	fmt.Println(id, c.Layout().Time(id))   // decoded locally
//
// If one RPC per ID is too many — and for anything issuing IDs per request, it is
// — ask for a batch and hand them out from it yourself:
//
//	ids, err := c.NextN(ctx, 500)
//
// That is deliberately left to the caller. A buffer inside the client has to
// decide what to do about a dead server, a closed client, and IDs whose timestamp
// has gone stale sitting in the queue, and getting any of those wrong is worse
// than the round trips it saves.
package client

import (
	"context"
	"fmt"

	snowidv1 "github.com/itmisx/snowid-server/gen/snowid/v1"
	"github.com/itmisx/snowid-server/pkg/snowflake"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client calls a snowid-server.
type Client struct {
	conn     *grpc.ClientConn
	rpc      snowidv1.SnowIdClient
	layout   snowflake.Layout
	maxBatch int

	datacenterID int64
	workerID     int64

	// ownsConn is false when the caller handed us the connection. Closing
	// somebody else's connection is not ours to do.
	ownsConn bool
}

// New dials target and fetches the server's layout.
//
// The defaults are plaintext (snowid-server is an internal service and does not
// terminate TLS) and round-robin across whatever the target resolves to, which is
// what you want behind the headless Service in deploy/k8s. Anything passed in opts
// is applied after those, so a caller supplying TLS overrides the default rather
// than fighting it.
//
// Fetching the layout on connect means New fails fast if the server is
// unreachable, rather than the first Next failing later.
func New(ctx context.Context, target string, opts ...grpc.DialOption) (*Client, error) {
	dial := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	}, opts...)

	conn, err := grpc.NewClient(target, dial...)
	if err != nil {
		return nil, fmt.Errorf("snowid: dial %s: %w", target, err)
	}
	c, err := newWithConn(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	c.ownsConn = true
	return c, nil
}

// NewWithConn builds a Client on a connection the caller already has, and does not
// take ownership of it: Close leaves it open.
func NewWithConn(ctx context.Context, conn *grpc.ClientConn) (*Client, error) {
	return newWithConn(ctx, conn)
}

func newWithConn(ctx context.Context, conn *grpc.ClientConn) (*Client, error) {
	c := &Client{conn: conn, rpc: snowidv1.NewSnowIdClient(conn)}

	l, err := c.rpc.Layout(ctx, &snowidv1.LayoutRequest{}, grpc.WaitForReady(true))
	if err != nil {
		return nil, fmt.Errorf("snowid: get layout: %w", err)
	}
	c.layout = snowflake.Layout{
		EpochMilli:     l.GetEpoch(),
		NodeBits:       uint8(l.GetNodeBits()),
		DatacenterBits: uint8(l.GetDatacenterBits()),
		StepBits:       uint8(l.GetStepBits()),
	}
	c.datacenterID = l.GetDatacenterId()
	c.workerID = l.GetWorkerId()
	c.maxBatch = int(l.GetMaxBatch())
	if c.maxBatch <= 0 {
		return nil, fmt.Errorf("snowid: server reported max_batch=%d", l.GetMaxBatch())
	}
	return c, nil
}

// Layout returns how the server packs its IDs, so they can be taken apart here
// rather than over the network:
//
//	c.Layout().Time(id)
//	c.Layout().DatacenterID(id)
//	c.Layout().WorkerID(id)
//
// It is fetched once, on connect. The layout is permanent, so it cannot go stale.
func (c *Client) Layout() snowflake.Layout { return c.layout }

// Identity returns the datacenter and worker the server on the other end runs as.
//
// It is worth nothing for decoding — every ID carries its own — and is here only
// so a caller can see which process answered. Behind a load balancer, that is
// whichever replica served the GetLayout call, not the one that will serve the
// next ID.
func (c *Client) Identity() (datacenterID, workerID int64) {
	return c.datacenterID, c.workerID
}

// MaxBatch is the largest n the server will accept in one NextN call.
func (c *Client) MaxBatch() int { return c.maxBatch }

// Next returns one ID. It costs one round trip.
func (c *Client) Next(ctx context.Context) (snowflake.ID, error) {
	ids, err := c.NextN(ctx, 1)
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

// NextN returns n IDs, ascending.
//
// n may exceed the server's MaxBatch; the request is split into as many calls as
// it takes. The IDs still arrive ascending, and are still unique — nothing about
// uniqueness depends on them arriving together.
func (c *Client) NextN(ctx context.Context, n int) ([]snowflake.ID, error) {
	if n <= 0 {
		return nil, fmt.Errorf("snowid: count %d must be positive", n)
	}

	out := make([]snowflake.ID, 0, n)
	for len(out) < n {
		want := min(n-len(out), c.maxBatch)

		resp, err := c.rpc.Next(ctx, &snowidv1.NextRequest{Count: int32(want)})
		if err != nil {
			return nil, fmt.Errorf("snowid: next: %w", err)
		}
		got := resp.GetIds()
		if len(got) != want {
			return nil, fmt.Errorf("snowid: asked for %d ids, server returned %d", want, len(got))
		}
		for _, id := range got {
			out = append(out, snowflake.ID(id))
		}
	}
	return out, nil
}

// Close releases the connection, unless the caller supplied it.
//
// There is no "already closed" error to check for: with no buffer to serve from,
// a Client whose connection is closed simply fails its next RPC, which is what a
// caller has to handle anyway.
func (c *Client) Close() error {
	if !c.ownsConn || c.conn == nil {
		return nil
	}
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("snowid: close: %w", err)
	}
	return nil
}
