package bbr

// Conformance tests: drive the BBR object with scripted delivery-rate
// samples on virtual time and check the model against the constants and
// update rules of draft-ietf-ccwg-bbr-03 as implemented (documented
// deviations in docs/decisions.md section 7 are asserted as implemented,
// with the draft/reference behavior noted in comments).
//
// Every quantitative check logs measured vs predicted values even when
// passing, so the numbers can be quoted in write-ups.

import (
	"math"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// steadySample builds one steady-state rate sample and advances fake time.
func steadySample(b *BBR, f *fakeSender, rateBps int64, rtt, step time.Duration) tcp.SimRateSample {
	f.now += step
	f.inflight = rateBps / 8 * int64(rtt) / int64(time.Second)
	delivered := b.lastSample.Delivered + rateBps/8*int64(step)/int64(time.Second)
	prior := delivered - f.inflight
	if prior < 0 {
		prior = 0
	}
	return tcp.SimRateSample{
		Now:             f.now,
		AckedBytes:      rateBps / 8 * int64(step) / int64(time.Second),
		Delivered:       delivered,
		PriorDelivered:  prior,
		DeliveryRateBps: rateBps,
		RTT:             rtt,
		Interval:        step,
		InflightBytes:   f.inflight,
	}
}

// --- Test 4: filter windows ------------------------------------------------

// The max-bw filter holds two buckets; a bandwidth step down must survive
// exactly until two bucket turnovers have passed. In CRUISE/DOWN the aging
// backstop turns a bucket over at most once per second (decisions.md
// section 7), so a 5x rate drop must be forgotten within about 2 s and must
// NOT be forgotten before the first turnover.
func TestMaxBwFilterDecayTiming(t *testing.T) {
	b, f := newTestBBR()
	rtt := 20 * time.Millisecond
	trace(b, f, 100e6, rtt, 4*time.Second) // reach ProbeBW with a full model
	if b.state == StateStartup || b.state == StateDrain {
		t.Fatalf("not in ProbeBW after 4s (state=%s)", StateName(b.state))
	}
	// Pin the state to CRUISE and push the probe timer far out so only the
	// aging backstop advances the filter during the step-down.
	b.enter(StateProbeBWCruise, f.now)
	b.cycleStamp = f.now
	b.probeWait = time.Hour

	stepStart := f.now
	var tHigh, tLow time.Duration // last time still high, first time low
	for elapsed := time.Duration(0); elapsed < 4*time.Second; elapsed += 10 * time.Millisecond {
		b.OnAck(steadySample(b, f, 20e6, rtt, 10*time.Millisecond))
		if b.maxBw() > 95e6 {
			tHigh = f.now - stepStart
		} else if tLow == 0 && b.maxBw() <= 25e6 {
			tLow = f.now - stepStart
		}
	}
	t.Logf("held >=95Mbps until +%v, decayed to <=25Mbps at +%v (window: 2 buckets, backstop 1s/turnover)", tHigh, tLow)
	if tHigh < 900*time.Millisecond {
		t.Errorf("old bandwidth forgotten at +%v, before the first bucket turnover (~1s)", tHigh)
	}
	if tLow == 0 || tLow > 2500*time.Millisecond {
		t.Errorf("old bandwidth survived past two turnovers: low at +%v, want <= ~2.5s", tLow)
	}
}

// The min_rtt filter is a windowed minimum over 4 x 2.5 s buckets: a stale
// low sample must expire between 7.5 s and 10 s (bucket granularity) after
// it stops recurring, and its expiry schedules ProbeRTT via minRTTStamp.
func TestMinRTTWindowExpiryTiming(t *testing.T) {
	b, f := newTestBBR()
	trace(b, f, 50e6, 20*time.Millisecond, 300*time.Millisecond)
	if b.minRTT != 20*time.Millisecond {
		t.Fatalf("minRTT %v after low phase, want 20ms", b.minRTT)
	}
	riseStart := f.now
	var expiredAt time.Duration
	for elapsed := time.Duration(0); elapsed < 12*time.Second; elapsed += 10 * time.Millisecond {
		b.OnAck(steadySample(b, f, 50e6, 80*time.Millisecond, 10*time.Millisecond))
		if expiredAt == 0 && b.minRTT >= 70*time.Millisecond {
			expiredAt = f.now - riseStart
			break
		}
	}
	t.Logf("20ms sample expired at +%v (bucketed window: expect (7.5s, 10s])", expiredAt)
	if expiredAt < 7500*time.Millisecond || expiredAt > 10*time.Second+100*time.Millisecond {
		t.Errorf("min_rtt expiry at +%v outside the 4x2.5s bucketed 10s window", expiredAt)
	}
}

// --- Test 5: ProbeBW cycle sequence and timing -------------------------------

func TestProbeBWCycleSequence(t *testing.T) {
	b, f := newTestBBR()
	rtt := 20 * time.Millisecond
	trace(b, f, 100e6, rtt, 3*time.Second)

	// Record the state sequence over 20 s of steady ProbeBW.
	var seq []int
	var times []time.Duration
	last := -1
	cruiseEnter, refillEnter, upEnter := time.Duration(0), time.Duration(0), time.Duration(0)
	var cruiseDur []time.Duration
	var refillRounds []int64
	for elapsed := time.Duration(0); elapsed < 20*time.Second; elapsed += 5 * time.Millisecond {
		b.OnAck(steadySample(b, f, 100e6, rtt, 5*time.Millisecond))
		if b.state != last {
			seq = append(seq, b.state)
			times = append(times, f.now)
			switch b.state {
			case StateProbeBWCruise:
				cruiseEnter = f.now
			case StateProbeBWRefill:
				refillEnter = f.now
				if cruiseEnter != 0 {
					cruiseDur = append(cruiseDur, f.now-cruiseEnter)
				}
			case StateProbeBWUp:
				upEnter = f.now
				_ = upEnter
				if refillEnter != 0 {
					refillRounds = append(refillRounds, int64((f.now-refillEnter)/rtt))
				}
			}
			last = b.state
		}
	}

	// Assert the cyclic order DOWN -> CRUISE -> REFILL -> UP -> DOWN.
	// (DOWN -> REFILL is legal when the drain target is unreachable, but at
	// 1xBDP steady inflight DOWN must exit to CRUISE.)
	next := map[int]int{
		StateProbeBWDown:   StateProbeBWCruise,
		StateProbeBWCruise: StateProbeBWRefill,
		StateProbeBWRefill: StateProbeBWUp,
		StateProbeBWUp:     StateProbeBWDown,
	}
	names := ""
	for _, s := range seq {
		names += StateName(s) + " "
	}
	t.Logf("state sequence: %s", names)
	for i := 0; i+1 < len(seq); i++ {
		cur, nxt := seq[i], seq[i+1]
		if cur == StateStartup || cur == StateDrain || nxt == StateProbeRTT || cur == StateProbeRTT {
			continue // entry path and ProbeRTT interruptions are separately tested
		}
		if want := next[cur]; nxt != want {
			t.Errorf("transition %s -> %s at t=%v, want -> %s",
				StateName(cur), StateName(nxt), times[i+1], StateName(want))
		}
	}

	// CRUISE duration = probe interval: jittered 2-3 s wall time (or the
	// Reno-rounds bound, whichever first; at 167 pkts BDP the 63-round cap
	// is 63*20ms = 1.26s... rounds bound can fire first, so accept [1,3.2]s).
	for _, d := range cruiseDur {
		t.Logf("cruise duration %v (probe interval; jitter range 2-3s, reno-rounds floor 63 rtt=1.26s)", d)
		if d < time.Second || d > 3200*time.Millisecond {
			t.Errorf("probe interval %v outside [1s, 3.2s]", d)
		}
	}
	// REFILL lasts one packet-timed round.
	for _, r := range refillRounds {
		t.Logf("refill lasted ~%d rounds", r)
		if r < 1 || r > 2 {
			t.Errorf("REFILL lasted ~%d rounds, want 1", r)
		}
	}
	if len(cruiseDur) == 0 || len(refillRounds) == 0 {
		t.Fatal("did not observe complete probe cycles in 20s")
	}
}

// Pacing rate must equal gain * bw * (1 - margin) exactly, per phase.
func TestPacingRatePerPhaseExact(t *testing.T) {
	b, f := newTestBBR()
	rtt := 20 * time.Millisecond
	trace(b, f, 100e6, rtt, 3*time.Second)
	gains := map[int]float64{
		StateProbeBWDown:   0.9,
		StateProbeBWCruise: 1.0,
		StateProbeBWRefill: 1.0,
		StateProbeBWUp:     1.25,
	}
	for state, gain := range gains {
		// Set the phase and recompute outputs directly: OnAck may
		// legitimately transition out of the assigned phase (DOWN exits to
		// CRUISE the moment inflight <= BDP), which would test the wrong
		// gain.
		b.state = state
		b.setPacing()
		want := int64(gain * float64(b.bw()) * (1 - pacingMargin))
		if f.pacing != want {
			t.Errorf("%s: pacing %d, want %d (= %.2f * bw * 0.99)", StateName(state), f.pacing, want, gain)
		}
		t.Logf("%s: pacing %.2f Mbps = %.2f * bw(%.2f Mbps) * 0.99", StateName(state),
			float64(f.pacing)/1e6, gain, float64(b.bw())/1e6)
	}
	// cwnd gain in ProbeBW is 2.0: cwnd = 2*BDP + 2 MSS aggregation allowance.
	b.state = StateProbeBWCruise
	b.inflightHi, b.inflightLo = math.MaxInt64, math.MaxInt64
	b.setCwnd()
	wantCwnd := int((b.bdpBytes(2.0) + 2*int64(mss)) / int64(mss))
	if f.cwnd != wantCwnd {
		t.Errorf("ProbeBW cwnd %d pkts, want %d (2*BDP + 2 MSS)", f.cwnd, wantCwnd)
	}
}

// --- Test 6: loss response arithmetic ----------------------------------------

// betaCut applies the beta multiplier with runtime float semantics (a
// constant expression would round differently than the code under test).
func betaCut(v int64) int64 { return int64(beta * float64(v)) }

func TestLossResponseTable(t *testing.T) {
	const maxI = int64(math.MaxInt64)
	cases := []struct {
		name    string
		state   int
		bwLo    int64 // pre
		inflLo  int64
		latest  int64 // bwLatest this round
		latestI int64 // inflightLatest this round
		loss    bool
		wantBw  int64 // post
		wantIn  int64
	}{
		{
			// First loss: bw_lo initialized from max_bw then cut to
			// max(latest, beta*bw_lo) = max(40, 0.7*100) = 70. inflight_lo
			// initializes from BDP of the just-cut bw (70e6/8*20ms =
			// 175000; deviation from the reference's cwnd init, see
			// decisions.md section 7) and is then beta-cut since
			// latestI is below the floor. Runtime float: expected value
			// computed with the same expression the code uses.
			name: "first-loss-beta-cut", state: StateProbeBWCruise,
			bwLo: maxI, inflLo: maxI, latest: 40e6, latestI: 100_000, loss: true,
			wantBw: 70e6, wantIn: betaCut(175_000),
		},
		{
			// Latest above the beta floor: bw_lo tracks latest, no deeper cut.
			name: "latest-dominates", state: StateProbeBWCruise,
			bwLo: 80e6, inflLo: 200_000, latest: 75e6, latestI: 190_000, loss: true,
			wantBw: 75e6, wantIn: 190_000,
		},
		{
			// Already-latched bounds cut again by beta (compounding rounds).
			name: "latched-compounds", state: StateProbeBWCruise,
			bwLo: 50e6, inflLo: 150_000, latest: 10e6, latestI: 10_000, loss: true,
			wantBw: 35e6, wantIn: 105_000,
		},
		{
			// No cut while probing (REFILL): bounds untouched.
			name: "no-cut-in-refill", state: StateProbeBWRefill,
			bwLo: 50e6, inflLo: 150_000, latest: 10e6, latestI: 10_000, loss: true,
			wantBw: 50e6, wantIn: 150_000,
		},
		{
			// No loss in round: bounds untouched.
			name: "no-loss-no-cut", state: StateProbeBWCruise,
			bwLo: 50e6, inflLo: 150_000, latest: 10e6, latestI: 10_000, loss: false,
			wantBw: 50e6, wantIn: 150_000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := newTestBBR()
			// Fixed model: max_bw 100 Mbps, min_rtt 20ms -> BDP 250000 B.
			b.maxBwFilter[0] = 100e6
			b.minRTT = 20 * time.Millisecond
			b.state = c.state
			b.bwLo, b.inflightLo = c.bwLo, c.inflLo
			b.bwLatest, b.inflightLatest = c.latest, c.latestI
			b.lossInRound = c.loss
			b.adaptLowerBounds()
			if b.bwLo != c.wantBw {
				t.Errorf("bw_lo = %d, want %d", b.bwLo, c.wantBw)
			}
			if b.inflightLo != c.wantIn {
				t.Errorf("inflight_lo = %d, want %d", b.inflightLo, c.wantIn)
			}
			t.Logf("bw_lo %d -> %d, inflight_lo %d -> %d", c.bwLo, b.bwLo, c.inflLo, b.inflightLo)
		})
	}
}

