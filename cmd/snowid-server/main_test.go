package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

var (
	testNow   = time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	testEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
)

// cfg is a valid default that each test bends in exactly one direction. It has no
// datacenters, so -1 ("not given") is the right datacenter id: there are no bits to
// put one in.
func cfg() config {
	return config{
		addr:           ":50052",
		workerID:       0,
		datacenterID:   -1,
		nodeBits:       10,
		datacenterBits: 0,
		stepBits:       12,
		epochMilli:     testEpoch,
	}
}

func TestValidateAcceptsTheDefaults(t *testing.T) {
	if err := cfg().validate(testNow); err != nil {
		t.Fatalf("the default layout must be valid: %v", err)
	}
}

// Twitter's original: the 10-bit node segment split 5/5.
func TestValidateAcceptsTwitterSplit(t *testing.T) {
	c := cfg()
	c.datacenterBits, c.datacenterID, c.workerID = 5, 31, 31

	if err := c.validate(testNow); err != nil {
		t.Fatalf("the last valid pair (31,31) of a 5/5 split was rejected: %v", err)
	}
	if got := c.workerBits(); got != 5 {
		t.Fatalf("worker bits = %d, want 5 — node-bits(10) less datacenter-bits(5)", got)
	}
}

// The whole rule, and the only one that needs stating: bwmarrin's own doc says
// "you have a total 22 bits to share between Node/Step" — and nothing in the
// library enforces it. Generate() shifts the timestamp clean out of its segment
// without a word, so this is the guard.
func TestNodeAndStepBitsCannotExceed22(t *testing.T) {
	for _, tc := range []struct {
		nodeBits, stepBits uint8
		wantAccepted       bool
	}{
		{10, 12, true},  // the default, and Twitter's: exactly 22
		{5, 17, true},   // 22
		{12, 10, true},  // 22
		{9, 12, true},   // 21, under the limit
		{11, 12, false}, // 23
		{12, 12, false}, // 24
		{20, 12, false}, // 32
	} {
		t.Run(fmt.Sprintf("node=%d/step=%d", tc.nodeBits, tc.stepBits), func(t *testing.T) {
			c := cfg()
			c.nodeBits, c.stepBits = tc.nodeBits, tc.stepBits

			err := c.validate(testNow)
			if accepted := err == nil; accepted != tc.wantAccepted {
				if tc.wantAccepted {
					t.Fatalf("rejected node(%d)+step(%d)=%d, which is within 22: %v",
						tc.nodeBits, tc.stepBits, tc.nodeBits+tc.stepBits, err)
				}
				t.Fatalf("accepted node(%d)+step(%d)=%d, over the 22 bits snowflake has for them; "+
					"the timestamp would be shifted out of its segment",
					tc.nodeBits, tc.stepBits, tc.nodeBits+tc.stepBits)
			}
		})
	}
}

// And the reason that one rule is enough: inside 22, the timestamp always keeps at
// least 41 bits, which is about 69 years. The layout cannot quietly expire, so
// nothing else has to check that it will not.
func TestStayingInside22LeavesAtLeast41TimestampBits(t *testing.T) {
	for nodeBits := uint8(0); nodeBits <= maxNodeAndStepBits; nodeBits++ {
		for stepBits := uint8(0); int(nodeBits)+int(stepBits) <= maxNodeAndStepBits; stepBits++ {
			c := cfg()
			c.nodeBits, c.stepBits = nodeBits, stepBits
			c.workerID, c.datacenterID = 0, 0

			if got := c.layout().TimestampBits(); got < 41 {
				t.Fatalf("node=%d step=%d leaves only %d timestamp bits", nodeBits, stepBits, got)
			}
		}
	}
}

// The datacenter is the top of the node segment; the worker is what is left. It
// cannot take more than there is.
func TestDatacenterBitsCannotExceedNodeBits(t *testing.T) {
	c := cfg()
	c.nodeBits, c.datacenterBits = 10, 11

	if err := c.validate(testNow); err == nil {
		t.Fatal("--datacenter-bits=11 does not fit inside --node-bits=10, want an error")
	}
}

