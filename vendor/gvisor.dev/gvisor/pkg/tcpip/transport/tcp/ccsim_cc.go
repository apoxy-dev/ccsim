// ccsim patch: pluggable congestion control with delivery-rate sampling,
// sender pacing and a minimal ECN echo path.
//
// This file carries almost all of the ccsim additions to package tcp; the
// edits to upstream files are deliberately tiny and are listed in the ccsim
// README ("gVisor patch surface").
//
// What it provides:
//
//   - RegisterSimCC: registers an external congestion control (ccsim's
//     BBRv3) under a name selectable via tcpip.CongestionControlOption.
//   - ccsimWrapper: wraps every CC (stock or registered) to count loss
//     events / RTOs, deliver per-ACK rate samples (RFC-style delivery rate
//     estimation) and route ECN echo to the CC (with an RFC 3168-ish
//     fallback for CCs that don't understand ECN).
//   - Pacing: a virtual-clock-timer token gate in sender.sendData. The
//     pacing rate is owned by the CC; granularity is one send quantum
//     (min(pacing_rate*1ms, 64KB), at least 2*MSS).
//   - A per-ACK ECE echo on the receive side (set in rcv.go when a CE data
//     segment arrives, cleared when any segment is sent) — per-ACK accuracy
//     like DCTCP/ACE rather than RFC 3168 latching, which is what BBRv3's
//     ECN alpha wants.
//   - SimSenderInfo: a probe snapshot for the ccsim instrumentation layer.

package tcp

import (
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/seqnum"
)

// SimAllowECTTOS, when true, stops SetSockOpt from masking the two ECN bits
// out of the IPv4 TOS byte so simulation endpoints can send ECT(0) traffic.
var SimAllowECTTOS = false

// SimRateSample is a per-ACK delivery rate sample (draft-cheng-iccrg-
// delivery-rate-estimation structure, simplified).
type SimRateSample struct {
	// Now is the monotonic receive time of the ACK.
	Now time.Duration
	// AckedBytes is the payload newly delivered by this ACK (cumulative +
	// newly SACKed).
	AckedBytes int64
	// Delivered is the cumulative delivered byte count.
	Delivered int64
	// DeliveredBytes is the volume delivered by this rate sample
	// (C.delivered - P.delivered).
	DeliveredBytes int64
	// DeliveredCEBytes is the CE-marked subset of DeliveredBytes
	// (C.delivered_ce - P.delivered_ce).
	DeliveredCEBytes int64
	// PriorDelivered is the cumulative delivered count at the transmit
	// time of the segment this rate sample was taken from, or -1 when the
	// ACK produced no sample (pure dup-ACK, ECE-only). CCs use it for
	// packet-timed round detection anchored at send time.
	PriorDelivered int64
	// DeliveryRateBps is the sampled delivery rate, 0 if not measurable.
	DeliveryRateBps int64
	// RTT is the sample RTT (ack time - send time of the sample segment).
	RTT time.Duration
	// IsAckDelayed reports that this ACK appears to cover a receiver-delayed
	// multi-segment acknowledgment. BBR avoids using such a sample to restart
	// an expired ProbeRTT scheduling window.
	IsAckDelayed bool
	// IsRetrans is true when the rate sample is anchored at a retransmitted
	// segment. Linux keeps these delivery samples (they are essential during
	// prolonged recovery) but does not use them as unambiguous RTT samples.
	IsRetrans bool
	// IsAckingTLPRetransmit reports that this ACK covers the sequence repaired
	// by a tail-loss-probe retransmission. BBR keeps the latest delivery-signal
	// filter across this boundary so the pre-tail flight remains observable.
	IsAckingTLPRetransmit bool
	// Interval is the sample interval used for the rate computation.
	Interval time.Duration
	// IsAppLimited marks samples taken while the sender was app-limited.
	IsAppLimited bool
	// InflightBytes is bytes in flight after processing the ACK.
	InflightBytes int64
	// ECE is true if the ACK carried an ECN echo.
	ECE bool
	// RetransSegsCum is the endpoint's cumulative retransmitted segment
	// count (loss proxy).
	RetransSegsCum uint64
	// TxInflight is C.inflight at the transmit time of the segment this
	// rate sample was taken from (the draft's P.tx_in_flight), 0 when the
	// ACK produced no sample.
	TxInflight int64
	// LostBytes is the volume newly marked lost between that transmit and
	// this ACK (the draft's RS.lost = C.lost - P.lost).
	LostBytes int64
	// LostBytesCum is the cumulative marked-lost byte count (C.lost).
	// Loss is counted when RACK or the RFC 6675 fallback first marks a
	// sequence range lost, or when an RTO fires, not when data is retransmitted.
	LostBytesCum int64
	// IsCwndLimited reports whether the most recent send attempt ended
	// with the congestion window fully utilized.
	IsCwndLimited bool
}

// SimSender is the restricted sender handle given to registered CCs.
type SimSender struct{ s *sender }

// MSS returns the sender's maximum payload size.
func (h SimSender) MSS() int { return h.s.MaxPayloadSize }

// CwndPkts returns the congestion window in packets.
func (h SimSender) CwndPkts() int { return h.s.SndCwnd }

// SetCwndPkts sets the congestion window (packets, floor 1). Registered CCs
// apply their own steady-state floor; BBR needs a one-packet RTO restart.
func (h SimSender) SetCwndPkts(c int) {
	if c < 1 {
		c = 1
	}
	h.s.SndCwnd = c
}

// SetPacingRateBps sets the pacing rate (bits/s; 0 disables pacing).
func (h SimSender) SetPacingRateBps(bps int64) {
	if h.s.ccsim.wrap != nil {
		h.s.ccsim.wrap.pacingBps = bps
	}
}

// PacingRateBps returns the current pacing rate.
func (h SimSender) PacingRateBps() int64 {
	if h.s.ccsim.wrap != nil {
		return h.s.ccsim.wrap.pacingBps
	}
	return 0
}

// InflightBytes returns SND.NXT - SND.UNA.
func (h SimSender) InflightBytes() int64 {
	return int64(h.s.SndUna.Size(h.s.SndNxt))
}

// SRTT returns the smoothed RTT estimate.
func (h SimSender) SRTT() time.Duration {
	h.s.rtt.Lock()
	defer h.s.rtt.Unlock()
	return h.s.rtt.TCPRTTState.SRTT
}

// Now returns the stack's monotonic time.
func (h SimSender) Now() time.Duration {
	return h.s.ep.stack.Clock().NowMonotonic().Sub(tcpip.MonotonicTime{})
}

// InRecovery reports whether the sender is in loss recovery.
func (h SimSender) InRecovery() bool { return h.s.inRecovery() }

