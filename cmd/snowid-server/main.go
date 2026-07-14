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

// maxNodeAndStepBits is bwmarrin's contract, in its own words:
//
//	// NodeBits holds the number of bits to use for Node
//	// Remember, you have a total 22 bits to share between Node/Step
//
// Nothing in the library enforces it. NewNode validates the node id's range and
// nothing else, so a wider split does not fail — Generate() shifts the timestamp
// clean out of its segment and hands back an ID whose time is wrong, whose sign
// bit may be set, and which repeats once the segment wraps. So it is enforced
// here.
//
// Staying inside it also means the timestamp always keeps at least 63-22 = 41
// bits — about 69 years — so the layout cannot quietly expire.
const maxNodeAndStepBits = 22

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

// config is everything the server is told at startup.
//
// The node segment is one segment, split in two: the datacenter takes the top
// DatacenterBits of it, and the worker gets whatever is left. So WorkerBits is
// derived, never configured — there is no way to write down a split that does not
// add up.
type config struct {
	addr           string
	workerID       int64
	datacenterID   int64
	nodeBits       uint8
	datacenterBits uint8
	stepBits       uint8
	epochMilli     int64
}

// workerBits is what is left of the node segment once the datacenter has taken
// its share. Only meaningful once validate has passed.
func (c config) workerBits() uint8 { return c.nodeBits - c.datacenterBits }

// datacenter is the id to actually pack into an id. A negative datacenterID means
// "not given", which validate only tolerates when there are no datacenter bits to
// put it in — so by the time this is called, it can only be resolving to the one
// datacenter that exists.
func (c config) datacenter() int64 {
	if c.datacenterID < 0 {
		return 0
	}
	return c.datacenterID
}

// layout is how ids get packed, and is what GetLayout hands to clients so they can
// take them apart again.
func (c config) layout() snowflake.Layout {
	return snowflake.Layout{
		EpochMilli:     c.epochMilli,
		DatacenterBits: c.datacenterBits,
		WorkerBits:     c.workerBits(),
		StepBits:       c.stepBits,
	}
}

// validate rejects a configuration that cannot produce correct IDs. Nothing below
// here will: see maxNodeAndStepBits.
func (c config) validate(now time.Time) error {
	if c.workerID < 0 {
		return errors.New("--worker-id is required: two live processes sharing an identity issue the same ids")
	}
	if used := int(c.nodeBits) + int(c.stepBits); used > maxNodeAndStepBits {
		return fmt.Errorf("--node-bits(%d) + --step-bits(%d) = %d, and snowflake has only %d bits "+
			"for the two of them; everything else is the timestamp",
			c.nodeBits, c.stepBits, used, maxNodeAndStepBits)
	}
	if c.datacenterBits > c.nodeBits {
		return fmt.Errorf("--datacenter-bits(%d) cannot exceed --node-bits(%d): the datacenter is "+
			"the top of the node segment, and the worker is what is left of it",
			c.datacenterBits, c.nodeBits)
	}

	// A datacenter id is required exactly when there are bits to put it in, for the
	// same reason --worker-id is required: a default of 0 is a default identity, and
	// the second cluster to take it silently issues the same ids as the first. Making
	// it explicit means the collision cannot happen by copying a manifest.
	if c.datacenterBits > 0 && c.datacenterID < 0 {
		return errors.New("--datacenter-id is required whenever --datacenter-bits is set: " +
			"two clusters both left at datacenter 0 issue the same ids")
	}
	// And it is meaningless when there are none: it would have nowhere to go, so an id
	// asking to be datacenter 3 without a datacenter segment is a config that does not
	// mean what its author thinks it means.
	if c.datacenterBits == 0 && c.datacenterID > 0 {
		return fmt.Errorf("--datacenter-id=%d has nowhere to go: --datacenter-bits is 0, so ids "+
			"carry no datacenter at all", c.datacenterID)
	}

	// Each id has to fit its own share of the node segment. If either overflowed it
	// would spill into the other's bits and land on an identity belonging to a
	// different (datacenter, worker) pair — a live collision, not a rounding error.
	if max := int64(1)<<c.datacenterBits - 1; c.datacenter() > max {
		return fmt.Errorf("--datacenter-id=%d is out of range [0,%d] for --datacenter-bits=%d",
			c.datacenterID, max, c.datacenterBits)
	}
	if max := int64(1)<<c.workerBits() - 1; c.workerID > max {
		return fmt.Errorf("--worker-id=%d is out of range [0,%d]: --node-bits(%d) less "+
			"--datacenter-bits(%d) leaves %d bits for the worker",
			c.workerID, max, c.nodeBits, c.datacenterBits, c.workerBits())
	}

	// The timestamp gets whatever the node and step segments do not, which the check
	// above keeps at 41 bits or more — about 69 years. Only an epoch older than that
	// can overflow it.
	elapsed := now.UnixMilli() - c.epochMilli
	bits := c.layout().TimestampBits()
	if elapsed < 0 || elapsed>>bits != 0 {
		return fmt.Errorf("--epoch=%d (%s) does not fit in %d timestamp bits, which span %s",
			c.epochMilli, time.UnixMilli(c.epochMilli).UTC().Format(time.RFC3339), bits,
			time.Duration(int64(1)<<bits)*time.Millisecond)
	}
	return nil
}

