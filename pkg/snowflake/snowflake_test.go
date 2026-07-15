package snowflake

import (
	"fmt"
	"testing"
	"time"
)

func testEpoch() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }

// twitterLayout is the original 5/5 split: 32 datacenters of 32 workers. Ten node
// bits, five of them the datacenter.
func twitterLayout() Layout {
	return Layout{
		EpochMilli:     testEpoch().UnixMilli(),
		NodeBits:       10,
		DatacenterBits: 5,
		StepBits:       DefaultStepBits,
	}
}

// defaultLayout has no datacenters: the whole node segment is the worker.
func defaultLayout() Layout {
	return Layout{
		EpochMilli:     testEpoch().UnixMilli(),
		NodeBits:       DefaultNodeBits,
		DatacenterBits: DefaultDatacenterBits,
		StepBits:       DefaultStepBits,
	}
}

// pack builds an ID the way the server would, so the decode below has something
// with a known answer to take apart.
func pack(l Layout, unixMilli, datacenter, worker, step int64) ID {
	elapsed := unixMilli - l.EpochMilli
	return ID(elapsed<<(l.NodeBits+l.StepBits) |
		datacenter<<(l.WorkerBits()+l.StepBits) |
		worker<<l.StepBits |
		step)
}

func TestLayoutDecodesEverySegment(t *testing.T) {
	l := twitterLayout()
	when := time.Date(2026, 7, 14, 12, 34, 56, 789_000_000, time.UTC)

	id := pack(l, when.UnixMilli(), 31, 17, 4095)

	if got := l.UnixMilli(id); got != when.UnixMilli() {
		t.Errorf("unix milli = %d, want %d", got, when.UnixMilli())
	}
	if got := l.Time(id); !got.Equal(when) {
		t.Errorf("time = %s, want %s", got, when)
	}
	if got := l.DatacenterID(id); got != 31 {
		t.Errorf("datacenter = %d, want 31", got)
	}
	if got := l.WorkerID(id); got != 17 {
		t.Errorf("worker = %d, want 17", got)
	}
	if got := l.Step(id); got != 4095 {
		t.Errorf("step = %d, want 4095", got)
	}
	if id <= 0 {
		t.Errorf("id %d is not positive; the sign bit must stay clear", id)
	}
}

// With no datacenter segment, Datacenter is always 0 and the worker gets the
// whole segment. This is the default, so it is the case that must not be subtly
// wrong.
func TestLayoutWithoutDatacenters(t *testing.T) {
	l := defaultLayout()
	when := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

	id := pack(l, when.UnixMilli(), 0, 1023, 7)

	if got := l.DatacenterID(id); got != 0 {
		t.Errorf("datacenter = %d with no datacenter segment, want 0", got)
	}
	if got := l.WorkerID(id); got != 1023 {
		t.Errorf("worker = %d, want 1023 — the worker must get the whole segment", got)
	}
	if got := l.Step(id); got != 7 {
		t.Errorf("step = %d, want 7", got)
	}
	if got := l.UnixMilli(id); got != when.UnixMilli() {
		t.Errorf("unix milli = %d, want %d", got, when.UnixMilli())
	}
}

// PackID concatenates; it does not add. Adding would put (datacenter=1, worker=2)
// and (datacenter=2, worker=1) on the same value — two live processes with one
// identity, and every id they issue in the same millisecond a duplicate.
func TestPackIDIsAConcatenationNotASum(t *testing.T) {
	l := twitterLayout()

	a := l.NodeID(1, 2)
	b := l.NodeID(2, 1)
	if a == b {
		t.Fatalf("(dc=1,worker=2) and (dc=2,worker=1) both pack to %d — two processes, one identity", a)
	}
	if want := int64(1<<5 | 2); a != want {
		t.Fatalf("PackID(1,2) = %d, want %d (a concatenation, not 1+2)", a, want)
	}
	if want := int64(2<<5 | 1); b != want {
		t.Fatalf("PackID(2,1) = %d, want %d", b, want)
	}

	// Every pair in the whole space lands on its own value, and every id decodes
	// back to the pair that made it.
	seen := make(map[int64][2]int64)
	for dc := range int64(1) << l.DatacenterBits {
		for w := range int64(1) << l.WorkerBits() {
			packed := l.NodeID(dc, w)
			if prev, dup := seen[packed]; dup {
				t.Fatalf("(dc=%d,worker=%d) and (dc=%d,worker=%d) both pack to %d",
					prev[0], prev[1], dc, w, packed)
			}
			seen[packed] = [2]int64{dc, w}

			id := pack(l, testEpoch().UnixMilli(), dc, w, 0)
			if got := l.DatacenterID(id); got != dc {
				t.Fatalf("(dc=%d,worker=%d) decodes to datacenter %d", dc, w, got)
			}
			if got := l.WorkerID(id); got != w {
				t.Fatalf("(dc=%d,worker=%d) decodes to worker %d", dc, w, got)
			}
		}
	}
	if want := 1 << l.NodeBits; len(seen) != want {
		t.Fatalf("%d distinct identities, want %d — the split wastes the segment", len(seen), want)
	}
}