// LocalPort returns the connection's local port (stable per-flow identity,
// used to derive deterministic per-flow randomness).
func (h SimSender) LocalPort() uint16 { return h.s.ep.ID.LocalPort }

// SetSsthresh sets the sender's slow-start threshold (packets).
func (h SimSender) SetSsthresh(v int) {
	if v < 2 {
		v = 2
	}
	h.s.Ssthresh = v
}

// SimSeed is the scenario seed, set by the harness before stacks are
// created (like SimSynchronousDispatch, it is per-process sim configuration;
// one simulation runs at a time). CC randomness must derive from it.
var SimSeed uint64

// Seed returns the scenario seed for deriving named per-flow sub-streams.
func (h SimSender) Seed() uint64 { return SimSeed }

// SimECNLowLatency is the simulated route's TCP_ECN_LOW capability. The
// harness sets it from queue configuration before creating endpoints.
var SimECNLowLatency bool

// ECNLowLatency reports whether both endpoints provide precise ECE feedback
// and the route is explicitly configured for shallow-threshold ECN.
func (h SimSender) ECNLowLatency() bool { return SimECNLowLatency }

// MarkAppLimited marks packets sent from this ACK onward as application-
// limited until the current delivery bubble has left the pipe. ProbeRTT uses
// this to keep its intentionally low-rate samples out of the bandwidth model.
func (h SimSender) MarkAppLimited() {
	st := &h.s.ccsim
	st.appLimited = true
	st.appLimitedSeq = h.s.SndNxt
}

// SimCC is the extended congestion control interface for registered CCs.
// It embeds the four upstream congestionControl methods plus the ccsim
// extensions.
type SimCC interface {
	HandleLossDetected()
	HandleRTOExpired()
	Update(packetsAcked int, rtt time.Duration)
	PostRecovery()
	// OnAck delivers one rate sample per processed ACK, after upstream ACK
	// processing and before more data is sent.
	OnAck(SimRateSample)
}

// SimCCProbe is the CC-internal state snapshot exported for instrumentation.
type SimCCProbe struct {
	State       int // CC-specific state/phase code
	PacingBps   int64
	BwBps       int64 // filtered bandwidth estimate
	DeliveryBps int64 // latest delivery rate sample
	MinRTT      time.Duration
	InflightHi  int64
	InflightLo  int64
	CycleIdx    int
}

// SimCCWithProbe is implemented by CCs that export internal state.
type SimCCWithProbe interface {
	SimProbe() SimCCProbe
}

// SimCCWithUndo is implemented by simulation CCs that can restore model state
// after the transport's Eifel logic proves a recovery episode spurious.
type SimCCWithUndo interface {
	UndoRecovery()
}

// SimCCWithIdleRestart is implemented by controllers that preserve and adapt
// their model when an application-limited connection starts a new flight from
// an empty pipe. The sender invokes it before applying generic RFC 5681 idle
// cwnd restart behavior.
type SimCCWithIdleRestart interface {
	HandleRestartFromIdle()
}

// SimLossProbeRecovery describes a TLP retransmission that repaired a loss.
// LostBytesCum includes this loss and IsAppLimited describes the original
// transmission, rather than the probe copy.
type SimLossProbeRecovery struct {
	LostBytes    int64
	LostBytesCum int64
	IsAppLimited bool
}

// SimCCWithLossProbeRecovery is implemented by controllers with a dedicated
// response to loss recovered by a tail-loss probe. Google BBRv3 consumes the
// equivalent Linux CA_EVENT_TLP_RECOVERY event.
type SimCCWithLossProbeRecovery interface {
	HandleLossProbeRecovery(SimLossProbeRecovery)
}

var simCCRegistry = map[string]func(SimSender) SimCC{}

// RegisterSimCC registers a congestion control constructor under name.
// Must be called before stacks are created (typically from init).
func RegisterSimCC(name string, f func(SimSender) SimCC) {
	simCCRegistry[name] = f
}

// SimCCRegistered reports whether a sim CC is registered under name.
func SimCCRegistered(name string) bool {
	_, ok := simCCRegistry[name]
	return ok
}