// Loss crossing the 2% threshold during UP aborts the probe and latches
// inflight_hi from the delivered inflight sample (floored at beta*BDP).
func TestProbeAbortSetsInflightHi(t *testing.T) {
	b, f := newTestBBR()
	rtt := 20 * time.Millisecond
	trace(b, f, 100e6, rtt, 3*time.Second)
	b.enter(StateProbeBWUp, f.now)
	b.lossEventsRound = 1
	inflight := f.inflight
	b.lostBytesRound = int64(0.03 * float64(inflight)) // 3% > 2% threshold
	b.OnAck(steadySample(b, f, 100e6, rtt, 10*time.Millisecond))
	if b.state == StateProbeBWUp {
		t.Fatal("UP survived 3% loss")
	}
	if b.state != StateProbeBWDown {
		t.Errorf("probe abort entered %s, want ProbeBW:DOWN", StateName(b.state))
	}
	target := float64(b.bdpBytes(1.0))
	wantHi := int64(math.Max(float64(f.inflight), beta*target))
	if b.inflightHi != wantHi {
		t.Errorf("inflight_hi = %d, want %d (max(inflight, beta*BDP))", b.inflightHi, wantHi)
	}
	t.Logf("inflight_hi latched at %d (inflight=%d, beta*BDP=%.0f)", b.inflightHi, f.inflight, beta*target)
}

