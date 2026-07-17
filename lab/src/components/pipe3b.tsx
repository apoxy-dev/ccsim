// Figure 3 (design option 3b): the animated pipe — sender, bottleneck queue
// and receiver with packet dots, drop ballistics, and an RTT strip showing
// raw sender RTT samples with the sender's SRTT estimate dashed alongside.
//
// The wire is an aggregate particle view of actual simulator telemetry.
// Forward particles are crossings of cumulative bytes presented to and
// dequeued from the simulated qdisc; ACK particles are crossings of
// cumulative reverse packets. Queue-input arrivals include rejected packets,
// so overload remains visible instead of disappearing into the drop counter.
// The browser never runs its own queue or re-spaces dots.
// To keep the stream small, the cumulative counters are sampled at the run's
// metric cadence and crossing times are linearly interpolated between those
// measured points.
//
// Rendering: static chrome and the per-run srtt path are SVG; everything
// that moves is drawn on a canvas overlay from its own rAF loop reading
// tr.tRef, so animation costs a canvas blit, not DOM reconciliation. The
// component itself only re-renders on the transport's coarse (~8 Hz) tick.

import { memo, useEffect, useMemo, useRef, type ReactNode } from 'react'
import { FigureCard } from './figure-card'
import { Transport, type TransportState } from './transport'
import { COLORS } from '../lib/trail'
import { coalesce, ptAt, type Pt } from '../lib/series'
import { NAIVE_RATE_MBPS, type CC, type Derived, type LabCfg } from '../lib/scenario'

const tx = (t: number) => 52 + t * 19.2

// Playback cruises slow enough to read and drops another order of magnitude
// around events: at cruise an RTT is tens of ms of wall time, so a drop
// would be over before the eye lands on it.
export const CRUISE_RATE = 0.4
const SLOMO_RATE = 0.15
const SLOMO_WIN = 0.2 // seconds of sim time on each side of an event

// Times worth slowing down for: drop episodes (coalesced so a taildrop
// burst is one event) plus BBR phase changes. Under heavy random loss the
// event set gets dense enough that slow-mo would be permanent — worse than
// none — so it disables itself.
export function pipeEventTimes(pts: Pt[], queueDropTimes: number[], wireDropTimes: number[]): number[] {
  const evs = coalesce([...queueDropTimes, ...wireDropTimes].sort((a, b) => a - b), 0.25)
  for (let i = 1; i < pts.length; i++) {
    if (pts[i].phase !== pts[i - 1].phase) evs.push(pts[i].t)
  }
  return evs.sort((a, b) => a - b)
}

export function rateAt(events: number[], t: number): number {
  if (events.length > 40) return CRUISE_RATE
  for (const e of events) {
    if (Math.abs(t - e) <= SLOMO_WIN) return SLOMO_RATE
    if (e > t + SLOMO_WIN) break
  }
  return CRUISE_RATE
}

// Queue stack geometry: the stack is scaled so a full buffer exactly
// reaches the limit line — at the overflow frame, the most important one in
// the animation, the stack must be visibly touching the line as the drop
// glyph bounces off. Dots always render up to the limit; the count text is
// a supplement, never a replacement.
const STACK_BASE = 122
const LIMIT_Y = 50

// Visual density target. Together with the configured link rate this picks
// the byte quantum represented by one dot; all dot times still come from
// measured cumulative counters. The target is √-compressed so the 10–400
// Mbps slider range stays readable rather than becoming a solid band.
const visCapFor = (rateMbps: number) => Math.max(2.5, Math.min(16, 6 * Math.sqrt(rateMbps / 50)))
// The forward segments use one screen-space velocity so dot density can be
// compared across the box without a geometry-induced distortion.
const WIRE_PX_PER_S = 160
const PRE_T = 148 / WIRE_PX_PER_S
const POST_T = 228 / WIRE_PX_PER_S
const ACK_T = 1.6

