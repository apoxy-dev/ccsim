// Thin promise wrapper around the wasm worker mailbox (wasm/worker.js,
// copied into public/sim/ by `make lab-assets`). One SimClient = one worker
// = one run; dispose() terminates the worker and with it the wasm instance.

import { decode } from '../../../stream/decoder.mjs'

export interface Rec {
  t: number
  flow: number
  kind: number
  value: number
}

interface RunCallbacks {
  onRecords: (recs: Rec[]) => void
  onProgress?: (tS: number) => void
}

// Sentinel for promises settled by dispose(); callers use isDisposed() to
// tell teardown apart from real failures.
const DISPOSED = 'sim client disposed'

export function isDisposed(e: unknown): boolean {
  return e instanceof Error && e.message === DISPOSED
}

export class SimClient {
  private worker: Worker
  private ready: Promise<void>
  // Every pending promise registers its reject here so that a fatal
  // condition — worker script failure, wasm bootstrap error, dispose —
  // settles all of them instead of leaving run() hanging forever.
  private rejectors = new Set<(e: Error) => void>()

  constructor(base = '/sim/') {
    this.worker = new Worker(base + 'worker.js')
    this.worker.addEventListener('error', (e) =>
      this.fatal(new Error(`worker failed: ${e.message || 'script error'}`)),
    )
    this.worker.addEventListener('messageerror', () =>
      this.fatal(new Error('worker message deserialization failed')),
    )
    this.ready = new Promise((resolve, reject) => {
      this.rejectors.add(reject)
      const onMsg = (e: MessageEvent) => {
        const m = e.data
        if (m?.type === 'ready') {
          this.worker.removeEventListener('message', onMsg)
          this.rejectors.delete(reject)
          resolve()
        } else if (m?.type === 'error') {
          this.worker.removeEventListener('message', onMsg)
          this.fatal(new Error(m.error))
        }
      }
      this.worker.addEventListener('message', onMsg)
    })
  }

  private fatal(e: Error) {
    const pending = [...this.rejectors]
    this.rejectors.clear()
    pending.forEach((reject) => reject(e))
  }

  // Loads the scenario and streams it flat out to completion, delivering
  // decoded sample records as they flush. Resolves with the run summary.
  async run(scenario: object, cb: RunCallbacks): Promise<unknown> {
    await this.ready
    return new Promise((resolve, reject) => {
      this.rejectors.add(reject)
      const settle = () => {
        this.rejectors.delete(reject)
        this.worker.removeEventListener('message', onMsg)
      }
      const onMsg = (e: MessageEvent) => {
        const m = e.data
        switch (m.type) {
          case 'loaded':
            this.worker.postMessage({ op: 'stream', batch_ms: 250 })
            break
          case 'samples':
            cb.onRecords(decode(m.buf))
            break
          case 'progress':
            cb.onProgress?.(m.t_s)
            break
          case 'summary':
            settle()
            resolve(m.summary)
            break
          case 'error':
            settle()
            reject(new Error(m.error))
            break
        }
      }
      this.worker.addEventListener('message', onMsg)
      this.worker.postMessage({ op: 'load', scenario })
    })
  }

  dispose() {
    this.fatal(new Error(DISPOSED))
    this.worker.terminate()
  }
}
