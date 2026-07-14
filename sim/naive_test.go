package sim

import (
	"testing"

	"ccsim/scenario"
	"ccsim/stream"
)

func TestNaiveFixedRateOverloadsBottleneck(t *testing.T) {
	cfg := vCfg(70, 5, 100, 20, 350, vBulk("naive", 0))
	cfg.Sample = scenario.SampleConfig{IntervalMs: 20, WireStats: true}
	recs, sum := runCfg(t, cfg)

	cumulativeRate := func(kind stream.Kind, after float64) float64 {
		t.Helper()
		var first, last *stream.Record
		for i := range recs {
			r := &recs[i]
			if r.Flow != stream.LinkFwd || r.Kind != kind || r.T < after {
				continue
			}
			if first == nil {
				first = r
			}
			last = r
		}
		if first == nil || last == first || last.T <= first.T {
			t.Fatalf("not enough kind %d records after %.1fs", kind, after)
		}
		return (last.Value - first.Value) * 8 / (last.T - first.T) / 1e6
	}

	arrival := cumulativeRate(stream.KindLinkArrivalBytesCum, 1)
	dequeue := cumulativeRate(stream.KindLinkDequeueBytesCum, 1)
	t.Logf("naive offered %.1f Mbps into 100 Mbps link; dequeued %.1f Mbps; drops=%d qmax=%d",
		arrival, dequeue, sum.Drops, sum.QDepthMaxPkt)
	// The pacer is 150 Mbps. TCP recovery and packet/header accounting make
	// qdisc arrival throughput slightly lower, but it must remain a sustained
	// overload rather than ACK-clock back down to the bottleneck rate.
	if arrival < 135 || arrival > 155 {
		t.Errorf("naive arrival rate %.1f Mbps, want near the 150 Mbps pacer", arrival)
	}
	if dequeue < 90 || dequeue > 101 {
		t.Errorf("bottleneck dequeue rate %.1f Mbps, want near 100", dequeue)
	}
	if arrival-dequeue < 35 {
		t.Errorf("overload gap only %.1f Mbps, want at least 35", arrival-dequeue)
	}
	if sum.Drops == 0 {
		t.Error("fixed-rate overload produced no packet drops")
	}
	if sum.QDepthMaxPkt != 350 {
		t.Errorf("maximum queue depth %d, want configured limit 350", sum.QDepthMaxPkt)
	}
}
