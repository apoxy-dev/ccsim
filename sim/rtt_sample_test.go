package sim

import (
	"testing"

	"ccsim/scenario"
	"ccsim/stream"
)

func TestWireStatsRawRTTIncludesJitter(t *testing.T) {
	meanRTT := func(jitterMs float64) float64 {
		t.Helper()
		// Naive's fixed 150 Mbps pacer is comfortably below this link, keeping
		// the queue empty so the comparison isolates propagation jitter.
		cfg := vCfg(71, 2, 500, 20, 100, vBulk("naive", 0))
		cfg.Link.JitterMs = jitterMs
		cfg.Sample = scenario.SampleConfig{IntervalMs: 20, WireStats: true}
		recs, _ := runCfg(t, cfg)

		var sum float64
		var n int
		for _, r := range recs {
			if r.Flow == 0 && r.Kind == stream.KindRTTSampleSec && r.T >= 0.5 && r.Value > 0 {
				sum += r.Value
				n++
			}
		}
		if n == 0 {
			t.Fatalf("jitter %.1f ms: no raw RTT samples", jitterMs)
		}
		return sum / float64(n)
	}

	clean := meanRTT(0)
	jittered := meanRTT(20)
	t.Logf("raw RTT mean: clean %.1f ms, 20 ms/direction jitter %.1f ms",
		clean*1000, jittered*1000)
	if jittered < clean+5e-3 {
		t.Fatalf("jittered raw RTT %.1f ms, want at least 5 ms above clean %.1f ms",
			jittered*1000, clean*1000)
	}
}
