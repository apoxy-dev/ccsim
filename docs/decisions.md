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

## 13. cwnd is grown by acked data, not assigned from the model

The old `setCwnd` recomputed gain*BDP(+2 MSS) and assigned it on every
ACK. The draft's `BBRSetCwnd` is a different control law: cwnd is a
persistent variable that grows by at most `rs.newly_acked` per ACK, the
model target (`BBR.max_inflight`) acts only as a cap, and the snap-down
arm is gated on `full_bw_reached`. Before the pipe is full, cwnd never
decreases — otherwise the first ACK's cold model (tiny bw x min_rtt) cuts
the window it is still trying to measure. Measured on 100 Kbps / 200 ms:
first ACK cut cwnd 10 -> 5 pre-fix; post-fix cwnd holds >= 10 through
Startup (growing to 19 while delivered < InitialCwnd, per the draft's
`C.delivered < C.InitialCwnd` arm) and snaps to the target only at
full-pipe detection. Alongside it, the rest of audit finding 2:
`BBRInitPacingRate` (pace the IW over the handshake SRTT, 1 ms fallback,
instead of an unpaced first flight), cwnd_gain 2.25 in ProbeBW:UP (draft
raises it from the default 2), DOWN's cwnd cap is inflight_hi rather than
0.85-headroom (draft `BBRBoundCwndForModel` reserves headroom for
CRUISE/ProbeRTT), and BBRSaveCwnd/BBRRestoreCwnd around loss recovery,
RTO and ProbeRTT so exits restore the last good window instead of
regrowing from the clamp. BDP truncation (sub-MSS, dominated by the
2-MSS allowance) and the extra_acked filter (receiver never aggregates
beyond delayed ACKs; fixed 2-MSS allowance, decisions §7) stay as
documented deviations.

