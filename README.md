# ccsim — deterministic TCP congestion control simulator

An event-driven TCP congestion control simulation harness built on gVisor's
netstack (`gvisor.dev/gvisor/pkg/tcpip`). Two full netstack instances
(sender / receiver) are connected through a configurable bottleneck link
model (rate, delay, loss, pluggable queue disciplines, ECN). Includes a
from-scratch **BBRv3** implementation registered alongside stock Cubic, plus
a deliberately congestion-oblivious fixed-rate controller for demonstrations,
a binary metric sample stream, a CLI runner, and a wasm build whose output is
**byte-identical** to the native build.

```
clock/      virtual clock implementing tcpip.Clock (single timer heap)
link/       bottleneck model: token-bucket rate, delay, seeded loss and
            jitter, tail-drop / RED / CoDel / FQ-CoDel, ECN CE marking,
            telemetry
bbr/        BBRv3 (draft-ietf-ccwg-bbr-03) as a netstack congestion control
naive/      fixed-rate 150 Mbps demonstration congestion control
probe/      per-flow taps, sample records, summaries, windowed analysis
scenario/   ScenarioConfig JSON model, validation, presets
sim/        harness: stacks, flow drivers, event loop, live-settable params
stream/     20-byte binary sample records; Go encoder + JS reference decoder
cmd/ccsim/  CLI runner
wasm/       thin wasm entry + worker glue + node parity runner + smoke page
lab/        CC Lab — React SPA rendering the wasm sample stream as live
            figures (operating-point panels, bandwidth-change experiment)
scenarios/  preset scenario JSON files
docs/       decisions.md — design notes for every forced deviation
            validation.md — validation methodology, findings, measured tables
```

## Running

```sh
go test ./...                                   # fast suite: smoke + validation + goldens
go test -tags slow ./sim -run TestSlow -v       # nightly sweeps (Mathis, fairness, coexistence)
make validate                                   # fast + slow + perf budgets + wasm memory
go build -o ccsim ./cmd/ccsim
./ccsim -preset bufferbloat -summary            # human table
./ccsim -scenario scenarios/rate-step.json -out run.bin -json
GOOS=js GOARCH=wasm go build -o wasm/main.wasm ./wasm
node wasm/parity.mjs wasm/main.wasm wasm/wasm_exec.js scenarios/cubic-single.json out.bin
node stream/decoder_test.mjs                    # JS decoder unit test
```

The validation suite (docs/validation.md) checks the harness against
published models — cubic's W(t)=C·(t−K)³+W_max fits with C=0.410 and
R²=0.9999, the RFC 9438 loss-response function within 0.68–0.95× per
point, RED's marking curve by χ² against the exact ramp pmf — plus BBRv3
draft conformance, invariant fuzzing, golden-stream regressions
(`sim/testdata/golden.json`, regenerate with `-update -reason "..."`) and
performance budgets. Measured tables in docs/validation.md are written by
the tests themselves under `-update`.

Browser demo: build `wasm/main.wasm` as above, serve the repo root
(`python3 -m http.server`), open `/wasm/index.html?preset=bufferbloat`.
The sim runs in a worker in **streaming mode** (flat out, yielding between
250 ms-sim batches) and the charts — srtt, throughput, cwnd, queue depth —
render progressively as sample chunks arrive, so there is no wait for the
run to finish. The rate / owd / loss / queue sliders mutate the live sim
mid-run via `set()`. Rendering uses per-pixel min/max binning over the
full-resolution stream, so loss spikes and sawtooth teeth survive
decimation. Self-contained page, no chart library.

**CC Lab** (`lab/`) is a Vite + React SPA over the same worker glue:
`make lab-dev` builds the wasm binary, copies it with the worker into
`lab/public/sim/`, and starts the dev server. It runs Cubic and BBRv3 as
independent single-flow runs per parameter set, with a third fixed-rate naive
run for the animated pipe, and draws the BBR paper's Figure-1 operating-point
panels (RTT + delivery vs. inflight, live trails) and the bandwidth-change
experiment (paper figure 3) straight from the decoded sample stream.
`make lab-build` produces `lab/dist/`.

The browser demos are deployed to Fly.io as the `ccsim` app in the
`apoxy-inc` organization: CC Lab at `/` and the original smoke/demo page at
`/wasm/`. The multi-stage `Dockerfile` builds one WebAssembly binary and the
SPA, fingerprints the WASM filename, and injects it into both workers. Caddy
serves fingerprinted assets with immutable caching while HTML and workers
always revalidate. Deploy it with:

```sh
flyctl deploy --config fly.toml
```

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
  `SimSenderInfo` probe snapshot, a single-pass classic RFC 6675 pipe
  calculation, and Linux-style incremental RACK pipe/transmit/loss queues.

