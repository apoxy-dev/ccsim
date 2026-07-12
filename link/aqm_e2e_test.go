package link_test

// End-to-end AQM correctness: real TCP flows through the full harness with
// CoDel / FQ-CoDel / ECN queue configurations. Every check logs measured vs
// predicted values even when passing.

import (
	"bytes"
	"testing"

	"ccsim/probe"
	"ccsim/scenario"
	"ccsim/sim"
	"ccsim/stream"
)

func run(t *testing.T, cfg *scenario.ScenarioConfig) ([]stream.Record, probe.RunSummary) {
	t.Helper()
	var buf bytes.Buffer
	w := stream.NewWriter(&buf, 0)
	s, err := sim.New(cfg, w)
	if err != nil {
		t.Fatal(err)
	}
	sum := s.Run(nil)
	s.Close()
	recs, err := stream.Decode(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return recs, sum
}

func bulk(cc string, at float64) scenario.FlowConfig {
	return scenario.FlowConfig{CC: cc, StartAt: at, App: scenario.AppConfig{Kind: "bulk"}}
}

// queueDelayMs converts the mean forward queue depth (bytes) over [from,to]
// into sojourn delay at the given link rate.
func queueDelayMs(recs []stream.Record, rateMbps, from, to float64) float64 {
	qBytes := probe.MeanOf(recs, stream.LinkFwd, stream.KindQDepthBytes, from, to)
	return qBytes * 8 / (rateMbps * 1e6) * 1000
}

// Test 16: a saturating cubic flow through CoDel must hold steady-state
// sojourn near the 5 ms target; the same flow through a deep tail-drop
// queue sits at least an order of magnitude higher.
func TestCoDelTargetTracking(t *testing.T) {
	base := scenario.ScenarioConfig{
		Seed: 21, Dur: 30,
		Link: scenario.LinkConfig{RateMbps: 50, OwdMs: 10,
			Queue: scenario.QueueConfig{Kind: "codel"}},
		Flows: []scenario.FlowConfig{bulk("cubic", 0)},
	}
	recsCoDel, sumCoDel := run(t, &base)
	codelDelay := queueDelayMs(recsCoDel, 50, 10, 30)

	deep := base
	deep.Seed = 22
	// ~200 ms of buffer: 833 pkts * 1500 B * 8 / 50 Mbps.
	deep.Link.Queue = scenario.QueueConfig{Kind: "taildrop", LimitPkts: 833}
	recsDeep, _ := run(t, &deep)
	deepDelay := queueDelayMs(recsDeep, 50, 10, 30)

	t.Logf("mean sojourn: codel=%.2f ms (target 5, interval 100), taildrop(200ms buf)=%.2f ms, ratio %.1fx; codel goodput %.1f Mbps",
		codelDelay, deepDelay, deepDelay/codelDelay, sumCoDel.Flows[0].GoodputMbps)
	// The mean can sit below the 5 ms target (cubic's sawtooth drains the
	// queue between epochs); the meaningful bounds are "a standing queue
	// exists but stays near target" plus high utilization, so low delay
	// cannot come from an idle link.
	if codelDelay < 1 || codelDelay > 15 {
		t.Errorf("codel steady-state sojourn %.2f ms outside [1, 15] (target 5ms)", codelDelay)
	}
	if util := sumCoDel.Flows[0].GoodputMbps / 50; util < 0.9 {
		t.Errorf("codel goodput only %.0f%% of link; sojourn number is not meaningful", util*100)
	}
	if deepDelay < 10*codelDelay {
		t.Errorf("tail-drop sojourn %.2f ms not >= 10x codel's %.2f ms", deepDelay, codelDelay)
	}
}

// Test 17: FQ-CoDel isolates a request/response flow from a bulk flow.
// The rr flow's FCT p95 through FQ-CoDel must be near the unloaded ideal,
// and at least 5x better than through a deep FIFO; DRR must not starve the
// bulk flow.
func TestFQCoDelIsolation(t *testing.T) {
	const (
		rateMbps  = 20.0
		owdMs     = 20.0
		respBytes = 16384
	)
	mk := func(kind string, limitPkts int, seed int64) *scenario.ScenarioConfig {
		return &scenario.ScenarioConfig{
			Seed: seed, Dur: 30,
			Link: scenario.LinkConfig{RateMbps: rateMbps, OwdMs: owdMs,
				Queue: scenario.QueueConfig{Kind: kind, LimitPkts: limitPkts}},
			Flows: []scenario.FlowConfig{
				bulk("cubic", 0),
				{CC: "cubic", StartAt: 0.5, App: scenario.AppConfig{
					Kind: "rr", ReqBytes: 100, RespBytes: respBytes, PoissonRate: 5}},
			},
		}
	}
	_, sumFQ := run(t, mk("fqcodel", 0, 23))
	_, sumFIFO := run(t, mk("taildrop", 1333, 23)) // 20xBDP deep FIFO

	fq, fifo := sumFQ.Flows[1], sumFIFO.Flows[1]
	if fq.FCTCount == 0 || fifo.FCTCount == 0 {
		t.Fatalf("missing FCTs: fq=%d fifo=%d", fq.FCTCount, fifo.FCTCount)
	}
	// Ideal FCT: RTT + response serialization (the response crosses the
	// uncongested reverse direction; requests cross the contested forward
	// queue).
	idealMs := 2*owdMs + float64(respBytes)*8/(rateMbps*1e6)*1000
	t.Logf("rr FCT p95: fqcodel=%.1f ms, fifo=%.1f ms (ideal %.1f ms, isolation %.1fx); bulk goodput fq=%.1f fifo=%.1f Mbps",
		fq.FCTP95Ms, fifo.FCTP95Ms, idealMs, fifo.FCTP95Ms/fq.FCTP95Ms,
		sumFQ.Flows[0].GoodputMbps, sumFIFO.Flows[0].GoodputMbps)
	if fq.FCTP95Ms > 3*idealMs {
		t.Errorf("fqcodel rr FCT p95 %.1f ms > 3x ideal %.1f ms", fq.FCTP95Ms, idealMs)
	}
	if fifo.FCTP95Ms < 5*fq.FCTP95Ms {
		t.Errorf("FIFO FCT p95 %.1f ms not >= 5x fqcodel's %.1f ms (isolation benefit missing)",
			fifo.FCTP95Ms, fq.FCTP95Ms)
	}
	// Isolation must not starve bulk: within 10% of what the same bulk
	// flow gets through FQ-CoDel with no rr flow present (cubic-under-
	// codel already costs a few percent of raw link; that is not DRR's
	// doing).
	solo := mk("fqcodel", 0, 23)
	solo.Flows = solo.Flows[:1]
	_, sumSolo := run(t, solo)
	bulkWith, bulkSolo := sumFQ.Flows[0].GoodputMbps, sumSolo.Flows[0].GoodputMbps
	t.Logf("bulk goodput with rr %.2f Mbps vs solo %.2f Mbps (%.1f%%)", bulkWith, bulkSolo, bulkWith/bulkSolo*100)
	if bulkWith < 0.9*bulkSolo {
		t.Errorf("bulk goodput %.1f Mbps < 90%% of its solo-through-fqcodel %.1f (DRR starving bulk)", bulkWith, bulkSolo)
	}
}

// Test 19: CoDel in ECN-marking mode must deliver the same goodput as
// drop mode within 10%, with (near) zero retransmits.
func TestECNDropEquivalence(t *testing.T) {
	mk := func(ecn bool) *scenario.ScenarioConfig {
		return &scenario.ScenarioConfig{
			Seed: 24, Dur: 30,
			Link: scenario.LinkConfig{RateMbps: 50, OwdMs: 10,
				Queue: scenario.QueueConfig{Kind: "codel", ECN: ecn}},
			Flows: []scenario.FlowConfig{bulk("cubic", 0)},
		}
	}
	_, sumDrop := run(t, mk(false))
	_, sumECN := run(t, mk(true))

	gDrop, gECN := sumDrop.Flows[0].GoodputMbps, sumECN.Flows[0].GoodputMbps
	t.Logf("goodput: drop=%.2f ecn=%.2f Mbps (ratio %.3f); retrans drop=%d ecn=%d; ce_marks=%d",
		gDrop, gECN, gECN/gDrop, sumDrop.Flows[0].Retransmits, sumECN.Flows[0].Retransmits, sumECN.CEMarks)
	if gECN < 0.9*gDrop || gECN > 1.1*gDrop {
		t.Errorf("ECN-mode goodput %.2f not within 10%% of drop-mode %.2f", gECN, gDrop)
	}
	if sumECN.CEMarks == 0 {
		t.Error("no CE marks in ECN mode")
	}
	if sumECN.Flows[0].Retransmits > 20 {
		t.Errorf("ECN mode retransmits = %d, want ~0 (marking should replace loss)", sumECN.Flows[0].Retransmits)
	}
	if sumDrop.Flows[0].Retransmits == 0 {
		t.Error("drop mode had zero retransmits: CoDel never dropped, comparison is vacuous")
	}
}
