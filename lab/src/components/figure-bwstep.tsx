// Figure 2: the bandwidth-change experiment from the BBR paper (ACM Queue
// 2016, figure 3) — a 10-Mbps, 40-ms bottleneck that doubles at t=20 s and
// halves back at t=40 s. Three lanes vs. time: delivery rate against the
// link-rate step, inflight, and srtt. Unlike the paper's BBR-only figure,
// Cubic runs the same experiment as an overlay: it discovers the up-step
// only by growing cwnd, and reacts to the down-step only after the queue
// overflows — the loss-based vs model-based contrast in one plot.

import { useMemo, type ReactNode } from 'react'
import { FigureCard } from './figure-card'
import { Transport, type TransportState } from './transport'
import { COLORS } from '../lib/trail'
import { ptAt, type Pt } from '../lib/series'
import { BWSTEP_CFG, BWSTEP_DUR_S, derive } from '../lib/scenario'

const tx = (t: number) => 52 + t * 9.6
const d = derive(BWSTEP_CFG)

// Lane scales. Delivery in Mbps (normalized pts carry delivery/10Mbps),
// inflight in kB, srtt in ms. The inflight and srtt ranges are sized for
// Cubic sitting at buffer-full (~250 kB, ~200 ms against a ~50 kB BDP),
// not just BBR's transient spikes.
const yDel = (mbps: number) => 110 - (90 * Math.max(0, Math.min(mbps, 24))) / 24
const yInf = (kb: number) => 226 - (76 * Math.max(0, Math.min(kb, 280))) / 280
const yRtt = (ms: number) => 330 - (76 * Math.max(0, Math.min(ms - 30, 200))) / 200

const STEPS = [
  { t: 20, label: 'BtlBw ×2' },
  { t: 40, label: 'BtlBw ÷2' },
]

const flowPath = (pts: Pt[]) => {
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
}

function ReadoutRow({ label, p, color }: { label: string; p?: Pt; color: string }) {
  return (
    <div className="fig-readout" style={{ color }}>
      <span className="ro" style={{ minWidth: 52 }}>
        {label}
      </span>
      <span className="ro" style={{ minWidth: 96 }}>
        {p ? `${(p.y * BWSTEP_CFG.rateMbps).toFixed(1)} Mbps` : '—'}
      </span>
      <span className="ro" style={{ minWidth: 72 }}>
        {p ? `${((p.x * d.bdpBytes) / 1000).toFixed(0)} kB` : ''}
      </span>
      <span className="ro" style={{ minWidth: 88 }}>
        {p ? `${(p.r * d.baseMs).toFixed(1)} ms` : ''}
      </span>
      <span className="ro" style={{ minWidth: 110 }}>
        {p?.phase ?? ''}
      </span>
    </div>
  )
}

export function FigureBwStep({
  cubic,
  bbr,
  tr,
  controls,
}: {
  cubic: Pt[]
  bbr: Pt[]
  tr: TransportState
  controls?: ReactNode
}) {
  const t = tr.t
  const cubicPaths = useMemo(() => flowPath(cubic), [cubic])
  const bbrPaths = useMemo(() => flowPath(bbr), [bbr])

  const pc = ptAt(cubic, t)
  const pb = ptAt(bbr, t)
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
      title="FIG. 2 — BANDWIDTH CHANGE · 10 MBPS × 40 MS"
      aside={
        <div className="fig-legend">
          <span style={{ color: COLORS.cubic }}>— cubic</span>
          <span style={{ color: COLORS.bbr }}>— bbrv3</span>
          <span style={{ color: COLORS.slate }}>— link rate</span>
        </div>
      }
      controls={controls}
      note={
        <>
          <a
            href="https://spawn-queue.acm.org/doi/pdf/10.1145/3012426.3022184"
            target="_blank"
            rel="noreferrer"
          >
            The paper's
          </a>{' '}
          figure 3, plus cubic: bbr converges on the up-step within a probe cycle (max-filter
          admits the new rate immediately) and drains the down-step spike once the old BtlBw
          estimate ages out; cubic grows into new bandwidth one RTT at a time and notices the
          down-step only when the buffer overflows.
        </>
      }
    >
      <svg viewBox="0 0 640 360" style={{ display: 'block', width: '100%', height: 'auto' }}>
        {lane(20, 110, 'DELIVERY RATE (MBPS)', [
          [yDel(10), '10'],
          [yDel(20), '20'],
        ])}
        {lane(140, 226, 'INFLIGHT (KB)', [
          [yInf(100), '100'],
          [yInf(200), '200'],
        ])}
        {lane(250, 330, 'SRTT (MS)', [
          [yRtt(80), '80'],
          [yRtt(160), '160'],
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
        {(cubicPaths || bbrPaths) && (
          <g clipPath="url(#bwClip)">
            <defs>
              <clipPath id="bwClip">
                <rect x={52} y={12} width={Math.max(0, tx(t) - 52)} height={330} />
              </clipPath>
            </defs>
            {cubicPaths && (
              <>
                <path d={cubicPaths.del} fill="none" stroke={COLORS.cubic} strokeWidth={1.3} />
                <path d={cubicPaths.inf} fill="none" stroke={COLORS.cubic} strokeWidth={1.3} />
                <path d={cubicPaths.rtt} fill="none" stroke={COLORS.cubic} strokeWidth={1.3} />
              </>
            )}
            {bbrPaths && (
              <>
                <path d={bbrPaths.del} fill="none" stroke={COLORS.bbr} strokeWidth={1.3} />
                <path d={bbrPaths.inf} fill="none" stroke={COLORS.bbr} strokeWidth={1.3} />
                <path d={bbrPaths.rtt} fill="none" stroke={COLORS.bbr} strokeWidth={1.3} />
              </>
            )}
          </g>
        )}
        <line x1={tx(t)} y1={12} x2={tx(t)} y2={330} stroke={COLORS.ink} strokeWidth={1} strokeOpacity={0.4} />
      </svg>
      <Transport tr={tr} T={BWSTEP_DUR_S} />
      {/* Fixed-width cells so live values don't reflow the rows. */}
      <div style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 4 }}>
        <ReadoutRow label="cubic" p={pc} color={COLORS.cubic} />
        <ReadoutRow label="bbrv3" p={pb} color={COLORS.bbr} />
      </div>
    </FigureCard>
  )
}