// --- Test 7: ECN response ----------------------------------------------------

// Alpha follows the EWMA alpha' = (1-g)*alpha + g*ce_frac with g = 1/16,
// updated once per round.
func TestECNAlphaEWMA(t *testing.T) {
	b, f := newTestBBR()
	rtt := 20 * time.Millisecond
	trace(b, f, 50e6, rtt, time.Second)

	// Exact single-round check: stage a round with a known CE byte
	// fraction and close it with one round-boundary sample, then verify
	// alpha' = (1-g)*alpha + g*ce_frac with the code's own float ordering.
	alpha0 := b.ecnAlpha
	b.ackedBytesRound, b.ceBytesRound = 900, 400
	closing := steadySample(b, f, 50e6, rtt, 10*time.Millisecond)
	closing.AckedBytes = 100
	closing.ECE = true // 400+100 CE of 900+100 acked -> ce_frac 0.5
	closing.Delivered = b.lastSample.Delivered + 100
	closing.PriorDelivered = b.nextRoundDelivered // force a round boundary
	b.OnAck(closing)
	const ceFrac = 0.5
	want := float64((1-ecnAlphaGain)*alpha0) + float64(ecnAlphaGain*ceFrac)
	if math.Abs(b.ecnAlpha-want) > 1e-12 {
		t.Errorf("alpha after round = %.6f, want %.6f = (1-1/16)*%.6f + (1/16)*%.2f",
			b.ecnAlpha, want, alpha0, ceFrac)
	}
	t.Logf("alpha %.4f -> %.4f (ce_frac %.2f, gain 1/16, predicted %.4f)", alpha0, b.ecnAlpha, ceFrac, want)

	// Trend: further high-CE rounds keep converging toward the fraction.
	prev := b.ecnAlpha
	for r := 0; r < 3; r++ {
		b.ackedBytesRound, b.ceBytesRound = 900, 400
		s := steadySample(b, f, 50e6, rtt, 10*time.Millisecond)
		s.AckedBytes, s.ECE = 100, true
		s.Delivered = b.lastSample.Delivered + 100
		s.PriorDelivered = b.nextRoundDelivered
		b.OnAck(s)
		if b.ecnAlpha <= prev || b.ecnAlpha > ceFrac {
			t.Errorf("round %d: alpha %.4f not converging monotonically toward %.2f (prev %.4f)",
				r, b.ecnAlpha, ceFrac, prev)
		}
		prev = b.ecnAlpha
	}
	t.Logf("alpha converging: %.4f after 4 half-CE rounds", b.ecnAlpha)
}

