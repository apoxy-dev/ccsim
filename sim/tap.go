package sim

import (
	"ccsim/probe"
)

// fillSenderTap augments metrics with sender internals (inflight, pacing
// rate, delivery rate, min-RTT, per-flow retransmits) exposed by the ccsim
// netstack patch. Before the patch lands this is a no-op.
func (s *Sim) fillSenderTap(f *flow, m *probe.FlowMetrics) {
}
