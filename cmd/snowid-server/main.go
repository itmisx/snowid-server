// Command snowid-server serves snowflake IDs over gRPC.
//
// The IDs come from github.com/bwmarrin/snowflake. This binary is a thin gRPC
// wrapper around it and nothing more.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	snowidv1 "github.com/itmisx/snowid-server/gen/snowid/v1"
	"github.com/itmisx/snowid-server/internal/server"
	"github.com/itmisx/snowid-server/pkg/snowflake"

	bw "github.com/bwmarrin/snowflake"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

// stepBits is bwmarrin's, and is not configurable: 12 bits is 4096 IDs per
// millisecond per worker, which is more than this service can serve anyway.
//
// Twitter called this segment the "sequence"; bwmarrin calls it the "step", and
// since it is bwmarrin doing the counting, so do we.
const stepBits = snowflake.DefaultStepBits

// drainTimeout bounds the graceful shutdown. Without it a client holding a
// long-lived stream (a health watch, server reflection) would keep GracefulStop
// waiting forever.
const drainTimeout = 10 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// On Kubernetes the worker id comes from the environment, because that is
	// where a StatefulSet can put its pod's ordinal. See envWorkerID.
	envWorker, err := envWorkerID()
	if err != nil {
		return err
	}
	envDatacenter, err := envInt64("SNOWID_DATACENTER_ID", 0)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("snowid-server", flag.ContinueOnError)
	var (
		addr        = fs.String("addr", ":50051", "gRPC listen address")
		workerID    = fs.Int64("worker-id", envWorker, "this worker's id within its datacenter; required, and no two live processes may share the same (datacenter, worker) pair [SNOWID_WORKER_ID]")
		datacenter  = fs.Int64("datacenter-id", envDatacenter, "this datacenter's id [SNOWID_DATACENTER_ID]")
		workerBits  = fs.Uint("worker-bits", uint(snowflake.DefaultWorkerBits), "width of the worker segment: how many workers there can be per datacenter; permanent once ids are issued")
		datacenters = fs.Uint("datacenter-bits", uint(snowflake.DefaultDatacenterBits), "width of the datacenter segment; 0 means no datacenters. These bits ADD to --worker-bits and come out of the timestamp, so take them off the worker: 5+5 lasts as long as 0+10, but 5+10 expires in two years. Permanent once ids are issued")
		epochMs     = fs.Int64("epoch", 1577836800000, "zero point of the id timestamp, in unix milliseconds; permanent once ids are issued")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	layout := snowflake.Layout{
		EpochMilli:     *epochMs,
		DatacenterBits: uint8(*datacenters),
		WorkerBits:     uint8(*workerBits),
		StepBits:       stepBits,
	}
	if err := validate(*datacenter, *workerID, layout, time.Now()); err != nil {
		return err
	}

	// bwmarrin has one identity segment, not two, and knows nothing of
	// datacenters. So the pair is packed into that single segment — concatenated,
	// never added; see Layout.PackID — and bwmarrin's "node" is the result.
	packed := layout.PackID(*datacenter, *workerID)
	nodeBits := layout.DatacenterBits + layout.WorkerBits

	// bwmarrin keeps the layout in package-level variables, which NewNode reads.
	bw.Epoch, bw.NodeBits, bw.StepBits = layout.EpochMilli, nodeBits, layout.StepBits
	node, err := bw.NewNode(packed)
	if err != nil {
		return fmt.Errorf("new node: %w", err)
	}

	fields := []any{
		"version", version,
		"worker_id", *workerID,
		"epoch", time.UnixMilli(layout.EpochMilli).UTC().Format(time.RFC3339),
		"worker_bits", layout.WorkerBits,
		"step_bits", layout.StepBits,
		"max_workers", int64(1) << layout.WorkerBits,
		"max_ids_per_second", (int64(1) << layout.StepBits) * 1000,
		// When the timestamp segment runs out of room and IDs start colliding.
		// Worth seeing at startup, because the layout cannot be changed later.
		"ids_valid_until", time.UnixMilli(layout.EpochMilli + int64(1)<<layout.TimestampBits()).UTC().Format(time.RFC3339),
	}
	// Only mention datacenters when there are any. Logging "datacenter_id=0
	// datacenter_bits=0 max_datacenters=1" on every ordinary startup would be
	// three fields saying nothing.
	if layout.DatacenterBits > 0 {
		fields = append(fields,
			"datacenter_id", *datacenter,
			"datacenter_bits", layout.DatacenterBits,
			"max_datacenters", int64(1)<<layout.DatacenterBits,
		)
	}
	slog.Info("generator ready", fields...)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	s := grpc.NewServer()
	snowidv1.RegisterSnowIdServer(s, server.New(node, layout, *datacenter, *workerID))

	// Standard gRPC health checking, for Kubernetes probes and load balancers.
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(s, healthSrv)
	// Reflection, so grpcurl and friends work without the .proto on hand.
	reflection.Register(s)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Stop trapping signals, so a second Ctrl-C kills us outright rather
		// than being swallowed while we drain.
		stop()
		slog.Info("shutting down")
		healthSrv.Shutdown()

		drained := make(chan struct{})
		go func() { s.GracefulStop(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(drainTimeout):
			slog.Warn("drain timed out; closing connections", "after", drainTimeout)
			s.Stop()
		}
	}()

	slog.Info("serving", "addr", *addr)
	if err := s.Serve(lis); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// envWorkerID reads the worker id out of SNOWID_WORKER_ID, and reports -1 ("not
// set") if the variable is missing or empty.
//
// That is how a StatefulSet hands each pod its ordinal, via the downward API:
//
//	env:
//	  - name: SNOWID_WORKER_ID
//	    valueFrom:
//	      fieldRef:
//	        fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
//
// The label is set by the StatefulSet controller and by nothing else, so this
// fails closed where it has to: run the same manifest as a Deployment and the
// label does not exist, the downward API yields "", and the server refuses to
// start rather than invent a worker id. Two processes sharing an identity issue
// the same ids, so refusing is the only safe answer.
//
// Note what this deliberately does NOT do: parse the hostname. A Deployment's pod
// name ("snowid-7d4b9c5f8-84272") ends in a random suffix drawn from an alphabet
// that contains digits, so roughly one pod in 850 ends in an all-digit segment
// that reads exactly like an ordinal. Telling a StatefulSet from a Deployment by
// the shape of a string cannot be done.
func envWorkerID() (int64, error) { return envInt64("SNOWID_WORKER_ID", -1) }

// envInt64 reads an integer out of the environment, and reports fallback if the
// variable is missing or empty. A malformed value is an error, never a silent
// fallback: an id that quietly becomes somebody else's is the one failure this
// server exists to prevent.
func envInt64(key string, fallback int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback, fmt.Errorf("%s=%q is not an integer", key, v)
	}
	return n, nil
}