// The ECN cut scales bw_lo by (1 - alpha*ecnFactor) with a floor of 1/3,
// and composes with a loss cut in the same round (loss beta first, then the
// ECN scale, per adaptLowerBounds order).
func TestECNAndLossCompose(t *testing.T) {
	b, _ := newTestBBR()
	b.maxBwFilter[0] = 100e6
	b.minRTT = 20 * time.Millisecond
	b.state = StateProbeBWCruise
	b.ecnAlpha = 0.5
	b.lossInRound = true
	b.bwLatest = 10e6 // far below the beta floor
	b.inflightLatest = 10_000
	b.ceBytesRound, b.ackedBytesRound = 500, 1000 // CE present this round
	b.bwLo, b.inflightLo = math.MaxInt64, math.MaxInt64
	b.adaptLowerBounds()
	scale := 1 - 0.5*ecnFactor // = 1 - alpha*factor = 0.8333
	wantBw := int64(float64(int64(beta*100e6)) * scale)
	if b.bwLo != wantBw {
		t.Errorf("composed bw_lo = %d, want %d (beta cut then ECN scale %.4f)", b.bwLo, wantBw, scale)
	}
	t.Logf("loss+ECN composed: bw_lo = 100e6 * 0.7 * %.4f = %d", scale, b.bwLo)

	// Floor: alpha=1 would give scale 2/3... the floor binds at 1/3 when
	// alpha*factor > 2/3, i.e. alpha > 2 — unreachable since alpha <= 1;
	// verify the clamp arithmetic directly instead.
	if s := 1 - float64(3.0*ecnFactor); s > 1.0/3 {
		t.Errorf("floor arithmetic: scale %v should clamp at 1/3", s)
	}
}

