// Run orchestration: each figure drives a pair of independent single-flow
// sims (cubic + bbr) for one scenario parameter set. Records accumulate
// into RunData while the workers stream; `version` bumps per batch so
// consumers re-derive their views progressively.

import { useEffect, useRef, useState } from 'react'
import { decode } from '../../../stream/decoder.mjs'
import { RunData } from './series'
import { isDisposed, SimClient } from './sim-client'
import { SMALL_MACHINE } from './scenario'

// Global worker-slot limiter. Each sim is a full Go runtime in a worker;
// four of them contending for a phone's two performance cores makes every
// run crawl, so small machines run at most two at a time. Combined with
// sequential pairs below, that gives every figure exactly one running sim
// instead of one figure hogging both slots while the other looks dead.
const MAX_SIMS = SMALL_MACHINE ? 2 : 4
let slots = MAX_SIMS
const waiters: (() => void)[] = []
const acquireSlot = (): Promise<void> => {
  if (slots > 0) {
    slots--
    return Promise.resolve()
  }
  return new Promise((r) => waiters.push(r))
}
const releaseSlot = () => {
  const w = waiters.shift()
  if (w) w()
  else slots++
}

export interface SimPair {
  cubic: RunData
  bbr: RunData
  version: number
  running: boolean
  error?: string
}

// Precomputed default streams (fig1Precomp/fig2Precomp in scenario.ts).
export interface PrePair {
  cubic: string
  bbr: string
}

// Runs the two scenarios side by side, restarting both whenever either
// scenario object changes identity — callers must memoize them (pre too).
// When pre names precomputed streams for this parameter set, they are
// fetched instead of simulating: determinism guarantees identical bytes,
// so defaults render instantly and wasm only runs off-defaults. A failed
// fetch quietly falls back to the live sims.
export function useSimPair(cubicScn: object, bbrScn: object, pre: PrePair | null): SimPair {
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
    const clients: SimClient[] = []
    const live = () => genRef.current === gen
    const bump = () => {
      if (live()) setState((s) => ({ ...s, version: s.version + 1 }))
    }
    // Debounce slider scrubbing (and StrictMode's discarded first mount).
    // The clients are constructed inside the timeout on purpose: a
    // SimClient spawns a worker that immediately starts fetching and
    // instantiating the wasm module, so creating them per input event
    // would burn a worker pair on every slider tick.
    // Set when either run fails so a sibling still queued for a slot does
    // not spawn a worker after the pair is already dead.
    let aborted = false
    const runOne = async (scn: object, data: RunData) => {
      await acquireSlot()
      if (!live() || aborted) {
        releaseSlot()
        return
      }
      const client = new SimClient()
      clients.push(client)
      try {
        await client.run(scn, { onRecords: (r) => (data.push(r), bump()) })
      } finally {
        releaseSlot()
      }
    }
    const startLive = () => {
      // Small machines run the pair back to back — one slot per figure —
      // so both figures make visible progress instead of the first one
      // hogging both slots while the second sits at zero for minutes.
      const pair = SMALL_MACHINE
        ? runOne(cubicScn, cubic).then(() => runOne(bbrScn, bbr))
        : Promise.all([runOne(cubicScn, cubic), runOne(bbrScn, bbr)])
      pair
        .then(() => {
          if (live()) setState((s) => ({ ...s, running: false }))
          // The runs are over and their records accumulated; terminating
          // the workers frees two idle Go runtimes' worth of memory.
          clients.forEach((c) => c.dispose())
        })
        .catch((e) => {
          // One failed run must also stop its sibling, or the error UI
          // renders over a figure that keeps animating.
          aborted = true
          clients.forEach((c) => c.dispose())
          if (live() && !isDisposed(e)) {
            setState((s) => ({ ...s, running: false, error: String(e) }))
          }
        })
    }
    // Both streams are fetched fully before either is pushed so a partial
    // failure cannot leave records behind for the live fallback to double.
    const loadPre = async (p: PrePair) => {
      const resps = await Promise.all([fetch(p.cubic), fetch(p.bbr)])
      if (!resps.every((r) => r.ok)) throw new Error('precomputed stream missing')
      const [cBuf, bBuf] = await Promise.all(resps.map((r) => r.arrayBuffer()))
      const cRecs = decode(cBuf)
      const bRecs = decode(bBuf)
      cubic.push(cRecs)
      bbr.push(bRecs)
    }
    const timer = setTimeout(() => {
      if (!live()) return
      if (pre) {
        loadPre(pre)
          .then(() => live() && setState((s) => ({ ...s, version: s.version + 1, running: false })))
          .catch(() => live() && startLive())
      } else {
        startLive()
      }
    }, 250)
    return () => {
      clearTimeout(timer)
      clients.forEach((c) => c.dispose())
    }
  }, [cubicScn, bbrScn, pre])

  return state
}
