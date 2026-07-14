// Package snowflake is the wire format of a snowflake ID: how one is packed, and
// how to take it apart again.
//
// An ID is laid out as, from the most significant bit down, the way Twitter's
// original snowflake laid it out:
//
//	| unused(1) | timestamp | datacenter(DatacenterBits) | worker(WorkerBits) | step(StepBits) |
//
// Uniqueness rests on all of it: the timestamp separates different milliseconds,
// the step separates IDs within one millisecond, and the datacenter and worker
// together separate concurrent processes. No two live processes may hold the same
// (datacenter, worker) pair.
//
// DatacenterBits is normally 0, which leaves that whole segment to the worker.
//
// This package does not generate IDs — the server does. What it holds is the
// vocabulary the server and its clients have to agree on, and in particular
// Layout, which is what lets a client decode an ID without a round trip: fetch the
// layout once over GetLayout, then decode locally forever.
//
// Layout is a value, deliberately. A client may talk to more than one service, or
// learn its layout at run time, and so must be able to hold more than one.
package snowflake

import (
	"strconv"
	"time"
)

// Defaults follow Twitter's original snowflake, with its 5 datacenter bits and 5
// worker bits merged into one 10-bit worker segment — 1024 workers, 4096 IDs per
// millisecond each, and ~69 years of timestamps before the epoch overflows.
//
// Set DatacenterBits to 5 and WorkerBits to 5 to get Twitter's split back.
const (
	DefaultDatacenterBits uint8 = 0
	DefaultWorkerBits     uint8 = 10
	DefaultStepBits       uint8 = 12
)

// ID is a snowflake ID.
type ID int64

// Int64 returns the ID as an int64.
func (id ID) Int64() int64 { return int64(id) }

// String returns the ID in base 10. JSON and JavaScript cannot hold a 64-bit
// integer exactly, so IDs cross those boundaries as strings.
func (id ID) String() string { return strconv.FormatInt(int64(id), 10) }

// Layout says how a generator packs its IDs, which is all a client needs to take
// one apart. Fetch it once at startup with GetLayout; decoding is a few bit
// operations and must never cost a round trip.
//
// The layout is permanent. Every ID ever issued is decoded with it, so changing
// the epoch or a segment width after the fact makes every existing ID decode to
// the wrong time and the wrong process.
type Layout struct {
	EpochMilli int64

	// DatacenterBits is the width of the datacenter segment. Zero — the usual
	// case — means there are no datacenters and Datacenter always returns 0.
	DatacenterBits uint8

	// WorkerBits is the width of the worker segment: how many workers there can
	// be *within one datacenter*.
	WorkerBits uint8

	// StepBits is the width of the step segment: how many IDs one worker can
	// issue within one millisecond.
	StepBits uint8
}

// Time returns the time an ID was generated.
func (l Layout) Time(id ID) time.Time {
	return time.UnixMilli(l.UnixMilli(id))
}

// UnixMilli returns the millisecond an ID was generated.
func (l Layout) UnixMilli(id ID) int64 {
	return int64(id)>>(l.DatacenterBits+l.WorkerBits+l.StepBits) + l.EpochMilli
}

// Datacenter returns the datacenter that generated an ID, and is always 0 when
// DatacenterBits is 0.
func (l Layout) Datacenter(id ID) int64 {
	return int64(id) >> (l.WorkerBits + l.StepBits) & (1<<l.DatacenterBits - 1)
}

// Worker returns the worker, within its datacenter, that generated an ID.
func (l Layout) Worker(id ID) int64 {
	return int64(id) >> l.StepBits & (1<<l.WorkerBits - 1)
}

// Step returns an ID's position within its millisecond.
func (l Layout) Step(id ID) int64 {
	return int64(id) & (1<<l.StepBits - 1)
}

// PackID packs a datacenter and a worker into the single segment that the
// underlying generator has room for.
//
// The two are CONCATENATED, not added. Adding them would put (datacenter=1,
// worker=2) and (datacenter=2, worker=1) on the same value — two live processes,
// one identity, and every ID they issue in the same millisecond a duplicate.
// Concatenation is what makes the *pair* unique.
func (l Layout) PackID(datacenterID, workerID int64) int64 {
	return datacenterID<<l.WorkerBits | workerID
}

// TimestampBits is what is left of the 63 usable bits once the other segments
// have taken their share. The 64th stays 0, so that IDs are positive in languages
// without unsigned integers.
func (l Layout) TimestampBits() uint8 {
	return 63 - l.DatacenterBits - l.WorkerBits - l.StepBits
}
