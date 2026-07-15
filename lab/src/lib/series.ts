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

// Mirrors link.DropReason. Drop records already carry this code, so retain
// enough of it to place queue overflow and random wire loss at their actual
// locations in the pipe schematic.
const DROP_WIRE = 2

export type Phase = (typeof BBR_PHASE)[number]

export class RunData {
  inflight = newTrack() // bytes
  cwnd = newTrack() // packets
  srtt = newTrack() // seconds
  rttSample = newTrack() // latest raw sender RTT sample, seconds (wire_stats)
  delivery = newTrack() // bits/s
  appBytes = newTrack() // cumulative receiver-side app bytes (true goodput counter)
  ccState = newTrack()
  qDepth = newTrack() // packets, forward link
  wireCV = newTrack() // bottleneck arrival-gap CV, forward link (wire_stats)
  fwdArrivalBytes = newTrack() // cumulative bytes offered at qdisc, including drops
  fwdEnqueueBytes = newTrack() // cumulative accepted bytes, actual link hook
  fwdDequeueBytes = newTrack() // cumulative transmitted bytes, actual link hook
  revEnqueuePkts = newTrack() // cumulative actual reverse packets (mostly ACKs)
  lossEvents: number[] = [] // sender loss-recovery entry times
  dropEvents: number[] = [] // forward queue/AQM/forced-drop times
  wireDropEvents: number[] = [] // forward random-loss times after serialization
  maxT = 0

  push(recs: Rec[]) {
    // Vite can retain RunData instances across a hot module replacement.
    // Lazily install newly added tracks so a development HMR cannot leave
    // an older instance shape behind.
    this.appBytes ??= newTrack()
    this.rttSample ??= newTrack()
    this.fwdArrivalBytes ??= newTrack()
    this.fwdEnqueueBytes ??= newTrack()
    this.fwdDequeueBytes ??= newTrack()
    this.revEnqueuePkts ??= newTrack()
    this.wireDropEvents ??= []
    for (const r of recs) {
      if (r.t > this.maxT) this.maxT = r.t
      // Forward drops are attributed to the owning flow when known, with
      // LINK_FWD as fallback; reverse-direction (ACK-path) drops arrive as
      // LINK_REV and are excluded — they are not bottleneck-queue events.
      if (r.kind === Kind.Drop) {
        if (r.flow !== LINK_REV) {
          if (r.value === DROP_WIRE) this.wireDropEvents.push(r.t)
          else this.dropEvents.push(r.t)
        }
        continue
      }
      if (r.flow === LINK_FWD) {
        if (r.kind === Kind.QDepthPkts) {
          this.qDepth.t.push(r.t)
          this.qDepth.v.push(r.value)
        } else if (r.kind === Kind.WireBurstCV) {
          this.wireCV.t.push(r.t)
          this.wireCV.v.push(r.value)
        } else if (r.kind === Kind.LinkArrivalBytesCum) {
          this.fwdArrivalBytes.t.push(r.t)
          this.fwdArrivalBytes.v.push(r.value)
        } else if (r.kind === Kind.LinkEnqueueBytesCum) {
          this.fwdEnqueueBytes.t.push(r.t)
          this.fwdEnqueueBytes.v.push(r.value)
        } else if (r.kind === Kind.LinkDequeueBytesCum) {
          this.fwdDequeueBytes.t.push(r.t)
          this.fwdDequeueBytes.v.push(r.value)
        }
        continue
      }
      if (r.flow === LINK_REV) {
        if (r.kind === Kind.LinkEnqueuePktsCum) {
          this.revEnqueuePkts.t.push(r.t)
          this.revEnqueuePkts.v.push(r.value)
        }
        continue
      }
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
        case Kind.RTTSampleSec:
          this.rttSample.t.push(r.t)
          this.rttSample.v.push(r.value)
          break
        case Kind.DeliveryBps:
          this.delivery.t.push(r.t)
          this.delivery.v.push(r.value)
          break
        case Kind.BytesAckedCum:
          this.appBytes.t.push(r.t)
          this.appBytes.v.push(r.value)
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
  rawR?: number // latest unsmoothed RTT sample, ×base RTT (wire_stats)
  q: number // bottleneck queue, packets
  w?: number // cwnd, packets
  cv?: number // bottleneck arrival-gap CV (wire_stats; absent in old streams)
  goodB?: number // cumulative receiver-side app bytes (true goodput)
  arrB?: number // cumulative forward bytes presented to the queue, including drops
  enqB?: number // cumulative forward bytes accepted by the simulated link
  deqB?: number // cumulative forward bytes dequeued for transmission
  ackN?: number // cumulative reverse packets accepted (ACK stream in this scenario)
  phase?: Phase
}

export interface LossMark {
  t: number
  kind: 'queue' | 'wire'
  x: number
  y: number
  r: number // srtt ratio when the forward packet was dropped
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
  const rawRTT = new Cursor(run.rttSample ?? newTrack())
  const del = new Cursor(run.delivery)
  const app = new Cursor(run.appBytes ?? newTrack())
  const st = new Cursor(run.ccState)
  const q = new Cursor(run.qDepth)
  const wcv = new Cursor(run.wireCV)
  const arr = new Cursor(run.fwdArrivalBytes ?? newTrack())
  const enq = new Cursor(run.fwdEnqueueBytes ?? newTrack())
  const deq = new Cursor(run.fwdDequeueBytes ?? newTrack())
  const ack = new Cursor(run.revEnqueuePkts ?? newTrack())
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
    const rawRTTS = rawRTT.at(t)
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
    const appV = app.at(t)
    const arrV = arr.at(t)
    const enqV = enq.at(t)
    const deqV = deq.at(t)
    const ackV = ack.at(t)
    pts.push({
      t,
      x,
      y,
      r,
      rawR: Number.isNaN(rawRTTS) || rawRTTS <= 0 ? undefined : (rawRTTS * 1000) / d.baseMs,
      q: Number.isNaN(qP) ? 0 : qP,
      w: Number.isNaN(cwB) ? undefined : cwB / 1500,
      cv: Number.isNaN(cvV) ? undefined : cvV,
      goodB: Number.isNaN(appV) ? undefined : appV,
      arrB: Number.isNaN(arrV) ? undefined : arrV,
      enqB: Number.isNaN(enqV) ? undefined : enqV,
      deqB: Number.isNaN(deqV) ? undefined : deqV,
      ackN: Number.isNaN(ackV) ? undefined : ackV,
      phase: bbrPhases && !Number.isNaN(code) ? BBR_PHASE[code] : undefined,
    })
  }
  return pts
}

// Converts actual forward-link packet drops into readable figure markers.
// Queue and wire events are coalesced separately so a dense tail-drop burst
// does not flood the SVG, while nearby drops at the two distinct stages are
// both retained. Reverse-path ACK drops were excluded when RunData decoded
// the stream and therefore never masquerade as forward data loss here.
export function lossMarks(
  run: RunData,
  pts: Pt[],
  d: Derived,
  dt = 0.02,
  episodeS = 0.25,
): LossMark[] {
  const events: Pick<LossMark, 't' | 'kind'>[] = [
    ...coalesce(run.dropEvents, episodeS).map((t) => ({ t, kind: 'queue' as const })),
    ...coalesce(run.wireDropEvents, episodeS).map((t) => ({ t, kind: 'wire' as const })),
  ].sort((a, b) => a.t - b.t)
  return events.map(({ t, kind }) => {
    const p = ptAt(pts, t, dt)
    return {
      t,
      kind,
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
