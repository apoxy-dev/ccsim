// Run orchestration: each figure drives a pair of independent single-flow
// sims (cubic + bbr) for one scenario parameter set. Records accumulate
// into RunData while the workers stream; `version` bumps per batch so
// consumers re-derive their views progressively.

import { useEffect, useRef, useState } from 'react'
import { RunData } from './series'
import { isDisposed, SimClient } from './sim-client'

export interface SimPair {
  cubic: RunData
  bbr: RunData
  version: number
  running: boolean
  error?: string
}

// Runs the two scenarios side by side, restarting both whenever either
// scenario object changes identity — callers must memoize them.
export function useSimPair(cubicScn: object, bbrScn: object): SimPair {
  const [state, setState] = useState<SimPair>(() => ({
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
    // Debounce slider scrubbing (and StrictMode's discarded first mount).
    // The clients are constructed inside the timeout on purpose: a
    // SimClient spawns a worker that immediately starts fetching and
    // instantiating the wasm module, so creating them per input event
    // would burn a worker pair on every slider tick.
    const timer = setTimeout(() => {
      if (!live()) return
      clients = [new SimClient(), new SimClient()]
      Promise.all([
        clients[0].run(cubicScn, { onRecords: (r) => (cubic.push(r), bump()) }),
        clients[1].run(bbrScn, { onRecords: (r) => (bbr.push(r), bump()) }),
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
  }, [cubicScn, bbrScn])

  return state
}
