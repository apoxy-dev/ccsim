// Package bbr implements BBRv3 congestion control per draft-ietf-ccwg-bbr-03
// (July 2025 revision of "BBR Congestion Control"), adapted to the ccsim
// netstack CC interface (tcp.SimCC).
//
// Implemented per draft: the Startup/Drain/ProbeBW(DOWN,CRUISE,REFILL,UP)/
// ProbeRTT state machine with the draft's gains; the windowed max-bandwidth
// filter (two probe cycles); min_rtt filter (10 s window) with ProbeRTT
// refresh every 5 s; inflight_hi/inflight_lo and bw_lo short-term bounds
// with beta = 0.7; 0.85 headroom when cruising below inflight_hi; 2% loss
// threshold aborting PROBE_UP; startup full-pipe detection via bandwidth
// plateau (<25% growth across 3 round trips) or excessive loss/ECN; ECN
// alpha (gain 1/16) with the 1/3 ECN cut factor; pacing with the draft's 1%
// pacing margin.
//
// Deliberate deviations (documented in docs/decisions.md):
//   - Loss is observed via the endpoint's cumulative retransmitted-segment
//     counter rather than per-packet loss marks, so per-round lost-byte
//     counts are approximate (retransmitted segments x MSS).
//   - CE feedback is per-ACK ECE echo (ACE-like) rather than RFC 3168
//     latched ECE; ce fraction per round is computed from bytes acked by
//     ECE-carrying ACKs.
//   - Ack aggregation (extra_acked) modeling is simplified to a fixed
//     2-packet cwnd allowance; the simulated receiver does not batch acks
//     beyond standard delayed acks.
package bbr

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// State codes exported through the probe layer.
const (
	StateStartup = iota
	StateDrain
	StateProbeBWDown
	StateProbeBWCruise
	StateProbeBWRefill
	StateProbeBWUp
	StateProbeRTT
)

// StateName maps state codes to names.
func StateName(s int) string {
	switch s {
	case StateStartup:
		return "Startup"
	case StateDrain:
		return "Drain"
	case StateProbeBWDown:
		return "ProbeBW:DOWN"
	case StateProbeBWCruise:
		return "ProbeBW:CRUISE"
	case StateProbeBWRefill:
		return "ProbeBW:REFILL"
	case StateProbeBWUp:
		return "ProbeBW:UP"
	case StateProbeRTT:
		return "ProbeRTT"
	}
	return "?"
}

// Draft constants (section "Constants" of draft-ietf-ccwg-bbr-03).
const (
	startupPacingGain = 2.77
	startupCwndGain   = 2.0
	drainPacingGain   = 1.0 / 2.77
	probeUpGain       = 1.25
	probeDownGain     = 0.9
	cruiseGain        = 1.0
	probeBWCwndGain   = 2.0
	probeRTTCwndGain  = 0.5

	beta         = 0.7  // loss response multiplier
	headroom     = 0.85 // fraction of inflight_hi usable while cruising
	lossThresh   = 0.02 // loss rate aborting PROBE_UP
	pacingMargin = 0.01 // pace at 99% of modeled bw

	ecnAlphaGain = 1.0 / 16
	ecnFactor    = 1.0 / 3
	ecnThresh    = 0.5

	minRTTFilterLen  = 10 * time.Second
	probeRTTInterval = 5 * time.Second
	probeRTTDuration = 200 * time.Millisecond

	fullBwThresh = 1.25 // <25% growth ...
	fullBwCount  = 3    // ... across 3 rounds => pipe full
	// Excessive loss during startup: this many loss events in one round
	// with loss rate above lossThresh ends startup (draft "full_loss_cnt").
	fullLossCount = 6

	maxBwFilterLen = 2 // max-bw filter window, in probe cycles
)

// Sender is the sender-side surface BBR needs. tcp.SimSender implements it;
// unit tests provide a fake.
type Sender interface {
	MSS() int
	CwndPkts() int
	SetCwndPkts(int)
	SetPacingRateBps(int64)
	InflightBytes() int64
	SRTT() time.Duration
	Now() time.Duration
	InRecovery() bool
	LocalPort() uint16
	SetSsthresh(int)
}

