package sim

import (
	"bytes"
	"runtime"
	"testing"

	"ccsim/scenario"
	"ccsim/stream"
)

// Sim.Close must make the whole sim collectible: each netstack instance
// pins ~10 goroutines, and through them ~29 MB of object graph per
// cubic-single run. This is the native half of the wasm memtest harness —
// a leak here becomes an ever-growing browser tab there.
func TestSimCloseReleasesResources(t *testing.T) {
	cfg, err := scenario.Preset("cubic-single")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Dur = 3

	cycle := func() {
		var buf bytes.Buffer
		s, err := New(cfg, stream.NewWriter(&buf, 0))
		if err != nil {
			t.Fatal(err)
		}
		s.Run(nil)
		s.Close()
	}
	cycle() // warmup: lazy runtime pools, timer plumbing
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	baseHeap := m.HeapAlloc

	for i := 0; i < 5; i++ {
		cycle()
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	grewMB := (float64(m.HeapAlloc) - float64(baseHeap)) / 1048576
	goroutines := runtime.NumGoroutine()
	t.Logf("after 5 closed cycles: heap %+.1f MB vs baseline, goroutines %d (baseline %d)",
		grewMB, goroutines, baseGoroutines)
	if goroutines > baseGoroutines+4 {
		t.Errorf("goroutines grew %d -> %d: stacks not torn down by Close", baseGoroutines, goroutines)
	}
	// Without Close this grows ~29 MB per cycle (~145 MB here); a small
	// allowance covers GC slack.
	if grewMB > 20 {
		t.Errorf("live heap grew %.1f MB over 5 closed cycles: sim retained after Close", grewMB)
	}
}