func simRegisteredCCNames() []string {
	names := make([]string, 0, len(simCCRegistry))
	for n := range simCCRegistry {
		names = append(names, n)
	}
	// Deterministic order for the available-CC option string.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// ccsimSenderState is embedded in sender (one added field upstream).
type ccsimSenderState struct {
	wrap *ccsimWrapper

	pacingTimer timer `state:"nosave"`
	delAckTimer timer `state:"nosave"`
	nextSend    tcpip.MonotonicTime

	// Delivery rate estimation state.
	delivered     int64
	deliveredCE   int64
	deliveredTime tcpip.MonotonicTime
	firstSent     tcpip.MonotonicTime
	appLimitedSeq seqnum.Value // app-limited until this sequence is acked
	appLimited    bool
	// idleRestartEligible remembers that the application exhausted its send
	// queue even after the final packet of that limited flight is ACKed.
	idleRestartEligible bool
	// idleRestartNotified prevents repeated TX_START notifications when pacing
	// causes sendData to revisit the same not-yet-acknowledged restart flight.
	idleRestartNotified bool

	// lostCum is the cumulative marked-lost byte count (the draft's C.lost),
	// fed by RACK, the RFC 6675 fallback, TLP recovery, and the RTO path.
	lostCum int64
	// cwndLimited records whether the last sendData pass ended with the
	// window fully used (the draft's C.is_cwnd_limited).
	cwndLimited bool

	// Reusable scratch for ccsimSetPipe: scoreboard snapshot and suffix
	// byte sums (index i holds the total SACKed bytes in ranges[i:]).
	pipeRanges   []header.SACKBlock `state:"nosave"`
	pipeSufBytes []seqnum.Size      `state:"nosave"`
	// rackPipe is the incrementally maintained number of transmissions that
	// RACK still considers in flight. Linux maintains the equivalent from its
	// packets_out/sacked_out/lost_out/retrans_out counters; keeping it here
	// avoids an O(cwnd) RFC 6675 scoreboard walk on every ACK while a flow is
	// in prolonged RACK recovery.
	rackPipe int
	// rackSent is RACK's transmit-time-ordered queue, equivalent to Linux's
	// tsorted_sent_queue. rackLost is the sequence-ordered set of marked
	// repairs awaiting transmission. Both are intrusive so ACK/loss/send
	// updates are O(1) in the common case and hold no extra segment refs.
	rackSentHead *segment `state:"nosave"`
	rackSentTail *segment `state:"nosave"`
	rackLostHead *segment `state:"nosave"`
	rackLostTail *segment `state:"nosave"`

	// Scratch captured by ccsimPreAck for ccsimPostAck.
	scratchAcked     int64
	scratchHasSample bool
	scratchAckingTLP bool
	scratchSeg       struct {
		delivered     int64
		deliveredTime tcpip.MonotonicTime
		firstSent     tcpip.MonotonicTime
		xmitTime      tcpip.MonotonicTime
		appLimited    bool
		isRetrans     bool
		txInflight    int64
		lostAtTx      int64
		deliveredCE   int64
	}

	// recoveryResendPending remembers an initial fast retransmit deferred by
	// pacing so the pacing timer can resume the active recovery walk.
	recoveryResendPending bool
	// rackRecoveryPending remembers that RACK's marked-loss walk stopped at
	// the pacing or cwnd gate. tlpProbePending does the same for a tail-loss
	// probe stopped at pacing.
	rackRecoveryPending bool
	tlpProbePending     bool
	// tlpOrigAppLimited is the app-limited state of the original tail segment;
	// retransmitting it stamps the segment with the probe's current state.
	tlpOrigAppLimited bool
}

// ccsimSegState is embedded in segment (one added field upstream).
type ccsimSegState struct {
	delivered     int64
	deliveredCE   int64
	deliveredTime tcpip.MonotonicTime
	firstSent     tcpip.MonotonicTime
	appLimited    bool
	counted       bool
	// txInflight is C.inflight at this segment's (most recent) transmit,
	// including the segment itself (P.tx_in_flight).
	txInflight int64
	// lostAtTx is the cumulative marked-lost count at transmit (P.lost).
	lostAtTx int64
	// lostCounted is set once this segment has been tallied into lostCum;
	// cleared on (re)transmit so a lost retransmission is counted again.
	lostCounted bool
	// rackPipeCopies is the number of live transmissions of this sequence
	// range. It is normally zero or one, and can briefly be two when a TLP
	// probe retransmits a tail whose original copy is not yet known lost.
	rackPipeCopies int
	rackSentPrev   *segment
	rackSentNext   *segment
	rackSentQueued bool
	rackLostPrev   *segment
	rackLostNext   *segment
	rackLostQueued bool
}

// ccsimWrapper wraps the active CC.
type ccsimWrapper struct {
	s         *sender
	inner     congestionControl
	sim       SimCC // non-nil if inner is a registered SimCC
	pacingBps int64
	latestRTT time.Duration // latest unambiguous, unsmoothed ACK RTT sample

	lossEvents   uint64
	rtoCount     uint64
	idleRestarts uint64
	lastECECut   tcpip.MonotonicTime
	deliveryBps  int64
}

var _ congestionControl = (*ccsimWrapper)(nil)

func (w *ccsimWrapper) HandleLossDetected() {
	w.lossEvents++
	w.inner.HandleLossDetected()
}

func (w *ccsimWrapper) HandleRTOExpired() {
	w.rtoCount++
	// An RTO means everything outstanding and not SACKed is presumed lost
	// (the tcp_enter_loss analog). This runs before the caller expunges
	// the scoreboard, so SACKed ranges are still excluded.
	w.s.ccsimMarkAllLost()
	w.inner.HandleRTOExpired()
}

func (w *ccsimWrapper) Update(packetsAcked int, rtt time.Duration) {
	w.inner.Update(packetsAcked, rtt)
}

func (w *ccsimWrapper) PostRecovery() { w.inner.PostRecovery() }

// ccsimInitCC is called from initCongestionControl to build the CC chain.
func (s *sender) ccsimInitCC(name tcpip.CongestionControlOption, stock congestionControl) congestionControl {
	w := &ccsimWrapper{s: s, inner: stock}
	// Install the wrapper before invoking an external constructor so its
	// SimSender can configure pacing immediately, before the first data burst.
	s.ccsim.wrap = w
	if f, ok := simCCRegistry[string(name)]; ok {
		sim := f(SimSender{s})
		w.inner = sim
		w.sim = sim
	}
	return w
}

// ccsimDelAckTimeout is the delayed-ACK flush deadline.
const ccsimDelAckTimeout = 5 * time.Millisecond

// ccsimMaybeDelayAck implements the delayed-ACK policy used under
// synchronous dispatch: ACK immediately once two or more full segments are
// unacknowledged, otherwise arm the delack timer.
//
// +checklocks:e.mu
func (e *Endpoint) ccsimMaybeDelayAck() {
	unacked := e.snd.MaxSentAck.Size(e.rcv.RcvNxt)
	if int(unacked) >= 2*e.snd.MaxPayloadSize {
		e.snd.ccsim.delAckTimer.disable()
		e.snd.sendAck()
		return
	}
	if !e.snd.ccsim.delAckTimer.enabled() {
		e.snd.ccsim.delAckTimer.enable(ccsimDelAckTimeout)
	}
}

// ccsimInitTimers initializes the pacing timer (called from newSender).
func (s *sender) ccsimInitTimers(ep *Endpoint) {
	s.ccsim.pacingTimer.init(ep.stack.Clock(), timerHandler(ep, func() tcpip.Error {
		if !s.ccsim.pacingTimer.checkExpiration() {
			return nil
		}
		s.ccsimPacingTimerExpired()
		return nil
	}))
	s.ccsim.delAckTimer.init(ep.stack.Clock(), timerHandler(ep, func() tcpip.Error {
		if !s.ccsim.delAckTimer.checkExpiration() {
			return nil
		}
		if ep.rcv != nil && ep.rcv.RcvNxt != s.MaxSentAck {
			s.sendAck()
		}
		return nil
	}))
}

func rackSegmentEnd(seg *segment) seqnum.Value {
	return seg.sequenceNumber.Add(seqnum.Size(seg.payloadSize()))
}

func (s *sender) ccsimRACKUnlinkSent(seg *segment) {
	if !seg.ccsim.rackSentQueued {
		return
	}
	prev, next := seg.ccsim.rackSentPrev, seg.ccsim.rackSentNext
	if prev != nil {
		prev.ccsim.rackSentNext = next
	} else {
		s.ccsim.rackSentHead = next
	}
	if next != nil {
		next.ccsim.rackSentPrev = prev
	} else {
		s.ccsim.rackSentTail = prev
	}
	seg.ccsim.rackSentPrev = nil
	seg.ccsim.rackSentNext = nil
	seg.ccsim.rackSentQueued = false
}

// ccsimRACKInsertSent inserts seg by (last transmit time, ending sequence),
// the RFC 8985 ordering used to break equal-clock-tick ties.
func (s *sender) ccsimRACKInsertSent(seg *segment) {
	if seg.ccsim.rackSentQueued {
		return
	}
	end := rackSegmentEnd(seg)
	prev := s.ccsim.rackSentTail
	for prev != nil && (seg.xmitTime.Before(prev.xmitTime) ||
		(seg.xmitTime == prev.xmitTime && end.LessThan(rackSegmentEnd(prev)))) {
		prev = prev.ccsim.rackSentPrev
	}
	if prev == nil {
		next := s.ccsim.rackSentHead
		seg.ccsim.rackSentNext = next
		if next != nil {
			next.ccsim.rackSentPrev = seg
		} else {
			s.ccsim.rackSentTail = seg
		}
		s.ccsim.rackSentHead = seg
	} else {
		next := prev.ccsim.rackSentNext
		seg.ccsim.rackSentPrev = prev
		seg.ccsim.rackSentNext = next
		prev.ccsim.rackSentNext = seg
		if next != nil {
			next.ccsim.rackSentPrev = seg
		} else {
			s.ccsim.rackSentTail = seg
		}
	}
	seg.ccsim.rackSentQueued = true
}

func (s *sender) ccsimRACKUnlinkLost(seg *segment) {
	if !seg.ccsim.rackLostQueued {
		return
	}
	prev, next := seg.ccsim.rackLostPrev, seg.ccsim.rackLostNext
	if prev != nil {
		prev.ccsim.rackLostNext = next
	} else {
		s.ccsim.rackLostHead = next
	}
	if next != nil {
		next.ccsim.rackLostPrev = prev
	} else {
		s.ccsim.rackLostTail = prev
	}
	seg.ccsim.rackLostPrev = nil
	seg.ccsim.rackLostNext = nil
	seg.ccsim.rackLostQueued = false
}

// ccsimRACKInsertLost keeps marked repairs in TCP sequence order, preserving
// the ordering of the original writeList recovery walk.
func (s *sender) ccsimRACKInsertLost(seg *segment) {
	if seg.ccsim.rackLostQueued {
		return
	}
	prev := s.ccsim.rackLostTail
	for prev != nil && seg.sequenceNumber.LessThan(prev.sequenceNumber) {
		prev = prev.ccsim.rackLostPrev
	}
	if prev == nil {
		next := s.ccsim.rackLostHead
		seg.ccsim.rackLostNext = next
		if next != nil {
			next.ccsim.rackLostPrev = seg
		} else {
			s.ccsim.rackLostTail = seg
		}
		s.ccsim.rackLostHead = seg
	} else {
		next := prev.ccsim.rackLostNext
		seg.ccsim.rackLostPrev = prev
		seg.ccsim.rackLostNext = next
		prev.ccsim.rackLostNext = seg
		if next != nil {
			next.ccsim.rackLostPrev = seg
		} else {
			s.ccsim.rackLostTail = seg
		}
	}
	seg.ccsim.rackLostQueued = true
}

func (s *sender) ccsimRACKTrackTransmit(seg *segment) {
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection == 0 {
		return
	}
	// A retransmission supersedes the prior RACK timestamp and consumes a
	// pending repair. Move the segment to the tail of the transmit-time queue.
	s.ccsimRACKUnlinkLost(seg)
	s.ccsimRACKUnlinkSent(seg)
	s.ccsimRACKInsertSent(seg)
}

// ccsimRACKSplitSegment transfers intrusive queue membership and live packet
// counts when netstack splits an already-transmitted GSO segment.
func (s *sender) ccsimRACKSplitSegment(seg, suffix *segment, oldPayload int) {
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection == 0 {
		return
	}
	wasSent := seg.ccsim.rackSentQueued
	wasLost := seg.ccsim.rackLostQueued
	if wasSent {
		s.ccsimRACKUnlinkSent(seg)
	}
	if wasLost {
		s.ccsimRACKUnlinkLost(seg)
	}
	// clone copied the intrusive links and membership flags; suffix was never
	// actually linked, so clear those copies before inserting it.
	suffix.ccsim.rackSentPrev = nil
	suffix.ccsim.rackSentNext = nil
	suffix.ccsim.rackSentQueued = false
	suffix.ccsim.rackLostPrev = nil
	suffix.ccsim.rackLostNext = nil
	suffix.ccsim.rackLostQueued = false

	copies := seg.ccsim.rackPipeCopies
	suffix.ccsim.rackPipeCopies = 0
	if copies > 0 && oldPayload > 0 {
		oldPackets := (oldPayload-1)/s.MaxPayloadSize + 1
		firstPackets := s.pCount(seg, s.MaxPayloadSize)
		firstCopies := copies * firstPackets / oldPackets
		seg.ccsim.rackPipeCopies = firstCopies
		suffix.ccsim.rackPipeCopies = copies - firstCopies
	}
	if wasSent {
		s.ccsimRACKInsertSent(seg)
		s.ccsimRACKInsertSent(suffix)
	}
	if wasLost {
		s.ccsimRACKInsertLost(seg)
		s.ccsimRACKInsertLost(suffix)
	}
}

// ccsimStampSegment stamps per-segment delivery-rate state at transmit time.
// Called from sendSegment after xmitTime is set.
func (s *sender) ccsimStampSegment(seg *segment) {
	st := &s.ccsim
	now := s.ep.stack.Clock().NowMonotonic()
	if s.SndUna == s.SndNxt || st.firstSent == (tcpip.MonotonicTime{}) {
		// Pipe was empty: restart the send chain.
		st.firstSent = now
		if st.deliveredTime == (tcpip.MonotonicTime{}) {
			st.deliveredTime = now
		}
	}
	seg.ccsim.delivered = st.delivered
	seg.ccsim.deliveredCE = st.deliveredCE
	seg.ccsim.deliveredTime = st.deliveredTime
	seg.ccsim.firstSent = st.firstSent
	seg.ccsim.appLimited = st.appLimited
	seg.ccsim.counted = false
	// P.tx_in_flight: neither counter includes this transmission yet. Under
	// RACK use the incremental Linux-style pipe counter; classic SACK recovery
	// continues to use RFC 6675's SetPipe result in s.Outstanding.
	pipe := s.Outstanding
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection != 0 {
		pipe = st.rackPipe
	}
	seg.ccsim.txInflight = int64(pipe)*int64(s.MaxPayloadSize) + int64(seg.payloadSize())
	seg.ccsim.lostAtTx = st.lostCum
	// A fresh (re)transmission supersedes any earlier lost mark; if this
	// copy is lost too the scoreboard walk counts it again.
	seg.ccsim.lostCounted = false
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection != 0 {
		s.ccsimRACKTrackTransmit(seg)
		packets := s.pCount(seg, s.MaxPayloadSize)
		seg.ccsim.rackPipeCopies += packets
		st.rackPipe += packets
	}
	st.firstSent = now
}

// ccsimRACKAcknowledge removes every live copy of a fully acknowledged
// sequence range from RACK's incremental pipe counter. ccsimPreAck calls this
// before upstream ACK processing can remove the segment from writeList.
func (s *sender) ccsimRACKAcknowledge(seg *segment) {
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection == 0 {
		return
	}
	s.ccsimRACKUnlinkSent(seg)
	s.ccsimRACKUnlinkLost(seg)
	seg.lost = false
	if seg.ccsim.rackPipeCopies == 0 {
		return
	}
	s.ccsim.rackPipe -= seg.ccsim.rackPipeCopies
	seg.ccsim.rackPipeCopies = 0
	if s.ccsim.rackPipe < 0 {
		panic("ccsim: negative RACK pipe after ACK")
	}
}

// ccsimRACKMarkLost removes the most recent live transmission of seg from
// RACK's pipe. A later repair is added back by ccsimStampSegment.
func (s *sender) ccsimRACKMarkLost(seg *segment) {
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection == 0 {
		return
	}
	s.ccsimRACKUnlinkSent(seg)
	if seg.lost && !seg.ccsim.counted {
		s.ccsimRACKInsertLost(seg)
	}
	if seg.ccsim.rackPipeCopies == 0 {
		return
	}
	packets := s.pCount(seg, s.MaxPayloadSize)
	if packets > seg.ccsim.rackPipeCopies {
		packets = seg.ccsim.rackPipeCopies
	}
	seg.ccsim.rackPipeCopies -= packets
	s.ccsim.rackPipe -= packets
	if s.ccsim.rackPipe < 0 {
		panic("ccsim: negative RACK pipe after loss")
	}
}

// ccsimMarkAppLimited is called at the end of sendData: if the sender ran
// out of data with spare cwnd, subsequent samples are app-limited.
func (s *sender) ccsimMarkAppLimited() {
	st := &s.ccsim
	if s.writeNext == nil && s.Outstanding < s.SndCwnd && s.SndUna != s.SndNxt {
		st.appLimited = true
		st.appLimitedSeq = s.SndNxt
		st.idleRestartEligible = true
	}
	st.cwndLimited = s.Outstanding >= s.SndCwnd
}

// ccsimMaybeHandleRestartFromIdle detects the reference's CA_EVENT_TX_START:
// new data is ready, the prior application-limited flight has drained, and the
// registered controller owns idle-restart behavior. It returns true while the
// restart flight is pending so generic TCP does not collapse cwnd to IW.
func (s *sender) ccsimMaybeHandleRestartFromIdle() bool {
	st := &s.ccsim
	w := st.wrap
	if w == nil || w.sim == nil || s.writeNext == nil || s.Outstanding != 0 || !st.idleRestartEligible {
		return false
	}
	h, ok := w.sim.(SimCCWithIdleRestart)
	if !ok {
		return false
	}
	if !st.idleRestartNotified {
		st.idleRestartNotified = true
		// Mark the restarted flight itself app-limited, as Linux does by
		// retaining tp->app_limited until data beyond the old marker is ACKed.
		st.appLimited = true
		st.appLimitedSeq = s.SndNxt
		w.idleRestarts++
		h.HandleRestartFromIdle()
	}
	return true
}

// ccsimPreAck captures delivery information for segments about to be acked
// (cumulatively or by new SACK blocks), before upstream ACK processing
// removes them.
func (s *sender) ccsimPreAck(rcvdSeg *segment) {
	st := &s.ccsim
	st.scratchAcked = 0
	st.scratchHasSample = false
	st.scratchAckingTLP = s.rc.tlpRxtOut && s.rc.tlpHighRxt.LessThanEq(rcvdSeg.ackNumber)
	if s.ccsim.wrap == nil {
		return
	}
	ack := rcvdSeg.ackNumber
	upper := ack
	for _, sb := range rcvdSeg.parsedOptions.SACKBlocks {
		if upper.LessThan(sb.End) {
			upper = sb.End
		}
	}
	for seg := s.writeList.Front(); seg != nil; seg = seg.Next() {
		if !s.isAssignedSequenceNumber(seg) {
			break
		}
		if !seg.sequenceNumber.LessThan(upper) {
			break
		}
		if seg.ccsim.counted {
			continue
		}
		endSeq := seg.sequenceNumber.Add(seqnum.Size(seg.logicalLen()))
		covered := endSeq.LessThanEq(ack)
		if !covered {
			for _, sb := range rcvdSeg.parsedOptions.SACKBlocks {
				if sb.Start.LessThanEq(seg.sequenceNumber) && endSeq.LessThanEq(sb.End) {
					covered = true
					break
				}
			}
		}
		if !covered {
			continue
		}
		s.ccsimRACKAcknowledge(seg)
		seg.ccsim.counted = true
		if seg.payloadSize() == 0 {
			continue
		}
		st.scratchAcked += int64(seg.payloadSize())
		// Prefer the most recently transmitted segment as the rate sample
		// source, including retransmissions. For equal transmit timestamps the
		// later sequence wins, matching Linux tcp_rate_skb_delivered.
		if !st.scratchHasSample || !seg.xmitTime.Before(st.scratchSeg.xmitTime) {
			st.scratchHasSample = true
			st.scratchSeg.delivered = seg.ccsim.delivered
			st.scratchSeg.deliveredTime = seg.ccsim.deliveredTime
			st.scratchSeg.firstSent = seg.ccsim.firstSent
			st.scratchSeg.xmitTime = seg.xmitTime
			st.scratchSeg.appLimited = seg.ccsim.appLimited
			st.scratchSeg.isRetrans = seg.xmitCount > 1
			st.scratchSeg.txInflight = seg.ccsim.txInflight
			st.scratchSeg.lostAtTx = seg.ccsim.lostAtTx
			st.scratchSeg.deliveredCE = seg.ccsim.deliveredCE
		}
	}
}

// ccsimPostAck finalizes the rate sample and delivers CC hooks after
// upstream ACK processing.
func (s *sender) ccsimPostAck(rcvdSeg *segment) {
	st := &s.ccsim
	w := st.wrap
	if w == nil {
		return
	}
	nowMT := s.ep.stack.Clock().NowMonotonic()
	now := nowMT.Sub(tcpip.MonotonicTime{})
	ece := rcvdSeg.flags.Contains(header.TCPFlagEce)

	if st.scratchAcked > 0 {
		st.delivered += st.scratchAcked
		if st.idleRestartNotified {
			st.idleRestartEligible = false
		}
		st.idleRestartNotified = false
		if ece {
			st.deliveredCE += st.scratchAcked
		}
		st.deliveredTime = nowMT
		// Clear app-limited once the limited chain is acked.
		if st.appLimited && st.appLimitedSeq.LessThanEq(rcvdSeg.ackNumber) {
			st.appLimited = false
		}
	}

	sample := SimRateSample{
		Now:            now,
		AckedBytes:     st.scratchAcked,
		Delivered:      st.delivered,
		PriorDelivered: -1,
		// Packets actually in the network, matching Linux's rate_sample
		// prior_in_flight = tcp_packets_in_flight(): s.Outstanding is
		// maintained per send/ack and recomputed as the RFC 6675 pipe
		// during SACK recovery. The raw span SND.NXT-SND.UNA was used
		// first and rejected: under persistent random loss a pinned
		// SND.UNA inflates it by the SACK-hole width, so BBR's
		// ProbeBW:DOWN drain condition never satisfies and the flow
		// wedges in DOWN holding a cwnd-limited standing queue.
		InflightBytes:         int64(s.Outstanding) * int64(s.MaxPayloadSize),
		ECE:                   ece,
		RetransSegsCum:        s.ep.stats.SendErrors.Retransmits.Value(),
		LostBytesCum:          st.lostCum,
		IsCwndLimited:         st.cwndLimited,
		IsAckDelayed:          st.scratchAcked >= 2*int64(s.MaxPayloadSize),
		IsAckingTLPRetransmit: st.scratchAckingTLP,
	}
	if st.scratchHasSample {
		sc := &st.scratchSeg
		sample.DeliveredBytes = st.delivered - sc.delivered
		sample.DeliveredCEBytes = st.deliveredCE - sc.deliveredCE
		sample.TxInflight = sc.txInflight
		sample.LostBytes = st.lostCum - sc.lostAtTx
		sample.PriorDelivered = sc.delivered
		ackElapsed := nowMT.Sub(sc.deliveredTime)
		sendElapsed := sc.xmitTime.Sub(sc.firstSent)
		interval := ackElapsed
		if sendElapsed > interval {
			interval = sendElapsed
		}
		sample.IsAppLimited = sc.appLimited
		sample.IsRetrans = sc.isRetrans
		if !sc.isRetrans {
			sample.RTT = nowMT.Sub(sc.xmitTime)
		}
		// Linux rejects rate intervals shorter than tcp_min_rtt because a
		// spurious retransmission can otherwise manufacture a high sample.
		validInterval := interval > 0
		if s.rc.minRTT > 0 && interval < s.rc.minRTT {
			validInterval = false
		}
		if validInterval {
			sample.Interval = interval
			sample.DeliveryRateBps = (st.delivered - sc.delivered) * 8 *
				int64(time.Second) / int64(interval)
		}
	}
	if sample.RTT > 0 {
		w.latestRTT = sample.RTT
	}
	if sample.DeliveryRateBps > 0 && (!sample.IsAppLimited || sample.DeliveryRateBps > w.deliveryBps) {
		w.deliveryBps = sample.DeliveryRateBps
	}

	if w.sim != nil {
		if st.scratchAcked > 0 || ece || st.scratchHasSample {
			w.sim.OnAck(sample)
		}
	} else if ece && st.scratchAcked > 0 {
		// RFC 3168-style response for stock CCs: at most one
		// window reduction per RTT.
		srtt := SimSender{s}.SRTT()
		if srtt <= 0 {
			srtt = 200 * time.Millisecond
		}
		if nowMT.Sub(w.lastECECut) > srtt {
			w.lastECECut = nowMT
			w.inner.HandleLossDetected()
			if s.SndCwnd > s.Ssthresh {
				s.SndCwnd = s.Ssthresh
			}
			if s.SndCwnd < 2 {
				s.SndCwnd = 2
			}
		}
	}
}

// handleRcvdSegment wraps upstream ACK processing with the ccsim delivery
// rate estimator (the upstream function was renamed to
// handleRcvdSegmentInner; see README patch surface).
//
// +checklocks:s.ep.mu
func (s *sender) handleRcvdSegment(rcvdSeg *segment) {
	s.ccsimPreAck(rcvdSeg)
	s.handleRcvdSegmentInner(rcvdSeg)
	s.ccsimPostAck(rcvdSeg)
}

// ccsimUndoRecovery forwards gVisor's spurious-recovery verdict to simulation
// controllers that keep loss-adapted model state in addition to cwnd.
func (s *sender) ccsimUndoRecovery() {
	if w := s.ccsim.wrap; w != nil && w.sim != nil {
		if u, ok := w.sim.(SimCCWithUndo); ok {
			u.UndoRecovery()
		}
	}
}

// ccsimHandleLossProbeRecovery records the single loss repaired by a TLP and
// forwards the transport event to controllers that model it explicitly. The
// retransmitted segment is still on writeList here, before ACK processing
// removes it.
func (s *sender) ccsimHandleLossProbeRecovery() {
	lost := s.ccsimMarkTLPLost()
	if lost == 0 {
		// A FIN-only probe carries no payload. Congestion control still needs a
		// one-packet signal, expressed in the sender's byte units.
		lost = int64(s.MaxPayloadSize)
	}
	w := s.ccsim.wrap
	if w == nil || w.sim == nil {
		return
	}
	if h, ok := w.sim.(SimCCWithLossProbeRecovery); ok {
		h.HandleLossProbeRecovery(SimLossProbeRecovery{
			LostBytes:    lost,
			LostBytesCum: s.ccsim.lostCum,
			IsAppLimited: s.ccsim.tlpOrigAppLimited,
		})
	}
}

// ccsimPacingTimerExpired resumes whichever send walk pacing suspended.
// Ordinary and RTO transmission uses sendData; RACK, TLP, and classic SACK
// recovery each have their own cursor/selection rules and must resume those
// walks instead.
func (s *sender) ccsimPacingTimerExpired() {
	if s.ccsim.tlpProbePending {
		s.ccsimSendTLPProbe()
		return
	}
	if s.ccsim.recoveryResendPending {
		if !s.FastRecovery.Active {
			s.ccsim.recoveryResendPending = false
			s.ccsim.rackRecoveryPending = false
			s.sendData()
			return
		}
		s.resendSegment()
		if s.ccsim.recoveryResendPending {
			return
		}
	}
	if s.ccsim.rackRecoveryPending {
		if !s.FastRecovery.Active || s.ep.tcpRecovery&tcpip.TCPRACKLossDetection == 0 {
			s.ccsim.rackRecoveryPending = false
			s.sendData()
			return
		}
		s.rc.DoRecovery(nil, false /* fastRetransmit */)
		if s.ccsim.rackRecoveryPending {
			return
		}
		// DoRecovery only walks marked repairs. Revisit ordinary sending so
		// available cwnd can carry new data and arm the next pacing deadline.
		s.sendData()
		return
	}
	if s.FastRecovery.Active && s.ep.SACKPermitted &&
		s.ep.tcpRecovery&tcpip.TCPRACKLossDetection == 0 {
		if sr, ok := s.lr.(*sackRecovery); ok {
			end := s.SndUna.Add(s.SndWnd)
			dataSent := sr.handleSACKRecovery(s.MaxPayloadSize, end)
			s.postXmit(dataSent, true /* shouldScheduleProbe */)
			return
		}
	}
	s.sendData()
}

// ccsimPacingAllows reports whether pacing permits a transmission now; if
// not it arms the pacing timer and the caller must stop sending.
func (s *sender) ccsimPacingAllows() bool {
	w := s.ccsim.wrap
	if w == nil || w.pacingBps <= 0 {
		return true
	}
	now := s.ep.stack.Clock().NowMonotonic()
	if !now.Before(s.ccsim.nextSend) {
		return true
	}
	s.ccsim.pacingTimer.enable(s.ccsim.nextSend.Sub(now))
	return false
}

// ccsimPacingCharge advances the pacing clock by size bytes at the current
// pacing rate, allowing catch-up bursts of one send quantum.
func (s *sender) ccsimPacingCharge(size int) {
	w := s.ccsim.wrap
	if w == nil || w.pacingBps <= 0 || size <= 0 {
		return
	}
	now := s.ep.stack.Clock().NowMonotonic()
	// Send quantum: min(rate * 1ms, 64KB), at least 2 MSS.
	quantum := w.pacingBps / 8 / 1000
	if quantum > 64<<10 {
		quantum = 64 << 10
	}
	if min := int64(2 * s.MaxPayloadSize); quantum < min {
		quantum = min
	}
	qt := time.Duration(quantum * 8 * int64(time.Second) / w.pacingBps)
	floor := now.Add(-qt)
	if s.ccsim.nextSend.Before(floor) {
		s.ccsim.nextSend = floor
	}
	txTime := time.Duration(int64(size) * 8 * int64(time.Second) / w.pacingBps)
	s.ccsim.nextSend = s.ccsim.nextSend.Add(txTime)
}

// ccsimMaybeECE ORs the ECE flag into outgoing segment flags while a CE
// echo is pending (per-ACK echo; see file comment).
func (e *Endpoint) ccsimMaybeECE(flags header.TCPFlags) header.TCPFlags {
	if e.ccsimEchoECE && flags.Contains(header.TCPFlagAck) {
		e.ccsimEchoECE = false
		return flags | header.TCPFlagEce
	}
	return flags
}

// ccsimNoteCE records that a CE-marked data segment arrived (called from
// rcv.go).
func (e *Endpoint) ccsimNoteCE(s *segment) {
	if s.payloadSize() == 0 {
		return
	}
	if s.pkt == nil || s.pkt.NetworkProtocolNumber != header.IPv4ProtocolNumber {
		return
	}
	h := header.IPv4(s.pkt.NetworkHeader().Slice())
	if len(h) < header.IPv4MinimumSize {
		return
	}
	tos, _ := h.TOS()
	if tos&0x3 == 0x3 {
		e.ccsimEchoECE = true
	}
}

// SimInfo is the probe snapshot for one endpoint.
type SimInfo struct {
	CwndPkts      int
	InflightBytes int64
	MSS           int
	RTTSample     time.Duration
	PacingBps     int64
	DeliveryBps   int64
	RetransSegs   uint64
	RTOs          uint64
	LossEvents    uint64
	IdleRestarts  uint64
	HasCCProbe    bool
	CCProbe       SimCCProbe
}

// SimSenderInfo snapshots sender internals for instrumentation. Returns
// false if ep is not a connected TCP endpoint.
func SimSenderInfo(tep tcpip.Endpoint) (SimInfo, bool) {
	ep, ok := tep.(*Endpoint)
	if !ok {
		return SimInfo{}, false
	}
	ep.LockUser()
	defer ep.UnlockUser()
	if ep.snd == nil {
		return SimInfo{}, false
	}
	s := ep.snd
	info := SimInfo{
		CwndPkts:      s.SndCwnd,
		InflightBytes: int64(s.SndUna.Size(s.SndNxt)),
		MSS:           s.MaxPayloadSize,
		RetransSegs:   ep.stats.SendErrors.Retransmits.Value(),
		RTOs:          ep.stats.SendErrors.Timeouts.Value(),
	}
	if w := s.ccsim.wrap; w != nil {
		info.RTTSample = w.latestRTT
		info.PacingBps = w.pacingBps
		info.DeliveryBps = w.deliveryBps
		info.LossEvents = w.lossEvents
		info.IdleRestarts = w.idleRestarts
		if w.rtoCount > info.RTOs {
			info.RTOs = w.rtoCount
		}
		if p, ok := w.inner.(SimCCWithProbe); ok {
			info.HasCCProbe = true
			info.CCProbe = p.SimProbe()
		}
	}
	return info, true
}

// ccsimSetPipe computes the RFC 6675 pipe value exactly as the upstream
// SetPipe loop did (packets, not bytes), but in a single ascending pass.
//
// The upstream loop issued two btree range queries (IsSACKED, IsRangeLost)
// per SMSS-sized chunk of outstanding data — O(cwnd * log ranges) per ACK,
// with IsRangeLost additionally counting scoreboard ranges above every
// chunk. During classic SACK recovery with a large window this dominated
// runtime (87% of a bufferbloat run's CPU). Because the chunk walk ascends in
// sequence space and the scoreboard ranges are disjoint and sorted, one
// snapshot plus a monotonically advancing cursor answers both queries:
//
//   - a chunk is SACKed iff the first range ending past its start contains
//     it (at most one range can overlap any given point);
//   - IsRangeLost's dup-SACK block/byte tallies "above the chunk" are
//     suffix aggregates over the snapshot, precomputed once.
//
// The result is bit-identical to the upstream computation (the scenario
// determinism suite would catch any divergence as a stream mismatch).
//
// +checklocks:s.ep.mu
func (s *sender) ccsimSetPipe() int {
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection != 0 {
		// RFC 8985 RACK has explicit per-transmission delivered/lost state.
		// Linux derives packets_in_flight from incrementally maintained packet
		// counters rather than re-running RFC 6675 SetPipe over the entire
		// retransmit queue for each ACK.
		return s.ccsim.rackPipe
	}

	board := s.ep.scoreboard
	ranges := s.ccsim.pipeRanges[:0]
	board.ranges.Ascend(func(r header.SACKBlock) bool {
		ranges = append(ranges, r)
		return true
	})
	s.ccsim.pipeRanges = ranges
	n := len(ranges)
	suf := s.ccsim.pipeSufBytes
	if cap(suf) < n+1 {
		suf = make([]seqnum.Size, n+1)
	}
	suf = suf[:n+1]
	suf[n] = 0
	for j := n - 1; j >= 0; j-- {
		suf[j] = suf[j+1] + ranges[j].Start.Size(ranges[j].End)
	}
	s.ccsim.pipeSufBytes = suf

	smss := seqnum.Size(board.SMSS())
	// Same expression (and integer types) as upstream IsRangeLost.
	lostBytes := seqnum.Size((nDupAckThreshold - 1) * board.SMSS())
	pipe := 0
	i := 0 // cursor: first range with End > current chunk start
	for s1 := s.writeList.Front(); s1 != nil && s1.payloadSize() != 0 && s.isAssignedSequenceNumber(s1); s1 = s1.Next() {
		// With GSO each segment can be much larger than SMSS. So check the
		// segment in SMSS sized ranges.
		segEnd := s1.sequenceNumber.Add(seqnum.Size(s1.payloadSize()))
		for startSeq := s1.sequenceNumber; startSeq.LessThan(segEnd); startSeq = startSeq.Add(smss) {
			endSeq := startSeq.Add(smss)
			if segEnd.LessThan(endSeq) {
				endSeq = segEnd
			}
			if !s1.sequenceNumber.LessThan(s.SndNxt) {
				break
			}
			for i < n && !startSeq.LessThan(ranges[i].End) {
				i++
			}
			r := header.SACKBlock{Start: startSeq, End: endSeq}
			// IsSACKED: only ranges[i] can overlap startSeq.
			if i < n && ranges[i].Contains(r) {
				continue
			}
			// RFC 8985 RACK marks the most recent transmission of a
			// segment lost independently of RFC 6675's SACK-block-count
			// heuristic. A marked transmission is no longer in flight; when
			// it is retransmitted sendSegment clears lost and HighRxt below
			// accounts for the replacement copy.
			if s1.lost {
				continue
			}
			// SetPipe(): (a) if IsLost(S1) returns false, Pipe++.
			if !ccsimRangeLost(ranges, suf, i, r, lostBytes) {
				pipe++
			} else if s1.xmitCount == 1 && !s1.ccsim.lostCounted {
				// First time the scoreboard implies this segment is
				// lost: tally it at mark time (C.lost). Counted per
				// segment, not per chunk — sim segments are one MSS.
				// Only original transmissions: a retransmitted segment's
				// range stays IsLost until the retransmit is SACKed, and
				// recounting it here would double every loss (a lost
				// retransmission is tallied by the RTO path instead).
				s1.ccsim.lostCounted = true
				s.ccsim.lostCum += int64(s1.payloadSize())
			}
			// SetPipe(): (b) if S1 <= HighRxt, Pipe++.
			if s1.sequenceNumber.LessThanEq(s.FastRecovery.HighRxt) {
				pipe++
			}
		}
	}
	return pipe
}

// ccsimMarkAllLost tallies every sent, un-SACKed, not-yet-counted segment
// into the marked-lost counter. Called on RTO, before the scoreboard is
// expunged.
//
// +checklocks:s.ep.mu
func (s *sender) ccsimMarkAllLost() {
	for seg := s.writeList.Front(); seg != nil && seg.xmitCount != 0; seg = seg.Next() {
		if seg.payloadSize() == 0 || seg.ccsim.lostCounted {
			continue
		}
		if s.ep.SACKPermitted && s.ep.scoreboard.IsSACKED(seg.sackBlock()) {
			continue
		}
		seg.ccsim.lostCounted = true
		s.ccsim.lostCum += int64(seg.payloadSize())
	}
	if s.ep.tcpRecovery&tcpip.TCPRACKLossDetection != 0 {
		// An RTO declares every live transmission no longer in flight. The
		// retransmit walk will add each replacement copy back as it is sent.
		for seg := s.writeList.Front(); seg != nil && seg.xmitCount != 0; seg = seg.Next() {
			seg.ccsim.rackPipeCopies = 0
			seg.ccsim.rackSentPrev = nil
			seg.ccsim.rackSentNext = nil
			seg.ccsim.rackSentQueued = false
			seg.ccsim.rackLostPrev = nil
			seg.ccsim.rackLostNext = nil
			seg.ccsim.rackLostQueued = false
		}
		s.ccsim.rackPipe = 0
		s.ccsim.rackSentHead = nil
		s.ccsim.rackSentTail = nil
		s.ccsim.rackLostHead = nil
		s.ccsim.rackLostTail = nil
	}
}

// ccsimMarkSegmentLost tallies one RACK loss at inference time. Unlike the
// RFC 6675 scoreboard, RACK can also infer that a retransmission was lost, so
// ccsimStampSegment clears lostCounted on every transmission and this helper
// deliberately has no xmit-count restriction.
func (s *sender) ccsimMarkSegmentLost(seg *segment) {
	if seg == nil {
		return
	}
	s.ccsimRACKMarkLost(seg)
	if seg.payloadSize() == 0 || seg.ccsim.lostCounted {
		return
	}
	seg.ccsim.lostCounted = true
	s.ccsim.lostCum += int64(seg.payloadSize())
}

// ccsimMarkTLPLost tallies the tail segment whose retransmission repaired the
// current TLP episode. RACK cannot know whether the original or probe copy was
// lost, but RFC 8985 requires a congestion response for one lost packet.
//
// +checklocks:s.ep.mu
func (s *sender) ccsimMarkTLPLost() int64 {
	for seg := s.writeList.Front(); seg != nil && seg.xmitCount != 0; seg = seg.Next() {
		end := seg.sequenceNumber.Add(seqnum.Size(seg.logicalLen()))
		if end != s.rc.tlpHighRxt || seg.payloadSize() == 0 {
			continue
		}
		lost := int64(seg.payloadSize())
		if !seg.ccsim.lostCounted {
			s.ccsimMarkSegmentLost(seg)
		}
		return lost
	}
	return 0
}

// ccsimRangeLost mirrors SACKScoreboard.IsRangeLost for a chunk r that is
// known not to be fully SACKed, given the sorted snapshot, its suffix byte
// sums and the cursor i (first index with ranges[i].End > r.Start).
//
// Upstream first inspects the last range starting at or below r.Start: if
// it partially overlaps r it bumps r.Start to that range's end, so the
// counting pass starts strictly above it. Ranges below the cursor end at
// or below r.Start and never participate. The early-exit thresholds in the
// upstream counting loop are equivalent to comparing the full suffix
// totals, since both tallies only grow.
func ccsimRangeLost(ranges []header.SACKBlock, suf []seqnum.Size, i int, r header.SACKBlock, lostBytes seqnum.Size) bool {
	n := len(ranges)
	if n == 0 {
		return false
	}
	k := i
	if i < n && !r.Start.LessThan(ranges[i].Start) {
		// ranges[i] starts at or below r.Start and (not being a
		// container) ends inside r: the partial-overlap bump means
		// counting starts at the next range.
		k = i + 1
	}
	if k >= n {
		return false
	}
	return n-k >= nDupAckThreshold || suf[k] >= lostBytes
}