// BBR is one connection's BBRv3 state.
type BBR struct {
	s   Sender
	rng *rand.Rand

	state     int
	stateTime time.Duration // entry time of current state

	// Model: bandwidth.
	maxBwFilter [maxBwFilterLen]int64 // bps, windowed max buckets
	cycleCount  int
	bwLatest    int64 // max delivery rate seen in current round
	bwLo        int64 // short-term bound (math.MaxInt64 = unset)
	fullBw      int64
	fullBwCount int
	filledPipe  bool

	// Model: RTT. minRTT is a windowed minimum: rttBuckets holds per-2.5s
	// sub-window minima so stale low samples expire after ~10s (a pinned
	// historical minimum under a competitor's standing queue starves the
	// cwnd model).
	minRTT        time.Duration
	minRTTStamp   time.Duration // when minRTT last decreased or was refreshed
	rttBuckets    [4]time.Duration
	rttBucketT    time.Duration // start time of current bucket
	probeRTTDone  time.Duration // when ProbeRTT hold completes (0 = not holding)
	probeRTTValid bool

	// Model: inflight bounds (bytes; MaxInt64 = unset).
	inflightHi     int64
	inflightLo     int64
	inflightLatest int64 // max inflight seen this round

	// Round tracking.
	nextRoundDelivered int64
	roundStart         bool
	roundCount         int64

	// Per-round loss/ECN accounting.
	lossInRound     bool
	lossEventsRound int
	lostBytesRound  int64
	prevRetransSegs uint64
	ceBytesRound    int64
	ackedBytesRound int64
	ecnAlpha        float64

	// ProbeBW cycling.
	probeWait     time.Duration // wall time to wait in CRUISE before probing
	cycleStamp    time.Duration // when current cycle started (DOWN entry)
	roundsInPhase int64
	probeUpRounds int64
	probeUpAcks   int64
	ackPhaseIsUp  bool

	// lastFilterAdvance backstops max-bw filter aging (see decisions.md:
	// stale bandwidth would otherwise persist up to two full probe cycles
	// after a rate drop that never causes packet loss).
	lastFilterAdvance time.Duration

	// Latest sample cache for probing/export.
	lastSample tcp.SimRateSample

	idleRestart bool
}

var _ tcp.SimCC = (*BBR)(nil)
var _ tcp.SimCCWithProbe = (*BBR)(nil)

// New creates a BBRv3 instance for one connection.
func New(s Sender) *BBR {
	b := &BBR{
		s:          s,
		rng:        rand.New(rand.NewPCG(uint64(s.LocalPort()), 0xBB3)),
		state:      StateStartup,
		bwLo:       math.MaxInt64,
		inflightHi: math.MaxInt64,
		inflightLo: math.MaxInt64,
		minRTT:     0,
		ecnAlpha:   1, // draft: alpha starts at 1
	}
	b.stateTime = s.Now()
	return b
}

// Register wires BBR into the patched netstack under the name "bbr".
func Register() {
	tcp.RegisterSimCC("bbr", func(h tcp.SimSender) tcp.SimCC { return New(h) })
}

// --- tcp.SimCC interface -------------------------------------------------

// Update is the legacy per-ack cwnd hook; BBR uses OnAck instead.
func (b *BBR) Update(packetsAcked int, rtt time.Duration) {}

// HandleLossDetected is invoked on entry to fast retransmit. Keep ssthresh
// out of the way: netstack recovery sets cwnd from ssthresh, and BBR wants
// cwnd mostly preserved (its loss response happens through bw_lo).
func (b *BBR) HandleLossDetected() {
	b.s.SetSsthresh(b.s.CwndPkts())
}

// HandleRTOExpired resets the model conservatively.
func (b *BBR) HandleRTOExpired() {
	// Draft: on RTO, save cwnd and restart from a conservative state; the
	// netstack sender already collapses cwnd. Reset short-term bounds so
	// the model can rebuild.
	b.bwLo = math.MaxInt64
	b.inflightLo = math.MaxInt64
}

// PostRecovery restores BBR's cwnd after netstack recovery ends.
func (b *BBR) PostRecovery() {
	b.setCwnd()
}

