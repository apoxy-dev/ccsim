package link

import (
	"testing"
	"time"

	vclock "ccsim/clock"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// mkPkt builds a valid IPv4/TCP packet of total size n with given ports.
func mkPkt(n int, sport, dport uint16, ect bool) []byte {
	if n < 40 {
		n = 40
	}
	b := make([]byte, n)
	ip := header.IPv4(b)
	var tos uint8
	if ect {
		tos = 0x02 // ECT(0)
	}
	ip.Encode(&header.IPv4Fields{
		TOS:         tos,
		TotalLength: uint16(n),
		TTL:         64,
		Protocol:    6,
		SrcAddr:     tcpAddr(10, 0, 0, 1),
		DstAddr:     tcpAddr(10, 0, 0, 2),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	b[20] = byte(sport >> 8)
	b[21] = byte(sport)
	b[22] = byte(dport >> 8)
	b[23] = byte(dport)
	return b
}

func tcpAddr(a, b, c, d byte) tcpip.Address {
	return tcpip.AddrFrom4([4]byte{a, b, c, d})
}

func newTestLink(t *testing.T, clk *vclock.Clock, cfg Config, hooks Hooks) *Link {
	t.Helper()
	if cfg.MakeQdisc == nil {
		cfg.MakeQdisc = func(dir Dir, sink QdiscSink) Qdisc {
			return NewTailDrop(100, 0, sink)
		}
	}
	return New(clk, cfg, 42, hooks)
}

func TestSerializationAndDelay(t *testing.T) {
	clk := vclock.New()
	var delivered []time.Duration
	l := newTestLink(t, clk, Config{
		RateBps: 8_000_000, // 1 byte/us
		Delay:   10 * time.Millisecond,
	}, Hooks{OnDeliver: func(e Event) { delivered = append(delivered, e.T) }})

	// 1000-byte packet: tx time 1ms, delay 10ms => deliver at 11ms.
	l.pipes[Fwd].send(mkPkt(1000, 100, 200, false))
	// Second packet queued behind: tx done at 2ms, deliver at 12ms.
	l.pipes[Fwd].send(mkPkt(1000, 100, 200, false))
	clk.RunUntilIdle()
	if len(delivered) != 2 {
		t.Fatalf("delivered %d packets, want 2", len(delivered))
	}
	if delivered[0] != 11*time.Millisecond || delivered[1] != 12*time.Millisecond {
		t.Fatalf("delivery times %v, want [11ms 12ms]", delivered)
	}
}

func TestTailDropLimit(t *testing.T) {
	clk := vclock.New()
	drops := 0
	l := newTestLink(t, clk, Config{
		RateBps: 8_000_000,
		Delay:   time.Millisecond,
		MakeQdisc: func(dir Dir, sink QdiscSink) Qdisc {
			return NewTailDrop(5, 0, sink)
		},
	}, Hooks{OnDrop: func(e Event) { drops++ }})

	for i := 0; i < 10; i++ {
		l.pipes[Fwd].send(mkPkt(1000, 100, 200, false))
	}
	// Queue holds 5; one is in the serializer; 10 - 1 - 5 = 4 dropped.
	if drops != 4 {
		t.Fatalf("drops=%d, want 4", drops)
	}
	clk.RunUntilIdle()
}

func TestWireLossDeterministic(t *testing.T) {
	run := func() int {
		clk := vclock.New()
		delivered := 0
		l := newTestLink(t, clk, Config{
			RateBps: 80_000_000,
			Delay:   time.Millisecond,
			LossP:   0.3,
		}, Hooks{OnDeliver: func(e Event) { delivered++ }})
		for i := 0; i < 200; i++ {
			l.pipes[Fwd].send(mkPkt(1000, 100, 200, false))
		}
		clk.RunUntilIdle()
		return delivered
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("nondeterministic loss: %d vs %d", a, b)
	}
	if a == 200 || a == 0 {
		t.Fatalf("loss had no effect: delivered=%d", a)
	}
}

func TestCoDelMarksECT(t *testing.T) {
	clk := vclock.New()
	marks, drops := 0, 0
	var ceSeen bool
	l := newTestLink(t, clk, Config{
		RateBps: 8_000_000, // 1000B packet = 1ms tx
		Delay:   time.Millisecond,
		MakeQdisc: func(dir Dir, sink QdiscSink) Qdisc {
			p := DefaultCoDelParams()
			p.ECN = true
			return NewCoDel(1000, 0, p, sink)
		},
	}, Hooks{
		OnMark: func(e Event) { marks++ },
		OnDrop: func(e Event) {
			if e.Reason == DropAQM {
				drops++
			}
		},
		OnDeliver: func(e Event) {},
	})
	l.Classify = func(p *Packet) int { return 0 }

	// Offer 1.1x the service rate: 55 packets every 50ms vs 1 pkt/ms
	// drain, so a standing queue forms and sojourn exceeds 5ms.
	for burst := 0; burst < 40; burst++ {
		b := burst
		clk.AfterFunc(time.Duration(b)*50*time.Millisecond, func() {
			for i := 0; i < 55; i++ {
				pk := &Packet{Data: mkPkt(1000, 100, 200, true), Flow: 0}
				if !l.pipes[Fwd].q.Enqueue(pk, clk.Elapsed()) {
					if !l.pipes[Fwd].busy {
						l.pipes[Fwd].startNext()
					}
				}
			}
		})
	}
	clk.RunUntilIdle()
	if marks == 0 {
		t.Fatalf("CoDel with ECN produced no CE marks (drops=%d)", drops)
	}
	if drops > 0 {
		t.Fatalf("CoDel with ECN dropped %d packets of ECT traffic", drops)
	}
	_ = ceSeen
}

func TestMarkCEChecksum(t *testing.T) {
	pk := &Packet{Data: mkPkt(100, 1, 2, true)}
	if !pk.MarkCE() {
		t.Fatal("MarkCE failed on ECT packet")
	}
	if !pk.CE() {
		t.Fatal("CE bit not set")
	}
	h := header.IPv4(pk.Data)
	if !h.IsChecksumValid() {
		t.Fatal("checksum invalid after CE mark")
	}
	non := &Packet{Data: mkPkt(100, 1, 2, false)}
	if non.MarkCE() {
		t.Fatal("MarkCE succeeded on non-ECT packet")
	}
}

func TestFQCoDelFairShare(t *testing.T) {
	clk := vclock.New()
	perFlow := map[int]int{}
	l := newTestLink(t, clk, Config{
		RateBps: 8_000_000,
		Delay:   time.Millisecond,
		MakeQdisc: func(dir Dir, sink QdiscSink) Qdisc {
			return NewFQCoDel(DefaultFQCoDelParams(), sink)
		},
	}, Hooks{OnDeliver: func(e Event) { perFlow[e.Flow]++ }})
	l.Classify = func(p *Packet) int {
		_, _, _, sp, _, _ := p.FiveTuple()
		return int(sp)
	}

	// Flow 1 offers 4x the packets of flow 2 simultaneously; DRR should
	// still alternate service while both are backlogged.
	for i := 0; i < 400; i++ {
		l.pipes[Fwd].send(mkPkt(1000, 1, 200, false))
	}
	for i := 0; i < 100; i++ {
		l.pipes[Fwd].send(mkPkt(1000, 2, 200, false))
	}
	// After 200ms (200 packets serviced), both flows should have gotten
	// close to 100 each.
	clk.Advance(201 * time.Millisecond)
	if perFlow[2] < 80 {
		t.Fatalf("flow 2 got %d of first 200 slots, want ~100 (flow1=%d)", perFlow[2], perFlow[1])
	}
	clk.RunUntilIdle()
}

func TestREDDropsBeforeFull(t *testing.T) {
	clk := vclock.New()
	aqmDrops, tailDrops := 0, 0
	l := newTestLink(t, clk, Config{
		RateBps: 8_000_000,
		Delay:   time.Millisecond,
		MakeQdisc: func(dir Dir, sink QdiscSink) Qdisc {
			rng := func() float64 { return 0.001 } // always below pa once avg>minth
			return NewRED(1000, REDParams{MinTh: 5, MaxTh: 15, MaxP: 0.1, Wq: 0.2}, rng, sink)
		},
	}, Hooks{OnDrop: func(e Event) {
		if e.Reason == DropAQM {
			aqmDrops++
		} else {
			tailDrops++
		}
	}})
	for i := 0; i < 200; i++ {
		l.pipes[Fwd].send(mkPkt(1000, 100, 200, false))
	}
	clk.RunUntilIdle()
	if aqmDrops == 0 {
		t.Fatal("RED produced no early drops")
	}
	if tailDrops != 0 {
		t.Fatalf("RED hit the hard limit (%d tail drops) before AQM", tailDrops)
	}
}
