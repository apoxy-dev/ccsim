// Run orchestration: two independent single-flow sims (cubic + bbr) per
// parameter set, plus the fixed bandwidth-change run. Records accumulate
// into RunData while the workers stream; `version` bumps per batch so
// consumers re-derive their views progressively.

import { useEffect, useMemo, useRef, useState } from 'react'
import { RunData } from './series'
import { isDisposed, SimClient } from './sim-client'
import { bwStepScenario, scenarioFor, type LabCfg } from './scenario'

export interface LabRuns {
  cubic: RunData
  bbr: RunData
  version: number
  running: boolean
  error?: string
}

export function useLabRuns(cfg: LabCfg): LabRuns {
  const [state, setState] = useState<LabRuns>(() => ({
    cubic: new RunData(),
    bbr: new RunData(),
    version: 0,
    running: false,
  }))
  const genRef = useRef(0)

  useEffect(() => {
    const gen = ++genRef.current
    const cubic = new RunData()
    const bbr = new RunData()
    setState({ cubic, bbr, version: 0, running: true })
    let clients: SimClient[] = []
    const live = () => genRef.current === gen
    const bump = () => {
      if (live()) setState((s) => ({ ...s, version: s.version + 1 }))
    }
    // Debounce slider scrubbing. The clients are constructed inside the
    // timeout on purpose: a SimClient spawns a worker that immediately
    // starts fetching and instantiating the wasm module, so creating them
    // per input event would burn a worker pair on every slider tick.
    const timer = setTimeout(() => {
      if (!live()) return
      clients = [new SimClient(), new SimClient()]
      Promise.all([
        clients[0].run(scenarioFor('cubic', cfg), { onRecords: (r) => (cubic.push(r), bump()) }),
        clients[1].run(scenarioFor('bbr', cfg), { onRecords: (r) => (bbr.push(r), bump()) }),
      ])
        .then(() => live() && setState((s) => ({ ...s, running: false })))
        .catch((e) => {
          // One failed run must also stop its sibling, or the error UI
          // renders over a figure that keeps animating.
          clients.forEach((c) => c.dispose())
          if (live() && !isDisposed(e)) {
            setState((s) => ({ ...s, running: false, error: String(e) }))
          }
        })
    }, 250)
    return () => {
      clearTimeout(timer)
      clients.forEach((c) => c.dispose())
    }
  }, [cfg])

  return state
}

export interface BwStepRun {
  run: RunData
  version: number
  running: boolean
  error?: string
}

export function useBwStepRun(): BwStepRun {
  const run = useMemo(() => new RunData(), [])
  const [version, setVersion] = useState(0)
  const [running, setRunning] = useState(true)
  const [error, setError] = useState<string>()
  useEffect(() => {
    let client: SimClient | null = null
    // A zero-delay timer keeps StrictMode's discarded first mount from
    // spawning a worker and starting a throwaway run.
    const timer = setTimeout(() => {
      client = new SimClient()
      client
        .run(bwStepScenario(), { onRecords: (r) => (run.push(r), setVersion((v) => v + 1)) })
        .then(() => setRunning(false))
        .catch((e) => {
          if (!isDisposed(e)) {
            setError(String(e))
            setRunning(false)
          }
        })
    }, 0)
    return () => {
      clearTimeout(timer)
      client?.dispose()
    }
  }, [run])
  return { run, version, running, error }
}