**Modified files (all edits marked `// ccsim patch`)**
- `dispatcher.go` — 4 lines: `queueEndpoint` branches to inline processing.
- `snd.go` — sender struct field (`ccsim ccsimSenderState`); timer init
  call; `initCongestionControl` wraps CCs via the registry; pacing
  gate/charge + app-limited mark in `sendData`; segment stamping in
  `sendSegment`; ECE echo in `sendEmptySegment` and
  `sendSegmentFromPacketBuffer` (data segments carry ACKs too);
  `handleRcvdSegment`
  renamed `handleRcvdSegmentInner`; FMA-blocking conversions in the
  RFC 7323 RTT smoothing; `SetPipe` body delegates to `ccsimSetPipe`; RTO
  recovery exit and spurious-recovery callbacks reach the simulation CC;
  RACK repair runs only when a new/pending loss batch exists.
- `rack.go` — RFC 8985 transmit-order comparison (including equal-timestamp
  sequence tie-break), pacing around RACK repairs and TLP probes, complete
  loss accounting, and resumable repair walks.
- `sack_scoreboard.go` — retains every valid in-flight SACK range instead of
  silently discarding new evidence after 100 disjoint ranges.
- `sack_recovery.go` — pacing gate/charge around direct RFC 6675 repair
  transmissions; the pacing timer resumes this walk when blocked.
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

`bbr/` follows **Google BBRv3 at `google/bbr` v3 commit `90210de4`**, using
the state machine described by draft-ietf-ccwg-bbr-03: Startup (pacing gain
710/256, cwnd gain 2.0) → Drain (88/256) → ProbeBW
DOWN(232/256)/CRUISE(1.0)/REFILL(1.0)/UP(1.25) → ProbeRTT (cwnd gain 0.5,
200 ms hold). It has separate 5 s ProbeRTT-scheduling and 10 s `min_rtt`
filters, a two-cycle max-bw filter, measured ACK aggregation, reference
fixed-point loss/headroom thresholds, idle-restart handling, and
`inflight_hi`/`inflight_lo`/`bw_lo` bounds. ECN control is enabled only for an
explicit shallow-threshold route with min RTT ≤5 ms; pacing uses a 1% margin.

**Pacing** is enforced in the sender integration layer (`ccsim_cc.go`), not
in the CC: `sendData` releases quantum-sized bursts
(`min(pacing_rate·1ms, 64KB)`, ≥2 MSS) gated by a virtual-clock timer.
Pacing granularity is therefore one send quantum. RACK repairs, TLP probes,
and classic-SACK fallback all use the same gate; the pacing timer resumes the
specific send walk that it suspended.

The harness enables RACK/TLP. Its loss ordering and transmit-time work queue
follow RFC 8985/Linux, while congestion feedback reaches BBR through normal
rate samples plus an explicit TLP-recovery event. The remaining deliberate
transport simplification is per-ACK (ACE-style) ECN echo rather than Linux's
full AccECN plumbing (full rationale in docs/decisions.md).

Internal state (phase, pacing rate, filtered bw, min_rtt, inflight_hi/lo,
cycle index) is exported to the probe layer on every sample tick.

## Smoke scenarios

All ten pass natively (`go test ./sim -run TestScenario`), including
`fairness` — no expected-fail needed: over t∈[20,60] s the cubic/BBR split
is ≈33%/67% of a 96%-utilized 100 Mbps link. Representative results:

| scenario | result |
|---|---|
| cubic-single | 96.5 Mbps, 3 cwnd cuts, 0 RTO |
| bbr-single | 93 Mbps, mean srtt 41.7 ms (base 40), ProbeRTT every ≤5 s |
| bufferbloat | cubic steady-state srtt 1070 ms vs bbr 32 ms (base 30); 1,387 drops repaired by exactly 1,387 retransmissions, 0 RTO |
| random-loss 1% | bbr 57 Mbps vs cubic 3.0 Mbps (reference `bw_lo` response active) |
| rate-step | 24 Mbps delivery immediately; 1.07×new BDP after the two-cycle max-bw turnover; restored capacity reused at 96 Mbps |
| ecn-codel | 654 CE marks, zero drops/retransmits, srtt ≤ 2.1×base |
| wasm-parity | byte-identical stream, matching summary |

## Sample stream format

Fixed 20-byte little-endian records:
`[f64 t_s][u16 flow_id][u8 kind][u8 pad][f64 value]`. The kind enum lives in
`stream/stream.go` and is mirrored by the reference decoder
`stream/decoder.mjs` (validated against a shared golden file). Flow ids
`0xFFFF`/`0xFFFE` are the forward/reverse link pseudo-flows. The optional
per-packet event stream (kinds 17-19) is enabled with
`"sample": {"packet_events": true}`.
