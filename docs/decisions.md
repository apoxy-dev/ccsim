# Design decisions

Notes recorded when netstack internals or measured behavior forced a design
change, per the project working style.

## 1. Synchronous TCP segment dispatch (determinism)

Upstream netstack processes inbound TCP segments on a pool of processor
goroutines (`transport/tcp/dispatcher.go`). Under a virtual clock this is
nondeterministic: segment processing races with virtual time advancement.
All processor wakeups funnel through `processor.queueEndpoint`, so the patch
adds a `SimSynchronousDispatch` mode that processes the endpoint's segment
queue inline on the calling goroutine (`ccsim_sync.go`). Every packet
delivery, timer fire and application write therefore runs to completion on
the single event-loop goroutine before virtual time advances again.

A per-endpoint reentrancy guard (`ccsimInlineActive`) is required: when a
passive handshake is completed by a data-bearing segment (the bare third
ACK was lost), upstream re-enqueues the segment and pokes the dispatcher
from inside `handleConnecting`, which still holds `ep.mu` via a
non-reentrant `TryLock`. Without the guard the inline loop spins forever
against its own lock and the simulation hangs
(`TestLossyHandshakeNoLivelock`).

## 2. CC integration surface (registry, rate samples, pacing)

netstack's `congestionControl` interface is cwnd-only: no delivery-rate
samples, no pacing, no ECN feedback, and the CC name switch is hard-coded.
Rather than fork the sender, `ccsim_cc.go` wraps every CC (stock or
registered) in `ccsimWrapper` and adds:

- `RegisterSimCC(name, factory)` consulted by `initCongestionControl`, and
  the protocol's available-CC list extended at stack creation.
- The wrapper is installed before a registered CC factory runs, allowing a
  controller to set its initial pacing rate before the first data burst.
- Delivery-rate estimation in the style of draft-cheng-iccrg-delivery-rate-
  estimation: per-segment `(delivered, delivered_time, first_sent)` stamps
  at transmit, one `SimRateSample` per ACK computed in a pre/post wrapper
  around `handleRcvdSegment` (upstream body renamed `handleRcvdSegmentInner`,
  a one-line diff).
- Pacing enforced in `sendData`'s transmit loop: a virtual-clock timer gate
  (`ccsimPacingAllows` / `ccsimPacingCharge`). Granularity is one send
  quantum: `min(pacing_rate * 1ms, 64KB)`, at least 2×MSS. Retransmissions
  in recovery bypass pacing (they are triggered outside `sendData`); this is
  a simplification, documented, and only affects loss episodes.

## 3. ECN path

netstack has no ECN support (gvisor.dev/issue/995). The minimal path added:

- `SimAllowECTTOS` lets endpoints set ECT(0) in the TOS byte (upstream masks
  the ECN bits in SetSockOpt).
- The receiver notes CE marks on arriving data segments (`rcv.go` one-liner
  into `ccsimNoteCE`) and echoes ECE on the next ACK-bearing segment,
  **per-ACK** (ACE/DCTCP-style) rather than RFC 3168 latched ECE+CWR. BBRv3's
  ECN alpha wants per-ACK CE fractions, and the latched handshake would only
  reduce fidelity here; documented deviation.
- Stock CCs (cubic/reno) get an RFC 3168-style response: at most one
  `HandleLossDetected` + cwnd=ssthresh cut per SRTT when ECE arrives.

## 4. Cubic fractional cwnd accumulation

netstack's cubic truncates the congestion window to an integer packet count
on every `Update`. At cwnd ≈ 500 packets the per-ACK increment is ≪ 1
packet, so all growth was lost to truncation and the window stalled for
multiple seconds (a step function with ~2×K period). The patch carries the
fractional remainder across calls (`ccsimFrac`), restoring the RFC 8312
trajectory. Without this, the cubic-single sawtooth assertion cannot pass.

## 5. Delayed ACKs under synchronous dispatch

Upstream netstack sends one ACK per *batch* of processed segments; batches
form naturally under asynchronous processing. Synchronous dispatch makes
every batch size 1, yielding one ACK per data segment (~2× packet count,
and ~30% of simulation CPU). The patch adds a classic delayed-ACK policy in
that path only (`SimSynchronousDispatch`): ACK immediately at ≥2 full
segments unacknowledged, else within 5 ms. All out-of-order/FIN/window
paths still ACK immediately.

## 6. FMA and cross-build float parity

