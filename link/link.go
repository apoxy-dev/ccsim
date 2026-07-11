// Package link implements the bottleneck link model: for each direction an
// ingress queue (pluggable discipline), serialization at a configured rate,
// fixed propagation delay (plus optional per-flow extra delay), optional
// Bernoulli wire loss, and delivery into the peer netstack.
//
// All events are scheduled on the shared virtual clock; nothing here uses
// wall-clock time or goroutines.
package link

import (
	"fmt"
	"math/rand/v2"
	"time"

	vclock "ccsim/clock"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Dir identifies a link direction.
type Dir int

const (
	// Fwd carries traffic from stack A (senders) to stack B (receivers).
	Fwd Dir = iota
	// Rev carries traffic from B back to A (mostly ACKs).
	Rev
)

func (d Dir) String() string {
	if d == Fwd {
		return "fwd"
	}
	return "rev"
}

// Event is a telemetry record emitted by the link hooks.
type Event struct {
	T      time.Duration // virtual time
	Dir    Dir
	Flow   int
	Size   int
	QLen   int // queue depth in packets after the event
	QBytes int // queue depth in bytes after the event
	Reason DropReason
}

// Hooks receives link telemetry. Any hook may be nil.
type Hooks struct {
	OnEnqueue func(Event)
	OnDequeue func(Event)
	OnDrop    func(Event)
	OnMark    func(Event)
	OnDeliver func(Event)
}

// Config is the runtime-independent link configuration.
type Config struct {
	RateBps   int64         // serialization rate, bits/s (both directions)
	Delay     time.Duration // one-way propagation delay per direction
	LossP     float64       // Bernoulli per-packet wire loss probability
	MTU       uint32        // link MTU (default 1500)
	MakeQdisc func(dir Dir, sink QdiscSink) Qdisc
}

// QdiscSink is the exported view of the drop/mark sink handed to qdisc
// factories.
type QdiscSink interface {
	qdiscSink
}

// Link is a bidirectional bottleneck link connecting two stacks.
type Link struct {
	clk   *vclock.Clock
	hooks Hooks
	// Classify maps a packet to a simulator flow id; set by the harness.
	Classify func(p *Packet) int
	pipes    [2]*pipe
	eps      [2]*Endpoint
}

// New creates a link. Use Endpoint(Fwd) / Endpoint(Rev) as the LinkEndpoints
// when creating NICs: the Fwd endpoint belongs to stack A, Rev to stack B.
// seed derives the per-direction loss PRNG streams.
func New(clk *vclock.Clock, cfg Config, seed int64, hooks Hooks) *Link {
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	l := &Link{clk: clk, hooks: hooks}
	for d := Fwd; d <= Rev; d++ {
		p := &pipe{
			link:    l,
			dir:     d,
			rateBps: cfg.RateBps,
			delay:   cfg.Delay,
			lossP:   cfg.LossP,
			// Independent, seed-derived PRNG stream per direction.
			rng: rand.New(rand.NewPCG(uint64(seed), 0x9E3779B97F4A7C15*uint64(d+1))),
		}
		p.q = cfg.MakeQdisc(d, p)
		l.pipes[d] = p
	}
	l.eps[Fwd] = &Endpoint{link: l, sendDir: Fwd, mtu: cfg.MTU}
	l.eps[Rev] = &Endpoint{link: l, sendDir: Rev, mtu: cfg.MTU}
	return l
}

// Endpoint returns the stack.LinkEndpoint that transmits into direction d.
func (l *Link) Endpoint(d Dir) *Endpoint { return l.eps[d] }

// SetRate updates the serialization rate of both directions.
func (l *Link) SetRate(bps int64) {
	for _, p := range l.pipes {
		p.rateBps = bps
	}
}

// SetLoss updates the wire loss probability of both directions.
func (l *Link) SetLoss(p float64) {
	for _, pp := range l.pipes {
		pp.lossP = p
	}
}

// SetDelay updates the one-way propagation delay of both directions.
func (l *Link) SetDelay(d time.Duration) {
	for _, p := range l.pipes {
		p.delay = d
	}
}

// SetQueueLimit updates queue limits on the forward-direction qdisc.
func (l *Link) SetQueueLimit(pkts, bytes int) {
	l.pipes[Fwd].q.SetLimit(pkts, bytes)
	l.pipes[Rev].q.SetLimit(pkts, bytes)
}

// SetFlowExtraDelay adds extra one-way delay for a flow id (both directions,
// emulating a longer path for that flow).
func (l *Link) SetFlowExtraDelay(flow int, d time.Duration) {
	for _, p := range l.pipes {
		if p.extraDelay == nil {
			p.extraDelay = map[int]time.Duration{}
		}
		p.extraDelay[flow] = d
	}
}

// QueueDepth returns the forward queue depth.
func (l *Link) QueueDepth() (pkts, bytes int) {
	return l.pipes[Fwd].q.Len(), l.pipes[Fwd].q.Bytes()
}

// pipe is one direction of the link.
type pipe struct {
	link       *Link
	dir        Dir
	q          Qdisc
	rateBps    int64
	delay      time.Duration
	lossP      float64
	rng        *rand.Rand
	extraDelay map[int]time.Duration
	busy       bool // serializer transmitting
	seq        uint64

	// Serializer timer state (single reusable timer, no per-packet
	// closures: this path runs a quarter million times per simulated
	// 30 s at 100 Mbps).
	txTimer tcpip.Timer
	txPkt   *Packet

	// Propagation-delay stage: time-ordered pending deliveries drained by
	// one reusable timer.
	delivQ     []delivEntry
	delivTimer tcpip.Timer
}

type delivEntry struct {
	at time.Duration
	pk *Packet
}

// now returns the current virtual time as a duration since epoch.
func (p *pipe) now() time.Duration { return p.link.clk.Elapsed() }

func (p *pipe) event(pk *Packet, reason DropReason) Event {
	return Event{
		T: p.now(), Dir: p.dir, Flow: pk.Flow, Size: pk.Size(),
		QLen: p.q.Len(), QBytes: p.q.Bytes(), Reason: reason,
	}
}

// qdiscDropped implements qdiscSink.
func (p *pipe) qdiscDropped(pk *Packet, reason DropReason) {
	if h := p.link.hooks.OnDrop; h != nil {
		h(p.event(pk, reason))
	}
}

// qdiscMarked implements qdiscSink.
func (p *pipe) qdiscMarked(pk *Packet) {
	if h := p.link.hooks.OnMark; h != nil {
		h(p.event(pk, 0))
	}
}

// send accepts a serialized IP packet from the local stack.
func (p *pipe) send(data []byte) {
	pk := &Packet{Data: data, Flow: -1}
	p.seq++
	pk.seq = p.seq
	if c := p.link.Classify; c != nil {
		pk.Flow = c(pk)
	}
	if dropped := p.q.Enqueue(pk, p.now()); dropped {
		return
	}
	if h := p.link.hooks.OnEnqueue; h != nil {
		h(p.event(pk, 0))
	}
	if !p.busy {
		p.startNext()
	}
}

// startNext pulls the next packet from the queue and schedules its
// serialization completion.
func (p *pipe) startNext() {
	pk := p.q.Dequeue(p.now())
	if pk == nil {
		p.busy = false
		return
	}
	if h := p.link.hooks.OnDequeue; h != nil {
		h(p.event(pk, 0))
	}
	p.busy = true
	tx := time.Duration(int64(pk.Size()) * 8 * int64(time.Second) / p.rateBps)
	p.txPkt = pk
	if p.txTimer == nil {
		p.txTimer = p.link.clk.AfterFunc(tx, p.onTxTimer)
	} else {
		p.txTimer.Reset(tx)
	}
}

// onTxTimer runs when the last bit of the current packet has been
// serialized onto the wire.
func (p *pipe) onTxTimer() {
	pk := p.txPkt
	p.txPkt = nil
	// Wire loss applies after serialization.
	if p.lossP > 0 && p.rng.Float64() < p.lossP {
		p.qdiscDropped(pk, DropWire)
	} else {
		d := p.delay
		if extra, ok := p.extraDelay[pk.Flow]; ok {
			d += extra
		}
		p.scheduleDelivery(p.now()+d, pk)
	}
	p.startNext()
}

// scheduleDelivery inserts pk into the time-ordered pending list and arms
// the delivery timer for the head entry.
func (p *pipe) scheduleDelivery(at time.Duration, pk *Packet) {
	e := delivEntry{at: at, pk: pk}
	// Almost always append (per-flow extra delay can reorder tails).
	i := len(p.delivQ)
	for i > 0 && p.delivQ[i-1].at > at {
		i--
	}
	p.delivQ = append(p.delivQ, delivEntry{})
	copy(p.delivQ[i+1:], p.delivQ[i:])
	p.delivQ[i] = e
	if i == 0 {
		p.armDelivTimer()
	}
}

func (p *pipe) armDelivTimer() {
	d := p.delivQ[0].at - p.now()
	if d < 0 {
		d = 0
	}
	if p.delivTimer == nil {
		p.delivTimer = p.link.clk.AfterFunc(d, p.onDelivTimer)
	} else {
		p.delivTimer.Reset(d)
	}
}

// onDelivTimer delivers all due packets to the peer stack.
func (p *pipe) onDelivTimer() {
	now := p.now()
	for len(p.delivQ) > 0 && p.delivQ[0].at <= now {
		pk := p.delivQ[0].pk
		p.delivQ[0].pk = nil
		p.delivQ = p.delivQ[1:]
		if h := p.link.hooks.OnDeliver; h != nil {
			h(p.event(pk, 0))
		}
		// The endpoint that receives traffic from direction d is the
		// opposite side's endpoint.
		p.link.eps[1-p.dir].inject(pk)
	}
	if len(p.delivQ) > 0 {
		p.armDelivTimer()
	} else if cap(p.delivQ) > 1024 {
		p.delivQ = nil // shed the grown backing array
	}
}

// Endpoint implements stack.LinkEndpoint. Packets written by its stack are
// transmitted into direction sendDir; packets from the opposite pipe are
// injected into its stack's dispatcher.
type Endpoint struct {
	link       *Link
	sendDir    Dir
	mtu        uint32
	dispatcher stack.NetworkDispatcher
}

var _ stack.LinkEndpoint = (*Endpoint)(nil)

// WritePackets implements stack.LinkWriter.
func (e *Endpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	for _, pb := range pkts.AsSlice() {
		// Single copy: flatten the packet buffer directly into a fresh
		// slice owned by the link model.
		buf := make([]byte, pb.Size())
		off := 0
		for _, sl := range pb.AsSlices() {
			off += copy(buf[off:], sl)
		}
		e.link.pipes[e.sendDir].send(buf)
		n++
	}
	return n, nil
}

// inject delivers a raw IP packet into this endpoint's stack.
func (e *Endpoint) inject(pk *Packet) {
	if e.dispatcher == nil {
		return
	}
	pb := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(pk.Data),
	})
	e.dispatcher.DeliverNetworkPacket(header.IPv4ProtocolNumber, pb)
	pb.DecRef()
}