// OnAck processes one delivery rate sample (the heart of BBRv3).
var dbgSamples int

// OnAckDebug counter limits sample logging.
func (b *BBR) OnAck(rs tcp.SimRateSample) {
	if debugBBR && dbgSamples < 80 {
		dbgSamples++
		fmt.Fprintf(os.Stderr, "[smp] t=%7.4f acked=%5d dlv=%8d rate=%6.2fMbps rtt=%5.1fms intv=%6.2fms infl=%6d applim=%v\n",
			rs.Now.Seconds(), rs.AckedBytes, rs.Delivered, float64(rs.DeliveryRateBps)/1e6,
			rs.RTT.Seconds()*1000, rs.Interval.Seconds()*1000, rs.InflightBytes, rs.IsAppLimited)
	}
	b.lastSample = rs
	b.updateRound(rs)
	b.updateLossECN(rs)
	b.updateBwModel(rs)
	b.updateMinRTT(rs)
	b.updateStateMachine(rs)
	b.boundLower()
	b.setPacing()
	b.setCwnd()
}

// SimProbe exports internal state for instrumentation.
func (b *BBR) SimProbe() tcp.SimCCProbe {
	p := tcp.SimCCProbe{
		State:       b.state,
		PacingBps:   int64(b.pacingGain() * float64(b.bw()) * (1 - pacingMargin)),
		BwBps:       b.bw(),
		DeliveryBps: b.lastSample.DeliveryRateBps,
		MinRTT:      b.minRTT,
		CycleIdx:    b.cycleCount,
	}
	if b.inflightHi != math.MaxInt64 {
		p.InflightHi = b.inflightHi
	}
	if b.inflightLo != math.MaxInt64 {
		p.InflightLo = b.inflightLo
	}
	return p
}

// --- model updates --------------------------------------------------------

func (b *BBR) updateRound(rs tcp.SimRateSample) {
	b.roundStart = false
	if rs.Delivered >= b.nextRoundDelivered {
		b.nextRoundDelivered = rs.Delivered + rs.InflightBytes
		b.roundStart = true
		b.roundCount++
		b.roundsInPhase++
	}
}

func (b *BBR) updateLossECN(rs tcp.SimRateSample) {
	if rs.RetransSegsCum > b.prevRetransSegs {
		b.lossInRound = true
		b.lossEventsRound++
		b.lostBytesRound += int64(rs.RetransSegsCum-b.prevRetransSegs) * int64(b.s.MSS())
		b.prevRetransSegs = rs.RetransSegsCum
	}
	b.ackedBytesRound += rs.AckedBytes
	if rs.ECE {
		b.ceBytesRound += rs.AckedBytes
	}
	if rs.InflightBytes > b.inflightLatest {
		b.inflightLatest = rs.InflightBytes
	}
	if b.roundStart && b.ackedBytesRound > 0 {
		// Per-round ECN alpha update (draft: once per round trip).
		ceFrac := float64(b.ceBytesRound) / float64(b.ackedBytesRound)
		// Explicit conversions block FMA fusion (native/wasm parity).
		b.ecnAlpha = float64((1-ecnAlphaGain)*b.ecnAlpha) + float64(ecnAlphaGain*ceFrac)
	}
}

func (b *BBR) updateBwModel(rs tcp.SimRateSample) {
	if rs.DeliveryRateBps <= 0 {
		return
	}
	// App-limited samples only raise the filter (they can't underestimate).
	if !rs.IsAppLimited || rs.DeliveryRateBps > b.maxBw() {
		if rs.DeliveryRateBps > b.bwLatest {
			b.bwLatest = rs.DeliveryRateBps
		}
		if rs.DeliveryRateBps > b.maxBwFilter[b.cycleCount%maxBwFilterLen] {
			b.maxBwFilter[b.cycleCount%maxBwFilterLen] = rs.DeliveryRateBps
		}
	}
}