// ACK return-path geometry. Keeping this in one place prevents the particle
// animation from silently taking a horizontal shortcut across the SVG path.
const ACK_PATH = [
  { x: 571, y: 170 },
  { x: 571, y: 200 },
  { x: 67, y: 200 },
  { x: 67, y: 176 },
] as const
const ACK_LEGS = ACK_PATH.slice(1).map((p, i) => {
  const from = ACK_PATH[i]
  return { from, to: p, len: Math.hypot(p.x - from.x, p.y - from.y) }
})
const ACK_PATH_LEN = ACK_LEGS.reduce((n, leg) => n + leg.len, 0)

function ackPos(progress: number): { x: number; y: number } {
  let left = Math.max(0, Math.min(1, progress)) * ACK_PATH_LEN
  for (const leg of ACK_LEGS) {
    if (left <= leg.len) {
      const f = leg.len === 0 ? 0 : left / leg.len
      return {
        x: leg.from.x + (leg.to.x - leg.from.x) * f,
        y: leg.from.y + (leg.to.y - leg.from.y) * f,
      }
    }
    left -= leg.len
  }
  return ACK_PATH[ACK_PATH.length - 1]
}

type CounterKey = 'arrB' | 'enqB' | 'deqB' | 'ackN'

// Times at which a measured cumulative counter crosses successive display
// quanta. Linear interpolation is only a 20 ms visualization decimation; the
// counter values themselves come directly from link enqueue/dequeue hooks.
function counterCrossings(pts: Pt[], key: CounterKey, quantum: number): number[] {
  const out: number[] = []
  let prevT = 0
  let prevV = 0
  let next = quantum
  for (const p of pts) {
    const v = p[key]
    if (v == null || v < prevV) continue
    const dv = v - prevV
    while (dv > 0 && next <= v) {
      out.push(prevT + ((next - prevV) / dv) * (p.t - prevT))
      next += quantum
    }
    prevT = p.t
    prevV = v
  }
  return out
}

// Lower bound: first index with a[i] >= v.
function lb(a: number[], v: number): number {
  let lo = 0
  let hi = a.length
  while (lo < hi) {
    const m = (lo + hi) >> 1
    if (a[m] < v) lo = m + 1
    else hi = m
  }
  return lo
}

