// Package bbr implements BBRv3 congestion control following the Google BBRv3
// reference at google/bbr v3 commit 90210de4 (whose state machine is described
// by draft-ietf-ccwg-bbr-03), adapted to the ccsim netstack CC interface
// (tcp.SimCC).
//
// Implemented per reference: the Startup/Drain/ProbeBW(DOWN,CRUISE,REFILL,UP)/
// ProbeRTT state machine with fixed-point gains; the windowed max-bandwidth
// filter (two probe cycles); separate 10 s min_rtt and 5 s ProbeRTT scheduling
// filters; measured ACK aggregation; inflight_hi/inflight_lo and bw_lo
// short-term bounds; startup full-pipe detection via bandwidth
// plateau (<25% growth across 3 round trips) or excessive loss/eligible ECN;
// ECN alpha (gain 1/16) with the 1/3 ECN cut factor on explicit low-latency
// routes with min RTT <= 5 ms; pacing with the draft's 1% pacing margin.
//
// Deliberate deviations (documented in docs/decisions.md):
//   - Loss marking comes from RFC 8985 RACK (RFC 6675 fallback plus mark-all
//     on RTO), while the per-lost-packet BBRHandleLostPacket machinery
//     (lost_prefix interpolation) is approximated by evaluating the loss gate
//     against the current rate sample's tx_in_flight.
//   - CE feedback is per-ACK ECE echo (ACE-like) rather than RFC 3168
//     latched ECE; CE volume is computed from bytes acked by ECE-carrying
//     ACKs (both per-round alpha and per-rate-sample threshold signals).
package bbr

