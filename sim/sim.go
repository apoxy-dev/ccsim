// Package sim wires the virtual clock, two netstack instances, the
// bottleneck link and the application flow drivers into a deterministic
// event-driven simulation.
package sim

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	vclock "ccsim/clock"
	"ccsim/link"
	"ccsim/probe"
	"ccsim/scenario"
	"ccsim/stream"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// Sim is one simulation instance.
type Sim struct {
	cfg *scenario.ScenarioConfig
	clk *vclock.Clock
	lnk *link.Link
	rec *probe.Recorder

	sndStack, rcvStack *stack.Stack
	flows              []*flow

	endT       time.Duration
	sampleIntv time.Duration
	lastSample time.Duration

	prevRetrans uint64 // stack-level, for single-flow attribution fallback
}

// New builds a simulation from a validated scenario. Sample records are
// emitted to w (nil for summary-only runs).
func New(cfg *scenario.ScenarioConfig, w *stream.Writer) (*Sim, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Sim{
		cfg:        cfg,
		clk:        vclock.New(),
		endT:       cfg.Duration(),
		sampleIntv: cfg.Sample.Interval(),
	}

	ccNames := make([]string, len(cfg.Flows))
	for i, f := range cfg.Flows {
		ccNames[i] = f.CC
	}
	s.rec = probe.NewRecorder(w, len(cfg.Flows), ccNames)
	s.rec.PacketEvents = cfg.Sample.PacketEvents
	s.rec.WireStats = cfg.Sample.WireStats

	qcfg := cfg.Link.Queue
	hasReverse := false
	for _, f := range cfg.Flows {
		if f.Reverse {
			hasReverse = true
		}
	}
	makeQdisc := func(dir link.Dir, sink link.QdiscSink) link.Qdisc {
		// The reverse direction gets a plain deep FIFO (it carries only
		// ACKs) — unless the scenario runs reverse data flows, in which
		// case both directions are shaped by the configured discipline.
		if dir == link.Rev && !hasReverse {
			return link.NewTailDrop(1<<16, 0, sink)
		}
		return buildQdisc(qcfg, cfg.Seed, sink)
	}
	s.lnk = link.New(s.clk, link.Config{
		RateBps:   cfg.Link.RateBps(),
		Delay:     cfg.Link.Owd(),
		LossP:     cfg.Link.Loss,
		Jitter:    cfg.Link.Jitter(),
		MakeQdisc: makeQdisc,
	}, cfg.Seed, s.rec.LinkHooks())
	s.lnk.Classify = s.classify
	if cfg.Link.RevOwdMs > 0 {
		s.lnk.SetDirDelay(link.Rev, time.Duration(cfg.Link.RevOwdMs*float64(time.Millisecond)))
	}

	// Registered CCs derive their per-flow randomness from the scenario
	// seed via SimSender.Seed() (per-process sim configuration, like
	// SimSynchronousDispatch; one sim runs at a time).
	tcp.SimSeed = uint64(cfg.Seed)
	tcp.SimECNLowLatency = cfg.Link.Queue.ECN && cfg.Link.Queue.ECNLowLatency

	bufSize := bufSizeFor(cfg)
	var err error
	s.sndStack, err = newStack(s.clk, cfg.Seed^0x5E4D1, s.lnk.Endpoint(link.Fwd), senderAddr, bufSize)
	if err != nil {
		return nil, err
	}
	s.rcvStack, err = newStack(s.clk, cfg.Seed^0x2ECF2, s.lnk.Endpoint(link.Rev), receiverAddr, bufSize)
	if err != nil {
		return nil, err
	}

	for i, fc := range cfg.Flows {
		f := &flow{sim: s, id: i, cfg: fc}
		s.flows = append(s.flows, f)
		if err := f.setupListener(); err != nil {
			return nil, err
		}
		if fc.ExtraOwdMs > 0 {
			s.lnk.SetFlowExtraDelay(i, time.Duration(fc.ExtraOwdMs*float64(time.Millisecond)))
		}
		startAt := time.Duration(fc.StartAt * float64(time.Second))
		ff := f
		s.clk.AfterFunc(startAt, func() {
			if err := ff.start(); err != nil {
				panic(err) // configuration was validated; this is a harness bug
			}
		})
	}

	for _, ev := range cfg.Events {
		ev := ev
		s.clk.AfterFunc(time.Duration(ev.At*float64(time.Second)), func() {
			var v float64
			_ = json.Unmarshal(ev.Value, &v)
			if err := s.Set(ev.Path, v); err != nil {
				panic(err)
			}
		})
	}

	// Periodic sampler.
	var tick func()
	tick = func() {
		now := s.clk.Elapsed()
		s.sample(now)
		if now < s.endT {
			s.clk.AfterFunc(s.sampleIntv, tick)
		}
	}
	s.clk.AfterFunc(s.sampleIntv, tick)

	return s, nil
}

