// Package naive implements a deliberately congestion-oblivious sender for
// demonstrations. It keeps a large congestion window and paces at a fixed
// 150 Mbps, ignoring ACK-rate, loss, ECN, and RTT feedback.
//
// This is not a deployable congestion-control algorithm. It is the control
// case that makes persistent overload visible: on a 100 Mbps bottleneck the
// sender continues offering 150 Mbps while the queue fills and tail-drops the
// excess.
package naive

import (
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

const (
	// RateBps is the fixed sender pacing rate.
	RateBps int64 = 150_000_000
	// WindowPkts is intentionally above the BDP plus buffer of the lab's
	// default path so ACKs and loss recovery cannot throttle the fixed pacer,
	// while remaining bounded enough for efficient SACK recovery.
	WindowPkts = 8192
)

// Sender is the sender-side surface used by the naive controller.
// tcp.SimSender implements it; tests provide a fake.
type Sender interface {
	SetCwndPkts(int)
	SetPacingRateBps(int64)
	SetSsthresh(int)
}

// Naive is one fixed-rate sender.
type Naive struct {
	s Sender
}

var _ tcp.SimCC = (*Naive)(nil)

// New creates and immediately configures a fixed-rate sender.
func New(s Sender) *Naive {
	n := &Naive{s: s}
	n.holdRate()
	return n
}

// Register wires the controller into the patched netstack under "naive".
func Register() {
	tcp.RegisterSimCC("naive", func(h tcp.SimSender) tcp.SimCC { return New(h) })
}

// holdRate reverses any window changes made by TCP recovery and keeps the
// pacer fixed. TCP still retransmits lost data and processes normal ACKs; it
// just receives no congestion response from this controller.
func (n *Naive) holdRate() {
	n.s.SetCwndPkts(WindowPkts)
	n.s.SetSsthresh(WindowPkts)
	n.s.SetPacingRateBps(RateBps)
}

func (n *Naive) HandleLossDetected() { n.holdRate() }
func (n *Naive) HandleRTOExpired()   { n.holdRate() }
func (n *Naive) Update(int, time.Duration) {
	n.holdRate()
}
func (n *Naive) PostRecovery() { n.holdRate() }
func (n *Naive) OnAck(tcp.SimRateSample) {
	n.holdRate()
}