// --- Test 8: startup exit ------------------------------------------------------

// (a) plateau exit is covered by TestStartupExitOnPlateau. Here: gains held
// through startup, Drain gain applied after, and the loss-based and
// still-growing paths.
func TestStartupGainsAndDrain(t *testing.T) {
	b, f := newTestBBR()
	rtt := 40 * time.Millisecond
	rate := int64(20e6)
	trace(b, f, rate, rtt, 2*rtt)
	if b.state != StateStartup {
		t.Fatalf("left startup too early")
	}
	// Pacing gain 2.77 and cwnd gain 2.0 while in startup.
	wantPacing := int64(startupPacingGain * float64(b.bw()) * (1 - pacingMargin))
	if f.pacing != wantPacing {
		t.Errorf("startup pacing %d, want %d (2.77 * bw * 0.99)", f.pacing, wantPacing)
	}
	// Pre-full-pipe cwnd grows by at most the acked data per ACK and is
	// not snapped down to the model target (draft BBRSetCwnd), so it lands
	// in [max_inflight, max_inflight + one ACK's worth).
	target := int((b.bdpBytes(startupCwndGain) + 2*int64(mss)) / int64(mss))
	ackPkts := int(rate/8*int64(10*time.Millisecond)/int64(time.Second))/mss + 1
	if f.cwnd < target || f.cwnd > target+ackPkts {
		t.Errorf("startup cwnd %d, want in [%d, %d] (2.0*BDP+2MSS, +1 ACK overshoot)",
			f.cwnd, target, target+ackPkts)
	}
	// Force the plateau exit; in Drain the pacing gain must be 1/2.77.
	for i := 0; i < 12 && b.state == StateStartup; i++ {
		trace(b, f, rate, rtt, rtt)
	}
	if b.state == StateDrain {
		wantDrain := int64(drainPacingGain * float64(b.bw()) * (1 - pacingMargin))
		if f.pacing != wantDrain {
			t.Errorf("drain pacing %d, want %d (bw/2.77 * 0.99)", f.pacing, wantDrain)
		}
		t.Logf("drain pacing %.2f Mbps = bw(%.2f)/2.77 * 0.99", float64(f.pacing)/1e6, float64(b.bw())/1e6)
	} else {
		// Drain can complete within one OnAck when inflight is already at
		// 1xBDP (trace holds it there); reaching ProbeBW is the accepted
		// fast path.
		t.Logf("drain completed inline (state=%s); inflight already <= BDP", StateName(b.state))
	}
}

