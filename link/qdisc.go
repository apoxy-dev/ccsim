package link

import "time"

// DropReason distinguishes queue drop causes in telemetry.
type DropReason int

const (
	DropTail DropReason = iota // queue limit exceeded
	DropAQM                    // active queue management decision (RED/CoDel)
	DropWire                   // random wire loss
)

func (r DropReason) String() string {
	switch r {
	case DropTail:
		return "tail"
	case DropAQM:
		return "aqm"
	case DropWire:
		return "wire"
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

// fifo is a simple slice-backed packet FIFO used as a building block.
type fifo struct {
	pkts  []*Packet
	bytes int
}

func (q *fifo) push(p *Packet) {
	q.pkts = append(q.pkts, p)
	q.bytes += p.Size()
}

func (q *fifo) pop() *Packet {
	if len(q.pkts) == 0 {
		return nil
	}
	p := q.pkts[0]
	q.pkts[0] = nil
	q.pkts = q.pkts[1:]
	q.bytes -= p.Size()
	return p
}

func (q *fifo) len() int { return len(q.pkts) }

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
