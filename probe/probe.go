// Package probe implements per-flow and link instrumentation: periodic
// sample taps, edge-triggered event records, online summary statistics and
// windowed analysis helpers used by tests.
package probe

import (
	"math"
	"sort"
	"time"

	"ccsim/link"
	"ccsim/stream"
)

// FlowMetrics is one periodic observation of a flow's sender state.
type FlowMetrics struct {
	CwndPkts      float64
	InflightBytes float64
	PacingBps     float64 // 0 if the CC does not pace
	SRTT          time.Duration
	MinRTT        time.Duration // 0 if unknown
	DeliveryBps   float64       // 0 if unknown
	BytesAcked    uint64
	Retransmits   uint64
	RTOs          uint64
	LossEvents    uint64
	CCState       int
}

// FlowSummary aggregates one flow over the whole run.
type FlowSummary struct {
	ID          int     `json:"id"`
	CC          string  `json:"cc"`
	GoodputMbps float64 `json:"goodput_mbps"`
	SRTTMeanMs  float64 `json:"srtt_mean_ms"`
	SRTTP95Ms   float64 `json:"srtt_p95_ms"`
	SRTTMaxMs   float64 `json:"srtt_max_ms"`
	Retransmits uint64  `json:"retransmits"`
	RTOs        uint64  `json:"rtos"`
	CwndCuts    int     `json:"cwnd_cuts"`
	FCTCount    int     `json:"fct_count,omitempty"`
	FCTP50Ms    float64 `json:"fct_p50_ms,omitempty"`
	FCTP95Ms    float64 `json:"fct_p95_ms,omitempty"`
	FCTP99Ms    float64 `json:"fct_p99_ms,omitempty"`
}

// RunSummary aggregates a whole run.
type RunSummary struct {
	DurS          float64       `json:"dur_s"`
	Flows         []FlowSummary `json:"flows"`
	Drops         uint64        `json:"drops"`
	CEMarks       uint64        `json:"ce_marks"`
	QDepthMeanPkt float64       `json:"qdepth_mean_pkts"`
	QDepthMaxPkt  int           `json:"qdepth_max_pkts"`
}

type flowState struct {
	cc        string
	srtts     []float64 // ms
	fcts      []float64 // ms
	appBytes  uint64
	prevCwnd  float64
	prevRetr  uint64
	prevRTOs  uint64
	prevLoss  uint64
	prevState int
	cwndCuts  int
	retrans   uint64
	rtos      uint64
	started   bool
	lastAcked uint64
	firstTick time.Duration
	lastTick  time.Duration
}

// Recorder writes the sample stream and maintains run summary accumulators.
type Recorder struct {
	W            *stream.Writer
	PacketEvents bool

	flows []flowState

	drops, marks   uint64
	qDepthSum      float64
	qDepthSamples  int
	qDepthMax      int
	deliveredBytes [2]uint64 // per direction, since last link sample
}

// NewRecorder creates a Recorder for n flows writing to w (which may be nil
// for summary-only runs).
func NewRecorder(w *stream.Writer, n int, ccNames []string) *Recorder {
	r := &Recorder{W: w, flows: make([]flowState, n)}
	for i := range r.flows {
		r.flows[i].cc = ccNames[i]
	}
	return r
}

func (r *Recorder) write(t time.Duration, flow uint16, k stream.Kind, v float64) {
	if r.W == nil {
		return
	}
	r.W.Write(stream.Record{T: t.Seconds(), Flow: flow, Kind: k, Value: v})
}

// OnFlowSample records one periodic flow observation.
func (r *Recorder) OnFlowSample(t time.Duration, id int, m FlowMetrics) {
	f := &r.flows[id]
	fid := uint16(id)
	r.write(t, fid, stream.KindCwndPkts, m.CwndPkts)
	r.write(t, fid, stream.KindInflightBytes, m.InflightBytes)
	r.write(t, fid, stream.KindPacingRateBps, m.PacingBps)
	r.write(t, fid, stream.KindSRTTSec, m.SRTT.Seconds())
	r.write(t, fid, stream.KindMinRTTSec, m.MinRTT.Seconds())
	r.write(t, fid, stream.KindDeliveryBps, m.DeliveryBps)
	r.write(t, fid, stream.KindBytesAckedCum, float64(m.BytesAcked))
	r.write(t, fid, stream.KindRetransCum, float64(m.Retransmits))
	r.write(t, fid, stream.KindCCState, float64(m.CCState))

	if m.SRTT > 0 {
		f.srtts = append(f.srtts, float64(m.SRTT)/float64(time.Millisecond))
	}
	// Edge detection.
	if f.started && m.LossEvents > f.prevLoss {
		f.cwndCuts += int(m.LossEvents - f.prevLoss)
		r.write(t, fid, stream.KindLossRecovery, m.CwndPkts)
	}
	if f.started && m.RTOs > f.prevRTOs {
		r.write(t, fid, stream.KindRTO, float64(m.RTOs))
	}
	if !f.started {
		f.firstTick = t
	}
	f.lastTick = t
	f.prevCwnd = m.CwndPkts
	f.prevRetr = m.Retransmits
	f.prevRTOs = m.RTOs
	f.prevLoss = m.LossEvents
	f.prevState = m.CCState
	f.retrans = m.Retransmits
	f.rtos = m.RTOs
	f.started = true
	f.lastAcked = m.BytesAcked
}