Native arm64 fuses `a*b + c` into FMA; wasm does not. Any float
multiply-add on the packet path eventually diverges the streams (first seen
as a 1 ns SRTT difference in netstack's RFC 7323 appendix-G smoothing).
All fusable sites on simulation paths — netstack RTT smoothing, cubic
window math, RED's EWMA, BBR's ECN alpha — force intermediate rounding with
explicit `float64(...)` conversions, which the Go spec defines as a rounding
barrier. Byte parity native↔wasm is enforced by test.

## 7. BBRv3 adaptations (see also bbr/bbr.go package comment)

- **Windowed min_rtt**: the draft's min_rtt filter is a windowed minimum.
  An early implementation pinned the historical minimum forever, which under
  a cubic competitor's standing queue drove `cwnd = 2·bw·min_rtt` far below
  `bw·RTT_actual` — a positive-feedback starvation spiral (BBR share → 2%).
  The windowed filter (4×2.5s buckets) fixes coexistence (39-48% share).
- **Max-bw filter aging**: bucket turnover is rate-limited to ≥2 s (the
  draft window is *two probe cycles* of 2-3 s; contested mini-cycles would
  otherwise erase bandwidth memory), plus a 1 s aging backstop in
  CRUISE/DOWN so a rate drop that never causes loss (cwnd-capped inflight
  fitting the queue, as in the rate-step scenario) is forgotten within ~2 s.
- **DOWN exit**: also exits to REFILL when the probe timer expires; the
  inflight≤BDP drain target is unreachable when a competitor maintains a
  standing queue (matches the reference implementation's
  `bbr_check_time_to_probe_bw` behavior in DOWN).
- **Excess-queue drain from CRUISE**: inflight > 1.5×BDP re-enters DOWN to
  deplete queue built at a stale higher rate.
- Loss is observed via the endpoint's cumulative retransmit counter
  (retransmitted segments × MSS approximates lost bytes); netstack does not
  expose per-packet loss marks without much deeper surgery.
- **Alignment pass (post-review, vs google/bbr tcp_bbr.c)**: rounds are
  packet-timed and anchored at the sample segment's transmit time
  (`SimRateSample.PriorDelivered`), matching `prior_delivered` semantics —
  ack-time anchoring degenerated to one round per ACK whenever inflight
  drained to zero. `inflight_hi` probing accumulates acked bytes against an
  inflight_hi-bytes threshold (a unit bug previously grew it ~MSS× too
  fast), and `bw_probe_up_acks` is reset on REFILL entry. Startup's ECN
  exit requires two consecutive high-CE rounds (`bbr_full_ecn_cnt`).
  ProbeRTT exit requires a packet-timed round at the reduced window in
  addition to the 200 ms hold. `bw_lo`'s floor is `max(1, bw_lo)` — an
  earlier 0.2·max_bw floor silently neutered the beta cuts on >5× rate
  drops. BBR's probe-cycle jitter derives from the scenario seed via a
  named PCG sub-stream (seed, `0xBB3<<32 | port`), not from the port
  alone.

## 8. RACK disabled, checksum offload claimed

RACK's per-ACK segment scan dominated CPU at large cwnd (30% of runtime);
classic SACK recovery (RFC 6675) is used instead — deterministic and
behaviorally adequate for these scenarios. The link endpoints claim TX/RX
checksum offload: packets never cross a real wire, so TCP checksum
computation/validation would only burn simulation CPU. (IPv4 header
checksums are still maintained, including after CE re-marking.)

## 9a. Validation-suite harness additions (no vendor changes)

Added for the validation suite (docs/validation.md), all in ccsim packages:

- **`Sim.Close()`** destroys both netstacks. Each stack pins ~10 goroutines
  which kept every replaced sim reachable (~29 MB/run); the wasm page
  leaked a full sim per preset load until `load()` started closing the
  previous session. Regression-tested natively
  (`TestSimCloseReleasesResources`) and in node (`wasm/memtest.mjs`).
- **Ring-buffer fifo** in link/qdisc.go: the slide-forward slice allocated
  once per packet whenever a queue oscillated around empty (the common
  uncongested case — measured exactly 1.0 allocs/packet). Byte-identical
  streams, now 0.0 allocs/packet.
- **Socket-buffer auto-sizing** (`bufSizeFor`): 2x(BDP + bottleneck queue),
  floored at the old fixed 32 MB. All original presets sit below the floor
  (behavior unchanged); 1 Gbps x 300 ms scenarios no longer go silently
  window-limited.
- **Reverse flows** (`flows[].reverse`) for two-way traffic, and the
  reverse direction gets the scenario's qdisc (instead of the deep ACK
  FIFO) only when reverse data flows exist — with the deep FIFO, the
  reverse bulk flow enjoyed an effectively unlimited buffer and starved
  the forward flow (30/93 Mbps split; symmetric queues give 77/77).
- **`link.drop_next` inject path** and **`rev_owd_ms`** for scripted
  single-loss experiments (cubic curve fit) and asymmetric paths.

## 9. SetPipe rewritten as a single-pass cursor walk

Profiling the bufferbloat preset showed 87% of the run's CPU inside
upstream `sender.SetPipe` (RFC 6675 pipe), nearly all of it in one ~2 s
recovery episode: a ~7000-packet window with ~1100 scoreboard holes means
two btree range queries per SMSS chunk per ACK, and `IsRangeLost`
additionally counts up to 100 ranges per chunk — O(cwnd²)-class work per
recovery. Since the chunk walk ascends in sequence space and scoreboard
ranges are disjoint and sorted, `ccsimSetPipe` (ccsim_cc.go) snapshots the
scoreboard once per call and answers both queries with a monotonically
advancing cursor plus precomputed suffix block/byte tallies —
O(cwnd + ranges) per ACK. The computation is semantically identical to
upstream (verified: byte-identical sample streams before/after); it is a
performance patch only. Bufferbloat wall time: 19.7 s → 2.1 s. The same
cost exists in real gVisor during SACK recovery on large-window paths;
an incremental-pipe variant would be the proper upstream fix.