// Either id overflowing its share of the node segment would spill into the other's
// bits and land on an identity that belongs to somebody else.
func TestIDsMustFitTheirShareOfTheNodeSegment(t *testing.T) {
	// 10 node bits split 5/5: 32 datacenters of 32 workers.
	split := func(dcID, workerID int64) config {
		c := cfg()
		c.datacenterBits, c.datacenterID, c.workerID = 5, dcID, workerID
		return c
	}

	if err := split(31, 31).validate(testNow); err != nil {
		t.Errorf("(31,31) is the last valid pair of a 5/5 split: %v", err)
	}
	if err := split(32, 0).validate(testNow); err == nil {
		t.Error("--datacenter-id=32 does not fit in 5 bits, want an error")
	}
	if err := split(0, 32).validate(testNow); err == nil {
		t.Error("--worker-id=32 does not fit in the 5 bits left over, want an error")
	}
}

// The datacenter id gets the same treatment as the worker id, and for the same
// reason: a default of 0 is a default IDENTITY. Two clusters that both take it are
// two processes with one identity, and they issue the same ids — which is exactly
// what happens when somebody copies a working manifest to a second cluster and
// changes nothing. So it must be spelled out whenever there are bits to hold it.
func TestDatacenterIDIsRequiredWhenThereAreDatacenterBits(t *testing.T) {
	c := cfg()
	c.datacenterBits, c.datacenterID = 5, -1 // -1 is what the flag defaults to

	if err := c.validate(testNow); err == nil {
		t.Fatal("--datacenter-bits=5 with no --datacenter-id was accepted; a second cluster " +
			"copying this manifest would silently issue duplicate ids")
	}

	c.datacenterID = 0 // spelled out, so the author meant it
	if err := c.validate(testNow); err != nil {
		t.Fatalf("an explicit --datacenter-id=0 must be accepted: %v", err)
	}
}

// And it is meaningless without them: it would have nowhere to go.
func TestDatacenterIDIsRefusedWithoutDatacenterBits(t *testing.T) {
	c := cfg() // datacenterBits: 0
	c.datacenterID = 3

	if err := c.validate(testNow); err == nil {
		t.Fatal("--datacenter-id=3 with --datacenter-bits=0 was accepted; the id has nowhere " +
			"to go, so the config does not mean what its author thinks")
	}
}

// Without datacenter bits, "not given" resolves to the one datacenter there is.
func TestDatacenterResolvesToZeroWhenThereAreNoDatacenters(t *testing.T) {
	c := cfg()
	if err := c.validate(testNow); err != nil {
		t.Fatalf("no datacenters and no --datacenter-id is the ordinary case: %v", err)
	}
	if got := c.datacenter(); got != 0 {
		t.Fatalf("datacenter() = %d, want 0 — never the -1 sentinel, which would pack a "+
			"negative into the node segment", got)
	}
}

// Without datacenters the worker gets the whole node segment.
func TestWorkerIDBoundaryWithoutDatacenters(t *testing.T) {
	c := cfg()
	if got := c.workerBits(); got != 10 {
		t.Fatalf("worker bits = %d, want the whole 10-bit node segment", got)
	}

	c.workerID = 1023
	if err := c.validate(testNow); err != nil {
		t.Errorf("worker id 1023 must fit in 10 bits: %v", err)
	}
	c.workerID = 1024
	if err := c.validate(testNow); err == nil {
		t.Error("worker id 1024 does not fit in 10 bits, want an error")
	}
}

func TestValidateRejectsABadEpoch(t *testing.T) {
	c := cfg()
	c.epochMilli = testNow.Add(time.Hour).UnixMilli()
	if err := c.validate(testNow); err == nil {
		t.Error("an epoch in the future was accepted")
	}

	// 41 timestamp bits span ~69 years, so an epoch from 1900 does not fit.
	c.epochMilli = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if err := c.validate(testNow); err == nil {
		t.Error("an epoch 126 years ago was accepted; 41 bits hold only ~69 years")
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
	c := cfg()
	c.workerID = -1

	if err := c.validate(testNow); err == nil {
		t.Fatal("a missing worker id was accepted; a Deployment would start and collide")
	}
}
