import { useCallback, useMemo, useState } from 'react'
import { Figure2a } from './components/figure2a'
import { FigureBwStep } from './components/figure-bwstep'
import { Pipe3b, pipeEventTimes, rateAt } from './components/pipe3b'
import { useTransport } from './components/transport'
import { useSimPair } from './lib/use-lab'
import { lossMarks, toPts } from './lib/series'
import {
  DEFAULT_CFG,
  RUN_DUR_S,
  BWSTEP_DUR_S,
  BWSTEP_CFG,
  SMALL_MACHINE,
  bwStepScenario,
  derive,
  fig1Precomp,
  fig2Precomp,
  scenarioFor,
  type CC,
  type LabCfg,
} from './lib/scenario'

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

// Precomputed streams cover the defaults; leaving them means simulating
// live, which few-core devices pay for in wall-clock minutes. Warn before
// the first slider move, not after.
function SlowWarning() {
  if (!SMALL_MACHINE) return null
  return (
    <div className="ctl-warn">
      ⚠ defaults are precomputed — moving a slider runs the simulator live on this device, which
      can take a while
    </div>
  )
}

function StatusLine({
  left,
  error,
  running,
  pct,
  durS,
  onReset,
}: {
  left?: string
  error?: string
  running: boolean
  pct: number // combined progress of the pair, 0..1
  durS: number
  onReset: () => void
}) {
  return (
    <div className="ctl-derived ctl-status">
      <span>
        {left && `${left} · `}
        {error ? (
          <span className="err">{error}</span>
        ) : running ? (
          `simulating… ${Math.round(pct * 100)} %`
        ) : (
          `${durS} s × 2 runs ready`
        )}
      </span>
      <button className="btn-toggle" onClick={onReset}>
        RESET
      </button>
    </div>
  )
}

