package link

import (
	"math"
	"time"
)

// REDParams configures Random Early Detection (Floyd & Jacobson 1993).
// Thresholds are in packets.
type REDParams struct {
	MinTh float64 // avg queue length to start marking/dropping
	MaxTh float64 // avg queue length for max probability
	MaxP  float64 // marking probability at MaxTh
	Wq    float64 // EWMA weight for the average queue estimate
	ECN   bool    // CE-mark ECT packets instead of dropping
}

// DefaultREDParams derives conventional RED parameters from a queue limit.
func DefaultREDParams(limitPkts int) REDParams {
	return REDParams{
		MinTh: float64(limitPkts) / 6,
		MaxTh: float64(limitPkts) / 2,
		MaxP:  0.02,
		Wq:    0.002,
	}
}

// RED implements the classic RED AQM over a FIFO with a hard tail-drop
// limit. The gentle variant is used above MaxTh (probability ramps from
// MaxP at MaxTh to 1 at 2*MaxTh).
type RED struct {
	fifo
	p          REDParams
	limitPkts  int
	sink       qdiscSink
	rng        func() float64
	avg        float64
	count      int // packets since last mark/drop
	idleSince  time.Duration
	idle       bool
	lastSample time.Duration
}

// NewRED creates a RED queue. rng must be a deterministic uniform [0,1)
// source derived from the scenario seed.
func NewRED(limitPkts int, p REDParams, rng func() float64, sink qdiscSink) *RED {
	if p.MaxTh <= p.MinTh {
		p.MaxTh = p.MinTh + 1
	}
	return &RED{p: p, limitPkts: limitPkts, sink: sink, rng: rng, idle: true, count: -1}
}

// Enqueue implements Qdisc.
func (q *RED) Enqueue(pk *Packet, now time.Duration) bool {
	// Update the average queue size estimate. While idle, decay the average
	// as if small packets had been transmitted the whole time (approximated
	// with a half-life style decay per ms idle).
	if q.idle {
		idleMs := float64((now - q.idleSince) / time.Millisecond)
		q.avg *= math.Pow(1-q.p.Wq, idleMs)
		q.idle = false
	}
	// Explicit conversions block FMA fusion (native/wasm parity).
	q.avg = float64((1-q.p.Wq)*q.avg) + float64(q.p.Wq*float64(q.len()))

	if q.limitPkts > 0 && q.len()+1 > q.limitPkts {
		q.sink.qdiscDropped(pk, DropTail)
		q.count = 0
		return true
	}

	if q.avg >= q.p.MinTh {
		var pb float64
		switch {
		case q.avg >= 2*q.p.MaxTh:
			pb = 1
		case q.avg >= q.p.MaxTh: // gentle RED
			pb = q.p.MaxP + float64((q.avg-q.p.MaxTh)/q.p.MaxTh*(1-q.p.MaxP))
		default:
			pb = q.p.MaxP * (q.avg - q.p.MinTh) / (q.p.MaxTh - q.p.MinTh)
		}
		q.count++
		pa := pb
		if q.count > 0 && float64(q.count)*pb < 1 {
			pa = pb / (1 - float64(float64(q.count)*pb))
		}
		if q.rng() < pa {
			q.count = 0
			if q.p.ECN && pk.MarkCE() {
				q.sink.qdiscMarked(pk)
				// marked packets are still queued
			} else {
				q.sink.qdiscDropped(pk, DropAQM)
				return true
			}
		}
	} else {
		q.count = -1
	}

	q.push(pk)
	return false
}

// Dequeue implements Qdisc.
func (q *RED) Dequeue(now time.Duration) *Packet {
	p := q.pop()
	if q.len() == 0 {
		q.idle = true
		q.idleSince = now
	}
	return p
}

// Len implements Qdisc.
func (q *RED) Len() int { return q.fifo.len() }

// Bytes implements Qdisc.
func (q *RED) Bytes() int { return q.fifo.bytes }

// SetLimit implements Qdisc.
func (q *RED) SetLimit(pkts, bytes int) {
	if pkts > 0 {
		q.limitPkts = pkts
	}
}
