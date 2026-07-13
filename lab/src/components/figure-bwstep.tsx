// Figure 2: the bandwidth-change experiment from the BBR paper (ACM Queue
// 2016, figure 3) — a 10-Mbps, 40-ms BBR flow whose bottleneck doubles at
// t=20 s and halves back at t=40 s. Three lanes vs. time: delivery rate
// against the link-rate step, inflight, and srtt.

import { useMemo } from 'react'
import { FigureCard } from './figure-card'
import { Transport, type TransportState } from './transport'
import { COLORS } from '../lib/trail'
import { ptAt, type Pt } from '../lib/series'
import { BWSTEP_CFG, BWSTEP_DUR_S, derive } from '../lib/scenario'

const tx = (t: number) => 52 + t * 9.6
const d = derive(BWSTEP_CFG)

// Lane scales. Delivery in Mbps (normalized pts carry delivery/10Mbps),
// inflight in kB, srtt in ms.
const yDel = (mbps: number) => 110 - (90 * Math.max(0, Math.min(mbps, 24))) / 24
const yInf = (kb: number) => 226 - (76 * Math.max(0, Math.min(kb, 160))) / 160
const yRtt = (ms: number) => 330 - (76 * Math.max(0, Math.min(ms - 30, 130))) / 130

const STEPS = [
  { t: 20, label: 'BtlBw ×2' },
  { t: 40, label: 'BtlBw ÷2' },
]

export function FigureBwStep({
  pts,
  tr,
  error,
}: {
  pts: Pt[]
  tr: TransportState
  error?: string
}) {
  const t = tr.t
  const paths = useMemo(() => {
    if (pts.length < 2) return null
    const path = (fn: (p: Pt) => number) =>
      pts
        .filter((_, i) => i % 3 === 0)
        .map((p, i) => (i ? 'L' : 'M') + tx(p.t).toFixed(1) + ' ' + fn(p).toFixed(1))
        .join(' ')
    return {
      del: path((p) => yDel(p.y * BWSTEP_CFG.rateMbps)),
      inf: path((p) => yInf((p.x * d.bdpBytes) / 1000)),
      rtt: path((p) => yRtt(p.r * d.baseMs)),
    }
  }, [pts])

  const p = ptAt(pts, t)
  const linkRate = `M ${tx(0)} ${yDel(10)} L ${tx(20)} ${yDel(10)} L ${tx(20)} ${yDel(20)} L ${tx(40)} ${yDel(20)} L ${tx(40)} ${yDel(10)} L ${tx(60)} ${yDel(10)}`

  const lane = (y0: number, y1: number, label: string, ticks: [number, string][]) => (
    <g key={label}>
      <line x1={52} y1={y1} x2={628} y2={y1} stroke={COLORS.fog} strokeWidth={1} />
      <text
        x={52}
        y={y0 - 6}
        fontFamily="JetBrains Mono"
        fontSize={9}
        fill={COLORS.stone}
        letterSpacing="0.08em"
      >
        {label}
      </text>
      {ticks.map(([y, txt], i) => (
        <g key={i}>
          <line x1={46} y1={y} x2={52} y2={y} stroke={COLORS.stone} strokeWidth={1} />
          <text x={43} y={y + 3} textAnchor="end" fontFamily="JetBrains Mono" fontSize={8.5} fill={COLORS.stone}>
            {txt}
          </text>
        </g>
      ))}
    </g>
  )

  return (
    <FigureCard
      title="FIG. 2 — BANDWIDTH CHANGE · BBR, 10 MBPS × 40 MS"
      aside={
        <div className="fig-legend">
          <span style={{ color: COLORS.bbr }}>— bbrv3</span>
          <span style={{ color: COLORS.slate }}>— link rate</span>
        </div>
      }
      note={
        <>
          {error && <span className="err">run failed: {error} · </span>}
          The paper's figure 3: on the up-step delivery converges within a probe cycle (max-filter
          admits the new rate immediately); on the down-step the old BtlBw estimate must age out of
          the filter, so inflight and rtt spike first, then drain.{' '}
          {p && (
            <span style={{ color: COLORS.bbr }}>
              now: {(p.y * BWSTEP_CFG.rateMbps).toFixed(1)} Mbps · {((p.x * d.bdpBytes) / 1000).toFixed(0)} kB ·{' '}
              {(p.r * d.baseMs).toFixed(1)} ms · {p.phase}
            </span>
          )}
        </>
      }
    >
      <svg viewBox="0 0 640 360" width="640" height="360" style={{ display: 'block' }}>
        {lane(20, 110, 'DELIVERY RATE (MBPS)', [
          [yDel(10), '10'],
          [yDel(20), '20'],
        ])}
        {lane(140, 226, 'INFLIGHT (KB)', [
          [yInf(50), '50'],
          [yInf(100), '100'],
        ])}
        {lane(250, 330, 'SRTT (MS)', [
          [yRtt(40), '40'],
          [yRtt(80), '80'],
        ])}
        {STEPS.map((s) => (
          <g key={s.t}>
            <line x1={tx(s.t)} y1={20} x2={tx(s.t)} y2={330} stroke={COLORS.ink} strokeWidth={1} strokeDasharray="3 4" strokeOpacity={0.35} />
            <text x={tx(s.t) + 5} y={30} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.graphite}>
              {s.label}
            </text>
          </g>
        ))}
        <g fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.stone}>
          {[0, 20, 40, 60].map((s) => (
            <text key={s} x={tx(s)} y={344} textAnchor="middle">
              {s}
            </text>
          ))}
        </g>
        <text x={340} y={358} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.graphite}>
          time (s)
        </text>

        <path d={linkRate} fill="none" stroke={COLORS.slate} strokeWidth={1.2} strokeDasharray="4 3" />
        {paths && (
          <g clipPath="url(#bwClip)">
            <defs>
              <clipPath id="bwClip">
                <rect x={52} y={12} width={Math.max(0, tx(t) - 52)} height={330} />
              </clipPath>
            </defs>
            <path d={paths.del} fill="none" stroke={COLORS.bbr} strokeWidth={1.3} />
            <path d={paths.inf} fill="none" stroke={COLORS.bbr} strokeWidth={1.3} />
            <path d={paths.rtt} fill="none" stroke={COLORS.bbr} strokeWidth={1.3} />
          </g>
        )}
        <line x1={tx(t)} y1={12} x2={tx(t)} y2={330} stroke={COLORS.ink} strokeWidth={1} strokeOpacity={0.4} />
      </svg>
      <Transport tr={tr} T={BWSTEP_DUR_S} />
    </FigureCard>
  )
}