// advanceMaxBwFilter turns over a max-bw bucket on probe-cycle boundaries.
// Turnover is rate-limited: the draft's window is two probe cycles of 2-3s
// each; under contested rapid cycling this keeps the intended 4-6s of
// bandwidth memory instead of decaying per mini-cycle.
func (b *BBR) advanceMaxBwFilter() {
	if now := b.s.Now(); now-b.lastFilterAdvance >= 2*time.Second {
		b.forceAdvanceMaxBwFilter(now)
	}
}

func (b *BBR) forceAdvanceMaxBwFilter(now time.Duration) {
	b.cycleCount++
	b.maxBwFilter[b.cycleCount%maxBwFilterLen] = 0
	b.lastFilterAdvance = now
}

func min64(a, c int64) int64 {
	if a < c {
		return a
	}
	return c
}

func (b *BBR) maxBw() int64 {
	m := b.maxBwFilter[0]
	if b.maxBwFilter[1] > m {
		m = b.maxBwFilter[1]
	}
	return m
}

// bw is the model bandwidth: windowed max bounded by the short-term bw_lo.
func (b *BBR) bw() int64 {
	bw := b.maxBw()
	if b.bwLo < bw {
		bw = b.bwLo
	}
	return bw
}

func (b *BBR) updateMinRTT(rs tcp.SimRateSample) {
	if rs.RTT <= 0 {
		return
	}
	now := b.s.Now()
	// Rotate the 2.5s sub-window buckets.
	const bucketLen = minRTTFilterLen / 4
	if b.rttBucketT == 0 {
		b.rttBucketT = now
	}
	for now-b.rttBucketT >= bucketLen {
		b.rttBucketT += bucketLen
		copy(b.rttBuckets[:], b.rttBuckets[1:])
		b.rttBuckets[3] = 0
	}
	if b.rttBuckets[3] == 0 || rs.RTT < b.rttBuckets[3] {
		b.rttBuckets[3] = rs.RTT
	}
	min := time.Duration(0)
	for _, v := range b.rttBuckets {
		if v != 0 && (min == 0 || v < min) {
			min = v
		}
	}
	if min != b.minRTT {
		if min < b.minRTT || b.minRTT == 0 {
			// A genuinely lower sample refreshes the ProbeRTT schedule;
			// equal samples deliberately do not (otherwise a constant-RTT
			// path would never confirm its floor via ProbeRTT).
			b.minRTTStamp = now
		}
		b.minRTT = min
	}
}

// bdpBytes computes gain * BDP from the filtered model.
func (b *BBR) bdpBytes(gain float64) int64 {
	if b.minRTT == 0 || b.bw() == 0 {
		// No model yet: fall back to a generous initial window.
		return int64(gain * float64(10*b.s.MSS()))
	}
	bdp := float64(b.bw()) / 8 * b.minRTT.Seconds()
	return int64(gain * bdp)
}

// --- state machine --------------------------------------------------------

