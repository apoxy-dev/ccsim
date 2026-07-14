// Play/scrub transport row shared by the figures, plus the rAF clock hook.

import { useEffect, useRef, useState, type ReactNode } from 'react'

export interface TransportState {
  t: number
  playing: boolean
  scrub: (t: number) => void // pause and jump (slider drags)
  seek: (t: number) => void // jump without pausing (event skip)
  toggle: () => void
}

// Advances t while playing, wrapping at T; never runs past the loaded data
// edge so progressive draws don't flatline at zero. rateOf lets a figure
// modulate playback speed as a function of sim time (auto slow-mo around
// events); it is read through a ref so its identity never restarts the loop.
export function useTransport(
  T: number,
  loadedT: number,
  autoplay = true,
  rateOf?: (t: number) => number,
): TransportState {
  const [t, setT] = useState(0)
  const [playing, setPlaying] = useState(autoplay)
  const last = useRef(performance.now())
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
      setT((cur) => {
        let next = cur + d * (rate.current ? rate.current(cur) : 1)
        if (next > T) next = 0
        if (next > edge.current) next = edge.current > 0 ? cur : 0
        return next
      })
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
  }, [playing, T])
  return {
    t,
    playing,
    scrub: (v) => {
      setPlaying(false)
      setT(Math.min(v, edge.current))
    },
    seek: (v) => {
      setT(Math.min(Math.max(0, v), edge.current))
      setPlaying(true)
    },
    toggle: () => setPlaying((p) => !p),
  }
}

export function Transport({ tr, T, extra }: { tr: TransportState; T: number; extra?: ReactNode }) {
  return (
    <div className="transport">
      <button className="btn-solid" onClick={tr.toggle}>
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