import (
	"math"
	"math/rand/v2"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// The BBRv3 state machine (draft-ietf-ccwg-bbr-03):
//
//	+---------+                              +-------+
//	| Startup | -- full pipe: bw plateau, -> | Drain |
//	+---------+    excess loss, or ECN       +-------+
//	                                             |
//	                            inflight <= BDP  |
//	                                             v
//	+----------------------------------------------------------+
//	| ProbeBW                       (entry: DOWN)              |
//	|                                                          |
//	|     +------+                              +--------+     |
//	|     | DOWN | - inflight <= BDP and -----> | CRUISE |     |
//	|     +------+   <= 0.85*inflight_hi        +--------+     |
//	|      ^   |                                    |          |
//	|      |   | probe timer expired                | time to  |
//	|      |   | (drain unreachable)                | probe    |
//	|      |   |                                    v          |
//	|      |   |                                +--------+     |
//	|      |   +------------------------------> | REFILL |     |
//	|      |                                    +--------+     |
//	|      | loss/ECN gate (> 2%)                   |          |
//	|      | or bw growth plateaued                 | 1 round  |
//	|      | (full_bw_now)                          |          |
//	|     +----+                                    |          |
//	|     | UP | <----------------------------------+          |
//	|     +----+                                               |
//	+----------------------------------------------------------+
//
// ProbeRTT is entered from any state when min_rtt has not been refreshed
// for probeRTTInterval (5 s); it caps cwnd at 0.5*BDP, holds for 200 ms
// once inflight is under the cap, then exits to ProbeBW:CRUISE (pipe
// full) or back to Startup (pipe not yet full). The max-bw filter
// advances one round after DOWN starts (ACKS_PROBE_STOPPING); entering
// REFILL releases bw_lo/inflight_lo.
//
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

// Reference constants. Google stores gains in fixed point with BBR_UNIT=256;
// these exact binary fractions preserve the rounded reference values while the
// simulator continues to model rates and volumes in bits and bytes.
const (
	bbrScale             = 256
	bbrUnit              = float64(bbrScale)
	betaNumerator        = 180
	lossThreshNumerator  = 5
	headroomCutNumerator = 38
	startupPacingGain    = 710.0 / bbrUnit // BBR_UNIT*277/100 + 1
	startupCwndGain      = 2.0
	drainPacingGain      = 88.0 / bbrUnit // BBR_UNIT*1000/2885
	probeUpGain          = 1.25
	probeDownGain        = 232.0 / bbrUnit // BBR_UNIT*91/100
	cruiseGain           = 1.0
	probeBWCwndGain      = 2.0
	// probeUpCwndGain: the draft raises cwnd_gain to 2.25 in ProbeBW:UP so
	// the cwnd does not cap the inflight the pacing probe is trying to grow.
	probeUpCwndGain  = 2.25
	probeRTTCwndGain = 0.5

	beta         = float64(betaNumerator) / bbrUnit // 1 - floor(BBR_UNIT*30/100)/BBR_UNIT
	headroom     = float64(bbrScale-headroomCutNumerator) / bbrUnit
	lossThresh   = float64(lossThreshNumerator) / bbrUnit
	pacingMargin = 0.01 // pace at 99% of modeled bw

	ecnAlphaGain = 1.0 / 16
	ecnFactor    = 85.0 / bbrUnit // floor(BBR_UNIT/3)/BBR_UNIT
	ecnThresh    = 0.5
	ecnMaxRTT    = 5 * time.Millisecond
	// startupFullECNCount is the number of consecutive high-CE rounds
	// required to declare the pipe full in startup (reference:
	// bbr_full_ecn_cnt).
	startupFullECNCount = 2

	minRTTFilterLen  = 10 * time.Second
	probeRTTInterval = 5 * time.Second
	probeRTTDuration = 200 * time.Millisecond

	fullBwThresh = 1.25 // <25% growth ...
	fullBwCount  = 3    // ... across 3 rounds => pipe full
	// Excessive loss during startup: this many loss events in one round
	// with loss rate above lossThresh ends startup (draft "full_loss_cnt").
	fullLossCount = 6

	maxBwFilterLen  = 2 // max-bw filter window, in probe cycles
	probeRandRounds = 2 // initial rounds_since_probe jitter: [0, 2)

	extraAckedWinRTTs          = 5
	extraAckedResetThreshPkts  = 1 << 20
	extraAckedMaxInterval      = 100 * time.Millisecond
	simulatorMaxSendQuantum    = 64 << 10
	simulatorMinSendQuantumPkt = 2
)

// ACK phases (draft BBR.ack_phase): which ProbeBW phase the data being
// acknowledged right now was sent in.
const (
	acksInit = iota
	acksRefilling
	acksProbeStarting
	acksProbeFeedback
	acksProbeStopping
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
	Seed() uint64
	ECNLowLatency() bool
	MarkAppLimited()
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
	bwLatest    int64 // max delivery rate seen in current loss round
	bwLo        int64 // short-term bound (math.MaxInt64 = unset)
	fullBw      int64
	fullBwCount int
	// startupEcnRounds counts consecutive startup rounds whose CE fraction
	// exceeded ecnThresh; reset by any low round.
	startupEcnRounds int
	filledPipe       bool
	// pacingBps is the last rate handed to the sender: before the pipe is
	// full the pacing rate only ratchets upward (see setPacing).
	pacingBps int64

	// ACK aggregation. extraAcked is the reference's two-slot max filter;
	// each slot covers five packet-timed rounds in steady state (one round in
	// Startup), retaining the maximum over roughly 5-10 rounds.
	ackEpochStart       time.Duration
	ackEpochAcked       int64
	extraAcked          [2]int64
	extraAckedWinRounds int
	extraAckedWinIdx    int

	// cwnd is the persistent congestion window (bytes), per the draft's
	// BBRSetCwnd: it grows by at most the newly-acked data per ACK and is
	// snapped down to the model target only once the pipe is full; the
	// model bounds apply as caps, never as assignments. priorCwnd is the
	// last known good window (BBRSaveCwnd/BBRRestoreCwnd), restored on
	// loss-recovery and ProbeRTT exit.
	cwnd        int64
	priorCwnd   int64
	initialCwnd int64

	// Model: RTT. probeRTTMin is the minimum over the current 5-second
	// scheduling window; minRTT is the 10-second path-model value. Separate
	// samples and stamps let a natural idle restart refresh ProbeRTT scheduling
	// without prematurely replacing the longer-lived model.
	minRTT            time.Duration
	minRTTStamp       time.Duration
	probeRTTMin       time.Duration
	probeRTTMinStamp  time.Duration
	probeRTTExpired   bool
	probeRTTDone      time.Duration // when ProbeRTT hold completes (0 = not holding)
	probeRTTRoundDone bool          // a full round elapsed at the reduced window

	// Model: inflight bounds (bytes; MaxInt64 = unset).
	inflightHi     int64
	inflightLo     int64
	inflightLatest int64 // max delivered volume per rate sample this loss round

	// Round tracking.
	nextRoundDelivered int64
	roundStart         bool
	roundCount         int64

	// Congestion accounting uses two independent packet-timed clocks, as in
	// tcp_bbr.c. roundStart drives the ECN-alpha/full-bandwidth estimators;
	// lossRoundStart closes a full flight of delivery signals after loss and
	// drives lower-bound adaptation.
	lossRoundDelivered int64
	lossRoundStart     bool
	lossInRound        bool
	ecnInRound         bool
	lossEventsRound    int
	lostBytesRound     int64
	prevLostBytes      int64
	ceBytesRound       int64
	ackedBytesRound    int64
	ecnAlpha           float64
	ecnEligible        bool

	// ProbeBW cycling.
	probeWait        time.Duration // wall time to wait in CRUISE before probing
	cycleStamp       time.Duration // when current cycle started (DOWN entry)
	roundsInPhase    int64
	roundsSinceProbe int64
	// probeUpRounds/probeUpAcks/probeUpCnt implement the draft's
	// exponential inflight_hi growth (BBRRaiseInflightLongtermSlope):
	// inflight_hi grows by one MSS per probeUpCnt bytes acked, and the
	// slope doubles each UP round.
	probeUpRounds int64
	probeUpAcks   int64
	probeUpCnt    int64
	// fullBwNow is the draft's BBR.full_bw_now: the per-probe plateau
	// verdict of the full-bw estimator. filledPipe (full_bw_reached)
	// latches for the connection's lifetime; fullBwNow is reset and re-run
	// during every ProbeBW UP so the probe ends when it stops discovering
	// bandwidth (BBRIsTimeToGoDown).
	fullBwNow bool
	// ackPhase tracks which phase the currently arriving ACKs' data was
	// sent in (draft BBR.ack_phase); its job is timing the max-bw filter
	// advance one round after DOWN starts, when the probe's last samples
	// have all landed.
	ackPhase int
	// bwProbeSamples is the draft's BBR.bw_probe_samples: loss feedback
	// aborts a bandwidth probe at most once per probe. Set on REFILL
	// entry, cleared by handleInflightTooHigh and on CRUISE entry.
	bwProbeSamples bool
	// probeStartDelivered is C.delivered at REFILL entry: a rate sample
	// belongs to the probe (was transmitted during REFILL/UP) iff its
	// PriorDelivered is at or past this mark. Loss among data sent
	// *before* the probe must not abort it (draft: the gate applies to
	// packets "sent in one of the accelerating phases").
	probeStartDelivered int64
	prevProbeTooHigh    bool
	stoppedRiskyProbe   bool

	// Latest sample cache for probing/export.
	lastSample tcp.SimRateSample

	idleRestart bool

	// Recovery undo snapshot. Google BBR saves the model bounds alongside
	// prior_cwnd so an Eifel/DSACK spurious-recovery verdict can restore the
	// complete last-known-good operating point.
	recoverySnapshot bool
	undoBwLo         int64
	undoInflightLo   int64
	undoInflightHi   int64
}

var _ tcp.SimCC = (*BBR)(nil)
var _ tcp.SimCCWithProbe = (*BBR)(nil)
var _ tcp.SimCCWithUndo = (*BBR)(nil)
var _ tcp.SimCCWithIdleRestart = (*BBR)(nil)
var _ tcp.SimCCWithLossProbeRecovery = (*BBR)(nil)

// New creates a BBRv3 instance for one connection.
func New(s Sender) *BBR {
	now := s.Now()
	b := &BBR{
		s: s,
		// Named PCG sub-stream: the scenario seed selects the stream family
		// and the flow's port distinguishes flows within it, so different
		// scenario seeds produce different probe schedules.
		rng:        rand.New(rand.NewPCG(s.Seed(), 0xBB3<<32|uint64(s.LocalPort()))),
		state:      StateStartup,
		bwLo:       math.MaxInt64,
		inflightHi: math.MaxInt64,
		inflightLo: math.MaxInt64,
		probeUpCnt: math.MaxInt64,
		ecnAlpha:   1, // draft: alpha starts at 1
		// Match bbr_init(): wait until data sent after the first delivered
		// byte is ACKed before closing the first loss-signal round.
		lossRoundDelivered: 1,
		ackEpochStart:      now,
	}
	b.stateTime = now
	if srtt := s.SRTT(); srtt > 0 {
		b.minRTT = srtt
		b.minRTTStamp = now
		b.probeRTTMin = srtt
		b.probeRTTMinStamp = now
	}
	b.initialCwnd = int64(s.CwndPkts()) * int64(s.MSS())
	b.cwnd = b.initialCwnd
	b.initPacingRate()
	return b
}

// initPacingRate implements the draft's BBRInitPacingRate: with no
// delivery samples yet, pace the initial window over the handshake SRTT
// (1 ms fallback when none exists) at the startup gain, so even the very
// first flight is not an unpaced line-rate burst.
func (b *BBR) initPacingRate() {
	srtt := b.s.SRTT()
	if srtt <= 0 {
		srtt = time.Millisecond
	}
	nominalBps := float64(8*b.initialCwnd) / srtt.Seconds()
	rate := int64(startupPacingGain * nominalBps)
	b.pacingBps = rate
	b.s.SetPacingRateBps(rate)
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
	b.saveCwnd()
	b.saveRecoveryModel()
	b.s.SetSsthresh(b.s.CwndPkts())
}

// HandleRTOExpired resets the model conservatively.
func (b *BBR) HandleRTOExpired() {
	// Draft BBROnEnterRTO: after the transport declares the old flight lost,
	// C.inflight is zero and C.cwnd becomes C.inflight + 1 packet. The gVisor
	// caller clears Outstanding immediately after this callback, so install the
	// one-packet live cwnd here rather than merely changing BBR's private copy.
	b.saveCwnd()
	b.saveRecoveryModel()
	b.resetFullBW()
	if !b.isProbingBandwidth() && b.inflightLo == math.MaxInt64 {
		b.inflightLo = b.cwnd
		if b.priorCwnd > b.inflightLo {
			b.inflightLo = b.priorCwnd
		}
	}
	b.cwnd = int64(b.s.MSS())
	b.s.SetCwndPkts(1)
}

// PostRecovery restores BBR's cwnd after netstack recovery ends
// (draft BBRRestoreCwnd on loss-recovery exit).
func (b *BBR) PostRecovery() {
	b.restoreCwnd()
	b.applyCwnd()
	b.recoverySnapshot = false
}

// UndoRecovery implements tcp.SimCCWithUndo. It mirrors bbr_undo_cwnd:
// discard congestion evidence from a recovery episode the transport proved
// spurious, restore the saved model bounds, and return to the prior cwnd.
func (b *BBR) UndoRecovery() {
	b.resetFullBW()
	b.lossInRound = false
	if b.recoverySnapshot {
		if b.undoBwLo > b.bwLo {
			b.bwLo = b.undoBwLo
		}
		if b.undoInflightLo > b.inflightLo {
			b.inflightLo = b.undoInflightLo
		}
		if b.undoInflightHi > b.inflightHi {
			b.inflightHi = b.undoInflightHi
		}
	}
	b.restoreCwnd()
	b.applyCwnd()
}

// HandleRestartFromIdle implements Google's CA_EVENT_TX_START handling. The
// transport invokes it immediately before sending a new flight from an empty,
// application-limited pipe.
func (b *BBR) HandleRestartFromIdle() {
	now := b.s.Now()
	b.idleRestart = true
	b.resetAckAggregationEpoch(now)
	if b.inProbeBW() {
		// Do not resume an old UP/DOWN gain after silence; return at the modeled
		// bandwidth without changing the saved cwnd.
		b.setPacingWithGain(cruiseGain)
		return
	}
	if b.state == StateProbeRTT && b.probeRTTDone != 0 && now >= b.probeRTTDone {
		// An idle interval already drained the pipe and satisfied the hold. The
		// reference permits this restart path to exit without another ACK round.
		b.probeRTTMinStamp = now
		b.probeRTTExpired = false
		b.exitProbeRTT(now)
		b.applyCwnd()
	}
}

// HandleLossProbeRecovery implements Google's CA_EVENT_TLP_RECOVERY path.
// A successful TLP proves one copy of the tail packet was lost. Preserve that
// signal even when the ACK cannot produce a normal rate sample, and, for data
// sent during a bandwidth probe, apply the reference's synthetic one-packet
// loss-rate test against the latest delivered flight.
func (b *BBR) HandleLossProbeRecovery(ev tcp.SimLossProbeRecovery) {
	b.noteLoss(ev.LostBytesCum, b.lastSample.Delivered)
	if !b.bwProbeSamples {
		return
	}
	lost := ev.LostBytes
	if lost <= 0 {
		lost = int64(b.s.MSS())
	}
	rs := tcp.SimRateSample{
		LostBytes:    lost,
		TxInflight:   b.inflightLatest + lost,
		IsAppLimited: ev.IsAppLimited,
	}
	if b.lossRateTooHigh(rs) {
		b.handleInflightTooHigh(rs, b.s.Now())
	}
}

func (b *BBR) saveRecoveryModel() {
	if b.recoverySnapshot {
		return
	}
	b.recoverySnapshot = true
	b.undoBwLo = b.bwLo
	b.undoInflightLo = b.inflightLo
	b.undoInflightHi = b.inflightHi
}

// saveCwnd and restoreCwnd implement the draft's BBRSaveCwnd and
// BBRRestoreCwnd: remember the latest cwnd unmodulated by loss recovery
// or ProbeRTT, and restore it on exit from either.
func (b *BBR) saveCwnd() {
	if !b.s.InRecovery() && b.state != StateProbeRTT {
		b.priorCwnd = b.cwnd
	} else if b.cwnd > b.priorCwnd {
		b.priorCwnd = b.cwnd
	}
}

func (b *BBR) restoreCwnd() {
	if b.priorCwnd > b.cwnd {
		b.cwnd = b.priorCwnd
	}
}

// OnAck processes one delivery rate sample (the heart of BBRv3).
func (b *BBR) OnAck(rs tcp.SimRateSample) {
	b.lastSample = rs
	b.updateRound(rs)
	b.updateLossECN(rs)
	b.updateBwModel(rs)
	b.updateAckAggregation(rs)
	b.updateMinRTT(rs)
	b.updateStateMachine(rs)
	b.boundLower()
	b.setPacing()
	b.setCwnd()
}

// SimProbe exports internal state for instrumentation.
func (b *BBR) SimProbe() tcp.SimCCProbe {
	// PacingBps is the rate actually in force: pre-full-pipe it can exceed
	// the instantaneous gain*bw formula because setPacing only ratchets up.
	pacing := b.pacingBps
	if pacing == 0 {
		pacing = int64(b.pacingGain() * float64(b.bw()) * (1 - pacingMargin))
	}
	p := tcp.SimCCProbe{
		State:       b.state,
		PacingBps:   pacing,
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
	// Linux advances the packet-timed round only for a valid delivery-rate
	// sample (rate_sample.interval_us > 0). In particular, ambiguous
	// retransmission samples shorter than min_rtt are invalidated by the
	// transport. Letting their PriorDelivered marker advance this clock can
	// turn several ACKs from one recovery flight into several fictitious RTTs,
	// prematurely ending Startup and ProbeBW:UP.
	if rs.Interval <= 0 {
		return
	}
	// Packet-timed rounds, anchored like the reference implementation: a
	// round ends when a segment SENT at-or-after the round marker is acked
	// (PriorDelivered is the delivered count at the sample's transmit
	// time). Anchoring on ack-time delivered+inflight degenerates when
	// inflight hits zero (every ACK becomes a round).
	if rs.PriorDelivered >= 0 && rs.PriorDelivered >= b.nextRoundDelivered {
		b.nextRoundDelivered = rs.Delivered
		b.roundStart = true
		b.roundCount++
		b.roundsInPhase++
		if b.filledPipe && b.roundsSinceProbe < math.MaxInt64 {
			b.roundsSinceProbe++
		}
	}
}

func (b *BBR) updateLossECN(rs tcp.SimRateSample) {
	b.lossRoundStart = false

	// Loss is observed at mark time (RFC 6675 scoreboard / RTO), not at
	// retransmit time: LostBytesCum is the netstack patch's C.lost. The first
	// mark starts a fresh loss round at the current delivered count, so the
	// lower-bound cut waits for a complete flight of post-mark observations.
	b.noteLoss(rs.LostBytesCum, rs.Delivered)

	// BBRUpdateLatestDeliverySignals is deliberately independent of max_bw
	// filtering and app-limited status. These are the current safe delivery
	// floors used if this loss round ends with congestion.
	validSample := rs.PriorDelivered >= 0 && rs.DeliveryRateBps > 0 && rs.AckedBytes > 0
	deliveredVolume := sampleDeliveredVolume(rs)
	if validSample {
		if rs.DeliveryRateBps > b.bwLatest {
			b.bwLatest = rs.DeliveryRateBps
		}
		if deliveredVolume > b.inflightLatest {
			b.inflightLatest = deliveredVolume
		}
		if rs.PriorDelivered >= b.lossRoundDelivered {
			b.lossRoundDelivered = rs.Delivered
			b.lossRoundStart = true
		}
	}

	// Google enables its ECN control law only for a negotiated precise-ECN,
	// shallow-threshold route whose min RTT is at most 5 ms. The simulator's
	// route capability is explicit on Sender and, like the reference, eligibility
	// latches at a packet-timed round boundary.
	if b.roundStart && !b.ecnEligible && b.s.ECNLowLatency() &&
		b.minRTT > 0 && b.minRTT <= ecnMaxRTT {
		b.ecnEligible = true
	}
	b.ackedBytesRound += rs.AckedBytes
	if b.ecnEligible && rs.ECE {
		b.ceBytesRound += rs.AckedBytes
		b.ecnInRound = true
	}
	if b.roundStart && b.ecnEligible && b.ackedBytesRound > 0 {
		// Per-round ECN alpha update (draft: once per round trip).
		ceFrac := float64(b.ceBytesRound) / float64(b.ackedBytesRound)
		// Explicit conversions block FMA fusion (native/wasm parity).
		b.ecnAlpha = float64((1-ecnAlphaGain)*b.ecnAlpha) + float64(ecnAlphaGain*ceFrac)
	}
}

func (b *BBR) noteLoss(lostBytesCum, delivered int64) {
	if lostBytesCum <= b.prevLostBytes {
		return
	}
	if !b.lossInRound {
		b.lossRoundDelivered = delivered
	}
	b.lossInRound = true
	if b.lossEventsRound < 0xf {
		b.lossEventsRound++
	}
	b.lostBytesRound += lostBytesCum - b.prevLostBytes
	b.prevLostBytes = lostBytesCum
}

func sampleDeliveredVolume(rs tcp.SimRateSample) int64 {
	if rs.DeliveredBytes > 0 {
		return rs.DeliveredBytes
	}
	if rs.PriorDelivered >= 0 {
		// Synthetic/legacy callers may omit DeliveredBytes; the transport
		// always supplies it explicitly.
		return rs.Delivered - rs.PriorDelivered
	}
	return 0
}

// advanceLatestDeliverySignals starts the next independent loss-signal
// round with the boundary sample, matching bbr_advance_latest_delivery_signals.
// An ACK covering a TLP retransmission preserves the prior filter so a tail
// loss does not erase the successfully delivered volume that preceded it.
func (b *BBR) advanceLatestDeliverySignals(rs tcp.SimRateSample) {
	if !b.lossRoundStart || rs.IsAckingTLPRetransmit {
		return
	}
	b.bwLatest = rs.DeliveryRateBps
	b.inflightLatest = sampleDeliveredVolume(rs)
}

func (b *BBR) updateBwModel(rs tcp.SimRateSample) {
	if rs.DeliveryRateBps <= 0 {
		return
	}
	// App-limited samples only raise the filter (they can't underestimate).
	if !rs.IsAppLimited || rs.DeliveryRateBps > b.maxBw() {
		if rs.DeliveryRateBps > b.maxBwFilter[1] {
			b.maxBwFilter[1] = rs.DeliveryRateBps
		}
	}
}

// resetAckAggregationEpoch starts a new interval for measuring how much data
// ACKs deliver beyond the amount expected at the current modeled bandwidth.
func (b *BBR) resetAckAggregationEpoch(now time.Duration) {
	b.ackEpochStart = now
	b.ackEpochAcked = 0
}

// updateAckAggregation implements bbr_update_ack_aggregation in byte units.
// The two max-filter slots each span five packet-timed rounds after Startup;
// before full pipe each spans one round so stale Startup bursts age quickly.
func (b *BBR) updateAckAggregation(rs tcp.SimRateSample) {
	if rs.AckedBytes <= 0 || rs.PriorDelivered < 0 || rs.Interval <= 0 {
		return
	}

	if b.roundStart {
		b.extraAckedWinRounds++
		window := extraAckedWinRTTs
		if !b.filledPipe {
			window = 1
		}
		if b.extraAckedWinRounds >= window {
			b.extraAckedWinRounds = 0
			b.extraAckedWinIdx ^= 1
			b.extraAcked[b.extraAckedWinIdx] = 0
		}
	}

	now := rs.Now
	if now == 0 {
		now = b.s.Now()
	}
	if now < b.ackEpochStart {
		b.resetAckAggregationEpoch(now)
	}
	elapsed := now - b.ackEpochStart
	expected := b.bw() / 8 * int64(elapsed) / int64(time.Second)
	resetThreshold := int64(extraAckedResetThreshPkts) * int64(b.s.MSS())
	if b.ackEpochAcked <= expected || b.ackEpochAcked+rs.AckedBytes >= resetThreshold {
		b.resetAckAggregationEpoch(now)
		expected = 0
	}
	b.ackEpochAcked += rs.AckedBytes
	extra := b.ackEpochAcked - expected
	if extra > b.cwnd {
		extra = b.cwnd
	}
	if extra > b.extraAcked[b.extraAckedWinIdx] {
		b.extraAcked[b.extraAckedWinIdx] = extra
	}
}

func (b *BBR) maxExtraAcked() int64 {
	if b.extraAcked[1] > b.extraAcked[0] {
		return b.extraAcked[1]
	}
	return b.extraAcked[0]
}

func (b *BBR) ackAggregationCwnd() int64 {
	extra := b.maxExtraAcked()
	maxExtra := b.bw() / 8 * int64(extraAckedMaxInterval) / int64(time.Second)
	if extra > maxExtra {
		extra = maxExtra
	}
	return extra
}

// advanceMaxBwFilter turns over the two-bucket max-bw filter exactly at the
// ProbeBW feedback boundary. An empty current bucket does not evict the older
// sample (the reference's app-limited/no-sample guard).
func (b *BBR) advanceMaxBwFilter() {
	if b.maxBwFilter[1] == 0 {
		return
	}
	b.cycleCount++
	b.maxBwFilter[0] = b.maxBwFilter[1]
	b.maxBwFilter[1] = 0
}

func min64(a, c int64) int64 {
	if a < c {
		return a
	}
	return c
}

// fixedMulFloor applies one BBR_UNIT-scaled integer factor with the same
// truncation as tcp_bbr.c's `(u64)value * factor >> BBR_SCALE` operations.
func fixedMulFloor(value int64, numerator int64) int64 {
	return value * numerator / bbrScale
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
	now := rs.Now
	if now == 0 {
		now = b.s.Now()
	}
	b.probeRTTExpired = b.probeRTTMin > 0 && now-b.probeRTTMinStamp > probeRTTInterval
	if rs.RTT <= 0 {
		return
	}

	// The short filter schedules ProbeRTT. Once it expires, the first
	// non-delayed RTT sample starts a fresh five-second window even if that
	// sample is above the old minimum.
	if b.probeRTTMin == 0 || rs.RTT < b.probeRTTMin ||
		(b.probeRTTExpired && !rs.IsAckDelayed) {
		b.probeRTTMin = rs.RTT
		b.probeRTTMinStamp = now
	}

	// The longer path-model sample changes only when the short-window minimum
	// is at least as good, or when the ten-second model itself expires.
	minExpired := b.minRTT > 0 && now-b.minRTTStamp > minRTTFilterLen
	if b.minRTT == 0 || b.probeRTTMin <= b.minRTT || minExpired {
		b.minRTT = b.probeRTTMin
		b.minRTTStamp = b.probeRTTMinStamp
	}
}

// bdpBytesAt computes ceil(gain * BDP) from an explicit bandwidth signal,
// rounded up to a full packet like bbr_bdp. State transitions that drain a
// probe use max_bw; output control uses bw (bounded by bw_lo).
func (b *BBR) bdpBytesAt(gain float64, bw int64) int64 {
	if b.minRTT == 0 || bw == 0 {
		return b.initialCwnd
	}
	bdp := float64(bw) / 8 * b.minRTT.Seconds()
	bytes := float64(gain * bdp)
	mss := float64(b.s.MSS())
	return int64(math.Ceil(bytes/mss)) * int64(b.s.MSS())
}

// bdpBytes computes gain * BDP from the bounded control model.
func (b *BBR) bdpBytes(gain float64) int64 {
	return b.bdpBytesAt(gain, b.bw())
}

// tsoSegsGoal adapts Google's host-offload allowance to the simulator's
// pacing layer. The pacing-rate term budgets roughly one millisecond of data;
// the RTT term gives very-low-latency paths extra burst tolerance, capped at
// the simulator's 64 KiB maximum send quantum.
func (b *BBR) tsoSegsGoal() int64 {
	bytes := b.pacingBps / 8 / 1024
	if b.minRTT > 0 {
		r := uint64(b.minRTT.Microseconds()) >> 9
		if r < 63 {
			bytes += int64(simulatorMaxSendQuantum) >> r
		}
	}
	if bytes > simulatorMaxSendQuantum {
		bytes = simulatorMaxSendQuantum
	}
	segs := bytes / int64(b.s.MSS())
	if segs < simulatorMinSendQuantumPkt {
		segs = simulatorMinSendQuantumPkt
	}
	return segs
}

// quantizationBudget reserves three send quanta for the sender, pacing layer,
// and receiver, enforces MinPipeCwnd, and adds two packets only in ProbeBW:UP
// so low-BDP gain cycling can actually rise above one BDP.
func (b *BBR) quantizationBudget(inflight int64) int64 {
	mss := int64(b.s.MSS())
	if offload := 3 * b.tsoSegsGoal() * mss; inflight < offload {
		inflight = offload
	}
	if minPipe := int64(4) * mss; inflight < minPipe {
		inflight = minPipe
	}
	if b.state == StateProbeBWUp {
		inflight += 2 * mss
	}
	return inflight
}

func (b *BBR) inflightBytesAt(gain float64, bw int64) int64 {
	return b.quantizationBudget(b.bdpBytesAt(gain, bw))
}

func (b *BBR) targetInflightBytes() int64 {
	target := b.inflightBytesAt(1.0, b.bw())
	if b.cwnd < target {
		target = b.cwnd
	}
	return target
}

// --- state machine --------------------------------------------------------

func (b *BBR) updateStateMachine(rs tcp.SimRateSample) {
	now := b.s.Now()

	// Close congestion-signal rounds before evaluating state transitions.
	// This is independent of BBR's ordinary packet-timed round clock: a loss
	// seen midway through a flight must collect one full flight of delivery
	// evidence before cutting the short-term model.
	if b.lossRoundStart {
		b.adaptLowerBounds()
	}

	// Startup exit checks (once per round).
	if b.state == StateStartup {
		b.checkFullPipe(rs)
		if b.filledPipe {
			b.enter(StateDrain, now)
			// Probe-generated congestion must not become a short-term bound as
			// soon as Startup changes its state label to Drain.
			b.resetCongestionSignals()
		}
	}

	if b.state == StateDrain {
		if rs.InflightBytes <= b.inflightBytesAt(1.0, b.maxBw()) {
			b.startProbeBWDown(now)
		}
	}

	// ProbeRTT scheduling uses the independent five-second probe_rtt_min
	// window. An idle restart can refresh that window from a naturally drained
	// path, so it suppresses entry until the first ACK of the restarted flight.
	if b.state != StateProbeRTT && b.minRTT != 0 &&
		b.probeRTTExpired && !b.idleRestart {
		b.saveCwnd()
		b.enter(StateProbeRTT, now)
		b.probeRTTDone = 0
		b.probeRTTRoundDone = false
		b.ackPhase = acksProbeStopping
		b.nextRoundDelivered = rs.Delivered
	}

	// Long-term model adaptation runs on every ACK once the pipe has been
	// filled, in every state (draft BBRUpdateProbeBWCyclePhase calls
	// BBRAdaptLongTermModel before the ProbeBW-state check): probe-caused
	// loss must be reacted to in whatever state its ACKs arrive, and the
	// full-bw estimator must keep running during UP so the probe can end.
	if b.filledPipe {
		b.checkFullBwReached(rs)
		b.adaptLongTermModel(rs, now)
	}

	switch b.state {
	case StateProbeBWDown:
		// Leave DOWN once inflight is at/below the target with headroom.
		if rs.InflightBytes <= b.inflightWithHeadroom() &&
			rs.InflightBytes <= b.inflightBytesAt(1.0, b.maxBw()) {
			b.enterCruise(now)
		} else if b.timeToProbeBW(now) {
			// A competing flow's standing queue can make the drain target
			// unreachable; go probe anyway when the wait expires (as in
			// the reference implementation).
			b.enterRefill(now)
		}
	case StateProbeBWCruise:
		if b.timeToProbeBW(now) {
			b.enterRefill(now)
		}
	case StateProbeBWRefill:
		// After one round of REFILL, start UP.
		if b.roundStart && b.roundsInPhase >= 1 {
			b.startProbeBWUp(now)
		}
	case StateProbeBWUp:
		if b.isTimeToGoDown(rs) {
			b.startProbeBWDown(now)
		}
	case StateProbeRTT:
		// Samples from the deliberately reduced ProbeRTT window are not robust
		// bandwidth observations. Mark the connection so packets sent from this
		// ACK onward produce app-limited delivery-rate samples.
		b.s.MarkAppLimited()
		cap := b.probeRTTCwndBytes()
		if b.probeRTTDone == 0 && rs.InflightBytes <= cap {
			// Inflight reached the ProbeRTT cap: hold for the duration and
			// for at least one packet-timed round at the reduced window
			// (reference: probe_rtt_round_done), so an RTT sample taken with
			// our queue actually drained lands in the filter before exit.
			b.probeRTTDone = now + probeRTTDuration
			b.probeRTTRoundDone = false
			b.nextRoundDelivered = rs.Delivered
		}
		if b.probeRTTDone != 0 && b.roundStart {
			b.probeRTTRoundDone = true
		}
		if b.probeRTTDone != 0 && b.probeRTTRoundDone && now >= b.probeRTTDone {
			// The hold sampled RTT with our own queue drained; the windowed
			// filters have absorbed those samples. Reschedule the next probe.
			b.probeRTTMinStamp = now
			b.probeRTTExpired = false
			b.exitProbeRTT(now)
		}
	}

	if b.lossRoundStart {
		// Congestion and latest-delivery signals use the independent loss
		// round. Seed the next round with this boundary sample at the end of
		// processing, after any state transition reset.
		b.lossInRound = false
		b.ecnInRound = false
		b.lossEventsRound = 0
		b.lostBytesRound = 0
		b.bwLatest = 0
		b.inflightLatest = 0
		b.advanceLatestDeliverySignals(rs)
	}
	if b.roundStart {
		// ECN alpha and Startup ECN exit use the ordinary packet-timed round.
		b.ceBytesRound = 0
		b.ackedBytesRound = 0
	}
	// Match tcp_bbr.c: idle restart remains latched while processing the first
	// ACK, then clears once that ACK delivered data.
	if sampleDeliveredVolume(rs) > 0 {
		b.idleRestart = false
	}
}

func (b *BBR) enter(state int, now time.Duration) {
	b.state = state
	b.stateTime = now
	b.roundsInPhase = 0
}

// enterRefill starts a bandwidth probe: release the short-term model
// bounds so the probe can fill the pipe, and reset per-probe accounting
// (the reference zeroes bw_probe_up_acks in bbr_start_bw_probe_refill;
// stale residue would grow inflight_hi on the first ACK of the probe).
func (b *BBR) enterRefill(now time.Duration) {
	b.enter(StateProbeBWRefill, now)
	b.bwLo = math.MaxInt64
	b.inflightLo = math.MaxInt64
	b.probeUpRounds = 0
	b.probeUpAcks = 0
	b.stoppedRiskyProbe = false
	b.ackPhase = acksRefilling
	b.bwProbeSamples = false
	b.probeStartDelivered = b.lastSample.Delivered
}

// startProbeBWUp begins the accelerating phase (draft BBRStartProbeBW_UP):
// the full-bw estimator is reset and reseeded from the latest delivery
// rate so it can detect, fresh, when *this* probe stops finding bandwidth.
func (b *BBR) startProbeBWUp(now time.Duration) {
	b.ackPhase = acksProbeStarting
	b.bwProbeSamples = true
	b.enter(StateProbeBWUp, now)
	b.resetFullBW()
	b.fullBw = b.lastSample.DeliveryRateBps
	b.raiseInflightHiSlope()
}

func (b *BBR) startProbeBWDown(now time.Duration) {
	b.resetCongestionSignals()
	b.enter(StateProbeBWDown, now)
	b.cycleStamp = now
	// Pick both reference probe-delay bounds: a 0-1 round initial offset and
	// a 2-3 s wall-clock interval, from the deterministic per-flow stream.
	b.roundsSinceProbe = b.rng.Int64N(probeRandRounds)
	b.probeWait = 2*time.Second + time.Duration(b.rng.Int64N(int64(time.Second)))
	// Not growing inflight_hi outside UP (draft: probe_up_cnt = Infinity).
	b.probeUpCnt = math.MaxInt64
	// The max-bw filter advances one round from now, when the last probe
	// samples have landed (ACKS_PROBE_STOPPING in adaptLongTermModel).
	b.ackPhase = acksProbeStopping
}

func (b *BBR) enterCruise(now time.Duration) {
	if b.inflightLo != math.MaxInt64 && b.inflightHi < b.inflightLo {
		b.inflightLo = b.inflightHi
	}
	b.enter(StateProbeBWCruise, now)
}

func (b *BBR) resetCongestionSignals() {
	b.lossInRound = false
	b.ecnInRound = false
	b.lossEventsRound = 0
	b.lostBytesRound = 0
	b.bwLatest = 0
	b.inflightLatest = 0
}

func (b *BBR) exitProbeRTT(now time.Duration) {
	b.restoreCwnd()
	b.bwLo = math.MaxInt64
	b.inflightLo = math.MaxInt64
	if b.filledPipe {
		b.startProbeBWDown(now)
		b.enterCruise(now)
	} else {
		b.enter(StateStartup, now)
	}
}

func (b *BBR) checkFullPipe(rs tcp.SimRateSample) {
	if b.filledPipe {
		return
	}
	// Excessive loss or ECN also ends startup (draft: full pipe due to
	// loss/ECN).
	if b.lossRoundStart && b.lossEventsRound >= fullLossCount &&
		b.s.InRecovery() && b.lossRateTooHigh(rs) {
		b.handleQueueTooHighInStartup()
		return
	}
	if b.roundStart {
		// ECN exit needs sustained evidence: two consecutive high-CE rounds
		// (reference: bbr_full_ecn_cnt = 2), so a single transient marking
		// burst does not end startup early.
		if b.ecnEligible && b.ackedBytesRound > 0 &&
			float64(b.ceBytesRound)/float64(b.ackedBytesRound) >= ecnThresh {
			b.startupEcnRounds++
			if b.startupEcnRounds >= startupFullECNCount {
				b.handleQueueTooHighInStartup()
				return
			}
		} else {
			b.startupEcnRounds = 0
		}
	}
	if rs.IsAppLimited {
		return
	}
	b.checkFullBwReached(rs)
}

// handleQueueTooHighInStartup mirrors bbr_handle_queue_too_high_in_startup:
// congestion ends Startup and establishes the first long-term inflight cap
// from the larger of the model BDP (including our quantization allowance)
// and the latest volume the path demonstrably delivered in one sample.
func (b *BBR) handleQueueTooHighInStartup() {
	b.filledPipe = true
	hi := b.inflightBytesAt(1.0, b.maxBw())
	if b.inflightLatest > hi {
		hi = b.inflightLatest
	}
	b.inflightHi = hi
}

// resetFullBW is the draft's BBRResetFullBW: restart the bandwidth
// plateau estimator.
func (b *BBR) resetFullBW() {
	b.fullBw = 0
	b.fullBwCount = 0
	b.fullBwNow = false
}

// checkFullBwReached is the draft's BBRCheckFullBWReached: once per round
// of non-app-limited samples, declare bandwidth growth plateaued
// (full_bw_now) after fullBwCount rounds of <25% max-bw filter growth.
// filledPipe (full_bw_reached) latches for the connection's lifetime.
func (b *BBR) checkFullBwReached(rs tcp.SimRateSample) {
	if b.fullBwNow || rs.IsAppLimited {
		return
	}
	if rs.DeliveryRateBps >= int64(float64(b.fullBw)*fullBwThresh) {
		// Bandwidth is still growing: reset and re-anchor.
		b.resetFullBW()
		b.fullBw = rs.DeliveryRateBps
		return
	}
	if !b.roundStart {
		return
	}
	b.fullBwCount++
	b.fullBwNow = b.fullBwCount >= fullBwCount
	if b.fullBwNow {
		b.filledPipe = true
	}
}

func (b *BBR) timeToProbeBW(now time.Duration) bool {
	if now-b.cycleStamp > b.probeWait {
		return true
	}
	// Reno coexistence: probe after about as many rounds as Reno needs to
	// grow one BDP (bounded at 63).
	inflightPkts := b.inflightBytesAt(1.0, b.bw()) / int64(b.s.MSS())
	if cwndPkts := b.cwnd / int64(b.s.MSS()); cwndPkts < inflightPkts {
		inflightPkts = cwndPkts
	}
	renoRounds := inflightPkts
	if renoRounds > 63 {
		renoRounds = 63
	}
	return b.roundsSinceProbe >= renoRounds && renoRounds > 0
}

// lossRateTooHigh is the draft's IsInflightTooHigh loss arm:
// RS.lost > RS.tx_in_flight * LossThresh, evaluated against the sample's
// transmit-time inflight and the data marked lost since that transmit.
func (b *BBR) lossRateTooHigh(rs tcp.SimRateSample) bool {
	return rs.TxInflight > 0 &&
		rs.LostBytes > fixedMulFloor(rs.TxInflight, lossThreshNumerator)
}

// ecnTooHigh is the ECN arm of IsInflightTooHigh. Google evaluates the
// delivered-CE fraction of this rate sample, not the accumulated round.
func (b *BBR) ecnTooHigh(rs tcp.SimRateSample) bool {
	return b.ecnEligible && rs.DeliveredBytes > 0 &&
		rs.DeliveredCEBytes > int64(ecnThresh*float64(rs.DeliveredBytes))
}

// inflightTooHigh is the draft's IsInflightTooHigh: is the loss or ECN
// rate of this sample beyond what steady-state operation should see?
// Ungated by probe attribution — a too-high sample must also block the
// safe-sample upward adaptation even when no probe owns it.
func (b *BBR) inflightTooHigh(rs tcp.SimRateSample) bool {
	return b.lossRateTooHigh(rs) || b.ecnTooHigh(rs)
}

// probeTooHigh decides whether a too-high sample belongs to the current
// bandwidth probe and should cut inflight_hi (draft BBRHandleLostPacket:
// only packets sent while bw_probe_samples, at most once per probe; our
// loss attribution additionally requires the sample's data to have been
// transmitted at or after REFILL entry, so residual loss from data sent
// while cruising cannot abort a probe that has produced no feedback).
func (b *BBR) probeTooHigh(rs tcp.SimRateSample) bool {
	if !b.bwProbeSamples {
		return false
	}
	if rs.PriorDelivered < b.probeStartDelivered {
		return false
	}
	return b.ecnTooHigh(rs) || b.lossRateTooHigh(rs)
}

func (b *BBR) handleInflightTooHigh(rs tcp.SimRateSample, now time.Duration) {
	b.prevProbeTooHigh = true
	b.bwProbeSamples = false // react once per bandwidth probe
	// An app-limited sample still ends this probe and marks it risky, but it
	// is not robust evidence for a new inflight_hi. This ordering matches
	// bbr_handle_inflight_too_high.
	if !rs.IsAppLimited {
		// Draft: inflight_longterm = max(RS.tx_in_flight, beta * target) —
		// the operating point when the losing data was sent, not the pipe
		// left after the ACK that revealed the loss.
		infl := rs.TxInflight
		if infl == 0 {
			infl = rs.InflightBytes
		}
		target := fixedMulFloor(b.targetInflightBytes(), betaNumerator)
		hi := infl
		if target > hi {
			hi = target
		}
		b.inflightHi = hi
	}
	if b.state == StateProbeBWUp {
		b.startProbeBWDown(now)
	}
}

func (b *BBR) inProbeBW() bool {
	switch b.state {
	case StateProbeBWDown, StateProbeBWCruise, StateProbeBWRefill, StateProbeBWUp:
		return true
	}
	return false
}

func (b *BBR) isProbingBandwidth() bool {
	return b.state == StateStartup || b.state == StateProbeBWRefill || b.state == StateProbeBWUp
}

// adaptLongTermModel is the draft's BBRAdaptLongTermModel, run on every
// ACK once the pipe is full: track ACK phases to advance the max-bw
// filter one round after DOWN starts (when the probe's samples have all
// landed), cut inflight_hi on probe-attributed loss/ECN in whatever
// state the feedback arrives, and adapt it upward from safe samples.
func (b *BBR) adaptLongTermModel(rs tcp.SimRateSample, now time.Duration) {
	if b.ackPhase == acksProbeStarting && b.roundStart {
		// Data sent while probing is now being acknowledged.
		b.ackPhase = acksProbeFeedback
	}
	if b.ackPhase == acksProbeStopping && b.roundStart {
		// End of samples from the bandwidth probe.
		b.ackPhase = acksInit
		b.bwProbeSamples = false
		if b.inProbeBW() && !rs.IsAppLimited {
			b.advanceMaxBwFilter()
		}
		// A probe that deliberately stopped at the previous inflight_hi and
		// then produced no excessive feedback gets an immediate refill: hold
		// the known-safe level for one flight, then accelerate beyond it.
		if b.inProbeBW() && b.stoppedRiskyProbe && !b.prevProbeTooHigh {
			b.enterRefill(now)
			return
		}
	}
	if b.inflightTooHigh(rs) {
		if b.probeTooHigh(rs) {
			b.handleInflightTooHigh(rs, now)
		}
		return
	}
	// Loss/ECN rate is safe: adjust the upper bound upward.
	if b.inflightHi == math.MaxInt64 {
		return
	}
	if rs.TxInflight > b.inflightHi {
		b.inflightHi = rs.TxInflight
	}
	if b.state == StateProbeBWUp {
		b.probeInflightHiUpward(rs)
	}
}

// isTimeToGoDown is the draft's BBRIsTimeToGoDown: while inflight_hi is
// the binding limit, keep probing and restart the plateau estimator;
// otherwise end UP as soon as bandwidth growth has plateaued.
func (b *BBR) isTimeToGoDown(rs tcp.SimRateSample) bool {
	if b.prevProbeTooHigh && rs.InflightBytes >= b.inflightHi {
		b.stoppedRiskyProbe = true
		b.prevProbeTooHigh = false
		return true
	}
	if rs.IsCwndLimited && b.cwnd >= b.inflightHi {
		// Bandwidth is limited by inflight_hi, not the path; the estimator
		// must not read that artificial plateau as "pipe full".
		b.resetFullBW()
		b.fullBw = rs.DeliveryRateBps
		return false
	}
	if b.fullBwNow {
		b.prevProbeTooHigh = false
		return true
	}
	return false
}

// raiseInflightHiSlope is the draft's BBRRaiseInflightLongtermSlope: the
// growth rate doubles every UP round — inflight_hi grows by
// (1 MSS << round) per cwnd of acked data, so long probes escalate from
// cautious to aggressive like slow start.
func (b *BBR) raiseInflightHiSlope() {
	growth := int64(1) << b.probeUpRounds // packets per cwnd of acks
	if b.probeUpRounds < 30 {
		b.probeUpRounds++
	}
	cnt := b.cwnd / growth
	if cnt < int64(b.s.MSS()) {
		cnt = int64(b.s.MSS())
	}
	b.probeUpCnt = cnt
}

// probeInflightHiUpward raises inflight_hi while probing without loss
// (draft BBRProbeInflightLongtermUpward): one MSS per probeUpCnt bytes
// acked, but only while the window is fully utilized and pressing
// against the bound; otherwise inflight_hi inflates past anything the
// flow has demonstrated.
func (b *BBR) probeInflightHiUpward(rs tcp.SimRateSample) {
	if b.inflightHi == math.MaxInt64 || b.inflightHi <= 0 {
		return
	}
	if !rs.IsCwndLimited || b.cwnd < b.inflightHi {
		return
	}
	b.probeUpAcks += rs.AckedBytes
	if b.probeUpAcks >= b.probeUpCnt {
		delta := b.probeUpAcks / b.probeUpCnt
		b.probeUpAcks -= delta * b.probeUpCnt
		b.inflightHi += delta * int64(b.s.MSS())
	}
	if b.roundStart {
		b.raiseInflightHiSlope()
	}
}

// adaptLowerBounds applies the once-per-round loss/ECN cuts to the
// short-term model bounds (never while probing for bandwidth).
func (b *BBR) adaptLowerBounds() {
	// Startup is a bandwidth probe: both the draft's BBRAdaptLowerBounds
	// pseudocode and the reference's bbr_is_probing_bandwidth() exempt it
	// alongside ProbeBW REFILL/UP. Without the exemption a single startup
	// loss pins bw_lo to a still-ramping round sample, pacing collapses
	// with it, and the resulting artificial delivery plateau trips the
	// full-bw startup exit at a fraction of the real bandwidth.
	if b.isProbingBandwidth() {
		return
	}
	ecnCut := b.ecnEligible && b.ecnAlpha > 0 && b.ecnInRound
	ecnInflightLo := int64(math.MaxInt64)

	// ECN reduces only the volume bound. Compute its candidate from the
	// pre-loss inflight_lo so simultaneous loss and ECN select the lower of
	// two independent responses rather than compounding them.
	if ecnCut {
		if b.inflightLo == math.MaxInt64 {
			b.inflightLo = int64(b.s.CwndPkts()) * int64(b.s.MSS())
		}
		scale := 1 - float64(b.ecnAlpha*ecnFactor)
		ecnInflightLo = int64(float64(b.inflightLo) * scale)
	}

	if b.lossInRound {
		if b.bwLo == math.MaxInt64 {
			b.bwLo = b.maxBw()
		}
		latest := b.bwLatest
		cut := fixedMulFloor(b.bwLo, betaNumerator)
		if latest > cut {
			b.bwLo = latest
		} else {
			b.bwLo = cut
		}
		if b.inflightLo == math.MaxInt64 {
			b.inflightLo = int64(b.s.CwndPkts()) * int64(b.s.MSS())
		}
		latestIn := b.inflightLatest
		cutIn := fixedMulFloor(b.inflightLo, betaNumerator)
		if latestIn > cutIn {
			b.inflightLo = latestIn
		} else {
			b.inflightLo = cutIn
		}
	}
	if ecnInflightLo < b.inflightLo {
		b.inflightLo = ecnInflightLo
	}
}

func (b *BBR) boundLower() {
	// Reference floor is max(1, bw_lo): the short-term bound may collapse
	// arbitrarily far, it just must stay positive. (An earlier 0.2*maxBw
	// floor here silently neutered the beta cuts on >5x rate drops.)
	if b.bwLo != math.MaxInt64 && b.bwLo < 1 {
		b.bwLo = 1
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
	case StateProbeBWUp:
		return probeUpCwndGain
	default:
		return probeBWCwndGain
	}
}

func (b *BBR) setPacing() {
	b.setPacingWithGain(b.pacingGain())
}

func (b *BBR) setPacingWithGain(gain float64) {
	bw := b.bw()
	if bw <= 0 {
		return
	}
	rate := int64(gain * float64(bw) * (1 - pacingMargin))
	if rate < 8000 {
		rate = 8000
	}
	// Before the pipe is full the pacing rate only ratchets upward
	// (reference bbr_set_pacing_rate: a lower rate applies only once
	// bbr_full_bw_reached). A transient dip in the still-growing model
	// must not throttle the ramp it is trying to measure.
	if !b.filledPipe && rate < b.pacingBps {
		return
	}
	b.pacingBps = rate
	b.s.SetPacingRateBps(rate)
}

func (b *BBR) inflightWithHeadroom() int64 {
	if b.inflightHi == math.MaxInt64 {
		return math.MaxInt64
	}
	// Google subtracts the truncated 38/256 headroom cut. This is subtly
	// different from multiplying by 218/256 and truncating the remainder.
	return b.inflightHi - fixedMulFloor(b.inflightHi, headroomCutNumerator)
}

func (b *BBR) probeRTTCwndBytes() int64 {
	c := b.bdpBytes(probeRTTCwndGain)
	if min := int64(4 * b.s.MSS()); c < min {
		c = min
	}
	return c
}

// maxInflightBytes is BBR.max_inflight: cwnd_gain*BDP plus the measured
// extra_acked allowance, then the host/pacing quantization budget.
func (b *BBR) maxInflightBytes() int64 {
	inflight := b.bdpBytes(b.cwndGain()) + b.ackAggregationCwnd()
	return b.quantizationBudget(inflight)
}

// setCwnd implements the draft's BBRSetCwnd: the persistent cwnd grows by
// at most the newly-acked data per ACK, and the model target only acts as
// a cap — and only once the pipe is full. Before that, cwnd never
// decreases (a cold or transiently low model must not cut the window it
// is still trying to measure), except through the explicit bounds applied
// in applyCwnd.
func (b *BBR) setCwnd() {
	acked := b.lastSample.AckedBytes
	maxInflight := b.maxInflightBytes()
	if b.filledPipe {
		b.cwnd += acked
		if b.cwnd > maxInflight {
			b.cwnd = maxInflight
		}
	} else if b.cwnd < maxInflight || b.lastSample.Delivered < b.initialCwnd {
		b.cwnd += acked
	}
	b.applyCwnd()
}

// applyCwnd applies the draft's floors and caps to the persistent cwnd
// (BBRBoundCwndForProbeRTT + BBRBoundCwndForModel) and pushes the result
// to the sender.
func (b *BBR) applyCwnd() {
	mss := int64(b.s.MSS())
	minPipe := 4 * mss
	if b.cwnd < minPipe {
		b.cwnd = minPipe
	}
	if b.state == StateProbeRTT {
		if c := b.probeRTTCwndBytes(); b.cwnd > c {
			b.cwnd = c
		}
	}
	// Volume caps: inflight_hi while probing (DOWN/REFILL/UP), headroom
	// under it while cruising or in ProbeRTT, and inflight_lo always (it
	// is released to infinity on REFILL entry and exempt while probing).
	bound := int64(math.MaxInt64)
	switch b.state {
	case StateProbeBWDown, StateProbeBWRefill, StateProbeBWUp:
		bound = b.inflightHi
	case StateProbeBWCruise, StateProbeRTT:
		bound = b.inflightWithHeadroom()
	}
	if b.inflightLo < bound {
		bound = b.inflightLo
	}
	if bound < minPipe {
		bound = minPipe
	}
	if b.cwnd > bound {
		b.cwnd = bound
	}
	b.s.SetCwndPkts(int(b.cwnd / mss))
}