export function App() {
  const [cfg, setCfg] = useState<LabCfg>(DEFAULT_CFG)
  const d = useMemo(() => derive(cfg), [cfg])
  const set = (k: keyof LabCfg) => (v: number) => setCfg((c) => ({ ...c, [k]: v }))

  const [bwLossPct, setBwLossPct] = useState(0)

  // Scenario identity drives the run hooks — memoize so unrelated renders
  // don't restart the sims.
  const cubicScn = useMemo(() => scenarioFor('cubic', cfg), [cfg])
  const bbrScn = useMemo(() => scenarioFor('bbr', cfg), [cfg])
  const pre1 = useMemo(() => {
    const c = fig1Precomp('cubic', cfg)
    const b = fig1Precomp('bbr', cfg)
    return c && b ? { cubic: c, bbr: b } : null
  }, [cfg])
  const runs = useSimPair(cubicScn, bbrScn, pre1)
  const bwCubicScn = useMemo(() => bwStepScenario('cubic', bwLossPct), [bwLossPct])
  const bwBbrScn = useMemo(() => bwStepScenario('bbr', bwLossPct), [bwLossPct])
  const pre2 = useMemo(() => {
    const c = fig2Precomp('cubic', bwLossPct)
    const b = fig2Precomp('bbr', bwLossPct)
    return c && b ? { cubic: c, bbr: b } : null
  }, [bwLossPct])
  const bw = useSimPair(bwCubicScn, bwBbrScn, pre2)

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
  const bwD = derive(BWSTEP_CFG)
  const bwCubicPts = useMemo(
    () => toPts(bw.cubic, bwD, BWSTEP_CFG.rateMbps, false),
    [bw.cubic, bw.version],
  )
  const bwBbrPts = useMemo(
    () => toPts(bw.bbr, bwD, BWSTEP_CFG.rateMbps, true),
    [bw.bbr, bw.version],
  )

  const loadedT = Math.min(runs.cubic.maxT, runs.bbr.maxT)
  const tr = useTransport(RUN_DUR_S, loadedT)
  const bwLoadedT = Math.min(bw.cubic.maxT, bw.bbr.maxT)
  const trBw = useTransport(BWSTEP_DUR_S, bwLoadedT)

  // The pipe replays the selected flow of the FIG 1 run on its own
  // transport: it cruises slower than real time and auto-slows around the
  // flow's events, which would make the other figures crawl if shared.
  const [pipeFlow, setPipeFlow] = useState<CC>('cubic')
  const pipePts = pipeFlow === 'cubic' ? cubicPts : bbrPts
  const pipeDrops = pipeFlow === 'cubic' ? runs.cubic.dropEvents : runs.bbr.dropEvents
  const pipeEvents = useMemo(() => pipeEventTimes(pipePts, pipeDrops), [pipePts, pipeDrops])
  const pipeRate = useCallback((t: number) => rateAt(pipeEvents, t), [pipeEvents])
  // coarse: the pipe animates from tr.tRef on a canvas, so its React tick
  // only needs to drive the slider row (~8 Hz) and the card stops
  // re-rendering at frame rate.
  const trPipe = useTransport(RUN_DUR_S, loadedT, true, pipeRate, true)

  return (
    <div className="page">
      <header className="hdr">
        <div className="hdr-title">CCSIM — CC LAB</div>
        <div className="hdr-sub">
          The{' '}
          <a
            href="https://spawn-queue.acm.org/doi/pdf/10.1145/3012426.3022184"
            target="_blank"
            rel="noreferrer"
          >
            BBR paper
          </a>
          's figures, drawn live: two full gVisor netstack instances (built to WebAssembly) —
          stock Cubic and a from-scratch BBRv3 — drive a simulated bottleneck link in your
          browser. Move the sliders to reshape the path; every run is deterministic and replays
          byte-for-byte.
        </div>
      </header>

      <Figure2a
        cubic={cubicPts}
        bbr={bbrPts}
        losses={losses}
        d={d}
        tr={tr}
        T={RUN_DUR_S}
        controls={
          <>
            <div className="ctl-row">
              <Slider label="link" value={cfg.rateMbps} min={10} max={400} step={5} fmt={(v) => `${v} Mbps`} onChange={set('rateMbps')} />
              <Slider label="owd" value={cfg.owdMs} min={5} max={50} step={1} fmt={(v) => `${v} ms`} onChange={set('owdMs')} />
              <Slider label="loss" value={cfg.lossPct} min={0} max={3} step={0.05} fmt={(v) => `${v.toFixed(2)} %`} onChange={set('lossPct')} />
              <Slider label="buffer" value={cfg.qlimPkts} min={20} max={2000} step={10} fmt={(v) => `${v} pkt`} onChange={set('qlimPkts')} />
            </div>
            <StatusLine
              left={`BDP ${Math.round(d.bdpPkts)} pkt · buf ${d.bufX.toFixed(2)}×BDP · base rtt ${d.baseMs} ms`}
              error={runs.error}
              running={runs.running}
              pct={(runs.cubic.maxT + runs.bbr.maxT) / (2 * RUN_DUR_S)}
              durS={RUN_DUR_S}
              onReset={() => setCfg(DEFAULT_CFG)}
            />
            <SlowWarning />
          </>
        }
      />
      <FigureBwStep
        cubic={bwCubicPts}
        bbr={bwBbrPts}
        tr={trBw}
        controls={
          <>
            <div className="ctl-row">
              <Slider label="loss" value={bwLossPct} min={0} max={3} step={0.05} fmt={(v) => `${v.toFixed(2)} %`} onChange={setBwLossPct} />
            </div>
            <StatusLine
              error={bw.error}
              running={bw.running}
              pct={(bw.cubic.maxT + bw.bbr.maxT) / (2 * BWSTEP_DUR_S)}
              durS={BWSTEP_DUR_S}
              onReset={() => setBwLossPct(0)}
            />
            <SlowWarning />
          </>
        }
      />

      <Pipe3b
        pts={pipePts}
        dropTimes={pipeDrops}
        events={pipeEvents}
        cfg={cfg}
        d={d}
        flow={pipeFlow}
        onFlow={setPipeFlow}
        tr={trPipe}
        T={RUN_DUR_S}
      />

      <footer className="ftr">
        deterministic: same scenario + seed ⇒ byte-identical streams, native and wasm ·{' '}
        <a href="https://github.com/apoxy-dev/ccsim">apoxy-dev/ccsim</a>
      </footer>
    </div>
  )
}
