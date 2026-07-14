package client

import (
	"context"
	"net"
	"sync"
	"testing"

	snowidv1 "github.com/itmisx/snowid-server/gen/snowid/v1"
	"github.com/itmisx/snowid-server/internal/server"
	"github.com/itmisx/snowid-server/pkg/snowflake"

	bw "github.com/bwmarrin/snowflake"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const (
	testDatacenterID = 1
	testWorkerID     = 2
)

// newTestServer runs a real server over an in-memory connection, so these tests
// exercise the actual gRPC path rather than a hand-written mock.
func newTestServer(t *testing.T) *grpc.ClientConn {
	t.Helper()

	// Twitter's 5/5 split, so these tests exercise the datacenter segment.
	l := snowflake.Layout{
		EpochMilli:     1577836800000, // 2020-01-01 UTC
		DatacenterBits: 5,
		WorkerBits:     5,
		StepBits:       snowflake.DefaultStepBits,
	}
	bw.Epoch = l.EpochMilli
	bw.NodeBits = l.DatacenterBits + l.WorkerBits
	bw.StepBits = l.StepBits

	node, err := bw.NewNode(l.PackID(testDatacenterID, testWorkerID))
	if err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	snowidv1.RegisterSnowIdServer(s, server.New(node, l, testDatacenterID, testWorkerID))
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewWithConn(t.Context(), newTestServer(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNext(t *testing.T) {
	c := newTestClient(t)

	id, err := c.Next(t.Context())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id %d is not positive", id)
	}
}

func TestNextNIsUniqueAndAscending(t *testing.T) {
	c := newTestClient(t)

	ids, err := c.NextN(t.Context(), 500)
	if err != nil {
		t.Fatalf("NextN: %v", err)
	}
	if len(ids) != 500 {
		t.Fatalf("got %d ids, want 500", len(ids))
	}

	seen := make(map[snowflake.ID]struct{}, len(ids))
	for i, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = struct{}{}
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("ids not ascending at %d: %d <= %d", i, id, ids[i-1])
		}
	}
}

// NextN takes an n larger than the server will accept in one call, and splits it.
// The ids must still come back unique and ascending across the seam.
func TestNextNSplitsBeyondMaxBatch(t *testing.T) {
	c := newTestClient(t)

	n := c.MaxBatch()*2 + 1
	ids, err := c.NextN(t.Context(), n)
	if err != nil {
		t.Fatalf("NextN(%d): %v", n, err)
	}
	if len(ids) != n {
		t.Fatalf("got %d ids, want %d", len(ids), n)
	}

	seen := make(map[snowflake.ID]struct{}, n)
	for i, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d across the batch boundary", id)
		}
		seen[id] = struct{}{}
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("ids not ascending at %d (batch boundary is %d): %d <= %d",
				i, c.MaxBatch(), id, ids[i-1])
		}
	}
}

func TestNextNRejectsNonPositiveCount(t *testing.T) {
	c := newTestClient(t)

	for _, n := range []int{0, -1} {
		if _, err := c.NextN(t.Context(), n); err == nil {
			t.Fatalf("NextN(%d) succeeded, want an error", n)
		}
	}
}

// The layout comes back on connect, and is what lets a caller decode an id without
// asking the server anything — including which datacenter and node made it.
func TestLayoutDecodesLocally(t *testing.T) {
	c := newTestClient(t)

	id, err := c.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	l := c.Layout()
	if got := l.Datacenter(id); got != testDatacenterID {
		t.Errorf("datacenter = %d, want %d", got, testDatacenterID)
	}
	if got := l.Worker(id); got != testWorkerID {
		t.Errorf("worker = %d, want %d", got, testWorkerID)
	}
	if l.Time(id).IsZero() {
		t.Error("time decoded to zero")
	}

	// And the server says who it is, which must agree with what the ids carry.
	dc, w := c.Identity()
	if dc != testDatacenterID || w != testWorkerID {
		t.Errorf("Identity() = (%d,%d), want (%d,%d)", dc, w, testDatacenterID, testWorkerID)
	}
}

func TestConcurrentUse(t *testing.T) {
	c := newTestClient(t)

	const goroutines, each = 8, 50
	var (
		mu   sync.Mutex
		seen = make(map[snowflake.ID]struct{}, goroutines*each)
		wg   sync.WaitGroup
	)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, err := c.NextN(t.Context(), each)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate id %d across goroutines", id)
					return
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()

	if len(seen) != goroutines*each {
		t.Fatalf("got %d unique ids, want %d", len(seen), goroutines*each)
	}
}

// Close must not close a connection the caller owns — it may be shared with the
// rest of their application.
func TestCloseLeavesABorrowedConnectionOpen(t *testing.T) {
	conn := newTestServer(t)
	c, err := NewWithConn(t.Context(), conn)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The connection is still usable, so a second client on it still works.
	c2, err := NewWithConn(t.Context(), conn)
	if err != nil {
		t.Fatalf("the borrowed connection was closed out from under its owner: %v", err)
	}
	if _, err := c2.Next(t.Context()); err != nil {
		t.Fatalf("the borrowed connection was closed out from under its owner: %v", err)
	}
}

// An unreachable server must fail at New, not at the first Next.
func TestNewFailsFastOnAnUnreachableServer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // nothing is listening; do not wait to find out

	if _, err := New(ctx, "passthrough:///nowhere"); err == nil {
		t.Fatal("New succeeded against an unreachable server")
	}
}
