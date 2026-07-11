package sim

import (
	"fmt"
	"math/rand"

	vclock "ccsim/clock"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

const (
	nicID    = tcpip.NICID(1)
	sndAddr4 = "\x0a\x00\x00\x01" // 10.0.0.1
	rcvAddr4 = "\x0a\x00\x00\x02" // 10.0.0.2

	// Buffer sizes chosen so window never limits the CC in any preset
	// (bufferbloat needs ~10 MB of inflight).
	bufSize = 32 << 20
)

var (
	senderAddr   = tcpip.AddrFrom4([4]byte{10, 0, 0, 1})
	receiverAddr = tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
)

// detReader is a deterministic io.Reader used as the stack's SecureRNG.
type detReader struct{ s uint64 }

func (r *detReader) Read(p []byte) (int, error) {
	for i := range p {
		// xorshift64*
		r.s ^= r.s >> 12
		r.s ^= r.s << 25
		r.s ^= r.s >> 27
		p[i] = byte((r.s * 0x2545F4914F6CDD1D) >> 56)
	}
	return len(p), nil
}

func init() {
	// Determinism requirement: all TCP segment processing must happen on
	// the event-loop goroutine (see the ccsim patch in
	// vendor/.../transport/tcp/ccsim_sync.go).
	tcp.SimSynchronousDispatch = true
}

// newStack builds one netstack instance bound to a link endpoint.
func newStack(clk *vclock.Clock, seed int64, ep stack.LinkEndpoint, addr tcpip.Address) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
		Clock:              clk,
		RandSource:         rand.NewSource(seed),
		SecureRNG:          &detReader{s: uint64(seed)*0x9E3779B97F4A7C15 + 1},
	})
	if err := s.CreateNIC(nicID, ep); err != nil {
		return nil, fmt.Errorf("sim: CreateNIC: %s", err)
	}
	pa := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: 24},
	}
	if err := s.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("sim: AddProtocolAddress: %s", err)
	}
	s.SetRouteTable([]tcpip.Route{{
		Destination: header4subnet(),
		NIC:         nicID,
	}})

	// SACK on; large fixed buffers.
	sackOpt := tcpip.TCPSACKEnabled(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sackOpt); err != nil {
		return nil, fmt.Errorf("sim: enabling SACK: %s", err)
	}
	sndBuf := tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: bufSize, Max: bufSize}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sndBuf); err != nil {
		return nil, fmt.Errorf("sim: send buffer option: %s", err)
	}
	rcvBuf := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: bufSize, Max: bufSize}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcvBuf); err != nil {
		return nil, fmt.Errorf("sim: receive buffer option: %s", err)
	}
	// Disable RACK: its per-ACK segment scan dominates CPU at large cwnd
	// and classic SACK recovery is sufficient (and deterministic) here.
	recovery := tcpip.TCPRecovery(0)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &recovery); err != nil {
		return nil, fmt.Errorf("sim: recovery option: %s", err)
	}
	return s, nil
}

func header4subnet() tcpip.Subnet {
	sub, err := tcpip.NewSubnet(
		tcpip.AddrFrom4([4]byte{10, 0, 0, 0}),
		tcpip.MaskFromBytes([]byte{255, 255, 255, 0}),
	)
	if err != nil {
		panic(err)
	}
	return sub
}
