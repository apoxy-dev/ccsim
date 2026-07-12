package sim

// Property-based tests: randomized scenarios checked against harness-level
// invariants, live-mutation determinism, and the equivalence of
// pre-declared events vs runtime injection. Failures print the scenario
// JSON so a case can be replayed exactly.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"ccsim/scenario"
	"ccsim/stream"
)

// fuzzIterations is raised to 200 by the slow build tag (property_slow_test.go).
var fuzzIterations = 15

// randScenario draws a bounded random valid config from rng.
func randScenario(rng *rand.Rand, iter int) *scenario.ScenarioConfig {
	dur := 3 + rng.Float64()*5 // 3-8 s
	cfg := &scenario.ScenarioConfig{
		Seed: rng.Int64N(1 << 30),
		Dur:  dur,
		Link: scenario.LinkConfig{
			// Log-uniform rate 1-1000 Mbps and RTT ~1-400 ms.
			RateMbps: mathPow10(rng.Float64() * 3),
			OwdMs:    0.5 * mathPow10(rng.Float64()*2.6),
			Loss:     [4]float64{0, 0, rng.Float64() * 0.01, rng.Float64() * 0.05}[rng.IntN(4)],
		},
	}
	kinds := []string{"taildrop", "red", "codel", "fqcodel"}
	q := scenario.QueueConfig{Kind: kinds[rng.IntN(len(kinds))]}
	switch q.Kind {
	case "taildrop", "red":
		q.LimitPkts = 16 + rng.IntN(5000)
	default:
		q.ECN = rng.IntN(2) == 0
	}
	cfg.Link.Queue = q

	nFlows := 1 + rng.IntN(8)
	ccs := []string{"cubic", "bbr", "reno"}
	for i := 0; i < nFlows; i++ {
		f := scenario.FlowConfig{
			CC:      ccs[rng.IntN(len(ccs))],
			StartAt: rng.Float64() * dur / 2,
			App:     scenario.AppConfig{Kind: "bulk"},
		}
		if rng.IntN(4) == 0 {
			f.App = scenario.AppConfig{Kind: "rr",
				ReqBytes: 64 + rng.IntN(4096), RespBytes: 256 + rng.IntN(65536),
				PoissonRate: 0.5 + rng.Float64()*20}
		}
		if rng.IntN(5) == 0 {
			f.ExtraOwdMs = rng.Float64() * 50
		}
		cfg.Flows = append(cfg.Flows, f)
	}
	// Random injected events on live-settable paths.
	paths := []string{"link.rate_mbps", "link.loss", "link.owd_ms"}
	for i, n := 0, rng.IntN(3); i < n; i++ {
		p := paths[rng.IntN(len(paths))]
		var v float64
		switch p {
		case "link.rate_mbps":
			v = mathPow10(rng.Float64() * 3)
		case "link.loss":
			v = rng.Float64() * 0.05
		case "link.owd_ms":
			v = 0.5 * mathPow10(rng.Float64()*2.6)
		}
		cfg.Events = append(cfg.Events, scenario.InjectEvent{
			At: rng.Float64() * dur, Path: p, Value: jsonNum(v),
		})
	}
	return cfg
}

func mathPow10(x float64) float64 { return math.Pow(10, x) }

func jsonNum(v float64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf("%g", v))
}

