// Package snowflake is the wire format of a snowflake ID: how one is packed, and
// how to take it apart again.
//
// An ID is laid out as, from the most significant bit down, the way Twitter's
// original snowflake laid it out:
//
//	| unused(1) | timestamp | datacenter | worker | step(StepBits) |
//	                         \____ node segment (NodeBits) ____/
//
// The datacenter and worker are one segment, split in two: the node segment is
// NodeBits wide, the datacenter takes DatacenterBits off its top, and the worker
// gets whatever is left. So there is no WorkerBits field — it is derived (see
// WorkerBits), and a split that does not add up cannot be written down.
//
// Uniqueness rests on all of it: the timestamp separates different milliseconds,
// the step separates IDs within one millisecond, and the datacenter and worker
// together separate concurrent processes. No two live processes may hold the same
// (datacenter, worker) pair.
//
// DatacenterBits is normally 0, which leaves the whole node segment to the worker.
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
	"fmt"
	"strconv"
	"time"
)

// Defaults follow Twitter's original snowflake, with its 5 datacenter bits and 5
// worker bits merged into one 10-bit node segment given whole to the worker — 1024
// workers, 4096 IDs per millisecond each, and ~69 years of timestamps before the
// epoch overflows. NodeBits + StepBits is 22, snowflake's whole budget for the two.
//
// Set DatacenterBits to 5 to get Twitter's 5/5 split back, 32 datacenters of 32.
const (
	DefaultNodeBits       uint8 = 10
	DefaultDatacenterBits uint8 = 0
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

	// NodeBits is the width of the whole node segment — the machine identity, one
	// contiguous span the datacenter and worker share. It is exactly the budget the
	// timestamp does not get: NodeBits + StepBits is snowflake's 22, and the
	// timestamp is the 41 or more left over.
	NodeBits uint8

	// DatacenterBits is how much of the node segment, from its top, is the
	// datacenter; the worker gets the rest. Zero — the usual case — means there are
	// no datacenters and DatacenterID always returns 0.
	DatacenterBits uint8

	// StepBits is the width of the step segment: how many IDs one worker can
	// issue within one millisecond.
	StepBits uint8
}

// WorkerBits is what is left of the node segment once the datacenter has taken its
// share off the top: how many workers there can be *within one datacenter*. It is
// derived, never stored, so a datacenter wider than the node segment cannot be
// represented — only produced, and the server rejects that at startup.
func (l Layout) WorkerBits() uint8 { return l.NodeBits - l.DatacenterBits }

// Time returns the time an ID was generated.
func (l Layout) Time(id ID) time.Time {
	return time.UnixMilli(l.UnixMilli(id))
}

// UnixMilli returns the millisecond an ID was generated. The datacenter/worker
// split does not matter here — only the whole node segment sits between the
// timestamp and the step.
func (l Layout) UnixMilli(id ID) int64 {
	return int64(id)>>(l.NodeBits+l.StepBits) + l.EpochMilli
}

// DatacenterID returns the datacenter that generated an ID, and is always 0 when
// DatacenterBits is 0.
func (l Layout) DatacenterID(id ID) int64 {
	return int64(id) >> (l.WorkerBits() + l.StepBits) & (1<<l.DatacenterBits - 1)
}

// WorkerID returns the worker, within its datacenter, that generated an ID.
func (l Layout) WorkerID(id ID) int64 {
	return int64(id) >> l.StepBits & (1<<l.WorkerBits() - 1)
}

// Step returns an ID's position within its millisecond.
func (l Layout) Step(id ID) int64 {
	return int64(id) & (1<<l.StepBits - 1)
}

// NodeID packs a datacenter and a worker into the single node segment that the
// underlying generator has room for. It is the inverse of DatacenterID and
// WorkerID: what those take apart, this puts together.
//
// The two are CONCATENATED, not added. Adding them would put (datacenter=1,
// worker=2) and (datacenter=2, worker=1) on the same value — two live processes,
// one identity, and every ID they issue in the same millisecond a duplicate.
// Concatenation is what makes the *pair* unique.
func (l Layout) NodeID(datacenterID, workerID int64) int64 {
	return datacenterID<<l.WorkerBits() | workerID
}

// TimestampBits is what is left of the 63 usable bits once the node and step
// segments have taken their share — 63 - (NodeBits + StepBits), so staying inside
// snowflake's 22-bit budget for those two always leaves the timestamp at least 41.
// The 64th bit stays 0, so that IDs are positive in languages without unsigned
// integers.
func (l Layout) TimestampBits() uint8 {
	return 63 - l.NodeBits - l.StepBits
}

// Valid reports whether the layout's segment widths are self-consistent: that the
// datacenter fits inside the node segment, and that node and step together leave
// room for a timestamp in a 63-bit id. A layout that fails this decodes to
// nonsense — WorkerBits and TimestampBits are unsigned subtractions that wrap
// rather than go negative, so an over-wide segment silently produces junk instead
// of an error.
//
// What Valid CANNOT do is tell you the layout is the RIGHT one. A layout with the
// wrong StepBits, or the wrong epoch, is perfectly self-consistent and will still
// decode every id to a plausible-looking wrong answer — a forgotten StepBits of 0,
// for instance, reads the low bits of the step segment as the worker. The only way
// to be sure a layout matches the generator that made an id is to fetch it from
// that generator with GetLayout, not to write the widths out by hand.
func (l Layout) Valid() error {
	if l.DatacenterBits > l.NodeBits {
		return fmt.Errorf("snowflake: datacenter bits (%d) exceed node bits (%d): the datacenter "+
			"is carved from the top of the node segment, it cannot be wider than it", l.DatacenterBits, l.NodeBits)
	}
	if used := int(l.NodeBits) + int(l.StepBits); used > 63 {
		return fmt.Errorf("snowflake: node bits (%d) + step bits (%d) = %d leave no room for a "+
			"timestamp in a 63-bit id", l.NodeBits, l.StepBits, used)
	}
	return nil
}