// bufSizeFor sizes the fixed socket buffers so the window never limits the
// congestion controller: twice the worst-case inflight (BDP plus the full
// bottleneck queue), floored at 32 MB. All original presets stay below the
// floor, so their behavior is unchanged; high-BDP scenarios (1 Gbps x
// 300 ms) grow the buffers instead of silently going window-limited.
func bufSizeFor(cfg *scenario.ScenarioConfig) int {
	rttS := (cfg.Link.OwdMs + cfg.Link.OwdMs) / 1000
	if cfg.Link.RevOwdMs > 0 {
		rttS = (cfg.Link.OwdMs + cfg.Link.RevOwdMs) / 1000
	}
	for _, f := range cfg.Flows {
		if extra := 2 * f.ExtraOwdMs / 1000; extra > 0 {
			rttS += extra
		}
	}
	bdp := cfg.Link.RateMbps * 1e6 / 8 * rttS
	var queueBytes float64
	switch cfg.Link.Queue.Kind {
	case "taildrop", "red":
		queueBytes = float64(cfg.Link.Queue.LimitBytes)
		if b := float64(cfg.Link.Queue.LimitPkts) * 1500; b > queueBytes {
			queueBytes = b
		}
	}
	need := int(2 * (bdp + queueBytes))
	if need < bufSizeFloor {
		return bufSizeFloor
	}
	if need > bufSizeCap {
		return bufSizeCap
	}
	return need
}

// buildQdisc constructs the configured forward-direction discipline.
func buildQdisc(q scenario.QueueConfig, seed int64, sink link.QdiscSink) link.Qdisc {
	switch q.Kind {
	case "taildrop":
		return link.NewTailDrop(q.LimitPkts, q.LimitBytes, sink)
	case "red":
		p := link.DefaultREDParams(q.LimitPkts)
		if q.MinTh > 0 {
			p.MinTh = q.MinTh
		}
		if q.MaxTh > 0 {
			p.MaxTh = q.MaxTh
		}
		if q.MaxP > 0 {
			p.MaxP = q.MaxP
		}
		p.ECN = q.ECN
		// Named PRNG sub-stream for RED decisions.
		rng := rand.New(rand.NewPCG(uint64(seed), 0x2ED))
		return link.NewRED(q.LimitPkts, p, rng.Float64, sink)
	case "codel":
		p := link.CoDelParams{ECN: q.ECN,
			Target:   time.Duration(q.TargetMs * float64(time.Millisecond)),
			Interval: time.Duration(q.IntervalMs * float64(time.Millisecond))}
		lim := q.LimitPkts
		if lim == 0 && q.LimitBytes == 0 {
			lim = 10240
		}
		return link.NewCoDel(lim, q.LimitBytes, p, sink)
	case "fqcodel":
		p := link.DefaultFQCoDelParams()
		p.CoDel.ECN = q.ECN
		if q.TargetMs > 0 {
			p.CoDel.Target = time.Duration(q.TargetMs * float64(time.Millisecond))
		}
		if q.IntervalMs > 0 {
			p.CoDel.Interval = time.Duration(q.IntervalMs * float64(time.Millisecond))
		}
		if q.LimitPkts > 0 {
			p.LimitPkts = q.LimitPkts
		}
		if q.LimitBytes > 0 {
			p.LimitBytes = q.LimitBytes
		}
		return link.NewFQCoDel(p, sink)
	}
	panic("unreachable: validated queue kind " + q.Kind)
}

// classify maps a packet to its flow id via the fixed per-flow ports.
func (s *Sim) classify(p *link.Packet) int {
	_, _, proto, sp, dp, ok := p.FiveTuple()
	if !ok || proto != 6 {
		return -1
	}
	for _, port := range []uint16{sp, dp} {
		if int(port) >= sndPortBase && int(port) < sndPortBase+len(s.flows) {
			return int(port) - sndPortBase
		}
	}
	return -1
}

// sample takes the periodic per-flow and link taps.
func (s *Sim) sample(now time.Duration) {
	for _, f := range s.flows {
		if f.ep == nil {
			continue
		}
		m := s.flowMetrics(f)
		s.rec.OnFlowSample(now, f.id, m)
	}
	qp, qb := s.lnk.QueueDepth()
	s.rec.OnLinkSample(now, qp, qb, now-s.lastSample)
	s.lastSample = now
}