// Test 20: the scenario fuzzer. Invariants: no panic; queue depth bounded
// by the configured limit; per-flow cwnd >= 1 pkt, monotone cumulative
// counters, inflight bounded by cwnd with slack; time monotone; every flow
// with >= 5 s of runtime delivers bytes; no timer leak at teardown.
func TestScenarioFuzz(t *testing.T) {
	master := rand.New(rand.NewPCG(0xF022, 7))
	for iter := 0; iter < fuzzIterations; iter++ {
		cfg := randScenario(master, iter)
		js, _ := json.Marshal(cfg)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iter %d PANIC: %v\nscenario for replay: %s", iter, r, js)
				}
			}()

			var buf bytes.Buffer
			w := stream.NewWriter(&buf, 0)
			s, err := New(cfg, w)
			if err != nil {
				t.Fatalf("iter %d: New: %v\nscenario: %s", iter, err, js)
			}
			// Continuous invariant: forward queue never exceeds its packet
			// limit (checked at every slice boundary).
			qLimit := cfg.Link.Queue.LimitPkts
			s.Run(func() bool {
				if qLimit > 0 && cfg.Link.Queue.Kind == "taildrop" {
					if qp, _ := s.lnk.QueueDepth(); qp > qLimit {
						t.Errorf("iter %d: queue depth %d > limit %d at t=%v\nscenario: %s",
							iter, qp, qLimit, s.Elapsed(), js)
						return false
					}
				}
				return true
			})

			recs, err := stream.Decode(buf.Bytes())
			if err != nil {
				t.Fatalf("iter %d: decode: %v\nscenario: %s", iter, err, js)
			}
			checkStreamInvariants(t, iter, cfg, recs, js)

			// Timer-leak bound: netstack keeps a handful of timers per
			// endpoint alive (keepalive, delayed-ACK, pacing, retransmit);
			// the bound catches unbounded leaks, not fixed per-flow state.
			if pending := s.PendingTimers(); pending > 40+30*len(cfg.Flows) {
				t.Errorf("iter %d: %d timers still scheduled at teardown (flows=%d)\nscenario: %s",
					iter, pending, len(cfg.Flows), js)
			}
			s.Close()
		}()
	}
}

// checkStreamInvariants validates the decoded sample stream.
func checkStreamInvariants(t *testing.T, iter int, cfg *scenario.ScenarioConfig, recs []stream.Record, js []byte) {
	t.Helper()
	prevT := -1.0
	type flowAgg struct {
		lastAcked, lastRetrans float64
		lastCwnd, peakCwnd     float64
		maxAcked               float64
		inflViolations         int
		established            bool // first nonzero cwnd seen (samples during the handshake read 0)
	}
	flows := make([]flowAgg, len(cfg.Flows))
	// cwnd (pkts) and inflight (bytes) arrive as separate records of the
	// same sample tick; remember the last cwnd to bound inflight.
	for _, r := range recs {
		if r.T < prevT {
			t.Fatalf("iter %d: time went backwards (%.6f after %.6f)\nscenario: %s", iter, r.T, prevT, js)
		}
		prevT = r.T
		if int(r.Flow) >= len(flows) {
			continue // link pseudo-flows
		}
		f := &flows[r.Flow]
		switch r.Kind {
		case stream.KindCwndPkts:
			if r.Value >= 1 {
				f.established = true
			} else if f.established {
				t.Fatalf("iter %d: flow %d cwnd %.1f < 1 pkt at t=%.3f (after establishment)\nscenario: %s",
					iter, r.Flow, r.Value, r.T, js)
			}
			f.lastCwnd = r.Value
			if r.Value > f.peakCwnd {
				f.peakCwnd = r.Value
			}
		case stream.KindInflightBytes:
			// The stream's inflight is SND.NXT - SND.UNA (outstanding).
			// Once a flow has retransmitted, RFC 6675 legitimately admits
			// outstanding far beyond cwnd (pipe = outstanding - sacked -
			// lost is what cwnd bounds, and the SACKed-hole span is
			// unbounded during a long recovery), so the tight bound is
			// only sound while the flow is loss-free.
			if f.lastRetrans == 0 && f.peakCwnd > 0 && r.Value > f.peakCwnd*1500*1.5+64*1500 {
				f.inflViolations++
				if f.inflViolations > 3 { // 1 ms sampling can miss a short cwnd peak
					t.Fatalf("iter %d: flow %d inflight %.0f B above 1.5x peak cwnd %.0f pkts at t=%.3f (loss-free flow)\nscenario: %s",
						iter, r.Flow, r.Value, f.peakCwnd, r.T, js)
				}
			}
		case stream.KindBytesAckedCum:
			if r.Value < f.lastAcked {
				t.Fatalf("iter %d: flow %d cumulative acked decreased at t=%.3f\nscenario: %s", iter, r.Flow, r.T, js)
			}
			f.lastAcked = r.Value
			if r.Value > f.maxAcked {
				f.maxAcked = r.Value
			}
		case stream.KindRetransCum:
			if r.Value < f.lastRetrans {
				t.Fatalf("iter %d: flow %d cumulative retrans decreased at t=%.3f\nscenario: %s", iter, r.Flow, r.T, js)
			}
			f.lastRetrans = r.Value
		}
	}
	// Liveness: every flow with >= 5 s of runtime delivered something.
	for i, fc := range cfg.Flows {
		if cfg.Dur-fc.StartAt >= 5 && flows[i].maxAcked == 0 {
			t.Errorf("iter %d: flow %d (%s, start %.1fs, dur %.1fs) delivered 0 bytes — silent deadlock?\nscenario: %s",
				iter, i, fc.CC, fc.StartAt, cfg.Dur, js)
		}
	}
}

