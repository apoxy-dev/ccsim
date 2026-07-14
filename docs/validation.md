# Validation

The smoke scenarios answer "does it run"; this suite answers "is it
*right*": conformance against published models and the BBRv3 draft,
property/invariant fuzzing, AQM correctness, golden-stream regressions and
performance budgets. Every quantitative test logs its measured value next
to the model's prediction even when passing; the tables below are written
by the tests themselves under `-update` (they are the data generators, so
the doc can never drift from the code).

## Running

```sh
go test ./...                                   # fast suite (PR gate), < 5 min
go test -tags slow ./sim -run TestSlow -v       # sweeps and long runs (nightly)
go test -tags slow ./sim -run TestSlowWasmMemoryStability -v
CCSIM_PERF=1 go test ./sim -run TestPerfBudget  # wall-clock budgets (quiet machine)
make validate                                    # all of the above
```

Regenerating measured artifacts:

```sh
go test ./sim -run TestGoldenStreams -update -reason "why the streams changed"
go test -tags slow ./sim -run TestSlow -update          # rewrite tables below
```

## Test map

| # | claim | test |
|---|---|---|
| 1 | harness does not distort stock cubic: W(t)=C(t−K)³+Wmax, C≈0.4, R²≥0.99 | `sim.TestCubicCurveFit` |
| 2 | cubic loss response tracks RFC 9438 within 1.6× per point | `sim.TestSlowMathisSweep` |
| 3 | RTT-fairness exponent: cubic e<1; BBR recorded | `sim.TestSlowRTTFairnessExponent` |
| 4–10 | BBRv3 filter windows, ProbeBW cycle, loss/ECN arithmetic, startup exit, ProbeRTT, app-limited | `bbr/conformance_test.go` |
| 11 | BBR operating point: ~1×BDP inflight, low delay, ≥92% util over 9 rate×RTT cells | `sim.TestBBROperatingPoint` (+`TestSlowBBROperatingPointGrid`) |
| 12–13 | intra-protocol convergence (Jain) | `sim.TestSlowIntraProtocolFairness` |
| 14 | late-joiner convergence: cubic < 15 s, bbr < 30 s (characterized) | `sim.TestLateJoinerConvergence` |
| 15 | cubic/bbr coexistence vs buffer depth | `sim.TestSlowCoexistenceSurface` |
| 16–19 | CoDel target + √n control law, FQ-CoDel isolation, RED ramp (χ²), ECN≈drop | `link/aqm_test.go`, `link/aqm_e2e_test.go` |
| 20–22 | scenario fuzz invariants; live-mutation determinism; inject≡declared events | `sim/property_test.go` |
| 23–27 | sub-BDP buffer, extreme BDP, idle restart, asymmetric path, two-way traffic | `sim/validation_test.go` |
| 28 | golden streams per preset | `sim.TestGoldenStreams` |
| 29 | ≥15×/≥3× real-time; 0 allocs/pkt in qdiscs | `sim.TestPerfBudget`, `link.TestQdiscAllocsPerPacket` |
| 30 | wasm memory stable across 20 load+run cycles | `sim.TestSlowWasmMemoryStability` + `wasm/memtest.mjs` |

## Methodology notes