func (b *BBR) updateStateMachine(rs tcp.SimRateSample) {
	now := b.s.Now()

	// Startup exit checks (once per round).
	if b.state == StateStartup {
		if b.roundStart {
			b.checkFullPipe(rs)
		}
		if b.filledPipe {
			b.enter(StateDrain, now)
		}
	}

	if b.state == StateDrain {
		if rs.InflightBytes <= b.bdpBytes(1.0) {
			b.startProbeBWDown(now)
		}
	}

	// ProbeRTT scheduling: applies in all states except ProbeRTT itself
	// (and not before we have a min_rtt sample).
	if b.state != StateProbeRTT && b.minRTT != 0 &&
		now-b.minRTTStamp > probeRTTInterval && !b.idleRestart {
		b.enter(StateProbeRTT, now)
		b.probeRTTDone = 0
	}

	// Time-bounded filter aging: in steady ProbeBW cruising, expire a
	// max-bw bucket at least once per second so a bandwidth drop that
	// never causes loss (cwnd-capped inflight fitting the queue) is
	// forgotten within ~2 s instead of two full probe cycles.
	if (b.state == StateProbeBWCruise || b.state == StateProbeBWDown) &&
		now-b.lastFilterAdvance > time.Second {
		b.forceAdvanceMaxBwFilter(now)
	}

	switch b.state {
	case StateProbeBWDown:
		// Leave DOWN once inflight is at/below the target with headroom.
		if rs.InflightBytes <= b.inflightWithHeadroom() &&
			rs.InflightBytes <= b.bdpBytes(1.0) {
			b.enter(StateProbeBWCruise, now)
		} else if b.timeToProbeBW(now) {
			// A competing flow's standing queue can make the drain target
			// unreachable; go probe anyway when the wait expires (as in
			// the reference implementation).
			b.enter(StateProbeBWRefill, now)
			b.bwLo = math.MaxInt64
			b.inflightLo = math.MaxInt64
			b.roundsInPhase = 0
		}
	case StateProbeBWCruise:
		// Excess-queue drain: if the bandwidth model dropped (e.g. after a
		// rate step) the inflight built at the old rate must be depleted;
		// DOWN's 0.9 gain does that. DOWN cannot deadlock here: it exits
		// to REFILL when the probe timer expires.
		if b.minRTT != 0 && rs.InflightBytes > b.bdpBytes(1.5) {
			b.startProbeBWDown(now)
			break
		}
		if b.timeToProbeBW(now) {
			b.enter(StateProbeBWRefill, now)
			// Release short-term bounds so the probe can fill the pipe.
			b.bwLo = math.MaxInt64
			b.inflightLo = math.MaxInt64
			b.roundsInPhase = 0
		}
	case StateProbeBWRefill:
		if b.roundStart && b.roundsInPhase >= 1 {
			b.enter(StateProbeBWUp, now)
			b.probeUpRounds = 0
			b.ackPhaseIsUp = true
		}
	case StateProbeBWUp:
		b.probeInflightHiUpward(rs)
		if b.isInflightTooHigh(rs) {
			b.handleInflightTooHigh(rs)
			b.startProbeBWDown(now)
		} else if b.roundStart {
			b.probeUpRounds++
			// Keep probing until the loss/ECN gate trips or the probe has
			// clearly run its course: inflight held at the 1.25x target
			// for a round, or a bounded number of rounds elapsed.
			if b.probeUpRounds >= 2 && rs.InflightBytes >= b.bdpBytes(probeUpGain) {
				b.startProbeBWDown(now)
			} else if b.probeUpRounds >= 8 {
				b.startProbeBWDown(now)
			}
		}
	case StateProbeRTT:
		cap := b.probeRTTCwndBytes()
		if b.probeRTTDone == 0 && rs.InflightBytes <= cap {
			// Inflight reached the ProbeRTT cap: hold for the duration.
			b.probeRTTDone = now + probeRTTDuration
			b.nextRoundDelivered = rs.Delivered + rs.InflightBytes
		}
		if b.probeRTTDone != 0 && now >= b.probeRTTDone {
			// The hold sampled RTT with our own queue drained; the windowed
			// filter has absorbed those samples. Reschedule the next probe.
			b.minRTTStamp = now
			b.exitProbeRTT(now)
		}
	}

	// Loss response outside PROBE_UP/REFILL (short-term model bounds).
	if b.roundStart {
		b.adaptLowerBounds()
		// Reset per-round accounting.
		b.lossInRound = false
		b.lossEventsRound = 0
		b.lostBytesRound = 0
		b.ceBytesRound = 0
		b.ackedBytesRound = 0
		b.bwLatest = 0
		b.inflightLatest = 0
	}
}

var debugBBR = os.Getenv("CCSIM_BBR_DEBUG") != ""

func (b *BBR) enter(state int, now time.Duration) {
	if debugBBR {
		fmt.Fprintf(os.Stderr, "[bbr %d] t=%8.3fs %s -> %s bw=%.2fMbps maxbw=%.2f bwlo=%.2f minrtt=%.1fms cwnd=%d infl=%d fullbw=%.2f cnt=%d\n",
			b.s.LocalPort(), now.Seconds(), StateName(b.state), StateName(state),
			float64(b.bw())/1e6, float64(b.maxBw())/1e6, float64(min64(b.bwLo, 1<<60))/1e6,
			b.minRTT.Seconds()*1000, b.s.CwndPkts(), b.s.InflightBytes(), float64(b.fullBw)/1e6, b.fullBwCount)
	}
	b.state = state
	b.stateTime = now
	b.roundsInPhase = 0
}