// Test 21: live-mutation determinism. The same schedule of Set calls at
// slice boundaries must reproduce byte-identical streams across runs.
func TestLiveMutationDeterminism(t *testing.T) {
	master := rand.New(rand.NewPCG(0xF123, 3))
	for iter := 0; iter < 3; iter++ {
		cfg := randScenario(master, iter)
		cfg.Events = nil // mutations come from the live path in this test
		type mut struct {
			at   time.Duration
			path string
			v    float64
		}
		var muts []mut
		for i := 0; i < 5; i++ {
			muts = append(muts, mut{
				at:   time.Duration(master.Int64N(int64(cfg.Duration()))),
				path: "link.rate_mbps",
				v:    mathPow10(master.Float64() * 3),
			})
		}
		runOnce := func() []byte {
			var buf bytes.Buffer
			w := stream.NewWriter(&buf, 0)
			s, err := New(cfg, w)
			if err != nil {
				t.Fatal(err)
			}
			next := 0
			s.Run(func() bool {
				for next < len(muts) && muts[next].at <= s.Elapsed() {
					if err := s.Set(muts[next].path, muts[next].v); err != nil {
						t.Fatal(err)
					}
					next++
				}
				return true
			})
			s.Close()
			return buf.Bytes()
		}
		a, b := runOnce(), runOnce()
		if !bytes.Equal(a, b) {
			js, _ := json.Marshal(cfg)
			t.Fatalf("iter %d: identical live-mutation schedules produced different streams (%d vs %d bytes)\nscenario: %s",
				iter, len(a), len(b), js)
		}
	}
}

// Test 22: a parameter change declared in the scenario's Events is
// byte-identical to the same change injected at runtime (InjectAt) — the
// control path and the event-loop scheduling boundary agree exactly.
func TestInjectVsDeclaredEventsEquivalence(t *testing.T) {
	base := vCfg(51, 12, 100, 10, 334, vBulk("cubic", 0), vBulk("bbr", 0))
	// Off-grid times: no collision with 1 ms sampling ticks, so heap
	// tie-breaking cannot mask an ordering difference.
	events := []scenario.InjectEvent{
		{At: 4.0037, Path: "link.rate_mbps", Value: jsonNum(25)},
		{At: 7.2093, Path: "link.owd_ms", Value: jsonNum(30)},
		{At: 9.5551, Path: "link.rate_mbps", Value: jsonNum(80)},
	}

	declared := *base
	declared.Events = events
	var bufA bytes.Buffer
	sA, err := New(&declared, stream.NewWriter(&bufA, 0))
	if err != nil {
		t.Fatal(err)
	}
	sA.Run(nil)
	sA.Close()

	runtime := *base
	runtime.Events = nil
	var bufB bytes.Buffer
	sB, err := New(&runtime, stream.NewWriter(&bufB, 0))
	if err != nil {
		t.Fatal(err)
	}
	injected := false
	sB.Run(func() bool {
		if !injected && sB.Elapsed() >= 1*time.Second {
			for _, e := range events {
				var v float64
				if err := json.Unmarshal(e.Value, &v); err != nil {
					t.Fatal(err)
				}
				sB.InjectAt(time.Duration(e.At*float64(time.Second)), e.Path, v)
			}
			injected = true
		}
		return true
	})
	sB.Close()

	a, b := bufA.Bytes(), bufB.Bytes()
	if !bytes.Equal(a, b) {
		div := firstDivergence(a, b)
		t.Fatalf("declared-events vs runtime-inject streams differ (%d vs %d bytes, first divergent record %d)",
			len(a), len(b), div)
	}
	t.Logf("declared vs injected: byte-identical streams (%d bytes)", len(a))
}

func firstDivergence(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i / stream.RecordSize
		}
	}
	return n / stream.RecordSize
}
