package server

import (
	"context"
	"testing"

	snowidv1 "github.com/itmisx/snowid-server/gen/snowid/v1"
	"github.com/itmisx/snowid-server/pkg/snowflake"

	bw "github.com/bwmarrin/snowflake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Twitter's 5/5 split, so the tests exercise the datacenter segment rather than
// the degenerate case where it is empty.
func testLayout() snowflake.Layout {
	return snowflake.Layout{
		EpochMilli:     1577836800000, // 2020-01-01 UTC
		DatacenterBits: 5,
		WorkerBits:     5,
		StepBits:       snowflake.DefaultStepBits,
	}
}

func newService(t *testing.T, datacenterID, workerID int64) *Service {
	t.Helper()

	l := testLayout()
	bw.Epoch = l.EpochMilli
	bw.NodeBits = l.DatacenterBits + l.WorkerBits
	bw.StepBits = l.StepBits

	node, err := bw.NewNode(l.PackID(datacenterID, workerID))
	if err != nil {
		t.Fatal(err)
	}
	return New(node, l, datacenterID, workerID)
}

func TestNextIsUniqueAndAscending(t *testing.T) {
	svc := newService(t, 1, 2)
	l := testLayout()

	const count = 5000
	var ids []int64
	for len(ids) < count {
		resp, err := svc.Next(context.Background(), &snowidv1.NextRequest{Count: MaxBatch})
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		ids = append(ids, resp.GetIds()...)
	}

	seen := make(map[int64]struct{}, len(ids))
	for i, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = struct{}{}
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("ids not ascending at %d: %d <= %d", i, id, ids[i-1])
		}
		if id <= 0 {
			t.Fatalf("id %d is not positive; the sign bit must stay clear", id)
		}
		if got := l.Datacenter(snowflake.ID(id)); got != 1 {
			t.Fatalf("id %d carries datacenter %d, want 1", id, got)
		}
		if got := l.Worker(snowflake.ID(id)); got != 2 {
			t.Fatalf("id %d carries worker %d, want 2", id, got)
		}
	}
}

// The reason the identity is split at all: two processes in different datacenters
// that happen to share a worker id must not collide. This is the pair that plain
// addition would break — (dc=1,worker=2) and (dc=2,worker=1) both sum to 3.
func TestDifferentIdentitiesNeverCollide(t *testing.T) {
	a := newService(t, 1, 2)
	b := newService(t, 2, 1)

	seen := make(map[int64]string)
	for range 5 {
		for name, svc := range map[string]*Service{"dc1/worker2": a, "dc2/worker1": b} {
			resp, err := svc.Next(context.Background(), &snowidv1.NextRequest{Count: MaxBatch})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, id := range resp.GetIds() {
				if prev, dup := seen[id]; dup {
					t.Fatalf("%s and %s both issued id %d", prev, name, id)
				}
				seen[id] = name
			}
		}
	}
	if want := 5 * 2 * MaxBatch; len(seen) != want {
		t.Fatalf("got %d unique ids, want %d", len(seen), want)
	}
}

// A count of zero or less means one id, not an error.
func TestNextTreatsNonPositiveCountAsOne(t *testing.T) {
	svc := newService(t, 0, 0)

	for _, count := range []int32{0, -1} {
		resp, err := svc.Next(context.Background(), &snowidv1.NextRequest{Count: count})
		if err != nil {
			t.Fatalf("Next(%d): %v", count, err)
		}
		if got := len(resp.GetIds()); got != 1 {
			t.Fatalf("Next(%d) returned %d ids, want 1", count, got)
		}
	}
}

// MaxBatch exists so that one request cannot allocate unboundedly.
func TestNextRejectsAnOversizedBatch(t *testing.T) {
	svc := newService(t, 0, 0)

	_, err := svc.Next(context.Background(), &snowidv1.NextRequest{Count: MaxBatch + 1})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("Next(MaxBatch+1) = %v, want %v", got, codes.InvalidArgument)
	}
}

// Clients decode ids with what GetLayout tells them, so it has to describe the
// generator that is actually running.
func TestGetLayoutDescribesTheRunningGenerator(t *testing.T) {
	svc := newService(t, 1, 2)
	want := testLayout()

	resp, err := svc.GetLayout(context.Background(), &snowidv1.GetLayoutRequest{})
	if err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	if resp.GetEpochUnixMilli() != want.EpochMilli ||
		resp.GetDatacenterBits() != int32(want.DatacenterBits) ||
		resp.GetWorkerBits() != int32(want.WorkerBits) ||
		resp.GetStepBits() != int32(want.StepBits) {
		t.Fatalf("layout = %+v, want %+v", resp, want)
	}
	if resp.GetDatacenterId() != 1 || resp.GetWorkerId() != 2 {
		t.Fatalf("identity = (%d,%d), want (1,2)", resp.GetDatacenterId(), resp.GetWorkerId())
	}
	if resp.GetMaxBatch() != MaxBatch {
		t.Fatalf("max batch = %d, want %d", resp.GetMaxBatch(), MaxBatch)
	}

	// And an id it just made must decode with exactly the layout it reported.
	next, err := svc.Next(context.Background(), &snowidv1.NextRequest{Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	reported := snowflake.Layout{
		EpochMilli:     resp.GetEpochUnixMilli(),
		DatacenterBits: uint8(resp.GetDatacenterBits()),
		WorkerBits:     uint8(resp.GetWorkerBits()),
		StepBits:       uint8(resp.GetStepBits()),
	}
	id := snowflake.ID(next.GetIds()[0])
	if got := reported.Datacenter(id); got != 1 {
		t.Fatalf("decoding with the reported layout gives datacenter %d, want 1", got)
	}
	if got := reported.Worker(id); got != 2 {
		t.Fatalf("decoding with the reported layout gives worker %d, want 2", got)
	}
}
