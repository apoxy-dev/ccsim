// ccsim patch: synchronous segment dispatch.
//
// Upstream netstack processes inbound TCP segments on a pool of processor
// goroutines (dispatcher.go). That is nondeterministic under ccsim's virtual
// clock: segment processing would race with virtual time advancement.
//
// With SimSynchronousDispatch enabled, every processor.queueEndpoint call
// (the single funnel for all "wake the processor" paths: inbound packets,
// UnlockUser, handshake completion, notifyProcessor) instead processes the
// endpoint's segment queue inline on the calling goroutine — which in ccsim
// is always the single event-loop goroutine driving the virtual clock.
//
// This file is part of the ccsim vendored patch surface; see README.md.

package tcp

// SimSynchronousDispatch makes all TCP segment processing run inline on the
// goroutine that delivers packets / releases endpoint locks, instead of on
// processor goroutines. Set once before creating stacks; not safe to toggle
// while stacks are live.
var SimSynchronousDispatch = false

// processEndpointInline is the synchronous equivalent of one processor loop
// iteration (dispatcher.go processor.start), repeated until the endpoint's
// segment queue is drained or ownership prevents processing.
func processEndpointInline(ep *Endpoint) {
	for !ep.segmentQueue.empty() && !ep.isOwnedByUser() {
		switch state := ep.EndpointState(); {
		case state.connecting():
			handleConnecting(ep)
		case state.connected() && state != StateTimeWait:
			handleConnected(ep)
		case state == StateTimeWait:
			handleTimeWait(ep)
		case state == StateListen:
			handleListen(ep)
		case state == StateError || state == StateClose:
			ep.mu.Lock()
			if st := ep.EndpointState(); st == StateError || st == StateClose {
				ep.drainClosingSegmentQueue()
			}
			ep.mu.Unlock()
			return
		default:
			return
		}
	}
}
