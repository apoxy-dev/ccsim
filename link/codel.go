package link

import (
	"math"
	"time"
)

// CoDelParams configures the CoDel AQM (RFC 8289).
type CoDelParams struct {
	Target   time.Duration // acceptable standing-queue sojourn time (default 5ms)
	Interval time.Duration // sliding window (default 100ms)
	ECN      bool          // CE-mark ECT packets instead of dropping
}

// DefaultCoDelParams returns RFC 8289 defaults.
func DefaultCoDelParams() CoDelParams {
	return CoDelParams{Target: 5 * time.Millisecond, Interval: 100 * time.Millisecond}
}

// codelState is the per-queue CoDel control-law state, shared by CoDel and
// FQ-CoDel (RFC 8289 pseudocode structure).
type codelState struct {
	firstAboveTime time.Duration // when sojourn first went above target (0 = below)
	dropNext       time.Duration // next drop time while dropping
	count          uint32        // drops since entering drop state
	lastCount      uint32
	dropping       bool
}

func controlLaw(t time.Duration, interval time.Duration, count uint32) time.Duration {
	return t + time.Duration(float64(interval)/math.Sqrt(float64(count)))
}

// dequeueStep decides whether the head packet (sojourn time given) should be
// dropped/marked now. Returns okToDrop per the RFC's doDequeue helper.
func (s *codelState) shouldDrop(sojourn, now time.Duration, p CoDelParams, qBytes int) bool {
	const mtu = 1514
	if sojourn < p.Target || qBytes <= mtu {
		s.firstAboveTime = 0
		return false
	}
	if s.firstAboveTime == 0 {
		s.firstAboveTime = now + p.Interval
		return false
	}
	return now >= s.firstAboveTime
}

// CoDel implements RFC 8289 over a FIFO with a hard limit as overflow
// protection.
type CoDel struct {
	fifo
	p          CoDelParams
	limitPkts  int
	limitBytes int
	sink       qdiscSink
	st         codelState
}

// NewCoDel creates a CoDel queue.
func NewCoDel(limitPkts, limitBytes int, p CoDelParams, sink qdiscSink) *CoDel {
	if p.Target == 0 {
		p.Target = 5 * time.Millisecond
	}
	if p.Interval == 0 {
		p.Interval = 100 * time.Millisecond
	}
	return &CoDel{p: p, limitPkts: limitPkts, limitBytes: limitBytes, sink: sink}
}

// Enqueue implements Qdisc.
func (q *CoDel) Enqueue(pk *Packet, now time.Duration) bool {
	if (q.limitPkts > 0 && q.len()+1 > q.limitPkts) ||
		(q.limitBytes > 0 && q.bytes+pk.Size() > q.limitBytes) {
		q.sink.qdiscDropped(pk, DropTail)
		return true
	}
	pk.EnqueuedAt = now
	q.push(pk)
	return false
}

// Dequeue implements Qdisc. Applies the CoDel control law: in ECN mode
// packets are CE-marked and delivered instead of dropped.
func (q *CoDel) Dequeue(now time.Duration) *Packet {
	pk, ok2 := q.codelDequeue(now)
	st := &q.st
	if pk == nil {
		st.dropping = false
		return nil
	}
	if st.dropping {
		if !ok2 {
			st.dropping = false
		} else {
			for now >= st.dropNext && st.dropping {
				st.count++
				if q.dropOrMark(pk) {
					// dropped: pull the next packet and keep going
					var okDrop bool
					pk, okDrop = q.codelDequeue(now)
					if !okDrop {
						st.dropping = false
					}
					if pk == nil {
						st.dropping = false
						return nil
					}
				}
				st.dropNext = controlLaw(st.dropNext, q.p.Interval, st.count)
			}
		}
	} else if ok2 {
		// Enter dropping state.
		q.dropOrMarkEnter(&pk, now)
	}
	return pk
}

// dropOrMark handles one AQM action on pk. Returns true if the packet was
// dropped (caller must fetch another), false if it was marked and should be
// transmitted.
func (q *CoDel) dropOrMark(pk *Packet) bool {
	if q.p.ECN && pk.MarkCE() {
		q.sink.qdiscMarked(pk)
		return false
	}
	q.sink.qdiscDropped(pk, DropAQM)
	return true
}

func (q *CoDel) dropOrMarkEnter(pk **Packet, now time.Duration) {
	st := &q.st
	dropped := q.dropOrMark(*pk)
	if dropped {
		next, _ := q.codelDequeue(now)
		*pk = next
	}
	st.dropping = true
	delta := st.count - st.lastCount
	if delta > 1 && now-st.dropNext < 16*q.p.Interval {
		st.count = delta
	} else {
		st.count = 1
	}
	st.dropNext = controlLaw(now, q.p.Interval, st.count)
	st.lastCount = st.count
}

// codelDequeue pops the head packet and evaluates the target/interval test.
func (q *CoDel) codelDequeue(now time.Duration) (*Packet, bool) {
	pk := q.pop()
	if pk == nil {
		return nil, false
	}
	sojourn := now - pk.EnqueuedAt
	return pk, q.st.shouldDrop(sojourn, now, q.p, q.bytes+pk.Size())
}

// Len implements Qdisc.
func (q *CoDel) Len() int { return q.fifo.len() }

// Bytes implements Qdisc.
func (q *CoDel) Bytes() int { return q.fifo.bytes }

// SetLimit implements Qdisc.
func (q *CoDel) SetLimit(pkts, bytes int) {
	if pkts > 0 {
		q.limitPkts = pkts
	}
	if bytes > 0 {
		q.limitBytes = bytes
	}
}
