// Figure 3 (design option 3b): the animated pipe — sender, bottleneck queue
// and receiver with packet dots, drop ballistics, and the srtt strip that
// spells out the bufferbloat equation (srtt = base + queueing delay).

import { useMemo, type ReactElement } from 'react'
import { FigureCard } from './figure-card'
import { Transport, type TransportState } from './transport'
import { COLORS } from '../lib/trail'
import { coalesce, ptAt, type Pt } from '../lib/series'
import type { CC, Derived, LabCfg } from '../lib/scenario'

const tx = (t: number) => 52 + t * 19.2

export function Pipe3b({
  pts,
  dropTimes,
  cfg,
  d,
  flow,
  onFlow,
  tr,
  T,
}: {
  pts: Pt[]
  dropTimes: number[]
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

  const drops = useMemo(() => coalesce(dropTimes, 0.1), [dropTimes])
  const pktPerQDot = Math.max(1, Math.round(d.bdpPkts / 11))

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
    // queue stack seated on the bottleneck box, limit tick at y=50
    const shown = Math.min(Math.floor(q / pktPerQDot), 12)
    for (let k = 0; k < shown; k++) {
      els.push(<circle key={'q' + k} cx={279} cy={120 - 6 * k} r={2.6} fill={col} />)
    }
    if (q > 5) {
      els.push(
        <text key="qn" x={279} y={192} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.slate}>
          q {Math.round(q)} pkt
        </text>,
      )
    }
    // forward dots: sender->queue, bottleneck->receiver
    const nM = Math.max(2, Math.min(16, Math.round((infPkts - q) / (d.bdpPkts / 13))))
    for (let i = 0; i < nM; i++) {
      const pr = (t * 0.55 + i / nM) % 1
      const x = pr < 0.45 ? 114 + (pr / 0.45) * 148 : 296 + ((pr - 0.45) / 0.55) * 228
      els.push(<circle key={'m' + i} cx={x.toFixed(1)} cy={140} r={3} fill={col} />)
    }
    // acks on the return path
    const nA = Math.max(1, Math.round(p.y * 5))
    for (let i = 0; i < nA; i++) {
      const pr = (t * 0.5 + i / nA) % 1
      els.push(<circle key={'ak' + i} cx={(556 - pr * 476).toFixed(1)} cy={200} r={2} fill={col} fillOpacity={0.55} />)
    }
    // drops eject up-and-right off the top of the stack, leaving a fading trace
    drops.forEach((evT, i) => {
      const d2 = t - evT
      if (d2 < 0 || d2 >= 0.8) return
      const px = (f: number) => 279 + f * 150
      const py = (f: number) => 46 - f * 95 + f * f * 60
      const fade = 1 - d2 / 0.8
      const steps = 8
      const trace: string[] = []
      for (let s = 0; s <= steps; s++) {
        const f = (d2 / 0.8) * (s / steps)
        trace.push(px(f).toFixed(1) + ',' + py(f).toFixed(1))
      }
      els.push(
        <polyline
          key={'drt' + i}
          points={trace.join(' ')}
          fill="none"
          stroke={COLORS.loss}
          strokeWidth={1.2}
          strokeOpacity={0.45 * fade}
          strokeDasharray="3 3"
        />,
        <circle
          key={'dr' + i}
          cx={px(d2 / 0.8).toFixed(1)}
          cy={py(d2 / 0.8).toFixed(1)}
          r={3}
          fill={COLORS.loss}
          fillOpacity={fade}
        />,
      )
    })
  }

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
          One dot ≈ {pktPerQDot} packets; <span style={{ color: COLORS.loss }}>drops fall out of the stack</span>.
          Above ~12 dots the queue renders as a count. Runs off the real sample stream of the selected flow's run.
        </>
      }
    >
      <svg viewBox="0 0 640 292" width="640" height="292" style={{ display: 'block' }}>
        <line x1={271} y1={50} x2={287} y2={50} stroke={COLORS.loss} strokeWidth={1.2} />
        <text x={294} y={53} fontFamily="JetBrains Mono" fontSize={9} fill={COLORS.loss}>
          buffer limit
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
      <Transport tr={tr} T={T} />
    </FigureCard>
  )
}