## 10. Jitter is a correlated delay walk, not iid per-packet noise

`link.jitter_ms` was first implemented the netem way: each packet draws an
independent uniform extra delay, with a FIFO clamp to prevent reordering.
That model is wrong in a way that matters: for streams paced faster than
the jitter magnitude (ACKs at 250 µs spacing vs 5 ms jitter), the clamp
turns white-noise delays into same-instant delivery batches — measured 11
ACKs at one timestamp — and the resulting ACK-compression bursts overflow
queues that survive on real jittery paths. Cubic lost 9% goodput and took
3.6k retransmits at just 5 ms; real WANs carry far more delay variance
without that.

Real jitter is cross-traffic queueing at other hops, which drifts over
tens of milliseconds and delays neighboring packets almost identically.
The model now follows that: the delay offset is a piecewise-linear walk —
a new uniform [0, jitter) target every 100 ms, interpolated between — so
spacing is approximately preserved (slew rate ≤ jitter/100 ms) and the
FIFO clamp almost never binds. With the correlated model cubic is
unaffected at 5 ms and degrades gradually from ~20 ms, which matches
expectations for a 40 ms-RTT path. The walk consumes its own seeded PCG
sub-stream, advanced lazily by sim time, so enabling jitter never
perturbs the loss sequence and jitter=0 draws nothing (byte-identical
streams).

## 11. BBR rate samples carry pipe-based inflight, not the sequence span

`SimRateSample.InflightBytes` originally reported `SND.NXT - SND.UNA`.
Under persistent random loss the flow lives in SACK recovery with SND.UNA
pinned behind a hole, so that span overstates packets in the network by
the width of the SACK holes (measured 964 pkt raw vs 645 cwnd at 0.35%
loss). BBR's ProbeBW:DOWN exit compares inflight to 1.0x estimated BDP —
with the inflated signal the drain "never completes", and the phase
histogram showed the flow wedged in DOWN 84% of the time, holding a
cwnd-limited standing queue (mean 253 of 350 pkt) that pegged the fig 1
operating point at the loss cliff.

Linux BBRv3 (google/bbr, v3 branch) never uses the sequence span:
`rs.prior_in_flight = tcp_packets_in_flight(tp)` = packets_out -
(sacked_out + lost_out) + retrans_out, further reduced by the EDT
correction in `bbr_packets_in_net_at_edt()`. gVisor already maintains the
equivalent quantity: `snd.Outstanding` is kept per send/ack and
recomputed as the RFC 6675 pipe during recovery (§9), so the sample now
carries `Outstanding * MSS`. Effect at 0.35% loss / 1.05xBDP buffer:
DOWN occupancy 84% -> 39% (CRUISE 50%), mean queue 253 -> 128 pkt, srtt
57 -> 48 ms; goodput 83 -> 68 Mbps because bw_lo/inflight_lo loss bounds
now govern from cruise instead of being masked by a permanently full
pipe. Clean-path presets shift within noise; goldens regenerated (see
golden_changelog.md), conformance suite unchanged.

## 12. Startup is a bandwidth probe: no lower-bound cuts, pacing only ratchets up

Two coupled omissions let a single packet loss during Startup collapse
the ramp. First, `adaptLowerBounds` exempted only ProbeBW REFILL/UP, but
both the draft's `BBRAdaptLowerBounds` pseudocode ("if (BBR.state ==
Startup) return") and the reference's `bbr_is_probing_bandwidth()` exempt
Startup too. Second, `setPacing` applied the gain*bw rate unconditionally,
while the reference (`bbr_set_pacing_rate`) refuses to lower the pacing
rate until `bbr_full_bw_reached()`.

The failure chain: one loss in a startup round set bw_lo =
max(bw_latest, 0.7*bw_lo), and early in startup bw_latest is a
still-ramping round sample far below the path capacity, so bw_lo pinned
low; bw() = min(max_bw, bw_lo) dragged pacing (and via inflight_lo, cwnd)
down with it; the throttled delivery plateaued max_bw, tripping the
three-round full-bw startup exit at a fraction of the real bandwidth —
and bw_lo persists until the next REFILL, seconds away. Measured on
100 Mbps / 40 ms / 10k-pkt buffer with exactly one drop at 0.2 s
(link.drop_next): goodput over 5 s was 28.2 Mbps pre-fix vs 90.4 post-fix
— identical to the clean run. The intended loss reaction in Startup is
the separate six-loss-event exit rule (`checkFullPipe`), which one loss
correctly does not satisfy.

Fixes: Startup added to the `adaptLowerBounds` exemption, and `setPacing`
now ratchets — a lower rate is ignored until `filledPipe`. `SimProbe`
exports the rate actually in force. Regression pinned by
`TestStartupSingleLossKeepsRamping` (fails pre-fix at the first round
after the loss). Goldens regenerated for the three bbr presets whose
startup bytes shifted.
