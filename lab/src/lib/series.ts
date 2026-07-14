// Accumulates decoded sample records from one run and resamples them into
// the normalized point model the figures share: x = inflight/BDP,
// y = delivery/BtlBw, r = srtt/base-RTT, q = bottleneck queue (packets).

import { Kind, LINK_FWD, LINK_REV } from '../../../stream/decoder.mjs'
import type { Rec } from './sim-client'
import type { Derived } from './scenario'

interface Track {
  t: number[]
  v: number[]
}

const newTrack = (): Track => ({ t: [], v: [] })

// BBR state codes exported through the probe layer (bbr.StateName order),
// mapped onto the design's phase keys.
const BBR_PHASE = [
  'startup',
  'drain',
  'bw:down',
  'bw:cruise',
  'bw:refill',
  'bw:up',
  'probertt',
] as const

export type Phase = (typeof BBR_PHASE)[number]

export class RunData {
  inflight = newTrack() // bytes
  cwnd = newTrack() // packets
  srtt = newTrack() // seconds
  delivery = newTrack() // bits/s
  ccState = newTrack()
  qDepth = newTrack() // packets, forward link
  wireCV = newTrack() // bottleneck arrival-gap CV, forward link (wire_stats)
  lossEvents: number[] = [] // cwnd-cut times
  dropEvents: number[] = [] // forward-link drop times
  maxT = 0

  push(recs: Rec[]) {
    for (const r of recs) {
      if (r.t > this.maxT) this.maxT = r.t
      // Forward drops are attributed to the owning flow when known, with
      // LINK_FWD as fallback; reverse-direction (ACK-path) drops arrive as
      // LINK_REV and are excluded — they are not bottleneck-queue events.
      if (r.kind === Kind.Drop) {
        if (r.flow !== LINK_REV) this.dropEvents.push(r.t)
        continue
      }
      if (r.flow === LINK_FWD) {
        if (r.kind === Kind.QDepthPkts) {
          this.qDepth.t.push(r.t)
          this.qDepth.v.push(r.value)
        } else if (r.kind === Kind.WireBurstCV) {
          this.wireCV.t.push(r.t)
          this.wireCV.v.push(r.value)
        }
        continue
      }
      if (r.flow === LINK_REV) continue
      switch (r.kind) {
        case Kind.InflightBytes:
          this.inflight.t.push(r.t)
          this.inflight.v.push(r.value)
          break
        case Kind.CwndPkts:
          this.cwnd.t.push(r.t)
          this.cwnd.v.push(r.value)
          break
        case Kind.SRTTSec:
          this.srtt.t.push(r.t)
          this.srtt.v.push(r.value)
          break
        case Kind.DeliveryBps:
          this.delivery.t.push(r.t)
          this.delivery.v.push(r.value)
          break
        case Kind.CCState:
          this.ccState.t.push(r.t)
          this.ccState.v.push(r.value)
          break
        case Kind.LossRecovery:
          this.lossEvents.push(r.t)
          break
      }
    }
  }
}

export interface Pt {
  t: number
  x: number // inflight, ×BDP
  y: number // delivery, ×BtlBw
  r: number // srtt, ×base RTT
  q: number // bottleneck queue, packets
  w?: number // cwnd, packets
  cv?: number // bottleneck arrival-gap CV (wire_stats; absent in old streams)
  phase?: Phase
}

export interface LossMark {
  t: number
  x: number
  y: number
  r: number // srtt ratio at the loss — buffer-overflow losses sit at the cliff, random losses do not
}

// Walks a time-ordered track alongside the resample grid; O(n) overall.
class Cursor {
  private i = 0
  private tr: Track
  constructor(tr: Track) {
    this.tr = tr
  }
  at(t: number): number {
    while (this.i + 1 < this.tr.t.length && this.tr.t[this.i + 1] <= t) this.i++
    if (this.tr.t.length === 0 || this.tr.t[this.i] > t) return NaN
    return this.tr.v[this.i]
  }
}

// bbrPhases: CCState codes only decode as BBR phases for a bbr flow; cubic
// exports different (recovery) codes that must not be phase-labeled.
export function toPts(run: RunData, d: Derived, rateMbps: number, bbrPhases: boolean, dt = 0.02): Pt[] {
  const inf = new Cursor(run.inflight)
  const cwn = new Cursor(run.cwnd)
  const rtt = new Cursor(run.srtt)
  const del = new Cursor(run.delivery)
  const st = new Cursor(run.ccState)
  const q = new Cursor(run.qDepth)
  const wcv = new Cursor(run.wireCV)
  const btlBps = rateMbps * 1e6
  const pts: Pt[] = []
  const n = Math.floor(run.maxT / dt)
  for (let i = 0; i <= n; i++) {
    const t = i * dt
    // InflightBytes is SND.NXT−SND.UNA (RFC 6675 outstanding): during loss
    // recovery a pinned SND.UNA inflates it by the SACK-hole span, far past
    // what is actually in the network. Capping at cwnd recovers the paper's
    // "amount inflight" operating point; outside recovery inflight ≤ cwnd
    // anyway, so the cap is inert.
    let infB = inf.at(t)
    const cwB = cwn.at(t) * 1500
    if (!Number.isNaN(cwB)) infB = Math.min(infB, cwB)
    const rttS = rtt.at(t)
    const delB = del.at(t)
    const code = st.at(t)
    const qP = q.at(t)
    // Values are the measured (ACK-clocked, smoothed) estimates. Time-series
    // figures plot them as is; the operating-point figure projects them onto
    // the feasible envelope itself (see figure2a) because its axes assume
    // same-instant measurement.
    const x = Number.isNaN(infB) ? 0.02 : Math.max(infB / d.bdpBytes, 0.02)
    const y = Number.isNaN(delB) ? 0.02 : Math.max(delB / btlBps, 0.02)
    const r = Number.isNaN(rttS) ? 1 : Math.max((rttS * 1000) / d.baseMs, 1)
    const cvV = wcv.at(t)
    pts.push({
      t,
      x,
      y,
      r,
      q: Number.isNaN(qP) ? 0 : qP,
      w: Number.isNaN(cwB) ? undefined : cwB / 1500,
      cv: Number.isNaN(cvV) ? undefined : cvV,
      phase: bbrPhases && !Number.isNaN(code) ? BBR_PHASE[code] : undefined,
    })
  }
  return pts
}

export function lossMarks(run: RunData, pts: Pt[], d: Derived, dt = 0.02): LossMark[] {
  return run.lossEvents.map((t) => {
    const p = ptAt(pts, t, dt)
    return {
      t,
      x: Math.min(p?.x ?? d.cliff, d.cliff),
      y: Math.min(p?.y ?? 1, 1),
      r: p?.r ?? Math.min(d.cliff, 2.3),
    }
  })
}

// Coalesces burst losses into at most one visual event per window.
export function coalesce(times: number[], windowS: number): number[] {
  const out: number[] = []
  for (const t of times) {
    if (out.length === 0 || t - out[out.length - 1] >= windowS) out.push(t)
  }
  return out
}

export function ptAt(pts: Pt[], t: number, dt = 0.02): Pt | undefined {
  if (pts.length === 0) return undefined
  return pts[Math.min(pts.length - 1, Math.max(0, Math.round(t / dt)))]
}