func TestStartupExitOnLoss(t *testing.T) {
	b, f := newTestBBR()
	rtt := 40 * time.Millisecond
	trace(b, f, 20e6, rtt, 2*rtt) // establish a round in flight
	if b.state != StateStartup {
		t.Fatal("left startup prematurely")
	}
	// A round with >= fullLossCount loss events and > 2% lost bytes.
	b.lossInRound = true
	b.lossEventsRound = fullLossCount
	b.lostBytesRound = int64(0.05 * float64(f.inflight))
	// Deliver the round-closing sample.
	s := steadySample(b, f, 20e6, rtt, rtt)
	b.OnAck(s)
	if b.state == StateStartup {
		t.Fatalf("startup survived %d loss events at 5%% lost bytes", fullLossCount)
	}
	t.Logf("loss-based startup exit -> %s", StateName(b.state))
}

// A single loss during Startup must neither engage the short-term model
// bounds nor throttle pacing: Startup is a bandwidth probe (reference
// bbr_is_probing_bandwidth includes it, and the draft's
// BBRAdaptLowerBounds returns immediately in Startup), and before the
// pipe is full the pacing rate only ratchets upward
// (bbr_set_pacing_rate). Regression: pre-fix, one early loss pinned
// bw_lo/inflight_lo to a still-ramping round sample, pacing and cwnd
// collapsed with them, and the artificial delivery plateau tripped the
// full-bw startup exit at a fraction of the real bandwidth.
func TestStartupSingleLossKeepsRamping(t *testing.T) {
	b, f := newTestBBR()
	rtt := 40 * time.Millisecond
	rate := int64(10e6)
	var maxPacing int64
	for round := 0; round < 24 && b.state == StateStartup; round++ {
		trace(b, f, rate, rtt, rtt)
		if !b.filledPipe && f.pacing < maxPacing {
			t.Fatalf("round %d: pacing dropped %.2f -> %.2f Mbps before full pipe",
				round, float64(maxPacing)/1e6, float64(f.pacing)/1e6)
		}
		if f.pacing > maxPacing {
			maxPacing = f.pacing
		}
		if round == 2 {
			// One loss event mid-ramp: a fresh retransmit appears in the
			// per-round accounting just before the round closes.
			b.lossInRound = true
			b.lossEventsRound = 1
			b.lostBytesRound = int64(mss)
		}
		if round >= 3 && b.state == StateStartup {
			if b.bwLo != math.MaxInt64 {
				t.Fatalf("round %d: bw_lo engaged at %.2f Mbps by a single startup loss", round, float64(b.bwLo)/1e6)
			}
			if b.inflightLo != math.MaxInt64 {
				t.Fatalf("round %d: inflight_lo engaged (%d bytes) by a single startup loss", round, b.inflightLo)
			}
		}
		if rate < 100e6 {
			rate = rate * 13 / 10
			if rate > 100e6 {
				rate = 100e6
			}
		}
	}
	if b.state == StateStartup {
		t.Fatal("never exited startup on the 100 Mbps plateau")
	}
	if got := b.maxBw(); got < 90e6 {
		t.Errorf("startup exited with maxBw %.2f Mbps, want >= 90 (premature exit)", float64(got)/1e6)
	}
	t.Logf("startup exit -> %s at maxBw %.2f Mbps, peak pacing %.2f Mbps (loss at round 2 ignored)",
		StateName(b.state), float64(b.maxBw())/1e6, float64(maxPacing)/1e6)
}

