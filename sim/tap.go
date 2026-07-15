package sim

import (
	"ccsim/probe"

	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// fillSenderTap augments metrics with sender internals (inflight, pacing
// rate, delivery rate, per-flow retransmits, CC probe state) exposed by the
// ccsim netstack patch.
func (s *Sim) fillSenderTap(f *flow, m *probe.FlowMetrics) {
	info, ok := tcp.SimSenderInfo(f.ep)
	if !ok {
		return
	}
	m.InflightBytes = float64(info.InflightBytes)
	m.RTTSample = info.RTTSample
	m.PacingBps = float64(info.PacingBps)
	m.DeliveryBps = float64(info.DeliveryBps)
	m.Retransmits = info.RetransSegs
	m.RTOs = info.RTOs
	m.LossEvents = info.LossEvents
	m.IdleRestarts = info.IdleRestarts
	if info.HasCCProbe {
		p := info.CCProbe
		m.CCState = p.State
		m.MinRTT = p.MinRTT
		if p.DeliveryBps > 0 {
			m.DeliveryBps = float64(p.DeliveryBps)
		}
		if p.PacingBps > 0 {
			m.PacingBps = float64(p.PacingBps)
		}
	}
}
