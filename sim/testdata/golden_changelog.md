# Golden stream changelog

Every `-update` of testdata/golden.json appends an entry here.

- 2026-07-12 — gvisor v0.0.0-20260710194257-2354a1a30e97, draft-ietf-ccwg-bbr-03 — changed: all (initial) — reason: initial goldens for the validation suite
- 2026-07-13 — gvisor v0.0.0-20260710194257-2354a1a30e97, draft-ietf-ccwg-bbr-03 — changed: determinism, random-loss — reason: reverse-direction drop records attributed to LINK_REV pseudo-flow instead of the owning flow id, so consumers can distinguish ACK-path wire loss from forward bottleneck drops