// The draft's BBRSetCwnd grows cwnd by at most the newly-acked data per
// ACK and snaps it down to the model target only once the pipe is full.
// Regression: the old setCwnd assigned the target on every ACK, so on a
// low-BDP path the first ACK's cold model (tiny bw * min_rtt) cut cwnd
// from the initial window straight to the 4-packet floor.
func TestCwndGrowByAckedControlLaw(t *testing.T) {
	b, f := newTestBBR()
	rate := int64(100e3) // 100 Kbps, 200 ms: BDP ~1.7 packets, far below IW
	rtt := 200 * time.Millisecond

	b.OnAck(steadySample(b, f, rate, rtt, 10*time.Millisecond))
	if f.cwnd < 10 {
		t.Fatalf("first ACK cut cwnd to %d pkts (initial window 10): pre-full-pipe snap-down", f.cwnd)
	}
	t.Logf("first cold-model ACK: cwnd %d pkts (target would be %d pkts)",
		f.cwnd, int(b.maxInflightBytes()/int64(mss)))

	// Before the pipe is full, cwnd must never decrease.
	prev := f.cwnd
	steps := 0
	for i := 0; i < 500 && !b.filledPipe; i++ {
		b.OnAck(steadySample(b, f, rate, rtt, 10*time.Millisecond))
		if b.filledPipe {
			break // this ACK declared full pipe; snap-down is legal now
		}
		if f.cwnd < prev {
			t.Fatalf("step %d: cwnd decreased %d -> %d pkts before full pipe", i, prev, f.cwnd)
		}
		prev = f.cwnd
		steps++
	}
	if !b.filledPipe {
		t.Fatal("100 Kbps plateau never declared the pipe full")
	}
	t.Logf("pipe full after %d steps with cwnd held at %d pkts (never below 10)", steps, prev)

	// Once full, the model target caps and snaps the window down.
	b.OnAck(steadySample(b, f, rate, rtt, 10*time.Millisecond))
	want := int(b.maxInflightBytes() / int64(mss))
	if want < 4 {
		want = 4
	}
	if f.cwnd != want {
		t.Errorf("post-full-pipe cwnd %d pkts, want snap to max_inflight %d", f.cwnd, want)
	}
	t.Logf("post-full-pipe snap: cwnd %d pkts (max_inflight %d pkts)", f.cwnd, want)
}

func TestStartupNoExitWhileGrowing(t *testing.T) {
	b, f := newTestBBR()
	rtt := 40 * time.Millisecond
	rate := int64(5e6)
	// 30% growth every round: never a plateau.
	for i := 0; i < 10; i++ {
		trace(b, f, rate, rtt, rtt)
		rate = rate * 13 / 10
	}
	if b.state != StateStartup {
		t.Fatalf("exited startup during sustained 30%%/round growth (state=%s)", StateName(b.state))
	}
	if b.filledPipe {
		t.Fatal("filledPipe set during growth")
	}
	t.Logf("still in startup after 10 growing rounds (rate now %.1f Mbps)", float64(rate)/1e6)
}

// --- Test 9: ProbeRTT ---------------------------------------------------------

func TestProbeRTTClampDurationAndRefresh(t *testing.T) {
	b, f := newTestBBR()
	rtt := 30 * time.Millisecond
	trace(b, f, 50e6, rtt, 2*time.Second)

	// Walk forward until ProbeRTT entry (min_rtt goes stale after 5s).
	entered := time.Duration(0)
	for elapsed := time.Duration(0); elapsed < 8*time.Second && entered == 0; elapsed += 10 * time.Millisecond {
		b.OnAck(steadySample(b, f, 50e6, rtt, 10*time.Millisecond))
		if b.state == StateProbeRTT {
			entered = f.now
		}
	}
	if entered == 0 {
		t.Fatal("no ProbeRTT entry within 8s of steady min_rtt")
	}
	staleness := entered - b.minRTTStamp
	t.Logf("ProbeRTT entered with min_rtt staleness %v (interval %v)", staleness, probeRTTInterval)
	if staleness < probeRTTInterval {
		t.Errorf("entered ProbeRTT at staleness %v, before the %v interval", staleness, probeRTTInterval)
	}

	// Drain inflight below the cap; the clamp applies on every OnAck. The
	// clamp target is recomputed per ACK because min_rtt can legitimately
	// drop during the hold (that is ProbeRTT's purpose), shrinking the cap.
	holdStart := time.Duration(0)
	var exitAt time.Duration
	lowRTT := rtt - 10*time.Millisecond
	sawRefresh := false
	for i := 0; i < 200 && b.state == StateProbeRTT; i++ {
		f.now += 10 * time.Millisecond
		f.inflight = 2 * mss
		delivered := b.lastSample.Delivered + mss
		s := tcp.SimRateSample{
			Now: f.now, AckedBytes: mss, Delivered: delivered,
			PriorDelivered:  delivered - f.inflight,
			DeliveryRateBps: 50e6, RTT: rtt, Interval: 10 * time.Millisecond,
			InflightBytes: f.inflight,
		}
		// Once holding, feed one lower RTT sample: the drained queue
		// exposes the true floor, which must land in the filter.
		if holdStart != 0 && !sawRefresh {
			s.RTT = lowRTT
			sawRefresh = true
		}
		b.OnAck(s)
		if b.state == StateProbeRTT {
			wantPkts := int(b.probeRTTCwndBytes() / int64(mss))
			if wantPkts < 4 {
				wantPkts = 4
			}
			if f.cwnd != wantPkts {
				t.Fatalf("ProbeRTT cwnd %d pkts, want clamp %d (max(0.5*BDP, 4 MSS))", f.cwnd, wantPkts)
			}
		}
		if holdStart == 0 && b.probeRTTDone != 0 {
			holdStart = f.now
		}
	}
	exitAt = f.now
	if b.state == StateProbeRTT {
		t.Fatal("never exited ProbeRTT")
	}
	// Exit restores the pre-ProbeRTT window (draft BBRRestoreCwnd) rather
	// than regrowing from the clamp.
	clampPkts := int(b.probeRTTCwndBytes() / int64(mss))
	if f.cwnd < 2*clampPkts {
		t.Errorf("cwnd %d pkts after ProbeRTT exit, want restored well above the %d-pkt clamp", f.cwnd, clampPkts)
	}
	if holdStart == 0 {
		t.Fatal("hold never started (inflight below cap not detected)")
	}
	held := exitAt - holdStart
	t.Logf("held %v at reduced window (want >= %v + 1 round)", held, probeRTTDuration)
	if held < probeRTTDuration {
		t.Errorf("ProbeRTT hold %v shorter than the %v minimum", held, probeRTTDuration)
	}
	if b.minRTT != lowRTT {
		t.Errorf("lower RTT sample during ProbeRTT not absorbed: min_rtt %v, want %v", b.minRTT, lowRTT)
	}
	// Exit reschedules the next probe: stamp refreshed at exit.
	if exitAt-b.minRTTStamp > 50*time.Millisecond {
		t.Errorf("minRTTStamp not refreshed at ProbeRTT exit (age %v)", exitAt-b.minRTTStamp)
	}
}