// MTU implements stack.NetworkLinkEndpoint.
func (e *Endpoint) MTU() uint32 { return e.mtu }

// SetMTU implements stack.NetworkLinkEndpoint.
func (e *Endpoint) SetMTU(mtu uint32) { e.mtu = mtu }

// MaxHeaderLength implements stack.NetworkLinkEndpoint.
func (e *Endpoint) MaxHeaderLength() uint16 { return 0 }

// LinkAddress implements stack.NetworkLinkEndpoint.
func (e *Endpoint) LinkAddress() tcpip.LinkAddress { return "" }

// SetLinkAddress implements stack.NetworkLinkEndpoint.
func (e *Endpoint) SetLinkAddress(tcpip.LinkAddress) {}

// Capabilities implements stack.NetworkLinkEndpoint. Checksum offload is
// claimed in both directions: packets never cross a real wire, so computing
// and validating TCP checksums would only burn simulation CPU.
func (e *Endpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityTXChecksumOffload | stack.CapabilityRXChecksumOffload
}

// Attach implements stack.NetworkLinkEndpoint.
func (e *Endpoint) Attach(dispatcher stack.NetworkDispatcher) { e.dispatcher = dispatcher }

// IsAttached implements stack.NetworkLinkEndpoint.
func (e *Endpoint) IsAttached() bool { return e.dispatcher != nil }

// Wait implements stack.NetworkLinkEndpoint.
func (e *Endpoint) Wait() {}

// ARPHardwareType implements stack.NetworkLinkEndpoint.
func (e *Endpoint) ARPHardwareType() header.ARPHardwareType { return header.ARPHardwareNone }

// AddHeader implements stack.NetworkLinkEndpoint.
func (e *Endpoint) AddHeader(*stack.PacketBuffer) {}

// ParseHeader implements stack.NetworkLinkEndpoint.
func (e *Endpoint) ParseHeader(*stack.PacketBuffer) bool { return true }

// Close implements stack.NetworkLinkEndpoint.
func (e *Endpoint) Close() {}

// SetOnCloseAction implements stack.NetworkLinkEndpoint.
func (e *Endpoint) SetOnCloseAction(func()) {}

// String aids debugging.
func (e *Endpoint) String() string { return fmt.Sprintf("simlink(%s)", e.sendDir) }
