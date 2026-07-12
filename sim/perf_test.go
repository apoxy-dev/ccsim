package sim

// Test 29 (throughput half): simulated-seconds per wall-second budgets.
// Wall-clock assertions are only meaningful on a quiet, known machine, so
// they are gated behind CCSIM_PERF=1 (set by `make validate`); CI shared
// runners would flake. The measured ratios are always logged.
//
// Budgets were calibrated on an Apple M-series laptop (2026-07):
// cubic-single ~21x real time, the 8-flow mix ~5x; asserted at ~0.7x of
// measured per the lock-at-0.7x policy.

import (
	"bytes"
	"os"
	"testing"
	"time"

	"ccsim/scenario"
	"ccsim/stream"
)

func measureSimRate(t *testing.T, cfg *scenario.ScenarioConfig) float64 {
	t.Helper()
	var buf bytes.Buffer
	s, err := New(cfg, stream.NewWriter(&buf, 0))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Run(nil)
	wall := time.Since(start).Seconds()
	s.Close()
	return cfg.Dur / wall
}

func TestPerfBudget(t *testing.T) {
	strict := os.Getenv("CCSIM_PERF") != ""
	if testing.Short() {
		t.Skip("short mode")
	}

	single, err := scenario.Preset("cubic-single")
	if err != nil {
		t.Fatal(err)
	}
	rate1 := measureSimRate(t, single)

	multi := vCfg(61, 30, 100, 15, 2*bdpPkts(100, 30),
		vBulk("cubic", 0), vBulk("cubic", 0.5), vBulk("cubic", 1), vBulk("cubic", 1.5),
		vBulk("bbr", 2), vBulk("bbr", 2.5), vBulk("bbr", 3), vBulk("bbr", 3.5))
	rate8 := measureSimRate(t, multi)

	t.Logf("sim-seconds/wall-second: cubic-single %.1fx (budget >= 15x), 8-flow mix %.1fx (budget >= 3x); strict=%v",
		rate1, rate8, strict)
	if !strict {
		t.Log("CCSIM_PERF not set: budgets logged, not asserted")
		return
	}
	if rate1 < 15 {
		t.Errorf("cubic-single runs at %.1fx real time, budget >= 15x", rate1)
	}
	if rate8 < 3 {
		t.Errorf("8-flow mix runs at %.1fx real time, budget >= 3x", rate8)
	}
}