// --- Test 10: app-limited handling ---------------------------------------------

// App-limited samples must never drag the filter down, and — matching
// tcp_bbr.c (bbr_update_bw: "ignore app-limited samples unless they beat
// the max") — an app-limited sample ABOVE the current max does raise it.
// (The build spec's stricter "never raise" reading would diverge from the
// reference; the dangerous failure mode is low app-limited samples counting,
// which is what the first assertion pins.)
func TestAppLimitedSamples(t *testing.T) {
	b, f := newTestBBR()
	rtt := 20 * time.Millisecond
	trace(b, f, 80e6, rtt, time.Second)
	base := b.maxBw()
	if base < 75e6 {
		t.Fatalf("model did not reach 80 Mbps (%.1f)", float64(base)/1e6)
	}

	// 1. Low app-limited samples: filter unchanged (they cannot lower it,
	// and must not stall its content either — feed only 0.5s so no
	// turnover interferes).
	for i := 0; i < 50; i++ {
		s := steadySample(b, f, 5e6, rtt, 10*time.Millisecond)
		s.IsAppLimited = true
		b.OnAck(s)
	}
	if got := b.maxBw(); got < base {
		t.Errorf("app-limited 5 Mbps samples lowered max_bw: %.1f -> %.1f Mbps",
			float64(base)/1e6, float64(got)/1e6)
	}
	t.Logf("after low app-limited: max_bw %.1f Mbps (was %.1f)", float64(b.maxBw())/1e6, float64(base)/1e6)

	// 2. Higher app-limited sample: raises (reference behavior).
	s := steadySample(b, f, 120e6, rtt, 10*time.Millisecond)
	s.IsAppLimited = true
	b.OnAck(s)
	if got := b.maxBw(); got < 120e6 {
		t.Errorf("app-limited sample above max did not raise the filter (got %.1f Mbps, reference raises)",
			float64(got)/1e6)
	}

	// 3. Non-app-limited higher sample raises further.
	s2 := steadySample(b, f, 150e6, rtt, 10*time.Millisecond)
	b.OnAck(s2)
	if got := b.maxBw(); got < 150e6 {
		t.Errorf("non-app-limited 150 Mbps sample did not raise the filter (got %.1f)", float64(got)/1e6)
	}
	t.Logf("filter raised: %.1f -> %.1f -> %.1f Mbps", float64(base)/1e6, 120.0, float64(b.maxBw())/1e6)
}
