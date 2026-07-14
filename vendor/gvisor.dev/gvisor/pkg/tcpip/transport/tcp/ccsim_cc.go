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
	// PriorDelivered is the cumulative delivered count at the transmit
	// time of the segment this rate sample was taken from, or -1 when the
	// ACK produced no sample (pure dup-ACK, ECE-only). CCs use it for
	// packet-timed round detection anchored at send time.
	PriorDelivered int64
	// DeliveryRateBps is the sampled delivery rate, 0 if not measurable.
	DeliveryRateBps int64
	// RTT is the sample RTT (ack time - send time of the sample segment).
	RTT time.Duration
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
}

// SimSender is the restricted sender handle given to registered CCs.
type SimSender struct{ s *sender }

// MSS returns the sender's maximum payload size.
func (h SimSender) MSS() int { return h.s.MaxPayloadSize }

// CwndPkts returns the congestion window in packets.
func (h SimSender) CwndPkts() int { return h.s.SndCwnd }

// SetCwndPkts sets the congestion window (packets, floor 2).
func (h SimSender) SetCwndPkts(c int) {
	if c < 2 {
		c = 2
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
	deliveredTime tcpip.MonotonicTime
	firstSent     tcpip.MonotonicTime
	appLimitedSeq seqnum.Value // app-limited until this sequence is acked
	appLimited    bool

	// Reusable scratch for ccsimSetPipe: scoreboard snapshot and suffix
	// byte sums (index i holds the total SACKed bytes in ranges[i:]).
	pipeRanges   []header.SACKBlock `state:"nosave"`
	pipeSufBytes []seqnum.Size      `state:"nosave"`

	// Scratch captured by ccsimPreAck for ccsimPostAck.
	scratchAcked     int64
	scratchHasSample bool
	scratchSeg       struct {
		delivered     int64
		deliveredTime tcpip.MonotonicTime
		firstSent     tcpip.MonotonicTime
		xmitTime      tcpip.MonotonicTime
		appLimited    bool
	}
}

// ccsimSegState is embedded in segment (one added field upstream).
type ccsimSegState struct {
	delivered     int64
	deliveredTime tcpip.MonotonicTime
	firstSent     tcpip.MonotonicTime
	appLimited    bool
	counted       bool
}

// ccsimWrapper wraps the active CC.
type ccsimWrapper struct {
	s         *sender
	inner     congestionControl
	sim       SimCC // non-nil if inner is a registered SimCC
	pacingBps int64

	lossEvents  uint64
	rtoCount    uint64
	lastECECut  tcpip.MonotonicTime
	deliveryBps int64
}

var _ congestionControl = (*ccsimWrapper)(nil)

func (w *ccsimWrapper) HandleLossDetected() {
	w.lossEvents++
	w.inner.HandleLossDetected()
}

func (w *ccsimWrapper) HandleRTOExpired() {
	w.rtoCount++
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
		s.sendData()
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
	seg.ccsim.deliveredTime = st.deliveredTime
	seg.ccsim.firstSent = st.firstSent
	seg.ccsim.appLimited = st.appLimited
	seg.ccsim.counted = false
	st.firstSent = now
}

// ccsimMarkAppLimited is called at the end of sendData: if the sender ran
// out of data with spare cwnd, subsequent samples are app-limited.
func (s *sender) ccsimMarkAppLimited() {
	st := &s.ccsim
	if s.writeNext == nil && s.Outstanding < s.SndCwnd && s.SndUna != s.SndNxt {
		st.appLimited = true
		st.appLimitedSeq = s.SndNxt
	}
}

// ccsimPreAck captures delivery information for segments about to be acked
// (cumulatively or by new SACK blocks), before upstream ACK processing
// removes them.
func (s *sender) ccsimPreAck(rcvdSeg *segment) {
	st := &s.ccsim
	st.scratchAcked = 0
	st.scratchHasSample = false
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
		if seg.ccsim.counted || seg.payloadSize() == 0 {
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
		seg.ccsim.counted = true
		st.scratchAcked += int64(seg.payloadSize())
		// Prefer the most recently transmitted, never-retransmitted
		// segment as the rate sample source.
		if seg.xmitCount == 1 && (!st.scratchHasSample || st.scratchSeg.xmitTime.Before(seg.xmitTime)) {
			st.scratchHasSample = true
			st.scratchSeg.delivered = seg.ccsim.delivered
			st.scratchSeg.deliveredTime = seg.ccsim.deliveredTime
			st.scratchSeg.firstSent = seg.ccsim.firstSent
			st.scratchSeg.xmitTime = seg.xmitTime
			st.scratchSeg.appLimited = seg.ccsim.appLimited
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
		InflightBytes:  int64(s.Outstanding) * int64(s.MaxPayloadSize),
		ECE:            ece,
		RetransSegsCum: s.ep.stats.SendErrors.Retransmits.Value(),
	}
	if st.scratchHasSample {
		sc := &st.scratchSeg
		sample.PriorDelivered = sc.delivered
		ackElapsed := nowMT.Sub(sc.deliveredTime)
		sendElapsed := sc.xmitTime.Sub(sc.firstSent)
		interval := ackElapsed
		if sendElapsed > interval {
			interval = sendElapsed
		}
		sample.RTT = nowMT.Sub(sc.xmitTime)
		sample.Interval = interval
		sample.IsAppLimited = sc.appLimited
		if interval > 0 {
			sample.DeliveryRateBps = (st.delivered - sc.delivered) * 8 *
				int64(time.Second) / int64(interval)
		}
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
	PacingBps     int64
	DeliveryBps   int64
	RetransSegs   uint64
	RTOs          uint64
	LossEvents    uint64
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
		info.PacingBps = w.pacingBps
		info.DeliveryBps = w.deliveryBps
		info.LossEvents = w.lossEvents
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
// with IsRangeLost additionally counting up to maxSACKBlocks ranges per
// chunk. During SACK recovery with a large window this dominated runtime
// (87% of a bufferbloat run's CPU). Because the chunk walk ascends in
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
			// SetPipe(): (a) if IsLost(S1) returns false, Pipe++.
			if !ccsimRangeLost(ranges, suf, i, r, lostBytes) {
				pipe++
			}
			// SetPipe(): (b) if S1 <= HighRxt, Pipe++.
			if s1.sequenceNumber.LessThanEq(s.FastRecovery.HighRxt) {
				pipe++
			}
		}
	}
	return pipe
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
