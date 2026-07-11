# ccsim — deterministic TCP congestion control simulator

An event-driven TCP congestion control simulation harness built on gVisor's
netstack (`gvisor.dev/gvisor/pkg/tcpip`). Two full netstack instances
(sender / receiver) are connected through a configurable bottleneck link
model (rate, delay, loss, pluggable queue disciplines, ECN). Includes a
from-scratch **BBRv3** implementation registered alongside the stock Cubic,
a binary metric sample stream, a CLI runner, and a wasm build whose output
is **byte-identical** to the native build.

```
clock/      virtual clock implementing tcpip.Clock (single timer heap)
link/       bottleneck model: token-bucket rate, delay, seeded loss,
            tail-drop / RED / CoDel / FQ-CoDel, ECN CE marking, telemetry
bbr/        BBRv3 (draft-ietf-ccwg-bbr-03) as a netstack congestion control
probe/      per-flow taps, sample records, summaries, windowed analysis
scenario/   ScenarioConfig JSON model, validation, presets
sim/        harness: stacks, flow drivers, event loop, live-settable params
stream/     20-byte binary sample records; Go encoder + JS reference decoder
cmd/ccsim/  CLI runner
wasm/       thin wasm entry + worker glue + node parity runner + smoke page
scenarios/  preset scenario JSON files
docs/       decisions.md — design notes for every forced deviation
```

## Running

```sh
go test ./...                                   # everything incl. 10 smoke scenarios
go build -o ccsim ./cmd/ccsim
./ccsim -preset bufferbloat -summary            # human table
./ccsim -scenario scenarios/rate-step.json -out run.bin -json
GOOS=js GOARCH=wasm go build -o wasm/main.wasm ./wasm
node wasm/parity.mjs wasm/main.wasm wasm/wasm_exec.js scenarios/cubic-single.json out.bin
node stream/decoder_test.mjs                    # JS decoder unit test
```

Browser demo: build `wasm/main.wasm` as above, serve the repo root
(`python3 -m http.server`), open `/wasm/index.html?preset=bufferbloat`.
The sim runs in a worker in **streaming mode** (flat out, yielding between
250 ms-sim batches) and the charts — srtt, throughput, cwnd, queue depth —
render progressively as sample chunks arrive, so there is no wait for the
run to finish. The rate / owd / loss / queue sliders mutate the live sim
mid-run via `set()`. Rendering uses per-pixel min/max binning over the
full-resolution stream, so loss spikes and sawtooth teeth survive
decimation. Self-contained page, no chart library.

The sim runs in **batch** mode (flat out) or **paced** mode (the worker glue
drives `step()` on a wall-clock timer at a configurable sim/real ratio; same
event loop). Live-settable while running: `link.rate_mbps`, `link.loss`,
`link.owd_ms`, `link.queue.limit_pkts|limit_bytes`. CC choice, flow set and
queue discipline are load-time only.

## Performance (acceptance targets)

Measured on an Apple M-series laptop, `cubic-single` preset (30 s sim,
single flow, 100 Mbps, 1 ms sampling):

| build | wall time | target |
|---|---|---|
| native (arm64) | **1.4 s** | < 2 s |
| wasm under node v26 | **7.6 s** | < 8 s |

## Determinism

Same scenario + same seed ⇒ byte-identical sample streams across runs and
across native/wasm builds (enforced by `TestScenarioDeterminism` and
`TestScenarioWasmParity`). Ingredients:

- One virtual clock; every event (netstack timers, link events, app writes,
  sampling ticks, scenario injections) lives on one min-heap with stable
  FIFO tie-breaking. There is exactly one source of time.
- All netstack TCP processing forced inline on the event-loop goroutine
  (synchronous dispatch patch, below). No goroutine ever races the clock.
- All randomness derives from the scenario seed via named PCG sub-streams:
  link loss fwd/rev, RED decisions, rr arrivals per flow, per-stack netstack
  RNG/ISN sources, per-flow BBR probe jitter (named sub-stream: scenario
  seed + flow port).
- Explicit `float64` conversions block FMA fusion at every float
  multiply-add on simulation paths, so arm64/amd64/wasm produce identical
  bits (see docs/decisions.md §6).

## gVisor version and patch surface

Pinned: `gvisor.dev/gvisor v0.0.0-20260710194257-2354a1a30e97` (go branch,
2026-07-10), vendored under `vendor/` with the patch applied in place (the
diff is tracked by git; **do not run `go mod vendor`** without re-applying).
All changes are in `pkg/tcpip/transport/tcp`:

**Added files**
- `ccsim_sync.go` — `SimSynchronousDispatch`: inline segment processing for
  determinism (`processEndpointInline`).