func run(args []string) error {
	// On Kubernetes the worker id comes from the environment, because that is where
	// a StatefulSet can put its pod's ordinal. See envWorkerID.
	envWorker, err := envWorkerID()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("snowid-server", flag.ContinueOnError)
	var (
		addr = fs.String("addr", ":50051", "gRPC listen address")
		// The worker id is the one value that differs from pod to pod, and a manifest
		// cannot write a different flag per replica — hence the environment. The
		// datacenter is the opposite: one value for the whole deployment, the same for
		// every replica in it, so it is an ordinary flag alongside the layout it belongs to.
		workerID = fs.Int64("worker-id", envWorker, "this worker's id within its datacenter; required, and no two live processes may share the same (datacenter, worker) pair [SNOWID_WORKER_ID]")
		dcID     = fs.Int64("datacenter-id", -1, "this datacenter's id; one value per cluster, shared by every replica in it. Required whenever --datacenter-bits is set")
		nodeBits = fs.Uint("node-bits", 10, "width of the whole node segment, datacenter and worker together; permanent once ids are issued")
		dcBits   = fs.Uint("datacenter-bits", 0, "how much of the node segment is the datacenter; the worker gets the rest. 0 means no datacenters; permanent once ids are issued")
		stepBits = fs.Uint("step-bits", 12, "width of the step segment: ids per millisecond per worker. node-bits + step-bits may not exceed 22; permanent once ids are issued")
		epochMs  = fs.Int64("epoch", 1727712000000, "zero point of the id timestamp, in unix milliseconds; permanent once ids are issued")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config{
		addr:           *addr,
		workerID:       *workerID,
		datacenterID:   *dcID,
		nodeBits:       uint8(*nodeBits),
		datacenterBits: uint8(*dcBits),
		stepBits:       uint8(*stepBits),
		epochMilli:     *epochMs,
	}
	if err := cfg.validate(time.Now()); err != nil {
		return err
	}
	layout := cfg.layout()

	// bwmarrin has one identity segment, not two, and knows nothing of datacenters.
	// So the pair is packed into that single segment — concatenated, never added;
	// see Layout.PackID — and bwmarrin's "node" is the result.
	nodeID := layout.PackID(cfg.datacenter(), cfg.workerID)

	// bwmarrin keeps the layout in package-level variables, which NewNode reads.
	bw.Epoch, bw.NodeBits, bw.StepBits = cfg.epochMilli, cfg.nodeBits, cfg.stepBits
	node, err := bw.NewNode(nodeID)
	if err != nil {
		return fmt.Errorf("new node: %w", err)
	}

	fields := []any{
		"version", version,
		"worker_id", cfg.workerID,
		"epoch", time.UnixMilli(cfg.epochMilli).UTC().Format(time.RFC3339),
		"node_bits", cfg.nodeBits,
		"step_bits", cfg.stepBits,
		"max_workers", int64(1) << cfg.workerBits(),
		"max_ids_per_second", (int64(1) << cfg.stepBits) * 1000,
		// When the timestamp segment runs out of room and ids start colliding. Worth
		// seeing at startup, because the layout cannot be changed later.
		"ids_valid_until", time.UnixMilli(cfg.epochMilli + int64(1)<<layout.TimestampBits()).UTC().Format(time.RFC3339),
	}
	// Only mention datacenters when there are any. On an ordinary startup
	// "datacenter_id=0 datacenter_bits=0" is two fields saying nothing.
	if cfg.datacenterBits > 0 {
		fields = append(fields,
			"datacenter_id", cfg.datacenter(),
			"datacenter_bits", cfg.datacenterBits,
			"max_datacenters", int64(1)<<cfg.datacenterBits,
			"node_id", nodeID, // what actually goes into every id's node segment
		)
	}
	slog.Info("generator ready", fields...)

	lis, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.addr, err)
	}

	s := grpc.NewServer()
	snowidv1.RegisterSnowIdServer(s, server.New(node, layout, cfg.datacenter(), cfg.workerID))

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
		// Stop trapping signals, so a second Ctrl-C kills us outright rather than
		// being swallowed while we drain.
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

	slog.Info("serving", "addr", cfg.addr)
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