export const Pipe3b = memo(function Pipe3b({
  pts,
  queueDropTimes,
  wireDropTimes,
  events,
  cfg,
  d,
  flow,
  onFlow,
  tr,
  T,
  controls,
}: {
  pts: Pt[]
  queueDropTimes: number[]
  wireDropTimes: number[]
  events: number[]
  cfg: LabCfg
  d: Derived
  flow: CC
  onFlow: (f: CC) => void
  tr: TransportState
  T: number
  controls?: ReactNode
}) {
  const col = flow === 'cubic' ? COLORS.cubic : flow === 'bbr' ? COLORS.bbr : COLORS.naive
  const base = d.baseMs
  // Keep the base RTT rule at y=272 while expanding the upper range for the
  // largest configured queue plus two independently jittered directions.
  // The default retains the original 1..2.25× scale.
  const modeledMaxRTT = base + (cfg.qlimPkts * 12) / cfg.rateMbps + 2 * cfg.jitterMs
  const rttTopRatio = Math.max(2.25, (modeledMaxRTT * 1.05) / base)
  const yS = (r: number) => 272 - 40 * Math.max(0, Math.min((r - 1) / (rttTopRatio - 1), 1))

  const limitDots = Math.max(4, Math.min(14, cfg.qlimPkts))
  const pktPerQDot = cfg.qlimPkts / limitDots
  const pitch = (STACK_BASE - LIMIT_Y) / limitDots
  const qDotR = Math.min(2.6, pitch * 0.42)

  // rawR is the latest unambiguous sender RTT sample (ACK time minus the
  // sampled segment's transmit time). It includes realized jitter in both
  // directions, queueing, serialization, and ACK timing. Before the first
  // sample there is deliberately no solid trace; reconstructing one from
  // queue depth would omit jitter and repeat the original bug.
  const rtt = useMemo(() => {
    if (pts.length < 2) return null
    const sampled = pts.filter((_, i) => i % 3 === 0)
    const path = (source: Pt[], val: (p: Pt) => number) =>
      source
        .map((p, i) => (i ? 'L' : 'M') + tx(p.t).toFixed(1) + ' ' + yS(val(p)).toFixed(1))
        .join(' ')
    const raw = sampled.filter((p): p is Pt & { rawR: number } => p.rawR != null)
    const actual = path(raw, (p) => p.rawR!)
    const firstX = raw.length > 0 ? tx(raw[0].t).toFixed(1) : null
    // Close at the last rendered sample. Closing at the fixed right edge
    // creates a diagonal wedge while a live stream is still arriving.
    const area = firstX == null ? null : `${actual} V 280 H ${firstX} Z`
    return { actual, area, srtt: path(sampled, (p) => p.r) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pts, rttTopRatio])

  // Rate strip: offered load (bytes presented to the queue, drops included)
  // against true goodput (receiver-side app bytes), both differentiated over
  // a ±160 ms window. Goodput deliberately does NOT come from the sender's
  // delivery-rate estimator: that is a per-burst instantaneous measure that
  // reads near wire rate even when most carried bytes are duplicate
  // retransmissions. The offered/goodput gap IS congestion: permanent
  // collapse for naive, a flash per overshoot for cubic, nothing for bbr.
  // The naive control needs headroom for its fixed 150 Mbps offered load.
  // Reusing that domain for feedback-controlled senders compresses Cubic's
  // loss-limited throughput into a few pixels, so give Cubic and BBR a
  // link-relative domain with a small amount of headroom instead.
  const rateMax = cfg.rateMbps * (flow === 'naive' ? 1.6 : 1.1)
  const yR = (v: number) => 364 - 44 * Math.min(v / rateMax, 1)
  const rate = useMemo(() => {
    if (pts.length < 2) return null
    const offKey: CounterKey | null = pts.some((p) => p.arrB != null)
      ? 'arrB'
      : pts.some((p) => p.enqB != null)
        ? 'enqB'
        : null
    const W = 8
    const deriv = (get: (p: Pt) => number | undefined) => {
      const out = new Array<number>(pts.length).fill(0)
      for (let i = 0; i < pts.length; i++) {
        const a = pts[Math.max(0, i - W)]
        const b = pts[Math.min(pts.length - 1, i + W)]
        const dt2 = b.t - a.t
        if (dt2 > 0) out[i] = ((get(b) ?? 0) - (get(a) ?? 0)) * 8 / 1e6 / dt2
      }
      return out
    }
    const offered = offKey ? deriv((p) => p[offKey]) : new Array<number>(pts.length).fill(0)
    const hasGood = pts.some((p) => p.goodB != null)
    const good = hasGood
      ? deriv((p) => p.goodB)
      : pts.map((p) => p.y * cfg.rateMbps) // old streams: sender estimate fallback
    const sampled = pts.filter((_, i) => i % 3 === 0)
    const path = (vals: number[]) => {
      let s = ''
      for (let i = 0; i < sampled.length; i++) {
        const p = sampled[i]
        const sourceIndex = i * 3
        s += (s ? 'L' : 'M') + tx(p.t).toFixed(1) + ' ' + yR(vals[sourceIndex]).toFixed(1)
      }
      return s
    }
    const goodPath = path(good)
    const firstX = tx(sampled[0].t).toFixed(1)
    return {
      offered,
      good,
      offeredPath: offKey ? path(offered) : null,
      goodPath,
      area: `${goodPath} V 364 H ${firstX} Z`,
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pts, cfg.rateMbps])

  // Drop glyph decimation happens once so each surviving ✕ keeps a stable
  // index (and thus fan lane) for its whole ballistic flight; per-frame
  // sampling would reshuffle the population and read as flicker.
  // pts is in the deps because RunData's event arrays are mutated in place
  // as sample batches arrive. A newly derived pts array marks the refresh.
  const visQueueDrops = useMemo(
    () => coalesce(queueDropTimes, 0.9 / 25),
    [queueDropTimes, pts],
  )
  const visWireDrops = useMemo(
    () => coalesce(wireDropTimes, 0.9 / 25),
    [wireDropTimes, pts],
  )

  // Display aggregation only: the quantum controls how many actual bytes one
  // visible dot represents. Its timing comes from measured cumulative link
  // counters below, not from cwnd, RTT, or a browser-side service model.
  const visCap = visCapFor(cfg.rateMbps)
  const dataQuantum = (cfg.rateMbps * 1e6) / 8 / visCap
  // Delayed ACKs commonly cover multiple data packets. Choose an ACK display
  // quantum that yields a readable return stream while retaining the timing
  // of the simulator's actual reverse packets.
  const ackQuantum = Math.max(1, dataQuantum / 1500 / 2)
  const wireR = visCap > 9 ? 2.4 : 3 // dot radius shrinks on dense fast links
  const sched = useMemo(() => {
    // New streams carry all qdisc arrivals, including packets rejected by a
    // full queue. Fall back to accepted enqueue bytes for older streams.
    const arrivalKey: CounterKey = pts.some((p) => p.arrB != null) ? 'arrB' : 'enqB'
    const em = counterCrossings(pts, arrivalKey, dataQuantum)
    // Dequeue timestamps are already the simulator's real service schedule.
    // PRE_T is only the schematic sender-to-qdisc travel offset.
    const dep = counterCrossings(pts, 'deqB', dataQuantum).map((t) => t + PRE_T)
    // The simulator counter is stamped where the reverse packet enters the
    // link. Shift the whole measured series onto the schematic timeline so
    // ACKs begin at the receiver after the corresponding forward traversal,
    // without changing any measured inter-ACK spacing.
    const ackShift = PRE_T + POST_T - cfg.owdMs / 1000
    const ack = counterCrossings(pts, 'ackN', ackQuantum).map((t) => t + ackShift)
    return { em, dep, ack }
  }, [pts, dataQuantum, ackQuantum, cfg.owdMs])

  const cvRef = useRef<HTMLCanvasElement>(null)
  useEffect(() => {
    const cv = cvRef.current
    const ctx = cv?.getContext('2d')
    if (!cv || !ctx) return
    const mono = (px: number, weight = 400) => `${weight} ${px}px "JetBrains Mono", monospace`
    const dot = (x: number, y: number, r: number) => {
      ctx.beginPath()
      ctx.arc(x, y, r, 0, Math.PI * 2)
      ctx.fill()
    }
    const cross = (x: number, y: number) => {
      ctx.beginPath()
      ctx.moveTo(x - 3, y - 3)
      ctx.lineTo(x + 3, y + 3)
      ctx.moveTo(x - 3, y + 3)
      ctx.lineTo(x + 3, y - 3)
      ctx.stroke()
    }
    let raf = 0
    const draw = () => {
      raf = requestAnimationFrame(draw)
      const rect = cv.getBoundingClientRect()
      const dpr = window.devicePixelRatio || 1
      const w = Math.round(rect.width * dpr)
      const h = Math.round(rect.height * dpr)
      if (w === 0 || h === 0) return
      if (cv.width !== w || cv.height !== h) {
        cv.width = w
        cv.height = h
      }
      const s = w / 640
      ctx.setTransform(s, 0, 0, s, 0, 0)
      ctx.clearRect(0, 0, 640, 376)
      const t = tr.tRef.current
      const p = ptAt(pts, t)
      if (!p) return

      // Queue stack + count.
      const infPkts = p.x * d.bdpPkts
      const q = Math.min(p.q, cfg.qlimPkts)
      const queuedPkts = Math.min(q, infPkts)
      const pathPkts = Math.max(0, infPkts - queuedPkts)
      const shown = Math.min(Math.round(q / pktPerQDot), limitDots)
      ctx.fillStyle = col
      for (let k = 0; k < shown; k++) dot(279, STACK_BASE - pitch * (k + 0.5), qDotR)
      if (q >= 1) {
        ctx.fillStyle = COLORS.slate
        ctx.font = mono(9)
        ctx.textAlign = 'center'
        ctx.fillText(`q ${Math.round(q)} pkt`, 279, 192)
      }

      ctx.fillStyle = COLORS.slate
      ctx.font = mono(8.5)
      ctx.textAlign = 'left'
      if (flow === 'naive') {
        // Under persistent loss SND.NXT-SND.UNA includes SACKed bytes behind
        // holes and is not a meaningful physical path occupancy. Show the
        // controller's actual invariant instead of relabeling that span.
        ctx.fillText(`fixed pacer ${NAIVE_RATE_MBPS} Mbps`, 24, 87)
        ctx.font = mono(7.5)
        ctx.fillText('ignores congestion feedback', 24, 100)
      } else {
        // Sender gauge: cwnd and in-flight in packets, on a scale of BDP +
        // buffer (the most the path can hold). Split the in-flight fill into
        // packets on the path and packets queued at the bottleneck. Cubic's
        // window growth is mostly the latter once the wire reaches capacity;
        // a faster-looking post-wire stream would incorrectly imply that the
        // fixed-rate bottleneck had gained bandwidth.
        const maxPkts = d.bdpPkts + cfg.qlimPkts
        const barW = 86
        const pathW = barW * Math.min(1, pathPkts / maxPkts)
        const queueW = barW * Math.min(1 - pathW / barW, queuedPkts / maxPkts)
        ctx.globalAlpha = 0.2
        ctx.fillStyle = col
        ctx.fillRect(24, 94, pathW, 6)
        ctx.globalAlpha = 0.68
        ctx.fillRect(24 + pathW, 94, queueW, 6)
        ctx.globalAlpha = 1
        ctx.strokeStyle = COLORS.fog
        ctx.lineWidth = 0.75
        ctx.strokeRect(24, 94, barW, 6)
        ctx.fillStyle = COLORS.slate
        ctx.fillText(`cwnd ${p.w != null ? Math.round(p.w) : '–'} · in flight ${Math.round(infPkts)} pkt`, 24, 87)
        ctx.font = mono(7.5)
        ctx.fillText(`path ${Math.round(pathPkts)} + queue ${Math.round(queuedPkts)}`, 24, 107)
        if (p.w != null) {
          const cwX = 24 + barW * Math.min(1, p.w / maxPkts)
          ctx.strokeStyle = COLORS.ink
          ctx.lineWidth = 1.2
          ctx.beginPath()
          ctx.moveTo(cwX, 92)
          ctx.lineTo(cwX, 102)
          ctx.stroke()
        }
      }

      // Wire dots: pure functions of each dot's fixed schedule. A dot lives
      // on the pre-wire for exactly [em, em+PRE_T), moves at constant
      // speed, and leaves only by entering the bottleneck box.
      const { em, dep } = sched
      ctx.fillStyle = col
      for (let k = lb(em, t - PRE_T); k < em.length && em[k] <= t; k++) {
        dot(114 + ((t - em[k]) / PRE_T) * 148, 140, wireR)
      }
      for (let k = lb(dep, t - POST_T); k < dep.length && dep[k] <= t; k++) {
        dot(296 + ((t - dep[k]) / POST_T) * 228, 140, wireR)
      }
      // ACKs: aggregate crossings of actual reverse packets emitted by the
      // receiver-side stack, including its delayed-ACK behavior.
      ctx.globalAlpha = 0.6
      const { ack } = sched
      for (let k = lb(ack, t - ACK_T); k < ack.length && ack[k] <= t; k++) {
        const a = ackPos((t - ack[k]) / ACK_T)
        dot(a.x, a.y, Math.max(1.5, wireR - 1))
      }
      ctx.globalAlpha = 1

      // Each ✕ is an actual dropped packet, decimated once so dense overload
      // stays readable. Queue overflow flies from the full queue; Bernoulli
      // wire loss starts downstream, where the link drops it after
      // serialization. Keeping separate origins avoids falsely presenting
      // random loss as another tail drop.
      ctx.strokeStyle = COLORS.loss
      ctx.lineWidth = 1.4
      for (let k = lb(visQueueDrops, t - 0.9); k < visQueueDrops.length && visQueueDrops[k] <= t; k++) {
        const f = (t - visQueueDrops[k]) / 0.9
        const fx = 290 + f * (110 + (k % 5) * 12)
        const fy = LIMIT_Y - 2 - f * 80 + f * f * 150
        ctx.globalAlpha = 1 - f
        cross(fx, fy)
      }
      for (let k = lb(visWireDrops, t - 0.9); k < visWireDrops.length && visWireDrops[k] <= t; k++) {
        const f = (t - visWireDrops[k]) / 0.9
        const lane = (k % 5) - 2
        const fx = 408 + f * (70 + lane * 9)
        const fy = 140 + f * (30 + Math.abs(lane) * 5) + f * f * 60
        ctx.globalAlpha = 1 - f
        cross(fx, fy)
      }
      ctx.globalAlpha = 1

      // RTT strip playhead, raw-sample marker and readout.
      ctx.strokeStyle = COLORS.ink
      ctx.globalAlpha = 0.35
      ctx.lineWidth = 1
      ctx.beginPath()
      ctx.moveTo(tx(t), 232)
      ctx.lineTo(tx(t), 280)
      ctx.stroke()
      ctx.globalAlpha = 1
      const rawRatio = p.rawR
      ctx.textAlign = 'right'
      if (rawRatio != null) {
        const rawRTT = rawRatio * base
        ctx.fillStyle = col
        ctx.strokeStyle = '#FFFFFF'
        ctx.lineWidth = 1.2
        ctx.beginPath()
        ctx.arc(tx(t), yS(rawRatio), 3.2, 0, Math.PI * 2)
        ctx.fill()
        ctx.stroke()
        const variable = Math.max(0, rawRTT - base)
        ctx.font = mono(10.5, 600)
        ctx.fillText(`latest rtt ${rawRTT.toFixed(1)} ms · ${base} base + ${variable.toFixed(1)} variable`, 628, 220)
      } else {
        ctx.fillStyle = COLORS.stone
        ctx.font = mono(9)
        ctx.fillText('waiting for first raw rtt sample', 628, 220)
      }
      ctx.font = mono(9)
      ctx.fillStyle = COLORS.stone
      ctx.fillText(`sender srtt ${(p.r * base).toFixed(1)} ms`, 628, 232)

      // Rate strip playhead, marker and readout.
      if (rate) {
        ctx.strokeStyle = COLORS.ink
        ctx.globalAlpha = 0.35
        ctx.lineWidth = 1
        ctx.beginPath()
        ctx.moveTo(tx(t), 320)
        ctx.lineTo(tx(t), 364)
        ctx.stroke()
        ctx.globalAlpha = 1
        const ri = Math.min(rate.good.length - 1, Math.max(0, Math.round(t / 0.02)))
        const good = rate.good[ri]
        ctx.fillStyle = col
        ctx.strokeStyle = '#FFFFFF'
        ctx.lineWidth = 1.2
        ctx.beginPath()
        ctx.arc(tx(t), yR(good), 3.2, 0, Math.PI * 2)
        ctx.fill()
        ctx.stroke()
        ctx.font = mono(10.5, 600)
        ctx.textAlign = 'right'
        ctx.fillText(`goodput ${good.toFixed(1)} Mbps`, 628, 306)
        const gw = ctx.measureText(`goodput ${good.toFixed(1)} Mbps`).width + 8
        ctx.font = mono(9)
        ctx.fillStyle = COLORS.stone
        ctx.fillText(`offered ${rate.offered[ri].toFixed(1)} ·`, 628 - gw, 306)
      }
    }
    raf = requestAnimationFrame(draw)
    return () => cancelAnimationFrame(raf)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pts, sched, visQueueDrops, visWireDrops, rate, cfg, d, col, base, limitDots, pktPerQDot, pitch, qDotR, wireR, tr.tRef])

  const t = tr.t
  const nextEv = events.find((e) => e > t + SLOMO_WIN)
  const slo = rateAt(events, t) < CRUISE_RATE

  return (
    <FigureCard
      title="FIG. 1 - BOTTLENECK"
      aside={
        <div style={{ display: 'flex', gap: 8 }}>
          <button className={flow === 'naive' ? 'btn-toggle on' : 'btn-toggle'} onClick={() => onFlow('naive')}>
            NAIVE
          </button>
          <button className={flow === 'cubic' ? 'btn-toggle on' : 'btn-toggle'} onClick={() => onFlow('cubic')}>
            CUBIC
          </button>
          <button className={flow === 'bbr' ? 'btn-toggle on' : 'btn-toggle'} onClick={() => onFlow('bbr')}>
            BBRV3
          </button>
        </div>
      }
      controls={controls}
      note={
        flow === 'naive' ? (
          <>
            Naive ignores congestion and keeps offering {NAIVE_RATE_MBPS} Mbps. On a{' '}
            {cfg.rateMbps} Mbps bottleneck, the denser input train fills the queue and the excess
            becomes <span style={{ color: COLORS.loss }}>actual tail drops</span>; the output train
            remains limited by the simulated link. Crosses launched from the full queue are tail
            drops; with configured random loss, crosses instead leave the downstream wire. ACKs
            and retransmissions are still real TCP.
            Note the RTT strip: the solid line is the latest raw TCP RTT sample, including realized
            jitter and queueing, while the sender&apos;s own srtt (dashed) barely moves — with
            thousands of packets in flight under sustained loss, TCP&apos;s smoothed estimator is
            nearly frozen. A sender this aggressive cannot even see the damage it does. The
            delivery strip shows classic congestion collapse: {NAIVE_RATE_MBPS} Mbps offered, the
            wire 100% busy — and goodput around a quarter of the link rate, because most of what
            the bottleneck carries are duplicate retransmissions of data dropped on earlier
            attempts. Useful work collapses while utilization stays perfect.
          </>
        ) : (
          <>
            Actual simulated traffic, aggregated so the particles remain readable. Their timing
            comes from forward and reverse link activity; the browser does not run another traffic
            model or impose output spacing. Cubic's growing window appears in the in-flight bar and
            queue stack, ACKs follow the complete return path, and queue-overflow{' '}
            <span style={{ color: COLORS.loss }}>✕ marks leave the queue</span> while random-loss
            marks leave the downstream wire. Playback
            cruises at {CRUISE_RATE}× and slows to {SLOMO_RATE}× around losses and phase changes.
          </>
        )
      }
    >
      <div style={{ position: 'relative', maxWidth: 640 }}>
        <svg viewBox="0 0 640 376" width="100%" style={{ display: 'block' }}>
          <line x1={266} y1={LIMIT_Y} x2={292} y2={LIMIT_Y} stroke={COLORS.loss} strokeWidth={1.2} />
          <text x={260} y={LIMIT_Y + 3} textAnchor="end" fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.loss}>
            buffer limit · {Math.round(cfg.qlimPkts)} pkt
          </text>
          <rect x={24} y={110} width={86} height={60} fill="none" stroke={COLORS.ink} strokeWidth={1.3} />
          <text x={67} y={144} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.ink}>
            SENDER
          </text>
          <line x1={110} y1={140} x2={266} y2={140} stroke={COLORS.fog} strokeWidth={1} />
          <rect x={262} y={122} width={34} height={36} fill="none" stroke={COLORS.ink} strokeWidth={1.3} />
          <g aria-label="bottleneck constriction">
            <path
              d="M 265 125 H 293 V 130 L 283 138 H 275 L 265 130 Z"
              fill={COLORS.ink}
              fillOpacity={0.12}
              stroke={COLORS.ink}
              strokeWidth={0.8}
            />
            <path
              d="M 265 155 H 293 V 150 L 283 142 H 275 L 265 150 Z"
              fill={COLORS.ink}
              fillOpacity={0.12}
              stroke={COLORS.ink}
              strokeWidth={0.8}
            />
          </g>
          <text x={279} y={180} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.slate}>
            bottleneck · {cfg.rateMbps} Mbps · tail-drop queue
          </text>
          <line x1={292} y1={140} x2={528} y2={140} stroke={COLORS.fog} strokeWidth={1} />
          <rect x={528} y={110} width={86} height={60} fill="none" stroke={COLORS.ink} strokeWidth={1.3} />
          <text x={571} y={144} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.ink}>
            RECEIVER
          </text>
          <path d="M 571 170 L 571 200 L 67 200 L 67 176" fill="none" stroke={COLORS.fog} strokeWidth={0.75} />
          <path d="M 63 182 L 67 174 L 71 182" fill="none" stroke={COLORS.stone} strokeWidth={1} />
          <text x={319} y={212} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.stone}>
            acks
          </text>
          <text x={52} y={230} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.stone} letterSpacing="0.08em">
            ROUND-TRIP TIME
          </text>
          <line x1={52} y1={yS(1)} x2={628} y2={yS(1)} stroke={COLORS.fog} strokeWidth={1} strokeDasharray="4 3" />
          <text x={52} y={288} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.stone}>
            base rtt {base} ms
          </text>
          {rtt && (
            <>
              {rtt.area && <path d={rtt.area} fill={col} fillOpacity={0.1} stroke="none" />}
              {rtt.actual && <path d={rtt.actual} fill="none" stroke={col} strokeWidth={1.3} />}
              <path d={rtt.srtt} fill="none" stroke={COLORS.stone} strokeWidth={0.9} strokeDasharray="3 3" />
            </>
          )}
          <text x={52} y={306} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.stone} letterSpacing="0.08em">
            DELIVERY RATE
          </text>
          <line x1={52} y1={yR(cfg.rateMbps)} x2={628} y2={yR(cfg.rateMbps)} stroke={COLORS.fog} strokeWidth={1} strokeDasharray="4 3" />
          <text x={52} y={373} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.stone}>
            link {cfg.rateMbps} Mbps
          </text>
          {rate && (
            <>
              <path d={rate.area} fill={col} fillOpacity={0.1} stroke="none" />
              <path d={rate.goodPath} fill="none" stroke={col} strokeWidth={1.3} />
              {rate.offeredPath && (
                <path d={rate.offeredPath} fill="none" stroke={COLORS.stone} strokeWidth={0.9} strokeDasharray="3 3" />
              )}
            </>
          )}
        </svg>
        <canvas
          ref={cvRef}
          style={{ position: 'absolute', inset: 0, width: '100%', height: '100%', pointerEvents: 'none' }}
        />
      </div>
      <Transport
        tr={tr}
        T={T}
        extra={
          <>
            <span className="transport-t" style={{ minWidth: 52 }}>
              {slo ? 'slo-mo' : `${CRUISE_RATE.toFixed(2)}×`}
            </span>
            <button
              className="btn-toggle"
              // naive is one continuous drop episode; skipping "to the next
              // event" is meaningless there.
              disabled={flow === 'naive' || nextEv == null}
              onClick={() => nextEv != null && tr.seek(nextEv - 0.3)}
            >
              EVENT ▸
            </button>
          </>
        }
      />
    </FigureCard>
  )
})
