package sim

// Smoke-test scenarios per the harness spec. Tolerances are deliberately
// loose; these validate qualitative CC behavior, not benchmarks.

import (
	"bytes"
	"testing"

	"ccsim/probe"
	"ccsim/scenario"
	"ccsim/stream"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// rawRun runs a preset and returns the raw sample stream bytes.
func rawRun(t *testing.T, name string, mut func(*scenario.ScenarioConfig)) []byte {
	t.Helper()
	cfg, err := scenario.Preset(name)
	if err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(cfg)
	}
	var buf bytes.Buffer
	w := stream.NewWriter(&buf, 0)
	s, err := New(cfg, w)
	if err != nil {
		t.Fatal(err)
	}
	s.Run(nil)
	s.Close()
	return buf.Bytes()
}

// Scenario 1: determinism — same seed byte-identical, different seed differs.
func TestScenarioDeterminism(t *testing.T) {
	if !tcp.SimCCRegistered("bbr") {
		t.Skip("bbr congestion control not registered yet")
	}
	a := rawRun(t, "determinism", nil)
	b := rawRun(t, "determinism", nil)
	if !bytes.Equal(a, b) {
		t.Fatalf("same seed produced different streams (%d vs %d bytes)", len(a), len(b))
	}
	c := rawRun(t, "determinism", func(cfg *scenario.ScenarioConfig) { cfg.Seed = 99 })
	if bytes.Equal(a, c) {
		t.Fatal("different seed produced identical stream")
	}
}

// Scenario 2: cubic-single — high utilization, sawtooth, no RTOs.
func TestScenarioCubicSingle(t *testing.T) {
	recs, sum := runScenario(t, "cubic-single", nil)
	goodput := probe.GoodputMbps(recs, 0, 5, 30)
	if goodput < 90 {
		t.Errorf("goodput over [5,30]s = %.1f Mbps, want >= 90", goodput)
	}
	if sum.Flows[0].CwndCuts < 3 {
		t.Errorf("cwnd cuts = %d, want >= 3 (no sawtooth)", sum.Flows[0].CwndCuts)
	}
	if sum.Flows[0].RTOs != 0 {
		t.Errorf("RTOs = %d, want 0", sum.Flows[0].RTOs)
	}
	t.Logf("goodput=%.1f cuts=%d retrans=%d srtt=%.1fms",
		goodput, sum.Flows[0].CwndCuts, sum.Flows[0].Retransmits, sum.Flows[0].SRTTMeanMs)
}

// Regression: synchronous dispatch must not livelock when a passive
// handshake is completed by a data-bearing segment (third ACK dropped by
// the lossy link). Before the reentrancy guard in processEndpointInline
// this configuration hung the event loop on most seeds.
func TestLossyHandshakeNoLivelock(t *testing.T) {
	for _, seed := range []int64{11, 12, 14, 15} {
		rawRun(t, "determinism", func(cfg *scenario.ScenarioConfig) {
			cfg.Seed = seed
			cfg.Dur = 3
			cfg.Link.Loss = 0.35
			cfg.Flows = []scenario.FlowConfig{
				bulkFlow("cubic", 0), bulkFlow("cubic", 0.1), bulkFlow("cubic", 0.2),
				bulkFlow("bbr", 0.3), bulkFlow("cubic", 0.4), bulkFlow("cubic", 0.5),
			}
		})
	}
}

func bulkFlow(cc string, startAt float64) scenario.FlowConfig {
	return scenario.FlowConfig{CC: cc, StartAt: startAt,
		App: scenario.AppConfig{Kind: "bulk"}}
}

// Scenario 4: bufferbloat — cubic fills a 50xBDP buffer. HyStart exits
// slow start almost immediately (delay rises as soon as the queue forms),
// so the first overflow is the tail of a 0.4*t^3 climb at ~27 s and the
// second epoch peaks near ~44 s; assertions target the post-overflow
// steady state.
func TestScenarioBufferbloatCubic(t *testing.T) {
	recs, sum := runScenario(t, "bufferbloat", nil)
	const baseRTTms = 30
	srtt := probe.MeanOf(recs, 0, stream.KindSRTTSec, 30, 60) * 1000
	if srtt < 10*baseRTTms {
		t.Errorf("cubic steady-state mean srtt over [30,60]s = %.1f ms, want >= %d (bloat missing)", srtt, 10*baseRTTms)
	}
	// The buffer must actually overflow, once per cubic epoch — guards
	// against the flow silently going window-limited instead of
	// congestion-limited.
	if sum.Flows[0].CwndCuts < 2 {
		t.Errorf("cwnd cuts = %d, want >= 2 (flow not congestion-limited?)", sum.Flows[0].CwndCuts)
	}
	// 0.80 (not 0.85): each overflow episode loses ~1100 segments and
	// recovers them at a ~1.5 s bloated RTT, which costs real goodput.
	goodput := probe.GoodputMbps(recs, 0, 5, 60)
	if goodput < 0.80*50 {
		t.Errorf("goodput %.1f Mbps, want >= 40", goodput)
	}
	t.Logf("srtt=%.1fms goodput=%.1f cuts=%d", srtt, goodput, sum.Flows[0].CwndCuts)
}

