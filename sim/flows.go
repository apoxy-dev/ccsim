package sim

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"time"

	"ccsim/scenario"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// Fixed, per-flow ports (no ephemeral randomness).
	sndPortBase = 40001
	rcvPortBase = 5001

	writeChunk = 256 << 10
)

// zeroChunk is the shared bulk payload source.
var zeroChunk = make([]byte, writeChunk)

// flow drives one TCP connection: sender endpoint on stack A, listener and
// accepted endpoint on stack B. All progress is event-driven off waiter
// notifications, re-entered via zero-delay virtual timers.
type flow struct {
	sim *Sim
	id  int
	cfg scenario.FlowConfig

	ep  tcpip.Endpoint // sender-side connection endpoint
	wq  *waiter.Queue
	lep tcpip.Endpoint // receiver-side listener
	lwq *waiter.Queue
	rep tcpip.Endpoint // receiver-side accepted endpoint
	rwq *waiter.Queue

	connected     bool
	writePending  bool // a writer pass is scheduled
	readPending   bool
	acceptPending bool

	deliveredBytes uint64 // receiver-side cumulative app bytes

	// rr state.
	rrRand      *rand.Rand
	reqSendTs   []time.Duration // send times of outstanding requests
	respRecvB   int             // bytes received of the in-progress response
	srvReqB     int             // server: bytes received of in-progress request
	srvRespOwed int             // server: response bytes not yet written
}

// maybeEnableECT stamps ECT(0) on the endpoint's traffic when the scenario
// queue does ECN marking (the ccsim netstack patch lets the ECN bits
// through; stock netstack masks them).
func (f *flow) maybeEnableECT(ep tcpip.Endpoint) {
	if !f.sim.cfg.Link.Queue.ECN {
		return
	}
	tcp.SimAllowECTTOS = true
	if err := ep.SetSockOptInt(tcpip.IPv4TOSOption, 0x02); err != nil {
		panic(fmt.Sprintf("sim: flow %d set ECT: %s", f.id, err))
	}
}

func (f *flow) sndPort() uint16 { return uint16(sndPortBase + f.id) }
func (f *flow) rcvPort() uint16 { return uint16(rcvPortBase + f.id) }

// setupListener creates the receiver-side listening socket at load time.
func (f *flow) setupListener() error {
	var wq waiter.Queue
	ep, err := f.sim.rcvStack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return fmt.Errorf("sim: flow %d listener: %s", f.id, err)
	}
	f.lep, f.lwq = ep, &wq
	f.maybeEnableECT(ep)
	if err := ep.Bind(tcpip.FullAddress{NIC: nicID, Addr: receiverAddr, Port: f.rcvPort()}); err != nil {
		return fmt.Errorf("sim: flow %d bind: %s", f.id, err)
	}
	if err := ep.Listen(2); err != nil {
		return fmt.Errorf("sim: flow %d listen: %s", f.id, err)
	}
	entry := waiter.NewFunctionEntry(waiter.ReadableEvents, func(waiter.EventMask) {
		f.scheduleOnce(&f.acceptPending, f.acceptPass)
	})
	f.lwq.EventRegister(&entry)
	return nil
}

// start creates the sender endpoint and initiates the connect. Called at the
// flow's start_at time.
func (f *flow) start() error {
	var wq waiter.Queue
	ep, err := f.sim.sndStack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return fmt.Errorf("sim: flow %d endpoint: %s", f.id, err)
	}
	f.ep, f.wq = ep, &wq
	f.maybeEnableECT(ep)
	cc := tcpip.CongestionControlOption(f.cfg.CC)
	if err := ep.SetSockOpt(&cc); err != nil {
		return fmt.Errorf("sim: flow %d set cc %q: %s", f.id, f.cfg.CC, err)
	}
	if err := ep.Bind(tcpip.FullAddress{NIC: nicID, Addr: senderAddr, Port: f.sndPort()}); err != nil {
		return fmt.Errorf("sim: flow %d bind: %s", f.id, err)
	}
	entry := waiter.NewFunctionEntry(waiter.WritableEvents, func(waiter.EventMask) {
		f.scheduleOnce(&f.writePending, f.writePass)
	})
	f.wq.EventRegister(&entry)
	rentry := waiter.NewFunctionEntry(waiter.ReadableEvents, func(waiter.EventMask) {
		f.scheduleOnce(&f.readPending, f.clientReadPass)
	})
	f.wq.EventRegister(&rentry)

	err2 := ep.Connect(tcpip.FullAddress{NIC: nicID, Addr: receiverAddr, Port: f.rcvPort()})
	if _, ok := err2.(*tcpip.ErrConnectStarted); !ok && err2 != nil {
		return fmt.Errorf("sim: flow %d connect: %s", f.id, err2)
	}
	if f.cfg.App.Kind == "rr" {
		// Named per-flow PRNG sub-stream for request arrivals.
		f.rrRand = rand.New(rand.NewPCG(uint64(f.sim.cfg.Seed), 0xA11CE00+uint64(f.id)))
		f.scheduleNextRequest()
	}
	return nil
}

// scheduleOnce schedules fn at the current virtual instant unless already
// pending — waiter callbacks fire deep inside netstack processing, so work
// is deferred to the clock loop.
func (f *flow) scheduleOnce(flag *bool, fn func()) {
	if *flag {
		return
	}
	*flag = true
	f.sim.clk.AfterFunc(0, func() {
		*flag = false
		fn()
	})
}