// flowMetrics gathers one flow's sender-side metrics.
func (s *Sim) flowMetrics(f *flow) probe.FlowMetrics {
	var info tcpip.TCPInfoOption
	if err := f.ep.GetSockOpt(&info); err != nil {
		return probe.FlowMetrics{}
	}
	m := probe.FlowMetrics{
		CwndPkts:   float64(info.SndCwnd),
		SRTT:       info.RTT,
		BytesAcked: f.deliveredBytes,
		CCState:    int(info.CcState),
	}
	// Stack-global counters; exact per-flow values come from the sender
	// tap once the CC integration patch fills them in.
	connStack, _, _, _ := f.endpoints()
	tstats := connStack.Stats().TCP
	if len(s.flows) == 1 {
		m.Retransmits = tstats.Retransmits.Value()
		m.RTOs = tstats.Timeouts.Value()
	}
	s.fillSenderTap(f, &m)
	return m
}

// Set applies a live-settable parameter change.
func (s *Sim) Set(path string, v float64) error {
	switch path {
	case "link.rate_mbps":
		s.lnk.SetRate(int64(v * 1e6))
	case "link.loss":
		s.lnk.SetLoss(v)
	case "link.owd_ms":
		s.lnk.SetDelay(time.Duration(v * float64(time.Millisecond)))
	case "link.jitter_ms":
		s.lnk.SetJitter(time.Duration(v * float64(time.Millisecond)))
	case "link.queue.limit_pkts":
		s.lnk.SetQueueLimit(int(v), 0)
	case "link.queue.limit_bytes":
		s.lnk.SetQueueLimit(0, int(v))
	case "link.drop_next":
		// Scripted loss: force-drop the next int(v) packets entering the
		// data direction (analytical single-loss experiments).
		s.lnk.DropNext(link.Fwd, int(v))
	default:
		return fmt.Errorf("sim: path %q is not live-settable", path)
	}
	return nil
}

// InjectAt schedules a live-settable parameter change at virtual time at
// (absolute, from sim start), exactly as a pre-declared scenario event would
// be. Callable while the sim is running, from between-slice callbacks.
func (s *Sim) InjectAt(at time.Duration, path string, v float64) {
	s.clk.AfterFunc(at-s.clk.Elapsed(), func() {
		if err := s.Set(path, v); err != nil {
			panic(err)
		}
	})
}

// PendingTimers exposes the clock's scheduled-timer count for invariant
// checks (timer leak detection in the fuzz suite).
func (s *Sim) PendingTimers() int { return s.clk.Pending() }

// Step advances virtual time by dt (bounded by the configured duration).
func (s *Sim) Step(dt time.Duration) {
	now := s.clk.Elapsed()
	target := now + dt
	if target > s.endT {
		target = s.endT
	}
	s.clk.Advance(target - now)
}

// Done reports whether the sim has reached its configured duration.
func (s *Sim) Done() bool { return s.clk.Elapsed() >= s.endT }

// Elapsed returns current virtual time.
func (s *Sim) Elapsed() time.Duration { return s.clk.Elapsed() }

// DefaultSlice is the event-loop slice between control-mailbox checks.
const DefaultSlice = 10 * time.Millisecond

// Run advances flat-out to the end (batch mode), invoking between (if
// non-nil) at slice boundaries; return false from between to pause/stop.
func (s *Sim) Run(between func() bool) probe.RunSummary {
	for !s.Done() {
		s.Step(DefaultSlice)
		if between != nil && !between() {
			break
		}
	}
	return s.Finish()
}

// Finish flushes the sample stream and returns the run summary.
func (s *Sim) Finish() probe.RunSummary {
	if s.rec.W != nil {
		s.rec.W.Flush()
	}
	return s.rec.Summary(s.endT)
}

// Close tears down both netstack instances so the sim becomes collectible.
// Each stack pins ~10 goroutines (and through them the whole object graph,
// ~29 MB per cubic-single run); without Close, hosts that create sims in a
// loop — the browser page reloading presets, the fuzz suite — leak the
// full sim per iteration. Safe to call once, after the sim is done.
func (s *Sim) Close() {
	for _, st := range []*stack.Stack{s.sndStack, s.rcvStack} {
		if st != nil {
			st.Destroy()
		}
	}
	s.sndStack, s.rcvStack = nil, nil
}
