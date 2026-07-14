// Figure 3 (design option 3b): the animated pipe — sender, bottleneck queue
// and receiver with packet dots, drop ballistics, and the srtt strip that
// spells out the bufferbloat equation (srtt = base + queueing delay).
//
// Wire-dot spacing is the figure's argument: right of the bottleneck the
// spacing is always even — serialization at link rate paces everyone's
// output — while the left side shows how the sender transmits: cubic's
// ack-clocked bursts vs bbr's pacing. Flipping the CC toggle smooths the
// left side and never changes the right, which is the entire basis of
// delivery-rate estimation drawn as dot spacing.

import { useMemo, type ReactElement } from 'react'
import { FigureCard } from './figure-card'
import { Transport, type TransportState } from './transport'
import { COLORS, cross } from '../lib/trail'
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

export function Pipe3b({
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
  const t = tr.t
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

  const p = ptAt(pts, t)
  const els: ReactElement[] = []
  if (p) {
    const infPkts = p.x * d.bdpPkts
    const q = Math.min(p.q, cfg.qlimPkts)
    const shown = Math.min(Math.round(q / pktPerQDot), limitDots)
    for (let k = 0; k < shown; k++) {
      els.push(
        <circle key={'q' + k} cx={279} cy={(STACK_BASE - pitch * (k + 0.5)).toFixed(1)} r={qDotR} fill={col} />,
      )
    }
    if (q >= 1) {
      els.push(
        <text key="qn" x={279} y={192} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.slate}>
          q {Math.round(q)} pkt
        </text>,
      )
    }

    // Sender gauge: cwnd and in-flight in packets, on a scale of
    // BDP + buffer (the most the path can hold). This is the congestion
    // control loop made visible: the CC raises cwnd, in-flight follows,
    // the wire fills, the queue builds, a drop bounces off the limit line,
    // cwnd is cut. The bar sawtooths for cubic and holds flat for bbr.
    const maxPkts = d.bdpPkts + cfg.qlimPkts
    const barW = 86
    els.push(
      <rect key="ib" x={24} y={96} width={(barW * Math.min(1, infPkts / maxPkts)).toFixed(1)} height={6} fill={col} fillOpacity={0.45} />,
      <rect key="ibf" x={24} y={96} width={barW} height={6} fill="none" stroke={COLORS.fog} strokeWidth={0.75} />,
      <text key="cwt" x={24} y={90} fontFamily="JetBrains Mono" fontSize={8.5} fill={COLORS.slate}>
        cwnd {p.w != null ? Math.round(p.w) : '–'} · in flight {Math.round(infPkts)} pkt
      </text>,
    )
    if (p.w != null) {
      const cwX = 24 + barW * Math.min(1, p.w / maxPkts)
      els.push(<line key="cw" x1={cwX.toFixed(1)} y1={94} x2={cwX.toFixed(1)} y2={104} stroke={COLORS.ink} strokeWidth={1.2} />)
    }

    // Wire burstiness from the measured arrival-gap CV (wire_stats record;
    // paced bbr ≈ 0.0–0.06, ack-clocked cubic ≈ 1.0, recovery spikes 4+),
    // averaged over ±100 ms so single-window spikes don't jerk the dots.
    const i0 = Math.round(t / 0.02)
    let cvSum = 0
    let cvN = 0
    for (let k = -5; k <= 5; k++) {
      const pp = pts[i0 + k]
      if (pp?.cv != null) {
        cvSum += pp.cv
        cvN++
      }
    }
    const b = Math.min(1, (cvN ? cvSum / cvN : flow === 'cubic' ? 1 : 0.05) / 1.2)

    // Pre-bottleneck wire: dot layout lerps from even spacing (b=0) to
    // tight clumps of 3 (b=1). Slot positions are fixed — a count change
    // pops a dot at the train's end instead of reshuffling every phase.
    const wirePkts = Math.max(0, infPkts - q)
    const PRE_SLOTS = 12
    const filledPre = Math.min(PRE_SLOTS, Math.round(wirePkts / (d.bdpPkts / 10)))
    for (let i = 0; i < filledPre; i++) {
      const burst = Math.floor(i / 3)
      const evenPh = i / PRE_SLOTS
      const clumpPh = burst / (PRE_SLOTS / 3) + (i % 3) * 0.008 + 0.04 * Math.sin(burst * 5.7)
      const ph = evenPh + (clumpPh - evenPh) * b
      const pr = (((t * 0.55 + ph) % 1) + 1) % 1
      els.push(<circle key={'w' + i} cx={(114 + pr * 148).toFixed(1)} cy={140} r={3} fill={col} />)
    }

    // Post-bottleneck: a backlogged bottleneck emits back-to-back — even
    // spacing at exactly the serialization pitch, whatever the sender does.
    // An idle link instead passes the sender's gaps through: bursts leave
    // at the pitch (never tighter), separated by the sender's idle gaps.
    const POST_SLOTS = 10
    const filled = Math.max(1, Math.min(POST_SLOTS, Math.round(p.y * POST_SLOTS)))
    const idle = Math.max(0, Math.min(1, 1 - q / 5)) * b
    const postPh: number[] = []
    for (let i = 0; i < filled; i++) {
      const burst = Math.floor(i / 3)
      const evenPh = i / POST_SLOTS
      const clumpPh = burst / Math.ceil(POST_SLOTS / 3) + (i % 3) / POST_SLOTS
      postPh.push(evenPh + (clumpPh - evenPh) * idle)
    }
    postPh.forEach((ph, i) => {
      const pr = (((t * 0.55 + ph) % 1) + 1) % 1
      els.push(<circle key={'p' + i} cx={(296 + pr * 228).toFixed(1)} cy={140} r={3} fill={col} />)
    })
    // ACKs inherit the bottleneck spacing — the return stream mirrors the
    // post-bottleneck train, and that is what ack-clocking feeds on.
    postPh.forEach((ph, i) => {
      const pr = (((t * 0.5 + ph) % 1) + 1) % 1
      els.push(
        <circle key={'ak' + i} cx={(556 - pr * 476).toFixed(1)} cy={200} r={2} fill={col} fillOpacity={0.6} />,
      )
    })

    // Each ✕ is one dropped packet. Losses are rare, discrete, and causally
    // important, so they are exempt from the dot aggregation: the glyph
    // bounces off the limit line and falls away, fanning so a burst reads
    // as several packets rather than one smear.
    let fan = 0
    for (let i = 0; i < dropTimes.length; i++) {
      const age = t - dropTimes[i]
      if (age < 0) break
      if (age >= 0.9) continue
      const f = age / 0.9
      const fx = 290 + f * (110 + (fan % 5) * 12)
      const fy = LIMIT_Y - 2 - f * 80 + f * f * 150
      els.push(cross(fx, fy, COLORS.loss, 1 - f, 'dx' + i, 3))
      if (++fan > 24) break
    }
  }

  const nextEv = events.find((e) => e > t + SLOMO_WIN)
  const slo = p != null && rateAt(events, t) < CRUISE_RATE

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
          The congestion-control loop, drawn live: the sender raises cwnd (bar), in-flight fills
          the wire, the surplus stacks up in the queue, a drop bounces off the buffer limit, cwnd
          is cut. While the queue is backlogged the bottleneck emits back-to-back — even spacing
          at link rate, whatever the sender does; an idle link passes the sender's gaps through.
          Left-side spacing is the measured arrival burstiness (inter-arrival CV per 20 ms
          window): cubic's ack-clocked bursts vs bbr's pacing. One queue dot ≈{' '}
          {Math.max(1, Math.round(pktPerQDot))} packets; each{' '}
          <span style={{ color: COLORS.loss }}>✕ is one dropped packet</span>. Playback cruises at{' '}
          {CRUISE_RATE}× and slows to {SLOMO_RATE}× around losses and phase changes; everything is
          driven by the selected run's real sample stream.
        </>
      }
    >
      <svg viewBox="0 0 640 292" width="100%" style={{ display: 'block', maxWidth: 640 }}>
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

        {/* live layers */}
        {srtt && (
          <>
            <path d={srtt.area} fill={col} fillOpacity={0.1} stroke="none" />
            <path d={srtt.line} fill="none" stroke={col} strokeWidth={1.3} />
          </>
        )}
        <line x1={tx(t)} y1={236} x2={tx(t)} y2={280} stroke={COLORS.ink} strokeWidth={1} strokeOpacity={0.35} />
        {p && (
          <>
            <circle cx={tx(t).toFixed(1)} cy={yS(p.r).toFixed(1)} r={3.2} fill={col} stroke="#FFFFFF" strokeWidth={1.2} />
            <text
              x={628}
              y={230}
              textAnchor="end"
              fontFamily="JetBrains Mono"
              fontSize={10.5}
              fontWeight={600}
              fill={col}
            >
              srtt {(p.r * base).toFixed(1)} ms = {base} base + {((p.r - 1) * base).toFixed(1)} queue
            </text>
          </>
        )}
        {els}
      </svg>
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
}
