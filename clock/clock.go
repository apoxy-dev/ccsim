// Package clock provides a deterministic virtual clock implementing
// tcpip.Clock. All time in the simulation flows from a single instance of
// this clock: netstack timers, link-model events, application drivers and
// sampling ticks all share one min-heap of timers keyed on virtual time.
//
// Timers due at the same virtual instant fire in insertion order (a stable
// sequence number breaks ties), which is a determinism requirement for the
// simulator.
package clock

import (
	"container/heap"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// epoch is the virtual wall-clock origin. Its concrete value is arbitrary but
// must be fixed so runs are reproducible across hosts and build targets.
var epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Clock is a virtual clock driven explicitly via Advance/AdvanceTo.
// It implements tcpip.Clock.
//
// The zero value is not usable; call New.
type Clock struct {
	mu sync.Mutex
	// now is virtual nanoseconds since epoch.
	now int64
	// seq is a monotonically increasing sequence number assigned to timers
	// at (re)schedule time to make firing order stable.
	seq uint64
	// timers is a min-heap ordered by (when, seq).
	timers timerHeap
	// advancing guards against reentrant Advance calls from timer callbacks.
	advancing bool
}

// New returns a Clock positioned at the virtual epoch.
func New() *Clock {
	return &Clock{}
}

// Now implements tcpip.Clock.Now.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return epoch.Add(time.Duration(c.now))
}

// NowMonotonic implements tcpip.Clock.NowMonotonic.
func (c *Clock) NowMonotonic() tcpip.MonotonicTime {
	c.mu.Lock()
	defer c.mu.Unlock()
	var mt tcpip.MonotonicTime
	return mt.Add(time.Duration(c.now))
}

// Elapsed returns the virtual time elapsed since the epoch.
func (c *Clock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Duration(c.now)
}

// AfterFunc implements tcpip.Clock.AfterFunc. The callback runs on the
// goroutine that advances the clock.
func (c *Clock) AfterFunc(d time.Duration, f func()) tcpip.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{clock: c, f: f}
	c.scheduleLocked(t, d)
	return t
}

// scheduleLocked (re)schedules t to fire d from now.
func (c *Clock) scheduleLocked(t *timer, d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.seq++
	t.when = c.now + int64(d)
	t.seq = c.seq
	t.state = timerScheduled
	heap.Push(&c.timers, t)
}

// Next returns the virtual time of the earliest pending timer.
func (c *Clock) Next() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.timers) > 0 {
		t := c.timers[0]
		if t.state != timerScheduled {
			heap.Pop(&c.timers) // drop stopped/superseded entries lazily
			continue
		}
		return epoch.Add(time.Duration(t.when)), true
	}
	return time.Time{}, false
}

// Pending returns the number of scheduled (not stopped) timers. Tests use
// it to bound timer leakage at teardown.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.state == timerScheduled {
			n++
		}
	}
	return n
}

// Advance moves virtual time forward by d, firing all timers due in
// (now, now+d] in timestamp order (ties by schedule order). Callbacks run
// synchronously on the calling goroutine and may schedule further timers,
// which fire in the same Advance if they fall within the window.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.advanceToLocked(c.now + int64(d))
}

// AdvanceTo moves virtual time forward to absolute virtual time t.
// It is a no-op if t is not in the future.
func (c *Clock) AdvanceTo(t time.Time) {
	c.mu.Lock()
	c.advanceToLocked(int64(t.Sub(epoch)))
}

// RunUntilIdle fires timers (advancing time as needed) until no timers
// remain, and returns the number fired. Use with care: a self-rearming timer
// makes this loop forever.
func (c *Clock) RunUntilIdle() int {
	fired := 0
	for {
		next, ok := c.Next()
		if !ok {
			return fired
		}
		c.AdvanceTo(next)
		fired++
	}
}

// advanceToLocked implements the advance loop. It is entered with c.mu held
// and releases it before returning. Callbacks are invoked without the lock.
func (c *Clock) advanceToLocked(target int64) {
	if c.advancing {
		c.mu.Unlock()
		panic("clock: reentrant Advance from timer callback")
	}
	c.advancing = true
	for {
		var t *timer
		for len(c.timers) > 0 {
			top := c.timers[0]
			if top.state != timerScheduled {
				heap.Pop(&c.timers)
				continue
			}
			if top.when > target {
				break
			}
			t = heap.Pop(&c.timers).(*timer)
			break
		}
		if t == nil {
			break
		}
		if t.when > c.now {
			c.now = t.when
		}
		t.state = timerFired
		f := t.f
		c.mu.Unlock()
		f()
		c.mu.Lock()
	}
	if c.now < target {
		c.now = target
	}
	c.advancing = false
	c.mu.Unlock()
}

type timerState int

const (
	timerIdle timerState = iota
	timerScheduled
	timerFired
	timerStopped
)

// timer implements tcpip.Timer.
type timer struct {
	clock *Clock
	f     func()
	when  int64
	seq   uint64
	state timerState
	// index within the heap, maintained by timerHeap; -1 when not in heap.
	index int
}

// Stop implements tcpip.Timer.Stop.
func (t *timer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.state != timerScheduled {
		return false
	}
	t.state = timerStopped
	if t.index >= 0 {
		heap.Remove(&c.timers, t.index)
	}
	return true
}

// Reset implements tcpip.Timer.Reset.
func (t *timer) Reset(d time.Duration) {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.state == timerScheduled && t.index >= 0 {
		heap.Remove(&c.timers, t.index)
	}
	c.scheduleLocked(t, d)
}

// timerHeap is a min-heap of timers ordered by (when, seq).
type timerHeap []*timer

func (h timerHeap) Len() int { return len(h) }

func (h timerHeap) Less(i, j int) bool {
	if h[i].when != h[j].when {
		return h[i].when < h[j].when
	}
	return h[i].seq < h[j].seq
}

func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *timerHeap) Push(x any) {
	t := x.(*timer)
	t.index = len(*h)
	*h = append(*h, t)
}

func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	t.index = -1
	*h = old[:n-1]
	return t
}
