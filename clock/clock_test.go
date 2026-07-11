package clock

import (
	"testing"
	"time"
)

func TestAdvanceFiresInOrder(t *testing.T) {
	c := New()
	var got []int
	c.AfterFunc(20*time.Millisecond, func() { got = append(got, 2) })
	c.AfterFunc(10*time.Millisecond, func() { got = append(got, 1) })
	c.AfterFunc(30*time.Millisecond, func() { got = append(got, 3) })
	c.Advance(25 * time.Millisecond)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v, want [1 2]", got)
	}
	c.Advance(10 * time.Millisecond)
	if len(got) != 3 || got[2] != 3 {
		t.Fatalf("got %v, want [1 2 3]", got)
	}
}

func TestTieBreakByInsertion(t *testing.T) {
	c := New()
	var got []int
	for i := 0; i < 10; i++ {
		i := i
		c.AfterFunc(5*time.Millisecond, func() { got = append(got, i) })
	}
	c.Advance(5 * time.Millisecond)
	for i, v := range got {
		if v != i {
			t.Fatalf("tie-break order violated: got %v", got)
		}
	}
}

func TestNestedScheduling(t *testing.T) {
	c := New()
	var got []string
	c.AfterFunc(10*time.Millisecond, func() {
		got = append(got, "a")
		// Fires within the same Advance window.
		c.AfterFunc(5*time.Millisecond, func() { got = append(got, "b") })
	})
	c.Advance(20 * time.Millisecond)
	if len(got) != 2 || got[1] != "b" {
		t.Fatalf("got %v, want [a b]", got)
	}
	if e := c.Elapsed(); e != 20*time.Millisecond {
		t.Fatalf("elapsed %v, want 20ms", e)
	}
}

func TestStopAndReset(t *testing.T) {
	c := New()
	fired := 0
	tm := c.AfterFunc(10*time.Millisecond, func() { fired++ })
	if !tm.Stop() {
		t.Fatal("Stop returned false on scheduled timer")
	}
	if tm.Stop() {
		t.Fatal("Stop returned true on stopped timer")
	}
	c.Advance(20 * time.Millisecond)
	if fired != 0 {
		t.Fatal("stopped timer fired")
	}
	tm.Reset(10 * time.Millisecond)
	c.Advance(10 * time.Millisecond)
	if fired != 1 {
		t.Fatalf("fired=%d after Reset+Advance, want 1", fired)
	}
	// Reset a fired timer refires it.
	tm.Reset(5 * time.Millisecond)
	c.Advance(5 * time.Millisecond)
	if fired != 2 {
		t.Fatalf("fired=%d, want 2", fired)
	}
}

func TestNextAndRunUntilIdle(t *testing.T) {
	c := New()
	if _, ok := c.Next(); ok {
		t.Fatal("Next reported a timer on empty clock")
	}
	fired := 0
	var rearm func()
	rearm = func() {
		fired++
		if fired < 3 {
			c.AfterFunc(time.Millisecond, rearm)
		}
	}
	c.AfterFunc(3*time.Millisecond, rearm)
	next, ok := c.Next()
	if !ok || next.Sub(epoch) != 3*time.Millisecond {
		t.Fatalf("Next=%v ok=%v", next, ok)
	}
	c.RunUntilIdle()
	if fired != 3 {
		t.Fatalf("fired=%d, want 3", fired)
	}
}

func TestMonotonicMatchesNow(t *testing.T) {
	c := New()
	m0 := c.NowMonotonic()
	c.Advance(1500 * time.Millisecond)
	if d := c.NowMonotonic().Sub(m0); d != 1500*time.Millisecond {
		t.Fatalf("monotonic delta %v, want 1.5s", d)
	}
}
