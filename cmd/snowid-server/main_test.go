package main

import (
	"os"
	"testing"
	"time"

	"github.com/itmisx/snowid-server/pkg/snowflake"
)

var (
	testNow   = time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	testEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
)

func layout(datacenterBits, workerBits uint8) snowflake.Layout {
	return snowflake.Layout{
		EpochMilli:     testEpoch.UnixMilli(),
		DatacenterBits: datacenterBits,
		WorkerBits:     workerBits,
		StepBits:       stepBits,
	}
}

// A StatefulSet hands each pod its ordinal through SNOWID_WORKER_ID. A Deployment
// sets no pod-index label, so the downward API yields "" — and that must stop the
// server, not become a guess. Two processes sharing an identity issue the same ids.
func TestWorkerIDFromTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		set   bool
		want  int64
		fails bool
	}{
		{name: "statefulset pod 0", env: "0", set: true, want: 0},
		{name: "statefulset pod 7", env: "7", set: true, want: 7},
		{name: "deployment: no pod-index label", env: "", set: true, want: -1},
		{name: "not set at all", set: false, want: -1},
		{name: "malformed", env: "abc", set: true, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SNOWID_WORKER_ID", tc.env)
			} else {
				os.Unsetenv("SNOWID_WORKER_ID")
			}

			got, err := envWorkerID()
			if tc.fails {
				if err == nil {
					t.Fatalf("envWorkerID(%q) succeeded, want an error — a malformed id must never "+
						"silently become somebody else's", tc.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("envWorkerID(%q): %v", tc.env, err)
			}
			if got != tc.want {
				t.Fatalf("envWorkerID(%q) = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// -1 — what an empty variable resolves to — must be refused, which is what makes
// running this as a Deployment fail closed rather than collide.
func TestDeploymentFailsClosed(t *testing.T) {
	if err := validate(0, -1, layout(0, 10), testNow); err == nil {
		t.Fatal("a missing worker id was accepted; a Deployment would start and collide")
	}
}

func TestValidateAcceptsTheDefaults(t *testing.T) {
	if err := validate(0, 0, layout(snowflake.DefaultDatacenterBits, snowflake.DefaultWorkerBits), testNow); err != nil {
		t.Fatalf("the default layout must be valid: %v", err)
	}
}

// Twitter's original split: 32 datacenters of 32 workers.
func TestValidateAcceptsTwitterSplit(t *testing.T) {
	if err := validate(31, 31, layout(5, 5), testNow); err != nil {
		t.Fatalf("the last valid pair (31,31) of a 5/5 split was rejected: %v", err)
	}
}

// Either id overflowing its segment would spill into the other's bits and land on
// an identity that belongs to somebody else.
func TestValidateRejectsIDsThatOverflowTheirSegment(t *testing.T) {
	l := layout(5, 5) // 32 datacenters of 32 workers

	if err := validate(32, 0, l, testNow); err == nil {
		t.Error("--datacenter-id=32 does not fit in 5 bits, want an error")
	}
	if err := validate(0, 32, l, testNow); err == nil {
		t.Error("--worker-id=32 does not fit in 5 bits, want an error")
	}
	if err := validate(-1, 0, l, testNow); err == nil {
		t.Error("a negative datacenter id was accepted")
	}
}

// The one that earns validate its place. bwmarrin's Generate() shifts the
// timestamp into position with no bounds check, so a layout with too few timestamp
// bits does not fail — it silently emits ids whose time is wrong, whose sign bit
// is set, and which repeat once the segment wraps. Nothing downstream would ever
// notice. So it has to be caught here, at startup, or not at all.
func TestValidateRejectsALayoutThatWouldSilentlyOverflow(t *testing.T) {
	// 7 datacenter bits + 12 worker bits + 12 step bits leaves 32 for the
	// timestamp: 49.7 days of range, against an epoch six years back.
	err := validate(0, 0, layout(7, 12), testNow)
	if err == nil {
		t.Fatal("a layout whose timestamp overflowed six years ago was accepted")
	}
	t.Log(err)
}

func TestValidateRejectsAnEpochInTheFuture(t *testing.T) {
	l := layout(0, 10)
	l.EpochMilli = testNow.Add(time.Hour).UnixMilli()

	if err := validate(0, 0, l, testNow); err == nil {
		t.Fatal("an epoch in the future was accepted")
	}
}

// The worker segment's whole job is to keep two processes apart, so the boundary
// of what fits in it has to be exact.
func TestValidateWorkerIDBoundary(t *testing.T) {
	l := layout(0, 10) // 1024 workers, 0..1023

	if err := validate(0, 1023, l, testNow); err != nil {
		t.Errorf("worker id 1023 must fit in 10 bits: %v", err)
	}
	if err := validate(0, 1024, l, testNow); err == nil {
		t.Error("worker id 1024 does not fit in 10 bits, want an error")
	}
}
