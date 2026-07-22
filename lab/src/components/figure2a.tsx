// Figure 1 (design option 2a): the BBR paper's Figure-1 layout — RTT ratio
// over delivery rate, sharing one inflight axis, with app-limited /
// bandwidth-limited / buffer-limited regions and live trails in both panels.

import { useMemo, type ReactNode } from 'react'
import { FigureCard } from './figure-card'
import { Transport, type TransportState } from './transport'
import { COLORS, PHASE_LIGHT, cross, trail, type Scale } from '../lib/trail'
import { ptAt, type LossMark, type Pt } from '../lib/series'
import type { Derived } from '../lib/scenario'

// Fixed panel scales from the design: x domain 0..2.6 ×BDP, RTT ratio
// 0.9..2.3, delivery 0..1.25 ×BtlBw.
const sxRaw: Scale = (v) => 56 + v * 215.385
const syR: Scale = (v) => 250 - (Math.min(v, 2.3) - 0.9) * 161.4
const syD: Scale = (v) => 560 - v * 208

export function Figure2a({
  cubic: cubicRaw,
  bbr: bbrRaw,
  cubicLosses,
  bbrLosses,
  d,
  tr,
  T,
  controls,
}: {
  cubic: Pt[]
  bbr: Pt[]
  cubicLosses: LossMark[]
  bbrLosses: LossMark[]
  d: Derived
  tr: TransportState
  T: number
  controls?: ReactNode
}) {
  const t = tr.t
  const cliff = Math.min(d.cliff, 2.5)

  // Project measured estimators onto the feasible envelope — only for this
  // figure. Its axes assume same-instant measurement, but srtt and the
  // delivery estimate describe packets ACKed an RTT ago while x is
  // instantaneous, so fast inflight changes would otherwise plot states in
  // the infeasible regions (RTT below the queue-implied delay, delivery
  // above the inflight-implied rate). Feasible excursions — srtt loops
  // above the envelope, delivery dips below it — pass through untouched.
  // Time-series figures keep the raw measured values.
  const project = (pts: Pt[]): Pt[] =>
    pts.map((p) => ({
      ...p,
      r: Math.max(p.r, Math.min(p.x, d.cliff)),
      y: Math.min(p.y, p.x),
    }))
  const cubic = useMemo(() => project(cubicRaw), [cubicRaw, d.cliff])
  const bbr = useMemo(() => project(bbrRaw), [bbrRaw, d.cliff])
  // Inflight clamps at the buffer-full line: in recovery RFC 6675
  // outstanding legitimately exceeds cwnd by the SACKed-hole span, which
  // would otherwise run the trail past the loss point into the infeasible
  // region.
  const sxA: Scale = (v) => sxRaw(Math.min(v, cliff))
  const xB = sxRaw(1)
  const xC = sxRaw(cliff)
  const rCliff = Math.min(d.cliff, 2.3)

  const lossElsFor = (losses: LossMark[], run: 'cubic' | 'bbr') =>
    losses.flatMap((ev, i) => {
      if (ev.t > t || ev.t < t - 6) return []
      // Apply the same feasible-envelope projection as the owning trail so
      // each actual drop timestamp lands on that run's displayed path.
      const y = Math.min(ev.y, ev.x)
      const r = Math.max(ev.r, Math.min(ev.x, d.cliff))
      const color = run === 'bbr' ? COLORS.bbr : COLORS.cubic
      return [
        <g key={`${run}-${ev.kind}-${i}`} data-drop-run={run} data-drop-kind={ev.kind}>
          <title>{`${run === 'bbr' ? 'BBRv3' : 'CUBIC'} ${ev.kind} packet-drop episode`}</title>
          {cross(sxA(ev.x), syD(y), color, 0.95, 'd', 3.2)}
          {cross(sxA(ev.x), syR(r), color, 0.95, 'r', 3.2)}
        </g>,
      ]
    })
  const lossEls = [
    ...lossElsFor(cubicLosses, 'cubic'),
    ...lossElsFor(bbrLosses, 'bbr'),
  ]

  const pc = ptAt(cubic, t)
  const pb = ptAt(bbr, t)

  return (
    <FigureCard
      title="FIG. 2 - RTT & DELIVERY VS. INFLIGHT"
      aside={
        <div className="fig-legend">
          <span style={{ color: COLORS.cubic }}>— cubic</span>
          <span style={{ color: COLORS.bbr }}>— bbrv3</span>
          <span style={{ color: COLORS.cubic }}>× cubic drop</span>
          <span style={{ color: COLORS.bbr }}>× bbrv3 drop</span>
        </div>
      }
      controls={controls}
      note={
        <>
          <div>
            Adapted from Figure 1 in the original{' '}
            <a
              href="https://spawn-queue.acm.org/doi/pdf/10.1145/3012426.3022184"
              target="_blank"
              rel="noreferrer"
            >
              BBR paper
            </a>
            . One BDP is the sweet spot: enough data in flight to fill the path without building a
            queue. CUBIC searches by crossing into loss; BBRv3 estimates the path and stays near
            the operating point.
          </div>
          <div className="fig-readout" style={{ marginTop: 8 }}>
            <span className="ro" style={{ minWidth: 200, color: COLORS.cubic }}>
              cubic {pc ? `${pc.x.toFixed(2)} bdp / rtt ${pc.r.toFixed(2)}×` : '—'}
            </span>
            <span className="ro" style={{ minWidth: 200, color: COLORS.bbr }}>
              bbrv3 {pb ? `${pb.x.toFixed(2)} bdp / rtt ${pb.r.toFixed(2)}×` : '—'}
            </span>
            <span className="ro" style={{ minWidth: 180 }}>
              bbr phase:{' '}
              <span style={{ color: (pb?.phase && PHASE_LIGHT[pb.phase]) || COLORS.bbr }}>
                ■ {pb?.phase ?? '—'}
              </span>
            </span>
          </div>
        </>
      }
    >
      <svg viewBox="0 0 640 610" style={{ display: 'block', width: '100%', height: 'auto' }}>
        <g fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.stone} letterSpacing="0.08em">
          <text x={(56 + xB) / 2} y={14} textAnchor="middle">
            APP-LIMITED
          </text>
          <text x={(xB + xC) / 2} y={14} textAnchor="middle">
            BANDWIDTH-LIMITED
          </text>
          <text x={(xC + 616) / 2} y={14} textAnchor="middle">
            BUFFER-LIMITED
          </text>
        </g>
        <line x1={xB} y1={24} x2={xB} y2={560} stroke={COLORS.fog} strokeWidth={1} strokeDasharray="2 4" />

        {/* top panel: RTT/RTprop. The shaded polygon is the paper's
            infeasible region — RTT cannot fall below RTprop, nor below the
            queueing delay implied by the amount inflight. */}
        <polygon
          points={`56,${syR(1)} ${xB},${syR(1)} ${xC},${syR(rCliff)} ${xC},250 56,250`}
          fill="rgba(30,29,28,0.045)"
        />
        <text
          x={(xB + xC) / 2 + 40}
          y={246}
          textAnchor="middle"
          fontFamily="JetBrains Mono"
          fontSize={9}
          fill={COLORS.stone}
          letterSpacing="0.08em"
        >
          INFEASIBLE
        </text>
        <line x1={56} y1={syR(1)} x2={xB} y2={syR(1)} stroke={COLORS.slate} strokeWidth={1.3} />
        <line x1={xB} y1={syR(1)} x2={xC} y2={syR(rCliff)} stroke={COLORS.slate} strokeWidth={1.3} />
        <line x1={xC} y1={24} x2={xC} y2={250} stroke={COLORS.loss} strokeWidth={1.3} strokeDasharray="3 4" />
        <text x={62} y={syR(1) - 8} fontFamily="JetBrains Mono" fontSize={10.5} fill={COLORS.slate}>
          RTprop
        </text>
        <text
          x={(xB + xC) / 2 - 32}
          y={(syR(1) + syR(rCliff)) / 2 - 25}
          fontFamily="JetBrains Mono"
          fontSize={10.5}
          fill={COLORS.slate}
          transform={`rotate(-37 ${(xB + xC) / 2 - 32} ${(syR(1) + syR(rCliff)) / 2 - 25})`}
        >
          slope = 1/BtlBw
        </text>
        <g fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.slate}>
          <text x={48} y={syR(1) + 4} textAnchor="end">
            1.0
          </text>
          <text x={48} y={syR(1.5) + 4} textAnchor="end">
            1.5
          </text>
          <text x={48} y={syR(2) + 4} textAnchor="end">
            2.0
          </text>
        </g>
        <text
          x={14}
          y={137}
          fontFamily="JetBrains Mono"
          fontSize={10.5}
          fill={COLORS.graphite}
          transform="rotate(-90 14 137)"
          textAnchor="middle"
        >
          RTT / RTprop
        </text>
        <line x1={56} y1={24} x2={56} y2={250} stroke={COLORS.ink} strokeWidth={1} />
        <line x1={56} y1={250} x2={616} y2={250} stroke={COLORS.fog} strokeWidth={1} />

        {/* bottom panel: delivery / BtlBw. Infeasible above the envelope —
            delivery cannot exceed inflight/RTprop nor BtlBw. */}
        <polygon
          points={`56,560 ${xB},${syD(1)} ${xC},${syD(1)} ${xC},300 56,300`}
          fill="rgba(30,29,28,0.045)"
        />
        <text
          x={(56 + xB) / 2}
          y={400}
          textAnchor="middle"
          fontFamily="JetBrains Mono"
          fontSize={9}
          fill={COLORS.stone}
          letterSpacing="0.08em"
        >
          INFEASIBLE
        </text>
        <text
          x={(xB + xC) / 2}
          y={546}
          textAnchor="middle"
          fontFamily="JetBrains Mono"
          fontSize={10}
          fill={COLORS.stone}
          letterSpacing="0.08em"
        >
          QUEUE BUILDS
        </text>
        <line x1={56} y1={560} x2={xB} y2={syD(1)} stroke={COLORS.slate} strokeWidth={1.3} />
        <line x1={xB} y1={syD(1)} x2={xC} y2={syD(1)} stroke={COLORS.slate} strokeWidth={1.3} />
        <line x1={xC} y1={300} x2={xC} y2={560} stroke={COLORS.loss} strokeWidth={1.3} strokeDasharray="3 4" />
        <circle cx={xB} cy={syD(1)} r={3.5} fill="none" stroke={COLORS.ink} strokeWidth={1.3} />
        <text x={xB - 7} y={syD(1) - 14} textAnchor="end" fontFamily="JetBrains Mono" fontSize={10.5} fill={COLORS.ink}>
          optimal point (Kleinrock)
        </text>
        <circle cx={xC} cy={syD(1)} r={3.5} fill="none" stroke={COLORS.loss} strokeWidth={1.3} />
        <text x={xC + 8} y={syD(1) - 14} fontFamily="JetBrains Mono" fontSize={10.5} fill={COLORS.loss}>
          loss point
        </text>
        <text
          x={(xB + xC) / 2}
          y={syD(1) - 7}
          fontFamily="JetBrains Mono"
          fontSize={10.5}
          fill={COLORS.slate}
          textAnchor="middle"
        >
          BtlBw
        </text>
        <g fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.slate}>
          <text x={48} y={564} textAnchor="end">
            0
          </text>
          <text x={48} y={syD(0.5) + 4} textAnchor="end">
            0.5
          </text>
          <text x={48} y={syD(1) + 4} textAnchor="end">
            1.0
          </text>
        </g>
        <text
          x={14}
          y={430}
          fontFamily="JetBrains Mono"
          fontSize={10.5}
          fill={COLORS.graphite}
          transform="rotate(-90 14 430)"
          textAnchor="middle"
        >
          delivery / BtlBw
        </text>
        <line x1={56} y1={300} x2={56} y2={560} stroke={COLORS.ink} strokeWidth={1} />
        <line x1={56} y1={560} x2={616} y2={560} stroke={COLORS.ink} strokeWidth={1} />
        <g fontFamily="JetBrains Mono" fontSize={10} fill={COLORS.graphite}>
          <text x={xB} y={578} textAnchor="middle">
            BDP
          </text>
          <text x={xC} y={578} textAnchor="middle" fill={COLORS.loss}>
            BDP + BtlneckBufSize
          </text>
        </g>
        <text x={336} y={600} textAnchor="middle" fontFamily="JetBrains Mono" fontSize={10.5} fill={COLORS.graphite}>
          amount inflight
        </text>

        {/* live layers — the BBR trail is colored by phase (design 1c) so
            the periodic ProbeRTT drain excursion reads as a labeled event
            rather than stray marks in the app-limited region */}
        <g>{trail(cubic, t, sxA, syR, { color: COLORS.cubic, yKey: 'r' })}</g>
        <g>{trail(bbr, t, sxA, syR, { color: COLORS.bbr, phaseColors: PHASE_LIGHT, yKey: 'r' })}</g>
        <g>{trail(cubic, t, sxA, syD, { color: COLORS.cubic })}</g>
        <g>{trail(bbr, t, sxA, syD, { color: COLORS.bbr, phaseColors: PHASE_LIGHT })}</g>
        <g>{lossEls}</g>
      </svg>
      <Transport tr={tr} T={T} />
    </FigureCard>
  )
}
