package snowflake

import (
	"testing"
	"time"
)

func testEpoch() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }

// twitterLayout is the original 5/5 split: 32 datacenters of 32 workers.
func twitterLayout() Layout {
	return Layout{
		EpochMilli:     testEpoch().UnixMilli(),
		DatacenterBits: 5,
		WorkerBits:     5,
		StepBits:       DefaultStepBits,
	}
}

// defaultLayout has no datacenters: the whole 10-bit segment is the worker.
func defaultLayout() Layout {
	return Layout{
		EpochMilli:     testEpoch().UnixMilli(),
		DatacenterBits: DefaultDatacenterBits,
		WorkerBits:     DefaultWorkerBits,
		StepBits:       DefaultStepBits,
	}
}

// pack builds an ID the way the server would, so the decode below has something
// with a known answer to take apart.
func pack(l Layout, unixMilli, datacenter, worker, step int64) ID {
	elapsed := unixMilli - l.EpochMilli
	return ID(elapsed<<(l.DatacenterBits+l.WorkerBits+l.StepBits) |
		datacenter<<(l.WorkerBits+l.StepBits) |
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
	if got := l.Datacenter(id); got != 31 {
		t.Errorf("datacenter = %d, want 31", got)
	}
	if got := l.Worker(id); got != 17 {
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

	if got := l.Datacenter(id); got != 0 {
		t.Errorf("datacenter = %d with no datacenter segment, want 0", got)
	}
	if got := l.Worker(id); got != 1023 {
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

	a := l.PackID(1, 2)
	b := l.PackID(2, 1)
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
		for w := range int64(1) << l.WorkerBits {
			packed := l.PackID(dc, w)
			if prev, dup := seen[packed]; dup {
				t.Fatalf("(dc=%d,worker=%d) and (dc=%d,worker=%d) both pack to %d",
					prev[0], prev[1], dc, w, packed)
			}
			seen[packed] = [2]int64{dc, w}

			id := pack(l, testEpoch().UnixMilli(), dc, w, 0)
			if got := l.Datacenter(id); got != dc {
				t.Fatalf("(dc=%d,worker=%d) decodes to datacenter %d", dc, w, got)
			}
			if got := l.Worker(id); got != w {
				t.Fatalf("(dc=%d,worker=%d) decodes to worker %d", dc, w, got)
			}
		}
	}
	if want := 1 << (l.DatacenterBits + l.WorkerBits); len(seen) != want {
		t.Fatalf("%d distinct identities, want %d — the split wastes the segment", len(seen), want)
	}
}

// The decode formulas the proto hands to clients writing their own decoder must
// agree with this package, or those clients are quietly wrong.
func TestLayoutMatchesTheDocumentedFormulas(t *testing.T) {
	l := twitterLayout()
	id := pack(l, time.Now().UnixMilli(), 3, 7, 1234)

	wantMilli := (int64(id) >> (l.DatacenterBits + l.WorkerBits + l.StepBits)) + l.EpochMilli
	wantDC := (int64(id) >> (l.WorkerBits + l.StepBits)) & ((1 << l.DatacenterBits) - 1)
	wantWorker := (int64(id) >> l.StepBits) & ((1 << l.WorkerBits) - 1)
	wantStep := int64(id) & ((1 << l.StepBits) - 1)

	if got := l.UnixMilli(id); got != wantMilli {
		t.Errorf("UnixMilli = %d, documented formula gives %d", got, wantMilli)
	}
	if got := l.Datacenter(id); got != wantDC {
		t.Errorf("Datacenter = %d, documented formula gives %d", got, wantDC)
	}
	if got := l.Worker(id); got != wantWorker {
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
