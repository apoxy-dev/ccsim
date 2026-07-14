# Golden stream changelog

Every `-update` of testdata/golden.json appends an entry here.

- 2026-07-12 — gvisor v0.0.0-20260710194257-2354a1a30e97, draft-ietf-ccwg-bbr-03 — changed: all (initial) — reason: initial goldens for the validation suite
- 2026-07-13 — gvisor v0.0.0-20260710194257-2354a1a30e97, draft-ietf-ccwg-bbr-03 — changed: determinism, random-loss — reason: reverse-direction drop records attributed to LINK_REV pseudo-flow instead of the owning flow id, so consumers can distinguish ACK-path wire loss from forward bottleneck drops
- 2026-07-14 — gvisor v0.0.0-20260710194257-2354a1a30e97, draft-ietf-ccwg-bbr-03 — changed: bbr-op-point, bbr-single, coexist-1bdp, determinism, ecn-codel, fairness, rate-step — reason: BBR rate samples now carry pipe-based inflight (s.Outstanding*MSS, matching Linux prior_in_flight=tcp_packets_in_flight) instead of the SACK-hole-inflated SND.NXT-SND.UNA span; under persistent loss ProbeBW:DOWN can now actually drain instead of wedging, halving the standing queue in lossy runs
- 2026-07-14 — gvisor v0.0.0-20260710194257-2354a1a30e97, draft-ietf-ccwg-bbr-03 — changed: bbr-single, coexist-1bdp, determinism — reason: BBR startup conformance: exempt Startup from bw_lo/inflight_lo cuts (bbr_is_probing_bandwidth) and ratchet pacing upward until full pipe (bbr_set_pacing_rate); a single startup loss no longer collapses the ramp (one drop at 0.2s on 100Mbps/40ms: 28.2 -> 90.4 Mbps)