func (b *BBR) startProbeBWDown(now time.Duration) {
	b.advanceMaxBwFilter()
	b.enter(StateProbeBWDown, now)
	b.cycleStamp = now
	// Wall-clock probe interval: 2-3 s (deterministic per-flow stream).
	b.probeWait = 2*time.Second + time.Duration(b.rng.Int64N(int64(time.Second)))
	b.ackPhaseIsUp = false
}

func (b *BBR) exitProbeRTT(now time.Duration) {
	b.bwLo = math.MaxInt64
	b.inflightLo = math.MaxInt64
	if b.filledPipe {
		b.startProbeBWDown(now)
		b.enter(StateProbeBWCruise, now)
	} else {
		b.enter(StateStartup, now)
	}
}

func (b *BBR) checkFullPipe(rs tcp.SimRateSample) {
	if b.filledPipe || rs.IsAppLimited {
		return
	}
	// Excessive loss or ECN also ends startup (draft: full pipe due to
	// loss/ECN).
	if b.lossInRound && b.lossEventsRound >= fullLossCount &&
		b.lostBytesRound > int64(lossThresh*float64(rs.InflightBytes)) {
		b.filledPipe = true
		return
	}
	if b.ackedBytesRound > 0 &&
		float64(b.ceBytesRound)/float64(b.ackedBytesRound) > ecnThresh {
		b.filledPipe = true
		return
	}
	if b.maxBw() >= int64(float64(b.fullBw)*fullBwThresh) {
		b.fullBw = b.maxBw()
		b.fullBwCount = 0
		return
	}
	b.fullBwCount++
	if b.fullBwCount >= fullBwCount {
		b.filledPipe = true
	}
}

func (b *BBR) timeToProbeBW(now time.Duration) bool {
	if now-b.cycleStamp > b.probeWait {
		return true
	}
	// Reno coexistence: probe after about as many rounds as Reno needs to
	// grow one BDP (bounded at 63).
	inflightPkts := b.bdpBytes(1.0) / int64(b.s.MSS())
	renoRounds := inflightPkts
	if renoRounds > 63 {
		renoRounds = 63
	}
	return b.roundsInPhase >= renoRounds && renoRounds > 0
}

// isInflightTooHigh implements the draft's loss/ECN gate for probing.
func (b *BBR) isInflightTooHigh(rs tcp.SimRateSample) bool {
	if rs.IsAppLimited {
		return false
	}
	if b.lossEventsRound > 0 && rs.InflightBytes > 0 &&
		b.lostBytesRound > int64(lossThresh*float64(rs.InflightBytes)) {
		return true
	}
	if b.ackedBytesRound > 0 &&
		float64(b.ceBytesRound)/float64(b.ackedBytesRound) > ecnThresh {
		return true
	}
	return false
}

func (b *BBR) handleInflightTooHigh(rs tcp.SimRateSample) {
	target := float64(b.bdpBytes(1.0))
	hi := int64(math.Max(float64(rs.InflightBytes), beta*target))
	if hi < b.inflightHi {
		b.inflightHi = hi
	}
}

// probeInflightHiUpward raises inflight_hi while probing without loss.
func (b *BBR) probeInflightHiUpward(rs tcp.SimRateSample) {
	if b.inflightHi == math.MaxInt64 {
		return
	}
	b.probeUpAcks += rs.AckedBytes
	// Grow inflight_hi by one MSS per inflight_hi bytes acked (draft's
	// slow-then-fast growth simplified to linear per-round growth).
	if b.probeUpAcks >= b.inflightHi/int64(b.s.MSS()) {
		b.probeUpAcks = 0
		b.inflightHi += int64(b.s.MSS())
	}
}

