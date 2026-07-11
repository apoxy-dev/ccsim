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

// Scenario 4: bufferbloat — cubic fills a 50xBDP buffer (bbr half added
// once bbr lands).
func TestScenarioBufferbloatCubic(t *testing.T) {
	recs, _ := runScenario(t, "bufferbloat", nil)
	const baseRTTms = 30
	srtt := probe.MeanOf(recs, 0, stream.KindSRTTSec, 10, 30) * 1000
	if srtt < 3*baseRTTms {
		t.Errorf("cubic mean srtt over [10,30]s = %.1f ms, want >= %d (bloat missing)", srtt, 3*baseRTTms)
	}
	goodput := probe.GoodputMbps(recs, 0, 5, 30)
	if goodput < 0.85*50 {
		t.Errorf("goodput %.1f Mbps, want >= 42.5", goodput)
	}
	t.Logf("srtt=%.1fms goodput=%.1f", srtt, goodput)
}