// The decode formulas the proto hands to clients writing their own decoder must
// agree with this package, or those clients are quietly wrong.
func TestLayoutMatchesTheDocumentedFormulas(t *testing.T) {
	l := twitterLayout()
	id := pack(l, time.Now().UnixMilli(), 3, 7, 1234)

	// A third-party decoder has datacenter_bits, worker_bits and step_bits off the
	// wire — the same three widths this package's methods reduce to.
	dcBits, workerBits, stepBits := l.DatacenterBits, l.WorkerBits(), l.StepBits
	wantMilli := (int64(id) >> (dcBits + workerBits + stepBits)) + l.EpochMilli
	wantDC := (int64(id) >> (workerBits + stepBits)) & ((1 << dcBits) - 1)
	wantWorker := (int64(id) >> stepBits) & ((1 << workerBits) - 1)
	wantStep := int64(id) & ((1 << stepBits) - 1)

	if got := l.UnixMilli(id); got != wantMilli {
		t.Errorf("UnixMilli = %d, documented formula gives %d", got, wantMilli)
	}
	if got := l.DatacenterID(id); got != wantDC {
		t.Errorf("Datacenter = %d, documented formula gives %d", got, wantDC)
	}
	if got := l.WorkerID(id); got != wantWorker {
		t.Errorf("Worker = %d, documented formula gives %d", got, wantWorker)
	}
	if got := l.Step(id); got != wantStep {
		t.Errorf("Step = %d, documented formula gives %d", got, wantStep)
	}
}

func TestTimestampBits(t *testing.T) {
	if got := defaultLayout().TimestampBits(); got != 41 {
		t.Errorf("default layout leaves %d timestamp bits, want 41", got)
	}
	if got := twitterLayout().TimestampBits(); got != 41 {
		t.Errorf("twitter's 5/5 split leaves %d timestamp bits, want 41", got)
	}
}

// Valid catches the layouts whose widths cannot add up — the ones whose decode
// math wraps rather than errors. It cannot catch a layout that is merely wrong (a
// forgotten StepBits is a legal layout), which is why the Layout RPC exists.
func TestValid(t *testing.T) {
	if err := twitterLayout().Valid(); err != nil {
		t.Errorf("twitter's 5/5 split is a valid layout: %v", err)
	}
	if err := defaultLayout().Valid(); err != nil {
		t.Errorf("the default layout is valid: %v", err)
	}

	// The datacenter cannot be wider than the node segment it is carved from. Left
	// unchecked, WorkerBits() = NodeBits - DatacenterBits underflows the uint8 and
	// Worker/Datacenter decode to junk.
	tooWide := Layout{NodeBits: 10, DatacenterBits: 11, StepBits: DefaultStepBits}
	if err := tooWide.Valid(); err == nil {
		t.Error("datacenter bits wider than node bits was accepted; WorkerBits would underflow")
	}

	// Node and step together must leave room for a timestamp.
	noTimestamp := Layout{NodeBits: 40, StepBits: 40}
	if err := noTimestamp.Valid(); err == nil {
		t.Error("node+step = 80 bits was accepted; there is no room for a timestamp in 63 bits")
	}

	// The trap that started all this: a forgotten StepBits is a *legal* layout, so
	// Valid says nothing. Only the generator's own layout, via the Layout RPC, can save
	// you here — not a self-check.
	forgotStep := Layout{NodeBits: 10, DatacenterBits: 5} // StepBits left 0
	if err := forgotStep.Valid(); err != nil {
		t.Errorf("a zero StepBits is structurally valid, Valid must not flag it: %v", err)
	}
}

// IDs cross JSON and JavaScript as strings, because neither can hold a 64-bit
// integer exactly.
func TestIDString(t *testing.T) {
	id := ID(864789624059359232)
	if got, want := id.String(), "864789624059359232"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := id.Int64(); got != 864789624059359232 {
		t.Fatalf("Int64() = %d, want 864789624059359232", got)
	}
}
func TestXxx(t *testing.T) {
	l := Layout{
		EpochMilli:     1727712000000,
		NodeBits:       10,
		DatacenterBits: 5,
		StepBits:       12,
	}
	fmt.Println(l.WorkerID(236432870472683520))
	fmt.Println(l.WorkerID(236463572589215744))
	fmt.Println(l.Step(236484211442192384))
}