// adaptLowerBounds applies the once-per-round loss/ECN cuts to the
// short-term model bounds (never while probing).
func (b *BBR) adaptLowerBounds() {
	if b.state == StateProbeBWRefill || b.state == StateProbeBWUp {
		return
	}
	ceFrac := 0.0
	if b.ackedBytesRound > 0 {
		ceFrac = float64(b.ceBytesRound) / float64(b.ackedBytesRound)
	}
	ecnCut := b.ecnAlpha > 0 && ceFrac > 0

	if b.lossInRound {
		if b.bwLo == math.MaxInt64 {
			b.bwLo = b.maxBw()
		}
		latest := b.bwLatest
		cut := int64(beta * float64(b.bwLo))
		if latest > cut {
			b.bwLo = latest
		} else {
			b.bwLo = cut
		}
		if b.inflightLo == math.MaxInt64 {
			b.inflightLo = b.bdpBytes(1.0)
		}
		latestIn := b.inflightLatest
		cutIn := int64(beta * float64(b.inflightLo))
		if latestIn > cutIn {
			b.inflightLo = latestIn
		} else {
			b.inflightLo = cutIn
		}
	}
	if ecnCut {
		// Draft ECN response: scale the short-term bounds by
		// (1 - alpha * ecnFactor).
		scale := 1 - float64(b.ecnAlpha*ecnFactor)
		if scale < 1.0/3 {
			scale = 1.0 / 3
		}
		if b.bwLo == math.MaxInt64 {
			b.bwLo = b.maxBw()
		}
		b.bwLo = int64(float64(b.bwLo) * scale)
		if b.inflightLo == math.MaxInt64 {
			b.inflightLo = b.bdpBytes(1.0)
		}
		b.inflightLo = int64(float64(b.inflightLo) * scale)
	}
}

func (b *BBR) boundLower() {
	if b.bwLo != math.MaxInt64 && b.bwLo < int64(float64(b.maxBw())*0.2) {
		// Never let the short-term bound collapse entirely.
		b.bwLo = int64(float64(b.maxBw()) * 0.2)
	}
}

// --- outputs ----------------------------------------------------------------

func (b *BBR) pacingGain() float64 {
	switch b.state {
	case StateStartup:
		return startupPacingGain
	case StateDrain:
		return drainPacingGain
	case StateProbeBWDown:
		return probeDownGain
	case StateProbeBWUp:
		return probeUpGain
	default:
		return cruiseGain
	}
}

func (b *BBR) cwndGain() float64 {
	switch b.state {
	case StateStartup, StateDrain:
		return startupCwndGain
	case StateProbeRTT:
		return probeRTTCwndGain
	default:
		return probeBWCwndGain
	}
}

func (b *BBR) setPacing() {
	bw := b.bw()
	if bw <= 0 {
		return // no model yet: unpaced (startup burst governed by cwnd)
	}
	rate := int64(b.pacingGain() * float64(bw) * (1 - pacingMargin))
	if rate < 8000 {
		rate = 8000
	}
	b.s.SetPacingRateBps(rate)
}

func (b *BBR) inflightWithHeadroom() int64 {
	if b.inflightHi == math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(headroom * float64(b.inflightHi))
}

func (b *BBR) probeRTTCwndBytes() int64 {
	c := b.bdpBytes(probeRTTCwndGain)
	if min := int64(4 * b.s.MSS()); c < min {
		c = min
	}
	return c
}

func (b *BBR) setCwnd() {
	mss := int64(b.s.MSS())
	var target int64
	if b.state == StateProbeRTT {
		target = b.probeRTTCwndBytes()
	} else {
		target = b.bdpBytes(b.cwndGain())
		// Fixed ack-aggregation allowance (see package comment).
		target += 2 * mss
		// Apply bounds: inflight_lo (short-term, loss) and inflight_hi
		// (long-term, probing), with headroom while cruising.
		if b.inflightLo != math.MaxInt64 && target > b.inflightLo &&
			b.state != StateProbeBWRefill && b.state != StateProbeBWUp {
			target = b.inflightLo
		}
		hi := b.inflightHi
		if b.state == StateProbeBWCruise || b.state == StateProbeBWDown {
			hi = b.inflightWithHeadroom()
		}
		if hi != math.MaxInt64 && target > hi {
			target = hi
		}
	}
	pkts := int(target / mss)
	if pkts < 4 {
		pkts = 4
	}
	b.s.SetCwndPkts(pkts)
}
