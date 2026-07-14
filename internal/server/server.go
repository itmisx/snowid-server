// Package server exposes a bwmarrin/snowflake generator over gRPC.
package server

import (
	"context"

	snowidv1 "github.com/itmisx/snowid-server/gen/snowid/v1"
	"github.com/itmisx/snowid-server/pkg/snowflake"

	bw "github.com/bwmarrin/snowflake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MaxBatch caps how many IDs one Next call may ask for. It is a constant, not a
// setting: its only job is to stop one request from allocating unboundedly, and
// 1000 IDs is already more than any caller needs in one round trip.
const MaxBatch = 1000

// Service implements the SnowId gRPC service.
type Service struct {
	snowidv1.UnimplementedSnowIdServer

	node   *bw.Node
	layout snowflake.Layout

	// datacenterID and workerID are this process's identity. They are reported by
	// GetLayout so a caller can see who it is talking to; they are not needed to
	// decode an ID, which carries them itself.
	datacenterID int64
	workerID     int64
}

// New returns a Service that hands out IDs from node.
func New(node *bw.Node, layout snowflake.Layout, datacenterID, workerID int64) *Service {
	return &Service{node: node, layout: layout, datacenterID: datacenterID, workerID: workerID}
}

// Next returns count IDs, ascending.
func (s *Service) Next(_ context.Context, req *snowidv1.NextRequest) (*snowidv1.NextResponse, error) {
	count := int(req.GetCount())
	if count <= 0 {
		count = 1
	}
	if count > MaxBatch {
		return nil, status.Errorf(codes.InvalidArgument, "count %d exceeds the maximum %d", count, MaxBatch)
	}

	ids := make([]int64, count)
	for i := range ids {
		ids[i] = s.node.Generate().Int64()
	}
	return &snowidv1.NextResponse{Ids: ids}, nil
}

// GetLayout returns the ID layout, so a client can decode IDs locally instead of
// asking the server what time an ID was made, or who made it.
func (s *Service) GetLayout(_ context.Context, _ *snowidv1.GetLayoutRequest) (*snowidv1.GetLayoutResponse, error) {
	return &snowidv1.GetLayoutResponse{
		EpochUnixMilli: s.layout.EpochMilli,
		DatacenterBits: int32(s.layout.DatacenterBits),
		WorkerBits:     int32(s.layout.WorkerBits),
		StepBits:       int32(s.layout.StepBits),
		DatacenterId:   s.datacenterID,
		WorkerId:       s.workerID,
		MaxBatch:       MaxBatch,
	}, nil
}