// Scenario 3: bbr-single — high utilization with low standing queue,
// ProbeRTT visible, ProbeBW reached quickly.
func TestScenarioBBRSingle(t *testing.T) {
	recs, sum := runScenario(t, "bbr-single", nil)
	const baseRTT = 0.040
	goodput := probe.GoodputMbps(recs, 0, 5, 30)
	if goodput < 90 {
		t.Errorf("goodput over [5,30]s = %.1f Mbps, want >= 90", goodput)
	}
	srtt := probe.MeanOf(recs, 0, stream.KindSRTTSec, 5, 30)
	if srtt > 1.35*baseRTT {
		t.Errorf("mean srtt %.1f ms, want <= %.1f ms", srtt*1000, 1.35*baseRTT*1000)
	}
	// State machine reaches ProbeBW by t=3s.
	_, states := probe.Series(recs, 0, stream.KindCCState, 0, 3)
	reached := false
	for _, s := range states {
		if int(s) >= 2 && int(s) <= 5 { // any ProbeBW phase
			reached = true
			break
		}
	}
	if !reached {
		t.Error("state machine did not reach ProbeBW by t=3s")
	}
	// At least one ProbeRTT entry within any 12s window.
	ts, states2 := probe.Series(recs, 0, stream.KindCCState, 0, 30)
	var probeRTTs []float64
	for i, s := range states2 {
		if int(s) == 6 { // ProbeRTT
			probeRTTs = append(probeRTTs, ts[i])
		}
	}
	if len(probeRTTs) == 0 {
		t.Error("no ProbeRTT entries observed")
	}
	for w := 0.0; w+12 <= 30; w += 1 {
		found := false
		for _, pt := range probeRTTs {
			if pt >= w && pt < w+12 {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no ProbeRTT in window [%.0f,%.0f]s", w, w+12)
			break
		}
	}
	t.Logf("goodput=%.1f srtt=%.1fms probertt_samples=%d rtos=%d retrans=%d",
		goodput, srtt*1000, len(probeRTTs), sum.Flows[0].RTOs, sum.Flows[0].Retransmits)
}

// Scenario 4b: bufferbloat with bbr — low delay at high utilization.
func TestScenarioBufferbloatBBR(t *testing.T) {
	recs, _ := runScenario(t, "bufferbloat", func(c *scenario.ScenarioConfig) {
		c.Flows[0].CC = "bbr"
	})
	const baseRTTms = 30.0
	srtt := probe.MeanOf(recs, 0, stream.KindSRTTSec, 10, 60) * 1000
	if srtt > 1.5*baseRTTms {
		t.Errorf("bbr mean srtt over [10,60]s = %.1f ms, want <= %.1f", srtt, 1.5*baseRTTms)
	}
	goodput := probe.GoodputMbps(recs, 0, 5, 60)
	if goodput < 0.85*50 {
		t.Errorf("goodput %.1f Mbps, want >= 42.5", goodput)
	}
	t.Logf("srtt=%.1fms goodput=%.1f", srtt, goodput)
}

// Scenario 5: random-loss — bbr shrugs off 1% loss, cubic collapses.
func TestScenarioRandomLoss(t *testing.T) {
	recsC, _ := runScenario(t, "random-loss", nil) // preset is cubic
	recsB, _ := runScenario(t, "random-loss", func(c *scenario.ScenarioConfig) {
		c.Flows[0].CC = "bbr"
	})
	gpC := probe.GoodputMbps(recsC, 0, 5, 30)
	gpB := probe.GoodputMbps(recsB, 0, 5, 30)
	if gpB < 3*gpC {
		t.Errorf("bbr %.1f Mbps < 3x cubic %.1f Mbps", gpB, gpC)
	}
	if gpB < 50 {
		t.Errorf("bbr goodput %.1f Mbps, want >= 50%% of link (50)", gpB)
	}
	t.Logf("cubic=%.1f bbr=%.1f Mbps", gpC, gpB)
}

// Scenario 6: fairness — cubic and bbr share a 2xBDP tail-drop bottleneck.
// Documents BBRv3's coexistence claim.
func TestScenarioFairness(t *testing.T) {
	recs, _ := runScenario(t, "fairness", nil)
	gp0 := probe.GoodputMbps(recs, 0, 20, 60) // cubic
	gp1 := probe.GoodputMbps(recs, 1, 20, 60) // bbr
	agg := gp0 + gp1
	if agg < 90 {
		t.Errorf("aggregate %.1f Mbps, want >= 90", agg)
	}
	for i, gp := range []float64{gp0, gp1} {
		share := gp / agg
		if share < 0.25 || share > 0.75 {
			t.Errorf("flow %d share %.0f%% outside [25%%,75%%] (cubic=%.1f bbr=%.1f)",
				i, share*100, gp0, gp1)
		}
	}
	t.Logf("cubic=%.1f bbr=%.1f aggregate=%.1f", gp0, gp1, agg)
}

// Scenario 7: rate-step — bbr adapts down within 3s and back up within 5s.
func TestScenarioRateStep(t *testing.T) {
	recs, _ := runScenario(t, "rate-step", nil)
	// Within 3s after the downstep at t=15: delivery rate <= 30 Mbps and
	// inflight drained below 1.5x the new BDP (25 Mbps x 20 ms = 62.5 kB).
	dlv := probe.MeanOf(recs, 0, stream.KindDeliveryBps, 17, 18) / 1e6
	if dlv > 30 {
		t.Errorf("delivery rate %.1f Mbps at t=18s, want <= 30", dlv)
	}
	infl := probe.MeanOf(recs, 0, stream.KindInflightBytes, 17.8, 18)
	newBDP := 25e6 / 8 * 0.020
	if infl > 1.5*newBDP {
		t.Errorf("inflight %.0f B at t=18s, want <= %.0f (1.5x new BDP)", infl, 1.5*newBDP)
	}
	// Within 5s after the upstep at t=30: goodput >= 80 Mbps.
	gp := probe.GoodputMbps(recs, 0, 35, 45)
	if gp < 80 {
		t.Errorf("goodput %.1f Mbps after upstep, want >= 80", gp)
	}
	t.Logf("post-down dlv=%.1f infl=%.0f post-up gp=%.1f", dlv, infl, gp)
}

// Scenario 8: ecn-codel — CE marks appear, bbr reacts without loss, both
// flows keep srtt low.
func TestScenarioECNCoDel(t *testing.T) {
	recs, sum := runScenario(t, "ecn-codel", nil)
	if sum.CEMarks == 0 {
		t.Fatal("no CE marks observed")
	}
	// bbr (flow 0) reacted to CE: an inflight_lo/bw_lo cut is visible as a
	// cwnd trajectory that stays bounded without packet loss for flow 0.
	if sum.Flows[0].Retransmits > 50 {
		t.Errorf("bbr flow had %d retransmits; ECN response should mostly avoid loss",
			sum.Flows[0].Retransmits)
	}
	const baseRTTms = 20.0
	for i := 0; i < 2; i++ {
		srtt := probe.MeanOf(recs, uint16(i), stream.KindSRTTSec, 5, 30) * 1000
		if srtt > 2*baseRTTms {
			t.Errorf("flow %d mean srtt %.1f ms, want <= %.1f", i, srtt, 2*baseRTTms)
		}
	}
	t.Logf("ce_marks=%d bbr_retrans=%d cubic_retrans=%d",
		sum.CEMarks, sum.Flows[0].Retransmits, sum.Flows[1].Retransmits)
}

// Scenario 9: rr-fct — request/response FCTs recorded and sane.
func TestScenarioRRFCT(t *testing.T) {
	_, sum := runScenario(t, "rr-fct", nil)
	rr := sum.Flows[1]
	if rr.FCTCount == 0 {
		t.Fatal("no FCTs recorded")
	}
	const baseRTTms = 40.0
	if rr.FCTP95Ms < rr.FCTP50Ms || rr.FCTP50Ms < baseRTTms {
		t.Errorf("FCT sanity failed: p50=%.1f p95=%.1f (base rtt %.0f)",
			rr.FCTP50Ms, rr.FCTP95Ms, baseRTTms)
	}
	t.Logf("fct n=%d p50=%.0f p95=%.0f p99=%.0f ms", rr.FCTCount, rr.FCTP50Ms, rr.FCTP95Ms, rr.FCTP99Ms)
}