// validate rejects a configuration that cannot produce correct IDs.
//
// It matters because nothing below here will complain: bwmarrin's Generate()
// returns no error and shifts the timestamp into place without a bounds check,
// so a layout with too few timestamp bits silently emits IDs whose time is wrong,
// whose sign bit is set, and which repeat once the segment wraps.
func validate(datacenterID, workerID int64, l snowflake.Layout, now time.Time) error {
	if workerID < 0 {
		return errors.New("--worker-id is required: two live processes sharing an identity issue the same ids")
	}

	// The top bit stays 0 so IDs are positive in languages without unsigned
	// integers, leaving 63 bits to divide up, of which the timestamp needs 32 to
	// be of any use.
	if used := int(l.DatacenterBits) + int(l.WorkerBits) + int(l.StepBits); used > 31 {
		return fmt.Errorf("--datacenter-bits=%d + --worker-bits=%d + step bits(%d) = %d, "+
			"which leaves fewer than 32 bits for the timestamp",
			l.DatacenterBits, l.WorkerBits, l.StepBits, used)
	}

	// Each id has to fit in its own segment. If either overflowed, it would spill
	// into the other's bits and land on an identity that belongs to a different
	// (datacenter, worker) pair — a live collision, not a rounding error.
	if max := int64(1)<<l.DatacenterBits - 1; datacenterID < 0 || datacenterID > max {
		return fmt.Errorf("--datacenter-id=%d is out of range [0,%d] for --datacenter-bits=%d",
			datacenterID, max, l.DatacenterBits)
	}
	if max := int64(1)<<l.WorkerBits - 1; workerID > max {
		return fmt.Errorf("--worker-id=%d is out of range [0,%d] for --worker-bits=%d",
			workerID, max, l.WorkerBits)
	}

	elapsed := now.UnixMilli() - l.EpochMilli
	if elapsed < 0 {
		return fmt.Errorf("--epoch=%d is in the future (%s)", l.EpochMilli,
			time.UnixMilli(l.EpochMilli).UTC().Format(time.RFC3339))
	}
	if bits := l.TimestampBits(); elapsed>>bits != 0 {
		return fmt.Errorf("--epoch=%d is too far in the past: %d bits of timestamp hold only %s, "+
			"and %s have passed since it", l.EpochMilli, bits,
			time.Duration(int64(1)<<bits)*time.Millisecond,
			time.Duration(elapsed)*time.Millisecond)
	}
	return nil
}
