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
| 14 | late-joiner convergence: cubic < 15 s, bbr < 90 s (characterized) | `sim.TestLateJoinerConvergence` |
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
   measured ~36/min (goodput still 82%). Mechanism: ccsim does not pace
   retransmissions (decisions.md §2) — recovery bursts overrun the
   25-packet queue and lose the retransmits themselves, escalating to RTO.
   Cubic, whose recovery is ACK-clocked rather than burst-limited by a
   missing pacer, shows 0. Fix requires pacing the retransmit path in the
   vendored patch; until then the test pins the characterized ~36/min so
   both regressions and the eventual fix surface.
2. **BBR intra-protocol Jain at N=4** (test 13): 0.855 vs the 0.90 target
   (N=2: 0.973, N=8: 0.915). Shares wander with probe-cycle phasing
   (37.8/22.3/20.5/11.5 Mbps at N=4) but no capture: the minimum share is
   50% of fair, far above the 10% line that marks BBRv1's bw-filter
   capture failure — the v3-specific claim holds and is asserted.
3. **Shallow-buffer coexistence aggregate** (test 15): 73/79 Mbps at
   0.25/0.5×BDP vs the 85% target. Both flows spend large fractions of
   time in loss recovery against a queue that cannot absorb a single
   probe's overshoot; ≥1×BDP the target holds with ~96% utilization. The
   deep-buffer end of the table is the interesting one: cubic's standing
   queue starves BBR to 1–4% share at 16–64×BDP (windowed min_rtt is not
   enough when the queue holds 16×BDP permanently).
4. **Qdisc hot path allocated 1.0/packet** (test 29, fixed in this change):
   the fifo's slide-forward slice reallocated per packet whenever a queue
   oscillated around empty. Replaced with a ring buffer; golden streams
   byte-identical.
5. **wasm page leaked a full sim per preset load** (test 30, fixed in this
   change): netstack goroutines pinned each replaced sim (~29 MB, +20
   goroutines per load). `sim.Close()` now destroys both stacks on
   replacement; linear memory is flat at 126.6 MB across 20 cycles.

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

<!-- begin:rtt-fairness -->
| cc | 20 ms flow | 120 ms flow | exponent e |
|---|---|---|---|
| cubic | 70.9 Mbps | 25.5 Mbps | 0.57 |
| bbr | 8.8 Mbps | 71.6 Mbps | -1.17 |
<!-- end:rtt-fairness -->

### BBR operating-point surface (test 11)

Single bbr flow, 4×BDP tail-drop, measured over [15,60] s.

<!-- begin:bbr-op-point -->
| rate Mbps | RTT ms | inflight ×BDP | queue delay / RTT | utilization |
|---|---|---|---|---|
| 10 | 10 | 1.23 | 13.5% | 93.4% |
| 10 | 40 | 1.00 | 3.3% | 93.1% |
| 10 | 150 | 0.98 | 4.7% | 92.6% |
| 100 | 10 | 0.97 | 2.4% | 93.5% |
| 100 | 40 | 0.95 | 1.7% | 93.0% |
| 100 | 150 | 0.97 | 4.3% | 92.3% |
| 500 | 10 | 0.96 | 2.6% | 93.6% |
| 500 | 40 | 0.95 | 1.8% | 93.0% |
| 500 | 150 | 0.97 | 4.4% | 92.3% |
<!-- end:bbr-op-point -->

### Late-joiner convergence (test 14)

1×BDP buffer, second flow joins at t=60 s; sliding 5 s windows (cubic
observed to t=100 s, bbr to t=160 s).

**Finding (2026-07-14):** adopting the draft's cwnd control law
(`BBRSetCwnd`: cwnd grows by at most the newly-acked data per ACK; the
model target is a cap, applied only once the pipe is full) slowed bbr
late-joiner reallocation from 4 s to 60 s at this seed. The old <15 s
figure was an artifact of the non-conformant assignment law, which leaped
cwnd straight to 2×BDP of the joiner's optimistic in-probe model. Under
the draft law the joiner gains share one 2–3 s probe cycle at a time —
the slow-reallocation behavior documented for real BBRv2/v3. First
crossing of 35% share varies widely with seed (0 s, 0 s, 60 s, 132 s over
seeds 34–37; share then oscillates 30–70% with probe-cycle beats), so the
bound is pinned at 90 s for the deterministic test seed (measured 60.0 s)
rather than tightened to the mean. Steady-state fairness (tests 12–13) is
unaffected.

<!-- begin:late-joiner -->
| cc | time to 35% share | final split (old/new) |
|---|---|---|
| cubic | 12.0 s | 47.9 / 48.5 Mbps |
| bbr | 60.0 s | 54.8 / 35.6 Mbps |
<!-- end:late-joiner -->

### Coexistence surface (test 15)

cubic vs bbr, 100 Mbps / 30 ms RTT, 60 s × 3 seeds, goodput over [20,60] s.

<!-- begin:coexistence -->
| buffer ×BDP | cubic Mbps | bbr Mbps | bbr share | aggregate |
|---|---|---|---|---|
| 0.25 | 18.6 | 54.5 | 75% | 73.2 |
| 0.50 | 26.7 | 51.9 | 66% | 78.6 |
| 1.00 | 55.1 | 41.3 | 43% | 96.4 |
| 2.00 | 56.4 | 39.9 | 41% | 96.3 |
| 4.00 | 72.1 | 24.4 | 25% | 96.4 |
| 16.00 | 94.5 | 1.0 | 1% | 95.6 |
| 64.00 | 85.7 | 3.1 | 4% | 88.8 |
<!-- end:coexistence -->
