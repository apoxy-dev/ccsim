package link

import (
	"testing"
	"time"
)

// Test 29 (alloc half): the qdisc enqueue/dequeue hot path must not
// allocate per packet. The fifo's slide-forward backing array reallocates
// amortized-rarely; the assertion bounds the average well below one
// allocation per packet so a per-packet regression (a closure, an event
// struct escape) is unmissable.
func TestQdiscAllocsPerPacket(t *testing.T) {
	sink := &captureSink{now: func() (d time.Duration) { return }}
	q := NewTailDrop(1024, 0, sink)
	p := pkt1500()
	allocs := testing.AllocsPerRun(100000, func() {
		q.Enqueue(p, 0)
		q.Dequeue(0)
	})
	t.Logf("taildrop enqueue+dequeue: %.4f allocs/packet", allocs)
	if allocs > 0.05 {
		t.Errorf("qdisc hot path allocates %.4f/packet, want amortized ~0", allocs)
	}

	qc := NewCoDel(1024, 0, DefaultCoDelParams(), sink)
	allocsC := testing.AllocsPerRun(100000, func() {
		qc.Enqueue(p, 0)
		qc.Dequeue(0)
	})
	t.Logf("codel enqueue+dequeue (uncongested): %.4f allocs/packet", allocsC)
	if allocsC > 0.05 {
		t.Errorf("codel hot path allocates %.4f/packet, want amortized ~0", allocsC)
	}
}
