package sim

// Shared helpers for the validation suite (validation_test.go and the
// slow-tagged validation_slow_test.go): scenario builders, statistics, and
// the docs/validation.md results writer.

import (
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"ccsim/probe"
	"ccsim/scenario"
	"ccsim/stream"
)

// -update regenerates the measured-results sections of docs/validation.md
// from the tests that own them (tests are the data generators for the doc).
var updateDocs = flag.Bool("update", false, "rewrite measured tables in docs/validation.md")

// vBulk returns a bulk flow config (name distinct from scenarios_test.go's
// bulkFlow to keep the smoke suite untouched).
func vBulk(cc string, at float64) scenario.FlowConfig {
	return scenario.FlowConfig{CC: cc, StartAt: at, App: scenario.AppConfig{Kind: "bulk"}}
}

// vCfg builds a taildrop scenario: rate, one-way delay, buffer in packets.
func vCfg(seed int64, dur, rateMbps, owdMs float64, limitPkts int, flows ...scenario.FlowConfig) *scenario.ScenarioConfig {
	return &scenario.ScenarioConfig{
		Seed: seed, Dur: dur,
		Link: scenario.LinkConfig{RateMbps: rateMbps, OwdMs: owdMs,
			Queue: scenario.QueueConfig{Kind: "taildrop", LimitPkts: limitPkts}},
		Flows: flows,
	}
}

// bdpBytesOf returns the bandwidth-delay product in bytes for rate (Mbps)
// and round-trip time (ms).
func bdpBytesOf(rateMbps, rttMs float64) float64 {
	return rateMbps * 1e6 / 8 * rttMs / 1000
}

// bdpPkts returns the BDP in 1500-byte packets, rounded.
func bdpPkts(rateMbps, rttMs float64) int {
	return int(bdpBytesOf(rateMbps, rttMs)/1500 + 0.5)
}

// runCfg runs a scenario and returns decoded records and the summary.
func runCfg(t *testing.T, cfg *scenario.ScenarioConfig) ([]stream.Record, probe.RunSummary) {
	t.Helper()
	recs, sum, err := runCfgE(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return recs, sum
}

func runCfgE(cfg *scenario.ScenarioConfig) ([]stream.Record, probe.RunSummary, error) {
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

// linFit is an ordinary least-squares fit y = slope*x + intercept with the
// coefficient of determination in y.
func linFit(xs, ys []float64) (slope, intercept, r2 float64) {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	slope = (n*sxy - sx*sy) / (n*sxx - sx*sx)
	intercept = (sy - slope*sx) / n
	mean := sy / n
	var ssTot, ssRes float64
	for i := range xs {
		pred := slope*xs[i] + intercept
		ssTot += (ys[i] - mean) * (ys[i] - mean)
		ssRes += (ys[i] - pred) * (ys[i] - pred)
	}
	r2 = 1 - ssRes/ssTot
	return
}

// jainIndex computes Jain's fairness index over per-flow throughputs.
func jainIndex(xs []float64) float64 {
	var sum, sumSq float64
	for _, x := range xs {
		sum += x
		sumSq += x * x
	}
	if sumSq == 0 {
		return 0
	}
	return sum * sum / (float64(len(xs)) * sumSq)
}

// updateDocSection rewrites the region between "<!-- begin:name -->" and
// "<!-- end:name -->" in docs/validation.md when -update is set. Tests call
// it unconditionally; without -update it is a no-op so the same test both
// asserts and (on demand) regenerates its table.
func updateDocSection(t *testing.T, name, content string) {
	t.Helper()
	if !*updateDocs {
		return
	}
	const path = "../docs/validation.md"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("update docs: %v", err)
	}
	begin := fmt.Sprintf("<!-- begin:%s -->", name)
	end := fmt.Sprintf("<!-- end:%s -->", name)
	s := string(data)
	i := strings.Index(s, begin)
	j := strings.Index(s, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("update docs: markers %q/%q not found in %s", begin, end, path)
	}
	out := s[:i+len(begin)] + "\n" + strings.TrimSpace(content) + "\n" + s[j:]
	if err := os.WriteFile(path, []byte(out), 0644); err != nil {
		t.Fatalf("update docs: %v", err)
	}
	t.Logf("docs/validation.md section %q updated", name)
}

// cubicResponseMbps is the RFC 9438 CUBIC average-rate model under random
// loss p: the deterministic-loss average window for the concave/convex
// regime, floored by the Reno-friendly (Mathis) window, converted to Mbps.
// (netstack cubic implements the TCP-friendly region, so the effective
// model is the max of the two.)
func cubicRespondMbps(p, rttS float64, mssBytes float64) float64 {
	const (
		c    = 0.4
		beta = 0.7
	)
	// W_avg for pure cubic under deterministic loss rate p:
	// (C*(3+beta)/(4*(1-beta)))^(1/4) * RTT^(3/4) * p^(-3/4).
	wCubic := math.Pow(c*(3+beta)/(4*(1-beta)), 0.25) * math.Pow(rttS, 0.75) * math.Pow(p, -0.75)
	// Reno/Mathis: sqrt(3/(2p)).
	wReno := math.Sqrt(3 / (2 * p))
	w := math.Max(wCubic, wReno)
	return w * mssBytes * 8 / rttS / 1e6
}
