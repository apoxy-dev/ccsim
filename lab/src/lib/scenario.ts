// Scenario construction mirroring scenario.ScenarioConfig's JSON shape.

export type CC = 'cubic' | 'bbr'

export interface LabCfg {
  rateMbps: number
  owdMs: number
  lossPct: number // percent, converted to a [0,1) fraction in the JSON
  qlimPkts: number
}

export const DEFAULT_CFG: LabCfg = { rateMbps: 100, owdMs: 20, lossPct: 0, qlimPkts: 350 }

export const RUN_DUR_S = 30

export interface Derived {
  baseMs: number // base RTT
  bdpPkts: number // 1500-byte packets
  bdpBytes: number
  bufX: number // buffer in ×BDP
  cliff: number // inflight (×BDP) at which the tail-drop buffer is full
}

export function derive(cfg: LabCfg): Derived {
  const baseMs = 2 * cfg.owdMs
  const bdpBytes = (cfg.rateMbps * 1e6 / 8) * (baseMs / 1000)
  const bdpPkts = Math.max(4, bdpBytes / 1500)
  const bufX = cfg.qlimPkts / bdpPkts
  return { baseMs, bdpPkts, bdpBytes, bufX, cliff: 1 + bufX }
}

export function scenarioFor(cc: CC, cfg: LabCfg): object {
  return {
    seed: 7,
    dur_s: RUN_DUR_S,
    link: {
      rate_mbps: cfg.rateMbps,
      owd_ms: cfg.owdMs,
      loss: cfg.lossPct / 100,
      queue: { kind: 'taildrop', limit_pkts: Math.round(cfg.qlimPkts) },
    },
    flows: [{ cc, start_at_s: 0, app: { kind: 'bulk' } }],
    sample: {},
  }
}

// The bandwidth-change experiment from the BBR paper (ACM Queue, figure 3):
// a 10-Mbps, 40-ms flow whose bottleneck doubles to 20 Mbps at t=20 s and
// drops back to 10 Mbps at t=40 s. Rate, delay, and buffer stay fixed —
// the figure's chrome (link-rate step path, lane ranges, step labels) is
// drawn to them — but wire loss is adjustable and either CC can run it.
export const BWSTEP_DUR_S = 60
export const BWSTEP_CFG: LabCfg = { rateMbps: 10, owdMs: 20, lossPct: 0, qlimPkts: 133 }

export function bwStepScenario(cc: CC, lossPct: number): object {
  return {
    ...scenarioFor(cc, { ...BWSTEP_CFG, lossPct }),
    dur_s: BWSTEP_DUR_S,
    events: [
      { at_s: 20, path: 'link.rate_mbps', value: 20 },
      { at_s: 40, path: 'link.rate_mbps', value: 10 },
    ],
  }
}
