// Shared SVG figure primitives, ported from the design prototype: fading
// comet trails, loss crosses, and the phase color ramps.

import type { ReactElement } from 'react'
import type { LossMark, Phase, Pt } from './series'

export const COLORS = {
  ink: '#1E1D1C',
  graphite: '#4A4847',
  slate: '#6B6967',
  stone: '#ABABAB',
  fog: '#D9D7D3',
  mist: '#EDECE8',
  cubic: '#C28A2A',
  bbr: '#3D91DC',
  naive: '#8A5FA8',
  loss: '#EA6A4E',
}

export const PHASE_LIGHT: Record<Phase, string> = {
  startup: '#8FBADF',
  drain: '#6FA6D9',
  'bw:down': '#2E72B0',
  'bw:cruise': '#3D91DC',
  'bw:refill': '#5D9FD8',
  'bw:up': '#24598A',
  probertt: '#A9C8E4',
}

export type Scale = (v: number) => number

export function cross(
  px: number,
  py: number,
  color: string,
  op: number,
  key: string,
  s = 3.2,
): ReactElement {
  return (
    <g key={key} stroke={color} strokeWidth={1.4} strokeOpacity={op}>
      <line x1={px - s} y1={py - s} x2={px + s} y2={py + s} />
      <line x1={px - s} y1={py + s} x2={px + s} y2={py - s} />
    </g>
  )
}

interface TrailOpts {
  color: string
  phaseColors?: Partial<Record<Phase, string>> | null
  dotStroke?: string
  win?: number
  segs?: number
  yKey?: 'y' | 'r'
}

// Fading trail: the last `win` seconds of the trajectory drawn as `segs`
// slices of increasing opacity/width, with the live dot on top. Paths break
// across cwnd/ProbeRTT discontinuities — never interpolate a line segment
// across a multiplicative reset.
export function trail(pts: Pt[], t: number, sx: Scale, sy: Scale, opts: TrailOpts): ReactElement[] {
  const { color, phaseColors = null, dotStroke = '#FFFFFF', win = 6, segs = 14, yKey = 'y' } = opts
  const lo = t - win
  const inWin = pts.filter((p) => p.t <= t && p.t >= lo)
  if (inWin.length < 2) return []
  const per = Math.max(2, Math.ceil(inWin.length / segs))
  const els: ReactElement[] = []
  for (let i = 0; i < segs; i++) {
    const slice = inWin.slice(i * per, (i + 1) * per + 1)
    if (slice.length < 2) continue
    const f = (i + 1) / segs
    const runs: Pt[][] = [[slice[0]]]
    for (let j = 1; j < slice.length; j++) {
      // Break on a fast swing in either plotted dimension, not just x:
      // under random loss the real trajectories jump (inflight_lo cuts,
      // recovery exits) and interpolating across a jump draws long
      // spurious lines through the middle of the panel. Phase-colored
      // trails also break on phase boundaries so each run takes one color.
      const jump =
        Math.abs(slice[j].x - slice[j - 1].x) > 0.25 ||
        Math.abs(slice[j][yKey] - slice[j - 1][yKey]) > 0.2 ||
        (phaseColors != null && slice[j].phase !== slice[j - 1].phase)
      if (jump) runs.push([slice[j]])
      else runs[runs.length - 1].push(slice[j])
    }
    runs.forEach((run, ri) => {
      if (run.length < 2) return
      const last = run[run.length - 1]
      const col = phaseColors ? (last.phase && phaseColors[last.phase]) || color : color
      els.push(
        <polyline
          key={`s${i}r${ri}`}
          points={run.map((p) => `${sx(p.x).toFixed(1)},${sy(p[yKey]).toFixed(1)}`).join(' ')}
          fill="none"
          stroke={col}
          strokeOpacity={+(0.05 + 0.82 * Math.pow(f, 1.6)).toFixed(3)}
          strokeWidth={+(1 + 1.5 * f).toFixed(2)}
          strokeLinejoin="round"
          strokeLinecap="round"
        />,
      )
    })
  }
  const cur = inWin[inWin.length - 1]
  const curCol = phaseColors ? (cur.phase && phaseColors[cur.phase]) || color : color
  els.push(
    <circle
      key="dot"
      cx={sx(cur.x).toFixed(1)}
      cy={sy(cur[yKey]).toFixed(1)}
      r={4.2}
      fill={curCol}
      stroke={dotStroke}
      strokeWidth={1.5}
    />,
  )
  return els
}

export function lossCrosses(
  losses: LossMark[],
  t: number,
  sx: Scale,
  sy: Scale,
  yOf: (ev: LossMark) => number,
  color: string,
  win: number,
  keyPrefix = 'x',
): ReactElement[] {
  const els: ReactElement[] = []
  losses.forEach((ev, i) => {
    if (ev.t > t || ev.t < t - win) return
    els.push(cross(sx(ev.x), sy(yOf(ev)), color, 0.9, keyPrefix + i, 3.2))
  })
  return els
}
