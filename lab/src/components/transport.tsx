// Play/scrub transport row shared by the figures, plus the rAF clock hook.

import { PauseFilled, PlayFilledAlt } from '@carbon/icons-react'
import { useEffect, useMemo, useRef, useState, type MutableRefObject, type ReactNode } from 'react'

export interface TransportState {
  t: number
  // The live clock, advanced every rAF tick regardless of `coarse`. Canvas
  // layers read this from their own draw loop so animation never routes
  // through React state.
  tRef: MutableRefObject<number>
  playing: boolean
  scrub: (t: number) => void // pause and jump (slider drags)
  seek: (t: number) => void // jump without pausing (event skip)
  toggle: () => void
}

// Advances t while playing, wrapping at T; never runs past the loaded data
// edge so progressive draws don't flatline at zero. rateOf lets a figure
// modulate playback speed as a function of sim time (auto slow-mo around
// events); it is read through a ref so its identity never restarts the loop.
// coarse throttles the React state mirror to ~8 Hz for figures that animate
// from tRef on a canvas — the state tick then only drives the slider row.
export function useTransport(
  T: number,
  loadedT: number,
  autoplay = true,
  rateOf?: (t: number) => number,
  coarse = false,
): TransportState {
  const [t, setT] = useState(0)
  const [playing, setPlaying] = useState(autoplay)
  const tRef = useRef(0)
  const last = useRef(performance.now())
  const lastPush = useRef(0)
  const edge = useRef(loadedT)
  edge.current = loadedT
  const rate = useRef(rateOf)
  rate.current = rateOf
  useEffect(() => {
    if (!playing) return
    let raf: number
    last.current = performance.now()
    const tick = (now: number) => {
      const d = (now - last.current) / 1000
      last.current = now
      const cur = tRef.current
      let next = cur + d * (rate.current ? rate.current(cur) : 1)
      if (next > T) next = 0
      if (next > edge.current) next = edge.current > 0 ? cur : 0
      tRef.current = next
      if (!coarse || now - lastPush.current > 125) {
        lastPush.current = now
        setT(next)
      }
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
  }, [playing, T, coarse])
  return useMemo(
    () => ({
      t,
      tRef,
      playing,
      scrub: (v: number) => {
        setPlaying(false)
        const nv = Math.min(v, edge.current)
        tRef.current = nv
        setT(nv)
      },
      seek: (v: number) => {
        const nv = Math.min(Math.max(0, v), edge.current)
        tRef.current = nv
        setT(nv)
        setPlaying(true)
      },
      toggle: () => setPlaying((p) => !p),
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [t, playing],
  )
}

export function Transport({ tr, T, extra }: { tr: TransportState; T: number; extra?: ReactNode }) {
  return (
    <div className="transport">
      <button className="btn-solid" onClick={tr.toggle}>
        {tr.playing ? <PauseFilled size={12} /> : <PlayFilledAlt size={12} />}
        {tr.playing ? 'PAUSE' : 'PLAY'}
      </button>
      <input
        type="range"
        min={0}
        max={T}
        step={0.02}
        value={tr.t}
        onChange={(e) => tr.scrub(+e.target.value)}
      />
      <span className="transport-t">t = {tr.t.toFixed(2).padStart(5, '0')} s</span>
      {extra}
    </div>
  )
}
