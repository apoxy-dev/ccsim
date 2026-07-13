import { useMemo, useState } from 'react'
import { Figure2a } from './components/figure2a'
import { FigureBwStep } from './components/figure-bwstep'
import { useTransport } from './components/transport'
import { useBwStepRun, useLabRuns } from './lib/use-lab'
import { lossMarks, toPts } from './lib/series'
import { DEFAULT_CFG, RUN_DUR_S, BWSTEP_DUR_S, BWSTEP_CFG, derive, type LabCfg } from './lib/scenario'

function Slider({
  label,
  value,
  min,
  max,
  step,
  fmt,
  onChange,
}: {
  label: string
  value: number
  min: number
  max: number
  step: number
  fmt?: (v: number) => string
  onChange: (v: number) => void
}) {
  return (
    <label className="ctl">
      <span className="ctl-label">
        {label} <b>{fmt ? fmt(value) : value}</b>
      </span>
      <input type="range" min={min} max={max} step={step} value={value} onChange={(e) => onChange(+e.target.value)} />
    </label>
  )
}

export function App() {
  const [cfg, setCfg] = useState<LabCfg>(DEFAULT_CFG)
  const d = useMemo(() => derive(cfg), [cfg])

  const runs = useLabRuns(cfg)
  const bw = useBwStepRun()

  // Re-derive figure points per sample batch; version is the cache key —
  // d/cfg are deliberately omitted because a cfg change replaces the
  // RunData objects (and resets version) in the same commit's effect.
  const cubicPts = useMemo(
    () => toPts(runs.cubic, d, cfg.rateMbps, false),
    [runs.cubic, runs.version],
  )
  const bbrPts = useMemo(
    () => toPts(runs.bbr, d, cfg.rateMbps, true),
    [runs.bbr, runs.version],
  )
  const losses = useMemo(() => lossMarks(runs.cubic, cubicPts, d), [cubicPts])
  const bwPts = useMemo(
    () => toPts(bw.run, derive(BWSTEP_CFG), BWSTEP_CFG.rateMbps, true),
    [bw.run, bw.version],
  )

  const loadedT = Math.min(runs.cubic.maxT, runs.bbr.maxT)
  const tr = useTransport(RUN_DUR_S, loadedT)
  const trBw = useTransport(BWSTEP_DUR_S, bw.run.maxT)

  const set = (k: keyof LabCfg) => (v: number) => setCfg((c) => ({ ...c, [k]: v }))

  return (
    <div className="page">
      <header className="hdr">
        <div className="hdr-title">CCSIM — CC LAB</div>
        <div className="hdr-sub">
          two full netstacks · one bottleneck · deterministic replays — cubic and bbrv3 on
          independent single-flow runs, drawn from the live wasm sample stream
        </div>
      </header>

      <div className="fig-card ctl-card">
        <div className="fig-title">SCENARIO</div>
        <div className="ctl-row">
          <Slider label="link" value={cfg.rateMbps} min={10} max={400} step={5} fmt={(v) => `${v} Mbps`} onChange={set('rateMbps')} />
          <Slider label="owd" value={cfg.owdMs} min={5} max={50} step={1} fmt={(v) => `${v} ms`} onChange={set('owdMs')} />
          <Slider label="loss" value={cfg.lossPct} min={0} max={3} step={0.05} fmt={(v) => `${v.toFixed(2)} %`} onChange={set('lossPct')} />
          <Slider label="buffer" value={cfg.qlimPkts} min={20} max={2000} step={10} fmt={(v) => `${v} pkt`} onChange={set('qlimPkts')} />
        </div>
        <div className="ctl-derived">
          BDP {Math.round(d.bdpPkts)} pkt · buf {d.bufX.toFixed(2)}×BDP · base rtt {d.baseMs} ms ·{' '}
          {runs.error ? (
            <span className="err">{runs.error}</span>
          ) : runs.running ? (
            `simulating… ${loadedT.toFixed(1)} / ${RUN_DUR_S} s`
          ) : (
            `${RUN_DUR_S} s × 2 runs ready`
          )}
        </div>
      </div>

      <Figure2a cubic={cubicPts} bbr={bbrPts} losses={losses} d={d} tr={tr} T={RUN_DUR_S} />
      <FigureBwStep pts={bwPts} tr={trBw} error={bw.error} />

      <footer className="ftr">
        deterministic: same scenario + seed ⇒ byte-identical streams, native and wasm ·{' '}
        <a href="https://github.com/apoxy-dev/ccsim">apoxy-dev/ccsim</a>
      </footer>
    </div>
  )
}
