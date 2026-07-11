package link

import "time"

// FQCoDelParams configures FQ-CoDel (RFC 8290).
type FQCoDelParams struct {
	CoDel      CoDelParams
	Flows      int // number of flow buckets (default 1024)
	QuantumB   int // DRR quantum in bytes (default 1514)
	LimitPkts  int // total packet limit (default 10240)
	LimitBytes int // optional total byte limit
}

// DefaultFQCoDelParams returns RFC 8290 defaults.
func DefaultFQCoDelParams() FQCoDelParams {
	return FQCoDelParams{CoDel: DefaultCoDelParams(), Flows: 1024, QuantumB: 1514, LimitPkts: 10240}
}

type fqFlow struct {
	fifo
	st      codelState
	deficit int
	active  bool // on newFlows or oldFlows list
}

// FQCoDel implements RFC 8290: 5-tuple hashed flow queues, DRR scheduling
// between them, per-flow CoDel.
type FQCoDel struct {
	p        FQCoDelParams
	sink     qdiscSink
	flows    []fqFlow
	newFlows []int // indices into flows
	oldFlows []int
	pkts     int
	bytes    int
}

// NewFQCoDel creates an FQ-CoDel queue.
func NewFQCoDel(p FQCoDelParams, sink qdiscSink) *FQCoDel {
	def := DefaultFQCoDelParams()
	if p.Flows <= 0 {
		p.Flows = def.Flows
	}
	if p.QuantumB <= 0 {
		p.QuantumB = def.QuantumB
	}
	if p.LimitPkts <= 0 {
		p.LimitPkts = def.LimitPkts
	}
	if p.CoDel.Target == 0 {
		p.CoDel.Target = def.CoDel.Target
	}
	if p.CoDel.Interval == 0 {
		p.CoDel.Interval = def.CoDel.Interval
	}
	return &FQCoDel{p: p, sink: sink, flows: make([]fqFlow, p.Flows)}
}

// Enqueue implements Qdisc.
func (q *FQCoDel) Enqueue(pk *Packet, now time.Duration) bool {
	if (q.p.LimitPkts > 0 && q.pkts+1 > q.p.LimitPkts) ||
		(q.p.LimitBytes > 0 && q.bytes+pk.Size() > q.p.LimitBytes) {
		// RFC 8290 drops from the fattest flow; approximate with head drop
		// from the fattest, then accept the new packet.
		fat := q.fattest()
		if fat < 0 {
			q.sink.qdiscDropped(pk, DropTail)
			return true
		}
		dropped := q.flows[fat].pop()
		q.pkts--
		q.bytes -= dropped.Size()
		q.sink.qdiscDropped(dropped, DropTail)
	}
	idx := int(pk.hash5() % uint32(len(q.flows)))
	f := &q.flows[idx]
	pk.EnqueuedAt = now
	f.push(pk)
	q.pkts++
	q.bytes += pk.Size()
	if !f.active {
		f.active = true
		f.deficit = q.p.QuantumB
		q.newFlows = append(q.newFlows, idx)
	}
	return false
}

func (q *FQCoDel) fattest() int {
	best, bestBytes := -1, 0
	for i := range q.flows {
		if q.flows[i].bytes > bestBytes {
			best, bestBytes = i, q.flows[i].bytes
		}
	}
	return best
}

// Dequeue implements Qdisc (RFC 8290 scheduler).
func (q *FQCoDel) Dequeue(now time.Duration) *Packet {
	for {
		var list *[]int
		if len(q.newFlows) > 0 {
			list = &q.newFlows
		} else if len(q.oldFlows) > 0 {
			list = &q.oldFlows
		} else {
			return nil
		}
		idx := (*list)[0]
		f := &q.flows[idx]
		if f.deficit <= 0 {
			f.deficit += q.p.QuantumB
			// Move to end of old flows.
			*list = (*list)[1:]
			q.oldFlows = append(q.oldFlows, idx)
			continue
		}
		pk := q.codelDequeueFlow(f, now)
		if pk == nil {
			// Empty queue: a new flow moves to old list; an old flow is
			// removed (per RFC, prevents starvation of old flows).
			*list = (*list)[1:]
			if list == &q.newFlows {
				q.oldFlows = append(q.oldFlows, idx)
			} else {
				f.active = false
			}
			continue
		}
		f.deficit -= pk.Size()
		return pk
	}
}

// codelDequeueFlow runs the CoDel control law on one flow queue.
func (q *FQCoDel) codelDequeueFlow(f *fqFlow, now time.Duration) *Packet {
	pop := func() (*Packet, bool) {
		pk := f.pop()
		if pk == nil {
			return nil, false
		}
		q.pkts--
		q.bytes -= pk.Size()
		sojourn := now - pk.EnqueuedAt
		return pk, f.st.shouldDrop(sojourn, now, q.p.CoDel, f.bytes+pk.Size())
	}
	dropOrMark := func(pk *Packet) bool {
		if q.p.CoDel.ECN && pk.MarkCE() {
			q.sink.qdiscMarked(pk)
			return false
		}
		q.sink.qdiscDropped(pk, DropAQM)
		return true
	}
	pk, ok := pop()
	if pk == nil {
		f.st.dropping = false
		return nil
	}
	st := &f.st
	if st.dropping {
		if !ok {
			st.dropping = false
		} else {
			for now >= st.dropNext && st.dropping {
				st.count++
				if dropOrMark(pk) {
					pk, ok = pop()
					if !ok {
						st.dropping = false
					}
					if pk == nil {
						st.dropping = false
						return nil
					}
				}
				st.dropNext = controlLaw(st.dropNext, q.p.CoDel.Interval, st.count)
			}
		}
	} else if ok {
		if dropOrMark(pk) {
			pk, _ = pop()
			if pk == nil {
				return nil
			}
		}
		st.dropping = true
		delta := st.count - st.lastCount
		if delta > 1 && now-st.dropNext < 16*q.p.CoDel.Interval {
			st.count = delta
		} else {
			st.count = 1
		}
		st.dropNext = controlLaw(now, q.p.CoDel.Interval, st.count)
		st.lastCount = st.count
	}
	return pk
}

// Len implements Qdisc.
func (q *FQCoDel) Len() int { return q.pkts }

// Bytes implements Qdisc.
func (q *FQCoDel) Bytes() int { return q.bytes }

// SetLimit implements Qdisc.
func (q *FQCoDel) SetLimit(pkts, bytes int) {
	if pkts > 0 {
		q.p.LimitPkts = pkts
	}
	if bytes > 0 {
		q.p.LimitBytes = bytes
	}
}
