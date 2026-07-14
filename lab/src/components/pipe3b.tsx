// Figure 3 (design option 3b): the animated pipe — sender, bottleneck queue
// and receiver with packet dots, drop ballistics, and the srtt strip that
// spells out the bufferbloat equation (srtt = base + queueing delay).
//
// The wire is a particle system with permanent dot identity, precomputed
// from the run data. Each visual dot has a fixed emission time (integrated
// from the sampled send rate = inflight/rtt, clumped by the measured
// arrival-gap CV) and flows through an actual FIFO at the bottleneck:
// depart[k] = max(arrive[k], depart[k-1] + serialization). Even output
// spacing is therefore not painted on — it emerges only while the queue is
// backlogged, and when the queue drains (after a cwnd cut) the sender's
// clumps pass straight through. A dot on the wire can never disappear or
// respace mid-flight: its position is a pure function of its schedule.
//
// Rendering: static chrome and the per-run srtt path are SVG; everything
// that moves is drawn on a canvas overlay from its own rAF loop reading
// tr.tRef, so animation costs a canvas blit, not DOM reconciliation. The
// component itself only re-renders on the transport's coarse (~8 Hz) tick.

import { memo, useEffect, useMemo, useRef } from 'react'
import { FigureCard } from './figure-card'
import { Transport, type TransportState } from './transport'
import { COLORS } from '../lib/trail'
import { coalesce, ptAt, type Pt } from '../lib/series'
import type { CC, Derived, LabCfg } from '../lib/scenario'

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
export function pipeEventTimes(pts: Pt[], dropTimes: number[]): number[] {
  const evs = coalesce(dropTimes, 0.25)
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

// Visual wire physics. visCap is the bottleneck's visual serialization
// rate in dots/s; the sender emits at visCap × (send-rate fraction of link
// capacity), so occupancy and spacing carry real meaning: the pre-wire can
// only get denser than the post-wire while the sender oversends, and the
// surplus is exactly what the queue absorbs. visCap scales with the
// configured link rate (√-compressed so the 10–400 Mbps slider range stays
// readable): a faster link visibly serializes more, tighter-spaced dots.
const visCapFor = (rateMbps: number) => Math.max(2.5, Math.min(16, 6 * Math.sqrt(rateMbps / 50)))
const PRE_T = 1.5 // visual pre-bottleneck transit, seconds
const POST_T = 1.4
const ACK_T = 1.6

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
  dropTimes,
  events,
  cfg,
  d,
  flow,
  onFlow,
  tr,
  T,
}: {
  pts: Pt[]
  dropTimes: number[]
  events: number[]
  cfg: LabCfg
  d: Derived
  flow: CC
  onFlow: (f: CC) => void
  tr: TransportState
  T: number
}) {
  const col = flow === 'cubic' ? COLORS.cubic : COLORS.bbr
  const base = d.baseMs
  // srtt strip y-scale: base RTT sits on the dashed rule at y=272; the strip
  // spans [0.75, 2.25]×base so queueing delay reads as height above it.
  const yS = (r: number) => 280 - (48 * Math.max(0, Math.min((r - 0.75) * base, 1.5 * base))) / (1.5 * base)

  const limitDots = Math.max(4, Math.min(14, cfg.qlimPkts))
  const pktPerQDot = cfg.qlimPkts / limitDots
  const pitch = (STACK_BASE - LIMIT_Y) / limitDots
  const qDotR = Math.min(2.6, pitch * 0.42)

  const srtt = useMemo(() => {
    if (pts.length < 2) return null
    const line = pts
      .filter((_, i) => i % 3 === 0)
      .map((p, i) => (i ? 'L' : 'M') + tx(p.t).toFixed(1) + ' ' + yS(p.r).toFixed(1))
      .join(' ')
    return { line, area: line + ' L 628 280 L 52 280 Z' }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pts, base])

  // The dot schedule, precomputed once per run. Emission integrates the
  // sampled send rate (inflight/rtt, as a fraction of link capacity — the
  // standard rate identity); burstiness batches emissions into clumps whose
  // size follows the measured arrival-gap CV (wire_stats record; paced bbr
  // ≈ 0, ack-clocked cubic ≈ 1, recovery spikes higher), averaged over
  // ±100 ms so a single-window spike doesn't restructure the train.
  const visCap = visCapFor(cfg.rateMbps)
  // Intra-burst gap at emission: just above back-to-back at the visual
  // serialization pitch, so a clump reads as one burst at any link rate.
  const clumpGap = 0.27 / visCap
  const wireR = visCap > 9 ? 2.4 : 3 // dot radius shrinks on dense fast links
  const sched = useMemo(() => {
    const dt = 0.02
    const em: number[] = []
    let credit = 0
    for (let i = 0; i < pts.length; i++) {
      const p = pts[i]
      let cvS = 0
      let cvN = 0
      for (let k = -5; k <= 5; k++) {
        const pp = pts[i + k]
        if (pp?.cv != null) {
          cvS += pp.cv
          cvN++
        }
      }
      const b = Math.min(1, (cvN ? cvS / cvN : flow === 'cubic' ? 1 : 0.05) / 1.2)
      const factor = Math.max(0, Math.min(1.8, p.x / Math.max(p.r, 0.5)))
      const rate = factor * visCap
      credit += rate * dt
      const cs = 1 + Math.round(2 * b)
      while (credit >= cs) {
        // Exact crossing time within the step, so even trains have no
        // 20 ms quantization jitter.
        const t0 = p.t - (credit - cs) / Math.max(rate, 1e-6)
        for (let j = 0; j < cs; j++) em.push(t0 + j * clumpGap)
        credit -= cs
      }
    }
    em.sort((a, b2) => a - b2)
    // The bottleneck FIFO: arrivals that find the server busy wait; while
    // backlogged, departures tick at exactly the serialization pitch. The
    // backlog test is the run's measured queue depth, not the visual
    // integral, so the box only evens spacing out during the windows where
    // the real buffer actually held packets (cubic: ~99% of the run; bbr:
    // ~10%) and passes the sender's gaps through everywhere else.
    const dep: number[] = new Array(em.length)
    let prev = -1e9
    for (let k = 0; k < em.length; k++) {
      const arr = em[k] + PRE_T
      const backlogged = (ptAt(pts, arr)?.q ?? 0) >= 2
      prev = Math.max(arr, backlogged ? prev + 1 / visCap : prev)
      dep[k] = prev
    }
    return { em, dep }
  }, [pts, flow, visCap, clumpGap])

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
      ctx.clearRect(0, 0, 640, 292)
      const t = tr.tRef.current
      const p = ptAt(pts, t)
      if (!p) return

      // Queue stack + count.
      const infPkts = p.x * d.bdpPkts
      const q = Math.min(p.q, cfg.qlimPkts)
      const shown = Math.min(Math.round(q / pktPerQDot), limitDots)
      ctx.fillStyle = col
      for (let k = 0; k < shown; k++) dot(279, STACK_BASE - pitch * (k + 0.5), qDotR)
      if (q >= 1) {
        ctx.fillStyle = COLORS.slate
        ctx.font = mono(9)
        ctx.textAlign = 'center'
        ctx.fillText(`q ${Math.round(q)} pkt`, 279, 192)
      }

      // Sender gauge: cwnd and in-flight in packets, on a scale of
      // BDP + buffer (the most the path can hold). The bar sawtooths for
      // cubic and holds flat for bbr.
      const maxPkts = d.bdpPkts + cfg.qlimPkts
      const barW = 86
      ctx.globalAlpha = 0.45
      ctx.fillStyle = col
      ctx.fillRect(24, 96, barW * Math.min(1, infPkts / maxPkts), 6)
      ctx.globalAlpha = 1
      ctx.strokeStyle = COLORS.fog
      ctx.lineWidth = 0.75
      ctx.strokeRect(24, 96, barW, 6)
      ctx.fillStyle = COLORS.slate
      ctx.font = mono(8.5)
      ctx.textAlign = 'left'
      ctx.fillText(`cwnd ${p.w != null ? Math.round(p.w) : '–'} · in flight ${Math.round(infPkts)} pkt`, 24, 90)
      if (p.w != null) {
        const cwX = 24 + barW * Math.min(1, p.w / maxPkts)
        ctx.strokeStyle = COLORS.ink
        ctx.lineWidth = 1.2
        ctx.beginPath()
        ctx.moveTo(cwX, 94)
        ctx.lineTo(cwX, 104)
        ctx.stroke()
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
      // ACKs: one per delivered dot, departing the receiver when its packet
      // arrives — the return stream inherits the bottleneck's spacing,
      // which is what ack-clocking feeds on.
      ctx.globalAlpha = 0.6
      for (let k = lb(dep, t - POST_T - ACK_T); k < dep.length && dep[k] + POST_T <= t; k++) {
        dot(556 - ((t - dep[k] - POST_T) / ACK_T) * 476, 200, wireR - 1)
      }
      ctx.globalAlpha = 1

      // Each ✕ is one dropped packet. Losses are rare, discrete, and
      // causally important, so they are exempt from the dot aggregation:
      // the glyph bounces off the limit line and falls away, fanning so a
      // burst reads as several packets rather than one smear.
      ctx.strokeStyle = COLORS.loss
      ctx.lineWidth = 1.4
      let fan = 0
      for (let i = 0; i < dropTimes.length; i++) {
        const age = t - dropTimes[i]
        if (age < 0) break
        if (age >= 0.9) continue
        const f = age / 0.9
        const fx = 290 + f * (110 + (fan % 5) * 12)
        const fy = LIMIT_Y - 2 - f * 80 + f * f * 150
        ctx.globalAlpha = 1 - f
        ctx.beginPath()
        ctx.moveTo(fx - 3, fy - 3)
        ctx.lineTo(fx + 3, fy + 3)
        ctx.moveTo(fx - 3, fy + 3)
        ctx.lineTo(fx + 3, fy - 3)
        ctx.stroke()
        if (++fan > 24) break
      }
      ctx.globalAlpha = 1

      // srtt strip playhead, marker and readout.
      ctx.strokeStyle = COLORS.ink
      ctx.globalAlpha = 0.35
      ctx.lineWidth = 1
      ctx.beginPath()
      ctx.moveTo(tx(t), 236)
      ctx.lineTo(tx(t), 280)
      ctx.stroke()
      ctx.globalAlpha = 1
      ctx.fillStyle = col
      ctx.strokeStyle = '#FFFFFF'
      ctx.lineWidth = 1.2
      ctx.beginPath()
      ctx.arc(tx(t), yS(p.r), 3.2, 0, Math.PI * 2)
      ctx.fill()
      ctx.stroke()
      ctx.font = mono(10.5, 600)
      ctx.textAlign = 'right'
      ctx.fillText(
        `srtt ${(p.r * base).toFixed(1)} ms = ${base} base + ${((p.r - 1) * base).toFixed(1)} queue`,
        628,
        230,
      )
    }
    raf = requestAnimationFrame(draw)
    return () => cancelAnimationFrame(raf)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pts, sched, dropTimes, cfg, d, col, base, limitDots, pktPerQDot, pitch, qDotR, wireR, tr.tRef])

  const t = tr.t
  const nextEv = events.find((e) => e > t + SLOMO_WIN)
  const slo = rateAt(events, t) < CRUISE_RATE

  return (
    <FigureCard
      title="FIG. 3 — THE PIPE"
      aside={
        <div style={{ display: 'flex', gap: 8 }}>
          <button className={flow === 'cubic' ? 'btn-toggle on' : 'btn-toggle'} onClick={() => onFlow('cubic')}>
            CUBIC
          </button>
          <button className={flow === 'bbr' ? 'btn-toggle on' : 'btn-toggle'} onClick={() => onFlow('bbr')}>
            BBRV3
          </button>
        </div>
      }
      note={
        <>
          Why congestion control exists, drawn live. Each dot's journey is fixed the moment the
          sender emits it: emission rate and clumping come from the run's measured send rate
          (inflight/rtt) and burstiness (inter-arrival CV), and the bottleneck box is a real FIFO —
          while it is backlogged, dots leave at exactly the serialization pitch; when it drains
          (watch right after a cubic cwnd cut) the sender's gaps pass straight through. The wire
          itself can only hold about a bandwidth-delay product: once it is full, every further
          cwnd packet (bar, top left) lives in the queue, buying srtt instead of throughput — until
          the buffer limit, where <span style={{ color: COLORS.loss }}>✕ marks each dropped
          packet</span>. The wire serializes faster on a faster link: at {cfg.rateMbps} Mbps one
          wire dot ≈ {Math.max(1, Math.round((cfg.rateMbps * 1e6) / 8 / 1500 / visCap))} packets;
          one queue dot ≈ {Math.max(1, Math.round(pktPerQDot))} packets. Playback
          cruises at {CRUISE_RATE}× and slows to {SLOMO_RATE}× around losses and phase changes.
        </>
      }
    >
      <div style={{ position: 'relative', maxWidth: 640 }}>
        <svg viewBox="0 0 640 292" width="100%" style={{ display: 'block' }}>
          <line x1={266} y1={LIMIT_Y} x2={292} y2={LIMIT_Y} stroke={COLORS.loss} strokeWidth={1.2} />
          <text x={298} y={LIMIT_Y + 3} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.loss}>
            buffer limit · {Math.round(cfg.qlimPkts)} pkt
          </text>
          <rect x={24} y={110} width={86} height={60} fill="none" stroke={COLORS.ink} strokeWidth={1.3} />
          <text x={67} y={144} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.ink}>
            SENDER
          </text>
          <line x1={110} y1={140} x2={266} y2={140} stroke={COLORS.fog} strokeWidth={1} />
          <rect x={266} y={126} width={26} height={28} fill="none" stroke={COLORS.ink} strokeWidth={1.3} />
          <text x={279} y={180} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.slate}>
            bottleneck {cfg.rateMbps} Mbps
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
          <line x1={52} y1={272} x2={628} y2={272} stroke={COLORS.fog} strokeWidth={1} strokeDasharray="4 3" />
          <text x={52} y={288} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.stone}>
            base rtt {base} ms
          </text>
          {srtt && (
            <>
              <path d={srtt.area} fill={col} fillOpacity={0.1} stroke="none" />
              <path d={srtt.line} fill="none" stroke={col} strokeWidth={1.3} />
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
              disabled={nextEv == null}
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
