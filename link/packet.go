package link

import (
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// Packet is a serialized IP packet traversing the link model.
type Packet struct {
	// Data is the full serialized IP packet.
	Data []byte
	// Flow is the simulator flow id (-1 if unclassified).
	Flow int
	// EnqueuedAt is the virtual time the packet entered the queue.
	EnqueuedAt time.Duration
	// seq orders packets within FQ-CoDel flow queues (assigned at enqueue).
	seq uint64
}

// Size returns the wire size in bytes.
func (p *Packet) Size() int { return len(p.Data) }

// ECT reports whether the packet is ECN-capable transport (ECT(0)/ECT(1)).
func (p *Packet) ECT() bool {
	if len(p.Data) < header.IPv4MinimumSize {
		return false
	}
	return tosOf(p.Data)&0x3 != 0
}

// MarkCE sets the CE codepoint on an ECT packet and fixes the IPv4 checksum.
// Returns false if the packet is not ECT (caller should drop instead).
func (p *Packet) MarkCE() bool {
	if !p.ECT() {
		return false
	}
	h := header.IPv4(p.Data)
	tos, _ := h.TOS()
	tos |= 0x3
	h.SetTOS(tos, 0)
	h.SetChecksum(0)
	h.SetChecksum(^h.CalculateChecksum())
	return true
}

// CE reports whether the CE codepoint is set.
func (p *Packet) CE() bool {
	if len(p.Data) < header.IPv4MinimumSize {
		return false
	}
	return tosOf(p.Data)&0x3 == 0x3
}

// FiveTuple extracts (srcIP, dstIP, proto, srcPort, dstPort) for hashing and
// flow classification. ok is false for non-TCP/UDP or malformed packets.
func (p *Packet) FiveTuple() (src, dst [4]byte, proto uint8, sport, dport uint16, ok bool) {
	if len(p.Data) < header.IPv4MinimumSize {
		return
	}
	h := header.IPv4(p.Data)
	if !h.IsValid(len(p.Data)) {
		return
	}
	copy(src[:], h.SourceAddressSlice())
	copy(dst[:], h.DestinationAddressSlice())
	proto = uint8(h.Protocol())
	hl := int(h.HeaderLength())
	if proto != 6 && proto != 17 { // TCP, UDP
		return src, dst, proto, 0, 0, true
	}
	if len(p.Data) < hl+4 {
		return
	}
	sport = uint16(p.Data[hl])<<8 | uint16(p.Data[hl+1])
	dport = uint16(p.Data[hl+2])<<8 | uint16(p.Data[hl+3])
	return src, dst, proto, sport, dport, true
}

// hash5 returns a deterministic FNV-1a hash of the 5-tuple.
func (p *Packet) hash5() uint32 {
	src, dst, proto, sp, dp, ok := p.FiveTuple()
	if !ok {
		return 0
	}
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	step := func(b byte) {
		h ^= uint32(b)
		h *= prime32
	}
	for _, b := range src {
		step(b)
	}
	for _, b := range dst {
		step(b)
	}
	step(proto)
	step(byte(sp >> 8))
	step(byte(sp))
	step(byte(dp >> 8))
	step(byte(dp))
	return h
}

func tosOf(data []byte) uint8 {
	t, _ := header.IPv4(data).TOS()
	return t
}