- **Cubic curve fit (1)** injects exactly one loss (`link.drop_next`) into a
  deep-buffer run and fits the recovery epoch via the cube-root
  linearization ∛(W−Wmax) = ∛C·(t−t₀−K), grid-searching Wmax because fast
  convergence may scale it. Measured: C=0.410 (2.5% from RFC 8312's 0.4),
  R²=0.9999, fitted K within 3% of the analytic ∛(0.3·Wmax/C). This is the
  control for every BBR claim: clock, pacing and ACK plumbing reproduce a
  known-good algorithm to four nines.
- **Mathis/Padhye (2)** compares against max(cubic-regime model,
  Reno-friendly floor) — netstack cubic implements the TCP-friendly region,
  so at 40 ms RTT the friendly region dominates and the measured log-log
  slope (−0.56) sits between Reno's −0.5 and pure cubic's −0.75.
- **RED (18)** pins q.avg (Wq=0) per level and χ²-tests the inter-drop gap
  distribution against the exact pmf implied by the configured ramp plus
  Floyd's count correction — no uniform-approximation slack.
- **Golden streams (28)** store the full-stream SHA-256 plus per-sim-second
  segment hashes, so a mismatch reports the first divergent second. The
  old stream is not stored, so record-level diffs of historical behavior
  are not reconstructable — rerun the old commit if needed.

## Findings (expected-fail policy: analysis, never silent tolerance bumps)

1. **BBR RTO rate at 0.1×BDP buffers** (test 23): target was <5 RTOs/min;
   measured ~36/min originally, ~13/min after mark-time loss signals
   (audit finding 3) — fewer escalations because the model reacts to the
   loss before the retransmits themselves are lost, though goodput dipped
   82% → ~70% as the honest signals also make BBR back off harder against
   a queue this shallow. Mechanism unchanged: ccsim does not pace
   retransmissions (decisions.md §2) — recovery bursts overrun the
   25-packet queue. Cubic, whose recovery is ACK-clocked rather than
   burst-limited by a missing pacer, shows 0. Fix requires pacing the
   retransmit path in the vendored patch; the test pins the characterized
   rate so both regressions and the eventual fix surface.
2. **BBR intra-protocol Jain vs the 0.90 target** (test 13): shares
   wander with probe-cycle phasing; the draft ProbeBW feedback machine
   (audit finding 4) lifted Jain to 0.915–0.947 across N (from 0.83 at
   N=4 with linear inflight_hi growth) with min share 66% of fair at
   worst — far above the 10% line that marks BBRv1's bw-filter capture
   failure, which is the v3-specific claim and is asserted (Jain pinned
   at 0.85). The small-N aggregate dip from the mark-time loss change
   mostly recovered (N=2 81.5 → 92.0, N=4 89.5 → 89.7, N=8 92.9 → 91.6
   Mbps vs the 90% target); pinned at 85 for N≤4.
3. **Coexistence vs buffer depth** (test 15): originally 73/79 Mbps
   aggregate at 0.25/0.5×BDP vs the 85% target; mark-time loss signals
   (audit finding 3) resolved the idle-link half — retransmit-time loss
   counting had kept both flows in recovery churn. The draft ProbeBW
   feedback machine (audit finding 4) then resolved the mid-buffer
   starvation: bbr share at 2×/4×BDP went 23%/19% → 51%/48% (linear
   inflight_hi growth could never rebuild the bound between cubic-induced
   cuts; the draft's exponential slope can). At sub-BDP buffers bbr now
   takes the larger share (68–71%), consistent with published BBR
   behavior in small buffers. What still stands is the deep-buffer end:
   cubic's permanent standing queue starves BBR to ~1% share at 16×BDP
   (windowed min_rtt is not enough when the queue never drains; 64×BDP
   recovers slightly to ~5% because even cubic cannot keep a 64×BDP queue
   full through its own loss cycles).
4. **Qdisc hot path allocated 1.0/packet** (test 29, fixed in this change):
   the fifo's slide-forward slice reallocated per packet whenever a queue
   oscillated around empty. Replaced with a ring buffer; golden streams
   byte-identical.
5. **wasm page leaked a full sim per preset load** (test 30, fixed in this
   change): netstack goroutines pinned each replaced sim (~29 MB, +20
   goroutines per load). `sim.Close()` now destroys both stacks on
   replacement; linear memory is flat at 126.6 MB across 20 cycles.
6. **BBR operating point at tiny BDPs** (test 11): the 10 Mbps × 10 ms
   cell (BDP ≈ 17 packets) measures 1.39×BDP inflight with 29% queue
   delay vs the 1.3×/25% grid bounds (1.23×/13.5% before the draft
   ProbeBW feedback machine; it was the outlier then too). One MSS of
   inflight_hi/cwnd granularity is ~6% of this BDP and the 4-packet
   MinPipeCwnd floor is ~24% of it, so probe overshoot quantizes to a
   visible standing queue; utilization is unaffected (93.5%). The other
   8 cells hold 0.98–1.09×BDP and ≤16% queue delay. Characterized
   bounds (1.5×, 35%) apply only below 25 packets of BDP.

## Measured results

### Cubic loss response vs RFC 9438 (test 2)

100 Mbps, 40 ms RTT, deep buffer, 120 s × 5 seeds per point.

<!-- begin:mathis -->
| loss | measured | RFC 9438 model | ratio |
|---|---|---|---|
| 0.03% | 19.4 | 20.5 | 0.95 |
| 0.10% | 10.1 | 11.2 | 0.90 |
| 0.30% | 5.8 | 6.5 | 0.89 |
| 1.00% | 3.0 | 3.5 | 0.84 |
| 3.00% | 1.4 | 2.0 | 0.68 |

log-log slope: -0.563 (R² 1.00)
<!-- end:mathis -->

### RTT fairness (test 3)

Shared 100 Mbps, 2×BDP buffer, RTTs 20 ms vs 120 ms, goodput over [30,120] s.

BBR's exponent has tracked the audit fixes: −1.17 with retransmit-time
loss signals (pathological long-RTT dominance driven by the short-RTT
flow's spurious loss feedback), 0.24 with mark-time signals, and −0.69
with the draft ProbeBW feedback machine (audit finding 4). The negative
steady-state exponent — long-RTT flows holding the larger share — is
BBR's documented inverse-RTT bias (probe magnitude scales with BDP, so
the long-RTT flow's probes displace more of the queue), the opposite of
loss-based CC and not a harness defect.

<!-- begin:rtt-fairness -->
| cc | 20 ms flow | 120 ms flow | exponent e |
|---|---|---|---|
| cubic | 70.9 Mbps | 25.5 Mbps | 0.57 |
| bbr | 21.1 Mbps | 72.2 Mbps | -0.69 |
<!-- end:rtt-fairness -->

### BBR operating-point surface (test 11)

Single bbr flow, 4×BDP tail-drop, measured over [15,60] s.

<!-- begin:bbr-op-point -->
| rate Mbps | RTT ms | inflight ×BDP | queue delay / RTT | utilization |
|---|---|---|---|---|
| 10 | 10 | 1.39 | 29.3% | 93.5% |
| 10 | 40 | 1.06 | 9.3% | 93.2% |
| 10 | 150 | 1.09 | 15.5% | 93.2% |
| 100 | 10 | 1.02 | 6.6% | 93.5% |
| 100 | 40 | 0.98 | 4.2% | 93.2% |
| 100 | 150 | 1.07 | 14.6% | 93.1% |
| 500 | 10 | 1.00 | 6.2% | 93.6% |
| 500 | 40 | 0.98 | 4.4% | 93.2% |
| 500 | 150 | 1.06 | 13.5% | 92.8% |
<!-- end:bbr-op-point -->

### Late-joiner convergence (test 14)

1×BDP buffer, second flow joins at t=60 s; sliding 5 s windows (cubic
observed to t=100 s, bbr to t=160 s).

**Finding (2026-07-14, updated through audit finding 4):** this number
has moved three times, for instructive reasons. The original <4 s
convergence was an artifact of the non-conformant assignment-law cwnd
(leaping straight to 2×BDP of the joiner's optimistic in-probe model);
adopting the draft's `BBRSetCwnd` (audit finding 2) slowed it to 60 s at
this seed with huge seed variance (0–132 s), and the bound was
provisionally pinned at 90 s. Fixing the rate-sample signals (audit
finding 3: loss counted at RFC 6675 mark time instead of retransmit
time, tx_in_flight-based probe gates scoped to probe-sent samples)
restored fast reallocation: 8–13 s over seeds 34–37. The draft ProbeBW
feedback machine (audit finding 4: exponential inflight_hi growth,
adapt-on-every-ACK, plateau-driven UP exit) tightened it further to
5–9 s (4.0 s at this seed) — a joiner rebuilding its bound climbs
exponentially instead of one MSS per round. The bound stays pinned at
30 s. Post-convergence the share keeps oscillating with probe-cycle
beats (joiner share 16–86% over the last 40 s of 200 s runs across
seeds 34–37) — whichever flow probed more recently holds the larger
bandwidth sample, a known BBR trait.

<!-- begin:late-joiner -->
| cc | time to 35% share | final split (old/new) |
|---|---|---|
| cubic | 12.0 s | 47.9 / 48.5 Mbps |
| bbr | 4.0 s | 32.4 / 54.9 Mbps |
<!-- end:late-joiner -->

### Coexistence surface (test 15)

cubic vs bbr, 100 Mbps / 30 ms RTT, 60 s × 3 seeds, goodput over [20,60] s.

<!-- begin:coexistence -->
| buffer ×BDP | cubic Mbps | bbr Mbps | bbr share | aggregate |
|---|---|---|---|---|
| 0.25 | 30.5 | 64.3 | 68% | 94.8 |
| 0.50 | 27.0 | 65.8 | 71% | 92.8 |
| 1.00 | 31.3 | 63.7 | 67% | 94.9 |
| 2.00 | 46.9 | 49.4 | 51% | 96.2 |
| 4.00 | 50.5 | 46.0 | 48% | 96.5 |
| 16.00 | 94.7 | 1.0 | 1% | 95.7 |
| 64.00 | 85.4 | 4.6 | 5% | 89.9 |
<!-- end:coexistence -->
