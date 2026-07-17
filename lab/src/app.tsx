import { useCallback, useMemo, useState } from 'react'
import { Figure2a } from './components/figure2a'
import { FigureBwStep } from './components/figure-bwstep'
import { Pipe3b, pipeEventTimes, rateAt } from './components/pipe3b'
import { useTransport } from './components/transport'
import { useSimPair, useSimRun } from './lib/use-lab'
import { lossMarks, toPts } from './lib/series'
import {
  DEFAULT_CFG,
  HEAVY_CFG,
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

// Zero is a useful control, but every non-zero point is logarithmic. At a
// 100 Mbps / 40 ms path, the old first step of 0.05% already drives Cubic
// down near 15 Mbps; the 1-3 decade sequence exposes the transition instead
// of jumping over it while retaining the deliberately harsh upper range.
const LOSS_PCT_STEPS = [0, 0.001, 0.003, 0.01, 0.03, 0.1, 0.3, 1, 3] as const

function lossStepIndex(value: number): number {
  let best = 0
  for (let i = 1; i < LOSS_PCT_STEPS.length; i++) {
    if (Math.abs(LOSS_PCT_STEPS[i] - value) < Math.abs(LOSS_PCT_STEPS[best] - value)) best = i
  }
  return best
}

function formatLossPct(value: number): string {
  if (value === 0) return '0 %'
  if (value < 0.01) return `${value.toFixed(3)} %`
  if (value < 0.1) return `${value.toFixed(2)} %`
  if (value < 1) return `${value.toFixed(1)} %`
  return `${value.toFixed(0)} %`
}

function LossSlider({ value, onChange }: { value: number; onChange: (v: number) => void }) {
  return (
    <label className="ctl">
      <span className="ctl-label">
        loss <b>{formatLossPct(value)}</b>
      </span>
      <input
        type="range"
        min={0}
        max={LOSS_PCT_STEPS.length - 1}
        step={1}
        value={lossStepIndex(value)}
        aria-valuetext={formatLossPct(value)}
        onChange={(e) => onChange(LOSS_PCT_STEPS[+e.target.value])}
      />
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
  runCount = 2,
  onReset,
}: {
  left?: string
  error?: string
  running: boolean
  pct: number // combined progress for the represented run(s), 0..1
  durS: number
  runCount?: number
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
          runCount === 1 ? `${durS} s run ready` : `${durS} s × ${runCount} runs ready`
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

  // Fig. 3 is a separate experiment. Its link knobs and selected-flow run
  // never reuse or mutate Fig. 1's comparison state.
  const [pipeCfg, setPipeCfg] = useState<LabCfg>(HEAVY_CFG)
  const pipeD = useMemo(() => derive(pipeCfg), [pipeCfg])
  const pipeSet = (k: keyof LabCfg) => (v: number) =>
    setPipeCfg((c) => ({ ...c, [k]: v }))
  const [pipeFlow, setPipeFlow] = useState<CC>('naive')

  const [bwLossPct, setBwLossPct] = useState(0)
  const [bwJitterMs, setBwJitterMs] = useState(0)

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
  // Run only Fig. 3's selected controller. This keeps its state isolated
  // without making a slider change launch three full Go/Wasm simulations.
  const pipeScn = useMemo(() => scenarioFor(pipeFlow, pipeCfg), [pipeFlow, pipeCfg])
  const pipePre = useMemo(() => fig1Precomp(pipeFlow, pipeCfg), [pipeFlow, pipeCfg])
  const pipe = useSimRun(pipeScn, pipePre)
  const bwCubicScn = useMemo(() => bwStepScenario('cubic', bwLossPct, bwJitterMs), [bwLossPct, bwJitterMs])
  const bwBbrScn = useMemo(() => bwStepScenario('bbr', bwLossPct, bwJitterMs), [bwLossPct, bwJitterMs])
  const pre2 = useMemo(() => {
    const c = fig2Precomp('cubic', bwLossPct, bwJitterMs)
    const b = fig2Precomp('bbr', bwLossPct, bwJitterMs)
    return c && b ? { cubic: c, bbr: b } : null
  }, [bwLossPct, bwJitterMs])
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
  const pipePts = useMemo(
    () => toPts(pipe.data, pipeD, pipeCfg.rateMbps, pipeFlow === 'bbr'),
    [pipe.data, pipe.version, pipeD, pipeCfg.rateMbps, pipeFlow],
  )
  const cubicLosses = useMemo(() => lossMarks(runs.cubic, cubicPts, d), [cubicPts])
  const bbrLosses = useMemo(() => lossMarks(runs.bbr, bbrPts, d), [bbrPts])
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

  // The pipe cruises slower than real time and auto-slows around its own
  // selected flow's events, independently of every other figure.
  const pipeQueueDrops = pipe.data.dropEvents
  const pipeWireDrops = pipe.data.wireDropEvents
  const pipeEvents = useMemo(
    () => pipeEventTimes(pipePts, pipeQueueDrops, pipeWireDrops),
    [pipePts, pipeQueueDrops, pipeWireDrops],
  )
  const pipeRate = useCallback((t: number) => rateAt(pipeEvents, t), [pipeEvents])
  // coarse: the pipe animates from tr.tRef on a canvas, so its React tick
  // only needs to drive the slider row (~8 Hz) and the card stops
  // re-rendering at frame rate.
  const pipeLoadedT = pipe.data.maxT
  const trPipe = useTransport(RUN_DUR_S, pipeLoadedT, true, pipeRate, true)

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
          's figures, drawn live: full gVisor sender/receiver netstacks (built to WebAssembly)
          drive a simulated bottleneck link in your browser. Compare stock Cubic, a from-scratch
          BBRv3, and Fig. 3's fixed-rate naive control; every run is deterministic and replays
          byte-for-byte.
        </div>
      </header>

      <Pipe3b
        pts={pipePts}
        queueDropTimes={pipeQueueDrops}
        wireDropTimes={pipeWireDrops}
        events={pipeEvents}
        cfg={pipeCfg}
        d={pipeD}
        flow={pipeFlow}
        onFlow={(f) => {
          setPipeFlow(f)
          trPipe.seek(0)
        }}
        tr={trPipe}
        T={RUN_DUR_S}
        controls={
          <>
            <div className="ctl-row">
              <LossSlider value={pipeCfg.lossPct} onChange={pipeSet('lossPct')} />
              <Slider label="jitter" value={pipeCfg.jitterMs} min={0} max={100} step={1} fmt={(v) => `${v} ms`} onChange={pipeSet('jitterMs')} />
            </div>
            <StatusLine
              left={`bottleneck ${pipeCfg.rateMbps} Mbps · base rtt ${pipeD.baseMs} ms · buffer ${Math.round(pipeCfg.qlimPkts)} pkt`}
              error={pipe.error}
              running={pipe.running}
              pct={pipe.data.maxT / RUN_DUR_S}
              durS={RUN_DUR_S}
              runCount={1}
              onReset={() => {
                setPipeCfg(HEAVY_CFG)
                trPipe.scrub(0)
              }}
            />
            <SlowWarning />
          </>
        }
      />

      <Figure2a
        cubic={cubicPts}
        bbr={bbrPts}
        cubicLosses={cubicLosses}
        bbrLosses={bbrLosses}
        d={d}
        tr={tr}
        T={RUN_DUR_S}
        controls={
          <>
            {/* Link rate is fixed at the default: above ~100 Mbps a live 30 s
                wasm run is impractically slow even on a fast laptop. */}
            <div className="ctl-row">
              <Slider label="owd" value={cfg.owdMs} min={5} max={50} step={1} fmt={(v) => `${v} ms`} onChange={set('owdMs')} />
              <Slider label="jitter" value={cfg.jitterMs} min={0} max={100} step={1} fmt={(v) => `${v} ms`} onChange={set('jitterMs')} />
              <LossSlider value={cfg.lossPct} onChange={set('lossPct')} />
              <Slider label="buffer" value={cfg.qlimPkts} min={20} max={2000} step={10} fmt={(v) => `${v} pkt`} onChange={set('qlimPkts')} />
            </div>
            <StatusLine
              left={`link ${cfg.rateMbps} Mbps · BDP ${Math.round(d.bdpPkts)} pkt · buf ${d.bufX.toFixed(2)}×BDP · base rtt ${d.baseMs} ms`}
              error={runs.error}
              running={runs.running}
              pct={(runs.cubic.maxT + runs.bbr.maxT) / (2 * RUN_DUR_S)}
              durS={RUN_DUR_S}
              onReset={() => {
                setCfg(DEFAULT_CFG)
                tr.scrub(0)
              }}
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
              <LossSlider value={bwLossPct} onChange={setBwLossPct} />
              <Slider label="jitter" value={bwJitterMs} min={0} max={100} step={1} fmt={(v) => `${v} ms`} onChange={setBwJitterMs} />
            </div>
            <StatusLine
              error={bw.error}
              running={bw.running}
              pct={(bw.cubic.maxT + bw.bbr.maxT) / (2 * BWSTEP_DUR_S)}
              durS={BWSTEP_DUR_S}
              onReset={() => {
                setBwLossPct(0)
                setBwJitterMs(0)
                trBw.scrub(0)
              }}
            />
            <SlowWarning />
          </>
        }
      />

      <footer className="ftr">
        deterministic: same scenario + seed ⇒ byte-identical streams, native and wasm ·{' '}
        <a href="https://github.com/apoxy-dev/ccsim">apoxy-dev/ccsim</a>
      </footer>
    </div>
  )
}