// OnLinkSample records the periodic link tap: queue depth and per-direction
// utilization over the elapsed window.
func (r *Recorder) OnLinkSample(t time.Duration, qPkts, qBytes int, window time.Duration) {
	r.write(t, stream.LinkFwd, stream.KindQDepthPkts, float64(qPkts))
	r.write(t, stream.LinkFwd, stream.KindQDepthBytes, float64(qBytes))
	if window > 0 {
		for d := 0; d < 2; d++ {
			fid := stream.LinkFwd
			if d == 1 {
				fid = stream.LinkRev
			}
			bps := float64(r.deliveredBytes[d]) * 8 / window.Seconds()
			r.write(t, fid, stream.KindUtilizationBps, bps)
			r.deliveredBytes[d] = 0
		}
	}
	r.qDepthSum += float64(qPkts)
	r.qDepthSamples++
	if qPkts > r.qDepthMax {
		r.qDepthMax = qPkts
	}
}

// LinkHooks returns link.Hooks wired into this recorder. The per-packet
// enqueue/dequeue taps are only installed when the scenario asks for the
// packet event stream — a nil hook lets the link skip building the event
// record entirely on the per-packet fast path.
func (r *Recorder) LinkHooks() link.Hooks {
	h := link.Hooks{
		OnDrop: func(e link.Event) {
			// Wire loss is not a queue drop but still counts as a drop record.
			r.drops++
			r.write(e.T, flowOrLink(e), stream.KindDrop, float64(e.Reason))
		},
		OnMark: func(e link.Event) {
			r.marks++
			r.write(e.T, flowOrLink(e), stream.KindCEMark, 1)
		},
		OnDeliver: func(e link.Event) {
			r.deliveredBytes[e.Dir] += uint64(e.Size)
		},
	}
	if r.PacketEvents {
		h.OnEnqueue = func(e link.Event) {
			r.write(e.T, flowOrLink(e), stream.KindPktEnqueue, float64(e.Size))
		}
		h.OnDequeue = func(e link.Event) {
			r.write(e.T, flowOrLink(e), stream.KindPktDequeue, float64(e.Size))
		}
	}
	return h
}

func flowOrLink(e link.Event) uint16 {
	if e.Flow >= 0 {
		return uint16(e.Flow)
	}
	if e.Dir == link.Fwd {
		return stream.LinkFwd
	}
	return stream.LinkRev
}

// OnAppBytes accumulates receiver-side application bytes (goodput).
func (r *Recorder) OnAppBytes(flow int, n int) {
	r.flows[flow].appBytes += uint64(n)
}

// OnFCT records one rr response completion time.
func (r *Recorder) OnFCT(t time.Duration, flow int, fct time.Duration) {
	f := &r.flows[flow]
	f.fcts = append(f.fcts, float64(fct)/float64(time.Millisecond))
	r.write(t, uint16(flow), stream.KindFCTSec, fct.Seconds())
}

// Marks returns the number of CE marks recorded so far.
func (r *Recorder) Marks() uint64 { return r.marks }

// Summary finalizes and returns the run summary. dur is the configured run
// duration; goodput uses each flow's active period.
func (r *Recorder) Summary(dur time.Duration) RunSummary {
	s := RunSummary{DurS: dur.Seconds(), Drops: r.drops, CEMarks: r.marks, QDepthMaxPkt: r.qDepthMax}
	if r.qDepthSamples > 0 {
		s.QDepthMeanPkt = r.qDepthSum / float64(r.qDepthSamples)
	}
	for i := range r.flows {
		f := &r.flows[i]
		fs := FlowSummary{ID: i, CC: f.cc, Retransmits: f.retrans, RTOs: f.rtos, CwndCuts: f.cwndCuts}
		active := f.lastTick - f.firstTick
		if active > 0 {
			fs.GoodputMbps = float64(f.appBytes) * 8 / active.Seconds() / 1e6
		}
		if len(f.srtts) > 0 {
			fs.SRTTMeanMs = mean(f.srtts)
			fs.SRTTP95Ms = percentile(f.srtts, 95)
			fs.SRTTMaxMs = percentile(f.srtts, 100)
		}
		if len(f.fcts) > 0 {
			fs.FCTCount = len(f.fcts)
			fs.FCTP50Ms = percentile(f.fcts, 50)
			fs.FCTP95Ms = percentile(f.fcts, 95)
			fs.FCTP99Ms = percentile(f.fcts, 99)
		}
		s.Flows = append(s.Flows, fs)
	}
	return s
}

func mean(xs []float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// percentile returns the p-th percentile (nearest-rank) of xs.
func percentile(xs []float64, p float64) float64 {
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	if p >= 100 {
		return c[len(c)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(c)))) - 1
	if rank < 0 {
		rank = 0
	}
	return c[rank]
}