Consequence, characterized in docs/validation.md ("late-joiner
convergence"): without the non-conformant jump-to-target, a bbr late
joiner reallocates share one probe cycle at a time — 60 s to 35% share at
the test seed (was 4 s), matching BBRv2/v3's documented slow-reallocation
trade-off; the bound is pinned at 90 s. Steady-state fairness and the
operating-point surface are unchanged. Regression pinned by
`TestCwndGrowByAckedControlLaw`; goldens regenerated (all seven
bbr-involving presets shift; cubic presets byte-identical).

## 14. Rate-sample signals: mark-time loss, tx_in_flight, delivered volume

Audit finding 3: the sample plumbing exposed post-ACK pipe occupancy and
a cumulative retransmission counter, and BBR consumed both with the
wrong temporal meaning. Three signal fixes, all inside the `ccsim_*`
patch files:

- **Loss at mark time.** `C.lost` (`SimRateSample.LostBytesCum`) now
  counts a segment when the RFC 6675 scoreboard first implies it is lost
  (tallied inside the `ccsimSetPipe` walk that already runs per
  SACK-carrying ACK, original transmissions only) plus a mark-everything
  pass on RTO before the scoreboard is expunged — not when data is
  retransmitted. Retransmit-time counting was late by a round trip and,
  worse, a retransmitted segment's range stays `IsLost` until the
  retransmit is SACKed, so counting without the `xmitCount == 1` guard
  double-counted every loss (measured: 1% Bernoulli loss read as ~2%,
  sitting exactly on BBR's LossThresh — every probe aborted and the
  model death-spiraled to ~30 Mbps on a 100 Mbps link). A lost
  retransmission is recounted by the RTO path (its `lostCounted` bit is
  cleared on re-transmit).
- **tx_in_flight.** Each segment is stamped at (re)transmit with
  `C.inflight` including itself (`P.tx_in_flight`) and the then-current
  `C.lost` (`P.lost`). The sample carries `TxInflight` and
  `LostBytes = C.lost - P.lost`, so the draft's `IsInflightTooHigh`
  (`RS.lost > RS.tx_in_flight * LossThresh`) and the `inflight_hi` latch
  (`max(RS.tx_in_flight, beta*target)`) now use the operating point that
  *sent* the losing data, not the pipe left after the ACK that revealed
  it. The per-lost-packet `BBRHandleLostPacket`/lost-prefix
  interpolation is still approximated at rate-sample granularity.
- **inflight_latest is delivered volume.** The round's
  `BBR.inflight_latest` is now `max(RS.delivered)` — the largest volume
  actually delivered over one sample's flight — rather than max pipe
  occupancy, so the `inflight_lo` floor is a demonstrated delivery
  volume (draft `BBRUpdateLatestDeliverySignals`).

Two consumers gained their missing draft gates: `inflight_hi` only grows
in PROBE_UP while `C.is_cwnd_limited` and cwnd is pressing against it
(`BBRProbeInflightLongtermUpward`; the new `IsCwndLimited` bit latches
"sendData ended with the window full"), and the loss abort applies only
to samples the probe itself transmitted, at most once per probe
(`BBR.bw_probe_samples` + a delivered-count mark at REFILL entry; the
draft scopes the gate to packets "sent in one of the accelerating
phases"). Without that scoping, residual mark-time losses from CRUISE
aborted every probe on its first ACK.

Net effect on the random-loss preset (100 Mbps, 40 ms, 1% loss): 89.5
Mbps at 47 ms mean SRTT, vs 87.5 Mbps at 50 ms before — same throughput,
smaller standing queue, and the loss signals now mean what the draft
says they mean. State-machine occupancy checks keep using post-ACK
inflight: that *is* the draft's `C.inflight` at ACK-processing time
(prior_in_flight is a Linux EDT implementation detail, not plumbed).

The validation surface moved substantially (all re-measured, tables and
findings updated in docs/validation.md): RTT-fairness exponent -1.17 →
0.24 (the pathological long-RTT dominance was a loss-signal artifact),
shallow-buffer coexistence 73/79 → 94/96 Mbps (meets the original 85%
target; finding 3 there re-characterized as resolved), late-joiner
convergence 60 s → 8 s (the provisional 90 s pin from §13 tightened to
30 s — retransmit-time loss counting had been suppressing the joiner's
probes), sub-BDP RTO rate 36 → 13/min (goodput 82% → 67%), and small-N
intra-bbr aggregates dip (N=2 81.5 Mbps, pinned at 80) because the
short-term bounds now floor at demonstrated delivered volume. Goldens
regenerated (four presets shift: bbr-single, coexist-1bdp, determinism,
fairness; the lossless/ECN-only bbr presets and all cubic presets are
byte-identical).

## 15. ProbeBW's feedback machine: adapt-on-every-ACK, ack phases, exponential inflight_hi

Audit finding 4: the upper-bound feedback machinery around ProbeBW was a
skeleton — loss/ECN gates checked only while the state was UP, feedback
arriving in DOWN silently dropped, `inflight_hi` growing linearly and
without preconditions, UP ending on an invented fixed schedule (2 rounds
at 1.25×BDP or an 8-round cap), and a never-read `ackPhaseIsUp` where the
draft's four-value ack-phase tracker belonged. Replaced with the draft's
structure:

- **`BBRAdaptLongTermModel` runs on every ACK once the pipe is full**, in
  every state: a too-high sample cuts `inflight_hi` (once per probe, via
  `bw_probe_samples` + the REFILL delivered mark from §14) in whatever
  state its ACKs arrive — probe-caused loss usually surfaces after the
  state machine has already flipped to DOWN. Safe samples adapt upward:
  `inflight_hi = max(inflight_hi, RS.tx_in_flight)` in any ProbeBW state.
- **Ack phases time the max-bw filter advance.** The filter now advances
  at `ACKS_PROBE_STOPPING && round_start` — one round *into* DOWN, when
  the probe's last samples have landed — instead of at DOWN entry, so the
  probe's peak delivery rate is fully credited to the closing window
  before turnover.
- **Exponential `inflight_hi` growth** (`BBRRaiseInflightLongtermSlope`):
  the growth rate doubles each UP round (1 MSS << round per cwnd acked)
  instead of a flat ~1 MSS/round, still gated on `is_cwnd_limited` with
  cwnd pressing the bound. Long probes escalate like slow start; after a
  deep cut the bound recovers in rounds, not probe cycles.
- **UP ends when it stops learning** (`BBRIsTimeToGoDown`): the full-bw
  plateau estimator is reset and reseeded at UP entry and runs during the
  probe; UP exits on `full_bw_now` (3 rounds of <25% max-bw growth). While
  the flow is cwnd-limited pressing `inflight_hi`, the estimator resets
  instead — an artificial plateau imposed by the bound must not read as
  "pipe full". This required splitting the draft's `full_bw_now` (per-probe
  verdict) from `full_bw_reached` (lifetime latch, our `filledPipe`);
  startup exit uses the same estimator unchanged.

Not adopted: `prev_probe_too_high` and `stopped_risky_probe` are
tcp_bbr.c internals absent from draft-03, and per-lost-packet
`BBRHandleLostPacket` lost-prefix interpolation stays approximated at
rate-sample granularity (§14).

Validation deltas (tables regenerated in docs/validation.md): the
mid-buffer coexistence starvation is gone — bbr share vs cubic at
2×/4×BDP went 23%/19% → 51%/48% (the linear growth could never rebuild
`inflight_hi` against cubic between cuts), and at sub-BDP buffers bbr now
takes the larger share (68-71%), consistent with published BBR behavior
in small buffers. The 16×BDP starvation (1% share) stands — that is the
windowed min_rtt under cubic's permanent standing queue, not the probe
machine. Intra-bbr aggregates recovered (N=2 81.5 → 92.0 Mbps, the §14
dip; pin raised 80 → 85), late-joiner convergence tightened to 5-9 s
over seeds 34-37, and the RTT-fairness exponent moved 0.24 → -0.69:
long-RTT flows now hold the larger share, which is BBR's documented
inverse-RTT bias (probe magnitude scales with BDP), not a defect — the
draft-faithful machine restored a known trait. Random-loss gives back a
little (89.5 → 87.5 Mbps): safe samples raising `inflight_hi` to
`tx_in_flight` keep the operating point nearer the loss gate under
random loss. The one cell that ran hotter is the operating-point grid's
smallest BDP (10 Mbps × 10 ms, ~17 packets): 1.23 → 1.39×BDP inflight —
MSS-granular `inflight_hi` growth and the 4-packet MinPipeCwnd floor are
large fractions of a BDP that small; characterized in validation.md
finding 6. Goldens regenerated: every bbr-carrying preset shifts (probe
cadence changed), cubic-only presets are byte-identical.
