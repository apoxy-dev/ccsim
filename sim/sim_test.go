package sim

import (
	"bytes"
	"testing"

	"ccsim/probe"
	"ccsim/scenario"
	"ccsim/stream"
)

// runScenario runs a preset (optionally mutated) and returns the decoded
// sample stream and summary.
func runScenario(t *testing.T, name string, mut func(*scenario.ScenarioConfig)) ([]stream.Record, probe.RunSummary) {
	t.Helper()
	recs, sum, err := runScenarioE(name, mut)
	if err != nil {
		t.Fatal(err)
	}
	return recs, sum
}

func runScenarioE(name string, mut func(*scenario.ScenarioConfig)) ([]stream.Record, probe.RunSummary, error) {
	cfg, err := scenario.Preset(name)
	if err != nil {
		return nil, probe.RunSummary{}, err
	}
	if mut != nil {
		mut(cfg)
	}
	var buf bytes.Buffer
	w := stream.NewWriter(&buf, 0)
	s, err := New(cfg, w)
	if err != nil {
		return nil, probe.RunSummary{}, err
	}
	sum := s.Run(nil)
	s.Close()
	recs, err := stream.Decode(buf.Bytes())
	if err != nil {
		return nil, probe.RunSummary{}, err
	}
	return recs, sum, nil
}

func TestBasicDataFlows(t *testing.T) {
	_, sum := runScenario(t, "cubic-single", func(c *scenario.ScenarioConfig) { c.Dur = 2 })
	if len(sum.Flows) != 1 {
		t.Fatalf("flows: %d", len(sum.Flows))
	}
	// 100 Mbps link, 2 s run: expect at least 10 Mbps average even with the
	// slow-start ramp.
	if sum.Flows[0].GoodputMbps < 10 {
		t.Fatalf("goodput %.2f Mbps, expected > 10", sum.Flows[0].GoodputMbps)
	}
	t.Logf("goodput=%.1f Mbps srtt=%.1f/%.1f/%.1fms retrans=%d cuts=%d",
		sum.Flows[0].GoodputMbps, sum.Flows[0].SRTTMeanMs, sum.Flows[0].SRTTP95Ms,
		sum.Flows[0].SRTTMaxMs, sum.Flows[0].Retransmits, sum.Flows[0].CwndCuts)
}