- `ccsim_cc.go` — everything else: `RegisterSimCC` CC registry,
  `SimSender` handle, `ccsimWrapper` (loss/RTO counting, ECE routing,
  RFC 3168 fallback for stock CCs), delivery-rate estimation
  (`ccsimPreAck`/`ccsimPostAck` around the renamed upstream ACK handler),
  pacing gate + timer, delayed-ACK policy, per-ACK ECE echo helpers,
  `SimSenderInfo` probe snapshot, and `ccsimSetPipe` — a single-pass
  RFC 6675 pipe calculation replacing the upstream per-chunk btree
  queries (identical result, O(cwnd + sacked-ranges) per ACK).

**Modified files (all edits marked `// ccsim patch`)**
- `dispatcher.go` — 4 lines: `queueEndpoint` branches to inline processing.
- `snd.go` — sender struct field (`ccsim ccsimSenderState`); timer init
  call; `initCongestionControl` wraps CCs via the registry; pacing
  gate/charge + app-limited mark in `sendData`; segment stamping in
  `sendSegment`; ECE echo in `sendEmptySegment` and
  `sendSegmentFromPacketBuffer` (data segments carry ACKs too);
  `handleRcvdSegment`
  renamed `handleRcvdSegmentInner`; FMA-blocking conversions in the
  RFC 7323 RTT smoothing; `SetPipe` body delegates to `ccsimSetPipe`.
- `cubic.go` — fractional cwnd accumulator (upstream truncation stalls the
  window at large cwnd); FMA-blocking conversions.
- `segment.go` — one field: `ccsim ccsimSegState` (delivery-rate stamps).
- `endpoint.go` — `ccsimEchoECE` and `ccsimInlineActive` fields; ccsim
  timer cleanup in `cleanupLocked`; TOS ECN-bit mask bypass under
  `SimAllowECTTOS`.
- `rcv.go` — one line: CE detection hook (`ccsimNoteCE`).
- `connect.go` — delayed-ACK policy in `handleSegmentsLocked` (sync mode
  only).
- `protocol.go` — one line: registered CC names appended to the
  available-CC list.

Why each change exists is recorded in `docs/decisions.md`.

## BBRv3

`bbr/` implements **draft-ietf-ccwg-bbr-03** (the July 2025 revision):
Startup (pacing gain 2.77 / cwnd gain 2.0) → Drain (1/2.77) →
ProbeBW DOWN(0.9)/CRUISE(1.0)/REFILL(1.0)/UP(1.25) → ProbeRTT (cwnd gain
0.5, 200 ms hold, ~5 s cadence, 10 s min_rtt window); windowed max-bw filter
(two probe cycles); `inflight_hi`/`inflight_lo` and `bw_lo` bounds with
beta 0.7 and 0.85 headroom; 2% loss-rate probe abort; startup full-pipe
detection (<25% growth across 3 rounds, or excessive loss/ECN); ECN alpha
(gain 1/16, cut factor 1/3, threshold 0.5); pacing at 1% margin.

**Pacing** is enforced in the sender integration layer (`ccsim_cc.go`), not
in the CC: `sendData` releases quantum-sized bursts
(`min(pacing_rate·1ms, 64KB)`, ≥2 MSS) gated by a virtual-clock timer.
Pacing granularity is therefore one send quantum; retransmissions in
recovery are not paced.

Deliberate deviations (full rationale in docs/decisions.md §7 and the
package comment): retransmit-counter loss attribution, per-ACK (ACE-style)
ECN echo, simplified extra-acked allowance, rate-limited max-bw filter
turnover with a cruise aging backstop, DOWN-timeout probe entry.

Internal state (phase, pacing rate, filtered bw, min_rtt, inflight_hi/lo,
cycle index) is exported to the probe layer on every sample tick.

## Smoke scenarios

All ten pass natively (`go test ./sim -run TestScenario`), including
`fairness` — no expected-fail needed: over t∈[20,60] s the cubic/BBR split
is ≈59%/41% of a ≥90%-utilized 100 Mbps link. Representative results:

| scenario | result |
|---|---|
| cubic-single | 96.5 Mbps, 3 cwnd cuts, 0 RTO |
| bbr-single | 91+ Mbps, mean srtt 40.7 ms (base 40), ProbeRTT every ≤5 s |
| bufferbloat | cubic steady-state srtt 825 ms vs bbr 31 ms (base 30); 2 overflow cuts in 60 s (HyStart makes the first fill a ~27 s t³ climb) |
| random-loss 1% | bbr 86-90 Mbps vs cubic 2.8 Mbps |
| rate-step | drained to ≈1×new BDP within 3 s of the down-step |
| ecn-codel | 60+ CE marks, zero retransmits, srtt ≤ 2×base |
| wasm-parity | byte-identical stream, matching summary |

## Sample stream format

Fixed 20-byte little-endian records:
`[f64 t_s][u16 flow_id][u8 kind][u8 pad][f64 value]`. The kind enum lives in
`stream/stream.go` and is mirrored by the reference decoder
`stream/decoder.mjs` (validated against a shared golden file). Flow ids
`0xFFFF`/`0xFFFE` are the forward/reverse link pseudo-flows. The optional
per-packet event stream (kinds 17-19) is enabled with
`"sample": {"packet_events": true}`.
