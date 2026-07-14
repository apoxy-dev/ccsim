package naive

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

type fakeSender struct {
	cwnd      int
	ssthresh  int
	pacingBps int64
}

func (s *fakeSender) SetCwndPkts(v int)        { s.cwnd = v }
func (s *fakeSender) SetSsthresh(v int)        { s.ssthresh = v }
func (s *fakeSender) SetPacingRateBps(v int64) { s.pacingBps = v }

func TestHoldsFixedRateAcrossCongestionSignals(t *testing.T) {
	s := &fakeSender{}
	n := New(s)
	assertFixed := func(where string) {
		t.Helper()
		if s.cwnd != WindowPkts || s.ssthresh != WindowPkts || s.pacingBps != RateBps {
			t.Fatalf("%s: cwnd=%d ssthresh=%d pacing=%d, want %d/%d/%d",
				where, s.cwnd, s.ssthresh, s.pacingBps, WindowPkts, WindowPkts, RateBps)
		}
	}
	assertFixed("new")

	for name, signal := range map[string]func(){
		"loss":     n.HandleLossDetected,
		"rto":      n.HandleRTOExpired,
		"update":   func() { n.Update(10, 40*time.Millisecond) },
		"recovery": n.PostRecovery,
		"ack":      func() { n.OnAck(tcp.SimRateSample{}) },
	} {
		s.cwnd, s.ssthresh, s.pacingBps = 2, 2, 1
		signal()
		assertFixed(name)
	}
}