// acceptPass accepts pending connections on the listener.
func (f *flow) acceptPass() {
	for {
		ep, wq, err := f.lep.Accept(nil)
		if err != nil {
			return // ErrWouldBlock and anything else: wait for next event
		}
		f.rep, f.rwq = ep, wq
		entry := waiter.NewFunctionEntry(waiter.ReadableEvents, func(waiter.EventMask) {
			f.scheduleOnce(&f.readPending, f.serverReadPass)
		})
		wq.EventRegister(&entry)
		// Drain anything that arrived with the final handshake ACK.
		f.serverReadPass()
	}
}

// writePass runs the sender application (bulk writer or rr response pump on
// the client side there is nothing to pump — requests are timer-driven).
func (f *flow) writePass() {
	if f.ep == nil {
		return
	}
	if !f.connected {
		// First writable event doubles as connect completion.
		f.connected = true
	}
	if f.cfg.App.Kind == "bulk" {
		f.pumpZeros(f.ep, math.MaxInt64)
	}
}

// pumpZeros writes up to limit bytes of zeros, returning bytes written.
func (f *flow) pumpZeros(ep tcpip.Endpoint, limit int64) int64 {
	var total int64
	for total < limit {
		n := int64(writeChunk)
		if rem := limit - total; rem < n {
			n = rem
		}
		r := bytes.NewReader(zeroChunk[:n])
		w, err := ep.Write(r, tcpip.WriteOptions{})
		total += w
		if err != nil {
			break // ErrWouldBlock: writable event will re-arm
		}
		if w < n {
			break
		}
	}
	return total
}

// countingDiscard counts bytes read.
type countingDiscard struct{ n int }

func (c *countingDiscard) Write(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}

// serverReadPass drains the accepted endpoint; for rr flows it assembles
// requests and queues responses.
func (f *flow) serverReadPass() {
	if f.rep == nil {
		return
	}
	var c countingDiscard
	for {
		_, err := f.rep.Read(&c, tcpip.ReadOptions{})
		if err != nil {
			break
		}
	}
	if c.n == 0 {
		return
	}
	f.deliveredBytes += uint64(c.n)
	f.sim.rec.OnAppBytes(f.id, c.n)
	if f.cfg.App.Kind == "rr" {
		f.srvReqB += c.n
		for f.srvReqB >= f.cfg.App.ReqBytes {
			f.srvReqB -= f.cfg.App.ReqBytes
			f.srvRespOwed += f.cfg.App.RespBytes
		}
		f.serverWritePass()
	}
}

// serverWritePass writes owed rr response bytes.
func (f *flow) serverWritePass() {
	if f.srvRespOwed == 0 || f.rep == nil {
		return
	}
	w := f.pumpZeros(f.rep, int64(f.srvRespOwed))
	f.srvRespOwed -= int(w)
	if f.srvRespOwed > 0 {
		entry := waiter.NewFunctionEntry(waiter.WritableEvents, func(waiter.EventMask) {
			f.sim.clk.AfterFunc(0, f.serverWritePass)
		})
		f.rwq.EventRegister(&entry)
		// Entry deliberately left registered; duplicate passes are harmless
		// because srvRespOwed bounds the writes.
	}
}

// clientReadPass consumes rr response bytes on the sender side and records
// completion times.
func (f *flow) clientReadPass() {
	if f.ep == nil || f.cfg.App.Kind != "rr" {
		// Bulk client never receives data.
		var c countingDiscard
		for {
			if _, err := f.ep.Read(&c, tcpip.ReadOptions{}); err != nil {
				return
			}
		}
	}
	var c countingDiscard
	for {
		if _, err := f.ep.Read(&c, tcpip.ReadOptions{}); err != nil {
			break
		}
	}
	if c.n == 0 {
		return
	}
	f.respRecvB += c.n
	now := f.sim.clk.Elapsed()
	for f.respRecvB >= f.cfg.App.RespBytes && len(f.reqSendTs) > 0 {
		f.respRecvB -= f.cfg.App.RespBytes
		sent := f.reqSendTs[0]
		f.reqSendTs = f.reqSendTs[1:]
		f.sim.rec.OnFCT(now, f.id, now-sent)
	}
}

// scheduleNextRequest arms the Poisson arrival timer for the next request.
func (f *flow) scheduleNextRequest() {
	mean := 1 / f.cfg.App.PoissonRate
	wait := time.Duration(f.rrRand.ExpFloat64() * mean * float64(time.Second))
	f.sim.clk.AfterFunc(wait, func() {
		if f.sim.clk.Elapsed() >= f.sim.endT {
			return
		}
		f.sendRequest()
		f.scheduleNextRequest()
	})
}

// sendRequest writes one rr request.
func (f *flow) sendRequest() {
	if f.ep == nil {
		return
	}
	req := make([]byte, f.cfg.App.ReqBytes)
	if _, err := f.ep.Write(bytes.NewReader(req), tcpip.WriteOptions{}); err != nil {
		return // request dropped (buffer full); rare and acceptable
	}
	f.reqSendTs = append(f.reqSendTs, f.sim.clk.Elapsed())
}

var _ = io.Discard
