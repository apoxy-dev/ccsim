package bbr

import (
	"math"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// fakeSender drives BBR without netstack.
type fakeSender struct {
	now      time.Duration
	mss      int
	cwnd     int
	ssthresh int
	pacing   int64
	inflight int64
	srtt     time.Duration
}

func (f *fakeSender) MSS() int                 { return f.mss }
func (f *fakeSender) CwndPkts() int            { return f.cwnd }
func (f *fakeSender) SetCwndPkts(c int)        { f.cwnd = c }
func (f *fakeSender) SetPacingRateBps(r int64) { f.pacing = r }
func (f *fakeSender) InflightBytes() int64     { return f.inflight }
func (f *fakeSender) SRTT() time.Duration      { return f.srtt }
func (f *fakeSender) Now() time.Duration       { return f.now }
func (f *fakeSender) InRecovery() bool         { return false }
func (f *fakeSender) LocalPort() uint16        { return 40001 }
func (f *fakeSender) SetSsthresh(v int)        { f.ssthresh = v }

const mss = 1448

// trace feeds a synthetic steady flow of ACK samples at the given rate and
// RTT for the given duration, advancing virtual time.
func trace(b *BBR, f *fakeSender, rateBps int64, rtt time.Duration, dur time.Duration) {
	step := 10 * time.Millisecond
	bytesPerStep := rateBps / 8 * int64(step) / int64(time.Second)
	for elapsed := time.Duration(0); elapsed < dur; elapsed += step {
		f.now += step
		f.inflight = rateBps / 8 * int64(rtt) / int64(time.Second)
		b.OnAck(tcp.SimRateSample{
			Now:             f.now,
			AckedBytes:      bytesPerStep,
			Delivered:       b.lastSample.Delivered + bytesPerStep,
			DeliveryRateBps: rateBps,
			RTT:             rtt,
			Interval:        step,
			InflightBytes:   f.inflight,
		})
	}
}

func newTestBBR() (*BBR, *fakeSender) {
	f := &fakeSender{mss: mss, cwnd: 10, srtt: 40 * time.Millisecond}
	return New(f), f
}

func TestStartupExitOnPlateau(t *testing.T) {
	b, f := newTestBBR()
	rtt := 40 * time.Millisecond
	// Growing bandwidth: stays in Startup.
	rate := int64(10e6)
	for i := 0; i < 6; i++ {
		trace(b, f, rate, rtt, rtt)
		rate = rate * 2
	}
	if b.state != StateStartup {
		t.Fatalf("left Startup during growth (state=%s)", StateName(b.state))
	}
	// Plateau: <25%% growth for 3+ rounds => Drain then ProbeBW.
	for i := 0; i < 10; i++ {
		trace(b, f, rate, rtt, rtt)
	}
	if b.state == StateStartup || b.state == StateDrain {
		t.Fatalf("did not exit Startup after plateau (state=%s)", StateName(b.state))
	}
	if !b.filledPipe {
		t.Fatal("filledPipe not set")
	}
}

func TestMaxBwFilterWindow(t *testing.T) {
	b, f := newTestBBR()
	rtt := 20 * time.Millisecond
	trace(b, f, 100e6, rtt, 500*time.Millisecond)
	if got := b.maxBw(); got < 95e6 {
		t.Fatalf("maxBw %.1f Mbps, want ~100", float64(got)/1e6)
	}
	// Rate drops; after two (rate-limited, >=2s apart) filter advances the
	// old sample must be forgotten.
	b.forceAdvanceMaxBwFilter(f.now)
	trace(b, f, 20e6, rtt, 100*time.Millisecond)
	if got := b.maxBw(); got < 95e6 {
		t.Fatalf("one advance should retain the old bucket (got %.1f)", float64(got)/1e6)
	}
	b.forceAdvanceMaxBwFilter(f.now)
	trace(b, f, 20e6, rtt, 100*time.Millisecond)
	if got := b.maxBw(); got > 25e6 {
		t.Fatalf("stale bandwidth survived two advances: %.1f Mbps", float64(got)/1e6)
	}
}

func TestMinRTTWindowExpiry(t *testing.T) {
	b, f := newTestBBR()
	// 20ms RTT initially, then a competitor inflates it to 80ms.
	trace(b, f, 50e6, 20*time.Millisecond, 300*time.Millisecond)
	if b.minRTT != 20*time.Millisecond {
		t.Fatalf("minRTT %v, want 20ms", b.minRTT)
	}
	trace(b, f, 50e6, 80*time.Millisecond, 11*time.Second)
	if b.minRTT < 70*time.Millisecond {
		t.Fatalf("stale min RTT %v survived the 10s window", b.minRTT)
	}
}

func TestProbeRTTScheduling(t *testing.T) {
	b, f := newTestBBR()
	rtt := 30 * time.Millisecond
	// Reach steady state.
	trace(b, f, 50e6, rtt, 2*time.Second)
	if b.state == StateStartup {
		t.Fatal("still in startup after 2s of steady samples")
	}
	// With min RTT never decreasing, a ProbeRTT must occur within ~5s+eps.
	sawProbeRTT := false
	for i := 0; i < 60; i++ {
		trace(b, f, 50e6, rtt, 100*time.Millisecond)
		if b.state == StateProbeRTT {
			sawProbeRTT = true
			// Simulate inflight draining below the ProbeRTT cap.
			f.inflight = 2 * mss
		}
	}
	if !sawProbeRTT {
		t.Fatal("no ProbeRTT within 6s")
	}
	// While holding at low inflight for the ProbeRTT duration, the state
	// must exit back to ProbeBW.
	for i := 0; i < 50 && b.state == StateProbeRTT; i++ {
		f.now += 10 * time.Millisecond
		f.inflight = 2 * mss
		b.OnAck(tcp.SimRateSample{
			Now: f.now, AckedBytes: mss, Delivered: b.lastSample.Delivered + mss,
			DeliveryRateBps: 50e6, RTT: rtt, Interval: 10 * time.Millisecond,
			InflightBytes: f.inflight,
		})
	}
	if b.state == StateProbeRTT {
		t.Fatal("stuck in ProbeRTT")
	}
}

func TestLossResponseMath(t *testing.T) {
	b, f := newTestBBR()
	rtt := 30 * time.Millisecond
	trace(b, f, 80e6, rtt, 2*time.Second)
	if b.state == StateProbeBWRefill || b.state == StateProbeBWUp {
		trace(b, f, 80e6, rtt, time.Second)
	}
	pre := b.bw()
	// One loss round: delivery rate halves, retransmissions observed.
	f.now += 10 * time.Millisecond
	b.OnAck(tcp.SimRateSample{
		Now: f.now, AckedBytes: mss, Delivered: b.lastSample.Delivered + mss,
		DeliveryRateBps: 40e6, RTT: rtt, Interval: 10 * time.Millisecond,
		InflightBytes: f.inflight, RetransSegsCum: 10,
	})
	// Complete the round so adaptLowerBounds runs.
	trace(b, f, 40e6, rtt, 2*rtt)
	if b.bwLo == math.MaxInt64 {
		t.Fatal("bw_lo not set after loss round")
	}
	// bw_lo must respect the beta=0.7 floor against the latest rate.
	if b.bwLo < int64(0.69*float64(pre)) && b.bwLo < 40e6 {
		t.Fatalf("bw_lo %.1f cut below both beta*bw (%.1f) and latest (40)",
			float64(b.bwLo)/1e6, 0.7*float64(pre)/1e6)
	}
	if b.bw() > b.bwLo {
		t.Fatal("model bw not bounded by bw_lo")
	}
}

func TestPacingGainsPerState(t *testing.T) {
	b, _ := newTestBBR()
	cases := []struct {
		state int
		gain  float64
	}{
		{StateStartup, 2.77},
		{StateDrain, 1.0 / 2.77},
		{StateProbeBWDown, 0.9},
		{StateProbeBWCruise, 1.0},
		{StateProbeBWRefill, 1.0},
		{StateProbeBWUp, 1.25},
		{StateProbeRTT, 1.0},
	}
	for _, c := range cases {
		b.state = c.state
		if g := b.pacingGain(); math.Abs(g-c.gain) > 1e-9 {
			t.Errorf("state %s pacing gain %v, want %v", StateName(c.state), g, c.gain)
		}
	}
	b.state = StateProbeRTT
	if g := b.cwndGain(); g != probeRTTCwndGain {
		t.Errorf("ProbeRTT cwnd gain %v, want %v", g, probeRTTCwndGain)
	}
}

func TestInflightTooHighAbortsProbe(t *testing.T) {
	b, f := newTestBBR()
	rtt := 30 * time.Millisecond
	trace(b, f, 80e6, rtt, 3*time.Second)
	// Force UP state with accumulated loss above 2% of inflight.
	b.enter(StateProbeBWUp, f.now)
	b.lossEventsRound = 3
	b.lostBytesRound = int64(0.1 * float64(f.inflight))
	f.now += 10 * time.Millisecond
	b.OnAck(tcp.SimRateSample{
		Now: f.now, AckedBytes: mss, Delivered: b.lastSample.Delivered + mss,
		DeliveryRateBps: 80e6, RTT: rtt, Interval: 10 * time.Millisecond,
		InflightBytes: f.inflight,
	})
	if b.state == StateProbeBWUp {
		t.Fatal("PROBE_UP survived loss above the 2% threshold")
	}
	if b.inflightHi == math.MaxInt64 {
		t.Fatal("inflight_hi not capped after loss abort")
	}
}

func TestExportedProbeState(t *testing.T) {
	b, f := newTestBBR()
	trace(b, f, 50e6, 30*time.Millisecond, time.Second)
	p := b.SimProbe()
	if p.BwBps <= 0 || p.MinRTT <= 0 || p.PacingBps <= 0 {
		t.Fatalf("probe export incomplete: %+v", p)
	}
}
