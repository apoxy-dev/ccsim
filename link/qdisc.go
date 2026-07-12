package link

import "time"

// DropReason distinguishes queue drop causes in telemetry.
type DropReason int

const (
	DropTail   DropReason = iota // queue limit exceeded
	DropAQM                      // active queue management decision (RED/CoDel)
	DropWire                     // random wire loss
	DropForced                   // scripted drop injected by the scenario
)

func (r DropReason) String() string {
	switch r {
	case DropTail:
		return "tail"
	case DropAQM:
		return "aqm"
	case DropWire:
		return "wire"
	case DropForced:
		return "forced"
	}
	return "?"
}

// qdiscSink receives drop/mark notifications from inside a discipline
// (CoDel drops at dequeue time, RED marks at enqueue time, ...).
type qdiscSink interface {
	qdiscDropped(p *Packet, reason DropReason)
	qdiscMarked(p *Packet)
}

// Qdisc is a queueing discipline for one link direction.
//
// Enqueue returns dropped=true when the packet was not accepted (the qdisc
// has already reported the drop to the sink). Dequeue returns nil when
// empty; it may internally drop or CE-mark packets (reported via the sink)
// before returning the packet to transmit.
type Qdisc interface {
	Enqueue(p *Packet, now time.Duration) (dropped bool)
	Dequeue(now time.Duration) *Packet
	Len() int
	Bytes() int
	// SetLimit updates the queue capacity (packets and/or bytes; zero means
	// leave unchanged / unlimited depending on discipline).
	SetLimit(pkts, bytes int)
}

// fifo is a ring-buffer packet FIFO used as a building block. A ring
// (rather than a slide-forward slice) keeps the enqueue/dequeue hot path
// allocation-free: the previous append/reslice pattern allocated once per
// packet whenever the queue oscillated around empty, which is the common
// uncongested case.
type fifo struct {
	pkts  []*Packet // ring storage; nil until first push
	head  int
	count int
	bytes int
}

func (q *fifo) push(p *Packet) {
	if q.count == len(q.pkts) {
		grown := make([]*Packet, max(16, 2*len(q.pkts)))
		for i := 0; i < q.count; i++ {
			grown[i] = q.pkts[(q.head+i)%len(q.pkts)]
		}
		q.pkts = grown
		q.head = 0
	}
	q.pkts[(q.head+q.count)%len(q.pkts)] = p
	q.count++
	q.bytes += p.Size()
}

func (q *fifo) pop() *Packet {
	if q.count == 0 {
		return nil
	}
	p := q.pkts[q.head]
	q.pkts[q.head] = nil
	q.head = (q.head + 1) % len(q.pkts)
	q.count--
	q.bytes -= p.Size()
	return p
}

func (q *fifo) len() int { return q.count }

// TailDrop is a FIFO with packet- and/or byte-count limits (0 = unlimited,
// but at least one limit must be set by the caller for a finite queue).
type TailDrop struct {
	fifo
	limitPkts  int
	limitBytes int
	sink       qdiscSink
}

// NewTailDrop creates a tail-drop FIFO.
func NewTailDrop(limitPkts, limitBytes int, sink qdiscSink) *TailDrop {
	return &TailDrop{limitPkts: limitPkts, limitBytes: limitBytes, sink: sink}
}

// Enqueue implements Qdisc.
func (q *TailDrop) Enqueue(p *Packet, now time.Duration) bool {
	if (q.limitPkts > 0 && q.len()+1 > q.limitPkts) ||
		(q.limitBytes > 0 && q.bytes+p.Size() > q.limitBytes) {
		q.sink.qdiscDropped(p, DropTail)
		return true
	}
	q.push(p)
	return false
}

// Dequeue implements Qdisc.
func (q *TailDrop) Dequeue(now time.Duration) *Packet { return q.pop() }

// Len implements Qdisc.
func (q *TailDrop) Len() int { return q.fifo.len() }

// Bytes implements Qdisc.
func (q *TailDrop) Bytes() int { return q.fifo.bytes }

// SetLimit implements Qdisc.
func (q *TailDrop) SetLimit(pkts, bytes int) {
	if pkts > 0 {
		q.limitPkts = pkts
	}
	if bytes > 0 {
		q.limitBytes = bytes
	}
}
