package link

// Regression for the jitter model choice: correlated jitter must preserve
// the spacing of a densely paced stream. The rejected iid-per-packet model
// compressed 250us-spaced packets into same-instant delivery batches of ~11
// at 5 ms jitter (see Config.Jitter), which no real path does.

import (
	"testing"
	"time"

	vclock "ccsim/clock"
)

func TestJitterNoBatching(t *testing.T) {
	for _, j := range []time.Duration{5 * time.Millisecond, 100 * time.Millisecond} {
		t.Run(j.String(), func(t *testing.T) { testJitterNoBatching(t, j) })
	}
}

func testJitterNoBatching(t *testing.T, jitter time.Duration) {
	clk := vclock.New()
	var times []time.Duration
	l := newTestLink(t, clk, Config{
		RateBps: 100_000_000,
		Delay:   20 * time.Millisecond,
		Jitter:  jitter,
	}, Hooks{OnDeliver: func(e Event) { times = append(times, e.T) }})
	// 2000 packets paced at 250us (ACK-like spacing), spanning 0.5 s — five
	// jitter walk segments.
	for i := 0; i < 2000; i++ {
		at := time.Duration(i) * 250 * time.Microsecond
		pk := mkPkt(60, 100, 200, false)
		clk.AfterFunc(at, func() { l.pipes[Rev].send(pk) })
	}
	clk.RunUntilIdle()
	if len(times) != 2000 {
		t.Fatalf("delivered %d packets, want 2000", len(times))
	}
	maxBatch, batch := 1, 1
	minOff, maxOff := time.Duration(1<<62), time.Duration(0)
	for i := 0; i < len(times); i++ {
		if i > 0 {
			if times[i] == times[i-1] {
				batch++
				if batch > maxBatch {
					maxBatch = batch
				}
			} else {
				batch = 1
			}
		}
		// Offset relative to the jitter-free delivery time of packet i.
		off := times[i] - time.Duration(i)*250*time.Microsecond
		if off < minOff {
			minOff = off
		}
		if off > maxOff {
			maxOff = off
		}
	}
	if maxBatch > 2 {
		t.Fatalf("max same-instant delivery batch %d, want <= 2 (jitter is compressing spacing)", maxBatch)
	}
	// The walk must actually move the delay around, not sit at one offset.
	if maxOff-minOff < 500*time.Microsecond {
		t.Fatalf("delay offset spread %v, want >= 500us; jitter walk seems inert", maxOff-minOff)
	}
}
